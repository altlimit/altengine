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

// A namespace that was only read holds no index, so it is not listed — hosted lists namespaces
// from its index registry. The index listing names the namespace it was read from.
func TestNamespacesListOnlyThoseWithIndexes(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		m := NewManager(dir)
		for _, ns := range []string{"", "7", "8"} {
			s, err := m.Open("inst", ns, true)
			if err != nil {
				t.Fatal(err)
			}
			if ns == "8" {
				if _, err := s.Put("products", []Document{{ID: "p", Fields: []Field{field("t", "atom", "x")}}}); err != nil {
					t.Fatal(err)
				}
			} else {
				s.Get("products", "p") // a read of an index that is not there: 404
			}
		}
		check := func(m *Manager, when string) {
			if got := m.Namespaces("inst"); strings.Join(got, "|") != "8" {
				t.Errorf("dir %q %s: namespaces = %q, want [8]", dir, when, got)
			}
		}
		check(m, "live")
		if dir != "" {
			check(NewManager(dir), "after restart")
		}
		s, _ := m.Open("inst", "8", true)
		rows, _, _, err := s.ListIndexes("", 10, "")
		if err != nil || len(rows) != 1 || rows[0]["namespace"] != "8" {
			t.Errorf("dir %q: ListIndexes in namespace 8 = %v (err %v), want products in namespace 8", dir, rows, err)
		}
	}
}
