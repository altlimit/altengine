package altengine_test

// Unit tests that run with a plain `go test ./...`. The conformance suite needs
// `-tags conformance` and an emulator; without these, this module ran no tests at all.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	altengine "github.com/altlimit/altengine/go"
)

type seen struct {
	auth, ua string
	calls    atomic.Int32
}

func server(t *testing.T, s *seen, statuses ...int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.calls.Add(1))
		s.auth, s.ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		if n <= len(statuses) {
			w.WriteHeader(statuses[n-1])
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"documents":[],"cursor":null}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func query(ae *altengine.Client) error {
	_, err := ae.Datastore("app").Query(context.Background(), "todos", altengine.QueryRequest{})
	return err
}

func TestAPIKeyFromEitherVariable(t *testing.T) {
	var s seen
	srv := server(t, &s)
	t.Setenv("ALTENGINE_API_KEY", "")
	t.Setenv("ALTENGINE_KEY", "ae_old")
	if err := query(altengine.New(altengine.WithBaseURL(srv.URL))); err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer ae_old" {
		t.Fatalf("ALTENGINE_KEY not used: %q", s.auth)
	}
	t.Setenv("ALTENGINE_API_KEY", "ae_new")
	if err := query(altengine.New(altengine.WithBaseURL(srv.URL))); err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer ae_new" {
		t.Fatalf("ALTENGINE_API_KEY does not win: %q", s.auth)
	}
}

func TestUserAgentIsSent(t *testing.T) {
	var s seen
	srv := server(t, &s)
	if err := query(altengine.New(altengine.WithBaseURL(srv.URL), altengine.WithAPIKey("k"))); err != nil {
		t.Fatal(err)
	}
	if s.ua != altengine.UserAgent {
		t.Fatalf("User-Agent = %q", s.ua)
	}
}

func TestBaseURLFromEnvironment(t *testing.T) {
	var s seen
	srv := server(t, &s)
	t.Setenv("ALTENGINE_URL", srv.URL)
	if err := query(altengine.New(altengine.WithAPIKey("k"))); err != nil || s.calls.Load() != 1 {
		t.Fatalf("ALTENGINE_URL not used: %v (%d calls)", err, s.calls.Load())
	}
}

func TestRetriesAServiceUnavailable(t *testing.T) {
	var s seen
	srv := server(t, &s, http.StatusServiceUnavailable)
	ae := altengine.New(altengine.WithBaseURL(srv.URL), altengine.WithAPIKey("k"),
		altengine.WithRetry(altengine.Retry{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	if err := query(ae); err != nil || s.calls.Load() != 2 {
		t.Fatalf("err %v after %d calls, want success on the 2nd", err, s.calls.Load())
	}
}

func TestPutMultiAfterLostResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keys      []any
		wantCalls int32
	}{
		{"generated", []any{nil}, 1},
		{"mixed", []any{"known", nil}, 1},
		{"typed nil", []any{(*string)(nil)}, 1},
		{"explicit", []any{0, "known"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close() // The write arrived, but its response never did.
					return
				}
				_, _ = w.Write([]byte(`{"keys":["0","known"]}`))
			}))
			defer srv.Close()
			c := altengine.New(altengine.WithBaseURL(srv.URL), altengine.WithAPIKey("k"),
				altengine.WithRetry(altengine.Retry{MaxAttempts: 2, BaseDelay: time.Millisecond}))
			var docs []altengine.PutDocument
			for _, key := range tc.keys {
				docs = append(docs, altengine.PutDocument{Key: key, Data: "value"})
			}
			_, err := c.Datastore("app").PutMulti(context.Background(), "things", docs)
			if calls.Load() != tc.wantCalls || (err == nil) != (tc.wantCalls == 2) {
				t.Fatalf("got %d calls, err %v", calls.Load(), err)
			}
		})
	}
}
