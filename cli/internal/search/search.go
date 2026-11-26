package search

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// SortSpec is one sort key. Default is the value for a document without the field: a
// string sorts an atom field, a number a number/date field; omitted, the schema decides.
type SortSpec struct {
	Expr string `json:"expr"`
	// Desc is a pointer because absent and false differ, as hosted: rank sorts
	// descending unless desc is false; a field sorts ascending unless desc is true.
	Desc    *bool `json:"desc"`
	Default any   `json:"default"`
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
	Limit             *int         `json:"limit"` // nil = 20; 0 = counts and facets only
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
	limit := 20
	if req.Limit != nil {
		if *req.Limit < 0 {
			return nil, common.BadRequest("limit must be a non-negative number")
		}
		limit = min(*req.Limit, 1000)
	}
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
		orderBy, sortJoins, sortArgs, err := ix.buildSort(req.Sort)
		if err != nil {
			return nil, err
		}
		mainSQL = fmt.Sprintf(`SELECT d.doc_id, d.rank, d.body FROM %s_docs d
			JOIN (%s) m ON m.doc_id = d.doc_id
			%s
			ORDER BY %s
			LIMIT %d OFFSET %d`,
			ix.prefix, matchedSQL, sortJoins, orderBy, fetchLimit+1, fetchOffset)
		args = append(append([]any{}, matchedArgs...), sortArgs...)
	}

	// `limit: 0` asks for counts and facets only, as hosted: no page query runs. The sort
	// was still resolved above, so a bad sort default is a 400 either way.
	var results []SearchResult
	hasMore := false
	if limit > 0 {
		rows, err := s.db.Query(mainSQL, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		n := 0
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
	}
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
	if hasMore && limit > 0 {
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

// sortDefault validates a sort default as hosted: absent, a finite number, or a string no
// longer than an atom value can be.
func sortDefault(v any) (any, error) {
	switch d := v.(type) {
	case nil:
		return nil, nil
	case string:
		if len(d) > maxAtomBytes {
			return nil, common.BadRequest(fmt.Sprintf("sort default exceeds %d bytes", maxAtomBytes))
		}
		return d, nil
	case float64:
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return nil, common.BadRequest("sort default must be a finite number")
		}
		return d, nil
	}
	return nil, common.BadRequest("sort default must be a number or a string")
}

// buildSort resolves each sort key as hosted: a string default sorts as atom (by the
// lowercased value, so atoms order case-insensitively, as they match), a number as
// numeric; with no default the field's schema decides, numeric unless the field is atom
// only. A default whose type the field never has is a 400.
func (ix *Index) buildSort(sorts []SortSpec) (orderBy, joins string, args []any, err error) {
	if len(sorts) == 0 {
		return "d.rank DESC, d.doc_id ASC", "", nil, nil
	}
	var schema map[string]map[string]bool
	var parts, joinParts []string
	var joinArgs, orderArgs []any
	ji := 0
	for _, s := range sorts {
		expr := strings.TrimSpace(s.Expr)
		dir := "ASC"
		switch expr {
		case "rank", "_rank", "":
			// rank defaults to descending; desc: false sorts it ascending
			if s.Desc == nil || *s.Desc {
				dir = "DESC"
			}
			parts = append(parts, "d.rank "+dir)
		case "_score":
			// no bm25 in the emulator; fall back to rank
			parts = append(parts, "d.rank DESC")
		default:
			def, err := sortDefault(s.Default)
			if err != nil {
				return "", "", nil, err
			}
			if schema == nil {
				if schema, err = ix.schemaTypes(); err != nil {
					return "", "", nil, err
				}
			}
			types := schema[expr]
			atom := types["atom"]
			numeric := types["number"] || types["date"]
			asAtom := false
			switch d := def.(type) {
			case string:
				if numeric && !atom {
					return "", "", nil, common.BadRequest(fmt.Sprintf("sort default for number field '%s' must be a number", expr))
				}
				asAtom, def = true, strings.ToLower(d)
			case float64:
				if atom && !numeric {
					return "", "", nil, common.BadRequest(fmt.Sprintf("sort default for atom field '%s' must be a string", expr))
				}
			default:
				asAtom = atom && !numeric
				if asAtom {
					def = ""
				} else {
					def = float64(0)
				}
			}
			key := "MIN(num_val) mv FROM %s_fields WHERE name=? AND type IN ('number','date')"
			if asAtom {
				key = "MIN(text_lc) mv FROM %s_fields WHERE name=? AND type='atom'"
			}
			alias := fmt.Sprintf("sf%d", ji)
			ji++
			joinParts = append(joinParts, fmt.Sprintf(
				`LEFT JOIN (SELECT doc_id, `+key+` GROUP BY doc_id) %s ON %s.doc_id=d.doc_id`,
				ix.prefix, alias, alias))
			joinArgs = append(joinArgs, expr)
			if s.Desc != nil && *s.Desc {
				dir = "DESC"
			}
			parts = append(parts, fmt.Sprintf("COALESCE(%s.mv, ?) %s", alias, dir))
			orderArgs = append(orderArgs, def)
		}
	}
	parts = append(parts, "d.doc_id ASC")
	return strings.Join(parts, ", "), strings.Join(joinParts, "\n"), append(joinArgs, orderArgs...), nil
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
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Number facets: one [min, max) range + count per name, as hosted.
	nq := fmt.Sprintf(`SELECT name, MIN(num_val), MAX(num_val), count(*) c FROM %s_facets
		WHERE kind=2 AND doc_id IN (%s)`, ix.prefix, base)
	nargs := append([]any{}, matchedArgs...)
	if !discover && len(names) > 0 {
		ph := make([]string, 0, len(names))
		for n := range names {
			ph = append(ph, "?")
			nargs = append(nargs, n)
		}
		nq += " AND name IN (" + strings.Join(ph, ",") + ")"
	}
	nq += " GROUP BY name ORDER BY name"
	nrows, err := s.db.Query(nq, nargs...)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var name string
		var lo, hi float64
		var c int
		if err := nrows.Scan(&name, &lo, &hi, &c); err != nil {
			return nil, err
		}
		if byName[name] != nil {
			continue
		}
		max := numberFacetMax(hi)
		byName[name] = &FacetResult{Name: name, Type: "number", Values: []FacetValue{{
			Value: jsNumber(lo) + "-" + jsNumber(hi), Count: c, Min: &lo, Max: &max,
		}}}
		orderNames = append(orderNames, name)
	}
	if err := nrows.Err(); err != nil {
		return nil, err
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

// numberFacetMax is the `max` of a number facet's range: one past the top value. A
// refinement is half-open [min, max), so max must sit strictly above the top value or
// refining on the range drops every document AT it; hi+1 admits nothing the count did not.
// Past 2^53 hi+1 == hi, so the next double is taken instead.
func numberFacetMax(hi float64) float64 {
	if hi+1 > hi {
		return hi + 1
	}
	return math.Nextafter(hi, math.Inf(1))
}

// jsNumber formats a number the way the hosted service's runtime prints one.
func jsNumber(f float64) string {
	if a := math.Abs(f); a != 0 && (a >= 1e21 || a < 1e-6) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		return strings.Replace(strings.Replace(s, "e+0", "e+", 1), "e-0", "e-", 1)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
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
