package functions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/identity"
)

// env.auth.verifyToken is how a function authenticates its OWN caller — the documented
// reason the binding exists, since a function URL is public. It dispatches into
// POST /v1/auth/{instance}/token/verify, and that route did not exist: the request fell
// through to the console's catch-all, came back as HTML, and the binding reported
// "unreadable response". Every function that checks who is calling was unrunnable
// locally, and no test noticed because nothing exercised this binding end to end.
func newAuthServer(t *testing.T) *http.ServeMux {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	idMgr := identity.NewManager("")
	identity.NewHandler(reg, identity.NewService(reg, idMgr, true)).Register(mux)
	NewHandler(reg, a, NewStore(""), mux).Register(mux)
	return mux
}

// signUpUser creates an end user on the `users` auth instance and returns its id_token.
func signUpUser(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"email": "person@example.test", "password": "correct-horse-battery-staple"})
	req := httptest.NewRequest("POST", "/v1/auth/users/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("signup failed: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.IDToken == "" {
		t.Fatalf("signup returned no id_token: %s", rec.Body.String())
	}
	return out.IDToken
}

func TestAuthVerifyTokenThroughStub(t *testing.T) {
	mux := newAuthServer(t)
	token := signUpUser(t, mux)

	deployFn(t, mux, "who", `export default { async fetch(request, env) {
		const bearer = (request.headers.get("authorization") || "").replace(/^Bearer /, "");
		const identity = await env.auth.verifyToken({ instance: "users" }, bearer);
		return Response.json({ identity });
	} };`, map[string]string{"auth": "read"})

	rec := invoke(t, mux, "/fn/main/who", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Identity == nil {
		t.Fatalf("a valid token did not verify: %s", rec.Body.String())
	}
	if out.Identity["identifier"] != "person@example.test" {
		t.Errorf("identifier = %v, want the signed-up email", out.Identity["identifier"])
	}
	// `uid` as well as `sub`. The token is signed with `sub` (the JWT subject), but every
	// other auth binding takes a `uid` — and the published example destructures `uid`, so
	// answering only `sub` makes the documented usage silently reject every caller.
	sub, _ := out.Identity["sub"].(string)
	if sub == "" {
		t.Errorf("no sub in %v", out.Identity)
	}
	if out.Identity["uid"] != sub {
		t.Errorf("uid = %v, want it to match sub %q", out.Identity["uid"], sub)
	}
}

// A bad token must come back as null, NOT as an error. The hosted stub returns null, and
// user code is written as `if (!identity) return 401` — throwing here would send that code
// down its failure path in the emulator only.
func TestAuthVerifyTokenRejectsQuietly(t *testing.T) {
	mux := newAuthServer(t)
	signUpUser(t, mux) // the instance must exist, or this tests the wrong refusal

	deployFn(t, mux, "bad", `export default { async fetch(request, env) {
		const identity = await env.auth.verifyToken({ instance: "users" }, "not-a-token");
		return Response.json({ verified: identity !== null && identity !== undefined });
	} };`, map[string]string{"auth": "read"})

	rec := invoke(t, mux, "/fn/main/bad")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"verified":false`) {
		t.Fatalf("an invalid token should verify as null, got %s", body)
	}
}
