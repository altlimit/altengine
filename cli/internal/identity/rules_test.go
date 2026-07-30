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
