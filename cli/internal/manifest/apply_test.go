package manifest_test

// Deploy to AltEngine, end to end against a real in-process emulator: a manifest on disk (or served
// over loopback http) planned and applied through the emulator's own MCP tools and REST routes,
// then redeployed. The local counterpart of the hosted test/deployapply.test.ts.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/manifest"
	"github.com/altlimit/altengine/cli/internal/server"
)

type M = map[string]any

type emu struct {
	t    *testing.T
	srv  *server.Server
	http *http.Server
	base string
}

func startEmu(t *testing.T, dataDir string) *emu {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{Addr: ln.Addr().String(), DataDir: dataDir, DevOpen: true})
	if err != nil {
		_ = ln.Close()
		t.Fatalf("server.New: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go func() { _ = hs.Serve(ln) }()
	e := &emu{t: t, srv: srv, http: hs, base: "http://" + ln.Addr().String()}
	t.Cleanup(e.stop)
	return e
}

func (e *emu) stop() {
	if e.http != nil {
		_ = e.http.Close()
		e.srv.Close()
		e.http = nil
	}
}

func (e *emu) deploy(req manifest.Request) (*manifest.Response, error) {
	e.t.Helper()
	return e.srv.Deploy(context.Background(), req)
}

func (e *emu) mustDeploy(req manifest.Request) *manifest.Response {
	e.t.Helper()
	resp, err := e.deploy(req)
	if err != nil {
		if resp != nil && resp.Plan != nil {
			e.t.Logf("plan problems: %v", resp.Plan.Problems)
		}
		e.t.Fatalf("deploy: %v", err)
	}
	return resp
}

func (e *emu) mcp(tool string, args M) M {
	e.t.Helper()
	body, _ := json.Marshal(M{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": M{"name": tool, "arguments": args}})
	req, _ := http.NewRequest("POST", e.base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var rpc struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	_ = json.NewDecoder(res.Body).Decode(&rpc)
	if rpc.Result.IsError {
		e.t.Fatalf("%s: %s", tool, rpc.Result.Content[0].Text)
	}
	out := M{}
	_ = json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out)
	return out
}

func (e *emu) config(service, instance string) M {
	e.t.Helper()
	return e.mcp("get_instance_config", M{"service": service, "instance": instance})["config"].(M)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (e *emu) getJSON(path string) M {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.base+path, nil)
	req.Header.Set("Authorization", "Bearer test")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := M{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

// --- a manifest on disk ---------------------------------------------------------------------

type project struct {
	t   *testing.T
	dir string
}

func newProject(t *testing.T) *project { return &project{t: t, dir: t.TempDir()} }

// file writes a file under the project and returns its hash and size, as a manifest lists them.
func (p *project) file(rel, body string) M {
	p.t.Helper()
	full := filepath.Join(p.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return M{"hash": hex.EncodeToString(sum[:]), "size": len(body)}
}

func (p *project) sha(rel, body string) string { return p.file(rel, body)["hash"].(string) }

func (p *project) manifest(doc M) string {
	p.t.Helper()
	b, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(p.dir, "altengine.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		p.t.Fatal(err)
	}
	return path
}

const boardCode = `export default { async fetch(request, env) {
  return Response.json({ console: env.CONSOLE_URL, db: env.DB, auth: env.AUTH_NAME, board: env.BOARD || null });
} }`

// duty is DutyBoard's manifest shape: suffixes, the console's origin wired into auth and
// functions, sign-up opened only at creation, a queue that targets a function, a datastore
// trigger into it, and a site with a runtime config.
func duty(p *project, version string) string {
	sha := p.sha("dist/board.mjs", boardCode)
	page := p.file("dist/console/index.html", "<h1>console "+version+"</h1>")
	return p.manifest(M{
		"altengine": 1,
		"name":      "duty",
		"version":   version,
		"instances": M{
			"datastore": M{"service": "datastore",
				"indexes": []any{M{"collection": "duties", "fields": []any{"status", "dueAt:desc"}}},
				"config":  M{"triggers": []any{M{"match": M{"collection": "duties", "ops": []any{"create"}}, "queue": "${jobs}/notify"}}},
			},
			"auth": M{
				"service":       "auth",
				"suffix":        "auth",
				"config":        M{"settings": M{"allowedOrigins": []any{"${console:origin}"}}},
				"initialConfig": M{"settings": M{"allowSignup": true}},
			},
			"jobs": M{"service": "tasks", "queues": M{"notify": M{"mode": "pull", "target": nil}}},
			"functions": M{
				"service": "functions",
				"env":     M{"CONSOLE_URL": "${console:url}", "DB": "${datastore}", "AUTH_NAME": "${auth}"},
				"config":  M{"corsOrigins": []any{"${console:origin}"}},
				"functions": M{"board": M{
					"module": M{"url": "./dist/board.mjs", "sha256": sha}, "grants": M{"datastore:${datastore}": "full"}, "subRequests": 500,
				}},
			},
			"console": M{
				"service":       "static",
				"suffix":        "console",
				"site":          M{"base": "./dist/console/", "files": M{"/index.html": page}},
				"runtimeConfig": M{"path": "/altengine.json", "expose": []any{"datastore", "auth", "functions"}},
			},
		},
	})
}

func instanceURL(resp *manifest.Response, key string) string {
	for _, i := range resp.Result.Instances {
		if i.Key == key && i.URL != nil {
			return *i.URL
		}
	}
	return ""
}

func TestNewInstallNamesWiresAndServes(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	path := duty(p, "1.0.0")

	resp := e.mustDeploy(manifest.Request{Manifest: path})
	var got [][]string
	for _, i := range resp.Plan.Instances {
		got = append(got, []string{i.Key, i.Name, i.Action})
	}
	// (Keys are in the order written; a Go map marshals them sorted.)
	want := [][]string{{"auth", "duty-auth", "create"}, {"console", "duty-console", "create"}, {"datastore", "duty", "create"}, {"functions", "duty", "create"}, {"jobs", "duty-jobs", "create"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v", got)
	}

	site := instanceURL(resp, "console")
	if !strings.HasPrefix(site, "http://127.0.0.1:") || !strings.HasSuffix(site, "/") {
		t.Fatalf("console url = %q", site)
	}
	origin := strings.TrimSuffix(site, "/")

	settings := e.config("auth", "duty-auth")["settings"].(M)
	if settings["allowSignup"] != true || !reflect.DeepEqual(settings["allowedOrigins"], []any{origin}) {
		t.Fatalf("auth settings = %v", settings)
	}
	if cors := e.getJSON("/v1/functions/duty/settings")["corsOrigins"]; !reflect.DeepEqual(cors, []any{origin}) {
		t.Fatalf("corsOrigins = %v", cors)
	}
	trig := e.config("datastore", "duty")["triggers"].([]any)[0].(M)
	if trig["queue"] != "duty-jobs/notify" {
		t.Fatalf("trigger = %v", trig)
	}
	idx := e.mcp("datastore_list_indexes", M{"instance": "duty", "collection": "duties"})
	if b, _ := json.Marshal(idx); !strings.Contains(string(b), "dueAt") {
		t.Fatalf("indexes = %s", b)
	}
	fns := e.getJSON("/v1/functions/duty")["functions"].([]any)
	if fn := fns[0].(M); fn["sub_requests"] != 500.0 || !reflect.DeepEqual(fn["grants"], M{"datastore:duty": "full"}) {
		t.Fatalf("function = %v", fn)
	}

	// The code sees its instance names and the site's address in env.
	_, body := get(t, e.base+"/fn/duty/board")
	var env M
	_ = json.Unmarshal([]byte(body), &env)
	if env["console"] != site || env["db"] != "duty" || env["auth"] != "duty-auth" {
		t.Fatalf("env = %s", body)
	}

	// The site serves its files and the runtime config, on its own port.
	if code, page := get(t, site); code != 200 || page != "<h1>console 1.0.0</h1>" {
		t.Fatalf("site / = %d %q", code, page)
	}
	_, rcBody := get(t, site+"altengine.json")
	var rc struct {
		App       string                    `json:"app"`
		Version   string                    `json:"version"`
		API       string                    `json:"api"`
		Instances map[string]map[string]any `json:"instances"`
	}
	if err := json.Unmarshal([]byte(rcBody), &rc); err != nil {
		t.Fatalf("runtime config %q: %v", rcBody, err)
	}
	authID := e.mcp("get_instance_config", M{"service": "auth", "instance": "duty-auth"})["id"]
	if rc.App != "duty" || rc.Version != "1.0.0" || rc.API != e.base || rc.Instances["auth"]["id"] != authID ||
		rc.Instances["auth"]["name"] != "duty-auth" || rc.Instances["functions"]["url"] != e.base+"/fn/duty/" || rc.Instances["datastore"]["url"] != nil {
		t.Fatalf("runtime config = %s", rcBody)
	}
	// It was not written into the build directory.
	if _, err := os.Stat(filepath.Join(p.dir, "dist", "console", "altengine.json")); err == nil {
		t.Fatal("the runtime config was written into the site's directory")
	}
}

func TestRedeployMergesConfigKeepsOwnerChangesAndSkipsUnchangedCode(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	first := e.mustDeploy(manifest.Request{Manifest: duty(p, "1.0.0")})
	origin := strings.TrimSuffix(instanceURL(first, "console"), "/")
	authID := e.mcp("get_instance_config", M{"service": "auth", "instance": "duty-auth"})["id"]

	// The owner closes sign-up, adds a dev origin and changes an unrelated setting by hand.
	cur := e.config("auth", "duty-auth")
	s := cur["settings"].(M)
	s["allowSignup"], s["allowedOrigins"], s["accessTokenTtl"] = false, append(s["allowedOrigins"].([]any), "http://localhost:5173"), 900
	e.mcp("patch_instance_config", M{"service": "auth", "instance": "duty-auth", "changes": M{"settings": s}})

	second := e.mustDeploy(manifest.Request{Manifest: duty(p, "1.1.0")})
	if second.Plan.Mode != "update" {
		t.Fatalf("mode = %s", second.Plan.Mode)
	}
	for _, i := range second.Plan.Instances {
		if i.Action != "update" {
			t.Fatalf("%s: action %s", i.Key, i.Action)
		}
	}
	after := e.config("auth", "duty-auth")["settings"].(M)
	if after["allowSignup"] != false || after["accessTokenTtl"] != 900.0 || !reflect.DeepEqual(after["allowedOrigins"], []any{origin, "http://localhost:5173"}) {
		t.Fatalf("settings after an upgrade = %v", after)
	}
	if id := e.mcp("get_instance_config", M{"service": "auth", "instance": "duty-auth"})["id"]; id != authID {
		t.Fatal("the redeploy replaced the auth instance")
	}
	// Unchanged code is left alone: no new version.
	if f := second.Result.Functions; len(f) != 1 || !f[0].Skipped || f[0].Version != 1 {
		t.Fatalf("functions = %+v", f)
	}
	// The site's port, and so its origin, did not move; the new page is live.
	if instanceURL(second, "console") != origin+"/" {
		t.Fatalf("site moved: %s → %s", origin, instanceURL(second, "console"))
	}
	if _, page := get(t, origin+"/"); page != "<h1>console 1.1.0</h1>" {
		t.Fatalf("page = %q", page)
	}
}

func TestChangedCodeIsANewVersion(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	e.mustDeploy(manifest.Request{Manifest: duty(p, "1.0.0")})
	sha := p.sha("dist/board.mjs", `export default { fetch() { return new Response("v2") } }`)
	doc := M{}
	b, _ := os.ReadFile(filepath.Join(p.dir, "altengine.json"))
	_ = json.Unmarshal(b, &doc)
	inst := doc["instances"].(M)["functions"].(M)["functions"].(M)["board"].(M)
	inst["module"].(M)["sha256"] = sha
	resp := e.mustDeploy(manifest.Request{Manifest: p.manifest(doc)})
	if f := resp.Result.Functions[0]; f.Skipped || f.Version != 2 {
		t.Fatalf("function = %+v", f)
	}
	if _, body := get(t, e.base+"/fn/duty/board"); body != "v2" {
		t.Fatalf("serving %q", body)
	}
}

func TestAdoptionNeedsConfirmationThenIsAnUpdate(t *testing.T) {
	e := startEmu(t, "")
	made := e.mcp("create_instance", M{"service": "auth", "name": "duty-auth"})
	// Sign-up closed by hand before the app existed: an adoption must not reopen it.
	cfg := e.config("auth", "duty-auth")
	cfg["settings"] = M{"allowSignup": false}
	e.mcp("patch_instance_config", M{"service": "auth", "instance": "duty-auth", "changes": cfg})

	p := newProject(t)
	path := duty(p, "1.0.0")
	resp, err := e.deploy(manifest.Request{Manifest: path})
	if err == nil || !strings.Contains(err.Error(), "CONFIRM_ADOPT") {
		t.Fatalf("err = %v", err)
	}
	if len(resp.Plan.Adoptions) != 1 || resp.Plan.Adoptions[0].ID != made["id"] {
		t.Fatalf("adoptions = %+v", resp.Plan.Adoptions)
	}
	if e.mcp("list_instances", M{"service": "datastore"})["datastore"].([]any) != nil && len(e.mcp("list_instances", M{"service": "datastore"})["datastore"].([]any)) != 0 {
		t.Fatal("a refused deploy created instances")
	}

	e.mustDeploy(manifest.Request{Manifest: path, Adopt: true})
	if id := e.mcp("get_instance_config", M{"service": "auth", "instance": "duty-auth"})["id"]; id != made["id"] {
		t.Fatal("the adopted instance was replaced")
	}
	if s := e.config("auth", "duty-auth")["settings"].(M); s["allowSignup"] != false || len(s["allowedOrigins"].([]any)) != 1 {
		t.Fatalf("adopted settings = %v (initialConfig must not apply to an adoption; config must)", s)
	}
	again := e.mustDeploy(manifest.Request{Manifest: path, DryRun: true})
	for _, i := range again.Plan.Instances {
		if i.Action != "update" {
			t.Fatalf("%s: %s after adoption", i.Key, i.Action)
		}
	}
	if len(again.Plan.Adoptions) != 0 {
		t.Fatalf("adoptions = %+v", again.Plan.Adoptions)
	}
}

func TestAnotherInstallsInstanceIsAConflict(t *testing.T) {
	e := startEmu(t, "")
	a, b := newProject(t), newProject(t)
	e.mustDeploy(manifest.Request{Manifest: a.manifest(M{"altengine": 1, "name": "shop", "instances": M{"orders-db": M{"service": "datastore"}}})})
	resp, err := e.deploy(manifest.Request{Manifest: b.manifest(M{"altengine": 1, "name": "shop-orders", "instances": M{"db": M{"service": "datastore"}}}), Adopt: true})
	if err == nil || !strings.Contains(err.Error(), "PLAN_BLOCKED") {
		t.Fatalf("err = %v", err)
	}
	if i := resp.Plan.Instances[0]; i.Name != "shop-orders-db" || i.Action != "conflict" {
		t.Fatalf("instance = %+v", i)
	}
	if len(resp.Plan.Adoptions) != 0 {
		t.Fatal("another app's instance offered for adoption")
	}
}

func TestHashMismatchIsRefused(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	path := duty(p, "1.0.0")
	p.file("dist/board.mjs", "export default {} // changed after publishing")
	_, err := e.deploy(manifest.Request{Manifest: path})
	if err == nil || !strings.Contains(err.Error(), "HASH_MISMATCH") {
		t.Fatalf("err = %v", err)
	}
	// A site file that does not match is refused too.
	path = duty(p, "1.0.0")
	p.file("dist/console/index.html", "tampered")
	if _, err := e.deploy(manifest.Request{Manifest: path}); err == nil || !strings.Contains(err.Error(), "HASH_MISMATCH") {
		t.Fatalf("site err = %v", err)
	}
}

func TestSecretsAreAskedForThenKept(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	sha := p.sha("web.mjs", `export default { fetch(r, env) { return new Response(env.MAIL_KEY + "|" + env.OTHER) } }`)
	path := p.manifest(M{"altengine": 1, "name": "mail", "instances": M{"functions": M{
		"service":   "functions",
		"secrets":   M{"MAIL_KEY": M{"description": "Mail API key", "required": true}},
		"functions": M{"web": M{"module": M{"url": "./web.mjs", "sha256": sha}}},
	}}})
	resp, err := e.deploy(manifest.Request{Manifest: path})
	if err == nil || !strings.Contains(err.Error(), "SECRETS_REQUIRED") || !strings.Contains(err.Error(), "MAIL_KEY") {
		t.Fatalf("err = %v", err)
	}
	if s := resp.Plan.Secrets; len(s) != 1 || s[0].Stored {
		t.Fatalf("secrets = %+v", s)
	}
	e.mustDeploy(manifest.Request{Manifest: path, Secrets: map[string]map[string]string{"functions": {"MAIL_KEY": "k-1"}}})
	// A secret set by hand is not deleted by a redeploy that merges.
	if err := put(e, "/v1/functions/mail/secrets", M{"secrets": M{"MAIL_KEY": M{}, "OTHER": "o"}}); err != nil {
		t.Fatal(err)
	}
	again := e.mustDeploy(manifest.Request{Manifest: path})
	if !again.Plan.Secrets[0].Stored {
		t.Fatal("stored secret not seen")
	}
	if _, body := get(t, e.base+"/fn/mail/web"); body != "k-1|o" {
		t.Fatalf("env = %q", body)
	}
}

func put(e *emu, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", e.base+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func TestVarsNameTheAppAndARedeployReusesThem(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	sha := p.sha("web.mjs", boardCode)
	path := p.manifest(M{
		"altengine": 1, "name": "board-{{board}}",
		"vars": M{"board": M{"pattern": "[a-z0-9-]{1,20}"}},
		"instances": M{"datastore": M{"service": "datastore"}, "functions": M{"service": "functions",
			"env":       M{"DB": "${datastore}", "BOARD": "{{board}}"},
			"functions": M{"board": M{"module": M{"url": "./web.mjs", "sha256": sha}, "grants": M{"datastore:${datastore}": "write"}}}}},
	})
	resp, err := e.deploy(manifest.Request{Manifest: path, DryRun: true})
	if err != nil || resp.Plan.Ready || !strings.Contains(strings.Join(resp.Plan.Problems, " "), "Enter a value for board") {
		t.Fatalf("plan without the var: %+v, %v", resp.Plan, err)
	}
	for _, b := range []string{"acme", "zeta"} {
		e.mustDeploy(manifest.Request{Manifest: path, Vars: map[string]string{"board": b}})
	}
	for _, b := range []string{"acme", "zeta"} {
		_, body := get(t, e.base+"/fn/board-"+b+"/board")
		if !strings.Contains(body, `"board":"`+b+`"`) || !strings.Contains(body, `"db":"board-`+b+`"`) {
			t.Fatalf("board-%s env = %s", b, body)
		}
	}
	// No --var: the latest install from this manifest supplies it.
	again := e.mustDeploy(manifest.Request{Manifest: path, DryRun: true})
	if again.Plan.Mode != "update" || again.Plan.AppName != "board-zeta" {
		t.Fatalf("redeploy plan = %s %s", again.Plan.Mode, again.Plan.AppName)
	}
	named := e.mustDeploy(manifest.Request{Manifest: path, Name: "board-acme", DryRun: true})
	if named.Plan.Mode != "update" || *named.Plan.Vars[0].Value != "acme" {
		t.Fatalf("named redeploy plan = %s %v", named.Plan.Mode, named.Plan.Vars)
	}
}

func TestSiteKeepsItsPortAcrossARestart(t *testing.T) {
	data := t.TempDir()
	p := newProject(t)
	path := duty(p, "1.0.0")

	e := startEmu(t, data)
	site := instanceURL(e.mustDeploy(manifest.Request{Manifest: path}), "console")
	e.stop()

	e2 := startEmu(t, data)
	if code, page := get(t, site); code != 200 || page != "<h1>console 1.0.0</h1>" {
		t.Fatalf("after restart %s = %d %q", site, code, page)
	}
	if code, rc := get(t, site+"altengine.json"); code != 200 || !strings.Contains(rc, `"app": "duty"`) {
		t.Fatalf("runtime config after restart = %d %q", code, rc)
	}
	// A redeploy keeps it there too.
	resp := e2.mustDeploy(manifest.Request{Manifest: duty(p, "1.0.1")})
	if instanceURL(resp, "console") != site {
		t.Fatalf("site moved to %s", instanceURL(resp, "console"))
	}
	b, err := os.ReadFile(filepath.Join(data, "deploys.json"))
	if err != nil || !strings.Contains(string(b), `"applied_sha256"`) {
		t.Fatalf("deploys.json: %v", err)
	}
}

func TestAManifestOverLoopbackHTTP(t *testing.T) {
	data := t.TempDir()
	e := startEmu(t, data)
	p := newProject(t)
	page := p.file("dist/index.html", "<h1>from a url</h1>")
	asset := p.file("dist/assets/app-4f2a91bc.js", "console.log(1)")
	p.manifest(M{"altengine": 1, "name": "site-{{env}}", "vars": M{"env": M{}}, "instances": M{"static": M{
		"service": "static", "config": M{"spa": true},
		"site":          M{"base": "./dist/", "files": M{"/index.html": page, "/assets/app-4f2a91bc.js": asset}},
		"runtimeConfig": M{"expose": []any{"static"}},
	}}})
	files := httptest.NewServer(http.FileServer(http.Dir(p.dir)))
	defer files.Close()

	resp := e.mustDeploy(manifest.Request{Manifest: files.URL + "/altengine.json?env=dev"})
	if resp.Plan.AppName != "site-dev" {
		t.Fatalf("app = %s", resp.Plan.AppName)
	}
	site := instanceURL(resp, "static")
	if _, body := get(t, site+"assets/app-4f2a91bc.js"); body != "console.log(1)" {
		t.Fatalf("asset = %q", body)
	}
	// spa from config: a deep link gets the shell.
	if code, body := get(t, site+"some/route"); code != 200 || body != "<h1>from a url</h1>" {
		t.Fatalf("deep link = %d %q", code, body)
	}
	matches, _ := filepath.Glob(filepath.Join(data, "sites", "*", "index.html"))
	if len(matches) != 1 {
		t.Fatalf("downloaded copies = %v", matches)
	}
	// Plain http to anywhere but this machine is refused.
	if _, err := e.deploy(manifest.Request{Manifest: "http://example.com/altengine.json"}); err == nil || !strings.Contains(err.Error(), "URL_REFUSED") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployEndpointIsLoopbackOnly(t *testing.T) {
	e := startEmu(t, "")
	p := newProject(t)
	path := p.manifest(M{"altengine": 1, "name": "tiny", "instances": M{"datastore": M{"service": "datastore"}}})
	body, _ := json.Marshal(manifest.Request{Manifest: path, DryRun: true})

	call := func(remote, origin, ctype string) int {
		req := httptest.NewRequest("POST", "/__deploy", bytes.NewReader(body))
		req.Host = "127.0.0.1:9191"
		req.RemoteAddr = remote
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := call("203.0.113.9:5555", "", "application/json"); c != http.StatusForbidden {
		t.Fatalf("non-loopback caller: %d", c)
	}
	if c := call("127.0.0.1:5555", "https://evil.example", "application/json"); c != http.StatusForbidden {
		t.Fatalf("cross-site origin: %d", c)
	}
	if c := call("127.0.0.1:5555", "", "text/plain"); c != http.StatusUnsupportedMediaType {
		t.Fatalf("form-shaped body: %d", c)
	}
	if c := call("127.0.0.1:5555", "", "application/json"); c != http.StatusOK {
		t.Fatalf("loopback dry run: %d", c)
	}

	// Over the real socket: a dry run plans and changes nothing.
	res, err := http.Post(e.base+"/__deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var resp manifest.Response
	_ = json.NewDecoder(res.Body).Decode(&resp)
	res.Body.Close()
	if res.StatusCode != 200 || resp.Plan == nil || resp.Plan.Instances[0].Action != "create" || resp.Result != nil {
		t.Fatalf("dry run = %d %+v", res.StatusCode, resp)
	}
	if list := e.mcp("list_instances", M{"service": "datastore"})["datastore"].([]any); len(list) != 0 {
		t.Fatalf("a dry run created %v", list)
	}
}

func TestUsesBindsAnExistingInstance(t *testing.T) {
	e := startEmu(t, "")
	e.mcp("create_instance", M{"service": "datastore", "name": "orders"})
	p := newProject(t)
	sha := p.sha("web.mjs", "export default { fetch() { return new Response('ok') } }")
	doc := func(level string) M {
		return M{"altengine": 1, "name": "reports",
			"uses": M{"orders": M{"service": "datastore", "level": level, "suggest": "orders"}},
			"instances": M{"functions": M{"service": "functions", "functions": M{"web": M{
				"module": M{"url": "./web.mjs", "sha256": sha}, "grants": M{"datastore:${orders}": level}}}}}}
	}
	resp := e.mustDeploy(manifest.Request{Manifest: p.manifest(doc("read"))})
	if b := resp.Plan.Bindings[0]; b.Instance == nil || b.Instance.Name != "orders" {
		t.Fatalf("binding = %+v", b)
	}
	fn := e.getJSON("/v1/functions/reports")["functions"].([]any)[0].(M)
	if !reflect.DeepEqual(fn["grants"], M{"datastore:orders": "read"}) {
		t.Fatalf("grants = %v", fn["grants"])
	}
	// More access on an update must be confirmed.
	_, err := e.deploy(manifest.Request{Manifest: p.manifest(doc("write"))})
	if err == nil || !strings.Contains(err.Error(), "CONFIRM_ESCALATION") {
		t.Fatalf("err = %v", err)
	}
	e.mustDeploy(manifest.Request{Manifest: p.manifest(doc("write")), ConfirmEscalation: true})
}
