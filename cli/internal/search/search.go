package search

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// SortSpec is one sort key.
type SortSpec struct {
	Expr    string  `json:"expr"`
	Desc    bool    `json:"desc"`
	Default float64 `json:"default"`
}

// Refinement is a facet refinement (atom value or number range).
type Refinement struct {
	Name  string   `json:"name"`
	Value string   `json:"value"`
	Min   *float64 `json:"min"`
	Max   *float64 `json:"max"`
}

// SnippetReq requests highlighted match context for a result. Mirrors the hosted
// API's `snippet` block: max_tokens is a TOKEN count (not chars), clamped 1..64.
type SnippetReq struct {
	Fields    []string `json:"fields"`
	MaxTokens int      `json:"max_tokens"`
	PreTag    string   `json:"pre_tag"`
	PostTag   string   `json:"post_tag"`
	Ellipsis  string   `json:"ellipsis"`
}

// CollapseReq requests field collapsing (distinct): keep the top `limit` docs per
// distinct value of `field` (an atom or number field). Default limit 1, clamped 1..10.
type CollapseReq struct {
	Field string `json:"field"`
	Limit int    `json:"limit"`
}

// SearchRequest mirrors the hosted search request body.
type SearchRequest struct {
	Query             string       `json:"query"`
	Limit             int          `json:"limit"`
	Offset            int          `json:"offset"`
	Cursor            string       `json:"cursor"`
	IDsOnly           bool         `json:"ids_only"`
	ReturnedFields    []string     `json:"returned_fields"`
	Sort              []SortSpec   `json:"sort"`
	Scorer            string       `json:"scorer"`
	FacetDiscover     int          `json:"facet_discover"`
	FacetValueLimit   int          `json:"facet_value_limit"`
	Facets            []string     `json:"facets"`
	FacetRefinements  []Refinement `json:"facet_refinements"`
	FacetDepth        int          `json:"facet_depth"`
	TotalHitsAccuracy int          `json:"total_hits_accuracy"`
	Snippet           *SnippetReq  `json:"snippet"`
	Collapse          *CollapseReq `json:"collapse"`
}

type SearchResult struct {
	ID       string            `json:"id"`
	Rank     int64             `json:"rank"`
	Document *Document         `json:"document,omitempty"`
	Snippet  map[string]string `json:"snippet,omitempty"`
}

type FacetValue struct {
	Value string   `json:"value"`
	Count int      `json:"count"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

type FacetResult struct {
	Name   string       `json:"name"`
	Type   string       `json:"type"`
	Values []FacetValue `json:"values"`
}

type SearchResponse struct {
	TotalHits      int            `json:"total_hits"`
	TotalHitsExact bool           `json:"total_hits_exact"`
	Returned       int            `json:"returned"`
	Results        []SearchResult `json:"results"`
	Cursor         *string        `json:"cursor,omitempty"`
	Facets         []FacetResult  `json:"facets,omitempty"`
	RuleData       []any          `json:"rule_data,omitempty"`
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// Search executes a search request against an index.
func (s *Store) Search(indexName string, req SearchRequest) (*SearchResponse, error) {
	ix, err := s.getIndex(indexName)
	if err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, common.NotFound("index not found")
	}
	set, err := ix.Compile(req.Query)
	if err != nil {
		return nil, err
	}

	// Field collapsing is validated up front (fast 400 on a bad field/combo) and, when
	// present, replaces the page query below with a group-by-value window.
	var collapse *collapsePlan
	if req.Collapse != nil {
		schema, err := ix.schemaTypes()
		if err != nil {
			return nil, err
		}
		collapse, err = planCollapse(&req, schema)
		if err != nil {
			return nil, err
		}
	}

	// Base matched set + args.
	var matchedSQL string
	var matchedArgs []any
	if set == nil {
		matchedSQL = fmt.Sprintf("SELECT doc_id FROM %s_docs", ix.prefix)
	} else {
		matchedSQL = set.sql
		matchedArgs = set.args
	}

	// Facet refinements: same-name OR, different-name AND.
	groups := map[string][]Refinement{}
	var order []string
	for _, r := range req.FacetRefinements {
		if _, ok := groups[r.Name]; !ok {
			order = append(order, r.Name)
		}
		groups[r.Name] = append(groups[r.Name], r)
	}
	for _, name := range order {
		var ors []string
		var args []any
		for _, r := range groups[name] {
			if r.Min != nil || r.Max != nil {
				lo, hi := -1e308, 1e308
				if r.Min != nil {
					lo = *r.Min
				}
				if r.Max != nil {
					hi = *r.Max
				}
				ors = append(ors, "(num_val >= ? AND num_val < ?)")
				args = append(args, lo, hi)
			} else {
				ors = append(ors, "(text_val = ?)")
				args = append(args, r.Value)
			}
		}
		grp := fmt.Sprintf("SELECT doc_id FROM %s_facets WHERE name=? AND (%s)", ix.prefix, strings.Join(ors, " OR "))
		gargs := append([]any{name}, args...)
		matchedSQL = "SELECT doc_id FROM (" + matchedSQL + ") INTERSECT " + grp
		matchedArgs = append(matchedArgs, gargs...)
	}

	// Query rules: hides and pinned ids are removed from the NATURAL set (count, page and
	// facets all see the exclusion — hides disappear entirely, pins are spliced back in at
	// their positions below). Rules match the RAW query, before synonym expansion.
	applied := matchRules(s.rules, req.Query)
	if len(applied.Hide) > 0 || len(applied.Pins) > 0 {
		exclude := append([]string{}, applied.Hide...)
		for _, p := range applied.Pins {
			exclude = append(exclude, p.ID)
		}
		matchedSQL, matchedArgs = excludeIDs(matchedSQL, matchedArgs, exclude)
	}

	// total_hits with accuracy cap.
	accuracy := clamp(req.TotalHitsAccuracy, 20, 10000)
	var counted int
	countSQL := fmt.Sprintf("SELECT count(*) FROM (SELECT doc_id FROM (%s) LIMIT %d)", matchedSQL, accuracy+1)
	if err := s.db.QueryRow(countSQL, matchedArgs...).Scan(&counted); err != nil {
		return nil, err
	}
	total := counted
	exact := true
	if counted > accuracy {
		total = accuracy
		exact = false
	}

	// Pagination.
	limit := clamp(req.Limit, 20, 1000)
	offset := req.Offset
	if req.Cursor != "" {
		if o, ok := decodeSearchCursor(req.Cursor); ok {
			offset = o
		}
	}
	if offset < 0 {
		offset = 0
	}
	if offset > 1000 {
		offset = 1000
	}

	// With pins, fetch the natural rows from position 0 through the end of the requested
	// window (plus room for the pins) so the splice below can place pins at their ABSOLUTE
	// positions before the offset/limit window is cut.
	fetchLimit, fetchOffset := limit, offset
	if len(applied.Pins) > 0 {
		fetchLimit = offset + limit + len(applied.Pins)
		fetchOffset = 0
	}

	// Page query: either the group-by-value collapse window, or the normal rank/sort page.
	var mainSQL string
	var args []any
	if collapse != nil {
		mainSQL, args = ix.collapseQuery(matchedSQL, matchedArgs, collapse, fetchLimit, fetchOffset)
	} else {
		orderBy, sortJoins, sortArgs := ix.buildSort(req.Sort)
		mainSQL = fmt.Sprintf(`SELECT d.doc_id, d.rank, d.body FROM %s_docs d
			JOIN (%s) m ON m.doc_id = d.doc_id
			%s
			ORDER BY %s
			LIMIT %d OFFSET %d`,
			ix.prefix, matchedSQL, sortJoins, orderBy, fetchLimit+1, fetchOffset)
		args = append(append([]any{}, matchedArgs...), sortArgs...)
	}

	rows, err := s.db.Query(mainSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	n := 0
	hasMore := false
	for rows.Next() {
		n++
		if n > fetchLimit {
			hasMore = true
			break
		}
		var id, body string
		var rank int64
		if err := rows.Scan(&id, &rank, &body); err != nil {
			return nil, err
		}
		res := SearchResult{ID: id, Rank: rank}
		if !req.IDsOnly {
			var d Document
			json.Unmarshal([]byte(body), &d)
			if len(req.ReturnedFields) > 0 {
				d = projectFields(d, req.ReturnedFields)
			}
			res.Document = &d
		}
		results = append(results, res)
	}
	rows.Close()
	if results == nil {
		results = []SearchResult{}
	}

	// Splice pinned documents into their absolute positions, then cut the requested
	// offset/limit window. Pinned docs are returned whether or not they match; a pin
	// naming a missing document is skipped. Live pins add to total_hits.
	if len(applied.Pins) > 0 {
		pinResults, livePins, err := ix.fetchPinned(s, applied.Pins)
		if err != nil {
			return nil, err
		}
		if req.IDsOnly {
			for i := range pinResults {
				pinResults[i].Document = nil
			}
		}
		final := results
		for i, p := range livePins {
			pos := p.Pos
			if pos > len(final) {
				pos = len(final)
			}
			final = slices.Insert(final, pos, pinResults[i])
		}
		total += len(livePins)
		end := offset + limit
		if end > len(final) {
			end = len(final)
		}
		if offset >= len(final) {
			results = []SearchResult{}
		} else {
			results = final[offset:end]
		}
		hasMore = hasMore || len(final) > end
	}

	var cursor *string
	if hasMore {
		c := encodeSearchCursor(offset + limit)
		cursor = &c
	}

	// Snippets: highlighted match context for the page's documents. total_hits and the
	// page selection are already fixed; this only decorates the returned rows.
	if req.Snippet != nil && len(results) > 0 {
		ids := make([]string, len(results))
		for i := range results {
			ids[i] = results[i].ID
		}
		snips, err := ix.fetchSnippets(s, ids, req.Query, req.Snippet)
		if err != nil {
			return nil, err
		}
		for i := range results {
			if m := snips[results[i].ID]; len(m) > 0 {
				results[i].Snippet = m
			}
		}
	}

	resp := &SearchResponse{
		TotalHits:      total,
		TotalHitsExact: exact,
		Returned:       len(results),
		Results:        results,
		Cursor:         cursor,
		RuleData:       applied.Data,
	}

	// Facets.
	facets, err := ix.discoverFacets(s, matchedSQL, matchedArgs, req)
	if err != nil {
		return nil, err
	}
	resp.Facets = facets
	return resp, nil
}

func projectFields(d Document, keep []string) Document {
	set := map[string]bool{}
	for _, k := range keep {
		set[k] = true
	}
	var out []Field
	for _, f := range d.Fields {
		if set[f.Name] {
			out = append(out, f)
		}
	}
	d.Fields = out
	return d
}

func (ix *Index) buildSort(sorts []SortSpec) (orderBy, joins string, args []any) {
	if len(sorts) == 0 {
		return "d.rank DESC, d.doc_id ASC", "", nil
	}
	var parts []string
	var joinParts []string
	ji := 0
	for _, s := range sorts {
		expr := strings.TrimSpace(s.Expr)
		dir := "ASC"
		switch expr {
		case "rank", "_rank", "":
			// rank defaults to descending
			if s.Desc || expr == "" || expr == "rank" || expr == "_rank" {
				dir = "DESC"
			}
			parts = append(parts, "d.rank "+dir)
		case "_score":
			// no bm25 in the emulator; fall back to rank
			parts = append(parts, "d.rank DESC")
		default:
			alias := fmt.Sprintf("sf%d", ji)
			ji++
			joinParts = append(joinParts, fmt.Sprintf(
				`LEFT JOIN (SELECT doc_id, MIN(num_val) mv FROM %s_fields WHERE name=? GROUP BY doc_id) %s ON %s.doc_id=d.doc_id`,
				ix.prefix, alias, alias))
			args = append(args, expr)
			if s.Desc {
				dir = "DESC"
			}
			parts = append(parts, fmt.Sprintf("COALESCE(%s.mv, %g) %s", alias, s.Default, dir))
		}
	}
	parts = append(parts, "d.doc_id ASC")
	return strings.Join(parts, ", "), strings.Join(joinParts, "\n"), args
}

func (ix *Index) discoverFacets(s *Store, matchedSQL string, matchedArgs []any, req SearchRequest) ([]FacetResult, error) {
	valueLimit := clamp(req.FacetValueLimit, 10, 1000)
	names := map[string]bool{}
	for _, n := range req.Facets {
		names[n] = true
	}
	discover := req.FacetDiscover > 0
	if !discover && len(names) == 0 {
		return nil, nil
	}

	// Restrict facet counting to matched docs.
	base := fmt.Sprintf("SELECT doc_id FROM (%s)", matchedSQL)

	// Discover atom facets: group by (name, text_val) over matched.
	q := fmt.Sprintf(`SELECT name, text_val, count(*) c FROM %s_facets
		WHERE kind=1 AND doc_id IN (%s)`, ix.prefix, base)
	args := append([]any{}, matchedArgs...)
	if !discover && len(names) > 0 {
		ph := make([]string, 0, len(names))
		for n := range names {
			ph = append(ph, "?")
			args = append(args, n)
		}
		q += " AND name IN (" + strings.Join(ph, ",") + ")"
	}
	q += " GROUP BY name, text_val ORDER BY name, c DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*FacetResult{}
	var orderNames []string
	for rows.Next() {
		var name, val string
		var c int
		if err := rows.Scan(&name, &val, &c); err != nil {
			return nil, err
		}
		fr := byName[name]
		if fr == nil {
			fr = &FacetResult{Name: name, Type: "atom"}
			byName[name] = fr
			orderNames = append(orderNames, name)
		}
		if len(fr.Values) < valueLimit {
			fr.Values = append(fr.Values, FacetValue{Value: val, Count: c})
		}
	}
	var out []FacetResult
	discoverLimit := req.FacetDiscover
	for _, name := range orderNames {
		if discover && !names[name] && discoverLimit <= 0 {
			continue
		}
		if discover && !names[name] {
			discoverLimit--
		}
		out = append(out, *byName[name])
	}
	return out, nil
}

func encodeSearchCursor(offset int) string {
	b, _ := json.Marshal(map[string]int{"o": offset})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSearchCursor(s string) (int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	var m map[string]int
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	o, ok := m["o"]
	return o, ok
}
