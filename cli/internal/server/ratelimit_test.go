package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A configured per-instance rate limit is enforced locally, so an app that exceeds the limit it
// set finds out here rather than in production. It used to be stored and ignored.
func TestConfiguredRateLimitIsEnforced(t *testing.T) {
	s, err := New(Options{DevOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_000_000, 0)
	s.lim.now = func() time.Time { return clock }
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	query := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/datastore/app/ns/_default/col/c/query", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer dev")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// No limit configured: never limited.
	for i := 0; i < 5; i++ {
		if code := query(); code != 200 {
			t.Fatalf("unlimited request %d = %d", i, code)
		}
	}
	if text, isErr := mcpCall(t, srv, "patch_instance_config", map[string]any{
		"service": "datastore", "instance": "app", "changes": map[string]any{"rateLimit": 2}}); isErr {
		t.Fatalf("patch: %s", text)
	}
	if a, b, c := query(), query(), query(); a != 200 || b != 200 || c != 429 {
		t.Fatalf("with rateLimit 2: %d %d %d, want 200 200 429", a, b, c)
	}
	clock = clock.Add(30 * time.Second) // one token back at 2/min
	if code := query(); code != 200 {
		t.Fatalf("after refill = %d", code)
	}
	if code := query(); code != 429 {
		t.Fatalf("refill gave more than it should: %d", code)
	}
}

// A client that opens a connection and sends nothing is dropped; the default server waited for
// ever.
func TestHTTPServerBoundsSlowClients(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Fatalf("timeouts unset: %+v", s)
	}
	if s.ReadTimeout != 0 || s.WriteTimeout != 0 {
		t.Fatal("a whole-request timeout would cut channel sockets and large uploads")
	}
}

func TestRateTargets(t *testing.T) {
	cases := []struct {
		method, path      string
		service, inst, fd string
		ok                bool
	}{
		{"POST", "/v1/datastore/app/ns/_default/col/c/query", "datastore", "app", "rateLimit", true},
		{"GET", "/v1/search/s/ns", "search", "s", "rateLimit", true},
		{"POST", "/v1/channel/ch/publish", "channel", "ch", "publishRateLimit", true},
		{"GET", "/v1/channel/ch/subscribe", "channel", "ch", "connectRateLimit", true},
		{"POST", "/v1/container/jobs", "container", "jobs", "rateLimit", true},
		{"GET", "/v1/container/jobs", "", "", "", false},
		{"GET", "/fn/api--v2/hello", "functions", "api--v2", "rateLimit", true},
		{"OPTIONS", "/v1/datastore/app/ns", "", "", "", false},
		{"GET", "/admin/datastore", "", "", "", false},
		{"POST", "/v1/auth/a/signin", "", "", "", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		svc, inst, _, field, ok := rateTarget(req)
		if ok != c.ok || (ok && (svc != c.service || inst != c.inst || field != c.fd)) {
			t.Errorf("%s %s = %s %s %s %v", c.method, c.path, svc, inst, field, ok)
		}
	}
}
