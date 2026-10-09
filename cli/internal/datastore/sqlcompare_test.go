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
