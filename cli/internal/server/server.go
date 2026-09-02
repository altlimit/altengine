// Package server wires the emulator's HTTP surface: the four data planes
// (/v1/search, /v1/datastore, /v1/channel, /v1/auth), the control-plane admin API
// (/admin), and the embedded admin console — mounted in the same order as the hosted
// platform (data plane, then admin, then static assets).
package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/admin"
	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/container"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/devsite"
	"github.com/altlimit/altengine/cli/internal/functions"
	"github.com/altlimit/altengine/cli/internal/identity"
	"github.com/altlimit/altengine/cli/internal/mcp"
	"github.com/altlimit/altengine/cli/internal/search"
)

// Options configures the emulator.
type Options struct {
	Addr    string
	DataDir string // "" => in-memory
	DevOpen bool

	// StaticDir, when set, serves that build output directory as a site — the local stand-in for
	// a deployment on the hosted static service. It gets its OWN LISTENER rather than a path
	// prefix under the API, because a generator emits root-absolute URLs (/assets/app.js) and a
	// site mounted under a prefix has none of them resolve. Hosted, a site has its own hostname;
	// locally the equivalent is its own port.
	StaticDir  string
	StaticAddr string // defaults to the API port + 1
	StaticSPA  *bool  // nil => detected from the directory
}

// Server is the assembled emulator.
type Server struct {
	opts Options
	fn   *functions.Handler
	mux  *http.ServeMux

	site   *devsite.Server
	siteLn net.Listener
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

	// Blob. With a data directory the objects are files on disk beside the other services'
	// databases — a blobkey stored in a local datastore document has to still resolve after a
	// restart, or the local app breaks in a way the hosted one does not.
	blob.NewHandler(reg, a, blob.NewStore(opts.DataDir)).Register(mux)

	// Functions. The stubs a function gets (env.datastore, env.search, ...) dispatch
	// IN-PROCESS into this same mux, so a call from a function goes through the very
	// handlers the REST API uses rather than a second implementation of them. Registered
	// after the data planes it dispatches into — the mux is shared, so the routes those
	// calls target must already be mounted.
	fnHandler := functions.NewHandler(reg, a, functions.NewStore(opts.DataDir), mux).WithHost(opts.Addr)
	fnHandler.Register(mux)

	// Containers, after functions: a job's completion callback is a function invocation
	// dispatched into this same mux, so the route it targets has to be mounted already.
	// Jobs live in memory only — a container that outlives the emulator process is not
	// something a restart should adopt.
	container.NewHandler(reg, a, container.NewStore(nil), mux).Register(mux)

	// Admin before MCP: MCP's tools dispatch into this same mux, and some of them (the
	// datastore collection listing) reach an /admin route, so those must already be mounted.
	// Its console handler is a catch-all on "/", which is also why MCP mounts an explicit
	// "POST /mcp" — an unmatched path here returns the console's HTML, not a 404.
	admin.NewHandler(reg, a, dsMgr, srMgr, hub).WithIdentity(idMgr).Register(mux)

	// MCP: the same agent surface the hosted service exposes, so an AI agent can build
	// against a local server instead of doing its experimenting in production.
	mcp.NewHandler(reg, mux, opts.DevOpen).Register(mux)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, 200, map[string]any{"ok": true})
	})

	// The static data plane exists hosted and not here, and the console's catch-all on "/" would
	// otherwise answer a deploy attempt with a page of HTML — which reads as a broken URL rather
	// than an unimplemented one. Say what to do instead.
	staticStub := common.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		return common.NewError(http.StatusNotImplemented,
			"the emulator does not store deployments: run `altengine dev --static ./dist` to serve a built site locally, "+
				"and point `altengine static deploy` at the hosted service (ALTENGINE_URL=https://api.altengine.net)",
			"UNIMPLEMENTED")
	})
	mux.HandleFunc("/v1/static", staticStub)
	mux.HandleFunc("/v1/static/", staticStub)

	srv := &Server{opts: opts, mux: mux, fn: fnHandler}

	if opts.StaticDir != "" {
		site, err := devsite.New(devsite.Options{
			Dir:       opts.StaticDir,
			SPA:       opts.StaticSPA,
			CleanURLs: true,
			NotFound:  "/404.html",
		})
		if err != nil {
			return nil, err
		}
		// Bound here rather than in ListenAndServe so a port already in use is a startup error
		// with the rest of them, not a goroutine that dies after the banner has printed.
		ln, err := net.Listen("tcp", srv.staticAddr())
		if err != nil {
			return nil, err
		}
		srv.site, srv.siteLn = site, ln
	}

	return srv, nil
}

// staticAddr is the site's listen address: the flag if given, otherwise the API port + 1.
func (s *Server) staticAddr() string {
	if s.opts.StaticAddr != "" {
		return s.opts.StaticAddr
	}
	host, port, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return s.opts.Addr
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return s.opts.Addr
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1))
}

// Handler returns the root http.Handler (with CORS and a simple request log).
func (s *Server) Handler() http.Handler {
	return logMW(corsMW(s.mux))
}

// ListenAndServe starts the HTTP server.
//
// The function scheduler starts HERE rather than in New, so building a server in a test
// does not spawn a ticker goroutine that outlives the test.
func (s *Server) ListenAndServe() error {
	s.fn.StartScheduler(context.Background())
	log.Printf("altengine listening on http://%s", s.opts.Addr)
	log.Printf("  admin console:  http://%s/", s.opts.Addr)
	log.Printf("  data:           %s", dataLabel(s.opts.DataDir))
	log.Printf("  api keys:       dev-open (any 'Authorization: Bearer <token>' works)")
	log.Printf("  auth service:   /v1/auth/{instance} — one-time codes are printed here")
	log.Printf("  functions:      /fn/{instance}/{function} — console.log lands here")
	log.Printf("  blob:           /v1/blob/{instance} — public objects at /blob/{instance}/{name}/{id}")
	log.Printf("  containers:     /v1/container/{instance} — jobs run on your local Docker daemon")
	log.Printf("  scheduler:      on, ticking each minute (UTC) for functions with a schedule")
	log.Printf("  mcp:            POST http://%s/mcp — point an AI agent here (any bearer token)", s.opts.Addr)
	s.serveSite()
	return http.ListenAndServe(s.opts.Addr, s.Handler())
}

// serveSite starts the static site listener, if one was configured.
//
// The site is a DIFFERENT ORIGIN from the API here, exactly as it is deployed (a site host and
// api.altengine.net are different origins too). So the app's fetches go to the API's absolute
// URL and are subject to CORS — which the emulator allows on /v1/* — rather than working
// same-origin locally and failing the first time they are deployed.
func (s *Server) serveSite() {
	if s.site == nil {
		return
	}
	log.Printf("  static site:    http://%s/ — serving %s", s.siteLn.Addr(), s.site.Dir())
	for _, line := range s.site.Summary() {
		log.Printf("                  %s", line)
	}
	log.Printf("                  read from disk on every request — rebuild and reload, no deploy step")
	log.Printf("                  served no-cache locally; deployed, fingerprinted files get a year")
	go func() {
		if err := http.Serve(s.siteLn, siteLogMW(s.site)); err != nil {
			log.Printf("static site server stopped: %v", err)
		}
	}()
}

func dataLabel(d string) string {
	if d == "" {
		return "in-memory (nothing persisted)"
	}
	return d
}

// corsMW makes the data plane reachable from a browser app served on another origin — the
// normal local setup (a Vite dev server on :5173 talking to the emulator on :9191), and the
// whole point of the auth service. It mirrors the hosted behavior: the request Origin is
// REFLECTED (with `Vary: Origin`) rather than a blanket "*", and credentials are deliberately
// NOT allowed. That is safe because these endpoints carry no cookies — they authenticate with
// a Bearer token that JS must attach explicitly, so a foreign page has no ambient credential
// to ride.
//
// Scoped to /v1/* and /_blob/* on purpose. The admin plane is same-origin (the console is served
// from this same server) and, hosted, is cookie-authed with a CSRF Origin check — giving it CORS
// here would teach a habit the hosted service does not allow.
//
// /_blob is where a browser PUTs an upload's bytes. Hosted that PUT goes to object storage on
// another origin entirely, where CORS is configured on the bucket; without the same allowance
// here, an upload that works in production would fail from a Vite dev server and look like an
// emulator bug. PUT is in the method list for that reason and no other.
func corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") && !strings.HasPrefix(r.URL.Path, "/_blob/") {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "GET,PUT,POST,DELETE,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		h.Set("Access-Control-Expose-Headers", "ETag")
		h.Set("Access-Control-Max-Age", "86400")
		h.Add("Vary", "Origin")
		// Preflight never reaches a route.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" && r.Method != "OPTIONS" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
	})
}

// siteLogMW logs the site's requests WITH their status, and does not skip "/" the way the API's
// logger does — on a website the homepage is the request you most want to see, and a 404 that
// looks identical to a 200 in the log is the thing you are usually there to find.
func siteLogMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("site %d %s %s", rec.status, r.Method, r.URL.Path)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
