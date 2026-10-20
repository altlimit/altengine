package identity

import (
	"encoding/json"
	"testing"

	"github.com/altlimit/altengine/cli/internal/control"
)

// A filter stored before field and value were checked keeps working at request time, so a later
// save that leaves it as it is must go through; a filter that is new or changed is still held to
// the checks. The registry hands the check the config being replaced, as the hosted save does.
func TestRuleFiltersStoredBeforeAreGrandfathered(t *testing.T) {
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	reg.OnValidateConfigChange("auth", ValidateConfigChange)
	in := reg.GetOrCreate("auth", "a")

	access := func(filters ...any) map[string]any {
		return map[string]any{"datastore:db": map[string]any{"level": "write", "rules": map[string]any{
			"_default": map[string]any{"c": map[string]any{
				"read":   filters,
				"update": map[string]any{"match": map[string]any{"any": []any{filters}}},
			}}}}}
	}
	legacy := map[string]any{"field": "a-b", "op": "=", "value": "$auth.uidx"}
	good := map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}

	// Fresh, the legacy filter is refused.
	if err := reg.SaveConfig(in, map[string]any{"access": access(legacy)}); err == nil {
		t.Fatal("a new invalid filter was accepted")
	}
	// Stored as it would have been before the checks existed.
	reg.SetConfig(in, map[string]any{"access": access(legacy)})

	// Saving it unchanged, alongside a new valid filter, goes through.
	if err := reg.SaveConfig(in, map[string]any{"access": access(legacy, good)}); err != nil {
		t.Fatalf("an unchanged stored filter blocked the save: %v", err)
	}
	// So does the same filter decoded from JSON, as the console's PUT body is.
	var fromJSON map[string]any
	b, _ := json.Marshal(map[string]any{"access": access(legacy)})
	if err := json.Unmarshal(b, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if err := reg.ReplaceConfig(in, fromJSON); err != nil {
		t.Fatalf("the stored filter, round-tripped through JSON, was refused: %v", err)
	}

	// A changed value, a changed field, or a new invalid filter is held to the checks.
	for name, f := range map[string]any{
		"changed value": map[string]any{"field": "a-b", "op": "=", "value": "$auth.uidy"},
		"changed field": map[string]any{"field": "a-c", "op": "=", "value": "$auth.uidx"},
		"changed op":    map[string]any{"field": "a-b", "op": "!=", "value": "$auth.uidx"},
	} {
		if err := reg.SaveConfig(in, map[string]any{"access": access(legacy, f)}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An unsupported op is never grandfathered.
	reg.SetConfig(in, map[string]any{"access": access(map[string]any{"field": "x", "op": "~", "value": "y"})})
	if err := reg.SaveConfig(in, map[string]any{"access": access(map[string]any{"field": "x", "op": "~", "value": "y"})}); err == nil {
		t.Error("a stored filter with an unsupported op was grandfathered")
	}
	// The level ceiling is not grandfathered either: it is not a filter check.
	reg.SetConfig(in, map[string]any{"access": access(legacy)})
	readOnly := access(legacy)
	readOnly["datastore:db"].(map[string]any)["level"] = "read"
	if err := reg.SaveConfig(in, map[string]any{"access": readOnly}); err == nil {
		t.Error("an update rule under level 'read' was accepted")
	}
}
