package search

import (
	"net/http"
	"strconv"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Handler serves the search data plane.
type Handler struct {
	Reg  *control.Registry
	Auth *auth.Store
	Mgr  *Manager
}

func NewHandler(reg *control.Registry, a *auth.Store, mgr *Manager) *Handler {
	return &Handler{Reg: reg, Auth: a, Mgr: mgr}
}

func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/search/{instance}"
	mux.HandleFunc("GET "+p+"/indexes", common.Wrap(h.listIndexes))
	mux.HandleFunc("GET "+p+"/indexes/{index}/schema", common.Wrap(h.schema))
	mux.HandleFunc("DELETE "+p+"/indexes/{index}", common.Wrap(h.dropIndex))
	mux.HandleFunc("PUT "+p+"/indexes/{index}/documents", common.Wrap(h.putDocs))
	mux.HandleFunc("GET "+p+"/indexes/{index}/documents/{docId}", common.Wrap(h.getDoc))
	mux.HandleFunc("GET "+p+"/indexes/{index}/documents", common.Wrap(h.listDocs))
	mux.HandleFunc("POST "+p+"/indexes/{index}/documents/delete", common.Wrap(h.deleteDocs))
	mux.HandleFunc("POST "+p+"/indexes/{index}/search", common.Wrap(h.search))
}

func (h *Handler) resolve(r *http.Request, need auth.Level) (*Store, error) {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return nil, err
	}
	if err := auth.Require(id, "search", name, need); err != nil {
		return nil, err
	}
	inst := h.Reg.GetOrCreate("search", name)
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = r.Header.Get("X-Namespace")
	}
	stemming := true
	if v, ok := inst.Config["stemming"].(bool); ok {
		stemming = v
	}
	store, err := h.Mgr.Open(inst.ID, ns, stemming)
	if err != nil {
		return nil, err
	}
	// Instance-level query-time config: the synonym dictionary (incl. computed numeral
	// synonyms) and the query rules, applied to every search the same way the hosted
	// service applies instance config.
	store.ApplyConfig(inst.Config)
	return store, nil
}

func (h *Handler) listIndexes(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	indexes, hasMore, err := store.ListIndexes(r.URL.Query().Get("q"), limit)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"indexes": indexes, "has_more": hasMore})
	return nil
}

func (h *Handler) schema(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	sc, err := store.Schema(r.PathValue("index"))
	if err != nil {
		return err
	}
	ns := r.URL.Query().Get("namespace")
	common.WriteJSON(w, 200, map[string]any{"name": r.PathValue("index"), "namespace": ns, "fields": sc})
	return nil
}

func (h *Handler) dropIndex(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	ok, err := store.DropIndex(r.PathValue("index"))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": ok})
	return nil
}

func (h *Handler) putDocs(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Documents []Document `json:"documents"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	ids, err := store.Put(r.PathValue("index"), body.Documents)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"ids": ids})
	return nil
}

func (h *Handler) getDoc(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	doc, err := store.Get(r.PathValue("index"), r.PathValue("docId"))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"document": doc})
	return nil
}

func (h *Handler) listDocs(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	includeStart := q.Get("include_start") != "false"
	idsOnly := q.Get("ids_only") == "true"
	docs, ids, err := store.List(r.PathValue("index"), q.Get("start_id"), includeStart, limit, idsOnly)
	if err != nil {
		return err
	}
	if idsOnly || docs == nil {
		if ids == nil {
			ids = []string{}
		}
		if idsOnly {
			common.WriteJSON(w, 200, map[string]any{"ids": ids})
			return nil
		}
	}
	if docs == nil {
		docs = []Document{}
	}
	common.WriteJSON(w, 200, map[string]any{"documents": docs})
	return nil
}

func (h *Handler) deleteDocs(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	n, err := store.DeleteDocs(r.PathValue("index"), body.IDs)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": n})
	return nil
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var req SearchRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	resp, err := store.Search(r.PathValue("index"), req)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, resp)
	return nil
}
