package admin

import (
	"net/http"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) error {
	keys := h.Reg.ListKeys()
	if keys == nil {
		keys = []*control.APIKey{}
	}
	common.WriteJSON(w, 200, map[string]any{"api_keys": keys})
	return nil
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Name   string            `json:"name"`
		Grants map[string]string `json:"grants"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Grants == nil {
		body.Grants = map[string]string{}
	}
	// Generate an ae_<prefix>.<secret> key in the same format as hosted API keys.
	secret := common.RandID(24)
	prefix := "ae_" + secret[:6]
	plaintext := prefix + "." + secret
	h.Auth.AddKey(plaintext, auth.Grants(body.Grants))
	k := &control.APIKey{
		ID:        common.UUID(),
		Name:      body.Name,
		Prefix:    prefix,
		Grants:    body.Grants,
		CreatedAt: nowMS(),
	}
	h.Reg.AddKey(k)
	// The plaintext key is returned exactly once.
	common.WriteJSON(w, 201, map[string]any{
		"id": k.ID, "name": k.Name, "key": plaintext, "prefix": prefix, "grants": body.Grants,
	})
	return nil
}

func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request) error {
	h.Reg.DeleteKey(r.PathValue("id"))
	// Note: the plaintext isn't retained, so the auth-store entry lingers until restart;
	// acceptable for a local emulator. Revocation removes it from the console list.
	common.WriteJSON(w, 200, map[string]any{"deleted": true})
	return nil
}

func nowMS() int64 { return control.NowMS() }
