package functions

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

// The subRequests cap is validated on deploy and enforced per invocation, as hosted. It was
// stored and ignored, so a function that fanned out past its cap worked here and failed there.
func TestSubRequestsCap(t *testing.T) {
	mux := newTestServer(t)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/functions/main/deploy", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer dev")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	code := `export default { async fetch() {
		try { await fetch("https://example.com/"); return new Response("fetched"); }
		catch (e) { return new Response(String(e && e.message || e), { status: 500 }); }
	} };`
	quoted := strings.ReplaceAll(strings.ReplaceAll(code, `\`, `\\`), `"`, `\"`)
	quoted = strings.ReplaceAll(strings.ReplaceAll(quoted, "\n", `\n`), "\t", `\t`)

	for _, bad := range []string{`"subRequests": 1001`, `"subRequests": -1`, `"cpuMs": 0`, `"cpuMs": 30001`} {
		if rec := post(`{"name":"f","code":"` + quoted + `",` + bad + `}`); rec.Code != 400 {
			t.Errorf("%s: deploy = %d %s, want 400", bad, rec.Code, rec.Body.String())
		}
	}

	if rec := post(`{"name":"f","code":"` + quoted + `","subRequests":0}`); rec.Code != 201 {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	rec := invoke(t, mux, "/fn/main/f")
	if !strings.Contains(rec.Body.String(), "Too many subrequests") {
		t.Fatalf("a function allowed 0 subrequests fetched: %d %s", rec.Code, rec.Body.String())
	}

	// A redeploy that omits the field keeps it; it is not reset to the default.
	if rec := post(`{"name":"f","code":"` + quoted + `"}`); rec.Code != 201 {
		t.Fatalf("redeploy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := invoke(t, mux, "/fn/main/f"); !strings.Contains(rec.Body.String(), "Too many subrequests") {
		t.Fatalf("an omitted subRequests reset the cap: %s", rec.Body.String())
	}
}
