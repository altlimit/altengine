// The globals a function sees.
//
// Written as JavaScript rather than Go host objects on purpose: Request/Response/Headers
// are almost entirely data shuffling, and a JS implementation is far easier to keep
// honest against the real thing than a pile of reflected Go structs. Only the parts that
// must reach outside — fetch, console, crypto, the service bindings — are Go.
//
// This is a SUBSET, and the omissions are deliberate: no streaming bodies, no WebSocket
// upgrade, no Cache API. A function relying on those needs a real deploy to test.

package functions

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dop251/goja"
	"github.com/google/uuid"
)

// The shim. Bodies are strings here: a local emulator handling multi-megabyte streaming
// bodies is not what anyone is testing, and buffering keeps the semantics obvious.
const shimJS = `
class Headers {
  constructor(init) {
    this.__map = {};
    if (init) {
      if (init instanceof Headers) Object.assign(this.__map, init.__map);
      else if (Array.isArray(init)) for (const [k, v] of init) this.append(k, v);
      else for (const k of Object.keys(init)) this.set(k, init[k]);
    }
  }
  get(k) { const v = this.__map[String(k).toLowerCase()]; return v === undefined ? null : v; }
  set(k, v) { this.__map[String(k).toLowerCase()] = String(v); }
  has(k) { return Object.prototype.hasOwnProperty.call(this.__map, String(k).toLowerCase()); }
  delete(k) { delete this.__map[String(k).toLowerCase()]; }
  append(k, v) {
    const key = String(k).toLowerCase();
    this.__map[key] = this.has(key) ? this.__map[key] + ", " + v : String(v);
  }
  forEach(fn) { for (const k of Object.keys(this.__map)) fn(this.__map[k], k, this); }
  keys() { return Object.keys(this.__map)[Symbol.iterator](); }
  entries() { return Object.keys(this.__map).map((k) => [k, this.__map[k]])[Symbol.iterator](); }
  [Symbol.iterator]() { return this.entries(); }
}

class Body {
  constructor(body) { this.__body = body === undefined || body === null ? null : String(body); }
  get bodyUsed() { return false; }
  async text() { return this.__body === null ? "" : this.__body; }
  async json() {
    const t = await this.text();
    // Match the platform: an empty or malformed body rejects rather than yielding null,
    // so a handler that forgot to send JSON fails where the mistake is.
    if (t === "") throw new SyntaxError("Unexpected end of JSON input");
    return JSON.parse(t);
  }
  async arrayBuffer() { throw new Error("arrayBuffer() is not supported by the emulator"); }
  async formData() { throw new Error("formData() is not supported by the emulator"); }
}

class Request extends Body {
  constructor(input, init) {
    init = init || {};
    let url = input, base = {};
    if (input && typeof input === "object" && input.__isRequest) { url = input.url; base = input; }
    super(init.body !== undefined ? init.body : base.__body);
    this.__isRequest = true;
    this.url = String(url);
    this.method = (init.method || base.method || "GET").toUpperCase();
    this.headers = new Headers(init.headers || base.headers);
    this.redirect = init.redirect || base.redirect || "follow";
  }
  clone() { return new Request(this, {}); }
}

class Response extends Body {
  constructor(body, init) {
    super(body);
    init = init || {};
    this.__isResponse = true;
    this.status = init.status === undefined ? 200 : init.status;
    this.statusText = init.statusText || "";
    this.headers = new Headers(init.headers);
    this.ok = this.status >= 200 && this.status < 300;
  }
  static json(data, init) {
    const r = new Response(JSON.stringify(data), init);
    if (!r.headers.has("content-type")) r.headers.set("content-type", "application/json");
    return r;
  }
  static redirect(url, status) {
    const r = new Response(null, { status: status || 302 });
    r.headers.set("location", String(url));
    return r;
  }
  clone() { const r = new Response(this.__body, { status: this.status, statusText: this.statusText }); r.headers = new Headers(this.headers); return r; }
}

globalThis.Headers = Headers;
globalThis.Request = Request;
globalThis.Response = Response;
`

// installGlobals wires the shim plus the Go-backed globals into a fresh runtime.
func installGlobals(vm *goja.Runtime, in *invocation) error {
	if _, err := vm.RunString(shimJS); err != nil {
		return fmt.Errorf("emulator shim failed: %w", err)
	}
	if err := installURL(vm); err != nil {
		return err
	}
	installConsole(vm, in)
	installCrypto(vm)
	if err := installFetch(vm, in); err != nil {
		return err
	}
	// atob/btoa: present in Workers, trivially useful, and their absence is a confusing
	// failure in code that only touches them on one branch.
	if _, err := vm.RunString(`
    globalThis.btoa = (s) => __b64encode(String(s));
    globalThis.atob = (s) => __b64decode(String(s));
  `); err != nil {
		return err
	}
	return nil
}

// installURL provides URL and URLSearchParams by delegating parsing to Go, so the emulator
// agrees with the real parser on the cases that actually bite (encoding, relative refs).
func installURL(vm *goja.Runtime) error {
	_ = vm.Set("__parseURL", func(raw, base string) (map[string]any, error) {
		u, err := parseURL(raw, base)
		if err != nil {
			return nil, err
		}
		return u, nil
	})
	_, err := vm.RunString(`
class URLSearchParams {
  constructor(init) {
    this.__pairs = [];
    if (typeof init === "string") {
      for (const part of init.replace(/^\?/, "").split("&")) {
        if (!part) continue;
        const i = part.indexOf("=");
        const k = i < 0 ? part : part.slice(0, i);
        const v = i < 0 ? "" : part.slice(i + 1);
        this.__pairs.push([decodeURIComponent(k.replace(/\+/g, " ")), decodeURIComponent(v.replace(/\+/g, " "))]);
      }
    } else if (init && typeof init === "object") {
      for (const k of Object.keys(init)) this.__pairs.push([k, String(init[k])]);
    }
  }
  get(k) { const p = this.__pairs.find((p) => p[0] === k); return p ? p[1] : null; }
  getAll(k) { return this.__pairs.filter((p) => p[0] === k).map((p) => p[1]); }
  has(k) { return this.__pairs.some((p) => p[0] === k); }
  set(k, v) {
    const i = this.__pairs.findIndex((p) => p[0] === k);
    if (i < 0) this.__pairs.push([k, String(v)]);
    else { this.__pairs[i] = [k, String(v)]; this.__pairs = this.__pairs.filter((p, j) => p[0] !== k || j <= i); }
  }
  append(k, v) { this.__pairs.push([k, String(v)]); }
  delete(k) { this.__pairs = this.__pairs.filter((p) => p[0] !== k); }
  forEach(fn) { for (const [k, v] of this.__pairs) fn(v, k, this); }
  keys() { return this.__pairs.map((p) => p[0])[Symbol.iterator](); }
  values() { return this.__pairs.map((p) => p[1])[Symbol.iterator](); }
  entries() { return this.__pairs.map((p) => [p[0], p[1]])[Symbol.iterator](); }
  [Symbol.iterator]() { return this.entries(); }
  toString() {
    return this.__pairs.map(([k, v]) => encodeURIComponent(k) + "=" + encodeURIComponent(v)).join("&");
  }
}

class URL {
  constructor(input, base) {
    const p = __parseURL(String(input), base === undefined ? "" : String(base));
    this.protocol = p.protocol; this.hostname = p.hostname; this.port = p.port;
    this.pathname = p.pathname; this.hash = p.hash; this.host = p.host;
    this.origin = p.origin; this.username = p.username; this.password = p.password;
    this.searchParams = new URLSearchParams(p.search);
  }
  get search() { const s = this.searchParams.toString(); return s ? "?" + s : ""; }
  set search(v) { this.searchParams = new URLSearchParams(String(v)); }
  toString() {
    const port = this.port ? ":" + this.port : "";
    return this.protocol + "//" + this.hostname + port + this.pathname + this.search + this.hash;
  }
  get href() { return this.toString(); }
}
globalThis.URL = URL;
globalThis.URLSearchParams = URLSearchParams;
`)
	return err
}

// installConsole routes console output to the emulator's log.
//
// This is the local equivalent of the hosted live tail — hosted, console output is
// streamed to the console UI only while someone is watching and is never stored. Here it
// goes to the terminal you are already looking at.
func installConsole(vm *goja.Runtime, in *invocation) {
	console := vm.NewObject()
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		lvl := level
		_ = console.Set(lvl, func(call goja.FunctionCall) goja.Value {
			in.logf(lvl, logLine(vm, call.Arguments))
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)
}

func installCrypto(vm *goja.Runtime) {
	c := vm.NewObject()
	_ = c.Set("randomUUID", func() string { return uuid.NewString() })
	_ = vm.Set("crypto", c)
	_ = vm.Set("__b64encode", b64encode)
	_ = vm.Set("__b64decode", b64decode)
}

// installFetch provides outbound HTTP under the SAME rules as the hosted proxy.
//
// Both halves matter for parity. A function that fetches a host it never allowlisted must
// fail HERE, or the first thing production does is refuse a call that worked all through
// development. And `{{SECRET}}` expansion must happen for egress-exposure secrets, since
// that is the only way such a secret can be used at all.
func installFetch(vm *goja.Runtime, in *invocation) error {
	allowed := in.cfg.AllowedHosts
	secrets := in.cfg.EgressSecrets()

	_ = vm.Set("__fetch", func(call goja.FunctionCall) goja.Value {
		raw := call.Argument(0)
		url := ""
		var initObj *goja.Object
		if o, ok := raw.(*goja.Object); ok && o.Get("__isRequest") != nil && o.Get("__isRequest").ToBoolean() {
			url = o.Get("url").String()
		} else {
			url = raw.String()
		}
		if o, ok := call.Argument(1).(*goja.Object); ok {
			initObj = o
		}

		method := "GET"
		header := http.Header{}
		var body string
		if initObj != nil {
			if m := initObj.Get("method"); m != nil && !goja.IsUndefined(m) {
				method = strings.ToUpper(m.String())
			}
			if h := initObj.Get("headers"); h != nil && !goja.IsUndefined(h) {
				if ho, ok := h.(*goja.Object); ok {
					src := ho
					if m := ho.Get("__map"); m != nil && !goja.IsUndefined(m) {
						if mo, ok := m.(*goja.Object); ok {
							src = mo
						}
					}
					for _, k := range src.Keys() {
						header.Set(k, src.Get(k).String())
					}
				}
			}
			if b := initObj.Get("body"); b != nil && !goja.IsUndefined(b) && !goja.IsNull(b) {
				body = b.String()
			}
		}

		status, respHeader, respBody, err := doEgress(url, method, header, []byte(body), allowed, secrets)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		hdr := map[string]any{}
		for k := range respHeader {
			hdr[strings.ToLower(k)] = respHeader.Get(k)
		}
		return vm.ToValue(map[string]any{"status": status, "headers": hdr, "body": string(respBody)})
	})

	_, err := vm.RunString(`
globalThis.fetch = async function (input, init) {
  const raw = __fetch(input, init);
  const r = new Response(raw.body, { status: raw.status });
  for (const k of Object.keys(raw.headers)) r.headers.set(k, raw.headers[k]);
  return r;
};
`)
	return err
}

// newRequestObject builds the Request handed to the function.
func newRequestObject(vm *goja.Runtime, r *http.Request, body []byte) (goja.Value, error) {
	ctor, ok := goja.AssertConstructor(vm.Get("Request"))
	if !ok {
		return nil, errors.New("emulator shim did not define Request")
	}
	headers := map[string]any{}
	for k := range r.Header {
		headers[strings.ToLower(k)] = r.Header.Get(k)
	}
	init := map[string]any{"method": r.Method, "headers": headers}
	if len(body) > 0 {
		init["body"] = string(body)
	}
	obj, err := ctor(nil, vm.ToValue(publicURL(r)), vm.ToValue(init))
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// publicURL reconstructs the absolute URL a function sees.
//
// Hosted this is https://{slug}.altengine.app/...; locally it is the emulator's own
// origin with the /fn/{instance} prefix intact. Deliberately NOT rewritten to look like
// the hosted URL: a function that builds links from request.url should produce URLs that
// actually work in the environment it is running in.
func publicURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

// buildEnv assembles the sandbox's `env`: secrets first, then a stub per GRANTED service.
//
// An ungranted service is ABSENT, not merely guarded — same as hosted, so a missing grant
// shows up as `env.datastore is undefined` here exactly as it would there, rather than as
// a permission error only production produces.
func buildEnv(vm *goja.Runtime, in *invocation) (goja.Value, error) {
	env := vm.NewObject()
	for k, v := range in.cfg.EnvSecrets() {
		_ = env.Set(k, v)
	}
	for _, svc := range []string{"datastore", "search", "auth", "channel"} {
		if !hasGrant(in.fn.Grants, svc) {
			continue
		}
		stub, err := in.bindings.stub(vm, svc, in.fn.Grants)
		if err != nil {
			return nil, err
		}
		_ = env.Set(svc, stub)
	}
	return env, nil
}

func hasGrant(grants map[string]string, service string) bool {
	for k := range grants {
		if k == service || strings.HasPrefix(k, service+":") {
			return true
		}
	}
	return false
}

// jsonRoundTrip normalizes a Go value through JSON so what reaches JS matches what the
// HTTP API would have returned — same field names, same nesting, no Go-typed surprises.
func jsonRoundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
