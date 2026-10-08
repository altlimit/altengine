package functions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSecrets("i1", map[string]json.RawMessage{"API_KEY": json.RawMessage(`"v"`)}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Config("i1").Secrets["API_KEY"].Value; got != "v" {
		t.Fatalf("after restart secret = %q, want %q", got, "v")
	}
}

// State that does not parse must stop the emulator, not load as empty — the next deploy would
// save the empty state over it and every function in it would be gone.
func TestCorruptStateIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "functions", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"configs":{"i1":{"functions":[{"na`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("NewStore = %v, want an error naming %s", err, path)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, corrupt) {
		t.Fatalf("the corrupt file was replaced with %q", b)
	}
}
