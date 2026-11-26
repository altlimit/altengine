package search

import (
	"errors"
	"slices"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

func ptr[T any](v T) *T { return &v }

func seedRanked(t *testing.T, s *Store, n int) {
	t.Helper()
	var docs []Document
	for i := 0; i < n; i++ {
		rank := int64(100 - i) // w0 ranks highest
		docs = append(docs, Document{ID: "w" + string(rune('0'+i)), Rank: &rank, Fields: []Field{field("body", "text", "widget")}})
	}
	promo := int64(1)
	docs = append(docs, Document{ID: "promo", Rank: &promo, Fields: []Field{field("body", "text", "unrelated")}})
	if _, err := s.Put("pg", docs); err != nil {
		t.Fatal(err)
	}
}

func hitIDs(res *SearchResponse) []string {
	out := []string{}
	for _, r := range res.Results {
		out = append(out, r.ID)
	}
	return out
}

// limit, as hosted: absent is 20, 0 is counts and facets only (no results, no cursor),
// negative is a 400, and anything over 1000 is 1000.
func TestSearchLimitZero(t *testing.T) {
	s := openTest(t)
	seedRanked(t, s, 5)
	res, err := s.Search("pg", SearchRequest{Query: "widget", Limit: ptr(0), Sort: []SortSpec{{Expr: "rank"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 0 || res.Cursor != nil || res.TotalHits != 5 {
		t.Fatalf("limit 0: results=%v cursor=%v total=%d", hitIDs(res), res.Cursor, res.TotalHits)
	}
	if res, _ := s.Search("pg", SearchRequest{Query: "widget"}); len(res.Results) != 5 {
		t.Fatalf("absent limit: got %v", hitIDs(res))
	}
	_, err = s.Search("pg", SearchRequest{Limit: ptr(-1)})
	var ae *common.APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "limit must be a non-negative number" {
		t.Fatalf("negative limit: err=%#v", err)
	}
	// A bad sort default is still a 400 with limit 0.
	if _, err := s.Search("pg", SearchRequest{Limit: ptr(0), Sort: []SortSpec{{Expr: "x", Default: true}}}); err == nil {
		t.Fatal("limit 0 must still validate the sort")
	}
}

// rank sorts descending unless desc is explicitly false; a field ascending unless desc is true.
func TestSortRankDirection(t *testing.T) {
	s := openTest(t)
	seedRanked(t, s, 3)
	for _, c := range []struct {
		desc *bool
		want []string
	}{
		{nil, []string{"w0", "w1", "w2"}},
		{ptr(true), []string{"w0", "w1", "w2"}},
		{ptr(false), []string{"w2", "w1", "w0"}},
	} {
		res, err := s.Search("pg", SearchRequest{Query: "widget", Sort: []SortSpec{{Expr: "rank", Desc: c.desc}}})
		if err != nil || !slices.Equal(hitIDs(res), c.want) {
			t.Errorf("desc=%v: got %v err=%v, want %v", c.desc, hitIDs(res), err, c.want)
		}
	}
}

// A page that pins fill entirely still carries a cursor to the natural results, and
// walking the cursor returns every document once with the pins in place.
func TestPinnedPagesPage(t *testing.T) {
	s := openTest(t)
	seedRanked(t, s, 5)
	s.ApplyConfig(map[string]any{"rules": jsonVal(t, `[
		{"when":{"query":"widget","match":"is"},"then":{"pin":[{"id":"promo","pos":0},{"id":"w4","pos":1}]}}
	]`)})
	var seen []string
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		res, err := s.Search("pg", SearchRequest{Query: "widget", Limit: ptr(2), Cursor: cursor, IDsOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if pages == 0 && res.Cursor == nil {
			t.Fatal("a fully pinned first page must carry a cursor")
		}
		seen = append(seen, hitIDs(res)...)
		if res.Cursor == nil {
			break
		}
		cursor = *res.Cursor
	}
	if want := []string{"promo", "w4", "w0", "w1", "w2", "w3"}; !slices.Equal(seen, want) {
		t.Fatalf("got %v, want %v", seen, want)
	}
	res, _ := s.Search("pg", SearchRequest{Query: "widget", Limit: ptr(0)})
	if len(res.Results) != 0 || res.Cursor != nil || res.TotalHits != 6 {
		t.Fatalf("limit 0 with pins: results=%v cursor=%v total=%d", hitIDs(res), res.Cursor, res.TotalHits)
	}
}
