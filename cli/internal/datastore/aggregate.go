package datastore

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

type Metric struct {
	Fn    string `json:"fn"`
	Field string `json:"field"`
	As    string `json:"as"`
}

type AggregateRequest struct {
	Where   []Filter `json:"where"`
	Group   []string `json:"group"`
	Metrics []Metric `json:"metrics"`
	Order   []Order  `json:"order"`
	Limit   int      `json:"limit"`
}

type AggGroup struct {
	Group   map[string]any     `json:"group"`
	Metrics map[string]float64 `json:"metrics"`
}

type AggregateResult struct {
	Groups      []AggGroup     `json:"groups"`
	AutoIndexed *AutoIndexInfo `json:"auto_indexed,omitempty"`
}

var aggFns = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true}

// Aggregate runs a grouped metrics query. Group dimensions are guarded like sort keys. orGroups
// carries the caller's row-level read rules as OR-of-AND groups: 0/1 group ANDs into the where,
// ≥2 groups inject an inline `AND ((g1) OR (g2))` — aggregates count each row once, so no
// merge/de-dup is needed (unlike a query). Each branch is independently index-served.
func (s *Store) Aggregate(collection string, req AggregateRequest, autoIndex bool, orGroups [][]Filter) (*AggregateResult, error) {
	if err := validateCollection(collection); err != nil {
		return nil, err
	}
	if len(req.Metrics) == 0 {
		return nil, common.BadRequest("aggregate requires at least one metric")
	}
	// Guard: group dimensions behave like an ordering requirement. Each read-group branch
	// (its filters ANDed with the user's where) must be independently index-served.
	var groupAsOrder []Order
	for _, g := range req.Group {
		groupAsOrder = append(groupAsOrder, Order{Field: g})
	}
	var autoInfo *AutoIndexInfo
	for _, w := range branchWheres(req.Where, orGroups) {
		served, suggested, err := s.indexServed(collection, w, groupAsOrder)
		if err != nil {
			return nil, err
		}
		if served {
			continue
		}
		if !autoIndex {
			return nil, common.NewError(400, "aggregate requires an index", "INDEX_REQUIRED").
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

	// 0/1 group ANDs into the base where; ≥2 groups add the inline OR fragment after it.
	effWhere := req.Where
	if len(orGroups) == 1 {
		effWhere = append(append([]Filter{}, req.Where...), orGroups[0]...)
	}
	whereSQL, args, err := compileFilters(effWhere)
	if err != nil {
		return nil, err
	}
	var orSQL string
	var orArgs []any
	if len(orGroups) >= 2 {
		if orSQL, orArgs, err = compileOrGroups(orGroups); err != nil {
			return nil, err
		}
	}

	// Build metric select expressions + aliases.
	var selMetrics []string
	aliases := make([]string, len(req.Metrics))
	for i, m := range req.Metrics {
		if !aggFns[m.Fn] {
			return nil, common.BadRequest("unsupported aggregate fn: " + m.Fn)
		}
		alias := m.As
		if !fieldSegRe.MatchString(alias) {
			alias = fmt.Sprintf("m%d", i)
		}
		aliases[i] = alias
		if m.Fn == "count" {
			selMetrics = append(selMetrics, "COUNT(*) AS "+quoteIdent(alias))
			continue
		}
		if m.Field == "" {
			return nil, common.BadRequest(m.Fn + " requires a field")
		}
		fe, err := fieldExpr(m.Field)
		if err != nil {
			return nil, err
		}
		selMetrics = append(selMetrics, strings.ToUpper(m.Fn)+"("+fe+") AS "+quoteIdent(alias))
	}

	// Group dimension expressions.
	var groupExprs []string
	groupAlias := make([]string, len(req.Group))
	var selGroups []string
	for i, g := range req.Group {
		fe, err := fieldExpr(g)
		if err != nil {
			return nil, err
		}
		alias := fmt.Sprintf("g%d", i)
		groupAlias[i] = alias
		groupExprs = append(groupExprs, fe)
		selGroups = append(selGroups, fe+" AS "+quoteIdent(alias))
	}

	sel := append([]string{}, selGroups...)
	sel = append(sel, selMetrics...)
	sql := "SELECT " + strings.Join(sel, ", ") + " FROM docs WHERE collection=?"
	full := []any{collection}
	if whereSQL != "" {
		sql += " AND " + whereSQL
		full = append(full, args...)
	}
	if orSQL != "" {
		sql += orSQL
		full = append(full, orArgs...)
	}
	if len(groupExprs) > 0 {
		sql += " GROUP BY " + strings.Join(groupExprs, ", ")
	}
	// Order by group alias or metric alias.
	if len(req.Order) > 0 {
		var ob []string
		validAlias := map[string]bool{}
		for _, a := range groupAlias {
			validAlias[a] = true
		}
		for i, g := range req.Group {
			validAlias[g] = true
			_ = i
		}
		aliasByGroupField := map[string]string{}
		for i, g := range req.Group {
			aliasByGroupField[g] = groupAlias[i]
		}
		metricAlias := map[string]bool{}
		for _, a := range aliases {
			metricAlias[a] = true
		}
		for _, o := range req.Order {
			dir := "ASC"
			if strings.EqualFold(o.Dir, "desc") {
				dir = "DESC"
			}
			target := o.Field
			if a, ok := aliasByGroupField[o.Field]; ok {
				target = a
			} else if !metricAlias[o.Field] {
				return nil, common.BadRequest("order must reference a group field or metric alias")
			}
			ob = append(ob, quoteIdent(target)+" "+dir)
		}
		sql += " ORDER BY " + strings.Join(ob, ", ")
	}
	if req.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", req.Limit)
	}

	rows, err := s.db.Query(sql, full...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var groups []AggGroup
	for rows.Next() {
		dest := make([]any, len(cols))
		for i := range dest {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		g := AggGroup{Group: map[string]any{}, Metrics: map[string]float64{}}
		for i, c := range cols {
			v := *(dest[i].(*any))
			if strings.HasPrefix(c, "g") && i < len(req.Group) {
				g.Group[req.Group[i]] = normScalar(v)
			} else {
				g.Metrics[c] = toFloat(v)
			}
		}
		groups = append(groups, g)
	}
	if groups == nil {
		groups = []AggGroup{}
	}
	return &AggregateResult{Groups: groups, AutoIndexed: autoInfo}, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func normScalar(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	case []byte:
		var f float64
		fmt.Sscanf(string(t), "%g", &f)
		return f
	}
	return 0
}

// --- transactions ---

type TxnOp struct {
	Op         string             `json:"op"`
	Collection string             `json:"collection"`
	Key        any                `json:"key"`
	Data       json.RawMessage    `json:"data"`
	Set        map[string]any     `json:"set"`
	Increment  map[string]float64 `json:"increment"`
	Remove     []string           `json:"remove"`
	Upsert     bool               `json:"upsert"`
	Exists     bool               `json:"exists"`
}

// RunTransaction applies operations atomically within the namespace.
func (s *Store) RunTransaction(ops []TxnOp) ([]any, int, error) {
	if len(ops) > maxTxnOps {
		return nil, 0, common.BadRequest(fmt.Sprintf("too many operations (max %d)", maxTxnOps))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	keys := make([]any, len(ops))
	written := 0
	now := nowMS()
	for i, op := range ops {
		if err := validateCollection(op.Collection); err != nil {
			return nil, 0, err
		}
		switch op.Op {
		case "check":
			key, _ := coerceKey(op.Key)
			var exists bool
			var n int
			tx.QueryRow(`SELECT count(*) FROM docs WHERE collection=? AND key=?`, op.Collection, key).Scan(&n)
			exists = n > 0
			if exists != op.Exists {
				return nil, 0, common.Precondition("check precondition failed")
			}
		case "delete":
			key, _ := coerceKey(op.Key)
			if _, err := tx.Exec(`DELETE FROM docs WHERE collection=? AND key=?`, op.Collection, key); err != nil {
				return nil, 0, err
			}
			written++
		case "put":
			if !isJSONObject(op.Data) {
				return nil, 0, common.BadRequest("put data must be a JSON object")
			}
			key, err := coerceKey(op.Key)
			if err != nil {
				return nil, 0, err
			}
			if key == "" {
				if key, err = s.genKeyTx(tx, op.Collection); err != nil {
					return nil, 0, err
				}
			}
			if _, err := tx.Exec(`INSERT INTO docs(collection,key,data,size,created,updated) VALUES(?,?,?,?,?,?)
				ON CONFLICT(collection,key) DO UPDATE SET data=excluded.data, size=excluded.size, updated=excluded.updated`,
				op.Collection, key, string(op.Data), len(op.Data), now, now); err != nil {
				return nil, 0, mapSQLiteErr(err)
			}
			keys[i] = key
			written++
		case "mutate":
			key, err := coerceKey(op.Key)
			if err != nil || key == "" {
				return nil, 0, common.BadRequest("mutate requires a key")
			}
			var cur string
			var created int64
			err = tx.QueryRow(`SELECT data, created FROM docs WHERE collection=? AND key=?`, op.Collection, key).Scan(&cur, &created)
			exists := err == nil
			var doc map[string]any
			if exists {
				_ = json.Unmarshal([]byte(cur), &doc)
			} else {
				if !op.Upsert {
					return nil, 0, common.Precondition("mutate target does not exist")
				}
				doc = map[string]any{}
				created = now
			}
			applyMutate(doc, op)
			nb, _ := json.Marshal(doc)
			if _, err := tx.Exec(`INSERT INTO docs(collection,key,data,size,created,updated) VALUES(?,?,?,?,?,?)
				ON CONFLICT(collection,key) DO UPDATE SET data=excluded.data, size=excluded.size, updated=excluded.updated`,
				op.Collection, key, string(nb), len(nb), created, now); err != nil {
				return nil, 0, mapSQLiteErr(err)
			}
			keys[i] = key
			written++
		default:
			return nil, 0, common.BadRequest("unsupported transaction op: " + op.Op)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, mapSQLiteErr(err)
	}
	return keys, written, nil
}

func applyMutate(doc map[string]any, op TxnOp) {
	for _, path := range op.Remove {
		deletePath(doc, path)
	}
	for path, v := range op.Set {
		setPath(doc, path, v)
	}
	for path, delta := range op.Increment {
		cur := lookupPath(doc, path)
		base := 0.0
		if f, ok := cur.(float64); ok {
			base = f
		}
		setPath(doc, path, base+delta)
	}
}

func setPath(doc map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := doc
	for i := 0; i < len(segs)-1; i++ {
		next, ok := cur[segs[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[segs[i]] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

func deletePath(doc map[string]any, path string) {
	segs := strings.Split(path, ".")
	cur := doc
	for i := 0; i < len(segs)-1; i++ {
		next, ok := cur[segs[i]].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, segs[len(segs)-1])
}
