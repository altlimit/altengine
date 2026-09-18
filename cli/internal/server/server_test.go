package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A browser app on another origin (Vite on :5173 → emulator on :9191) is the normal local
// setup for a client-side app, so the data plane must answer with CORS headers or every
// fetch fails before it reaches a route.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := New(Options{DataDir: "", DevOpen: true})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestDataPlaneReflectsRequestOrigin(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/auth/testauth/config", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want the request origin reflected", got)
	}
	if got := res.Header.Get("Vary"); got == "" {
		t.Fatalf("Vary must include Origin when the origin is reflected, got %q", got)
	}
	// Reflecting an origin without allowing credentials is what keeps this safe: these
	// endpoints carry no cookies, so there is no ambient credential for a foreign page to ride.
	if got := res.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Access-Control-Allow-Credentials must not be set, got %q", got)
	}
}

func TestPreflightIsAnsweredWithoutReachingARoute(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/v1/datastore/app/ns/_default/col/posts/query", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", res.StatusCode)
	}
	// The browser refuses the real request unless the bearer header is permitted.
	if got := res.Header.Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatalf("preflight must allow Authorization/Content-Type, got %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatalf("preflight must advertise allowed methods, got %q", got)
	}
}

// The admin plane is same-origin here and cookie-authed + CSRF-checked when hosted. Handing
// it CORS locally would teach a habit that breaks in production.
func TestAdminPlaneGetsNoCORS(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/admin/auth", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("admin must not send CORS headers, got %q", got)
	}
}

// The static data plane exists hosted and not here. Without an explicit route the console's
// catch-all on "/" answers a deploy attempt with a page of HTML, which reads as a broken URL
// rather than an unimplemented one — so the reply has to name what to do instead.
func TestStaticDeployIsRefusedWithDirections(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/v1/static", "/v1/static/mysite/deployments"} {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s status = %d, want 501", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s Content-Type = %q, want the error envelope rather than console HTML", path, ct)
		}
		if !strings.Contains(string(body), "--static") {
			t.Fatalf("%s body does not say what to do instead: %s", path, body)
		}
	}
}

// The site's port defaults to the API's + 1, and --static-port overrides it.
func TestStaticAddrDefaultsToApiPortPlusOne(t *testing.T) {
	for _, c := range []struct{ addr, flag, want string }{
		{"127.0.0.1:9191", "", "127.0.0.1:9192"},
		{"0.0.0.0:8080", "", "0.0.0.0:8081"},
		{"127.0.0.1:9191", "127.0.0.1:4000", "127.0.0.1:4000"},
	} {
		s := &Server{opts: Options{Addr: c.addr, StaticAddr: c.flag}}
		if got := s.staticAddr(); got != c.want {
			t.Errorf("staticAddr(%q, %q) = %q, want %q", c.addr, c.flag, got, c.want)
		}
	}
}

// The listener is bound in New so a port already in use is a startup error alongside the rest,
// not a goroutine that dies after the banner has printed.
func TestStaticSiteIsBoundAtStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Port 0 rather than the real default: a developer running the emulator while the tests run
	// must not turn this into a failure about their own machine.
	s, err := New(Options{Addr: "127.0.0.1:9191", StaticAddr: "127.0.0.1:0", DevOpen: true, StaticDir: dir})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer s.siteLn.Close()

	if s.site == nil {
		t.Fatal("no site server was built")
	}
	if _, err := net.Listen("tcp", s.siteLn.Addr().String()); err == nil {
		t.Error("the site port was not actually held from New onward")
	}
}

// A port that cannot be bound must fail New, not ListenAndServe.
func TestStaticSitePortInUseFailsStartup(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	if _, err := New(Options{Addr: "127.0.0.1:9191", StaticAddr: taken.Addr().String(), DevOpen: true, StaticDir: t.TempDir()}); err == nil {
		t.Error("New accepted a port already in use")
	}
}

// Retention over the surface an agent uses: patch_instance_config refuses a bad keepVersions,
// lowering it prunes, and a rolled-back function keeps serving through a later staged deploy.
func TestFunctionRetentionOverMCP(t *testing.T) {
	srv := newTestServer(t)
	tool := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer dev")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil || len(out.Result.Content) == 0 {
			t.Fatalf("%s: undecodable (%v)", name, err)
		}
		return out.Result.Content[0].Text, out.Result.IsError
	}
	deploy := func(body string, activate bool) {
		t.Helper()
		code := `export default { fetch() { return new Response("` + body + `"); } };`
		if text, isErr := tool("functions_deploy", map[string]any{"instance": "fx", "name": "hello", "code": code, "activate": activate}); isErr {
			t.Fatalf("deploy %s: %s", body, text)
		}
	}
	get := func(path string) (int, string) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	if text, isErr := tool("create_instance", map[string]any{"service": "functions", "name": "fx"}); isErr {
		t.Fatalf("create_instance: %s", text)
	}
	for i := 1; i <= 3; i++ {
		deploy(fmt.Sprintf("v%d", i), true)
	}
	if text, isErr := tool("patch_instance_config", map[string]any{"service": "functions", "instance": "fx", "changes": map[string]any{"keepVersions": 51}}); !isErr || !strings.Contains(text, "keepVersions") {
		t.Fatalf("keepVersions=51 was not refused: %s", text)
	}
	if text, isErr := tool("patch_instance_config", map[string]any{"service": "functions", "instance": "fx", "changes": map[string]any{"keepVersions": 2}}); isErr {
		t.Fatalf("keepVersions=2: %s", text)
	}
	if text, isErr := tool("functions_rollback", map[string]any{"instance": "fx", "name": "hello", "version": 2}); isErr {
		t.Fatalf("rollback: %s", text)
	}
	deploy("v4", false)
	deploy("v5", false)

	text, _ := tool("functions_versions", map[string]any{"instance": "fx", "name": "hello"})
	var listed struct {
		Versions []struct {
			Version int    `json:"version"`
			URL     string `json:"url"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("versions: %s", text)
	}
	var got []int
	for _, v := range listed.Versions {
		got = append(got, v.Version)
		if want := fmt.Sprintf("/fn/fx--v%d/hello", v.Version); v.URL != want {
			t.Errorf("v%d url = %q, want %q", v.Version, v.URL, want)
		}
	}
	if fmt.Sprint(got) != "[5 4 2]" {
		t.Fatalf("versions = %v, want [5 4 2]", got)
	}
	if status, body := get("/fn/fx/hello"); status != 200 || body != "v2" {
		t.Errorf("live = %d %q, want v2", status, body)
	}
	if status, body := get("/fn/fx--v5/hello"); status != 200 || body != "v5" {
		t.Errorf("v5 address = %d %q", status, body)
	}
	if status, _ := get("/fn/fx--v1/hello"); status != 404 {
		t.Errorf("pruned v1 address = %d, want 404", status)
	}
}

// No --static, no listener: the emulator must not claim a second port for a site nobody asked
// for — that port belongs to whatever else the developer is running.
func TestNoStaticDirClaimsNoPort(t *testing.T) {
	s, err := New(Options{Addr: "127.0.0.1:9191", DevOpen: true})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if s.site != nil || s.siteLn != nil {
		t.Error("a site listener was created without --static")
	}
}

// While `altengine dev` is running, every website the developer visits can reach it. Hosted, the
// reflected-origin policy is safe because a request needs an API key a foreign page does not
// have; in dev-open mode ANY bearer works, so the page simply sends one. Reflecting its origin
// would hand it a complete API client for the developer's local data — and, with Docker running,
// a path to the machine itself through the container service.
func TestForeignOriginGetsNoCorsHeadersInDevOpen(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/auth/testauth/config", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want none for a foreign origin", got)
	}
}

func TestAllowOriginWidensItDeliberately(t *testing.T) {
	s, err := New(Options{DataDir: "", DevOpen: true, AllowOrigins: []string{"https://phone.example"}})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/auth/testauth/config", nil)
	req.Header.Set("Origin", "https://phone.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://phone.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want the allowed origin reflected", got)
	}
}

// A browser resolves attacker.example to 127.0.0.1 and then treats the emulator as SAME-ORIGIN
// with the attacker's page — which defeats every same-origin protection, including the Origin
// check below. The Host header is the only thing that tells the two apart.
func TestRefusesARebindingHost(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/auth/testauth/config", nil)
	req.Host = "attacker.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-local Host", res.StatusCode)
	}
}

// /admin and /mcp are same-origin surfaces with no CORS. A no-cors POST from any page still
// ARRIVES — the response being unreadable does not undo a delete — and a cross-site request
// always carries an Origin, which is the whole check.
func TestAdminAndMcpRefuseACrossSitePost(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/admin/instances", "/mcp"} {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "text/plain")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403 for a cross-site POST", path, res.StatusCode)
		}
	}
}

func TestAdminStillWorksFromTheConsoleAndFromATerminal(t *testing.T) {
	srv := newTestServer(t)
	// The console: same-origin, so its Origin is this server's own.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/instances", nil)
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("console request: %v", err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusForbidden {
		t.Fatal("the console's own same-origin request must not be refused")
	}
	// A terminal client sends no Origin at all.
	res2, err := http.Get(srv.URL + "/admin/instances")
	if err != nil {
		t.Fatalf("cli request: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode == http.StatusForbidden {
		t.Fatal("a request with no Origin must not be refused")
	}
}
