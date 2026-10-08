package main

import (
	"runtime/debug"
	"testing"
)

// A `go install` build stamps no version, so it printed "version dev" whatever it was built
// from. It now prints the module version Go recorded, or the commit.
func TestResolveVersion(t *testing.T) {
	info := func(v string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: v}, Settings: settings}, true
		}
	}
	cases := []struct {
		stamped string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"v0.3.0", info("v9.9.9"), "v0.3.0"},
		{"dev", info("v0.3.0"), "v0.3.0"},
		{"dev", info("(devel)", debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"}), "dev-0123456789ab"},
		{"dev", info("(devel)", debug.BuildSetting{Key: "vcs.revision", Value: "abc"}, debug.BuildSetting{Key: "vcs.modified", Value: "true"}), "dev-abc-dirty"},
		{"dev", info("(devel)"), "dev"},
		{"dev", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
	}
	for _, c := range cases {
		if got := resolveVersion(c.stamped, c.read); got != c.want {
			t.Errorf("resolveVersion(%q) = %q, want %q", c.stamped, got, c.want)
		}
	}
}
