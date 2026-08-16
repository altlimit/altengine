// The functions HTTP surface: the deploy API (identical to hosted) and invocation.
//
// INVOCATION ADDRESSING DIFFERS, and it is the one place it has to.
//
// Hosted, a function is served from its instance's own subdomain —
// `{slug}.altengine.app/{function}/...` — because that is what gives every tenant an
// isolated origin (cookies, CORS, storage). Locally there is no wildcard DNS and no
// certificate, so the emulator serves the same handler under a PATH prefix:
//
//     http://127.0.0.1:9191/fn/{instance}/{function}/...
//
// A function therefore sees a different origin and a different pathname prefix locally.
// Read the path from the request as your router does hosted and it makes no difference;
// hard-code `/{function}/...` offsets and it will.

package functions

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Handler serves both surfaces.
type Handler struct {
	reg    *control.Registry
	auth   *auth.Store
	store  *Store
	cache  *compileCache
	binder *bindings
	sched  *schedState
}

// NewHandler builds the functions handler. `mux` is the emulator's own router: stub calls
// are dispatched into it in-process, so `env.datastore` behaves exactly as the REST API
// does rather than as a second implementation of it.
func NewHandler(reg *control.Registry, a *auth.Store, store *Store, mux http.Handler) *Handler {
	return &Handler{
		reg:    reg,
		auth:   a,
		store:  store,
		cache:  newCompileCache(),
		binder: &bindings{mux: mux, token: "emulator-internal"},
		sched:  newSchedState(),
	}
}

// Register mounts both the deploy API and the invocation prefix.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/functions/{instance}"
	mux.HandleFunc("GET "+p, common.Wrap(h.list))
	mux.HandleFunc("POST "+p+"/deploy", common.Wrap(h.deploy))
	mux.HandleFunc("GET "+p+"/{fn}/versions", common.Wrap(h.versions))
	mux.HandleFunc("POST "+p+"/{fn}/activate", common.Wrap(h.activate))
	mux.HandleFunc("GET "+p+"/{fn}/versions/{version}/code", common.Wrap(h.code))
	mux.HandleFunc("PUT "+p+"/secrets", common.Wrap(h.setSecrets))
	mux.HandleFunc("GET "+p+"/secrets", common.Wrap(h.listSecrets))
	mux.HandleFunc("PUT "+p+"/settings", common.Wrap(h.setSettings))

	// Invocation. The trailing slash makes this a subtree so everything under the
	// function name reaches the handler, which is what a function's own router needs.
	mux.HandleFunc("/fn/{instance}/{fn}/", common.Wrap(h.invoke))
	mux.HandleFunc("/fn/{instance}/{fn}", common.Wrap(h.invoke))
}

// resolve authenticates and finds the addressed instance.
func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, error) {
	id, err := h.auth.Resolve(r)
	if err != nil {
		return nil, err
	}
	name := r.PathValue("instance")
	if err := auth.Require(id, "functions", name, need); err != nil {
		return nil, err
	}
	// GetOrCreate matches how the other emulator services treat a first reference: naming
	// an instance brings it into being, so local development needs no provisioning step.
	return h.reg.GetOrCreate("functions", name), nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	cfg := h.store.Config(in.ID)
	out := []map[string]any{}
	for _, f := range cfg.Functions {
		out = append(out, map[string]any{
			"name":           f.Name,
			"active_version": f.ActiveVersion,
			"grants":         f.Grants,
			"cpu_ms":         f.CPUMs,
			"sub_requests":   f.SubRequests,
			"url":            fmt.Sprintf("/fn/%s/%s", in.Name, f.Name),
		})
	}
	common.WriteJSON(w, 200, map[string]any{"instance": in.Name, "host": nil, "functions": out})
	return nil
}

func (h *Handler) deploy(w http.ResponseWriter, r *http.Request) error {
	// `full`, not `write`: a deploy replaces the code that runs with the instance's
	// granted capabilities, so it is strictly more powerful than writing data through
	// them. Same rule as hosted.
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var req DeployRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	res, err := h.store.Deploy(in.ID, req)
	if err != nil {
		return err
	}
	// A redeploy under a reused version number must not keep serving the old program.
	h.cache.drop(in.ID + ":" + req.Name + ":")
	log.Printf("functions: deployed %s/%s v%v", in.Name, req.Name, res["version"])
	common.WriteJSON(w, 201, res)
	return nil
}

func (h *Handler) versions(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	name := r.PathValue("fn")
	cfg := h.store.Config(in.ID)
	active := 0
	if f := cfg.Find(name); f != nil {
		active = f.ActiveVersion
	}
	common.WriteJSON(w, 200, map[string]any{"versions": h.store.Versions(in.ID, name), "active_version": active})
	return nil
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		Version int `json:"version"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	name := r.PathValue("fn")
	if err := h.store.Activate(in.ID, name, body.Version); err != nil {
		return err
	}
	h.cache.drop(in.ID + ":" + name + ":")
	common.WriteJSON(w, 200, map[string]any{"name": name, "active_version": body.Version})
	return nil
}

func (h *Handler) code(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		return common.BadRequest("version must be a number")
	}
	src, ok := h.store.Code(in.ID, r.PathValue("fn"), v)
	if !ok {
		return common.NotFound("no code for that version")
	}
	common.WriteJSON(w, 200, map[string]any{"code": src})
	return nil
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	// Names and exposure, never values — the same contract as hosted, where a value
	// cannot be read back once written.
	common.WriteJSON(w, 200, map[string]any{"secrets": h.store.Config(in.ID).SecretNames()})
	return nil
}

func (h *Handler) setSecrets(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		Secrets map[string]json.RawMessage `json:"secrets"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if _, err := h.store.SetSecrets(in.ID, body.Secrets); err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"secrets": h.store.Config(in.ID).SecretNames()})
	return nil
}

func (h *Handler) setSettings(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		AllowedHosts []string `json:"allowedHosts"`
		CORSOrigins  []string `json:"corsOrigins"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if err := h.store.SetSettings(in.ID, body.AllowedHosts, body.CORSOrigins); err != nil {
		return err
	}
	cfg := h.store.Config(in.ID)
	common.WriteJSON(w, 200, map[string]any{"allowedHosts": cfg.AllowedHosts, "corsOrigins": cfg.CORSOrigins})
	return nil
}

// invoke runs a deployed function. NO AUTH: a function URL is public hosted, which is the
// entire point of the service, so requiring a key here would hide the fact that anything
// a function does not check itself is reachable by anyone.
func (h *Handler) invoke(w http.ResponseWriter, r *http.Request) error {
	instName := r.PathValue("instance")
	fnName := r.PathValue("fn")
	in := h.reg.Get("functions", instName)
	if in == nil {
		return common.NotFound(fmt.Sprintf("no functions instance '%s'", instName))
	}
	cfg := h.store.Config(in.ID)
	fn := cfg.Find(fnName)
	if fn == nil {
		return common.NotFound(fmt.Sprintf("function '%s' not found", fnName))
	}
	src, ok := h.store.Code(in.ID, fnName, fn.ActiveVersion)
	if !ok {
		return common.NewError(500, fmt.Sprintf("function '%s' has no deployed code for version %d", fnName, fn.ActiveVersion), "INTERNAL")
	}

	origin := r.Header.Get("Origin")
	// Preflight is answered without running the function: an OPTIONS should not cost an
	// invocation, and user code has nothing useful to say about it.
	if r.Method == http.MethodOptions {
		if allow := allowOrigin(cfg.CORSOrigins, origin); allow != "" {
			writeCORS(w, allow, r.Header.Get("Access-Control-Request-Headers"))
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	body, err := readBody(r)
	if err != nil {
		return common.BadRequest("could not read request body")
	}
	// Strip every inbound x-ae-* header so a caller cannot forge our trusted context,
	// then set the ones the runtime guarantees. Same as hosted.
	scrubbed := r.Clone(r.Context())
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-ae-") {
			scrubbed.Header.Del(name)
		}
	}
	requestID := common.UUID()
	scrubbed.Header.Set("x-ae-trigger", "http")
	scrubbed.Header.Set("x-ae-request-id", requestID)
	scrubbed.Header.Set("x-ae-fn", fnName)
	scrubbed.Header.Set("x-ae-version", strconv.Itoa(fn.ActiveVersion))

	status, header, out, err := h.run(&invocation{
		instanceName: in.Name,
		fn:           fn,
		cfg:          cfg,
		source:       src,
		cacheKey:     fmt.Sprintf("%s:%s:%d", in.ID, fnName, fn.ActiveVersion),
		req:          scrubbed,
		body:         body,
		bindings:     h.binder,
		logf: func(level, msg string) {
			// The local equivalent of the hosted live tail. Hosted this is streamed to
			// the console only while someone is watching, and never stored.
			log.Printf("[%s/%s] %s: %s", in.Name, fnName, level, msg)
		},
	})
	if err != nil {
		log.Printf("[%s/%s] failed: %v", in.Name, fnName, err)
		if allow := allowOrigin(cfg.CORSOrigins, origin); allow != "" {
			writeCORS(w, allow, "")
		}
		common.WriteJSON(w, 500, map[string]any{"error": map[string]any{
			"code": "FUNCTION_ERROR", "message": fmt.Sprintf("function '%s' failed: %v", fnName, err), "request_id": requestID,
		}})
		return nil
	}

	for k, vals := range header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if allow := allowOrigin(cfg.CORSOrigins, origin); allow != "" {
		writeCORS(w, allow, "")
	}
	w.WriteHeader(status)
	_, _ = w.Write(out)
	return nil
}

// allowOrigin mirrors the hosted rule: nothing when unconfigured, "*" when open, and an
// exact match otherwise.
func allowOrigin(origins []string, origin string) string {
	if len(origins) == 0 {
		return ""
	}
	for _, o := range origins {
		if o == "*" {
			return "*"
		}
	}
	for _, o := range origins {
		if origin != "" && strings.EqualFold(o, origin) {
			return origin
		}
	}
	return ""
}

// writeCORS never sets Allow-Credentials. With "*" a browser refuses the pair anyway, and
// with a reflected origin it would turn any allowlisted site into a session-riding vector.
func writeCORS(w http.ResponseWriter, allow, reqHeaders string) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allow)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
	if reqHeaders != "" {
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
	}
	h.Set("Access-Control-Max-Age", "86400")
}
