package datastore

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Handler serves the datastore data plane.
type Handler struct {
	Reg  *control.Registry
	Auth *auth.Store
	Mgr  *Manager
}

// NewHandler builds a datastore handler.
func NewHandler(reg *control.Registry, a *auth.Store, mgr *Manager) *Handler {
	return &Handler{Reg: reg, Auth: a, Mgr: mgr}
}

// Register mounts the datastore routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/datastore/{instance}"
	mux.HandleFunc("GET "+p+"/namespaces", common.Wrap(h.listNamespaces))
	mux.HandleFunc("DELETE "+p+"/namespaces/{ns}", common.Wrap(h.deleteNamespace))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/transaction", common.Wrap(h.transaction))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/documents", common.Wrap(h.putDocs))
	mux.HandleFunc("GET "+p+"/namespaces/{ns}/collections/{collection}/documents/{key}", common.Wrap(h.getDoc))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/documents/get", common.Wrap(h.batchGet))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/documents/delete", common.Wrap(h.deleteDocs))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/query", common.Wrap(h.query))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/aggregate", common.Wrap(h.aggregate))
	mux.HandleFunc("GET "+p+"/namespaces/{ns}/collections/{collection}/indexes", common.Wrap(h.listIndexes))
	mux.HandleFunc("POST "+p+"/namespaces/{ns}/collections/{collection}/indexes", common.Wrap(h.createIndex))
	mux.HandleFunc("DELETE "+p+"/namespaces/{ns}/collections/{collection}/indexes/{id}", common.Wrap(h.dropIndex))
}

// resolve authenticates the request and opens the store for the path's namespace.
func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, *Store, error) {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return nil, nil, err
	}
	if err := auth.Require(id, "datastore", name, need); err != nil {
		return nil, nil, err
	}
	inst := h.Reg.GetOrCreate("datastore", name)
	ns := r.PathValue("ns")
	autoID, _ := inst.Config["autoId"].(string)
	store, err := h.Mgr.Open(inst.ID, ns, autoID)
	if err != nil {
		return nil, nil, err
	}
	return inst, store, nil
}

func autoIndexEnabled(inst *control.Instance) bool {
	if v, ok := inst.Config["autoIndex"].(bool); ok {
		return v
	}
	return true
}

func (h *Handler) listNamespaces(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "datastore", name, auth.Read); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("datastore", name)
	all := h.Mgr.Namespaces(inst.ID)
	sort.Strings(all)
	q := r.URL.Query().Get("q")
	limit := 20
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

func (h *Handler) deleteNamespace(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "datastore", name, auth.Full); err != nil {
		return err
	}
	// Deliberately NOT h.resolve: opening the store would auto-create the very
	// namespace being deleted.
	inst := h.Reg.GetOrCreate("datastore", name)
	existed, err := h.Mgr.Drop(inst.ID, r.PathValue("ns"))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": existed})
	return nil
}

func (h *Handler) putDocs(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Documents []PutDoc `json:"documents"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	keys, _, err := store.Put(r.PathValue("collection"), body.Documents)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"keys": keys})
	return nil
}

func (h *Handler) getDoc(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	doc, err := store.Get(r.PathValue("collection"), r.PathValue("key"))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"document": doc})
	return nil
}

func (h *Handler) batchGet(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var body struct {
		Keys []any `json:"keys"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	docs, err := store.BatchGet(r.PathValue("collection"), body.Keys)
	if err != nil {
		return err
	}
	if docs == nil {
		docs = []StoredDoc{}
	}
	common.WriteJSON(w, 200, map[string]any{"documents": docs})
	return nil
}

func (h *Handler) deleteDocs(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		Keys []any `json:"keys"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	n, err := store.Delete(r.PathValue("collection"), body.Keys)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": n})
	return nil
}

func (h *Handler) query(w http.ResponseWriter, r *http.Request) error {
	inst, store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var req QueryRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	res, err := store.Query(r.PathValue("collection"), req, autoIndexEnabled(inst))
	if err != nil {
		return err
	}
	if res.Documents == nil && !req.KeysOnly {
		res.Documents = []StoredDoc{}
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request) error {
	inst, store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var req AggregateRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	res, err := store.Aggregate(r.PathValue("collection"), req, autoIndexEnabled(inst))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) transaction(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Operations []TxnOp `json:"operations"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	keys, _, err := store.RunTransaction(body.Operations)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"keys": keys})
	return nil
}

func (h *Handler) listIndexes(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	ix, err := store.ListIndexes(r.PathValue("collection"))
	if err != nil {
		return err
	}
	if ix == nil {
		ix = []IndexRow{}
	}
	common.WriteJSON(w, 200, map[string]any{"indexes": ix})
	return nil
}

func (h *Handler) createIndex(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Fields []string        `json:"fields"`
		Unique bool            `json:"unique"`
		Raw    json.RawMessage `json:"-"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	coll := r.PathValue("collection")
	if _, err := store.CreateIndex(coll, body.Fields, body.Unique); err != nil {
		return err
	}
	ix, _ := store.ListIndexes(coll)
	var created *IndexRow
	for i := range ix {
		if fieldsEqual(ix[i].Fields, body.Fields) && ix[i].Unique == body.Unique {
			created = &ix[i]
		}
	}
	common.WriteJSON(w, 200, map[string]any{"index": created})
	return nil
}

func fieldsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (h *Handler) dropIndex(w http.ResponseWriter, r *http.Request) error {
	_, store, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return common.BadRequest("index id must be an integer")
	}
	ok, err := store.DropIndex(r.PathValue("collection"), id)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": ok})
	return nil
}
