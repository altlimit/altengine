package altengine

import (
	"context"
	"encoding/json"
	"fmt"
)

// PublishMode is a token's publish capability: "" (none), "http", "ws", or
// "all". The wire also accepts/returns booleans (true ≡ "all"); unmarshaling
// maps them onto these values.
type PublishMode string

// Publish capability values.
const (
	PublishNone PublishMode = ""
	PublishHTTP PublishMode = "http"
	PublishWS   PublishMode = "ws"
	PublishAll  PublishMode = "all"
)

// UnmarshalJSON accepts both the boolean and string wire forms.
func (m *PublishMode) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case bool:
		if t {
			*m = PublishAll
		} else {
			*m = PublishNone
		}
	case string:
		*m = PublishMode(t)
	case nil:
		*m = PublishNone
	default:
		return fmt.Errorf("altengine: invalid publish mode %s", b)
	}
	return nil
}

// TokenRequest mints a subscriber token.
type TokenRequest struct {
	// Channels this token may subscribe to (≤100 names, each ≤200 bytes).
	Channels []string `json:"channels"`
	// TTLSeconds defaults to 3600, max 14400.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Publish grants publish capability; minting a publish-capable token
	// requires a write grant on the API key.
	Publish PublishMode `json:"publish,omitempty"`
	// PresenceID is a stable presence identity (≤128 bytes) bound into the
	// token server-side.
	PresenceID string `json:"presence_id,omitempty"`
}

// TokenResponse is a minted subscriber token.
type TokenResponse struct {
	// Token is the subscriber JWT — safe to hand to a browser.
	Token string `json:"token"`
	// ExpiresAt is unix seconds.
	ExpiresAt int64       `json:"expires_at"`
	Channels  []string    `json:"channels"`
	Publish   PublishMode `json:"publish"`
	// WSURL is a ready-made WebSocket URL (wss://…/subscribe?token=…).
	WSURL      string `json:"ws_url"`
	PresenceID string `json:"presence_id,omitempty"`
}

// ChannelMessage is a delivered message frame (same shape over WS and HTTP
// publish).
type ChannelMessage struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
	// TS is the server timestamp (unix ms).
	TS int64 `json:"ts"`
}

// DataAs unmarshals the message's data into v.
func (m *ChannelMessage) DataAs(v any) error { return json.Unmarshal(m.Data, v) }

// PresenceMember is one presence roster entry.
type PresenceMember struct {
	ID          string `json:"id"`
	Connections int    `json:"connections"`
}

// PresenceResponse is a channel's presence roster.
type PresenceResponse struct {
	Channel     string `json:"channel"`
	Occupancy   int    `json:"occupancy"`
	MemberCount int    `json:"member_count"`
	// Members holds at most 1000 members; see Truncated.
	Members   []PresenceMember `json:"members"`
	Truncated bool             `json:"truncated"`
}

// ChannelClient is the server-side client for one channel instance (API-key
// auth): mint subscriber tokens, publish over HTTP, and read presence. For a
// managed WebSocket subscriber, use ChannelSocket.
type ChannelClient struct {
	Instance string
	http     *transport
	base     string
}

// Channel binds a server-side client for a channel instance.
func (c *Client) Channel(instance string) *ChannelClient {
	return &ChannelClient{Instance: instance, http: c.http, base: "/v1/channel/" + seg(instance)}
}

// CreateToken mints a subscriber token (JWT) for the given channels.
func (c *ChannelClient) CreateToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	var out TokenResponse
	err := c.http.do(ctx, request{method: "POST", path: c.base + "/tokens", body: req}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Publish sends a message (framed ≤32 KiB) to a channel; returns the number
// of subscribers reached. NOT retried automatically (a retry double-delivers).
func (c *ChannelClient) Publish(ctx context.Context, channel string, data any) (int, error) {
	var out struct {
		Delivered int `json:"delivered"`
	}
	err := c.http.do(ctx, request{
		method:  "POST",
		path:    c.base + "/publish",
		body:    map[string]any{"channel": channel, "data": data},
		noRetry: true,
	}, &out)
	return out.Delivered, err
}

// Presence returns the presence roster for a channel (requires the instance's
// presence flag).
func (c *ChannelClient) Presence(ctx context.Context, channel string) (*PresenceResponse, error) {
	q := make(map[string][]string)
	q["channel"] = []string{channel}
	var out PresenceResponse
	err := c.http.do(ctx, request{method: "GET", path: c.base + "/presence", query: q}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
