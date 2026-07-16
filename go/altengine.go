// Package altengine is the official Go SDK for altengine
// (https://www.altengine.net) — managed datastore, search, and realtime
// channels behind one API key.
//
//	ae := altengine.New(altengine.WithAPIKey("ae_..."))
//
//	db := ae.Datastore("myapp")
//	keys, err := db.Put(ctx, "todos", []altengine.PutDocument{{Data: todo}})
//
//	idx := ae.Search("myapp").Index("products")
//	res, err := idx.Search(ctx, altengine.SearchRequest{Query: "shoes price<100"})
//
//	ch := ae.Channel("myapp")
//	tok, err := ch.CreateToken(ctx, altengine.TokenRequest{Channels: []string{"room:1"}})
package altengine

import (
	"os"
	"time"
)

// DefaultBaseURL is the production API origin used when no override is given.
const DefaultBaseURL = "https://api.altengine.net"

// DevBaseURL is where `altengine dev` (the local emulator) listens by default.
const DevBaseURL = "http://127.0.0.1:9191"

// Retry configures the retry policy for retryable failures
// (429/502/503/504/network). Transactions and publishes are never retried.
type Retry struct {
	// MaxAttempts is the total attempts including the first (default 3).
	// Set 1 to disable retries.
	MaxAttempts int
	// BaseDelay is the base backoff delay (default 250ms); doubles per attempt
	// with jitter.
	BaseDelay time.Duration
	// MaxDelay is the backoff ceiling (default 4s).
	MaxDelay time.Duration
}

// Client is the entry point: construct with New, then bind service clients
// with Datastore, Search, and Channel.
type Client struct {
	http *transport
}

// Option configures a Client.
type Option func(*clientConfig)

type clientConfig struct {
	baseURL    string
	dev        bool
	apiKey     string
	httpClient httpDoer
	timeout    time.Duration
	retry      Retry
}

// WithBaseURL sets an explicit API origin. Resolution order: this option →
// WithDev → the ALTENGINE_URL env var → https://api.altengine.net (production).
func WithBaseURL(url string) Option { return func(c *clientConfig) { c.baseURL = url } }

// WithDev targets the local emulator (`altengine dev`) at http://127.0.0.1:9191.
func WithDev() Option { return func(c *clientConfig) { c.dev = true } }

// WithAPIKey sets the org API key (Authorization: Bearer). Falls back to the
// ALTENGINE_API_KEY env var when unset.
func WithAPIKey(key string) Option { return func(c *clientConfig) { c.apiKey = key } }

// WithHTTPClient injects a custom *http.Client (or compatible Do-er).
func WithHTTPClient(h httpDoer) Option { return func(c *clientConfig) { c.httpClient = h } }

// WithTimeout sets the per-request timeout (default 30s).
func WithTimeout(d time.Duration) Option { return func(c *clientConfig) { c.timeout = d } }

// WithRetry overrides the default retry policy.
func WithRetry(r Retry) Option { return func(c *clientConfig) { c.retry = r } }

// New builds a client. With no options it targets production and reads the
// API key from ALTENGINE_API_KEY.
func New(opts ...Option) *Client {
	cfg := clientConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.baseURL == "" {
		if cfg.dev {
			cfg.baseURL = DevBaseURL
		} else if v := os.Getenv("ALTENGINE_URL"); v != "" {
			cfg.baseURL = v
		} else {
			cfg.baseURL = DefaultBaseURL
		}
	}
	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("ALTENGINE_API_KEY")
	}
	return &Client{http: newTransport(cfg)}
}
