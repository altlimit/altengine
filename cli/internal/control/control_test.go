package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Deleting an instance has to take its DATA with it, and the registry is where the two halves
// are joined: a service says how to drop its files, the registry runs that before the instance
// row it is the only handle on.
func TestDeleteInstanceDropsDataThenIdentity(t *testing.T) {
	reg, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	in, err := reg.Create("datastore", "orders")
	if err != nil {
		t.Fatal(err)
	}

	var dropped []string
	reg.OnDelete("datastore", func(id string) { dropped = append(dropped, id) })
	// A hook belongs to one service: deleting a datastore instance must not drop a search one.
	reg.OnDelete("search", func(id string) { t.Errorf("search drop ran for a datastore delete: %s", id) })

	if !reg.DeleteInstance("datastore", in.ID) {
		t.Fatal("DeleteInstance reported the instance did not exist")
	}
	if len(dropped) != 1 || dropped[0] != in.ID {
		t.Errorf("data drop ran %v, want one call for %s", dropped, in.ID)
	}
	if reg.Get("datastore", "orders") != nil {
		t.Error("the instance is still listed after being deleted")
	}
	// The name is free again — an agent that deletes and re-creates must not hit ALREADY_EXISTS.
	if _, err := reg.Create("datastore", "orders"); err != nil {
		t.Errorf("re-creating the deleted name failed: %v", err)
	}
}

func TestDeleteInstanceUnknownIDDropsNothing(t *testing.T) {
	reg, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	reg.OnDelete("datastore", func(id string) { t.Errorf("dropped data for an instance that does not exist: %s", id) })
	if reg.DeleteInstance("datastore", "nope") {
		t.Error("DeleteInstance claimed to delete an instance that does not exist")
	}
}

func TestResolveAddressed(t *testing.T) {
	reg, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	app := reg.GetOrCreate("functions", "myapp")

	if in, v := reg.ResolveAddressed("functions", "myapp"); in != app || v != 0 {
		t.Errorf("live address = (%v, %d), want myapp live", in, v)
	}
	if in, v := reg.ResolveAddressed("functions", "myapp--v3"); in != app || v != 3 {
		t.Errorf("versioned address = (%v, %d), want myapp v3", in, v)
	}
	for _, label := range []string{"myapp--v0", "myapp--v03", "my--app", "other--v3"} {
		if in, _ := reg.ResolveAddressed("functions", label); in != nil {
			t.Errorf("%q resolved to %s, want nothing", label, in.Name)
		}
	}
}

func TestSaveConfigValidatesThenRunsHooks(t *testing.T) {
	reg, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	in := reg.GetOrCreate("functions", "fx")
	reg.OnValidateConfig("functions", func(cfg map[string]any) error {
		if cfg["bad"] == true {
			return errBad
		}
		return nil
	})
	var before map[string]any
	reg.OnConfigSaved("functions", func(_ *Instance, b map[string]any) { before = b })

	if err := reg.SaveConfig(in, map[string]any{"bad": true}); err != errBad {
		t.Fatalf("SaveConfig = %v, want the validator's error", err)
	}
	if _, written := reg.ConfigSnapshot(in)["bad"]; written || before != nil {
		t.Fatal("a refused config was written, or its hooks ran")
	}

	if err := reg.SaveConfig(in, map[string]any{"keepVersions": 7}); err != nil {
		t.Fatal(err)
	}
	if before["keepVersions"] != 3 || reg.ConfigSnapshot(in)["keepVersions"] != 7 {
		t.Errorf("hook saw before=%v, config now %v", before, reg.ConfigSnapshot(in))
	}
}

// A registry file that does not parse must stop the emulator, not load as empty — the next save
// would write the empty registry over it and orphan every instance's data.
func TestNewRefusesCorruptRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.json")
	corrupt := []byte(`{"instances":[{"id":"abc","service":"datastore","na`) // a half-written file
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("New = %v, want an error naming %s", err, path)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, corrupt) {
		t.Fatalf("the corrupt file was replaced with %q", b)
	}
}

func TestRegistryPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	reg, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := reg.GetOrCreate("datastore", "orders")
	reg2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg2.Get("datastore", "orders"); got == nil || got.ID != in.ID {
		t.Fatalf("after restart got %+v, want instance %s", got, in.ID)
	}
}

// A save that fails must leave the previous file as it was.
func TestFailedSaveKeepsPreviousFile(t *testing.T) {
	dir := t.TempDir()
	reg, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := reg.GetOrCreate("search", "docs")
	path := filepath.Join(dir, "control.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// NaN cannot be encoded, so this save fails.
	reg.SetConfig(in, map[string]any{"bad": math.NaN()})
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("a failed save changed the file:\n%s", after)
	}
	if _, err := New(dir); err != nil {
		t.Fatalf("the file left by a failed save does not load: %v", err)
	}
}

// Requests read an instance's config while the console saves it. Run under -race: a save that
// wrote into the map readers hold is a fatal "concurrent map read and map write".
func TestConfigReadsRaceSaves(t *testing.T) {
	reg, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	in := reg.GetOrCreate("search", "docs")
	held := in.Config()
	heldLen := len(held)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for k, v := range reg.Get("search", "docs").Config() {
					_, _ = k, v
				}
				_, _ = json.Marshal(in)
				_ = reg.ConfigSnapshot(in)
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if err := reg.SaveConfig(in, map[string]any{"rateLimit": i, fmt.Sprintf("k%d", i%7): i}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	if len(held) != heldLen {
		t.Errorf("a config snapshot a reader held changed under it: %v", held)
	}
	if got := in.Config()["rateLimit"]; got != 199 {
		t.Errorf("rateLimit = %v, want 199", got)
	}
}

var errBad = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "bad" }
