package server

import (
	"net/http"
	"net/http/httptest"
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
