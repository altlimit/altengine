package admin

import (
	"net/http"

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
