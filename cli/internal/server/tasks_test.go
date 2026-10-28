package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tasks service as the emulator wires it: triggers saved through the console's config PUT and
// MCP's patch are checked against real queues, a datastore write lands in a queue, a queue's
// config is the hosted shape, and deleting the instance drops its tasks.
func TestTasksAreWiredIntoTheEmulator(t *testing.T) {
	s, err := New(Options{DataDir: "", DevOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Host = "127.0.0.1:9191"
		req.Header.Set("Authorization", "Bearer dev")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, inst := do("POST", "/admin/tasks", map[string]any{"name": "jobs", "region": "weur"})
	if code != 201 || inst["config"].(map[string]any)["region"] != "weur" || inst["config"].(map[string]any)["retentionDays"] != float64(7) {
		t.Fatalf("create: %d %v", code, inst)
	}
	if code, out := do("PUT", "/admin/tasks/"+inst["id"].(string)+"/config", map[string]any{"config": map[string]any{"region": "enam"}}); code != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "cannot be changed") {
		t.Fatalf("region change: %d %v", code, out)
	}
	if code, out := do("POST", "/v1/tasks/jobs/queues", map[string]any{"name": "events", "mode": "pull"}); code != 201 {
		t.Fatalf("queue: %d %v", code, out)
	}

	code, ds := do("POST", "/admin/datastore", map[string]any{"name": "shop"})
	if code != 201 {
		t.Fatalf("datastore: %d %v", code, ds)
	}
	rule := func(queue string) map[string]any {
		return map[string]any{"config": map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "orders", "namespace": "_default"}, "queue": queue}}}}
	}
	if code, out := do("PUT", "/admin/datastore/"+ds["id"].(string)+"/config", rule("jobs/nope")); code != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "has no queue 'nope'") {
		t.Fatalf("missing queue: %d %v", code, out)
	}
	code, saved := do("PUT", "/admin/datastore/"+ds["id"].(string)+"/config", rule("jobs/events"))
	if code != 200 {
		t.Fatalf("save: %d %v", code, saved)
	}
	// Stored normalized, as hosted returns it: `_default` is the default namespace.
	_, got := do("GET", "/admin/datastore/"+ds["id"].(string), nil)
	trig := got["config"].(map[string]any)["triggers"].([]any)[0].(map[string]any)
	if trig["match"].(map[string]any)["namespace"] != "" {
		t.Fatalf("stored trigger = %v", trig)
	}

	if code, out := do("POST", "/v1/datastore/shop/ns/_default/col/orders/documents", map[string]any{"documents": []any{map[string]any{"key": "o1", "data": map[string]any{"total": 3}}}}); code != 200 {
		t.Fatalf("write: %d %v", code, out)
	}
	s.task.Wait()
	_, leased := do("POST", "/v1/tasks/jobs/queues/events/lease", map[string]any{"count": 10})
	list := leased["tasks"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["payload"].(map[string]any)["type"] != "datastore.write" {
		t.Fatalf("leased = %v", leased)
	}

	// The tasks plane is rate limited by its instance's own limit, as hosted.
	if code, out := do("PUT", "/admin/tasks/"+inst["id"].(string)+"/config", map[string]any{"config": map[string]any{"region": "weur", "rateLimit": 1}}); code != 200 {
		t.Fatalf("rate limit: %d %v", code, out)
	}
	do("GET", "/v1/tasks/jobs", nil)
	if code, _ := do("GET", "/v1/tasks/jobs", nil); code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", code)
	}

	if code, out := do("DELETE", "/admin/tasks/"+inst["id"].(string), nil); code != 200 || out["deleted"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}
}
