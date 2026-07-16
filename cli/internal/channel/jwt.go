package channel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Claims is the subscriber-token payload, matching src/channel/jwt.ts.
type Claims struct {
	Iss      string   `json:"iss"`
	Channels []string `json:"channels"`
	Exp      int64    `json:"exp"` // unix seconds
	Pub      string   `json:"pub,omitempty"`
	Pid      string   `json:"pid,omitempty"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignJWT produces an HS256 token.
func SignJWT(c Claims, secret string) string {
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(c)
	signing := header + "." + b64(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

// PeekClaims decodes the payload WITHOUT verifying (to find the instance).
func PeekClaims(token string) *Claims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
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
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	if !hmac.Equal([]byte(b64(mac.Sum(nil))), []byte(parts[2])) {
		return nil
	}
	c := PeekClaims(token)
	if c == nil || len(c.Channels) == 0 {
		return nil
	}
	if c.Exp != 0 && c.Exp*1000 <= time.Now().UnixMilli() {
		return nil
	}
	return c
}

// LooksLikeJWT reports whether a bearer token is a JWT (3 dot-parts) vs an API key.
func LooksLikeJWT(token string) bool { return len(strings.Split(token, ".")) == 3 }

// CanPublish reports whether claims permit publishing over the given transport.
func CanPublish(c *Claims, transport string) bool {
	return c.Pub == "all" || c.Pub == transport
}
