package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

func newRevocationServer(t *testing.T) (string, *control.Registry, *Service) {
	t.Helper()
	reg, _ := control.New("")
	svc := NewService(reg, NewManager(""), true)
	mux := http.NewServeMux()
	NewHandler(reg, svc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/auth/app", reg, svc
}

func signupToken(t *testing.T, base, email string) (string, string) {
	t.Helper()
	status, out := do(t, "POST", base+"/signup", `{"email":"`+email+`","password":"hunter2hunter"}`, "")
	if status != 201 {
		t.Fatalf("signup -> %d: %v", status, out)
	}
	user, _ := out["user"].(map[string]any)
	return out["id_token"].(string), user["uid"].(string)
}

func appStore(t *testing.T, reg *control.Registry, svc *Service) *Store {
	t.Helper()
	s, err := svc.Mgr.Open(reg.GetOrCreate("auth", "app").ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Each identity token carries typ "id", and the verifier refuses any other type — and an
// untyped token that outlives the cutoff for tokens minted before typ existed.
func TestIdentityTokenTyp(t *testing.T) {
	base, reg, _ := newRevocationServer(t)
	tok, _ := signupToken(t, base, "typ@example.com")
	var payload map[string]any
	_ = json.Unmarshal(common.PeekJWTPayload(tok), &payload)
	if payload["typ"] != "id" {
		t.Fatalf("identity token payload = %v, want typ id", payload)
	}
	inst := reg.GetOrCreate("auth", "app")
	sign := func(extra map[string]any) string {
		c := map[string]any{"iss": inst.ID, "sub": "u1", "identifier": "a@example.com", "exp": time.Now().Add(time.Hour).Unix()}
		for k, v := range extra {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		return common.SignJWTPayload(b, inst.Secret)
	}
	for name, tok := range map[string]string{
		"mfa challenge":        sign(map[string]any{"typ": "mfa", "purpose": "mfa"}),
		"ceremony state":       sign(map[string]any{"typ": "wa-auth", "purpose": "wa-auth"}),
		"typ id with purpose":  sign(map[string]any{"typ": "id", "purpose": "mfa"}),
		"untyped, long-lived":  sign(map[string]any{"exp": untypedTokensExpireBy + 1}),
		"untyped with purpose": sign(map[string]any{"purpose": "mfa"}),
	} {
		if VerifyIdentity(tok, inst.Secret) != nil {
			t.Errorf("%s was accepted as an identity token", name)
		}
	}
	if VerifyIdentity(sign(map[string]any{"typ": "id"}), inst.Secret) == nil {
		t.Error("a typed identity token was refused")
	}
	if time.Now().Unix() < untypedTokensExpireBy {
		if VerifyIdentity(sign(map[string]any{"exp": untypedTokensExpireBy}), inst.Secret) == nil {
			t.Error("an untyped token expiring by the cutoff was refused")
		}
	}
}

// Disabling or deleting an account revokes the identity tokens it already holds — on the
// auth plane's own /me, in token verification for functions, and on every data plane — and
// re-enabling does not revive them.
func TestDisableAndDeleteRevokeIdentityTokens(t *testing.T) {
	base, reg, svc := newRevocationServer(t)
	store := appStore(t, reg, svc)
	tok, uid := signupToken(t, base, "dis@example.com")

	if s, _ := do(t, "GET", base+"/me", "", tok); s != 200 {
		t.Fatalf("/me before disable -> %d", s)
	}
	if _, err := store.SetDisabled(uid, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDisabled(uid, false, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if s, out := do(t, "GET", base+"/me", "", tok); s != 401 {
		t.Fatalf("/me with a token from before the disable -> %d %v, want 401", s, out)
	}
	if _, err := svc.ResolveToken(tok); err == nil {
		t.Fatal("the data plane accepted a token from before the disable")
	}
	if s, out := do(t, "POST", base+"/token/verify", `{"token":"`+tok+`"}`, ""); s != 200 || out != nil {
		t.Fatalf("token/verify -> %d %v, want null", s, out)
	}

	// Signing in again mints a token in the new epoch, which works.
	_, si := do(t, "POST", base+"/signin", `{"identifier":"dis@example.com","password":"hunter2hunter"}`, "")
	fresh, _ := si["id_token"].(string)
	if _, err := svc.ResolveToken(fresh); err != nil {
		t.Fatalf("a fresh token was refused: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(common.PeekJWTPayload(fresh), &payload)
	if payload["ce"] != float64(1) {
		t.Fatalf("fresh token ce = %v, want 1", payload["ce"])
	}

	if _, err := store.DeleteUser(uid); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveToken(fresh); err == nil {
		t.Fatal("the data plane accepted a deleted account's token")
	}
}

// A reset revokes the tokens issued before it; a disabled account cannot reset at all, even
// with a code issued before it was disabled.
func TestResetRevokesAndDisabledCannotReset(t *testing.T) {
	base, reg, svc := newRevocationServer(t)
	store := appStore(t, reg, svc)
	tok, uid := signupToken(t, base, "rs@example.com")

	_, start := do(t, "POST", base+"/password/reset/start", `{"identifier":"rs@example.com"}`, "")
	code, _ := start["dev_code"].(string)
	if s, out := do(t, "POST", base+"/password/reset/verify",
		`{"identifier":"rs@example.com","code":"`+code+`","new_password":"brand-new-secret"}`, ""); s != 200 {
		t.Fatalf("reset -> %d %v", s, out)
	}
	if _, err := svc.ResolveToken(tok); err == nil {
		t.Fatal("a token from before the reset still works")
	}

	_, start = do(t, "POST", base+"/password/reset/start", `{"identifier":"rs@example.com"}`, "")
	code, _ = start["dev_code"].(string)
	if _, err := store.SetDisabled(uid, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	s, out := do(t, "POST", base+"/password/reset/verify",
		`{"identifier":"rs@example.com","code":"`+code+`","new_password":"another-secret"}`, "")
	if s != 403 || errCode(out) != "PERMISSION_DENIED" {
		t.Fatalf("disabled reset -> %d %v, want 403", s, out)
	}
	row, _ := store.ByUID(uid)
	if !VerifyPassword("brand-new-secret", row.PwHash) {
		t.Fatal("a refused reset changed the password")
	}
	if changed, _ := store.SetPassword(uid, "x", time.Now().UnixMilli()); changed {
		t.Fatal("SetPassword reset a disabled account")
	}
}

// Turning the verification gate on grandfathers the accounts that predate it — but never
// one that signed up while the gate was on and has not verified.
func TestVerifyPendingIsNotGrandfathered(t *testing.T) {
	base, reg, _ := newRevocationServer(t)
	inst := reg.GetOrCreate("auth", "app")
	setGate := func(on bool) {
		t.Helper()
		if err := reg.ReplaceConfig(inst, map[string]any{"requireEmailVerification": on}); err != nil {
			t.Fatal(err)
		}
	}
	signin := func(email string) (int, string) {
		s, out := do(t, "POST", base+"/signin", `{"identifier":"`+email+`","password":"hunter2hunter"}`, "")
		return s, errCode(out)
	}
	signupToken(t, base, "early@example.com") // before the gate: grandfathered

	setGate(true)
	if s, out := do(t, "POST", base+"/signup", `{"email":"gated@example.com","password":"hunter2hunter"}`, ""); s != 201 {
		t.Fatalf("gated signup -> %d %v", s, out)
	}
	if s, _ := signin("early@example.com"); s != 200 {
		t.Fatalf("a pre-gate account was locked out: %d", s)
	}
	setGate(false)
	if s, _ := signin("gated@example.com"); s != 200 {
		t.Fatalf("gate off: %d", s)
	}
	setGate(true)
	if s, code := signin("gated@example.com"); s != 403 || code != "EMAIL_UNVERIFIED" {
		t.Fatalf("re-enabling the gate let an unverified gated signup in: %d %s", s, code)
	}
	if s, _ := signin("early@example.com"); s != 200 {
		t.Fatalf("grandfathered account locked out after re-enabling: %d", s)
	}
}

// A new password is 8–256 characters; an auth request body is at most 64 KB.
func TestPasswordMaxAndAuthBodyCap(t *testing.T) {
	base, _, _ := newRevocationServer(t)
	long := strings.Repeat("a", 257)
	s, out := do(t, "POST", base+"/signup", `{"email":"long@example.com","password":"`+long+`"}`, "")
	if s != 400 || !strings.Contains(errMessage(out), "at most 256 characters") {
		t.Fatalf("257-char password -> %d %v", s, out)
	}
	if s, out := do(t, "POST", base+"/signup", `{"email":"ok@example.com","password":"`+long[:256]+`"}`, ""); s != 201 {
		t.Fatalf("256-char password -> %d %v", s, out)
	}
	big := `{"email":"big@example.com","password":"hunter2hunter","pad":"` + strings.Repeat("x", 64*1024) + `"}`
	if s, out := do(t, "POST", base+"/signup", big, ""); s != 413 {
		t.Fatalf("oversized body -> %d %v, want 413", s, out)
	}
}
