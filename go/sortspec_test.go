package altengine_test

import (
	"encoding/json"
	"testing"

	altengine "github.com/altlimit/altengine/go"
)

// SortSpec sends a number default for a number/date field and DefaultText as a string
// default for an atom field — both as the wire's single `default` key.
func TestSortSpecDefaultJSON(t *testing.T) {
	n, s := 2.5, "zzz"
	for _, c := range []struct {
		spec altengine.SortSpec
		want string
	}{
		{altengine.SortSpec{Expr: "price"}, `{"expr":"price"}`},
		{altengine.SortSpec{Expr: "price", Desc: true, Default: &n}, `{"expr":"price","desc":true,"default":2.5}`},
		{altengine.SortSpec{Expr: "sku", DefaultText: &s}, `{"expr":"sku","default":"zzz"}`},
		{altengine.SortSpec{Expr: "sku", DefaultText: ptr("")}, `{"expr":"sku","default":""}`},
		{altengine.SortSpec{Expr: "price", Default: ptr(0.0)}, `{"expr":"price","default":0}`},
		{altengine.SortSpec{Expr: "rank", Asc: true}, `{"expr":"rank","desc":false}`},
	} {
		b, err := json.Marshal(c.spec)
		if err != nil || string(b) != c.want {
			t.Errorf("%+v: got %s err=%v, want %s", c.spec, b, err, c.want)
		}
	}
	if _, err := json.Marshal(altengine.SortSpec{Expr: "x", Default: &n, DefaultText: &s}); err == nil {
		t.Error("setting both Default and DefaultText should fail to marshal")
	}
	if _, err := json.Marshal(altengine.SortSpec{Expr: "x", Desc: true, Asc: true}); err == nil {
		t.Error("setting both Desc and Asc should fail to marshal")
	}
}

func ptr[T any](v T) *T { return &v }

// CountOnly is the only way to send `limit: 0` (a zero Limit is omitted).
func TestSearchRequestCountOnlyJSON(t *testing.T) {
	for _, c := range []struct {
		req  altengine.SearchRequest
		want string
	}{
		{altengine.SearchRequest{Query: "q"}, `{"query":"q"}`},
		{altengine.SearchRequest{Query: "q", Limit: 5}, `{"query":"q","limit":5}`},
		{altengine.SearchRequest{Query: "q", CountOnly: true}, `{"query":"q","limit":0}`},
	} {
		b, err := json.Marshal(c.req)
		if err != nil || string(b) != c.want {
			t.Errorf("%+v: got %s err=%v, want %s", c.req, b, err, c.want)
		}
	}
	if _, err := json.Marshal(altengine.SearchRequest{Limit: 5, CountOnly: true}); err == nil {
		t.Error("setting both Limit and CountOnly should fail to marshal")
	}
}
