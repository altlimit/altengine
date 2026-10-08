package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/hosted"
)

// isolate points the saved-credentials file at a temp path, so a test never reads the
// developer's own.
func isolate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg", "credentials.json")
	t.Setenv(credsEnv, p)
	t.Setenv("ALTENGINE_URL", "")
	t.Setenv(keyEnv, "")
	t.Setenv(legacyKeyEnv, "")
	return p
}

// The CLI read ALTENGINE_KEY and the SDKs ALTENGINE_API_KEY, so one shell setup worked for one
// and not the other. Both are read now, the documented one first, and a flag beats both.
func TestHostedCredsReadsBothKeyVariables(t *testing.T) {
	isolate(t)
	t.Setenv("ALTENGINE_URL", "https://api.example.com")
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
	if _, _, err := hostedCreds("", ""); err == nil || !strings.Contains(err.Error(), "altengine login") {
		t.Fatalf("missing key should say how to fix it: %v", err)
	}
}

// `altengine login` keeps a key in an owner-only file, so it need not sit in a flag (visible in
// ps) or an environment variable; the hosted commands use it when nothing else is given.
func TestLoginSavesAKeyTheCommandsUse(t *testing.T) {
	p := isolate(t)

	r, w, _ := os.Pipe()
	_, _ = w.WriteString("ae_saved\n")
	w.Close()
	stdin := os.Stdin
	os.Stdin = r
	loginCmd([]string{"--url", "https://eu.example.com"})
	os.Stdin = stdin

	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("credentials file: %v %v", fi, err)
		}
	}
	url, key, err := hostedCreds("", "")
	if err != nil || key != "ae_saved" || url != "https://eu.example.com" {
		t.Fatalf("saved creds = %q %q %v", url, key, err)
	}
	// An environment key still wins, and then the URL is the default, not the saved key's.
	t.Setenv(keyEnv, "ae_env")
	if url, key, _ := hostedCreds("", ""); key != "ae_env" || url != hosted.DefaultURL {
		t.Fatalf("env key = %q at %q", key, url)
	}
	t.Setenv(keyEnv, "")

	logoutCmd(nil)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("logout left the file: %v", err)
	}
	if _, _, err := hostedCreds("", ""); err == nil {
		t.Fatal("a key survived logout")
	}
}
