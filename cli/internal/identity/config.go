package identity

import "strings"

// Per-auth-instance configuration, read tolerantly out of the registry's generic
// `map[string]any` config blob (the same shape the admin console PUTs). Config keys are
// camelCase; wire fields on the HTTP surface stay snake_case.
//
// Two nested blobs carry the interesting parts:
//
//	signup — the configurable signup form: which fields to collect and which one is the
//	         unique login handle (`identityField`). The identity value becomes the user's
//	         `identifier`; every other collected field becomes a custom claim.
//	access — what an end-user token minted here may reach: keyed "<service>:<instance>",
//	         each entry carrying a `level` ceiling plus optional per-collection row
//	         `rules` (datastore) or `channels` templates (channel).
//
// Reads never fail: a missing or malformed value falls back to its default, mirroring the
// tolerant config parsing the search instance config uses.

// TTL bounds (seconds), matching the hosted service.
const (
	minAccessTTL     = 300
	maxAccessTTL     = 24 * 60 * 60
	defaultAccessTTL = 60 * 60
	minRefreshTTL    = 60 * 60
	maxRefreshTTL    = 90 * 24 * 60 * 60
	defaultRefresh   = 30 * 24 * 60 * 60
	minCodeTTL       = 300
	maxCodeTTL       = 1800
	defaultCodeTTL   = 600
)

// SignupField is one collected field on the signup form.
type SignupField struct {
	Key      string `json:"key"`
	Type     string `json:"type"` // email | string | number | boolean
	Required bool   `json:"required"`
	Label    string `json:"label,omitempty"`
}

// SignupConfig is the signup form: the fields to collect plus which one is the handle.
type SignupConfig struct {
	IdentityField string        `json:"identityField"`
	Fields        []SignupField `json:"fields"`
}

// Config is a fully-defaulted auth instance configuration.
type Config struct {
	AllowSignup         bool
	AccessTokenTTL      int64
	RefreshTokenTTL     int64
	PasswordlessEnabled bool
	PasswordlessCodeTTL int64
	PasskeysEnabled     bool
	TotpEnabled         bool
	CaptchaEnabled      bool
	CaptchaSiteKey      string
	Signup              SignupConfig
	Access              AccessConfig
}

// DefaultSignup is the classic email + password form.
func DefaultSignup() SignupConfig {
	return SignupConfig{
		IdentityField: "email",
		Fields:        []SignupField{{Key: "email", Type: "email", Required: true}},
	}
}

// lookup reads a config key from the flat blob, also accepting it nested under a
// `settings` object so a config copied from the hosted console parses unchanged.
func lookup(cfg map[string]any, key string) any {
	if s, ok := cfg["settings"].(map[string]any); ok {
		if v, ok := s[key]; ok {
			return v
		}
	}
	return cfg[key]
}

func boolOr(cfg map[string]any, key string, def bool) bool {
	if v, ok := lookup(cfg, key).(bool); ok {
		return v
	}
	return def
}

func stringOr(cfg map[string]any, key, def string) string {
	if v, ok := lookup(cfg, key).(string); ok {
		return v
	}
	return def
}

// ttl reads a numeric seconds value and clamps it into [min,max].
func ttl(cfg map[string]any, key string, min, max, def int64) int64 {
	n := def
	switch v := lookup(cfg, key).(type) {
	case float64:
		n = int64(v)
	case int:
		n = int64(v)
	case int64:
		n = v
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

// ParseConfig assembles a fully-defaulted Config from an instance's config blob.
func ParseConfig(cfg map[string]any) Config {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return Config{
		AllowSignup:         boolOr(cfg, "allowSignup", true),
		AccessTokenTTL:      ttl(cfg, "accessTokenTtl", minAccessTTL, maxAccessTTL, defaultAccessTTL),
		RefreshTokenTTL:     ttl(cfg, "refreshTokenTtl", minRefreshTTL, maxRefreshTTL, defaultRefresh),
		PasswordlessEnabled: boolOr(cfg, "passwordlessEnabled", false),
		PasswordlessCodeTTL: ttl(cfg, "passwordlessCodeTtl", minCodeTTL, maxCodeTTL, defaultCodeTTL),
		PasskeysEnabled:     boolOr(cfg, "passkeysEnabled", false),
		TotpEnabled:         boolOr(cfg, "totpEnabled", false),
		CaptchaEnabled:      boolOr(cfg, "captchaEnabled", false),
		CaptchaSiteKey:      stringOr(cfg, "captchaSiteKey", ""),
		Signup:              parseSignup(lookup(cfg, "signup")),
		Access:              parseAccess(cfg["access"]),
	}
}

// parseSignup tolerantly reads the signup form; anything invalid falls back to the
// email + password default (a broken form must never lock signup into an unusable shape).
func parseSignup(v any) SignupConfig {
	m, ok := v.(map[string]any)
	if !ok {
		return DefaultSignup()
	}
	rawFields, _ := m["fields"].([]any)
	var fields []SignupField
	for _, rf := range rawFields {
		fm, ok := rf.(map[string]any)
		if !ok {
			continue
		}
		key, _ := fm["key"].(string)
		if key == "" {
			continue
		}
		typ, _ := fm["type"].(string)
		switch typ {
		case "email", "string", "number", "boolean":
		default:
			typ = "string"
		}
		req, _ := fm["required"].(bool)
		label, _ := fm["label"].(string)
		fields = append(fields, SignupField{Key: key, Type: typ, Required: req, Label: label})
	}
	idf, _ := m["identityField"].(string)
	if len(fields) == 0 || idf == "" {
		return DefaultSignup()
	}
	// The identity field must name one of the collected fields and be textual — it is the
	// unique login handle, so it is always present and always a string.
	for _, f := range fields {
		if f.Key == idf && (f.Type == "email" || f.Type == "string") {
			return SignupConfig{IdentityField: idf, Fields: fields}
		}
	}
	return DefaultSignup()
}

// IdentityType is the declared type of the instance's identity field.
func (s SignupConfig) IdentityType() string {
	for _, f := range s.Fields {
		if f.Key == s.IdentityField && f.Type == "string" {
			return "string"
		}
	}
	return "email"
}

// DeriveEmail returns the address to put on an identity token: the identifier itself when
// the identity field is email-typed, else a collected `email` field, else "" (a pure
// username instance simply has no email on the token). `email` is a signup-collected field,
// so it lives in the user-supplied profile bag.
func (s SignupConfig) DeriveEmail(identifier string, profile map[string]any) string {
	if s.IdentityType() == "email" {
		return identifier
	}
	if e, ok := profile["email"].(string); ok {
		return strings.TrimSpace(e)
	}
	return ""
}
