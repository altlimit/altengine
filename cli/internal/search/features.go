package search

import (
	"fmt"
	"html"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Per-request result-shaping features: snippets (highlighted match context) and field
// collapsing (distinct). Both mirror the hosted service's semantics.

const (
	defaultSnippetTokens = 32
	maxSnippetTokens     = 64
	maxCollapseLimit     = 10
	// The collapse candidate window: like a field sort, collapse is exact within the
	// top-`collapseSortLimit` by rank and approximate beyond, so the work stays bounded.
	collapseSortLimit = 1000
	// Sentinel marks handed to FTS5 snippet() so the returned content can be HTML-escaped
	// (FTS5 does not escape) WITHOUT escaping our own tags — then the sentinels are swapped
	// for the real pre/post tags. Keeps a <script> in a document from becoming live markup.
	snipMarkPre  = "\x02"
	snipMarkPost = "\x03"
)

// collectMatchText builds an FTS MATCH expression from the highlightable terms in a query
// (words + phrases, minus boolean keywords and field names). Used only to locate snippet
// context: over-collecting is harmless because snippet() only marks tokens that actually
// occur in the content.
func collectMatchText(query string) string {
	toks, err := lex(query)
	if err != nil {
		return ""
	}
	var terms []string
	for i, t := range toks {
		switch t.kind {
		case tWord:
			switch t.text {
			case "OR", "AND", "NOT", "distance", "geopoint":
				continue
			}
			// A word immediately followed by an operator is a field name, not a term.
			if i+1 < len(toks) && toks[i+1].kind == tOp {
				continue
			}
			w := strings.TrimSuffix(t.text, "*")
			if w != "" {
				terms = append(terms, ftsQuote(w))
			}
		case tPhrase:
			terms = append(terms, ftsQuote(t.text))
		}
	}
	return strings.Join(terms, " OR ")
}

func snippetDefaults(req *SnippetReq) (tokens int, ell string) {
	tokens = req.MaxTokens
	if tokens <= 0 {
		tokens = defaultSnippetTokens
	}
	if tokens > maxSnippetTokens {
		tokens = maxSnippetTokens
	}
	ell = req.Ellipsis
	if ell == "" {
		ell = "…" // …
	}
	return
}

// fetchSnippets returns docID -> field -> highlighted snippet for the page's documents.
// With no `fields`, only the single best-matching field per document is kept.
func (ix *Index) fetchSnippets(s *Store, docIDs []string, query string, req *SnippetReq) (map[string]map[string]string, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}
	match := collectMatchText(query)
	if match == "" {
		return nil, nil
	}
	tokens, ell := snippetDefaults(req)
	pre, post := req.PreTag, req.PostTag
	if pre == "" {
		pre = "<b>"
	}
	if post == "" {
		post = "</b>"
	}

	// snippet() params appear in the SELECT list, so they bind BEFORE the MATCH/IN
	// params in the WHERE clause (SQLite binds left-to-right by position in the text).
	args := []any{snipMarkPre, snipMarkPost, ell, tokens, match}
	ph := make([]string, len(docIDs))
	for i, id := range docIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	q := fmt.Sprintf(`SELECT doc_id, name, bm25(%s_fts) AS b, snippet(%s_fts, 0, ?, ?, ?, ?) AS s
		FROM %s_fts WHERE content MATCH ? AND doc_id IN (%s)`,
		ix.prefix, ix.prefix, ix.prefix, strings.Join(ph, ","))
	if len(req.Fields) > 0 {
		fph := make([]string, len(req.Fields))
		for i, f := range req.Fields {
			fph[i] = "?"
			args = append(args, f)
		}
		q += " AND name IN (" + strings.Join(fph, ",") + ")"
	}
	q += " ORDER BY b" // best (lowest bm25) first, so best-field selection is trivial

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	wantAll := len(req.Fields) > 0
	for rows.Next() {
		var docID, name, snip string
		var b float64
		if err := rows.Scan(&docID, &name, &b, &snip); err != nil {
			return nil, err
		}
		// Escape the FTS content, then substitute the real tags for the sentinels.
		snip = html.EscapeString(snip)
		snip = strings.ReplaceAll(snip, snipMarkPre, pre)
		snip = strings.ReplaceAll(snip, snipMarkPost, post)
		m := out[docID]
		if m == nil {
			m = map[string]string{}
			out[docID] = m
		}
		if wantAll {
			if _, seen := m[name]; !seen {
				m[name] = snip
			}
		} else if len(m) == 0 {
			// best-field only: rows are bm25-ordered, so the first per doc wins.
			m[name] = snip
		}
	}
	return out, rows.Err()
}

// collapsePlan is a validated field-collapsing request.
type collapsePlan struct {
	field      string
	valCol     string // text_lc | num_val
	typeClause string // SQL predicate selecting the field's rows
	perGroup   int
}

// planCollapse validates req.Collapse against the schema. Returns nil when no collapse is
// requested. Collapse needs an atom/number field and (v1) cannot combine with sort/scorer.
func planCollapse(req *SearchRequest, schema map[string]map[string]bool) (*collapsePlan, error) {
	c := req.Collapse
	if c == nil {
		return nil, nil
	}
	if strings.TrimSpace(c.Field) == "" {
		return nil, common.BadRequest("collapse.field is required")
	}
	types := schema[c.Field]
	if types == nil {
		return nil, common.BadRequest("collapse.field is not an indexed field: " + c.Field)
	}
	var plan collapsePlan
	switch {
	case types["atom"]:
		plan.valCol, plan.typeClause = "text_lc", "type='atom'"
	case types["number"] || types["date"]:
		plan.valCol, plan.typeClause = "num_val", "type IN ('number','date')"
	default:
		return nil, common.BadRequest("collapse.field must be an atom or number field")
	}
	if len(req.Sort) > 0 || (req.Scorer != "" && req.Scorer != "none") {
		return nil, common.BadRequest("collapse cannot be combined with sort or scorer")
	}
	plan.field = c.Field
	plan.perGroup = c.Limit
	if plan.perGroup <= 0 {
		plan.perGroup = 1
	}
	if plan.perGroup > maxCollapseLimit {
		plan.perGroup = maxCollapseLimit
	}
	return &plan, nil
}

// collapseQuery builds the page query that keeps the top `perGroup` docs per distinct
// value. Documents lacking the field each become their own group (a 'd'||doc_id partition
// key, disjoint from the 'v'||value keys). total_hits and facets are computed separately
// and stay UNCOLLAPSED — collapse only reshapes the page.
func (ix *Index) collapseQuery(matchedSQL string, matchedArgs []any, plan *collapsePlan, limit, offset int) (string, []any) {
	p := ix.prefix
	cand := fmt.Sprintf(`SELECT d.doc_id, d.rank, d.body FROM %s_docs d JOIN (%s) m ON m.doc_id=d.doc_id ORDER BY d.rank DESC LIMIT %d`,
		p, matchedSQL, collapseSortLimit)
	gv := fmt.Sprintf(`SELECT doc_id, MIN(%s) AS gval FROM %s_fields WHERE name=? AND %s GROUP BY doc_id`,
		plan.valCol, p, plan.typeClause)
	sql := fmt.Sprintf(`WITH cand AS (%s), g AS (
		SELECT c.doc_id, c.rank, c.body,
			ROW_NUMBER() OVER (PARTITION BY CASE WHEN gv.doc_id IS NULL THEN 'd'||c.doc_id ELSE 'v'||gv.gval END
				ORDER BY c.rank DESC, c.doc_id ASC) AS rn
		FROM cand c LEFT JOIN (%s) gv ON gv.doc_id=c.doc_id)
	SELECT doc_id, rank, body FROM g WHERE rn <= %d ORDER BY rank DESC, doc_id ASC LIMIT %d OFFSET %d`,
		cand, gv, plan.perGroup, limit+1, offset)
	args := append(append([]any{}, matchedArgs...), plan.field)
	return sql, args
}
