package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

// A flag after the file argument used to be dropped without a word, so `deploy app.js --dry-run`
// deployed and `static deploy ./dist --no-activate` went live.
func TestFlagsAreParsedWhereverTheyAreWritten(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		dryRun   bool
		instance string
		rest     []string
	}{
		{"flags first", []string{"--dry-run", "--instance", "web", "app.js"}, true, "web", []string{"app.js"}},
		{"flags last", []string{"app.js", "--dry-run", "--instance", "web"}, true, "web", []string{"app.js"}},
		{"either side", []string{"--instance", "web", "app.js", "--dry-run"}, true, "web", []string{"app.js"}},
		{"single dash", []string{"app.js", "-dry-run"}, true, "", []string{"app.js"}},
		{"name=value", []string{"app.js", "--instance=web"}, false, "web", []string{"app.js"}},
		{"a value that looks like a flag", []string{"app.js", "--instance", "-web"}, false, "-web", []string{"app.js"}},
		{"-- ends the flags", []string{"--dry-run", "--", "--instance"}, true, "", []string{"--instance"}},
		{"a negative number is a value", []string{"key", "-5"}, false, "", []string{"key", "-5"}},
		{"no flags", []string{"a", "b"}, false, "", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			dryRun := fs.Bool("dry-run", false, "")
			instance := fs.String("instance", "", "")
			if err := fs.Parse(flagsFirst(fs, c.args)); err != nil {
				t.Fatal(err)
			}
			if *dryRun != c.dryRun || *instance != c.instance || !reflect.DeepEqual(fs.Args(), c.rest) {
				t.Fatalf("dry-run=%v instance=%q rest=%v", *dryRun, *instance, fs.Args())
			}
		})
	}
}

// A flag nobody defined is reported by name, not taken as a second file to deploy.
func TestAnUnknownFlagAfterTheFileIsRefused(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("dry-run", false, "")
	if err := fs.Parse(flagsFirst(fs, []string{"app.js", "--dryrun"})); err == nil {
		t.Fatal("a misspelled flag after the file was accepted")
	}
}

func TestParseGrants(t *testing.T) {
	// Empty means "inherit whatever the function already has" — the server treats an
	// absent grants map that way, so a routine redeploy never strips access.
	if g, err := parseGrants("  "); err != nil || g != nil {
		t.Fatalf("empty should yield nil, nil; got %v, %v", g, err)
	}

	g, err := parseGrants("datastore:appdb=full, search=read")
	if err != nil {
		t.Fatal(err)
	}
	if g["datastore:appdb"] != "full" || g["search"] != "read" || len(g) != 2 {
		t.Errorf("unexpected grants: %v", g)
	}

	for _, bad := range []string{"datastore:appdb=admin", "datastore:appdb", "=full"} {
		if _, err := parseGrants(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}
