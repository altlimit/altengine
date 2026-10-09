package datastore

import (
	"strings"
	"testing"
)

// A malformed query or aggregate body is a 400 INVALID_ARGUMENT naming the field, as hosted.
func TestMalformedQueryBodyIs400(t *testing.T) {
	e := newRuleEnv(t)
	cases := map[string]string{
		"/secrets/query|null":                                   "query body must be a JSON object",
		"/secrets/query|[]":                                     "query body must be a JSON object",
		`/secrets/query|{"where":"x"}`:                          "'where' must be an array",
		`/secrets/query|{"where":[null]}`:                       "'where[0]' must be an object",
		`/secrets/query|{"where":[{"field":5,"op":"="}]}`:       "'where[0].field' must be a string",
		`/secrets/query|{"order":{}}`:                           "'order' must be an array",
		`/secrets/query|{"cursor":5}`:                           "'cursor' must be a string",
		`/secrets/query|{"join":[{"as":"a","collection":"b"}]}`: "'join[0].local_field' must be a string",
		"/secrets/aggregate|null":                               "aggregate body must be a JSON object",
		`/secrets/aggregate|{"metrics":[null]}`:                 "'metrics[0]' must be an object",
		`/secrets/aggregate|{"metrics":[{"fn":1}]}`:             "'metrics[0].fn' must be a string",
		`/secrets/aggregate|{"group":[1],"metrics":[]}`:         "'group[0]' must be a string",
	}
	for c, want := range cases {
		path, body, _ := strings.Cut(c, "|")
		status, out := e.call(t, path, body, "devkey")
		errObj, _ := out["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if status != 400 || errObj["code"] != "INVALID_ARGUMENT" || msg != want {
			t.Errorf("%s %s -> %d %v, want 400 %q", path, body, status, out, want)
		}
	}
}
