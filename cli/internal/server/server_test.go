package server

import (
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
