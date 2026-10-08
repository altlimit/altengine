package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --json puts exactly one JSON document on stdout, for scripts and CI; there was no
// machine-readable output at all.
func TestJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/functions/app"):
			_, _ = w.Write([]byte(`{"instance":"app","functions":[{"name":"hello","active_version":2}]}`))
		case strings.HasSuffix(r.URL.Path, "/site"):
			_, _ = w.Write([]byte(`{"name":"web","url":"https://web.example","deployments":3}`))
		case strings.HasSuffix(r.URL.Path, "/runs"):
			_, _ = w.Write([]byte(`{"runs":[{"id":"r1","status":"done"}],"cursor":"c2"}`))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			_, _ = w.Write([]byte(`{"deployments":[],"active":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	isolate(t)
	t.Setenv("ALTENGINE_URL", srv.URL)
	t.Setenv(keyEnv, "ae_test")

	cases := map[string]func(map[string]any) bool{
		"functions list --instance app --json": func(m map[string]any) bool { return len(m["functions"].([]any)) == 1 },
		"static info --instance web --json":    func(m map[string]any) bool { return m["name"] == "web" },
		"static list --instance web --json":    func(m map[string]any) bool { return m["deployments"] != nil },
		"automation runs --instance f --json":  func(m map[string]any) bool { return m["cursor"] == "c2" },
	}
	for args, check := range cases {
		code, out, errOut := runMain(t, args)
		var m map[string]any
		if code != 0 || json.Unmarshal([]byte(out), &m) != nil || !check(m) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}
