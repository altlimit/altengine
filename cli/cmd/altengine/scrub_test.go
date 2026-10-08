package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This repo is public, and the hosted service's infrastructure is not part of what it documents.
// Every tracked file is checked for the provider names and private paths that have leaked into
// it before. The list is ROT13-encoded so this file does not itself name them.
var scrubbed = []string{
	"pybhqsyner",     // provider
	"qhenoyr bowrpg", // storage primitive
	"jbexreq",        // runtime
	"jenatyre",       // deploy tool
	"syl.vb",         // compute provider
	"nygratvarncc",   // private repo
	"/ubzr/",         // a developer's home directory
}

// scrubbedExact entries are matched case-sensitively: the reserved env prefix is upper-case, and the
// lower-case letters turn up in ordinary words.
var scrubbedExact = []string{"SYL_"}

// The function-bindings contract must stay byte-identical with the hosted copy, so it is the one
// file this cannot hold to the rule.
var scrubExempt = map[string]bool{"conformance/fn-bindings.json": true}

func rot13(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			return 'A' + (r-'A'+13)%26
		}
		return r
	}, s)
}

func TestTrackedFilesNameNoPrivateInfrastructure(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	self, _ := filepath.Abs("scrub_test.go")
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || scrubExempt[rel] {
			continue
		}
		path := filepath.Join(root, rel)
		if path == self {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue // deleted in the working tree
		}
		lower := bytes.ToLower(b)
		for _, enc := range scrubbed {
			if bytes.Contains(lower, []byte(rot13(enc))) {
				t.Errorf("%s names %q", rel, enc+" (rot13)")
			}
		}
		for _, enc := range scrubbedExact {
			if bytes.Contains(b, []byte(rot13(enc))) {
				t.Errorf("%s names %q", rel, enc+" (rot13)")
			}
		}
	}
}
