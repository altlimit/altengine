package identity

import "testing"

// A row rule for an endpoint the entry's `level` can't reach is dead config that would
// silently 403 at runtime. ValidateAccessLevels rejects it at admin save time, mirroring the
// hosted guard so a config that stores in dev also stores in prod. Endpoint→level:
// read=read, create/update=write, delete=full.
func TestValidateAccessLevels(t *testing.T) {
	deleteRule := map[string]any{"match": []any{map[string]any{"field": "author_uid", "op": "=", "value": "$auth.uid"}}}
	createRule := map[string]any{"stamp": map[string]any{"author_uid": "$auth.uid"}}

	// rules are keyed by namespace ("_default" = the default ns) → collection.
	entry := func(level string, coll map[string]any) map[string]any {
		return map[string]any{"datastore:db": map[string]any{"level": level, "rules": map[string]any{"_default": map[string]any{"c": coll}}}}
	}

	cases := []struct {
		name    string
		access  any
		wantErr bool
	}{
		{"delete rule at write is rejected", entry("write", map[string]any{"delete": deleteRule}), true},
		{"delete rule at read is rejected", entry("read", map[string]any{"delete": deleteRule}), true},
		{"delete rule at full is allowed", entry("full", map[string]any{"delete": deleteRule}), false},
		{"create rule at read is rejected", entry("read", map[string]any{"create": createRule}), true},
		{"update rule at read is rejected", entry("read", map[string]any{"update": map[string]any{"match": []any{}}}), true},
		{"create rule at write is allowed", entry("write", map[string]any{"create": createRule}), false},
		{"read-only rule at read is allowed", entry("read", map[string]any{"read": "authenticated"}), false},
		{"absent access is allowed", nil, false},
		{"no rules is allowed", map[string]any{"datastore:db": map[string]any{"level": "read"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAccessLevels(tc.access)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAccessLevels() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// The save-time guard also rejects the structural mistakes the tolerant read path silently drops:
// a malformed/over-cap OR, and a nested (dotted) field-rule key whose read mask would no-op (a
// fail-open field leak). Mirrors the hosted validateMatch + top-level field-key rejection.
func TestValidateAccessShapeRejections(t *testing.T) {
	entry := func(coll map[string]any) map[string]any {
		return map[string]any{"datastore:db": map[string]any{"level": "read", "rules": map[string]any{"_default": map[string]any{"c": coll}}}}
	}
	filter := []any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}}
	fiveGroups := make([]any, 5)
	for i := range fiveGroups {
		fiveGroups[i] = filter
	}

	cases := []struct {
		name    string
		access  any
		wantErr bool
	}{
		{"valid OR read", entry(map[string]any{"read": map[string]any{"any": []any{filter, filter}}}), false},
		{"empty OR is rejected", entry(map[string]any{"read": map[string]any{"any": []any{}}}), true},
		{"over-cap OR is rejected", entry(map[string]any{"read": map[string]any{"any": fiveGroups}}), true},
		{"non-array OR group is rejected", entry(map[string]any{"read": map[string]any{"any": []any{"nope"}}}), true},
		{"valid top-level field read", entry(map[string]any{"fields": map[string]any{"email": map[string]any{"read": "authenticated"}}}), false},
		{"nested field key is rejected", entry(map[string]any{"fields": map[string]any{"payment.card": map[string]any{"read": filter}}}), true},
		{"valid field write (Match)", entry(map[string]any{"fields": map[string]any{"pinned": map[string]any{"write": filter}}}), false},
		{"malformed field write is rejected", entry(map[string]any{"fields": map[string]any{"pinned": map[string]any{"write": map[string]any{"any": []any{}}}}}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAccessLevels(tc.access); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAccessLevels() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// A nested (dotted) field-rule key is DROPPED by the tolerant read path — its mask can't be
// enforced (masking is top-level only), so it must not silently take effect as something else.
func TestParseDropsNestedFieldKey(t *testing.T) {
	cfg := ParseConfig(map[string]any{"access": map[string]any{
		"datastore:db": map[string]any{
			"level": "read",
			"rules": map[string]any{"_default": map[string]any{"c": map[string]any{
				"read": "authenticated",
				"fields": map[string]any{
					"email":        map[string]any{"read": []any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}}},
					"payment.card": map[string]any{"read": "authenticated"},
				},
			}}},
		},
	}})
	cr, ok := cfg.Access["datastore:db"].collectionRulesFor("", "c")
	if !ok {
		t.Fatal("expected rules for c")
	}
	if _, ok := cr.Fields["email"]; !ok {
		t.Fatal("top-level field rule should survive")
	}
	if _, ok := cr.Fields["payment.card"]; ok {
		t.Fatal("nested field key must be dropped by the tolerant parser")
	}
}

// OR read substitutes into OR-of-AND groups; a flat filter list is one group.
func TestDatastoreReadGroupsShape(t *testing.T) {
	cfg := ParseConfig(map[string]any{"access": map[string]any{
		"datastore:db": map[string]any{
			"level": "full",
			"rules": map[string]any{"_default": map[string]any{
				"docs": map[string]any{"read": map[string]any{"any": []any{
					[]any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}},
					[]any{map[string]any{"field": "visibility", "op": "=", "value": "public"}},
				}}},
				"flat": map[string]any{"read": []any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}}},
			}},
		},
	}})
	u := &EndUser{UID: "alice", Access: cfg.Access}

	groups, err := u.DatastoreReadGroups("db", "", "docs")
	if err != nil {
		t.Fatalf("DatastoreReadGroups(docs): %v", err)
	}
	if len(groups) != 2 || len(groups[0]) != 1 || groups[0][0].Value != "alice" || groups[1][0].Value != "public" {
		t.Fatalf("OR read groups wrong: %+v", groups)
	}
	flat, err := u.DatastoreReadGroups("db", "", "flat")
	if err != nil {
		t.Fatalf("DatastoreReadGroups(flat): %v", err)
	}
	if len(flat) != 1 || len(flat[0]) != 1 {
		t.Fatalf("flat read should be one AND-group: %+v", flat)
	}
}
