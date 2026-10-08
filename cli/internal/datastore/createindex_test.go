package datastore

import (
	"fmt"
	"strings"
	"testing"
)

// An index declaration is checked with the hosted rules. Locally anything non-empty used to be
// stored, so a declaration production refuses worked all through development.
func TestCreateIndexValidation(t *testing.T) {
	s := openTest(t)
	bad := []struct {
		fields []string
		unique bool
		want   string
	}{
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, false, "1..8 fields"},
		{[]string{"a", "a:desc"}, false, "repeat the same field"},
		{[]string{"__key__"}, false, "reserved field __key__"},
		{[]string{"__updated__"}, false, "last column"},
		{[]string{"__created__", "a"}, false, "last column"},
		{[]string{"a", "__updated__"}, true, "unique index cannot include"},
		{[]string{"a-b"}, false, "invalid field path"},
	}
	for _, c := range bad {
		_, err := s.CreateIndex("c", c.fields, c.unique)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v unique=%v: got %v, want %q", c.fields, c.unique, err, c.want)
		}
	}

	if _, err := s.CreateIndex("c", []string{"status", "__updated__"}, false); err != nil {
		t.Fatalf("a recency composite was refused: %v", err)
	}
	// "age" and "age:asc" are one index.
	if _, err := s.CreateIndex("c", []string{"age"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIndex("c", []string{"age:ASC"}, false); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListIndexes("c")
	if len(list) != 2 {
		t.Fatalf("age:asc was stored as a second index: %+v", list)
	}
}

func TestCreateIndexCapPerCollection(t *testing.T) {
	s := openTest(t)
	for i := 0; i < maxIndexesPerCollection; i++ {
		if _, err := s.CreateIndex("c", []string{fmt.Sprintf("f%d", i)}, false); err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
	}
	if _, err := s.CreateIndex("c", []string{"one_more"}, false); err == nil || !strings.Contains(err.Error(), "which is the limit") {
		t.Fatalf("the 65th index: %v", err)
	}
	// Re-declaring one it already has still works at the cap.
	if _, err := s.CreateIndex("c", []string{"f0"}, false); err != nil {
		t.Fatalf("re-declaring at the cap: %v", err)
	}
}
