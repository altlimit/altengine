package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/devsite"
)

// Options wires a Deployer into an emulator.
type Options struct {
	Registry *control.Registry
	// Mux is the emulator's router. Every step is a request into it — an MCP tool call or a REST
	// route — so a manifest is validated by exactly what validates the same change made by hand.
	Mux     http.Handler
	Store   *Store
	Sites   *SiteHost
	DataDir string
	// APIBase is the emulator's own address, e.g. http://127.0.0.1:9191: a runtime config's
	// `api`, and the base of a functions or blob instance's `${key:url}`.
	APIBase string
}

// Deployer plans and applies manifests.
type Deployer struct {
	reg     *control.Registry
	mux     http.Handler
	store   *Store
	sites   *SiteHost
	dataDir string
	api     string
	// One deploy at a time, as hosted holds a lock per install: two deploys interleaving
	// function versions or config writes would leave a state neither asked for.
	mu sync.Mutex
}

// New builds a Deployer.
func New(o Options) *Deployer {
	return &Deployer{reg: o.Registry, mux: o.Mux, store: o.Store, sites: o.Sites, dataDir: o.DataDir, api: strings.TrimRight(o.APIBase, "/")}
}

// Store returns the install store.
func (d *Deployer) Store() *Store { return d.store }

// Request is one deploy: POST /__deploy's body.
type Request struct {
	// Manifest is a file path or an http(s) URL.
	Manifest string            `json:"manifest"`
	Name     string            `json:"name,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
	// Secrets: instance key → secret name → value. Never stored anywhere but the functions
	// instance.
	Secrets map[string]map[string]string `json:"secrets,omitempty"`
	// Bindings: `uses` key → existing instance name or id. The hosted page's picker.
	Bindings          map[string]string `json:"bindings,omitempty"`
	Adopt             bool              `json:"adopt,omitempty"`
	ConfirmEscalation bool              `json:"confirmEscalation,omitempty"`
	ConfirmSource     bool              `json:"confirmSource,omitempty"`
	DryRun            bool              `json:"dryRun,omitempty"`
}

// Response is the plan, and the result when the deploy was applied.
type Response struct {
	Plan   *Plan            `json:"plan,omitempty"`
	Result *Result          `json:"result,omitempty"`
	Error  *common.APIError `json:"error,omitempty"`
}

// Deploy plans a manifest and, unless it is a dry run, applies it. A non-nil error is also set
// as resp.Error; the plan is returned whenever one could be made.
func (d *Deployer) Deploy(ctx context.Context, req Request) (*Response, *common.APIError) {
	d.mu.Lock()
	defer d.mu.Unlock()
	resp := &Response{}
	fail := func(err error) (*Response, *common.APIError) {
		ae := asAPIError(err)
		resp.Error = ae
		return resp, ae
	}
	p, err := d.plan(ctx, req)
	if err != nil {
		return fail(err)
	}
	noteGivenSecrets(p.plan, req.Secrets)
	resp.Plan = p.plan
	if req.DryRun {
		return resp, nil
	}
	plan := p.plan
	if !plan.Ready {
		first := "this deploy cannot go ahead"
		if len(plan.Problems) > 0 {
			first = plan.Problems[0]
		}
		return fail(common.NewError(409, first, "PLAN_BLOCKED").WithDetails(map[string]any{"problems": plan.Problems}))
	}
	if len(plan.Escalations) > 0 && !req.ConfirmEscalation {
		return fail(common.NewError(409, "this update asks for more access than the installed version; confirm it (--confirm-escalation)", "CONFIRM_ESCALATION").
			WithDetails(map[string]any{"escalations": plan.Escalations}))
	}
	if plan.SourceChanged != nil && !req.ConfirmSource {
		return fail(common.NewError(409, fmt.Sprintf("'%s' was installed from %s; confirm deploying it from %s (--confirm-source)", plan.AppName, plan.SourceChanged.From, plan.SourceChanged.To), "CONFIRM_SOURCE"))
	}
	if len(plan.Adoptions) > 0 && !req.Adopt {
		var names []string
		for _, a := range plan.Adoptions {
			names = append(names, a.Name)
		}
		return fail(common.NewError(409, fmt.Sprintf("this deploy takes over existing instances (%s); confirm it (--adopt)", strings.Join(names, ", ")), "CONFIRM_ADOPT").
			WithDetails(map[string]any{"adoptions": plan.Adoptions}))
	}
	res, err := d.apply(ctx, p, req)
	if err != nil {
		return fail(err)
	}
	resp.Result = res
	return resp, nil
}

func asAPIError(err error) *common.APIError {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*common.APIError); ok {
		return ae
	}
	if mv, ok := err.(*MissingVarsError); ok {
		return mv.APIError()
	}
	return common.NewError(500, err.Error(), "INTERNAL")
}

// --- the endpoint ---------------------------------------------------------------------------

// ServeHTTP is POST /__deploy. Loopback only, and no API key, like the rest of the emulator's dev
// posture — but this one deploys code that runs in the emulator's process, so it also refuses a
// cross-site request: a foreign Origin, or a body that is not JSON (which a page can only send
// after a CORS preflight this route never answers).
func (d *Deployer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackRemote(r.RemoteAddr) {
		common.WriteError(w, common.NewError(http.StatusForbidden, "the deploy endpoint only answers requests from this machine", "PERMISSION_DENIED"))
		return
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !isLoopbackHost(u.Hostname()) {
			common.WriteError(w, common.NewError(http.StatusForbidden, "cross-origin requests are not allowed here", "PERMISSION_DENIED"))
			return
		}
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		common.WriteError(w, common.NewError(http.StatusUnsupportedMediaType, "send the request as application/json", "INVALID_ARGUMENT"))
		return
	}
	var req Request
	if err := common.ReadJSON(r, &req); err != nil {
		common.WriteError(w, err)
		return
	}
	resp, ae := d.Deploy(r.Context(), req)
	status := http.StatusOK
	if ae != nil {
		status = ae.Status
		log.Printf("deploy: %s: %s", ae.Code, ae.Message)
	} else if resp.Result != nil {
		log.Printf("deploy: %s applied from %s", resp.Plan.AppName, resp.Plan.Manifest.Source)
	}
	common.WriteJSON(w, status, resp)
}

func loopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// --- in-process calls -----------------------------------------------------------------------

// rest issues a request against the emulator's own routes, in process, as MCP's tools do.
func (d *Deployer) rest(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload)).WithContext(ctx)
	req = req.WithContext(common.WithCaller(req.Context(), "deploy"))
	req.Header.Set("Authorization", "Bearer emulator-internal")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		var env struct {
			Error *common.APIError `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &env) == nil && env.Error != nil && env.Error.Message != "" {
			env.Error.Status = rec.Code
			return env.Error
		}
		return common.NewError(rec.Code, fmt.Sprintf("%s %s failed with status %d", method, path, rec.Code), "INTERNAL")
	}
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			return fmt.Errorf("%s %s: unreadable response: %w", method, path, err)
		}
	}
	return nil
}

var toolErrCode = regexp.MustCompile(`^([A-Z][A-Z_]+): (.*)$`)

// tool calls one of the emulator's MCP tools, over its JSON-RPC endpoint, in process.
func (d *Deployer) tool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := d.rest(ctx, http.MethodPost, "/mcp", call, &rpc); err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, common.NewError(500, name+": "+rpc.Error.Message, "INTERNAL")
	}
	text := ""
	if len(rpc.Result.Content) > 0 {
		text = rpc.Result.Content[0].Text
	}
	if rpc.Result.IsError {
		code, msg, status := "INVALID_ARGUMENT", text, 400
		if m := toolErrCode.FindStringSubmatch(text); m != nil {
			code, msg = m[1], m[2]
			switch code {
			case "NOT_FOUND":
				status = 404
			case "ALREADY_EXISTS", "FAILED_PRECONDITION", "PRECONDITION_FAILED":
				status = 409
			}
		}
		return nil, common.NewError(status, msg, code)
	}
	out := map[string]any{}
	if text != "" {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, fmt.Errorf("%s: unreadable result", name)
		}
	}
	return out, nil
}

// --- addresses ------------------------------------------------------------------------------

// linkOf is a hosted instance's local address. Functions and blob are path-addressed under the
// API (/fn/<name>/, /blob/<name>/); a static site is its own port.
func (d *Deployer) linkOf(service, name string, port int) Link {
	var u string
	switch service {
	case "functions":
		u = d.api + "/fn/" + url.PathEscape(name) + "/"
	case "blob":
		u = d.api + "/blob/" + url.PathEscape(name) + "/"
	case "static":
		if port == 0 {
			return Link{}
		}
		u = d.sites.URL(port)
	default:
		return Link{}
	}
	pu, err := url.Parse(u)
	if err != nil {
		return Link{URL: u}
	}
	return Link{URL: u, Origin: pu.Scheme + "://" + pu.Host}
}

// --- sites on startup -------------------------------------------------------------------------

// ReopenSites serves every recorded static site again, each on the port it was given.
func (d *Deployer) ReopenSites() {
	for _, s := range d.store.Sites() {
		if err := d.sites.Open(s.ID, s.Name, s.Site.Port); err != nil {
			log.Printf("deploy: %v — the site of app %s is not served; free the port and restart", err, s.App)
			continue
		}
		if s.Site.Published {
			if err := d.publish(s.ID, s.Site, nil); err != nil {
				log.Printf("deploy: site %s: %v", s.Name, err)
				continue
			}
		}
		log.Printf("  deployed site:  %s — %s (app %s)", d.sites.URL(s.Site.Port), s.Name, s.App)
	}
}

// publish points a site's listener at what its record says to serve.
func (d *Deployer) publish(id string, site *SiteRecord, files map[string][]byte) error {
	cfg := parseStaticConfig(site.Config)
	overlay := map[string][]byte{}
	for p, b := range files {
		overlay[p] = b
	}
	if site.RuntimeConfigPath != "" && len(site.RuntimeConfig) > 0 {
		overlay[site.RuntimeConfigPath] = []byte(site.RuntimeConfig)
	}
	spa := cfg.SPA
	return d.sites.Publish(id, devsite.Options{Dir: site.Dir, SPA: &spa, CleanURLs: cfg.CleanURLs, NotFound: cfg.NotFound, Overlay: overlay}, files)
}

// staticConfig is the part of a static instance's config the local site honors.
type staticConfig struct {
	SPA       bool
	CleanURLs bool
	NotFound  string
}

func parseStaticConfig(c map[string]any) staticConfig {
	out := staticConfig{CleanURLs: true, NotFound: "/404.html"}
	if v, ok := c["spa"].(bool); ok {
		out.SPA = v
	}
	if v, ok := c["cleanUrls"].(bool); ok {
		out.CleanURLs = v
	}
	if v, ok := c["notFound"].(string); ok && strings.TrimSpace(v) != "" {
		out.NotFound = strings.TrimSpace(v)
		if !strings.HasPrefix(out.NotFound, "/") {
			out.NotFound = "/" + out.NotFound
		}
	}
	return out
}

// validateStaticConfig holds a static config to the hosted write rules.
func validateStaticConfig(c map[string]any) error {
	if v, ok := c["spa"]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return common.BadRequest("spa must be a boolean")
		}
	}
	if v, ok := c["cleanUrls"]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return common.BadRequest("cleanUrls must be a boolean")
		}
	}
	if v, ok := c["notFound"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || strings.ContainsAny(s, "\r\n") {
			return common.BadRequest("notFound must be a site path such as '/404.html'")
		}
	}
	if v, ok := c["keepDeployments"]; ok && v != nil {
		n, isNum := v.(float64)
		if !isNum || !isInteger(n) || n < 1 || n > 50 {
			return common.BadRequest("keepDeployments must be an integer in 1..50")
		}
	}
	return common.CheckRateLimit(c, "rateLimit")
}
