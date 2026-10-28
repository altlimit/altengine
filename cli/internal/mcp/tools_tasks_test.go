package mcp

import (
	"strings"
	"testing"
)

// An agent as the WORKER: make a pull queue, add to it, lease, ack, give one back — and an agent
// wiring an app: watch, read, cancel, run, purge, delete. Every call is the /v1 call it maps to.
func TestTasksToolsRoundTrip(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "tasks", "jobs")

	qs := runTool(t, h, "tasks_list_queues", map[string]any{"instance": "jobs"})
	if qs["instance"] != "jobs" || len(qs["queues"].([]any)) != 1 {
		t.Fatalf("queues = %v", qs)
	}
	q := runTool(t, h, "tasks_set_queue", map[string]any{"instance": "jobs", "name": "todo", "create": true, "mode": "pull", "retry": map[string]any{"maxAttempts": 3}})
	if st := q["queue"].(map[string]any)["settings"].(map[string]any); st["mode"] != "pull" || st["retry"].(map[string]any)["maxAttempts"] != float64(3) {
		t.Fatalf("set_queue = %v", q)
	}
	// Only what is passed changes.
	q = runTool(t, h, "tasks_set_queue", map[string]any{"instance": "jobs", "name": "todo", "paused": false, "maxConcurrent": 2})
	if st := q["queue"].(map[string]any)["settings"].(map[string]any); st["mode"] != "pull" || st["maxConcurrent"] != float64(2) {
		t.Fatalf("patch = %v", q)
	}

	added := runTool(t, h, "tasks_add", map[string]any{"instance": "jobs", "queue": "todo", "tasks": []any{
		map[string]any{"name": "a", "payload": map[string]any{"n": 1}},
		map[string]any{"payload": map[string]any{"n": 2}},
	}})
	res := added["tasks"].([]any)
	if len(res) != 2 || res[0].(map[string]any)["ok"] != true {
		t.Fatalf("add = %v", added)
	}
	// Every name taken: the per-task answer, not a tool error.
	dup := runTool(t, h, "tasks_add", map[string]any{"instance": "jobs", "queue": "todo", "tasks": []any{map[string]any{"name": "a", "payload": nil}}})
	if r := dup["tasks"].([]any)[0].(map[string]any); r["code"] != "TASK_EXISTS" {
		t.Fatalf("dup = %v", dup)
	}

	leased := runTool(t, h, "tasks_lease", map[string]any{"instance": "jobs", "queue": "todo", "count": 5, "leaseSeconds": 60})["tasks"].([]any)
	if len(leased) != 2 {
		t.Fatalf("leased = %v", leased)
	}
	first, second := leased[0].(map[string]any), leased[1].(map[string]any)
	if first["source"] != "mcp" || first["payload"].(map[string]any)["n"] != float64(1) {
		t.Fatalf("leased task = %v", first)
	}
	acked := runTool(t, h, "tasks_ack", map[string]any{"instance": "jobs", "id": first["id"], "leaseId": first["leaseId"], "result": "fine"})
	if acked["task"].(map[string]any)["state"] != "done" {
		t.Fatalf("ack = %v", acked)
	}
	rel := runTool(t, h, "tasks_release", map[string]any{"instance": "jobs", "id": second["id"], "leaseId": second["leaseId"], "error": "later", "delaySeconds": 600})
	if rel["task"].(map[string]any)["lastError"] != "later" {
		t.Fatalf("release = %v", rel)
	}
	if text, isErr := toolText(t, h, "tasks_ack", map[string]any{"instance": "jobs", "id": second["id"], "leaseId": second["leaseId"]}); !isErr || !strings.Contains(text, "LEASE_LOST") {
		t.Fatalf("ack after release: %s", text)
	}

	page := runTool(t, h, "tasks_list", map[string]any{"instance": "jobs", "queue": "todo", "state": "done", "limit": 10})
	if n := len(page["tasks"].([]any)); n != 1 {
		t.Fatalf("done = %v", page)
	}
	got := runTool(t, h, "tasks_get", map[string]any{"instance": "jobs", "id": first["id"]})
	if got["task"].(map[string]any)["result"] != "fine" {
		t.Fatalf("get = %v", got)
	}
	ran := runTool(t, h, "tasks_run", map[string]any{"instance": "jobs", "id": second["id"]})
	if ran["task"].(map[string]any)["state"] != "scheduled" {
		t.Fatalf("run = %v", ran)
	}
	cancelled := runTool(t, h, "tasks_cancel", map[string]any{"instance": "jobs", "ids": []any{second["id"], first["id"]}})
	if len(cancelled["cancelled"].([]any)) != 1 || len(cancelled["skipped"].([]any)) != 1 {
		t.Fatalf("cancel = %v", cancelled)
	}

	secret := runTool(t, h, "tasks_reveal_webhook_secret", map[string]any{"instance": "jobs"})
	if s, _ := secret["secret"].(string); !strings.HasPrefix(s, "whsec_") {
		t.Fatalf("secret = %v", secret)
	}
	if _, isErr := toolText(t, h, "tasks_rotate_webhook_secret", map[string]any{"instance": "jobs"}); !isErr {
		t.Fatal("rotate must ask for confirm")
	}
	rot := runTool(t, h, "tasks_rotate_webhook_secret", map[string]any{"instance": "jobs", "confirm": true})
	if rot["rotated"] != true || rot["secret"] == secret["secret"] {
		t.Fatalf("rotate = %v", rot)
	}

	purged := runTool(t, h, "tasks_purge_queue", map[string]any{"instance": "jobs", "queue": "todo", "confirm": true})
	if purged["purged"] != float64(2) {
		t.Fatalf("purge = %v", purged)
	}
	runTool(t, h, "tasks_delete_queue", map[string]any{"instance": "jobs", "queue": "todo", "confirm": true})
	if text, isErr := toolText(t, h, "tasks_delete_queue", map[string]any{"instance": "jobs", "queue": "default", "confirm": true}); !isErr || !strings.Contains(text, "purge it instead") {
		t.Fatalf("delete default: %s", text)
	}
	if text, isErr := toolText(t, h, "tasks_list_queues", map[string]any{"instance": "nope"}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("missing instance: %s", text)
	}
}

func TestTasksGuideIsServed(t *testing.T) {
	contents, err := readResource("docs://tasks/guide")
	if err != nil {
		t.Fatal(err)
	}
	text := contents[0]["text"].(string)
	// Every tool the guide names exists.
	for _, name := range []string{"tasks_set_queue", "tasks_add", "tasks_lease", "tasks_ack", "tasks_release", "tasks_list", "tasks_get", "tasks_run", "tasks_cancel", "tasks_purge_queue"} {
		if !strings.Contains(text, name) {
			t.Errorf("guide does not mention %s", name)
		}
		if _, ok := registry[name]; !ok {
			t.Errorf("guide names %s, which is not a tool", name)
		}
	}
}
