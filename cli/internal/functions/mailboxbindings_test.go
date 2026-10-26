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
