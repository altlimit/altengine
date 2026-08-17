// Package blob emulates altengine's blob service: a place to put a file, an upload that does
// not travel through the API, and a public URL for the ones you publish.
//
// WHAT IS DIFFERENT HERE, AND WHY IT DOES NOT SHOW.
//
// Hosted, the bytes never touch altengine: the API mints a presigned URL and the client PUTs
// straight to object storage. There is no commit step — storage itself reports the upload, and
// the record is promoted to `ready` on the strength of what storage says rather than what the
// uploader claims.
//
// Locally there is no second origin to hand the bytes to, so the emulator plays both parts. It
// still mints a URL and the client still PUTs to it, and the record is still promoted by the
// side that received the bytes, using the size and digest IT measured. So the shape of the flow,
// the order of the steps, and the fact that a size the client lied about is caught by storage
// rather than by the API are all preserved. What is emulated is who owns the disk.
//
// The upload URL is signed and expiring for the same reason the real one is: it is a bearer
// capability. Whoever holds it can write those bytes and nothing else, until it expires.
package blob

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	// DefaultMaxObjectBytes is what a new instance allows for one object. Generous for images
	// and documents, small enough that a leaked upload URL is not an open invitation.
	DefaultMaxObjectBytes int64 = 100 << 20
	// MaxObjectBytesCeiling is object storage's single-PUT limit. Anything larger needs a
	// multipart upload, which one presigned URL cannot express.
	MaxObjectBytesCeiling int64 = 5 << 30

	MaxNameLen       = 512
	MaxListLimit     = 200
	DefaultListLimit = 25
	MaxDeleteIDs     = 200

	// An upload URL is minutes, not hours: the client that asked for it is about to use it.
	// A download URL is shorter still — these end up in browser history and referrers.
	UploadTTL   = 15 * time.Minute
	DownloadTTL = 5 * time.Minute
)

// Config is the per-instance settings blob, matching the hosted service's `settings` key.
//
// `rateLimit` and `region` are accepted and ignored: one is meaningless against a local process
// and the other places a database in a data centre. Rejecting them would make a config that works
// hosted fail locally, which is the opposite of the point.
type Config struct {
	MaxObjectBytes int64
	DefaultPublic  bool
}

func DefaultConfig() Config {
	return Config{MaxObjectBytes: DefaultMaxObjectBytes}
}

// ParseConfig reads an instance's stored config. Tolerant by design — a setting we cannot read
// falls back to its default rather than taking the data plane down.
func ParseConfig(raw map[string]any) Config {
	c := DefaultConfig()
	if n, ok := numOf(raw["maxObjectBytes"]); ok && n >= 1 {
		c.MaxObjectBytes = int64(n)
		if c.MaxObjectBytes > MaxObjectBytesCeiling {
			c.MaxObjectBytes = MaxObjectBytesCeiling
		}
	}
	if b, ok := raw["defaultPublic"].(bool); ok {
		c.DefaultPublic = b
	}
	return c
}

func numOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Record is one object's metadata — the wire shape, field for field, of the hosted service's.
type Record struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Size        int64          `json:"size"`
	ContentType string         `json:"content_type"`
	ETag        string         `json:"etag"`
	Status      string         `json:"status"` // pending | ready
	Public      bool           `json:"public"`
	Created     int64          `json:"created"`
	Updated     int64          `json:"updated"`
	Meta        map[string]any `json:"meta"`
}

func (r *Record) clone() *Record {
	c := *r
	c.Meta = map[string]any{}
	for k, v := range r.Meta {
		c.Meta[k] = v
	}
	return &c
}

// Store holds every instance's objects: their metadata, and their bytes.
//
// With a data directory the bytes are files and the metadata is a JSON index beside them, so an
// upload survives a restart — a blobkey stored in a local datastore document has to still resolve
// tomorrow or the local app is broken in a way the hosted one is not. Without one, everything is
// in memory and goes when the process does.
type Store struct {
	mu   sync.Mutex
	dir  string // "" => in-memory
	recs map[string]map[string]*Record
	data map[string][]byte // "instance/id" => bytes, memory mode only
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, recs: map[string]map[string]*Record{}, data: map[string][]byte{}}
}

func (s *Store) memory() bool { return s.dir == "" }

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func (s *Store) instDir(instanceID string) string {
	return filepath.Join(s.dir, "blob", sanitize(instanceID))
}

// index loads (once) and returns the map for an instance. Caller holds the lock.
func (s *Store) index(instanceID string) map[string]*Record {
	if m, ok := s.recs[instanceID]; ok {
		return m
	}
	m := map[string]*Record{}
	if !s.memory() {
		if data, err := os.ReadFile(filepath.Join(s.instDir(instanceID), "index.json")); err == nil {
			var rows []*Record
			if json.Unmarshal(data, &rows) == nil {
				for _, r := range rows {
					if r.Meta == nil {
						r.Meta = map[string]any{}
					}
					m[r.ID] = r
				}
			}
		}
	}
	s.recs[instanceID] = m
	return m
}

// save rewrites an instance's index. Caller holds the lock. Whole-file rewrite because a local
// index is small and a torn write is worse than a slow one.
func (s *Store) save(instanceID string) {
	if s.memory() {
		return
	}
	dir := s.instDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	rows := make([]*Record, 0, len(s.recs[instanceID]))
	for _, r := range s.recs[instanceID] {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Created < rows[j].Created })
	data, err := json.Marshal(rows)
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, "index.json.tmp")
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "index.json"))
	}
}

func (s *Store) objectPath(instanceID, id string) string {
	return filepath.Join(s.instDir(instanceID), sanitize(id)+".bin")
}

// separators is every character that would break a name out of its one path segment or forge a
// header. A RUN of them collapses to a single dash — matching hosted exactly, because this name
// ends up in a public URL and a name that normalizes differently in the two places is a URL that
// differs between local and production.
var separators = regexp.MustCompile(`[\r\n\\/]+`)

// CleanName normalizes a display name. It is cosmetic — the id is the lookup key — so this only
// has to be safe in a URL path segment and in a Content-Disposition header. Slashes are dropped
// rather than escaped: a name is one segment, not a path.
func CleanName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > MaxNameLen {
		return "", common.BadRequest(fmt.Sprintf("name must be <= %d characters", MaxNameLen))
	}
	return strings.TrimLeft(separators.ReplaceAllString(s, "-"), "."), nil
}

// MintRequest is a request for an upload slot.
type MintRequest struct {
	Name        string         `json:"name"`
	Size        *int64         `json:"size"`
	ContentType string         `json:"content_type"`
	Public      *bool          `json:"public"`
	Meta        map[string]any `json:"meta"`
}

// Reserve validates a mint request and records a pending row.
//
// The size is REQUIRED and checked here, before anything is stored, because hosted it is signed
// into the upload URL — the ceiling has to bite at mint time or it does not bite at all.
func (s *Store) Reserve(instanceID string, cfg Config, req MintRequest) (*Record, error) {
	if req.Size == nil || *req.Size < 1 {
		return nil, common.BadRequest("size (the exact byte length you will upload) is required")
	}
	if *req.Size > cfg.MaxObjectBytes {
		return nil, common.BadRequest(fmt.Sprintf(
			"size %d exceeds this instance's limit of %d bytes", *req.Size, cfg.MaxObjectBytes))
	}
	name, err := CleanName(req.Name)
	if err != nil {
		return nil, err
	}
	ct := strings.TrimSpace(req.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	if strings.ContainsAny(ct, "\r\n") {
		return nil, common.BadRequest("content_type may not contain line breaks")
	}
	public := cfg.DefaultPublic
	if req.Public != nil {
		public = *req.Public
	}
	meta := req.Meta
	if meta == nil {
		meta = map[string]any{}
	}

	now := time.Now().UnixMilli()
	rec := &Record{
		ID: strings.ReplaceAll(common.UUID(), "-", ""), Name: name, Size: *req.Size,
		ContentType: ct, Status: "pending", Public: public, Created: now, Updated: now, Meta: meta,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// The pending row carries the size and type the URL was minted for. Hosted these are signed
	// into the URL because storage has no row to consult; here the row IS available, so the
	// upload is checked against it instead. Same enforcement, at the same moment.
	s.index(instanceID)[rec.ID] = rec
	s.save(instanceID)
	return rec.clone(), nil
}

// Pending returns a reserved row awaiting its bytes.
func (s *Store) Pending(instanceID, id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.index(instanceID)[id]
	if !ok || r.Status != "pending" {
		return nil, false
	}
	return r.clone(), true
}

// Commit stores the bytes and promotes the row, using the size and digest measured HERE.
//
// This is the emulator standing in for the storage event. It is deliberately the only path that
// can make a row `ready`: nothing a client says about its own upload is taken on trust, hosted or
// locally.
func (s *Store) Commit(instanceID, id string, body []byte) (*Record, error) {
	sum := md5.Sum(body)
	etag := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.index(instanceID)[id]
	if !ok {
		return nil, common.NotFound("blob not found")
	}
	if !s.memory() {
		if err := os.MkdirAll(s.instDir(instanceID), 0o755); err != nil {
			return nil, common.NewError(500, "could not write the object to disk", "INTERNAL")
		}
		if err := os.WriteFile(s.objectPath(instanceID, id), body, 0o644); err != nil {
			return nil, common.NewError(500, "could not write the object to disk", "INTERNAL")
		}
	} else {
		s.data[instanceID+"/"+id] = body
	}
	rec.Size = int64(len(body))
	rec.ETag = etag
	rec.Status = "ready"
	rec.Updated = time.Now().UnixMilli()
	s.save(instanceID)
	return rec.clone(), nil
}

// Bytes returns a stored object's content.
func (s *Store) Bytes(instanceID, id string) ([]byte, bool) {
	s.mu.Lock()
	memory := s.memory()
	b, ok := s.data[instanceID+"/"+id]
	path := s.objectPath(instanceID, id)
	s.mu.Unlock()
	if memory {
		return b, ok
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

func (s *Store) Get(instanceID, id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.index(instanceID)[id]
	if !ok {
		return nil, false
	}
	return r.clone(), true
}

// List returns a newest-first page of READY objects, keyset-paged on "created:id".
//
// Pending rows are excluded: a reservation is a promise that a URL was handed out, not a claim
// that anything was stored, and a listing full of files that do not exist is worse than useless.
func (s *Store) List(instanceID, prefix string, limit int, cursor string) ([]*Record, string) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []*Record
	for _, r := range s.index(instanceID) {
		if r.Status != "ready" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Created != rows[j].Created {
			return rows[i].Created > rows[j].Created
		}
		return rows[i].ID < rows[j].ID
	})

	if cursor != "" {
		if c, id, ok := strings.Cut(cursor, ":"); ok {
			created, err := strconv.ParseInt(c, 10, 64)
			if err == nil {
				kept := rows[:0]
				for _, r := range rows {
					if r.Created < created || (r.Created == created && r.ID > id) {
						kept = append(kept, r)
					}
				}
				rows = kept
			}
		}
	}

	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		next = fmt.Sprintf("%d:%s", last.Created, last.ID)
		rows = rows[:limit]
	}
	out := make([]*Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.clone())
	}
	return out, next
}

func (s *Store) SetPublic(instanceID, id string, public bool) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.index(instanceID)[id]
	if !ok {
		return nil, false
	}
	r.Public = public
	r.Updated = time.Now().UnixMilli()
	s.save(instanceID)
	return r.clone(), true
}

// Remove deletes rows and their bytes, returning the rows that existed so the caller can report
// an honest count.
func (s *Store) Remove(instanceID string, ids []string) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.index(instanceID)
	var removed []*Record
	for _, id := range ids {
		r, ok := m[id]
		if !ok {
			continue
		}
		removed = append(removed, r.clone())
		delete(m, id)
		if s.memory() {
			delete(s.data, instanceID+"/"+id)
		} else {
			_ = os.Remove(s.objectPath(instanceID, id))
		}
	}
	if len(removed) > 0 {
		s.save(instanceID)
	}
	return removed
}

// Drop forgets an instance entirely, bytes included (on instance delete).
func (s *Store) Drop(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, instanceID)
	for k := range s.data {
		if strings.HasPrefix(k, instanceID+"/") {
			delete(s.data, k)
		}
	}
	if !s.memory() {
		_ = os.RemoveAll(s.instDir(instanceID))
	}
}
