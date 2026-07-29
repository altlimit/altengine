package datastore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/identity"
)

// Row-rule enforcement for END-USER identity tokens. The fixture mirrors a real
// forum-shaped app: posts are readable by any signed-in user, authored rows are stamped
// with the author's uid from the VERIFIED token, and only the author may update or delete
// them. `notes` is owner-private; `secrets` is deliberately absent from the rules so the
// default-deny behavior is covered.
func rulesFixture() map[string]any {
	return map[string]any{
		"datastore:appdb": map[string]any{
			"level": "full",
			"rules": map[string]any{
				"posts": map[string]any{
					"read": "authenticated",
					"create": map[string]any{
						// `author_name` is stamped from the self-asserted PROFILE (a display
						// value); `author_uid` from the authoritative token uid.
						"stamp": map[string]any{"author_uid": "$auth.uid", "author_name": "$auth.profile.name"},
					},
					"update": map[string]any{
						"match":     []any{map[string]any{"field": "author_uid", "op": "=", "value": "$auth.uid"}},
						"immutable": []any{"author_uid", "board"},
					},
					"delete": map[string]any{
						"match": []any{map[string]any{"field": "author_uid", "op": "=", "value": "$auth.uid"}},
					},
				},
				"notes": map[string]any{
					"read":   []any{map[string]any{"field": "owner", "op": "=", "value": "$auth.uid"}},
					"create": map[string]any{"stamp": map[string]any{"owner": "$auth.uid"}},
				},
				// `author_uid` is BOTH stamped on update AND immutable — the overlap that
				// pins the order of those two steps (TestImmutableIsCheckedBeforeStamping).
				"articles": map[string]any{
					"read":   "authenticated",
					"create": map[string]any{"stamp": map[string]any{"author_uid": "$auth.uid"}},
					"update": map[string]any{
						"match":     []any{map[string]any{"field": "author_uid", "op": "=", "value": "$auth.uid"}},
						"stamp":     map[string]any{"author_uid": "$auth.uid"},
						"immutable": []any{"author_uid"},
					},
				},
			},
		},
		"channel:appfeed": map[string]any{
			"level":    "read",
			"channels": []any{"posts.*", "dm.$auth.uid"},
		},
	}
}

type ruleEnv struct {
	srv      *httptest.Server
	reg      *control.Registry
	authInst *control.Instance
}

func newRuleEnv(t *testing.T) *ruleEnv {
	t.Helper()
	reg, _ := control.New("")
	keys := auth.NewStore(true)
	idSvc := identity.NewService(reg, identity.NewManager(""), true)
	mux := http.NewServeMux()
	NewHandler(reg, keys, NewManager("")).WithIdentity(idSvc).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	authInst := reg.GetOrCreate("auth", "appauth")
	reg.SetConfig(authInst, map[string]any{"access": rulesFixture()})
	return &ruleEnv{srv: srv, reg: reg, authInst: authInst}
}

// token mints an identity token directly (the auth data plane's own round-trip is covered
// in the identity package's tests). `name` is a signup-collected field, so it rides in the
// self-asserted PROFILE bag (read as $auth.profile.name), not the authoritative claims.
func (e *ruleEnv) token(uid, name string) string {
	return identity.SignIdentity(identity.IdentityClaims{
		Iss:        e.authInst.ID,
		Sub:        uid,
		Identifier: uid + "@example.com",
		Email:      uid + "@example.com",
		Profile:    map[string]any{"name": name},
		Exp:        time.Now().Unix() + 3600,
	}, e.authInst.Secret)
}

func (e *ruleEnv) call(t *testing.T, path, body, bearer string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+"/v1/datastore/appdb/ns/_default/col"+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func firstKey(t *testing.T, out map[string]any) string {
	t.Helper()
	keys, _ := out["keys"].([]any)
	if len(keys) == 0 {
		t.Fatalf("expected a key in %v", out)
	}
	return keys[0].(string)
}

func TestStampMakesAuthorshipForgeProof(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")

	// Alice claims Bob wrote it — the stamp overwrites the forged value from the token.
	status, out := e.call(t, "/posts/documents",
		`{"documents":[{"key":"p1","data":{"title":"hello","author_uid":"bob","author_name":"Bob"}}]}`, alice)
	if status != 200 {
		t.Fatalf("put -> %d: %v", status, out)
	}
	status, got := e.call(t, "/posts/documents/get", `{"keys":["p1"]}`, alice)
	if status != 200 {
		t.Fatalf("get -> %d: %v", status, got)
	}
	docs, _ := got["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %v", got)
	}
	data, _ := docs[0].(map[string]any)["data"].(map[string]any)
	if data["author_uid"] != "alice" || data["author_name"] != "Alice" {
		t.Fatalf("stamp did not overwrite the forged author: %v", data)
	}
}

func TestUserCannotWriteAnotherUsersRow(t *testing.T) {
	e := newRuleEnv(t)
	alice, bob := e.token("alice", "Alice"), e.token("bob", "Bob")

	if status, out := e.call(t, "/posts/documents", `{"documents":[{"key":"p1","data":{"title":"alice's"}}]}`, alice); status != 200 {
		t.Fatalf("alice put -> %d: %v", status, out)
	}
	// Bob updating Alice's row fails the update rule's match against the EXISTING row.
	status, out := e.call(t, "/posts/documents", `{"documents":[{"key":"p1","data":{"title":"hijacked"}}]}`, bob)
	if status != 403 {
		t.Fatalf("bob update of alice's row -> %d: %v", status, out)
	}
	// ...and so does deleting it.
	status, out = e.call(t, "/posts/documents/delete", `{"keys":["p1"]}`, bob)
	if status != 403 {
		t.Fatalf("bob delete of alice's row -> %d: %v", status, out)
	}
	// Alice may delete her own.
	status, out = e.call(t, "/posts/documents/delete", `{"keys":["p1"]}`, alice)
	if status != 200 || out["deleted"].(float64) != 1 {
		t.Fatalf("alice delete -> %d: %v", status, out)
	}
}

func TestImmutableFieldsRejected(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")
	if status, out := e.call(t, "/posts/documents",
		`{"documents":[{"key":"p1","data":{"title":"t","board":"general"}}]}`, alice); status != 200 {
		t.Fatalf("create -> %d: %v", status, out)
	}
	// Changing an immutable field is refused...
	status, out := e.call(t, "/posts/documents",
		`{"documents":[{"key":"p1","data":{"title":"t2","board":"offtopic","author_uid":"alice"}}]}`, alice)
	if status != 403 {
		t.Fatalf("immutable change -> %d: %v", status, out)
	}
	// ...as is dropping one (a put replaces the whole document, so an omitted immutable
	// field is a change like any other).
	status, out = e.call(t, "/posts/documents", `{"documents":[{"key":"p1","data":{"title":"t2"}}]}`, alice)
	if status != 403 {
		t.Fatalf("dropping an immutable field -> %d: %v", status, out)
	}
	// ...while carrying them through unchanged is fine.
	if status, out := e.call(t, "/posts/documents",
		`{"documents":[{"key":"p1","data":{"title":"t2","board":"general","author_uid":"alice"}}]}`, alice); status != 200 {
		t.Fatalf("legal update -> %d: %v", status, out)
	}
}

// When a field is both stamped and immutable on update, the immutable comparison must run
// against the CLIENT's document, BEFORE the stamp is applied. Stamping first would rewrite
// the forged value to the caller's own uid, so the comparison would always find them equal
// and silently accept a request the hosted service rejects with 403 — an emulator that is
// more permissive than production is worse than no emulator.
func TestImmutableIsCheckedBeforeStamping(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")
	if status, out := e.call(t, "/articles/documents",
		`{"documents":[{"key":"a1","data":{"title":"t"}}]}`, alice); status != 200 {
		t.Fatalf("create -> %d: %v", status, out)
	}
	// Alice owns a1 (so `match` passes) but tries to reassign authorship to bob.
	status, out := e.call(t, "/articles/documents",
		`{"documents":[{"key":"a1","data":{"title":"t2","author_uid":"bob"}}]}`, alice)
	if status != 403 {
		t.Fatalf("forged author_uid on an immutable+stamped field -> %d (want 403): %v", status, out)
	}
	// Carrying the real value through is still a legal update.
	if status, out := e.call(t, "/articles/documents",
		`{"documents":[{"key":"a1","data":{"title":"t2","author_uid":"alice"}}]}`, alice); status != 200 {
		t.Fatalf("legal update -> %d: %v", status, out)
	}
}

func TestReadRulesScopeQueriesAndPointReads(t *testing.T) {
	e := newRuleEnv(t)
	alice, bob := e.token("alice", "Alice"), e.token("bob", "Bob")

	if status, _ := e.call(t, "/notes/documents", `{"documents":[{"key":"n1","data":{"body":"alice note"}}]}`, alice); status != 200 {
		t.Fatal("alice note put failed")
	}
	if status, _ := e.call(t, "/notes/documents", `{"documents":[{"key":"n2","data":{"body":"bob note"}}]}`, bob); status != 200 {
		t.Fatal("bob note put failed")
	}

	// A query only ever returns the caller's own rows: the rule filters are ANDed in.
	status, out := e.call(t, "/notes/query", `{}`, alice)
	if status != 200 {
		t.Fatalf("query -> %d: %v", status, out)
	}
	docs, _ := out["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("alice should see exactly her own note, got %v", out)
	}
	if docs[0].(map[string]any)["key"] != "n1" {
		t.Fatalf("alice saw the wrong row: %v", docs[0])
	}

	// A point-read of someone else's key returns nothing rather than leaking the row.
	status, got := e.call(t, "/notes/documents/get", `{"keys":["n1","n2"]}`, alice)
	if status != 200 {
		t.Fatalf("get -> %d: %v", status, got)
	}
	docs, _ = got["documents"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["key"] != "n1" {
		t.Fatalf("point read leaked a foreign row: %v", got)
	}
}

func TestDefaultDenyForUnlistedCollectionAndMode(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")

	// `secrets` isn't listed in the rules at all.
	if status, out := e.call(t, "/secrets/documents", `{"documents":[{"data":{"x":1}}]}`, alice); status != 403 {
		t.Fatalf("create on an unlisted collection -> %d: %v", status, out)
	}
	if status, out := e.call(t, "/secrets/query", `{}`, alice); status != 403 {
		t.Fatalf("read on an unlisted collection -> %d: %v", status, out)
	}
	// `notes` has no update or delete rule — those modes are denied even for the owner.
	if status, _ := e.call(t, "/notes/documents", `{"documents":[{"key":"n1","data":{"body":"mine"}}]}`, alice); status != 200 {
		t.Fatal("notes create should be allowed")
	}
	if status, out := e.call(t, "/notes/documents", `{"documents":[{"key":"n1","data":{"body":"edited"}}]}`, alice); status != 403 {
		t.Fatalf("update with no update rule -> %d: %v", status, out)
	}
	if status, out := e.call(t, "/notes/documents/delete", `{"keys":["n1"]}`, alice); status != 403 {
		t.Fatalf("delete with no delete rule -> %d: %v", status, out)
	}
}

func TestBackendOnlySurfacesRefuseIdentityTokens(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")

	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/datastore/appdb/ns/_default/transaction",
		bytes.NewBufferString(`{"operations":[]}`))
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("transaction with an identity token -> %d (want 403)", resp.StatusCode)
	}

	if status, out := e.call(t, "/posts/indexes", `{"fields":["title"]}`, alice); status != 403 {
		t.Fatalf("index management with an identity token -> %d: %v", status, out)
	}
}

func TestApiKeyBypassesRowRules(t *testing.T) {
	e := newRuleEnv(t)
	alice := e.token("alice", "Alice")
	if status, _ := e.call(t, "/posts/documents", `{"documents":[{"key":"p1","data":{"title":"alice's"}}]}`, alice); status != 200 {
		t.Fatal("alice put failed")
	}
	// A trusted backend credential is full-trust: no stamping, no owner match, no denial.
	status, out := e.call(t, "/posts/documents", `{"documents":[{"key":"p1","data":{"title":"backend edit"}}]}`, "devkey")
	if status != 200 {
		t.Fatalf("api-key update -> %d: %v", status, out)
	}
	if status, out := e.call(t, "/secrets/documents", `{"documents":[{"key":"s1","data":{"x":1}}]}`, "devkey"); status != 200 {
		t.Fatalf("api-key write to an unruled collection -> %d: %v", status, out)
	}
}

func TestUnresolvablePlaceholderIsAHardDeny(t *testing.T) {
	e := newRuleEnv(t)
	// A rule referencing a claim this identity doesn't carry must deny, never drop the
	// constraint and hand back everyone's rows.
	reg := e.reg
	reg.SetConfig(e.authInst, map[string]any{"access": map[string]any{
		"datastore:appdb": map[string]any{
			"level": "full",
			"rules": map[string]any{
				"team": map[string]any{
					"read": []any{map[string]any{"field": "team", "op": "=", "value": "$auth.claims.team"}},
				},
			},
		},
	}})
	noTeam := identity.SignIdentity(identity.IdentityClaims{
		Iss: e.authInst.ID, Sub: "alice", Identifier: "alice@example.com",
		Claims: map[string]any{"name": "Alice"}, Exp: time.Now().Unix() + 3600,
	}, e.authInst.Secret)

	if status, out := e.call(t, "/team/query", `{}`, noTeam); status != 403 {
		t.Fatalf("missing claim in a rule -> %d: %v (want 403)", status, out)
	}
}

func TestIdentityWithoutAccessToInstanceIsDenied(t *testing.T) {
	e := newRuleEnv(t)
	reg := e.reg
	// The token's auth instance grants nothing on `appdb`.
	reg.SetConfig(e.authInst, map[string]any{"access": map[string]any{}})
	tok := e.token("alice", "Alice")
	if status, out := e.call(t, "/posts/query", `{}`, tok); status != 403 {
		t.Fatalf("no access entry -> %d: %v", status, out)
	}
}
