//go:build conformance

package altengine_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	altengine "github.com/altlimit/altengine/go"
)

type searchCorpus struct {
	Documents []altengine.SearchDocument `json:"documents"`
}

func seedSearch(t *testing.T) (*altengine.Search, *altengine.SearchIndex, searchCorpus) {
	t.Helper()
	var corpus searchCorpus
	loadFixture(t, "search-corpus.json", &corpus)
	s := client().Search(uniq("sdk-conf"))
	idx := s.Index("products")
	ids, err := idx.Put(ctx(t), corpus.Documents)
	if err != nil {
		t.Fatalf("seeding corpus: %v", err)
	}
	want := make([]string, len(corpus.Documents))
	for i, d := range corpus.Documents {
		want[i] = d.ID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("put ids = %v, want %v", ids, want)
	}
	return s, idx, corpus
}

func hitIDs(res *altengine.SearchResponse) []string {
	ids := make([]string, len(res.Results))
	for i, h := range res.Results {
		ids[i] = h.ID
	}
	return ids
}

func sortedHitIDs(res *altengine.SearchResponse) []string {
	ids := hitIDs(res)
	sort.Strings(ids)
	return ids
}

func TestSearchDocuments(t *testing.T) {
	_, idx, _ := seedSearch(t)

	t.Run("get roundtrips all field types", func(t *testing.T) {
		doc, err := idx.Get(ctx(t), "p1")
		if err != nil || doc == nil {
			t.Fatalf("doc=%v err=%v", doc, err)
		}
		byName := map[string]altengine.SearchField{}
		for _, f := range doc.Fields {
			byName[f.Name] = f
		}
		if byName["title"].Value != "Blue Suede Shoes" {
			t.Fatalf("title=%v", byName["title"].Value)
		}
		if byName["price"].Value != float64(59) {
			t.Fatalf("price=%v", byName["price"].Value)
		}
		geo, ok := byName["store"].Value.(map[string]any)
		if !ok || geo["lat"] != 37.77 || geo["lng"] != -122.41 {
			t.Fatalf("store=%v", byName["store"].Value)
		}
		if len(doc.Facets) != 2 {
			t.Fatalf("facets=%v", doc.Facets)
		}
	})

	t.Run("get of a missing doc returns nil", func(t *testing.T) {
		doc, err := idx.Get(ctx(t), "nope")
		if err != nil || doc != nil {
			t.Fatalf("doc=%v err=%v", doc, err)
		}
	})

	t.Run("server assigns ids when omitted", func(t *testing.T) {
		ids, err := idx.Put(ctx(t), []altengine.SearchDocument{{Fields: []altengine.SearchField{altengine.Text("title", "temp")}}})
		if err != nil || len(ids) != 1 || ids[0] == "" {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
		idx.Delete(ctx(t), ids)
	})

	t.Run("lists documents with keyset pagination", func(t *testing.T) {
		var seen []string
		for doc, err := range idx.ListAllDocuments(ctx(t), 2) {
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, doc.ID)
		}
		sort.Strings(seen)
		if !reflect.DeepEqual(seen, []string{"p1", "p2", "p3", "p4"}) {
			t.Fatalf("got %v", seen)
		}
	})
}

func TestSearchQueries(t *testing.T) {
	_, idx, _ := seedSearch(t)

	t.Run("matches bare terms and field terms", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{Query: "shoes"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sortedHitIDs(res), []string{"p1", "p2", "p4"}) {
			t.Fatalf("got %v", sortedHitIDs(res))
		}
		atom, err := idx.Search(ctx(t), altengine.SearchRequest{Query: `sku:"BS-001"`})
		if err != nil || !reflect.DeepEqual(hitIDs(atom), []string{"p1"}) {
			t.Fatalf("got %v err=%v", hitIDs(atom), err)
		}
	})

	t.Run("supports boolean operators and comparisons", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{Query: "shoes AND price<100"})
		if err != nil || !reflect.DeepEqual(sortedHitIDs(res), []string{"p1", "p4"}) {
			t.Fatalf("got %v err=%v", sortedHitIDs(res), err)
		}
		not, err := idx.Search(ctx(t), altengine.SearchRequest{Query: "shoes NOT blue"})
		if err != nil || !reflect.DeepEqual(hitIDs(not), []string{"p2"}) {
			t.Fatalf("got %v err=%v", hitIDs(not), err)
		}
	})

	t.Run("sorts by field and paginates with cursors", func(t *testing.T) {
		page1, err := idx.Search(ctx(t), altengine.SearchRequest{
			Query: "shoes",
			Sort:  []altengine.SortSpec{{Expr: "price"}},
			Limit: 2,
		})
		if err != nil || !reflect.DeepEqual(hitIDs(page1), []string{"p4", "p1"}) || page1.Cursor == "" {
			t.Fatalf("page1=%v cursor=%q err=%v", hitIDs(page1), page1.Cursor, err)
		}
		page2, err := idx.Search(ctx(t), altengine.SearchRequest{
			Query:  "shoes",
			Sort:   []altengine.SortSpec{{Expr: "price"}},
			Limit:  2,
			Cursor: page1.Cursor,
		})
		if err != nil || !reflect.DeepEqual(hitIDs(page2), []string{"p2"}) {
			t.Fatalf("page2=%v err=%v", hitIDs(page2), err)
		}
	})

	t.Run("returns facets with counts and honors refinements", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{Query: "", Facets: []string{"category"}})
		if err != nil {
			t.Fatal(err)
		}
		var shoesCount int
		for _, f := range res.Facets {
			if f.Name == "category" {
				for _, v := range f.Values {
					if v.Value == "shoes" {
						shoesCount = v.Count
					}
				}
			}
		}
		if shoesCount != 3 {
			t.Fatalf("shoes facet count=%d", shoesCount)
		}
		refined, err := idx.Search(ctx(t), altengine.SearchRequest{
			Query:            "",
			FacetRefinements: []altengine.FacetRefinement{{Name: "category", Value: "accessories"}},
		})
		if err != nil || !reflect.DeepEqual(hitIDs(refined), []string{"p3"}) {
			t.Fatalf("refined=%v err=%v", hitIDs(refined), err)
		}
	})

	t.Run("snippets highlight matches", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{
			Query:   "suede",
			Snippet: &altengine.SnippetRequest{Fields: []string{"title"}, PreTag: "<em>", PostTag: "</em>"},
		})
		if err != nil || len(res.Results) == 0 || !strings.Contains(res.Results[0].Snippet["title"], "<em>") {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("collapse keeps top doc per distinct value", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{
			Query:    "shoes",
			Collapse: &altengine.CollapseRequest{Field: "category", Limit: 1},
		})
		if err != nil || len(res.Results) != 1 {
			t.Fatalf("results=%v err=%v", hitIDs(res), err)
		}
	})

	t.Run("ids_only omits documents", func(t *testing.T) {
		res, err := idx.Search(ctx(t), altengine.SearchRequest{Query: "shoes", IDsOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range res.Results {
			if h.Document != nil {
				t.Fatalf("hit %s carries a document", h.ID)
			}
		}
	})
}

func TestSearchIndexManagement(t *testing.T) {
	s, idx, _ := seedSearch(t)

	t.Run("schema reflects the union of fields", func(t *testing.T) {
		schema, err := idx.Schema(ctx(t))
		if err != nil || schema.Name != "products" {
			t.Fatalf("schema=%+v err=%v", schema, err)
		}
		for _, f := range []string{"title", "price", "sku"} {
			if _, ok := schema.Fields[f]; !ok {
				t.Fatalf("schema missing field %s", f)
			}
		}
		found := false
		for _, ty := range schema.Fields["price"] {
			if ty == "number" {
				found = true
			}
		}
		if !found {
			t.Fatalf("price types=%v", schema.Fields["price"])
		}
	})

	t.Run("namespaces are isolated and listable", func(t *testing.T) {
		other := s.WithNamespace(uniq("ns"))
		if _, err := other.Index("products").Put(ctx(t), []altengine.SearchDocument{
			{ID: "only", Fields: []altengine.SearchField{altengine.Text("title", "hidden")}},
		}); err != nil {
			t.Fatal(err)
		}
		if doc, _ := idx.Get(ctx(t), "only"); doc != nil {
			t.Fatal("namespace leak")
		}
		page, err := other.ListIndexes(ctx(t), altengine.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ix := range page.Indexes {
			if ix.Name == "products" {
				found = true
			}
		}
		if !found {
			t.Fatal("index missing from namespaced list")
		}

		nss, err := s.ListNamespaces(ctx(t), altengine.ListOptions{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		hasOther, hasDefault := false, false
		for _, ns := range nss.Namespaces {
			if ns == other.Namespace {
				hasOther = true
			}
			if ns == "" {
				hasDefault = true
			}
		}
		if !hasOther || !hasDefault {
			t.Fatalf("namespaces=%v", nss.Namespaces)
		}
		filtered, err := s.ListNamespaces(ctx(t), altengine.ListOptions{Q: other.Namespace[:8]})
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, ns := range filtered.Namespaces {
			if ns == other.Namespace {
				found = true
			}
		}
		if !found {
			t.Fatalf("filtered namespaces=%v", filtered.Namespaces)
		}

		if destructiveOk() {
			if _, err := other.DeleteIndex(ctx(t), "products"); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("rejects invalid namespaces with 400 INVALID_ARGUMENT", func(t *testing.T) {
		// Via ?namespace= (ListIndexes) — a query param survives URL-encoding
		// for all three cases, including the NUL byte a header could never carry.
		for _, bad := range []string{"a\x00b", strings.Repeat("x", 101), "café"} {
			_, err := s.WithNamespace(bad).ListIndexes(ctx(t), altengine.ListOptions{})
			var ae *altengine.APIError
			if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "INVALID_ARGUMENT" {
				t.Fatalf("namespace %q: err=%v", bad, err)
			}
		}
	})

	t.Run("lists indexes and deletes documents", func(t *testing.T) {
		page, err := s.ListIndexes(ctx(t), altengine.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ix := range page.Indexes {
			if ix.Name == "products" {
				found = true
			}
		}
		if !found {
			t.Fatal("products index missing")
		}

		if _, err := idx.Put(ctx(t), []altengine.SearchDocument{
			{ID: "todelete", Fields: []altengine.SearchField{altengine.Text("title", "x")}},
		}); err != nil {
			t.Fatal(err)
		}
		deleted, err := idx.Delete(ctx(t), []string{"todelete", "never-existed"})
		if err != nil || deleted != 1 {
			t.Fatalf("deleted=%d err=%v", deleted, err)
		}
	})
}
