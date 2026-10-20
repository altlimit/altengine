package search

import (
	"slices"
	"testing"
)

// A field written as more than one type matches by every one of them, as hosted: the
// results are the union of the full-text, atom and untokenprefix matches. And a number or
// date value compares numerically whatever the field's types.
func TestFieldOfSeveralTypesMatchesByEach(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "text", Fields: []Field{field("code", "text", "Blue Widget"), field("year", "text", "2011")}},
		{ID: "atom", Fields: []Field{field("code", "atom", "blue")}},
		{ID: "untok", Fields: []Field{field("code", "untokenprefix", "Blueberry Jam")}},
		{ID: "num", Fields: []Field{field("year", "number", float64(2011))}},
		{ID: "date", Fields: []Field{field("on", "date", "2011-02-28")}},
	}
	if _, err := s.Put("multi", docs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string][]string{
		`code:blue`:            {"atom", "text", "untok"},
		`code=BLUE`:            {"atom", "text", "untok"},
		`code:widget`:          {"text"},
		`code:"blue widget"`:   {"text"},
		`code:"blueberry j"`:   {"untok"},
		`code:(blue OR jam)`:   {"atom", "text", "untok"},
		`NOT code:blue`:        {"date", "num"},
		`year:2011`:            {"num"},
		`year:"2011"`:          {"text"},
		`on:2011-02-28`:        {"date"},
		`code:blue year:2011`:  nil,
		`code:blue OR year:20`: {"atom", "text", "untok"},
	} {
		res, err := s.Search("multi", SearchRequest{Query: q, IDsOnly: true})
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
