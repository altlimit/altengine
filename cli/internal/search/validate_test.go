package search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
)

// A malformed search body is a 400 INVALID_ARGUMENT naming what is wrong, as hosted.
func TestMalformedSearchBodyIs400(t *testing.T) {
	reg, _ := control.New("")
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewManager("")).Register(mux)
	put := httptest.NewRequest("POST", "/v1/search/main/ns/_default/idx/films/documents",
		strings.NewReader(`{"documents":[{"id":"g1","fields":[{"name":"loc","type":"geo","value":{"lat":1,"lng":1}}]}]}`))
	put.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, put)
	if rec.Code != 200 {
		t.Fatalf("put -> %d %s", rec.Code, rec.Body.String())
	}
	cases := map[string]string{
		`null`:                         "search body must be a JSON object",
		`{"query":5}`:                  "query must be a string",
		`{"cursor":7}`:                 "invalid cursor",
		`{"facets":"x"}`:               "facets must be an array of strings",
		`{"returned_fields":[1]}`:      "returned_fields must be an array of strings",
		`{"sort":{}}`:                  "sort must be an array",
		`{"sort":[null]}`:              "each sort entry must be an object with a string 'expr'",
		`{"facet_refinements":[null]}`: "each facet refinement must be an object with a string 'name'",
		`{"facet_refinements":[{"name":"a","value":1}]}`:                                "facet refinement 'value' must be a string",
		`{"facet_refinements":[{"name":"a","min":"x"}]}`:                                "facet refinement 'min' must be a finite number",
		`{"query":"distance(loc, geopoint(91, 0)) < 5"}`:                                "latitude must be within ±90",
		`{"query":"distance(loc, geopoint(0, 0)) < 1` + strings.Repeat("9", 400) + `"}`: "number out of range",
	}
	for body, want := range cases {
		req := httptest.NewRequest("POST", "/v1/search/main/ns/_default/idx/films/search", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 400 || out.Error.Code != "INVALID_ARGUMENT" || !strings.Contains(out.Error.Message, want) {
			t.Errorf("%s -> %d %s, want 400 %q", body, rec.Code, rec.Body.String(), want)
		}
	}
}
