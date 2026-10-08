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

// Mixed-case names are escaped now (so `Orders` and `orders` are two files on a case-insensitive
// filesystem), but a mixed-case namespace an older emulator stored verbatim still opens its data.
func TestLegacyMixedCaseNamespaceStillOpens(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "datastore", "inst")
	m := NewManager(dir)
	s, err := m.Open("inst", "probe", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put("things", []PutDoc{{Key: "k", Data: []byte(`{"v":1}`)}}); err != nil {
		t.Fatal(err)
	}
	// Where an older emulator would have put namespace "Orders".
	if err := os.Rename(filepath.Join(instDir, "probe.db"), filepath.Join(instDir, "Orders.db")); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir)
	if got := m2.Namespaces("inst"); len(got) != 1 || got[0] != "Orders" {
		t.Errorf("namespaces = %q, want [Orders]", got)
	}
	s2, err := m2.Open("inst", "Orders", "")
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := s2.Get("things", "k"); err != nil || doc == nil {
		t.Fatalf("legacy Orders data did not open: %v %v", doc, err)
	}
	// A namespace differing only in case is a separate, empty one.
	s3, err := m2.Open("inst", "orders", "")
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := s3.Get("things", "k"); doc != nil {
		t.Error("orders read Orders' document")
	}
	if existed, err := m2.Drop("inst", "Orders"); err != nil || !existed {
		t.Errorf("Drop(Orders) = %v, %v", existed, err)
	}
	if _, err := os.Stat(filepath.Join(instDir, "Orders.db")); !os.IsNotExist(err) {
		t.Errorf("Drop left the legacy file: %v", err)
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
