package datastore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
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

// ToGroups converts rule-engine OR-of-AND groups into datastore filter groups.
func ToGroups(gs [][]identity.Filter) [][]Filter {
	if len(gs) == 0 {
		return nil
	}
	out := make([][]Filter, len(gs))
	for i, g := range gs {
		out[i] = ToFilters(g)
	}
	return out
}

// ToFieldGroups converts a rule-engine field->groups map (masks / write gates) into datastore form.
func ToFieldGroups(m map[string][][]identity.Filter) map[string][][]Filter {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][][]Filter, len(m))
	for k, gs := range m {
		out[k] = ToGroups(gs)
	}
	return out
}

// LiveDoc is a committed document change, carried to the live bridge.
type LiveDoc struct {
	Key  string
	Data json.RawMessage
}

// serialIDRetries is how many times a scoped put re-draws a serial id that landed on a taken
// key before giving up.
const serialIDRetries = 4

// freeAutoKeyTx draws the key for a keyless scoped put. A `serial` id can land on a key
// somebody wrote by hand, or one named elsewhere in the batch; a doc sent without a key is a
// create, so it must neither overwrite that row nor be judged by its owner's update rule. It
// takes the next id instead, up to serialIDRetries times, then *serialExhausted.
func (s *Store) freeAutoKeyTx(tx *sql.Tx, collection string, named map[string]bool) (string, error) {
	for round := 0; ; round++ {
		key, err := s.genKeyTx(tx, collection)
		if err != nil || (s.autoID != "serial" && s.autoID != "incrementing") {
			return key, err
		}
		taken := named[key]
		if !taken {
			var one int
			switch err := tx.QueryRow(`SELECT 1 FROM docs WHERE collection=? AND key=?`, collection, key).Scan(&one); err {
			case nil:
				taken = true
			case sql.ErrNoRows:
			default:
				return "", err
			}
		}
		if !taken {
			return key, nil
		}
		if round == serialIDRetries {
			last, _ := strconv.ParseInt(key, 10, 64)
			return "", &serialExhausted{collection: collection, last: last}
		}
	}
}

// serialExhausted is a keyless scoped put whose serial draws all landed on taken keys. last is
// the highest id drawn: the draws are spent even though the write is not, so the next put
// starts past them instead of re-drawing the same taken run forever.
type serialExhausted struct {
	collection string
	last       int64
}

func (e *serialExhausted) Error() string {
	return "could not allocate a free document id in '" + e.collection + "' — retry, or supply keys"
}

// spendSerial persists a sequence advance a rolled-back write drew, outside that write.
func (s *Store) spendSerial(e *serialExhausted) error {
	_, err := s.db.Exec(
		`INSERT INTO _dsseq(collection, next) VALUES(?, ?)
		 ON CONFLICT(collection) DO UPDATE SET next = max(next, excluded.next)`,
		e.collection, e.last)
	return err
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
	createMatch := ToGroups(create.Match)
	updateMatch := ToGroups(update.Match)
	createFieldWrites := ToFieldGroups(create.FieldWrites)
	updateFieldWrites := ToFieldGroups(update.FieldWrites)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	// Keys this batch names itself: a serial id must not land on one, wherever it sits.
	named := map[string]bool{}
	for _, d := range docs {
		if k, err := coerceKey(d.Key); err == nil && k != "" {
			named[k] = true
		}
	}

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
			if !matchGroups(existing, key, created, updated, updateMatch) {
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
				if !jsonEqual(lookupField(existing, field), lookupField(doc, field)) {
					return nil, nil, common.PermissionDenied("not permitted: field '" + field + "' is immutable")
				}
			}
			// Per-field write gates: a field may only CHANGE when the EXISTING row satisfies its
			// write rule (evaluated on the row as it is — "may I change this field on this row").
			// A server-stamped field is authoritative, so it's exempt from the client's gate.
			// Checked against the CLIENT's document, BEFORE stamping, like the immutable guard.
			for field, groups := range updateFieldWrites {
				if _, stamped := update.Stamp[field]; stamped {
					continue
				}
				if !jsonEqual(lookupField(existing, field), lookupField(doc, field)) &&
					!matchGroups(existing, key, created, updated, groups) {
					return nil, nil, common.PermissionDenied("not permitted: change field '" + field + "' on '" + collection + "'")
				}
			}
			applyStamp(doc, update.Stamp)
		} else {
			if create.Denied {
				return nil, nil, common.PermissionDenied("not permitted: create on '" + collection + "'")
			}
			applyStamp(doc, create.Stamp)
			if key == "" {
				if key, err = s.freeAutoKeyTx(tx, collection, named); err != nil {
					var ex *serialExhausted
					if errors.As(err, &ex) {
						_ = tx.Rollback()
						if err := s.spendSerial(ex); err != nil {
							return nil, nil, err
						}
						return nil, nil, common.AlreadyExists(ex.Error())
					}
					return nil, nil, err
				}
			}
			if !matchGroups(doc, key, now, now, createMatch) {
				return nil, nil, common.PermissionDenied("not permitted: create on '" + collection + "' (document does not match the rule)")
			}
			// Per-field write gates on create: a (non-stamped) field may only be SET when the new
			// doc satisfies its write rule. Lets a collection be creatable by anyone but reserve a
			// field (e.g. `pinned`) to docs meeting a condition.
			for field, groups := range createFieldWrites {
				if _, stamped := create.Stamp[field]; stamped {
					continue
				}
				if _, present := doc[field]; present && !matchGroups(doc, key, now, now, groups) {
					return nil, nil, common.PermissionDenied("not permitted: set field '" + field + "' on '" + collection + "'")
				}
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
func (s *Store) DeleteScoped(collection string, keys []any, match [][]Filter) (int, error) {
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
		if !matchGroups(doc, sk, created, updated, match) {
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

// BatchGetScoped is BatchGet with the caller's row-level read groups AND per-field read masks
// applied server-side: a row the caller may not read is omitted (never leaked), and a masked
// field is projected out of the rows they can read.
func (s *Store) BatchGetScoped(collection string, keys []any, groups [][]Filter, fieldReads map[string][][]Filter) ([]StoredDoc, error) {
	docs, err := s.BatchGet(collection, keys)
	if err != nil {
		return nil, err
	}
	var admitted []StoredDoc
	if len(groups) == 0 {
		admitted = docs
	} else {
		admitted = make([]StoredDoc, 0, len(docs))
		for _, d := range docs {
			m := map[string]any{}
			_ = json.Unmarshal(d.Data, &m)
			if matchGroups(m, d.Key, d.Created, d.Updated, groups) {
				admitted = append(admitted, d)
			}
		}
	}
	return MaskFields(admitted, fieldReads), nil
}

// matchGroups evaluates OR-of-AND groups (disjunctive normal form) against a decoded document.
// Each inner group is an AND (via matchDoc); the doc matches if it satisfies ANY group. No groups
// means unconstrained → true (org key / public / authenticated / rules-less).
func matchGroups(doc map[string]any, key string, created, updated int64, groups [][]Filter) bool {
	if len(groups) == 0 {
		return true
	}
	for _, g := range groups {
		if matchDoc(doc, key, created, updated, g) {
			return true
		}
	}
	return false
}

// MaskFields projects restricted fields OUT of docs the caller may read at the row level: for
// each field in fieldReads, remove it from a doc that doesn't satisfy that field's mask groups
// (nil groups = always visible). Returns NEW StoredDocs only where a field was actually removed
// — the input docs are never mutated. Fields are top-level; each field's mask is evaluated
// against the doc's ORIGINAL data (removals are decided before any deletion is applied).
func MaskFields(docs []StoredDoc, fieldReads map[string][][]Filter) []StoredDoc {
	if len(fieldReads) == 0 {
		return docs
	}
	out := make([]StoredDoc, len(docs))
	for i, d := range docs {
		data, ok := decodeFaithful(d.Data)
		if !ok {
			out[i] = d // non-object / null / undecodable → return verbatim (nothing to mask)
			continue
		}
		var remove []string
		for field, groups := range fieldReads {
			if _, present := data[field]; present && !matchGroups(data, d.Key, d.Created, d.Updated, groups) {
				remove = append(remove, field)
			}
		}
		if len(remove) == 0 {
			out[i] = d
			continue
		}
		for _, f := range remove {
			delete(data, f)
		}
		nb, err := encodeFaithful(data)
		if err != nil {
			out[i] = d
			continue
		}
		nd := d
		nd.Data = json.RawMessage(nb)
		out[i] = nd
	}
	return out
}

// decodeFaithful unmarshals a JSON object using json.Number so large integers survive a
// re-encode without being coerced to float64 (and losing precision above 2^53). Returns
// ok=false for a non-object or undecodable body.
func decodeFaithful(raw json.RawMessage) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil || data == nil {
		return nil, false
	}
	return data, true
}

// encodeFaithful marshals a masked document WITHOUT Go's default HTML escaping, so `<`, `>`
// and `&` in string values round-trip byte-for-byte (matching the stored/hosted representation
// — otherwise a masked doc would render `<` while its unmasked neighbors keep `<`).
func encodeFaithful(data map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil // Encoder appends a trailing newline
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

// metaTypeMismatch reports whether f compares a meta selector with a value (or, for `in`, any
// element) of a type other than its column's. SQL converts a filter value to the column's
// affinity — `key` is TEXT, `created`/`updated` INTEGER, so `__key__ > 5` compares as text —
// and the in-Go matcher does not, so such a filter could admit a row here that a query
// refuses. It is denied instead.
func metaTypeMismatch(f Filter) bool {
	wantString := false
	switch f.Field {
	case "__key__":
		wantString = true
	case "__created__", "__updated__":
	default:
		return false
	}
	vals := []any{f.Value}
	if arr, ok := f.Value.([]any); ok && f.Op == "in" {
		vals = arr
	}
	for _, v := range vals {
		if wantString {
			if _, ok := v.(string); !ok {
				return true
			}
		} else if _, ok := toNumber(v); !ok {
			return true
		}
	}
	return false
}

// matchDoc reports whether a decoded document satisfies every filter.
func matchDoc(doc map[string]any, key string, created, updated int64, filters []Filter) bool {
	for _, f := range filters {
		if metaTypeMismatch(f) {
			return false
		}
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

// matchOne evaluates one filter as the SQL compiler's query would, so a rule admits the same
// rows on a write or point read as on a query. Values are seen as SQLite sees them (booleans
// are 1/0, an object or array is its JSON text, a missing value is NULL); two values of
// different storage classes order by class — numbers before text — and text compares by
// byte (UTF-8 code point) order. The one deliberate difference from SQL: `!=` admits a
// missing field, where SQL's `<>` is NULL for it.
func matchOne(actual any, op string, want any) bool {
	eq := func(c int) bool { return c == 0 }
	switch op {
	case "=":
		if want == nil {
			return actual == nil
		}
		return sqlHolds(actual, want, eq)
	case "!=":
		if want == nil {
			return actual != nil
		}
		return !(actual != nil && sqlHolds(actual, want, eq))
	case "in":
		arr, ok := want.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			if sqlHolds(actual, v, eq) {
				return true
			}
		}
		return false
	case "<":
		return sqlHolds(actual, want, func(c int) bool { return c < 0 })
	case "<=":
		return sqlHolds(actual, want, func(c int) bool { return c <= 0 })
	case ">":
		return sqlHolds(actual, want, func(c int) bool { return c > 0 })
	case ">=":
		return sqlHolds(actual, want, func(c int) bool { return c >= 0 })
	}
	return false
}

// sqlValue is a value as SQLite sees it: a number (float64), text (string), or NULL (nil).
func sqlValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool:
		if t {
			return float64(1)
		}
		return float64(0)
	case string:
		return t
	case map[string]any, []any:
		b, err := encodeFaithful(map[string]any{"v": t})
		if err != nil {
			return nil
		}
		// encodeFaithful wraps in an object to share its no-HTML-escape encoder; unwrap.
		return string(b[len(`{"v":`) : len(b)-1])
	}
	if n, ok := toNumber(v); ok {
		return n
	}
	return nil
}

// sqlCompare is SQLite's comparison of two non-NULL values with no column affinity.
func sqlCompare(a, b any) int {
	an, aNum := a.(float64)
	bn, bNum := b.(float64)
	switch {
	case aNum && bNum:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aNum:
		return -1 // numbers order before text
	case bNum:
		return 1
	}
	return strings.Compare(a.(string), b.(string)) // BINARY collation: UTF-8 byte order
}

// sqlHolds is `actual <op> want` as SQL evaluates it: false when either side is NULL.
func sqlHolds(actual, want any, test func(int) bool) bool {
	a, b := sqlValue(actual), sqlValue(want)
	return a != nil && b != nil && test(sqlCompare(a, b))
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

// jsonEqual compares two decoded JSON values structurally, so an object or array resent
// unchanged is equal to what is stored (valuesEqual compares scalars only).
func jsonEqual(a, b any) bool {
	switch at := a.(type) {
	case map[string]any:
		bt, ok := b.(map[string]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for k, av := range at {
			bv, ok := bt[k]
			if !ok || !jsonEqual(av, bv) {
				return false
			}
		}
		return true
	case []any:
		bt, ok := b.([]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for i := range at {
			if !jsonEqual(at[i], bt[i]) {
				return false
			}
		}
		return true
	}
	return valuesEqual(a, b)
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
