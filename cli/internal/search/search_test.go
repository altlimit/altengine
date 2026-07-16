package search

import (
	"encoding/json"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	m := NewManager("")
	s, err := m.Open("inst-"+t.Name(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func field(name, typ string, val any) Field {
	b, _ := json.Marshal(val)
	return Field{Name: name, Type: typ, Value: b}
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	r := func(v int64) *int64 { return &v }
	docs := []Document{
		{ID: "m1", Rank: r(100), Fields: []Field{field("title", "text", "Up in the Air"), field("genre", "atom", "drama"), field("rating", "number", 4)},
			Facets: []Facet{{Name: "genre", Type: "atom", Value: json.RawMessage(`"drama"`)}}},
		{ID: "m2", Rank: r(90), Fields: []Field{field("title", "text", "The Hangover"), field("genre", "atom", "comedy"), field("rating", "number", 3)}},
		{ID: "m3", Rank: r(80), Fields: []Field{field("title", "text", "Air Force One running"), field("genre", "atom", "action"), field("rating", "number", 5)}},
	}
	if _, err := s.Put("movies", docs); err != nil {
		t.Fatal(err)
	}
}

func ids(res *SearchResponse) []string {
	var out []string
	for _, r := range res.Results {
		out = append(out, r.ID)
	}
	return out
}

func TestFieldAndBoolean(t *testing.T) {
	s := openTest(t)
	seed(t, s)

	res, err := s.Search("movies", SearchRequest{Query: "title:air", IDsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res); len(got) != 2 {
		t.Fatalf("title:air expected 2, got %v", got)
	}

	res, _ = s.Search("movies", SearchRequest{Query: "genre:comedy OR rating > 4", IDsOnly: true})
	if len(res.Results) != 2 {
		t.Fatalf("OR expected 2, got %v", ids(res))
	}

	res, _ = s.Search("movies", SearchRequest{Query: "NOT genre:comedy", IDsOnly: true})
	if len(res.Results) != 2 {
		t.Fatalf("NOT expected 2, got %v", ids(res))
	}
}

func TestSortAndFacets(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	res, _ := s.Search("movies", SearchRequest{Sort: []SortSpec{{Expr: "rating", Desc: true}}, IDsOnly: true})
	if got := ids(res); len(got) != 3 || got[0] != "m3" {
		t.Fatalf("sort by rating desc expected m3 first, got %v", got)
	}
	res, _ = s.Search("movies", SearchRequest{FacetDiscover: 5})
	if len(res.Facets) == 0 {
		t.Fatalf("expected facets")
	}
}

func TestGeoDistance(t *testing.T) {
	s := openTest(t)
	docs := []Document{{ID: "g1", Fields: []Field{field("loc", "geo", map[string]float64{"lat": 37.77, "lng": -122.41})}}}
	if _, err := s.Put("places", docs); err != nil {
		t.Fatal(err)
	}
	res, err := s.Search("places", SearchRequest{Query: "distance(loc, geopoint(37.78, -122.42)) < 5000", IDsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("geo expected 1, got %v", ids(res))
	}
}

func TestSnippet(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "d1", Fields: []Field{field("title", "text", "The Godfather Part II"), field("body", "text", "an aging patriarch of a crime dynasty")}},
	}
	if _, err := s.Put("films", docs); err != nil {
		t.Fatal(err)
	}
	res, err := s.Search("films", SearchRequest{Query: "godfather", Snippet: &SnippetReq{Fields: []string{"title"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("expected 1 result, got %v", ids(res))
	}
	got := res.Results[0].Snippet["title"]
	if want := "<b>Godfather</b>"; !strings.Contains(got, want) {
		t.Fatalf("snippet %q missing %q", got, want)
	}
}

func TestSnippetEscapesHTML(t *testing.T) {
	s := openTest(t)
	docs := []Document{{ID: "x1", Fields: []Field{field("body", "text", "<script>alert(1)</script> godfather here")}}}
	if _, err := s.Put("films", docs); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search("films", SearchRequest{Query: "godfather", Snippet: &SnippetReq{Fields: []string{"body"}}})
	got := res.Results[0].Snippet["body"]
	if strings.Contains(got, "<script>") {
		t.Fatalf("snippet leaked raw markup: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "<b>godfather</b>") {
		t.Fatalf("snippet not safely highlighted: %q", got)
	}
}

func seedFilms(t *testing.T, s *Store) {
	t.Helper()
	r := func(v int64) *int64 { return &v }
	docs := []Document{
		{ID: "gf1", Rank: r(96), Fields: []Field{field("title", "text", "The Godfather"), field("franchise", "atom", "godfather")}},
		{ID: "gf2", Rank: r(92), Fields: []Field{field("title", "text", "The Godfather Part II"), field("franchise", "atom", "godfather")}},
		{ID: "ts1", Rank: r(88), Fields: []Field{field("title", "text", "Toy Story"), field("franchise", "atom", "toystory")}},
		{ID: "ts2", Rank: r(84), Fields: []Field{field("title", "text", "Toy Story 2"), field("franchise", "atom", "toystory")}},
		{ID: "solo", Rank: r(70), Fields: []Field{field("title", "text", "Casablanca")}}, // no franchise
	}
	if _, err := s.Put("films", docs); err != nil {
		t.Fatal(err)
	}
}

func TestCollapse(t *testing.T) {
	s := openTest(t)
	seedFilms(t, s)
	res, err := s.Search("films", SearchRequest{Collapse: &CollapseReq{Field: "franchise"}, Limit: 20, TotalHitsAccuracy: 100, IDsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// One best-ranked per franchise; the franchise-less doc is its own group.
	got := ids(res)
	want := []string{"gf1", "ts1", "solo"}
	if len(got) != len(want) {
		t.Fatalf("collapse expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collapse order: expected %v, got %v", want, got)
		}
	}
	if !res.TotalHitsExact || res.TotalHits != 5 {
		t.Fatalf("total_hits must stay uncollapsed (5), got %d exact=%v", res.TotalHits, res.TotalHitsExact)
	}
}

func TestCollapseTopN(t *testing.T) {
	s := openTest(t)
	seedFilms(t, s)
	res, _ := s.Search("films", SearchRequest{Collapse: &CollapseReq{Field: "franchise", Limit: 2}, Limit: 20, IDsOnly: true})
	got := ids(res)
	// Two per franchise now: godfather gf1,gf2 ; toystory ts1,ts2 ; solo.
	if len(got) != 5 {
		t.Fatalf("collapse limit 2 expected 5 rows, got %v", got)
	}
}

func TestCollapseValidation(t *testing.T) {
	s := openTest(t)
	seedFilms(t, s)
	if _, err := s.Search("films", SearchRequest{Collapse: &CollapseReq{Field: "title"}}); err == nil {
		t.Fatal("collapse on a text field must be rejected")
	}
	if _, err := s.Search("films", SearchRequest{Collapse: &CollapseReq{Field: "nope"}}); err == nil {
		t.Fatal("collapse on an unknown field must be rejected")
	}
	if _, err := s.Search("films", SearchRequest{Collapse: &CollapseReq{Field: "franchise"}, Sort: []SortSpec{{Expr: "rank"}}}); err == nil {
		t.Fatal("collapse + sort must be rejected")
	}
}

func jsonVal(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSynonyms(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "laptop", Fields: []Field{field("body", "text", "a fast laptop")}},
		{ID: "notebook", Fields: []Field{field("body", "text", "a light notebook")}},
		{ID: "sneaker", Fields: []Field{field("body", "text", "a canvas sneaker")}},
		{ID: "shoe", Fields: []Field{field("body", "text", "a leather shoe")}},
	}
	if _, err := s.Put("things", docs); err != nil {
		t.Fatal(err)
	}
	s.ApplyConfig(map[string]any{"synonyms": jsonVal(t, `{
		"equivalents": [["laptop","notebook"]],
		"oneWay": [{"from":"shoe","to":["sneaker"]}]
	}`)})

	res, _ := s.Search("things", SearchRequest{Query: "laptop", IDsOnly: true})
	if got := ids(res); len(got) != 2 {
		t.Fatalf("equivalents should widen laptop -> 2 docs, got %v", got)
	}
	res, _ = s.Search("things", SearchRequest{Query: "shoe", IDsOnly: true})
	if got := ids(res); len(got) != 2 {
		t.Fatalf("oneWay shoe -> sneaker expected 2, got %v", got)
	}
	res, _ = s.Search("things", SearchRequest{Query: "sneaker", IDsOnly: true})
	if got := ids(res); len(got) != 1 {
		t.Fatalf("oneWay must not reverse, got %v", got)
	}
	// expansion inside NOT: excluding one form excludes the other
	res, _ = s.Search("things", SearchRequest{Query: "a NOT laptop", IDsOnly: true})
	for _, id := range ids(res) {
		if id == "laptop" || id == "notebook" {
			t.Fatalf("NOT laptop leaked %s", id)
		}
	}
}

func TestNumeralSynonyms(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "g2", Fields: []Field{field("t", "text", "the godfather 2")}},
		{ID: "gII", Fields: []Field{field("t", "text", "the godfather ii")}},
		{ID: "gTwo", Fields: []Field{field("t", "text", "the godfather two")}},
		{ID: "sx", Fields: []Field{field("t", "text", "series x")}},
		{ID: "s10", Fields: []Field{field("t", "text", "series 10")}},
	}
	if _, err := s.Put("films", docs); err != nil {
		t.Fatal(err)
	}

	// off by default: each written form only matches its own doc
	res, _ := s.Search("films", SearchRequest{Query: "godfather 2", IDsOnly: true})
	if got := ids(res); len(got) != 1 || got[0] != "g2" {
		t.Fatalf("no expansion by default, got %v", got)
	}

	// oneway: digit -> forms, but NOT the reverse
	s.ApplyConfig(map[string]any{"synonyms": jsonVal(t, `{"numberRoman":"oneway","numberWords":"oneway"}`)})
	res, _ = s.Search("films", SearchRequest{Query: "godfather 2", IDsOnly: true})
	if got := ids(res); len(got) != 3 {
		t.Fatalf("oneway digit should reach all three forms, got %v", got)
	}
	res, _ = s.Search("films", SearchRequest{Query: "godfather ii", IDsOnly: true})
	if got := ids(res); len(got) != 1 || got[0] != "gII" {
		t.Fatalf("oneway must not reverse roman->digit, got %v", got)
	}

	// twoway: the reverse works too
	s.ApplyConfig(map[string]any{"synonyms": jsonVal(t, `{"numberRoman":"twoway"}`)})
	res, _ = s.Search("films", SearchRequest{Query: "godfather ii", IDsOnly: true})
	if got := ids(res); len(got) != 2 {
		t.Fatalf("twoway roman->digit expected g2+gII, got %v", got)
	}

	// single-letter guard: "series x" never becomes "series 10"
	res, _ = s.Search("films", SearchRequest{Query: "series x", IDsOnly: true})
	if got := ids(res); len(got) != 1 || got[0] != "sx" {
		t.Fatalf("single-letter roman must not convert, got %v", got)
	}
}

func TestRules(t *testing.T) {
	s := openTest(t)
	seedFilms(t, s)
	s.ApplyConfig(map[string]any{"rules": jsonVal(t, `[
		{"when":{"query":"story","match":"contains"},
		 "then":{"pin":[{"id":"solo","pos":0}],"hide":["ts2"],"data":{"banner":"toys!"}}}
	]`)})

	res, err := s.Search("films", SearchRequest{Query: "story", IDsOnly: true, Limit: 20, TotalHitsAccuracy: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res)
	// natural match for "story" is ts1+ts2; ts2 is hidden, solo is pinned to position 0.
	if len(got) != 2 || got[0] != "solo" || got[1] != "ts1" {
		t.Fatalf("rules expected [solo ts1], got %v", got)
	}
	if res.TotalHits != 2 { // 1 natural (ts1) + 1 live pin
		t.Fatalf("total_hits expected 2, got %d", res.TotalHits)
	}
	if len(res.RuleData) != 1 {
		t.Fatalf("expected rule_data echoed, got %v", res.RuleData)
	}
	// a different query does not trip the rule
	res, _ = s.Search("films", SearchRequest{Query: "godfather", IDsOnly: true})
	if len(res.RuleData) != 0 {
		t.Fatalf("rule must not fire for godfather, got %v", res.RuleData)
	}
	for _, id := range ids(res) {
		if id == "solo" {
			t.Fatal("pin leaked into a non-matching query")
		}
	}
}
