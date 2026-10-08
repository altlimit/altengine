package static

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/hosted"
)

func fileOf(t *testing.T, body string) File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return File{Path: "/f", Local: p, Size: int64(len(body)), Hash: "h"}
}

// A hash the server asks for that is not ours is refused before anything uploads. It used to
// return from inside the loop, leaving the uploads already started running behind it — and a
// short hash panicked the slice that printed it.
func TestPutAllChecksEveryHashFirst(t *testing.T) {
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { puts.Add(1) }))
	defer srv.Close()
	byHash := map[string]File{"h": fileOf(t, "x")}
	err := PutAll([]Upload{{Hash: "h", URL: srv.URL}, {Hash: "abc", URL: srv.URL}}, byHash, nil)
	if err == nil {
		t.Fatal("an unknown hash was accepted")
	}
	if n := puts.Load(); n != 0 {
		t.Fatalf("%d uploads started before the refusal", n)
	}
}

// One dropped attempt does not fail the deploy: a PUT of the same bytes is safe to repeat.
func TestPutRetries(t *testing.T) {
	prev := hosted.Sleep
	hosted.Sleep = func(time.Duration) {}
	t.Cleanup(func() { hosted.Sleep = prev })
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	if err := PutAll([]Upload{{Hash: "h", URL: srv.URL}}, map[string]File{"h": fileOf(t, "x")}, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d attempts, want 2", calls.Load())
	}

	// A refusal that will not change is not retried.
	calls.Store(0)
	srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv403.Close()
	if err := PutAll([]Upload{{Hash: "h", URL: srv403.URL}}, map[string]File{"h": fileOf(t, "x")}, nil); err == nil || calls.Load() != 1 {
		t.Fatalf("403: %d attempts, err %v", calls.Load(), err)
	}
}
