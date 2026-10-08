package automation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fake(t *testing.T, h func(path, cursor string) any) Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(h(r.URL.Path, r.URL.Query().Get("cursor")))
	}))
	t.Cleanup(srv.Close)
	return Config{BaseURL: srv.URL, APIKey: "k", Instance: "fleet"}
}

// The script list and a script's history are paged hosted; reading the first page as the whole
// list printed a shorter one with nothing to say so.
func TestScriptsAndVersionsWalkEveryPage(t *testing.T) {
	c := fake(t, func(path, cursor string) any {
		switch path {
		case "/v1/automation/fleet/scripts":
			if cursor == "" {
				return map[string]any{"scripts": []any{map[string]any{"name": "a"}}, "cursor": "p2"}
			}
			return map[string]any{"scripts": []any{map[string]any{"name": "b"}}}
		default:
			if cursor == "" {
				return map[string]any{"script": map[string]any{"active_version": 3},
					"versions": []any{map[string]any{"version": 3}}, "versions_cursor": "v2"}
			}
			return map[string]any{"script": map[string]any{"active_version": 3}, "versions": []any{map[string]any{"version": 2}}}
		}
	})
	scripts, err := c.Scripts()
	if err != nil || len(scripts) != 2 {
		t.Fatalf("scripts = %v %v", scripts, err)
	}
	versions, active, err := c.Versions("a")
	if err != nil || len(versions) != 2 || active != 3 {
		t.Fatalf("versions = %v %d %v", versions, active, err)
	}
}

// A page of runs comes back with the cursor for the next one, which it used to drop.
func TestRunsReturnsTheCursor(t *testing.T) {
	var asked string
	c := fake(t, func(path, cursor string) any {
		asked = cursor
		return map[string]any{"runs": []any{map[string]any{"id": "r1"}}, "cursor": "next"}
	})
	runs, next, err := c.Runs(1, "", "", "here")
	if err != nil || len(runs) != 1 || next != "next" || asked != "here" {
		t.Fatalf("runs=%v next=%q asked=%q err=%v", runs, next, asked, err)
	}
}
