package functions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/admin"
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

// The user-administration half of env.auth — find a user, read one, set their claims — is
// what a function needs to grant access to something. All three dispatched to routes that
// did not answer them: listUsers named the instance where the admin API wanted its id,
// getUser had no route at all, and setClaims sent POST to a PUT route. Each fell through to
// the console's catch-all and came back as HTML. Hosted, the same calls work, so an app that
// shares things between users could be written but not run locally.
func TestAuthUserAdminThroughStub(t *testing.T) {
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	idMgr := identity.NewManager("")
	identity.NewHandler(reg, identity.NewService(reg, idMgr, true)).Register(mux)
	admin.NewHandler(reg, a, nil, nil, nil).WithIdentity(idMgr).Register(mux)
	NewHandler(reg, a, NewStore(""), mux).Register(mux)

	signUpUser(t, mux)

	deployFn(t, mux, "share", `export default { async fetch(request, env) {
		const target = { instance: "users" };
		const page = await env.auth.listUsers(target, { q: "PERSON@example", limit: 10 });
		const found = (page.users || []).find((u) => u.identifier === "person@example.test");
		const before = await env.auth.getUser(target, found.uid);
		await env.auth.setClaims(target, found.uid, { boards: ["payments"] });
		const after = await env.auth.getUser(target, found.uid);
		const none = await env.auth.listUsers(target, { q: "nobody-by-this-name" });
		return Response.json({ uid: found.uid, before: before.claims, after: after.claims, none: none.users.length });
	} };`, map[string]string{"auth": "write"})

	rec := invoke(t, mux, "/fn/main/share")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		UID    string         `json:"uid"`
		Before map[string]any `json:"before"`
		After  map[string]any `json:"after"`
		None   int            `json:"none"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	if out.UID == "" {
		t.Fatalf("listUsers did not find the user by a case-insensitive substring: %s", rec.Body.String())
	}
	if len(out.Before) != 0 {
		t.Errorf("claims before = %v, want none", out.Before)
	}
	boards, _ := out.After["boards"].([]any)
	if len(boards) != 1 || boards[0] != "payments" {
		t.Errorf("claims after setClaims = %v, want boards [payments]", out.After)
	}
	// q is a filter, not a hint. The console's browser ignored it and returned everyone,
	// which a function looking a person up by email would read as a match.
	if out.None != 0 {
		t.Errorf("listUsers with a q that matches nobody returned %d users", out.None)
	}
}
