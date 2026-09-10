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
