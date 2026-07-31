package identity

import (
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Row-level rule engine. An auth instance's `access` config turns into concrete,
// identity-scoped constraints the datastore and channel data planes enforce:
//
//	read            -> filters ANDed into the query/aggregate/point-read
//	create          -> `stamp` fields overwritten from the verified token (forge-proof
//	                   authorship) plus optional `match` validation of the resulting doc
//	update / delete -> a `match` guard the EXISTING row must satisfy, plus (update) an
//	                   `immutable` field set that may not change
//
// The security crux: `$auth.*` placeholders resolve ONLY from the verified token, never
// from the request body, and a placeholder that cannot be resolved is a hard 403 — a rule
// must never silently degrade into an unconstrained match. An API-key caller (no end-user
// identity) bypasses rules entirely: it is a trusted backend. An identity whose access
// entry carries no `rules` gets level-only access. When `rules` IS present every operation
// is default-deny: a collection or mode that isn't listed is refused.

// Filter mirrors the datastore filter DSL. It is redeclared here (rather than imported)
// so this package stays free of any data-plane dependency; the datastore converts.
type Filter struct {
	Field string
	Op    string
	Value any
}

// RuleFilter is a filter as stored in config, whose `value` may be a `$auth.*` placeholder.
type RuleFilter struct {
	Field string
	Op    string
	Value any
}

// Match is a row condition in disjunctive normal form: OR-of-AND groups. A flat filter list
// (wire `[...]`) is one AND-group; `{ any: [[...], ...] }` is the groups directly. A row
// satisfies a Match if it satisfies ANY group (each group is an AND of its filters). An empty
// Match (`nil`) is unconstrained (matches everything) — the public/authenticated/org-key case.
type Match [][]RuleFilter

// maxMatchGroups bounds an OR disjunction so the multi-query merge stays a small, bounded
// compound SELECT. Matches the hosted service's limit so a rule that stores there stores here.
const maxMatchGroups = 4

// CollectionRules is the per-collection rule set for a datastore target.
type CollectionRules struct {
	// Read is "public", "authenticated", a Match, or absent (deny).
	ReadPolicy string // "public" | "authenticated" | "match" | "" (absent => deny)
	ReadMatch  Match  // set when ReadPolicy == "match"
	Create     *WriteRule
	Update     *WriteRule
	Delete     *WriteRule
	// Fields carries per-field access under the row rules (read masking + write gates).
	Fields map[string]FieldRule
}

// FieldRule is per-field access layered under the row-level rules. `read` masks the field OUT
// of returned docs unless the caller matches (row-level read still applies first — masking only
// narrows what a readable doc exposes); `write` gates setting/changing the field.
type FieldRule struct {
	ReadPolicy string // "public" | "authenticated" | "match" | "" (absent => field always visible)
	ReadMatch  Match  // set when ReadPolicy == "match"
	Write      Match  // gate for setting/changing the field (nil => no gate)
	HasWrite   bool
}

// WriteRule is the create/update/delete side of a collection rule.
type WriteRule struct {
	Match     Match
	Stamp     map[string]string
	Immutable []string
}

// AccessEntry is one target instance an end-user token may reach.
type AccessEntry struct {
	Level string // read | write | full
	// Rules is keyed by NAMESPACE (wire form; "_default" = the empty default ns) → collection.
	// An identity token is default-denied in any namespace not listed, so a namespace is a real
	// isolation boundary (a shared auth instance can front many namespaces without one
	// namespace's users reaching another's rows).
	Rules    map[string]map[string]CollectionRules
	HasRules bool
	Channels []string
}

// nsKey maps a decoded namespace ("" for default) to its rules-map key (wire form "_default").
func nsKey(namespace string) string {
	if namespace == "" {
		return "_default"
	}
	return namespace
}

// collectionRulesFor looks up the rules for (namespace, collection); ok=false when the
// namespace isn't listed at all (which callers treat as default-deny).
func (e AccessEntry) collectionRulesFor(namespace, collection string) (CollectionRules, bool) {
	byColl, ok := e.Rules[nsKey(namespace)]
	if !ok {
		return CollectionRules{}, false
	}
	cr, ok := byColl[collection]
	return cr, ok
}

// AccessConfig is keyed "<service>:<instance>", e.g. "datastore:appdb".
type AccessConfig map[string]AccessEntry

var ruleOps = map[string]bool{"=": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "in": true}

// parseAccess tolerantly reads the `access` blob. Malformed pieces are dropped rather
// than throwing — but dropping never widens access: an entry without a valid level is
// skipped entirely, and a rules object that parses to nothing still marks the entry as
// ruled (so default-deny applies).
func parseAccess(v any) AccessConfig {
	m, ok := v.(map[string]any)
	if !ok {
		return AccessConfig{}
	}
	out := AccessConfig{}
	for target, raw := range m {
		em, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		level, _ := em["level"].(string)
		switch level {
		case "read", "write", "full":
		default:
			continue
		}
		entry := AccessEntry{Level: level}
		if chans, ok := em["channels"].([]any); ok {
			for _, c := range chans {
				if s, ok := c.(string); ok && s != "" {
					entry.Channels = append(entry.Channels, s)
				}
			}
		}
		if rm, ok := em["rules"].(map[string]any); ok {
			// rules: namespace (wire form; "_default" = default ns) → collection → modes.
			entry.HasRules = true
			entry.Rules = map[string]map[string]CollectionRules{}
			for nsRaw, collsRaw := range rm {
				colls, ok := collsRaw.(map[string]any)
				if !ok {
					continue
				}
				ns := nsRaw
				if ns == "" {
					ns = "_default" // canonicalize the default ns to wire form
				}
				byColl := map[string]CollectionRules{}
				for coll, cr := range colls {
					cm, ok := cr.(map[string]any)
					if !ok {
						continue
					}
					byColl[coll] = parseCollectionRules(cm)
				}
				entry.Rules[ns] = byColl
			}
		}
		out[target] = entry
	}
	return out
}

// ValidateAccessLevels rejects a row rule for an endpoint the entry's `level` can't reach —
// dead config that would silently 403 at runtime. `level` is a ceiling on which ENDPOINTS an
// identity token reaches; rules then scope WHICH rows. Endpoint→level: read=read,
// create/update=write, delete=full (ranked read<write<full). This mirrors the hosted admin
// save-time guard so a config that stores in dev also stores in prod (and vice-versa). Called
// on the auth admin config write; the read path (parseAccess) stays tolerant.
func ValidateAccessLevels(access any) error {
	m, ok := access.(map[string]any)
	if !ok {
		return nil // absent/malformed access → tolerant read handles it
	}
	for target, raw := range m {
		em, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		level, _ := em["level"].(string)
		rm, ok := em["rules"].(map[string]any)
		if !ok {
			continue
		}
		// rules: namespace → collection → modes. Validate the level ceiling per (ns, collection).
		for ns, collsRaw := range rm {
			colls, ok := collsRaw.(map[string]any)
			if !ok {
				continue
			}
			for coll, cr := range colls {
				cm, ok := cr.(map[string]any)
				if !ok {
					continue
				}
				loc := "access['" + target + "'].rules['" + ns + "']['" + coll + "']"
				if _, hasDelete := cm["delete"]; hasDelete && cm["delete"] != nil && level != "full" {
					return common.BadRequest(loc + " declares a delete rule, but level '" + level + "' can't reach the delete endpoint — set this target's level to \"full\" (delete requires full).")
				}
				_, hasCreate := cm["create"]
				_, hasUpdate := cm["update"]
				if (hasCreate && cm["create"] != nil || hasUpdate && cm["update"] != nil) && level == "read" {
					return common.BadRequest(loc + " declares a create/update rule, but level 'read' is read-only — set this target's level to \"write\" or \"full\".")
				}
				if err := validateCollectionShape(cm, loc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateCollectionShape rejects, at admin save time, the structural mistakes the tolerant
// read path silently drops — an OR that's empty / over-cap / malformed, and a nested (dotted)
// field-rule key whose read mask would silently no-op (a fail-open field leak). Mirrors the
// hosted validateMatch + top-level field-key guard so a config that stores in dev stores in prod.
func validateCollectionShape(cm map[string]any, loc string) error {
	if v, ok := cm["read"]; ok && v != nil {
		if err := validateReadPolicyShape(v, loc+".read"); err != nil {
			return err
		}
	}
	for _, mode := range []string{"create", "update", "delete"} {
		wm, ok := cm[mode].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := wm["match"]; ok && v != nil {
			if err := validateMatchShape(v, loc+"."+mode+".match"); err != nil {
				return err
			}
		}
	}
	fields, ok := cm["fields"].(map[string]any)
	if !ok {
		return nil
	}
	for fname, fr := range fields {
		if !topLevelFieldRe.MatchString(fname) {
			return common.BadRequest(loc + ".fields key '" + fname + "' must be a single top-level field name (letters/numbers/underscore, no nested paths)")
		}
		fm, ok := fr.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := fm["read"]; ok && v != nil {
			if err := validateReadPolicyShape(v, loc+".fields['"+fname+"'].read"); err != nil {
				return err
			}
		}
		if v, ok := fm["write"]; ok && v != nil {
			if err := validateMatchShape(v, loc+".fields['"+fname+"'].write"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateReadPolicyShape validates a read policy: the string "public"/"authenticated", or a
// Match. A string that ISN'T one of the two keywords is rejected — the tolerant read path leaves
// such a value with an empty policy, which at COLLECTION level is default-deny (fail-closed) but
// at FIELD level means "no mask → always visible" (fail-OPEN, a silent field leak). Mirrors the
// hosted validator, which routes a non-keyword read string through validateMatch and throws.
func validateReadPolicyShape(v any, loc string) error {
	if s, ok := v.(string); ok {
		if s != "public" && s != "authenticated" {
			return common.BadRequest(loc + ` must be "public", "authenticated", an array of filters, or { any: [[...], ...] }`)
		}
		return nil
	}
	return validateMatchShape(v, loc)
}

// validateMatchShape rejects a Match that isn't a flat filter array or a well-formed
// `{ any: [[...], ...] }` (non-empty, ≤ maxMatchGroups, each group a valid filter list).
func validateMatchShape(v any, loc string) error {
	switch t := v.(type) {
	case []any:
		return validateFilterList(v, loc)
	case map[string]any:
		anyRaw, ok := t["any"].([]any)
		if !ok {
			return common.BadRequest(loc + " must be an array of filters or { any: [[...], ...] }")
		}
		if len(anyRaw) == 0 {
			return common.BadRequest(loc + ".any must have at least one filter group")
		}
		if len(anyRaw) > maxMatchGroups {
			return common.BadRequest(loc + ".any allows at most 4 OR-groups")
		}
		for i, g := range anyRaw {
			if err := validateFilterList(g, loc+".any["+itoa(i)+"]"); err != nil {
				return err
			}
		}
		return nil
	}
	return common.BadRequest(loc + " must be an array of filters or { any: [[...], ...] }")
}

// validateFilterList rejects a filter list whose entries aren't well-formed — an entry that
// isn't an object, a missing/empty field, or an UNSUPPORTED OPERATOR. The last is the important
// one: the tolerant read path silently DROPS a filter with a bad op, and a group that drops all
// of its filters collapses to an empty (match-all) group — a fail-open that OR amplifies (one
// malformed branch makes the whole disjunction match every row). Mirrors the hosted
// validateFilters op check so a rule rejected in prod is rejected here too.
func validateFilterList(v any, loc string) error {
	arr, ok := v.([]any)
	if !ok {
		return common.BadRequest(loc + " must be an array of filters")
	}
	for i, fv := range arr {
		fm, ok := fv.(map[string]any)
		if !ok {
			return common.BadRequest(loc + "[" + itoa(i) + "] must be an object")
		}
		if field, _ := fm["field"].(string); field == "" {
			return common.BadRequest(loc + "[" + itoa(i) + "].field must be a non-empty string")
		}
		if op, _ := fm["op"].(string); !ruleOps[op] {
			return common.BadRequest(loc + "[" + itoa(i) + "].op must be one of =, !=, <, <=, >, >=, in")
		}
	}
	return nil
}

func parseCollectionRules(cm map[string]any) CollectionRules {
	var out CollectionRules
	switch rv := cm["read"].(type) {
	case string:
		if rv == "public" || rv == "authenticated" {
			out.ReadPolicy = rv
		}
	default:
		if m, ok := parseMatch(rv); ok {
			out.ReadPolicy = "match"
			out.ReadMatch = m
		}
	}
	out.Create = parseWriteRule(cm["create"])
	out.Update = parseWriteRule(cm["update"])
	out.Delete = parseWriteRule(cm["delete"])
	out.Fields = parseFields(cm["fields"])
	return out
}

// parseMatch tolerantly reads a Match: a flat filter array (one AND-group) OR
// `{ any: [[...], ...] }` (OR of AND-groups, bounded to maxMatchGroups). ok=false when the
// value is neither shape (so the caller drops it). Empty/over-cap disjunctions collapse to
// ok=false rather than widening access.
func parseMatch(v any) (Match, bool) {
	switch t := v.(type) {
	case []any:
		return Match{parseRuleFilters(t)}, true
	case map[string]any:
		anyRaw, ok := t["any"].([]any)
		if !ok || len(anyRaw) == 0 || len(anyRaw) > maxMatchGroups {
			return nil, false
		}
		groups := make(Match, 0, len(anyRaw))
		for _, g := range anyRaw {
			ga, ok := g.([]any)
			if !ok {
				return nil, false
			}
			groups = append(groups, parseRuleFilters(ga))
		}
		return groups, true
	}
	return nil, false
}

// topLevelFieldRe matches a single top-level field name — masking removes a top-level key, so a
// dotted/nested field rule can't be enforced. A dotted key is dropped here (its mask would
// no-op, matching the hosted runtime) and rejected at admin save time (ValidateAccessLevels).
var topLevelFieldRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseFields reads the per-field access map. Non-top-level (dotted) field keys are dropped.
func parseFields(v any) map[string]FieldRule {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]FieldRule{}
	for fname, fr := range m {
		if !topLevelFieldRe.MatchString(fname) {
			continue // unenforceable nested mask — drop (no-op), rejected at save time
		}
		fm, ok := fr.(map[string]any)
		if !ok {
			continue
		}
		var pf FieldRule
		switch rv := fm["read"].(type) {
		case string:
			if rv == "public" || rv == "authenticated" {
				pf.ReadPolicy = rv
			}
		default:
			if mm, ok := parseMatch(rv); ok {
				pf.ReadPolicy = "match"
				pf.ReadMatch = mm
			}
		}
		if w, ok := parseMatch(fm["write"]); ok {
			pf.Write = w
			pf.HasWrite = true
		}
		out[fname] = pf
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseWriteRule(v any) *WriteRule {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	wr := &WriteRule{}
	if match, ok := parseMatch(m["match"]); ok {
		wr.Match = match
	}
	if sm, ok := m["stamp"].(map[string]any); ok {
		wr.Stamp = map[string]string{}
		for k, sv := range sm {
			if s, ok := sv.(string); ok {
				wr.Stamp[k] = s
			}
		}
	}
	if arr, ok := m["immutable"].([]any); ok {
		for _, f := range arr {
			if s, ok := f.(string); ok && s != "" {
				wr.Immutable = append(wr.Immutable, s)
			}
		}
	}
	return wr
}

func parseRuleFilters(arr []any) []RuleFilter {
	var out []RuleFilter
	for _, fv := range arr {
		fm, ok := fv.(map[string]any)
		if !ok {
			continue
		}
		field, _ := fm["field"].(string)
		op, _ := fm["op"].(string)
		if field == "" || !ruleOps[op] {
			continue
		}
		out = append(out, RuleFilter{Field: field, Op: op, Value: fm["value"]})
	}
	return out
}

func deny(what string) error { return common.PermissionDenied("not permitted: " + what) }

// claimPath reads a dot-path out of the identity's custom claims.
func claimPath(claims map[string]any, path string) (any, bool) {
	var cur any = claims
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Substitute resolves a rule value that MAY be a `$auth.*` placeholder (or an array of
// them) from the VERIFIED identity. A placeholder that resolves to nothing is a hard
// deny; literals pass through unchanged.
func Substitute(value any, u *EndUser) (any, error) {
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			sv, err := Substitute(v, u)
			if err != nil {
				return nil, err
			}
			out[i] = sv
		}
		return out, nil
	}
	s, ok := value.(string)
	if !ok || !strings.HasPrefix(s, "$auth") {
		return value, nil
	}
	switch {
	case s == "$auth.uid":
		return u.UID, nil
	case s == "$auth.identifier":
		return u.Identifier, nil
	case s == "$auth.email":
		if !u.HasEmail {
			return nil, deny("rule references $auth.email but this identity has no email")
		}
		return u.Email, nil
	case strings.HasPrefix(s, "$auth.claims."):
		// Authoritative — server/admin-set only. Safe to authorize on.
		v, ok := claimPath(u.Claims, strings.TrimPrefix(s, "$auth.claims."))
		if !ok || v == nil {
			return nil, deny("rule references missing claim in '" + s + "'")
		}
		return v, nil
	case strings.HasPrefix(s, "$auth.profile."):
		// User-supplied (signup). Fine to stamp/match on as display data, but NEVER trust it
		// for access control — the user chose this value. A missing field is a hard deny, just
		// like a claim, so a rule never silently degrades to an unconstrained match.
		v, ok := claimPath(u.Profile, strings.TrimPrefix(s, "$auth.profile."))
		if !ok || v == nil {
			return nil, deny("rule references missing profile field in '" + s + "'")
		}
		return v, nil
	}
	return nil, deny("unknown rule placeholder '" + s + "'")
}

func toFilters(rfs []RuleFilter, u *EndUser) ([]Filter, error) {
	out := make([]Filter, 0, len(rfs))
	for _, rf := range rfs {
		if !ruleOps[rf.Op] {
			return nil, deny("rule uses unsupported operator '" + rf.Op + "'")
		}
		v, err := Substitute(rf.Value, u)
		if err != nil {
			return nil, err
		}
		out = append(out, Filter{Field: rf.Field, Op: rf.Op, Value: v})
	}
	return out, nil
}

// entryFor returns the datastore access entry for `instance`.
func (u *EndUser) entryFor(service, instance string) (AccessEntry, error) {
	e, ok := u.Access[service+":"+instance]
	if !ok {
		return AccessEntry{}, deny("no access to " + service + " '" + instance + "'")
	}
	return e, nil
}

// toGroups substitutes a Match into OR-of-AND groups of concrete Filters (disjunctive normal
// form). A nil Match yields nil (unconstrained). Enforcement treats the outer slice as OR, each
// inner slice as AND.
func toGroups(m Match, u *EndUser) ([][]Filter, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make([][]Filter, 0, len(m))
	for _, g := range m {
		fs, err := toFilters(g, u)
		if err != nil {
			return nil, err
		}
		out = append(out, fs)
	}
	return out, nil
}

// DatastoreReadGroups returns the OR-of-AND filter groups to scope a datastore read for this
// identity. `nil` means unconstrained (a rules-less entry, or a `public`/`authenticated` read
// policy). Returns 403 when the rules don't permit the read (an unlisted namespace or collection
// is default-deny). 0/1 group ANDs into the query; ≥2 groups drive the multi-query merge.
func (u *EndUser) DatastoreReadGroups(instance, namespace, collection string) ([][]Filter, error) {
	entry, err := u.entryFor("datastore", instance)
	if err != nil {
		return nil, err
	}
	if !entry.HasRules {
		return nil, nil // level-only access
	}
	// An unlisted namespace is default-deny — a token can't reach a collection by switching the
	// namespace segment to one it wasn't granted rules for.
	cr, ok := entry.collectionRulesFor(namespace, collection)
	if !ok || cr.ReadPolicy == "" {
		return nil, deny("read on '" + collection + "' in namespace '" + nsKey(namespace) + "'")
	}
	if cr.ReadPolicy != "match" {
		return nil, nil // public / authenticated
	}
	return toGroups(cr.ReadMatch, u)
}

// DatastoreFieldReads returns the per-field read masks for this identity: field name -> OR-groups
// a doc must satisfy for the field to be returned. A `public`/`authenticated` field read maps to
// `nil` groups (always visible to an identity — they're authenticated); a field with no read rule
// is absent from the map (always visible). Row-level read access is enforced separately by
// DatastoreReadGroups; this only projects fields OUT of docs the caller may already read.
func (u *EndUser) DatastoreFieldReads(instance, namespace, collection string) (map[string][][]Filter, error) {
	entry, err := u.entryFor("datastore", instance)
	if err != nil {
		return nil, err
	}
	if !entry.HasRules {
		return nil, nil
	}
	cr, ok := entry.collectionRulesFor(namespace, collection)
	if !ok || len(cr.Fields) == 0 {
		return nil, nil
	}
	out := map[string][][]Filter{}
	for field, fr := range cr.Fields {
		if fr.ReadPolicy == "" {
			continue // no read rule => field always visible
		}
		if fr.ReadPolicy != "match" {
			out[field] = nil // public / authenticated => always visible
			continue
		}
		g, err := toGroups(fr.ReadMatch, u)
		if err != nil {
			return nil, err
		}
		out[field] = g
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// WritePolicy is the resolved constraint set for one create/update/delete.
type WritePolicy struct {
	// Denied is set when the identity's rules don't cover this collection/mode. An
	// upsert can't know create-vs-update until it reads the row, so the policy is
	// carried (not thrown) and only refused on the branch actually taken.
	Denied bool
	// Match is OR-of-AND groups (DNF) the existing row (update/delete) or resulting doc (create)
	// must satisfy; nil = unconstrained.
	Match     [][]Filter
	Stamp     map[string]any // server-set fields from the identity (create/update)
	Immutable []string       // fields that may not change (update)
	// FieldWrites gates per-field set (create) / change (update): field -> OR-groups the target
	// doc must satisfy for that field to be written. Empty = no per-field gates.
	FieldWrites map[string][][]Filter
}

// DatastoreWritePolicy resolves the create/update/delete policy for a collection.
// An unconstrained policy (no rules on the entry) has Denied=false and no constraints.
func (u *EndUser) DatastoreWritePolicy(instance, namespace, collection, mode string) (WritePolicy, error) {
	entry, err := u.entryFor("datastore", instance)
	if err != nil {
		return WritePolicy{}, err
	}
	if !entry.HasRules {
		return WritePolicy{}, nil // level-only access
	}
	// An unlisted namespace (or collection) is default-deny — no cross-namespace writes.
	cr, ok := entry.collectionRulesFor(namespace, collection)
	if !ok {
		return WritePolicy{Denied: true}, nil
	}
	var spec *WriteRule
	switch mode {
	case "create":
		spec = cr.Create
	case "update":
		spec = cr.Update
	case "delete":
		spec = cr.Delete
	}
	if spec == nil {
		return WritePolicy{Denied: true}, nil
	}
	match, err := toGroups(spec.Match, u)
	if err != nil {
		return WritePolicy{}, err
	}
	pol := WritePolicy{Match: match}
	if mode != "delete" && len(spec.Stamp) > 0 {
		pol.Stamp = map[string]any{}
		for field, placeholder := range spec.Stamp {
			v, err := Substitute(placeholder, u)
			if err != nil {
				return WritePolicy{}, err
			}
			pol.Stamp[field] = v
		}
	}
	if mode == "update" {
		pol.Immutable = spec.Immutable
	}
	// Per-field write gates apply to create + update (not delete). Substitute each field's write
	// Match into OR-groups; the store checks a field is only set/changed when the doc satisfies them.
	if mode != "delete" && len(cr.Fields) > 0 {
		fw := map[string][][]Filter{}
		for field, fr := range cr.Fields {
			if !fr.HasWrite {
				continue
			}
			g, err := toGroups(fr.Write, u)
			if err != nil {
				return WritePolicy{}, err
			}
			fw[field] = g
		}
		if len(fw) > 0 {
			pol.FieldWrites = fw
		}
	}
	return pol, nil
}

// --- channel: identity-scoped channel patterns ---
// A channel access entry lists channel PATTERNS, e.g. ["posts.*", "dm.$auth.uid"].
// Unlike datastore rule values (whole-string placeholders) these embed `$auth.*` INSIDE
// the string, so they are interpolated; a trailing `*` is a prefix wildcard.

// SubstituteTemplate interpolates `$auth.*` occurrences inside a string from the verified
// identity. A missing or non-scalar claim is a hard deny (never a wildcard).
func SubstituteTemplate(tpl string, u *EndUser) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tpl); {
		if !strings.HasPrefix(tpl[i:], "$auth.") {
			b.WriteByte(tpl[i])
			i++
			continue
		}
		rest := tpl[i+len("$auth."):]
		name := scanPlaceholder(rest)
		if name == "" {
			return "", deny("channel template has an empty placeholder")
		}
		val, err := templateValue(name, u)
		if err != nil {
			return "", err
		}
		b.WriteString(val)
		i += len("$auth.") + len(name)
	}
	return b.String(), nil
}

// scanPlaceholder reads the longest [A-Za-z0-9_.] run that forms a placeholder name,
// trimming a trailing '.' so "dm.$auth.uid.x" style separators stay outside the name.
func scanPlaceholder(s string) string {
	n := 0
	for n < len(s) {
		c := s[n]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			n++
			continue
		}
		break
	}
	name := s[:n]
	for strings.HasSuffix(name, ".") {
		name = name[:len(name)-1]
	}
	return name
}

func templateValue(name string, u *EndUser) (string, error) {
	switch {
	case name == "uid":
		return u.UID, nil
	case name == "identifier":
		return u.Identifier, nil
	case name == "email":
		if !u.HasEmail {
			return "", deny("channel template references $auth.email but this identity has no email")
		}
		return u.Email, nil
	case strings.HasPrefix(name, "claims."):
		v, ok := claimPath(u.Claims, strings.TrimPrefix(name, "claims."))
		if !ok {
			return "", deny("channel template references missing claim '$auth." + name + "'")
		}
		s, ok := scalarString(v)
		if !ok {
			return "", deny("channel template references non-scalar claim '$auth." + name + "'")
		}
		return s, nil
	case strings.HasPrefix(name, "profile."):
		// User-supplied (signup) — never authoritative, but fine to interpolate into a
		// channel name. A missing or non-scalar field is a hard deny (never a wildcard).
		v, ok := claimPath(u.Profile, strings.TrimPrefix(name, "profile."))
		if !ok {
			return "", deny("channel template references missing profile field '$auth." + name + "'")
		}
		s, ok := scalarString(v)
		if !ok {
			return "", deny("channel template references non-scalar profile field '$auth." + name + "'")
		}
		return s, nil
	}
	return "", deny("unknown channel template placeholder '$auth." + name + "'")
}

// ChannelPatterns returns the concrete channel patterns this identity may use on the
// instance (templates interpolated). 403 when the identity has no channel access.
func (u *EndUser) ChannelPatterns(instance string) ([]string, error) {
	entry, ok := u.Access["channel:"+instance]
	if !ok || len(entry.Channels) == 0 {
		return nil, deny("no channel access to '" + instance + "'")
	}
	out := make([]string, 0, len(entry.Channels))
	for _, tpl := range entry.Channels {
		s, err := SubstituteTemplate(tpl, u)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ChannelAllowed reports whether a concrete channel name is allowed by the patterns.
// A trailing `*` is a prefix wildcard; otherwise the match is exact.
func ChannelAllowed(patterns []string, channel string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(channel, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if channel == p {
			return true
		}
	}
	return false
}
