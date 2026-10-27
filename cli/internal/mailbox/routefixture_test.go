package mailbox

import (
	"encoding/json"
	"os"
	"testing"
)

// Which route a message takes, against the cases shared with the hosted service
// (conformance/fixtures/mailbox-routes.json, byte-identical there). A message that takes a
// different route locally than in production runs a different function, and nothing else would
// notice.

type routeFixture struct {
	Globs []struct {
		Pattern string `json:"pattern"`
		Value   string `json:"value"`
		Match   bool   `json:"match"`
	} `json:"globs"`
	Cases []struct {
		Name    string `json:"name"`
		Routes  []any  `json:"routes"`
		Message struct {
			Local           string `json:"local"`
			From            string `json:"from"`
			Subject         string `json:"subject"`
			AttachmentCount int    `json:"attachmentCount"`
		} `json:"message"`
		Expect *int `json:"expect"`
	} `json:"cases"`
}

func loadRouteFixture(t *testing.T) routeFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../conformance/fixtures/mailbox-routes.json")
	if err != nil {
		t.Fatalf("read the shared route fixture: %v", err)
	}
	var f routeFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Globs) == 0 || len(f.Cases) == 0 {
		t.Fatal("the shared route fixture is empty")
	}
	return f
}

func TestSharedGlobFixture(t *testing.T) {
	for _, g := range loadRouteFixture(t).Globs {
		if got := GlobMatch(g.Pattern, g.Value); got != g.Match {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", g.Pattern, g.Value, got, g.Match)
		}
	}
}

// Each case's routes go through what a saved config does — the strict write check, the
// normalized shape that is stored, a JSON round trip, the tolerant read — and then the matcher.
func TestSharedRouteFixture(t *testing.T) {
	for _, c := range loadRouteFixture(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg := map[string]any{"routes": c.Routes}
			if err := ValidateConfig(cfg); err != nil {
				t.Fatalf("routes refused: %v", err)
			}
			b, err := json.Marshal(NormalizedConfigMap(cfg))
			if err != nil {
				t.Fatal(err)
			}
			stored := map[string]any{}
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			routes := ParseConfig(stored).Routes
			if len(routes) != len(c.Routes) {
				t.Fatalf("%d routes saved, %d read back", len(c.Routes), len(routes))
			}
			m := c.Message
			hit := MatchRoute(routes, MatchInput{Local: m.Local, From: m.From, Subject: m.Subject, AttachmentCount: m.AttachmentCount})
			got := -1
			for i := range routes {
				if hit == &routes[i] {
					got = i
				}
			}
			want := -1
			if c.Expect != nil {
				want = *c.Expect
			}
			if got != want {
				t.Errorf("route %d matched, want %d (-1 = none)", got, want)
			}
		})
	}
}
