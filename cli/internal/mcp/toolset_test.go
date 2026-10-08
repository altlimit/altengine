package mcp

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// conformance/mcp-tools.json names every tool the hosted service exposes. The emulator serves
// each of them or lists it as not emulated, with the reason — SCENARIOS.md used to claim parity
// while 19 hosted tools were missing here and nothing said which.
func TestToolSetAgainstTheHostedList(t *testing.T) {
	raw, err := os.ReadFile("../../../conformance/mcp-tools.json")
	if err != nil {
		t.Fatalf("read the hosted tool list: %v", err)
	}
	var doc struct {
		Tools       []string          `json:"tools"`
		NotEmulated map[string]string `json:"not_emulated"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	hosted := map[string]bool{}
	for _, n := range doc.Tools {
		hosted[n] = true
	}
	var missing []string
	for _, n := range doc.Tools {
		_, served := registry[n]
		_, excused := doc.NotEmulated[n]
		switch {
		case served && excused:
			t.Errorf("%s is served here but listed as not emulated — remove it from not_emulated", n)
		case !served && !excused:
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("hosted tools neither served nor listed as not emulated: %v", missing)
	}
	for n := range doc.NotEmulated {
		if !hosted[n] {
			t.Errorf("not_emulated names %s, which is not a hosted tool", n)
		}
	}
	for n := range registry {
		if !hosted[n] {
			t.Errorf("%s exists only here — a tool that exists only locally teaches a workflow that fails hosted", n)
		}
	}
	// The argument table in mcp_test.go must cover every tool served here.
	for n := range registry {
		if _, ok := hostedTools[n]; !ok {
			t.Errorf("%s has no entry in hostedTools, so its argument names are unchecked", n)
		}
	}
}
