package manifest

// The install record: one entry per app deployed into the emulator, kept in <data>/deploys.json
// (in memory with --memory). The local form of hosted's installStore.ts, keyed by app name.
//
// WHAT IT DECIDES (as hosted)
//   - Ownership. An app may change an instance only if an `owned` row of ITS install holds that
//     instance's id. A name that happens to match is not ownership: that is an adoption (when no
//     install owns it) or a conflict (when another one does).
//   - Bindings. A `uses` row is an existing instance bound to a manifest's `uses` key, at a level,
//     with the references the app made to it — what an update's escalations are measured against.
//
// AND ONE THING HOSTED DOES NOT NEED: a static site's port. The emulator has no static service, so
// a manifest's static instance is a site the emulator serves on a port of its own, and that port is
// part of the origin written into auth and functions config. It is assigned once and kept here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// InstallInstance is one instance an install created (owned) or was bound to (uses).
type InstallInstance struct {
	Key          string `json:"key"`
	Service      string `json:"service"`
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	Relation     string `json:"relation"` // owned | uses
	Level        string `json:"level,omitempty"`
	// Managed is false once the key is no longer in the manifest: the instance stays, the app
	// stops changing it.
	Managed bool     `json:"managed"`
	Refs    []string `json:"refs,omitempty"`
	// Site is a static instance's local site.
	Site *SiteRecord `json:"site,omitempty"`
}

// SiteRecord is everything needed to serve a deployed static instance again after a restart.
type SiteRecord struct {
	Port int `json:"port"`
	// Dir is what is served: the site's base directory for a file manifest (live), or the copy
	// downloaded under <data>/sites for a URL manifest. Empty before the first publish, and for a
	// downloaded site in --memory mode (its files are held in memory).
	Dir string `json:"dir,omitempty"`
	// Downloaded marks Dir as the emulator's own copy, which it may delete.
	Downloaded bool `json:"downloaded,omitempty"`
	// Config is the instance's static config (spa, cleanUrls, notFound, ...).
	Config map[string]any `json:"config"`
	// RuntimeConfigPath and RuntimeConfig are the generated file, served from memory.
	RuntimeConfigPath string `json:"runtime_config_path,omitempty"`
	RuntimeConfig     string `json:"runtime_config,omitempty"`
	Published         bool   `json:"published"`
}

// Install is one deployed app.
type Install struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	ManifestURL   string             `json:"manifest_url"`
	ManifestHost  string             `json:"manifest_host"`
	Title         string             `json:"title,omitempty"`
	Version       string             `json:"manifest_version,omitempty"`
	AppliedSHA256 string             `json:"applied_sha256,omitempty"`
	Status        string             `json:"status"` // applying | ok | failed
	LastError     string             `json:"last_error,omitempty"`
	Vars          map[string]string  `json:"vars"`
	CreatedAt     int64              `json:"created_at"`
	UpdatedAt     int64              `json:"updated_at"`
	Instances     []*InstallInstance `json:"instances"`
	// PendingInitial are keys a run created but did not finish: the next run still gives them
	// initialConfig, as hosted's resumed apply does.
	PendingInitial []string `json:"pending_initial,omitempty"`
}

// Row returns the install's row for key, or nil.
func (in *Install) Row(key string) *InstallInstance {
	for _, r := range in.Instances {
		if r.Key == key {
			return r
		}
	}
	return nil
}

func (in *Install) clone() *Install {
	b, _ := json.Marshal(in)
	var out Install
	_ = json.Unmarshal(b, &out)
	return &out
}

// Store holds every install.
type Store struct {
	mu       sync.Mutex
	path     string
	installs map[string]*Install // by name
	last     int64               // the newest UpdatedAt, so two saves in one millisecond still order
}

// NewStore loads <dir>/deploys.json, or starts empty in memory when dir is "".
func NewStore(dir string) (*Store, error) {
	s := &Store{installs: map[string]*Install{}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, "deploys.json")
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	// Refused rather than treated as empty: the next deploy would save over it and every app
	// would lose the record of which instances it owns.
	var p struct {
		Installs []*Install `json:"installs"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s is not a valid deploy record (%v); repair it or move it aside to start with none", s.path, err)
	}
	for _, in := range p.Installs {
		s.installs[in.Name] = in
		s.last = max(s.last, in.UpdatedAt)
	}
	return s, nil
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*Install, 0, len(s.installs))
	for _, n := range sortedKeys(s.installs) {
		list = append(list, s.installs[n])
	}
	b, err := json.MarshalIndent(map[string]any{"installs": list}, "", "  ")
	if err == nil {
		err = common.WriteFileAtomic(s.path, b, common.DataFilePerm)
	}
	if err != nil {
		log.Printf("deploy: saving %s: %v", s.path, err)
	}
}

// Get returns a copy of the install named name, or nil.
func (s *Store) Get(name string) *Install {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in := s.installs[name]; in != nil {
		return in.clone()
	}
	return nil
}

// List returns copies of every install, by name.
func (s *Store) List() []*Install {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Install{}
	for _, n := range sortedKeys(s.installs) {
		out = append(out, s.installs[n].clone())
	}
	return out
}

// FromSource returns the installs deployed from ref, most recently updated first.
func (s *Store) FromSource(ref string) []*Install {
	var out []*Install
	for _, in := range s.List() {
		if in.ManifestURL == ref {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// Owner returns the name of the install that owns an instance id, or "".
func (s *Store) Owner(service, id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range sortedKeys(s.installs) {
		for _, r := range s.installs[n].Instances {
			if r.Relation == "owned" && r.Service == service && r.InstanceID == id && id != "" {
				return n
			}
		}
	}
	return ""
}

// Put saves an install (a copy of it), replacing the one of that name.
func (s *Store) Put(in *Install) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := control.NowMS()
	if now <= s.last {
		now = s.last + 1
	}
	s.last, in.UpdatedAt = now, now
	s.installs[in.Name] = in.clone()
	s.saveLocked()
}

// Sites returns every recorded static site, with the install and key it belongs to.
func (s *Store) Sites() []SiteRef {
	var out []SiteRef
	for _, in := range s.List() {
		for _, r := range in.Instances {
			if r.Relation == "owned" && r.Service == "static" && r.Site != nil && r.InstanceID != "" {
				out = append(out, SiteRef{App: in.Name, Key: r.Key, ID: r.InstanceID, Name: r.InstanceName, Site: r.Site})
			}
		}
	}
	return out
}

// StaticByRef finds a recorded static instance by id or name.
func (s *Store) StaticByRef(ref string) *SiteRef {
	for _, sr := range s.Sites() {
		if sr.ID == ref || sr.Name == ref {
			c := sr
			return &c
		}
	}
	return nil
}

// SiteRef is one recorded static site.
type SiteRef struct {
	App, Key, ID, Name string
	Site               *SiteRecord
}
