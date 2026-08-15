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
	path    func(t target, args []goja.Value) (string, error)
	body    func(args []goja.Value) (any, error)
	unwrap  string // field to lift out of the response, "" to return the whole object
	minArgs int
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
	if err := auth.Require(&auth.Identity{Grants: auth.Grants(grants)}, service, t.instance, m.level); err != nil {
		return nil, fmt.Errorf("env.%s.%s: %w", service, name, err)
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
		"delete": {
			method: "POST", level: auth.Write, minArgs: 3,
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
		"createIndex": {
			method: "POST", level: auth.Full, minArgs: 3,
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
		"delete": {
			method: "POST", level: auth.Write, minArgs: 3,
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
