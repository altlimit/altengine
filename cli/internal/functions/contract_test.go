package functions

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/dop251/goja"
)

// The env.* grant ladder is enforced twice — here, and in the hosted worker's
// src/functions/api/*.ts, which this repo cannot import. conformance/fn-bindings.json is the
// contract between them, and this asserts our half of it.
//
// It is not busywork: the two ladders HAD drifted. datastore.delete and search.delete asked
// for `write` here against `full` hosted, so a function deleting documents on a write grant
// worked locally and 403'd in production — the exact class of bug an emulator exists to
// prevent, and one that no test on either side could see while each only checked itself.

func levelName(l auth.Level) string {
	switch l {
	case auth.Read:
		return "read"
	case auth.Write:
		return "write"
	case auth.Full:
		return "full"
	}
	return "none"
}

// levelsForCall reports every level a call can demand: one for a fixed level, or the set a
// levelFor can return. Only channel.token is argument-dependent, and both of its outcomes are
// exercised here rather than assumed.
func levelsForCall(vm *goja.Runtime, m call) []string {
	if m.levelFor == nil {
		return []string{levelName(m.level)}
	}
	probes := [][]goja.Value{
		{vm.ToValue(map[string]any{"instance": "i"}), vm.ToValue(map[string]any{})},
		{vm.ToValue(map[string]any{"instance": "i"}), vm.ToValue(map[string]any{"publish": true})},
	}
	seen := map[string]bool{}
	for _, args := range probes {
		seen[levelName(m.levelFor(args))] = true
	}
	order := map[string]int{"read": 0, "write": 1, "full": 2}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

func TestBindingLevelsMatchContract(t *testing.T) {
	raw, err := os.ReadFile("../../../conformance/fn-bindings.json")
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var doc struct {
		Services map[string]map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse contract: %v", err)
	}

	vm := goja.New()
	got := map[string]map[string][]string{}
	for svc, methods := range serviceMethods {
		got[svc] = map[string][]string{}
		for name, m := range methods {
			got[svc][name] = levelsForCall(vm, m)
		}
	}

	// Every service in the contract exists here, with the same methods and the same levels.
	for svc, methods := range doc.Services {
		ours, ok := got[svc]
		if !ok {
			t.Fatalf("contract has service %q with no bindings", svc)
		}
		for name, rawLevel := range methods {
			want := decodeLevels(t, rawLevel)
			have, ok := ours[name]
			if !ok {
				t.Errorf("env.%s.%s is in the contract but not implemented here", svc, name)
				continue
			}
			if !reflect.DeepEqual(want, have) {
				t.Errorf("env.%s.%s: contract says %v, bindings demand %v", svc, name, want, have)
			}
		}
		for name := range ours {
			if _, ok := methods[name]; !ok {
				t.Errorf("env.%s.%s exists here but is not in the contract", svc, name)
			}
		}
	}
	for svc := range got {
		if _, ok := doc.Services[svc]; !ok {
			t.Errorf("service %q has bindings but no contract entry", svc)
		}
	}
}

func decodeLevels(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		t.Fatalf("contract level is neither a string nor an array: %s", string(raw))
	}
	return many
}
