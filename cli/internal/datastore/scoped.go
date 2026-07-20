package datastore

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/identity"
)

// Row-scoped storage operations: the write/read paths taken when the caller is an END-USER
// identity token rather than an org API key. The rule engine (internal/identity) resolves
// an auth instance's row rules into concrete constraints; this file enforces them against
// real rows:
//
//	PutScoped        create -> stamp server-set fields, then validate the resulting doc
//	                 update -> the EXISTING row must satisfy `match`; then `immutable`
//	                           fields are compared against the CLIENT's document; only
//	                           then are stamp fields applied (stamping first would make
//	                           a stamped+immutable field always compare equal, voiding
//	                           the guard)
//	DeleteScoped     the EXISTING row must satisfy `match`
//	BatchGetScoped   a point-read can't be scoped in SQL, so rows the caller may not read
//	                 are filtered out server-side (omitted, never leaked)
//
// Stamping is what makes authorship forge-proof: the value comes from the verified token,
// overwriting whatever the client sent.

// ToFilters converts rule-engine filters into datastore filters.
func ToFilters(fs []identity.Filter) []Filter {
	if len(fs) == 0 {
		return nil
	}
	out := make([]Filter, 0, len(fs))
	for _, f := range fs {
		out = append(out, Filter{Field: f.Field, Op: f.Op, Value: f.Value})
	}
	return out
}

// LiveDoc is a committed document change, carried to the live bridge.
type LiveDoc struct {
	Key  string
	Data json.RawMessage
}

// PutScoped inserts or replaces documents under a create/update rule policy, returning the
// committed keys and their final (post-stamp) bodies.
func (s *Store) PutScoped(collection string, docs []PutDoc, create, update identity.WritePolicy) ([]string, []LiveDoc, error) {
	if err := validateCollection(collection); err != nil {
		return nil, nil, err
	}
	if len(docs) > maxDocsPerBatch {
		return nil, nil, common.BadRequest("too many documents")
	}
	createMatch := ToFilters(create.Match)
	updateMatch := ToFilters(update.Match)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	keys := make([]string, 0, len(docs))
	live := make([]LiveDoc, 0, len(docs))
	now := nowMS()
	for _, d := range docs {
		if !isJSONObject(d.Data) {
			return nil, nil, common.BadRequest("document data must be a JSON object")
		}
		if len(d.Data) > maxDocBytes {
			return nil, nil, common.NewError(400, "document exceeds 1MiB", "DOCUMENT_TOO_LARGE")
		}
		var doc map[string]any
		if err := json.Unmarshal(d.Data, &doc); err != nil {
			return nil, nil, common.BadRequest("document data must be a JSON object")
		}
		key, err := coerceKey(d.Key)
		if err != nil {
			return nil, nil, err
		}

		// Look the row up first: an upsert can't know create-vs-update until it reads.
		var existing map[string]any
		var created, updated int64
		if key != "" {
			var raw string
			err := tx.QueryRow(`SELECT data, created, updated FROM docs WHERE collection=? AND key=?`,
				collection, key).Scan(&raw, &created, &updated)
			if err == nil {
				existing = map[string]any{}
				_ = json.Unmarshal([]byte(raw), &existing)
			} else if err != sql.ErrNoRows {
				return nil, nil, err
			}
		}

		if existing != nil {
			if update.Denied {
				return nil, nil, common.PermissionDenied("not permitted: update on '" + collection + "'")
			}
			if !matchDoc(existing, key, created, updated, updateMatch) {
				return nil, nil, common.PermissionDenied("not permitted: update on '" + collection + "' (row does not match the rule)")
			}
			// Immutable is compared against the CLIENT's document, BEFORE stamping.
			// Stamping first would make a field that is both stamped and immutable
			// always compare equal, silently voiding the guard.
			for _, field := range update.Immutable {
				// `created` is server-preserved on upsert, so it can never differ by
				// client action; only data fields are guarded here.
				if field == "__created__" {
					continue
				}
				if !valuesEqual(lookupField(existing, field), lookupField(doc, field)) {
					return nil, nil, common.PermissionDenied("not permitted: field '" + field + "' is immutable")
				}
			}
			applyStamp(doc, update.Stamp)
		} else {
			if create.Denied {
				return nil, nil, common.PermissionDenied("not permitted: create on '" + collection + "'")
			}
			applyStamp(doc, create.Stamp)
			if key == "" {
				if key, err = s.genKeyTx(tx, collection); err != nil {
					return nil, nil, err
				}
			}
			if !matchDoc(doc, key, now, now, createMatch) {
				return nil, nil, common.PermissionDenied("not permitted: create on '" + collection + "' (document does not match the rule)")
			}
		}

		body, err := json.Marshal(doc)
		if err != nil {
			return nil, nil, common.BadRequest("document data must be a JSON object")
		}
		if _, err := tx.Exec(
			`INSERT INTO docs(collection, key, data, size, created, updated) VALUES(?,?,?,?,?,?)
			 ON CONFLICT(collection, key) DO UPDATE SET data=excluded.data, size=excluded.size, updated=excluded.updated`,
			collection, key, string(body), len(body), now, now); err != nil {
			return nil, nil, mapSQLiteErr(err)
		}
		keys = append(keys, key)
		live = append(live, LiveDoc{Key: key, Data: body})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, mapSQLiteErr(err)
	}
	return keys, live, nil
}

// DeleteScoped removes documents whose EXISTING row satisfies the delete rule's match.
// A row that doesn't match is refused outright (rather than silently skipped) so a client
// gets a clear 403 instead of a confusing "deleted: 0".
func (s *Store) DeleteScoped(collection string, keys []any, match []Filter) (int, error) {
	if len(keys) > maxKeysPerDelete {
		return 0, common.BadRequest("too many keys")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	total := 0
	for _, k := range keys {
		sk, err := coerceKey(k)
		if err != nil {
			return 0, err
		}
		var raw string
		var created, updated int64
		err = tx.QueryRow(`SELECT data, created, updated FROM docs WHERE collection=? AND key=?`, collection, sk).
			Scan(&raw, &created, &updated)
		if err == sql.ErrNoRows {
			continue // nothing to delete (and nothing to leak about)
		}
		if err != nil {
			return 0, err
		}
		doc := map[string]any{}
		_ = json.Unmarshal([]byte(raw), &doc)
		if !matchDoc(doc, sk, created, updated, match) {
			return 0, common.PermissionDenied("not permitted: delete on '" + collection + "' (row does not match the rule)")
		}
		res, err := tx.Exec(`DELETE FROM docs WHERE collection=? AND key=?`, collection, sk)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// BatchGetScoped is BatchGet with the caller's read filters applied server-side.
func (s *Store) BatchGetScoped(collection string, keys []any, filters []Filter) ([]StoredDoc, error) {
	docs, err := s.BatchGet(collection, keys)
	if err != nil {
		return nil, err
	}
	if len(filters) == 0 {
		return docs, nil
	}
	out := make([]StoredDoc, 0, len(docs))
	for _, d := range docs {
		m := map[string]any{}
		_ = json.Unmarshal(d.Data, &m)
		if matchDoc(m, d.Key, d.Created, d.Updated, filters) {
			out = append(out, d)
		}
	}
	return out, nil
}

// --- in-Go filter evaluation (the point-read / write-guard counterpart of the SQL compiler) ---

// lookupField resolves a filter field against a decoded document, honoring the same
// dot-paths the SQL compiler supports. Meta selectors are handled by matchDoc.
func lookupField(doc map[string]any, field string) any {
	var cur any = doc
	for _, seg := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// matchDoc reports whether a decoded document satisfies every filter.
func matchDoc(doc map[string]any, key string, created, updated int64, filters []Filter) bool {
	for _, f := range filters {
		var actual any
		switch f.Field {
		case "__key__":
			actual = key
		case "__created__":
			actual = float64(created)
		case "__updated__":
			actual = float64(updated)
		default:
			actual = lookupField(doc, f.Field)
		}
		if !matchOne(actual, f.Op, f.Value) {
			return false
		}
	}
	return true
}

func matchOne(actual any, op string, want any) bool {
	switch op {
	case "=":
		return valuesEqual(actual, want)
	case "!=":
		return !valuesEqual(actual, want)
	case "in":
		arr, ok := want.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			if valuesEqual(actual, v) {
				return true
			}
		}
		return false
	case "<", "<=", ">", ">=":
		c, ok := compareValues(actual, want)
		if !ok {
			return false
		}
		switch op {
		case "<":
			return c < 0
		case "<=":
			return c <= 0
		case ">":
			return c > 0
		default:
			return c >= 0
		}
	}
	return false
}

func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if c, ok := compareValues(a, b); ok {
		return c == 0
	}
	if ab, ok := a.(bool); ok {
		bb, ok2 := b.(bool)
		return ok2 && ab == bb
	}
	return false
}

// compareValues orders two JSON scalars of the same kind. Cross-kind comparisons report
// false so a mismatched type never satisfies a rule.
func compareValues(a, b any) (int, bool) {
	if an, ok := toNumber(a); ok {
		if bn, ok := toNumber(b); ok {
			switch {
			case an < bn:
				return -1, true
			case an > bn:
				return 1, true
			default:
				return 0, true
			}
		}
		return 0, false
	}
	as, ok1 := a.(string)
	bs, ok2 := b.(string)
	if ok1 && ok2 {
		return strings.Compare(as, bs), true
	}
	return 0, false
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// applyStamp overwrites server-set fields on a document from the verified identity. A
// dotted field name writes into (and creates) the nested object it names.
func applyStamp(doc map[string]any, stamp map[string]any) {
	for field, value := range stamp {
		segs := strings.Split(field, ".")
		cur := doc
		for i := 0; i < len(segs)-1; i++ {
			next, ok := cur[segs[i]].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[segs[i]] = next
			}
			cur = next
		}
		cur[segs[len(segs)-1]] = value
	}
}
