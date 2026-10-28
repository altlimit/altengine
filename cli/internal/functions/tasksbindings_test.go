// env.tasks, exercised the way it is meant to be used: a function adds work for later, a queue
// runs another function with it, and a function that writes what triggers it is cut off.

package functions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/tasks"
)

func newTasksServer(t *testing.T) (*http.ServeMux, *control.Registry, *tasks.Service) {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	svc := tasks.NewService(reg, tasks.NewStore(""), mux, channel.NewHub())
	datastore.NewHandler(reg, a, datastore.NewManager("")).WithTriggers(svc).Register(mux)
	NewHandler(reg, a, memStore(), mux).Register(mux)
	tasks.NewHandler(reg, a, svc).Register(mux)
	reg.OnValidateConfig("datastore", svc.ValidateDatastoreConfig)
	return mux, reg, svc
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// drain runs the queue engine until it stops finding work.
func drain(svc *tasks.Service) {
	for i := 0; i < 20; i++ {
		svc.Tick()
		svc.Wait()
	}
}

func TestTasksBindingAddsWorkAFunctionRuns(t *testing.T) {
	mux, _, svc := newTasksServer(t)
	deployFn(t, mux, "producer", `export default { async fetch(request, env) {
		const res = await env.tasks.add({ instance: "jobs" }, "mail", { name: "welcome-1", payload: { to: "a@example.com" }, params: { kind: "welcome" } });
		const again = await env.tasks.add({ instance: "jobs" }, "mail", [{ name: "welcome-1", payload: {} }]);
		const q = await env.tasks.getQueue({ instance: "jobs" }, "mail");
		return new Response(JSON.stringify({ res, again, scheduled: q.counts.scheduled }));
	} };`, map[string]string{"tasks:jobs": "write"})
	deployFn(t, mux, "sender", `export default { async fetch(request, env) {
		const body = await request.json();
		await env.datastore.put({ instance: "log" }, "sent", [{ key: body.task.id, data: {
			to: body.payload.to, kind: body.task.params.kind, trigger: request.headers.get("x-ae-trigger"),
			attempt: request.headers.get("x-task-attempt"), source: body.task.source,
		} }]);
		return new Response("ok");
	} };`, map[string]string{"datastore:log": "write"})
	if code, out := doJSON(t, mux, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "mail", "target": map[string]any{"function": "main/sender"}}); code != 201 {
		t.Fatalf("create queue: %d %v", code, out)
	}

	rec := invoke(t, mux, "/fn/main/producer")
	if rec.Code != 200 {
		t.Fatalf("producer: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Res       []map[string]any `json:"res"`
		Again     []map[string]any `json:"again"`
		Scheduled float64          `json:"scheduled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Res) != 1 || got.Res[0]["ok"] != true || got.Scheduled != 1 {
		t.Fatalf("add = %s", rec.Body.String())
	}
	// A name already taken comes back as a result, not a throw.
	if len(got.Again) != 1 || got.Again[0]["code"] != "TASK_EXISTS" {
		t.Fatalf("again = %v", got.Again)
	}
	id := got.Res[0]["id"].(string)

	drain(svc)
	code, out := doJSON(t, mux, "POST", "/v1/datastore/log/ns/_default/col/sent/documents/get", map[string]any{"keys": []any{id}})
	docs, _ := out["documents"].([]any)
	if code != 200 || len(docs) != 1 {
		t.Fatalf("the task did not run the function: %d %v", code, out)
	}
	data := docs[0].(map[string]any)["data"].(map[string]any)
	if data["to"] != "a@example.com" || data["kind"] != "welcome" || data["trigger"] != "task" || data["attempt"] != "1" || data["source"] != "function" {
		t.Fatalf("the function saw %v", data)
	}
	_, task := doJSON(t, mux, "GET", "/v1/tasks/jobs/tasks/"+id, nil)
	if st := task["task"].(map[string]any); st["state"] != "done" || st["result"] != "ok" {
		t.Fatalf("task = %v", st)
	}
}

func TestTasksBindingGrants(t *testing.T) {
	mux, _, _ := newTasksServer(t)
	deployFn(t, mux, "reader", `export default { async fetch(request, env) {
		const out = {};
		out.queues = (await env.tasks.queues({ instance: "jobs" })).map((q) => q.name);
		try { await env.tasks.add({ instance: "jobs" }, "default", { payload: 1 }); } catch (e) { out.add = String(e.message || e); }
		try { await env.tasks.createQueue({ instance: "jobs" }, "q2", { mode: "pull" }); } catch (e) { out.create = String(e.message || e); }
		try { await env.tasks.queues({ instance: "other" }); } catch (e) { out.other = String(e.message || e); }
		return new Response(JSON.stringify(out));
	} };`, map[string]string{"tasks:jobs": "read"})
	rec := invoke(t, mux, "/fn/main/reader")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if q, _ := out["queues"].([]any); len(q) != 1 || q[0] != "default" {
		t.Fatalf("queues = %v (%s)", out["queues"], rec.Body.String())
	}
	for _, k := range []string{"add", "create", "other"} {
		if s, _ := out[k].(string); !strings.Contains(s, "PERMISSION_DENIED") && !strings.Contains(strings.ToLower(s), "grant") {
			t.Errorf("%s was not refused: %v", k, out[k])
		}
	}
}

func TestTasksBindingWorksAPullQueue(t *testing.T) {
	mux, _, _ := newTasksServer(t)
	deployFn(t, mux, "worker", `export default { async fetch(request, env) {
		const t = { instance: "jobs" };
		await env.tasks.createQueue(t, "todo", { mode: "pull" });
		await env.tasks.add(t, "todo", [{ payload: "a" }, { payload: "b" }]);
		const leased = await env.tasks.lease(t, "todo", { count: 10, leaseSeconds: 60 });
		const done = await env.tasks.ack(t, leased[0].id, leased[0].leaseId, { handled: leased[0].payload });
		const back = await env.tasks.release(t, leased[1].id, leased[1].leaseId, { delaySeconds: 0, error: "later" });
		const page = await env.tasks.list(t, "todo", { state: "done" });
		const one = await env.tasks.get(t, leased[0].id);
		const cancelled = await env.tasks.cancel(t, [leased[1].id]);
		return new Response(JSON.stringify({ n: leased.length, done: done.state, result: one.result, back: back.state, listed: page.tasks.length, cancelled: cancelled.cancelled.length }));
	} };`, map[string]string{"tasks:jobs": "full"})
	rec := invoke(t, mux, "/fn/main/worker")
	if rec.Code != 200 {
		t.Fatalf("worker: %d %s", rec.Code, rec.Body.String())
	}
	want := `{"n":2,"done":"done","result":"{\"handled\":\"a\"}","back":"scheduled","listed":1,"cancelled":1}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("got  %s\nwant %s", rec.Body.String(), want)
	}
}

// A function that writes the document that triggers it would run for ever. Each event is one
// deeper than the write that fired it, and the chain is cut past the limit.
func TestATriggerLoopIsCut(t *testing.T) {
	mux, reg, svc := newTasksServer(t)
	deployFn(t, mux, "looper", `export default { async fetch(request, env) {
		const body = await request.json();
		await env.datastore.put({ instance: "app" }, "loop", [{ key: "k", data: { depth: body.task.depth } }]);
		return new Response("ok");
	} };`, map[string]string{"datastore:app": "write"})
	if code, out := doJSON(t, mux, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "q", "target": map[string]any{"function": "main/looper"}}); code != 201 {
		t.Fatalf("queue: %d %v", code, out)
	}
	ds := reg.GetOrCreate("datastore", "app")
	if err := reg.SaveConfig(ds, map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "loop"}, "queue": "jobs/q"}}}); err != nil {
		t.Fatal(err)
	}
	if code, out := doJSON(t, mux, "POST", "/v1/datastore/app/ns/_default/col/loop/documents", map[string]any{"documents": []any{map[string]any{"key": "k", "data": map[string]any{"depth": 0}}}}); code != 200 {
		t.Fatalf("seed write: %d %v", code, out)
	}
	drain(svc)
	_, page := doJSON(t, mux, "GET", "/v1/tasks/jobs/queues/q/tasks?limit=50", nil)
	list := page["tasks"].([]any)
	if len(list) != tasks.MaxTriggerDepth {
		t.Fatalf("%d tasks ran, want the chain cut at %d", len(list), tasks.MaxTriggerDepth)
	}
	deepest := 0.0
	for _, x := range list {
		task := x.(map[string]any)
		if task["state"] != "done" {
			t.Fatalf("task = %v", task)
		}
		if d := task["depth"].(float64); d > deepest {
			deepest = d
		}
	}
	if deepest != tasks.MaxTriggerDepth {
		t.Fatalf("deepest task = %v", deepest)
	}
}
