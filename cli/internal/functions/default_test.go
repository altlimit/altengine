package functions

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

// The default function answers the instance root and every path no function is named for, with
// the full path, so one function can serve a server-rendered site.
func TestDefaultFunctionCatchesUnnamedPaths(t *testing.T) {
	mux, reg, _ := newRetentionServer(t)
	deployFn(t, mux, "web", `export default { fetch(r) {
		const u = new URL(r.url);
		return new Response("<h1>" + u.pathname + u.search + "</h1>", { headers: { "content-type": "text/html; charset=utf-8" } });
	} };`, nil)
	deployFn(t, mux, "hello", `export default { fetch() { return new Response("hello"); } };`, nil)
	in := reg.Get("functions", "main")

	// Without a default, the root and an unknown name 404.
	if rec := invoke(t, mux, "/fn/main/"); rec.Code != 404 {
		t.Fatalf("root without a default: %d", rec.Code)
	}
	if rec := invoke(t, mux, "/fn/main/about"); rec.Code != 404 {
		t.Fatalf("unknown path without a default: %d", rec.Code)
	}

	if err := reg.SaveConfig(in, map[string]any{"defaultFunction": "web"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/fn/main":                    "<h1>/fn/main</h1>",
		"/fn/main/":                   "<h1>/fn/main/</h1>",
		"/fn/main/blog/2026/post?x=1": "<h1>/fn/main/blog/2026/post?x=1</h1>",
		"/fn/main/assets/app.css":     "<h1>/fn/main/assets/app.css</h1>",
	} {
		rec := invoke(t, mux, path)
		if rec.Code != 200 || rec.Body.String() != want {
			t.Fatalf("%s: %d %q, want %q", path, rec.Code, rec.Body.String(), want)
		}
		if ct := rec.Header().Get("content-type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("%s: content-type %q", path, ct)
		}
	}
	// A named function still wins.
	if rec := invoke(t, mux, "/fn/main/hello/x"); rec.Body.String() != "hello" {
		t.Fatalf("named function: %q", rec.Body.String())
	}
	// The hosted console's own prefixes never reach it.
	for _, path := range []string{"/fn/main/api/auth/sign-in/email", "/fn/main/admin/me", "/fn/main/v1/x"} {
		if rec := invoke(t, mux, path); rec.Code != 404 {
			t.Fatalf("%s reached the default: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestDefaultFunctionMustBeDeployed(t *testing.T) {
	mux, reg, _ := newRetentionServer(t)
	deployFn(t, mux, "web", `export default { fetch() { return new Response("ok"); } };`, nil)
	in := reg.Get("functions", "main")
	err := reg.SaveConfig(in, map[string]any{"defaultFunction": "nope"})
	if err == nil || !strings.Contains(err.Error(), "not a deployed function") {
		t.Fatalf("an undeployed default was accepted: %v", err)
	}
	if err := reg.SaveConfig(in, map[string]any{"defaultFunction": 7}); err == nil {
		t.Fatal("a non-string default was accepted")
	}
}

func TestDeletingTheDefaultFunctionClearsIt(t *testing.T) {
	mux, reg, _ := newRetentionServer(t)
	deployFn(t, mux, "web", `export default { fetch() { return new Response("ok"); } };`, nil)
	in := reg.Get("functions", "main")
	if err := reg.SaveConfig(in, map[string]any{"defaultFunction": "web"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/v1/functions/main/web", nil)
	req.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if got := DefaultFunction(reg.ConfigSnapshot(in)); got != "" {
		t.Fatalf("default survived its function: %q", got)
	}
	// A later deploy of the same name does not bring it back.
	deployFn(t, mux, "web", `export default { fetch() { return new Response("ok"); } };`, nil)
	if rec := invoke(t, mux, "/fn/main/"); rec.Code != 404 {
		t.Fatalf("root after redeploy: %d", rec.Code)
	}
}

// A platform trigger names its function exactly: a payload for a function that is gone must
// 404, not reach the default.
func TestTriggerDoesNotFallBackToTheDefault(t *testing.T) {
	mux, reg, _ := newRetentionServer(t)
	deployFn(t, mux, "web", `export default { fetch(r) { return new Response(r.headers.get("x-ae-trigger")); } };`, nil)
	if err := reg.SaveConfig(reg.Get("functions", "main"), map[string]any{"defaultFunction": "web"}); err != nil {
		t.Fatal(err)
	}
	asTrigger := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, common.WithTrigger(httptest.NewRequest(http.MethodPost, path, nil), "container"))
		return rec
	}
	if rec := asTrigger("/fn/main/deleted"); rec.Code != 404 {
		t.Fatalf("trigger fell back to the default: %d %s", rec.Code, rec.Body.String())
	}
	// Addressed exactly, it runs and is told what invoked it, as hosted.
	if rec := asTrigger("/fn/main/web"); rec.Body.String() != "container" {
		t.Fatalf("trigger header: %q", rec.Body.String())
	}
}
