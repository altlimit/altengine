package identity

import (
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// ValidateConfig is the auth instance's config check on a write: the hosted rules a local config
// most often trips on. Settings may sit at the top level or under `settings`, as ParseConfig reads
// them.
func ValidateConfig(cfg map[string]any) error {
	settings := cfg
	if s, ok := cfg["settings"].(map[string]any); ok {
		settings = s
	}
	if err := common.CheckRegion(settings); err != nil {
		return err
	}
	key, _ := lookup(cfg, "captchaSiteKey").(string)
	key = strings.TrimSpace(key)
	if len(key) > 200 {
		return common.BadRequest("captchaSiteKey is too long")
	}
	if on, _ := lookup(cfg, "captchaEnabled").(bool); on && key == "" {
		return common.BadRequest("captchaSiteKey is required when captcha is enabled")
	}
	if err := ValidateAccessLevels(cfg["access"]); err != nil {
		return err
	}
	parsed := ParseConfig(cfg)
	if parsed.RequireEmailVerification && !signupCanEmail(parsed.Signup) {
		return common.BadRequest("requireEmailVerification needs an email address to send to: make the identity field an email, or collect an 'email' field in the signup form")
	}
	return nil
}

// signupCanEmail reports whether this signup form can ever produce an address to send a code to.
func signupCanEmail(s SignupConfig) bool {
	for _, f := range s.Fields {
		if (f.Key == s.IdentityField && f.Type == "email") || f.Key == "email" {
			return true
		}
	}
	return false
}
