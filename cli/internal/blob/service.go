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
	// MaxObjectBytesCeiling is the largest object storage will hold, and therefore the highest an
	// instance's own limit may be set. That limit is a COST control and a different question from
	// what one request can carry.
	MaxObjectBytesCeiling int64 = 5492060580741 // 4.995 TiB
	// MaxSinglePutBytes is what ONE presigned PUT can carry. Not a policy number: object storage
	// caps a single upload here, and a presigned URL is exactly one request. Past it an upload
	// goes multipart.
	MaxSinglePutBytes int64 = 5 << 30

	// MinPartBytes is the smallest part storage accepts, except for the last one. It is a floor
	// under the PLAN and not a check on the way in: a part that arrives undersized is refused by
	// storage hosted and accepted here, deliberately, because the alternative is that the flow a
	// customer's 20 GB export will take cannot be exercised with a test file on a laptop.
	MinPartBytes int64 = 5 << 20
	// DefaultPartBytes is chosen for the UPLINK rather than for the ceiling: a part that fails is
	// re-sent in full, so a smaller part is a cheaper retry. 64 MB still reaches 640 GB before
	// the part count matters.
	DefaultPartBytes int64 = 64 << 20
	// MaxParts is storage's hard limit on parts in one upload.
	MaxParts = 10000
	// MaxPartURLsPerRequest bounds one signing request. An upload may legitimately have ten
	// thousand parts, and URLs for all of them would expire long before a client reached the last.
	MaxPartURLsPerRequest = 100

	MaxNameLen       = 512
	MaxListLimit     = 200
	DefaultListLimit = 25
	MaxDeleteIDs     = 200

	// An upload URL is minutes, not hours: the client that asked for it is about to use it.
	// A download URL is shorter still — these end up in browser history and referrers.
	UploadTTL   = 15 * time.Minute
	DownloadTTL = 5 * time.Minute
	// A multipart window outlives a single PUT's: these are handed out across an upload that may
	// run for hours, and re-signed as it goes.
	MultipartTTL = 30 * time.Minute
)

// PlanParts is the part size for an object of a given size, and the number of parts it implies.
//
// Grows the part rather than the count once the default would overflow MaxParts: a bigger part is
// a worse retry, but a ten-thousand-and-first part is not an option and the alternative is
// refusing an upload we could have taken.
func PlanParts(size int64) (int64, int) {
	if size < 1 {
		size = 1
	}
	partSize := max(MinPartBytes, DefaultPartBytes)
	if (size+partSize-1)/partSize > MaxParts {
		partSize = (size + MaxParts - 1) / MaxParts
	}
	return partSize, int((size + partSize - 1) / partSize)
}

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
	// Multipart uploads in flight, keyed "instance/id/uploadID". Never persisted: hosted these
	// live in object storage until they are completed or aborted, and an upload that outlives the
	// process it was started in is one nothing can finish anyway.
	mpu map[string]map[int][]byte
}

func NewStore(dir string) *Store {
	return &Store{
		dir:  dir,
		recs: map[string]map[string]*Record{},
		data: map[string][]byte{},
		mpu:  map[string]map[int][]byte{},
	}
}

func mpuKey(instanceID, id, uploadID string) string {
	return instanceID + "/" + id + "/" + uploadID
}

// CreateUpload opens a multipart upload against a reserved row and returns its id.
func (s *Store) CreateUpload(instanceID, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.index(instanceID)[id]
	if !ok || rec.Status != "pending" {
		return "", common.NotFound("this upload URL has already been used, or its reservation is gone")
	}
	uploadID := strings.ReplaceAll(common.UUID(), "-", "")
	s.mpu[mpuKey(instanceID, id, uploadID)] = map[int][]byte{}
	return uploadID, nil
}

// PutPart stores one part and returns the digest storage computed for it.
//
// Parts are held until Complete because nothing is readable before then — an object exists once
// storage has glued its parts together, and not a moment earlier.
func (s *Store) PutPart(instanceID, id, uploadID string, n int, body []byte) (string, error) {
	if n < 1 || n > MaxParts {
		return "", common.BadRequest(fmt.Sprintf("partNumber must be between 1 and %d", MaxParts))
	}
	sum := md5.Sum(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	parts, ok := s.mpu[mpuKey(instanceID, id, uploadID)]
	if !ok {
		return "", common.NotFound("no such upload — it was completed, aborted, or never created")
	}
	parts[n] = body
	return hex.EncodeToString(sum[:]), nil
}

// CompleteUpload assembles the parts the client named, in the order it named them, and promotes
// the row exactly as a single PUT does.
func (s *Store) CompleteUpload(instanceID, id, uploadID string, want []int) (*Record, error) {
	s.mu.Lock()
	held, ok := s.mpu[mpuKey(instanceID, id, uploadID)]
	if !ok {
		s.mu.Unlock()
		return nil, common.NotFound("no such upload — it was completed, aborted, or never created")
	}
	var body []byte
	for _, n := range want {
		part, ok := held[n]
		if !ok {
			s.mu.Unlock()
			return nil, common.BadRequest(fmt.Sprintf("part %d was never uploaded", n))
		}
		body = append(body, part...)
	}
	delete(s.mpu, mpuKey(instanceID, id, uploadID))
	s.mu.Unlock()
	return s.Commit(instanceID, id, body)
}

// AbortUpload drops an upload's parts. Aborting one that is already gone is success: this is the
// call a client makes when it has just failed, and a refusal here would leave parts behind for
// the sake of tidiness.
func (s *Store) AbortUpload(instanceID, id, uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mpu, mpuKey(instanceID, id, uploadID))
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

// Reserve validates a mint request for a SINGLE PUT and records a pending row.
//
// The size is REQUIRED and checked here, before anything is stored, because hosted it is signed
// into the upload URL — the ceiling has to bite at mint time or it does not bite at all.
func (s *Store) Reserve(instanceID string, cfg Config, req MintRequest) (*Record, error) {
	return s.reserve(instanceID, cfg, req, true)
}

// BeginMultipart reserves a row for an upload that arrives in parts, and says how to cut it up.
// Bounded by the INSTANCE's limit only: getting past what one request can carry is the whole
// reason this path exists.
func (s *Store) BeginMultipart(instanceID string, cfg Config, req MintRequest) (*Record, int64, int, error) {
	rec, err := s.reserve(instanceID, cfg, req, false)
	if err != nil {
		return nil, 0, 0, err
	}
	partSize, parts := PlanParts(rec.Size)
	return rec, partSize, parts, nil
}

func (s *Store) reserve(instanceID string, cfg Config, req MintRequest, singlePut bool) (*Record, error) {
	if req.Size == nil || *req.Size < 1 {
		return nil, common.BadRequest("size (the exact byte length you will upload) is required")
	}
	if *req.Size > cfg.MaxObjectBytes {
		return nil, common.BadRequest(fmt.Sprintf(
			"size %d exceeds this instance's limit of %d bytes", *req.Size, cfg.MaxObjectBytes))
	}
	// The instance's limit is a COST control and may be far above what one request can carry.
	// This is the other limit, and it is not ours. Checked second, so a caller over both is told
	// about the one they can change.
	if singlePut && *req.Size > MaxSinglePutBytes {
		return nil, common.BadRequest(fmt.Sprintf(
			"size %d is over the %d-byte limit of a single upload — use the multipart flow (POST /uploads/multipart)",
			*req.Size, MaxSinglePutBytes))
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
	for k := range s.mpu {
		if strings.HasPrefix(k, instanceID+"/") {
			delete(s.mpu, k)
		}
	}
	if !s.memory() {
		_ = os.RemoveAll(s.instDir(instanceID))
	}
}
