package altengine

import (
	"reflect"
	"strings"
	"testing"
)

// The token goes in a subprotocol, not the URL, whenever there is an endpoint to dial
// without it; a response with only ws_url comes from an older server and is opened as is.
func TestSocketTarget(t *testing.T) {
	const tok = "a.b.c"
	want := []string{"altengine.v1", "altengine.token.a.b.c"}

	cases := []struct {
		name      string
		opts      SocketOptions
		resp      TokenResponse
		url       string
		protocols []string
	}{
		{
			name: "ws_endpoint with protocols",
			resp: TokenResponse{Token: tok, WSEndpoint: "wss://x.test/v1/channel/i/subscribe", Protocols: want, WSURL: "wss://x.test/v1/channel/i/subscribe?token=a.b.c"},
			url:  "wss://x.test/v1/channel/i/subscribe", protocols: want,
		},
		{
			name: "ws_endpoint without protocols",
			resp: TokenResponse{Token: tok, WSEndpoint: "wss://x.test/v1/channel/i/subscribe"},
			url:  "wss://x.test/v1/channel/i/subscribe", protocols: want,
		},
		{
			name: "older server: ws_url only",
			resp: TokenResponse{Token: tok, WSURL: "wss://x.test/v1/channel/i/subscribe?token=a.b.c"},
			url:  "wss://x.test/v1/channel/i/subscribe?token=a.b.c",
		},
		{
			name: "explicit URL",
			opts: SocketOptions{URL: "wss://x.test/v1/channel/i/subscribe", Channels: []string{"r1"}},
			resp: TokenResponse{Token: tok},
			url:  "wss://x.test/v1/channel/i/subscribe?channels=r1", protocols: want,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewChannelSocket(c.opts)
			u, p, err := s.target(&c.resp)
			if err != nil {
				t.Fatal(err)
			}
			if u != c.url || !reflect.DeepEqual(p, c.protocols) {
				t.Fatalf("got %q %v, want %q %v", u, p, c.url, c.protocols)
			}
			if c.protocols != nil && strings.Contains(u, tok) {
				t.Fatalf("token in the URL: %q", u)
			}
		})
	}

	if _, _, err := NewChannelSocket(SocketOptions{}).target(&TokenResponse{Token: tok}); err == nil {
		t.Fatal("want an error with no URL at all")
	}
}
