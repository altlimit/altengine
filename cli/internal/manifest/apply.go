package manifest

// Carrying out a plan. A port of hosted apply.ts, in its order:
//
//   create instances  →  secrets/env  →  queues  →  indexes  →  config (all but functions/static)
//   →  stage function versions  →  site files  →  activate functions  →  functions config
//   →  publish site  →  static config  →  finish
//
// Queues come before datastore triggers (a trigger names a queue that must exist). Function code is
// stored with activate:false until every changed function of the instance is stored, then each is
// activated — per function, as the emulator's API has no single switch for all of them. The site
// goes live last; a deploy that stops before then leaves the previous site serving.
//
// Hosted, an apply runs in bounded batches and resumes from saved progress. Locally it is one call;
// a failed run is simply run again, every step looks at real state before it writes, and the keys a
// failed run created are remembered so the retry still gives them their initialConfig.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Result is what an applied deploy produced.
type Result struct {
	AppName   string           `json:"app_name"`
	InstallID string           `json:"install_id"`
	Instances []ResultInstance `json:"instances"`
	// Functions lists each function of the manifest and what happened to it.
	Functions []ResultFunction `json:"functions"`
}

// ResultInstance is one instance the app manages.
type ResultInstance struct {
	Key     string  `json:"key"`
	Service string  `json:"service"`
	Name    string  `json:"name"`
	ID      string  `json:"id"`
	URL     *string `json:"url"`
}

// ResultFunction is one function: deployed as a new version, or left alone.
type ResultFunction struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Version  int    `json:"version,omitempty"`
	Skipped  bool   `json:"unchanged,omitempty"`
}

type stagedFn struct {
	name    string
	version int
}

type preparedSite struct {
	dir        string
	downloaded bool
	files      map[string][]byte // in memory, when there is no data directory
	rcPath     string
	rc         []byte
}

type run struct {
	d        *Deployer
	ctx      context.Context
	p        *planned
	in       *Install
	owned    map[string]string // key → instance id
	created  map[string]bool
	secrets  map[string]map[string]string
	staged   map[string][]stagedFn
	sites    map[string]*preparedSite
	fnResult []ResultFunction
	links    map[string]Link
	origins  map[string][]string
}

func (r *run) save() { r.d.store.Put(r.in) }

// linksFor are the addresses of every hosted instance, for ${key:url} and ${key:origin}.
func (r *run) linksFor() (map[string]Link, map[string][]string) {
	if r.links != nil {
		return r.links, r.origins
	}
	links, origins := map[string]Link{}, map[string][]string{}
	add := func(key, service, name string, port int) {
		l := r.d.linkOf(service, name, port)
		links[key] = l
		if l.Origin != "" {
			origins[key] = []string{l.Origin}
		}
	}
	for _, decl := range r.p.m.Instances {
		if !IsHosted(decl.Service) || r.owned[decl.Key] == "" {
			continue
		}
		port := 0
		if row := r.in.Row(decl.Key); row != nil && row.Site != nil {
			port = row.Site.Port
		}
		add(decl.Key, decl.Service, r.p.names[decl.Key], port)
	}
	for _, u := range r.p.m.Uses {
		if !IsHosted(u.Service) {
			continue
		}
		if found := r.d.lookup(u.Service, r.p.bindings[u.Key]); found != nil {
			add(u.Key, u.Service, found.Name, found.Port)
		}
	}
	r.links, r.origins = links, origins
	return links, origins
}

func (r *run) resolve(v any) (any, error) {
	links, _ := r.linksFor()
	return ResolveRefs(v, r.p.names, links)
}

// apply carries out a ready plan.
func (d *Deployer) apply(ctx context.Context, p *planned, req Request) (*Result, error) {
	in := p.install
	if in == nil {
		in = &Install{ID: common.UUID(), Name: p.app, CreatedAt: nowMS()}
	}
	in.ManifestURL, in.ManifestHost = p.src.Ref, p.src.Host
	in.Vars = p.m.VarValues
	in.Status, in.LastError = "applying", ""

	// Updated and adopted instances are recorded as owned, by id, before any step runs.
	planIDs := map[string]string{}
	for _, it := range p.plan.Instances {
		if (it.Action == "update" || it.Action == "adopt") && it.ID != nil {
			planIDs[it.Key] = *it.ID
		}
	}
	managed := map[string]bool{}
	for _, decl := range p.m.Instances {
		managed[decl.Key] = true
		row := in.Row(decl.Key)
		if row == nil {
			row = &InstallInstance{Key: decl.Key}
			in.Instances = append(in.Instances, row)
		}
		if row.Relation != "owned" || row.Service != decl.Service {
			row.InstanceID, row.Site = "", nil
		}
		row.Service, row.InstanceName, row.Relation, row.Level, row.Refs, row.Managed = decl.Service, p.names[decl.Key], "owned", "", nil, true
		if id := planIDs[decl.Key]; id != "" {
			row.InstanceID = id
		} else if p.actions[decl.Key] == "create" {
			// The instance it recorded is gone: this run creates a new one.
			row.InstanceID, row.Site = "", nil
		}
	}
	for _, u := range p.m.Uses {
		managed[u.Key] = true
		row := in.Row(u.Key)
		if row == nil {
			row = &InstallInstance{Key: u.Key}
			in.Instances = append(in.Instances, row)
		}
		*row = InstallInstance{Key: u.Key, Service: u.Service, InstanceID: p.bindings[u.Key], InstanceName: p.names[u.Key],
			Relation: "uses", Level: u.Level, Managed: true, Refs: RefsFor(p.refs, u.Key)}
	}
	for _, row := range in.Instances {
		if !managed[row.Key] {
			row.Managed = false
		}
	}
	sort.SliceStable(in.Instances, func(i, j int) bool { return in.Instances[i].Key < in.Instances[j].Key })

	r := &run{d: d, ctx: ctx, p: p, in: in, owned: map[string]string{}, created: map[string]bool{},
		secrets: req.Secrets, staged: map[string][]stagedFn{}, sites: map[string]*preparedSite{}}
	for _, k := range in.PendingInitial {
		r.created[k] = true
	}
	for _, row := range in.Instances {
		if row.Relation == "owned" && row.InstanceID != "" {
			r.owned[row.Key] = row.InstanceID
		}
	}
	r.save()

	if err := r.steps(); err != nil {
		in.Status = "failed"
		in.LastError = asAPIError(err).Message
		r.save()
		return nil, err
	}

	in.Status, in.LastError, in.AppliedSHA256, in.Title, in.Version, in.PendingInitial = "ok", "", p.src.SHA, p.m.Title, p.m.Version, nil
	r.save()
	res := &Result{AppName: p.app, InstallID: in.ID, Instances: []ResultInstance{}, Functions: r.fnResult}
	if res.Functions == nil {
		res.Functions = []ResultFunction{}
	}
	links, _ := r.linksFor()
	for _, decl := range p.m.Instances {
		ri := ResultInstance{Key: decl.Key, Service: decl.Service, Name: p.names[decl.Key], ID: r.owned[decl.Key]}
		if l, ok := links[decl.Key]; ok && l.URL != "" {
			ri.URL = strp(l.URL)
		}
		res.Instances = append(res.Instances, ri)
	}
	return res, nil
}

func nowMS() int64 { return control.NowMS() }

// steps runs every step in the hosted order. An error names the step it stopped at.
func (r *run) steps() error {
	m := r.p.m
	hasConfig := func(d InstanceDecl) bool { return len(d.Config) > 0 || len(d.InitialConfig) > 0 }
	late := func(svc string) bool { return svc == "functions" || svc == "static" }
	step := func(label string, f func() error) error {
		if err := f(); err != nil {
			ae := asAPIError(err)
			return &common.APIError{Status: ae.Status, Code: ae.Code, Message: label + ": " + ae.Message, Details: ae.Details}
		}
		return nil
	}
	type st struct {
		label string
		f     func() error
	}
	var list []st
	each := func(cond func(InstanceDecl) bool, label func(InstanceDecl) string, f func(InstanceDecl) error) {
		for _, d := range m.Instances {
			if cond(d) {
				d := d
				list = append(list, st{label(d), func() error { return f(d) }})
			}
		}
	}
	all := func(InstanceDecl) bool { return true }
	name := func(d InstanceDecl) string { return r.p.names[d.Key] }
	each(all, func(d InstanceDecl) string { return "Creating " + name(d) }, r.stepCreate)
	each(func(d InstanceDecl) bool { return len(d.Secrets) > 0 || len(d.Env) > 0 }, func(d InstanceDecl) string { return "Saving secrets on " + name(d) }, r.stepSecrets)
	each(func(d InstanceDecl) bool { return len(d.Queues) > 0 }, func(d InstanceDecl) string { return "Setting up queues on " + name(d) }, r.stepQueues)
	each(func(d InstanceDecl) bool { return len(d.Indexes) > 0 }, func(d InstanceDecl) string { return "Adding indexes to " + name(d) }, r.stepIndexes)
	each(func(d InstanceDecl) bool { return hasConfig(d) && !late(d.Service) }, func(d InstanceDecl) string { return "Configuring " + name(d) }, r.stepConfig)
	for _, d := range m.Instances {
		for _, f := range d.Functions {
			d, f := d, f
			list = append(list, st{"Uploading function " + f.Name, func() error { return r.stepStage(d, f) }})
		}
	}
	each(func(d InstanceDecl) bool { return d.Site != nil }, func(d InstanceDecl) string { return "Uploading site files of " + name(d) }, r.stepSite)
	each(func(d InstanceDecl) bool { return len(d.Functions) > 0 }, func(d InstanceDecl) string { return "Switching " + name(d) + " to the new functions" }, r.stepActivateFunctions)
	each(func(d InstanceDecl) bool { return d.Service == "functions" && hasConfig(d) }, func(d InstanceDecl) string { return "Configuring " + name(d) }, r.stepConfig)
	each(func(d InstanceDecl) bool { return d.Site != nil }, func(d InstanceDecl) string { return "Publishing the site " + name(d) }, r.stepActivateSite)
	each(func(d InstanceDecl) bool { return d.Service == "static" && hasConfig(d) }, func(d InstanceDecl) string { return "Configuring " + name(d) }, r.stepConfig)
	for _, s := range list {
		if err := step(s.label, s.f); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) markCreated(key, id string) {
	r.owned[key] = id
	r.created[key] = true
	if !isOneOf(key, r.in.PendingInitial) {
		r.in.PendingInitial = append(r.in.PendingInitial, key)
	}
	r.links = nil
}

func (r *run) stepCreate(d InstanceDecl) error {
	name := r.p.names[d.Key]
	row := r.in.Row(d.Key)
	if d.Service == "static" {
		if id := r.owned[d.Key]; id != "" && row.Site != nil {
			return r.d.sites.Open(id, name, row.Site.Port)
		}
		if r.d.byName("static", name) != nil {
			return common.NewError(409, fmt.Sprintf("a static instance named '%s' already exists and this app did not create it", name), "CONFLICT")
		}
		port, err := r.d.sites.Assign(r.d.takenPorts())
		if err != nil {
			return err
		}
		id := common.UUID()
		if err := r.d.sites.Open(id, name, port); err != nil {
			return err
		}
		row.InstanceID, row.Site = id, &SiteRecord{Port: port, Config: map[string]any{}}
		r.markCreated(d.Key, id)
		r.save()
		return nil
	}
	if existing := r.d.reg.Get(d.Service, name); existing != nil {
		if r.owned[d.Key] == existing.ID {
			return nil
		}
		return common.NewError(409, fmt.Sprintf("a %s instance named '%s' already exists and this app did not create it", d.Service, name), "CONFLICT")
	}
	args := map[string]any{"service": d.Service, "name": name}
	if d.Region != "" {
		args["region"] = d.Region
	}
	out, err := r.d.tool(r.ctx, "create_instance", args)
	if err != nil {
		return err
	}
	id, _ := out["id"].(string)
	row.InstanceID = id
	r.markCreated(d.Key, id)
	r.save()
	return nil
}

func (r *run) fnPath(d InstanceDecl, suffix string) string {
	return "/v1/functions/" + url.PathEscape(r.p.names[d.Key]) + suffix
}

func (r *run) stepSecrets(d InstanceDecl) error {
	stored, err := r.d.storedSecrets(r.ctx, r.p.names[d.Key])
	if err != nil {
		return err
	}
	given := r.secrets[d.Key]
	// The emulator's PUT replaces the whole set; `{}` keeps a stored value. So every stored
	// secret is sent as kept, and the manifest's values are merged over them.
	update := map[string]any{}
	for n := range stored {
		update[n] = map[string]any{}
	}
	changed := false
	var missing []string
	for _, s := range d.Secrets {
		v := given[s.Name]
		switch {
		case v != "" && s.Egress:
			update[s.Name] = map[string]any{"value": v, "egress": true, "hosts": s.Hosts}
			changed = true
		case v != "":
			update[s.Name] = v
			changed = true
		case !stored[s.Name] && s.Required:
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		return common.NewError(400, "enter a value for "+strings.Join(missing, ", "), "SECRETS_REQUIRED").WithDetails(map[string]any{"missing": missing})
	}
	// Env values are the app's own settings: written every time, ${key} resolved to a name.
	for _, e := range d.Env {
		v, err := r.resolve(e.Value)
		if err != nil {
			return err
		}
		update[e.Name] = v
		changed = true
	}
	if !changed {
		return nil
	}
	return r.d.rest(r.ctx, http.MethodPut, r.fnPath(d, "/secrets"), map[string]any{"secrets": update}, nil)
}

func (r *run) stepQueues(d InstanceDecl) error {
	name := r.p.names[d.Key]
	list, err := r.d.tool(r.ctx, "tasks_list_queues", map[string]any{"instance": name})
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	if qs, ok := list["queues"].([]any); ok {
		for _, q := range qs {
			if m, ok := q.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					exists[n] = true
				}
			}
		}
	}
	resolved := map[string]map[string]any{}
	dlqs := map[string]bool{}
	for _, q := range d.Queues {
		v, err := r.resolve(q.Settings)
		if err != nil {
			return err
		}
		resolved[q.Name] = v.(map[string]any)
		if s, ok := resolved[q.Name]["deadLetterQueue"].(string); ok {
			dlqs[s] = true
		}
	}
	// A dead-letter queue must exist before the queue that names it.
	order := append([]QueueDecl{}, d.Queues...)
	sort.SliceStable(order, func(i, j int) bool { return dlqs[order[i].Name] && !dlqs[order[j].Name] })
	for _, q := range order {
		args := map[string]any{}
		for k, v := range resolved[q.Name] {
			args[k] = v
		}
		args["instance"], args["name"] = name, q.Name
		if !exists[q.Name] {
			args["create"] = true
		}
		if _, err := r.d.tool(r.ctx, "tasks_set_queue", args); err != nil {
			return fmt.Errorf("queue %s: %w", q.Name, err)
		}
	}
	return nil
}

func (r *run) stepIndexes(d InstanceDecl) error {
	for _, ix := range d.Indexes {
		args := map[string]any{"instance": r.p.names[d.Key], "collection": ix.Collection, "fields": ix.Fields, "unique": ix.Unique}
		if ix.Namespace != "" {
			args["namespace"] = ix.Namespace
		}
		if _, err := r.d.tool(r.ctx, "datastore_create_index", args); err != nil {
			return fmt.Errorf("index %s(%s): %w", ix.Collection, strings.Join(ix.Fields, ", "), err)
		}
	}
	return nil
}

func (r *run) stepConfig(d InstanceDecl) error {
	_, origins := r.linksFor()
	changesV, err := r.resolve(ExpandOriginLists(d.Service, d.Config, origins))
	if err != nil {
		return err
	}
	changes := changesV.(map[string]any)
	// initialConfig only on the run that created the instance: never on an update or an adoption.
	initial := map[string]any{}
	if r.created[d.Key] {
		v, err := r.resolve(ExpandOriginLists(d.Service, d.InitialConfig, origins))
		if err != nil {
			return err
		}
		initial = v.(map[string]any)
	}
	if len(changes) == 0 && len(initial) == 0 {
		return nil
	}
	rw := &row{ID: r.owned[d.Key], Name: r.p.names[d.Key]}
	current, err := r.d.currentConfig(r.ctx, d.Service, rw, r.in, d.Key)
	if err != nil {
		return err
	}
	wanted := MergeConfig(d.Service, MergeConfig(d.Service, current, changes), initial)

	switch d.Service {
	case "static":
		if err := validateStaticConfig(wanted); err != nil {
			return err
		}
		row := r.in.Row(d.Key)
		row.Site.Config = wanted
		r.save()
		if row.Site.Published {
			return r.d.publish(row.InstanceID, row.Site, r.d.sites.Files(row.InstanceID))
		}
		return nil
	case "functions":
		// The outbound allowlist and CORS origins live beside the config in the emulator, behind
		// their own route; everything else is instance config.
		hosts, cors := wanted["allowedHosts"], wanted["corsOrigins"]
		if !jsonEqual(hosts, current["allowedHosts"]) || !jsonEqual(cors, current["corsOrigins"]) {
			if err := r.d.rest(r.ctx, http.MethodPut, r.fnPath(d, "/settings"), map[string]any{"allowedHosts": orEmptyAny(hosts), "corsOrigins": orEmptyAny(cors)}, nil); err != nil {
				return err
			}
		}
		delete(wanted, "allowedHosts")
		delete(wanted, "corsOrigins")
		delete(current, "allowedHosts")
		delete(current, "corsOrigins")
		if jsonEqual(wanted, current) {
			return nil
		}
	}
	_, err = r.d.tool(r.ctx, "patch_instance_config", map[string]any{"service": d.Service, "instance": r.p.names[d.Key], "changes": wanted})
	return err
}

func orEmptyAny(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

func (r *run) stepStage(d InstanceDecl, f FunctionDecl) error {
	name := r.p.names[d.Key]
	gv, err := r.resolve(f.Grants)
	if err != nil {
		return err
	}
	grants := gv.(map[string]string)
	if f.Module.Zip != "" {
		return common.NewError(400, fmt.Sprintf("function %s: modules inside an archive are not supported yet", f.Name), "NOT_SUPPORTED")
	}
	live, err := r.d.liveFunctions(r.ctx, name)
	if err != nil {
		return err
	}
	if same, _ := functionUnchanged(live[f.Name], f, grants); same {
		r.fnResult = append(r.fnResult, ResultFunction{Instance: name, Name: f.Name, Version: live[f.Name].ActiveVersion, Skipped: true})
		return nil
	}
	code, err := r.d.readModule(r.ctx, r.p.src, f.Name, f.Module)
	if err != nil {
		return err
	}
	schedules := f.Schedules
	if schedules == nil {
		schedules = []string{} // left out clears them, as hosted
	}
	body := map[string]any{"name": f.Name, "code": code, "grants": grants, "schedules": schedules, "activate": false}
	if f.CPUMs != nil {
		body["cpuMs"] = int(*f.CPUMs)
	}
	if f.SubRequests != nil {
		body["subRequests"] = int(*f.SubRequests)
	}
	var out struct {
		Version int `json:"version"`
	}
	if err := r.d.rest(r.ctx, http.MethodPost, r.fnPath(d, "/deploy"), body, &out); err != nil {
		return err
	}
	r.staged[d.Key] = append(r.staged[d.Key], stagedFn{name: f.Name, version: out.Version})
	r.fnResult = append(r.fnResult, ResultFunction{Instance: name, Name: f.Name, Version: out.Version})
	return nil
}

func (r *run) stepActivateFunctions(d InstanceDecl) error {
	for _, s := range r.staged[d.Key] {
		if err := r.d.rest(r.ctx, http.MethodPost, r.fnPath(d, "/"+url.PathEscape(s.name)+"/activate"), map[string]any{"version": s.version}, nil); err != nil {
			return fmt.Errorf("function %s: %w", s.name, err)
		}
	}
	return nil
}

// runtimeConfig is the generated file a prebuilt site reads to find its backend, shaped as hosted.
func (r *run) runtimeConfig(d InstanceDecl) ([]byte, error) {
	links, _ := r.linksFor()
	instances := map[string]any{}
	for _, key := range d.RuntimeConfig.Expose {
		svc, id := "", ""
		if decl := r.p.m.Instance(key); decl != nil {
			svc, id = decl.Service, r.owned[key]
		} else if u := r.p.m.Use(key); u != nil {
			svc, id = u.Service, r.p.bindings[key]
		}
		if id == "" {
			return nil, common.NewError(409, key+": the instance it names is gone", "PLAN_STALE")
		}
		var u any
		if l, ok := links[key]; ok && l.URL != "" {
			u = l.URL
		}
		instances[key] = map[string]any{"service": svc, "name": r.p.names[key], "id": id, "url": u}
	}
	var version any
	if r.p.m.Version != "" {
		version = r.p.m.Version
	}
	doc := struct {
		App       string            `json:"app"`
		Version   any               `json:"version"`
		API       string            `json:"api"`
		Vars      map[string]string `json:"vars"`
		Instances map[string]any    `json:"instances"`
	}{r.p.app, version, r.d.api, r.p.m.VarValues, instances}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// stepSite checks every listed file against the manifest and gets them ready to serve. A file
// manifest's site is served from its base directory, live; a URL manifest's is downloaded.
func (r *run) stepSite(d InstanceDecl) error {
	if d.Site.Zip != "" {
		return common.NewError(400, d.Key+": site files inside an archive are not supported yet", "NOT_SUPPORTED")
	}
	row := r.in.Row(d.Key)
	base, err := r.p.src.siteBase(d.Site)
	if err != nil {
		return err
	}
	prep := &preparedSite{}
	paths := sortedKeys(d.Site.Files)
	switch {
	case base.url == nil:
		for _, p := range paths {
			if _, err := r.d.checkFile(r.ctx, base, p, d.Site.Files[p]); err != nil {
				return err
			}
		}
		prep.dir = base.path
	case r.d.dataDir == "":
		prep.files = map[string][]byte{}
		have := r.d.sites.Files(row.InstanceID)
		for _, p := range paths {
			f := d.Site.Files[p]
			if b, ok := have[p]; ok && int64(len(b)) == f.Size && sha256Hex(b) == f.Hash {
				prep.files[p] = b
				continue
			}
			b, err := r.d.checkFile(r.ctx, base, p, f)
			if err != nil {
				return err
			}
			prep.files[p] = b
		}
	default:
		// A new copy beside the live one, switched to at publish: the old site keeps serving
		// until then. Files whose hash did not change are copied, not fetched again.
		root := filepath.Join(r.d.dataDir, "sites")
		if err := os.MkdirAll(root, common.DataDirPerm); err != nil {
			return err
		}
		dir, err := os.MkdirTemp(root, row.InstanceID+"-")
		if err != nil {
			return err
		}
		prev := ""
		if row.Site != nil && row.Site.Downloaded {
			prev = row.Site.Dir
		}
		for _, p := range paths {
			f := d.Site.Files[p]
			rel := filepath.FromSlash(strings.TrimPrefix(p, "/"))
			var b []byte
			if prev != "" {
				if old, err := os.ReadFile(filepath.Join(prev, rel)); err == nil && int64(len(old)) == f.Size && sha256Hex(old) == f.Hash {
					b = old
				}
			}
			if b == nil {
				if b, err = r.d.checkFile(r.ctx, base, p, f); err != nil {
					_ = os.RemoveAll(dir)
					return err
				}
			}
			target := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				_ = os.RemoveAll(dir)
				return err
			}
			if err := os.WriteFile(target, b, 0o644); err != nil {
				_ = os.RemoveAll(dir)
				return err
			}
		}
		prep.dir, prep.downloaded = dir, true
	}
	if d.RuntimeConfig != nil {
		rc, err := r.runtimeConfig(d)
		if err != nil {
			return err
		}
		prep.rcPath, prep.rc = d.RuntimeConfig.Path, rc
	}
	r.sites[d.Key] = prep
	return nil
}

func (r *run) stepActivateSite(d InstanceDecl) error {
	row := r.in.Row(d.Key)
	prep := r.sites[d.Key]
	if prep == nil || row.Site == nil {
		return common.NewError(500, d.Key+": no site was prepared", "INTERNAL")
	}
	old := ""
	if row.Site.Downloaded && row.Site.Dir != prep.dir {
		old = row.Site.Dir
	}
	row.Site.Dir, row.Site.Downloaded = prep.dir, prep.downloaded
	row.Site.RuntimeConfigPath, row.Site.RuntimeConfig = prep.rcPath, string(prep.rc)
	row.Site.Published = true
	if err := r.d.publish(row.InstanceID, row.Site, prep.files); err != nil {
		return err
	}
	r.save()
	if old != "" && strings.HasPrefix(old, filepath.Join(r.d.dataDir, "sites")+string(filepath.Separator)) {
		_ = os.RemoveAll(old)
	}
	return nil
}
