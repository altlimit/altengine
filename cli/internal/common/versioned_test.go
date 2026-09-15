package common

import "testing"

func TestParseVersionedName(t *testing.T) {
	valid := map[string]struct {
		base string
		n    int
	}{
		"myapp--v3":             {"myapp", 3},
		"my-app--v12":           {"my-app", 12},
		"abc123def--v999999999": {"abc123def", 999999999},
	}
	for in, want := range valid {
		base, n, ok := ParseVersionedName(in)
		if !ok || base != want.base || n != want.n {
			t.Errorf("ParseVersionedName(%q) = (%q, %d, %v), want (%q, %d, true)", in, base, n, ok, want.base, want.n)
		}
	}

	// One spelling per version, versions start at 1, and `--` alone is not a version.
	for _, in := range []string{
		"myapp--v0", "myapp--v03", "my--app", "myapp", "--v3", "myapp--v", "myapp--v1234567890", "myapp--V3", "myapp-v3",
	} {
		if base, n, ok := ParseVersionedName(in); ok {
			t.Errorf("ParseVersionedName(%q) = (%q, %d, true), want not versioned", in, base, n)
		}
	}
}
