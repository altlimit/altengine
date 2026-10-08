package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --reset used to RemoveAll whatever --data named, unasked: `--data . --reset` deleted the
// project. Only an emulator data directory (or an empty one) is wiped, and only once confirmed.
func TestResetOnlyWipesADataDirectory(t *testing.T) {
	yes := func(string) bool { return true }

	project := t.TempDir()
	_ = os.WriteFile(filepath.Join(project, "main.go"), []byte("package main"), 0o600)
	if err := resetDataDir(project, yes); err == nil {
		t.Fatal("reset a directory that is not a data directory")
	}
	if _, err := os.Stat(filepath.Join(project, "main.go")); err != nil {
		t.Fatal("the project file is gone")
	}

	data := t.TempDir()
	_ = os.WriteFile(filepath.Join(data, "control.json"), []byte("{}"), 0o600)
	if err := resetDataDir(data, func(string) bool { return false }); err == nil {
		t.Fatal("a refused confirmation still reset")
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatal("a refused confirmation deleted the directory")
	}
	if err := resetDataDir(data, yes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("the data directory survived a confirmed reset")
	}

	if err := resetDataDir(filepath.Join(t.TempDir(), "missing"), yes); err != nil {
		t.Fatalf("a missing directory: %v", err)
	}
}
