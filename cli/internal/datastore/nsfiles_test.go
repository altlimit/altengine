package datastore

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

// `a b`, `a/b` and `a_b` used to share one database file, so writing to one wrote to all three.
func TestNamespacesThatOnceCollidedAreSeparate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	names := []string{"a b", "a/b", "a_b"}
	for _, ns := range names {
		s, err := m.Open("inst", ns, "")
		if err != nil {
			t.Fatalf("open %q: %v", ns, err)
		}
		if _, _, err := s.Put("things", []PutDoc{{Key: "k", Data: []byte(`{"ns":"` + ns + `"}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range names {
		s, _ := m.Open("inst", ns, "")
		doc, err := s.Get("things", "k")
		if err != nil || doc == nil || !strings.Contains(string(doc.Data), ns) {
			t.Errorf("namespace %q reads %v (err %v), want its own document", ns, doc, err)
		}
	}
	// And from disk, after a restart: three files, listed back under their own names.
	got := NewManager(dir).Namespaces("inst")
	sort.Strings(got)
	if want := []string{"a b", "a/b", "a_b"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("namespaces after restart = %q, want %q", got, want)
	}
}

// A namespace that was valid before keeps the file it always had, so existing local data opens.
func TestExistingSimpleNamespaceOpensItsOldFile(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	for _, ns := range []string{"", "orders", "tenant-1.prod_v2"} {
		if _, err := m.Open("inst", ns, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "datastore", "inst", ns+".db")); err != nil {
			t.Errorf("namespace %q is not at its old file: %v", ns, err)
		}
	}
}

func TestInvalidNamespaceIsRefused(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, ns := range []string{"_default", "a\x00b", strings.Repeat("x", 101), "café", "tab\there"} {
		_, err := m.Open("inst", ns, "")
		var ae *common.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != "INVALID_ARGUMENT" {
			t.Errorf("Open(%q) = %v, want 400 INVALID_ARGUMENT", ns, err)
		}
		if _, err := m.Drop("inst", ns); err == nil {
			t.Errorf("Drop(%q) accepted an invalid namespace", ns)
		}
	}
	if ns := m.Namespaces("inst"); len(ns) != 0 {
		t.Errorf("refused namespaces were created: %q", ns)
	}
}

// Over HTTP the refusal has the hosted shape.
func TestInvalidNamespaceOverHTTP(t *testing.T) {
	srv := nsEnv(t, "")
	for _, seg := range []string{"caf%C3%A9", strings.Repeat("x", 101), "a%00b"} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/datastore/appdb/ns/"+seg+"/col/things/documents/get",
			strings.NewReader(`{"keys":["k"]}`))
		req.Header.Set("Authorization", "Bearer devkey")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("namespace %s -> %d, want 400", seg, resp.StatusCode)
		}
	}
}
