package altengine

import (
	"context"
	"iter"
	"net/url"
	"strconv"
	"time"
)

// --- wire types (field names are wire-verbatim snake_case) ---

// GeoPoint is a lat/lng pair for geo fields.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// SearchField is one typed document field. Type is one of text, html, atom,
// number, date, geo, tokenprefix, untokenprefix. Prefer the Text/HTML/Atom/
// Number/Date/Geo/TokenPrefix/UntokenPrefix builders.
type SearchField struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Value is a string for text/html/atom/tokenprefix/untokenprefix, a number
	// for number, an ISO string or epoch ms for date, a GeoPoint for geo.
	Value    any    `json:"value"`
	Language string `json:"language,omitempty"`
}

// Field builders — Text("title", "Blue Shoes") reads better than a struct
// literal and pins the right type string.

// Text builds a full-text field.
func Text(name, value string) SearchField { return SearchField{Name: name, Type: "text", Value: value} }

// HTML builds an html field (tags stripped for matching).
func HTML(name, value string) SearchField { return SearchField{Name: name, Type: "html", Value: value} }

// Atom builds an exact-match atom field.
func Atom(name, value string) SearchField { return SearchField{Name: name, Type: "atom", Value: value} }

// Number builds a numeric field.
func Number(name string, value float64) SearchField {
	return SearchField{Name: name, Type: "number", Value: value}
}

// Date builds a date field from a time.Time.
func Date(name string, value time.Time) SearchField {
	return SearchField{Name: name, Type: "date", Value: value.UTC().Format(time.RFC3339)}
}

// Geo builds a geo field.
func Geo(name string, lat, lng float64) SearchField {
	return SearchField{Name: name, Type: "geo", Value: GeoPoint{Lat: lat, Lng: lng}}
}

// TokenPrefix builds a tokenized prefix-match field.
func TokenPrefix(name, value string) SearchField {
	return SearchField{Name: name, Type: "tokenprefix", Value: value}
}

// UntokenPrefix builds an untokenized prefix-match field.
func UntokenPrefix(name, value string) SearchField {
	return SearchField{Name: name, Type: "untokenprefix", Value: value}
}

// SearchFacet is one document facet; Type is "atom" or "number". Prefer the
// AtomFacet/NumberFacet builders.
type SearchFacet struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// AtomFacet builds an atom facet.
func AtomFacet(name, value string) SearchFacet {
	return SearchFacet{Name: name, Type: "atom", Value: value}
}

// NumberFacet builds a number facet.
func NumberFacet(name string, value float64) SearchFacet {
	return SearchFacet{Name: name, Type: "number", Value: value}
}

// SearchDocument is one search document.
type SearchDocument struct {
	// ID is server-assigned when empty (returned from Put).
	ID string `json:"id,omitempty"`
	// Rank is the sort/tiebreak rank; defaults to seconds since 2011-01-01.
	Rank   int           `json:"rank,omitempty"`
	Lang   string        `json:"lang,omitempty"`
	Fields []SearchField `json:"fields"`
	Facets []SearchFacet `json:"facets,omitempty"`
}

// SortSpec is one sort clause. Expr is a field name, "rank", or "_score".
type SortSpec struct {
	Expr string `json:"expr"`
	Desc bool   `json:"desc,omitempty"`
	// Default is the sort value for documents missing the field.
	Default *float64 `json:"default,omitempty"`
}

// FacetRefinement narrows results to an atom facet value or a number range
// (min inclusive, max exclusive).
type FacetRefinement struct {
	Name  string   `json:"name"`
	Value string   `json:"value,omitempty"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

// SnippetRequest asks for highlighted extracts of matching fields.
type SnippetRequest struct {
	// Fields lists text/html fields to snippet; defaults to the query's fields.
	Fields []string `json:"fields,omitempty"`
	// MaxTokens is a token count (not chars), clamped 1..64.
	MaxTokens int    `json:"max_tokens,omitempty"`
	PreTag    string `json:"pre_tag,omitempty"`
	PostTag   string `json:"post_tag,omitempty"`
	Ellipsis  string `json:"ellipsis,omitempty"`
}

// CollapseRequest keeps the top docs per distinct value of a field.
type CollapseRequest struct {
	// Field is an atom or number field to collapse (distinct) on.
	Field string `json:"field"`
	// Limit is the top docs kept per distinct value, clamped 1..10.
	// Incompatible with sort.
	Limit int `json:"limit,omitempty"`
}

// SearchRequest is a full search request (App Engine Search API parity).
type SearchRequest struct {
	// Query uses the boolean query language: bare terms, field:value,
	// comparisons, AND/OR/NOT, -term, grouping, "phrases", ~stem,
	// distance(loc, geopoint(lat,lng)) < n.
	Query string `json:"query"`
	// Limit defaults to 20, max 1000.
	Limit int `json:"limit,omitempty"`
	// Offset max 1000; prefer Cursor for deep paging.
	Offset         int        `json:"offset,omitempty"`
	Cursor         string     `json:"cursor,omitempty"`
	IDsOnly        bool       `json:"ids_only,omitempty"`
	ReturnedFields []string   `json:"returned_fields,omitempty"`
	Sort           []SortSpec `json:"sort,omitempty"`
	// SortLimit caps documents examined for sorting (default 1000, max 10000).
	SortLimit int `json:"sort_limit,omitempty"`
	// Scorer is "match" or "rescore".
	Scorer string `json:"scorer,omitempty"`
	// FacetDiscover auto-discovers the top-N facets over matching documents.
	FacetDiscover int `json:"facet_discover,omitempty"`
	// Facets lists explicit facet names to aggregate.
	Facets           []string          `json:"facets,omitempty"`
	FacetValueLimit  int               `json:"facet_value_limit,omitempty"`
	FacetRefinements []FacetRefinement `json:"facet_refinements,omitempty"`
	// FacetDepth is the matching docs examined for facet counts (max 10000).
	FacetDepth int `json:"facet_depth,omitempty"`
	// TotalHitsAccuracy caps counting cost — total_hits is exact up to this
	// (default 20, max 10000).
	TotalHitsAccuracy int              `json:"total_hits_accuracy,omitempty"`
	Snippet           *SnippetRequest  `json:"snippet,omitempty"`
	Collapse          *CollapseRequest `json:"collapse,omitempty"`
}

// SearchHit is one result.
type SearchHit struct {
	ID    string   `json:"id"`
	Rank  int      `json:"rank"`
	Score *float64 `json:"score,omitempty"`
	// Document is absent when IDsOnly.
	Document *SearchDocument `json:"document,omitempty"`
	// Snippet maps field name → highlighted HTML.
	Snippet map[string]string `json:"snippet,omitempty"`
}

// FacetValueResult is one facet value with its count (Min/Max for number
// range buckets).
type FacetValueResult struct {
	Value string   `json:"value"`
	Count int      `json:"count"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

// FacetResult is the aggregation for one facet name.
type FacetResult struct {
	Name   string             `json:"name"`
	Type   string             `json:"type"`
	Values []FacetValueResult `json:"values"`
}

// SearchResponse is a search result page.
type SearchResponse struct {
	TotalHits      int         `json:"total_hits"`
	TotalHitsExact bool        `json:"total_hits_exact"`
	Returned       int         `json:"returned"`
	Results        []SearchHit `json:"results"`
	// Cursor is present iff more pages exist.
	Cursor string        `json:"cursor,omitempty"`
	Facets []FacetResult `json:"facets,omitempty"`
	// RuleData is free-form data attached by matching query rules.
	RuleData []any `json:"rule_data,omitempty"`
}

// IndexInfo is one index listing row.
type IndexInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	CreatedAt int64  `json:"created_at"`
}

// IndexesPage is one page of index listings.
type IndexesPage struct {
	Indexes []IndexInfo `json:"indexes"`
	HasMore bool        `json:"has_more"`
}

// IndexSchema is the union schema of an index: for every field name, the
// types it has been indexed with.
type IndexSchema struct {
	Name      string              `json:"name"`
	Namespace string              `json:"namespace"`
	Fields    map[string][]string `json:"fields"`
}

// ListDocumentsOptions pages through an index's documents in id order
// (keyset pagination).
type ListDocumentsOptions struct {
	// StartID resumes from this document id.
	StartID string
	// ExcludeStart skips StartID itself (the server includes it by default).
	ExcludeStart bool
	Limit        int
	// IDsOnly returns ids without documents.
	IDsOnly bool
}

// DocumentsPage is one page of documents (or ids when IDsOnly).
type DocumentsPage struct {
	Documents []SearchDocument `json:"documents"`
	IDs       []string         `json:"ids"`
}

// --- client ---

// Search is a client for one search instance, bound to a namespace (sent as
// X-Namespace, default "").
type Search struct {
	Instance  string
	Namespace string
	http      *transport
	base      string
}

// Search binds a client for a search instance in the default namespace.
func (c *Client) Search(instance string) *Search {
	return &Search{Instance: instance, http: c.http, base: "/v1/search/" + seg(instance)}
}

// WithNamespace returns a client for the same instance bound to another
// namespace.
func (s *Search) WithNamespace(namespace string) *Search {
	return &Search{Instance: s.Instance, Namespace: namespace, http: s.http, base: s.base}
}

func (s *Search) headers() map[string]string {
	if s.Namespace == "" {
		return nil
	}
	return map[string]string{"X-Namespace": s.Namespace}
}

// Index binds operations on one search index.
func (s *Search) Index(name string) *SearchIndex {
	return &SearchIndex{Name: name, s: s, path: s.base + "/indexes/" + seg(name)}
}

// ListIndexes lists the namespace's indexes.
func (s *Search) ListIndexes(ctx context.Context, opts ListOptions) (*IndexesPage, error) {
	q := opts.query()
	if s.Namespace != "" {
		q.Set("namespace", s.Namespace)
	}
	var out IndexesPage
	err := s.http.do(ctx, request{method: "GET", path: s.base + "/indexes", query: q}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListNamespaces lists distinct namespaces with live indexes — alphabetical,
// Q substring search, Limit default 50 (max 100). Instance-wide (not bound to
// this client's namespace); the default namespace appears as "".
func (s *Search) ListNamespaces(ctx context.Context, opts ListOptions) (*NamespacesPage, error) {
	var out NamespacesPage
	err := s.http.do(ctx, request{method: "GET", path: s.base + "/namespaces", query: opts.query()}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteIndex deletes an index and all its documents. Requires a full grant.
func (s *Search) DeleteIndex(ctx context.Context, name string) (bool, error) {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	err := s.http.do(ctx, request{
		method:  "DELETE",
		path:    s.base + "/indexes/" + seg(name),
		headers: s.headers(),
	}, &out)
	return out.Deleted, err
}

// SearchIndex is operations on one search index.
type SearchIndex struct {
	Name string
	s    *Search
	path string
}

// Put upserts up to 200 documents; returns their ids in order
// (server-assigned when a document omits ID).
func (i *SearchIndex) Put(ctx context.Context, documents []SearchDocument) ([]string, error) {
	var out struct {
		IDs []string `json:"ids"`
	}
	err := i.s.http.do(ctx, request{
		method:  "PUT",
		path:    i.path + "/documents",
		body:    map[string]any{"documents": documents},
		headers: i.s.headers(),
	}, &out)
	return out.IDs, err
}

// Get fetches one document, or nil (with a nil error) when it — or the
// index — doesn't exist.
func (i *SearchIndex) Get(ctx context.Context, id string) (*SearchDocument, error) {
	var out struct {
		Document *SearchDocument `json:"document"`
	}
	err := i.s.http.do(ctx, request{
		method:  "GET",
		path:    i.path + "/documents/" + seg(id),
		headers: i.s.headers(),
	}, &out)
	if IsNotFound(err) {
		return nil, nil
	}
	return out.Document, err
}

// Delete removes up to 200 documents by id; missing ids are no-ops. Requires
// a full grant. Returns the number actually deleted.
func (i *SearchIndex) Delete(ctx context.Context, ids []string) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	err := i.s.http.do(ctx, request{
		method:  "POST",
		path:    i.path + "/documents/delete",
		body:    map[string]any{"ids": ids},
		headers: i.s.headers(),
	}, &out)
	return out.Deleted, err
}

// Search runs a search request.
func (i *SearchIndex) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	err := i.s.http.do(ctx, request{
		method:  "POST",
		path:    i.path + "/search",
		body:    req,
		headers: i.s.headers(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SearchAll iterates every hit across cursor pages. Iteration stops at the
// first error, yielded as the second value.
func (i *SearchIndex) SearchAll(ctx context.Context, req SearchRequest) iter.Seq2[*SearchHit, error] {
	return func(yield func(*SearchHit, error) bool) {
		req.Offset = 0
		for {
			page, err := i.Search(ctx, req)
			if err != nil {
				yield(nil, err)
				return
			}
			for n := range page.Results {
				if !yield(&page.Results[n], nil) {
					return
				}
			}
			if page.Cursor == "" {
				return
			}
			req.Cursor = page.Cursor
		}
	}
}

// ListDocuments returns one page of documents in id order (keyset pagination
// via StartID).
func (i *SearchIndex) ListDocuments(ctx context.Context, opts ListDocumentsOptions) (*DocumentsPage, error) {
	q := url.Values{}
	if opts.StartID != "" {
		q.Set("start_id", opts.StartID)
	}
	if opts.ExcludeStart {
		q.Set("include_start", "false")
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.IDsOnly {
		q.Set("ids_only", "true")
	}
	var out DocumentsPage
	err := i.s.http.do(ctx, request{
		method:  "GET",
		path:    i.path + "/documents",
		query:   q,
		headers: i.s.headers(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllDocuments iterates every document in the index (keyset pagination
// handled for you). Iteration stops at the first error, yielded as the
// second value.
func (i *SearchIndex) ListAllDocuments(ctx context.Context, limit int) iter.Seq2[*SearchDocument, error] {
	return func(yield func(*SearchDocument, error) bool) {
		opts := ListDocumentsOptions{Limit: limit}
		for {
			page, err := i.ListDocuments(ctx, opts)
			if err != nil {
				yield(nil, err)
				return
			}
			if len(page.Documents) == 0 {
				return
			}
			for n := range page.Documents {
				if !yield(&page.Documents[n], nil) {
					return
				}
			}
			opts.StartID = page.Documents[len(page.Documents)-1].ID
			opts.ExcludeStart = true
		}
	}
}

// Schema returns the index's union field schema.
func (i *SearchIndex) Schema(ctx context.Context) (*IndexSchema, error) {
	var out IndexSchema
	err := i.s.http.do(ctx, request{method: "GET", path: i.path + "/schema", headers: i.s.headers()}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
