// The capability stubs handed to a function as `env.datastore`, `env.search`,
// `env.auth` and `env.channel`.
//
// HOW THESE WORK, AND WHY IT IS DIFFERENT FROM HOSTED
//
// Hosted, each stub is a direct in-process handle into the service itself: no HTTP, no
// credential, and the scoping (org, grants) is held on the platform's side of the sandbox
// boundary, where function code can neither read nor forge it.
//
// The emulator cannot reproduce that, and imitating it by re-implementing 25 methods
// against the Go managers would mean two implementations of every rule — with the copy
// used by functions being the one nobody exercises. So a stub call is dispatched
// IN-PROCESS against the emulator's own /v1 handlers: a synthetic http.Request served
// straight into the mux, no socket, no network. The behaviour a function sees is
// therefore the behaviour the REST API has, by construction.
//
// What still has to be enforced here, because it is not the HTTP layer's job:
//
//   GRANTS. `requireGrant` runs BEFORE dispatch, with the same ladder as an API key
//   (read < write < full) and the same instance-specific-overrides-service-wide rule. A
//   function granted `datastore:a` must not reach `datastore:b`, and locally that check
//   is the only thing standing between them — the emulator's auth store is dev-open, so
//   the synthetic request would otherwise be allowed anything.

package functions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/dop251/goja"
)

// bindings dispatches stub calls into the emulator's own data planes.
type bindings struct {
	mux http.Handler
	// token is any bearer string: the emulator's auth store is dev-open, so this only
	// has to be present. Grants are enforced before we ever get here.
	token string
}

// call is one stub method: the HTTP shape it maps to, and how it reads its arguments.
type call struct {
	method  string
	level   auth.Level
	// levelFor overrides `level` when the grant a call needs depends on its ARGUMENTS.
	// Only channel.token does: minting a publish-capable token is a write, minting a
	// subscriber is a read, exactly as POST /tokens decides it. A static level here would
	// make the emulator the MORE PERMISSIVE of the two, which is the one direction of
	// divergence that actually costs something — a function that mints publish tokens
	// locally on a read grant, then 403s in production.
	levelFor func(args []goja.Value) auth.Level
	path    func(t target, args []goja.Value) (string, error)
	body    func(args []goja.Value) (any, error)
	unwrap  string // field to lift out of the response, "" to return the whole object
	minArgs int
	// custom replaces the single-request path entirely, for the two blob methods that have no
	// REST equivalent because they are binding-only hosted (`put` writes bytes straight to
	// storage; `bytes` reads them back). Expressing them as the PUBLIC calls they are
	// equivalent to keeps the emulator's HTTP surface identical to the hosted one — inventing
	// an endpoint here would be a route that works locally and 404s in production.
	custom func(b *bindings, t target, args []goja.Value) (any, error)
}

type target struct {
	instance  string
	namespace string
}

// stub builds one service's object with a Go function per method.
func (b *bindings) stub(vm *goja.Runtime, service string, grants map[string]string) (*goja.Object, error) {
	methods, ok := serviceMethods[service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	obj := vm.NewObject()
	for name, spec := range methods {
		m := spec
		mName := name
		err := obj.Set(mName, func(fnCall goja.FunctionCall) goja.Value {
			v, err := b.invoke(vm, service, mName, m, grants, fnCall.Arguments)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			// Every stub method is async hosted, so return a resolved promise and code
			// that awaits works identically in both places.
			p, resolveFn, _ := vm.NewPromise()
			_ = resolveFn(v)
			return vm.ToValue(p)
		})
		if err != nil {
			return nil, err
		}
	}
	return obj, nil
}

func (b *bindings) invoke(vm *goja.Runtime, service, name string, m call, grants map[string]string, args []goja.Value) (goja.Value, error) {
	if len(args) < m.minArgs {
		return nil, fmt.Errorf("env.%s.%s expects at least %d arguments", service, name, m.minArgs)
	}
	t, err := readTarget(args)
	if err != nil {
		return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
	}
	// THE blast-radius bound. Same ladder as an API key's grants.
	level := m.level
	if m.levelFor != nil {
		level = m.levelFor(args)
	}
	if err := auth.Require(&auth.Identity{Grants: auth.Grants(grants)}, service, t.instance, level); err != nil {
		return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
	}

	if m.custom != nil {
		v, err := m.custom(b, t, args)
		if err != nil {
			return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
		}
		return vm.ToValue(v), nil
	}

	path, err := m.path(t, args)
	if err != nil {
		return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
	}
	var payload []byte
	if m.body != nil {
		v, err := m.body(args)
		if err != nil {
			return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
		}
		if payload, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}

	req := httptest.NewRequest(m.method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, req)

	var decoded any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			return nil, fmt.Errorf("env.%s.%s: unreadable response", service, name)
		}
	}
	if rec.Code >= 400 {
		return nil, fmt.Errorf("env.%s.%s: %s", service, name, errMessage(decoded, rec.Code))
	}
	if m.unwrap != "" {
		if obj, ok := decoded.(map[string]any); ok {
			if v, ok := obj[m.unwrap]; ok {
				return vm.ToValue(v), nil
			}
		}
	}
	return vm.ToValue(decoded), nil
}

// do makes one in-process request and decodes it, so the custom methods below report errors
// exactly as invoke does. A non-JSON response comes back as raw bytes, which is how `bytes`
// gets a file rather than a parse failure.
func (b *bindings) do(method, path, contentType string, payload []byte) (any, error) {
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+b.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	b.mux.ServeHTTP(rec, req)

	var decoded any
	isJSON := strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json")
	if rec.Body.Len() > 0 && isJSON {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			return nil, fmt.Errorf("unreadable response")
		}
	}
	if rec.Code >= 400 {
		return nil, fmt.Errorf("%s", errMessage(decoded, rec.Code))
	}
	if !isJSON {
		return rec.Body.Bytes(), nil
	}
	return decoded, nil
}

// errMessage lifts the API's error message out, so a function sees the same text the
// REST caller would rather than a bare status.
func errMessage(decoded any, status int) string {
	if obj, ok := decoded.(map[string]any); ok {
		if e, ok := obj["error"].(map[string]any); ok {
			code, _ := e["code"].(string)
			msg, _ := e["message"].(string)
			if msg != "" {
				if code != "" {
					return code + ": " + msg
				}
				return msg
			}
		}
	}
	return fmt.Sprintf("request failed with status %d", status)
}

// readTarget reads the { instance, namespace } first argument every stub method takes.
func readTarget(args []goja.Value) (target, error) {
	if len(args) == 0 {
		return target{}, fmt.Errorf("target.instance is required")
	}
	obj, ok := args[0].(*goja.Object)
	if !ok {
		return target{}, fmt.Errorf("target.instance is required")
	}
	inst := ""
	if v := obj.Get("instance"); v != nil && !goja.IsUndefined(v) {
		inst = v.String()
	}
	if inst == "" {
		return target{}, fmt.Errorf("target.instance is required")
	}
	ns := ""
	if v := obj.Get("namespace"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		ns = v.String()
	}
	// `_default` is only the URL spelling of the empty namespace; accept it so someone
	// copying it out of a REST path is not told it is reserved.
	if ns == "_default" {
		ns = ""
	}
	return target{instance: inst, namespace: ns}, nil
}

func arg(args []goja.Value, i int) goja.Value {
	if i < len(args) {
		return args[i]
	}
	return goja.Undefined()
}

func argStr(args []goja.Value, i int) string {
	v := arg(args, i)
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	return v.String()
}

func argAny(args []goja.Value, i int) any {
	v := arg(args, i)
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v.Export()
}

// nsSegment spells the namespace the way the URL does.
func nsSegment(ns string) string {
	if ns == "" {
		return "_default"
	}
	return url.PathEscape(ns)
}

// readBytes turns whatever user code passed into bytes. Strings, ArrayBuffers and typed arrays
// all cross the hosted RPC boundary, so all three have to work here or a function that stores an
// image locally would need different code than the one that stores it in production.
func readBytes(v goja.Value) ([]byte, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, fmt.Errorf("content is required")
	}
	switch x := v.Export().(type) {
	case string:
		return []byte(x), nil
	case []byte:
		return x, nil
	case goja.ArrayBuffer:
		return x.Bytes(), nil
	}
	return nil, fmt.Errorf("content must be a string, an ArrayBuffer, or a typed array")
}

// optField reads one field out of an options OBJECT argument, or nil when the argument or the
// field is absent. The stubs take named options rather than positional parameters (hosted does
// the same) so a later addition cannot shift an existing caller's arguments.
func optField(args []goja.Value, i int, field string) any {
	m, ok := argAny(args, i).(map[string]any)
	if !ok {
		return nil
	}
	v, ok := m[field]
	if !ok {
		return nil
	}
	return v
}

func esc(s string) string { return url.PathEscape(s) }

// serviceMethods maps every stub method onto its REST equivalent. The signatures match
// src/functions/api/*.ts exactly — a function written against the hosted stubs runs here
// unchanged, which is the entire point.
var serviceMethods = map[string]map[string]call{
	"datastore": {
		"put": {
			method: "POST", level: auth.Write, minArgs: 3, unwrap: "",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"documents": argAny(a, 2)}, nil },
		},
		"get": {
			method: "POST", level: auth.Read, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents/get", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"keys": argAny(a, 2)}, nil },
		},
		// `full`, matching the hosted stub. This was `write` and let a function delete
		// locally that would 403 in production — the works-here-fails-there divergence the
		// emulator exists to prevent.
		"delete": {
			method: "POST", level: auth.Full, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents/delete", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"keys": argAny(a, 2)}, nil },
		},
		"query": {
			method: "POST", level: auth.Read, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/query", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return argAny(a, 2), nil },
		},
		"aggregate": {
			method: "POST", level: auth.Read, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/aggregate", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return argAny(a, 2), nil },
		},
		// The headline capability: a cross-collection transaction is exactly what an
		// end-user identity token structurally cannot do, and the reason this service
		// exists at all.
		"transaction": {
			method: "POST", level: auth.Write, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/transaction", esc(t.instance), nsSegment(t.namespace)), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"operations": argAny(a, 1)}, nil },
		},
		"listIndexes": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/indexes", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
		},
		// `write`, matching the hosted stub — this was `full`, so a function correctly
		// granted write could not create an index locally though production allows it.
		"createIndex": {
			method: "POST", level: auth.Write, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/indexes", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) {
				unique := false
				if v := arg(a, 3); !goja.IsUndefined(v) {
					unique = v.ToBoolean()
				}
				return map[string]any{"fields": argAny(a, 2), "unique": unique}, nil
			},
		},
		// `full`, not the `write` that creates one: dropping an index un-serves every query
		// that relied on it, which the structural guard then rejects.
		"deleteIndex": {
			method: "DELETE", level: auth.Full, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/indexes/%v", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1)), esc(argStr(a, 2))), nil
			},
		},
	},
	"search": {
		"index": {
			method: "POST", level: auth.Write, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"documents": argAny(a, 2)}, nil },
		},
		"get": {
			method: "POST", level: auth.Read, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents/get", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"ids": argAny(a, 2)}, nil },
		},
		// `full`, matching the hosted stub — same fix as datastore.delete above.
		"delete": {
			method: "POST", level: auth.Full, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents/delete", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"ids": argAny(a, 2)}, nil },
		},
		"search": {
			method: "POST", level: auth.Read, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/search", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return argAny(a, 2), nil },
		},
		"schema": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/schema", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
		},
		"listIndexes": {
			method: "GET", level: auth.Read, minArgs: 1,
			path: func(t target, a []goja.Value) (string, error) {
				q := url.Values{}
				if v := optField(a, 1, "q"); v != nil {
					q.Set("q", fmt.Sprint(v))
				}
				if v := optField(a, 1, "limit"); v != nil {
					q.Set("limit", fmt.Sprintf("%v", v))
				}
				p := fmt.Sprintf("/v1/search/%s/ns/%s/idx", esc(t.instance), nsSegment(t.namespace))
				if len(q) > 0 {
					p += "?" + q.Encode()
				}
				return p, nil
			},
		},
		// `full`, not write: this destroys the whole index, not documents in it.
		"deleteIndex": {
			method: "DELETE", level: auth.Full, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s", esc(t.instance), nsSegment(t.namespace), esc(argStr(a, 1))), nil
			},
		},
	},
	"channel": {
		"publish": {
			method: "POST", level: auth.Write, minArgs: 3, unwrap: "",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/channel/%s/publish", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) {
				return map[string]any{"channel": argStr(a, 1), "data": argAny(a, 2)}, nil
			},
		},
		// Mint a subscriber token. The options object is camelCase because that is what the
		// hosted stub takes; the REST body is snake_case. Translating here rather than
		// accepting both is deliberate — a function that works locally with `ttl_seconds`
		// would silently fall back to the default TTL in production, where the field is
		// simply not read.
		"token": {
			method: "POST", level: auth.Read, minArgs: 1,
			levelFor: func(a []goja.Value) auth.Level {
				if pub := optField(a, 1, "publish"); pub != nil && pub != false {
					return auth.Write
				}
				return auth.Read
			},
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/channel/%s/tokens", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) {
				out := map[string]any{}
				for stub, rest := range map[string]string{
					"channels": "channels", "ttlSeconds": "ttl_seconds",
					"publish": "publish", "presenceId": "presence_id",
				} {
					if v := optField(a, 1, stub); v != nil {
						out[rest] = v
					}
				}
				return out, nil
			},
		},
		"presence": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/channel/%s/presence?channel=%s", esc(t.instance), url.QueryEscape(argStr(a, 1))), nil
			},
		},
	},
	// Blob. `put` and `bytes` are the only methods here that are not one REST call: hosted they
	// reach storage directly, and there is no public endpoint for either. Rather than invent one
	// — a route that would work locally and 404 in production — they are composed from the calls
	// a REST client would actually make.
	"blob": {
		"put": {
			level: auth.Write, minArgs: 3,
			custom: func(b *bindings, t target, a []goja.Value) (any, error) {
				content, err := readBytes(arg(a, 2))
				if err != nil {
					return nil, err
				}
				opts, _ := argAny(a, 3).(map[string]any)
				mint := map[string]any{"name": argStr(a, 1), "size": len(content)}
				if v, ok := opts["contentType"]; ok {
					mint["content_type"] = v
				}
				for from, to := range map[string]string{"public": "public", "meta": "meta"} {
					if v, ok := opts[from]; ok {
						mint[to] = v
					}
				}
				payload, _ := json.Marshal(mint)
				res, err := b.do("POST", fmt.Sprintf("/v1/blob/%s/uploads", esc(t.instance)), "application/json", payload)
				if err != nil {
					return nil, err
				}
				minted, _ := res.(map[string]any)
				uploadURL, _ := minted["upload_url"].(string)
				headers, _ := minted["required_headers"].(map[string]any)
				ct, _ := headers["content-type"].(string)
				u, err := url.Parse(uploadURL)
				if err != nil || uploadURL == "" {
					return nil, fmt.Errorf("the upload URL was unreadable")
				}
				if _, err := b.do("PUT", u.RequestURI(), ct, content); err != nil {
					return nil, err
				}
				// Read it back, so the caller gets the finished record with its public URL —
				// the same return value the hosted stub gives.
				got, err := b.do("GET", fmt.Sprintf("/v1/blob/%s/%s", esc(t.instance), esc(fmt.Sprint(minted["id"]))), "", nil)
				if err != nil {
					return nil, err
				}
				if obj, ok := got.(map[string]any); ok {
					return obj["blob"], nil
				}
				return got, nil
			},
		},
		"bytes": {
			level: auth.Read, minArgs: 2,
			custom: func(b *bindings, t target, a []goja.Value) (any, error) {
				got, err := b.do("GET", fmt.Sprintf("/v1/blob/%s/%s", esc(t.instance), esc(argStr(a, 1))), "", nil)
				if err != nil {
					return nil, err
				}
				obj, _ := got.(map[string]any)
				dl, _ := obj["download_url"].(string)
				u, err := url.Parse(dl)
				if err != nil || dl == "" {
					return nil, fmt.Errorf("this blob has no download URL")
				}
				raw, err := b.do("GET", u.RequestURI(), "", nil)
				if err != nil {
					return nil, err
				}
				body, _ := raw.([]byte)
				return body, nil
			},
		},
		"uploadUrl": {
			method: "POST", level: auth.Write, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/blob/%s/uploads", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) {
				o, _ := argAny(a, 1).(map[string]any)
				out := map[string]any{}
				for from, to := range map[string]string{"name": "name", "size": "size", "contentType": "content_type", "public": "public", "meta": "meta"} {
					if v, ok := o[from]; ok {
						out[to] = v
					}
				}
				return out, nil
			},
		},
		"get": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/blob/%s/%s", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
		"list": {
			method: "GET", level: auth.Read, minArgs: 1,
			path: func(t target, a []goja.Value) (string, error) {
				o, _ := argAny(a, 1).(map[string]any)
				q := url.Values{}
				for from, to := range map[string]string{"prefix": "prefix", "limit": "limit", "cursor": "cursor"} {
					if v, ok := o[from]; ok && v != nil {
						q.Set(to, fmt.Sprint(v))
					}
				}
				return fmt.Sprintf("/v1/blob/%s?%s", esc(t.instance), q.Encode()), nil
			},
		},
		"setPublic": {
			method: "POST", level: auth.Write, minArgs: 2, unwrap: "blob",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/blob/%s/%s/public", esc(t.instance), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) {
				pub := true
				if v := arg(a, 2); !goja.IsUndefined(v) && !goja.IsNull(v) {
					pub = v.ToBoolean()
				}
				return map[string]any{"public": pub}, nil
			},
		},
		"delete": {
			method: "POST", level: auth.Full, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/blob/%s/delete", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"ids": argAny(a, 1)}, nil },
		},
	},
	// Containers. `run` returns as soon as the container starts — there is nothing to await,
	// which is the same shape hosted and the reason a completion function exists at all.
	"container": {
		"run": {
			method: "POST", level: auth.Write, minArgs: 2, unwrap: "job",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/container/%s", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) { return argAny(a, 1), nil },
		},
		"get": {
			method: "GET", level: auth.Read, minArgs: 2, unwrap: "job",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/container/%s/%s", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
		"list": {
			method: "GET", level: auth.Read, minArgs: 1,
			path: func(t target, a []goja.Value) (string, error) {
				o, _ := argAny(a, 1).(map[string]any)
				q := url.Values{}
				for from, to := range map[string]string{"status": "status", "limit": "limit", "before": "before"} {
					if v, ok := o[from]; ok && v != nil {
						q.Set(to, fmt.Sprint(v))
					}
				}
				return fmt.Sprintf("/v1/container/%s?%s", esc(t.instance), q.Encode()), nil
			},
		},
		"cancel": {
			method: "POST", level: auth.Write, minArgs: 2, unwrap: "job",
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/container/%s/%s/cancel", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
		"logs": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				q := url.Values{}
				if c := argStr(a, 2); c != "" {
					q.Set("cursor", c)
				}
				return fmt.Sprintf("/v1/container/%s/%s/logs?%s", esc(t.instance), esc(argStr(a, 1)), q.Encode()), nil
			},
		},
	},
	"auth": {
		"verifyToken": {
			method: "POST", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/v1/auth/%s/token/verify", esc(t.instance)), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"token": argStr(a, 1)}, nil },
		},
		"getUser": {
			method: "GET", level: auth.Read, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
		"listUsers": {
			method: "GET", level: auth.Read, minArgs: 1,
			path: func(t target, a []goja.Value) (string, error) {
				q := url.Values{}
				if o, ok := arg(a, 1).(*goja.Object); ok {
					if v := o.Get("q"); v != nil && !goja.IsUndefined(v) {
						q.Set("q", v.String())
					}
					if v := o.Get("limit"); v != nil && !goja.IsUndefined(v) {
						q.Set("limit", v.String())
					}
				}
				p := fmt.Sprintf("/admin/auth/%s/users", esc(t.instance))
				if len(q) > 0 {
					p += "?" + q.Encode()
				}
				return p, nil
			},
		},
		"setClaims": {
			method: "POST", level: auth.Write, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s/claims", esc(t.instance), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"claims": argAny(a, 2)}, nil },
		},
		"setProfile": {
			method: "POST", level: auth.Write, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s/profile", esc(t.instance), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) { return map[string]any{"profile": argAny(a, 2)}, nil },
		},
		"setDisabled": {
			method: "POST", level: auth.Write, minArgs: 3,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s/disabled", esc(t.instance), esc(argStr(a, 1))), nil
			},
			body: func(a []goja.Value) (any, error) {
				return map[string]any{"disabled": arg(a, 2).ToBoolean()}, nil
			},
		},
		"deleteUser": {
			method: "DELETE", level: auth.Full, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
		"revokeSessions": {
			method: "POST", level: auth.Write, minArgs: 2,
			path: func(t target, a []goja.Value) (string, error) {
				return fmt.Sprintf("/admin/auth/%s/users/%s/revoke", esc(t.instance), esc(argStr(a, 1))), nil
			},
		},
	},
}

var _ = strings.ToLower
