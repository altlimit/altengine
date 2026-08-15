// Package auth emulates altengine's data-plane authentication: a Bearer API key
// resolves to an org + a grants map. For local development the emulator runs
// "dev-open": any non-empty Bearer token is accepted and mapped to the single dev org
// with full grants on every service. Explicitly minted keys (via the admin console)
// are still honored with their real grants so a developer can exercise grant-scoping
// locally.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/altlimit/altengine/cli/internal/common"
)

const DevOrgID = "dev"

// Level is a grant level; read < write < full (full includes delete).
type Level int

const (
	None Level = iota
	Read
	Write
	Full
)

func parseLevel(s string) Level {
	switch s {
	case "read":
		return Read
	case "write":
		return Write
	case "full":
		return Full
	}
	return None
}

// Grants maps "service" or "service:instance" to a level string.
type Grants map[string]string

// Effective returns the level for (service, instance): an instance-specific grant
// overrides the service-wide grant.
func (g Grants) Effective(service, instance string) Level {
	if v, ok := g[service+":"+instance]; ok {
		return parseLevel(v)
	}
	if v, ok := g[service]; ok {
		return parseLevel(v)
	}
	return None
}

// Identity is a resolved caller.
type Identity struct {
	OrgID  string
	Grants Grants
}

var bearerRe = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// Bearer extracts the bearer token from a request, or "".
func Bearer(r *http.Request) string {
	m := bearerRe.FindStringSubmatch(strings.TrimSpace(r.Header.Get("Authorization")))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// Store holds minted API keys (hash -> Identity) for the realism path. Dev-open mode
// resolves unknown tokens to full access rather than rejecting them.
type Store struct {
	mu      sync.RWMutex
	byHash  map[string]*Identity
	devOpen bool
}

func NewStore(devOpen bool) *Store {
	return &Store{byHash: map[string]*Identity{}, devOpen: devOpen}
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AddKey registers a minted key's plaintext with its grants.
func (s *Store) AddKey(plaintext string, grants Grants) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[hashKey(plaintext)] = &Identity{OrgID: DevOrgID, Grants: grants}
}

// RemoveKey revokes a minted key by plaintext.
func (s *Store) RemoveKey(plaintext string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHash, hashKey(plaintext))
}

// Resolve turns a request's Authorization into an Identity. Missing token -> 401.
// A minted key resolves to its grants; any other non-empty token resolves to full
// access in dev-open mode, else 401.
func (s *Store) Resolve(r *http.Request) (*Identity, error) {
	tok := Bearer(r)
	if tok == "" {
		return nil, common.Unauthenticated("missing or malformed Authorization header")
	}
	s.mu.RLock()
	id, ok := s.byHash[hashKey(tok)]
	s.mu.RUnlock()
	if ok {
		return id, nil
	}
	if s.devOpen {
		// Every service, or a new one silently 403s in dev-open mode — which reads as a
		// broken emulator rather than a missing line here.
		return &Identity{OrgID: DevOrgID, Grants: Grants{
			"search": "full", "channel": "full", "datastore": "full", "auth": "full", "functions": "full",
		}}, nil
	}
	return nil, common.Unauthenticated("invalid API key")
}

// Require enforces that the identity's grant for (service, instance) meets need.
func Require(id *Identity, service, instance string, need Level) error {
	if id.Grants.Effective(service, instance) < need {
		return common.PermissionDenied("insufficient grant for " + service)
	}
	return nil
}
