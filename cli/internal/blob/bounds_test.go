package blob

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Multipart parts are held in memory until Complete, so they are bounded by the size the upload
// was reserved for. They used to be read whole and kept, however many bytes a client sent.
func TestPartsAreBoundedByTheReservedSize(t *testing.T) {
	srv, _ := newTestBlob(t, "")
	status, begun := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart", map[string]any{
		"name": "x.bin", "size": 10, "content_type": "application/octet-stream",
	})
	if status != http.StatusCreated {
		t.Fatalf("begin: %d %v", status, begun)
	}
	_, body := send(t, http.MethodPost, begun["create_url"].(string), nil)
	uploadID := tagOf(body, "UploadId")
	_, signed := do(t, "POST", srv.URL+"/v1/blob/files/uploads/multipart/urls", map[string]any{
		"id": begun["id"], "upload_id": uploadID, "from": 1, "count": 2,
	})
	urls := signed["part_urls"].([]any)
	if code, body := send(t, http.MethodPut, urls[0].(map[string]any)["url"].(string), []byte(strings.Repeat("a", 11))); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an 11-byte part of a 10-byte object = %d %s", code, body)
	}
	if code, _ := send(t, http.MethodPut, urls[0].(map[string]any)["url"].(string), []byte("aaaaaa")); code != http.StatusOK {
		t.Fatalf("part 1 = %d", code)
	}
	if code, body := send(t, http.MethodPut, urls[1].(map[string]any)["url"].(string), []byte("bbbbbb")); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("parts totalling 12 of 10 bytes = %d %s", code, body)
	}
}

// A reservation nobody finished is reaped after the grace period, parts and all.
func TestReapDropsStalePending(t *testing.T) {
	srv, h := newTestBlob(t, "")
	status, minted := do(t, "POST", srv.URL+"/v1/blob/files/uploads", map[string]any{
		"name": "a.txt", "size": 3, "content_type": "text/plain",
	})
	if status != http.StatusCreated {
		t.Fatalf("mint: %d %v", status, minted)
	}
	id := minted["id"].(string)
	inst := h.reg.Get("blob", "files")
	if n := h.store.Reap(time.Now().Add(-PendingGrace)); n != 0 {
		t.Fatalf("a fresh reservation was reaped (%d)", n)
	}
	if n := h.store.Reap(time.Now().Add(time.Minute)); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if _, ok := h.store.Pending(inst.ID, id); ok {
		t.Fatal("the stale reservation is still pending")
	}
}
