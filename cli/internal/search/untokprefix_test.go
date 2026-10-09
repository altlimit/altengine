package search

import (
	"slices"
	"testing"
)

// An untokenprefix prefix matches % _ and \ literally.
func TestUntokenPrefixMatchesWildcardsLiterally(t *testing.T) {
	s := openTest(t)
	var docs []Document
	for id, v := range map[string]string{"pct": "50% off", "fifty": "50x off", "bs": `50\x`, "us": "a_b", "ux": "axb"} {
		docs = append(docs, Document{ID: id, Fields: []Field{field("code", "untokenprefix", v)}})
	}
	if _, err := s.Put("codes", docs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string][]string{
		`code:50%*`:  {"pct"},
		`code:a_*`:   {"us"},
		`code:50\x*`: {"bs"},
	} {
		res, err := s.Search("codes", SearchRequest{Query: q, IDsOnly: true})
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
