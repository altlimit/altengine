package search

import (
	"slices"
	"testing"
)

// An untokenprefix query is always a case-insensitive prefix match, as hosted: the value is the
// prefix with or without a trailing `*`, which is part of the prefix rather than an operator.
// % _ and \ match literally.
func TestUntokenPrefixQuery(t *testing.T) {
	s := openTest(t)
	var docs []Document
	for id, v := range map[string]string{
		"pct": "50% off", "fifty": "50x off", "bs": `50\x`, "us": "a_b", "ux": "axb",
		"sku": "ABC-123", "sku2": "abc-124", "star": "abc*def", "hello": "Hello World",
	} {
		docs = append(docs, Document{ID: id, Fields: []Field{field("code", "untokenprefix", v)}})
	}
	if _, err := s.Put("codes", docs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string][]string{
		`code:50%`:           {"pct"},
		`code:a_`:            {"us"},
		`code:50\x`:          {"bs"},
		`code:abc`:           {"sku", "sku2", "star"},
		`code:ABC-12`:        {"sku", "sku2"},
		`code=abc-123`:       {"sku"},
		`code:abc*`:          {"star"},
		`code:"hello wo"`:    {"hello"},
		`code:"Hello World"`: {"hello"},
		`code:zzz`:           nil,
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
