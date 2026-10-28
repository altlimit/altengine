package mailbox

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Options configures the mailbox service.
type Options struct {
	// Domain is the mail domain every address is on. Default DefaultDomain.
	Domain string
	// Base is this emulator's own origin (http://127.0.0.1:9191), for URLs minted outside a
	// request: the attachment links in a function or webhook payload.
	Base string
	// Loopback, when true (the default), delivers a send to an address on a local mailbox into
	// that mailbox — what the hosted service does too, by real mail.
	Loopback *bool
}

// Service is the mailbox service: the store, the inbound path, triggers and sending.
type Service struct {
	reg    *control.Registry
	store  *Store
	mux    http.Handler
	hub    *channel.Hub
	domain string
	base   string
	sig    *signer
	client *http.Client
	// queues lets a route hand mail to a task queue (nil = no tasks service).
	queues QueueSink

	loopback atomic.Bool

	orgMu   sync.Mutex
	orgDay  string
	orgSent int

	inflightMu sync.Mutex
	inflight   map[string]bool
	wg         sync.WaitGroup

	// now is the clock, swappable in tests.
	now func() int64
}

// NewService builds the service. mux is the emulator's router, used to call functions in
// process; hub is the channel hub routes publish into.
func NewService(reg *control.Registry, store *Store, mux http.Handler, hub *channel.Hub, o Options) *Service {
	d := NormalizeDomain(o.Domain)
	if d == "" {
		d = DefaultDomain
	}
	s := &Service{
		reg: reg, store: store, mux: mux, hub: hub, domain: d, base: o.Base, sig: newSigner(),
		client: &http.Client{
			Timeout:       WebhookTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		inflight: map[string]bool{},
		now:      func() int64 { return time.Now().UnixMilli() },
	}
	s.loopback.Store(o.Loopback == nil || *o.Loopback)
	return s
}

// Domain is the mail domain.
func (s *Service) Domain() string { return s.domain }

// Store is the underlying store.
func (s *Service) Store() *Store { return s.store }

// Loopback reports and sets whether sends to local addresses are delivered locally.
func (s *Service) Loopback() bool     { return s.loopback.Load() }
func (s *Service) SetLoopback(v bool) { s.loopback.Store(v) }

// Wait blocks until background deliveries started so far have finished (tests).
func (s *Service) Wait() { s.wg.Wait() }

// Drop is the instance-delete hook: every message, file and key the instance holds.
func (s *Service) Drop(instanceID string) { s.store.Drop(instanceID) }

// addressInfo is how an instance's addresses look: domain, main address and pattern. The address
// and pattern are null when the instance's name is not a valid slug.
func (s *Service) addressInfo(inst *control.Instance) map[string]any {
	slug := SlugOf(inst.Name)
	out := map[string]any{"domain": s.domain, "address": nil, "pattern": nil}
	if slug != "" {
		out["address"] = AddressFor(slug, DefaultLocal, s.domain)
		out["pattern"] = AddressPattern(slug, s.domain)
	}
	return out
}

func (s *Service) addressOf(inst *control.Instance, local string) any {
	slug := SlugOf(inst.Name)
	if slug == "" {
		return nil
	}
	return AddressFor(slug, local, s.domain)
}

// --- the org's daily send cap ----------------------------------------------------------------

// OrgSendsPerDay is the emulator's org-wide send cap. Hosted, an org may send 10 a day without a
// payment method and 100 with one. The emulator has no billing, so it applies the HIGHER of the
// two — local is never stricter than any hosted org, and never allows more than every hosted org
// does. It counts in memory: a restart starts the day over.
const OrgSendsPerDay = 100

func (s *Service) takeOrgSend(now int64) bool {
	s.orgMu.Lock()
	defer s.orgMu.Unlock()
	day := UTCDay(now)
	if s.orgDay != day {
		s.orgDay, s.orgSent = day, 0
	}
	if s.orgSent >= OrgSendsPerDay {
		return false
	}
	s.orgSent++
	return true
}

// --- signed download URLs --------------------------------------------------------------------

// Hosted, attachment and raw-message links are presigned storage URLs. The property that matters
// is reproduced: a bearer capability for one object that stops working after a while. The key is
// random per process.
type signer struct{ key []byte }

func newSigner() *signer {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &signer{key: k}
}

func (g *signer) mac(instanceID, messageID, part string, exp int64) string {
	h := hmac.New(sha256.New, g.key)
	fmt.Fprintf(h, "%s\n%s\n%s\n%d", instanceID, messageID, part, exp)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// signedURL is a download link for part "raw" or an attachment index.
func (s *Service) signedURL(origin, instanceID, messageID, part string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).UnixMilli()
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", s.sig.mac(instanceID, messageID, part, exp))
	p := "/_mailbox/" + url.PathEscape(instanceID) + "/" + url.PathEscape(messageID) + "/"
	if part == "raw" {
		p += "raw"
	} else {
		p += "attachments/" + part
	}
	return origin + p + "?" + q.Encode()
}

func (s *Service) verify(instanceID, messageID, part string, q url.Values) error {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		return common.Unauthenticated("this URL is missing its signature")
	}
	if time.Now().UnixMilli() > exp {
		return common.Unauthenticated("this URL has expired — ask for a new one")
	}
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.sig.mac(instanceID, messageID, part, exp))) {
		return common.Unauthenticated("this URL's signature is not valid")
	}
	return nil
}
