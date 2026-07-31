package datastore

import (
	"time"

	"testing"

	"github.com/altlimit/altengine/cli/internal/identity"
)

// OR-matching (disjunctive read rules) and field-level access (read masking + write gates) for
// end-user identity tokens — the emulator counterpart of the hosted query-or-merge / field tests.
// These mirror the hosted enforcement so a rule that behaves one way in prod behaves the same in
// the local emulator.

// tokenClaims mints an identity token carrying authoritative claims (admin-set), used to exercise
// rules that authorize on `$auth.claims.*`.
func (e *ruleEnv) tokenClaims(uid string, claims map[string]any) string {
	return identity.SignIdentity(identity.IdentityClaims{
		Iss:        e.authInst.ID,
		Sub:        uid,
		Identifier: uid + "@example.com",
		Email:      uid + "@example.com",
		Claims:     claims,
		Exp:        time.Now().Unix() + 3600,
	}, e.authInst.Secret)
}

// orFixture: `docs` is readable when you own it OR it's public (a two-branch OR read that drives
// the multi-query merge); `profiles` masks `email` to the owner (field read); `items` reserves the
// `featured` field to docs whose `tier` matches the caller's authoritative tier claim (field write).
func orFixture() map[string]any {
	return map[string]any{
		"datastore:appdb": map[string]any{
			"level": "full",
			"rules": map[string]any{
				"_default": map[string]any{
					"docs": map[string]any{
						"read": map[string]any{"any": []any{
							[]any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}},
							[]any{map[string]any{"field": "visibility", "op": "=", "value": "public"}},
						}},
						"create": map[string]any{"stamp": map[string]any{"owner": "$auth.uid"}},
					},
					"profiles": map[string]any{
						"read":   "authenticated",
						"create": map[string]any{"stamp": map[string]any{"owner": "$auth.uid"}},
						"fields": map[string]any{
							// email is visible only on rows you own; masked out otherwise.
							"email": map[string]any{"read": []any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}}},
						},
					},
					"items": map[string]any{
						"read":   "authenticated",
						"create": map[string]any{"stamp": map[string]any{"owner": "$auth.uid"}},
						"fields": map[string]any{
							// `featured` may be set only on a doc whose tier equals the caller's claim.
							"featured": map[string]any{"write": []any{map[string]any{"field": "tier", "op": "=", "value": "$auth.claims.tier"}}},
						},
					},
				},
			},
		},
	}
}

func (e *ruleEnv) useOrFixture() {
	e.reg.SetConfig(e.authInst, map[string]any{"access": orFixture()})
}

func TestORReadMergesOwnedAndPublic(t *testing.T) {
	e := newRuleEnv(t)
	e.useOrFixture()
	alice, bob := e.token("alice", "Alice"), e.token("bob", "Bob")

	// alice: one private (owned), one public+owned (the de-dup case). bob: one public, one private.
	if s, _ := e.call(t, "/docs/documents", `{"documents":[{"key":"a1","data":{"visibility":"private"}}]}`, alice); s != 200 {
		t.Fatal("alice a1 put failed")
	}
	if s, _ := e.call(t, "/docs/documents", `{"documents":[{"key":"a2","data":{"visibility":"public"}}]}`, alice); s != 200 {
		t.Fatal("alice a2 put failed")
	}
	if s, _ := e.call(t, "/docs/documents", `{"documents":[{"key":"b1","data":{"visibility":"public"}}]}`, bob); s != 200 {
		t.Fatal("bob b1 put failed")
	}
	if s, _ := e.call(t, "/docs/documents", `{"documents":[{"key":"b2","data":{"visibility":"private"}}]}`, bob); s != 200 {
		t.Fatal("bob b2 put failed")
	}

	// alice sees: a1 (owned), a2 (owned+public), b1 (public). NOT b2 (bob's private).
	status, out := e.call(t, "/docs/query", `{}`, alice)
	if status != 200 {
		t.Fatalf("query -> %d: %v", status, out)
	}
	got := keySet(t, out)
	want := map[string]bool{"a1": true, "a2": true, "b1": true}
	if len(got) != 3 || !sameSet(got, want) {
		t.Fatalf("OR read returned %v, want %v", got, want)
	}
	// a2 matches BOTH branches (owned AND public) yet appears exactly once — UNION de-dups.
	if countKey(out, "a2") != 1 {
		t.Fatalf("a2 should appear once (UNION de-dup), got %d", countKey(out, "a2"))
	}
}

func TestORReadAggregateCountsRowsOnce(t *testing.T) {
	e := newRuleEnv(t)
	e.useOrFixture()
	alice := e.token("alice", "Alice")
	// One doc that matches both OR branches (owned + public); the aggregate must count it once,
	// not once per branch (inline OR, not a UNION).
	if s, _ := e.call(t, "/docs/documents", `{"documents":[{"key":"a1","data":{"visibility":"public"}}]}`, alice); s != 200 {
		t.Fatal("put failed")
	}
	status, out := e.call(t, "/docs/aggregate", `{"metrics":[{"fn":"count","as":"n"}]}`, alice)
	if status != 200 {
		t.Fatalf("aggregate -> %d: %v", status, out)
	}
	groups, _ := out["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %v", out)
	}
	metrics, _ := groups[0].(map[string]any)["metrics"].(map[string]any)
	if metrics["n"].(float64) != 1 {
		t.Fatalf("OR aggregate double-counted a both-branch row: n=%v", metrics["n"])
	}
}

func TestFieldReadMaskingHidesForeignField(t *testing.T) {
	e := newRuleEnv(t)
	e.useOrFixture()
	alice, bob := e.token("alice", "Alice"), e.token("bob", "Bob")
	if s, _ := e.call(t, "/profiles/documents", `{"documents":[{"key":"alice","data":{"email":"alice@x.io","bio":"a"}}]}`, alice); s != 200 {
		t.Fatal("alice profile put failed")
	}
	if s, _ := e.call(t, "/profiles/documents", `{"documents":[{"key":"bob","data":{"email":"bob@x.io","bio":"b"}}]}`, bob); s != 200 {
		t.Fatal("bob profile put failed")
	}

	// bob queries: he reads BOTH profiles (read=authenticated), but email is masked out of
	// alice's row (he doesn't own it) while his own email stays visible.
	status, out := e.call(t, "/profiles/query", `{}`, bob)
	if status != 200 {
		t.Fatalf("query -> %d: %v", status, out)
	}
	docs, _ := out["documents"].([]any)
	if len(docs) != 2 {
		t.Fatalf("bob should read both profiles, got %v", out)
	}
	for _, d := range docs {
		m := d.(map[string]any)
		data := m["data"].(map[string]any)
		if m["key"] == "alice" {
			if _, ok := data["email"]; ok {
				t.Fatalf("alice's email must be masked from bob: %v", data)
			}
			if data["bio"] != "a" {
				t.Fatalf("non-masked fields must remain: %v", data)
			}
		} else {
			if data["email"] != "bob@x.io" {
				t.Fatalf("bob's own email must be visible: %v", data)
			}
		}
	}

	// A point-read masks the same way.
	status, got := e.call(t, "/profiles/documents/get", `{"keys":["alice","bob"]}`, bob)
	if status != 200 {
		t.Fatalf("get -> %d: %v", status, got)
	}
	for _, d := range got["documents"].([]any) {
		m := d.(map[string]any)
		data := m["data"].(map[string]any)
		_, hasEmail := data["email"]
		if m["key"] == "alice" && hasEmail {
			t.Fatalf("point-read leaked alice's masked email: %v", data)
		}
		if m["key"] == "bob" && !hasEmail {
			t.Fatalf("point-read masked bob's own email: %v", data)
		}
	}
}

func TestFieldWriteGateReservesField(t *testing.T) {
	e := newRuleEnv(t)
	e.useOrFixture()
	gold := e.tokenClaims("g", map[string]any{"tier": "gold"})
	silver := e.tokenClaims("s", map[string]any{"tier": "silver"})

	// gold caller may set `featured` on a gold-tier doc (doc.tier == claim tier).
	if s, out := e.call(t, "/items/documents", `{"documents":[{"key":"i1","data":{"tier":"gold","featured":true}}]}`, gold); s != 200 {
		t.Fatalf("gold setting featured on a gold doc -> %d: %v", s, out)
	}
	// gold caller may NOT set `featured` on a silver-tier doc (tier mismatch).
	if s, out := e.call(t, "/items/documents", `{"documents":[{"key":"i2","data":{"tier":"silver","featured":true}}]}`, gold); s != 403 {
		t.Fatalf("gold setting featured on a silver doc -> %d (want 403): %v", s, out)
	}
	// Not setting the gated field is always fine, whatever the tier.
	if s, out := e.call(t, "/items/documents", `{"documents":[{"key":"i3","data":{"tier":"silver"}}]}`, gold); s != 200 {
		t.Fatalf("creating without the gated field -> %d: %v", s, out)
	}
	// A silver caller can't set featured on a gold doc either.
	if s, out := e.call(t, "/items/documents", `{"documents":[{"key":"i4","data":{"tier":"gold","featured":true}}]}`, silver); s != 403 {
		t.Fatalf("silver setting featured on a gold doc -> %d (want 403): %v", s, out)
	}
}

// --- helpers ---

func keySet(t *testing.T, out map[string]any) map[string]bool {
	t.Helper()
	docs, _ := out["documents"].([]any)
	set := map[string]bool{}
	for _, d := range docs {
		set[d.(map[string]any)["key"].(string)] = true
	}
	return set
}

func countKey(out map[string]any, key string) int {
	docs, _ := out["documents"].([]any)
	n := 0
	for _, d := range docs {
		if d.(map[string]any)["key"] == key {
			n++
		}
	}
	return n
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
