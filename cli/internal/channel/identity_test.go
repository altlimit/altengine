package channel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/identity"
	"github.com/gorilla/websocket"
)

// End-user identity tokens minting channel tokens: the mint is confined to the auth
// instance's templated channel patterns, is always subscribe-only, and forces the presence
// identity to the token's uid.
func newIdentityEnv(t *testing.T) (*httptest.Server, *control.Instance) {
	t.Helper()
	srv, _, authInst := newIdentityEnvReg(t)
	return srv, authInst
}

func newIdentityEnvReg(t *testing.T) (*httptest.Server, *control.Registry, *control.Instance) {
	t.Helper()
	reg, _ := control.New("")
	idSvc := identity.NewService(reg, identity.NewManager(""), true)
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewHub()).WithIdentity(idSvc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	authInst := reg.GetOrCreate("auth", "appauth")
	reg.SetConfig(authInst, map[string]any{"access": map[string]any{
		"channel:chat": map[string]any{
			"level":    "read",
			"channels": []any{"posts.*", "dm.$auth.uid"},
		},
	}})
	return srv, reg, authInst
}

func identityToken(inst *control.Instance, uid string) string {
	return identity.SignIdentity(identity.IdentityClaims{
		Iss: inst.ID, Sub: uid, Identifier: uid + "@example.com", Email: uid + "@example.com",
		Exp: time.Now().Unix() + 3600,
	}, inst.Secret)
}

func mint(t *testing.T, srv *httptest.Server, body, bearer string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/channel/chat/tokens", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestIdentityTokenMintIsPatternScoped(t *testing.T) {
	srv, authInst := newIdentityEnv(t)
	alice := identityToken(authInst, "alice")

	// A prefix-wildcard pattern and an interpolated one both resolve.
	status, out := mint(t, srv, `{"channels":["posts.general","dm.alice"]}`, alice)
	if status != 200 {
		t.Fatalf("mint -> %d: %v", status, out)
	}
	if out["publish"] != false {
		t.Fatalf("an identity mint must be subscribe-only, got publish=%v", out["publish"])
	}
	if out["presence_id"] != "alice" {
		t.Fatalf("presence identity must be forced to the uid, got %v", out["presence_id"])
	}

	// Another user's direct-message channel is outside alice's interpolated pattern.
	if status, out := mint(t, srv, `{"channels":["dm.bob"]}`, alice); status != 403 {
		t.Fatalf("dm.bob for alice -> %d: %v", status, out)
	}
	// So is a channel matching no pattern at all.
	if status, out := mint(t, srv, `{"channels":["admin"]}`, alice); status != 403 {
		t.Fatalf("unlisted channel -> %d: %v", status, out)
	}
	// Asking for publish doesn't grant it — the mint downgrades to subscribe-only.
	status, out = mint(t, srv, `{"channels":["posts.general"],"publish":true,"presence_id":"spoofed"}`, alice)
	if status != 200 {
		t.Fatalf("mint -> %d: %v", status, out)
	}
	if out["publish"] != false || out["presence_id"] != "alice" {
		t.Fatalf("identity mint escalated: %v", out)
	}
}

func TestIdentityTokenWithoutChannelAccessIsDenied(t *testing.T) {
	srv, reg, authInst := newIdentityEnvReg(t)
	// Strip the channel entry: no access means no mint, whatever is requested.
	reg.SetConfig(authInst, map[string]any{"access": map[string]any{}})
	if status, out := mint(t, srv, `{"channels":["posts.general"]}`, identityToken(authInst, "alice")); status != 403 {
		t.Fatalf("no channel access -> %d: %v", status, out)
	}
}

func TestIdentityTokenMayNotPublishOverHTTP(t *testing.T) {
	srv, authInst := newIdentityEnv(t)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/channel/chat/publish",
		bytes.NewBufferString(`{"channel":"posts.general","data":{"x":1}}`))
	req.Header.Set("Authorization", "Bearer "+identityToken(authInst, "alice"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("identity publish -> %d (want 403)", resp.StatusCode)
	}
}

// An end user's channel token lives no longer than the identity token it was minted from,
// names the account (aui/sub/ce), and is refused at subscribe once the account is revoked.
func TestEndUserChannelTokenEndsWithTheAccount(t *testing.T) {
	reg, _ := control.New("")
	mgr := identity.NewManager("")
	idSvc := identity.NewService(reg, mgr, true)
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewHub()).WithIdentity(idSvc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	authInst := reg.GetOrCreate("auth", "appauth")
	reg.SetConfig(authInst, map[string]any{"access": map[string]any{
		"channel:chat": map[string]any{"level": "read", "channels": []any{"posts.*"}},
	}})
	store, err := mgr.Open(authInst.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUser("carol@example.com", "x", nil, time.Now().UnixMilli(), false)
	if err != nil {
		t.Fatal(err)
	}
	idExp := time.Now().Unix() + 600
	idTok := identity.SignIdentity(identity.IdentityClaims{
		Iss: authInst.ID, Sub: u.UID, Identifier: "carol@example.com", Exp: idExp,
	}, authInst.Secret)

	status, out := mint(t, srv, `{"channels":["posts.general"],"ttl_seconds":3600}`, idTok)
	if status != 200 {
		t.Fatalf("mint -> %d %v", status, out)
	}
	if out["expires_at"] != float64(idExp) {
		t.Fatalf("expires_at = %v, want the identity token's %d", out["expires_at"], idExp)
	}
	tok := out["token"].(string)
	c := PeekClaims(tok)
	if c.Exp != idExp || c.Aui != authInst.ID || c.Sub != u.UID || c.Ce == nil || *c.Ce != 0 {
		t.Fatalf("claims = %+v", c)
	}

	subscribe := func() int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/channel/chat/subscribe?token="+tok, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := subscribe(); s == 401 {
		t.Fatalf("a live account's token was refused")
	}
	if _, err := store.SetDisabled(u.UID, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if s := subscribe(); s != 401 {
		t.Fatalf("subscribe after disable -> %d, want 401", s)
	}
}

// One end user holds at most 20 sockets on a channel; org-key sockets are not capped by it.
func TestPerEndUserSocketCap(t *testing.T) {
	h := NewHub()
	newConn := func(uid string) *conn { return &conn{uid: uid, subscribed: map[string]bool{}} }
	for i := 0; i < maxConnectionsPerIdentity; i++ {
		c := newConn("u1")
		h.join("i", "ch", c)
		if !c.subscribed["ch"] {
			t.Fatalf("socket %d was refused under the cap", i+1)
		}
	}
	over := newConn("u1")
	h.join("i", "ch", over)
	if over.subscribed["ch"] {
		t.Fatal("a 21st socket for one end user was admitted")
	}
	other := newConn("u2")
	h.join("i", "ch", other)
	elsewhere := newConn("u1")
	h.join("i", "other", elsewhere)
	if !other.subscribed["ch"] || !elsewhere.subscribed["other"] {
		t.Fatal("the cap leaked to another user or another channel")
	}
	for i := 0; i < maxConnectionsPerIdentity+5; i++ {
		c := newConn("")
		h.join("i", "ch", c)
		if !c.subscribed["ch"] {
			t.Fatal("an org-key socket was capped")
		}
	}
}

// A channel asked for and not admitted is named in the ack's `refused` — at open (a cap) and
// on a subscribe frame (a cap, or not in the token) — instead of vanishing without a word.
func TestSubscribedAckNamesRefused(t *testing.T) {
	srv, authInst := newIdentityEnv(t)
	alice := identityToken(authInst, "alice")
	status, out := mint(t, srv, `{"channels":["posts.general","posts.other"]}`, alice)
	if status != 200 {
		t.Fatalf("mint -> %d: %v", status, out)
	}
	wsURL := strings.Replace(out["ws_url"].(string), "http://", "ws://", 1)
	type ack struct {
		Type     string   `json:"type"`
		Channels []string `json:"channels"`
		Refused  []string `json:"refused"`
	}
	read := func(ws *websocket.Conn) ack {
		t.Helper()
		var raw map[string]any
		ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := ws.ReadJSON(&raw); err != nil {
			t.Fatalf("read: %v", err)
		}
		b, _ := json.Marshal(raw)
		var a ack
		_ = json.Unmarshal(b, &a)
		if a.Type != "subscribed" {
			t.Fatalf("frame = %s", b)
		}
		if _, has := raw["refused"]; has && len(a.Refused) == 0 {
			t.Fatalf("an empty refused list was sent: %s", b)
		}
		return a
	}
	dial := func(query string) *websocket.Conn {
		t.Helper()
		ws, _, err := websocket.DefaultDialer.Dial(wsURL+query, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { ws.Close() })
		return ws
	}

	// Fill alice's per-channel socket cap on posts.general.
	for i := 0; i < maxConnectionsPerIdentity; i++ {
		if a := read(dial("&channel=posts.general")); len(a.Refused) != 0 || len(a.Channels) != 1 {
			t.Fatalf("socket %d under the cap: %+v", i+1, a)
		}
	}

	// At open: the capped channel is refused, the other admitted.
	ws := dial("")
	a := read(ws)
	if len(a.Channels) != 1 || a.Channels[0] != "posts.other" || len(a.Refused) != 1 || a.Refused[0] != "posts.general" {
		t.Fatalf("open ack = %+v", a)
	}

	// On subscribe: a capped channel and one the token does not carry are refused; the
	// already-subscribed one is not.
	ws.WriteJSON(map[string]any{"type": "subscribe", "channels": []string{"posts.general", "posts.secret", "posts.other", "posts.secret"}})
	a = read(ws)
	if len(a.Channels) != 1 || len(a.Refused) != 2 || a.Refused[0] != "posts.general" || a.Refused[1] != "posts.secret" {
		t.Fatalf("subscribe ack = %+v", a)
	}

	// An unsubscribe never carries refused.
	ws.WriteJSON(map[string]any{"type": "unsubscribe", "channels": []string{"posts.other"}})
	if a = read(ws); len(a.Channels) != 0 || len(a.Refused) != 0 {
		t.Fatalf("unsubscribe ack = %+v", a)
	}
}
