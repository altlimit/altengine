// Package functions emulates the altengine functions service: sandboxed JavaScript,
// deployed as one bundled ES module and invoked over HTTP, with capability-scoped access
// to the other services and nothing else.
//
// # WHAT IS FAITHFUL, AND WHAT IS NOT
//
// Faithful: the deploy API, the capability model (a function reaches only the services
// and instances its grants name), the two secret exposures, the outbound allowlist and
// its SSRF refusals, CORS, and the shape of `env` and of `export default { fetch }`.
// Code you deploy here runs there.
//
// NOT faithful, and deliberately: this is not a security sandbox. Hosted, tenant code runs
// in a hard sandbox with no ambient capabilities at all. Here it runs in an embedded
// JS interpreter inside the emulator process, because the point of running locally is to
// see your own code work — not to defend the machine against it. Never point this at code
// you would not run yourself, and never expose the emulator beyond localhost.
//
// The interpreter is ES2020-era and single-threaded. Anything relying on a real Workers
// runtime detail (streaming bodies, WebSocket upgrades, precise timer semantics) is worth
// checking against a real deploy before trusting it.
package functions

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Limits mirror the hosted service so a bundle that is refused there is refused here.
const (
	MaxCodeBytes  = 1 << 20 // 1 MiB, same as hosted
	// The biggest REQUEST body a deployed function may receive. Nothing to do with the
	// code limit above, which it used to share — a function that accepts an uploaded file
	// inline is not a function with a big bundle, and reading a request through the code
	// limit truncated it at 1 MiB.
	MaxRequestBytes = 10 << 20 // 10 MiB
	// Stored versions kept per function, from the instance's `keepVersions` setting. The active
	// version is always kept on top of it, however old.
	DefaultKeepVersions = 10
	MaxKeepVersions     = 50
	MaxSecrets    = 32
	MaxSecretSize = 4096
	DefaultCPUMs  = 50
	DefaultSubReq = 50
)

var (
	fnNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	// A hostname, optionally with a leading "*." for subdomains. No scheme, no path.
	hostRe = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// Function is one deployed entry point.
type Function struct {
	Name          string            `json:"name"`
	Grants        map[string]string `json:"grants"`
	ActiveVersion int               `json:"activeVersion"`
	CPUMs         int               `json:"cpuMs"`
	SubRequests   int               `json:"subRequests"`
	// Five-field UTC cron expressions. A LIST because one expression cannot express every
	// schedule: hour and day-of-week are ANDed within an expression, so "09:00 weekdays
	// AND 12:00 Saturday" is genuinely two.
	Schedules []string `json:"schedules,omitempty"`
}

// Version is one immutable deploy of one function.
type Version struct {
	Version   int    `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
}

// Secret carries its exposure. Hosted, an `egress` secret exists ONLY inside the outbound
// proxy and never enters the sandbox; the same split is enforced here so a function that
// works locally is not relying on a value it will not be given in production.
type Secret struct {
	Value  string `json:"value"`
	Egress bool   `json:"egress"`
}

// Config is the per-instance settings blob, mirroring the hosted `instance_config` rows.
type Config struct {
	Functions    []*Function          `json:"functions"`
	AllowedHosts []string             `json:"allowedHosts"`
	CORSOrigins  []string             `json:"corsOrigins"`
	Secrets      map[string]Secret    `json:"secrets"`
	Versions     map[string][]Version `json:"versions"` // fn name -> history, newest last
}

// Store holds deployed code and per-instance config. Code lives on disk when the emulator
// was given a data dir, so a restart does not lose a deploy; in memory otherwise.
type Store struct {
	mu      sync.RWMutex
	dataDir string
	configs map[string]*Config           // instance id -> config
	code    map[string]map[string]string // instance id -> "fn@version" -> source
}

// NewStore builds the store, loading anything previously persisted.
func NewStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir, configs: map[string]*Config{}, code: map[string]map[string]string{}}
	if dataDir != "" {
		_ = os.MkdirAll(s.dir(), 0o755)
		s.load()
	}
	return s
}

func (s *Store) dir() string { return filepath.Join(s.dataDir, "functions") }

func (s *Store) load() {
	b, err := os.ReadFile(filepath.Join(s.dir(), "state.json"))
	if err != nil {
		return
	}
	var st struct {
		Configs map[string]*Config           `json:"configs"`
		Code    map[string]map[string]string `json:"code"`
	}
	if json.Unmarshal(b, &st) != nil {
		return
	}
	if st.Configs != nil {
		s.configs = st.Configs
	}
	if st.Code != nil {
		s.code = st.Code
	}
}

// save persists under the caller's lock. Best-effort: a local emulator losing a deploy on
// a disk error is an annoyance, not a reason to fail the request that triggered it.
func (s *Store) save() {
	if s.dataDir == "" {
		return
	}
	b, err := json.MarshalIndent(map[string]any{"configs": s.configs, "code": s.code}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir(), "state.json"), b, 0o600)
}

// Config returns a COPY of an instance's config, so callers cannot mutate shared state.
func (s *Store) Config(instanceID string) *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.configs[instanceID])
}

func cloneConfig(c *Config) *Config {
	out := &Config{Functions: []*Function{}, AllowedHosts: []string{}, CORSOrigins: []string{},
		Secrets: map[string]Secret{}, Versions: map[string][]Version{}}
	if c == nil {
		return out
	}
	for _, f := range c.Functions {
		cp := *f
		cp.Grants = map[string]string{}
		for k, v := range f.Grants {
			cp.Grants[k] = v
		}
		out.Functions = append(out.Functions, &cp)
	}
	out.AllowedHosts = append(out.AllowedHosts, c.AllowedHosts...)
	out.CORSOrigins = append(out.CORSOrigins, c.CORSOrigins...)
	for k, v := range c.Secrets {
		out.Secrets[k] = v
	}
	for k, v := range c.Versions {
		out.Versions[k] = append([]Version{}, v...)
	}
	return out
}

// Find returns the named function from a config, or nil.
func (c *Config) Find(name string) *Function {
	for _, f := range c.Functions {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// EnvSecrets are the values the sandbox may read as env.NAME.
func (c *Config) EnvSecrets() map[string]string { return c.pick(false) }

// EgressSecrets are the values only the outbound proxy may see, for {{NAME}} expansion.
func (c *Config) EgressSecrets() map[string]string { return c.pick(true) }

func (c *Config) pick(egress bool) map[string]string {
	out := map[string]string{}
	for k, v := range c.Secrets {
		if v.Egress == egress {
			out[k] = v.Value
		}
	}
	return out
}

// DeployRequest is the body of POST /v1/functions/{instance}/deploy.
type DeployRequest struct {
	Name        string            `json:"name"`
	Code        string            `json:"code"`
	Grants      map[string]string `json:"grants"`
	CPUMs       int               `json:"cpuMs"`
	SubRequests int               `json:"subRequests"`
	// json.RawMessage, not []string: an ABSENT field must inherit the deployed schedules
	// while an explicit null clears them, and both decode to a nil slice. Only the raw
	// bytes tell the two apart. Accepts a list or one newline-separated string.
	Schedules json.RawMessage `json:"schedules"`
	// Alias, from before one expression turned out not to be enough.
	Schedule json.RawMessage `json:"schedule"`
	Activate *bool           `json:"activate"`
}

// Deploy stores a new version and (by default) activates it.
//
// Mirrors the hosted pipeline including the parts that are easy to get wrong: an omitted
// field INHERITS from the existing function rather than resetting it, so redeploying code
// does not silently drop the grants the function needs.
//
// keep is the instance's retention setting (see KeepVersions); history beyond it is pruned after
// the deploy, never including the version the function serves.
func (s *Store) Deploy(instanceID string, req DeployRequest, keep int) (map[string]any, error) {
	if !fnNameRe.MatchString(req.Name) {
		return nil, common.BadRequest("name must be 1-63 chars of [a-z0-9_-], starting alphanumeric")
	}
	if req.Code == "" {
		return nil, common.BadRequest("code is required")
	}
	if len(req.Code) > MaxCodeBytes {
		return nil, common.BadRequest(fmt.Sprintf("code exceeds %d bytes", MaxCodeBytes))
	}
	for k, v := range req.Grants {
		if v != "read" && v != "write" && v != "full" {
			return nil, common.BadRequest(fmt.Sprintf("grant '%s' must be read, write or full", k))
		}
		if strings.TrimSpace(strings.SplitN(k, ":", 2)[0]) == "" {
			return nil, common.BadRequest("grant key must name a service")
		}
	}
	// Validated BEFORE anything is written, so a bad expression never stores a version.
	schedules, err := decodeSchedules(req.Schedules, req.Schedule)
	if err != nil {
		return nil, common.BadRequest(err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		cfg = &Config{Secrets: map[string]Secret{}, Versions: map[string][]Version{}}
		s.configs[instanceID] = cfg
	}
	if cfg.Versions == nil {
		cfg.Versions = map[string][]Version{}
	}

	existing := cfg.Find(req.Name)
	next := 1
	if hist := cfg.Versions[req.Name]; len(hist) > 0 {
		next = hist[len(hist)-1].Version + 1
	}
	v := Version{Version: next, SizeBytes: len(req.Code), SHA256: sha256Hex(req.Code), CreatedAt: time.Now().UnixMilli()}
	cfg.Versions[req.Name] = append(cfg.Versions[req.Name], v)

	if s.code[instanceID] == nil {
		s.code[instanceID] = map[string]string{}
	}
	s.code[instanceID][codeKey(req.Name, next)] = req.Code

	activate := req.Activate == nil || *req.Activate
	fn := existing
	if fn == nil {
		fn = &Function{Name: req.Name, Grants: map[string]string{}, CPUMs: DefaultCPUMs, SubRequests: DefaultSubReq}
		cfg.Functions = append(cfg.Functions, fn)
	}
	// Omitted fields INHERIT. Resetting them here is the bug that silently strips a
	// function's grants on a code-only redeploy.
	if req.Grants != nil {
		fn.Grants = req.Grants
	}
	if req.CPUMs > 0 {
		fn.CPUMs = req.CPUMs
	}
	if req.SubRequests > 0 {
		fn.SubRequests = req.SubRequests
	}
	if schedules != nil {
		fn.Schedules = *schedules
	}
	if activate {
		fn.ActiveVersion = next
	}
	// After the pointer moves, so "the active version" is the one this deploy leaves live — the
	// new one, or whatever was serving before an activate:false.
	s.pruneLocked(instanceID, cfg, req.Name, keep)
	s.save()

	return map[string]any{"name": req.Name, "version": next, "size_bytes": v.SizeBytes, "active": activate}, nil
}

// Prune applies a retention setting to every function in an instance, returning how many versions
// were removed. Called when the setting is lowered, so it takes effect now rather than at each
// function's next deploy.
func (s *Store) Prune(instanceID string, keep int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		return 0
	}
	removed := 0
	for name := range cfg.Versions {
		removed += s.pruneLocked(instanceID, cfg, name, keep)
	}
	if removed > 0 {
		s.save()
	}
	return removed
}

// pruneLocked keeps one function's newest `keep` versions plus the active one when it is older,
// deleting the rest with their code. It never deletes the version being served: a function rolled
// back to v1 that then stages v11 with activate:false is still serving v1.
func (s *Store) pruneLocked(instanceID string, cfg *Config, name string, keep int) int {
	if keep < 1 {
		keep = 1
	}
	hist := cfg.Versions[name] // oldest first
	if len(hist) <= keep {
		return 0
	}
	active := 0
	if fn := cfg.Find(name); fn != nil {
		active = fn.ActiveVersion
	}
	cut := len(hist) - keep
	kept := make([]Version, 0, keep+1)
	removed := 0
	for i, v := range hist {
		if i >= cut || v.Version == active {
			kept = append(kept, v)
			continue
		}
		delete(s.code[instanceID], codeKey(name, v.Version))
		removed++
	}
	cfg.Versions[name] = kept
	return removed
}

// KeepVersions reads the retention setting from an instance's config. A missing or malformed
// value reads as the default, exactly as hosted parses a stored setting.
func KeepVersions(cfg map[string]any) int {
	if n, ok := keepValue(cfg["keepVersions"]); ok {
		return n
	}
	return DefaultKeepVersions
}

// ValidateConfig refuses a functions config whose `keepVersions` is not an integer in 1..50.
func ValidateConfig(cfg map[string]any) error {
	raw, present := cfg["keepVersions"]
	if !present || raw == nil {
		return nil
	}
	if _, ok := keepValue(raw); !ok {
		return common.BadRequest(fmt.Sprintf("config.keepVersions must be an integer in 1..%d", MaxKeepVersions))
	}
	return nil
}

func keepValue(raw any) (int, bool) {
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case json.Number:
		p, err := v.Float64()
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	if f < 1 || f > MaxKeepVersions || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// Activate points a function at an existing version (rollback), returning the version it
// was serving before.
func (s *Store) Activate(instanceID, name string, version int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		return 0, common.NotFound("function '" + name + "' not found")
	}
	fn := cfg.Find(name)
	if fn == nil {
		return 0, common.NotFound("function '" + name + "' not found")
	}
	if _, ok := s.code[instanceID][codeKey(name, version)]; !ok {
		return 0, common.NotFound(fmt.Sprintf("function '%s' has no version %d", name, version))
	}
	previous := fn.ActiveVersion
	fn.ActiveVersion = version
	s.save()
	return previous, nil
}

// DeleteVersion removes one stored version. Refuses the version being served, as hosted
// does: removing it would leave the function pointing at code that no longer exists.
func (s *Store) DeleteVersion(instanceID, name string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg != nil {
		if fn := cfg.Find(name); fn != nil && fn.ActiveVersion == version {
			return common.Precondition(fmt.Sprintf("v%d of '%s' is being served; activate another version first", version, name))
		}
	}
	if _, ok := s.code[instanceID][codeKey(name, version)]; !ok || cfg == nil {
		return common.NotFound(fmt.Sprintf("function '%s' has no version %d", name, version))
	}
	hist := cfg.Versions[name]
	kept := make([]Version, 0, len(hist))
	for _, v := range hist {
		if v.Version != version {
			kept = append(kept, v)
		}
	}
	cfg.Versions[name] = kept
	delete(s.code[instanceID], codeKey(name, version))
	s.save()
	return nil
}

// DeleteFunction removes a function, its schedules, and every stored version. Returns how
// many versions were removed.
func (s *Store) DeleteFunction(instanceID, name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil || (cfg.Find(name) == nil && len(cfg.Versions[name]) == 0) {
		return 0, common.NotFound("function '" + name + "' not found")
	}
	kept := cfg.Functions[:0]
	for _, f := range cfg.Functions {
		if f.Name != name {
			kept = append(kept, f)
		}
	}
	cfg.Functions = kept
	removed := len(cfg.Versions[name])
	for _, v := range cfg.Versions[name] {
		delete(s.code[instanceID], codeKey(name, v.Version))
	}
	delete(cfg.Versions, name)
	s.save()
	return removed, nil
}

// Code returns one version's source.
func (s *Store) Code(instanceID, name string, version int) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.code[instanceID][codeKey(name, version)]
	return src, ok
}

// Versions returns the deploy history for one function, newest first.
func (s *Store) Versions(instanceID, name string) []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		return nil
	}
	hist := cfg.Versions[name]
	out := make([]Version, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		out = append(out, hist[i])
	}
	return out
}

// SetSecrets replaces the whole secret map, resolving keep-existing entries.
//
// A submitted entry may omit its value to mean "keep the stored one" — hosted, values are
// write-only and cannot be read back, so without this an editor could not change one
// secret's exposure without re-typing every other credential.
func (s *Store) SetSecrets(instanceID string, in map[string]json.RawMessage) (map[string]Secret, error) {
	if len(in) > MaxSecrets {
		return nil, common.BadRequest(fmt.Sprintf("at most %d secrets per instance", MaxSecrets))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		cfg = &Config{Secrets: map[string]Secret{}, Versions: map[string][]Version{}}
		s.configs[instanceID] = cfg
	}
	out := map[string]Secret{}
	for name, raw := range in {
		if !secretNameRe.MatchString(name) {
			return nil, common.BadRequest(fmt.Sprintf("secret name '%s' must be 1-64 chars of A-Z, 0-9 and _, starting with a letter", name))
		}
		var str string
		if json.Unmarshal(raw, &str) == nil {
			out[name] = Secret{Value: str}
			continue
		}
		var obj struct {
			Value  *string `json:"value"`
			Egress *bool   `json:"egress"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return nil, common.BadRequest(fmt.Sprintf("secret '%s' must be a string, or { value, egress }", name))
		}
		prev, had := cfg.Secrets[name]
		switch {
		case obj.Value != nil:
			eg := false
			if obj.Egress != nil {
				eg = *obj.Egress
			}
			out[name] = Secret{Value: *obj.Value, Egress: eg}
		case had:
			eg := prev.Egress
			if obj.Egress != nil {
				eg = *obj.Egress
			}
			out[name] = Secret{Value: prev.Value, Egress: eg}
		default:
			return nil, common.BadRequest(fmt.Sprintf("secret '%s' has no value and does not exist", name))
		}
		if len(out[name].Value) > MaxSecretSize {
			return nil, common.BadRequest(fmt.Sprintf("secret '%s' exceeds %d bytes", name, MaxSecretSize))
		}
	}
	cfg.Secrets = out
	s.save()
	return out, nil
}

// SetSettings replaces the outbound allowlist and CORS origins.
func (s *Store) SetSettings(instanceID string, allowedHosts, corsOrigins []string) error {
	for _, h := range allowedHosts {
		n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if strings.ContainsAny(n, "/:") || !hostRe.MatchString(n) {
			return common.BadRequest(fmt.Sprintf("allowedHosts entry '%s' must be a bare hostname", h))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.configs[instanceID]
	if cfg == nil {
		cfg = &Config{Secrets: map[string]Secret{}, Versions: map[string][]Version{}}
		s.configs[instanceID] = cfg
	}
	cfg.AllowedHosts = normalizeAll(allowedHosts)
	cfg.CORSOrigins = normalizeAll(corsOrigins)
	s.save()
	return nil
}

// SecretNames lists names and exposure, never values — the same contract as hosted, where
// a value cannot be read back once written.
func (c *Config) SecretNames() []map[string]any {
	names := make([]string, 0, len(c.Secrets))
	for n := range c.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "egress": c.Secrets[n].Egress})
	}
	return out
}

func normalizeAll(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func codeKey(name string, version int) string { return fmt.Sprintf("%s@%d", name, version) }
