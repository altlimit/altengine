package identity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/altlimit/altengine/cli/internal/control"
)

func newTestServer(t *testing.T) (*httptest.Server, *control.Registry) {
	t.Helper()
	reg, _ := control.New("")
	svc := NewService(reg, NewManager(""), true)
	mux := http.NewServeMux()
	NewHandler(reg, svc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

// do issues a request and returns the status plus the decoded JSON body.
func do(t *testing.T, method, url, body, bearer string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBufferString("")
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func errMessage(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

func TestSignupSigninRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/v1/auth/app"

	status, out := do(t, "POST", base+"/signup", `{"email":"Alice@Example.com","password":"hunter2hunter"}`, "")
	if status != 201 {
		t.Fatalf("signup -> %d: %v", status, out)
	}
	if out["id_token"] == nil || out["refresh_token"] == nil || out["expires_at"] == nil || out["refresh_expires_at"] == nil {
		t.Fatalf("signup response missing tokens: %v", out)
	}
	user, _ := out["user"].(map[string]any)
	if user["identifier"] != "alice@example.com" {
		t.Fatalf("identifier should be normalized, got %v", user["identifier"])
	}

	// The same handle can't be claimed twice.
	status, out = do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"another-one"}`, "")
	if status != 409 || errCode(out) != "ALREADY_EXISTS" {
		t.Fatalf("duplicate signup -> %d %v", status, out)
	}

	// Sign in with the same credentials.
	status, signin := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"hunter2hunter"}`, "")
	if status != 200 {
		t.Fatalf("signin -> %d: %v", status, signin)
	}
	if signin["id_token"] == nil {
		t.Fatalf("signin returned no id_token: %v", signin)
	}

	// /me resolves the token back to the account.
	status, me := do(t, "GET", base+"/me", "", signin["id_token"].(string))
	if status != 200 {
		t.Fatalf("me -> %d: %v", status, me)
	}
	meUser, _ := me["user"].(map[string]any)
	if meUser["identifier"] != "alice@example.com" {
		t.Fatalf("me returned %v", me)
	}

	// /me without a token is unauthenticated.
	if status, _ := do(t, "GET", base+"/me", "", ""); status != 401 {
		t.Fatalf("me without token -> %d", status)
	}
}

func TestSigninDoesNotEnumerateAccounts(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	if status, out := do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, ""); status != 201 {
		t.Fatalf("signup -> %d %v", status, out)
	}

	wrongStatus, wrong := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"not-the-password"}`, "")
	missingStatus, missing := do(t, "POST", base+"/signin", `{"identifier":"nobody@example.com","password":"not-the-password"}`, "")
	if wrongStatus != 401 || missingStatus != 401 {
		t.Fatalf("expected 401/401, got %d/%d", wrongStatus, missingStatus)
	}
	if errMessage(wrong) != errMessage(missing) || errCode(wrong) != errCode(missing) {
		t.Fatalf("wrong password and unknown user must be indistinguishable: %q vs %q", errMessage(wrong), errMessage(missing))
	}
}

func TestRefreshRotatesAndSignoutRevokes(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	_, out := do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, "")
	first := out["refresh_token"].(string)

	status, rot := do(t, "POST", base+"/token/refresh", `{"refresh_token":"`+first+`"}`, "")
	if status != 200 {
		t.Fatalf("refresh -> %d: %v", status, rot)
	}
	second, _ := rot["refresh_token"].(string)
	if second == "" || second == first {
		t.Fatalf("refresh must rotate the token, got %q", second)
	}
	if rot["id_token"] == nil {
		t.Fatalf("refresh returned no id_token: %v", rot)
	}
	// The consumed token is dead.
	if status, _ := do(t, "POST", base+"/token/refresh", `{"refresh_token":"`+first+`"}`, ""); status != 401 {
		t.Fatalf("reusing a rotated refresh token -> %d", status)
	}
	// Sign out revokes the live one.
	if status, _ := do(t, "POST", base+"/signout", `{"refresh_token":"`+second+`"}`, ""); status != 200 {
		t.Fatalf("signout -> %d", status)
	}
	if status, _ := do(t, "POST", base+"/token/refresh", `{"refresh_token":"`+second+`"}`, ""); status != 401 {
		t.Fatalf("refresh after signout -> %d", status)
	}
}

func TestPasswordlessCodeFlow(t *testing.T) {
	srv, reg := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	inst := reg.GetOrCreate("auth", "app")
	reg.SetConfig(inst, map[string]any{"passwordlessEnabled": true})

	if status, out := do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, ""); status != 201 {
		t.Fatalf("signup -> %d %v", status, out)
	}

	status, start := do(t, "POST", base+"/passwordless/start", `{"identifier":"alice@example.com"}`, "")
	if status != 200 {
		t.Fatalf("passwordless start -> %d: %v", status, start)
	}
	code, _ := start["dev_code"].(string)
	if code == "" {
		t.Fatalf("dev-open start should echo the code, got %v", start)
	}

	// A wrong code is refused...
	if status, _ := do(t, "POST", base+"/passwordless/verify", `{"identifier":"alice@example.com","code":"000000x"}`, ""); status != 401 {
		t.Fatalf("bad code -> %d", status)
	}
	// ...and the right one signs in.
	status, verify := do(t, "POST", base+"/passwordless/verify", `{"identifier":"alice@example.com","code":"`+code+`"}`, "")
	if status != 200 || verify["id_token"] == nil {
		t.Fatalf("passwordless verify -> %d: %v", status, verify)
	}
	// The code is single-use.
	if status, _ := do(t, "POST", base+"/passwordless/verify", `{"identifier":"alice@example.com","code":"`+code+`"}`, ""); status != 401 {
		t.Fatalf("code reuse -> %d", status)
	}

	// An unknown account still answers 200 (no enumeration) and issues nothing.
	status, unknown := do(t, "POST", base+"/passwordless/start", `{"identifier":"nobody@example.com"}`, "")
	if status != 200 {
		t.Fatalf("start for unknown account -> %d", status)
	}
	if unknown["dev_code"] != nil {
		t.Fatalf("no code should be issued for an unknown account: %v", unknown)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, "")

	_, start := do(t, "POST", base+"/password/reset/start", `{"identifier":"alice@example.com"}`, "")
	code, _ := start["dev_code"].(string)
	if code == "" {
		t.Fatalf("reset start should echo the code in dev-open mode: %v", start)
	}
	status, out := do(t, "POST", base+"/password/reset/verify",
		`{"identifier":"alice@example.com","code":"`+code+`","new_password":"brand-new-secret"}`, "")
	if status != 200 {
		t.Fatalf("reset verify -> %d: %v", status, out)
	}
	if status, _ := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"hunter2hunter"}`, ""); status != 401 {
		t.Fatalf("old password should no longer work, got %d", status)
	}
	if status, _ := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"brand-new-secret"}`, ""); status != 200 {
		t.Fatalf("new password should work, got %d", status)
	}
}

func TestConfiguredSignupFormBecomesProfile(t *testing.T) {
	srv, reg := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	inst := reg.GetOrCreate("auth", "app")
	reg.SetConfig(inst, map[string]any{"signup": map[string]any{
		"identityField": "username",
		"fields": []any{
			map[string]any{"key": "username", "type": "string", "required": true},
			map[string]any{"key": "name", "type": "string", "required": true},
			map[string]any{"key": "age", "type": "number", "required": false},
		},
	}})

	status, cfg := do(t, "GET", base+"/config", "", "")
	if status != 200 {
		t.Fatalf("config -> %d", status)
	}
	if cfg["identity_field"] != "username" || cfg["allow_signup"] != true {
		t.Fatalf("unexpected config: %v", cfg)
	}
	if fields, _ := cfg["fields"].([]any); len(fields) != 3 {
		t.Fatalf("expected 3 form fields, got %v", cfg["fields"])
	}

	// A missing required field is refused.
	if status, _ := do(t, "POST", base+"/signup", `{"username":"alice","password":"hunter2hunter"}`, ""); status != 400 {
		t.Fatalf("missing required field -> %d", status)
	}
	status, out := do(t, "POST", base+"/signup", `{"username":"Alice","name":"Alice A","age":31,"password":"hunter2hunter"}`, "")
	if status != 201 {
		t.Fatalf("signup -> %d: %v", status, out)
	}
	user, _ := out["user"].(map[string]any)
	profile, _ := user["profile"].(map[string]any)
	if user["identifier"] != "alice" || profile["name"] != "Alice A" || profile["age"].(float64) != 31 {
		t.Fatalf("identity field and profile wrong: %v", user)
	}
	// Signup writes ONLY the self-asserted profile — the authoritative claims bag stays empty
	// (a user can never write it), which is the whole point of the split.
	if claims, _ := user["claims"].(map[string]any); len(claims) != 0 {
		t.Fatalf("signup must not populate authoritative claims, got %v", claims)
	}

	// The non-identity fields ride on the token in the profile bag, not claims.
	tokenClaims := VerifyIdentity(out["id_token"].(string), reg.GetOrCreate("auth", "app").Secret)
	if tokenClaims == nil || tokenClaims.Identifier != "alice" || tokenClaims.Profile["name"] != "Alice A" {
		t.Fatalf("token profile wrong: %+v", tokenClaims)
	}
	if len(tokenClaims.Claims) != 0 {
		t.Fatalf("token must not carry authoritative claims from signup, got %v", tokenClaims.Claims)
	}
	// A username-typed instance carries no email on the token.
	if tokenClaims.Email != "" {
		t.Fatalf("expected no email claim, got %q", tokenClaims.Email)
	}
}

// The email-verification gate (requireEmailVerification): signup issues no session, a correct
// password is refused EMAIL_UNVERIFIED, and confirming the emailed code lifts the gate. The
// emulator echoes the code as dev_code (dev-open), so the whole flow is exercisable locally.
func TestEmailVerifyGate(t *testing.T) {
	srv, reg := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	reg.SetConfig(reg.GetOrCreate("auth", "app"), map[string]any{"requireEmailVerification": true})

	// Signup creates the account UNVERIFIED — no session, just verification_required + the dev code.
	status, out := do(t, "POST", base+"/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, "")
	if status != 201 || out["verification_required"] != true {
		t.Fatalf("signup -> %d: %v", status, out)
	}
	if out["id_token"] != nil {
		t.Fatalf("no session should be issued before verification: %v", out)
	}
	user, _ := out["user"].(map[string]any)
	if user["emailVerified"] != false {
		t.Fatalf("new account should be unverified: %v", user)
	}
	code, _ := out["dev_code"].(string)
	if code == "" {
		t.Fatalf("dev_code expected in dev-open mode: %v", out)
	}

	// A correct password is refused with a DISTINCT code until verified.
	status, si := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"hunter2hunter"}`, "")
	if status != 403 || errCode(si) != "EMAIL_UNVERIFIED" {
		t.Fatalf("unverified signin -> %d %v (want 403 EMAIL_UNVERIFIED)", status, si)
	}

	// A wrong code is refused; the right one confirms and returns a session.
	if s, _ := do(t, "POST", base+"/email/verify/confirm", `{"identifier":"alice@example.com","code":"000000x"}`, ""); s != 401 {
		t.Fatalf("wrong verify code -> %d", s)
	}
	status, vc := do(t, "POST", base+"/email/verify/confirm", `{"identifier":"alice@example.com","code":"`+code+`"}`, "")
	if status != 200 || vc["id_token"] == nil {
		t.Fatalf("verify confirm -> %d: %v", status, vc)
	}

	// Now the same password signs in.
	status, ok := do(t, "POST", base+"/signin", `{"identifier":"alice@example.com","password":"hunter2hunter"}`, "")
	if status != 200 || ok["id_token"] == nil {
		t.Fatalf("signin after verification -> %d: %v", status, ok)
	}
}

func TestDeferredMethodsReportUnimplemented(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/v1/auth/app"
	for _, path := range []string{"/2fa/totp/start", "/2fa/verify", "/passkey/login/start"} {
		status, out := do(t, "POST", base+path, `{}`, "")
		if status != 501 || errCode(out) != "UNIMPLEMENTED" {
			t.Fatalf("%s -> %d %v (want 501 UNIMPLEMENTED)", path, status, out)
		}
	}
}

func TestSignupDisabled(t *testing.T) {
	srv, reg := newTestServer(t)
	inst := reg.GetOrCreate("auth", "app")
	reg.SetConfig(inst, map[string]any{"allowSignup": false})
	status, out := do(t, "POST", srv.URL+"/v1/auth/app/signup", `{"email":"a@b.co","password":"hunter2hunter"}`, "")
	if status != 403 || errCode(out) != "PERMISSION_DENIED" {
		t.Fatalf("signup with allowSignup=false -> %d %v", status, out)
	}
}

func TestTokenFromAnotherInstanceIsRejected(t *testing.T) {
	srv, reg := newTestServer(t)
	do(t, "POST", srv.URL+"/v1/auth/app/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, "")
	_, out := do(t, "POST", srv.URL+"/v1/auth/other/signup", `{"email":"alice@example.com","password":"hunter2hunter"}`, "")
	otherToken := out["id_token"].(string)

	// The "other" instance's token must not authenticate against "app".
	if status, _ := do(t, "GET", srv.URL+"/v1/auth/app/me", "", otherToken); status != 401 {
		t.Fatalf("cross-instance token -> %d", status)
	}
	// ...and each instance really does have its own signing secret.
	if reg.GetOrCreate("auth", "app").Secret == reg.GetOrCreate("auth", "other").Secret {
		t.Fatal("auth instances must have distinct secrets")
	}
}

// Adding a sign-up field does not backfill users who registered before it existed, and a
// rule that stamps a missing claim is a hard deny — so those accounts can only be rescued
// by editing their claims. This is the path the local console uses.
func TestSetClaimsBackfillsAnOlderAccount(t *testing.T) {
	m := NewManager("")
	s, err := m.Open("inst-" + t.Name())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A user created while the form collected only an identifier has no claims.
	u, err := s.CreateUser("old@example.com", "pw", nil, 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := s.ByUID(u.UID); len(got.Claims) != 0 {
		t.Fatalf("expected no claims, got %v", got.Claims)
	}

	ok, err := s.SetClaims(u.UID, map[string]any{"name": "Old User"}, 2)
	if err != nil || !ok {
		t.Fatalf("SetClaims -> %v %v", ok, err)
	}
	got, _ := s.ByUID(u.UID)
	if got.Claims["name"] != "Old User" {
		t.Fatalf("claims not backfilled: %v", got.Claims)
	}
	if got.Updated != 2 {
		t.Fatalf("updated not bumped: %d", got.Updated)
	}

	// A missing user reports false rather than silently succeeding.
	if ok, _ := s.SetClaims("no-such-uid", map[string]any{"a": 1}, 3); ok {
		t.Fatal("SetClaims on a missing user should report false")
	}
}

func TestMergeClaimsAllUsersBackfill(t *testing.T) {
	m := NewManager("")
	s, err := m.Open("inst-" + t.Name())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a, _ := s.CreateUser("a@example.com", "pw", nil, 1)
	b, _ := s.CreateUser("b@example.com", "pw", nil, 1)
	if _, err := s.SetClaims(a.UID, map[string]any{"role": "admin", "team": "x"}, 1); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := s.SetClaims(b.UID, map[string]any{"team": "y"}, 1); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	// only_missing=true: fill `role` where absent, but DON'T clobber a's existing admin.
	n, err := s.MergeClaimsAllUsers(map[string]any{"role": "member"}, 2, true)
	if err != nil || n != 2 {
		t.Fatalf("fill-gaps -> %d, %v (want 2)", n, err)
	}
	ga, _ := s.ByUID(a.UID)
	gb, _ := s.ByUID(b.UID)
	if ga.Claims["role"] != "admin" || gb.Claims["role"] != "member" {
		t.Fatalf("fill-gaps wrong: a=%v b=%v", ga.Claims, gb.Claims)
	}

	// only_missing=false: force role=member on everyone (patch wins).
	if _, err := s.MergeClaimsAllUsers(map[string]any{"role": "member"}, 3, false); err != nil {
		t.Fatalf("override: %v", err)
	}
	if ga, _ = s.ByUID(a.UID); ga.Claims["role"] != "member" {
		t.Fatalf("override should have set a.role=member: %v", ga.Claims)
	}

	// A null value deletes that claim across all users (RFC 7386 merge-patch).
	if _, err := s.MergeClaimsAllUsers(map[string]any{"team": nil}, 4, false); err != nil {
		t.Fatalf("null-delete: %v", err)
	}
	ga, _ = s.ByUID(a.UID)
	gb, _ = s.ByUID(b.UID)
	if _, ok := ga.Claims["team"]; ok {
		t.Fatalf("null should have deleted a.team: %v", ga.Claims)
	}
	if _, ok := gb.Claims["team"]; ok {
		t.Fatalf("null should have deleted b.team: %v", gb.Claims)
	}
}

// The profile/claims split is a SECURITY BOUNDARY: `profile` is self-asserted (a user
// chooses their own signup values), `claims` is admin-set and authoritative. A rule that
// authorizes on `$auth.claims.role` must therefore NEVER be satisfiable by a profile value —
// otherwise a user could grant themselves `role:"admin"` at signup. This proves it: with the
// role only in the profile bag, a `$auth.claims.role` lookup denies while `$auth.profile.role`
// resolves; once an admin sets it authoritatively, `$auth.claims.role` resolves too.
func TestProfileClaimsSecurityBoundary(t *testing.T) {
	// A user who self-asserted role:"admin" at signup — it lands in profile, never claims.
	u := &EndUser{UID: "u1", Profile: map[string]any{"role": "admin"}, Claims: map[string]any{}}

	if _, err := Substitute("$auth.claims.role", u); err == nil {
		t.Fatal("a self-asserted profile value must NOT satisfy an authoritative $auth.claims.role lookup")
	}
	got, err := Substitute("$auth.profile.role", u)
	if err != nil {
		t.Fatalf("$auth.profile.role should resolve the self-asserted value: %v", err)
	}
	if got != "admin" {
		t.Fatalf("$auth.profile.role = %v, want \"admin\"", got)
	}

	// The channel-template path enforces the same boundary.
	if _, err := SubstituteTemplate("room.$auth.claims.role", u); err == nil {
		t.Fatal("channel template must not resolve $auth.claims.role from a profile value")
	}
	if s, err := SubstituteTemplate("room.$auth.profile.role", u); err != nil || s != "room.admin" {
		t.Fatalf("channel template $auth.profile.role -> %q, %v (want \"room.admin\")", s, err)
	}

	// Once an admin sets the claim authoritatively, the claims lookup resolves.
	u.Claims = map[string]any{"role": "moderator"}
	if got, err := Substitute("$auth.claims.role", u); err != nil || got != "moderator" {
		t.Fatalf("admin-set $auth.claims.role -> %v, %v (want \"moderator\")", got, err)
	}
}
