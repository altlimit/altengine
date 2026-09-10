// Package control is the emulator's control plane: the registry of service instances
// (search / channel / datastore / auth) and minted API keys for the single local dev org.
// It lives in memory and is persisted to a small JSON file so instances and keys
// survive a restart. Instances auto-create on first
// data-plane use so `altengine dev` followed by a request to /v1/datastore/myapp
// just works with no setup.
package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Instance is one service instance owned by the dev org.
type Instance struct {
	ID        string         `json:"id"`
	Service   string         `json:"service"` // search | channel | datastore | auth
	Name      string         `json:"name"`
	CreatedAt int64          `json:"created_at"`
	Config    map[string]any `json:"config"`
	Secret    string         `json:"secret,omitempty"` // channel / auth JWT signing secret
}

// APIKey is a minted key's metadata. The plaintext is returned once at creation and
// registered with auth.Store; only metadata is retained here.
type APIKey struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Prefix     string            `json:"prefix"`
	Grants     map[string]string `json:"grants"`
	CreatedAt  int64             `json:"created_at"`
	LastUsedAt *int64            `json:"last_used_at"`
}

type persisted struct {
	Instances []*Instance `json:"instances"`
	Keys      []*APIKey   `json:"api_keys"`
}

// Registry is the control-plane store.
type Registry struct {
	mu    sync.RWMutex
	insts map[string]*Instance // key: service + "/" + name
	keys  map[string]*APIKey   // key: id
	path  string               // persistence file ("" = memory only)
	drops map[string][]func(instanceID string)
}

func nowMS() int64 { return time.Now().UnixMilli() }

// NowMS returns the current time in unix milliseconds (exported for other packages).
func NowMS() int64 { return nowMS() }

// New loads (or initializes) a registry. dir "" means in-memory only.
func New(dir string) (*Registry, error) {
	r := &Registry{insts: map[string]*Instance{}, keys: map[string]*APIKey{}, drops: map[string][]func(string){}}
	if dir == "" {
		return r, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r.path = filepath.Join(dir, "control.json")
	if data, err := os.ReadFile(r.path); err == nil {
		var p persisted
		if json.Unmarshal(data, &p) == nil {
			for _, in := range p.Instances {
				r.insts[in.Service+"/"+in.Name] = in
			}
			for _, k := range p.Keys {
				r.keys[k.ID] = k
			}
		}
	}
	return r, nil
}

func (r *Registry) save() {
	if r.path == "" {
		return
	}
	p := persisted{}
	for _, in := range r.insts {
		p.Instances = append(p.Instances, in)
	}
	for _, k := range r.keys {
		p.Keys = append(p.Keys, k)
	}
	if data, err := json.MarshalIndent(p, "", "  "); err == nil {
		_ = os.WriteFile(r.path, data, 0o644)
	}
}

// Services is every service the emulator knows how to hold instances for.
//
// It exists so that adding one is a single edit. The dev-open grant map used to carry its own
// copy of this list, and when the container service landed nobody added it there — so every
// container call in the emulator's DEFAULT mode answered 403, which reads as a broken emulator
// rather than a missing line in an auth file. A test asserts dev-open covers all of these.
var Services = []string{"search", "datastore", "channel", "auth", "functions", "blob", "container"}

// needsSecret reports whether a service signs tokens with a per-instance secret: channel
// subscriber tokens and auth end-user identity tokens.
func needsSecret(service string) bool { return service == "channel" || service == "auth" }

func defaultConfig(service string) map[string]any {
	switch service {
	case "search":
		return map[string]any{"rateLimit": 0, "stemming": true}
	case "datastore":
		return map[string]any{"rateLimit": 0, "autoId": "uuid", "autoIndex": true}
	case "channel":
		return map[string]any{"presence": false, "publishRateLimit": 0, "connectRateLimit": 0}
	case "blob":
		// defaultPublic is FALSE on purpose: the failure mode of getting it wrong in the other
		// direction is publishing something nobody meant to publish.
		return map[string]any{"maxObjectBytes": 104857600, "defaultPublic": false}
	case "container":
		// allowedImages is EMPTY on purpose — a new instance runs nothing until someone says
		// what it may run. See internal/container.
		return map[string]any{
			"allowedImages": []any{},
			"maxTimeoutMs":  300000,
			"maxConcurrent": 2,
			"maxJobCostUsd": 1,
		}
	case "auth":
		// The seed blob an auth instance starts with. Every key is optional — the auth
		// service defaults anything missing — so this is what the console pre-populates,
		// not the source of truth for defaults.
		return map[string]any{
			"allowSignup":         true,
			"accessTokenTtl":      3600,
			"refreshTokenTtl":     2592000,
			"passwordlessEnabled": false,
			"passwordlessCodeTtl": 600,
			"passkeysEnabled":     false,
			"totpEnabled":         false,
			"captchaEnabled":      false,
			"signup": map[string]any{
				"identityField": "email",
				"fields":        []any{map[string]any{"key": "email", "type": "email", "required": true}},
			},
			"access": map[string]any{},
		}
	}
	return map[string]any{}
}

// GetOrCreate returns the instance for (service, name), auto-creating it if absent.
func (r *Registry) GetOrCreate(service, name string) *Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := service + "/" + name
	if in, ok := r.insts[key]; ok {
		return in
	}
	in := &Instance{
		ID:        common.UUID(),
		Service:   service,
		Name:      name,
		CreatedAt: nowMS(),
		Config:    defaultConfig(service),
	}
	if needsSecret(service) {
		in.Secret = common.RandID(32)
	}
	r.insts[key] = in
	r.save()
	return in
}

// Get returns the instance for (service, name), or nil.
func (r *Registry) Get(service, name string) *Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.insts[service+"/"+name]
}

// GetByID returns the instance with the given id, or nil.
func (r *Registry) GetByID(service, id string) *Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, in := range r.insts {
		if in.Service == service && in.ID == id {
			return in
		}
	}
	return nil
}

// List returns instances of a service, newest first.
func (r *Registry) List(service string) []*Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Instance
	for _, in := range r.insts {
		if in.Service == service {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Create explicitly creates an instance, erroring if the name is taken.
func (r *Registry) Create(service, name string) (*Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := service + "/" + name
	if _, ok := r.insts[key]; ok {
		return nil, common.AlreadyExists("instance already exists")
	}
	in := &Instance{ID: common.UUID(), Service: service, Name: name, CreatedAt: nowMS(), Config: defaultConfig(service)}
	if needsSecret(service) {
		in.Secret = common.RandID(32)
	}
	r.insts[key] = in
	r.save()
	return in, nil
}

// SetConfig replaces an instance's config (shallow merge of provided keys).
func (r *Registry) SetConfig(in *Instance, cfg map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range cfg {
		in.Config[k] = v
	}
	r.save()
}

// OnDelete registers what drops one instance's DATA for a service — its databases, its files —
// called by DeleteInstance before the instance row goes.
//
// Services register their own because only they know where their data lives, and it is a hook
// rather than a switch in DeleteInstance so that a service is torn down by the package that
// created it. A service with nothing on disk registers nothing.
func (r *Registry) OnDelete(service string, drop func(instanceID string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops[service] = append(r.drops[service], drop)
}

// DeleteInstance removes an instance AND the data it holds. The console and the MCP tool both
// call this, so "deleted" means the same thing whichever surface asked.
//
// Data first, identity last: the instance row is the only handle on where that data lives, so a
// failure part-way leaves files an operator can still find rather than orphans nothing names.
func (r *Registry) DeleteInstance(service, id string) bool {
	r.mu.RLock()
	var found *Instance
	for _, in := range r.insts {
		if in.Service == service && in.ID == id {
			found = in
			break
		}
	}
	drops := append([]func(string){}, r.drops[service]...)
	r.mu.RUnlock()
	if found == nil {
		return false
	}
	for _, drop := range drops {
		drop(found.ID)
	}
	return r.Delete(service, id)
}

// Delete removes an instance by service+id, leaving its data alone. Returns whether it existed.
// Prefer DeleteInstance — this is the identity half of it.
func (r *Registry) Delete(service, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, in := range r.insts {
		if in.Service == service && in.ID == id {
			delete(r.insts, key)
			r.save()
			return true
		}
	}
	return false
}

// --- API keys ---

// AddKey stores minted key metadata.
func (r *Registry) AddKey(k *APIKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys[k.ID] = k
	r.save()
}

// ListKeys returns all key metadata, newest first.
func (r *Registry) ListKeys() []*APIKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*APIKey
	for _, k := range r.keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// DeleteKey removes key metadata by id, returning it (for auth revocation) or nil.
func (r *Registry) DeleteKey(id string) *APIKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.keys[id]
	if k != nil {
		delete(r.keys, id)
		r.save()
	}
	return k
}
