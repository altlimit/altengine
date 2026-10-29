package tasks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// --- fixtures ----------------------------------------------------------------------------------

type fnCall struct {
	path    string
	trigger string
	header  http.Header
	body    map[string]any
}

type world struct {
	reg  *control.Registry
	svc  *Service
	mux  *http.ServeMux
	hub  *channel.Hub
	mu   sync.Mutex
	fnIn []fnCall
	// answer is what the stand-in function answers: status and headers.
	answer func(n int) (int, http.Header, string)
	clock  int64
	auth   *auth.Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	reg.OnValidateConfig("tasks", ValidateConfig)
	w := &world{reg: reg, mux: http.NewServeMux(), hub: channel.NewHub(), clock: 1_800_000_000_000}
	w.answer = func(int) (int, http.Header, string) { return 200, nil, `{"ok":true}` }
	// A stand-in for the functions plane: records what it was handed, answers as told.
	w.mux.HandleFunc("POST /fn/{instance}/{fn}", func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		w.fnIn = append(w.fnIn, fnCall{path: r.URL.Path, trigger: common.Trigger(r), header: r.Header.Clone(), body: body})
		n := len(w.fnIn)
		w.mu.Unlock()
		status, h, out := w.answer(n)
		for k, v := range h {
			rw.Header()[k] = v
		}
		rw.WriteHeader(status)
		_, _ = io.WriteString(rw, out)
	})
	w.svc = NewService(reg, NewStore(""), w.mux, w.hub)
	w.svc.SetClock(func() int64 {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.clock
	})
	w.auth = auth.NewStore(true)
	NewHandler(reg, w.auth, w.svc).Register(w.mux)
	reg.OnDelete("tasks", w.svc.Drop)
	if _, err := reg.Create("functions", "app"); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) advance(ms int64) {
	w.mu.Lock()
	w.clock += ms
	w.mu.Unlock()
}

func (w *world) calls() []fnCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]fnCall(nil), w.fnIn...)
}

// pump runs the engine until nothing more is delivered at the current time.
func (w *world) pump() {
	for i := 0; i < 5; i++ {
		w.svc.Tick()
		w.svc.Wait()
	}
}

func (w *world) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return w.doAs(t, "dev", method, path, body)
}

func (w *world) doAs(t *testing.T, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (w *world) mustDo(t *testing.T, want int, method, path string, body any) map[string]any {
	t.Helper()
	code, out := w.do(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d: %v", method, path, code, want, out)
	}
	return out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func errMsg(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func (w *world) task(t *testing.T, id string) map[string]any {
	t.Helper()
	out := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/tasks/"+id, nil)
	return out["task"].(map[string]any)
}

func (w *world) addOne(t *testing.T, queue string, task any) string {
	t.Helper()
	out := w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues/"+queue+"/tasks", task)
	res := out["tasks"].([]any)[0].(map[string]any)
	return res["id"].(string)
}

func (w *world) pushQueue(t *testing.T, name string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "target": map[string]any{"function": "app/work"}}
	for k, v := range extra {
		body[k] = v
	}
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", body)
}

// --- the surface -------------------------------------------------------------------------------

func TestInstanceHasADefaultQueue(t *testing.T) {
	w := newWorld(t)
	out := w.mustDo(t, 200, "GET", "/v1/tasks/jobs", nil)
	if out["name"] != "jobs" || out["retentionDays"] != float64(7) {
		t.Fatalf("info = %v", out)
	}
	qs := out["queues"].([]any)
	if len(qs) != 1 || qs[0].(map[string]any)["name"] != "default" {
		t.Fatalf("queues = %v", qs)
	}
	q := qs[0].(map[string]any)
	st := q["settings"].(map[string]any)
	if st["mode"] != "push" || st["ratePerSecond"] != float64(10) || st["maxConcurrent"] != float64(5) || st["targetControl"] != true || st["target"] != nil {
		t.Fatalf("default settings = %v", st)
	}
	if q["counts"].(map[string]any)["scheduled"] != float64(0) {
		t.Fatalf("counts = %v", q["counts"])
	}
	// The default queue stays; it is purged instead.
	out = w.mustDo(t, 400, "DELETE", "/v1/tasks/jobs/queues/default", nil)
	if !strings.Contains(errMsg(out), "purge it instead") {
		t.Fatalf("delete default: %v", out)
	}
}

func TestQueueSettingsArePatched(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "emails", map[string]any{"ratePerSecond": 2})
	out := w.mustDo(t, 200, "PATCH", "/v1/tasks/jobs/queues/emails", map[string]any{"maxConcurrent": 3})
	st := out["queue"].(map[string]any)["settings"].(map[string]any)
	// Changing one setting keeps the others: a PATCH, not a replace.
	if st["maxConcurrent"] != float64(3) || st["ratePerSecond"] != float64(2) || st["target"].(map[string]any)["function"] != "app/work" {
		t.Fatalf("settings = %v", st)
	}
	cases := []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"ratePerSecond": 0}, "ratePerSecond must be a number from 0.01 to 100"},
		{map[string]any{"maxConcurrent": 51}, "maxConcurrent must be an integer from 1 to 50"},
		{map[string]any{"mode": "fifo"}, "mode must be 'push' or 'pull'"},
		{map[string]any{"target": map[string]any{"webhook": "http://x.example"}}, "target.webhook must be an https URL"},
		{map[string]any{"target": map[string]any{"function": "app/work", "channel": "c/x"}}, "exactly one of"},
		{map[string]any{"target": map[string]any{"function": "nope/work"}}, "no functions instance 'nope'"},
		{map[string]any{"retry": map[string]any{"minBackoffSeconds": 100, "maxBackoffSeconds": 10}}, "maxBackoffSeconds must be at least minBackoffSeconds"},
		{map[string]any{"deadLetterQueue": "emails"}, "its own dead-letter queue"},
		{map[string]any{"deadLetterQueue": "ghost"}, "deadLetterQueue 'ghost' does not exist"},
	}
	for _, c := range cases {
		out := w.mustDo(t, 400, "PATCH", "/v1/tasks/jobs/queues/emails", c.body)
		if !strings.Contains(errMsg(out), c.msg) {
			t.Errorf("%v: %q, want %q", c.body, errMsg(out), c.msg)
		}
	}
	out = w.mustDo(t, 409, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "emails"})
	if errCode(out) != "ALREADY_EXISTS" {
		t.Fatalf("duplicate: %v", out)
	}
	out = w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "Emails"})
	if !strings.Contains(errMsg(out), "1-63 characters") {
		t.Fatalf("bad name: %v", out)
	}
	// A pull queue never has a target.
	out = w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "inbox", "mode": "pull", "target": map[string]any{"function": "app/work"}})
	if out["queue"].(map[string]any)["settings"].(map[string]any)["target"] != nil {
		t.Fatalf("pull queue kept a target: %v", out)
	}
}

func TestAddValidatesAndDedupes(t *testing.T) {
	w := newWorld(t)
	out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/default/tasks", map[string]any{"payload": 1})
	if errCode(out) != "NO_TARGET" {
		t.Fatalf("no target: %v", out)
	}
	w.pushQueue(t, "q", nil)
	bad := []struct {
		body any
		msg  string
	}{
		{map[string]any{"name": "x"}, "task.payload is required"},
		{map[string]any{"payload": 1, "delay": 5, "runAt": 1}, "give delay or runAt, not both"},
		{map[string]any{"payload": 1, "delay": -1}, "delay must be a number of seconds"},
		{map[string]any{"payload": 1, "delay": 31 * 86400}, "more than 30 days ahead"},
		{map[string]any{"payload": 1, "name": "has space"}, "task.name must be 1-500 characters"},
		{map[string]any{"payload": strings.Repeat("x", 130*1024)}, "task.payload is larger than 128 KB"},
		{map[string]any{"payload": 1, "params": []any{1}}, "task.params must be an object"},
		{[]any{}, "no tasks to add"},
		{[]any{map[string]any{"payload": 1}, map[string]any{}}, "tasks[1].payload is required"},
	}
	for _, c := range bad {
		out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/q/tasks", c.body)
		if !strings.Contains(errMsg(out), c.msg) {
			t.Errorf("%v: %q, want %q", c.body, errMsg(out), c.msg)
		}
	}
	many := make([]any, 101)
	for i := range many {
		many[i] = map[string]any{"payload": i}
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/q/tasks", many); !strings.Contains(errMsg(out), "at most 100") {
		t.Fatalf("101 tasks: %v", out)
	}

	// A name makes an add idempotent; a second add of it is refused per task, 409 when none landed.
	first := w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues/q/tasks", map[string]any{"tasks": []any{map[string]any{"name": "order-1", "payload": 1}}})
	id := first["tasks"].([]any)[0].(map[string]any)["id"].(string)
	again := w.mustDo(t, 409, "POST", "/v1/tasks/jobs/queues/q/tasks", map[string]any{"name": "order-1", "payload": 2})
	r := again["tasks"].([]any)[0].(map[string]any)
	if r["ok"] != false || r["code"] != "TASK_EXISTS" || r["id"] != id {
		t.Fatalf("dedupe = %v", r)
	}
	mixed := w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues/q/tasks", []any{map[string]any{"name": "order-1", "payload": 1}, map[string]any{"payload": 3}})
	if res := mixed["tasks"].([]any); res[0].(map[string]any)["ok"] != false || res[1].(map[string]any)["ok"] != true {
		t.Fatalf("mixed = %v", res)
	}
	if out := w.mustDo(t, 404, "POST", "/v1/tasks/jobs/queues/ghost/tasks", map[string]any{"payload": 1}); errCode(out) != "QUEUE_NOT_FOUND" {
		t.Fatalf("missing queue: %v", out)
	}
}

func TestPushDeliversToAFunction(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	id := w.addOne(t, "q", map[string]any{"name": "n1", "payload": map[string]any{"b": 1, "a": 2}, "params": map[string]any{"p": true}})
	w.pump()
	calls := w.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.path != "/fn/app/work" || c.trigger != "task" {
		t.Fatalf("call = %s %s", c.path, c.trigger)
	}
	if c.header.Get("x-task-id") != id || c.header.Get("x-task-queue") != "q" || c.header.Get("x-task-attempt") != "1" {
		t.Fatalf("headers = %v", c.header)
	}
	task := c.body["task"].(map[string]any)
	if task["id"] != id || task["name"] != "n1" || task["attempt"] != float64(1) || task["maxAttempts"] != float64(10) ||
		task["source"] != "api" || task["params"].(map[string]any)["p"] != true {
		t.Fatalf("envelope = %v", task)
	}
	if c.body["payload"].(map[string]any)["a"] != float64(2) {
		t.Fatalf("payload = %v", c.body["payload"])
	}
	got := w.task(t, id)
	if got["state"] != "done" || got["lastStatus"] != "http:200" || got["result"] != `{"ok":true}` || got["attempts"] != float64(1) {
		t.Fatalf("task = %v", got)
	}
	log := got["attemptLog"].([]any)
	if len(log) != 1 || log[0].(map[string]any)["status"] != "http:200" {
		t.Fatalf("attempts = %v", log)
	}
}

func TestDelayedTasksWait(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	id := w.addOne(t, "q", map[string]any{"payload": 1, "delay": 60})
	w.pump()
	if len(w.calls()) != 0 || w.task(t, id)["state"] != "scheduled" {
		t.Fatal("a delayed task ran early")
	}
	w.advance(60_000)
	w.pump()
	if len(w.calls()) != 1 || w.task(t, id)["state"] != "done" {
		t.Fatalf("delayed task did not run: %v", w.task(t, id))
	}
}

func TestRetriesBackOffThenFailIntoTheDeadLetterQueue(t *testing.T) {
	w := newWorld(t)
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "dead", "mode": "pull"})
	w.pushQueue(t, "q", map[string]any{"deadLetterQueue": "dead", "retry": map[string]any{"maxAttempts": 3, "minBackoffSeconds": 10}})
	w.answer = func(int) (int, http.Header, string) { return 503, nil, "busy" }
	id := w.addOne(t, "q", map[string]any{"payload": map[string]any{"x": 1}})
	start := w.clock
	w.pump()
	got := w.task(t, id)
	if got["state"] != "scheduled" || got["attempts"] != float64(1) || got["lastStatus"] != "http:503" || got["lastError"] != "busy" {
		t.Fatalf("after one failure: %v", got)
	}
	// 10s after the first failure, then 20s after the second.
	if got["runAt"] != float64(start+10_000) {
		t.Fatalf("runAt = %v, want %d", got["runAt"], start+10_000)
	}
	w.advance(10_000)
	w.pump()
	if got = w.task(t, id); got["runAt"] != float64(start+10_000+20_000) {
		t.Fatalf("second backoff: %v", got["runAt"])
	}
	w.advance(20_000)
	w.pump()
	got = w.task(t, id)
	if got["state"] != "failed" || got["attempts"] != float64(3) || len(w.calls()) != 3 {
		t.Fatalf("after the last attempt: %v (%d calls)", got, len(w.calls()))
	}
	// The failed task's copy waits in the dead-letter queue, with the same payload.
	page := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues/dead/tasks?state=scheduled", nil)
	list := page["tasks"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["deadLetterOf"] != id || list[0].(map[string]any)["source"] != "deadletter" {
		t.Fatalf("dead letters = %v", list)
	}
	// A queue that is another's dead-letter queue cannot be deleted.
	if out := w.mustDo(t, 409, "DELETE", "/v1/tasks/jobs/queues/dead", nil); errCode(out) != "QUEUE_IN_USE" {
		t.Fatalf("delete dlq: %v", out)
	}
}

func TestAFourHundredFailsAtOnce(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	for _, status := range []int{400, 408, 429} {
		w.answer = func(int) (int, http.Header, string) { return status, nil, "" }
		id := w.addOne(t, "q", map[string]any{"payload": status})
		w.pump()
		state := w.task(t, id)["state"]
		want := "failed"
		if status != 400 {
			want = "scheduled"
		}
		if state != want {
			t.Errorf("%d: state %v, want %s", status, state, want)
		}
	}
}

func TestTheTargetSteersItsRetries(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	// defer: not done, not failed, and not an attempt.
	w.answer = func(int) (int, http.Header, string) {
		return 202, hdr("x-task-retry", "defer", "retry-after", "30"), ""
	}
	id := w.addOne(t, "q", map[string]any{"payload": 1})
	w.pump()
	got := w.task(t, id)
	if got["state"] != "scheduled" || got["attempts"] != float64(0) || got["defers"] != float64(1) || got["runAt"] != float64(w.clock+30_000) {
		t.Fatalf("deferred: %v", got)
	}

	// never: a 500 that will not get better.
	w.answer = func(int) (int, http.Header, string) { return 500, hdr("x-task-retry", "never"), "" }
	id = w.addOne(t, "q", map[string]any{"payload": 2})
	w.pump()
	if got = w.task(t, id); got["state"] != "failed" || got["attempts"] != float64(1) {
		t.Fatalf("never: %v", got)
	}

	// retry on a 2xx, at the time asked for, with this task's attempt limit lowered.
	w.answer = func(int) (int, http.Header, string) {
		return 200, hdr("x-task-retry", "retry", "x-task-retry-after", "5", "x-task-max-attempts", "2"), ""
	}
	id = w.addOne(t, "q", map[string]any{"payload": 3})
	w.pump()
	got = w.task(t, id)
	if got["state"] != "scheduled" || got["runAt"] != float64(w.clock+5_000) || got["maxAttempts"] != float64(2) {
		t.Fatalf("retry: %v", got)
	}
	w.advance(5_000)
	w.pump()
	if got = w.task(t, id); got["state"] != "failed" || got["attempts"] != float64(2) {
		t.Fatalf("retry limit: %v", got)
	}

	// With targetControl off the headers mean nothing.
	w.mustDo(t, 200, "PATCH", "/v1/tasks/jobs/queues/q", map[string]any{"targetControl": false})
	w.answer = func(int) (int, http.Header, string) { return 200, hdr("x-task-retry", "retry"), "" }
	id = w.addOne(t, "q", map[string]any{"payload": 4})
	w.pump()
	if got = w.task(t, id); got["state"] != "done" {
		t.Fatalf("targetControl off: %v", got)
	}
}

func TestPausedQueuesHoldTheirTasks(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/q/pause", nil)
	id := w.addOne(t, "q", map[string]any{"payload": 1})
	w.pump()
	if len(w.calls()) != 0 {
		t.Fatal("a paused queue delivered")
	}
	out := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/q/resume", nil)
	if out["queue"].(map[string]any)["settings"].(map[string]any)["paused"] != false {
		t.Fatalf("resume = %v", out)
	}
	w.pump()
	if w.task(t, id)["state"] != "done" {
		t.Fatal("resumed queue did not deliver")
	}
}

func TestConcurrencyAndRateBoundDelivery(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", map[string]any{"ratePerSecond": 2, "maxConcurrent": 50})
	for i := 0; i < 6; i++ {
		w.addOne(t, "q", map[string]any{"payload": i})
	}
	w.pump()
	// The bucket holds max(1, rate) tokens, refilled at the rate.
	if n := len(w.calls()); n != 2 {
		t.Fatalf("first second: %d deliveries, want 2", n)
	}
	w.advance(1_000)
	w.pump()
	if n := len(w.calls()); n != 4 {
		t.Fatalf("after a second: %d deliveries, want 4", n)
	}
}

func TestCancelRunAndRerun(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	id := w.addOne(t, "q", map[string]any{"payload": 1, "delay": 600})
	w.mustDo(t, 200, "DELETE", "/v1/tasks/jobs/tasks/"+id, nil)
	if got := w.task(t, id); got["state"] != "cancelled" {
		t.Fatalf("cancel: %v", got)
	}
	out := w.mustDo(t, 409, "DELETE", "/v1/tasks/jobs/tasks/"+id, nil)
	if errMsg(out) != "a cancelled task cannot be cancelled" {
		t.Fatalf("cancel twice: %v", out)
	}
	w.mustDo(t, 404, "DELETE", "/v1/tasks/jobs/tasks/nope", nil)
	many := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/cancel", map[string]any{"ids": []any{id, "nope"}})
	sk := many["skipped"].([]any)
	if len(many["cancelled"].([]any)) != 0 || len(sk) != 2 || sk[0].(map[string]any)["state"] != "cancelled" || sk[1].(map[string]any)["state"] != nil {
		t.Fatalf("cancel many = %v", many)
	}

	// Run a cancelled task: back to scheduled, now.
	got := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+id+"/run", nil)["task"].(map[string]any)
	if got["state"] != "scheduled" || got["lastStatus"] != "requeued" || got["runAt"] != float64(w.clock) {
		t.Fatalf("run cancelled: %v", got)
	}
	w.pump()
	if w.task(t, id)["state"] != "done" {
		t.Fatal("re-run did not deliver")
	}
	// Run a finished one: a NEW task with the same payload; the old record stays history.
	again := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+id+"/run", nil)["task"].(map[string]any)
	if again["id"] == id || again["source"] != "rerun" || again["payload"] != float64(1) {
		t.Fatalf("rerun: %v", again)
	}
}

func TestListingPagesByState(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", map[string]any{"paused": true})
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, w.addOne(t, "q", map[string]any{"payload": i, "delay": i}))
		// Ids are time-ordered by the millisecond; two in the same one order at random.
		w.advance(1)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/v1/tasks/jobs/queues/q/tasks?state=scheduled&limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		out := w.mustDo(t, 200, "GET", path, nil)
		for _, x := range out["tasks"].([]any) {
			seen = append(seen, x.(map[string]any)["id"].(string))
			if _, has := x.(map[string]any)["payload"]; has {
				t.Fatal("a listing must not carry payloads")
			}
		}
		c, _ := out["cursor"].(string)
		if c == "" {
			break
		}
		cursor = c
	}
	if strings.Join(seen, ",") != strings.Join(ids, ",") {
		t.Fatalf("scheduled, soonest first: %v, want %v", seen, ids)
	}
	// Without a state: newest first.
	out := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues/q/tasks?limit=1", nil)
	if out["tasks"].([]any)[0].(map[string]any)["id"] != ids[4] {
		t.Fatalf("newest first: %v", out)
	}
	if out := w.mustDo(t, 400, "GET", "/v1/tasks/jobs/queues/q/tasks?state=lost", nil); !strings.Contains(errMsg(out), "state must be one of") {
		t.Fatalf("bad state: %v", out)
	}
	counts := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues/q", nil)["queue"].(map[string]any)["counts"].(map[string]any)
	if counts["scheduled"] != float64(5) {
		t.Fatalf("counts = %v", counts)
	}
	purged := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/q/purge", nil)
	if purged["purged"] != float64(5) || purged["pending"] != false {
		t.Fatalf("purge = %v", purged)
	}
	w.mustDo(t, 200, "DELETE", "/v1/tasks/jobs/queues/q", nil)
	w.mustDo(t, 404, "GET", "/v1/tasks/jobs/queues/q", nil)
}

func TestPullQueuesLeaseAckAndRelease(t *testing.T) {
	w := newWorld(t)
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "work", "mode": "pull", "retry": map[string]any{"maxAttempts": 2}})
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/default/lease", map[string]any{}); errCode(out) != "NOT_PULL_QUEUE" {
		t.Fatalf("lease push: %v", out)
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/work/tasks", map[string]any{"payload": 1, "target": map[string]any{"function": "app/work"}}); !strings.Contains(errMsg(out), "pull queue") {
		t.Fatalf("target on pull: %v", out)
	}
	a := w.addOne(t, "work", map[string]any{"payload": "a"})
	b := w.addOne(t, "work", map[string]any{"payload": "b"})
	w.pump()
	if len(w.calls()) != 0 {
		t.Fatal("a pull queue delivered")
	}
	leased := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/work/lease", map[string]any{"count": 5, "leaseSeconds": 60})["tasks"].([]any)
	if len(leased) != 2 {
		t.Fatalf("leased %d", len(leased))
	}
	first := leased[0].(map[string]any)
	lease := first["leaseId"].(string)
	if first["id"] != a || first["payload"] != "a" || first["state"] != "leased" || first["attempts"] != float64(1) {
		t.Fatalf("leased = %v", first)
	}
	// Nothing left to lease.
	if more := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/work/lease", nil)["tasks"].([]any); len(more) != 0 {
		t.Fatalf("leased twice: %v", more)
	}
	if out := w.mustDo(t, 409, "POST", "/v1/tasks/jobs/tasks/"+a+"/ack", map[string]any{"leaseId": "wrong"}); errCode(out) != "LEASE_LOST" {
		t.Fatalf("wrong lease: %v", out)
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/tasks/"+a+"/ack", map[string]any{}); !strings.Contains(errMsg(out), "leaseId") {
		t.Fatalf("no lease: %v", out)
	}
	w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+a+"/extend", map[string]any{"leaseId": lease, "leaseSeconds": 120})
	acked := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+a+"/ack", map[string]any{"leaseId": lease, "result": map[string]any{"ok": 1}})["task"].(map[string]any)
	if acked["state"] != "done" || acked["result"] != `{"ok":1}` || acked["lastStatus"] != "acked" {
		t.Fatalf("ack = %v", acked)
	}
	// Release: back after the delay, the attempt counted as failed.
	rel := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+b+"/release", map[string]any{"leaseId": lease, "delaySeconds": 30, "error": "not now"})["task"].(map[string]any)
	if rel["state"] != "scheduled" || rel["runAt"] != float64(w.clock+30_000) || rel["lastError"] != "not now" || rel["lastStatus"] != "released" {
		t.Fatalf("release = %v", rel)
	}
	// A lease that runs out puts the task back with the attempt spent — here its last.
	w.advance(30_000)
	again := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/work/lease", map[string]any{"leaseSeconds": 10})["tasks"].([]any)
	if len(again) != 1 || again[0].(map[string]any)["attempts"] != float64(2) {
		t.Fatalf("re-lease = %v", again)
	}
	w.advance(11_000)
	w.pump()
	got := w.task(t, b)
	if got["state"] != "failed" || got["lastStatus"] != "lost" || got["lastError"] != "the lease expired without an ack" {
		t.Fatalf("expired lease = %v", got)
	}
}

func TestChannelAndWebhookTargets(t *testing.T) {
	w := newWorld(t)
	if _, err := w.reg.Create("channel", "live"); err != nil {
		t.Fatal(err)
	}
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "ch", "target": map[string]any{"channel": "live/updates"}})
	id := w.addOne(t, "ch", map[string]any{"payload": 1})
	w.pump()
	if got := w.task(t, id); got["state"] != "done" || got["lastStatus"] != "published:0" {
		t.Fatalf("channel: %v", got)
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "x", "target": map[string]any{"channel": "nope/updates"}}); !strings.Contains(errMsg(out), "no channel instance 'nope'") {
		t.Fatalf("missing channel: %v", out)
	}

	var got struct {
		sig, ts string
		body    []byte
	}
	recv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got.sig, got.ts = r.Header.Get(SignatureHeader), r.Header.Get(TimestampHeader)
		got.body, _ = io.ReadAll(r.Body)
		rw.WriteHeader(204)
	}))
	defer recv.Close()
	w.svc.SetHTTPClient(recv.Client())
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "hook", "target": map[string]any{"webhook": recv.URL + "/in"}})
	id = w.addOne(t, "hook", map[string]any{"payload": map[string]any{"k": "v"}})
	w.pump()
	if st := w.task(t, id); st["state"] != "done" || st["lastStatus"] != "http:204" {
		t.Fatalf("webhook: %v", st)
	}
	secret := w.mustDo(t, 200, "POST", "/admin/tasks/jobs/webhook-secret/reveal", nil)["secret"].(string)
	ts, _ := strconv.ParseInt(got.ts, 10, 64)
	if !strings.HasPrefix(secret, "whsec_") || got.sig != SignWebhook(secret, ts, got.body) {
		t.Fatalf("signature %q does not verify", got.sig)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["task"].(map[string]any)["id"] != id || body["payload"].(map[string]any)["k"] != "v" {
		t.Fatalf("webhook body = %s", got.body)
	}
	rotated := w.mustDo(t, 200, "POST", "/admin/tasks/jobs/webhook-secret/rotate", nil)
	if rotated["secret"] == secret || rotated["rotated"] != true {
		t.Fatalf("rotate = %v", rotated)
	}
}

// A channel target is a publish: naming one takes a channel write grant, as publishing does.
func TestAChannelTargetNeedsAChannelGrant(t *testing.T) {
	w := newWorld(t)
	if _, err := w.reg.Create("channel", "live"); err != nil {
		t.Fatal(err)
	}
	w.auth.AddKey("tasks-only", auth.Grants{"tasks": "full", "channel:live": "read"})
	w.auth.AddKey("both", auth.Grants{"tasks": "full", "channel:live": "write"})
	target := map[string]any{"channel": "live/updates"}
	code, out := w.doAs(t, "tasks-only", "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "ch", "target": target})
	if code != 403 || errMsg(out) != "API key lacks 'write' access to channel 'live' (only has 'read')" {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := w.doAs(t, "both", "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "ch", "target": target}); code != 201 {
		t.Fatalf("create with the grant: %d %v", code, out)
	}
	w.pushQueue(t, "fn", nil)
	code, out = w.doAs(t, "tasks-only", "POST", "/v1/tasks/jobs/queues/fn/tasks", map[string]any{"payload": 1, "target": target})
	if code != 403 || errCode(out) != "PERMISSION_DENIED" {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, out := w.doAs(t, "tasks-only", "POST", "/v1/tasks/jobs/queues/fn/tasks", map[string]any{"payload": 1}); code != 201 {
		t.Fatalf("a function target needs no second grant: %d %v", code, out)
	}
	// Pausing a queue whose target is a channel names no target.
	if code, out := w.doAs(t, "tasks-only", "POST", "/v1/tasks/jobs/queues/ch/pause", nil); code != 200 {
		t.Fatalf("pause: %d %v", code, out)
	}
}

func TestAnAddThatLandsNothingSaysWhy(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	w.addOne(t, "q", map[string]any{"name": "once", "payload": 1})
	out := w.mustDo(t, 409, "POST", "/v1/tasks/jobs/queues/q/tasks", map[string]any{"name": "once", "payload": 1})
	if errCode(out) != "TASK_EXISTS" || !strings.Contains(errMsg(out), "already exists in queue 'q'") || len(out["tasks"].([]any)) != 1 {
		t.Fatalf("409 = %v", out)
	}
	if ok := w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues/q/tasks", map[string]any{"payload": 2}); ok["error"] != nil {
		t.Fatalf("201 carries an error: %v", ok)
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	w.answer = func(int) (int, http.Header, string) { return 302, http.Header{"Location": {"/elsewhere"}}, "" }
	id := w.addOne(t, "q", map[string]any{"payload": 1})
	w.pump()
	if got := w.task(t, id); got["state"] != "failed" || got["lastStatus"] != "http:302" {
		t.Fatalf("redirect: %v", got)
	}
}

// One deadline for every kind of target: the queue's deadlineSeconds, or the task's own. The
// target is told when it is, and past it the attempt counts as failed and is retried.
func TestTheDeliveryDeadline(t *testing.T) {
	w := newWorld(t)
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "q", "deadlineSeconds": 14}); errMsg(out) != "deadlineSeconds must be an integer from 15 to 840" {
		t.Fatalf("deadline 14: %v", out)
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "q", "deadlineSeconds": 841}); !strings.Contains(errMsg(out), "from 15 to 840") {
		t.Fatalf("deadline 841: %v", out)
	}
	if st := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues/default", nil)["queue"].(map[string]any)["settings"].(map[string]any); st["deadlineSeconds"] != float64(600) {
		t.Fatalf("default deadline = %v", st["deadlineSeconds"])
	}
	w.pushQueue(t, "q", map[string]any{"deadlineSeconds": 30})
	// A change that does not name it keeps it.
	if st := w.mustDo(t, 200, "PATCH", "/v1/tasks/jobs/queues/q", map[string]any{"maxConcurrent": 2})["queue"].(map[string]any)["settings"].(map[string]any); st["deadlineSeconds"] != float64(30) {
		t.Fatalf("patched deadline = %v", st["deadlineSeconds"])
	}
	if out := w.mustDo(t, 400, "POST", "/v1/tasks/jobs/queues/q/tasks", map[string]any{"payload": 1, "deadlineSeconds": 5}); errMsg(out) != "task.deadlineSeconds must be an integer from 15 to 840" {
		t.Fatalf("task deadline 5: %v", out)
	}

	queueDeadline := w.addOne(t, "q", map[string]any{"payload": 1})
	own := w.addOne(t, "q", map[string]any{"payload": 2, "deadlineSeconds": 90})
	if got := w.task(t, queueDeadline); got["deadlineSeconds"] != nil {
		t.Fatalf("a task without its own deadline reads null: %v", got["deadlineSeconds"])
	}
	if got := w.task(t, own); got["deadlineSeconds"] != float64(90) {
		t.Fatalf("own deadline = %v", got["deadlineSeconds"])
	}
	claimed := w.clock
	w.pump()
	deadlines := map[string]float64{}
	for _, c := range w.calls() {
		task := c.body["task"].(map[string]any)
		deadlines[task["id"].(string)] = task["deadlineAt"].(float64)
	}
	if deadlines[queueDeadline] != float64(claimed+30_000) || deadlines[own] != float64(claimed+90_000) {
		t.Fatalf("deadlineAt = %v (claimed at %d)", deadlines, claimed)
	}
	// A re-run keeps the task's own deadline.
	if again := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/tasks/"+own+"/run", nil)["task"].(map[string]any); again["deadlineSeconds"] != float64(90) {
		t.Fatalf("re-run deadline = %v", again["deadlineSeconds"])
	}

	// Past the deadline the queue stops waiting: the attempt failed, and it is retried.
	w.svc.waitCap = 50 * time.Millisecond
	w.answer = func(int) (int, http.Header, string) {
		time.Sleep(500 * time.Millisecond)
		return 200, nil, ""
	}
	slow := w.addOne(t, "q", map[string]any{"payload": 3})
	w.pump()
	got := w.task(t, slow)
	if got["state"] != "scheduled" || got["lastStatus"] != "timeout" || got["lastError"] != "no answer within the 30s deadline" {
		t.Fatalf("timed out: %v", got)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A deadline stops the queue WAITING; it cannot stop the call. So past it the attempt is recorded
// as a timeout and its retry scheduled, but the task's slot stays taken until the call returns:
// maxConcurrent is a true ceiling, a task never runs beside itself, and the timed-out attempt
// records what the target finally answered.
func TestALateCallKeepsItsSlot(t *testing.T) {
	w := newWorld(t)
	w.svc.waitCap = 30 * time.Millisecond
	release := make(chan struct{})
	w.answer = func(n int) (int, http.Header, string) {
		c := w.calls()[n-1]
		// Each "slow" task's first call outlives its deadline; everything else answers at once.
		if c.body["payload"] == "slow" && c.header.Get("x-task-attempt") == "1" {
			<-release
		}
		return 200, nil, "ok"
	}
	retry := map[string]any{"minBackoffSeconds": 1}
	w.pushQueue(t, "one", map[string]any{"maxConcurrent": 1, "deadlineSeconds": 15, "retry": retry})
	w.pushQueue(t, "two", map[string]any{"maxConcurrent": 2, "deadlineSeconds": 15, "retry": retry})
	a := w.addOne(t, "one", map[string]any{"payload": "slow"})
	c := w.addOne(t, "two", map[string]any{"payload": "slow"})
	w.svc.Tick()
	for _, id := range []string{a, c} {
		id := id
		eventually(t, "the timeout to be recorded", func() bool { return w.task(t, id)["lastStatus"] == "timeout" })
	}
	if got := w.task(t, a); got["state"] != "scheduled" || got["lastError"] != "no answer within the 15s deadline" {
		t.Fatalf("timed-out task: %v", got)
	}
	callsFor := func(id string) int {
		n := 0
		for _, call := range w.calls() {
			if call.header.Get("x-task-id") == id {
				n++
			}
		}
		return n
	}

	// Both retries are due now, and so are two new tasks.
	b := w.addOne(t, "one", map[string]any{"payload": "fast"})
	d := w.addOne(t, "two", map[string]any{"payload": "fast"})
	w.advance(2_000)
	w.svc.Tick()
	eventually(t, "the free slot in 'two' to be used", func() bool { return w.task(t, d)["state"] == "done" })
	w.svc.Tick()
	time.Sleep(50 * time.Millisecond)
	// 1. 'one' runs one call at a time: its only slot is still held by the late call.
	if n := callsFor(b); n != 0 || w.task(t, b)["state"] != "scheduled" {
		t.Fatalf("a maxConcurrent-1 queue ran a second call beside a late one (%d calls)", n)
	}
	// 2. 'two' had a slot free and used it — but not for the task whose own call is still running.
	if n := callsFor(c); n != 1 {
		t.Fatalf("a task ran beside its own late call: %d calls", n)
	}

	// 3. When the late calls return, the slots free, the retries run, and the timed-out attempt
	// says what the target answered and when.
	w.advance(3_000)
	close(release)
	w.svc.Wait()
	for _, id := range []string{a, c} {
		log := w.task(t, id)["attemptLog"].([]any)
		if e := log[0].(map[string]any)["error"]; e != "no answer within the 15s deadline; the target answered http:200 after 5s" {
			t.Fatalf("attempt 1 of %s: %v", id, e)
		}
	}
	w.pump()
	for _, id := range []string{a, b, c} {
		if got := w.task(t, id); got["state"] != "done" {
			t.Fatalf("after the late calls returned, %s is %v", id, got["state"])
		}
	}
	if callsFor(a) != 2 || callsFor(c) != 2 || callsFor(b) != 1 {
		t.Fatalf("calls: a=%d b=%d c=%d", callsFor(a), callsFor(b), callsFor(c))
	}
}

// The hold is bounded: a receiver that never answers keeps its task's slot only until the hold's
// budget (14.5 minutes from the attempt's start) runs out. Then the request is cancelled, the
// slot is freed, and the attempt says the target was still running.
func TestALateCallsHoldIsBounded(t *testing.T) {
	w := newWorld(t)
	w.svc.waitCap = 30 * time.Millisecond
	w.svc.holdCap = 300 * time.Millisecond
	cancelled := make(chan struct{})
	var slowOnce sync.Once
	recv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			first := false
			slowOnce.Do(func() { first = true })
			if first {
				// Never answers: it only learns the request was given up on. (The body is read
				// first: only then can the server notice the client hanging up.)
				_, _ = io.ReadAll(r.Body)
				<-r.Context().Done()
				close(cancelled)
				return
			}
		}
		rw.WriteHeader(204)
	}))
	defer recv.Close()
	w.svc.SetHTTPClient(recv.Client())
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{
		"name": "hooks", "target": map[string]any{"webhook": recv.URL + "/fast"},
		"maxConcurrent": 1, "deadlineSeconds": 15, "retry": map[string]any{"minBackoffSeconds": 1},
	})
	a := w.addOne(t, "hooks", map[string]any{"payload": 1, "target": map[string]any{"webhook": recv.URL + "/slow"}})
	w.svc.Tick()
	eventually(t, "the timeout to be recorded", func() bool { return w.task(t, a)["lastStatus"] == "timeout" })

	// While the hold lasts, the queue's only slot is taken.
	b := w.addOne(t, "hooks", map[string]any{"payload": 2})
	w.advance(2_000)
	w.svc.Tick()
	time.Sleep(50 * time.Millisecond)
	if got := w.task(t, b); got["state"] != "scheduled" || got["attempts"] != float64(0) {
		t.Fatalf("a second call started beside a held one: %v", got)
	}

	// When it runs out: the request is cancelled, the slot freed, and the attempt says why.
	w.svc.Wait()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the request outlived the hold: the receiver never saw it cancelled")
	}
	log := w.task(t, a)["attemptLog"].([]any)
	if e := log[0].(map[string]any)["error"]; e != "no answer within the 15s deadline; still running when this run of the queue ended (2s)" {
		t.Fatalf("attempt 1: %v", e)
	}
	w.pump()
	for _, id := range []string{a, b} {
		if got := w.task(t, id); got["state"] != "done" {
			t.Fatalf("after the hold ran out, %s is %v (%v)", id, got["state"], got["lastError"])
		}
	}
}

func TestATargetThatIsGoneFailsAtOnce(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", nil)
	id := w.addOne(t, "q", map[string]any{"payload": 1, "target": map[string]any{"function": "gone/work"}})
	w.pump()
	got := w.task(t, id)
	if got["state"] != "failed" || got["lastError"] != "no functions instance 'gone'" {
		t.Fatalf("gone target: %v", got)
	}
}

func TestDeletingTheInstanceDropsItsTasks(t *testing.T) {
	w := newWorld(t)
	w.pushQueue(t, "q", map[string]any{"paused": true})
	w.addOne(t, "q", map[string]any{"payload": 1})
	in := w.reg.Get("tasks", "jobs")
	if !w.reg.DeleteInstance("tasks", in.ID) {
		t.Fatal("delete failed")
	}
	// The name auto-creates a fresh instance on next use, with only its default queue.
	out := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues", nil)
	if qs := out["queues"].([]any); len(qs) != 1 {
		t.Fatalf("queues after delete = %v", qs)
	}
}

func TestRetentionIsValidated(t *testing.T) {
	w := newWorld(t)
	in := w.reg.GetOrCreate("tasks", "jobs")
	if err := w.reg.SaveConfig(in, map[string]any{"retentionDays": 31}); err == nil || !strings.Contains(err.Error(), "retentionDays must be an integer from 1 to 30") {
		t.Fatalf("retention 31: %v", err)
	}
	if err := w.reg.SaveConfig(in, map[string]any{"retentionDays": 3}); err != nil {
		t.Fatal(err)
	}
	if retentionDays(in) != 3 {
		t.Fatalf("retention = %d", retentionDays(in))
	}
}
