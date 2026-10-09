package datastore

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// A rule filter evaluated in Go (writes, point reads) must admit exactly the rows the same
// filter admits as SQL (queries): SQLite's ordering — booleans as 1/0, every number below
// every string, text in byte order, and no reading into arrays.
func TestRuleFilterComparesLikeSQLite(t *testing.T) {
	s := openTest(t)
	stored := []string{`9`, `10`, `-1`, `0`, `1`, `2.5`, `"10"`, `"9"`, `"abc"`, `"Abc"`, `"é"`, `"😀"`, `"ｚ"`,
		`""`, `true`, `false`, `null`, `[1,2]`, `{"a":1}`}
	var docs []string
	for i, v := range stored {
		docs = append(docs, fmt.Sprintf("d%02d", i), `{"v":`+v+`,"tags":[1,2,3]}`)
	}
	docs = append(docs, "missing", `{"other":1}`)
	mustPut(t, s, "c", docs...)

	operands := []any{float64(9), float64(10), float64(0), float64(1), "10", "9", "abc", "é", "😀", "ｚ", "", true, false}
	var mismatches []string
	check := func(f Filter) {
		res, err := s.Query("c", QueryRequest{Where: []Filter{f}, Limit: 100}, true, nil)
		if err != nil {
			t.Fatalf("%v: %v", f, err)
		}
		var viaSQL []string
		for _, d := range res.Documents {
			viaSQL = append(viaSQL, d.Key)
		}
		var viaGo []string
		for i := 0; i < len(docs); i += 2 {
			data, _ := decodeFaithful(json.RawMessage(docs[i+1]))
			if matchDoc(data, docs[i], 0, 0, []Filter{f}) {
				viaGo = append(viaGo, docs[i])
			}
		}
		slices.Sort(viaSQL)
		slices.Sort(viaGo)
		if !slices.Equal(viaSQL, viaGo) {
			mismatches = append(mismatches, fmt.Sprintf("v %s %v: sql %v, go %v", f.Op, f.Value, viaSQL, viaGo))
		}
	}
	for _, op := range []string{"=", "<", "<=", ">", ">="} {
		for _, v := range operands {
			check(Filter{Field: "v", Op: op, Value: v})
		}
	}
	check(Filter{Field: "v", Op: "in", Value: []any{float64(1), "abc", true}})
	// A path never reads into an array: tags.length is NULL, not a count.
	check(Filter{Field: "tags.length", Op: ">", Value: float64(0)})
	if len(mismatches) > 0 {
		t.Fatalf("%d mismatches, e.g.\n%s", len(mismatches), mismatches[0])
	}
}

// SQL converts a filter value to a meta column's affinity (`__key__ > 5` compares as text);
// the in-Go matcher would order a number below every string and grant what a query refuses,
// so a meta selector compared with a wrong-typed value denies, for every op.
func TestRuleFilterMetaSelectorWrongTypeDenies(t *testing.T) {
	doc := map[string]any{"v": float64(1)}
	match := func(f Filter) bool { return matchDoc(doc, "p1", 100, 200, []Filter{f}) }
	for _, op := range []string{"=", "!=", "<", "<=", ">", ">="} {
		for _, v := range []any{float64(5), true, nil, []any{"p1"}, map[string]any{"a": float64(1)}} {
			if match(Filter{Field: "__key__", Op: op, Value: v}) {
				t.Errorf("__key__ %s %v matched", op, v)
			}
		}
		for _, field := range []string{"__created__", "__updated__"} {
			for _, v := range []any{"100", "", true, nil, []any{float64(100)}} {
				if match(Filter{Field: field, Op: op, Value: v}) {
					t.Errorf("%s %s %v matched", field, op, v)
				}
			}
		}
	}
	// `in`: one wrong-typed element refuses the whole filter, even beside a matching one.
	if match(Filter{Field: "__key__", Op: "in", Value: []any{"p1", float64(5)}}) {
		t.Error("__key__ in [p1, 5] matched")
	}
	if match(Filter{Field: "__created__", Op: "in", Value: []any{float64(100), "100"}}) {
		t.Error(`__created__ in [100, "100"] matched`)
	}
	// The right types still compare.
	for _, f := range []Filter{
		{Field: "__key__", Op: "in", Value: []any{"p1", "p2"}},
		{Field: "__key__", Op: ">", Value: "p0"},
		{Field: "__created__", Op: "=", Value: float64(100)},
		{Field: "__updated__", Op: ">", Value: float64(150)},
		{Field: "__created__", Op: "in", Value: []any{float64(1), float64(100)}},
		{Field: "__created__", Op: "=", Value: json.Number("100")},
	} {
		if !match(f) {
			t.Errorf("%v did not match", f)
		}
	}
}
