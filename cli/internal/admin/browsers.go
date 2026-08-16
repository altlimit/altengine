package admin

import (
	"net/http"
	"strconv"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/search"
)

// --- search browser ---

func (h *Handler) searchStore(r *http.Request) (*search.Store, error) {
	in, err := h.resolveInst(r, "search")
	if err != nil {
		return nil, err
	}
	ns := r.URL.Query().Get("namespace")
	stemming := true
	if v, ok := in.Config["stemming"].(bool); ok {
		stemming = v
	}
	store, err := h.Srch.Open(in.ID, ns, stemming)
	if err != nil {
		return nil, err
	}
	// Apply the instance's synonyms + rules so the admin browser previews results exactly
	// as the data plane serves them (same ApplyConfig entry point routes.go uses).
	store.ApplyConfig(in.Config)
	return store, nil
}

func (h *Handler) searchIndexes(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
	if err != nil {
		return err
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	indexes, hasMore, err := store.ListIndexes(r.URL.Query().Get("q"), limit)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"indexes": indexes, "has_more": hasMore})
	return nil
}

func (h *Handler) searchListDocs(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 25)
	docs, _, err := store.List(r.PathValue("index"), q.Get("start_id"), q.Get("include_start") != "false", limit, false)
	if err != nil {
		return err
	}
	if docs == nil {
		docs = []search.Document{}
	}
	common.WriteJSON(w, 200, map[string]any{"documents": docs})
	return nil
}

func (h *Handler) searchQuery(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
	if err != nil {
		return err
	}
	var req search.SearchRequest
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

func (h *Handler) searchPut(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Documents []search.Document `json:"documents"`
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

func (h *Handler) searchDelete(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
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

func (h *Handler) searchDropIndex(w http.ResponseWriter, r *http.Request) error {
	store, err := h.searchStore(r)
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

// --- datastore browser ---

func (h *Handler) dsStore(r *http.Request) (*datastore.Store, *bool, error) {
	in, err := h.resolveInst(r, "datastore")
	if err != nil {
		return nil, nil, err
	}
	autoID, _ := in.Config["autoId"].(string)
	autoIndex := true
	if v, ok := in.Config["autoIndex"].(bool); ok {
		autoIndex = v
	}
	// DecodeNs, not the raw segment: "_default" is the wire spelling of the default
	// namespace (""), because a URL path cannot carry an empty segment. Passing it through
	// undecoded opened a namespace literally named "_default" — a different, always-empty
	// store — so the data browser reported no collections for the namespace holding all the
	// data, and a phantom "_default" appeared in the namespace list beside the real one.
	store, err := h.DS.Open(in.ID, datastore.DecodeNs(r.PathValue("ns")), autoID)
	return store, &autoIndex, err
}

func (h *Handler) dsNamespaces(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolveInst(r, "datastore")
	if err != nil {
		return err
	}
	nss := h.DS.Namespaces(in.ID)
	out := make([]map[string]any, 0, len(nss))
	for _, ns := range nss {
		out = append(out, map[string]any{"namespace": ns})
	}
	common.WriteJSON(w, 200, map[string]any{"namespaces": out, "has_more": false})
	return nil
}

func (h *Handler) dsCollections(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
	if err != nil {
		return err
	}
	cols, err := store.Collections()
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"collections": cols})
	return nil
}

func (h *Handler) dsQuery(w http.ResponseWriter, r *http.Request) error {
	store, autoIndex, err := h.dsStore(r)
	if err != nil {
		return err
	}
	var req datastore.QueryRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	// The admin data browser is a full-trust console view (no end-user identity), so no row
	// rules apply — pass nil read groups.
	res, err := store.Query(r.PathValue("collection"), req, *autoIndex, nil)
	if err != nil {
		return err
	}
	if res.Documents == nil && !req.KeysOnly {
		res.Documents = []datastore.StoredDoc{}
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) dsAggregate(w http.ResponseWriter, r *http.Request) error {
	store, autoIndex, err := h.dsStore(r)
	if err != nil {
		return err
	}
	var req datastore.AggregateRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	res, err := store.Aggregate(r.PathValue("collection"), req, *autoIndex, nil)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) dsPut(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Documents []datastore.PutDoc `json:"documents"`
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

func (h *Handler) dsDelete(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
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

func (h *Handler) dsListIndexes(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
	if err != nil {
		return err
	}
	ix, err := store.ListIndexes(r.PathValue("collection"))
	if err != nil {
		return err
	}
	if ix == nil {
		ix = []datastore.IndexRow{}
	}
	common.WriteJSON(w, 200, map[string]any{"indexes": ix})
	return nil
}

func (h *Handler) dsCreateIndex(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
	if err != nil {
		return err
	}
	var body struct {
		Fields []string `json:"fields"`
		Unique bool     `json:"unique"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if _, err := store.CreateIndex(r.PathValue("collection"), body.Fields, body.Unique); err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"created": true})
	return nil
}

func (h *Handler) dsDropIndex(w http.ResponseWriter, r *http.Request) error {
	store, _, err := h.dsStore(r)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(r.PathValue("idx"), 10, 64)
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
