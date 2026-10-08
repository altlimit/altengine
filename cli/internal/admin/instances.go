package admin

import (
	"net/http"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

func instanceJSON(in *control.Instance) map[string]any {
	m := map[string]any{"id": in.ID, "name": in.Name, "created_at": in.CreatedAt, "config": in.Config()}
	return m
}

func (h *Handler) listInstances(w http.ResponseWriter, service string) error {
	list := h.Reg.List(service)
	out := make([]map[string]any, 0, len(list))
	for _, in := range list {
		out = append(out, instanceJSON(in))
	}
	common.WriteJSON(w, 200, map[string]any{"instances": out})
	return nil
}

func (h *Handler) createInstance(w http.ResponseWriter, r *http.Request, service string) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Name == "" {
		return common.BadRequest("name is required")
	}
	in, err := h.Reg.Create(service, body.Name)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, instanceJSON(in))
	return nil
}

func (h *Handler) getInstance(w http.ResponseWriter, r *http.Request, service string) error {
	in, err := h.resolveInst(r, service)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, instanceJSON(in))
	return nil
}

func (h *Handler) setConfig(w http.ResponseWriter, r *http.Request, service string) error {
	in, err := h.resolveInst(r, service)
	if err != nil {
		return err
	}
	// `{"config": {...}}` or the bare object, as hosted. Read once: the body is a stream, and a
	// second decode of it sees nothing — which is how a bare body used to save nothing and 200.
	var body map[string]any
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	cfg := body
	if inner, present := body["config"]; present {
		m, ok := inner.(map[string]any)
		if !ok {
			return common.BadRequest("config must be an object")
		}
		cfg = m
	}
	// A full replace, as hosted: a key the body leaves out is removed. Each service's validator
	// runs inside it, the same check MCP's patch gets.
	if err := h.Reg.ReplaceConfig(in, cfg); err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"config": in.Config()})
	return nil
}

func (h *Handler) deleteInstance(w http.ResponseWriter, r *http.Request, service string) error {
	// Data and identity together — see control.Registry.DeleteInstance.
	ok := h.Reg.DeleteInstance(service, r.PathValue("id"))
	common.WriteJSON(w, 200, map[string]any{"deleted": ok})
	return nil
}

func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request) error {
	return h.rotate(w, r, "channel")
}

// rotateAuthSecret re-keys an auth instance: every outstanding identity token stops
// verifying immediately (clients sign in again).
func (h *Handler) rotateAuthSecret(w http.ResponseWriter, r *http.Request) error {
	return h.rotate(w, r, "auth")
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request, service string) error {
	in, err := h.resolveInst(r, service)
	if err != nil {
		return err
	}
	in.Secret = common.RandID(32)
	h.Reg.SetConfig(in, map[string]any{}) // triggers persistence
	common.WriteJSON(w, 200, map[string]any{"rotated": true})
	return nil
}
