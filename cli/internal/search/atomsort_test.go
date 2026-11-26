package search

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Sorting by an atom field, as hosted: case-insensitive order, a string default for
// documents without the field, MIN for a multi-valued field, and a 400 for a default the
// field's type can never use. Before this every field sort read num_val, so an atom sort
// ordered every document as its default.
func TestSortByAtom(t *testing.T) {
	s := openTest(t)
	r := func(v int64) *int64 { return &v }
	docs := []Document{
		{ID: "d1", Rank: r(5), Fields: []Field{field("name", "atom", "cherry"), field("n", "number", 1)}},
		{ID: "d2", Rank: r(4), Fields: []Field{field("name", "atom", "Banana"), field("n", "number", 2)}},
		{ID: "d3", Rank: r(3), Fields: []Field{field("name", "atom", "apple"), field("n", "number", 1)}},
		{ID: "d4", Rank: r(2), Fields: []Field{field("name", "atom", "zucchini"), field("name", "atom", "avocado")}},
		{ID: "d5", Rank: r(6), Fields: []Field{field("n", "number", 2)}},
		{ID: "m1", Rank: r(1), Fields: []Field{field("mixed", "atom", "b")}},
		{ID: "m2", Rank: r(0), Fields: []Field{field("mixed", "number", 7)}},
	}
	if _, err := s.Put("as", docs); err != nil {
		t.Fatal(err)
	}
	run := func(q string, sort ...SortSpec) []string {
		t.Helper()
		res, err := s.Search("as", SearchRequest{Query: q, Sort: sort, IDsOnly: true})
		if err != nil {
			t.Fatalf("%q %v: %v", q, sort, err)
		}
		var ids []string
		for _, h := range res.Results {
			ids = append(ids, h.ID)
		}
		return ids
	}
	const body = "NOT mixed:b"
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"asc, missing as empty default", run("", SortSpec{Expr: "name"}), []string{"d5", "m1", "m2", "d3", "d4", "d2", "d1"}},
		{"desc, multi-valued by MIN", run(body, SortSpec{Expr: "name", Desc: ptr(true)}), []string{"d1", "d2", "d4", "d3", "d5", "m2"}},
		{"string default, lowercased", run(body, SortSpec{Expr: "name", Default: "BZZ"}), []string{"d3", "d4", "d2", "d5", "m2", "d1"}},
		{"number then atom", run("n >= 1", SortSpec{Expr: "n", Desc: ptr(true)}, SortSpec{Expr: "name"}), []string{"d5", "d2", "d3", "d1"}},
		{"mixed field: numeric without a default", run("mixed:b OR mixed = 7", SortSpec{Expr: "mixed", Desc: ptr(true)}), []string{"m2", "m1"}},
		{"mixed field: atom with a string default", run("mixed:b OR mixed = 7", SortSpec{Expr: "mixed", Desc: ptr(true), Default: ""}), []string{"m1", "m2"}},
		{"unknown field sorts as its default", run(body, SortSpec{Expr: "nosuch", Default: "q"}), []string{"d1", "d2", "d3", "d4", "d5", "m2"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	for _, c := range []struct {
		sort SortSpec
		msg  string
	}{
		{SortSpec{Expr: "name", Default: float64(0)}, "sort default for atom field 'name' must be a string"},
		{SortSpec{Expr: "n", Default: "a"}, "sort default for number field 'n' must be a number"},
		{SortSpec{Expr: "name", Default: map[string]any{"x": 1}}, "sort default must be a number or a string"},
		{SortSpec{Expr: "name", Default: strings.Repeat("x", 501)}, "sort default exceeds 500 bytes"},
	} {
		_, err := s.Search("as", SearchRequest{Sort: []SortSpec{c.sort}})
		var ae *common.APIError
		if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != c.msg {
			t.Errorf("%v: err = %#v, want 400 %q", c.sort, err, c.msg)
		}
	}
}

// The request shape check rejects a sort default that is neither a number nor a string
// before the engine sees it, with the hosted message.
func TestValidateSortDefault(t *testing.T) {
	for _, d := range []any{map[string]any{}, []any{}, true} {
		err := ValidateSearchRequest(map[string]any{"sort": []any{map[string]any{"expr": "x", "default": d}}})
		var ae *common.APIError
		if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "sort default must be a number or a string" {
			t.Errorf("default %#v: err = %#v", d, err)
		}
	}
	for _, d := range []any{nil, "x", float64(3)} {
		if err := ValidateSearchRequest(map[string]any{"sort": []any{map[string]any{"expr": "x", "default": d}}}); err != nil {
			t.Errorf("default %#v: unexpected %v", d, err)
		}
	}
}
