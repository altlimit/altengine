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
	"github.com/gorilla/websocket"
)

func newTestServer(t *testing.T) (*httptest.Server, *control.Registry) {
	t.Helper()
	reg, _ := control.New("")
	a := auth.NewStore(true)
	h := NewHandler(reg, a, NewHub())
	mux := http.NewServeMux()
	h.Register(mux)
	return httptest.NewServer(mux), reg
}

func post(t *testing.T, url, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer devkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s -> %d: %v", url, resp.StatusCode, m)
	}
	return m
}

func TestPublishSubscribe(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	base := srv.URL + "/v1/channel/chat"

	// Mint a subscriber token with publish capability.
	tok := post(t, base+"/tokens", `{"channels":["room1","room2"],"publish":true,"presence_id":"alice"}`)
	wsURL := strings.Replace(tok["ws_url"].(string), "http", "ws", 1)
	// httptest gives an http URL; convert scheme to ws.
	wsURL = strings.Replace(tok["ws_url"].(string), "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	// First frame should be the subscribed ack.
	var ack map[string]any
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := ws.ReadJSON(&ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack["type"] != "subscribed" {
		t.Fatalf("expected subscribed ack, got %v", ack)
	}

	// Give the connection a moment to register in the hub.
	time.Sleep(50 * time.Millisecond)

	// Publish over HTTP.
	res := post(t, base+"/publish", `{"channel":"room1","data":{"hello":"world"}}`)
	if res["delivered"].(float64) != 1 {
		t.Fatalf("expected delivered=1, got %v", res["delivered"])
	}

	// Receive the delivered frame.
	var frame map[string]any
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := ws.ReadJSON(&frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame["channel"] != "room1" {
		t.Fatalf("wrong channel: %v", frame)
	}
	data := frame["data"].(map[string]any)
	if data["hello"] != "world" {
		t.Fatalf("wrong data: %v", frame)
	}
	if _, ok := frame["ts"]; !ok {
		t.Fatalf("missing ts: %v", frame)
	}

	// Publish over the websocket itself.
	ws.WriteJSON(map[string]any{"type": "publish", "channel": "room2", "data": 42})
	// Expect an ack and (since we're the only sub) the delivered frame.
	sawPublished := false
	for i := 0; i < 2; i++ {
		var f map[string]any
		ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := ws.ReadJSON(&f); err != nil {
			t.Fatalf("read ws-publish resp: %v", err)
		}
		if f["type"] == "published" {
			sawPublished = true
		}
	}
	if !sawPublished {
		t.Fatalf("did not see published ack")
	}
}

func TestPresence(t *testing.T) {
	srv, reg := newTestServer(t)
	defer srv.Close()
	base := srv.URL + "/v1/channel/chat2"

	// Enable presence on the instance.
	inst := reg.GetOrCreate("channel", "chat2")
	reg.SetConfig(inst, map[string]any{"presence": true})

	tok := post(t, base+"/tokens", `{"channels":["lobby"],"presence_id":"bob"}`)
	wsURL := strings.Replace(strings.Replace(tok["ws_url"].(string), "https://", "wss://", 1), "http://", "ws://", 1)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	var ack map[string]any
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	ws.ReadJSON(&ack)
	time.Sleep(50 * time.Millisecond)

	// GET presence.
	req, _ := http.NewRequest("GET", base+"/presence?channel=lobby", nil)
	req.Header.Set("Authorization", "Bearer devkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 200 {
		t.Fatalf("presence -> %d: %v", resp.StatusCode, m)
	}
	if m["occupancy"].(float64) != 1 || m["member_count"].(float64) != 1 {
		t.Fatalf("unexpected presence: %v", m)
	}
	members := m["members"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["id"] != "bob" {
		t.Fatalf("unexpected members: %v", members)
	}
}
