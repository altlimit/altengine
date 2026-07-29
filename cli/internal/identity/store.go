// Package identity emulates the altengine auth data plane: per-tenant end-user accounts
// (`/v1/auth/{instance}`) that issue identity tokens the browser presents to the datastore
// and channel data planes instead of an org API key. It owns the end-user store, the
// token minting, and the row-level rule engine those data planes enforce.
//
// The package is deliberately named `identity` (not `auth`) because `internal/auth` is the
// API-key/grants package; nothing here depends on the data-plane packages, so datastore
// and channel can import it without an import cycle.
package identity

import (
	"database/sql"
	"encoding/json"
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
	maxEmailBytes  = 320
	maxHandleLen   = 64
	maxStringField = 512
	maxCodeAttempt = 5 // wrong-code guesses before a code self-invalidates
)

// Manager owns one SQLite database per auth instance.
type Manager struct {
	mu     sync.Mutex
	dbs    map[string]*sql.DB
	dir    string
	memory bool
}

// NewManager creates an identity manager. dir "" keeps everything in RAM.
func NewManager(dir string) *Manager {
	return &Manager{dbs: map[string]*sql.DB{}, dir: dir, memory: dir == ""}
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitize(s string) string { return sanitizeRe.ReplaceAllString(s, "_") }

func nowMS() int64 { return time.Now().UnixMilli() }

// Open returns the store for an auth instance, creating its database on first use.
func (m *Manager) Open(instanceID string) (*Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if db, ok := m.dbs[instanceID]; ok {
		return &Store{db: db}, nil
	}
	var dsn string
	if m.memory {
		// A named shared-cache in-memory database keeps one logical store per instance.
		dsn = "file:auth_" + sanitize(instanceID) + "?mode=memory&cache=shared"
	} else {
		dir := filepath.Join(m.dir, "auth")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(dir, sanitize(instanceID)+".db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the store is single-writer
	if err := ensureSchema(db); err != nil {
		return nil, err
	}
	m.dbs[instanceID] = db
	return &Store{db: db}, nil
}

// Drop closes and removes an instance's store (used when the instance is deleted).
func (m *Manager) Drop(instanceID string) {
	m.mu.Lock()
	if db, ok := m.dbs[instanceID]; ok {
		_ = db.Close()
		delete(m.dbs, instanceID)
	}
	m.mu.Unlock()
	if !m.memory {
		_ = os.Remove(filepath.Join(m.dir, "auth", sanitize(instanceID)+".db"))
	}
}

func ensureSchema(db *sql.DB) error {
	stmts := []string{
		// One row per end-user. The unique login handle lives in `identifier` — it may be
		// an email OR a username depending on the instance's configured identity field.
		//
		// There are TWO bags, and the split is a security boundary:
		//   - `profile`: everything the USER supplied at signup (name, etc.). Self-asserted,
		//                NEVER authoritative. Rules read it as `$auth.profile.X` — safe to
		//                display/stamp, never to authorize on.
		//   - `claims`:  server/admin-set only. Authoritative. Rules read it as
		//                `$auth.claims.X`. A user can never write here, so an authorization
		//                rule that trusts `$auth.claims.role` cannot be satisfied by a
		//                self-chosen signup value.
		`CREATE TABLE IF NOT EXISTS users (
			uid        TEXT PRIMARY KEY,
			identifier TEXT NOT NULL UNIQUE,
			pw_hash    TEXT NOT NULL,
			profile    TEXT NOT NULL DEFAULT '{}',
			claims     TEXT NOT NULL DEFAULT '{}',
			disabled   INTEGER NOT NULL DEFAULT 0,
			created    INTEGER NOT NULL,
			updated    INTEGER NOT NULL
		)`,
		// Only the hash of a refresh token is stored, so reading the database never
		// yields a usable credential.
		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			token_hash TEXT PRIMARY KEY,
			uid        TEXT NOT NULL,
			exp        INTEGER NOT NULL,
			revoked    INTEGER NOT NULL DEFAULT 0,
			created    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS refresh_by_uid ON refresh_tokens(uid)`,
		// One-time email codes (passwordless sign-in + password reset), hashed at rest,
		// single-use, short-TTL, with a bounded number of guesses per issued code.
		`CREATE TABLE IF NOT EXISTS email_codes (
			id        TEXT PRIMARY KEY,
			uid       TEXT NOT NULL,
			purpose   TEXT NOT NULL,
			code_hash TEXT NOT NULL,
			exp       INTEGER NOT NULL,
			consumed  INTEGER NOT NULL DEFAULT 0,
			attempts  INTEGER NOT NULL DEFAULT 0,
			created   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS email_codes_by_uid ON email_codes(uid, purpose)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	// One-time migration for stores created before the profile/claims split: add the
	// `profile` column and MOVE the old signup data into it, resetting `claims` to the new
	// authoritative-empty default. The whole point of the split is that a user can't write
	// `claims`, so old signup-populated claims must not survive as authoritative data.
	// Probe with PRAGMA rather than catching a failed ALTER — the error text isn't portable.
	// Fresh stores already have the column (CREATE TABLE above), so this is a no-op for them.
	has, err := hasColumn(db, "users", "profile")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN profile TEXT NOT NULL DEFAULT '{}'`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE users SET profile = claims, claims = '{}'`); err != nil {
			return err
		}
	}
	return nil
}

// hasColumn reports whether `table` has a column named `col`, via PRAGMA table_info.
func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Store is a handle to one auth instance's database.
type Store struct{ db *sql.DB }

// UserRow is the stored end-user record. `Profile` is the user-supplied signup bag;
// `Claims` is the server/admin-set authoritative bag (see the security note on the schema).
type UserRow struct {
	UID        string
	Identifier string
	PwHash     string
	Profile    map[string]any
	Claims     map[string]any
	Disabled   bool
	Created    int64
	Updated    int64
}

// PublicUser is the wire shape of a user (snake_case JSON, matching the hosted API).
type PublicUser struct {
	UID         string         `json:"uid"`
	Identifier  string         `json:"identifier"`
	Profile     map[string]any `json:"profile"` // user-supplied (signup)
	Claims      map[string]any `json:"claims"`  // server/admin-set (authoritative)
	Disabled    bool           `json:"disabled"`
	TotpEnabled bool           `json:"totpEnabled"`
	Created     int64          `json:"created"`
	Updated     int64          `json:"updated"`
}

func (r *UserRow) public() PublicUser {
	p := r.Profile
	if p == nil {
		p = map[string]any{}
	}
	c := r.Claims
	if c == nil {
		c = map[string]any{}
	}
	return PublicUser{UID: r.UID, Identifier: r.Identifier, Profile: p, Claims: c, Disabled: r.Disabled,
		TotpEnabled: false, Created: r.Created, Updated: r.Updated}
}

// NormalizeEmail lowercases/trims an address and applies a deliberately permissive check
// (a full RFC validation belongs to a verification email, not to storage).
func NormalizeEmail(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest("email is required")
	}
	e := strings.ToLower(strings.TrimSpace(s))
	if e == "" || len(e) > maxEmailBytes {
		return "", common.BadRequest("email must be 1.." + itoa(maxEmailBytes) + " bytes")
	}
	if !emailRe.MatchString(e) {
		return "", common.BadRequest("email is not a valid address")
	}
	return e, nil
}

var (
	emailRe  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// NormalizeHandle lowercases/trims a username-style login handle.
func NormalizeHandle(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest("identifier is required")
	}
	h := strings.ToLower(strings.TrimSpace(s))
	if h == "" || len(h) > maxHandleLen {
		return "", common.BadRequest("identifier must be 1.." + itoa(maxHandleLen) + " characters")
	}
	if !handleRe.MatchString(h) {
		return "", common.BadRequest("identifier may contain only letters, numbers, and . _ - (and must start alphanumeric)")
	}
	return h, nil
}

// NormalizeIdentifier normalizes the login handle for the instance's identity-field type.
func NormalizeIdentifier(v any, typ string) (string, error) {
	if typ == "string" {
		return NormalizeHandle(v)
	}
	return NormalizeEmail(v)
}

func itoa(n int) string { return strconv.Itoa(n) }

// mustJSON marshals a claims object, falling back to an empty object.
func mustJSON(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseJSONObject decodes a stored claims blob; malformed JSON reads as empty.
func parseJSONObject(raw string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// CreateUser inserts an end-user. The signup-collected fields go to `profile` (self-asserted);
// new accounts have NO authoritative `claims` (admin-set only), so it starts empty. Returns
// 409 ALREADY_EXISTS when the handle is taken.
func (s *Store) CreateUser(identifier, pwHash string, profile map[string]any, now int64) (*UserRow, error) {
	uid := common.UUID()
	profileJSON := mustJSON(profile)
	_, err := s.db.Exec(
		`INSERT INTO users (uid, identifier, pw_hash, profile, claims, disabled, created, updated) VALUES (?,?,?,?,'{}',0,?,?)`,
		uid, identifier, pwHash, profileJSON, now, now)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "unique") || strings.Contains(msg, "constraint") {
			return nil, common.AlreadyExists("an account with that identifier already exists")
		}
		return nil, err
	}
	return &UserRow{UID: uid, Identifier: identifier, PwHash: pwHash, Profile: profile, Claims: map[string]any{}, Created: now, Updated: now}, nil
}

func (s *Store) scanUser(row *sql.Row) (*UserRow, error) {
	var r UserRow
	var profile, claims string
	var disabled int
	err := row.Scan(&r.UID, &r.Identifier, &r.PwHash, &profile, &claims, &disabled, &r.Created, &r.Updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Disabled = disabled == 1
	r.Profile = parseJSONObject(profile)
	r.Claims = parseJSONObject(claims)
	return &r, nil
}

// ByIdentifier looks a user up by their unique login handle (nil when absent).
func (s *Store) ByIdentifier(identifier string) (*UserRow, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT uid, identifier, pw_hash, profile, claims, disabled, created, updated FROM users WHERE identifier = ?`, identifier))
}

// ByUID looks a user up by uid (nil when absent).
func (s *Store) ByUID(uid string) (*UserRow, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT uid, identifier, pw_hash, profile, claims, disabled, created, updated FROM users WHERE uid = ?`, uid))
}

// ListUsers returns end users newest-first, for the local console's browser. The password
// hash never leaves the store — callers get the public shape only.
func (s *Store) ListUsers(limit int) ([]PublicUser, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT uid, identifier, pw_hash, profile, claims, disabled, created, updated
		   FROM users ORDER BY created DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicUser{}
	for rows.Next() {
		var r UserRow
		var profile, claims string
		var disabled int
		if err := rows.Scan(&r.UID, &r.Identifier, &r.PwHash, &profile, &claims, &disabled, &r.Created, &r.Updated); err != nil {
			return nil, err
		}
		r.Disabled = disabled == 1
		r.Profile = parseJSONObject(profile)
		r.Claims = parseJSONObject(claims)
		out = append(out, r.public())
	}
	return out, rows.Err()
}

// SetClaims replaces an end user's AUTHORITATIVE claims (the admin-only surface). These are
// what rules may safely authorize on via `$auth.claims.X` — a user has no path to write here.
// It is also the backfill path for accounts created before a needed claim existed (a rule
// that references a missing claim is a hard deny).
func (s *Store) SetClaims(uid string, claims map[string]any, now int64) (bool, error) {
	if claims == nil {
		claims = map[string]any{}
	}
	blob, err := json.Marshal(claims)
	if err != nil {
		return false, common.BadRequest("claims must be a JSON object")
	}
	res, err := s.db.Exec(`UPDATE users SET claims = ?, updated = ? WHERE uid = ?`, string(blob), now, uid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetProfile replaces an end user's PROFILE (the self-asserted signup bag). Safe to expose as
// a self-service "edit my profile" path — profile is never authoritative, so a user editing it
// can't escalate. Rules read it as `$auth.profile.X`.
func (s *Store) SetProfile(uid string, profile map[string]any, now int64) (bool, error) {
	if profile == nil {
		profile = map[string]any{}
	}
	blob, err := json.Marshal(profile)
	if err != nil {
		return false, common.BadRequest("profile must be a JSON object")
	}
	res, err := s.db.Exec(`UPDATE users SET profile = ?, updated = ? WHERE uid = ?`, string(blob), now, uid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUser removes an end user along with their outstanding refresh tokens. Reports
// whether a user was actually removed.
func (s *Store) DeleteUser(uid string) (bool, error) {
	if _, err := s.db.Exec(`DELETE FROM refresh_tokens WHERE uid = ?`, uid); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM users WHERE uid = ?`, uid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetPassword replaces a user's password hash and revokes every outstanding refresh
// token (a reset must invalidate sessions minted with the old credential).
func (s *Store) SetPassword(uid, pwHash string, now int64) error {
	if _, err := s.db.Exec(`UPDATE users SET pw_hash = ?, updated = ? WHERE uid = ?`, pwHash, now, uid); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE refresh_tokens SET revoked = 1 WHERE uid = ?`, uid)
	return err
}

// --- refresh tokens ---

// AddRefresh records a newly-minted refresh token (by hash).
func (s *Store) AddRefresh(token, uid string, exp, now int64) error {
	_, err := s.db.Exec(`INSERT INTO refresh_tokens (token_hash, uid, exp, revoked, created) VALUES (?,?,?,0,?)`,
		hashSecret(token), uid, exp, now)
	return err
}

// RotateRefresh consumes a live refresh token and issues its replacement atomically,
// returning the uid it belonged to ("" when the token is unknown, revoked, or expired).
func (s *Store) RotateRefresh(oldToken, newToken string, exp, now int64) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var uid string
	var revoked int
	var oldExp int64
	err = tx.QueryRow(`SELECT uid, revoked, exp FROM refresh_tokens WHERE token_hash = ?`, hashSecret(oldToken)).
		Scan(&uid, &revoked, &oldExp)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if revoked == 1 || oldExp*1000 <= now {
		return "", nil
	}
	if _, err := tx.Exec(`UPDATE refresh_tokens SET revoked = 1 WHERE token_hash = ?`, hashSecret(oldToken)); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO refresh_tokens (token_hash, uid, exp, revoked, created) VALUES (?,?,?,0,?)`,
		hashSecret(newToken), uid, exp, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return uid, nil
}

// RevokeRefresh revokes one refresh token (sign-out). Unknown tokens are a no-op.
func (s *Store) RevokeRefresh(token string) error {
	_, err := s.db.Exec(`UPDATE refresh_tokens SET revoked = 1 WHERE token_hash = ?`, hashSecret(token))
	return err
}

// --- one-time email codes ---

// StartEmailCode issues a code for (uid, purpose). Returns false without issuing when a
// live code is already outstanding — the "one code per TTL window" rule that stops
// mail-bombing an address.
func (s *Store) StartEmailCode(uid, purpose, code string, exp, now int64) (bool, error) {
	nowSec := now / 1000
	var id string
	err := s.db.QueryRow(`SELECT id FROM email_codes WHERE uid = ? AND purpose = ? AND consumed = 0 AND exp > ? LIMIT 1`,
		uid, purpose, nowSec).Scan(&id)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if _, err := s.db.Exec(`DELETE FROM email_codes WHERE uid = ? AND purpose = ?`, uid, purpose); err != nil {
		return false, err
	}
	_, err = s.db.Exec(
		`INSERT INTO email_codes (id, uid, purpose, code_hash, exp, consumed, attempts, created) VALUES (?,?,?,?,?,0,0,?)`,
		common.UUID(), uid, purpose, hashSecret(code), exp, now)
	return err == nil, err
}

// ConsumeEmailCode verifies and burns a code. A wrong guess increments `attempts` and the
// code self-invalidates once the guess budget is spent.
func (s *Store) ConsumeEmailCode(uid, purpose, code string, now int64) (bool, error) {
	nowSec := now / 1000
	var id, storedHash string
	var attempts int
	err := s.db.QueryRow(
		`SELECT id, code_hash, attempts FROM email_codes WHERE uid = ? AND purpose = ? AND consumed = 0 AND exp > ?
		 ORDER BY created DESC LIMIT 1`, uid, purpose, nowSec).Scan(&id, &storedHash, &attempts)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedHash == hashSecret(code) {
		_, err := s.db.Exec(`UPDATE email_codes SET consumed = 1 WHERE id = ?`, id)
		return err == nil, err
	}
	attempts++
	if attempts >= maxCodeAttempt {
		_, err = s.db.Exec(`UPDATE email_codes SET consumed = 1 WHERE id = ?`, id)
	} else {
		_, err = s.db.Exec(`UPDATE email_codes SET attempts = ? WHERE id = ?`, attempts, id)
	}
	return false, err
}
