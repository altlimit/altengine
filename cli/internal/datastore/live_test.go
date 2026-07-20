package datastore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
)

// fakePublisher records what the live bridge publishes.
type fakePublisher struct {
	mu   sync.Mutex
	sent []published
}

type published struct {
	instanceID string
	channel    string
	data       map[string]any
}

func (f *fakePublisher) Publish(instanceID, channel string, data json.RawMessage) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	f.sent = append(f.sent, published{instanceID: instanceID, channel: channel, data: m})
	return 1, nil
}

func (f *fakePublisher) events() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.sent...)
}

func newLiveEnv(t *testing.T) (*httptest.Server, *control.Registry, *fakePublisher) {
	t.Helper()
	reg, _ := control.New("")
	pub := &fakePublisher{}
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewManager("")).WithLive(pub).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	inst := reg.GetOrCreate("datastore", "appdb")
	reg.SetConfig(inst, map[string]any{"live": map[string]any{
		"channelInstance": "appfeed",
		"collections": map[string]any{
			"posts":  map[string]any{"keyBy": "board"},
			"alerts": map[string]any{},
		},
	}})
	return srv, reg, pub
}

func post(t *testing.T, srv *httptest.Server, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/datastore/appdb/ns/main/col"+path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer devkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestLiveBridgePublishesCommittedChanges(t *testing.T) {
	srv, reg, pub := newLiveEnv(t)

	if code := post(t, srv, "/posts/documents",
		`{"documents":[{"key":"p1","data":{"title":"a","board":"general"}},{"key":"p2","data":{"title":"b","board":"general"}},{"key":"p3","data":{"title":"c","board":"offtopic"}}]}`); code != 200 {
		t.Fatalf("put -> %d", code)
	}
	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected one publish per partition, got %d: %+v", len(events), events)
	}
	byChannel := map[string]published{}
	for _, e := range events {
		byChannel[e.channel] = e
	}
	general, ok := byChannel["posts.general"]
	if !ok {
		t.Fatalf("expected a posts.general event, got %+v", events)
	}
	if general.instanceID != reg.GetOrCreate("channel", "appfeed").ID {
		t.Fatalf("published to the wrong channel instance: %v", general.instanceID)
	}
	if general.data["op"] != "put" || general.data["collection"] != "posts" || general.data["ns"] != "main" {
		t.Fatalf("unexpected event payload: %v", general.data)
	}
	keys, _ := general.data["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("expected the two general keys, got %v", general.data["keys"])
	}
	if _, ok := byChannel["posts.offtopic"]; !ok {
		t.Fatalf("expected a posts.offtopic event, got %+v", events)
	}
	// The event carries keys only — never a document body.
	if _, leaked := general.data["documents"]; leaked {
		t.Fatalf("live event must not carry document bodies: %v", general.data)
	}

	// A delete republishes on the same partition (the body is read before the row goes).
	pub.sent = nil
	if code := post(t, srv, "/posts/documents/delete", `{"keys":["p1"]}`); code != 200 {
		t.Fatalf("delete -> %d", code)
	}
	events = pub.events()
	if len(events) != 1 || events[0].channel != "posts.general" || events[0].data["op"] != "delete" {
		t.Fatalf("unexpected delete events: %+v", events)
	}
}

func TestLiveBridgeSkipsUnlistedAndUnpartitionedCollections(t *testing.T) {
	srv, _, pub := newLiveEnv(t)

	// A collection with no keyBy publishes on the bare collection channel.
	if code := post(t, srv, "/alerts/documents", `{"documents":[{"key":"a1","data":{"level":"high"}}]}`); code != 200 {
		t.Fatalf("put -> %d", code)
	}
	if events := pub.events(); len(events) != 1 || events[0].channel != "alerts" {
		t.Fatalf("expected one 'alerts' event, got %+v", events)
	}

	// A collection that isn't listed in `live` publishes nothing.
	pub.sent = nil
	if code := post(t, srv, "/drafts/documents", `{"documents":[{"key":"d1","data":{"x":1}}]}`); code != 200 {
		t.Fatalf("put -> %d", code)
	}
	if events := pub.events(); len(events) != 0 {
		t.Fatalf("unlisted collection should publish nothing, got %+v", events)
	}

	// Nor does a document missing its partition field.
	if code := post(t, srv, "/posts/documents", `{"documents":[{"key":"p9","data":{"title":"no board"}}]}`); code != 200 {
		t.Fatalf("put -> %d", code)
	}
	if events := pub.events(); len(events) != 0 {
		t.Fatalf("a document without its keyBy field should publish nothing, got %+v", events)
	}
}
