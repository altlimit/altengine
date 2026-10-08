package search

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// `a b`, `a/b` and `a_b` used to share one database file; the default namespace and the simple
// names keep the files they always had.
func TestNamespaceFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	names := []string{"", "orders", "a b", "a/b", "a_b"}
	for _, ns := range names {
		s, err := m.Open("inst", ns, true)
		if err != nil {
			t.Fatalf("open %q: %v", ns, err)
		}
		if _, err := s.Put("idx", []Document{{ID: "d", Fields: []Field{field("ns", "atom", ns)}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range names {
		s, _ := m.Open("inst", ns, true)
		doc, err := s.Get("idx", "d")
		if err != nil || doc == nil || string(doc.Fields[0].Value) != `"`+ns+`"` {
			t.Errorf("namespace %q reads %+v (err %v), want its own document", ns, doc, err)
		}
	}
	for stem, ns := range map[string]string{"_default": "", "orders": "orders", "a_b": "a_b"} {
		if _, err := os.Stat(filepath.Join(dir, "search", "inst", stem+".db")); err != nil {
			t.Errorf("namespace %q is not at %s.db: %v", ns, stem, err)
		}
	}
	got := NewManager(dir).Namespaces("inst")
	sort.Strings(names)
	if strings.Join(got, "|") != strings.Join(names, "|") {
		t.Errorf("namespaces after restart = %q, want %q", got, names)
	}
}

func TestOpenRefusesInvalidNamespace(t *testing.T) {
	m := NewManager("")
	for _, ns := range []string{"_default", "a\x00b", strings.Repeat("x", 101), "café"} {
		if _, err := m.Open("inst", ns, true); err == nil {
			t.Errorf("Open(%q) accepted an invalid namespace", ns)
		}
	}
	if ns := m.Namespaces("inst"); len(ns) != 0 {
		t.Errorf("refused namespaces were recorded: %q", ns)
	}
}
