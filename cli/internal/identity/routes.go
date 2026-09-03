package identity

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// The /v1/auth/{instance} data plane — the tenant's end-user identity provider. These are
// PUBLIC, browser-facing endpoints (no org API key): a browser signs up or signs in and
// receives an identity token it then presents to the other data planes.
//
//	GET  /config             -> the client-safe instance config (form + enabled methods)
//	POST /signup             -> { id_token, refresh_token, user }   (201)
//	POST /signin             -> { id_token, refresh_token, user }
//	POST /token/refresh      -> { id_token, refresh_token }         (rotates)
//	POST /signout            -> { ok: true }                        (revokes)
//	GET  /me                 -> { user }                            (Bearer id_token)
//	POST /passwordless/start · /passwordless/verify
//	POST /password/reset/start · /password/reset/verify

const minPasswordLen = 8

// Handler serves the auth data plane.
type Handler struct {
	Reg *control.Registry
	Svc *Service
}

// NewHandler builds an auth data-plane handler.
func NewHandler(reg *control.Registry, svc *Service) *Handler {
	return &Handler{Reg: reg, Svc: svc}
}

// Register mounts the auth routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/auth/{instance}"
	mux.HandleFunc("GET "+p+"/config", common.Wrap(h.config))
	mux.HandleFunc("POST "+p+"/signup", common.Wrap(h.signup))
	mux.HandleFunc("POST "+p+"/signin", common.Wrap(h.signin))
	mux.HandleFunc("POST "+p+"/token/refresh", common.Wrap(h.refresh))
	mux.HandleFunc("POST "+p+"/signout", common.Wrap(h.signout))
	mux.HandleFunc("GET "+p+"/me", common.Wrap(h.me))
	// Not a public endpoint — this is what `env.auth.verifyToken` dispatches into. The
	// binding has always pointed here; the route did not exist, so every call landed on the
	// console's catch-all and came back as HTML, which the binding reported as "unreadable
	// response". A function that authenticates its caller — the documented reason
	// verifyToken exists — could not run locally at all.
	mux.HandleFunc("POST "+p+"/token/verify", common.Wrap(h.verifyToken))
	mux.HandleFunc("POST "+p+"/passwordless/start", common.Wrap(h.passwordlessStart))
	mux.HandleFunc("POST "+p+"/passwordless/verify", common.Wrap(h.passwordlessVerify))
	mux.HandleFunc("POST "+p+"/password/reset/start", common.Wrap(h.resetStart))
	mux.HandleFunc("POST "+p+"/password/reset/verify", common.Wrap(h.resetVerify))
	mux.HandleFunc("POST "+p+"/email/verify/start", common.Wrap(h.verifyStart))
	mux.HandleFunc("POST "+p+"/email/verify/confirm", common.Wrap(h.verifyConfirm))

	// Not emulated: TOTP 2FA and passkeys need a real authenticator app / platform
	// authenticator, which a local emulator can't stand in for. They answer with a clear
	// UNIMPLEMENTED rather than pretending to work.
	for _, route := range []string{
		"POST " + p + "/2fa/totp/start", "POST " + p + "/2fa/totp/confirm", "POST " + p + "/2fa/totp/disable",
		"POST " + p + "/2fa/verify",
	} {
		mux.HandleFunc(route, common.Wrap(unimplemented("TOTP 2FA")))
	}
	for _, route := range []string{
		"POST " + p + "/passkey/register/start", "POST " + p + "/passkey/register/finish",
		"POST " + p + "/passkey/login/start", "POST " + p + "/passkey/login/finish",
		"GET " + p + "/passkeys", "DELETE " + p + "/passkeys/{credId}",
	} {
		mux.HandleFunc(route, common.Wrap(unimplemented("passkeys (WebAuthn)")))
	}
}

func unimplemented(what string) common.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		return common.NewError(http.StatusNotImplemented,
			what+" is not emulated locally — use the hosted service to exercise it", "UNIMPLEMENTED")
	}
}

// instance resolves the addressed auth instance (auto-creating it, like every other
// emulated service) plus its parsed config and end-user store.
func (h *Handler) instance(r *http.Request) (*control.Instance, Config, *Store, error) {
	inst := h.Reg.GetOrCreate("auth", r.PathValue("instance"))
	cfg := ParseConfig(inst.Config)
	store, err := h.Svc.Mgr.Open(inst.ID)
	if err != nil {
		return nil, cfg, nil, err
	}
	return inst, cfg, store, nil
}

// --- GET /config ---
// Client-SAFE subset only: the signup form shape, which methods are on, and the PUBLIC
// captcha site key. No secrets.

func (h *Handler) config(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, _, err := h.instance(r)
	if err != nil {
		return err
	}
	fields := cfg.Signup.Fields
	if fields == nil {
		fields = []SignupField{}
	}
	common.WriteJSON(w, 200, map[string]any{
		"instance":       inst.Name,
		"allow_signup":   cfg.AllowSignup,
		"identity_field": cfg.Signup.IdentityField,
		"fields":         fields,
		"captcha":        map[string]any{"enabled": cfg.CaptchaEnabled, "site_key": cfg.CaptchaSiteKey},
		"methods": map[string]any{
			"password":     true,
			"passkeys":     cfg.PasskeysEnabled,
			"passwordless": cfg.PasswordlessEnabled,
			"totp":         cfg.TotpEnabled,
		},
	})
	return nil
}

// --- POST /signup ---

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	if !cfg.AllowSignup {
		return common.PermissionDenied("public signup is disabled for this instance")
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	identifier, profile, err := collectSignup(cfg.Signup, body)
	if err != nil {
		return err
	}
	password, err := validatePassword(body["password"])
	if err != nil {
		return err
	}
	pwHash, err := HashPassword(password)
	if err != nil {
		return err
	}
	// Signup-collected fields go to `profile` (self-asserted); `claims` (authoritative,
	// admin-set) starts empty — a user can never write it.
	user, err := store.CreateUser(identifier, pwHash, profile, nowMS())
	if err != nil {
		return err
	}
	// Email-verify gate: when required, the account is created UNVERIFIED and gets no session —
	// instead we issue a verify code and the client must confirm before it can sign in.
	if cfg.RequireEmailVerification {
		code, err := h.issueCode(cfg, store, identifier, "verify")
		if err != nil {
			return err
		}
		out := map[string]any{"verification_required": true, "user": user.public()}
		if h.Svc.DevOpen && code != "" {
			out["dev_code"] = code // LOCAL DEV ONLY
		}
		common.WriteJSON(w, 201, out)
		return nil
	}
	tokens, err := h.issueTokens(inst, cfg, store, user)
	if err != nil {
		return err
	}
	tokens["user"] = user.public()
	common.WriteJSON(w, 201, tokens)
	return nil
}

// --- POST /signin ---

func (h *Handler) signin(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	identifier, idErr := normalizeBodyIdentifier(cfg, body)
	password, ok := body["password"].(string)
	if !ok {
		return common.BadRequest("password is required")
	}
	// Same error whether the account is missing, the handle is malformed, or the password
	// is wrong — never leak which handles are registered. The dummy verification keeps the
	// work (and so the timing) flat on a miss.
	var user *UserRow
	if idErr == nil {
		if user, err = store.ByIdentifier(identifier); err != nil {
			return err
		}
	}
	pwOK := false
	if user != nil {
		pwOK = VerifyPassword(password, user.PwHash)
	} else {
		VerifyPassword(password, dummyHash)
	}
	if user == nil || !pwOK {
		return common.Unauthenticated("invalid credentials")
	}
	if user.Disabled {
		return common.PermissionDenied("this account is disabled")
	}
	// Email-verify gate: a correct password is not enough while verification is required and the
	// address is unconfirmed. Distinct code so the client can route to the verify flow.
	if cfg.RequireEmailVerification && !user.EmailVerified {
		return common.NewError(403, "email address not verified", "EMAIL_UNVERIFIED")
	}
	tokens, err := h.issueTokens(inst, cfg, store, user)
	if err != nil {
		return err
	}
	tokens["user"] = user.public()
	common.WriteJSON(w, 200, tokens)
	return nil
}

// --- POST /token/refresh (rotates) ---

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.RefreshToken == "" {
		return common.BadRequest("refresh_token is required")
	}
	now := nowMS()
	newToken := common.RandID(32)
	refreshExp := now/1000 + cfg.RefreshTokenTTL
	uid, err := store.RotateRefresh(body.RefreshToken, newToken, refreshExp, now)
	if err != nil {
		return err
	}
	if uid == "" {
		return common.Unauthenticated("invalid or expired refresh token")
	}
	// Re-read the account: claims may have changed and it may have been disabled.
	user, err := store.ByUID(uid)
	if err != nil {
		return err
	}
	if user == nil || user.Disabled {
		return common.PermissionDenied("account is disabled")
	}
	exp := now/1000 + cfg.AccessTokenTTL
	common.WriteJSON(w, 200, map[string]any{
		"id_token":           h.mintIDToken(inst, cfg, user, exp),
		"refresh_token":      newToken,
		"expires_at":         exp,
		"refresh_expires_at": refreshExp,
	})
	return nil
}

// --- POST /signout ---

func (h *Handler) signout(w http.ResponseWriter, r *http.Request) error {
	_, _, store, err := h.instance(r)
	if err != nil {
		return err
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.RefreshToken != "" {
		if err := store.RevokeRefresh(body.RefreshToken); err != nil {
			return err
		}
	}
	common.WriteJSON(w, 200, map[string]any{"ok": true})
	return nil
}

// --- GET /me ---

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	inst, _, store, err := h.instance(r)
	if err != nil {
		return err
	}
	claims, err := requireIdentity(r, inst)
	if err != nil {
		return err
	}
	user, err := store.ByUID(claims.Sub)
	if err != nil {
		return err
	}
	if user == nil {
		return common.NotFound("user not found")
	}
	common.WriteJSON(w, 200, map[string]any{"user": user.public()})
	return nil
}

// verifyToken backs `env.auth.verifyToken` for functions.
//
// Answers the claims for a good token and a JSON `null` for a bad one, with 200 either
// way. That asymmetry is deliberate and matches hosted: the stub RETURNS null rather than
// throwing, so user code written as `const id = await env.auth.verifyToken(...); if (!id)
// return 401` behaves the same in both places. Answering 401 here would surface as a
// thrown binding error and send that code down its failure path instead.
func (h *Handler) verifyToken(w http.ResponseWriter, r *http.Request) error {
	inst, _, _, err := h.instance(r)
	if err != nil {
		return err
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	claims := VerifyIdentity(strings.TrimSpace(body.Token), inst.Secret)
	if claims == nil || claims.Iss != inst.ID {
		common.WriteJSON(w, 200, nil)
		return nil
	}
	// `uid` alongside `sub`: the token is signed with `sub` (it is the JWT subject), but
	// every other auth binding takes a `uid`, so the one call that produces the value
	// spelling it differently is a trap worth closing on both sides.
	common.WriteJSON(w, 200, map[string]any{
		"iss":        claims.Iss,
		"sub":        claims.Sub,
		"uid":        claims.Sub,
		"identifier": claims.Identifier,
		"email":      claims.Email,
		"profile":    claims.Profile,
		"claims":     claims.Claims,
		"exp":        claims.Exp,
	})
	return nil
}

// requireIdentity verifies the Bearer id_token against the addressed instance.
func requireIdentity(r *http.Request, inst *control.Instance) (*IdentityClaims, error) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return nil, common.Unauthenticated("missing bearer token")
	}
	claims := VerifyIdentity(strings.TrimSpace(h[7:]), inst.Secret)
	if claims == nil || claims.Iss != inst.ID {
		return nil, common.Unauthenticated("invalid or expired token")
	}
	return claims, nil
}

// --- passwordless sign-in + password reset (one-time email code) ---
// Both flows share one machinery: a 6-digit code, hashed at rest, single-use, short-TTL,
// throttled to one live code per user per purpose. `/start` ALWAYS returns 200 and never
// reveals whether an account exists.
//
// The emulator cannot send email. Instead the code is PRINTED TO THE SERVER LOG, and — in
// dev-open mode only — echoed in the `/start` response so a test or a local client can
// complete the flow without a mailbox. That echo is a local-development affordance and has
// no counterpart in the hosted service.

// GenCode is exported so the admin sign-in-code route mints codes exactly as this flow
// does. Two generators would be two answers to "what is a valid code".
func GenCode() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(b[:])%1000000)
}

// issueCode issues (and "delivers") a one-time code. Silently no-ops when the user is
// missing, disabled, has no address on file, or already has a live code — the caller
// always answers 200 regardless. Returns the code when one was issued.
func (h *Handler) issueCode(cfg Config, store *Store, identifier, purpose string) (string, error) {
	user, err := store.ByIdentifier(identifier)
	if err != nil {
		return "", err
	}
	if user == nil || user.Disabled {
		return "", nil
	}
	email := cfg.Signup.DeriveEmail(user.Identifier, user.Profile)
	if email == "" {
		return "", nil // username-only instance with no address on file — nothing to send
	}
	code := GenCode()
	exp := nowMS()/1000 + cfg.PasswordlessCodeTTL
	issued, err := store.StartEmailCode(user.UID, purpose, code, exp, nowMS())
	if err != nil || !issued {
		return "", err
	}
	label := "sign-in"
	if purpose == "reset" {
		label = "password reset"
	} else if purpose == "verify" {
		label = "email verification"
	}
	log.Printf("[auth] %s code for %s: %s", label, email, code)
	return code, nil
}

// codeResponse is the always-200 `/start` answer, carrying the code only in dev-open mode.
func (h *Handler) codeResponse(w http.ResponseWriter, code string) {
	out := map[string]any{"ok": true}
	if h.Svc.DevOpen && code != "" {
		// LOCAL DEV ONLY: the hosted service never returns the code.
		out["dev_code"] = code
	}
	common.WriteJSON(w, 200, out)
}

func (h *Handler) passwordlessStart(w http.ResponseWriter, r *http.Request) error {
	_, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	if !cfg.PasswordlessEnabled {
		return common.PermissionDenied("passwordless sign-in is disabled")
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code := ""
	if identifier, idErr := normalizeBodyIdentifier(cfg, body); idErr == nil {
		if code, err = h.issueCode(cfg, store, identifier, "login"); err != nil {
			return err
		}
	}
	h.codeResponse(w, code) // always 200 — never reveal whether the account exists
	return nil
}

func (h *Handler) passwordlessVerify(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	if !cfg.PasswordlessEnabled {
		return common.PermissionDenied("passwordless sign-in is disabled")
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code, _ := body["code"].(string)
	if strings.TrimSpace(code) == "" {
		return common.BadRequest("code is required")
	}
	identifier, idErr := normalizeBodyIdentifier(cfg, body)
	if idErr != nil {
		return common.Unauthenticated("invalid or expired code")
	}
	user, err := store.ByIdentifier(identifier)
	if err != nil {
		return err
	}
	if user == nil {
		return common.Unauthenticated("invalid or expired code")
	}
	ok, err := store.ConsumeEmailCode(user.UID, "login", strings.TrimSpace(code), nowMS())
	if err != nil {
		return err
	}
	if !ok {
		return common.Unauthenticated("invalid or expired code")
	}
	if user.Disabled {
		return common.PermissionDenied("this account is disabled")
	}
	// Receiving a passwordless code proves control of the address — mark verified (once) so a
	// passwordless user is never blocked by the verify gate.
	if !user.EmailVerified {
		if _, err := store.SetEmailVerified(user.UID, nowMS()); err != nil {
			return err
		}
		user.EmailVerified = true
	}
	tokens, err := h.issueTokens(inst, cfg, store, user)
	if err != nil {
		return err
	}
	tokens["user"] = user.public()
	common.WriteJSON(w, 200, tokens)
	return nil
}

func (h *Handler) resetStart(w http.ResponseWriter, r *http.Request) error {
	_, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code := ""
	if identifier, idErr := normalizeBodyIdentifier(cfg, body); idErr == nil {
		if code, err = h.issueCode(cfg, store, identifier, "reset"); err != nil {
			return err
		}
	}
	h.codeResponse(w, code)
	return nil
}

func (h *Handler) resetVerify(w http.ResponseWriter, r *http.Request) error {
	_, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code, _ := body["code"].(string)
	if strings.TrimSpace(code) == "" {
		return common.BadRequest("code is required")
	}
	newPassword, err := validateNewPassword(body["new_password"]) // validate BEFORE burning the code
	if err != nil {
		return err
	}
	identifier, idErr := normalizeBodyIdentifier(cfg, body)
	if idErr != nil {
		return common.Unauthenticated("invalid or expired code")
	}
	user, err := store.ByIdentifier(identifier)
	if err != nil {
		return err
	}
	if user == nil {
		return common.Unauthenticated("invalid or expired code")
	}
	ok, err := store.ConsumeEmailCode(user.UID, "reset", strings.TrimSpace(code), nowMS())
	if err != nil {
		return err
	}
	if !ok {
		return common.Unauthenticated("invalid or expired code")
	}
	pwHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	// SetPassword also revokes every outstanding refresh token — the user re-authenticates.
	if err := store.SetPassword(user.UID, pwHash, nowMS()); err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"ok": true})
	return nil
}

// --- email verification (opt-in gate: requireEmailVerification) ---

func (h *Handler) verifyStart(w http.ResponseWriter, r *http.Request) error {
	_, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	if !cfg.RequireEmailVerification {
		return common.PermissionDenied("email verification is not enabled")
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code := ""
	if identifier, idErr := normalizeBodyIdentifier(cfg, body); idErr == nil {
		if code, err = h.issueCode(cfg, store, identifier, "verify"); err != nil {
			return err
		}
	}
	h.codeResponse(w, code) // always 200 — never reveal whether the account exists
	return nil
}

func (h *Handler) verifyConfirm(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, store, err := h.instance(r)
	if err != nil {
		return err
	}
	if !cfg.RequireEmailVerification {
		return common.PermissionDenied("email verification is not enabled")
	}
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	code, _ := body["code"].(string)
	if strings.TrimSpace(code) == "" {
		return common.BadRequest("code is required")
	}
	identifier, idErr := normalizeBodyIdentifier(cfg, body)
	if idErr != nil {
		return common.Unauthenticated("invalid or expired code")
	}
	user, err := store.ByIdentifier(identifier)
	if err != nil {
		return err
	}
	if user == nil {
		return common.Unauthenticated("invalid or expired code")
	}
	ok, err := store.ConsumeEmailCode(user.UID, "verify", strings.TrimSpace(code), nowMS())
	if err != nil {
		return err
	}
	if !ok {
		return common.Unauthenticated("invalid or expired code")
	}
	if user.Disabled {
		return common.PermissionDenied("this account is disabled")
	}
	if _, err := store.SetEmailVerified(user.UID, nowMS()); err != nil {
		return err
	}
	user.EmailVerified = true
	// Confirming proves control of the address → issue a session (auto sign-in).
	tokens, err := h.issueTokens(inst, cfg, store, user)
	if err != nil {
		return err
	}
	tokens["user"] = user.public()
	common.WriteJSON(w, 200, tokens)
	return nil
}

// --- token minting ---

func (h *Handler) mintIDToken(inst *control.Instance, cfg Config, user *UserRow, exp int64) string {
	c := IdentityClaims{
		Iss:        inst.ID,
		Sub:        user.UID,
		Identifier: user.Identifier,
		Email:      cfg.Signup.DeriveEmail(user.Identifier, user.Profile),
		Exp:        exp,
	}
	// The token carries BOTH bags: profile (user-supplied, $auth.profile.X) and claims
	// (server/admin-set, $auth.claims.X). The target data plane keeps them apart.
	if len(user.Profile) > 0 {
		c.Profile = user.Profile
	}
	if len(user.Claims) > 0 {
		c.Claims = user.Claims
	}
	return SignIdentity(c, inst.Secret)
}

// issueTokens mints an id_token plus a fresh refresh token, persisting the refresh hash.
func (h *Handler) issueTokens(inst *control.Instance, cfg Config, store *Store, user *UserRow) (map[string]any, error) {
	now := nowMS()
	exp := now/1000 + cfg.AccessTokenTTL
	refreshToken := common.RandID(32)
	refreshExp := now/1000 + cfg.RefreshTokenTTL
	if err := store.AddRefresh(refreshToken, user.UID, refreshExp, now); err != nil {
		return nil, err
	}
	return map[string]any{
		"id_token":           h.mintIDToken(inst, cfg, user, exp),
		"refresh_token":      refreshToken,
		"expires_at":         exp,
		"refresh_expires_at": refreshExp,
	}, nil
}

// --- signup-form parsing ---

// collectSignup parses a signup body against the configured fields: the identity field's
// value becomes the unique `identifier`, every other field becomes part of the user's
// PROFILE (self-asserted, non-authoritative) — NOT `claims`, which is admin-set only.
func collectSignup(sc SignupConfig, body map[string]any) (string, map[string]any, error) {
	profile := map[string]any{}
	identifier := ""
	for _, f := range sc.Fields {
		raw, present := body[f.Key]
		if s, ok := raw.(string); ok && strings.TrimSpace(s) == "" {
			present = false
		}
		if !present || raw == nil {
			if f.Required {
				return "", nil, common.BadRequest(labelOf(f) + " is required")
			}
			continue
		}
		if f.Key == sc.IdentityField {
			typ := "email"
			if f.Type == "string" {
				typ = "string"
			}
			id, err := NormalizeIdentifier(raw, typ)
			if err != nil {
				return "", nil, err
			}
			identifier = id
			continue
		}
		val, err := coerceField(f, raw)
		if err != nil {
			return "", nil, err
		}
		profile[f.Key] = val
	}
	if identifier == "" {
		return "", nil, common.BadRequest("the identity field is required")
	}
	return identifier, profile, nil
}

func labelOf(f SignupField) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Key
}

func coerceField(f SignupField, raw any) (any, error) {
	switch f.Type {
	case "email":
		return NormalizeEmail(raw)
	case "number":
		n, ok := raw.(float64)
		if !ok {
			return nil, common.BadRequest(labelOf(f) + " must be a number")
		}
		return n, nil
	case "boolean":
		if b, ok := raw.(bool); ok {
			return b, nil
		}
		return raw == "true", nil
	default:
		s, ok := raw.(string)
		if !ok {
			s = fmt.Sprint(raw)
		}
		s = strings.TrimSpace(s)
		if len(s) > maxStringField {
			return nil, common.BadRequest(labelOf(f) + " is too long")
		}
		return s, nil
	}
}

// normalizeBodyIdentifier reads the login handle from a request body, accepting `email`
// as a back-compat alias for email-typed instances.
func normalizeBodyIdentifier(cfg Config, body map[string]any) (string, error) {
	raw, ok := body["identifier"]
	if !ok || raw == nil {
		raw = body["email"]
	}
	return NormalizeIdentifier(raw, cfg.Signup.IdentityType())
}

func validatePassword(v any) (string, error) {
	s, ok := v.(string)
	if !ok || len(s) < minPasswordLen {
		return "", common.BadRequest(fmt.Sprintf("password must be at least %d characters", minPasswordLen))
	}
	return s, nil
}

func validateNewPassword(v any) (string, error) {
	s, ok := v.(string)
	if !ok || len(s) < minPasswordLen {
		return "", common.BadRequest(fmt.Sprintf("new_password must be at least %d characters", minPasswordLen))
	}
	return s, nil
}
