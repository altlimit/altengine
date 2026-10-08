package datastore

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/identity"
)

// A field an update rule's match binds to the caller (`owner = $auth.uid`) is what makes the row
// that caller's, so the update may not change it: otherwise an owner could re-point `owner` at
// someone else, or drop it in a full replace. A stamped field and a field with its own write gate
// are exempt — the rule already says who sets those — and a literal-matched field stays mutable.
func boundFixture() map[string]any {
	eq := func(field string, value any) map[string]any {
		return map[string]any{"field": field, "op": "=", "value": value}
	}
	return map[string]any{
		"datastore:appdb": map[string]any{
			"level": "full",
			"rules": map[string]any{
				"_default": map[string]any{
					"tasks": map[string]any{
						"read":   "authenticated",
						"create": map[string]any{"stamp": map[string]any{"owner": "$auth.uid", "last_editor": "$auth.uid"}},
						"update": map[string]any{
							"match": map[string]any{"any": []any{
								[]any{eq("owner", "$auth.uid"), eq("status", "open")},
								[]any{eq("last_editor", "$auth.uid")},
								[]any{eq("reviewer", "$auth.uid")},
								[]any{map[string]any{"field": "crew", "op": "in", "value": []any{"$auth.uid"}}},
							}},
							"stamp": map[string]any{"last_editor": "$auth.uid"},
						},
						"fields": map[string]any{
							"reviewer": map[string]any{"write": []any{eq("owner", "$auth.uid")}},
							"tags":     map[string]any{"write": []any{eq("owner", "$auth.uid")}},
						},
					},
				},
			},
		},
	}
}

func newBoundEnv(t *testing.T) *ruleEnv {
	t.Helper()
	reg, _ := control.New("")
	idSvc := identity.NewService(reg, identity.NewManager(""), true)
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewManager("")).WithIdentity(idSvc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	authInst := reg.GetOrCreate("auth", "appauth")
	reg.SetConfig(authInst, map[string]any{"access": boundFixture()})
	return &ruleEnv{srv: srv, reg: reg, authInst: authInst}
}

func TestUpdateBoundFieldsAreImmutable(t *testing.T) {
	e := newBoundEnv(t)
	alice := e.token("alice", "Alice")
	n := 0
	// seed creates a fresh task alice owns.
	seed := func() string {
		n++
		key := fmt.Sprintf("t%d", n)
		body := `{"documents":[{"key":"` + key + `","data":{"title":"x","status":"open","reviewer":"bob","crew":{"lead":"bob","ids":[1,2]}}}]}`
		if status, out := e.call(t, "/tasks/documents", body, alice); status != 200 {
			t.Fatalf("create -> %d: %v", status, out)
		}
		return key
	}
	update := func(key, data string) (int, map[string]any) {
		return e.call(t, "/tasks/documents", `{"documents":[{"key":"`+key+`","data":`+data+`}]}`, alice)
	}
	const keep = `"title":"x","reviewer":"bob","crew":{"lead":"bob","ids":[1,2]}`

	cases := []struct {
		name string
		data string
		want int
	}{
		{"re-pointing the owner is refused", `{"owner":"mallory","last_editor":"alice","status":"open",` + keep + `}`, 403},
		{"omitting the owner in a full replace is refused", `{"last_editor":"alice","status":"open",` + keep + `}`, 403},
		{"a literal-matched field stays changeable", `{"owner":"alice","last_editor":"alice","status":"done",` + keep + `}`, 200},
		{"a stamped bound field is exempt", `{"owner":"alice","last_editor":"zed","status":"open",` + keep + `}`, 200},
		{"a bound field with its own write gate is exempt",
			`{"owner":"alice","last_editor":"alice","status":"open","title":"x","reviewer":"carol","crew":{"lead":"bob","ids":[1,2]}}`, 200},
		{"an unchanged object-valued bound field is not a change",
			`{"owner":"alice","last_editor":"alice","status":"open","title":"y","reviewer":"bob","crew":{"ids":[1,2],"lead":"bob"}}`, 200},
		{"a changed object-valued bound field is refused",
			`{"owner":"alice","last_editor":"alice","status":"open","title":"x","reviewer":"bob","crew":{"lead":"alice","ids":[1,2]}}`, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := seed()
			if status, out := update(key, c.data); status != c.want {
				t.Fatalf("update -> %d (want %d): %v", status, c.want, out)
			}
		})
	}

	// The stamp still wins on the exempt field: the forged editor never lands.
	key := seed()
	if status, out := update(key, `{"owner":"alice","last_editor":"zed","status":"open",`+keep+`}`); status != 200 {
		t.Fatalf("update -> %d: %v", status, out)
	}
	_, got := e.call(t, "/tasks/documents/get", `{"keys":["`+key+`"]}`, alice)
	docs, _ := got["documents"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["data"].(map[string]any)["last_editor"] != "alice" {
		t.Fatalf("stamped last_editor = %v, want alice", got)
	}
}

// A field write gate guards CHANGING the field. A non-owner who may update the row (here as its
// reviewer) resends the whole document in a full replace; an owner-gated array resent unchanged
// is not a change, and must not trip the gate.
func TestWriteGateComparesStructurally(t *testing.T) {
	e := newBoundEnv(t)
	alice, bob := e.token("alice", "Alice"), e.token("bob", "Bob")
	body := func(tags string) string {
		return `{"documents":[{"key":"t1","data":{"owner":"alice","last_editor":"alice","status":"open","title":"x",` +
			`"reviewer":"bob","crew":{"lead":"bob"},"tags":` + tags + `}}]}`
	}
	if status, out := e.call(t, "/tasks/documents", body(`["a","b"]`), alice); status != 200 {
		t.Fatalf("create -> %d: %v", status, out)
	}
	if status, out := e.call(t, "/tasks/documents", body(`["a","b"]`), bob); status != 200 {
		t.Fatalf("reviewer resending unchanged tags -> %d (want 200): %v", status, out)
	}
	if status, out := e.call(t, "/tasks/documents", body(`["a","b","c"]`), bob); status != 403 {
		t.Fatalf("reviewer changing owner-gated tags -> %d (want 403): %v", status, out)
	}
}
