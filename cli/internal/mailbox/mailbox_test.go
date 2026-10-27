package mailbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
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

type world struct {
	reg  *control.Registry
	svc  *Service
	auth *auth.Store
	mux  *http.ServeMux
	hub  *channel.Hub
	mu   sync.Mutex
	fnIn []fnCall
	// fnStatus is what the fake function answers.
	fnStatus int
}

type fnCall struct {
	path    string
	trigger string
	body    map[string]any
}

func newWorld(t *testing.T, dir string) *world {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	reg.OnValidateConfig("mailbox", ValidateConfig)
	w := &world{reg: reg, mux: http.NewServeMux(), hub: channel.NewHub(), fnStatus: 200}
	// A stand-in for the functions plane: records what it was handed.
	w.mux.HandleFunc("POST /fn/{instance}/{fn}", func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		w.fnIn = append(w.fnIn, fnCall{path: r.URL.Path, trigger: common.Trigger(r), body: body})
		status := w.fnStatus
		w.mu.Unlock()
		rw.WriteHeader(status)
	})
	w.svc = NewService(reg, NewStore(dir), w.mux, w.hub, Options{})
	w.auth = auth.NewStore(true)
	NewHandler(reg, w.auth, w.svc).Register(w.mux)
	reg.OnDelete("mailbox", w.svc.Drop)
	return w
}

func (w *world) create(t *testing.T, name string, cfg map[string]any) *control.Instance {
	t.Helper()
	in, err := w.reg.Create("mailbox", name)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		if err := w.reg.SaveConfig(in, cfg); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

func mimeMsg(from, subject, body string, attach map[string][]byte) []byte {
	out := OutMessage{From: AddressValue{Address: from, Name: "Jane"}, To: []AddressValue{{Address: "x@mail.localhost"}}, Subject: subject, Text: body, MessageID: "<m1@example.com>"}
	for name, b := range attach {
		out.Attachments = append(out.Attachments, OutAttachment{Filename: name, ContentType: "application/pdf", Content: b})
	}
	return BuildMessage(out)
}

func (w *world) do(t *testing.T, method, path string, body io.Reader) (int, map[string]any) {
	t.Helper()
	return w.doAs(t, "dev", method, path, body)
}

func (w *world) doAs(t *testing.T, key, method, path string, body io.Reader) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func rejection(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.SMTP
	}
	return ""
}

// --- addresses ---------------------------------------------------------------------------------

func TestAddresses(t *testing.T) {
	const d = "mail.localhost"
	cases := []struct {
		in   string
		want Recipient
		ok   bool
	}{
		{"acme@" + d, Recipient{"acme", "inbox", ""}, true},
		{"Acme.Signup-7f3a@" + d, Recipient{"acme", "signup-7f3a", ""}, true},
		// The first dot separates; later dots belong to the name.
		{"acme.eu.billing+Q3@" + d, Recipient{"acme", "eu.billing", "q3"}, true},
		{"acme@evil" + d, Recipient{}, false},
		{"acme@x." + d, Recipient{}, false},
		{"a--b.x@" + d, Recipient{}, false},
		{"ab@" + d, Recipient{}, false},
		{"acme.a b@" + d, Recipient{}, false},
	}
	for _, c := range cases {
		got, ok := ParseRecipient(c.in, d)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseRecipient(%q) = %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if _, ok := ParseRecipient("acme@"+d, ""); ok {
		t.Error("no domain must mean no address")
	}
	if AddressFor("acme", "inbox", d) != "acme@"+d || AddressFor("acme", "support", d) != "acme.support@"+d {
		t.Error("AddressFor")
	}
	if p, ok := ParseLocal("Support+Ticket42"); !ok || p != (Local{"support", "ticket42"}) {
		t.Errorf("ParseLocal = %v", p)
	}
	if _, ok := ParseLocal(`"quoted"`); ok {
		t.Error("quoted local accepted")
	}
	for raw, want := range map[string]string{"Support": "support", "acme.support@" + d: "support", "acme@" + d: "inbox", "": ""} {
		if got, err := NameFilter(raw, "acme", d); err != nil || got != want {
			t.Errorf("NameFilter(%q) = %q %v", raw, got, err)
		}
	}
	if _, err := NameFilter("other.support@"+d, "acme", d); err == nil {
		t.Error("a filter on another instance's address must be refused")
	}
	if v, err := ValidateLocal("Sign-Up.1", ""); err != nil || v != "sign-up.1" {
		t.Errorf("ValidateLocal = %q %v", v, err)
	}
	if _, err := ValidateLocal("a+b", ""); err == nil {
		t.Error("plus accepted")
	}
	if _, err := ValidateLocal(strings.Repeat("x", 60), "acme"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long local: %v", err)
	}
	if _, err := ValidateLocal(strings.Repeat("x", 59), "acme"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"x@y.com\r\nBcc: z@w.com", "<x@y.com>", "x@localhost"} {
		if _, err := ValidateOutboundAddress(bad, "to"); err == nil {
			t.Errorf("outbound %q accepted", bad)
		}
	}
	if v, _ := ValidateOutboundAddress(" x@y.com ", "to"); v != "x@y.com" {
		t.Errorf("outbound trim = %q", v)
	}
}

// --- config and routes -------------------------------------------------------------------------

func TestConfig(t *testing.T) {
	c := ParseConfig(map[string]any{})
	if c.Settings.AcceptMode != "any" || c.Settings.SendMode != "reply" || c.Settings.RetentionDays != 7 || !c.Settings.KeepRaw || len(c.Routes) != 0 {
		t.Fatalf("defaults = %+v", c)
	}
	if err := ValidateConfig(DefaultConfigMap()); err != nil {
		t.Fatalf("the default config must validate: %v", err)
	}
	for _, bad := range []map[string]any{
		{"settings": map[string]any{"retentionDays": 91}},
		{"settings": map[string]any{"inboundDailyCap": 0}},
		{"settings": map[string]any{"sendMode": "spam"}},
		{"settings": map[string]any{"keepRaw": "yes"}},
		{"settings": map[string]any{"region": "mars"}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"function": "fns/bad name"}}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"function": "fns/onEmail"}}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"channel": "rt/!reserved"}}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"channel": "nochannel"}}}}},
		{"routes": []any{map[string]any{"do": []any{}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"webhook": "http://x.example/h"}}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"webhook": "https://u:p@x.example/h"}}}}},
		{"routes": []any{map[string]any{"do": []any{map[string]any{"webhook": "https://x.example", "channel": "a/b"}}}}},
	} {
		if err := ValidateConfig(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := ValidateConfig(map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"function": "fns/onEmail"}}}}}); err == nil || !strings.Contains(err.Error(), "a-z0-9") {
		t.Errorf("function name message: %v", err)
	}
	routes, err := ValidateRoutes([]any{map[string]any{"match": map[string]any{"to": "Support*"}, "do": []any{map[string]any{"channel": "rt/inbox/eu", "includeText": true}}}})
	if err != nil || routes[0].Match.To != "support*" || routes[0].Do[0] != (Action{Channel: "rt/inbox/eu", IncludeText: true}) {
		t.Fatalf("routes = %+v %v", routes, err)
	}
}

func TestRouteMatching(t *testing.T) {
	yes := true
	_ = yes
	routes, err := ValidateRoutes([]any{
		map[string]any{"match": map[string]any{"to": "invoices", "hasAttachment": true}, "do": []any{map[string]any{"function": "billing/ingest"}}},
		map[string]any{"match": map[string]any{"from": "*@example.com", "subject": "urgent"}, "do": []any{map[string]any{"channel": "rt/urgent"}}},
		map[string]any{"match": map[string]any{}, "do": []any{map[string]any{"channel": "rt/all"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := MatchInput{Local: "invoices", From: "a@example.com", Subject: "URGENT: pay"}
	if r := MatchRoute(routes, base); r.Do[0].Channel != "rt/urgent" {
		t.Errorf("got %+v", r)
	}
	att := base
	att.AttachmentCount = 1
	if r := MatchRoute(routes, att); r.Do[0].Function != "billing/ingest" {
		t.Errorf("got %+v", r)
	}
	other := base
	other.From = "a@other.com"
	if r := MatchRoute(routes, other); r.Do[0].Channel != "rt/all" {
		t.Errorf("got %+v", r)
	}
}

func TestGlobIsLinear(t *testing.T) {
	if !GlobMatch("*@example.com", "Jane@Example.com") || !GlobMatch("sup?ort", "support") || GlobMatch("a*b", "ac") {
		t.Fatal("glob semantics")
	}
	evil := strings.Repeat("*a", 60) + "b"
	start := time.Now()
	if GlobMatch(evil, strings.Repeat("a", 250)) {
		t.Fatal("matched")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("pathological glob took %v", time.Since(start))
	}
}

func TestActionKeys(t *testing.T) {
	for _, a := range []Action{{Function: "f/x"}, {Channel: "c/y/z"}, {Channel: "c/y", IncludeText: true}, {Webhook: "https://h.example/x?a=b:c"}} {
		got, ok := ParseActionKey(ActionKey(a))
		if !ok || got != a {
			t.Errorf("round trip %+v -> %+v", a, got)
		}
	}
	if _, ok := ParseActionKey("carrier-pigeon:x"); ok {
		t.Error("unknown kind parsed")
	}
}

// --- the store ---------------------------------------------------------------------------------

func TestAdmission(t *testing.T) {
	s := NewStore("")
	now := time.Now().UnixMilli()
	for _, l := range []string{"a", "b"} {
		if r, _ := s.Admit("i", l, now, "any", 2); !r.OK {
			t.Fatalf("admit %s: %+v", l, r)
		}
	}
	if r, _ := s.Admit("i", "c", now, "any", 2); r.OK || r.Code != 452 || r.Reason != "452 4.2.2 mailbox is over its daily limit, try again later" {
		t.Fatalf("over cap: %+v", r)
	}
	if r, _ := s.Admit("i", "c", now+86_400_000, "any", 2); !r.OK {
		t.Fatal("a new day must reset the cap")
	}
	if r, _ := s.Admit("i", "x", now, "registered", 10); r.OK || r.Reason != "550 5.1.1 no such mailbox" {
		t.Fatalf("unregistered: %+v", r)
	}
	_, _ = s.PutAddress("i", "x", nil, nil, now)
	if r, _ := s.Admit("i", "x", now+86_400_000, "registered", 10); !r.OK {
		t.Fatal("registered refused")
	}
	_, _ = s.DisableAddress("i", "x", now)
	if r, _ := s.Admit("i", "x", now+86_400_000, "any", 10); r.OK || r.Reason != "550 5.1.1 mailbox disabled" {
		t.Fatalf("disabled: %+v", r)
	}
}

func newMsg(id, local, from string, verified bool, at int64) NewMessage {
	text := "hello " + id
	return NewMessage{ID: id, Direction: "in", Local: local, From: AddressValue{Address: from}, Subject: "s", Text: &text,
		Verified: verified, Status: "received", ReceivedAt: at, ExpiresAt: at + 86_400_000}
}

func TestListAndWait(t *testing.T) {
	s := NewStore("")
	base := time.Now().UnixMilli()
	var ids []string
	for i := 0; i < 5; i++ {
		id := NewMessageID(base + int64(i))
		ids = append(ids, id)
		if _, err := s.StoreMessage("i", newMsg(id, "a", "x@y.com", false, base), nil); err != nil {
			t.Fatal(err)
		}
	}
	page, cursor, _ := s.List("i", ListOptions{Limit: 2})
	if len(page) != 2 || page[0].ID != ids[4] || cursor == nil || *cursor != ids[3] {
		t.Fatalf("newest-first page = %v %v", page, cursor)
	}
	next, _, _ := s.List("i", ListOptions{Limit: 2, Cursor: *cursor})
	if next[0].ID != ids[2] {
		t.Fatalf("second page = %v", next)
	}
	after, _, _ := s.List("i", ListOptions{After: ids[2]})
	if len(after) != 2 || after[0].ID != ids[3] {
		t.Fatalf("after = %v", after)
	}

	// A waiter parked now is woken by the next message for its name, not another's.
	done := make(chan []Summary, 1)
	go func() {
		got, _ := s.WaitFor(context.Background(), "i", "b", ResolveAfter("now", time.Now().UnixMilli()), 10, 5*time.Second)
		done <- got
	}()
	time.Sleep(50 * time.Millisecond)
	_, _ = s.StoreMessage("i", newMsg(NewMessageID(time.Now().UnixMilli()), "a", "x@y.com", false, base), nil)
	wantID := NewMessageID(time.Now().UnixMilli() + 1)
	_, _ = s.StoreMessage("i", newMsg(wantID, "b", "x@y.com", false, base), nil)
	select {
	case got := <-done:
		if len(got) != 1 || got[0].ID != wantID {
			t.Fatalf("woke with %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken")
	}
	got, _ := s.WaitFor(context.Background(), "i", "zzz", ResolveAfter("now", time.Now().UnixMilli()), 10, 50*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("timeout returned %v", got)
	}
}

func TestBodiesAreBounded(t *testing.T) {
	s := NewStore("")
	now := time.Now().UnixMilli()
	big := strings.Repeat("é", BodyMax+10)
	m := newMsg(NewMessageID(now), "a", "x@y.com", false, now)
	m.Text = &big
	_, _ = s.StoreMessage("i", m, nil)
	rec, _ := s.Get("i", m.ID)
	if !rec.TextTruncated || len([]rune(*rec.Text)) != BodyMax {
		t.Fatalf("truncated=%v len=%d", rec.TextTruncated, len([]rune(*rec.Text)))
	}
}

func TestOnlyAuthenticatedSendersUnlockReply(t *testing.T) {
	s := NewStore("")
	now := time.Now().UnixMilli()
	_, _ = s.StoreMessage("i", newMsg(NewMessageID(now), "a", "Forged@Bank.com", false, now), nil)
	_, _ = s.StoreMessage("i", newMsg(NewMessageID(now+1), "a", "jane@example.com", true, now), nil)
	err := s.PrepareSend("i", "a", []string{"forged@bank.com"}, true, nil, now)
	var ae *common.APIError
	if !errors.As(err, &ae) || ae.Code != "NOT_A_REPLY" {
		t.Fatalf("unverified sender unlocked reply: %v", err)
	}
	if err := s.PrepareSend("i", "a", []string{"jane@example.com"}, true, nil, now); err != nil {
		t.Fatal(err)
	}
	// The instance's own cap, and a slot given back.
	one := int64(1)
	if err := s.PrepareSend("i", "a", []string{"x@y.com"}, false, &one, now+86_400_000); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareSend("i", "a", []string{"x@y.com"}, false, &one, now+86_400_000); !errors.As(err, &ae) || ae.Code != "SEND_LIMIT" {
		t.Fatalf("cap: %v", err)
	}
	s.ReturnSend("i", now+86_400_000)
	if err := s.PrepareSend("i", "a", []string{"x@y.com"}, false, &one, now+86_400_000); err != nil {
		t.Fatalf("returned slot: %v", err)
	}
}

func TestRetrySchedule(t *testing.T) {
	if RetryAt(1, 0) != 60_000 || RetryAt(2, 0) != 300_000 || RetryAt(4, 0) != 7_200_000 || RetryAt(5, 0) != 0 {
		t.Fatal("retry schedule differs from hosted")
	}
}

// --- authentication verdict --------------------------------------------------------------------

func TestSenderVerified(t *testing.T) {
	const as = "mail.localhost"
	ours := as + "; dkim=pass header.d=example.com; spf=fail smtp.mailfrom=x@evil.com; dmarc=pass header.from=example.com"
	forged := "evil.example; dmarc=pass header.from=bank.com"
	if !SenderVerified([]string{ours, forged}, "jane@example.com", as) {
		t.Error("our verdict")
	}
	if SenderVerified([]string{forged}, "ceo@bank.com", as) || SenderVerified(nil, "jane@example.com", as) {
		t.Error("forged verdict believed")
	}
	if SenderVerified([]string{as + "; dkim=pass header.d=other.com"}, "jane@example.com", as) {
		t.Error("unaligned dkim")
	}
	if !SenderVerified([]string{as + "; dkim=pass header.d=example.com"}, "jane@mail.example.com", as) {
		t.Error("relaxed alignment")
	}
	if !SenderVerified([]string{as + "; spf=pass smtp.mailfrom=bounce@example.com"}, "jane@example.com", as) {
		t.Error("spf")
	}
	if SenderVerified([]string{as + "; dmarc=fail header.from=example.com"}, "jane@example.com", as) {
		t.Error("dmarc fail")
	}
	// The emulator's own stamp, both ways.
	if !SenderVerified([]string{authStamp(as, "a@b.com", true)}, "a@b.com", as) || SenderVerified([]string{authStamp(as, "a@b.com", false)}, "a@b.com", as) {
		t.Error("authStamp")
	}
}

// --- inbound -----------------------------------------------------------------------------------

func TestInboundRefusesBeforeTheBody(t *testing.T) {
	w := newWorld(t, "")
	w.create(t, "acme", nil)
	for to, want := range map[string]string{
		"a@nope.com":            rejectNoAddress,
		"zzzz.a@mail.localhost": rejectNoAddress,
	} {
		if _, _, err := w.svc.AdmitAddress(to, 10); rejection(err) != want {
			t.Errorf("%s: %v", to, err)
		}
	}
	if _, _, err := w.svc.AdmitAddress("acme.a@mail.localhost", 26*1024*1024); rejection(err) != rejectTooLarge {
		t.Errorf("too large: %v", err)
	}
	_, r, err := w.svc.AdmitAddress("acme.Help+t@mail.localhost", 10)
	if err != nil || r != (Recipient{"acme", "help", "t"}) {
		t.Fatalf("admit = %v %v", r, err)
	}
}

func TestIngestStoresDropsAndRoutes(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		w := newWorld(t, dir)
		w.reg.Create("channel", "rt")
		in := w.create(t, "acme", map[string]any{"routes": []any{
			map[string]any{"match": map[string]any{"hasAttachment": true}, "do": []any{map[string]any{"channel": "rt/files"}}},
			map[string]any{"do": []any{map[string]any{"channel": "rt/all"}}},
		}})
		big, err := w.svc.DeliverLocal(in, "help+x", "bounce@example.com", mimeMsg("jane@example.com", "big", "hi", map[string][]byte{"huge.pdf": make([]byte, MaxAttachmentBytes+1)}), false)
		if err != nil {
			t.Fatal(err)
		}
		if a := big.Attachments[0]; a.Rejected == nil || *a.Rejected != "too large" || a.Key != "" || *a.Filename != "huge.pdf" || big.AttachmentCount != 0 {
			t.Fatalf("oversize attachment = %+v", a)
		}
		small, err := w.svc.DeliverLocal(in, "help", "bounce@example.com", mimeMsg("jane@example.com", "small", "hi", map[string][]byte{"ok.pdf": bytes.Repeat([]byte{7}, 1000)}), false)
		if err != nil {
			t.Fatal(err)
		}
		a := small.Attachments[0]
		if a.Rejected != nil || a.Size != 1000 || a.ContentType != "application/pdf" {
			t.Fatalf("attachment = %+v", a)
		}
		if b, ok := w.svc.store.Bytes(in.ID, a.Key); !ok || len(b) != 1000 {
			t.Fatal("attachment bytes not stored")
		}
		if b, ok := w.svc.store.Bytes(in.ID, small.RawKey); !ok || !bytes.HasPrefix(b, []byte("Authentication-Results: mail.localhost;")) {
			t.Fatal("raw copy not kept with our verdict on top")
		}
		if small.From != (AddressValue{Address: "jane@example.com", Name: "Jane"}) || small.Verified {
			t.Fatalf("from = %+v verified=%v", small.From, small.Verified)
		}
		w.svc.Wait()
		got, _ := w.svc.store.Get(in.ID, small.ID)
		if len(got.Deliveries) != 1 || got.Deliveries[0].Action != "channel:rt/files" || got.Deliveries[0].Status != "ok:0 subscribers" {
			t.Fatalf("small deliveries = %+v", got.Deliveries)
		}
		got, _ = w.svc.store.Get(in.ID, big.ID)
		if got.Deliveries[0].Action != "channel:rt/all" {
			t.Fatalf("big deliveries = %+v", got.Deliveries)
		}
		// Delete takes the bytes with the row.
		if n, _ := w.svc.store.Remove(in.ID, []string{small.ID}); n != 1 {
			t.Fatal("remove")
		}
		if _, ok := w.svc.store.Bytes(in.ID, a.Key); ok {
			t.Fatal("attachment bytes outlived the message")
		}
		w.reg.DeleteInstance("mailbox", in.ID)
	}
}

func TestKeepRawOffAndRetention(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", map[string]any{"settings": map[string]any{"keepRaw": false, "retentionDays": 2}})
	m, err := w.svc.DeliverLocal(in, "a", "x@y.com", mimeMsg("x@y.com", "s", "b", nil), true)
	if err != nil {
		t.Fatal(err)
	}
	if m.RawKey != "" || m.ExpiresAt != m.ReceivedAt+2*86_400_000 || !m.Verified {
		t.Fatalf("rec = %+v", m.Summary)
	}
	// Expiry removes it.
	if n, _ := w.svc.store.Expire(in.ID, m.ExpiresAt); n != 1 {
		t.Fatal("not expired")
	}
}

func TestMIMEParsing(t *testing.T) {
	raw := "From: =?utf-8?q?J=C3=BCrgen?= <j@example.com>\r\nTo: a@x.com, \"B\" <b@x.com>\r\nSubject: =?iso-8859-1?q?Gr=FC=DFe?=\r\n" +
		"Authentication-Results: top; dkim=pass\r\nAuthentication-Results: below; dkim=fail\r\n" +
		"Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\nContent-Type: multipart/alternative; boundary=Y\r\n\r\n" +
		"--Y\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nH=E9llo\r\n" +
		"--Y\r\nContent-Type: text/html\r\n\r\n<p>Hi</p>\r\n--Y--\r\n" +
		"--X\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"a.png\"\r\nContent-ID: <c1>\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\n--X--\r\n"
	e := ParseEmail([]byte(raw))
	if len(e.From) != 1 || e.From[0].Name != "Jürgen" || len(e.To) != 2 || e.Subject != "Grüße" {
		t.Fatalf("headers = %+v %q", e.From, e.Subject)
	}
	if e.Text == nil || strings.TrimSpace(*e.Text) != "Héllo" || e.HTML == nil || strings.TrimSpace(*e.HTML) != "<p>Hi</p>" {
		t.Fatalf("bodies = %v %v", e.Text, e.HTML)
	}
	if len(e.Attachments) != 1 || string(e.Attachments[0].Content) != "hello" || e.Attachments[0].ContentID != "<c1>" || e.Attachments[0].Disposition != "inline" {
		t.Fatalf("attachments = %+v", e.Attachments)
	}
	if len(e.AuthResults) != 2 || !strings.HasPrefix(e.AuthResults[0], "top") {
		t.Fatalf("auth results order = %v", e.AuthResults)
	}
	// Malformed beyond reading: the whole thing is the body, nothing fails.
	if e := ParseEmail([]byte("not a message at all")); e.Text == nil {
		t.Fatal("malformed message lost")
	}
	// What BuildMessage writes, ParseEmail reads back.
	built := BuildMessage(OutMessage{From: AddressValue{Address: "a@b.com"}, To: []AddressValue{{Address: "c@d.com"}}, Subject: "Ünïcode",
		Text: "line1\nline2", HTML: "<b>x</b>", Attachments: []OutAttachment{{Filename: "f.txt", ContentType: "text/plain", Content: []byte("file")}}})
	e = ParseEmail(built)
	if e.Subject != "Ünïcode" || *e.Text != "line1\r\nline2" || *e.HTML != "<b>x</b>" || len(e.Attachments) != 1 || string(e.Attachments[0].Content) != "file" {
		t.Fatalf("round trip = %q %q %q %+v", e.Subject, *e.Text, *e.HTML, e.Attachments)
	}
}

// --- triggers ----------------------------------------------------------------------------------

func TestChannelPayload(t *testing.T) {
	text := "hello"
	key := "secret-key"
	m := &Record{Summary: Summary{ID: "id1", Local: "help", PlusTag: &text, From: AddressValue{Address: "jane@example.com"}, To: []AddressValue{{Address: "help+t@x"}}},
		Text: &text, Attachments: []Attachment{{Idx: 0, ContentType: "application/pdf", Size: 3, Key: key}}}
	p := ChannelPayload("support", "inbox", m, false)
	if p["type"] != "mailbox.received" || p["local"] != "help" {
		t.Fatalf("payload = %v", p)
	}
	if _, has := p["text"]; has {
		t.Fatal("text without includeText")
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), key) || strings.Contains(string(b), "url") {
		t.Fatal("a storage key or URL reached a channel")
	}
	long := strings.Repeat("é\"", 200_000)
	m.Text = &long
	p = ChannelPayload("support", "inbox", m, true)
	framed, _ := json.Marshal(map[string]any{"channel": "inbox", "data": p, "ts": time.Now().UnixMilli()})
	if len(framed) > channel.MaxMessageBytes || p["textTruncated"] != true {
		t.Fatalf("framed %d bytes, truncated=%v", len(framed), p["textTruncated"])
	}
	if v, cut := ClipBytes("ééé", 3); v != "é" || !cut {
		t.Fatalf("ClipBytes = %q %v", v, cut)
	}
}

func TestFunctionTrigger(t *testing.T) {
	w := newWorld(t, "")
	w.reg.Create("functions", "fns")
	in := w.create(t, "acme", map[string]any{"routes": []any{
		map[string]any{"match": map[string]any{"to": "ok"}, "do": []any{map[string]any{"function": "fns/on-mail"}}},
		map[string]any{"match": map[string]any{"to": "gone"}, "do": []any{map[string]any{"function": "nofns/x"}, map[string]any{"channel": "nochan/x"}}},
	}})
	m, err := w.svc.DeliverLocal(in, "ok", "x@y.com", mimeMsg("jane@example.com", "Invoice", "pay me", map[string][]byte{"inv.pdf": []byte("%PDF")}), true)
	if err != nil {
		t.Fatal(err)
	}
	w.svc.Wait()
	w.mu.Lock()
	calls := append([]fnCall{}, w.fnIn...)
	w.mu.Unlock()
	if len(calls) != 1 || calls[0].path != "/fn/fns/on-mail" || calls[0].trigger != "mailbox" {
		t.Fatalf("calls = %+v", calls)
	}
	body := calls[0].body
	if body["type"] != "mailbox.received" || body["text"] == nil || body["verified"] != true {
		t.Fatalf("payload = %v", body)
	}
	atts := body["attachments"].([]any)
	url := atts[0].(map[string]any)["url"].(string)
	// The attachment link in the payload downloads the attachment.
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != 200 || rec.Body.String() != "%PDF" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Disposition"))
	}
	got, _ := w.svc.store.Get(in.ID, m.ID)
	if got.Deliveries[0].Status != "ok:200" || got.Deliveries[0].Attempts != 1 {
		t.Fatalf("delivery = %+v", got.Deliveries)
	}

	// Targets that do not exist are permanent errors, recorded on the first attempt.
	m2, _ := w.svc.DeliverLocal(in, "gone", "x@y.com", mimeMsg("jane@example.com", "s", "b", nil), true)
	w.svc.Wait()
	got, _ = w.svc.store.Get(in.ID, m2.ID)
	want := map[string]string{"function:nofns/x": "error:no functions instance 'nofns'", "channel:nochan/x": "error:no channel instance 'nochan'"}
	for _, d := range got.Deliveries {
		if want[d.Action] != d.Status || d.NextAt != nil || d.Attempts != 1 {
			t.Errorf("delivery %+v", d)
		}
	}

	// A failing function is retried on the hosted schedule; replay runs the current routes now.
	w.fnStatus = 500
	m3, _ := w.svc.DeliverLocal(in, "ok", "x@y.com", mimeMsg("jane@example.com", "s", "b", nil), true)
	w.svc.Wait()
	got, _ = w.svc.store.Get(in.ID, m3.ID)
	if d := got.Deliveries[0]; d.Status != "pending" || d.Attempts != 1 || d.NextAt == nil {
		t.Fatalf("retry = %+v", d)
	}
	w.fnStatus = 202
	replayed, err := w.svc.Replay(in, m3.ID)
	if err != nil || replayed.Deliveries[0].Status != "ok:202" {
		t.Fatalf("replay = %+v %v", replayed.Deliveries, err)
	}

	// The background tick re-drives a due retry.
	w.fnStatus = 500
	m4, _ := w.svc.DeliverLocal(in, "ok", "x@y.com", mimeMsg("jane@example.com", "s", "b", nil), true)
	w.svc.Wait()
	w.fnStatus = 200
	real := w.svc.now
	w.svc.now = func() int64 { return real() + 2*60_000 }
	w.svc.Tick()
	w.svc.now = real
	got, _ = w.svc.store.Get(in.ID, m4.ID)
	if d := got.Deliveries[0]; d.Status != "ok:200" || d.Attempts != 2 {
		t.Fatalf("after tick = %+v", d)
	}
}

func TestWebhookTrigger(t *testing.T) {
	type hit struct {
		sig, ts string
		body    []byte
	}
	hits := make(chan hit, 4)
	recv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- hit{r.Header.Get(SignatureHeader), r.Header.Get(TimestampHeader), b}
		rw.WriteHeader(204)
	}))
	defer recv.Close()
	w := newWorld(t, "")
	w.svc.client = recv.Client()
	w.svc.client.Timeout = WebhookTimeout
	in := w.create(t, "acme", map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"webhook": recv.URL + "/hook"}}}}})
	m, err := w.svc.DeliverLocal(in, "a", "x@y.com", mimeMsg("jane@example.com", "s", "body", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	w.svc.Wait()
	h := <-hits
	secret, _ := w.svc.store.WebhookSecret(in.ID)
	var ts int64
	fmt.Sscan(h.ts, &ts)
	if h.sig != SignWebhook(secret, ts, h.body) || !strings.HasPrefix(h.sig, "v1=") {
		t.Fatalf("signature %q does not verify", h.sig)
	}
	var payload map[string]any
	_ = json.Unmarshal(h.body, &payload)
	if payload["id"] != m.ID || payload["text"] == nil {
		t.Fatalf("payload = %v", payload)
	}
	got, _ := w.svc.store.Get(in.ID, m.ID)
	if got.Deliveries[0].Status != "ok:204" {
		t.Fatalf("delivery = %+v", got.Deliveries)
	}
	_ = tls.VersionTLS12
}

// --- sending -----------------------------------------------------------------------------------

func TestSendRules(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", nil)
	// Reply mode: a stranger is refused; an authenticated sender can be answered, threaded.
	_, err := w.svc.Send(in, map[string]any{"to": []any{"x@example.com"}, "subject": "hi", "text": "t"})
	var ae *common.APIError
	if !errors.As(err, &ae) || ae.Code != "NOT_A_REPLY" {
		t.Fatalf("stranger: %v", err)
	}
	raw := "From: Jane <jane@example.com>\r\nReply-To: desk@example.com\r\nSubject: Help\r\nMessage-ID: <orig@example.com>\r\nReferences: <root@example.com>\r\n\r\nbody\r\n"
	orig, err := w.svc.DeliverLocal(in, "support", "jane@example.com", []byte(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Send(in, map[string]any{"replyToMessageId": orig.ID, "text": "on it"}); !errors.As(err, &ae) || ae.Code != "NOT_A_REPLY" {
		// Reply-To differs from the authenticated From: desk@ never wrote to us.
		t.Fatalf("reply to an unverified Reply-To: %v", err)
	}
	sent, err := w.svc.Send(in, map[string]any{"replyToMessageId": orig.ID, "to": []any{"jane@example.com"}, "text": "on it"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Subject != "Re: Help" || sent.Local != "support" || sent.From.Address != "acme.support@mail.localhost" || *sent.InReplyTo != "<orig@example.com>" || sent.Status != StatusNotDelivered {
		t.Fatalf("sent = %+v", sent)
	}
	full, _ := w.svc.store.Get(in.ID, sent.ID)
	if *full.References != "<root@example.com> <orig@example.com>" {
		t.Fatalf("references = %v", *full.References)
	}
	// Off means off.
	_ = w.reg.SaveConfig(in, map[string]any{"settings": map[string]any{"sendMode": "off"}})
	if _, err := w.svc.Send(in, map[string]any{"to": []any{"jane@example.com"}, "subject": "x", "text": "y"}); !errors.As(err, &ae) || ae.Code != "SEND_DISABLED" {
		t.Fatalf("off: %v", err)
	}
	// Validation.
	// The reply above already took one of today's slots.
	_ = w.reg.SaveConfig(in, map[string]any{"settings": map[string]any{"sendMode": "any", "dailySendCap": 2}})
	tooMany := []any{}
	for i := 0; i < 51; i++ {
		tooMany = append(tooMany, fmt.Sprintf("u%d@example.com", i))
	}
	for _, bad := range []map[string]any{
		{"to": tooMany, "subject": "x", "text": "y"},
		{"to": []any{"a@b.com"}, "text": "y"},
		{"to": []any{"a@b.com"}, "subject": "x"},
		{"to": []any{"<a@b.com>"}, "subject": "x", "text": "y"},
	} {
		if _, err := w.svc.Send(in, bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := w.svc.Send(in, map[string]any{"to": []any{"a@b.com"}, "subject": "x", "text": "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Send(in, map[string]any{"to": []any{"a@b.com"}, "subject": "x", "text": "y"}); !errors.As(err, &ae) || ae.Code != "SEND_LIMIT" {
		t.Fatalf("instance cap: %v", err)
	}
}

func TestLoopback(t *testing.T) {
	w := newWorld(t, "")
	a := w.create(t, "alpha", map[string]any{"settings": map[string]any{"sendMode": "any"}})
	b := w.create(t, "bravo", nil)
	sent, err := w.svc.Send(a, map[string]any{"to": []any{"bravo.desk@mail.localhost", "someone@example.com"}, "subject": "Ping", "text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent.Status, "delivered locally to bravo.desk@mail.localhost") {
		t.Fatalf("status = %q", sent.Status)
	}
	got, _, _ := w.svc.store.List(b.ID, ListOptions{})
	if len(got) != 1 || got[0].Local != "desk" || got[0].From.Address != "alpha@mail.localhost" || !got[0].Verified {
		t.Fatalf("looped = %+v", got)
	}
	// Bravo is in reply mode and alpha wrote to it authenticated: bravo may answer.
	if _, err := w.svc.Send(b, map[string]any{"replyToMessageId": got[0].ID, "text": "pong"}); err != nil {
		t.Fatal(err)
	}
	inA, _, _ := w.svc.store.List(a.ID, ListOptions{Direction: "in"})
	if len(inA) != 1 || inA[0].Subject != "Re: Ping" {
		t.Fatalf("reply did not loop back: %+v", inA)
	}
	w.svc.SetLoopback(false)
	sent, _ = w.svc.Send(a, map[string]any{"to": []any{"bravo@mail.localhost"}, "subject": "x", "text": "y"})
	if sent.Status != StatusNotDelivered {
		t.Fatalf("loopback off: %q", sent.Status)
	}
}

// --- the HTTP surface ----------------------------------------------------------------------------

func TestV1Surface(t *testing.T) {
	w := newWorld(t, "")
	code, info := w.do(t, "GET", "/v1/mailbox/acme", nil)
	if code != 200 || info["address"] != "acme@mail.localhost" || info["acceptMode"] != "any" {
		t.Fatalf("info = %d %v", code, info)
	}
	// An instance whose name cannot be a slug has no address, but still takes inject.
	_, odd := w.do(t, "GET", "/v1/mailbox/My_Box", nil)
	if odd["address"] != nil || odd["domain"] != "mail.localhost" {
		t.Fatalf("odd = %v", odd)
	}

	code, out := w.do(t, "POST", "/v1/mailbox/acme/inject?to=help", bytes.NewReader(mimeMsg("jane@example.com", "Hi", "body", map[string][]byte{"a.pdf": []byte("PDF")})))
	if code != 201 {
		t.Fatalf("inject = %d %v", code, out)
	}
	id := out["message"].(map[string]any)["id"].(string)
	code, out = w.do(t, "POST", "/v1/mailbox/acme/inject?to=other.help@mail.localhost", strings.NewReader("Subject: x\r\n\r\nb"))
	if code != 422 || out["rejected"] != rejectNoAddress {
		t.Fatalf("inject to another instance = %d %v", code, out)
	}
	if code, out = w.do(t, "POST", "/v1/mailbox/acme/inject", strings.NewReader("x")); code != 400 {
		t.Fatalf("inject without to = %d", code)
	}

	code, out = w.do(t, "GET", "/v1/mailbox/acme/messages/"+id+"?raw=1", nil)
	msg := out["message"].(map[string]any)
	if code != 200 || msg["hasRaw"] != true || out["rawUrl"] == nil {
		t.Fatalf("get = %d %v", code, out)
	}
	if _, leaked := msg["rawKey"]; leaked {
		t.Fatal("storage key in the response")
	}
	att := msg["attachments"].([]any)[0].(map[string]any)
	if att["stored"] != true {
		t.Fatalf("attachment = %v", att)
	}
	code, out = w.do(t, "GET", "/v1/mailbox/acme/messages/"+id+"/attachments/0", nil)
	if code != 200 || out["filename"] != "a.pdf" {
		t.Fatalf("attachment link = %d %v", code, out)
	}
	if code, _ = w.do(t, "GET", "/v1/mailbox/acme/messages/"+id+"/attachments/9", nil); code != 404 {
		t.Fatal("missing attachment")
	}

	if code, out = w.do(t, "GET", "/v1/mailbox/acme/messages?unread=1", nil); len(out["messages"].([]any)) != 1 {
		t.Fatalf("unread = %v", out)
	}
	w.do(t, "POST", "/v1/mailbox/acme/messages/"+id+"/read", nil)
	if _, out = w.do(t, "GET", "/v1/mailbox/acme/messages?unread=1", nil); len(out["messages"].([]any)) != 0 {
		t.Fatalf("still unread: %v", out)
	}
	for q, want := range map[string]int{"?direction=sideways": 400, "?wait=x&after=now": 400, "?wait=1": 400, "?to=a+b@x": 400, "?limit=NaN": 200} {
		if code, _ := w.do(t, "GET", "/v1/mailbox/acme/messages"+q, nil); code != want {
			t.Errorf("%s = %d, want %d", q, code, want)
		}
	}
	code, out = w.do(t, "GET", "/v1/mailbox/acme/messages?wait=0&after=0", nil)
	if code != 200 || out["cursor"] != id {
		t.Fatalf("wait from 0 = %d %v", code, out)
	}

	code, out = w.do(t, "POST", "/v1/mailbox/acme/addresses", strings.NewReader(`{"label":"minted"}`))
	if code != 201 || !strings.HasPrefix(out["local"].(string), "m") || !strings.HasPrefix(out["address"].(string), "acme.m") {
		t.Fatalf("mint = %d %v", code, out)
	}
	code, out = w.do(t, "DELETE", "/v1/mailbox/acme/addresses/help", nil)
	if code != 200 || out["disabledAt"] == nil {
		t.Fatalf("disable = %d %v", code, out)
	}
	code, out = w.do(t, "POST", "/v1/mailbox/acme/inject?to=help", strings.NewReader("Subject: x\r\n\r\nb"))
	if code != 422 || out["rejected"] != "550 5.1.1 mailbox disabled" {
		t.Fatalf("to a disabled address = %d %v", code, out)
	}

	if code, out = w.do(t, "DELETE", "/v1/mailbox/acme/messages/"+id, nil); code != 200 || out["deleted"] != float64(1) {
		t.Fatalf("delete = %d %v", code, out)
	}
	if code, _ = w.do(t, "GET", "/v1/mailbox/acme/messages/"+id, nil); code != 404 {
		t.Fatal("deleted message still readable")
	}
}

func TestComposeAndEml(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", map[string]any{"settings": map[string]any{"acceptMode": "registered"}})
	body := `{"from":"jane@example.com","to":"acme@mail.localhost","subject":"Hi","text":"t","html":"<b>h</b>","attachments":[{"filename":"n.txt","contentType":"text/plain","content":"bm90ZQ=="}]}`
	code, out := w.do(t, "POST", "/admin/mailbox/"+in.ID+"/compose", strings.NewReader(body))
	if code != 422 || out["rejected"] != "550 5.1.1 no such mailbox" {
		t.Fatalf("registered-only = %d %v", code, out)
	}
	_, _ = w.svc.store.PutAddress(in.ID, "inbox", nil, nil, 0)
	code, out = w.do(t, "POST", "/admin/mailbox/"+in.ID+"/compose", strings.NewReader(body))
	if code != 201 {
		t.Fatalf("compose = %d %v", code, out)
	}
	m := out["message"].(map[string]any)
	if m["verified"] != true || m["html"] != "<b>h</b>" || len(m["attachments"].([]any)) != 1 {
		t.Fatalf("composed = %v", m)
	}
	code, out = w.do(t, "POST", "/admin/mailbox/"+in.ID+"/eml?to=inbox&authenticated=0", strings.NewReader("From: a@b.com\r\nSubject: raw\r\n\r\nx"))
	if code != 201 || out["message"].(map[string]any)["verified"] != false {
		t.Fatalf("eml = %d %v", code, out)
	}
	code, out = w.do(t, "GET", "/admin/mailbox/"+in.ID+"/messages/"+m["id"].(string), nil)
	if code != 200 || out["rawUrl"] == nil || out["message"].(map[string]any)["attachments"].([]any)[0].(map[string]any)["url"] == nil {
		t.Fatalf("admin get = %d %v", code, out)
	}
}

// --- SMTP --------------------------------------------------------------------------------------

func TestSMTPListener(t *testing.T) {
	w := newWorld(t, "")
	in := w.create(t, "acme", nil)
	srv, err := w.svc.ListenSMTP("127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	c, err := smtp.Dial(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("app@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("nobody@mail.localhost"); err == nil || !strings.Contains(err.Error(), "550") || !strings.Contains(err.Error(), "5.1.1 no such address") {
		t.Fatalf("unknown recipient: %v", err)
	}
	if err := c.Rcpt("acme.orders+42@mail.localhost"); err != nil {
		t.Fatal(err)
	}
	wc, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(wc, "From: App <app@example.com>\r\nSubject: Order 42\r\n\r\n.leading dot\r\nThanks\r\n")
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Quit()

	got, _, _ := w.svc.store.List(in.ID, ListOptions{})
	if len(got) != 1 || got[0].Local != "orders" || got[0].PlusTag == nil || *got[0].PlusTag != "42" || got[0].Subject != "Order 42" || !got[0].Verified {
		t.Fatalf("received = %+v", got)
	}
	rec, _ := w.svc.store.Get(in.ID, got[0].ID)
	// The client stuffs the leading dot; the listener must take it back off.
	if !strings.HasPrefix(*rec.Text, ".leading dot") || strings.HasPrefix(*rec.Text, "..") {
		t.Fatalf("dot-stuffing not undone: %q", *rec.Text)
	}
}
