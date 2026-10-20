package search

import (
	"errors"
	"slices"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

// A comparison, `!=` included, takes a number or a date, as hosted: a text value is a 400
// INVALID_ARGUMENT naming the operator and the field. `!=` matches documents whose
// number/date field differs — a document without the field is not one of them.
func TestComparisonsTakeANumber(t *testing.T) {
	s := openTest(t)
	docs := []Document{
		{ID: "a", Fields: []Field{field("code", "untokenprefix", "abc"), field("n", "number", float64(1))}},
		{ID: "b", Fields: []Field{field("code", "untokenprefix", "abd"), field("n", "number", float64(2))}},
		{ID: "c", Fields: []Field{field("code", "untokenprefix", "xyz")}},
	}
	if _, err := s.Put("cmp", docs); err != nil {
		t.Fatal(err)
	}
	for q, msg := range map[string]string{
		`code != abc`:   "operator '!=' on field 'code' requires a numeric value",
		`code != "abc"`: "operator '!=' on field 'code' requires a numeric value",
		`n != ~2`:       "operator '!=' on field 'n' requires a numeric value",
		`n > abc`:       "operator '>' on field 'n' requires a numeric value",
		`n <= 1e3`:      "operator '<=' on field 'n' requires a numeric value",
	} {
		_, err := s.Search("cmp", SearchRequest{Query: q, IDsOnly: true})
		var ae *common.APIError
		if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "INVALID_ARGUMENT" || ae.Message != msg {
			t.Errorf("%s: err = %#v, want 400 INVALID_ARGUMENT %q", q, err, msg)
		}
	}
	for q, want := range map[string][]string{
		`n != 2`:        {"a"},
		`n != 3`:        {"a", "b"},
		`code != 5`:     nil,
		`n >= 1`:        {"a", "b"},
		`n < 2`:         {"a"},
		`NOT n != 2`:    {"b", "c"},
		`code:ab n!=1`:  {"b"},
		`n > -1 n < 10`: {"a", "b"},
	} {
		res, err := s.Search("cmp", SearchRequest{Query: q, IDsOnly: true})
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
