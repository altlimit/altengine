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
	users, err := s.ListUsers(atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"users": users})
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
