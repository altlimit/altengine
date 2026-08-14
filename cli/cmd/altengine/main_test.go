package main

import "testing"

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
