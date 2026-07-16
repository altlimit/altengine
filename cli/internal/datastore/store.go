// Package datastore emulates the altengine datastore data plane: a per-namespace
// JSON document store with a bounded, index-served query/aggregate/transaction engine.
// It keeps one SQLite database per (instance, namespace) so queries, keyset cursors,
// and unique-index semantics behave the same as the hosted service.
package datastore

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	_ "modernc.org/sqlite"
)

const (
	maxDocBytes      = 1 << 20 // 1 MiB serialized doc
	maxKeyBytes      = 512
	maxDocsPerBatch  = 500
	maxKeysPerDelete = 500
	maxKeysPerGet    = 500
	maxTxnOps        = 500
	rowsPerReadUnit  = 100
)

var (
	collectionRe = regexp.MustCompile(`^[\x20-\x7e]{1,128}$`)
)

// Manager owns the per-(instance,namespace) SQLite databases.
type Manager struct {
	mu     sync.Mutex
	dbs    map[string]*sql.DB
	seen   map[string]map[string]bool // instanceID -> namespaces opened
	dir    string                     // "" => in-memory
	memory bool
}

// NewManager creates a datastore manager. dir "" (or memory) keeps everything in RAM.
func NewManager(dir string) *Manager {
	m := &Manager{dbs: map[string]*sql.DB{}, seen: map[string]map[string]bool{}, dir: dir, memory: dir == ""}
	return m
}

// Namespaces lists namespaces known for an instance (opened this session, plus, in
// file mode, any *.db already on disk).
func (m *Manager) Namespaces(instanceID string) []string {
	m.mu.Lock()
	set := map[string]bool{}
	for ns := range m.seen[instanceID] {
		set[ns] = true
	}
	m.mu.Unlock()
	if !m.memory {
		instDir := filepath.Join(m.dir, "datastore", sanitize(instanceID))
		if entries, err := os.ReadDir(instDir); err == nil {
			for _, e := range entries {
				if n := e.Name(); strings.HasSuffix(n, ".db") {
					set[strings.TrimSuffix(n, ".db")] = true
				}
			}
		}
	}
	var out []string
	for ns := range set {
		out = append(out, ns)
	}
	return out
}

// Drop deletes a namespace: closes its handle, forgets it, and (in file mode)
// removes the database file. Returns whether the namespace existed.
func (m *Manager) Drop(instanceID, namespace string) (bool, error) {
	m.mu.Lock()
	key := instanceID + "\x00" + namespace
	existed := m.seen[instanceID][namespace]
	if db, ok := m.dbs[key]; ok {
		existed = true
		_ = db.Close() // in memory mode this destroys the shared-cache database
		delete(m.dbs, key)
	}
	delete(m.seen[instanceID], namespace)
	m.mu.Unlock()
	if !m.memory {
		path := filepath.Join(m.dir, "datastore", sanitize(instanceID), sanitize(namespace)+".db")
		if _, err := os.Stat(path); err == nil {
			existed = true
			if err := os.Remove(path); err != nil {
				return existed, err
			}
		}
	}
	return existed, nil
}

func (m *Manager) handle(instanceID, namespace string) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := instanceID + "\x00" + namespace
	if m.seen[instanceID] == nil {
		m.seen[instanceID] = map[string]bool{}
	}
	m.seen[instanceID][namespace] = true
	if db, ok := m.dbs[key]; ok {
		return db, nil
	}
	var dsn string
	if m.memory {
		// A named shared-cache in-memory DB keeps one logical database per namespace.
		dsn = "file:" + sanitize(key) + "?mode=memory&cache=shared"
	} else {
		instDir := filepath.Join(m.dir, "datastore", sanitize(instanceID))
		if err := os.MkdirAll(instDir, 0o755); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(instDir, sanitize(namespace)+".db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-threaded DO semantics
	if err := ensureSchema(db); err != nil {
		return nil, err
	}
	m.dbs[key] = db
	return db, nil
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitize(s string) string { return sanitizeRe.ReplaceAllString(s, "_") }

func ensureSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS docs (
			collection TEXT NOT NULL,
			key TEXT NOT NULL,
			data TEXT NOT NULL,
			size INTEGER NOT NULL,
			created INTEGER NOT NULL,
			updated INTEGER NOT NULL,
			PRIMARY KEY (collection, key)
		)`,
		`CREATE INDEX IF NOT EXISTS docs_coll_updated ON docs(collection, updated, key)`,
		`CREATE INDEX IF NOT EXISTS docs_coll_created ON docs(collection, created, key)`,
		`CREATE TABLE IF NOT EXISTS _dsindexes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			collection TEXT NOT NULL,
			fields TEXT NOT NULL,
			is_unique INTEGER NOT NULL DEFAULT 0,
			created INTEGER NOT NULL,
			UNIQUE(collection, fields, is_unique)
		)`,
		`CREATE TABLE IF NOT EXISTS _dsseq (collection TEXT PRIMARY KEY, next INTEGER NOT NULL)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

// StoredDoc is the wire shape of a document.
type StoredDoc struct {
	Key     string          `json:"key"`
	Data    json.RawMessage `json:"data"`
	Created int64           `json:"created,omitempty"`
	Updated int64           `json:"updated,omitempty"`
	Joins   map[string]any  `json:"joins,omitempty"`
}

func validateCollection(c string) error {
	if !collectionRe.MatchString(c) {
		return common.BadRequest("invalid collection name")
	}
	return nil
}

// coerceKey turns a JSON key value (string or number) into its canonical string form.
func coerceKey(v any) (string, error) {
	switch k := v.(type) {
	case string:
		if len(k) == 0 || len(k) > maxKeyBytes || strings.ContainsRune(k, 0) {
			return "", common.BadRequest("invalid key")
		}
		return k, nil
	case float64:
		if k == float64(int64(k)) {
			return strconv.FormatInt(int64(k), 10), nil
		}
		return strconv.FormatFloat(k, 'f', -1, 64), nil
	case json.Number:
		return k.String(), nil
	case nil:
		return "", nil
	}
	return "", common.BadRequest("key must be a string or number")
}

func scatteredID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := binary.BigEndian.Uint64(b[:])
	return fmt.Sprintf("%020d", n)
}

// genKey produces a key for a keyless put according to the instance autoId setting.
func (s *Store) genKey(collection string) (string, error) {
	switch s.autoID {
	case "scattered":
		return scatteredID(), nil
	case "incrementing":
		var next int64
		err := s.db.QueryRow(
			`INSERT INTO _dsseq(collection, next) VALUES(?, 1)
			 ON CONFLICT(collection) DO UPDATE SET next = next + 1 RETURNING next`,
			collection).Scan(&next)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(next, 10), nil
	case "manual":
		return "", common.BadRequest("key is required (autoId=manual)")
	default: // uuid
		return common.UUID(), nil
	}
}

// genKeyTx is like genKey but runs inside a transaction.
func (s *Store) genKeyTx(tx *sql.Tx, collection string) (string, error) {
	switch s.autoID {
	case "scattered":
		return scatteredID(), nil
	case "incrementing":
		var next int64
		err := tx.QueryRow(
			`INSERT INTO _dsseq(collection, next) VALUES(?, 1)
			 ON CONFLICT(collection) DO UPDATE SET next = next + 1 RETURNING next`,
			collection).Scan(&next)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(next, 10), nil
	case "manual":
		return "", common.BadRequest("key is required (autoId=manual)")
	default:
		return common.UUID(), nil
	}
}

// Store is a handle to one namespace's database plus its instance settings.
type Store struct {
	db     *sql.DB
	autoID string
}

// Open returns a Store for (instance, namespace).
func (m *Manager) Open(instanceID, namespace, autoID string) (*Store, error) {
	db, err := m.handle(instanceID, namespace)
	if err != nil {
		return nil, err
	}
	if autoID == "" {
		autoID = "uuid"
	}
	return &Store{db: db, autoID: autoID}, nil
}

type PutDoc struct {
	Key  any             `json:"key"`
	Data json.RawMessage `json:"data"`
}

func isJSONObject(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return len(t) > 0 && t[0] == '{'
}

// Put inserts or replaces documents, returning their keys in input order.
func (s *Store) Put(collection string, docs []PutDoc) ([]string, int, error) {
	if err := validateCollection(collection); err != nil {
		return nil, 0, err
	}
	if len(docs) > maxDocsPerBatch {
		return nil, 0, common.BadRequest(fmt.Sprintf("too many documents (max %d)", maxDocsPerBatch))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	keys := make([]string, 0, len(docs))
	now := nowMS()
	for _, d := range docs {
		if !isJSONObject(d.Data) {
			return nil, 0, common.BadRequest("document data must be a JSON object")
		}
		if len(d.Data) > maxDocBytes {
			return nil, 0, common.NewError(400, "document exceeds 1MiB", "DOCUMENT_TOO_LARGE")
		}
		key, err := coerceKey(d.Key)
		if err != nil {
			return nil, 0, err
		}
		if key == "" {
			if key, err = s.genKey(collection); err != nil {
				return nil, 0, err
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO docs(collection, key, data, size, created, updated) VALUES(?,?,?,?,?,?)
			 ON CONFLICT(collection, key) DO UPDATE SET data=excluded.data, size=excluded.size, updated=excluded.updated`,
			collection, key, string(d.Data), len(d.Data), now, now); err != nil {
			return nil, 0, mapSQLiteErr(err)
		}
		keys = append(keys, key)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, mapSQLiteErr(err)
	}
	return keys, len(docs), nil
}

// Get returns one document by key, or a NOT_FOUND error.
func (s *Store) Get(collection, key string) (*StoredDoc, error) {
	var d StoredDoc
	var data string
	err := s.db.QueryRow(`SELECT key, data, created, updated FROM docs WHERE collection=? AND key=?`,
		collection, key).Scan(&d.Key, &data, &d.Created, &d.Updated)
	if err == sql.ErrNoRows {
		return nil, common.NotFound("document not found")
	}
	if err != nil {
		return nil, err
	}
	d.Data = json.RawMessage(data)
	return &d, nil
}

// BatchGet returns the found documents for the given keys (order unspecified).
func (s *Store) BatchGet(collection string, keys []any) ([]StoredDoc, error) {
	if len(keys) > maxKeysPerGet {
		return nil, common.BadRequest(fmt.Sprintf("too many keys (max %d)", maxKeysPerGet))
	}
	var out []StoredDoc
	// Chunk to stay well under SQLite's parameter cap.
	strKeys := make([]string, 0, len(keys))
	for _, k := range keys {
		s2, err := coerceKey(k)
		if err != nil {
			return nil, err
		}
		strKeys = append(strKeys, s2)
	}
	for i := 0; i < len(strKeys); i += 90 {
		end := min(i+90, len(strKeys))
		chunk := strKeys[i:end]
		ph := strings.Repeat("?,", len(chunk))
		ph = ph[:len(ph)-1]
		args := []any{collection}
		for _, k := range chunk {
			args = append(args, k)
		}
		rows, err := s.db.Query(`SELECT key, data, created, updated FROM docs WHERE collection=? AND key IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d StoredDoc
			var data string
			if err := rows.Scan(&d.Key, &data, &d.Created, &d.Updated); err != nil {
				rows.Close()
				return nil, err
			}
			d.Data = json.RawMessage(data)
			out = append(out, d)
		}
		rows.Close()
	}
	return out, nil
}

// Delete removes documents by key, returning the number removed.
func (s *Store) Delete(collection string, keys []any) (int, error) {
	if len(keys) > maxKeysPerDelete {
		return 0, common.BadRequest(fmt.Sprintf("too many keys (max %d)", maxKeysPerDelete))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	total := 0
	for _, k := range keys {
		sk, err := coerceKey(k)
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`DELETE FROM docs WHERE collection=? AND key=?`, collection, sk)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

func mapSQLiteErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") {
		return common.AlreadyExists("unique constraint violation")
	}
	return err
}
