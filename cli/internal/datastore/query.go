package datastore

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	maxLimit     = 500
	defaultLimit = 25
	maxInValues  = 80
	maxJoins     = 5
	sqlVarLimit  = 90
)

// --- DSL types ---

type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type Order struct {
	Field string `json:"field"`
	Dir   string `json:"dir"`
}

type Join struct {
	As         string `json:"as"`
	Collection string `json:"collection"`
	LocalField string `json:"local_field"`
}

type QueryRequest struct {
	Where    []Filter `json:"where"`
	Order    []Order  `json:"order"`
	Limit    int      `json:"limit"`
	Cursor   string   `json:"cursor"`
	KeysOnly bool     `json:"keys_only"`
	Join     []Join   `json:"join"`
}

type QueryResult struct {
	Documents   []StoredDoc    `json:"documents,omitempty"`
	Keys        []string       `json:"keys,omitempty"`
	Cursor      *string        `json:"cursor"`
	RowsRead    int            `json:"rowsRead"`
	AutoIndexed *AutoIndexInfo `json:"auto_indexed,omitempty"`
}

type AutoIndexInfo struct {
	Fields      []string `json:"fields"`
	RowsWritten int      `json:"rows_written"`
}

var fieldSegRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// meta maps reserved selectors to physical columns.
var meta = map[string]string{"__key__": "key", "__created__": "created", "__updated__": "updated"}

// fieldExpr compiles a dot-path (or meta selector) to a SQL scalar expression.
func fieldExpr(field string) (string, error) {
	if col, ok := meta[field]; ok {
		return col, nil
	}
	segs := strings.Split(field, ".")
	if len(segs) == 0 || len(segs) > 8 {
		return "", common.BadRequest("invalid field path")
	}
	for _, s := range segs {
		if !fieldSegRe.MatchString(s) {
			return "", common.BadRequest("invalid field path segment: " + s)
		}
	}
	return "json_extract(data, '$." + strings.Join(segs, ".") + "')", nil
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, float64, bool, nil, json.Number:
		return true
	}
	return false
}

// scalarArg normalizes a filter value for binding (bool -> 1/0).
func scalarArg(v any) (any, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string, float64, json.Number:
		return t, nil
	}
	return nil, common.BadRequest("filter value must be scalar")
}

// compileFilters builds the WHERE fragment and args for the data filters.
func compileFilters(filters []Filter) (string, []any, error) {
	var clauses []string
	var args []any
	for _, f := range filters {
		expr, err := fieldExpr(f.Field)
		if err != nil {
			return "", nil, err
		}
		switch f.Op {
		case "=":
			if f.Value == nil {
				clauses = append(clauses, expr+" IS NULL")
			} else {
				a, err := scalarArg(f.Value)
				if err != nil {
					return "", nil, err
				}
				clauses = append(clauses, expr+" = ?")
				args = append(args, a)
			}
		case "!=":
			if f.Value == nil {
				clauses = append(clauses, expr+" IS NOT NULL")
			} else {
				a, err := scalarArg(f.Value)
				if err != nil {
					return "", nil, err
				}
				clauses = append(clauses, "("+expr+" IS NOT ? )")
				args = append(args, a)
			}
		case "<", "<=", ">", ">=":
			a, err := scalarArg(f.Value)
			if err != nil {
				return "", nil, err
			}
			clauses = append(clauses, expr+" "+f.Op+" ?")
			args = append(args, a)
		case "in":
			arr, ok := f.Value.([]any)
			if !ok || len(arr) == 0 {
				return "", nil, common.BadRequest("'in' requires a non-empty array")
			}
			if len(arr) > maxInValues {
				return "", nil, common.BadRequest(fmt.Sprintf("'in' accepts at most %d values", maxInValues))
			}
			ph := make([]string, 0, len(arr))
			for _, v := range arr {
				if !isScalar(v) {
					return "", nil, common.BadRequest("'in' values must be scalar")
				}
				a, _ := scalarArg(v)
				ph = append(ph, "?")
				args = append(args, a)
			}
			clauses = append(clauses, expr+" IN ("+strings.Join(ph, ",")+")")
		default:
			return "", nil, common.BadRequest("unsupported operator: " + f.Op)
		}
	}
	if len(args) > sqlVarLimit {
		return "", nil, common.NewError(400, "too many bound parameters", "TOO_MANY_PARAMS")
	}
	return strings.Join(clauses, " AND "), args, nil
}

type orderKey struct {
	field string
	expr  string
	desc  bool
}

// buildOrderKeys returns the ordering expressions plus a trailing key tiebreaker.
func buildOrderKeys(orders []Order) ([]orderKey, error) {
	var keys []orderKey
	trailingDesc := false
	for _, o := range orders {
		expr, err := fieldExpr(o.Field)
		if err != nil {
			return nil, err
		}
		desc := strings.EqualFold(o.Dir, "desc")
		trailingDesc = desc
		keys = append(keys, orderKey{field: o.Field, expr: expr, desc: desc})
	}
	// Stable pagination: always tiebreak by key in the trailing direction.
	hasKey := false
	for _, k := range keys {
		if k.field == "__key__" {
			hasKey = true
		}
	}
	if !hasKey {
		keys = append(keys, orderKey{field: "__key__", expr: "key", desc: trailingDesc})
	}
	return keys, nil
}

func encodeCursor(vals []any) (string, error) {
	b, err := json.Marshal(vals)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(s string, n int) ([]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, common.BadRequest("invalid cursor")
	}
	var vals []any
	if err := json.Unmarshal(raw, &vals); err != nil || len(vals) != n {
		return nil, common.BadRequest("invalid cursor")
	}
	return vals, nil
}

// cursorPredicate builds the lexicographic "after cursor" comparison for keyset paging.
func cursorPredicate(keys []orderKey, vals []any) (string, []any) {
	// (e1 cmp v1) OR (e1 = v1 AND e2 cmp v2) OR ...
	var ors []string
	var args []any
	for i := range keys {
		var terms []string
		for j := 0; j < i; j++ {
			terms = append(terms, "("+keys[j].expr+" IS ?)")
			args = append(args, vals[j])
		}
		cmp := ">"
		if keys[i].desc {
			cmp = "<"
		}
		terms = append(terms, "("+keys[i].expr+" "+cmp+" ?)")
		args = append(args, vals[i])
		ors = append(ors, "("+strings.Join(terms, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// --- index-served guard ---

type indexSpec struct {
	fields []string // field names, meta allowed
	unique bool
}

func parseFieldSpecName(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i]
	}
	return s
}

// loadIndexes reads declared indexes for a collection.
func (s *Store) loadIndexes(collection string) ([]indexSpec, error) {
	rows, err := s.db.Query(`SELECT fields, is_unique FROM _dsindexes WHERE collection=?`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indexSpec
	for rows.Next() {
		var fieldsJSON string
		var uniq int
		if err := rows.Scan(&fieldsJSON, &uniq); err != nil {
			return nil, err
		}
		var raw []string
		_ = json.Unmarshal([]byte(fieldsJSON), &raw)
		names := make([]string, len(raw))
		for i, r := range raw {
			names[i] = parseFieldSpecName(r)
		}
		if uniq == 0 {
			names = append(names, "__key__") // non-unique indexes implicitly append key
		}
		out = append(out, indexSpec{fields: names, unique: uniq != 0})
	}
	return out, nil
}

var equalityOps = map[string]bool{"=": true, "in": true}
var rangeOps = map[string]bool{"<": true, "<=": true, ">": true, ">=": true, "!=": true}

// requiredIndex computes the field sequence an index must cover to serve the query.
// Returns nil when no index is needed (bare listing / pure recency handled by built-ins).
func requiredIndex(filters []Filter, orders []Order) []string {
	if len(filters) == 0 && len(orders) == 0 {
		return nil
	}
	var eq, rng []string
	seen := map[string]bool{}
	for _, f := range filters {
		if equalityOps[f.Op] && !seen["=|"+f.Field] {
			seen["=|"+f.Field] = true
			eq = append(eq, f.Field)
		} else if rangeOps[f.Op] && !seen["r|"+f.Field] {
			seen["r|"+f.Field] = true
			rng = append(rng, f.Field)
		}
	}
	sort.Strings(eq)
	sort.Strings(rng)
	if len(orders) == 0 {
		// A single filtered field suffices for AND filters.
		if len(eq) > 0 {
			return []string{eq[0]}
		}
		return []string{rng[0]}
	}
	var req []string
	add := func(f string) {
		for _, x := range req {
			if x == f {
				return
			}
		}
		req = append(req, f)
	}
	for _, f := range eq {
		add(f)
	}
	orderFields := map[string]bool{}
	for _, o := range orders {
		orderFields[o.Field] = true
	}
	for _, f := range rng {
		if !orderFields[f] {
			add(f)
		}
	}
	for _, o := range orders {
		add(o.Field)
	}
	return req
}

func hasPrefix(index, req []string) bool {
	if len(req) > len(index) {
		return false
	}
	for i := range req {
		if index[i] != req[i] {
			return false
		}
	}
	return true
}

// builtinServes reports whether the emulator's built-in indexes cover req.
func builtinServes(req []string) bool {
	builtins := [][]string{{"__key__"}, {"__updated__", "__key__"}, {"__created__", "__key__"}}
	for _, b := range builtins {
		if hasPrefix(b, req) {
			return true
		}
	}
	return false
}

// indexServed reports whether the query is index-served; if not, returns the suggested
// index fields to create.
func (s *Store) indexServed(collection string, filters []Filter, orders []Order) (bool, []string, error) {
	req := requiredIndex(filters, orders)
	if req == nil {
		return true, nil, nil
	}
	if builtinServes(req) {
		return true, nil, nil
	}
	indexes, err := s.loadIndexes(collection)
	if err != nil {
		return false, nil, err
	}
	for _, ix := range indexes {
		if hasPrefix(ix.fields, req) {
			return true, nil, nil
		}
	}
	return false, req, nil
}

// --- query execution ---

// Query runs a QueryRequest. autoIndex controls whether an INDEX_REQUIRED failure
// auto-creates the suggested index and retries. readGroups carries the caller's row-level read
// rules as OR-of-AND groups: 0/1 group ANDs into the query, ≥2 groups run the multi-query
// (UNION) merge — each branch independently index-served.
func (s *Store) Query(collection string, req QueryRequest, autoIndex bool, readGroups [][]Filter) (*QueryResult, error) {
	if err := validateCollection(collection); err != nil {
		return nil, err
	}
	if len(req.Join) > maxJoins {
		return nil, common.BadRequest(fmt.Sprintf("at most %d joins", maxJoins))
	}
	// Every branch (each read group ANDed with the user's where) must be index-served.
	autoInfo, err := s.ensureServed(collection, branchWheres(req.Where, readGroups), req.Order, autoIndex)
	if err != nil {
		return nil, err
	}
	res, err := s.runQuery(collection, req, readGroups)
	if err != nil {
		return nil, err
	}
	res.AutoIndexed = autoInfo
	return res, nil
}

// branchWheres expands a base where + read groups into the effective where of each query branch.
// No groups -> a single branch (the base where); N groups -> N branches (base where AND group).
func branchWheres(where []Filter, groups [][]Filter) [][]Filter {
	if len(groups) == 0 {
		return [][]Filter{where}
	}
	out := make([][]Filter, len(groups))
	for i, g := range groups {
		b := make([]Filter, 0, len(where)+len(g))
		b = append(b, where...)
		b = append(b, g...)
		out[i] = b
	}
	return out
}

// ensureServed checks each branch is index-served, auto-creating the suggested index when
// autoIndex is on (else returns INDEX_REQUIRED for the first unserved branch). Returns the
// combined auto-index info (last suggested fields + summed rows) when anything was built.
func (s *Store) ensureServed(collection string, branches [][]Filter, order []Order, autoIndex bool) (*AutoIndexInfo, error) {
	var autoInfo *AutoIndexInfo
	for _, w := range branches {
		served, suggested, err := s.indexServed(collection, w, order)
		if err != nil {
			return nil, err
		}
		if served {
			continue
		}
		if !autoIndex {
			return nil, common.NewError(400, "query requires an index", "INDEX_REQUIRED").
				WithDetails(map[string]any{"suggested_index": suggested})
		}
		rw, err := s.CreateIndex(collection, suggested, false)
		if err != nil {
			return nil, err
		}
		if autoInfo == nil {
			autoInfo = &AutoIndexInfo{}
		}
		autoInfo.Fields = suggested
		autoInfo.RowsWritten += rw
	}
	return autoInfo, nil
}

// compileOrGroups compiles OR-of-AND groups into an inline ` AND ((g1) OR (g2) ...)` fragment
// (used by aggregates, which count each row once — a single WHERE disjunction is correct, no
// merge/de-dup). An empty group matches all. Returns "" for no groups.
func compileOrGroups(groups [][]Filter) (string, []any, error) {
	if len(groups) == 0 {
		return "", nil, nil
	}
	var parts []string
	var args []any
	for _, g := range groups {
		clause, a, err := compileFilters(g)
		if err != nil {
			return "", nil, err
		}
		if clause == "" {
			parts = append(parts, "1") // an empty group matches all
			continue
		}
		parts = append(parts, "("+clause+")")
		args = append(args, a...)
	}
	return " AND (" + strings.Join(parts, " OR ") + ")", args, nil
}

// compileSingleSelect builds the plain single-branch page query: SELECT key, data, created,
// updated, <order exprs> FROM docs WHERE collection AND <where> [AND cursor] ORDER BY <order>.
func compileSingleSelect(collection string, where []Filter, keys []orderKey, cursorVals []any, limit int) (string, []any, error) {
	whereSQL, args, err := compileFilters(where)
	if err != nil {
		return "", nil, err
	}
	full := []any{collection}
	conds := []string{"collection=?"}
	if whereSQL != "" {
		conds = append(conds, whereSQL)
		full = append(full, args...)
	}
	if cursorVals != nil {
		pred, cargs := cursorPredicate(keys, cursorVals)
		conds = append(conds, pred)
		full = append(full, cargs...)
	}
	sel := []string{"key", "data", "created", "updated"}
	var orderBy []string
	for _, k := range keys {
		sel = append(sel, k.expr)
		dir := "ASC"
		if k.desc {
			dir = "DESC"
		}
		orderBy = append(orderBy, k.expr+" "+dir)
	}
	query := "SELECT " + strings.Join(sel, ", ") + " FROM docs WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY " + strings.Join(orderBy, ", ") + fmt.Sprintf(" LIMIT %d", limit+1)
	return query, full, nil
}

// compileMergedSelect compiles an OR read policy (≥2 AND-groups) into ONE compound SELECT: each
// branch scans `where AND <group>`, the branches are UNION-ed (de-duping a row that matches
// several branches), then SQLite orders + limits the whole union — the multi-query merge in one
// round trip. Every branch shares the same ORDER BY and cursor predicate, so the projected order
// aliases (o0..oN) give a keyset cursor identical to the single-query path. Order expressions are
// aliased because a compound SELECT's ORDER BY must reference output columns.
func compileMergedSelect(collection string, where []Filter, groups [][]Filter, keys []orderKey, cursorVals []any, limit int) (string, []any, error) {
	var selExtra []string
	for i, k := range keys {
		selExtra = append(selExtra, k.expr+" AS o"+strconv.Itoa(i))
	}
	var cursorPred string
	var cursorArgs []any
	if cursorVals != nil {
		cursorPred, cursorArgs = cursorPredicate(keys, cursorVals)
	}
	var branches []string
	var full []any
	for _, g := range groups {
		bw := append(append([]Filter{}, where...), g...)
		whereSQL, args, err := compileFilters(bw)
		if err != nil {
			return "", nil, err
		}
		conds := []string{"collection=?"}
		full = append(full, collection)
		if whereSQL != "" {
			conds = append(conds, whereSQL)
			full = append(full, args...)
		}
		if cursorPred != "" {
			conds = append(conds, cursorPred)
			full = append(full, cursorArgs...)
		}
		branches = append(branches, "SELECT key, data, created, updated, "+strings.Join(selExtra, ", ")+
			" FROM docs WHERE "+strings.Join(conds, " AND "))
	}
	var orderBy []string
	for i, k := range keys {
		dir := "ASC"
		if k.desc {
			dir = "DESC"
		}
		orderBy = append(orderBy, "o"+strconv.Itoa(i)+" "+dir)
	}
	query := strings.Join(branches, " UNION ") + " ORDER BY " + strings.Join(orderBy, ", ") + fmt.Sprintf(" LIMIT %d", limit+1)
	return query, full, nil
}

func (s *Store) runQuery(collection string, req QueryRequest, readGroups [][]Filter) (*QueryResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	keys, err := buildOrderKeys(req.Order)
	if err != nil {
		return nil, err
	}
	var cursorVals0 []any
	if req.Cursor != "" {
		if cursorVals0, err = decodeCursor(req.Cursor, len(keys)); err != nil {
			return nil, err
		}
	}

	// 0/1 read group compiles to one plain SELECT (the single group ANDs into the where); ≥2
	// groups run the multi-query merge (UNION), each branch independently index-served (asserted
	// by the caller). Both shapes project key, data, created, updated + the order expressions.
	var query string
	var full []any
	if len(readGroups) <= 1 {
		where := req.Where
		if len(readGroups) == 1 {
			where = append(append([]Filter{}, req.Where...), readGroups[0]...)
		}
		query, full, err = compileSingleSelect(collection, where, keys, cursorVals0, limit)
	} else {
		query, full, err = compileMergedSelect(collection, req.Where, readGroups, keys, cursorVals0, limit)
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(query, full...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []StoredDoc
	var cursorVals []any
	rowsRead := 0
	nOrder := len(keys)
	for rows.Next() {
		rowsRead++
		scanDest := make([]any, 4+nOrder)
		var key, data string
		var created, updated int64
		scanDest[0], scanDest[1], scanDest[2], scanDest[3] = &key, &data, &created, &updated
		orderVals := make([]any, nOrder)
		for i := range orderVals {
			scanDest[4+i] = new(any)
		}
		if err := rows.Scan(scanDest...); err != nil {
			return nil, err
		}
		for i := range orderVals {
			orderVals[i] = *(scanDest[4+i].(*any))
		}
		if len(docs) < limit {
			docs = append(docs, StoredDoc{Key: key, Data: json.RawMessage(data), Created: created, Updated: updated})
			cursorVals = orderVals
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := rowsRead > limit
	var cursor *string
	if hasMore && len(cursorVals) > 0 {
		c, err := encodeCursor(normalizeCursorVals(cursorVals))
		if err != nil {
			return nil, err
		}
		cursor = &c
	}

	res := &QueryResult{Cursor: cursor, RowsRead: rowsRead}
	if req.KeysOnly {
		res.Keys = make([]string, len(docs))
		for i, d := range docs {
			res.Keys[i] = d.Key
		}
	} else {
		if err := s.enrichJoins(docs, req.Join); err != nil {
			return nil, err
		}
		res.Documents = docs
	}
	return res, nil
}

// normalizeCursorVals coerces driver-returned []byte to string for stable JSON round-trip.
func normalizeCursorVals(vals []any) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		if b, ok := v.([]byte); ok {
			out[i] = string(b)
		} else {
			out[i] = v
		}
	}
	return out
}

// enrichJoins attaches referenced documents by primary-key seek.
func (s *Store) enrichJoins(docs []StoredDoc, joins []Join) error {
	if len(joins) == 0 {
		return nil
	}
	for i := range docs {
		var top map[string]any
		if err := json.Unmarshal(docs[i].Data, &top); err != nil {
			continue
		}
		jm := map[string]any{}
		for _, j := range joins {
			if !fieldSegRe.MatchString(j.As) {
				return common.BadRequest("invalid join alias")
			}
			lv := lookupPath(top, j.LocalField)
			key, err := coerceKey(lv)
			if err != nil || key == "" {
				jm[j.As] = nil
				continue
			}
			ref, err := s.Get(j.Collection, key)
			if err != nil {
				jm[j.As] = nil
				continue
			}
			jm[j.As] = ref
		}
		docs[i].Joins = jm
	}
	return nil
}

func lookupPath(m map[string]any, path string) any {
	cur := any(m)
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[seg]
	}
	return cur
}

// --- indexes ---

// CreateIndex declares an index and, when unique, creates a backing SQLite unique index.
// Returns rows written (the one-time build cost, approximated by the collection size).
func (s *Store) CreateIndex(collection string, fields []string, unique bool) (int, error) {
	if err := validateCollection(collection); err != nil {
		return 0, err
	}
	if len(fields) == 0 {
		return 0, common.BadRequest("index requires at least one field")
	}
	fieldsJSON, _ := json.Marshal(fields)
	uniq := 0
	if unique {
		uniq = 1
	}
	res, err := s.db.Exec(`INSERT INTO _dsindexes(collection, fields, is_unique, created) VALUES(?,?,?,?)
		ON CONFLICT(collection, fields, is_unique) DO NOTHING`, collection, string(fieldsJSON), uniq, nowMS())
	if err != nil {
		return 0, err
	}
	if unique {
		if err := s.buildUniqueIndex(collection, fields); err != nil {
			return 0, err
		}
	}
	id, _ := res.LastInsertId()
	_ = id
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM docs WHERE collection=?`, collection).Scan(&n)
	return n, nil
}

func (s *Store) buildUniqueIndex(collection string, fields []string) error {
	var exprs []string
	for _, f := range fields {
		name := parseFieldSpecName(f)
		e, err := fieldExpr(name)
		if err != nil {
			return err
		}
		exprs = append(exprs, e)
	}
	name := "u_" + sanitize(collection) + "_" + fmt.Sprintf("%x", strings.Join(fields, ","))
	if len(name) > 60 {
		name = name[:60]
	}
	stmt := fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS %q ON docs(collection, %s) WHERE collection=%s`,
		name, strings.Join(exprs, ", "), quoteLit(collection))
	if _, err := s.db.Exec(stmt); err != nil {
		return mapSQLiteErr(err)
	}
	return nil
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

type IndexRow struct {
	ID         int64    `json:"id"`
	Collection string   `json:"collection"`
	Fields     []string `json:"fields"`
	Unique     bool     `json:"unique"`
}

func (s *Store) ListIndexes(collection string) ([]IndexRow, error) {
	rows, err := s.db.Query(`SELECT id, fields, is_unique FROM _dsindexes WHERE collection=? ORDER BY id`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		var fieldsJSON string
		var uniq int
		if err := rows.Scan(&r.ID, &fieldsJSON, &uniq); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(fieldsJSON), &r.Fields)
		r.Collection = collection
		r.Unique = uniq != 0
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) DropIndex(collection string, id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM _dsindexes WHERE collection=? AND id=?`, collection, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Collections lists distinct collection names in the namespace.
func (s *Store) Collections() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT collection FROM docs ORDER BY collection`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func billableReads(rowsRead int) int {
	u := rowsRead / rowsPerReadUnit
	if u < 1 {
		u = 1
	}
	return u
}

var _ = sql.ErrNoRows
