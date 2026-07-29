package identity

import (
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

// CollectionRules is the per-collection rule set for a datastore target.
type CollectionRules struct {
	// Read is "public", "authenticated", a []RuleFilter, or absent (deny).
	ReadPolicy  string // "public" | "authenticated" | "filters" | "" (absent => deny)
	ReadFilters []RuleFilter
	Create      *WriteRule
	Update      *WriteRule
	Delete      *WriteRule
}

// WriteRule is the create/update/delete side of a collection rule.
type WriteRule struct {
	Match     []RuleFilter
	Stamp     map[string]string
	Immutable []string
}

// AccessEntry is one target instance an end-user token may reach.
type AccessEntry struct {
	Level    string // read | write | full
	Rules    map[string]CollectionRules
	HasRules bool
	Channels []string
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
			entry.HasRules = true
			entry.Rules = map[string]CollectionRules{}
			for coll, cr := range rm {
				cm, ok := cr.(map[string]any)
				if !ok {
					continue
				}
				entry.Rules[coll] = parseCollectionRules(cm)
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
		for coll, cr := range rm {
			cm, ok := cr.(map[string]any)
			if !ok {
				continue
			}
			if _, hasDelete := cm["delete"]; hasDelete && cm["delete"] != nil && level != "full" {
				return common.BadRequest("access['" + target + "'].rules['" + coll + "'] declares a delete rule, but level '" + level + "' can't reach the delete endpoint — set this target's level to \"full\" (delete requires full).")
			}
			_, hasCreate := cm["create"]
			_, hasUpdate := cm["update"]
			if (hasCreate && cm["create"] != nil || hasUpdate && cm["update"] != nil) && level == "read" {
				return common.BadRequest("access['" + target + "'].rules['" + coll + "'] declares a create/update rule, but level 'read' is read-only — set this target's level to \"write\" or \"full\".")
			}
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
	case []any:
		out.ReadPolicy = "filters"
		out.ReadFilters = parseRuleFilters(rv)
	}
	out.Create = parseWriteRule(cm["create"])
	out.Update = parseWriteRule(cm["update"])
	out.Delete = parseWriteRule(cm["delete"])
	return out
}

func parseWriteRule(v any) *WriteRule {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	wr := &WriteRule{}
	if arr, ok := m["match"].([]any); ok {
		wr.Match = parseRuleFilters(arr)
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

// DatastoreReadFilters returns the filters to AND into a datastore read for this
// identity. An empty slice means unconstrained (a rules-less entry, or a `public`/
// `authenticated` read policy). Returns 403 when the rules don't permit the read.
func (u *EndUser) DatastoreReadFilters(instance, collection string) ([]Filter, error) {
	entry, err := u.entryFor("datastore", instance)
	if err != nil {
		return nil, err
	}
	if !entry.HasRules {
		return nil, nil // level-only access
	}
	cr, ok := entry.Rules[collection]
	if !ok || cr.ReadPolicy == "" {
		return nil, deny("read on '" + collection + "'")
	}
	if cr.ReadPolicy != "filters" {
		return nil, nil // public / authenticated
	}
	return toFilters(cr.ReadFilters, u)
}

// WritePolicy is the resolved constraint set for one create/update/delete.
type WritePolicy struct {
	// Denied is set when the identity's rules don't cover this collection/mode. An
	// upsert can't know create-vs-update until it reads the row, so the policy is
	// carried (not thrown) and only refused on the branch actually taken.
	Denied    bool
	Match     []Filter       // the existing row (update/delete) or resulting doc (create)
	Stamp     map[string]any // server-set fields from the identity (create/update)
	Immutable []string       // fields that may not change (update)
}

// DatastoreWritePolicy resolves the create/update/delete policy for a collection.
// An unconstrained policy (no rules on the entry) has Denied=false and no constraints.
func (u *EndUser) DatastoreWritePolicy(instance, collection, mode string) (WritePolicy, error) {
	entry, err := u.entryFor("datastore", instance)
	if err != nil {
		return WritePolicy{}, err
	}
	if !entry.HasRules {
		return WritePolicy{}, nil // level-only access
	}
	cr, ok := entry.Rules[collection]
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
	match, err := toFilters(spec.Match, u)
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
