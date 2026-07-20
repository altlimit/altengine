package channel

import (
	"encoding/json"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Claims is the subscriber-token payload.
type Claims struct {
	Iss      string   `json:"iss"`
	Channels []string `json:"channels"`
	Exp      int64    `json:"exp"` // unix seconds
	Pub      string   `json:"pub,omitempty"`
	Pid      string   `json:"pid,omitempty"`
}

// SignJWT produces an HS256 token.
func SignJWT(c Claims, secret string) string {
	payload, _ := json.Marshal(c)
	return common.SignJWTPayload(payload, secret)
}

// PeekClaims decodes the payload WITHOUT verifying (to find the instance).
func PeekClaims(token string) *Claims {
	raw := common.PeekJWTPayload(token)
	if raw == nil {
		return nil
	}
	var c Claims
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return &c
}

// VerifyJWT checks the signature and expiry, returning the claims or nil.
func VerifyJWT(token, secret string) *Claims {
	raw := common.VerifyJWTPayload(token, secret)
	if raw == nil {
		return nil
	}
	var c Claims
	if json.Unmarshal(raw, &c) != nil || len(c.Channels) == 0 {
		return nil
	}
	if c.Exp != 0 && c.Exp*1000 <= time.Now().UnixMilli() {
		return nil
	}
	return &c
}

// LooksLikeJWT reports whether a bearer token is a JWT (3 dot-parts) vs an API key.
func LooksLikeJWT(token string) bool { return common.LooksLikeJWT(token) }

// CanPublish reports whether claims permit publishing over the given transport.
func CanPublish(c *Claims, transport string) bool {
	return c.Pub == "all" || c.Pub == transport
}
