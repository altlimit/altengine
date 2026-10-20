package search

import (
	"slices"
	"testing"
)

// There is no `*` operator, as hosted. In full text a trailing `*` is a separator the
// tokenizer drops, so `abc*` matches the word "abc" and not "abcdef"; a tokenprefix field
// always matches its value's first word as a prefix, star or not; an atom keeps the `*` as
// a character.
func TestNoStarOperator(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "abc", Fields: []Field{field("body", "text", "abc ghi")}},
		{ID: "abcdef", Fields: []Field{field("body", "text", "abcdef ghi")}},
		{ID: "won", Fields: []Field{field("tp", "tokenprefix", "Hello wonderful world")}},
		{ID: "atom", Fields: []Field{field("tag", "atom", "abc*")}},
	}
	if _, err := s.Put("star", docs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string][]string{
		`abc*`:         {"abc", "atom"},
		`body:abc*`:    {"abc"},
		`body:abc`:     {"abc"},
		`tp:won`:       {"won"},
		`tp:won*`:      {"won"},
		`tp:WON`:       {"won"},
		`tp:"won"`:     nil,
		`tp:"hello"`:   {"won"},
		`tag:abc*`:     {"atom"},
		`tag:abc`:      nil,
		`NOT body:ab*`: {"abc", "abcdef", "atom", "won"},
	} {
		res, err := s.Search("star", SearchRequest{Query: q, IDsOnly: true})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got := ids(res)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s -> %v, want %v", q, got, want)
		}
	}
}
