package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceFileNameIsOneToOne(t *testing.T) {
	names := []string{"", "orders", "Orders", "ORDERS", "a_b", "a b", "a/b", "a.b", "A_b", "~", "~61", "_default", "..",
		strings.Repeat("~", 100), "con", "CON", "nul.txt", "com1", "lpt9.db"}
	seen := map[string]string{}
	for _, ns := range names {
		stem := NamespaceFileName(ns)
		// Compared folded: on a case-insensitive filesystem that is what makes two files one.
		if prev, dup := seen[strings.ToLower(stem)]; dup {
			t.Errorf("%q and %q share the file name %q", prev, ns, stem)
		}
		seen[strings.ToLower(stem)] = ns
		if back, ok := NamespaceFromFileName(stem); !ok || back != ns {
			t.Errorf("NamespaceFromFileName(%q) = (%q, %v), want %q", stem, back, ok, ns)
		}
		if len(stem)+len(".db") > 255 {
			t.Errorf("%q encodes to %d bytes, over a file name's limit", ns, len(stem))
		}
		if strings.ContainsAny(stem, `/\:*?"<>|% `) {
			t.Errorf("%q encodes to %q, which is not a safe file name", ns, stem)
		}
	}
	// The names that were already safe keep the file they always had.
	for _, ns := range []string{"orders", "tenant-1.prod_v2", "console", "com0", "lpt10", "nul_x"} {
		if NamespaceFileName(ns) != ns {
			t.Errorf("%q moved to %q", ns, NamespaceFileName(ns))
		}
	}
	// Uppercase and Windows device names are escaped.
	for _, ns := range []string{"A_b", "Orders", "con", "Aux", "nul.txt", "com1", "LPT9", "prn.db"} {
		if !strings.HasPrefix(NamespaceFileName(ns), "~") {
			t.Errorf("%q is stored verbatim as %q", ns, NamespaceFileName(ns))
		}
	}
	// A stem an older emulator wrote verbatim still reads back as its namespace.
	if ns, ok := NamespaceFromFileName("Orders"); !ok || ns != "Orders" {
		t.Errorf("legacy stem Orders = (%q, %v)", ns, ok)
	}
	// Stems nothing encodes to are not namespaces.
	for _, stem := range []string{"~", "~zz", "~6", "~6162", "~4F", "a b"} {
		if ns, ok := NamespaceFromFileName(stem); ok {
			t.Errorf("NamespaceFromFileName(%q) = %q, want not ok", stem, ns)
		}
	}
}

// A namespace an older emulator stored verbatim keeps its file; a different-case file is not it.
func TestNamespaceFileKeepsALegacyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Orders.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "con.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for ns, want := range map[string]string{
		"Orders": "Orders.db",
		"con":    "con.db",
		"ORDERS": NamespaceFileName("ORDERS") + ".db", // not Orders.db, whatever the filesystem
		"orders": "orders.db",
		"Fresh":  NamespaceFileName("Fresh") + ".db",
	} {
		if got := NamespaceFile(dir, ns, NamespaceFileName(ns)); got != filepath.Join(dir, want) {
			t.Errorf("NamespaceFile(%q) = %s, want %s", ns, filepath.Base(got), want)
		}
	}
	// Once the escaped file exists it wins.
	if err := os.WriteFile(filepath.Join(dir, NamespaceFileName("Orders")+".db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NamespaceFile(dir, "Orders", NamespaceFileName("Orders")); filepath.Base(got) != NamespaceFileName("Orders")+".db" {
		t.Errorf("NamespaceFile(Orders) = %s, want the escaped file", filepath.Base(got))
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, ok := range []string{"", "a", "a b", "a/b", strings.Repeat("x", 100), "~!@#"} {
		if err := ValidateNamespace(ok); err != nil {
			t.Errorf("ValidateNamespace(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"_default", "a\x00b", strings.Repeat("x", 101), "café", "a\nb", "\x7f"} {
		err := ValidateNamespace(bad)
		if ae, ok := err.(*APIError); !ok || ae.Status != 400 || ae.Code != "INVALID_ARGUMENT" {
			t.Errorf("ValidateNamespace(%q) = %v, want 400 INVALID_ARGUMENT", bad, err)
		}
	}
}
