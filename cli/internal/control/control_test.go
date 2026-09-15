package control

import "testing"

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

	if err := reg.SaveConfig(in, map[string]any{"keepVersions": 3}); err != nil {
		t.Fatal(err)
	}
	if before["keepVersions"] != 10 || reg.ConfigSnapshot(in)["keepVersions"] != 3 {
		t.Errorf("hook saw before=%v, config now %v", before, reg.ConfigSnapshot(in))
	}
}

var errBad = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "bad" }
