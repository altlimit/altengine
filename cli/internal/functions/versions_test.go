package functions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
)

// newRetentionServer is newTestServer with the registry and store in reach, so a test can save
// config the way the console and MCP do and look at what is stored.
func newRetentionServer(t *testing.T) (*http.ServeMux, *control.Registry, *Store) {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewStore(true)
	mux := http.NewServeMux()
	datastore.NewHandler(reg, a, datastore.NewManager("")).Register(mux)
	store := NewStore("")
	NewHandler(reg, a, store, mux).Register(mux)
	return mux, reg, store
}

func deployVersion(t *testing.T, mux *http.ServeMux, name, body string, activate bool, grants map[string]string) {
	t.Helper()
	req := DeployRequest{
		Name:     name,
		Code:     fmt.Sprintf(`export default { fetch(request, env) { return new Response(%q + " " + Object.keys(env).sort().join(",")); } };`, body),
		Grants:   grants,
		Activate: &activate,
	}
	raw, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/functions/main/deploy", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 201 {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
}

func activateVersion(t *testing.T, mux *http.ServeMux, name string, version int) {
	t.Helper()
	raw, _ := json.Marshal(map[string]int{"version": version})
	r := httptest.NewRequest("POST", "/v1/functions/main/"+name+"/activate", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer dev")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
}

func storedVersions(s *Store, instanceID, name string) []int {
	out := []int{}
	for _, v := range s.Versions(instanceID, name) {
		out = append(out, v.Version)
	}
	return out
}

// The outage this pins: roll back, stage fixes without activating them, and the trim deletes the
// code the function is serving.
func TestPruneNeverDeletesTheActiveVersion(t *testing.T) {
	mux, reg, store := newRetentionServer(t)
	for i := 1; i <= 3; i++ {
		deployVersion(t, mux, "hello", fmt.Sprintf("v%d", i), true, nil)
	}
	in := reg.Get("functions", "main")
	if err := reg.SaveConfig(in, map[string]any{"keepVersions": float64(2)}); err != nil {
		t.Fatal(err)
	}
	activateVersion(t, mux, "hello", 2)
	for i := 4; i <= 5; i++ {
		deployVersion(t, mux, "hello", fmt.Sprintf("v%d", i), false, nil)
	}

	if got := storedVersions(store, in.ID, "hello"); !reflect.DeepEqual(got, []int{5, 4, 2}) {
		t.Fatalf("stored versions = %v, want [5 4 2]", got)
	}
	if _, ok := store.Code(in.ID, "hello", 2); !ok {
		t.Fatal("the active version's code was pruned")
	}
	if _, ok := store.Code(in.ID, "hello", 3); ok {
		t.Fatal("v3 is past retention and not active, but its code is still stored")
	}
	if got := invoke(t, mux, "/fn/main/hello").Body.String(); got != "v2 " {
		t.Fatalf("live function = %q, want v2", got)
	}
}

func TestLoweringRetentionPrunesImmediately(t *testing.T) {
	mux, reg, store := newRetentionServer(t)
	for i := 1; i <= 6; i++ {
		deployVersion(t, mux, "hello", fmt.Sprintf("v%d", i), true, nil)
		deployVersion(t, mux, "other", fmt.Sprintf("o%d", i), true, nil)
	}
	activateVersion(t, mux, "hello", 2)
	in := reg.Get("functions", "main")

	// Raising it prunes nothing.
	if err := reg.SaveConfig(in, map[string]any{"keepVersions": float64(20)}); err != nil {
		t.Fatal(err)
	}
	if got := storedVersions(store, in.ID, "hello"); len(got) != 6 {
		t.Fatalf("raising retention changed history: %v", got)
	}

	if err := reg.SaveConfig(in, map[string]any{"keepVersions": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if got := storedVersions(store, in.ID, "hello"); !reflect.DeepEqual(got, []int{6, 2}) {
		t.Fatalf("hello = %v, want [6 2]", got)
	}
	if got := storedVersions(store, in.ID, "other"); !reflect.DeepEqual(got, []int{6}) {
		t.Fatalf("other = %v, want [6]", got)
	}
	for _, v := range []int{1, 3, 4, 5} {
		if _, ok := store.Code(in.ID, "hello", v); ok {
			t.Errorf("hello v%d's code survived the prune", v)
		}
	}
}

func TestKeepVersionsIsBounded(t *testing.T) {
	_, reg, _ := newRetentionServer(t)
	in := reg.GetOrCreate("functions", "main")
	if KeepVersions(reg.ConfigSnapshot(in)) != DefaultKeepVersions {
		t.Fatalf("a new instance keeps %d, want %d", KeepVersions(reg.ConfigSnapshot(in)), DefaultKeepVersions)
	}
	for _, bad := range []any{float64(0), float64(MaxKeepVersions + 1), 1.5, "3", true} {
		err := reg.SaveConfig(in, map[string]any{"keepVersions": bad})
		var ae *common.APIError
		if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "INVALID_ARGUMENT" {
			t.Errorf("keepVersions=%v: err = %v, want 400 INVALID_ARGUMENT", bad, err)
		}
	}
	if got := reg.ConfigSnapshot(in)["keepVersions"]; got != 10 {
		t.Errorf("a refused save changed the setting to %v", got)
	}
	// The read path never trusts what is stored.
	for stored, want := range map[any]int{float64(0): 10, float64(99): 10, "x": 10, float64(25): 25} {
		if got := KeepVersions(map[string]any{"keepVersions": stored}); got != want {
			t.Errorf("KeepVersions(%v) = %d, want %d", stored, got, want)
		}
	}
}

// `/fn/{instance}--v{n}/{fn}` is the local `{slug}--v{n}-fn` host: stored code, current grants.
func TestVersionedAddressRunsStoredVersion(t *testing.T) {
	mux, _, _ := newRetentionServer(t)
	deployVersion(t, mux, "hello", "v1", true, nil)
	// v2 is stored but not live, and brings a grant the function now has.
	deployVersion(t, mux, "hello", "v2", false, map[string]string{"datastore": "read"})

	if got := invoke(t, mux, "/fn/main--v1/hello").Body.String(); got != "v1 datastore" {
		t.Errorf("v1 on its own address = %q, want v1's code with the CURRENT grants", got)
	}
	rec := invoke(t, mux, "/fn/main--v2/hello/world")
	if rec.Code != 200 || rec.Body.String() != "v2 datastore" {
		t.Errorf("v2 = %d %q", rec.Code, rec.Body.String())
	}
	if got := invoke(t, mux, "/fn/main/hello").Body.String(); got != "v1 datastore" {
		t.Errorf("live address = %q, want v1", got)
	}

	rec = invoke(t, mux, "/fn/main--v9/hello")
	if rec.Code != 404 {
		t.Fatalf("unknown version = %d, want 404", rec.Code)
	}
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "NOT_FOUND" || env.Error.Message != "function 'hello' has no version 9" {
		t.Errorf("unknown version body = %s", rec.Body.String())
	}

	for _, path := range []string{"/fn/main--v0/hello", "/fn/main--v01/hello", "/fn/ma--in/hello"} {
		if rec := invoke(t, mux, path); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestVersionListingCarriesURLs(t *testing.T) {
	mux, _, _ := newRetentionServer(t)
	deployVersion(t, mux, "hello", "v1", true, nil)
	deployVersion(t, mux, "hello", "v2", true, nil)

	rec := invoke(t, mux, "/v1/functions/main/hello/versions", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer dev")
	})
	var out struct {
		Versions []struct {
			Version int     `json:"version"`
			URL     *string `json:"url"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Versions) != 2 {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
	}
	for _, v := range out.Versions {
		want := fmt.Sprintf("/fn/main--v%d/hello", v.Version)
		if v.URL == nil || *v.URL != want {
			t.Errorf("v%d url = %v, want %s", v.Version, v.URL, want)
		}
		if got := invoke(t, mux, *v.URL).Body.String(); got != fmt.Sprintf("v%d ", v.Version) {
			t.Errorf("v%d's own url served %q", v.Version, got)
		}
	}
}
