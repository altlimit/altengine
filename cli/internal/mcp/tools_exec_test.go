// Do the tools actually RUN?
//
// The parity test next door checks names and argument spellings, which is what stops an agent's
// workflow breaking on deploy. It cannot tell whether a tool works: it drives a Handler with a
// bare mux, so every tool that dispatches into a data plane is unexercised by it. That is how
// 26 tools could be missing and the suite stay green.
//
// So this file wires the real data planes and drives the tools through the actual MCP endpoint.
// One test per service group, following the sequence an agent would: create, write, read back,
// then the destructive one behind its confirm.

package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/admin"
	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/container"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/identity"
	"github.com/altlimit/altengine/cli/internal/search"
)

// newLiveHandler builds an MCP handler over real, wired data planes — the same objects the
// emulator serves, in the same order.
func newLiveHandler(t *testing.T) *Handler {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	idMgr := identity.NewManager("")
	idSvc := identity.NewService(reg, idMgr, true)
	hub := channel.NewHub()
	dsMgr := datastore.NewManager("")
	srMgr := search.NewManager("")

	datastore.NewHandler(reg, a, dsMgr).WithIdentity(idSvc).Register(mux)
	search.NewHandler(reg, a, srMgr).Register(mux)
	channel.NewHandler(reg, a, hub).WithIdentity(idSvc).Register(mux)
	identity.NewHandler(reg, idSvc).Register(mux)
	blob.NewHandler(reg, a, blob.NewStore("")).Register(mux)
	container.NewHandler(reg, a, container.NewStore(nil), mux).Register(mux)
	// Admin before MCP, as the real server does: some tools reach an /admin route, and admin's
	// console handler is a catch-all on "/" that would otherwise swallow them.
	admin.NewHandler(reg, a, dsMgr, srMgr, hub).WithIdentity(idMgr).Register(mux)

	h := NewHandler(reg, mux, true)
	h.Register(mux)
	return h
}

// runTool calls a tool and fails the test if it errored, returning the decoded payload.
func runTool(t *testing.T, h *Handler, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := toolText(t, h, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s returned undecodable content: %s", name, text)
	}
	return out
}

func TestBlobToolsRoundTrip(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "blob", "files")

	put := runTool(t, h, "blob_put", map[string]any{
		"instance": "files", "name": "readme.md", "content": "# hello", "public": true,
	})
	rec := put["blob"].(map[string]any)
	if rec["status"] != "ready" {
		t.Fatalf("status = %v, want ready — blob_put must return the finished record", rec["status"])
	}
	// The size is measured by whoever received the bytes, not taken from what we sent.
	if rec["size"].(float64) != float64(len("# hello")) {
		t.Fatalf("size = %v", rec["size"])
	}
	if u, _ := rec["url"].(string); u == "" {
		t.Fatal("a public object must come back with its URL")
	}
	id := rec["id"].(string)

	got := runTool(t, h, "blob_get", map[string]any{"instance": "files", "id": id})
	if got["download_url"] == nil {
		t.Fatal("blob_get must include a download URL")
	}

	listed := runTool(t, h, "blob_list", map[string]any{"instance": "files"})
	if n := len(listed["blobs"].([]any)); n != 1 {
		t.Fatalf("listed %d objects, want 1", n)
	}

	mint := runTool(t, h, "blob_upload_url", map[string]any{
		"instance": "files", "name": "big.bin", "size": 4096,
	})
	hdrs := mint["required_headers"].(map[string]any)
	if hdrs["content-length"] != "4096" {
		t.Fatalf("the size must be bound into the URL, got %v", hdrs)
	}

	// Destructive tools refuse without confirm — that is the guard, not a formality.
	if _, isErr := toolText(t, h, "blob_delete", map[string]any{"instance": "files", "ids": []any{id}}); !isErr {
		t.Fatal("blob_delete must refuse without confirm")
	}
	del := runTool(t, h, "blob_delete", map[string]any{"instance": "files", "ids": []any{id}, "confirm": true})
	if del["deleted"].(float64) != 1 {
		t.Fatalf("deleted = %v, want 1", del["deleted"])
	}
}

func TestContainerToolsRefuseHonestly(t *testing.T) {
	// No Docker in CI, so what is checked is that the tools REACH the service and get its real
	// answer rather than a routing error. A 503 "needs Docker" is the correct response and
	// proves the dispatch works; a 404 would mean the path is wrong.
	h := newLiveHandler(t)
	mustCreate(t, h, "container", "jobs")

	jobs := runTool(t, h, "container_list_jobs", map[string]any{"instance": "jobs"})
	if _, ok := jobs["jobs"]; !ok {
		t.Fatalf("container_list_jobs returned %v", jobs)
	}

	text, isErr := toolText(t, h, "container_run", map[string]any{"instance": "jobs", "image": "alpine:3"})
	if !isErr {
		t.Skip("Docker is available; the refusal path is not exercised here")
	}
	// Either "no allowed images" (the empty-allowlist default) or "needs Docker" is a real
	// answer from the service. "no such endpoint" is not.
	if strings.Contains(text, "no such endpoint") {
		t.Fatalf("container_run did not reach the service: %s", text)
	}

	if _, isErr := toolText(t, h, "container_cancel_job", map[string]any{"instance": "jobs", "job_id": "j1"}); !isErr {
		t.Fatal("container_cancel_job must refuse without confirm")
	}
}

func TestDatastoreAndSearchToolsRoundTrip(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "datastore", "appdb")

	runTool(t, h, "datastore_put", map[string]any{
		"instance": "appdb", "collection": "notes",
		"documents": []any{
			map[string]any{"key": "n1", "data": map[string]any{"title": "one", "done": false}},
			map[string]any{"key": "n2", "data": map[string]any{"title": "two", "done": true}},
		},
	})

	got := runTool(t, h, "datastore_get", map[string]any{
		"instance": "appdb", "collection": "notes", "keys": []any{"n1"},
	})
	docs := got["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("datastore_get returned %d documents, want 1", len(docs))
	}

	// Deliberately spelled `op`, the alias — hosted's schema documented `op` while its engine
	// only ever read `fn`, so this exact call returned "unsupported aggregate: undefined" in
	// production until it was fixed. Both spellings must work, and both must count 2.
	for _, key := range []string{"op", "fn"} {
		agg := runTool(t, h, "datastore_aggregate", map[string]any{
			"instance": "appdb", "collection": "notes",
			"metrics": []any{map[string]any{key: "count"}},
		})
		groups, _ := agg["groups"].([]any)
		if len(groups) != 1 {
			t.Fatalf("aggregate with %q returned %v", key, agg)
		}
		metrics := groups[0].(map[string]any)["metrics"].(map[string]any)
		if metrics["m0"].(float64) != 2 {
			t.Fatalf("aggregate with %q counted %v, want 2", key, metrics["m0"])
		}
	}

	if _, isErr := toolText(t, h, "datastore_delete", map[string]any{
		"instance": "appdb", "collection": "notes", "keys": []any{"n1"},
	}); !isErr {
		t.Fatal("datastore_delete must refuse without confirm")
	}
	runTool(t, h, "datastore_delete", map[string]any{
		"instance": "appdb", "collection": "notes", "keys": []any{"n1"}, "confirm": true,
	})
	after := runTool(t, h, "datastore_get", map[string]any{
		"instance": "appdb", "collection": "notes", "keys": []any{"n1"},
	})
	if len(after["documents"].([]any)) != 0 {
		t.Fatal("the document survived a confirmed delete")
	}

	mustCreate(t, h, "search", "idx")
	runTool(t, h, "search_put_documents", map[string]any{
		"instance": "idx", "index": "products",
		"documents": []any{map[string]any{"id": "p1", "fields": []any{
			map[string]any{"name": "title", "type": "text", "value": "blue shoes"},
		}}},
	})
	if _, isErr := toolText(t, h, "search_delete_documents", map[string]any{
		"instance": "idx", "index": "products", "ids": []any{"p1"},
	}); !isErr {
		t.Fatal("search_delete_documents must refuse without confirm")
	}
	runTool(t, h, "search_delete_documents", map[string]any{
		"instance": "idx", "index": "products", "ids": []any{"p1"}, "confirm": true,
	})
}

func TestChannelTools(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "channel", "live")

	// Publishing to a channel nobody is listening on is legal and delivers to nobody. That is
	// worth pinning: an agent seeing 0 recipients should not read it as a failure.
	runTool(t, h, "channel_publish", map[string]any{
		"instance": "live", "channel": "room:1", "data": map[string]any{"hello": "world"},
	})

	rooms := runTool(t, h, "channel_list_rooms", map[string]any{"instance": "live"})
	// Nothing is subscribed, so there are no live rooms — an empty list, not an error.
	if n := len(rooms["rooms"].([]any)); n != 0 {
		t.Fatalf("expected no live rooms with nothing subscribed, got %d", n)
	}
}

func TestAuthTools(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "auth", "users")

	created := runTool(t, h, "auth_create_user", map[string]any{
		"instance": "users",
		"fields":   map[string]any{"email": "ada@example.com"},
		"password": "correct horse battery staple",
	})
	user, _ := created["user"].(map[string]any)
	if user == nil {
		t.Fatalf("auth_create_user returned %v", created)
	}
	uid, _ := user["uid"].(string)
	if uid == "" {
		t.Fatalf("no uid in %v", user)
	}

	listed := runTool(t, h, "auth_list_users", map[string]any{"instance": "users"})
	if n := len(listed["users"].([]any)); n != 1 {
		t.Fatalf("listed %d users, want 1", n)
	}

	runTool(t, h, "auth_set_user_claims", map[string]any{
		"instance": "users", "uid": uid, "claims": map[string]any{"role": "admin"},
	})

	// Disabling is reversible and is the action to prefer over deletion.
	dis := runTool(t, h, "auth_disable_user", map[string]any{"instance": "users", "uid": uid})
	if dis["disabled"] != true {
		t.Fatalf("auth_disable_user returned %v", dis)
	}
	back := runTool(t, h, "auth_disable_user", map[string]any{"instance": "users", "uid": uid, "disabled": false})
	if back["disabled"] != false {
		t.Fatalf("re-enabling failed: %v", back)
	}

	// Rules: read, then write the whole object back. The description says as much, because a
	// partial write silently drops every rule not repeated.
	rules := runTool(t, h, "auth_get_rules", map[string]any{"instance": "users"})
	if rules["access"] == nil {
		t.Fatal("auth_get_rules must always return an access object, even when empty")
	}
	set := runTool(t, h, "auth_set_rules", map[string]any{
		"instance": "users",
		"access": map[string]any{
			"datastore": map[string]any{"notes": map[string]any{"read": map[string]any{"match": "true"}}},
		},
	})
	if set["access"] == nil {
		t.Fatalf("auth_set_rules returned %v", set)
	}
	again := runTool(t, h, "auth_get_rules", map[string]any{"instance": "users"})
	if again["access"].(map[string]any)["datastore"] == nil {
		t.Fatal("the rules did not survive a write followed by a read")
	}

	if _, isErr := toolText(t, h, "auth_delete_user", map[string]any{"instance": "users", "uid": uid}); !isErr {
		t.Fatal("auth_delete_user must refuse without confirm")
	}
	runTool(t, h, "auth_delete_user", map[string]any{"instance": "users", "uid": uid, "confirm": true})
}

// mustCreate makes an instance through the tool an agent would use, so instance creation is
// itself covered for every service.
func mustCreate(t *testing.T, h *Handler, service, name string) {
	t.Helper()
	runTool(t, h, "create_instance", map[string]any{"service": service, "name": name})
}
