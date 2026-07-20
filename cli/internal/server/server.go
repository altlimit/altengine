// Package server wires the emulator's HTTP surface: the four data planes
// (/v1/search, /v1/datastore, /v1/channel, /v1/auth), the control-plane admin API
// (/admin), and the embedded admin console — mounted in the same order as the hosted
// platform (data plane, then admin, then static assets).
package server

import (
	"log"
	"net/http"

	"github.com/altlimit/altengine/cli/internal/admin"
	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/identity"
	"github.com/altlimit/altengine/cli/internal/search"
)

// Options configures the emulator.
type Options struct {
	Addr    string
	DataDir string // "" => in-memory
	DevOpen bool
}

// Server is the assembled emulator.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds the server and all service handlers.
func New(opts Options) (*Server, error) {
	reg, err := control.New(opts.DataDir)
	if err != nil {
		return nil, err
	}
	a := auth.NewStore(opts.DevOpen)
	mux := http.NewServeMux()

	dsMgr := datastore.NewManager(opts.DataDir)
	srMgr := search.NewManager(opts.DataDir)
	idMgr := identity.NewManager(opts.DataDir)
	hub := channel.NewHub()
	idSvc := identity.NewService(reg, idMgr, opts.DevOpen)

	// Data planes first. The datastore and channel planes also accept END-USER identity
	// tokens minted by the auth plane: the datastore enforces row rules on them, and the
	// channel token mint scopes them to the auth instance's channel patterns. The
	// datastore's live bridge publishes committed changes straight into the hub.
	datastore.NewHandler(reg, a, dsMgr).WithIdentity(idSvc).WithLive(hub).Register(mux)
	search.NewHandler(reg, a, srMgr).Register(mux)
	channel.NewHandler(reg, a, hub).WithIdentity(idSvc).Register(mux)
	identity.NewHandler(reg, idSvc).Register(mux)

	// Admin last: its console handler is a catch-all on "/".
	admin.NewHandler(reg, a, dsMgr, srMgr, hub).Register(mux)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, 200, map[string]any{"ok": true})
	})

	return &Server{opts: opts, mux: mux}, nil
}

// Handler returns the root http.Handler (with a simple request log).
func (s *Server) Handler() http.Handler {
	return logMW(s.mux)
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	log.Printf("altengine listening on http://%s", s.opts.Addr)
	log.Printf("  admin console:  http://%s/", s.opts.Addr)
	log.Printf("  data:           %s", dataLabel(s.opts.DataDir))
	log.Printf("  api keys:       dev-open (any 'Authorization: Bearer <token>' works)")
	log.Printf("  auth service:   /v1/auth/{instance} — one-time codes are printed here")
	return http.ListenAndServe(s.opts.Addr, s.Handler())
}

func dataLabel(d string) string {
	if d == "" {
		return "in-memory (nothing persisted)"
	}
	return d
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" && r.Method != "OPTIONS" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
	})
}
