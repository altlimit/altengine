package auth

import (
	"net/http"
	"testing"

	"github.com/altlimit/altengine/cli/internal/control"
)

// Dev-open mode is what `altengine dev` runs by default, so a service missing from its grant map
// is not a small gap — it is that service being unusable locally, answering 403 to a developer
// who has done nothing wrong. That is exactly how the container service shipped: the map was
// hand-written and nobody added it.
//
// This test is the thing that makes registering a service enough.
func TestDevOpenGrantsEveryKnownService(t *testing.T) {
	s := NewStore(true)
	req, _ := http.NewRequest("GET", "http://local/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer whatever")

	id, err := s.Resolve(req)
	if err != nil {
		t.Fatalf("dev-open must accept any bearer token: %v", err)
	}
	for _, svc := range control.Services {
		if got := id.Grants.Effective(svc, "any-instance"); got != Full {
			t.Fatalf("dev-open grant for %q = %v, want Full — %q is in control.Services but not reachable locally", svc, got, svc)
		}
	}
}

// The realism path: a minted key gets exactly its grants and nothing else, so grant-scoping can
// be exercised locally rather than discovered in production.
func TestMintedKeyGetsOnlyItsGrants(t *testing.T) {
	s := NewStore(false)
	s.AddKey("tok", Grants{"blob:files": "read", "search": "write"})
	req, _ := http.NewRequest("GET", "http://local/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer tok")

	id, err := s.Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := id.Grants.Effective("blob", "files"); got != Read {
		t.Fatalf("blob:files = %v, want Read", got)
	}
	// An instance-scoped grant says nothing about any other instance.
	if got := id.Grants.Effective("blob", "other"); got != None {
		t.Fatalf("blob:other = %v, want None", got)
	}
	// A service-wide grant applies to every instance of that service.
	if got := id.Grants.Effective("search", "anything"); got != Write {
		t.Fatalf("search = %v, want Write", got)
	}
	if got := id.Grants.Effective("datastore", "x"); got != None {
		t.Fatalf("ungranted service = %v, want None", got)
	}

	// Without dev-open, an unknown token is not a caller at all.
	req.Header.Set("Authorization", "Bearer nope")
	if _, err := s.Resolve(req); err == nil {
		t.Fatal("an unminted token must be rejected when dev-open is off")
	}
}
