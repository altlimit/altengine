package admin

import (
	"net/http"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/identity"
)

// End-user browser for an auth instance. These are the TENANT's end users (the people who
// sign in to the app being built), not console operators — the emulator has no operator
// accounts at all.

func (h *Handler) authStore(r *http.Request) (*identity.Store, error) {
	if h.Ident == nil {
		return nil, common.NotFound("auth service is not available")
	}
	in, err := h.resolveInst(r, "auth")
	if err != nil {
		return nil, err
	}
	s, err := h.Ident.Open(in.ID)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (h *Handler) authUsers(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	users, err := s.ListUsers(atoiDefault(r.URL.Query().Get("limit"), 100), r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	// `has_more` because the hosted page carries it, and a caller written against one should
	// not find the field missing on the other.
	common.WriteJSON(w, 200, map[string]any{"users": users, "has_more": false})
	return nil
}

// authUser reads one end user — env.auth.getUser. Answers null for a uid that is not there,
// as the hosted stub does, so `if (!user)` behaves the same on both.
func (h *Handler) authUser(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	row, err := s.ByUID(r.PathValue("uid"))
	if err != nil {
		return err
	}
	if row == nil {
		common.WriteJSON(w, 200, nil)
		return nil
	}
	common.WriteJSON(w, 200, row.Public())
	return nil
}

// authSetClaims backfills/edits an end user's claims. Adding a sign-up field does not
// retroactively give existing users that claim, and a rule stamping a missing claim is a
// hard deny — so without this, accounts created before a field existed are stuck.
func (h *Handler) authSetClaims(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Claims map[string]any `json:"claims"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	ok, err := s.SetClaims(r.PathValue("uid"), body.Claims, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if !ok {
		return common.NotFound("user not found")
	}
	common.WriteJSON(w, 200, map[string]any{"claims": body.Claims})
	return nil
}

// authSetDisabled locks or unlocks an account — suspension without deletion, which is the
// moderation action an app reaches for first and the one the emulator could not perform.
func (h *Handler) authSetDisabled(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	disabled := body.Disabled == nil || *body.Disabled
	ok, err := s.SetDisabled(r.PathValue("uid"), disabled, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if !ok {
		return common.NotFound("user not found")
	}
	common.WriteJSON(w, 200, map[string]any{"uid": r.PathValue("uid"), "disabled": disabled})
	return nil
}

// authSignInCode mints a one-time sign-in code and hands it BACK instead of delivering it.
//
// The hosted service grew this as an MCP tool (auth_issue_signin_code) for the case that
// makes the whole agent-provisioned install possible: an account created without the public
// sign-up flow has a password only its creator ever saw, and the way in afterwards is a
// passwordless code — which needs an instance that can send mail. A new one often cannot,
// and then the account is real, correct and unreachable.
//
// The emulator cannot send mail at all, so it already echoes the code in dev-open mode. This
// is not that: `/passwordless/start` refuses to issue anything for a user with no address on
// file, and the whole point of the hosted tool is that no address is needed. So this mints
// directly, and the two surfaces agree about what an agent can do.
func (h *Handler) authSignInCode(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolveInst(r, "auth")
	if err != nil {
		return err
	}
	cfg := identity.ParseConfig(in.Config)
	// The public route's own check. An instance that has turned the method off has turned it
	// off, and a door around that is not a door anyone asked for.
	if !cfg.PasswordlessEnabled {
		return common.NewError(http.StatusBadRequest,
			"passwordless sign-in is disabled on this instance — enable it before issuing a code",
			"FAILED_PRECONDITION")
	}
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Identifier string `json:"identifier"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	identifier, err := identity.NormalizeIdentifier(body.Identifier, cfg.Signup.IdentityType())
	if err != nil {
		return err
	}
	user, err := s.ByIdentifier(identifier)
	if err != nil {
		return err
	}
	// Unlike /passwordless/start this does NOT hide an unknown handle. That silence stops
	// account enumeration by strangers; this caller holds the admin key and can already list
	// every user, so the same silence would only make a typo look like a delivered code.
	if user == nil {
		return common.NotFound("no user '" + identifier + "' on this instance")
	}
	if user.Disabled {
		return common.PermissionDenied("this account is disabled")
	}
	code := identity.GenCode()
	now := time.Now()
	exp := now.Unix() + cfg.PasswordlessCodeTTL
	issued, err := s.StartEmailCode(user.UID, "login", code, exp, now.UnixMilli())
	if err != nil {
		return err
	}
	if !issued {
		// One live code per TTL window, the same throttle the public route has. Saying so
		// beats returning nothing, which reads as success with a field missing.
		return common.AlreadyExists("a sign-in code for '" + identifier + "' is already outstanding and unused — wait for it to expire or have them use it")
	}
	common.WriteJSON(w, 200, map[string]any{
		"identifier":  identifier,
		"code":        code,
		"expires_at":  exp,
		"ttl_seconds": cfg.PasswordlessCodeTTL,
	})
	return nil
}

// authBulkClaims merges a claims patch into EVERY user of an auth instance (backfill) — the
// recovery path when a rule starts requiring a claim that accounts created earlier don't have.
// only_missing (default true) fills gaps without overwriting per-user values; false forces the
// patch everywhere; a null value deletes a claim across all users. Distinct path from
// /users/{uid}/claims, so no route collision.
func (h *Handler) authBulkClaims(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Claims      map[string]any `json:"claims"`
		OnlyMissing *bool          `json:"only_missing"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if len(body.Claims) == 0 {
		return common.BadRequest("claims must be a non-empty object")
	}
	onlyMissing := true
	if body.OnlyMissing != nil {
		onlyMissing = *body.OnlyMissing
	}
	n, err := s.MergeClaimsAllUsers(body.Claims, time.Now().UnixMilli(), onlyMissing)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"updated": n, "only_missing": onlyMissing})
	return nil
}

func (h *Handler) authDeleteUser(w http.ResponseWriter, r *http.Request) error {
	s, err := h.authStore(r)
	if err != nil {
		return err
	}
	removed, err := s.DeleteUser(r.PathValue("uid"))
	if err != nil {
		return err
	}
	if !removed {
		return common.NotFound("user not found")
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": true})
	return nil
}
