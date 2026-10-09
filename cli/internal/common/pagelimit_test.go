package common

import "testing"

// A limit that is not a finite number of at least 1 is the default, never "no limit".
func TestPageLimit(t *testing.T) {
	for raw, want := range map[string]int{"": 200, "abc": 200, "NaN": 200, "Inf": 200, "0": 200, "-3": 200, "5": 5, "5.9": 5, "5000": 1000} {
		if got := PageLimit(raw, 200, 1000); got != want {
			t.Errorf("PageLimit(%q) = %d, want %d", raw, got, want)
		}
	}
}
