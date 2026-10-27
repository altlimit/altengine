package mailbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
)

// Outbound attachments, per-message expiry and the instance's sendPaused, as hosted.

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func errOf(out map[string]any) (code, message string) {
	e, _ := out["error"].(map[string]any)
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	return
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// fetch follows a signed download URL through the mux.
func (w *world) fetch(t *testing.T, raw string) (int, []byte, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, httptest.NewRequest("GET", u.RequestURI(), nil))
	return rec.Code, rec.Body.Bytes(), rec.Header().Get("Content-Disposition")
}

func TestSendAttachmentRefusals(t *testing.T) {
	w := newWorld(t, "")
	// One send a day: a refusal that took the slot would make the final send fail.
	w.create(t, "acme", map[string]any{"settings": map[string]any{"sendMode": "any", "dailySendCap": 1}})
	tooMany := []any{}
	for i := 0; i < MaxSendAttachments+1; i++ {
		tooMany = append(tooMany, map[string]any{"content": b64("x")})
	}
	cases := []struct {
		attachments any
		want        string
	}{
		{"x", "attachments must be an array"},
		{map[string]any{"content": b64("x")}, "attachments must be an array"},
		{tooMany, "at most 32 attachments per message"},
		{[]any{1}, "attachments[0] must be an object"},
		{[]any{[]any{}}, "attachments[0] must be an object"},
		{[]any{map[string]any{"content": b64("x")}, map[string]any{"filename": "a.txt"}}, "attachments[1].content is required: base64 text, or bytes from a function"},
		{[]any{map[string]any{"content": 42}}, "attachments[0].content is required: base64 text, or bytes from a function"},
		{[]any{map[string]any{"content": "not base64!"}}, "attachments[0].content must be base64"},
		{[]any{map[string]any{"content": "QUJDRA=x"}}, "attachments[0].content must be base64"},
		{[]any{map[string]any{"content": "QUJDR"}}, "attachments[0].content must be base64"},
		{[]any{map[string]any{"content": ""}}, "attachments[0].content is empty"},
		{[]any{map[string]any{"content": b64("x"), "filename": 5}}, "attachments[0].filename must be a string"},
		{[]any{map[string]any{"content": b64("x"), "contentType": true}}, "attachments[0].contentType must be a string"},
		{[]any{map[string]any{"content": b64("x"), "contentId": "a b"}}, "attachments[0].contentId must be 1–100 letters, digits, '.', '_', '@' or '-'"},
		{[]any{map[string]any{"content": b64("x"), "contentId": strings.Repeat("a", 101)}}, "attachments[0].contentId must be 1–100 letters, digits, '.', '_', '@' or '-'"},
		{[]any{map[string]any{"content": b64("x"), "contentId": 7}}, "attachments[0].contentId must be 1–100 letters, digits, '.', '_', '@' or '-'"},
	}
	for _, c := range cases {
		code, out := w.do(t, "POST", "/v1/mailbox/acme/send", jsonBody(t, map[string]any{
			"to": []any{"a@example.com"}, "subject": "s", "text": "t", "attachments": c.attachments,
		}))
		gotCode, msg := errOf(out)
		if code != 400 || gotCode != "INVALID_ARGUMENT" || msg != c.want {
			t.Errorf("%v: %d %s %q, want 400 %q", c.attachments, code, gotCode, msg, c.want)
		}
	}
	// Base64 the way a browser's decoder takes it: whitespace ignored, padding optional.
	code, out := w.do(t, "POST", "/v1/mailbox/acme/send", jsonBody(t, map[string]any{
		"to": []any{"a@example.com"}, "subject": "s", "text": "t",
		"attachments": []any{map[string]any{"content": "QUJD\r\nRA", "filename": nil, "contentType": nil, "contentId": nil}},
	}))
	if code != 201 {
		t.Fatalf("send after the refusals = %d %v (a refusal took the day's slot?)", code, out)
	}
}

func TestSendTooLargeIsRefusedBeforeASlot(t *testing.T) {
	w := newWorld(t, "")
	w.create(t, "acme", map[string]any{"settings": map[string]any{"sendMode": "any", "dailySendCap": 1}})
	// 3.9 MB of file is past 5 MiB once base64'd.
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 3_900_000))
	code, out := w.do(t, "POST", "/v1/mailbox/acme/send", jsonBody(t, map[string]any{
		"to": []any{"a@example.com"}, "subject": "s", "text": "t",
		"attachments": []any{map[string]any{"filename": "big.bin", "content": big}},
	}))
	errCode, msg := errOf(out)
	if code != 413 || errCode != "INVALID_ARGUMENT" || !strings.Contains(msg, "larger than 5242880 bytes once encoded") {
		t.Fatalf("too large = %d %s %q", code, errCode, msg)
	}
	// The slot is still there.
	if code, out := w.do(t, "POST", "/v1/mailbox/acme/send", jsonBody(t, map[string]any{"to": []any{"a@example.com"}, "subject": "s", "text": "t"})); code != 201 {
		t.Fatalf("send after the 413 = %d %v", code, out)
	}

	// A request body past 8 MiB is refused as a body, before it is parsed.
	huge := `{"to":["a@example.com"],"subject":"s","text":"` + strings.Repeat("x", MaxSendRequestBytes) + `"}`
	for _, p := range []string{"/v1/mailbox/acme/send", "/v1/mailbox/acme/messages/x/reply"} {
		code, out := w.do(t, "POST", p, strings.NewReader(huge))
		if errCode, msg := errOf(out); code != 413 || errCode != "INVALID_ARGUMENT" || msg != fmt.Sprintf("request body exceeds %d bytes", MaxSendRequestBytes) {
			t.Errorf("%s with a 8 MiB+ body = %d %s %q", p, code, errCode, msg)
		}
	}
}

// The hosted estimate, term by term.
func TestEncodedSize(t *testing.T) {
	part := func(n int) outboundPart { return outboundPart{base64Len: base64.StdEncoding.EncodedLen(n)} }
	if got := encodedSize("", "", nil); got != 16384 {
		t.Errorf("empty = %d", got)
	}
	// ceil(2 × 1.1) + 512, and ceil(10 × 1.1) + 512, plus the headers allowance.
	if got := encodedSize("hi", "0123456789", nil); got != 3+512+11+512+16384 {
		t.Errorf("bodies = %d", got)
	}
	// 3 bytes are 4 base64 characters: ceil(4 × 78 / 76) + 512. 57 bytes are 76: exactly 78.
	if got := encodedSize("", "", []outboundPart{part(3), part(57)}); got != (5+512)+(78+512)+16384 {
		t.Errorf("parts = %d", got)
	}
	// "é" is two bytes of UTF-8.
	if got := encodedSize("é", "", nil); got != 3+512+16384 {
		t.Errorf("utf-8 = %d", got)
	}
}

func TestSentAttachmentsAreStoredAndListed(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", map[string]any{"settings": map[string]any{"sendMode": "any"}})
	pdf := "%PDF-1.4 fake"
	code, out := w.do(t, "POST", "/v1/mailbox/acme/send", jsonBody(t, map[string]any{
		"to": []any{"a@example.com"}, "subject": "Invoice", "text": "attached", "html": `<img src="cid:logo">`,
		"attachments": []any{
			map[string]any{"filename": ` a/b"c\d.pdf `, "contentType": "Application/PDF; name=x", "content": b64(pdf)},
			map[string]any{"contentType": "not a type", "content": b64("PNG"), "contentId": "logo"},
		},
	}))
	if code != 201 {
		t.Fatalf("send = %d %v", code, out)
	}
	sent := out["message"].(map[string]any)
	wantSize := float64(len("attached") + len(`<img src="cid:logo">`) + len(pdf) + len("PNG"))
	if sent["attachmentCount"] != float64(2) || sent["size"] != wantSize || sent["direction"] != "out" {
		t.Fatalf("summary = %v", sent)
	}
	id := sent["id"].(string)
	_, got := w.do(t, "GET", "/v1/mailbox/acme/messages/"+id, nil)
	atts := got["message"].(map[string]any)["attachments"].([]any)
	if len(atts) != 2 {
		t.Fatalf("attachments = %v", atts)
	}
	a0, a1 := atts[0].(map[string]any), atts[1].(map[string]any)
	if a0["filename"] != `a_b_c_d.pdf` || a0["contentType"] != "application/pdf" || a0["disposition"] != "attachment" ||
		a0["contentId"] != nil || a0["stored"] != true || a0["size"] != float64(len(pdf)) || a0["rejected"] != nil {
		t.Errorf("attachment 0 = %v", a0)
	}
	if a1["filename"] != "attachment-2" || a1["contentType"] != "application/octet-stream" || a1["disposition"] != "inline" || a1["contentId"] != "logo" {
		t.Errorf("attachment 1 = %v", a1)
	}
	// Downloadable through the same route as a received attachment.
	code, link := w.do(t, "GET", "/v1/mailbox/acme/messages/"+id+"/attachments/0", nil)
	if code != 200 || link["filename"] != "a_b_c_d.pdf" {
		t.Fatalf("attachment link = %d %v", code, link)
	}
	status, body, disp := w.fetch(t, link["url"].(string))
	if status != 200 || string(body) != pdf || !strings.HasPrefix(disp, "attachment;") {
		t.Fatalf("download = %d %q %q", status, body, disp)
	}
	// Stored bytes count them; deleting the message deletes them.
	if _, stored, _ := w.svc.store.Stats(in.ID); stored != int64(len(pdf)+len("PNG")) {
		t.Errorf("stored bytes = %d", stored)
	}
	w.do(t, "DELETE", "/v1/mailbox/acme/messages/"+id, nil)
	if _, ok := w.svc.store.Bytes(in.ID, AttachmentKey(id, 0)); ok {
		t.Error("attachment bytes outlived their message")
	}
}

func TestLoopbackCarriesAttachments(t *testing.T) {
	w := newWorld(t, "")
	w.create(t, "alpha", map[string]any{"settings": map[string]any{"sendMode": "any"}})
	b := w.create(t, "bravo", map[string]any{"routes": []any{
		map[string]any{"match": map[string]any{"hasAttachment": true}, "do": []any{map[string]any{"channel": "rt/files"}}},
	}})
	code, out := w.do(t, "POST", "/v1/mailbox/alpha/send", jsonBody(t, map[string]any{
		"to": []any{"bravo.desk@mail.localhost"}, "subject": "Files", "text": "see attached", "html": `<p><img src="cid:logo@x"></p>`,
		"attachments": []any{
			map[string]any{"filename": "report.csv", "contentType": "text/csv", "content": b64("a,b\n1,2\n")},
			map[string]any{"filename": "logo.png", "contentType": "image/png", "content": b64("\x89PNG"), "contentId": "logo@x"},
		},
	}))
	if code != 201 || !strings.Contains(out["message"].(map[string]any)["status"].(string), "delivered locally") {
		t.Fatalf("send = %d %v", code, out)
	}
	got, _, _ := w.svc.store.List(b.ID, ListOptions{})
	if len(got) != 1 || got[0].AttachmentCount != 2 {
		t.Fatalf("looped = %+v", got)
	}
	rec, _ := w.svc.store.Get(b.ID, got[0].ID)
	if *rec.Text != "see attached" || len(rec.Attachments) != 2 {
		t.Fatalf("received = %+v", rec)
	}
	csv, logo := rec.Attachments[0], rec.Attachments[1]
	if *csv.Filename != "report.csv" || csv.ContentType != "text/csv" || *csv.Disposition != "attachment" {
		t.Errorf("csv = %+v", csv)
	}
	if bs, _ := w.svc.store.Bytes(b.ID, csv.Key); string(bs) != "a,b\n1,2\n" {
		t.Errorf("csv bytes = %q", bs)
	}
	if *logo.Filename != "logo.png" || *logo.Disposition != "inline" || logo.ContentID == nil || *logo.ContentID != "<logo@x>" {
		t.Errorf("logo = %+v", logo)
	}
	if bs, _ := w.svc.store.Bytes(b.ID, logo.Key); string(bs) != "\x89PNG" {
		t.Errorf("logo bytes = %q", bs)
	}
	// The receiving mailbox's hasAttachment route saw them.
	if len(rec.Deliveries) != 1 || rec.Deliveries[0].Action != "channel:rt/files" {
		t.Errorf("deliveries = %+v", rec.Deliveries)
	}
	w.svc.Wait()
}

func TestKeepMessage(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", nil)
	var now int64 = 1_800_000_000_000
	w.svc.now = func() int64 { return now }
	ids := []string{}
	for i := 0; i < 2; i++ {
		rec, err := w.svc.DeliverLocal(in, "inbox", "jane@example.com", []byte(fmt.Sprintf("Subject: m%d\r\n\r\nbody", i)), false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	first, _ := w.svc.store.Get(in.ID, ids[0])
	defaultExpiry := first.ExpiresAt
	if defaultExpiry != now+7*86_400_000 {
		t.Fatalf("default expiry = %d", defaultExpiry)
	}

	for _, body := range []string{`{"days":0}`, `{"days":91}`, `{"days":1.5}`, `{"days":"7"}`, `{"days":null}`, `{}`, `{"days":true}`} {
		code, out := w.do(t, "POST", "/v1/mailbox/acme/messages/"+ids[0]+"/keep", strings.NewReader(body))
		if errCode, msg := errOf(out); code != 400 || errCode != "INVALID_ARGUMENT" || msg != "days must be an integer from 1 to 90" {
			t.Errorf("%s = %d %s %q", body, code, errCode, msg)
		}
	}
	code, out := w.do(t, "POST", "/v1/mailbox/acme/messages/nope/keep", strings.NewReader(`{"days":3}`))
	if errCode, msg := errOf(out); code != 404 || errCode != "NOT_FOUND" || msg != "message not found" {
		t.Fatalf("unknown id = %d %s %q", code, errCode, msg)
	}

	for _, days := range []int64{90, 1} {
		code, out = w.do(t, "POST", "/v1/mailbox/acme/messages/"+ids[0]+"/keep", strings.NewReader(fmt.Sprintf(`{"days":%d}`, days)))
		m, _ := out["message"].(map[string]any)
		if code != 200 || m["id"] != ids[0] || m["expiresAt"] != float64(now+days*86_400_000) {
			t.Fatalf("keep %d = %d %v", days, code, out)
		}
		if _, hasBody := m["text"]; hasBody {
			t.Fatal("keep returns the summary, without bodies")
		}
	}
	// Only that message moved.
	if other, _ := w.svc.store.Get(in.ID, ids[1]); other.ExpiresAt != defaultExpiry {
		t.Errorf("the other message's expiry moved to %d", other.ExpiresAt)
	}
	// And it expires on its own new date.
	if n, _ := w.svc.store.Expire(in.ID, now+86_400_000); n != 1 {
		t.Fatalf("expired %d at the kept date", n)
	}
	if gone, _ := w.svc.store.Get(in.ID, ids[0]); gone != nil {
		t.Fatal("kept-for-a-day message outlived its day")
	}

	// The console takes the same rule.
	code, out = w.do(t, "POST", "/admin/mailbox/"+in.ID+"/messages/"+ids[1]+"/keep", strings.NewReader(`{"days":30}`))
	if code != 200 || out["message"].(map[string]any)["expiresAt"] != float64(now+30*86_400_000) {
		t.Fatalf("console keep = %d %v", code, out)
	}
	if code, _ = w.do(t, "POST", "/admin/mailbox/"+in.ID+"/messages/"+ids[1]+"/keep", strings.NewReader(`{"days":365}`)); code != 400 {
		t.Fatalf("console keep 365 = %d", code)
	}
}

// Moving an expiry earlier is a deferred delete: `write` may only extend, `full` may shorten, and
// the level is the one the key holds on THIS instance (an instance grant over a service-wide one).
func TestKeepShorteningNeedsFull(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", nil)
	var now int64 = 1_800_000_000_000
	w.svc.now = func() int64 { return now }
	rec, err := w.svc.DeliverLocal(in, "inbox", "jane@example.com", []byte("Subject: x\r\n\r\nbody"), false)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/mailbox/acme/messages/" + rec.ID + "/keep"
	w.auth.AddKey("k-write", auth.Grants{"mailbox": "write"})
	w.auth.AddKey("k-write-here", auth.Grants{"mailbox": "full", "mailbox:acme": "write"})
	w.auth.AddKey("k-full-here", auth.Grants{"mailbox": "read", "mailbox:acme": "full"})
	w.auth.AddKey("k-read", auth.Grants{"mailbox": "read"})
	const denied = "keeping a message for less time than it already has left deletes it sooner, which needs 'full' access; 'write' can only extend"

	if code, _ := w.doAs(t, "k-read", "POST", path, strings.NewReader(`{"days":30}`)); code != 403 {
		t.Fatalf("read key = %d", code)
	}
	// Write extends (7 → 30), and may set the same date again.
	for _, days := range []int{30, 30} {
		code, out := w.doAs(t, "k-write", "POST", path, strings.NewReader(fmt.Sprintf(`{"days":%d}`, days)))
		if code != 200 || out["message"].(map[string]any)["expiresAt"] != float64(now+30*86_400_000) {
			t.Fatalf("write extends to %d = %d %v", days, code, out)
		}
	}
	// Write cannot shorten (30 → 7), whichever way it holds write; nothing changes.
	for _, key := range []string{"k-write", "k-write-here"} {
		code, out := w.doAs(t, key, "POST", path, strings.NewReader(`{"days":7}`))
		if errCode, msg := errOf(out); code != 403 || errCode != "PERMISSION_DENIED" || msg != denied {
			t.Fatalf("%s shortening = %d %s %q", key, code, errCode, msg)
		}
	}
	if m, _ := w.svc.store.Get(in.ID, rec.ID); m.ExpiresAt != now+30*86_400_000 {
		t.Fatalf("a refused shorten moved the expiry to %d", m.ExpiresAt)
	}
	// Bounds and existence are answered first, as hosted.
	if code, _ := w.doAs(t, "k-write", "POST", path, strings.NewReader(`{"days":0}`)); code != 400 {
		t.Fatalf("write, days 0 = %d", code)
	}
	if code, _ := w.doAs(t, "k-write", "POST", "/v1/mailbox/acme/messages/nope/keep", strings.NewReader(`{"days":1}`)); code != 404 {
		t.Fatalf("write, unknown id = %d", code)
	}
	// Full shortens (30 → 2).
	code, out := w.doAs(t, "k-full-here", "POST", path, strings.NewReader(`{"days":2}`))
	if code != 200 || out["message"].(map[string]any)["expiresAt"] != float64(now+2*86_400_000) {
		t.Fatalf("full shortens = %d %v", code, out)
	}
	// The console may shorten too.
	if code, out = w.do(t, "POST", "/admin/mailbox/"+in.ID+"/messages/"+rec.ID+"/keep", strings.NewReader(`{"days":1}`)); code != 200 {
		t.Fatalf("console shortens = %d %v", code, out)
	}
}

func TestInfoSendPausedIsNull(t *testing.T) {
	w := newWorld(t, "")
	code, info := w.do(t, "GET", "/v1/mailbox/acme", nil)
	v, present := info["sendPaused"]
	if code != 200 || !present || v != nil {
		t.Fatalf("info = %d %v", code, info)
	}
}
