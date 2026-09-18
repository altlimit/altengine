package search

import (
	"net/http"
	"strconv"
	"strings"

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
	mux.HandleFunc("GET "+p+"/ns", common.Wrap(h.listNamespaces))
	i := p + "/ns/{ns}/idx"
	mux.HandleFunc("GET "+i, common.Wrap(h.listIndexes))
	mux.HandleFunc("GET "+i+"/{index}/schema", common.Wrap(h.schema))
	mux.HandleFunc("DELETE "+i+"/{index}", common.Wrap(h.dropIndex))
	mux.HandleFunc("POST "+i+"/{index}/documents", common.Wrap(h.putDocs))
	mux.HandleFunc("POST "+i+"/{index}/documents/get", common.Wrap(h.batchGet))
	mux.HandleFunc("GET "+i+"/{index}/documents", common.Wrap(h.listDocs))
	mux.HandleFunc("POST "+i+"/{index}/documents/delete", common.Wrap(h.deleteDocs))
	mux.HandleFunc("POST "+i+"/{index}/search", common.Wrap(h.search))
}

// nsDefaultSegment is the reserved wire spelling of the empty (default)
// namespace — a URL path can't carry an empty segment (routers collapse "//").
const nsDefaultSegment = "_default"

// decodeNs maps a {ns} path segment to its canonical namespace ("" for default).
func decodeNs(seg string) string {
	if seg == nsDefaultSegment {
		return ""
	}
	return seg
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
	ns := decodeNs(r.PathValue("ns"))
	if err := validateNamespace(ns); err != nil {
		return nil, err
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

// validateNamespace enforces the hosted API's namespace rules: printable ASCII,
// at most 100 bytes; "" (the default namespace) is allowed. Everything keyed on
// (namespace, name) NUL-joins the pair, so a namespace may never contain NUL.
func validateNamespace(ns string) error {
	if ns == "" {
		return nil
	}
	if ns == nsDefaultSegment {
		return common.BadRequest(`"` + nsDefaultSegment + `" is reserved; use the default namespace ("")`)
	}
	if len(ns) > 100 {
		return common.BadRequest("namespace must be at most 100 bytes")
	}
	for i := 0; i < len(ns); i++ {
		if ns[i] < 0x20 || ns[i] > 0x7e {
			return common.BadRequest("namespace must contain only printable ASCII characters")
		}
	}
	return nil
}

// GET /namespaces — distinct namespaces with live indexes, alphabetical, `q`
// substring search, `limit` (default 50, max 100). Mirrors datastore's listing.
func (h *Handler) listNamespaces(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "search", name, auth.Read); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("search", name)
	all := h.Mgr.Namespaces(inst.ID)
	q := r.URL.Query().Get("q")
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v >= 1 && v <= 100 {
		limit = v
	}
	matched := make([]string, 0, len(all))
	for _, ns := range all {
		if q == "" || strings.Contains(ns, q) {
			matched = append(matched, ns)
		}
	}
	hasMore := len(matched) > limit
	if hasMore {
		matched = matched[:limit]
	}
	common.WriteJSON(w, 200, map[string]any{"namespaces": matched, "has_more": hasMore})
	return nil
}

func (h *Handler) listIndexes(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	indexes, hasMore, next, err := store.ListIndexes(r.URL.Query().Get("q"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	// `cursor` carries a value exactly when there is another page and is null otherwise — the
	// hosted API's shape, so a paging loop does not have to know which one it is talking to.
	var cursor any
	if next != "" {
		cursor = next
	}
	common.WriteJSON(w, 200, map[string]any{"indexes": indexes, "has_more": hasMore, "cursor": cursor})
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
	common.WriteJSON(w, 200, map[string]any{"name": r.PathValue("index"), "namespace": decodeNs(r.PathValue("ns")), "fields": sc})
	return nil
}

func (h *Handler) batchGet(w http.ResponseWriter, r *http.Request) error {
	store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	docs, err := store.GetDocs(r.PathValue("index"), body.IDs)
	if err != nil {
		return err
	}
	if docs == nil {
		docs = []Document{}
	}
	common.WriteJSON(w, 200, map[string]any{"documents": docs})
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
