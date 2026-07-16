// Package server wires the emulator's HTTP surface: the three data planes
// (/v1/search, /v1/datastore, /v1/channel), the control-plane admin API (/admin), and
// the embedded admin console — mounted in the same order as the hosted platform
// (data plane, then admin, then static assets).
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
	hub := channel.NewHub()

	datastore.NewHandler(reg, a, dsMgr).Register(mux)
	search.NewHandler(reg, a, srMgr).Register(mux)
	channel.NewHandler(reg, a, hub).Register(mux)

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
	log.Printf("  auth:           dev-open (any 'Authorization: Bearer <token>' works)")
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
