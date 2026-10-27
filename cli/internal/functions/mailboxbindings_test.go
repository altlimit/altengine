// env.mailbox, exercised the way it is meant to be used: a mailbox route calls a function with
// the arriving message, and the function answers it through the binding.

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
	"github.com/altlimit/altengine/cli/internal/mailbox"
)

func newMailboxServer(t *testing.T) (*http.ServeMux, *control.Registry, *mailbox.Service) {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	reg.OnValidateConfig("mailbox", mailbox.ValidateConfig)
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	NewHandler(reg, a, memStore(), mux).Register(mux)
	svc := mailbox.NewService(reg, mailbox.NewStore(""), mux, channel.NewHub(), mailbox.Options{})
	mailbox.NewHandler(reg, a, svc).Register(mux)
	return mux, reg, svc
}

func TestMailboxRouteCallsAFunctionThatReplies(t *testing.T) {
	mux, reg, svc := newMailboxServer(t)
	deployFn(t, mux, "onmail", `export default { async fetch(request, env) {
		const m = JSON.parse(await request.text());
		const info = await env.mailbox.info({ instance: "desk" });
		const sent = await env.mailbox.reply({ instance: "desk" }, m.id, {
			text: "got " + m.subject + " via " + request.headers.get("x-ae-trigger") + " at " + info.address,
		});
		const page = await env.mailbox.list({ instance: "desk" }, { direction: "out" });
		return new Response(JSON.stringify({ sent: sent.id, outbox: page.messages.length }));
	} };`, map[string]string{"mailbox:desk": "write"})
	in, err := reg.Create("mailbox", "desk")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SaveConfig(in, map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"function": "main/onmail"}}}}}); err != nil {
		t.Fatal(err)
	}

	raw := "From: Jane <jane@example.com>\r\nSubject: Help\r\nMessage-ID: <q1@example.com>\r\n\r\nplease\r\n"
	req := httptest.NewRequest("POST", "/v1/mailbox/desk/inject?to=inbox&authenticated=1", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("inject: %d %s", rec.Code, rec.Body.String())
	}
	var injected struct {
		Message struct{ ID string } `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &injected)
	svc.Wait()

	msg, _ := svc.Store().Get(in.ID, injected.Message.ID)
	if len(msg.Deliveries) != 1 || msg.Deliveries[0].Status != "ok:200" {
		t.Fatalf("delivery = %+v", msg.Deliveries)
	}
	out, _, _ := svc.Store().List(in.ID, mailbox.ListOptions{Direction: "out"})
	if len(out) != 1 {
		t.Fatalf("outbox = %+v", out)
	}
	sent, _ := svc.Store().Get(in.ID, out[0].ID)
	if *sent.Text != "got Help via mailbox at desk@mail.localhost" || sent.Subject != "Re: Help" || *sent.InReplyTo != "<q1@example.com>" {
		t.Fatalf("reply = %q %q", *sent.Text, sent.Subject)
	}
}

func TestMailboxBindingGrants(t *testing.T) {
	mux, _, _ := newMailboxServer(t)
	deployFn(t, mux, "reader", `export default { async fetch(request, env) {
		const page = await env.mailbox.list({ instance: "desk" });
		let refused = "";
		try { await env.mailbox.delete({ instance: "desk" }, "x"); } catch (e) { refused = String(e); }
		try { await env.mailbox.send({ instance: "desk" }, { to: ["a@b.com"], subject: "s", text: "t" }); } catch (e) { refused += " | " + String(e); }
		return new Response(JSON.stringify({ n: page.messages.length, refused }));
	} };`, map[string]string{"mailbox:desk": "read"})
	rec := invoke(t, mux, "/fn/main/reader")
	if rec.Code != 200 {
		t.Fatalf("invoke: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		N       int    `json:"n"`
		Refused string `json:"refused"`
	}
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.N != 0 || strings.Count(got.Refused, "insufficient grant") != 2 {
		t.Fatalf("got %+v", got)
	}
}

// A function attaches bytes or base64 text, and keeps a message longer — on a write grant only.
func TestMailboxBindingAttachmentsAndKeep(t *testing.T) {
	mux, reg, svc := newMailboxServer(t)
	in, err := reg.Create("mailbox", "desk")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SaveConfig(in, map[string]any{"settings": map[string]any{"sendMode": "any"}}); err != nil {
		t.Fatal(err)
	}
	deployFn(t, mux, "mailer", `export default { async fetch(request, env) {
		const desk = { instance: "desk" };
		const sent = await env.mailbox.send(desk, { to: ["a@example.com"], subject: "Files", text: "see attached", attachments: [
			{ filename: "u8.txt", contentType: "text/plain", content: new TextEncoder().encode("from bytes") },
			{ filename: "ab.bin", content: new Uint8Array([1, 2, 3]).buffer },
			{ filename: "b64.txt", content: "YmFzZTY0" },
			{ filename: "u16.bin", content: new Uint16Array([0x0201]) },
		] });
		let bad = "";
		try { await env.mailbox.send(desk, { to: ["a@example.com"], subject: "s", text: "t", attachments: [{ content: "%%%" }] }); } catch (e) { bad = String(e); }
		const kept = await env.mailbox.keep(desk, sent.id, 30);
		let badDays = "";
		try { await env.mailbox.keep(desk, sent.id, 91); } catch (e) { badDays = String(e); }
		let missing = "";
		try { await env.mailbox.keep(desk, "nope", 3); } catch (e) { missing = String(e); }
		let shorter = "";
		try { await env.mailbox.keep(desk, sent.id, 7); } catch (e) { shorter = String(e); }
		return new Response(JSON.stringify({ id: sent.id, count: sent.attachmentCount, kept: kept.expiresAt, bad, badDays, missing, shorter }));
	} };`, map[string]string{"mailbox:desk": "write"})
	rec := invoke(t, mux, "/fn/main/mailer")
	if rec.Code != 200 {
		t.Fatalf("invoke: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID      string `json:"id"`
		Count   int    `json:"count"`
		Kept    int64  `json:"kept"`
		Bad     string `json:"bad"`
		BadDays string `json:"badDays"`
		Missing string `json:"missing"`
		Shorter string `json:"shorter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// A write grant extends; moving the date earlier is refused, though the bearer the binding
	// carries internally could do it.
	if got.Count != 4 || !strings.Contains(got.Bad, "attachments[0].content must be base64") ||
		!strings.Contains(got.BadDays, "days must be an integer from 1 to 90") || !strings.Contains(got.Missing, "message not found") ||
		!strings.Contains(got.Shorter, "PERMISSION_DENIED: keeping a message for less time than it already has left deletes it sooner") {
		t.Fatalf("got %+v", got)
	}
	m, _ := svc.Store().Get(in.ID, got.ID)
	if m.ExpiresAt != got.Kept || got.Kept < m.ReceivedAt+30*86_400_000-60_000 {
		t.Fatalf("kept until %d, stored %d (received %d)", got.Kept, m.ExpiresAt, m.ReceivedAt)
	}
	want := []string{"from bytes", "\x01\x02\x03", "base64", "\x01\x02"}
	for i, w := range want {
		b, _ := svc.Store().Bytes(in.ID, m.Attachments[i].Key)
		if string(b) != w {
			t.Errorf("attachment %d = %q, want %q", i, b, w)
		}
	}

	// keep is a write: a read grant is refused before anything moves.
	deployFn(t, mux, "keeper", `export default { async fetch(request, env) {
		try { await env.mailbox.keep({ instance: "desk" }, "`+got.ID+`", 1); return new Response("kept"); } catch (e) { return new Response(String(e)); }
	} };`, map[string]string{"mailbox:desk": "read"})
	if rec := invoke(t, mux, "/fn/main/keeper"); !strings.Contains(rec.Body.String(), "insufficient grant") {
		t.Fatalf("keep on a read grant: %s", rec.Body.String())
	}
	if m2, _ := svc.Store().Get(in.ID, got.ID); m2.ExpiresAt != got.Kept {
		t.Fatal("a refused keep moved the expiry")
	}

	// A full grant may shorten.
	deployFn(t, mux, "owner", `export default { async fetch(request, env) {
		const m = await env.mailbox.keep({ instance: "desk" }, "`+got.ID+`", 1);
		return new Response(String(m.expiresAt));
	} };`, map[string]string{"mailbox:desk": "full"})
	if rec := invoke(t, mux, "/fn/main/owner"); rec.Code != 200 {
		t.Fatalf("keep on a full grant: %d %s", rec.Code, rec.Body.String())
	}
	if m3, _ := svc.Store().Get(in.ID, got.ID); m3.ExpiresAt >= got.Kept {
		t.Fatalf("full did not shorten: %d", m3.ExpiresAt)
	}
}
