package channel

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// An inbound frame far past the message cap closes the socket (1009) instead of being buffered
// whole — there was no read limit, so one client frame could be any size.
func TestOversizedClientFrameClosesTheSocket(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	tok := post(t, srv.URL+"/v1/channel/chat/tokens", `{"channels":["room1"],"publish":true}`)
	wsURL := strings.Replace(tok["ws_url"].(string), "http://", "ws://", 1)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	var ack map[string]any
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := ws.ReadJSON(&ack); err != nil {
		t.Fatalf("ack: %v", err)
	}

	huge := `{"type":"publish","channel":"room1","data":"` + strings.Repeat("x", maxClientFrameBytes+1) + `"}`
	if err := ws.WriteMessage(websocket.TextMessage, []byte(huge)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue
		}
		if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
			t.Fatalf("socket ended with %v, want close 1009", err)
		}
		return
	}
}
