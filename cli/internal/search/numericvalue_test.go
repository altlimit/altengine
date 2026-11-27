package search

import (
	"slices"
	"sort"
	"testing"
)

// A number or date value in `field:value` matches every type the field holds, as hosted:
// an atom or untokenprefix by its literal text, full text by its token, a number or date
// numerically. Before, only the numeric reading existed, so `sku:12345` found nothing.
func TestNumericValueMatchesByFieldType(t *testing.T) {
	s := openTest(t)
	r := func(v int64) *int64 { return &v }
	docs := []Document{
		{ID: "p1", Rank: r(5), Fields: []Field{field("sku", "atom", "12345"), field("title", "text", "Back to 2011"), field("code", "untokenprefix", "12345-A")}},
		{ID: "p2", Rank: r(4), Fields: []Field{field("sku", "atom", "007"), field("title", "text", "Other"), field("code", "untokenprefix", "99-B")}},
		{ID: "p3", Rank: r(3), Fields: []Field{field("sku", "atom", "2024-01-01"), field("year", "number", 2011)}},
		{ID: "p4", Rank: r(2), Fields: []Field{field("sku", "atom", "-5"), field("year", "number", 1999)}},
		{ID: "m1", Rank: r(1), Fields: []Field{field("mix", "atom", "100"), field("title", "text", "Other")}},
		{ID: "m2", Rank: r(0), Fields: []Field{field("mix", "number", 100), field("title", "text", "Other")}},
	}
	if _, err := s.Put("nv", docs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string][]string{
		"sku:12345":               {"p1"},
		`sku:"12345"`:             {"p1"},
		"sku=12345":               {"p1"},
		"sku:007":                 {"p2"},
		"sku:7":                   nil,
		"sku:-5":                  {"p4"},
		"sku:2024-01-01":          {"p3"},
		"code:123":                {"p1"},
		"title:2011":              {"p1"},
		"year:2011":               {"p3"},
		"year:2011.0":             {"p3"},
		"year > 2000":             {"p3"},
		"mix:100":                 {"m1", "m2"},
		"mix:100.0":               {"m2"},
		"NOT sku:12345":           {"m1", "m2", "p2", "p3", "p4"},
		"title:other NOT mix:100": {"p2"},
		"NOT code:123":            {"m1", "m2", "p2", "p3", "p4"},
	} {
		res, err := s.Search("nv", SearchRequest{Query: q, IDsOnly: true, Limit: ptr(50), TotalHitsAccuracy: 100})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		got := hitIDs(res)
		sort.Strings(got)
		if len(want) == 0 {
			want = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", q, got, want)
		}
		if res.TotalHits != len(got) {
			t.Errorf("%s: total_hits %d, results %d", q, res.TotalHits, len(got))
		}
	}
}
