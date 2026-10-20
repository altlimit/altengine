package channel

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// The subscribe upgrade takes its token from Sec-WebSocket-Protocol or Authorization, as on
// the hosted service; ?token= still works and is answered with the deprecation headers.

func mintConnect(t *testing.T, base string) (token, endpoint string, protocols []string) {
	t.Helper()
	tok := post(t, base+"/tokens", `{"channels":["room1"]}`)
	token = tok["token"].(string)
	endpoint = tok["ws_endpoint"].(string)
	for _, p := range tok["protocols"].([]any) {
		protocols = append(protocols, p.(string))
	}
	if !strings.HasSuffix(endpoint, "/v1/channel/chat/subscribe") || strings.Contains(endpoint, token) {
		t.Fatalf("ws_endpoint = %q", endpoint)
	}
	if want := endpoint + "?token=" + url.QueryEscape(token); tok["ws_url"] != want {
		t.Fatalf("ws_url = %v, want %v", tok["ws_url"], want)
	}
	if len(protocols) != 2 || protocols[0] != "altengine.v1" || protocols[1] != "altengine.token."+token {
		t.Fatalf("protocols = %v", protocols)
	}
	return
}

func dial(t *testing.T, u string, protocols []string, h http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := *websocket.DefaultDialer
	d.Subprotocols = protocols
	ws, resp, err := d.Dial(u, h)
	if ws != nil {
		t.Cleanup(func() { ws.Close() })
	}
	return ws, resp, err
}

func noDeprecation(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.Header.Get("Deprecation") != "" || resp.Header.Get("Sunset") != "" {
		t.Fatalf("unexpected deprecation headers: %v", resp.Header)
	}
}

func TestSubscribeTokenTransport(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	base := srv.URL + "/v1/channel/chat"
	token, endpoint, protocols := mintConnect(t, base)
	endpoint = strings.Replace(endpoint, "http://", "ws://", 1)

	t.Run("subprotocol, echoing only altengine.v1", func(t *testing.T) {
		ws, resp, err := dial(t, endpoint, protocols, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if ws.Subprotocol() != "altengine.v1" {
			t.Fatalf("subprotocol = %q", ws.Subprotocol())
		}
		for _, vs := range resp.Header {
			for _, v := range vs {
				if strings.Contains(v, token) {
					t.Fatalf("token echoed in a response header: %v", resp.Header)
				}
			}
		}
		noDeprecation(t, resp)
	})

	t.Run("Authorization header", func(t *testing.T) {
		_, resp, err := dial(t, endpoint, nil, http.Header{"Authorization": {"Bearer " + token}})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		noDeprecation(t, resp)
	})

	t.Run("older bearer subprotocol", func(t *testing.T) {
		ws, _, err := dial(t, endpoint, []string{"bearer", token}, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if ws.Subprotocol() != "bearer" {
			t.Fatalf("subprotocol = %q", ws.Subprotocol())
		}
	})

	t.Run("?token= works and is deprecated", func(t *testing.T) {
		_, resp, err := dial(t, endpoint+"?token="+url.QueryEscape(token), nil, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if resp.Header.Get("Deprecation") != "true" ||
			resp.Header.Get("Sunset") != "Sun, 31 Jan 2027 00:00:00 GMT" ||
			resp.Header.Get("Link") != `<https://altengine.net/docs/channel#token-auth>; rel="deprecation"` {
			t.Fatalf("deprecation headers = %v", resp.Header)
		}
	})

	t.Run("protocol and header win over ?token=", func(t *testing.T) {
		_, resp, err := dial(t, endpoint+"?token=junk.junk.junk", protocols, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		noDeprecation(t, resp)
		_, resp, err = dial(t, endpoint+"?token="+url.QueryEscape(token), []string{"altengine.v1", "altengine.token.bad.bad.bad"}, nil)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("a bad protocol token beside a good query should be 401, got err=%v resp=%v", err, resp)
		}
		_, resp, err = dial(t, endpoint+"?token=junk.junk.junk", nil, http.Header{"Authorization": {"Bearer " + token}})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		noDeprecation(t, resp)
	})

	t.Run("token subprotocol without altengine.v1 is refused", func(t *testing.T) {
		_, resp, err := dial(t, endpoint, []string{"altengine.token." + token}, nil)
		if err == nil || resp == nil || resp.StatusCode != 400 {
			t.Fatalf("want 400, got err=%v resp=%v", err, resp)
		}
	})

	t.Run("no token is 401", func(t *testing.T) {
		_, resp, err := dial(t, endpoint, []string{"altengine.v1"}, nil)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("want 401, got err=%v resp=%v", err, resp)
		}
	})
}
