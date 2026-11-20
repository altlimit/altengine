package manifest

// Static sites. The emulator has no static service, so a manifest's static instance is a site the
// emulator serves itself, with internal/devsite (the hosted path resolution), on a listener of its
// own — hosted a site has its own hostname, and locally the equivalent is its own port.
//
// THE PORT IS PART OF THE ORIGIN, and the origin is written into config (auth allowedOrigins,
// functions corsOrigins, a `${key:url}` env value). So a site's port is assigned once, kept in the
// install record, and the same port is opened again on every start.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/altlimit/altengine/cli/internal/devsite"
)

// maxPortScan bounds how far upward Assign looks for a free port.
const maxPortScan = 200

// SiteHost serves every deployed static site, one listener each.
type SiteHost struct {
	bindHost string
	urlHost  string
	first    int
	reserved map[int]bool

	mu   sync.Mutex
	open map[string]*liveSite // by instance id
}

type liveSite struct {
	name    string
	port    int
	ln      net.Listener
	srv     *http.Server
	handler atomic.Value // handlerBox
	// files is a downloaded site's content when there is no data directory to put it in.
	files map[string][]byte
}

// NewSiteHost serves sites on bindHost. URLs name urlHost. The first site tries port `first`;
// `reserved` ports (the API's, --static's) are never assigned.
func NewSiteHost(bindHost, urlHost string, first int, reserved []int) *SiteHost {
	h := &SiteHost{bindHost: bindHost, urlHost: urlHost, first: first, reserved: map[int]bool{}, open: map[string]*liveSite{}}
	for _, p := range reserved {
		h.reserved[p] = true
	}
	return h
}

// URL is the address a site on port is served at.
func (h *SiteHost) URL(port int) string {
	return "http://" + net.JoinHostPort(h.urlHost, strconv.Itoa(port)) + "/"
}

func (h *SiteHost) addr(port int) string { return net.JoinHostPort(h.bindHost, strconv.Itoa(port)) }

// Assign picks the port a new site gets: the first one at or above the starting port that no
// recorded site holds (`taken`), the emulator does not use, and nothing else is listening on.
func (h *SiteHost) Assign(taken map[int]bool) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for p := h.first; p < h.first+maxPortScan && p < 65536; p++ {
		if p <= 0 || h.reserved[p] || taken[p] {
			continue
		}
		inUse := false
		for _, s := range h.open {
			inUse = inUse || s.port == p
		}
		if inUse {
			continue
		}
		ln, err := net.Listen("tcp", h.addr(p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port for a site between %d and %d", h.first, h.first+maxPortScan-1)
}

// handlerBox gives atomic.Value one concrete type to hold, whatever the handler is.
type handlerBox struct{ h http.Handler }

type unpublished struct{ name string }

func (u unpublished) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, "the site %s has not been published yet", u.name)
}

// Open starts serving a site's port (with a "not published yet" page until Publish). Opening a
// site that is already open is a no-op.
func (h *SiteHost) Open(id, name string, port int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.open[id]; s != nil {
		if s.port == port {
			return nil
		}
		_ = s.srv.Close()
		delete(h.open, id)
	}
	ln, err := net.Listen("tcp", h.addr(port))
	if err != nil {
		return fmt.Errorf("site %s: port %d: %w", name, port, err)
	}
	s := &liveSite{name: name, port: port, ln: ln}
	s.handler.Store(handlerBox{unpublished{name}})
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.serve),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	h.open[id] = s
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("site %s stopped: %v", name, err)
		}
	}()
	return nil
}

// serve logs each request with its status, as the --static site does.
func (s *liveSite) serve(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.handler.Load().(handlerBox).h.ServeHTTP(rec, r)
	log.Printf("site %s %d %s %s", s.name, rec.status, r.Method, r.URL.Path)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Publish switches an open site to serve o (and, for an in-memory site, files).
func (h *SiteHost) Publish(id string, o devsite.Options, files map[string][]byte) error {
	site, err := devsite.New(o)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.open[id]
	if s == nil {
		return fmt.Errorf("site %s is not open", id)
	}
	s.files = files
	s.handler.Store(handlerBox{site})
	return nil
}

// Files returns an in-memory site's current files (nil when it is served from a directory).
func (h *SiteHost) Files(id string) map[string][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.open[id]; s != nil {
		return s.files
	}
	return nil
}

// Close stops every site.
func (h *SiteHost) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.open {
		_ = s.srv.Close()
		delete(h.open, id)
	}
}
