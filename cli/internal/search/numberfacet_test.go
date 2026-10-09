package search

import (
	"encoding/json"
	"testing"
)

// A number facet is one [min, max) range whose max is one past the top value, so refining on
// the returned range counts back every document the facet counted — including those at the top.
func TestNumberFacetRangeRefinesBackToItsCount(t *testing.T) {
	s := openTest(t)
	var docs []Document
	for i, year := range []int{2010, 2012, 2014, 2014} {
		docs = append(docs, Document{
			ID:     string(rune('a' + i)),
			Fields: []Field{field("t", "atom", "x")},
			Facets: []Facet{{Name: "year", Type: "number", Value: json.RawMessage(jsonNum(year))}},
		})
	}
	if _, err := s.Put("films", docs); err != nil {
		t.Fatal(err)
	}
	res, err := s.Search("films", SearchRequest{Facets: []string{"year"}, IDsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facets) != 1 || res.Facets[0].Type != "number" || len(res.Facets[0].Values) != 1 {
		t.Fatalf("facets = %+v", res.Facets)
	}
	v := res.Facets[0].Values[0]
	if v.Value != "2010-2014" || v.Count != 4 || v.Min == nil || *v.Min != 2010 || v.Max == nil || *v.Max != 2015 {
		t.Fatalf("number facet = %+v (min %v max %v), want 2010-2014 x4 [2010, 2015)", v, deref(v.Min), deref(v.Max))
	}
	refined, err := s.Search("films", SearchRequest{IDsOnly: true,
		FacetRefinements: []Refinement{{Name: "year", Min: v.Min, Max: v.Max}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(refined.Results) != 4 {
		t.Fatalf("refining on the facet's own range found %d of 4 documents", len(refined.Results))
	}
	if numberFacetMax(1<<53) <= 1<<53 {
		t.Fatal("past 2^53 the max must still sit above the top value")
	}
}

func jsonNum(n int) string { b, _ := json.Marshal(n); return string(b) }

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
