package search

import (
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	sqlite "modernc.org/sqlite"
)

// Register a haversine(lat1,lng1,lat2,lng2) -> meters function so geo distance()
// queries compile straight to SQL with the same semantics as the hosted service.
func init() {
	sqlite.MustRegisterScalarFunction("haversine", 4, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		toF := func(v any) float64 {
			switch t := v.(type) {
			case int64:
				return float64(t)
			case float64:
				return t
			}
			return 0
		}
		lat1, lng1, lat2, lng2 := toF(args[0]), toF(args[1]), toF(args[2]), toF(args[3])
		const R = 6371000.0
		p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
		dp := (lat2 - lat1) * math.Pi / 180
		dl := (lng2 - lng1) * math.Pi / 180
		a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
		return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a)), nil
	})
}

// --- lexer ---

type tokKind int

const (
	tWord tokKind = iota
	tPhrase
	tOp     // : = != < <= > >=
	tLParen // (
	tRParen // )
	tComma  // ,
	tMinus  // - (negation)
	tTilde  // ~
	tEOF
)

type token struct {
	kind tokKind
	text string
}

func isDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '(', ')', ',', '<', '>', '=', '!', ':', '~':
		return true
	}
	return false
}

func lex(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, common.BadRequest("unterminated phrase")
			}
			toks = append(toks, token{tPhrase, b.String()})
			i = j + 1
		case c == '(':
			toks = append(toks, token{tLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")"})
			i++
		case c == ',':
			toks = append(toks, token{tComma, ","})
			i++
		case c == '~':
			toks = append(toks, token{tTilde, "~"})
			i++
		case c == ':':
			toks = append(toks, token{tOp, ":"})
			i++
		case c == '=':
			toks = append(toks, token{tOp, "="})
			i++
		case c == '!':
			if i+1 < len(s) && s[i+1] == '=' {
				toks = append(toks, token{tOp, "!="})
				i += 2
			} else {
				i++ // stray !
			}
		case c == '<' || c == '>':
			if i+1 < len(s) && s[i+1] == '=' {
				toks = append(toks, token{tOp, string(c) + "="})
				i += 2
			} else {
				toks = append(toks, token{tOp, string(c)})
				i++
			}
		case c == '-':
			// Negative number if immediately followed by a digit and previous token
			// isn't a word/value; otherwise treat as negation operator.
			if i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
				j := i + 1
				for j < len(s) && !isDelim(s[j]) {
					j++
				}
				toks = append(toks, token{tWord, s[i:j]})
				i = j
			} else {
				toks = append(toks, token{tMinus, "-"})
				i++
			}
		default:
			j := i
			for j < len(s) && !isDelim(s[j]) {
				j++
			}
			toks = append(toks, token{tWord, s[i:j]})
			i = j
		}
	}
	toks = append(toks, token{tEOF, ""})
	return toks, nil
}

// --- parser + compiler ---

// setSQL is a doc_id set expression: a SELECT yielding a doc_id column, plus args.
type setSQL struct {
	sql  string
	args []any
}

type parser struct {
	toks   []token
	pos    int
	ix     *Index
	schema map[string]map[string]bool
	depth  int
}

const maxQueryLen = 2048

// Compile parses a query string into a doc_id set expression for this index. An empty
// query compiles to nil (match-all).
func (ix *Index) Compile(query string) (*setSQL, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if len(query) > maxQueryLen {
		return nil, common.BadRequest("query too long")
	}
	schema, err := ix.schemaTypes()
	if err != nil {
		return nil, err
	}
	toks, err := lex(query)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, ix: ix, schema: schema}
	set, err := p.orExpr("")
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tEOF {
		return nil, common.BadRequest("unexpected token: " + p.cur().text)
	}
	return set, nil
}

func (p *parser) cur() token  { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) orExpr(field string) (*setSQL, error) {
	left, err := p.andExpr(field)
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tWord && p.cur().text == "OR" {
		p.next()
		right, err := p.andExpr(field)
		if err != nil {
			return nil, err
		}
		left = union(left, right)
	}
	return left, nil
}

func (p *parser) andExpr(field string) (*setSQL, error) {
	left, err := p.unary(field)
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		if t.kind == tEOF || t.kind == tRParen {
			break
		}
		if t.kind == tWord && t.text == "OR" {
			break
		}
		if t.kind == tWord && t.text == "AND" {
			p.next()
		}
		right, err := p.unary(field)
		if err != nil {
			return nil, err
		}
		left = intersect(left, right)
	}
	return left, nil
}

func (p *parser) unary(field string) (*setSQL, error) {
	t := p.cur()
	if (t.kind == tWord && t.text == "NOT") || t.kind == tMinus {
		p.next()
		child, err := p.unary(field)
		if err != nil {
			return nil, err
		}
		return negate(p.ix, child), nil
	}
	return p.primary(field)
}

func (p *parser) primary(field string) (*setSQL, error) {
	p.depth++
	if p.depth > 100 {
		return nil, common.BadRequest("query too deeply nested")
	}
	defer func() { p.depth-- }()

	t := p.cur()
	if t.kind == tLParen {
		p.next()
		set, err := p.orExpr(field)
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tRParen {
			return nil, common.BadRequest("expected )")
		}
		p.next()
		return set, nil
	}

	// distance(field, geopoint(lat,lng)) op num
	if t.kind == tWord && t.text == "distance" && p.peek(1).kind == tLParen {
		return p.geo()
	}

	// field op value  |  field:(subexpr)
	if t.kind == tWord && p.peek(1).kind == tOp {
		fname := t.text
		opTok := p.toks[p.pos+1] // the operator token after the field
		if opTok.text == ":" && p.peek(2).kind == tLParen {
			// scoped subexpression
			p.next() // field
			p.next() // :
			p.next() // (
			set, err := p.orExpr(fname)
			if err != nil {
				return nil, err
			}
			if p.cur().kind != tRParen {
				return nil, common.BadRequest("expected )")
			}
			p.next()
			return set, nil
		}
		p.next() // field
		p.next() // op
		return p.fieldValue(fname, opTok.text)
	}

	// bare / global term (respecting an outer field scope)
	return p.term(field)
}

func (p *parser) peek(n int) token {
	i := p.pos + n
	if i >= len(p.toks) {
		return token{tEOF, ""}
	}
	return p.toks[i]
}

// term parses a value token as a term scoped to `field` ("" = global).
func (p *parser) term(field string) (*setSQL, error) {
	stem := false
	if p.cur().kind == tTilde {
		p.next()
		stem = true
	}
	t := p.cur()
	switch t.kind {
	case tWord:
		p.next()
		w := t.text
		prefix := false
		if strings.HasSuffix(w, "*") {
			prefix = true
			w = strings.TrimSuffix(w, "*")
		}
		return p.compileTermExpanded(field, w, prefix, stem, false), nil
	case tPhrase:
		p.next()
		return p.compileTermExpanded(field, t.text, false, stem, true), nil
	}
	return nil, common.BadRequest("unexpected token in query: " + t.text)
}

// --- leaf compilation ---

func ftsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// compileTermExpanded compiles a term PLUS its synonym/numeral alternatives from the
// instance config (additive OR — the original term is always one branch, so expansion
// can only widen). Expanding at the leaf means `NOT laptop` naturally excludes the
// synonyms too (the negation wraps the expanded set). No expansion for prefix/stem
// terms or on numeric/untokenized fields, matching the hosted engine's gating; `!=`
// deliberately calls compileTerm directly and is never expanded.
func (p *parser) compileTermExpanded(field, word string, prefix, stem, phrase bool) *setSQL {
	base := p.compileTerm(field, word, prefix, stem, phrase)
	if prefix || stem || p.ix == nil || p.ix.syn == nil {
		return base
	}
	if field != "" {
		switch p.fieldType(field) {
		case "number", "date", "untokenprefix":
			return base
		}
	}
	for _, alt := range p.ix.synAlts(word) {
		base = union(base, p.compileTerm(field, alt, false, false, strings.Contains(alt, " ")))
	}
	return base
}

// compileTerm builds a set for a term in a text-ish context.
func (p *parser) compileTerm(field, word string, prefix, stem, phrase bool) *setSQL {
	pfx := p.ix.prefix
	ftsTable := pfx + "_fts"
	if stem && p.ix.stemming {
		ftsTable = pfx + "_fts_stem"
	}
	var match string
	switch {
	case phrase:
		match = ftsQuote(word)
	case prefix:
		match = ftsQuote(word) + "*"
	default:
		match = ftsQuote(word)
	}
	if field != "" {
		// Field-scoped: FTS on that field name, OR atom/untokenprefix exact/prefix.
		ftype := p.fieldType(field)
		switch {
		case ftype == "atom":
			return &setSQL{fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type='atom' AND text_lc=?`, pfx),
				[]any{field, strings.ToLower(word)}}
		case ftype == "untokenprefix":
			if prefix {
				return &setSQL{fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type='untokenprefix' AND text_lc LIKE ?`, pfx),
					[]any{field, strings.ToLower(word) + "%"}}
			}
			return &setSQL{fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type='untokenprefix' AND text_lc=?`, pfx),
				[]any{field, strings.ToLower(word)}}
		case ftype == "number" || ftype == "date":
			// equality on a numeric field
			n, _ := strconv.ParseFloat(word, 64)
			return &setSQL{fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type IN ('number','date') AND num_val=?`, pfx),
				[]any{field, n}}
		default:
			return &setSQL{fmt.Sprintf(`SELECT doc_id FROM %s WHERE %s MATCH ? AND name=?`, ftsTable, ftsTable),
				[]any{match, field}}
		}
	}
	// Global term: any tokenized field OR any atom exact.
	return &setSQL{
		fmt.Sprintf(`SELECT doc_id FROM %s WHERE %s MATCH ? UNION SELECT doc_id FROM %s_fields WHERE type='atom' AND text_lc=?`,
			ftsTable, ftsTable, pfx),
		[]any{match, strings.ToLower(word)},
	}
}

// fieldType returns the primary declared type for a field, or "".
func (p *parser) fieldType(field string) string {
	types := p.schema[field]
	if types == nil {
		return ""
	}
	for _, t := range []string{"atom", "untokenprefix", "number", "date", "geo", "text", "html", "tokenprefix"} {
		if types[t] {
			return t
		}
	}
	return ""
}

// fieldValue compiles `field op value` for comparison/equality operators.
func (p *parser) fieldValue(field, op string) (*setSQL, error) {
	pfx := p.ix.prefix
	// value token
	stem := false
	if p.cur().kind == tTilde {
		p.next()
		stem = true
	}
	vt := p.cur()
	if vt.kind != tWord && vt.kind != tPhrase {
		return nil, common.BadRequest("expected a value after " + op)
	}
	p.next()
	val := vt.text

	switch op {
	case ":", "=":
		prefix := false
		w := val
		if vt.kind == tWord && strings.HasSuffix(w, "*") {
			prefix = true
			w = strings.TrimSuffix(w, "*")
		}
		return p.compileTermExpanded(field, w, prefix, stem, vt.kind == tPhrase), nil
	case "!=":
		eq := p.compileTerm(field, val, false, stem, vt.kind == tPhrase)
		return negate(p.ix, eq), nil
	case "<", "<=", ">", ">=":
		n, err := parseNumOrDate(val)
		if err != nil {
			return nil, err
		}
		return &setSQL{
			fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type IN ('number','date') AND num_val %s ?`, pfx, op),
			[]any{field, n},
		}, nil
	}
	return nil, common.BadRequest("unsupported operator: " + op)
}

func parseNumOrDate(s string) (float64, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, nil
	}
	// date YYYY-MM-DD or RFC3339
	ms, err := parseDate([]byte(`"` + s + `"`))
	if err != nil {
		return 0, common.BadRequest("invalid numeric/date value: " + s)
	}
	return float64(ms), nil
}

func (p *parser) geo() (*setSQL, error) {
	pfx := p.ix.prefix
	// distance ( field , geopoint ( lat , lng ) ) op num
	p.next() // distance
	if p.next().kind != tLParen {
		return nil, common.BadRequest("expected ( after distance")
	}
	fnameTok := p.next()
	if fnameTok.kind != tWord {
		return nil, common.BadRequest("expected geo field name")
	}
	if p.next().kind != tComma {
		return nil, common.BadRequest("expected , in distance()")
	}
	gp := p.next()
	if gp.kind != tWord || gp.text != "geopoint" {
		return nil, common.BadRequest("expected geopoint()")
	}
	if p.next().kind != tLParen {
		return nil, common.BadRequest("expected ( after geopoint")
	}
	latTok := p.next()
	if p.next().kind != tComma {
		return nil, common.BadRequest("expected , in geopoint()")
	}
	lngTok := p.next()
	if p.next().kind != tRParen {
		return nil, common.BadRequest("expected ) after geopoint args")
	}
	if p.next().kind != tRParen {
		return nil, common.BadRequest("expected ) after distance args")
	}
	opTok := p.next()
	if opTok.kind != tOp || (opTok.text != "<" && opTok.text != "<=" && opTok.text != ">" && opTok.text != ">=") {
		return nil, common.BadRequest("distance() requires a comparison operator")
	}
	distTok := p.next()
	lat, err1 := strconv.ParseFloat(latTok.text, 64)
	lng, err2 := strconv.ParseFloat(lngTok.text, 64)
	dist, err3 := strconv.ParseFloat(distTok.text, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, common.BadRequest("invalid geo numbers")
	}
	return &setSQL{
		fmt.Sprintf(`SELECT doc_id FROM %s_fields WHERE name=? AND type='geo' AND haversine(lat,lng,?,?) %s ?`, pfx, opTok.text),
		[]any{fnameTok.text, lat, lng, dist},
	}, nil
}

// --- set combinators ---

func wrap(s *setSQL) string { return "SELECT doc_id FROM (" + s.sql + ")" }

func intersect(a, b *setSQL) *setSQL {
	return &setSQL{wrap(a) + " INTERSECT " + wrap(b), append(append([]any{}, a.args...), b.args...)}
}

func union(a, b *setSQL) *setSQL {
	return &setSQL{wrap(a) + " UNION " + wrap(b), append(append([]any{}, a.args...), b.args...)}
}

func negate(ix *Index, a *setSQL) *setSQL {
	universe := fmt.Sprintf("SELECT doc_id FROM %s_docs", ix.prefix)
	return &setSQL{universe + " EXCEPT " + wrap(a), append([]any{}, a.args...)}
}
