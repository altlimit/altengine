// Package search emulates the altengine search data plane — an App Engine Search API
// feature-parity service. It uses one SQLite database per instance+namespace with
// per-index table prefixes (FTS5 for text plus a typed EAV table for range/sort/facet
// values) so the boolean query language, facets, sorting and pagination behave the
// same as the hosted service.
package search

import (
	"database/sql"
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
	maxDocsPerBatch = 200
	maxIDsPerDelete = 200
	maxAtomBytes    = 500
	// rank default epoch: seconds since 2011-01-01Z.
	rankEpoch = 1293840000
)

var (
	fieldNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	docIDRe     = regexp.MustCompile(`^[\x20-\x7e]{1,500}$`)
	indexNameRe = regexp.MustCompile(`^[\x20-\x7e]{1,100}$`)
	reservedRe  = regexp.MustCompile(`^__.*__$`)
)

// Manager owns the per-(instance,namespace) SQLite databases.
type Manager struct {
	mu     sync.Mutex
	dbs    map[string]*sql.DB
	dir    string
	memory bool
}

func NewManager(dir string) *Manager {
	return &Manager{dbs: map[string]*sql.DB{}, dir: dir, memory: dir == ""}
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitize(s string) string {
	if s == "" {
		return "_default"
	}
	return sanitizeRe.ReplaceAllString(s, "_")
}

func (m *Manager) handle(instanceID, namespace string) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := instanceID + "\x00" + namespace
	if db, ok := m.dbs[key]; ok {
		return db, nil
	}
	var dsn string
	if m.memory {
		dsn = "file:s_" + sanitize(key) + "?mode=memory&cache=shared"
	} else {
		instDir := filepath.Join(m.dir, "search", sanitize(instanceID))
		if err := os.MkdirAll(instDir, 0o755); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(instDir, sanitize(namespace)+".db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _indexes (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL, created INTEGER NOT NULL)`); err != nil {
		return nil, err
	}
	m.dbs[key] = db
	return db, nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

// Index is a handle to one search index.
type Index struct {
	db       *sql.DB
	id       int64
	name     string
	prefix   string
	stemming bool
	// Instance-level query-time config (see routes.go resolve): the synonym dictionary
	// (incl. computed numerals) applied during term compilation.
	syn     *SynonymConfig
	synDict map[string][]string
}

// Store scopes index operations to one (instance, namespace).
type Store struct {
	db       *sql.DB
	stemming bool
	// Instance-level query-time config, set by the route layer after Open (same package).
	syn     *SynonymConfig
	synDict map[string][]string
	rules   []SearchRule
}

func (m *Manager) Open(instanceID, namespace string, stemming bool) (*Store, error) {
	db, err := m.handle(instanceID, namespace)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, stemming: stemming}, nil
}

// ApplyConfig threads the instance's query-time config (the synonym dictionary incl.
// computed numerals, and the query rules) into this store. This is the SINGLE entry
// point — the data plane (routes.go) and the admin browser (admin/browsers.go) both call
// it, so neither path can drift into silently ignoring instance config. Tolerant parse:
// malformed config degrades to none.
func (s *Store) ApplyConfig(cfg map[string]any) {
	if cfg == nil {
		return
	}
	s.syn = parseSynonyms(cfg["synonyms"])
	s.synDict = buildSynDict(s.syn)
	s.rules = parseRules(cfg["rules"])
}

func validateIndexName(name string) error {
	if !indexNameRe.MatchString(name) || strings.HasPrefix(name, "!") || reservedRe.MatchString(name) {
		return common.BadRequest("invalid index name")
	}
	return nil
}

// ensureIndex creates the index (lazily) and returns a handle.
func (s *Store) ensureIndex(name string) (*Index, error) {
	if err := validateIndexName(name); err != nil {
		return nil, err
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM _indexes WHERE name=?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		res, err := s.db.Exec(`INSERT INTO _indexes(name, created) VALUES(?,?)`, name, nowMS())
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
		if err := s.createTables(id); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &Index{db: s.db, id: id, name: name, prefix: fmt.Sprintf("ix%d", id), stemming: s.stemming, syn: s.syn, synDict: s.synDict}, nil
}

// getIndex returns an existing index handle or nil (no creation).
func (s *Store) getIndex(name string) (*Index, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM _indexes WHERE name=?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Index{db: s.db, id: id, name: name, prefix: fmt.Sprintf("ix%d", id), stemming: s.stemming, syn: s.syn, synDict: s.synDict}, nil
}

func (s *Store) createTables(id int64) error {
	p := fmt.Sprintf("ix%d", id)
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE %s_docs (doc_id TEXT PRIMARY KEY, rank INTEGER NOT NULL, lang TEXT, body TEXT NOT NULL)`, p),
		fmt.Sprintf(`CREATE TABLE %s_fields (doc_id TEXT, name TEXT, type TEXT, text_lc TEXT, num_val REAL, lat REAL, lng REAL)`, p),
		fmt.Sprintf(`CREATE INDEX %s_fields_dn ON %s_fields(name, type)`, p, p),
		fmt.Sprintf(`CREATE INDEX %s_fields_doc ON %s_fields(doc_id)`, p, p),
		fmt.Sprintf(`CREATE VIRTUAL TABLE %s_fts USING fts5(content, doc_id UNINDEXED, name UNINDEXED, tokenize='unicode61')`, p),
		fmt.Sprintf(`CREATE TABLE %s_facets (doc_id TEXT, name TEXT, kind INTEGER, text_val TEXT, num_val REAL)`, p),
		fmt.Sprintf(`CREATE INDEX %s_facets_n ON %s_facets(name)`, p, p),
		fmt.Sprintf(`CREATE TABLE %s_schema (name TEXT, type TEXT, PRIMARY KEY(name, type))`, p),
	}
	if s.stemming {
		stmts = append(stmts, fmt.Sprintf(`CREATE VIRTUAL TABLE %s_fts_stem USING fts5(content, doc_id UNINDEXED, name UNINDEXED, tokenize='porter unicode61')`, p))
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return err
		}
	}
	return nil
}

// ListIndexes returns index registry rows matching q (name substring), newest first.
func (s *Store) ListIndexes(q string, limit int) ([]map[string]any, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT name, created FROM _indexes WHERE name LIKE ? ORDER BY created DESC LIMIT ?`,
		"%"+q+"%", limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var name string
		var created int64
		if err := rows.Scan(&name, &created); err != nil {
			return nil, false, err
		}
		out = append(out, map[string]any{"name": name, "namespace": "", "created_at": created})
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, hasMore, nil
}

// DropIndex removes an index and its tables. Returns whether it existed.
func (s *Store) DropIndex(name string) (bool, error) {
	ix, err := s.getIndex(name)
	if err != nil || ix == nil {
		return false, err
	}
	p := ix.prefix
	for _, t := range []string{"_docs", "_fields", "_fts", "_facets", "_schema", "_fts_stem"} {
		s.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s%s`, p, t))
	}
	s.db.Exec(`DELETE FROM _indexes WHERE id=?`, ix.id)
	return true, nil
}

// Schema returns the union field schema (name -> list of types).
func (s *Store) Schema(name string) (map[string][]string, error) {
	ix, err := s.getIndex(name)
	if err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, common.NotFound("index not found")
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT name, type FROM %s_schema ORDER BY name`, ix.prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var n, t string
		if err := rows.Scan(&n, &t); err != nil {
			return nil, err
		}
		out[n] = append(out[n], t)
	}
	return out, nil
}

// schemaTypes loads the field->types map for query planning.
func (ix *Index) schemaTypes() (map[string]map[string]bool, error) {
	rows, err := ix.db.Query(fmt.Sprintf(`SELECT name, type FROM %s_schema`, ix.prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var n, t string
		if err := rows.Scan(&n, &t); err != nil {
			return nil, err
		}
		if out[n] == nil {
			out[n] = map[string]bool{}
		}
		out[n][t] = true
	}
	return out, nil
}

var _ = json.Marshal
var _ = strconv.Itoa
