package common

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// HS256 JWT primitives shared by every token the emulator mints: channel subscriber
// tokens and end-user identity tokens. Kept here (rather than in one service package)
// so the identity service and the channel service can both use them without importing
// each other.

// B64 encodes bytes as unpadded base64url (the JWT segment encoding).
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignJWTPayload wraps an already-marshalled claims payload in an HS256 JWT.
func SignJWTPayload(payload []byte, secret string) string {
	header := B64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	signing := header + "." + B64(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + B64(mac.Sum(nil))
}

// PeekJWTPayload decodes a token's claims segment WITHOUT verifying the signature.
// Only ever use the result to find which secret to verify with — never to authorize.
func PeekJWTPayload(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	return raw
}

// VerifyJWTPayload checks the HS256 signature and returns the claims segment, or nil.
// Expiry is NOT checked here — the caller owns its own `exp` semantics.
func VerifyJWTPayload(token, secret string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	if !hmac.Equal([]byte(B64(mac.Sum(nil))), []byte(parts[2])) {
		return nil
	}
	return PeekJWTPayload(token)
}

// LooksLikeJWT reports whether a bearer credential is a JWT (three dot-separated
// segments) rather than an API key (`prefix.secret`, two segments).
func LooksLikeJWT(token string) bool { return len(strings.Split(token, ".")) == 3 }
