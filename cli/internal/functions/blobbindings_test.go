// env.blob and env.container, exercised through a real function.
//
// The point of these is that a function written against the hosted stubs runs here unchanged.
// So each test calls the stub the way a customer would and checks the value that comes back has
// the shape hosted returns — not that some HTTP request was made.
//
// `put` and `bytes` get the most attention because they are the two methods that are NOT one
// REST call: hosted they reach storage directly, and there is no public endpoint for either.
// Here they are composed from the calls a REST client would make, so what has to be proven is
// that the composition is invisible from inside the function.

package functions

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/control"
)

func newBlobServer(t *testing.T) *http.ServeMux {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	blob.NewHandler(reg, a, blob.NewStore("")).Register(mux)
	NewHandler(reg, a, NewStore(""), mux).Register(mux)
	return mux
}

func jsonBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("response was not JSON: %s", raw)
	}
	return out
}

func TestBlobPutAndReadBackThroughStub(t *testing.T) {
	mux := newBlobServer(t)
	deployFn(t, mux, "store", `export default { async fetch(request, env) {
		const rec = await env.blob.put({ instance: "files" }, "notes.txt", "written by a function", { public: true });
		const back = await env.blob.bytes({ instance: "files" }, rec.id);
		return Response.json({ rec, text: new TextDecoder().decode(back) });
	} };`, map[string]string{"blob": "full"})

	res := invoke(t, mux, "/fn/main/store")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())

	// The bytes survive the round trip — through a minted URL, an upload and a download, none
	// of which the function can see.
	if got["text"] != "written by a function" {
		t.Fatalf("read back %q", got["text"])
	}
	rec := got["rec"].(map[string]any)
	// The record is the finished one: promoted by whoever received the bytes, with the size and
	// digest measured there rather than taken from the caller.
	if rec["status"] != "ready" {
		t.Fatalf("status = %v, want ready", rec["status"])
	}
	if rec["size"].(float64) != float64(len("written by a function")) {
		t.Fatalf("size = %v", rec["size"])
	}
	if rec["etag"] == "" {
		t.Fatal("etag must be the digest of the stored bytes")
	}
	// Public, so the caller is handed the URL rather than having to assemble one.
	if u, _ := rec["url"].(string); u == "" {
		t.Fatal("a public blob must carry its URL")
	}
	if key, _ := rec["blobkey"].(string); key == "" {
		t.Fatal("a blob must carry the key you store elsewhere")
	}
}

func TestBlobUploadUrlThroughStub(t *testing.T) {
	// The decision that belongs in a function: how big an upload the browser may make. The size
	// is bound into the URL, so it is the real limit on what anyone holding it can store.
	mux := newBlobServer(t)
	deployFn(t, mux, "mint", `export default { async fetch(request, env) {
		return Response.json(await env.blob.uploadUrl({ instance: "files" }, {
			name: "photo.jpg", size: 1024, contentType: "image/jpeg",
		}));
	} };`, map[string]string{"blob": "write"})

	res := invoke(t, mux, "/fn/main/mint")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["upload_url"] == nil || got["id"] == nil {
		t.Fatalf("uploadUrl returned %v", got)
	}
	headers := got["required_headers"].(map[string]any)
	if headers["content-length"] != "1024" || headers["content-type"] != "image/jpeg" {
		t.Fatalf("the headers the URL was signed for are wrong: %v", headers)
	}
}

func TestBlobListSetPublicAndDeleteThroughStub(t *testing.T) {
	mux := newBlobServer(t)
	deployFn(t, mux, "manage", `export default { async fetch(request, env) {
		const a = await env.blob.put({ instance: "files" }, "a.txt", "one");
		await env.blob.put({ instance: "files" }, "b.txt", "two");
		const listed = await env.blob.list({ instance: "files" }, {});
		const published = await env.blob.setPublic({ instance: "files" }, a.id, true);
		const gone = await env.blob.delete({ instance: "files" }, [a.id]);
		const after = await env.blob.list({ instance: "files" }, {});
		return Response.json({
			count: listed.blobs.length, publicUrl: published.url,
			deleted: gone.deleted, remaining: after.blobs.length,
		});
	} };`, map[string]string{"blob": "full"})

	res := invoke(t, mux, "/fn/main/manage")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["count"].(float64) != 2 {
		t.Fatalf("listed %v, want 2", got["count"])
	}
	if u, _ := got["publicUrl"].(string); u == "" {
		t.Fatal("publishing must return the object's URL")
	}
	if got["deleted"].(float64) != 1 || got["remaining"].(float64) != 1 {
		t.Fatalf("delete reported %v, %v remaining", got["deleted"], got["remaining"])
	}
}

// The grant is the blast radius, and it is checked before anything is touched — the same
// ladder every other stub obeys.
func TestBlobStubEnforcesGrants(t *testing.T) {
	mux := newBlobServer(t)
	deployFn(t, mux, "readonly", `export default { async fetch(request, env) {
		try {
			await env.blob.put({ instance: "files" }, "x.txt", "nope");
			return new Response("WROTE-WITH-READ-GRANT", { status: 500 });
		} catch (e) { return new Response("denied: " + e.message); }
	} };`, map[string]string{"blob": "read"})

	res := invoke(t, mux, "/fn/main/readonly")
	body := res.Body.String()
	if body == "WROTE-WITH-READ-GRANT" {
		t.Fatal("a read grant must not be able to write")
	}
	if !strings.Contains(body, "denied: ") {
		t.Fatalf("expected a refusal, got %q", body)
	}

	// A grant on one instance says nothing about another.
	deployFn(t, mux, "elsewhere", `export default { async fetch(request, env) {
		try {
			await env.blob.list({ instance: "other" }, {});
			return new Response("REACHED-UNGRANTED-INSTANCE", { status: 500 });
		} catch (e) { return new Response("denied: " + e.message); }
	} };`, map[string]string{"blob:files": "full"})
	if b := invoke(t, mux, "/fn/main/elsewhere").Body.String(); !strings.Contains(b, "denied: ") {
		t.Fatalf("an instance-scoped grant reached another instance: %q", b)
	}
}

// A service with no grant is ABSENT from env, not present-and-refused — so the mistake shows up
// as "env.blob is undefined" at the call site rather than as a permission error later.
func TestUngrantedServicesAreAbsentFromEnv(t *testing.T) {
	mux := newBlobServer(t)
	deployFn(t, mux, "peek", `export default { async fetch(request, env) {
		return Response.json({ keys: Object.keys(env).sort() });
	} };`, map[string]string{"blob": "read"})

	got := jsonBody(t, invoke(t, mux, "/fn/main/peek").Body.String())
	keys := got["keys"].([]any)
	var has = func(k string) bool {
		for _, v := range keys {
			if v == k {
				return true
			}
		}
		return false
	}
	if !has("blob") {
		t.Fatalf("granted service missing from env: %v", keys)
	}
	if has("container") || has("datastore") {
		t.Fatalf("an ungranted service was handed to the function: %v", keys)
	}
}
