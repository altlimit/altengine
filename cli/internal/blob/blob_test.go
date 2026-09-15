// What these tests are for.
//
// The emulator's job is to make a local run WRONG in the same places production is wrong. So
// most of what follows is about refusals: an upload URL that is good for one upload of one exact
// size and type, a public host that will not admit a private object exists, and a listing that
// does not show files nobody uploaded. Getting any of those wrong locally means a developer
// builds against a service more permissive than the real one and finds out on deploy.

package blob

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
)

func newTestBlob(t *testing.T, dir string) (*httptest.Server, *Handler) {
	t.Helper()
	reg, err := control.New(dir)
	if err != nil {
		t.Fatalf("control.New: %v", err)
	}
	mux := http.NewServeMux()
	h := NewHandler(reg, auth.NewStore(true), NewStore(dir))
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, h
}

func do(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Authorization", "Bearer dev")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// put uploads bytes to a minted URL with the headers it asked for, optionally overridden.
func put(t *testing.T, url string, body []byte, contentType string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// upload runs the whole flow — mint, then PUT — and returns the blob's id and the mint response.
func upload(t *testing.T, srv *httptest.Server, instance, name, contentType string, body []byte, public bool) (string, map[string]any) {
	t.Helper()
	status, mint := do(t, "POST", srv.URL+"/v1/blob/"+instance+"/uploads", map[string]any{
		"name": name, "size": len(body), "content_type": contentType, "public": public,
	})
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want 201 (%v)", status, mint)
	}
	if res := put(t, mint["upload_url"].(string), body, contentType); res.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", res.StatusCode)
	}
	return mint["id"].(string), mint
}

func TestUploadRoundTrip(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	content := []byte("hello from a local blob")
	id, mint := upload(t, srv, "files", "greeting.txt", "text/plain", content, false)

	// The blobkey is what a caller stores in a datastore document, so its shape is contractual.
	if key, _ := mint["blobkey"].(string); !strings.HasPrefix(key, "blob:") || !strings.HasSuffix(key, ":"+id) {
		t.Fatalf("blobkey = %q, want blob:<instance>:<id>", key)
	}

	status, got := do(t, "GET", srv.URL+"/v1/blob/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%v)", status, got)
	}
	rec := got["blob"].(map[string]any)
	if rec["status"] != "ready" {
		t.Fatalf("status = %v, want ready — the side that received the bytes promotes the row", rec["status"])
	}
	// The size and digest come from what was RECEIVED, never from what the client claimed.
	if int(rec["size"].(float64)) != len(content) {
		t.Fatalf("size = %v, want %d", rec["size"], len(content))
	}
	if rec["etag"] == "" {
		t.Fatal("etag must be the digest of the stored bytes")
	}
	// Not public, so there is no public URL to hand out.
	if rec["url"] != nil {
		t.Fatalf("url = %v, want null for a private object", rec["url"])
	}

	res, err := http.Get(got["download_url"].(string))
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !bytes.Equal(body, content) {
		t.Fatalf("downloaded %q, want %q", body, content)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("content-type = %q, want the type it was stored with", ct)
	}
}

// The size ceiling is the whole reason the size is required up front: an upload URL is a bearer
// capability, and the size is what bounds what anyone holding one can make us store.
func TestMintEnforcesSize(t *testing.T) {
	srv, _ := newTestBlob(t, "")

	if status, out := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{"name": "x"}); status != 400 {
		t.Fatalf("mint without a size = %d, want 400 (%v)", status, out)
	}
	if status, _ := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{"size": 0}); status != 400 {
		t.Fatalf("mint with size 0 = %d, want 400", status)
	}
	status, out := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{"size": DefaultMaxObjectBytes + 1})
	if status != 400 {
		t.Fatalf("mint above the instance limit = %d, want 400", status)
	}
	if msg, _ := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "limit") {
		t.Fatalf("the refusal should name the limit, got %q", msg)
	}
}

// A minted URL is good for ONE upload, of exactly what it was minted for. Object storage enforces
// this hosted because both values are signed in; the emulator checks them against the reservation,
// so the same PUT fails in the same place.
func TestUploadIsBoundToWhatWasMinted(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	content := []byte("four")

	mintFor := func() string {
		_, mint := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{
			"name": "f.txt", "size": len(content), "content_type": "text/plain",
		})
		return mint["upload_url"].(string)
	}

	if res := put(t, mintFor(), []byte("this body is much longer than four"), "text/plain"); res.StatusCode != 400 {
		t.Fatalf("oversized upload = %d, want 400", res.StatusCode)
	}
	if res := put(t, mintFor(), content, "image/png"); res.StatusCode != 400 {
		t.Fatalf("wrong content-type = %d, want 400", res.StatusCode)
	}

	// Single use. A second PUT must not be able to change bytes a public URL may already be
	// serving under a long cache header.
	url := mintFor()
	if res := put(t, url, content, "text/plain"); res.StatusCode != 200 {
		t.Fatalf("first upload = %d, want 200", res.StatusCode)
	}
	if res := put(t, url, content, "text/plain"); res.StatusCode != 404 {
		t.Fatalf("re-using an upload URL = %d, want 404", res.StatusCode)
	}
}

func TestSignedURLsAreCapabilities(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	_, mint := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{"size": 3, "content_type": "text/plain"})
	url := mint["upload_url"].(string)

	if res := put(t, strings.Split(url, "?")[0], []byte("abc"), "text/plain"); res.StatusCode != 401 {
		t.Fatalf("unsigned upload = %d, want 401", res.StatusCode)
	}
	if res := put(t, url+"x", []byte("abc"), "text/plain"); res.StatusCode != 401 {
		t.Fatalf("tampered signature = %d, want 401", res.StatusCode)
	}

	// An expired URL must say it expired. "Bad signature" sends someone looking for the wrong bug.
	s := newSigner()
	q, _ := s.query(http.MethodPut, "inst", "id", -time.Minute, nil)
	err := s.verify(http.MethodPut, "inst", "id", mustQuery(t, q))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired URL error = %v, want it to say expired", err)
	}

	// A capability is scoped to one method on one object — a download URL cannot upload, and a
	// signature for one blob is worthless for another.
	q2, _ := s.query(http.MethodGet, "inst", "id", time.Minute, nil)
	if err := s.verify(http.MethodPut, "inst", "id", mustQuery(t, q2)); err == nil {
		t.Fatal("a GET capability must not authorize a PUT")
	}
	if err := s.verify(http.MethodGet, "inst", "other", mustQuery(t, q2)); err == nil {
		t.Fatal("a capability for one blob must not authorize another")
	}
}

func mustQuery(t *testing.T, raw string) map[string][]string {
	t.Helper()
	u, err := http.NewRequest("GET", "http://x/?"+raw, nil)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	return u.URL.Query()
}

// The public host takes no credential, so a private object must answer exactly like a missing
// one — a 403 would confirm the id exists.
func TestPublicHost(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	content := []byte("<!doctype html>hi")
	id, _ := upload(t, srv, "assets", "index.html", "text/html", content, false)

	base := srv.URL + "/blob/assets/index.html/"
	res, err := http.Get(base + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("a private object on the public host = %d, want 404", res.StatusCode)
	}

	status, out := do(t, "POST", srv.URL+"/v1/blob/assets/"+id+"/public", map[string]any{"public": true})
	if status != 200 {
		t.Fatalf("publish = %d, want 200 (%v)", status, out)
	}
	// Once public, the caller is handed the URL rather than having to assemble it.
	url, _ := out["blob"].(map[string]any)["url"].(string)
	if url == "" {
		t.Fatal("a public blob must carry its URL")
	}

	res, err = http.Get(url)
	if err != nil {
		t.Fatalf("public get: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Equal(body, content) {
		t.Fatalf("public get = %d %q", res.StatusCode, body)
	}
	// Uploaded files must never be sniffed into something executable, or run script in the
	// platform's own name.
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("public objects must be served with nosniff")
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("public objects must be served with a CSP")
	}

	// The name is cosmetic; the id is the lookup key. A different name is redirected, not served,
	// so one object never has two live URLs.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = noRedirect.Get(srv.URL + "/blob/assets/other-name.html/" + id)
	if err != nil {
		t.Fatalf("wrong-name get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 301 {
		t.Fatalf("wrong name = %d, want 301 to the canonical URL", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); !strings.HasSuffix(loc, "/index.html/"+id) {
		t.Fatalf("redirect location = %q, want the canonical name", loc)
	}

	// Blob has no versions: the local form of a `{slug}--v{n}-blob` host serves nothing, even
	// though `assets` exists and the object is public.
	res, err = noRedirect.Get(srv.URL + "/blob/assets--v1/index.html/" + id)
	if err != nil {
		t.Fatalf("versioned get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("versioned blob address = %d, want 404", res.StatusCode)
	}

	// Unpublishing takes it away again.
	do(t, "POST", srv.URL+"/v1/blob/assets/"+id+"/public", map[string]any{"public": false})
	res, err = http.Get(url)
	if err != nil {
		t.Fatalf("get after unpublish: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("after unpublish = %d, want 404", res.StatusCode)
	}
}

// A range request is what media seeking is made of, and it has to work locally or an <audio>
// element behaves differently against the emulator than against production.
func TestPublicHostServesRanges(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	content := []byte("0123456789")
	id, _ := upload(t, srv, "assets", "clip.bin", "application/octet-stream", content, true)

	req, _ := http.NewRequest("GET", srv.URL+"/blob/assets/clip.bin/"+id, nil)
	req.Header.Set("Range", "bytes=2-5")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", res.StatusCode)
	}
	if string(body) != "2345" {
		t.Fatalf("range body = %q, want \"2345\"", body)
	}
}

func TestListIsReadyOnlyNewestFirstAndPaged(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	var ids []string
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		id, _ := upload(t, srv, "files", n, "text/plain", []byte(n), false)
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond) // distinct creation stamps, so the order is deterministic
	}
	// A reservation nobody uploaded to. It must not appear in a listing: it is a promise that a
	// URL was handed out, not a claim that anything was stored.
	do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{"name": "never.txt", "size": 5})

	_, out := do(t, "GET", srv.URL+"/v1/blob/files", nil)
	blobs := out["blobs"].([]any)
	if len(blobs) != 3 {
		t.Fatalf("listed %d blobs, want 3 ready ones (pending excluded)", len(blobs))
	}
	if got := blobs[0].(map[string]any)["id"]; got != ids[2] {
		t.Fatalf("first listed = %v, want the newest (%s)", got, ids[2])
	}

	_, page := do(t, "GET", srv.URL+"/v1/blob/files?limit=2", nil)
	if len(page["blobs"].([]any)) != 2 || page["cursor"] == nil {
		t.Fatalf("a full page must carry a cursor, got %v", page)
	}
	_, rest := do(t, "GET", srv.URL+"/v1/blob/files?limit=2&cursor="+page["cursor"].(string), nil)
	if n := len(rest["blobs"].([]any)); n != 1 {
		t.Fatalf("second page had %d, want the remaining 1", n)
	}
	if rest["cursor"] != nil {
		t.Fatalf("the last page must not offer a cursor, got %v", rest["cursor"])
	}

	_, filtered := do(t, "GET", srv.URL+"/v1/blob/files?prefix=b", nil)
	if n := len(filtered["blobs"].([]any)); n != 1 {
		t.Fatalf("prefix filter matched %d, want 1", n)
	}
}

func TestDelete(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	id, _ := upload(t, srv, "files", "gone.txt", "text/plain", []byte("bye"), true)

	if status, out := do(t, "POST", srv.URL+"/v1/blob/files/delete", map[string]any{"ids": []string{}}); status != 400 {
		t.Fatalf("delete with no ids = %d, want 400 (%v)", status, out)
	}

	// An honest count: only what actually existed.
	_, out := do(t, "POST", srv.URL+"/v1/blob/files/delete", map[string]any{"ids": []string{id, "not-a-real-id"}})
	if int(out["deleted"].(float64)) != 1 {
		t.Fatalf("deleted = %v, want 1", out["deleted"])
	}
	if status, _ := do(t, "GET", srv.URL+"/v1/blob/files/"+id, nil); status != 404 {
		t.Fatalf("get after delete = %d, want 404", status)
	}
	res, err := http.Get(srv.URL + "/blob/files/gone.txt/" + id)
	if err != nil {
		t.Fatalf("public get after delete: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("public URL after delete = %d, want 404", res.StatusCode)
	}
}

// With a data directory an upload has to survive a restart, or a blobkey written into a local
// datastore document stops resolving overnight — a way for the emulator to break an app that the
// hosted service never would.
func TestObjectsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newTestBlob(t, dir)
	content := []byte("still here tomorrow")
	id, _ := upload(t, srv, "files", "keep.txt", "text/plain", content, false)
	srv.Close()

	srv2, _ := newTestBlob(t, dir)
	status, out := do(t, "GET", srv2.URL+"/v1/blob/files/"+id, nil)
	if status != 200 {
		t.Fatalf("get after restart = %d, want 200 (%v)", status, out)
	}
	res, err := http.Get(out["download_url"].(string))
	if err != nil {
		t.Fatalf("download after restart: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(body, content) {
		t.Fatalf("bytes after restart = %q, want %q", body, content)
	}
}

// A name is one path segment and one header value — never a path, and never a header of its own.
func TestNameIsOneSafeSegment(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"photo.png", "photo.png"},
		// The leading dots go, the separators become dashes — the same string hosted produces,
		// which matters because this ends up in the object's public URL.
		{"../../etc/passwd", "-..-etc-passwd"},
		{"a/b/c.txt", "a-b-c.txt"},
		{"a//b", "a-b"}, // a RUN of separators collapses to one dash
		{"bad\r\nX-Injected: 1", "bad-X-Injected: 1"},
		{"   spaced.txt  ", "spaced.txt"},
		{"", ""},
	} {
		got, err := CleanName(tc.in)
		if err != nil {
			t.Fatalf("CleanName(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("CleanName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := CleanName(strings.Repeat("x", MaxNameLen+1)); err == nil {
		t.Fatal("an over-long name must be refused, not truncated")
	}
}

func TestConfigIsToleratedNotTrusted(t *testing.T) {
	// Unreadable settings fall back to defaults rather than taking the data plane down.
	if c := ParseConfig(map[string]any{"maxObjectBytes": "not a number"}); c.MaxObjectBytes != DefaultMaxObjectBytes {
		t.Fatalf("garbage maxObjectBytes = %d, want the default", c.MaxObjectBytes)
	}
	// ...but it can never exceed what a single upload can express.
	if c := ParseConfig(map[string]any{"maxObjectBytes": float64(MaxObjectBytesCeiling * 2)}); c.MaxObjectBytes != MaxObjectBytesCeiling {
		t.Fatalf("maxObjectBytes = %d, want it clamped to the single-upload ceiling", c.MaxObjectBytes)
	}
	if ParseConfig(map[string]any{}).DefaultPublic {
		t.Fatal("defaultPublic must default to false")
	}
}

// An instance-scoped grant must not reach another instance's objects, and the levels have to
// mean what they mean hosted: reading is not writing, and deleting needs full.
func TestGrantsAreScoped(t *testing.T) {
	reg, err := control.New("")
	if err != nil {
		t.Fatalf("control.New: %v", err)
	}
	a := auth.NewStore(false) // NOT dev-open: real grants
	a.AddKey("tok-read", auth.Grants{"blob:files": "read"})
	mux := http.NewServeMux()
	NewHandler(reg, a, NewStore("")).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	call := func(method, path string, body any) int {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, r)
		req.Header.Set("Authorization", "Bearer tok-read")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := call("GET", "/v1/blob/files", nil); got != 200 {
		t.Fatalf("read grant listing = %d, want 200", got)
	}
	if got := call("POST", "/v1/blob/files/uploads", map[string]any{"size": 1}); got != 403 {
		t.Fatalf("read grant minting an upload = %d, want 403", got)
	}
	if got := call("GET", "/v1/blob/other", nil); got != 403 {
		t.Fatalf("a grant on one instance reached another = %d, want 403", got)
	}
}

// --- multipart ------------------------------------------------------------

// send runs one request against a signed object URL and returns its status and body.
func send(t *testing.T, method, url string, body []byte) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, r)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func tagOf(xmlBody, tag string) string {
	_, rest, ok := strings.Cut(xmlBody, "<"+tag+">")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, "</"+tag+">")
	return v
}

// The whole multipart flow, in the order a client performs it: begin, create, sign a window,
// upload the parts, complete. The object it produces must be indistinguishable from one that
// arrived in a single PUT — same row, same digest, same download.
func TestMultipartRoundTrip(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	content := []byte("the parts of one object, glued back together by storage")

	status, begun := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart", map[string]any{
		"name": "export.bin", "size": len(content), "content_type": "application/octet-stream",
	})
	if status != http.StatusCreated {
		t.Fatalf("begin status = %d, want 201 (%v)", status, begun)
	}
	id := begun["id"].(string)
	if begun["parts"].(float64) != 1 || int64(begun["part_size"].(float64)) != DefaultPartBytes {
		t.Fatalf("plan = %v parts of %v bytes, want 1 of %d", begun["parts"], begun["part_size"], DefaultPartBytes)
	}

	code, body := send(t, http.MethodPost, begun["create_url"].(string), nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (%s)", code, body)
	}
	uploadID := tagOf(body, "UploadId")
	if uploadID == "" {
		t.Fatalf("create returned no upload id: %s", body)
	}

	// Two parts against a plan of one: the plan is advice about part SIZE, and a client that cuts
	// its own bytes differently is still uploading the object it declared.
	status, signed := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart/urls", map[string]any{
		"id": id, "upload_id": uploadID, "from": 1, "count": 2,
	})
	if status != http.StatusOK {
		t.Fatalf("urls status = %d, want 200 (%v)", status, signed)
	}
	urls := signed["part_urls"].([]any)
	if len(urls) != 2 {
		t.Fatalf("part_urls = %d, want 2", len(urls))
	}

	half := len(content) / 2
	for i, chunk := range [][]byte{content[:half], content[half:]} {
		u := urls[i].(map[string]any)["url"].(string)
		if code, body := send(t, http.MethodPut, u, chunk); code != http.StatusOK {
			t.Fatalf("part %d status = %d, want 200 (%s)", i+1, code, body)
		}
	}

	xmlBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"a"</ETag></Part>` +
		`<Part><PartNumber>2</PartNumber><ETag>"b"</ETag></Part></CompleteMultipartUpload>`
	if code, body := send(t, http.MethodPost, signed["complete_url"].(string), []byte(xmlBody)); code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200 (%s)", code, body)
	}

	status, got := do(t, "GET", srv.URL+"/v1/blob/files/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%v)", status, got)
	}
	rec := got["blob"].(map[string]any)
	if rec["status"] != "ready" || int(rec["size"].(float64)) != len(content) {
		t.Fatalf("row = %v, want a ready object of %d bytes", rec, len(content))
	}
	code, downloaded := send(t, http.MethodGet, got["download_url"].(string), nil)
	if code != http.StatusOK || downloaded != string(content) {
		t.Fatalf("download = %d %q, want the assembled object", code, downloaded)
	}
}

// A part URL is a capability for ONE part of ONE upload. The multipart vocabulary lives in the
// query string, so a signature that did not cover it would let whoever holds part 1's URL write
// part 7 — or write into somebody else's upload of the same object.
func TestPartURLIsScopedToItsPart(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	_, begun := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart", map[string]any{"size": 10})
	_, body := send(t, http.MethodPost, begun["create_url"].(string), nil)
	uploadID := tagOf(body, "UploadId")

	_, signed := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart/urls", map[string]any{
		"id": begun["id"].(string), "upload_id": uploadID, "from": 1, "count": 1,
	})
	u := signed["part_urls"].([]any)[0].(map[string]any)["url"].(string)

	if code, _ := send(t, http.MethodPut, strings.Replace(u, "partNumber=1", "partNumber=7", 1), []byte("x")); code != 401 {
		t.Fatalf("re-aimed part number = %d, want 401", code)
	}
	if code, _ := send(t, http.MethodPut, strings.Replace(u, "uploadId="+uploadID, "uploadId=other", 1), []byte("x")); code != 401 {
		t.Fatalf("re-aimed upload id = %d, want 401", code)
	}
	// The abort URL ends the upload, and doing it twice is success — a client aborts when it has
	// already failed, and a refusal there would leave the parts behind.
	if code, _ := send(t, http.MethodDelete, signed["abort_url"].(string), nil); code != 204 {
		t.Fatalf("abort = %d, want 204", code)
	}
	if code, _ := send(t, http.MethodDelete, signed["abort_url"].(string), nil); code != 204 {
		t.Fatalf("second abort = %d, want 204", code)
	}
	if code, _ := send(t, http.MethodPut, u, []byte("x")); code != 404 {
		t.Fatalf("part after abort = %d, want 404", code)
	}
}

// What an instance will STORE and what one request can CARRY are different questions, and they
// were one constant until multipart existed. An instance raised to the object ceiling still
// refuses an oversize single PUT — and names the flow that can take it — while accepting the very
// same size through multipart.
func TestSinglePutCeilingIsNotTheObjectCeiling(t *testing.T) {
	if MaxObjectBytesCeiling <= MaxSinglePutBytes {
		t.Fatal("the object ceiling must be above what one request can carry, or multipart buys nothing")
	}
	s := NewStore("")
	cfg := Config{MaxObjectBytes: MaxObjectBytesCeiling}
	size := MaxSinglePutBytes + 1

	_, err := s.Reserve("inst", cfg, MintRequest{Size: &size})
	if err == nil || !strings.Contains(err.Error(), "multipart") {
		t.Fatalf("oversize single PUT = %v, want a refusal naming the multipart flow", err)
	}
	if _, _, parts, err := s.BeginMultipart("inst", cfg, MintRequest{Size: &size}); err != nil {
		t.Fatalf("multipart at %d bytes = %v, want it accepted", size, err)
	} else if parts < 2 {
		t.Fatalf("plan for %d bytes = %d parts, want more than one", size, parts)
	}

	// The instance's own limit still binds, and it is the one a caller can change — so a size
	// over both is told about that one rather than about storage's.
	small := Config{MaxObjectBytes: 10}
	if _, err := s.Reserve("inst", small, MintRequest{Size: &size}); err == nil ||
		!strings.Contains(err.Error(), "this instance's limit") {
		t.Fatalf("over both limits = %v, want the instance's limit named", err)
	}
}
