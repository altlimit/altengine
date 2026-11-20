// Package manifest deploys a "Deploy to AltEngine" manifest into the emulator: the same JSON file
// the hosted deploy page reads, parsed with the same rules, planned with the same create / update /
// adopt / conflict classification, and applied in the same step order — through the emulator's own
// MCP tools and REST routes, so a config the emulator refuses anywhere is refused here too.
//
// This file is the parser: a port of the hosted src/deploy/manifest.ts. Its error messages and its
// limits are the hosted ones, so a manifest refused hosted is refused locally with the same words.
//
// THE RULES THAT MATTER MOST (hosted's, unchanged)
//   - Every instance an app creates is named from the app name: `N` for a key equal to its
//     service, `N-<key>` otherwise, `N-<suffix>` with a suffix. An author cannot pick names.
//   - Anywhere config names another instance it does so with `${key}`, never a literal name.
//   - Config fields that move code or data on their own cannot be set through `config`.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/altlimit/altengine/cli/internal/common"
)

// FormatVersion is the manifest format version (`"altengine": 1`).
const FormatVersion = 1

// DeployServices are the services a v1 manifest may declare or use, as hosted.
var DeployServices = []string{"search", "datastore", "channel", "auth", "functions", "static", "tasks", "blob"}

// HostedServices are served from a public address of their own: what `:url` and `:origin` name.
var HostedServices = []string{"static", "functions", "blob"}

// Limits are the hosted MANIFEST_LIMITS.
var Limits = struct {
	Bytes, Instances, Uses, IndexesPerInstance, QueuesPerInstance, SecretsPerInstance int
	FunctionsPerInstance, StaticFiles, Text, URL                                      int
}{
	Bytes: 2 * 1024 * 1024, Instances: 12, Uses: 8, IndexesPerInstance: 128, QueuesPerInstance: 20,
	SecretsPerInstance: 32, FunctionsPerInstance: 100, StaticFiles: 5000, Text: 200, URL: 2048,
}

// MaxVars caps the `vars` block.
const MaxVars = 20

var (
	levels = []string{"read", "write", "full"}
	rank   = map[string]int{"read": 1, "write": 2, "full": 3}

	// AppNameRE: app names become instance names.
	AppNameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	keyRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	hex64       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envNameRE   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	placeholder = regexp.MustCompile(`\$\{([a-z][a-z0-9-]{0,30})(?::(url|origin))?\}`)
	originPH    = regexp.MustCompile(`^\$\{([a-z][a-z0-9-]{0,30}):origin\}$`)
	exactPH     = regexp.MustCompile(`^\$\{([a-z][a-z0-9-]{0,30})\}$`)
	schemeRE    = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
	httpRE      = regexp.MustCompile(`(?i)^https?://`)
	controlRE   = regexp.MustCompile("[\u0000-\u001f\u007f]")

	varNameRE   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,31}$`)
	varValueRE  = regexp.MustCompile("^[^\u0000-\u001f\u007f${}\"\\\\]{0,200}$")
	varPHRE     = regexp.MustCompile(`\{\{\s*([a-zA-Z][a-zA-Z0-9_]{0,31})\s*\}\}`)
	reservedEnv = map[string]bool{"DATASTORE": true, "SEARCH": true, "AUTH": true, "CHANNEL": true}
)

// LevelAtLeast reports whether `have` covers `need`.
func LevelAtLeast(have, need string) bool { return rank[have] >= rank[need] }

func isOneOf(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// IsHosted reports whether a service has a public address of its own.
func IsHosted(service string) bool { return isOneOf(service, HostedServices) }

// --- declarations ---------------------------------------------------------------------------

// UsesDecl is an existing instance the app needs access to.
type UsesDecl struct {
	Key     string `json:"key"`
	Service string `json:"service"`
	Level   string `json:"level"`
	Why     string `json:"why,omitempty"`
	Suggest string `json:"suggest,omitempty"`
}

// ModuleRef is a function's code: a URL and its sha256, or a path inside the source archive.
type ModuleRef struct {
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Zip    string `json:"zip,omitempty"`
}

// FunctionDecl is one function of a functions instance.
type FunctionDecl struct {
	Name   string            `json:"name"`
	Module ModuleRef         `json:"module"`
	Grants map[string]string `json:"grants"`
	// Schedules is nil when the manifest leaves them out (which clears them on deploy).
	Schedules   []string `json:"schedules,omitempty"`
	CPUMs       *float64 `json:"cpuMs,omitempty"`
	SubRequests *float64 `json:"subRequests,omitempty"`
}

// SecretDecl is a secret the installer types a value for.
type SecretDecl struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required"`
	Egress      bool     `json:"egress"`
	Hosts       []string `json:"hosts,omitempty"`
}

// VarDecl is one `{{name}}` variable.
type VarDecl struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Default     *string `json:"default,omitempty"`
	Required    bool    `json:"required"`
	Pattern     string  `json:"pattern,omitempty"`
	re          *regexp.Regexp
}

// IndexDecl is one datastore index.
type IndexDecl struct {
	Namespace  string   `json:"namespace"`
	Collection string   `json:"collection"`
	Fields     []string `json:"fields"`
	Unique     bool     `json:"unique"`
}

// SiteFile is one file of a static site.
type SiteFile struct {
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
}

// SiteDecl is a static site: files fetched from base + path, or a path in the source archive.
type SiteDecl struct {
	Base  string              `json:"base,omitempty"`
	Files map[string]SiteFile `json:"files,omitempty"`
	Zip   string              `json:"zip,omitempty"`
}

// RuntimeConfigDecl is the generated JSON file a site reads to find its backend.
type RuntimeConfigDecl struct {
	Path   string   `json:"path"`
	Expose []string `json:"expose"`
}

// QueueDecl is one queue of a tasks instance, with its settings as tasks_set_queue takes them.
type QueueDecl struct {
	Name     string         `json:"name"`
	Settings map[string]any `json:"settings"`
}

// EnvVar is a plain environment value for a functions instance.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// InstanceDecl is one instance the app creates.
type InstanceDecl struct {
	Key           string             `json:"key"`
	Service       string             `json:"service"`
	Suffix        string             `json:"suffix,omitempty"`
	Region        string             `json:"region,omitempty"`
	Config        map[string]any     `json:"config"`
	InitialConfig map[string]any     `json:"initialConfig"`
	Indexes       []IndexDecl        `json:"indexes"`
	Queues        []QueueDecl        `json:"queues"`
	Secrets       []SecretDecl       `json:"secrets"`
	Functions     []FunctionDecl     `json:"functions"`
	Env           []EnvVar           `json:"env"`
	Site          *SiteDecl          `json:"site,omitempty"`
	RuntimeConfig *RuntimeConfigDecl `json:"runtimeConfig,omitempty"`
}

// SourceDecl is `source.zip`: an archive the files come from (not supported yet, as hosted).
type SourceDecl struct {
	Zip   string `json:"zip"`
	Strip string `json:"strip"`
}

// Manifest is a parsed, validated manifest with every default filled in.
type Manifest struct {
	Name        string            `json:"name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Homepage    string            `json:"homepage,omitempty"`
	Version     string            `json:"version,omitempty"`
	Source      *SourceDecl       `json:"source,omitempty"`
	Vars        []VarDecl         `json:"vars"`
	VarValues   map[string]string `json:"varValues"`
	Uses        []UsesDecl        `json:"uses"`
	Instances   []InstanceDecl    `json:"instances"`
}

// Instance returns the declared instance for key, or nil.
func (m *Manifest) Instance(key string) *InstanceDecl {
	for i := range m.Instances {
		if m.Instances[i].Key == key {
			return &m.Instances[i]
		}
	}
	return nil
}

// Use returns the `uses` entry for key, or nil.
func (m *Manifest) Use(key string) *UsesDecl {
	for i := range m.Uses {
		if m.Uses[i].Key == key {
			return &m.Uses[i]
		}
	}
	return nil
}

// Ref is one place a manifest names another instance, and the level that needs on it.
type Ref struct {
	From    string `json:"from"`
	Field   string `json:"field"`
	Target  string `json:"target"`
	Service string `json:"service"`
	Level   string `json:"level"`
}

// --- errors -----------------------------------------------------------------------------------

// MissingVarsError names the vars a deploy still needs a value for.
type MissingVarsError struct{ Missing []string }

func (e *MissingVarsError) Error() string {
	return "enter a value for " + strings.Join(e.Missing, ", ")
}

// APIError renders it the way the hosted service does: 400 VARS_REQUIRED.
func (e *MissingVarsError) APIError() *common.APIError {
	return common.NewError(http.StatusBadRequest, e.Error(), "VARS_REQUIRED").WithDetails(map[string]any{"missing": e.Missing})
}

// parseFail carries an error out of the parser's panics (the hosted parser throws).
type parseFail struct{ err error }

func bad(format string, a ...any) {
	panic(parseFail{common.NewError(http.StatusBadRequest, fmt.Sprintf(format, a...), "INVALID_MANIFEST")})
}

func badArg(msg string) {
	panic(parseFail{common.BadRequest(msg)})
}

func catch(err *error) {
	if r := recover(); r != nil {
		if f, ok := r.(parseFail); ok {
			*err = f.err
			return
		}
		panic(r)
	}
}

// --- ordered JSON ----------------------------------------------------------------------------
//
// A manifest's instances, functions and queues are applied in the order they are written, as
// hosted (a JS object keeps insertion order). Go maps do not, so the manifest is decoded into Obj,
// which keeps its keys in order. Config values, where order means nothing, become plain maps.

// Obj is a JSON object that remembers its key order.
type Obj struct {
	Keys []string
	M    map[string]any
}

func (o *Obj) get(k string) any { return o.M[k] }
func (o *Obj) has(k string) bool {
	_, ok := o.M[k]
	return ok
}

// DecodeJSON decodes one JSON document, objects as *Obj.
func DecodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("unexpected data after the document")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &Obj{M: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := o.M[k]; !dup {
					o.Keys = append(o.Keys, k)
				}
				o.M[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}

// Plain converts *Obj trees to map[string]any, the shape every service's config is held in.
func Plain(v any) any {
	switch t := v.(type) {
	case *Obj:
		m := make(map[string]any, len(t.M))
		for k, x := range t.M {
			m[k] = Plain(x)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Plain(x)
		}
		return out
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = Plain(x)
		}
		return m
	}
	return v
}

// --- small readers (manifest.ts's obj/optObj/text/level/service/count/urlRef/zipPath) ---------

func obj(v any, where string) *Obj {
	o, ok := v.(*Obj)
	if !ok {
		bad("%s must be an object", where)
	}
	return o
}

func optObj(v any, where string) *Obj {
	if v == nil {
		return &Obj{M: map[string]any{}}
	}
	return obj(v, where)
}

func text(v any, where string, max int) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		bad("%s must be a string", where)
	}
	s = strings.TrimSpace(controlRE.ReplaceAllString(s, ""))
	if utf8.RuneCountInString(s) > max {
		bad("%s is longer than %d characters", where, max)
	}
	return s
}

func level(v any, where string) string {
	s, _ := v.(string)
	if !isOneOf(s, levels) {
		bad("%s must be read, write or full", where)
	}
	return s
}

func service(v any, where string) string {
	s, isStr := v.(string)
	if !isStr || !isOneOf(s, DeployServices) {
		got := ""
		if isStr {
			got = fmt.Sprintf(" (got '%s')", s)
		}
		bad("%s must be one of %s%s", where, strings.Join(DeployServices, ", "), got)
	}
	return s
}

func count(o *Obj, max int, where string) {
	if len(o.Keys) > max {
		bad("%s has more than %d entries", where, max)
	}
}

func urlRef(v any, where string) string {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		bad("%s must be a URL", where)
	}
	t := strings.TrimSpace(s)
	if utf8.RuneCountInString(t) > Limits.URL {
		bad("%s is longer than %d characters", where, Limits.URL)
	}
	if schemeRE.MatchString(t) && !httpRE.MatchString(t) {
		bad("%s must be an https URL or a relative path", where)
	}
	if controlRE.MatchString(t) {
		bad("%s contains control characters", where)
	}
	return t
}

var leadingDotSlash = regexp.MustCompile(`^\.?/+`)

func zipPath(v any, where string) string {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		bad("%s must be a path inside the archive", where)
	}
	p := leadingDotSlash.ReplaceAllString(strings.TrimSpace(s), "")
	if strings.Contains(p, "..") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\x00\r\n") || strings.HasPrefix(p, "/") {
		bad("%s is not a valid archive path", where)
	}
	return p
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func isInteger(f float64) bool { return f == float64(int64(f)) }

// jsString is String(v) for a string, number or boolean, as the hosted parser applies it.
func jsString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

// --- naming -------------------------------------------------------------------------------------

// InstanceName is the instance name a key gets under app name `app`.
func InstanceName(app, key, svc, suffix string) string {
	if suffix != "" {
		return app + "-" + suffix
	}
	if key == svc {
		return app
	}
	return app + "-" + key
}

// ValidateAppName checks the name an installer chose.
func ValidateAppName(name string) error {
	if !AppNameRE.MatchString(name) {
		return common.BadRequest("the app name must be 3-40 lowercase letters, digits or hyphens, starting and ending with a letter or digit")
	}
	return nil
}

// forbiddenConfig are the config fields no manifest may set.
var forbiddenConfig = map[string][]string{
	"functions": {"functions", "secretsBlob"},
	"static":    {"activeDeployment"},
}

// --- references ---------------------------------------------------------------------------------

type refCtx struct {
	from  string
	kinds map[string]string
	refs  *[]Ref
}

func refTo(ctx *refCtx, raw, field, want, lvl string) {
	m := exactPH.FindStringSubmatch(raw)
	if m == nil {
		bad("%s.%s: '%s' must be a reference like ${key} to an instance this manifest declares or uses", ctx.from, field, raw)
	}
	target := m[1]
	kind, ok := ctx.kinds[target]
	if !ok {
		bad("%s.%s: ${%s} is not declared in instances or uses", ctx.from, field, target)
	}
	if kind != want {
		bad("%s.%s: ${%s} is a %s instance, not %s", ctx.from, field, target, kind, want)
	}
	*ctx.refs = append(*ctx.refs, Ref{From: ctx.from, Field: field, Target: target, Service: want, Level: lvl})
}

func slashRef(ctx *refCtx, raw any, field, want, lvl string) {
	s, ok := raw.(string)
	if !ok {
		bad("%s.%s must be a string like ${key}/name", ctx.from, field)
	}
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		bad("%s.%s must look like ${key}/name", ctx.from, field)
	}
	refTo(ctx, s[:i], field, want, lvl)
}

func colonRef(ctx *refCtx, rawKey, field string, allowed []string, lvl string) {
	i := strings.Index(rawKey, ":")
	svc := ""
	if i > 0 {
		svc = rawKey[:i]
	}
	if !isOneOf(svc, allowed) {
		bad("%s.%s: key '%s' must be \"<%s>:${key}\"", ctx.from, field, rawKey, strings.Join(allowed, "|"))
	}
	refTo(ctx, rawKey[i+1:], field, svc, lvl)
}

// hasPlaceholder reports whether `${` appears in any string or key (JSON.stringify(v) /\$\{/).
func hasPlaceholder(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, "${")
	case *Obj:
		for _, k := range t.Keys {
			if strings.Contains(k, "${") || hasPlaceholder(t.M[k]) {
				return true
			}
		}
	case map[string]any:
		for k, x := range t {
			if strings.Contains(k, "${") || hasPlaceholder(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if hasPlaceholder(x) {
				return true
			}
		}
	}
	return false
}

func noStray(v any, where string) {
	if hasPlaceholder(v) {
		bad("%s: ${…} references are only allowed in reference fields (grants, access, live, triggers, queue targets)", where)
	}
}

// without returns a shallow copy of o with keys set to nil (`{...o, key: null}`).
func without(o *Obj, keys ...string) *Obj {
	out := &Obj{Keys: append([]string{}, o.Keys...), M: make(map[string]any, len(o.M))}
	for k, v := range o.M {
		out.M[k] = v
	}
	for _, k := range keys {
		if !out.has(k) {
			out.Keys = append(out.Keys, k)
		}
		out.M[k] = nil
	}
	return out
}

func originList(ctx *refCtx, list any, where string) any {
	arr, ok := list.([]any)
	if !ok {
		return list
	}
	out := []any{}
	for _, item := range arr {
		s, isStr := item.(string)
		var m []string
		if isStr {
			m = originPH.FindStringSubmatch(s)
		}
		if m == nil {
			out = append(out, item)
			continue
		}
		kind, ok := ctx.kinds[m[1]]
		if !ok {
			bad("%s.%s: ${%s:origin} is not declared in instances or uses", ctx.from, where, m[1])
		}
		if !IsHosted(kind) {
			bad("%s.%s: a %s instance has no origin", ctx.from, where, kind)
		}
	}
	return out
}

func configRefs(ctx *refCtx, svc string, config *Obj) {
	rest := without(config)
	if svc == "auth" {
		if s, ok := config.get("settings").(*Obj); ok {
			ns := without(s)
			ns.M["allowedOrigins"] = originList(ctx, s.get("allowedOrigins"), "config.settings.allowedOrigins")
			rest.M["settings"] = ns
		}
	}
	if svc == "functions" {
		rest.M["corsOrigins"] = originList(ctx, config.get("corsOrigins"), "config.corsOrigins")
	}
	if svc == "auth" && config.get("access") != nil {
		access := obj(config.get("access"), ctx.from+".config.access")
		for _, k := range access.Keys {
			entry := obj(access.M[k], fmt.Sprintf("%s.config.access['%s']", ctx.from, k))
			lvl := level(entry.get("level"), fmt.Sprintf("%s.config.access['%s'].level", ctx.from, k))
			colonRef(ctx, k, "config.access", []string{"datastore", "channel"}, lvl)
			noStray(entry, fmt.Sprintf("%s.config.access['%s']", ctx.from, k))
		}
		delete(rest.M, "access")
	}
	if (svc == "datastore" || svc == "blob") && config.get("triggers") != nil {
		list, ok := config.get("triggers").([]any)
		if !ok {
			bad("%s.config.triggers must be a list", ctx.from)
		}
		for i, t := range list {
			o := obj(t, fmt.Sprintf("%s.config.triggers[%d]", ctx.from, i))
			slashRef(ctx, o.get("queue"), fmt.Sprintf("config.triggers[%d].queue", i), "tasks", "write")
			noStray(without(o, "queue"), fmt.Sprintf("%s.config.triggers[%d]", ctx.from, i))
		}
		delete(rest.M, "triggers")
	}
	if svc == "datastore" && config.get("live") != nil {
		live := obj(config.get("live"), ctx.from+".config.live")
		ci, ok := live.get("channelInstance").(string)
		if !ok {
			bad("%s.config.live.channelInstance must be a reference like ${key}", ctx.from)
		}
		refTo(ctx, ci, "config.live.channelInstance", "channel", "write")
		noStray(without(live, "channelInstance"), ctx.from+".config.live")
		delete(rest.M, "live")
	}
	noStray(rest, ctx.from+".config")
}

func queueRefs(ctx *refCtx, name string, q *Obj) {
	if q.get("target") != nil {
		t := obj(q.get("target"), fmt.Sprintf("%s.queues.%s.target", ctx.from, name))
		switch {
		case t.has("function"):
			slashRef(ctx, t.get("function"), fmt.Sprintf("queues.%s.target.function", name), "functions", "read")
		case t.has("channel"):
			slashRef(ctx, t.get("channel"), fmt.Sprintf("queues.%s.target.channel", name), "channel", "write")
		default:
			noStray(t, fmt.Sprintf("%s.queues.%s.target", ctx.from, name))
		}
	}
	noStray(without(q, "target"), fmt.Sprintf("%s.queues.%s", ctx.from, name))
}

// --- variables ----------------------------------------------------------------------------------

// ReadVarDecls reads and validates the `vars` block of a raw (decoded) manifest.
func ReadVarDecls(raw any) (out []VarDecl, err error) {
	defer catch(&err)
	return readVarDecls(raw), nil
}

func readVarDecls(raw any) []VarDecl {
	o, ok := raw.(*Obj)
	if !ok {
		o = &Obj{M: map[string]any{}}
	}
	v := optObj(o.get("vars"), "manifest.vars")
	count(v, MaxVars, "manifest.vars")
	out := []VarDecl{}
	for _, name := range v.Keys {
		if !varNameRE.MatchString(name) {
			bad("vars: '%s' must be letters, digits or _, starting with a letter", name)
		}
		e := optObj(v.M[name], "vars."+name)
		decl := VarDecl{Name: name}
		if p := e.get("pattern"); p != nil {
			ps, ok := p.(string)
			if !ok || len(ps) > 200 {
				bad("vars.%s.pattern must be a regular expression", name)
			}
			re, err := regexp.Compile(`^(?:` + ps + `)$`)
			if err != nil {
				bad("vars.%s.pattern is not a valid regular expression", name)
			}
			decl.Pattern, decl.re = ps, re
		}
		decl.Description = text(e.get("description"), fmt.Sprintf("vars.%s.description", name), Limits.Text)
		decl.Required = e.get("required") != false
		if d := e.get("default"); d != nil {
			s, ok := jsString(d)
			if !ok {
				bad("vars.%s.default must be a string", name)
			}
			s = checkVar(name, s, decl)
			decl.Default = &s
		}
		out = append(out, decl)
	}
	return out
}

func checkVar(name, value string, decl VarDecl) string {
	if !varValueRE.MatchString(value) {
		badArg(fmt.Sprintf(`%s: up to 200 characters, without $ { } " \ or control characters`, name))
	}
	if decl.re != nil && !decl.re.MatchString(value) {
		badArg(fmt.Sprintf("%s: '%s' does not match %s", name, value, decl.Pattern))
	}
	return value
}

func resolveVarValues(decls []VarDecl, sources []map[string]string) map[string]string {
	values := map[string]string{}
	var missing []string
	for _, d := range decls {
		given, found := "", false
		for _, s := range sources {
			if v, ok := s[d.Name]; ok && v != "" {
				given, found = v, true
				break
			}
		}
		switch {
		case found:
			values[d.Name] = checkVar(d.Name, given, d)
		case d.Default != nil:
			values[d.Name] = *d.Default
		case d.Required:
			missing = append(missing, d.Name)
		default:
			values[d.Name] = ""
		}
	}
	if len(missing) > 0 {
		panic(parseFail{&MissingVarsError{Missing: missing}})
	}
	return values
}

func substituteVars(raw any, values map[string]string) any {
	sub := func(s string) string {
		return varPHRE.ReplaceAllStringFunc(s, func(m string) string {
			n := varPHRE.FindStringSubmatch(m)[1]
			v, ok := values[n]
			if !ok {
				bad("{{%s}} is used but not declared in vars", n)
			}
			return v
		})
	}
	var walk func(v any, top bool) any
	walk = func(v any, top bool) any {
		switch t := v.(type) {
		case string:
			return sub(t)
		case []any:
			out := make([]any, len(t))
			for i, x := range t {
				out[i] = walk(x, false)
			}
			return out
		case *Obj:
			out := &Obj{M: map[string]any{}}
			for _, k := range t.Keys {
				nk, nv := k, t.M[k]
				if !(top && k == "vars") {
					nk, nv = sub(k), walk(nv, false)
				}
				if _, dup := out.M[nk]; !dup {
					out.Keys = append(out.Keys, nk)
				}
				out.M[nk] = nv
			}
			return out
		}
		return v
	}
	return walk(raw, true)
}

func parseEnv(v any, where string, kinds map[string]string) []EnvVar {
	o := optObj(v, where)
	count(o, Limits.SecretsPerInstance, where)
	out := []EnvVar{}
	for _, name := range o.Keys {
		if !envNameRE.MatchString(name) {
			bad("%s: '%s' must be A-Z, 0-9 and _, starting with a letter", where, name)
		}
		if reservedEnv[name] {
			bad("%s: '%s' is reserved by the runtime", where, name)
		}
		s, ok := jsString(o.M[name])
		if !ok {
			bad("%s.%s must be a string", where, name)
		}
		if utf8.RuneCountInString(s) > 4096 {
			bad("%s.%s is longer than 4096 characters", where, name)
		}
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			kind, ok := kinds[m[1]]
			if !ok {
				bad("%s.%s: ${%s} is not declared in instances or uses", where, name, m[1])
			}
			if m[2] != "" && !IsHosted(kind) {
				bad("%s.%s: a %s instance has no %s", where, name, kind, m[2])
			}
		}
		out = append(out, EnvVar{Name: name, Value: s})
	}
	return out
}

// --- parse ------------------------------------------------------------------------------------

func parseModule(v any, where string) ModuleRef {
	o := obj(v, where)
	if o.get("zip") != nil {
		return ModuleRef{Zip: zipPath(o.get("zip"), where+".zip")}
	}
	u := urlRef(o.get("url"), where+".url")
	sha, _ := o.get("sha256").(string)
	sha = strings.ToLower(sha)
	if !hex64.MatchString(sha) {
		bad("%s.sha256 must be the hex sha256 of the module's bytes", where)
	}
	return ModuleRef{URL: u, SHA256: sha}
}

func parseSite(v any, where string) *SiteDecl {
	o := obj(v, where)
	if o.get("zip") != nil {
		return &SiteDecl{Zip: zipPath(o.get("zip"), where+".zip")}
	}
	baseRaw := o.get("base")
	if baseRaw == nil {
		baseRaw = "./"
	}
	base := urlRef(baseRaw, where+".base")
	files := obj(o.get("files"), where+".files")
	n := len(files.Keys)
	if n == 0 {
		bad("%s.files must list at least one file", where)
	}
	if n > Limits.StaticFiles {
		bad("%s.files has %d files; the limit is %d", where, n, Limits.StaticFiles)
	}
	out := map[string]SiteFile{}
	for _, raw := range files.Keys {
		p := raw
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if strings.Contains(p, "..") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\x00\r\n") || strings.HasSuffix(p, "/") {
			short := raw
			if len(short) > 80 {
				short = short[:80]
			}
			bad("%s.files: '%s' is not a valid site path", where, short)
		}
		e := obj(files.M[raw], fmt.Sprintf("%s.files['%s']", where, p))
		hash, _ := e.get("hash").(string)
		hash = strings.ToLower(hash)
		if !hex64.MatchString(hash) {
			bad("%s.files['%s'].hash must be a hex sha256", where, p)
		}
		size, ok := number(e.get("size"))
		if !ok || !isInteger(size) || size < 0 {
			bad("%s.files['%s'].size must be a byte count", where, p)
		}
		f := SiteFile{Hash: hash, Size: int64(size)}
		if ct, ok := e.get("contentType").(string); ok {
			f.ContentType = ct
		}
		out[p] = f
	}
	return &SiteDecl{Base: base, Files: out}
}

func parseIndexes(v any, where string) []IndexDecl {
	if v == nil {
		return []IndexDecl{}
	}
	list, ok := v.([]any)
	if !ok {
		bad("%s must be a list", where)
	}
	if len(list) > Limits.IndexesPerInstance {
		bad("%s has more than %d indexes", where, Limits.IndexesPerInstance)
	}
	out := []IndexDecl{}
	for i, raw := range list {
		o := obj(raw, fmt.Sprintf("%s[%d]", where, i))
		coll := text(o.get("collection"), fmt.Sprintf("%s[%d].collection", where, i), Limits.Text)
		if coll == "" {
			bad("%s[%d].collection is required", where, i)
		}
		fl, ok := o.get("fields").([]any)
		var fields []string
		for _, f := range fl {
			s, isStr := f.(string)
			if !isStr {
				ok = false
				break
			}
			fields = append(fields, s)
		}
		if !ok || len(fl) == 0 {
			bad("%s[%d].fields must be a list of field specs", where, i)
		}
		out = append(out, IndexDecl{
			Namespace:  text(o.get("namespace"), fmt.Sprintf("%s[%d].namespace", where, i), Limits.Text),
			Collection: coll, Fields: fields, Unique: o.get("unique") == true,
		})
	}
	return out
}

func parseSecrets(v any, where string) []SecretDecl {
	o := optObj(v, where)
	count(o, Limits.SecretsPerInstance, where)
	out := []SecretDecl{}
	for _, name := range o.Keys {
		if !envNameRE.MatchString(name) {
			bad("%s: secret name '%s' must be A-Z, 0-9 and _, starting with a letter", where, name)
		}
		if reservedEnv[name] {
			bad("%s: '%s' is reserved by the runtime", where, name)
		}
		e := optObj(o.M[name], where+"."+name)
		egress := e.get("egress") == true
		var hosts []string
		if h := e.get("hosts"); h != nil {
			list, ok := h.([]any)
			if !ok {
				bad("%s.%s.hosts must be a list of hostnames", where, name)
			}
			hosts = []string{}
			for _, x := range list {
				s, ok := x.(string)
				if !ok {
					bad("%s.%s.hosts must be a list of hostnames", where, name)
				}
				hosts = append(hosts, s)
			}
		}
		if egress && len(hosts) == 0 {
			bad("%s.%s: an egress secret must name the hosts it may be sent to", where, name)
		}
		out = append(out, SecretDecl{
			Name: name, Description: text(e.get("description"), where+"."+name+".description", Limits.Text),
			Required: e.get("required") != false, Egress: egress, Hosts: hosts,
		})
	}
	return out
}

// Parse validates a decoded manifest (see DecodeJSON). varSources are searched in order of
// precedence for each var's value. Returns the manifest and every reference it makes. A
// *MissingVarsError means the manifest is valid so far but needs var values.
func Parse(raw any, varSources []map[string]string) (m *Manifest, refs []Ref, err error) {
	defer catch(&err)
	m, refs = parse(raw, varSources)
	return m, refs, nil
}

func parse(raw any, varSources []map[string]string) (*Manifest, []Ref) {
	obj(raw, "manifest")
	vars := readVarDecls(raw)
	varValues := resolveVarValues(vars, varSources)
	o := obj(substituteVars(raw, varValues), "manifest")
	if v, ok := number(o.get("altengine")); !ok || v != FormatVersion {
		bad("manifest.altengine must be %d (the manifest format version)", FormatVersion)
	}
	name, _ := o.get("name").(string)
	if !AppNameRE.MatchString(name) {
		suffix := ""
		if len(vars) > 0 {
			suffix = " (after its {{vars}} are filled in)"
		}
		bad("manifest.name must be 3-40 lowercase letters, digits or hyphens%s", suffix)
	}

	var source *SourceDecl
	if o.get("source") != nil {
		s := obj(o.get("source"), "manifest.source")
		strip := s.get("strip")
		if strip == nil {
			strip = "auto"
		}
		if strip != "auto" && strip != "none" {
			bad("manifest.source.strip must be auto or none")
		}
		source = &SourceDecl{Zip: urlRef(s.get("zip"), "manifest.source.zip"), Strip: strip.(string)}
	}

	usesRaw := optObj(o.get("uses"), "manifest.uses")
	instRaw := obj(o.get("instances"), "manifest.instances")
	count(usesRaw, Limits.Uses, "manifest.uses")
	count(instRaw, Limits.Instances, "manifest.instances")
	if len(instRaw.Keys) == 0 {
		bad("manifest.instances must declare at least one instance")
	}

	kinds := map[string]string{}
	uses := []UsesDecl{}
	for _, key := range usesRaw.Keys {
		if !keyRE.MatchString(key) {
			bad("uses key '%s' must be lowercase letters, digits or hyphens, starting with a letter", key)
		}
		u := obj(usesRaw.M[key], "uses."+key)
		svc := service(u.get("service"), "uses."+key+".service")
		uses = append(uses, UsesDecl{
			Key: key, Service: svc, Level: level(u.get("level"), "uses."+key+".level"),
			Why: text(u.get("why"), "uses."+key+".why", Limits.Text), Suggest: text(u.get("suggest"), "uses."+key+".suggest", 100),
		})
		kinds[key] = svc
	}
	for _, key := range instRaw.Keys {
		if !keyRE.MatchString(key) {
			bad("instance key '%s' must be lowercase letters, digits or hyphens, starting with a letter", key)
		}
		if _, dup := kinds[key]; dup {
			bad("'%s' is declared in both instances and uses", key)
		}
		svc := service(obj(instRaw.M[key], "instances."+key).get("service"), "instances."+key+".service")
		if isOneOf(key, DeployServices) && key != svc {
			bad("instances.%s is named after the %s service but declares %s", key, key, svc)
		}
		kinds[key] = svc
	}

	refs := []Ref{}
	instances := []InstanceDecl{}
	tails := map[string]string{}
	for _, key := range instRaw.Keys {
		d := obj(instRaw.M[key], "instances."+key)
		svc := kinds[key]
		ctx := &refCtx{from: key, kinds: kinds, refs: &refs}
		configs := map[string]map[string]any{}
		for _, field := range []string{"config", "initialConfig"} {
			c := optObj(d.get(field), fmt.Sprintf("instances.%s.%s", key, field))
			if c.has("region") {
				bad("instances.%s.%s.region: set region on the instance (instances.%s.region), not in %s", key, field, key, field)
			}
			for _, f := range forbiddenConfig[svc] {
				if c.has(f) {
					bad("instances.%s.%s.%s cannot be set by a manifest", key, field, f)
				}
			}
			configRefs(ctx, svc, c)
			configs[field] = Plain(c).(map[string]any)
		}

		decl := InstanceDecl{Key: key, Service: svc, Config: configs["config"], InitialConfig: configs["initialConfig"],
			Indexes: []IndexDecl{}, Queues: []QueueDecl{}, Secrets: []SecretDecl{}, Functions: []FunctionDecl{}, Env: []EnvVar{}}
		if sfx := d.get("suffix"); sfx != nil {
			s, ok := sfx.(string)
			if !ok || !keyRE.MatchString(s) {
				bad("instances.%s.suffix must be lowercase letters, digits or hyphens, starting with a letter", key)
			}
			decl.Suffix = s
		}
		tailPart := decl.Suffix
		if tailPart == "" && key != svc {
			tailPart = key
		}
		tail := svc + ":" + tailPart
		if other, dup := tails[tail]; dup {
			bad("instances.%s would have the same name as instances.%s", key, other)
		}
		tails[tail] = key
		if d.get("region") != nil {
			decl.Region = text(d.get("region"), "instances."+key+".region", 40)
		}

		only := func(field, allowed string) {
			if d.get(field) != nil && svc != allowed {
				bad("instances.%s.%s is only for %s instances", key, field, allowed)
			}
		}
		only("indexes", "datastore")
		only("queues", "tasks")
		only("secrets", "functions")
		only("functions", "functions")
		only("env", "functions")
		only("site", "static")
		only("runtimeConfig", "static")

		decl.Indexes = parseIndexes(d.get("indexes"), "instances."+key+".indexes")

		queues := optObj(d.get("queues"), "instances."+key+".queues")
		count(queues, Limits.QueuesPerInstance, "instances."+key+".queues")
		for _, qn := range queues.Keys {
			qo := obj(queues.M[qn], fmt.Sprintf("instances.%s.queues.%s", key, qn))
			queueRefs(ctx, qn, qo)
			decl.Queues = append(decl.Queues, QueueDecl{Name: qn, Settings: Plain(qo).(map[string]any)})
		}

		decl.Secrets = parseSecrets(d.get("secrets"), "instances."+key+".secrets")
		decl.Env = parseEnv(d.get("env"), "instances."+key+".env", kinds)
		for _, e := range decl.Env {
			for _, s := range decl.Secrets {
				if s.Name == e.Name {
					bad("instances.%s: '%s' is both a secret and an env value", key, e.Name)
				}
			}
		}
		if len(decl.Env)+len(decl.Secrets) > Limits.SecretsPerInstance {
			bad("instances.%s: at most %d secrets and env values together", key, Limits.SecretsPerInstance)
		}

		fns := optObj(d.get("functions"), "instances."+key+".functions")
		count(fns, Limits.FunctionsPerInstance, "instances."+key+".functions")
		for _, fn := range fns.Keys {
			w := fmt.Sprintf("instances.%s.functions.%s", key, fn)
			fo := obj(fns.M[fn], w)
			grants := map[string]string{}
			g := optObj(fo.get("grants"), w+".grants")
			for _, gk := range g.Keys {
				lvl := level(g.M[gk], fmt.Sprintf("%s.grants['%s']", w, gk))
				colonRef(ctx, gk, "functions."+fn+".grants", DeployServices, lvl)
				grants[gk] = lvl
			}
			var schedules []string
			if s := fo.get("schedules"); s != nil {
				list, ok := s.([]any)
				if !ok {
					bad("%s.schedules must be a list of cron expressions", w)
				}
				schedules = []string{}
				for _, x := range list {
					str, ok := x.(string)
					if !ok {
						bad("%s.schedules must be a list of cron expressions", w)
					}
					schedules = append(schedules, str)
				}
			}
			module := parseModule(fo.get("module"), w+".module")
			num := func(v any, where string) *float64 {
				if v == nil {
					return nil
				}
				f, ok := number(v)
				if !ok {
					bad("%s must be a number", where)
				}
				return &f
			}
			decl.Functions = append(decl.Functions, FunctionDecl{
				Name: fn, Module: module, Grants: grants, Schedules: schedules,
				CPUMs: num(fo.get("cpuMs"), w+".cpuMs"), SubRequests: num(fo.get("subRequests"), w+".subRequests"),
			})
		}

		if d.get("site") != nil {
			decl.Site = parseSite(d.get("site"), "instances."+key+".site")
		}
		if d.get("runtimeConfig") != nil {
			rc := obj(d.get("runtimeConfig"), "instances."+key+".runtimeConfig")
			p := "/altengine.json"
			if s, ok := rc.get("path").(string); ok && s != "" {
				p = s
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
			}
			if strings.Contains(p, "..") || strings.HasSuffix(p, "/") {
				bad("instances.%s.runtimeConfig.path is not a valid site path", key)
			}
			list, ok := rc.get("expose").([]any)
			expose := []string{}
			for _, x := range list {
				s, isStr := x.(string)
				if _, known := kinds[s]; !isStr || !known {
					ok = false
					break
				}
				expose = append(expose, s)
			}
			if !ok {
				bad("instances.%s.runtimeConfig.expose must list keys from instances or uses", key)
			}
			decl.RuntimeConfig = &RuntimeConfigDecl{Path: p, Expose: expose}
		}

		usesZip := decl.Site != nil && decl.Site.Zip != ""
		for _, f := range decl.Functions {
			usesZip = usesZip || f.Module.Zip != ""
		}
		if usesZip && source == nil {
			bad("instances.%s names files inside an archive, but manifest.source.zip is not set", key)
		}
		instances = append(instances, decl)
	}

	m := &Manifest{
		Name:        name,
		Title:       text(o.get("title"), "manifest.title", 80),
		Description: text(o.get("description"), "manifest.description", 500),
		Version:     text(o.get("version"), "manifest.version", 40),
		Source:      source,
		Vars:        vars,
		VarValues:   varValues,
		Uses:        uses,
		Instances:   instances,
	}
	if o.get("homepage") != nil {
		m.Homepage = urlRef(o.get("homepage"), "manifest.homepage")
	}
	return m, refs
}

// --- resolving and merging ------------------------------------------------------------------------

// Link is a hosted instance's public address: what `${key:url}` and `${key:origin}` become.
type Link struct {
	URL    string `json:"url"`
	Origin string `json:"origin"`
}

// ResolveRefs replaces every `${key}` (and `${key:url}` / `${key:origin}`) in a value, keys
// included. Only called on fields Parse has confirmed hold references in reference positions.
func ResolveRefs(value any, names map[string]string, links map[string]Link) (out any, err error) {
	defer catch(&err)
	return resolveRefs(value, names, links), nil
}

func resolveRefs(value any, names map[string]string, links map[string]Link) any {
	sub := func(s string) string {
		return placeholder.ReplaceAllStringFunc(s, func(m string) string {
			g := placeholder.FindStringSubmatch(m)
			k, prop := g[1], g[2]
			var v string
			var ok bool
			switch prop {
			case "url":
				l, has := links[k]
				v, ok = l.URL, has && l.URL != ""
			case "origin":
				l, has := links[k]
				v, ok = l.Origin, has && l.Origin != ""
			default:
				v, ok = names[k]
			}
			if !ok {
				p := ""
				if prop != "" {
					p = ":" + prop
				}
				panic(parseFail{common.NewError(500, fmt.Sprintf("unresolved reference ${%s%s}", k, p), "INTERNAL")})
			}
			return v
		})
	}
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			return sub(t)
		case []any:
			out := make([]any, len(t))
			for i, x := range t {
				out[i] = walk(x)
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, x := range t {
				out[sub(k)] = walk(x)
			}
			return out
		case map[string]string:
			out := make(map[string]string, len(t))
			for k, x := range t {
				out[sub(k)] = sub(x)
			}
			return out
		}
		return v
	}
	return walk(value)
}

// unionLists are the origin lists a deploy adds to rather than replaces.
var unionLists = map[string][][]string{
	"auth":      {{"settings", "allowedOrigins"}},
	"functions": {{"corsOrigins"}},
}

func isPlain(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ExpandOriginLists replaces each `${key:origin}` item of the two origin lists with every origin
// that instance is served at. Other items are untouched.
func ExpandOriginLists(service string, config map[string]any, origins map[string][]string) map[string]any {
	expand := func(list any) any {
		arr, ok := list.([]any)
		if !ok {
			return list
		}
		var items []any
		for _, item := range arr {
			if s, isStr := item.(string); isStr {
				if m := originPH.FindStringSubmatch(s); m != nil {
					for _, o := range origins[m[1]] {
						items = append(items, o)
					}
					continue
				}
			}
			items = append(items, item)
		}
		return dedupe(items)
	}
	if service == "auth" {
		if s, ok := config["settings"].(map[string]any); ok {
			if _, isList := s["allowedOrigins"].([]any); isList {
				out := copyMap(config)
				ns := copyMap(s)
				ns["allowedOrigins"] = expand(s["allowedOrigins"])
				out["settings"] = ns
				return out
			}
		}
	}
	if service == "functions" {
		if _, isList := config["corsOrigins"].([]any); isList {
			out := copyMap(config)
			out["corsOrigins"] = expand(config["corsOrigins"])
			return out
		}
	}
	return config
}

func dedupe(items []any) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, x := range items {
		b, _ := json.Marshal(x)
		if seen[string(b)] {
			continue
		}
		seen[string(b)] = true
		out = append(out, x)
	}
	return out
}

// MergeConfig lands a manifest's config on an instance's: a top-level key is set; a key whose value
// is an object is merged one level; the auth `settings.allowedOrigins` and functions `corsOrigins`
// lists are added to rather than replaced.
func MergeConfig(service string, current, changes map[string]any) map[string]any {
	out := copyMap(current)
	for k, v := range changes {
		cur := current[k]
		switch {
		case isPlain(v) && isPlain(cur):
			m := copyMap(cur.(map[string]any))
			for kk, vv := range v.(map[string]any) {
				m[kk] = vv
			}
			out[k] = m
		case isPlain(v):
			out[k] = copyMap(v.(map[string]any))
		default:
			out[k] = v
		}
	}
	for _, path := range unionLists[service] {
		get := func(o any) any {
			for _, p := range path {
				m, ok := o.(map[string]any)
				if !ok {
					return nil
				}
				o = m[p]
			}
			return o
		}
		before, ok1 := get(current).([]any)
		given, ok2 := get(changes).([]any)
		if !ok1 || !ok2 {
			continue
		}
		union := dedupe(append(append([]any{}, before...), given...))
		node := out
		for _, p := range path[:len(path)-1] {
			node = node[p].(map[string]any)
		}
		node[path[len(path)-1]] = union
	}
	return out
}

// sortedKeys returns a map's keys in order, for deterministic output.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
