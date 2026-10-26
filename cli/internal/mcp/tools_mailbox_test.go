package mcp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func injectMail(t *testing.T, h *Handler, instance, to, query, raw string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/mailbox/"+instance+"/inject?to="+to+query, strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("inject: %d %s", rec.Code, rec.Body.String())
	}
}

const signupMail = "From: GitHub <noreply@github.com>\r\nTo: acme.signup@mail.localhost\r\nSubject: Your code\r\n" +
	"Message-ID: <abc@github.com>\r\n\r\nUse 482913 to sign in. (c) 2026. Or visit https://github.com/verify?t=1.\r\n"

func TestMailboxToolsRoundTrip(t *testing.T) {
	h := newLiveHandler(t)
	mustCreate(t, h, "mailbox", "acme")

	info := runTool(t, h, "mailbox_info", map[string]any{"instance": "acme"})
	if info["address"] != "acme@mail.localhost" || info["pattern"] != "acme.<anything>@mail.localhost" || info["sendMode"] != "reply" {
		t.Fatalf("info = %v", info)
	}

	// Nothing yet: a zero-second wait answers at once, with no message.
	empty := runTool(t, h, "mailbox_wait_for_message", map[string]any{"instance": "acme", "after": "now", "timeoutSeconds": 0})
	if empty["message"] != nil {
		t.Fatalf("expected no message, got %v", empty)
	}

	// A wait parked BEFORE the mail arrives is woken by it.
	got := make(chan map[string]any, 1)
	go func() {
		got <- runTool(t, h, "mailbox_wait_for_message", map[string]any{
			"instance": "acme", "after": "now", "from": "*@github.com", "extract": "code", "timeoutSeconds": 10,
		})
	}()
	time.Sleep(150 * time.Millisecond)
	injectMail(t, h, "acme", "signup", "&authenticated=1", signupMail)
	var res map[string]any
	select {
	case res = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not wake")
	}
	msg, _ := res["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("wait returned no message: %v", res)
	}
	if codes, _ := msg["codes"].([]any); len(codes) != 1 || codes[0] != "482913" {
		t.Fatalf("codes = %v", msg["codes"])
	}
	id := msg["id"].(string)

	links := runTool(t, h, "mailbox_get_message", map[string]any{"instance": "acme", "id": id, "extract": "links"})
	if l, _ := links["links"].([]any); len(l) != 1 || l[0] != "https://github.com/verify?t=1" {
		t.Fatalf("links = %v", links["links"])
	}

	list := runTool(t, h, "mailbox_list_messages", map[string]any{"instance": "acme", "to": "signup"})
	if msgs, _ := list["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("list = %v", list)
	}

	// The sender authenticated, so reply mode lets us answer — and only them.
	reply := runTool(t, h, "mailbox_reply", map[string]any{"instance": "acme", "id": id, "text": "thanks"})
	sent := reply["message"].(map[string]any)
	if sent["subject"] != "Re: Your code" || sent["direction"] != "out" || sent["local"] != "signup" {
		t.Fatalf("reply = %v", sent)
	}
	if text, isErr := toolText(t, h, "mailbox_send", map[string]any{
		"instance": "acme", "from": "signup", "to": []any{"stranger@example.com"}, "subject": "hi", "text": "x",
	}); !isErr || !strings.Contains(text, "NOT_A_REPLY") {
		t.Fatalf("send to a stranger: %v %s", isErr, text)
	}

	addr := runTool(t, h, "mailbox_create_address", map[string]any{"instance": "acme", "local": "support", "label": "help desk"})
	if addr["address"] != "acme.support@mail.localhost" {
		t.Fatalf("create_address = %v", addr)
	}
	addrs := runTool(t, h, "mailbox_list_addresses", map[string]any{"instance": "acme"})
	rows := addrs["addresses"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["local"] != "support" {
		t.Fatalf("addresses = %v", addrs)
	}
	if _, has := rows[0].(map[string]any)["address"]; has {
		t.Fatal("list_addresses returns the stored rows, as hosted")
	}

	// The signing key: revealed, and rotated only behind confirm.
	s1 := runTool(t, h, "mailbox_reveal_webhook_secret", map[string]any{"instance": "acme"})["secret"].(string)
	if !strings.HasPrefix(s1, "whsec_") {
		t.Fatalf("secret = %q", s1)
	}
	if _, isErr := toolText(t, h, "mailbox_rotate_webhook_secret", map[string]any{"instance": "acme"}); !isErr {
		t.Fatal("rotate ran without confirm")
	}
	s2 := runTool(t, h, "mailbox_rotate_webhook_secret", map[string]any{"instance": "acme", "confirm": true})["secret"].(string)
	if s2 == s1 || runTool(t, h, "mailbox_reveal_webhook_secret", map[string]any{"instance": "acme"})["secret"] != s2 {
		t.Fatal("rotation did not replace the secret")
	}

	// Routes are validated through the generic config tool.
	if _, isErr := toolText(t, h, "patch_instance_config", map[string]any{
		"service": "mailbox", "instance": "acme", "changes": map[string]any{"routes": []any{map[string]any{"do": []any{map[string]any{"webhook": "http://x.example"}}}}},
	}); !isErr {
		t.Fatal("an http webhook was accepted")
	}

	if _, isErr := toolText(t, h, "mailbox_delete_message", map[string]any{"instance": "acme", "id": id}); !isErr {
		t.Fatal("delete ran without confirm")
	}
	if out := runTool(t, h, "mailbox_delete_message", map[string]any{"instance": "acme", "id": id, "confirm": true}); out["deleted"] != float64(1) {
		t.Fatalf("delete = %v", out)
	}
}

func TestVerificationExtraction(t *testing.T) {
	if got := ExtractCodes("Your code", "Use 482913 to sign in. © 2026. Ref AB12CD."); !reflect.DeepEqual(got, []string{"482913", "AB12CD"}) {
		t.Errorf("codes = %v", got)
	}
	if got := ExtractCodes("", "order 12345678901 and part-1234 and 9999x"); len(got) != 0 {
		t.Errorf("codes inside longer tokens = %v", got)
	}
	got := ExtractLinks("go https://a.com/x?y=1. Or (https://c.com/z), ok", `<a href="https://b.com/v?a=1&amp;b=2">v</a>`)
	want := []string{"https://b.com/v?a=1&b=2", "https://a.com/x?y=1", "https://c.com/z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("links = %v", got)
	}
}
