package datastore

import (
	"slices"
	"testing"
)

// Collections are listed a page at a time, in name order, with the last name as the cursor.
func TestCollectionsArePaged(t *testing.T) {
	s := openTest(t)
	for _, c := range []string{"b", "a", "c"} {
		mustPut(t, s, c, "k1", `{}`, "k2", `{}`)
	}
	page, cursor, err := s.Collections("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(page, []string{"a", "b"}) || cursor == nil || *cursor != "b" {
		t.Fatalf("first page = %v cursor %v", page, cursor)
	}
	page, cursor, err = s.Collections(*cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(page, []string{"c"}) || cursor != nil {
		t.Fatalf("second page = %v cursor %v", page, cursor)
	}
}
