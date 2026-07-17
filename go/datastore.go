package altengine

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/url"
	"strconv"
)

// --- wire types (field names are wire-verbatim snake_case) ---

// Document is a stored datastore document.
type Document struct {
	Key     string          `json:"key"`
	Data    json.RawMessage `json:"data"`
	Created int64           `json:"created"`
	Updated int64           `json:"updated"`
	// Joins holds joined documents keyed by the query's join[].as names.
	Joins map[string]json.RawMessage `json:"joins,omitempty"`
}

// DataAs unmarshals the document's data into v.
func (d *Document) DataAs(v any) error { return json.Unmarshal(d.Data, v) }

// PutDocument is one document to upsert. Omit Key to use the instance's
// auto-id strategy; numeric keys are stored as decimal strings (5 ≡ "5").
type PutDocument struct {
	Key  any `json:"key,omitempty"`
	Data any `json:"data"`
}

// Filter is one query predicate. Field is a dot-path into the document
// ("a.b.c", depth ≤ 8) or __key__ / __created__ / __updated__.
// Op is one of = != < <= > >= in.
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// Order is one sort clause; Dir is "asc" (default) or "desc".
type Order struct {
	Field string `json:"field"`
	Dir   string `json:"dir,omitempty"`
}

// Join attaches a referenced document under Joins[As].
type Join struct {
	As         string `json:"as"`
	Collection string `json:"collection"`
	// LocalField is the field on the queried document whose value is the
	// joined document's key.
	LocalField string `json:"local_field"`
}

// QueryRequest is one page of an index-served query. Limit defaults to 25
// (max 500).
type QueryRequest struct {
	Where    []Filter `json:"where,omitempty"`
	Order    []Order  `json:"order,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
	KeysOnly bool     `json:"keys_only,omitempty"`
	Join     []Join   `json:"join,omitempty"`
}

// AutoIndexInfo describes an index the instance auto-created to serve a
// query (present only on the request that triggered the build).
type AutoIndexInfo struct {
	Fields      []string `json:"fields"`
	RowsWritten int      `json:"rows_written"`
}

// QueryResult is one query page. Cursor is nil when the result set is
// exhausted; pass it back via QueryRequest.Cursor to continue.
type QueryResult struct {
	Documents []*Document `json:"documents"`
	Keys      []string    `json:"keys"`
	Cursor    *string     `json:"cursor"`
	// AutoIndexed is set when the instance auto-created an index to serve
	// this query.
	AutoIndexed *AutoIndexInfo `json:"auto_indexed,omitempty"`
}

// Metric is one aggregate function: fn is count, sum, avg, min, or max.
// Field is required for all fns except count. As names the result (defaults
// to fn or fn_field).
type Metric struct {
	Fn    string `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as,omitempty"`
}

// AggregateRequest computes grouped metrics over an index-served filter.
type AggregateRequest struct {
	Where   []Filter `json:"where,omitempty"`
	Group   []string `json:"group,omitempty"`
	Metrics []Metric `json:"metrics"`
	Order   []Order  `json:"order,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

// AggregateGroup is one result group; metric values are nil for empty inputs
// (e.g. avg of nothing).
type AggregateGroup struct {
	Group   map[string]any      `json:"group"`
	Metrics map[string]*float64 `json:"metrics"`
}

// AggregateResult is the aggregate response.
type AggregateResult struct {
	Groups      []AggregateGroup `json:"groups"`
	AutoIndexed *AutoIndexInfo   `json:"auto_indexed,omitempty"`
}

// TxnOp is one atomic transaction operation. All ops in a transaction apply
// atomically within a single namespace; a failed check aborts with a 409.
// Use the TxnPut/TxnDelete/TxnMutate/TxnCheck constructors.
type TxnOp struct {
	Op         string             `json:"op"`
	Collection string             `json:"collection"`
	Key        any                `json:"key,omitempty"`
	Data       any                `json:"data,omitempty"`
	Set        map[string]any     `json:"set,omitempty"`
	Increment  map[string]float64 `json:"increment,omitempty"`
	Remove     []string           `json:"remove,omitempty"`
	Upsert     bool               `json:"upsert,omitempty"`
	Exists     *bool              `json:"exists,omitempty"`
}

// TxnPut upserts a document; pass a nil key for auto-id.
func TxnPut(collection string, key any, data any) TxnOp {
	return TxnOp{Op: "put", Collection: collection, Key: key, Data: data}
}

// TxnDelete deletes a document by key.
func TxnDelete(collection string, key any) TxnOp {
	return TxnOp{Op: "delete", Collection: collection, Key: key}
}

// TxnMutate partially updates a document in place. Zero-value fields are
// omitted; set Upsert to create the document when missing.
func TxnMutate(collection string, key any, m TxnOp) TxnOp {
	m.Op, m.Collection, m.Key = "mutate", collection, key
	return m
}

// TxnCheck asserts a document's existence; a failed check aborts the
// transaction with a 409 and nothing is applied.
func TxnCheck(collection string, key any, exists bool) TxnOp {
	return TxnOp{Op: "check", Collection: collection, Key: key, Exists: &exists}
}

// IndexSpec describes one secondary index. Fields entries are "field" or
// "field:asc" / "field:desc" (max 8).
type IndexSpec struct {
	ID         int64    `json:"id"`
	Collection string   `json:"collection"`
	Fields     []string `json:"fields"`
	Unique     bool     `json:"unique"`
}

// NamespacesPage is one page of namespace names.
type NamespacesPage struct {
	Namespaces []string `json:"namespaces"`
	HasMore    bool     `json:"has_more"`
}

// ListOptions filters paged name listings (namespaces, indexes).
type ListOptions struct {
	// Q is a substring filter.
	Q string
	// Limit caps the page size (server default 20–50 depending on endpoint).
	Limit int
}

func (o ListOptions) query() url.Values {
	q := url.Values{}
	if o.Q != "" {
		q.Set("q", o.Q)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	return q
}

// --- client ---

// Datastore is a client for one datastore instance, bound to a namespace.
type Datastore struct {
	// Instance is the datastore instance name.
	Instance string
	// Namespace this client is bound to (default "").
	Namespace string
	http      *transport
	nsBase    string
}

// Datastore binds a client for a datastore instance in the default namespace.
func (c *Client) Datastore(instance string) *Datastore {
	return newDatastore(c.http, instance, "")
}

func newDatastore(t *transport, instance, namespace string) *Datastore {
	return &Datastore{
		Instance:  instance,
		Namespace: namespace,
		http:      t,
		nsBase:    "/v1/datastore/" + seg(instance) + "/namespaces/" + seg(namespace),
	}
}

// WithNamespace returns a client for the same instance bound to another
// namespace.
func (d *Datastore) WithNamespace(namespace string) *Datastore {
	return newDatastore(d.http, d.Instance, namespace)
}

// Put upserts one document and returns its key; pass a nil key to use the
// instance's auto-id strategy.
func (d *Datastore) Put(ctx context.Context, collection string, key any, data any) (string, error) {
	keys, err := d.PutMulti(ctx, collection, []PutDocument{{Key: key, Data: data}})
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", errors.New("altengine: server returned no key")
	}
	return keys[0], nil
}

// PutMulti upserts up to 500 documents and returns their keys in order.
func (d *Datastore) PutMulti(ctx context.Context, collection string, documents []PutDocument) ([]string, error) {
	var out struct {
		Keys []string `json:"keys"`
	}
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/documents",
		body:   map[string]any{"documents": documents},
	}, &out)
	return out.Keys, err
}

// Get fetches one document, or nil (with a nil error) when it doesn't exist.
func (d *Datastore) Get(ctx context.Context, collection string, key any) (*Document, error) {
	var out struct {
		Document *Document `json:"document"`
	}
	err := d.http.do(ctx, request{
		method: "GET",
		path:   d.nsBase + "/collections/" + seg(collection) + "/documents/" + seg(keyString(key)),
	}, &out)
	if IsNotFound(err) {
		return nil, nil
	}
	return out.Document, err
}

// GetMulti fetches up to 500 documents in one round trip, order-preserving
// with nil placeholders for missing keys (App Engine db.get semantics).
func (d *Datastore) GetMulti(ctx context.Context, collection string, keys []any) ([]*Document, error) {
	var out struct {
		Documents []*Document `json:"documents"`
	}
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/documents/get",
		body:   map[string]any{"keys": keys},
	}, &out)
	if err != nil {
		return nil, err
	}
	// The wire response omits missing keys; rebuild positional correspondence.
	byKey := make(map[string]*Document, len(out.Documents))
	for _, doc := range out.Documents {
		byKey[doc.Key] = doc
	}
	docs := make([]*Document, len(keys))
	for i, k := range keys {
		docs[i] = byKey[keyString(k)]
	}
	return docs, nil
}

// Delete removes one document by key (a no-op when missing) and reports
// whether it existed.
func (d *Datastore) Delete(ctx context.Context, collection string, key any) (bool, error) {
	n, err := d.DeleteMulti(ctx, collection, []any{key})
	return n > 0, err
}

// DeleteMulti removes up to 500 documents by key; missing keys are no-ops.
// Returns the number actually deleted.
func (d *Datastore) DeleteMulti(ctx context.Context, collection string, keys []any) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/documents/delete",
		body:   map[string]any{"keys": keys},
	}, &out)
	return out.Deleted, err
}

// Query runs one page of an index-served query.
func (d *Datastore) Query(ctx context.Context, collection string, req QueryRequest) (*QueryResult, error) {
	var out QueryResult
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/query",
		body:   req,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryAll iterates every matching document across pages (cursor handled for
// you). Iteration stops at the first error, yielded as the second value.
func (d *Datastore) QueryAll(ctx context.Context, collection string, req QueryRequest) iter.Seq2[*Document, error] {
	return func(yield func(*Document, error) bool) {
		for {
			page, err := d.Query(ctx, collection, req)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, doc := range page.Documents {
				if !yield(doc, nil) {
					return
				}
			}
			if page.Cursor == nil {
				return
			}
			req.Cursor = *page.Cursor
		}
	}
}

// Aggregate computes grouped metrics over an index-served filter.
func (d *Datastore) Aggregate(ctx context.Context, collection string, req AggregateRequest) (*AggregateResult, error) {
	var out AggregateResult
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/aggregate",
		body:   req,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Transaction applies up to 500 operations atomically within this namespace.
// NOT retried automatically (increments would double-apply); a failed check
// returns a 409 APIError. The result holds the per-op resulting key, nil for
// delete/check ops.
func (d *Datastore) Transaction(ctx context.Context, operations []TxnOp) ([]*string, error) {
	var out struct {
		Keys []*string `json:"keys"`
	}
	err := d.http.do(ctx, request{
		method:  "POST",
		path:    d.nsBase + "/transaction",
		body:    map[string]any{"operations": operations},
		noRetry: true,
	}, &out)
	return out.Keys, err
}

// ListIndexes lists the collection's secondary indexes.
func (d *Datastore) ListIndexes(ctx context.Context, collection string) ([]IndexSpec, error) {
	var out struct {
		Indexes []IndexSpec `json:"indexes"`
	}
	err := d.http.do(ctx, request{
		method: "GET",
		path:   d.nsBase + "/collections/" + seg(collection) + "/indexes",
	}, &out)
	return out.Indexes, err
}

// CreateIndex creates a secondary index (idempotent — re-creating an
// identical spec returns the existing index).
func (d *Datastore) CreateIndex(ctx context.Context, collection string, fields []string, unique bool) (*IndexSpec, error) {
	var out struct {
		Index *IndexSpec `json:"index"`
	}
	err := d.http.do(ctx, request{
		method: "POST",
		path:   d.nsBase + "/collections/" + seg(collection) + "/indexes",
		body:   map[string]any{"fields": fields, "unique": unique},
	}, &out)
	return out.Index, err
}

// DeleteIndex drops a secondary index by id.
func (d *Datastore) DeleteIndex(ctx context.Context, collection string, id int64) (bool, error) {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	err := d.http.do(ctx, request{
		method: "DELETE",
		path:   d.nsBase + "/collections/" + seg(collection) + "/indexes/" + strconv.FormatInt(id, 10),
	}, &out)
	return out.Deleted, err
}

// ListNamespaces lists the instance's namespaces (instance-wide, not bound to
// this client's namespace).
func (d *Datastore) ListNamespaces(ctx context.Context, opts ListOptions) (*NamespacesPage, error) {
	var out NamespacesPage
	err := d.http.do(ctx, request{
		method: "GET",
		path:   "/v1/datastore/" + seg(d.Instance) + "/namespaces",
		query:  opts.query(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteNamespace deletes a namespace and everything in it. Requires a full
// grant.
func (d *Datastore) DeleteNamespace(ctx context.Context, namespace string) (bool, error) {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	err := d.http.do(ctx, request{
		method: "DELETE",
		path:   "/v1/datastore/" + seg(d.Instance) + "/namespaces/" + seg(namespace),
	}, &out)
	return out.Deleted, err
}
