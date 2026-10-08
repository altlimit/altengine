package common

import (
	"strings"
	"testing"
)

func TestNamespaceFileNameIsOneToOne(t *testing.T) {
	names := []string{"", "orders", "a_b", "a b", "a/b", "a.b", "A_b", "~", "~61", "_default", "..", strings.Repeat("~", 100)}
	seen := map[string]string{}
	for _, ns := range names {
		stem := NamespaceFileName(ns)
		if prev, dup := seen[stem]; dup {
			t.Errorf("%q and %q share the file name %q", prev, ns, stem)
		}
		seen[stem] = ns
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
	for _, ns := range []string{"orders", "tenant-1.prod_v2", "A_b"} {
		if NamespaceFileName(ns) != ns {
			t.Errorf("%q moved to %q", ns, NamespaceFileName(ns))
		}
	}
	// Stems nothing encodes to are not namespaces.
	for _, stem := range []string{"~", "~zz", "~6", "~6162", "a b"} {
		if ns, ok := NamespaceFromFileName(stem); ok {
			t.Errorf("NamespaceFromFileName(%q) = %q, want not ok", stem, ns)
		}
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
