//go:build conformance

package altengine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	altengine "github.com/altlimit/altengine/go"
)

func TestChannelTokensAndPublish(t *testing.T) {
	ch := client().Channel(uniq("sdk-conf"))

	t.Run("mints a token with ws_url and echoes channels", func(t *testing.T) {
		tok, err := ch.CreateToken(ctx(t), altengine.TokenRequest{
			Channels:   []string{"room:1", "room:2"},
			TTLSeconds: 600,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.Split(tok.Token, ".")) != 3 {
			t.Fatalf("token not a JWT: %q", tok.Token)
		}
		if len(tok.Channels) != 2 || tok.Channels[0] != "room:1" {
			t.Fatalf("channels=%v", tok.Channels)
		}
		if !strings.Contains(tok.WSURL, "/subscribe") {
			t.Fatalf("ws_url=%q", tok.WSURL)
		}
		if tok.ExpiresAt <= time.Now().Unix() {
			t.Fatalf("expires_at=%d", tok.ExpiresAt)
		}
	})

	t.Run("HTTP publish reports delivered count (0 with no subscribers)", func(t *testing.T) {
		delivered, err := ch.Publish(ctx(t), "room:empty", map[string]any{"hello": 1})
		if err != nil || delivered != 0 {
			t.Fatalf("delivered=%d err=%v", delivered, err)
		}
	})
}

func TestChannelWebSocketLifecycle(t *testing.T) {
	ch := client().Channel(uniq("sdk-conf"))

	newSocket := func(req altengine.TokenRequest, onMsg func(altengine.ChannelMessage), channels []string) *altengine.ChannelSocket {
		return altengine.NewChannelSocket(altengine.SocketOptions{
			GetToken: func(c context.Context) (*altengine.TokenResponse, error) {
				return ch.CreateToken(c, req)
			},
			Channels:  channels,
			OnMessage: onMsg,
		})
	}

	t.Run("subscribes and receives an HTTP-published message", func(t *testing.T) {
		got := make(chan altengine.ChannelMessage, 8)
		sock := newSocket(altengine.TokenRequest{Channels: []string{"room:live"}}, func(m altengine.ChannelMessage) { got <- m }, nil)
		defer sock.Close()
		if err := sock.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		if sock.State() != altengine.SocketOpen {
			t.Fatalf("state=%s", sock.State())
		}
		// Publish may race the subscribe registration; retry until delivered.
		for i := 0; i < 50; i++ {
			delivered, err := ch.Publish(ctx(t), "room:live", map[string]any{"n": i})
			if err != nil {
				t.Fatal(err)
			}
			if delivered > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		select {
		case msg := <-got:
			if msg.Channel != "room:live" || msg.TS <= 0 {
				t.Fatalf("msg=%+v", msg)
			}
			var data struct {
				N int `json:"n"`
			}
			if err := msg.DataAs(&data); err != nil || data.N < 0 {
				t.Fatalf("data=%+v err=%v", data, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for message")
		}
	})

	t.Run("publishes over the socket with a ws-capable token", func(t *testing.T) {
		got := make(chan altengine.ChannelMessage, 8)
		a := newSocket(altengine.TokenRequest{Channels: []string{"room:ws"}, Publish: altengine.PublishWS}, nil, nil)
		b := newSocket(altengine.TokenRequest{Channels: []string{"room:ws"}}, func(m altengine.ChannelMessage) { got <- m }, nil)
		defer a.Close()
		defer b.Close()
		if err := a.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		if err := b.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		// Retry until b's subscription is registered server-side.
		delivered := 0
		for i := 0; i < 50 && delivered < 2; i++ {
			var err error
			delivered, err = a.Publish(ctx(t), "room:ws", map[string]any{"via": "ws"})
			if err != nil {
				t.Fatal(err)
			}
			if delivered < 2 {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if delivered < 2 { // a + b are both subscribed
			t.Fatalf("delivered=%d", delivered)
		}
		select {
		case msg := <-got:
			var data struct {
				Via string `json:"via"`
			}
			if err := msg.DataAs(&data); err != nil || data.Via != "ws" {
				t.Fatalf("data=%+v err=%v", data, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for ws-published message")
		}
	})

	t.Run("subscribe and unsubscribe at runtime get acks", func(t *testing.T) {
		sock := newSocket(altengine.TokenRequest{Channels: []string{"room:a", "room:b"}}, nil, []string{"room:a"})
		defer sock.Close()
		if err := sock.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		acked, err := sock.Subscribe(ctx(t), []string{"room:b"})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range acked {
			if c == "room:b" {
				found = true
			}
		}
		if !found {
			t.Fatalf("acked=%v", acked)
		}
		if err := sock.Unsubscribe(ctx(t), []string{"room:a"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("subscriber-only socket cannot publish", func(t *testing.T) {
		sock := newSocket(altengine.TokenRequest{Channels: []string{"room:x"}}, nil, nil)
		defer sock.Close()
		if err := sock.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := sock.Publish(ctx(t), "room:x", map[string]any{"nope": true}); err == nil {
			t.Fatal("publish succeeded on a subscriber-only token")
		}
	})
}
