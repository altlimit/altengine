package tasks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/mailbox"
)

// triggerWorld is a world with the three sources wired to the tasks service the way the server
// wires them, and a pull queue `jobs/inbox` to read what they fired.
func triggerWorld(t *testing.T) (*world, *mailbox.Service) {
	t.Helper()
	w := newWorld(t)
	a := auth.NewStore(true)
	datastore.NewHandler(w.reg, a, datastore.NewManager("")).WithTriggers(w.svc).Register(w.mux)
	blob.NewHandler(w.reg, a, blob.NewStore("")).WithTriggers(w.svc).Register(w.mux)
	mail := mailbox.NewService(w.reg, mailbox.NewStore(""), w.mux, w.hub, mailbox.Options{}).WithQueues(w.svc)
	mailbox.NewHandler(w.reg, a, mail).Register(w.mux)
	w.reg.OnValidateConfig("datastore", datastore.ValidateConfig)
	w.reg.OnValidateConfig("datastore", w.svc.ValidateDatastoreConfig)
	w.reg.OnValidateConfig("blob", blob.ValidateConfig)
	w.reg.OnValidateConfig("blob", w.svc.ValidateBlobConfig)
	w.reg.OnValidateConfig("mailbox", mailbox.ValidateConfig)
	w.reg.OnValidateConfig("mailbox", w.svc.ValidateMailboxConfig)
	w.mustDo(t, 201, "POST", "/v1/tasks/jobs/queues", map[string]any{"name": "inbox", "mode": "pull"})
	return w, mail
}

// settle lets the events a write fired land, then moves the clock on, so tasks from successive
// writes lease in the order they were written (ids order by the millisecond).
func (w *world) settle() {
	w.svc.Wait()
	w.advance(1)
}

func (w *world) leaseAll(t *testing.T) []map[string]any {
	t.Helper()
	w.svc.Wait()
	out := w.mustDo(t, 200, "POST", "/v1/tasks/jobs/queues/inbox/lease", map[string]any{"count": 100})
	var list []map[string]any
	for _, x := range out["tasks"].([]any) {
		list = append(list, x.(map[string]any))
	}
	return list
}

func TestDatastoreTriggers(t *testing.T) {
	w, _ := triggerWorld(t)
	ds := w.reg.GetOrCreate("datastore", "shop")
	// A rule's queue must exist when it is saved.
	err := w.reg.SaveConfig(ds, map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "orders"}, "queue": "jobs/nope"}}})
	if err == nil || !strings.Contains(err.Error(), "tasks instance 'jobs' has no queue 'nope'") {
		t.Fatalf("missing queue: %v", err)
	}
	err = w.reg.SaveConfig(ds, map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "  "}, "queue": "jobs/inbox"}}})
	if err == nil || !strings.Contains(err.Error(), "config.triggers[0].match.collection is blank") {
		t.Fatalf("blank collection: %v", err)
	}
	err = w.reg.SaveConfig(ds, map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "x", "ops": []any{"upsert"}}, "queue": "jobs/inbox"}}})
	if err == nil || !strings.Contains(err.Error(), "must be a non-empty list of: create, update, delete") {
		t.Fatalf("bad op: %v", err)
	}
	if err := w.reg.SaveConfig(ds, map[string]any{"triggers": []any{
		map[string]any{"match": map[string]any{"collection": "ord*", "namespace": "_default"}, "queue": "jobs/inbox", "params": map[string]any{"via": "rule1"}},
		map[string]any{"match": map[string]any{"collection": "audit", "ops": []any{"delete"}}, "queue": "jobs/inbox", "document": false},
	}}); err != nil {
		t.Fatal(err)
	}

	put := func(coll string, docs ...map[string]any) {
		w.mustDo(t, 200, "POST", "/v1/datastore/shop/ns/_default/col/"+coll+"/documents", map[string]any{"documents": docs})
		w.settle()
	}
	put("orders", map[string]any{"key": "o1", "data": map[string]any{"total": 5}})
	put("orders", map[string]any{"key": "o1", "data": map[string]any{"total": 6}}, map[string]any{"key": "o2", "data": map[string]any{"total": 1}})
	put("unwatched", map[string]any{"key": "u", "data": map[string]any{}})
	put("audit", map[string]any{"key": "a1", "data": map[string]any{"x": 1}})
	w.mustDo(t, 200, "POST", "/v1/datastore/shop/ns/_default/col/audit/documents/delete", map[string]any{"keys": []any{"a1", "missing"}})
	w.settle()
	// A write in another namespace is not this rule's: it named the default one.
	w.mustDo(t, 200, "POST", "/v1/datastore/shop/ns/eu/col/orders/documents", map[string]any{"documents": []any{map[string]any{"key": "e", "data": map[string]any{}}}})
	w.settle()
	w.mustDo(t, 200, "POST", "/v1/datastore/shop/ns/_default/transaction", map[string]any{"operations": []any{
		map[string]any{"op": "mutate", "collection": "orders", "key": "o2", "increment": map[string]any{"total": 1}},
		map[string]any{"op": "delete", "collection": "orders", "key": "o1"},
	}})

	tasks := w.leaseAll(t)
	if len(tasks) != 4 {
		t.Fatalf("fired %d tasks, want 4: %v", len(tasks), tasks)
	}
	var events []string
	for _, tk := range tasks {
		p := tk["payload"].(map[string]any)
		if p["type"] != "datastore.write" || p["instance"] != "shop" || p["namespace"] != "" || tk["source"] != "datastore" || tk["depth"] != float64(1) {
			t.Fatalf("task = %v", tk)
		}
		if !strings.HasPrefix(tk["name"].(string), "datastore-") {
			t.Fatalf("name = %v", tk["name"])
		}
		for _, e := range p["events"].([]any) {
			ev := e.(map[string]any)
			s := p["collection"].(string) + ":" + ev["op"].(string) + ":" + ev["key"].(string)
			if doc, ok := ev["doc"].(map[string]any); ok {
				s += ":" + strings.TrimSuffix(strings.TrimPrefix(mustJSON(doc), "{"), "}")
			}
			if _, ok := ev["before"]; ok {
				s += ":before"
			}
			events = append(events, s)
		}
		if p["collection"] == "orders" {
			if tk["params"].(map[string]any)["via"] != "rule1" {
				t.Fatalf("params = %v", tk["params"])
			}
		}
	}
	want := []string{
		`orders:create:o1:"total":5`,
		`orders:update:o1:"total":6`, `orders:create:o2:"total":1`,
		`audit:delete:a1`,
		`orders:update:o2:"total":2:before`, `orders:delete:o1:before`,
	}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events:\n got %v\nwant %v", events, want)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTriggerChainsAreCut(t *testing.T) {
	w, _ := triggerWorld(t)
	ds := w.reg.GetOrCreate("datastore", "shop")
	if err := w.reg.SaveConfig(ds, map[string]any{"triggers": []any{map[string]any{"match": map[string]any{"collection": "*"}, "queue": "jobs/inbox"}}}); err != nil {
		t.Fatal(err)
	}
	changes := []datastore.WriteChange{{Collection: "c", Key: "k", Op: "create", Doc: json.RawMessage(`{}`)}}
	w.svc.DatastoreWrite(ds, "", changes, MaxTriggerDepth-1)
	w.svc.DatastoreWrite(ds, "", changes, MaxTriggerDepth)
	tasks := w.leaseAll(t)
	if len(tasks) != 1 || tasks[0]["depth"] != float64(MaxTriggerDepth) {
		t.Fatalf("a chain past %d must be cut: %v", MaxTriggerDepth, tasks)
	}
}

func TestLargeWritesAreChunked(t *testing.T) {
	big := strings.Repeat("x", MaxEventDocBytes+1)
	var changes []datastore.WriteChange
	for i := 0; i < 150; i++ {
		changes = append(changes, datastore.WriteChange{Collection: "c", Key: "k", Op: "create", Doc: json.RawMessage(`{"n":1}`)})
	}
	changes = append(changes, datastore.WriteChange{Collection: "c", Key: "big", Op: "create", Doc: json.RawMessage(`"` + big + `"`)})
	out := DatastoreEventTasks([]DatastoreTrigger{{Match: DatastoreMatch{Collection: "c"}, Queue: "j/q"}}, "src", "", changes)
	if len(out) != 2 {
		t.Fatalf("%d tasks, want 2", len(out))
	}
	if n := len(out[0].Payload.(dsPayload).Events); n != MaxEventsPerTask {
		t.Fatalf("first chunk %d events", n)
	}
	last := out[1].Payload.(dsPayload).Events
	if e := last[len(last)-1]; e.Doc != nil || !e.DocTruncated {
		t.Fatalf("an oversized document must be sent as its key: %+v", e)
	}
}

func TestBlobTriggers(t *testing.T) {
	w, _ := triggerWorld(t)
	b := w.reg.GetOrCreate("blob", "files")
	if err := w.reg.SaveConfig(b, map[string]any{"triggers": []any{
		map[string]any{"match": map[string]any{"prefix": "in-", "contentType": "image/*", "ops": []any{"create"}}, "queue": "jobs/inbox"},
		map[string]any{"match": map[string]any{"ops": []any{"delete"}}, "queue": "jobs/inbox", "params": map[string]any{"gone": true}},
	}}); err != nil {
		t.Fatal(err)
	}
	upload := func(name, ct, body string) string {
		minted := w.mustDo(t, 201, "POST", "/v1/blob/files/uploads", map[string]any{"name": name, "size": len(body), "content_type": ct})
		u, _ := url.Parse(minted["upload_url"].(string))
		req := httptest.NewRequest("PUT", u.RequestURI(), bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		w.mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("upload %s: %d %s", name, rec.Code, rec.Body.String())
		}
		w.settle()
		return minted["id"].(string)
	}
	pic := upload("in-cat.png", "image/png", "png!")
	upload("in-notes.txt", "text/plain", "hi")
	upload("out-dog.png", "image/png", "png")

	tasks := w.leaseAll(t)
	if len(tasks) != 1 {
		t.Fatalf("fired %d, want 1: %v", len(tasks), tasks)
	}
	created := tasks[0]["payload"].(map[string]any)
	if created["type"] != "blob.create" || created["name"] != "in-cat.png" || created["size"] != float64(4) || created["instance"] != "files" ||
		created["key"] != "blob:"+b.ID+":"+pic || !strings.Contains(created["url"].(string), "/_blob/") || tasks[0]["source"] != "blob" {
		t.Fatalf("create = %v", created)
	}
	// The link in the event downloads the bytes with no key.
	u, _ := url.Parse(created["url"].(string))
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, httptest.NewRequest("GET", u.RequestURI(), nil))
	if rec.Body.String() != "png!" {
		t.Fatalf("event url served %d %q", rec.Code, rec.Body.String())
	}

	w.mustDo(t, 200, "POST", "/v1/blob/files/delete", map[string]any{"ids": []any{pic}})
	tasks = w.leaseAll(t)
	if len(tasks) != 1 {
		t.Fatalf("delete fired %d, want 1: %v", len(tasks), tasks)
	}
	deleted := tasks[0]["payload"].(map[string]any)
	if deleted["type"] != "blob.delete" || deleted["id"] != pic || deleted["url"] != nil || tasks[0]["params"].(map[string]any)["gone"] != true {
		t.Fatalf("delete = %v", tasks[0])
	}
}

func TestMailboxRoutesHandMailToAQueue(t *testing.T) {
	w, mail := triggerWorld(t)
	mb := w.reg.GetOrCreate("mailbox", "desk")
	err := w.reg.SaveConfig(mb, map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"queue": "ghost/inbox"}}}}})
	if err == nil || !strings.Contains(err.Error(), "no tasks instance 'ghost'") {
		t.Fatalf("missing instance: %v", err)
	}
	err = w.reg.SaveConfig(mb, map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"queue": "jobs/Inbox"}}}}})
	if err == nil || !strings.Contains(err.Error(), "queue name must be 1-63 chars") {
		t.Fatalf("bad queue name: %v", err)
	}
	if err := w.reg.SaveConfig(mb, map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"queue": "jobs/inbox"}}}}}); err != nil {
		t.Fatal(err)
	}
	raw := "From: Jane <jane@example.com>\r\nSubject: Invoice\r\nMessage-ID: <i1@example.com>\r\n\r\nplease pay\r\n"
	req := httptest.NewRequest("POST", "/v1/mailbox/desk/inject?to=billing", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("inject: %d %s", rec.Code, rec.Body.String())
	}
	var injected struct {
		Message struct{ ID string } `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &injected)
	mail.Wait()

	m, _ := mail.Store().Get(mb.ID, injected.Message.ID)
	if len(m.Deliveries) != 1 || m.Deliveries[0].Action != "queue:jobs/inbox" || m.Deliveries[0].Status != "ok:queued" {
		t.Fatalf("deliveries = %+v", m.Deliveries)
	}
	tasks := w.leaseAll(t)
	if len(tasks) != 1 {
		t.Fatalf("fired %d", len(tasks))
	}
	p := tasks[0]["payload"].(map[string]any)
	if p["type"] != "mailbox.received" || p["subject"] != "Invoice" || p["text"] != "please pay\r\n" && p["text"] != "please pay" || tasks[0]["source"] != "mailbox" {
		t.Fatalf("task = %v", tasks[0])
	}
	if !strings.HasPrefix(tasks[0]["name"].(string), "mail-"+injected.Message.ID+"-") {
		t.Fatalf("name = %v", tasks[0]["name"])
	}
	// A replayed delivery lands on the same name and is refused as a duplicate — still delivered.
	if _, err := mail.Replay(mb, injected.Message.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = mail.Store().Get(mb.ID, injected.Message.ID)
	if m.Deliveries[0].Status != "ok:queued" {
		t.Fatalf("replay = %+v", m.Deliveries)
	}
	page := w.mustDo(t, 200, "GET", "/v1/tasks/jobs/queues/inbox/tasks", nil)
	if n := len(page["tasks"].([]any)); n != 1 {
		t.Fatalf("a replay added the message twice: %d tasks", n)
	}
	_ = http.StatusOK
}
