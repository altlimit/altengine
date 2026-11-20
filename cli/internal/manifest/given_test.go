package manifest

import (
	"strings"
	"testing"
)

// A plan for a request that carries a secret's value does not ask for it.
func TestPlanNotesGivenSecrets(t *testing.T) {
	plan := &Plan{
		Secrets:  []PlanSecret{{Key: "functions", Name: "A", Required: true}, {Key: "functions", Name: "B", Required: true}},
		Warnings: []string{"other", "Enter values for A, B."},
	}
	noteGivenSecrets(plan, map[string]map[string]string{"functions": {"A": "x"}})
	if !plan.Secrets[0].Given || plan.Secrets[1].Given {
		t.Fatalf("given flags wrong: %+v", plan.Secrets)
	}
	if strings.Join(plan.Warnings, "|") != "other|Enter a value for B." {
		t.Fatalf("warnings: %q", plan.Warnings)
	}
}
