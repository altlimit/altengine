package hosted

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The API key rides in a header on every request, so a URL that would carry it in clear text over
// the network is refused before anything is sent.
func TestCheckURL(t *testing.T) {
	ok := []string{
		"https://api.altengine.net",
		"https://example.com:8443/base",
		"http://127.0.0.1:9191",
		"http://localhost:9191",
		"http://[::1]:9191",
		"http://app.localhost",
	}
	bad := []string{
		"http://api.altengine.net",
		"http://192.168.1.10:9191",
		"http://10.0.0.1",
		"ftp://api.altengine.net",
		"api.altengine.net",
		"",
		"http://localhost.example.com",
	}
	for _, u := range ok {
		if err := CheckURL(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestDoRefusesPlainHTTPBeforeSending(t *testing.T) {
	err := Client{BaseURL: "http://api.altengine.net", APIKey: "ae_secret"}.Do(http.MethodGet, "/v1/x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func noSleep(t *testing.T) {
	prev := Sleep
	Sleep = func(time.Duration) {}
	t.Cleanup(func() { Sleep = prev })
}

// A request the service turned away (429/503) is retried whatever its method; one that may have
// been acted on (502/504) is retried only when repeating it is safe. A deploy is a POST, and two
// of them would be two versions.
func TestRetries(t *testing.T) {
	noSleep(t)
	cases := []struct {
		method string
		status int
		calls  int32
	}{
		{"POST", 503, 4},
		{"POST", 429, 4},
		{"POST", 502, 1},
		{"GET", 502, 4},
		{"GET", 404, 1},
		{"PUT", 504, 4},
	}
	for _, c := range cases {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(c.status)
		}))
		err := Client{BaseURL: srv.URL, APIKey: "k"}.Do(c.method, "/x", map[string]any{"a": 1}, nil)
		srv.Close()
		if err == nil || calls.Load() != c.calls {
			t.Errorf("%s %d: %d calls (err %v), want %d", c.method, c.status, calls.Load(), err, c.calls)
		}
	}
}

func TestRetrySucceedsAndResendsTheBody(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	var out map[string]any
	if err := (Client{BaseURL: srv.URL, APIKey: "k"}).Do("POST", "/x", map[string]any{"a": 1}, &out); err != nil {
		t.Fatal(err)
	}
	if out["a"] != float64(1) || calls.Load() != 2 {
		t.Fatalf("got %v after %d calls", out, calls.Load())
	}
}

func TestBackoffHonoursRetryAfter(t *testing.T) {
	if got := Backoff(1, "3"); got != 3*time.Second {
		t.Errorf("Retry-After 3 = %v", got)
	}
	if got := Backoff(1, "999"); got != 30*time.Second {
		t.Errorf("Retry-After is not capped: %v", got)
	}
	if Backoff(3, "") != 4*BaseBackoff {
		t.Errorf("backoff does not double: %v", Backoff(3, ""))
	}
}

func TestShort(t *testing.T) {
	if Short("abc", 12) != "abc" || Short("0123456789abcdef", 12) != "0123456789ab" {
		t.Fatal("Short")
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"needs full"}}`))
	}))
	defer srv.Close()
	err := Client{BaseURL: srv.URL, APIKey: "k"}.Do(http.MethodGet, "/x", nil, nil)
	he, ok := err.(*Error)
	if !ok || he.Status != 403 || he.Code != "FORBIDDEN" || err.Error() != "needs full (FORBIDDEN)" {
		t.Fatalf("got %#v", err)
	}
}
