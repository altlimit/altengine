package identity

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// The end-user identity token: a short-lived HS256 JWT signed with the auth instance's
// per-instance secret, carrying the end-user's identity + custom claims. This is the
// credential a browser presents to the OTHER data planes (datastore, channel) instead of
// an org API key.
//
// The token deliberately carries only identity (uid / identifier / email / claims) — NOT
// the grants or the row rules. Those are resolved fresh at the target from the issuing
// instance's `access` config, so editing access or rules takes effect without re-minting
// every outstanding token.

// IdentityClaims is the identity-token payload. `Profile` (user-supplied, read as
// `$auth.profile.X`, NEVER authoritative) and `Claims` (server/admin-set, read as
// `$auth.claims.X`, authoritative) are the two identity bags — the split is a security
// boundary: a user chooses their profile values, so an authorization rule must trust only
// claims.
type IdentityClaims struct {
	Iss        string         `json:"iss"` // issuing auth instance id
	Sub        string         `json:"sub"` // end-user uid
	Identifier string         `json:"identifier"`
	Email      string         `json:"email,omitempty"`
	Profile    map[string]any `json:"profile,omitempty"` // user-supplied signup fields — $auth.profile.X, NEVER authoritative
	Claims     map[string]any `json:"claims,omitempty"`  // server/admin-set — $auth.claims.X, authoritative
	Exp        int64          `json:"exp"`               // unix seconds
	// The credential epoch the token was minted in (omitted for epoch 0). A token from an
	// earlier epoch than the account's current one — disabled, deleted or reset since — is
	// refused wherever it is presented.
	Ce int64 `json:"ce,omitempty"`
	// The token type: "id" for an identity token. The per-instance secret signs other kinds
	// of token hosted (an MFA challenge, passkey ceremony state); each verifier accepts only
	// its own type.
	Typ string `json:"typ,omitempty"`
	// Carried by the other token kinds, never by an identity token; read only to refuse one.
	Purpose string `json:"purpose,omitempty"`
}

// untypedTokensExpireBy is the cutoff for identity tokens minted before `typ` existed: an
// untyped token is accepted only if it expires by this moment (2026-11-01T00:00:00Z), as
// hosted. After it, every identity token must carry typ "id".
var untypedTokensExpireBy = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()

// SignIdentity mints an identity token, typed "id".
func SignIdentity(c IdentityClaims, secret string) string {
	c.Typ = "id"
	c.Purpose = ""
	payload, _ := json.Marshal(c)
	return common.SignJWTPayload(payload, secret)
}

// PeekIssuer reads the UNVERIFIED `iss` so the right instance secret can be loaded before
// the real signature check. Never trust it for authorization.
func PeekIssuer(token string) string {
	raw := common.PeekJWTPayload(token)
	if raw == nil {
		return ""
	}
	var c IdentityClaims
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.Iss
}

// VerifyIdentity checks the signature, the expiry, and the claim shape. Returns nil on
// any failure — never trust the result until it returns non-nil.
func VerifyIdentity(token, secret string) *IdentityClaims {
	raw := common.VerifyJWTPayload(token, secret)
	if raw == nil {
		return nil
	}
	var c IdentityClaims
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	// Only an identity token: typ "id" and no purpose, or — minted before typ existed — no
	// purpose and an expiry no later than the cutoff.
	if c.Purpose != "" {
		return nil
	}
	if c.Typ != "id" && (c.Typ != "" || c.Exp == 0 || c.Exp > untypedTokensExpireBy) {
		return nil
	}
	if c.Iss == "" || c.Sub == "" || c.Identifier == "" {
		return nil
	}
	if c.Exp != 0 && c.Exp*1000 <= time.Now().UnixMilli() {
		return nil
	}
	return &c
}

// scalarString renders a scalar claim value for channel-template interpolation.
// Objects, arrays, and nulls are not scalars and are rejected by the caller.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}
