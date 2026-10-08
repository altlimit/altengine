package main

import (
	"strings"
	"testing"
)

// The CLI read ALTENGINE_KEY and the SDKs ALTENGINE_API_KEY, so one shell setup worked for one
// and not the other. Both are read now, the documented one first, and a flag beats both.
func TestHostedCredsReadsBothKeyVariables(t *testing.T) {
	t.Setenv("ALTENGINE_URL", "https://api.example.com")
	t.Setenv(keyEnv, "")
	t.Setenv(legacyKeyEnv, "old")
	if _, key, err := hostedCreds("", ""); err != nil || key != "old" {
		t.Fatalf("legacy only: %q %v", key, err)
	}
	t.Setenv(keyEnv, "new")
	if _, key, _ := hostedCreds("", ""); key != "new" {
		t.Fatalf("documented variable does not win: %q", key)
	}
	if _, key, _ := hostedCreds("", "flag"); key != "flag" {
		t.Fatalf("flag does not win: %q", key)
	}
	t.Setenv(keyEnv, "")
	t.Setenv(legacyKeyEnv, "")
	if _, _, err := hostedCreds("", ""); err == nil || !strings.Contains(err.Error(), keyEnv) {
		t.Fatalf("missing key should name %s: %v", keyEnv, err)
	}
}
