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
	"github.com/altlimit/altengine/cli/internal/identity"
)

// Handler serves the datastore data plane.
type Handler struct {
	Reg  *control.Registry
	Auth *auth.Store
	Mgr  *Manager
	// Ident resolves end-user identity tokens (nil = API keys only).
	Ident *identity.Service
	// Live publishes change events for the datastore→channel live bridge (nil = off).
	Live LivePublisher
}

// NewHandler builds a datastore handler.
func NewHandler(reg *control.Registry, a *auth.Store, mgr *Manager) *Handler {
	return &Handler{Reg: reg, Auth: a, Mgr: mgr}
}

// WithIdentity enables end-user identity tokens (and their row rules) on this handler.
func (h *Handler) WithIdentity(svc *identity.Service) *Handler { h.Ident = svc; return h }

// WithLive enables the datastore→channel live bridge.
func (h *Handler) WithLive(p LivePublisher) *Handler { h.Live = p; return h }

// Register mounts the datastore routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/datastore/{instance}"
	i := p + "/ns/{ns}/col"
	mux.HandleFunc("GET "+p+"/ns", common.Wrap(h.listNamespaces))
	mux.HandleFunc("DELETE "+p+"/ns/{ns}", common.Wrap(h.deleteNamespace))
	mux.HandleFunc("POST "+p+"/ns/{ns}/transaction", common.Wrap(h.transaction))
	mux.HandleFunc("POST "+i+"/{collection}/documents", common.Wrap(h.putDocs))
	mux.HandleFunc("POST "+i+"/{collection}/documents/get", common.Wrap(h.batchGet))
	mux.HandleFunc("POST "+i+"/{collection}/documents/delete", common.Wrap(h.deleteDocs))
	mux.HandleFunc("POST "+i+"/{collection}/query", common.Wrap(h.query))
	mux.HandleFunc("POST "+i+"/{collection}/aggregate", common.Wrap(h.aggregate))
	mux.HandleFunc("GET "+i+"/{collection}/indexes", common.Wrap(h.listIndexes))
	mux.HandleFunc("POST "+i+"/{collection}/indexes", common.Wrap(h.createIndex))
	mux.HandleFunc("DELETE "+i+"/{collection}/indexes/{id}", common.Wrap(h.dropIndex))
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

// resolve authenticates the request and opens the store for the path's namespace. The
// returned EndUser is non-nil when the caller presented an end-user identity token rather
// than an org API key — that caller's reads and writes are row-scoped by the issuing auth
// instance's rules.
func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, *Store, *identity.EndUser, error) {
	name := r.PathValue("instance")
	id, user, err := h.Ident.ResolveRequest(r, h.Auth)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := auth.Require(id, "datastore", name, need); err != nil {
		return nil, nil, nil, err
	}
	inst := h.Reg.GetOrCreate("datastore", name)
	ns := decodeNs(r.PathValue("ns"))
	autoID, _ := inst.Config["autoId"].(string)
	store, err := h.Mgr.Open(inst.ID, ns, autoID)
	if err != nil {
		return nil, nil, nil, err
	}
	return inst, store, user, nil
}

// backendOnly refuses an end-user identity token on a route that requires full backend
// trust (namespace administration, transactions, index management) — matching the hosted
// service, which accepts only an org API key on those.
func backendOnly(user *identity.EndUser, what string) error {
	if user != nil {
		return common.PermissionDenied(what + " requires an org API key, not an end-user token")
	}
	return nil
}

func autoIndexEnabled(inst *control.Instance) bool {
	if v, ok := inst.Config["autoIndex"].(bool); ok {
		return v
	}
	return true
}

func (h *Handler) listNamespaces(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	id, user, err := h.Ident.ResolveRequest(r, h.Auth)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "datastore", name, auth.Read); err != nil {
		return err
	}
	if err := backendOnly(user, "listing namespaces"); err != nil {
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
	id, user, err := h.Ident.ResolveRequest(r, h.Auth)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "datastore", name, auth.Full); err != nil {
		return err
	}
	if err := backendOnly(user, "dropping a namespace"); err != nil {
		return err
	}
	// Deliberately NOT h.resolve: opening the store would auto-create the very
	// namespace being deleted.
	inst := h.Reg.GetOrCreate("datastore", name)
	existed, err := h.Mgr.Drop(inst.ID, decodeNs(r.PathValue("ns")))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": existed})
	return nil
}

func (h *Handler) putDocs(w http.ResponseWriter, r *http.Request) error {
	inst, store, user, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Documents []PutDoc `json:"documents"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	collection := r.PathValue("collection")
	var keys []string
	var changed []LiveDoc
	if user != nil {
		// An identity write is row-scoped: create/update rules decide per document (owner
		// match, immutable fields) and server `stamp` fields are applied from the token.
		create, err := user.DatastoreWritePolicy(inst.Name, decodeNs(r.PathValue("ns")), collection, "create")
		if err != nil {
			return err
		}
		update, err := user.DatastoreWritePolicy(inst.Name, decodeNs(r.PathValue("ns")), collection, "update")
		if err != nil {
			return err
		}
		keys, changed, err = store.PutScoped(collection, body.Documents, create, update)
		if err != nil {
			return err
		}
	} else {
		keys, _, err = store.Put(collection, body.Documents)
		if err != nil {
			return err
		}
		for i, k := range keys {
			changed = append(changed, LiveDoc{Key: k, Data: body.Documents[i].Data})
		}
	}
	h.emitLive(inst, decodeNs(r.PathValue("ns")), collection, "put", changed)
	common.WriteJSON(w, 200, map[string]any{"keys": keys})
	return nil
}

func (h *Handler) batchGet(w http.ResponseWriter, r *http.Request) error {
	inst, store, user, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var body struct {
		Keys []any `json:"keys"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	collection := r.PathValue("collection")
	var docs []StoredDoc
	if user != nil {
		// A point-read can't be scoped in SQL, so the read rules are applied server-side: a row
		// the caller may not read is omitted (never leaked), and masked fields are projected out.
		ns := decodeNs(r.PathValue("ns"))
		groups, err := h.readGroups(user, inst, ns, collection)
		if err != nil {
			return err
		}
		fieldReads, err := h.fieldReads(user, inst, ns, collection)
		if err != nil {
			return err
		}
		docs, err = store.BatchGetScoped(collection, body.Keys, groups, fieldReads)
		if err != nil {
			return err
		}
	} else if docs, err = store.BatchGet(collection, body.Keys); err != nil {
		return err
	}
	if docs == nil {
		docs = []StoredDoc{}
	}
	common.WriteJSON(w, 200, map[string]any{"documents": docs})
	return nil
}

func (h *Handler) deleteDocs(w http.ResponseWriter, r *http.Request) error {
	inst, store, user, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		Keys []any `json:"keys"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	collection := r.PathValue("collection")
	// The live event needs each document's partition field, which is gone once the row is
	// deleted — read the bodies first (only when this collection actually publishes).
	var removed []LiveDoc
	if h.livePublishes(inst, collection) {
		if existing, err := store.BatchGet(collection, body.Keys); err == nil {
			for _, d := range existing {
				removed = append(removed, LiveDoc{Key: d.Key, Data: d.Data})
			}
		}
	}
	var n int
	if user != nil {
		del, err := user.DatastoreWritePolicy(inst.Name, decodeNs(r.PathValue("ns")), collection, "delete")
		if err != nil {
			return err
		}
		if del.Denied {
			return common.PermissionDenied("not permitted: delete on '" + collection + "'")
		}
		if n, err = store.DeleteScoped(collection, body.Keys, ToGroups(del.Match)); err != nil {
			return err
		}
	} else if n, err = store.Delete(collection, body.Keys); err != nil {
		return err
	}
	if n > 0 {
		h.emitLive(inst, decodeNs(r.PathValue("ns")), collection, "delete", removed)
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": n})
	return nil
}

func (h *Handler) query(w http.ResponseWriter, r *http.Request) error {
	inst, store, user, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var req QueryRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	collection := r.PathValue("collection")
	// A join reads a SECOND collection by key WITHOUT applying that collection's read rules,
	// so joins require an org API key, never an end-user token — an identity reads one
	// collection at a time through its own scoped filters below.
	if user != nil && len(req.Join) > 0 {
		return common.PermissionDenied("query joins require an org API key, not an end-user token")
	}
	// Row-level read rules scope the query (0/1 group ANDed in; ≥2 run the UNION merge) and are
	// resolved BEFORE Query runs so its index guard covers the filters that actually execute.
	// Per-field read masks are applied to the result docs afterward (masking is an output
	// projection, layered under the row-level read).
	var readGroups [][]Filter
	var fieldReads map[string][][]Filter
	if user != nil {
		ns := decodeNs(r.PathValue("ns"))
		if readGroups, err = h.readGroups(user, inst, ns, collection); err != nil {
			return err
		}
		if fieldReads, err = h.fieldReads(user, inst, ns, collection); err != nil {
			return err
		}
	}
	res, err := store.Query(collection, req, autoIndexEnabled(inst), readGroups)
	if err != nil {
		return err
	}
	if user != nil && len(res.Documents) > 0 {
		res.Documents = MaskFields(res.Documents, fieldReads)
	}
	if res.Documents == nil && !req.KeysOnly {
		res.Documents = []StoredDoc{}
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request) error {
	inst, store, user, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	var req AggregateRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	collection := r.PathValue("collection")
	// Same row scoping as query: an identity aggregates only over rows it may read. 0/1 group
	// ANDs into the where; ≥2 groups inject an inline OR (aggregates count each row once).
	var readGroups [][]Filter
	if user != nil {
		if readGroups, err = h.readGroups(user, inst, decodeNs(r.PathValue("ns")), collection); err != nil {
			return err
		}
	}
	res, err := store.Aggregate(collection, req, autoIndexEnabled(inst), readGroups)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) transaction(w http.ResponseWriter, r *http.Request) error {
	_, store, user, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	// A multi-document transaction can't be row-scoped per operation, so it stays a
	// backend-only surface (an identity's single-document writes go through the rules).
	if err := backendOnly(user, "transactions"); err != nil {
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
	_, store, user, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	if err := backendOnly(user, "index management"); err != nil {
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
	_, store, user, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	if err := backendOnly(user, "index management"); err != nil {
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
	_, store, user, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	if err := backendOnly(user, "index management"); err != nil {
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

// readGroups resolves the row-level read rules for an identity into datastore OR-of-AND filter
// groups. nil means unconstrained; a collection the rules don't cover is a 403.
func (h *Handler) readGroups(user *identity.EndUser, inst *control.Instance, namespace, collection string) ([][]Filter, error) {
	gs, err := user.DatastoreReadGroups(inst.Name, namespace, collection)
	if err != nil {
		return nil, err
	}
	return ToGroups(gs), nil
}

// fieldReads resolves the per-field read masks for an identity: field -> OR-groups the doc must
// satisfy for that field to be returned. nil means nothing is masked.
func (h *Handler) fieldReads(user *identity.EndUser, inst *control.Instance, namespace, collection string) (map[string][][]Filter, error) {
	fr, err := user.DatastoreFieldReads(inst.Name, namespace, collection)
	if err != nil {
		return nil, err
	}
	return ToFieldGroups(fr), nil
}

// livePublishes reports whether a collection emits live change events.
func (h *Handler) livePublishes(inst *control.Instance, collection string) bool {
	if h.Live == nil {
		return false
	}
	live := parseLive(inst.Config)
	if live == nil {
		return false
	}
	_, ok := live.Collections[collection]
	return ok
}
