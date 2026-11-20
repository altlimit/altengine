package manifest

// The plan: what deploying this manifest under this name would do. A port of hosted plan.ts —
// create / update / adopt / conflict per instance, bindings for `uses`, escalations on an update,
// problems that block and warnings that do not. It reads and never writes.
//
// What the emulator leaves out: role checks (there is one local org and no members), quotas, the
// functions beta gate, and validating config before the apply (several checks need the instances
// to exist — a trigger names a queue — so a bad value is reported by the step that writes it).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/functions"
)

// Plan is what a deploy would do.
type Plan struct {
	Mode          string         `json:"mode"` // new | update
	AppName       string         `json:"app_name"`
	InstallID     *string        `json:"install_id"`
	Manifest      PlanManifest   `json:"manifest"`
	Vars          []PlanVar      `json:"vars"`
	Instances     []PlanInstance `json:"instances"`
	Bindings      []PlanBinding  `json:"bindings"`
	Unmanaged     []PlanRef      `json:"unmanaged"`
	Secrets       []PlanSecret   `json:"secrets"`
	Warnings      []string       `json:"warnings"`
	Problems      []string       `json:"problems"`
	Escalations   []string       `json:"escalations"`
	SourceChanged *SourceChange  `json:"source_changed"`
	Adoptions     []PlanAdoption `json:"adoptions"`
	Ready         bool           `json:"ready"`
}

// PlanManifest describes the manifest a plan was made from.
type PlanManifest struct {
	Source      string  `json:"source"`
	Host        string  `json:"host"`
	SHA256      string  `json:"sha256"`
	Title       *string `json:"title"`
	Version     *string `json:"version"`
	Description *string `json:"description"`
	Homepage    *string `json:"homepage"`
}

// PlanInstance is one instance the app creates or manages.
type PlanInstance struct {
	Key     string   `json:"key"`
	Service string   `json:"service"`
	Name    string   `json:"name"`
	Action  string   `json:"action"` // create | update | adopt | conflict
	ID      *string  `json:"id"`
	URL     *string  `json:"url"`
	Changes []string `json:"changes"`
	// Problems block the deploy.
	Problems []string `json:"problems"`
}

// PlanBinding is one `uses` entry and the instance it is bound to.
type PlanBinding struct {
	Key        string    `json:"key"`
	Service    string    `json:"service"`
	Level      string    `json:"level"`
	Why        *string   `json:"why"`
	Suggest    *string   `json:"suggest"`
	Instance   *PlanRef  `json:"instance"`
	Uses       []string  `json:"uses"`
	Candidates []PlanRef `json:"candidates"`
	Problems   []string  `json:"problems"`
}

// PlanRef names an instance.
type PlanRef struct {
	Key     string `json:"key,omitempty"`
	Service string `json:"service,omitempty"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
}

// PlanSecret is a secret the deploy asks for.
type PlanSecret struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Required    bool    `json:"required"`
	Stored      bool    `json:"stored"`
}

// PlanVar is a var and the value it has for this deploy.
type PlanVar struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Value       *string `json:"value"`
	Required    bool    `json:"required"`
	Default     *string `json:"default"`
}

// SourceChange is an update from a different place than the install came from.
type SourceChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PlanAdoption is an existing instance the deploy would take over.
type PlanAdoption struct {
	Key     string `json:"key"`
	Service string `json:"service"`
	Name    string `json:"name"`
	ID      string `json:"id"`
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// row is an existing instance.
type row struct {
	ID   string
	Name string
	Port int // static only
}

// planned is a plan with what applying it needs.
type planned struct {
	src      *Source
	m        *Manifest
	refs     []Ref
	app      string
	install  *Install // nil for a new install
	names    map[string]string
	bindings map[string]string // uses key → instance id
	rows     map[string]*row   // instance key → existing instance (create: absent)
	actions  map[string]string
	plan     *Plan
}

// lookup finds an instance by name or id.
func (d *Deployer) lookup(service, ref string) *row {
	if ref == "" {
		return nil
	}
	if service == "static" {
		if s := d.store.StaticByRef(ref); s != nil {
			return &row{ID: s.ID, Name: s.Name, Port: s.Site.Port}
		}
		return nil
	}
	in := d.reg.Get(service, ref)
	if in == nil {
		in = d.reg.GetByID(service, ref)
	}
	if in == nil {
		return nil
	}
	return &row{ID: in.ID, Name: in.Name}
}

// byName finds an instance by name only (what a planned name collides with).
func (d *Deployer) byName(service, name string) *row {
	if service == "static" {
		for _, s := range d.store.Sites() {
			if s.Name == name {
				return &row{ID: s.ID, Name: s.Name, Port: s.Site.Port}
			}
		}
		return nil
	}
	if in := d.reg.Get(service, name); in != nil {
		return &row{ID: in.ID, Name: in.Name}
	}
	return nil
}

func (d *Deployer) candidates(service string) []PlanRef {
	out := []PlanRef{}
	if service == "static" {
		for _, s := range d.store.Sites() {
			out = append(out, PlanRef{ID: s.ID, Name: s.Name})
		}
	} else {
		for _, in := range d.reg.List(service) {
			out = append(out, PlanRef{ID: in.ID, Name: in.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// takenPorts are the ports every recorded site holds.
func (d *Deployer) takenPorts() map[int]bool {
	taken := map[int]bool{}
	for _, s := range d.store.Sites() {
		taken[s.Site.Port] = true
	}
	return taken
}

// RefsFor are the references an app makes to a key, as a binding stores them.
func RefsFor(refs []Ref, key string) []string {
	out := []string{}
	for _, r := range refs {
		if r.Target == key {
			out = append(out, fmt.Sprintf("%s.%s:%s", r.From, r.Field, r.Level))
		}
	}
	sort.Strings(out)
	return out
}

func describeRef(r Ref) string {
	return fmt.Sprintf("%s %s (%s)", r.From, strings.TrimPrefix(r.Field, "config."), r.Level)
}

func planVars(decls []VarDecl, values map[string]string) []PlanVar {
	out := []PlanVar{}
	for _, d := range decls {
		pv := PlanVar{Name: d.Name, Description: strp(d.Description), Required: d.Required && d.Default == nil, Default: d.Default}
		if values != nil {
			if v, ok := values[d.Name]; ok {
				vv := v
				pv.Value = &vv
			}
		}
		out = append(out, pv)
	}
	return out
}

// liveFunction is a deployed function's state, as the functions API reports it.
type liveFunction struct {
	Name          string            `json:"name"`
	ActiveVersion int               `json:"active_version"`
	Grants        map[string]string `json:"grants"`
	Schedules     []string          `json:"schedules"`
	CPUMs         int               `json:"cpu_ms"`
	SubRequests   int               `json:"sub_requests"`
	sha           string
}

// liveFunctions reads an existing functions instance's functions and each one's live code hash.
func (d *Deployer) liveFunctions(ctx context.Context, instance string) (map[string]*liveFunction, error) {
	var list struct {
		Functions []*liveFunction `json:"functions"`
	}
	base := "/v1/functions/" + url.PathEscape(instance)
	if err := d.rest(ctx, http.MethodGet, base, nil, &list); err != nil {
		return nil, err
	}
	out := map[string]*liveFunction{}
	for _, f := range list.Functions {
		var v struct {
			Versions []struct {
				Version int    `json:"version"`
				SHA256  string `json:"sha256"`
			} `json:"versions"`
		}
		if err := d.rest(ctx, http.MethodGet, base+"/"+url.PathEscape(f.Name)+"/versions", nil, &v); err != nil {
			return nil, err
		}
		for _, x := range v.Versions {
			if x.Version == f.ActiveVersion {
				f.sha = x.SHA256
			}
		}
		out[f.Name] = f
	}
	return out, nil
}

// storedSecrets reads the names of an existing functions instance's secrets.
func (d *Deployer) storedSecrets(ctx context.Context, instance string) (map[string]bool, error) {
	var res struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := d.rest(ctx, http.MethodGet, "/v1/functions/"+url.PathEscape(instance)+"/secrets", nil, &res); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range res.Secrets {
		out[s.Name] = true
	}
	return out, nil
}

// currentConfig is an existing instance's config as a manifest's config merges onto it. For
// functions, the outbound allowlist and CORS origins (kept beside the config in the emulator) are
// read back into it, where hosted keeps them.
func (d *Deployer) currentConfig(ctx context.Context, service string, r *row, install *Install, key string) (map[string]any, error) {
	if service == "static" {
		if install != nil {
			if ir := install.Row(key); ir != nil && ir.Site != nil && ir.Site.Config != nil {
				return copyMap(ir.Site.Config), nil
			}
		}
		return map[string]any{}, nil
	}
	in := d.reg.GetByID(service, r.ID)
	if in == nil {
		return map[string]any{}, nil
	}
	cur := Plain(d.reg.ConfigSnapshot(in)).(map[string]any)
	if service == "functions" {
		var s struct {
			AllowedHosts []string `json:"allowedHosts"`
			CORSOrigins  []string `json:"corsOrigins"`
		}
		if err := d.rest(ctx, http.MethodGet, "/v1/functions/"+url.PathEscape(r.Name)+"/settings", nil, &s); err != nil {
			return nil, err
		}
		cur["allowedHosts"] = toAnyList(s.AllowedHosts)
		cur["corsOrigins"] = toAnyList(s.CORSOrigins)
	}
	return cur, nil
}

func toAnyList(in []string) []any {
	out := []any{}
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

var (
	fnNameRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	reservedFnNames = map[string]bool{"api": true, "admin": true, "v1": true, "assets": true, "healthz": true, "robots": true,
		"favicon": true, "site": true, "__resetdb": true, "well-known": true}
)

// functionDefProblem is hosted validateFunctionDef's answer for a manifest function, or "".
func functionDefProblem(f FunctionDecl) string {
	if !fnNameRE.MatchString(f.Name) {
		return "function.name must be 1-63 chars of [a-z0-9_-], starting alphanumeric"
	}
	if reservedFnNames[f.Name] {
		return fmt.Sprintf("function.name '%s' is reserved by the platform", f.Name)
	}
	if f.CPUMs != nil && (!isInteger(*f.CPUMs) || *f.CPUMs < 1 || *f.CPUMs > functions.MaxCPUMs) {
		return fmt.Sprintf("function.cpuMs must be an integer in 1..%d", functions.MaxCPUMs)
	}
	if f.SubRequests != nil && (!isInteger(*f.SubRequests) || *f.SubRequests < 0 || *f.SubRequests > functions.MaxSubReq) {
		return fmt.Sprintf("function.subRequests must be an integer in 0..%d", functions.MaxSubReq)
	}
	if _, err := functions.NormalizeSchedules(f.Schedules); err != nil {
		return "function.schedules: " + err.Error()
	}
	return ""
}

// functionUnchanged is the hosted skip rule: same code, grants, schedules and limits.
func functionUnchanged(live *liveFunction, f FunctionDecl, grants map[string]string) (same bool, why string) {
	if live == nil {
		return false, "new"
	}
	if live.sha != f.Module.SHA256[:32] {
		return false, "new code"
	}
	lg := live.Grants
	if lg == nil {
		lg = map[string]string{}
	}
	if !jsonEqual(lg, grants) {
		return false, "new access"
	}
	sched, _ := functions.NormalizeSchedules(f.Schedules)
	if !jsonEqual(orEmpty(live.Schedules), orEmpty(sched)) {
		return false, "new schedules"
	}
	if f.CPUMs != nil && int(*f.CPUMs) != live.CPUMs || f.SubRequests != nil && int(*f.SubRequests) != live.SubRequests {
		return false, "new limits"
	}
	return true, ""
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// plan loads the manifest and plans it against the current state.
func (d *Deployer) plan(ctx context.Context, req Request) (*planned, error) {
	src, err := d.LoadSource(ctx, req.Manifest)
	if err != nil {
		return nil, err
	}
	requested := strings.TrimSpace(req.Name)
	if requested != "" {
		if err := ValidateAppName(requested); err != nil {
			return nil, err
		}
	}
	// Var values: explicit, then the manifest URL's query string, then the values the install was
	// last deployed with — the install named, or else the latest one from this same manifest.
	var prior *Install
	if requested != "" {
		prior = d.store.Get(requested)
	} else if list := d.store.FromSource(src.Ref); len(list) > 0 {
		prior = list[0]
	}
	sources := []map[string]string{req.Vars, src.Query}
	if prior != nil {
		sources = append(sources, prior.Vars)
	}
	m, refs, err := Parse(src.JSON, sources)
	if mv, ok := err.(*MissingVarsError); ok {
		return &planned{src: src, plan: needsVars(src, sources, mv.Missing)}, nil
	}
	if err != nil {
		return nil, err
	}
	app := requested
	if app == "" {
		app = m.Name
	}
	p := &planned{src: src, m: m, refs: refs, app: app, install: d.store.Get(app),
		names: map[string]string{}, bindings: map[string]string{}, rows: map[string]*row{}, actions: map[string]string{}}
	bindings := map[string]string{}
	for k, v := range req.Bindings {
		if strings.TrimSpace(v) != "" {
			bindings[k] = strings.TrimSpace(v)
		}
	}
	if p.install != nil && len(bindings) == 0 {
		for _, r := range p.install.Instances {
			if r.Relation == "uses" && r.InstanceID != "" {
				bindings[r.Key] = r.InstanceID
			}
		}
	}
	plan, err := d.buildPlan(ctx, p, bindings)
	if err != nil {
		return nil, err
	}
	p.plan = plan
	return p, nil
}

func needsVars(src *Source, sources []map[string]string, missing []string) *Plan {
	raw, _ := src.JSON.(*Obj)
	decls, _ := ReadVarDecls(src.JSON)
	known := map[string]string{}
	for _, d := range decls {
		for _, s := range sources {
			if v, ok := s[d.Name]; ok && v != "" {
				known[d.Name] = v
				break
			}
		}
	}
	str := func(k string) *string {
		if raw == nil {
			return nil
		}
		if s, ok := raw.get(k).(string); ok {
			return &s
		}
		return nil
	}
	return &Plan{
		Mode: "new", Vars: planVars(decls, known),
		Manifest:  PlanManifest{Source: src.Ref, Host: src.Host, SHA256: src.SHA, Title: str("title"), Version: str("version"), Description: str("description")},
		Instances: []PlanInstance{}, Bindings: []PlanBinding{}, Unmanaged: []PlanRef{}, Secrets: []PlanSecret{}, Warnings: []string{},
		Problems: []string{fmt.Sprintf("Enter a value for %s.", strings.Join(missing, ", "))}, Escalations: []string{}, Adoptions: []PlanAdoption{},
	}
}

func (d *Deployer) buildPlan(ctx context.Context, p *planned, wantBind map[string]string) (*Plan, error) {
	m, refs, install := p.m, p.refs, p.install
	problems, warnings, escalations := []string{}, []string{}, []string{}

	if m.Source != nil {
		problems = append(problems, "This manifest reads its files from an archive (source.zip), which is not supported yet. List the files instead.")
	}
	for _, d := range m.Instances {
		if d.Site != nil && d.Site.Zip != "" {
			problems = append(problems, d.Key+": site files inside an archive are not supported yet")
		}
		for _, f := range d.Functions {
			if f.Module.Zip != "" {
				problems = append(problems, fmt.Sprintf("%s: function %s inside an archive is not supported yet", d.Key, f.Name))
			}
		}
	}

	for _, d := range m.Instances {
		p.names[d.Key] = InstanceName(p.app, d.Key, d.Service, d.Suffix)
	}

	// --- bindings (uses) ---
	installed := map[string]*InstallInstance{}
	if install != nil {
		for _, r := range install.Instances {
			installed[r.Key] = r
		}
	}
	links := map[string]Link{}
	origins := map[string][]string{}
	bindings := []PlanBinding{}
	for _, u := range m.Uses {
		b := PlanBinding{Key: u.Key, Service: u.Service, Level: u.Level, Why: strp(u.Why), Suggest: strp(u.Suggest), Uses: []string{}, Candidates: d.candidates(u.Service), Problems: []string{}}
		for _, r := range refs {
			if r.Target == u.Key {
				b.Uses = append(b.Uses, describeRef(r))
			}
		}
		prev := installed[u.Key]
		wanted := wantBind[u.Key]
		if wanted == "" && prev != nil && prev.Relation == "uses" {
			wanted = prev.InstanceID
		}
		if wanted == "" && u.Suggest != "" && d.byName(u.Service, u.Suggest) != nil {
			wanted = u.Suggest
		}
		if wanted != "" {
			found := d.lookup(u.Service, wanted)
			if found == nil {
				b.Problems = append(b.Problems, fmt.Sprintf("no %s instance '%s'", u.Service, wanted))
			} else {
				b.Instance = &PlanRef{ID: found.ID, Name: found.Name}
				p.names[u.Key] = found.Name
				p.bindings[u.Key] = found.ID
				for _, r := range refs {
					if r.Target == u.Key && !LevelAtLeast(u.Level, r.Level) {
						b.Problems = append(b.Problems, fmt.Sprintf("%s needs '%s', more than the '%s' this manifest asks for", describeRef(r), r.Level, u.Level))
					}
				}
				if IsHosted(u.Service) {
					links[u.Key] = d.linkOf(u.Service, found.Name, found.Port)
					if links[u.Key].Origin != "" {
						origins[u.Key] = []string{links[u.Key].Origin}
					}
				}
			}
		} else {
			b.Problems = append(b.Problems, fmt.Sprintf("choose which %s instance '%s' should use (bindings: {\"%s\": \"<instance>\"}, or --bind %s=<instance>)", u.Service, u.Key, u.Key, u.Key))
		}
		if install != nil && b.Instance != nil {
			now := RefsFor(refs, u.Key)
			if prev == nil || prev.Relation != "uses" {
				escalations = append(escalations, fmt.Sprintf("uses %s '%s' (%s), which the installed version did not", u.Service, b.Instance.Name, u.Level))
			} else {
				if prev.InstanceID != b.Instance.ID {
					escalations = append(escalations, fmt.Sprintf("'%s' now uses %s '%s' instead of '%s'", u.Key, u.Service, b.Instance.Name, prev.InstanceName))
				}
				if prev.Level != "" && rank[u.Level] > rank[prev.Level] {
					escalations = append(escalations, fmt.Sprintf("'%s' asks for '%s' on '%s', up from '%s'", u.Key, u.Level, b.Instance.Name, prev.Level))
				}
				var added []string
				for _, r := range now {
					if !isOneOf(r, prev.Refs) {
						added = append(added, r)
					}
				}
				if len(added) > 0 {
					escalations = append(escalations, fmt.Sprintf("new access to '%s': %s", b.Instance.Name, strings.Join(added, ", ")))
				}
			}
		}
		if b.Instance != nil {
			warnings = append(warnings, fmt.Sprintf("This app gets %s access to your existing %s instance '%s'.", u.Level, u.Service, b.Instance.Name))
		}
		bindings = append(bindings, b)
	}
	allBound := true
	for _, b := range bindings {
		allBound = allBound && b.Instance != nil
	}

	// --- owned instances: what exists under each planned name, and its address ---
	taken := d.takenPorts()
	for _, decl := range m.Instances {
		name := p.names[decl.Key]
		if r := d.byName(decl.Service, name); r != nil {
			p.rows[decl.Key] = r
		}
		if !IsHosted(decl.Service) {
			continue
		}
		port := 0
		if decl.Service == "static" {
			if r := p.rows[decl.Key]; r != nil {
				port = r.Port
			} else {
				// Not created yet: shown at the port it will get, as hosted shows a new site at the
				// address it will most likely get.
				if np, err := d.sites.Assign(taken); err == nil {
					port = np
					taken[np] = true
				}
			}
		}
		links[decl.Key] = d.linkOf(decl.Service, name, port)
		if links[decl.Key].Origin != "" {
			origins[decl.Key] = []string{links[decl.Key].Origin}
		}
	}

	pending := map[string]bool{}
	if install != nil {
		for _, k := range install.PendingInitial {
			pending[k] = true
		}
	}

	instances := []PlanInstance{}
	secrets := []PlanSecret{}
	for _, decl := range m.Instances {
		name := p.names[decl.Key]
		r := p.rows[decl.Key]
		mine := installed[decl.Key]
		owned := r != nil && mine != nil && mine.Relation == "owned" && mine.InstanceID == r.ID
		item := PlanInstance{Key: decl.Key, Service: decl.Service, Name: name, Changes: []string{}, Problems: []string{}}
		switch {
		case r == nil:
			item.Action = "create"
		case owned:
			item.Action = "update"
		default:
			owner := d.store.Owner(decl.Service, r.ID)
			if owner != "" && (install == nil || owner != install.Name) {
				item.Action = "conflict"
			} else {
				item.Action = "adopt"
			}
		}
		if r != nil {
			item.ID = strp(r.ID)
		}
		if l, ok := links[decl.Key]; ok && l.URL != "" {
			item.URL = strp(l.URL)
		}
		p.actions[decl.Key] = item.Action
		switch item.Action {
		case "conflict":
			item.Problems = append(item.Problems, fmt.Sprintf("another deployed app manages the %s instance '%s' — choose another app name", decl.Service, name))
		case "adopt":
			if decl.Service == "static" {
				item.Problems = append(item.Problems, fmt.Sprintf("a local site named '%s' is recorded under another key of this app; deploy under another app name", name))
			} else {
				item.Changes = append(item.Changes, fmt.Sprintf("take over the existing %s instance '%s'; this app manages it from now on", decl.Service, name))
			}
		case "create":
			where := ""
			if decl.Region != "" {
				where = " in " + decl.Region
			}
			line := fmt.Sprintf("create %s instance '%s'%s", decl.Service, name, where)
			if decl.Service == "static" && item.URL != nil {
				line += " at " + *item.URL
			}
			item.Changes = append(item.Changes, line)
			if decl.Region != "" {
				if err := common.CheckRegion(map[string]any{"region": decl.Region}); err != nil {
					item.Problems = append(item.Problems, "region must be one of: "+strings.Join(common.Regions, ", "))
				} else if decl.Region != "auto" && decl.Service != "static" && !control.RegionAtCreate(decl.Service) {
					item.Problems = append(item.Problems, fmt.Sprintf("region cannot be chosen at creation for %s", decl.Service))
				}
			}
		}

		// Config: merged onto what is there, so the plan can say which keys change.
		created := item.Action == "create" || pending[decl.Key]
		initial := map[string]any{}
		if created {
			initial = decl.InitialConfig
		}
		if (len(decl.Config) > 0 || len(initial) > 0) && item.Action != "conflict" && allBound {
			problem := func() string {
				resolved, err := ResolveRefs(ExpandOriginLists(decl.Service, decl.Config, origins), p.names, links)
				if err != nil {
					return "config: " + asAPIError(err).Message
				}
				resolvedInitial, err := ResolveRefs(ExpandOriginLists(decl.Service, initial, origins), p.names, links)
				if err != nil {
					return "config: " + asAPIError(err).Message
				}
				current := map[string]any{}
				if r != nil {
					c, err := d.currentConfig(ctx, decl.Service, r, install, decl.Key)
					if err != nil {
						return "config: " + asAPIError(err).Message
					}
					current = c
				}
				rc, ri := resolved.(map[string]any), resolvedInitial.(map[string]any)
				wanted := MergeConfig(decl.Service, MergeConfig(decl.Service, current, rc), ri)
				var changed []string
				for _, k := range sortedKeys(mergeKeys(rc, ri)) {
					if !jsonEqual(current[k], wanted[k]) {
						changed = append(changed, k)
					}
				}
				if len(changed) > 0 {
					item.Changes = append(item.Changes, "set "+strings.Join(changed, ", "))
				}
				if len(ri) > 0 {
					item.Changes = append(item.Changes, "on creation only: "+strings.Join(sortedKeys(ri), ", "))
				}
				if decl.Service == "static" {
					if err := validateStaticConfig(wanted); err != nil {
						return "config: " + err.(*common.APIError).Message
					}
				}
				return ""
			}()
			if problem != "" {
				item.Problems = append(item.Problems, problem)
			}
		}

		if n := len(decl.Indexes); n > 0 {
			word := "indexes"
			if n == 1 {
				word = "index"
			}
			item.Changes = append(item.Changes, fmt.Sprintf("%d datastore %s (added if missing)", n, word))
		}
		if len(decl.Queues) > 0 {
			var qn []string
			for _, q := range decl.Queues {
				qn = append(qn, q.Name)
			}
			item.Changes = append(item.Changes, "queues: "+strings.Join(qn, ", "))
		}

		if decl.Service == "functions" {
			stored := map[string]bool{}
			live := map[string]*liveFunction{}
			if r != nil && item.Action != "conflict" {
				var err error
				if stored, err = d.storedSecrets(ctx, r.Name); err != nil {
					return nil, err
				}
				if live, err = d.liveFunctions(ctx, r.Name); err != nil {
					return nil, err
				}
			}
			if n := len(decl.Env); n > 0 {
				var en []string
				for _, e := range decl.Env {
					en = append(en, e.Name)
				}
				s := "s"
				if n == 1 {
					s = ""
				}
				item.Changes = append(item.Changes, fmt.Sprintf("set %d environment value%s: %s", n, s, strings.Join(en, ", ")))
			}
			for _, s := range decl.Secrets {
				secrets = append(secrets, PlanSecret{Key: decl.Key, Name: s.Name, Description: strp(s.Description), Required: s.Required, Stored: stored[s.Name]})
			}
			for _, f := range decl.Functions {
				if bad := functionDefProblem(f); bad != "" {
					item.Problems = append(item.Problems, bad)
				}
				grants := map[string]string{}
				if allBound {
					g, err := ResolveRefs(f.Grants, p.names, links)
					if err != nil {
						item.Problems = append(item.Problems, fmt.Sprintf("function %s: %s", f.Name, asAPIError(err).Message))
						continue
					}
					grants = g.(map[string]string)
				}
				if same, why := functionUnchanged(live[f.Name], f, grants); !same {
					item.Changes = append(item.Changes, fmt.Sprintf("deploy function %s (%s)", f.Name, why))
				}
			}
		}

		if decl.Site != nil && decl.Site.Files != nil {
			var bytes int64
			for _, f := range decl.Site.Files {
				bytes += f.Size
			}
			size := fmt.Sprintf("%d KB", max(1, (bytes+512)/1024))
			if bytes >= 1024*1024 {
				size = fmt.Sprintf("%.1f MB", float64(bytes)/1024/1024)
			}
			with := ""
			if decl.RuntimeConfig != nil {
				with = " with " + decl.RuntimeConfig.Path
			}
			item.Changes = append(item.Changes, fmt.Sprintf("publish a site of %d files (%s)%s", len(decl.Site.Files), size, with))
		}
		if r != nil && len(item.Changes) == 0 {
			item.Changes = append(item.Changes, "no changes")
		}
		instances = append(instances, item)
	}

	// --- the install's other rows, and where it came from ---
	keys := map[string]bool{}
	for _, d := range m.Instances {
		keys[d.Key] = true
	}
	for _, u := range m.Uses {
		keys[u.Key] = true
	}
	unmanaged := []PlanRef{}
	if install != nil {
		for _, r := range install.Instances {
			if r.Relation == "owned" && r.InstanceID != "" && !keys[r.Key] {
				unmanaged = append(unmanaged, PlanRef{Key: r.Key, Service: r.Service, Name: r.InstanceName})
			}
		}
	}
	if n := len(unmanaged); n > 0 {
		if n == 1 {
			warnings = append(warnings, "1 instance is no longer in this manifest. It is kept, and this app stops changing it.")
		} else {
			warnings = append(warnings, fmt.Sprintf("%d instances are no longer in this manifest. They are kept, and this app stops changing them.", n))
		}
	}
	var sourceChanged *SourceChange
	if install != nil && install.ManifestHost != p.src.Host {
		sourceChanged = &SourceChange{From: install.ManifestURL, To: p.src.Ref}
	}
	for _, d := range m.Instances {
		if d.RuntimeConfig == nil {
			continue
		}
		for _, k := range d.RuntimeConfig.Expose {
			for _, b := range bindings {
				if b.Key == k && b.Instance != nil {
					warnings = append(warnings, fmt.Sprintf("The site publishes the name and id of your existing %s instance '%s' in %s.", b.Service, b.Instance.Name, d.RuntimeConfig.Path))
				}
			}
		}
	}
	var missing []string
	for _, s := range secrets {
		if s.Required && !s.Stored {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) == 1 {
		warnings = append(warnings, fmt.Sprintf("Enter a value for %s.", missing[0]))
	} else if len(missing) > 1 {
		warnings = append(warnings, fmt.Sprintf("Enter values for %s.", strings.Join(missing, ", ")))
	}

	all := append([]string{}, problems...)
	for _, i := range instances {
		for _, pr := range i.Problems {
			all = append(all, i.Name+": "+pr)
		}
	}
	for _, b := range bindings {
		for _, pr := range b.Problems {
			all = append(all, b.Key+": "+pr)
		}
	}
	adoptions := []PlanAdoption{}
	for _, i := range instances {
		if i.Action == "adopt" && i.ID != nil {
			adoptions = append(adoptions, PlanAdoption{Key: i.Key, Service: i.Service, Name: i.Name, ID: *i.ID})
		}
	}
	plan := &Plan{
		Mode: "new", AppName: p.app, Vars: planVars(m.Vars, m.VarValues),
		Manifest: PlanManifest{Source: p.src.Ref, Host: p.src.Host, SHA256: p.src.SHA, Title: strp(m.Title), Version: strp(m.Version),
			Description: strp(m.Description), Homepage: strp(m.Homepage)},
		Instances: instances, Bindings: bindings, Unmanaged: unmanaged, Secrets: secrets, Warnings: warnings,
		Problems: all, Escalations: escalations, SourceChanged: sourceChanged, Adoptions: adoptions, Ready: len(all) == 0,
	}
	if install != nil {
		plan.Mode = "update"
		plan.InstallID = strp(install.ID)
	}
	return plan, nil
}

func mergeKeys(a, b map[string]any) map[string]any {
	out := copyMap(a)
	for k, v := range b {
		out[k] = v
	}
	return out
}
