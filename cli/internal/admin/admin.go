// Package admin is the emulator's control plane and embedded admin console. It exposes
// a simplified /admin REST API (instance CRUD, data browsers that reuse the data-plane
// engines, and API-key minting) and serves the static single-page console. Unlike the
// hosted console there are no sessions or CSRF — the emulator is a local, single-user
// dev tool, so /admin is open on localhost.
package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/search"
)

//go:embed web/*
var webFS embed.FS

// Handler is the control-plane + console handler.
type Handler struct {
	Reg   *control.Registry
	Auth  *auth.Store
	DS    *datastore.Manager
	Srch  *search.Manager
	Hub   *channel.Hub
	files http.Handler
}

func NewHandler(reg *control.Registry, a *auth.Store, ds *datastore.Manager, srch *search.Manager, hub *channel.Hub) *Handler {
	sub, _ := fs.Sub(webFS, "web")
	return &Handler{Reg: reg, Auth: a, DS: ds, Srch: srch, Hub: hub, files: http.FileServer(http.FS(sub))}
}

// Register mounts the admin API and the console (catch-all "/").
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/me", common.Wrap(h.me))

	// Instances (per service).
	for _, svc := range []string{"search", "datastore", "channel", "auth"} {
		s := svc
		mux.HandleFunc("GET /admin/"+s, common.Wrap(func(w http.ResponseWriter, r *http.Request) error { return h.listInstances(w, s) }))
		mux.HandleFunc("POST /admin/"+s, common.Wrap(func(w http.ResponseWriter, r *http.Request) error { return h.createInstance(w, r, s) }))
		mux.HandleFunc("GET /admin/"+s+"/{id}", common.Wrap(func(w http.ResponseWriter, r *http.Request) error { return h.getInstance(w, r, s) }))
		mux.HandleFunc("PUT /admin/"+s+"/{id}/config", common.Wrap(func(w http.ResponseWriter, r *http.Request) error { return h.setConfig(w, r, s) }))
		mux.HandleFunc("DELETE /admin/"+s+"/{id}", common.Wrap(func(w http.ResponseWriter, r *http.Request) error { return h.deleteInstance(w, r, s) }))
	}
	mux.HandleFunc("POST /admin/channel/{id}/rotate-secret", common.Wrap(h.rotateSecret))
	mux.HandleFunc("POST /admin/auth/{id}/rotate-secret", common.Wrap(h.rotateAuthSecret))

	// Search data browser.
	mux.HandleFunc("GET /admin/search/{id}/indexes", common.Wrap(h.searchIndexes))
	mux.HandleFunc("GET /admin/search/{id}/indexes/{index}/documents", common.Wrap(h.searchListDocs))
	mux.HandleFunc("POST /admin/search/{id}/indexes/{index}/search", common.Wrap(h.searchQuery))
	mux.HandleFunc("POST /admin/search/{id}/indexes/{index}/documents", common.Wrap(h.searchPut))
	mux.HandleFunc("POST /admin/search/{id}/indexes/{index}/documents/delete", common.Wrap(h.searchDelete))
	mux.HandleFunc("DELETE /admin/search/{id}/indexes/{index}", common.Wrap(h.searchDropIndex))

	// Datastore data browser.
	mux.HandleFunc("GET /admin/datastore/{id}/namespaces", common.Wrap(h.dsNamespaces))
	mux.HandleFunc("GET /admin/datastore/{id}/namespaces/{ns}/collections", common.Wrap(h.dsCollections))
	mux.HandleFunc("POST /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/query", common.Wrap(h.dsQuery))
	mux.HandleFunc("POST /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/aggregate", common.Wrap(h.dsAggregate))
	mux.HandleFunc("POST /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/documents", common.Wrap(h.dsPut))
	mux.HandleFunc("POST /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/documents/delete", common.Wrap(h.dsDelete))
	mux.HandleFunc("GET /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/indexes", common.Wrap(h.dsListIndexes))
	mux.HandleFunc("POST /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/indexes", common.Wrap(h.dsCreateIndex))
	mux.HandleFunc("DELETE /admin/datastore/{id}/namespaces/{ns}/collections/{collection}/indexes/{idx}", common.Wrap(h.dsDropIndex))

	// API keys.
	mux.HandleFunc("GET /admin/keys", common.Wrap(h.listKeys))
	mux.HandleFunc("POST /admin/keys", common.Wrap(h.createKey))
	mux.HandleFunc("DELETE /admin/keys/{id}", common.Wrap(h.deleteKey))

	// Console (catch-all). "/" matches anything not matched by a more specific pattern.
	mux.HandleFunc("/", h.console)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	common.WriteJSON(w, 200, map[string]any{
		"user":         map[string]any{"email": "dev@localhost"},
		"organization": map[string]any{"id": auth.DevOrgID, "name": "Local Dev"},
		"role":         "owner", "is_admin": true,
	})
	return nil
}

// console serves the embedded SPA, falling back to index.html for client-side routes.
func (h *Handler) console(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" || !strings.Contains(p, ".") {
		// Serve index.html for the root and any extension-less (SPA) path.
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "console not built", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}
	h.files.ServeHTTP(w, r)
}

// resolveInst finds an instance by id for a service, or 404.
func (h *Handler) resolveInst(r *http.Request, service string) (*control.Instance, error) {
	inst := h.Reg.GetByID(service, r.PathValue("id"))
	if inst == nil {
		return nil, common.NotFound("instance not found")
	}
	return inst, nil
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
