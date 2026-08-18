// The env.* methods added when the stub surface was completed: channel token/presence, search
// index listing and deletion, datastore index deletion.
//
// Same standard as blobbindings_test.go — call them the way a customer would and check the
// returned VALUE, because the whole promise of the emulator is that the same function source
// runs in both places. env.channel.token matters most: it is how a backend-less app does
// channel authorization at all, so a shape difference here is a browser that cannot connect.

package functions

import (
	"net/http"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/search"
)

func newSurfaceServer(t *testing.T) *http.ServeMux {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	channel.NewHandler(reg, a, channel.NewHub()).Register(mux)
	search.NewHandler(reg, a, search.NewManager("")).Register(mux)
	datastore.NewHandler(reg, a, datastore.NewManager("")).Register(mux)
	NewHandler(reg, a, NewStore(""), mux).Register(mux)
	return mux
}

func TestChannelTokenThroughStub(t *testing.T) {
	mux := newSurfaceServer(t)
	deployFn(t, mux, "mint", `export default { async fetch(request, env) {
		const t = await env.channel.token({ instance: "live" }, {
			channels: ["room:1"], ttlSeconds: 60, presenceId: "u-7",
		});
		return Response.json(t);
	} };`, map[string]string{"channel": "full"})

	res := invoke(t, mux, "/fn/main/mint")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["token"] == nil || got["token"] == "" {
		t.Fatalf("no token minted: %v", got)
	}
	// The options are camelCase on the stub and snake_case over REST. If the translation were
	// missing the call would still succeed — with a DEFAULT ttl and no presence id — so the
	// echoed values are the only thing that proves the fields were carried across.
	if got["presence_id"] != "u-7" {
		t.Fatalf("presenceId did not reach the mint: %v", got)
	}
	chans, _ := got["channels"].([]any)
	if len(chans) != 1 || chans[0] != "room:1" {
		t.Fatalf("channels did not reach the mint: %v", got)
	}
}

func TestChannelTokenPublishNeedsWrite(t *testing.T) {
	mux := newSurfaceServer(t)
	// A READ grant. Minting a subscriber is a read; minting a publish-capable token is a
	// write, and the emulator has to draw that line in the same place hosted does or a
	// function that mints publish tokens locally starts 403ing the moment it deploys.
	deployFn(t, mux, "mintpub", `export default { async fetch(request, env) {
		const out = {};
		try { out.sub = !!(await env.channel.token({ instance: "live" }, { channels: ["c"] })).token; }
		catch (e) { out.sub = "ERR:" + e.message; }
		try { await env.channel.token({ instance: "live" }, { channels: ["c"], publish: true }); out.pub = "ALLOWED"; }
		catch (e) { out.pub = "denied"; }
		return Response.json(out);
	} };`, map[string]string{"channel": "read"})

	res := invoke(t, mux, "/fn/main/mintpub")
	got := jsonBody(t, res.Body.String())
	if got["sub"] != true {
		t.Fatalf("a read grant must still mint a subscriber token: %v", got)
	}
	if got["pub"] != "denied" {
		t.Fatalf("a read grant must NOT mint a publish-capable token: %v", got)
	}
}

func TestChannelPresenceThroughStub(t *testing.T) {
	mux := newSurfaceServer(t)
	deployFn(t, mux, "who", `export default { async fetch(request, env) {
		try { return Response.json(await env.channel.presence({ instance: "live" }, "room:1")); }
		catch (e) { return Response.json({ error: e.message }); }
	} };`, map[string]string{"channel": "full"})

	res := invoke(t, mux, "/fn/main/who")
	got := jsonBody(t, res.Body.String())
	// Presence is off by default, so the honest answer is a refusal naming presence — not an
	// empty roster, which would read as "nobody is here".
	if msg, ok := got["error"].(string); ok {
		if !strings.Contains(msg, "presence") {
			t.Fatalf("refusal should name presence: %s", msg)
		}
		return
	}
	if got["channel"] != "room:1" {
		t.Fatalf("presence did not echo the channel: %v", got)
	}
}

func TestSearchIndexListAndDeleteThroughStub(t *testing.T) {
	mux := newSurfaceServer(t)
	deployFn(t, mux, "idx", `export default { async fetch(request, env) {
		await env.search.index({ instance: "cat" }, "items", [
			{ id: "i1", fields: [{ name: "title", type: "text", value: "red hat" }] },
		]);
		const before = await env.search.listIndexes({ instance: "cat" });
		const dropped = await env.search.deleteIndex({ instance: "cat" }, "items");
		const after = await env.search.listIndexes({ instance: "cat" });
		return Response.json({ before: before.indexes.length, dropped, after: after.indexes.length });
	} };`, map[string]string{"search": "full"})

	res := invoke(t, mux, "/fn/main/idx")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["before"].(float64) != 1 || got["after"].(float64) != 0 {
		t.Fatalf("listIndexes/deleteIndex did not round-trip: %v", got)
	}
}

func TestSearchDeleteNeedsFullNotWrite(t *testing.T) {
	mux := newSurfaceServer(t)
	// The divergence this whole contract exists to stop: deleting used to be `write` here and
	// `full` hosted, so this exact function worked locally and 403'd in production.
	deployFn(t, mux, "wr", `export default { async fetch(request, env) {
		const out = {};
		await env.search.index({ instance: "cat2" }, "items", [{ id: "i1", fields: [] }]);
		try { await env.search.delete({ instance: "cat2" }, "items", ["i1"]); out.docs = "ALLOWED"; }
		catch (e) { out.docs = "denied"; }
		try { await env.search.deleteIndex({ instance: "cat2" }, "items"); out.index = "ALLOWED"; }
		catch (e) { out.index = "denied"; }
		return Response.json(out);
	} };`, map[string]string{"search": "write"})

	got := jsonBody(t, invoke(t, mux, "/fn/main/wr").Body.String())
	if got["docs"] != "denied" || got["index"] != "denied" {
		t.Fatalf("a write grant must not delete: %v", got)
	}
}

func TestDatastoreDeleteIndexThroughStub(t *testing.T) {
	mux := newSurfaceServer(t)
	deployFn(t, mux, "dsidx", `export default { async fetch(request, env) {
		await env.datastore.put({ instance: "appdb" }, "tasks", [
			{ key: "t1", data: { owner: "amy", due: 3 } },
		]);
		const made = await env.datastore.createIndex({ instance: "appdb" }, "tasks", ["owner", "due"]);
		const before = await env.datastore.listIndexes({ instance: "appdb" }, "tasks");
		const id = (made.index && made.index.id) ?? (before.indexes[0] && before.indexes[0].id);
		await env.datastore.deleteIndex({ instance: "appdb" }, "tasks", id);
		const after = await env.datastore.listIndexes({ instance: "appdb" }, "tasks");
		return Response.json({ before: before.indexes.length, after: after.indexes.length });
	} };`, map[string]string{"datastore": "full"})

	res := invoke(t, mux, "/fn/main/dsidx")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["before"].(float64) == 0 || got["after"].(float64) != got["before"].(float64)-1 {
		t.Fatalf("deleteIndex did not remove exactly one index: %v", got)
	}
}
