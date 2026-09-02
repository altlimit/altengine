package functions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
)

// newTestServer builds an emulator with the datastore and functions planes wired the same
// way the real server does — including the in-process dispatch that backs env.datastore.
func newTestServer(t *testing.T) *http.ServeMux {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	datastore.NewHandler(reg, a, datastore.NewManager("")).Register(mux)
	NewHandler(reg, a, NewStore(""), mux).Register(mux)
	return mux
}

func deployFn(t *testing.T, mux *http.ServeMux, name, code string, grants map[string]string) {
	t.Helper()
	body, _ := json.Marshal(DeployRequest{Name: name, Code: code, Grants: grants})
	req := httptest.NewRequest("POST", "/v1/functions/main/deploy", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("deploy failed: %d %s", rec.Code, rec.Body.String())
	}
}

func invoke(t *testing.T, mux *http.ServeMux, path string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestInvokeReturnsResponse(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "hello", `export default { async fetch(request) {
		return new Response("hi " + new URL(request.url).pathname);
	} };`, nil)

	rec := invoke(t, mux, "/fn/main/hello/world")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hi /fn/main/hello/world" {
		t.Fatalf("body = %q", got)
	}
}

func TestResponseJSONAndHeaders(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "j", `export default { fetch() {
		return Response.json({ ok: true, n: 2 }, { status: 201, headers: { "x-custom": "v" } });
	} };`, nil)

	rec := invoke(t, mux, "/fn/main/j")
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("x-custom") != "v" {
		t.Fatalf("headers = %v", rec.Header())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["n"].(float64) != 2 {
		t.Fatalf("body = %v", out)
	}
}

// The request body and method have to survive the boundary — this is the most common
// thing a real function does.
func TestRequestBodyAndMethod(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "echo", `export default { async fetch(request) {
		const body = await request.json();
		return Response.json({ method: request.method, got: body.name, ct: request.headers.get("content-type") });
	} };`, nil)

	req := httptest.NewRequest("POST", "/fn/main/echo", strings.NewReader(`{"name":"ada"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["method"] != "POST" || out["got"] != "ada" || out["ct"] != "application/json" {
		t.Fatalf("out = %v (%s)", out, rec.Body.String())
	}
}

// The whole reason the service exists: a cross-collection transaction, which an end-user
// identity token structurally cannot perform.
func TestDatastoreTransactionThroughStub(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "invite", `export default { async fetch(request, env) {
		await env.datastore.transaction({ instance: "maindb" }, [
			{ op: "put", collection: "members", key: "m1", data: { uid: "u1" } },
			{ op: "put", collection: "invites", key: "i1", data: { used_by: "u1" } },
		]);
		const got = await env.datastore.get({ instance: "maindb" }, "members", ["m1"]);
		return Response.json({ uid: got.documents[0].data.uid });
	} };`, map[string]string{"datastore:maindb": "write"})

	rec := invoke(t, mux, "/fn/main/invite")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["uid"] != "u1" {
		t.Fatalf("out = %v", out)
	}
}

// Grants are the blast-radius bound, and locally they are the ONLY thing between two
// instances — the emulator's auth store is dev-open, so an unchecked dispatch would be
// allowed anything.
func TestGrantsBoundWhichInstanceIsReachable(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "wrongdb", `export default { async fetch(request, env) {
		try {
			await env.datastore.get({ instance: "otherdb" }, "notes", ["k"]);
			return new Response("REACHED-UNGRANTED-INSTANCE", { status: 500 });
		} catch (e) { return new Response("denied: " + e.message); }
	} };`, map[string]string{"datastore:maindb": "full"})

	rec := invoke(t, mux, "/fn/main/wrongdb")
	body := rec.Body.String()
	if strings.Contains(body, "REACHED-UNGRANTED-INSTANCE") {
		t.Fatalf("a function reached an instance it was not granted: %s", body)
	}
	if !strings.Contains(body, "denied:") {
		t.Fatalf("body = %q", body)
	}
}

func TestGrantLevelLadder(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "readonly", `export default { async fetch(request, env) {
		try {
			await env.datastore.put({ instance: "maindb" }, "notes", [{ key: "x", data: {} }]);
			return new Response("WROTE-WITH-READ-GRANT", { status: 500 });
		} catch (e) { return new Response("denied: " + e.message); }
	} };`, map[string]string{"datastore:maindb": "read"})

	body := invoke(t, mux, "/fn/main/readonly").Body.String()
	if strings.Contains(body, "WROTE-WITH-READ-GRANT") {
		t.Fatalf("a read grant wrote: %s", body)
	}
}

// An ungranted service is ABSENT from env, not merely guarded — so a missing grant looks
// the same locally as it does hosted.
func TestUngrantedServiceIsAbsentFromEnv(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "nogrants", `export default { fetch(request, env) {
		return Response.json({ keys: Object.keys(env).sort() });
	} };`, nil)

	var out struct{ Keys []string }
	_ = json.Unmarshal(invoke(t, mux, "/fn/main/nogrants").Body.Bytes(), &out)
	if len(out.Keys) != 0 {
		t.Fatalf("expected no bindings, got %v", out.Keys)
	}
}

func TestSecretExposureSplit(t *testing.T) {
	mux := newTestServer(t)
	body, _ := json.Marshal(map[string]any{"secrets": map[string]any{
		"IN_ENV":    "visible",
		"VIA_PROXY": map[string]any{"value": "hidden-value", "egress": true},
	}})
	req := httptest.NewRequest("PUT", "/v1/functions/main/secrets", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("set secrets: %d %s", rec.Code, rec.Body.String())
	}
	// The list returns names and exposure, never values.
	if strings.Contains(rec.Body.String(), "hidden-value") || strings.Contains(rec.Body.String(), "visible") {
		t.Fatalf("secret VALUES leaked from the list endpoint: %s", rec.Body.String())
	}

	deployFn(t, mux, "peek", `export default { fetch(request, env) {
		return Response.json({ inEnv: env.IN_ENV ?? null, viaProxy: env.VIA_PROXY ?? null });
	} };`, nil)

	var out map[string]any
	_ = json.Unmarshal(invoke(t, mux, "/fn/main/peek").Body.Bytes(), &out)
	if out["inEnv"] != "visible" {
		t.Fatalf("env secret missing: %v", out)
	}
	// An egress-exposure secret must never enter the sandbox — that is the entire
	// difference between the two exposures.
	if out["viaProxy"] != nil {
		t.Fatalf("egress secret leaked into env: %v", out)
	}
}

func TestFetchRefusedWithoutAllowlist(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "caller", `export default { async fetch(request, env) {
		try {
			await fetch("https://example.com/");
			return new Response("REACHED", { status: 500 });
		} catch (e) { return new Response("blocked: " + e.message); }
	} };`, nil)

	body := invoke(t, mux, "/fn/main/caller").Body.String()
	if strings.Contains(body, "REACHED") {
		t.Fatalf("egress was not blocked: %s", body)
	}
	if !strings.Contains(body, "blocked:") {
		t.Fatalf("body = %q", body)
	}
}

func TestVersionsAndRollback(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "v", `export default { fetch() { return new Response("one"); } };`, nil)
	deployFn(t, mux, "v", `export default { fetch() { return new Response("two"); } };`, nil)
	if got := invoke(t, mux, "/fn/main/v").Body.String(); got != "two" {
		t.Fatalf("expected the newest version, got %q", got)
	}

	body, _ := json.Marshal(map[string]int{"version": 1})
	req := httptest.NewRequest("POST", "/v1/functions/main/v/activate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	// The compile cache must not keep serving the rolled-back program.
	if got := invoke(t, mux, "/fn/main/v").Body.String(); got != "one" {
		t.Fatalf("rollback did not take effect, got %q", got)
	}
}

// An omitted field must INHERIT. Resetting grants on a code-only redeploy is the bug that
// silently strips a function's capabilities.
func TestRedeployInheritsGrants(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "g", `export default { fetch(r, env) { return Response.json({ keys: Object.keys(env) }); } };`,
		map[string]string{"datastore:maindb": "write"})
	deployFn(t, mux, "g", `export default { fetch(r, env) { return Response.json({ keys: Object.keys(env) }); } };`, nil)

	var out struct{ Keys []string }
	_ = json.Unmarshal(invoke(t, mux, "/fn/main/g").Body.Bytes(), &out)
	if len(out.Keys) != 1 || out.Keys[0] != "datastore" {
		t.Fatalf("grants were not inherited on redeploy: %v", out.Keys)
	}
}

func TestBadExportIsAClearError(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "bad", `export const notDefault = 1;`, nil)
	body := invoke(t, mux, "/fn/main/bad").Body.String()
	if !strings.Contains(body, "default export") {
		t.Fatalf("expected an error naming the required shape, got %q", body)
	}
}

func TestThrownErrorSurfaces(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "boom", `export default { fetch() { throw new Error("kaboom"); } };`, nil)
	rec := invoke(t, mux, "/fn/main/boom")
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestInboundAEHeadersAreStripped(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "hdr", `export default { fetch(request) {
		return Response.json({ trigger: request.headers.get("x-ae-trigger"), fn: request.headers.get("x-ae-fn") });
	} };`, nil)

	rec := invoke(t, mux, "/fn/main/hdr", func(r *http.Request) {
		r.Header.Set("x-ae-trigger", "forged")
		r.Header.Set("x-ae-fn", "someone-else")
	})
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["trigger"] != "http" || out["fn"] != "hdr" {
		t.Fatalf("forged x-ae-* headers survived: %v", out)
	}
}

func TestCORSPreflightAndOrigins(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "c", `export default { fetch() { return new Response("ok"); } };`, nil)

	// Unconfigured: no CORS headers at all.
	rec := invoke(t, mux, "/fn/main/c", func(r *http.Request) { r.Header.Set("Origin", "https://app.example.com") })
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS headers sent without configuration")
	}

	body, _ := json.Marshal(map[string]any{"corsOrigins": []string{"https://app.example.com"}})
	req := httptest.NewRequest("PUT", "/v1/functions/main/settings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	rec = invoke(t, mux, "/fn/main/c", func(r *http.Request) { r.Header.Set("Origin", "https://app.example.com") })
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("listed origin not reflected: %v", rec.Header())
	}
	// Credentials are never allowed.
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("credentials allowed")
	}
	// An unlisted origin gets nothing.
	rec = invoke(t, mux, "/fn/main/c", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unlisted origin was allowed")
	}
}

func TestEgressChecks(t *testing.T) {
	allow := []string{"api.example.com", "*.stripe.com"}
	ok := func(u string) bool { return checkEgress(u, allow) == nil }

	if !ok("https://api.example.com/v1") {
		t.Fatal("listed host refused")
	}
	if ok("http://api.example.com/") {
		t.Fatal("plaintext http allowed")
	}
	if ok("https://evil.com/") {
		t.Fatal("unlisted host allowed")
	}
	if !ok("https://files.stripe.com/x") {
		t.Fatal("wildcard subdomain refused")
	}
	// A wildcard must not widen to the apex, nor match a lookalike registrable domain.
	if ok("https://stripe.com/") || ok("https://api.stripe.com.evil.net/") {
		t.Fatal("wildcard matched too broadly")
	}
	// One rule kills metadata, loopback and private ranges together.
	for _, h := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.5", "[::1]"} {
		if checkEgress("https://"+h+"/", []string{h}) == nil {
			t.Fatalf("IP literal %s allowed", h)
		}
	}
	for _, h := range []string{"localhost", "db.internal", "printer.local"} {
		if checkEgress("https://"+h+"/", []string{h}) == nil {
			t.Fatalf("non-public host %s allowed", h)
		}
	}
}

func TestExpandSecrets(t *testing.T) {
	secrets := map[string]string{"TOKEN": "t0ken-value-long"}
	h := http.Header{}
	h.Set("Authorization", "Bearer {{TOKEN}}")
	h.Set("X-Other", "{{UNKNOWN}}")

	u, touched, values := expandSecrets("https://api.example.com/x?key={{TOKEN}}&q=1", h, secrets)
	if h.Get("Authorization") != "Bearer t0ken-value-long" {
		t.Fatalf("header not expanded: %q", h.Get("Authorization"))
	}
	// An unknown placeholder is left alone rather than failing the request.
	if h.Get("X-Other") != "{{UNKNOWN}}" {
		t.Fatalf("unknown placeholder touched: %q", h.Get("X-Other"))
	}
	if !strings.Contains(u, "key=t0ken-value-long") || !strings.Contains(u, "q=1") {
		t.Fatalf("query not expanded: %s", u)
	}
	if len(touched) != 1 || touched[0] != "authorization" {
		t.Fatalf("touched = %v", touched)
	}
	if len(values) != 1 {
		t.Fatalf("values = %v", values)
	}

	// The host is never touched, so an expansion cannot move the request off the allowlist.
	h2 := http.Header{}
	u2, _, _ := expandSecrets("https://api.example.com/{{TOKEN}}", h2, secrets)
	if !strings.Contains(u2, "api.example.com") || strings.Contains(u2, "t0ken") {
		t.Fatalf("path or host was expanded: %s", u2)
	}

	// No placeholder anywhere: the URL must come back byte-identical, since re-encoding
	// would break a signature-sensitive API.
	raw := "https://api.example.com/x?b=2&a=1&sig=%2Fx%2By"
	if got, _, _ := expandSecrets(raw, http.Header{}, secrets); got != raw {
		t.Fatalf("URL rewritten with no placeholder:\n got %s\nwant %s", got, raw)
	}
}

func TestSecretKeepExisting(t *testing.T) {
	s := NewStore("")
	if _, err := s.SetSecrets("i1", raw(map[string]string{"A": `"keep-me"`})); err != nil {
		t.Fatal(err)
	}
	// Omitting the value keeps the stored one, so exposure can change without re-typing
	// every credential — values are write-only.
	if _, err := s.SetSecrets("i1", raw(map[string]string{"A": `{"egress":true}`})); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config("i1")
	if cfg.Secrets["A"].Value != "keep-me" || !cfg.Secrets["A"].Egress {
		t.Fatalf("secret = %+v", cfg.Secrets["A"])
	}
	// A keep-existing entry for an unknown name is an error, not an empty secret.
	if _, err := s.SetSecrets("i2", raw(map[string]string{"NOPE": `{"egress":true}`})); err == nil {
		t.Fatal("expected an error for a keep-existing entry with nothing stored")
	}
}

func raw(in map[string]string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range in {
		out[k] = json.RawMessage(v)
	}
	return out
}

var _ = fmt.Sprintf

// WebCrypto. Hosted functions run on workerd, which has all of it; the emulator had only
// crypto.randomUUID. So `crypto.getRandomValues` and `crypto.subtle.digest` — the two you
// cannot mint or hash a token without — worked in production and threw "Object has no
// member 'getRandomValues'" locally. That is the emulator failing at its one job, and the
// reason these are asserted rather than assumed.
func TestCryptoGetRandomValuesAndDigest(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "c", `export default { async fetch() {
		const a = new Uint8Array(32);
		const ret = crypto.getRandomValues(a);
		const hex = (b) => [...new Uint8Array(b)].map((x) => x.toString(16).padStart(2, "0")).join("");
		let floatRefused = false;
		try { crypto.getRandomValues(new Float64Array(2)); } catch { floatRefused = true; }
		let signSays = "";
		try { await crypto.subtle.sign(); } catch (e) { signSays = e.message; }
		return Response.json({
			// Filled IN PLACE and the same view handed back: both call shapes are used in the wild.
			filled: a.some((b) => b !== 0),
			sameView: ret === a,
			// A wider view must be filled across its whole byte length, not its element count.
			wide: crypto.getRandomValues(new Uint32Array(4)).some((v) => v !== 0),
			distinct: hex(crypto.getRandomValues(new Uint8Array(16)).buffer) !==
			          hex(crypto.getRandomValues(new Uint8Array(16)).buffer),
			abc: hex(await crypto.subtle.digest("SHA-256", new TextEncoder().encode("abc"))),
			// "sha256" and {name:"SHA-256"} are the same algorithm; a hyphen must not decide.
			looseName: hex(await crypto.subtle.digest({ name: "sha256" }, new TextEncoder().encode("abc"))),
			floatRefused,
			signSays,
		});
	} };`, nil)

	rec := invoke(t, mux, "/fn/main/c")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"filled", "sameView", "wide", "distinct", "floatRefused"} {
		if out[k] != true {
			t.Errorf("%s = %v, want true", k, out[k])
		}
	}
	// The published SHA-256 test vector for "abc" — a digest that merely returns 32 bytes
	// would pass a length check and still be wrong.
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if out["abc"] != abc {
		t.Errorf("SHA-256(abc) = %v, want %s", out["abc"], abc)
	}
	if out["looseName"] != abc {
		t.Errorf("digest({name:'sha256'}) = %v, want the same digest", out["looseName"])
	}
	// An unemulated method must name itself, not fail as "undefined is not a function"
	// somewhere inside whichever library reached for it.
	if msg, _ := out["signSays"].(string); !strings.Contains(msg, "not emulated") {
		t.Errorf("crypto.subtle.sign said %q, want it to say it is not emulated", msg)
	}
}
