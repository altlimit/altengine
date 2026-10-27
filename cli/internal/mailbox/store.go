package mailbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/altlimit/altengine/cli/internal/common"
	_ "modernc.org/sqlite"
)

// The store holds, per mailbox instance, every message it has received or sent, the addresses
// that carry a setting, the day's counters, what each trigger did, and the instance's webhook
// signing key — one SQLite database per instance, like the hosted per-instance store. The BYTES
// (the raw .eml and every attachment) are files beside the database, or memory without a data
// directory. Bodies are inline up to BodyMax each, as hosted.

const (
	// BodyMax is the inline body cap per part, in UTF-16 code units as hosted.
	BodyMax = 512 * 1024
	// MaxListLimit / defaultListLimit bound a listing.
	MaxListLimit     = 100
	defaultListLimit = 25
	// MaxWaiters is long-polls parked at once per instance; past it a waiter is answered empty.
	MaxWaiters = 50
	// MaxWaitMS is the longest a long-poll is held.
	MaxWaitMS = 30_000
	// MaxDeliveryAttempts is attempts before a delivery is given up on.
	MaxDeliveryAttempts = 5
)

// retryAfter is the wait before each retry, as hosted.
var retryAfter = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// RetryAt is when the next attempt is due after `attempts` failures, or 0 when there is none left.
func RetryAt(attempts int, now int64) int64 {
	if attempts >= MaxDeliveryAttempts {
		return 0
	}
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(retryAfter) {
		i = len(retryAfter) - 1
	}
	return now + retryAfter[i].Milliseconds()
}

// AddressValue is one mailbox (address and display name).
type AddressValue struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

// Attachment is one attachment's record. Key is where its bytes are stored, "" when they were
// not (a rejected part).
type Attachment struct {
	Idx         int     `json:"idx"`
	Filename    *string `json:"filename"`
	ContentType string  `json:"contentType"`
	Size        int64   `json:"size"`
	ContentID   *string `json:"contentId"`
	Disposition *string `json:"disposition"`
	Rejected    *string `json:"rejected"`
	Key         string  `json:"-"`
}

// Summary is a message as every list returns it: everything but the bodies.
type Summary struct {
	ID              string         `json:"id"`
	Direction       string         `json:"direction"`
	Local           string         `json:"local"`
	PlusTag         *string        `json:"plusTag"`
	From            AddressValue   `json:"from"`
	To              []AddressValue `json:"to"`
	Cc              []AddressValue `json:"cc"`
	ReplyTo         []AddressValue `json:"replyTo"`
	Subject         string         `json:"subject"`
	Snippet         string         `json:"snippet"`
	MessageID       *string        `json:"messageId"`
	InReplyTo       *string        `json:"inReplyTo"`
	Verified        bool           `json:"verified"`
	Status          string         `json:"status"`
	Size            int64          `json:"size"`
	AttachmentCount int            `json:"attachmentCount"`
	ReceivedAt      int64          `json:"receivedAt"`
	ReadAt          *int64         `json:"readAt"`
	ExpiresAt       int64          `json:"expiresAt"`
}

// Delivery is what one route action did for one message.
type Delivery struct {
	Action   string `json:"action"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	NextAt   *int64 `json:"nextAt"`
}

// Record is a full message.
type Record struct {
	Summary
	Text          *string      `json:"text"`
	HTML          *string      `json:"html"`
	TextTruncated bool         `json:"textTruncated"`
	HTMLTruncated bool         `json:"htmlTruncated"`
	References    *string      `json:"references"`
	AuthResults   *string      `json:"authResults"`
	RawKey        string       `json:"-"`
	Attachments   []Attachment `json:"attachments"`
	Deliveries    []Delivery   `json:"deliveries"`
}

// NewMessage is what the inbound and send paths hand over to be stored.
type NewMessage struct {
	ID          string
	Direction   string
	Local       string
	PlusTag     string
	From        AddressValue
	To, Cc      []AddressValue
	ReplyTo     []AddressValue
	Subject     string
	Text, HTML  *string
	MessageID   string
	InReplyTo   string
	References  string
	AuthResults string
	Verified    bool
	Status      string
	RawKey      string
	Size        int64
	StoredBytes int64
	Attachments []Attachment
	ReceivedAt  int64
	ExpiresAt   int64
}

// AddressRecord is an address that carries a setting.
type AddressRecord struct {
	Local      string         `json:"local"`
	Label      *string        `json:"label"`
	Meta       map[string]any `json:"meta"`
	Registered bool           `json:"registered"`
	CreatedAt  int64          `json:"createdAt"`
	DisabledAt *int64         `json:"disabledAt"`
}

// AdmitResult is the answer to "is this message taken".
type AdmitResult struct {
	OK     bool
	Code   int
	Reason string
}

// ListOptions select one page of messages.
type ListOptions struct {
	Local     string
	Cursor    string // newest-first: older than this id
	After     string // oldest-first: newer than this id
	Limit     int
	Unread    bool
	Direction string
}

// Clip truncates to at most max UTF-16 code units without splitting a surrogate pair — the
// unit the hosted service measures in.
func Clip(s *string, max int) (*string, bool) {
	if s == nil {
		return nil, false
	}
	u := utf16.Encode([]rune(*s))
	if len(u) <= max {
		return s, false
	}
	end := max
	if c := u[end-1]; c >= 0xd800 && c <= 0xdbff {
		end--
	}
	v := string(utf16.Decode(u[:end]))
	return &v, true
}

var (
	styleScriptRe = regexp.MustCompile(`(?is)<style.*?</style>|<script.*?</script>`)
	tagRe2        = regexp.MustCompile(`<[^>]+>`)
	spaceRe       = regexp.MustCompile(`\s+`)
)

// SnippetOf is a one-line preview: the text body, whitespace collapsed, bounded.
func SnippetOf(text, html *string) string {
	src := ""
	switch {
	case text != nil:
		src = *text
	case html != nil:
		src = tagRe2.ReplaceAllString(styleScriptRe.ReplaceAllString(*html, " "), " ")
	}
	src = strings.TrimSpace(spaceRe.ReplaceAllString(src, " "))
	v, _ := Clip(&src, 200)
	return *v
}

func base36(ms int64) string {
	s := strconv.FormatInt(ms, 36)
	for len(s) < 9 {
		s = "0" + s
	}
	return s
}

// NewMessageID is time-ordered and fixed-width: base36 milliseconds (9 chars) then random hex.
// Lexicographic order is creation order, so an id IS a keyset cursor.
func NewMessageID(now int64) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return base36(now) + hex.EncodeToString(b)
}

// ResolveAfter turns `now` into the current time's id prefix; anything else is an id as given.
func ResolveAfter(v string, now int64) string {
	if v == "now" {
		return base36(now)
	}
	return v
}

// UTCDay is the day a moment falls in, as the counter key.
func UTCDay(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

func clampLimit(n int) int {
	if n < 1 {
		return defaultListLimit
	}
	if n > MaxListLimit {
		return MaxListLimit
	}
	return n
}

type waiter struct {
	local string
	after string
	ch    chan Summary
}

// Store is every mailbox instance's data.
type Store struct {
	mu      sync.Mutex
	dir     string
	dbs     map[string]*sql.DB
	bytes   map[string][]byte // memory mode: instanceID + "/" + key
	waiters map[string]map[*waiter]bool
	// counters serializes the read-then-write of a day counter. A single writer makes the cap a
	// bound under concurrent deliveries, as the hosted single-threaded object does.
	counters sync.Mutex
}

// NewStore opens the store. dir "" keeps everything in memory.
func NewStore(dir string) *Store {
	return &Store{dir: dir, dbs: map[string]*sql.DB{}, bytes: map[string][]byte{}, waiters: map[string]map[*waiter]bool{}}
}

func (s *Store) memory() bool { return s.dir == "" }

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitize(v string) string { return sanitizeRe.ReplaceAllString(v, "_") }

func (s *Store) instDir(instanceID string) string {
	return filepath.Join(s.dir, "mailbox", sanitize(instanceID))
}

const schema = `
CREATE TABLE IF NOT EXISTS message (
  id TEXT PRIMARY KEY, direction TEXT NOT NULL, local TEXT NOT NULL, plus_tag TEXT,
  from_addr TEXT NOT NULL, from_name TEXT, to_json TEXT NOT NULL DEFAULT '[]', cc_json TEXT NOT NULL DEFAULT '[]',
  reply_to_json TEXT NOT NULL DEFAULT '[]', subject TEXT NOT NULL DEFAULT '', snippet TEXT NOT NULL DEFAULT '',
  text TEXT, html TEXT, text_truncated INTEGER NOT NULL DEFAULT 0, html_truncated INTEGER NOT NULL DEFAULT 0,
  message_id TEXT, in_reply_to TEXT, refs TEXT, auth_results TEXT, verified INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL, raw_key TEXT, size INTEGER NOT NULL DEFAULT 0, stored_bytes INTEGER NOT NULL DEFAULT 0,
  attachment_count INTEGER NOT NULL DEFAULT 0, received_at INTEGER NOT NULL, read_at INTEGER, expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS message_local ON message(local, id);
CREATE INDEX IF NOT EXISTS message_expires ON message(expires_at);
CREATE INDEX IF NOT EXISTS message_from_in ON message(from_addr) WHERE direction = 'in' AND verified = 1;
CREATE TABLE IF NOT EXISTS attachment (
  message_id TEXT NOT NULL, idx INTEGER NOT NULL, filename TEXT, content_type TEXT NOT NULL, size INTEGER NOT NULL,
  file_key TEXT, content_id TEXT, disposition TEXT, rejected TEXT, PRIMARY KEY (message_id, idx));
CREATE TABLE IF NOT EXISTS address (
  local TEXT PRIMARY KEY, label TEXT, meta TEXT NOT NULL DEFAULT '{}', registered INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, disabled_at INTEGER);
CREATE TABLE IF NOT EXISTS day_count (kind TEXT NOT NULL, day TEXT NOT NULL, n INTEGER NOT NULL, PRIMARY KEY (kind, day));
CREATE TABLE IF NOT EXISTS delivery (
  message_id TEXT NOT NULL, action TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER, PRIMARY KEY (message_id, action));
CREATE INDEX IF NOT EXISTS delivery_due ON delivery(status, next_at);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

func (s *Store) db(instanceID string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.dbs[instanceID]; ok {
		return db, nil
	}
	var dsn string
	if s.memory() {
		dsn = "file:mb_" + hex.EncodeToString([]byte(instanceID)) + "?mode=memory&cache=shared"
	} else {
		d := s.instDir(instanceID)
		if err := os.MkdirAll(d, common.DataDirPerm); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(d, "mailbox.db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s.dbs[instanceID] = db
	return db, nil
}

// --- bytes ---------------------------------------------------------------------------------

// RawKey / AttachmentKey name where a message's bytes are stored within its instance.
func RawKey(messageID string) string                 { return messageID + ".eml" }
func AttachmentKey(messageID string, idx int) string { return fmt.Sprintf("%s/%d", messageID, idx) }

func (s *Store) bytesPath(instanceID, key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = sanitize(p)
	}
	return filepath.Join(append([]string{s.instDir(instanceID), "files"}, parts...)...)
}

// PutBytes stores one object's bytes.
func (s *Store) PutBytes(instanceID, key string, b []byte) error {
	if s.memory() {
		s.mu.Lock()
		s.bytes[instanceID+"/"+key] = append([]byte(nil), b...)
		s.mu.Unlock()
		return nil
	}
	p := s.bytesPath(instanceID, key)
	if err := os.MkdirAll(filepath.Dir(p), common.DataDirPerm); err != nil {
		return err
	}
	return common.WriteFileAtomic(p, b, common.DataFilePerm)
}

// Bytes reads one object's bytes.
func (s *Store) Bytes(instanceID, key string) ([]byte, bool) {
	if key == "" {
		return nil, false
	}
	if s.memory() {
		s.mu.Lock()
		defer s.mu.Unlock()
		b, ok := s.bytes[instanceID+"/"+key]
		return b, ok
	}
	b, err := os.ReadFile(s.bytesPath(instanceID, key))
	return b, err == nil
}

func (s *Store) deleteBytes(instanceID string, keys []string) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if s.memory() {
			s.mu.Lock()
			delete(s.bytes, instanceID+"/"+k)
			s.mu.Unlock()
			continue
		}
		p := s.bytesPath(instanceID, k)
		_ = os.Remove(p)
		// An attachment lives in a per-message folder; drop it once empty.
		_ = os.Remove(filepath.Dir(p))
	}
}

// --- inbound -------------------------------------------------------------------------------

// Admit decides, before the body is read, whether a message for `local` is taken — and if so
// counts it against today's inbound cap. Counting here rather than at store is what makes the
// cap a bound under concurrent deliveries.
func (s *Store) Admit(instanceID, local string, now int64, acceptMode string, cap int64) (AdmitResult, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return AdmitResult{}, err
	}
	var registered int
	var disabled sql.NullInt64
	err = db.QueryRow(`SELECT registered, disabled_at FROM address WHERE local = ?`, local).Scan(&registered, &disabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AdmitResult{}, err
	}
	if disabled.Valid {
		return AdmitResult{Code: 550, Reason: "550 5.1.1 mailbox disabled"}, nil
	}
	if acceptMode == "registered" && registered != 1 {
		return AdmitResult{Code: 550, Reason: "550 5.1.1 no such mailbox"}, nil
	}
	ok, err := s.takeCount(db, "in", now, cap)
	if err != nil {
		return AdmitResult{}, err
	}
	if !ok {
		return AdmitResult{Code: 452, Reason: "452 4.2.2 mailbox is over its daily limit, try again later"}, nil
	}
	return AdmitResult{OK: true}, nil
}

func (s *Store) takeCount(db *sql.DB, kind string, now, cap int64) (bool, error) {
	s.counters.Lock()
	defer s.counters.Unlock()
	day := UTCDay(now)
	var n int64
	err := db.QueryRow(`SELECT n FROM day_count WHERE kind = ? AND day = ?`, kind, day).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if n >= cap {
		return false, nil
	}
	_, err = db.Exec(`INSERT INTO day_count (kind, day, n) VALUES (?, ?, 1) ON CONFLICT(kind, day) DO UPDATE SET n = n + 1`, kind, day)
	return err == nil, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strPtr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func mustJSON(v any) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func addrs(v []AddressValue) []AddressValue {
	if v == nil {
		return []AddressValue{}
	}
	return v
}

// StoreMessage stores a message (inbound or sent), its attachments and the deliveries its route
// chose, then wakes any long-poll it satisfies.
func (s *Store) StoreMessage(instanceID string, m NewMessage, deliveries []string) (Summary, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return Summary{}, err
	}
	text, textCut := Clip(m.Text, BodyMax)
	html, htmlCut := Clip(m.HTML, BodyMax)
	subject := m.Subject
	if sub, _ := Clip(&subject, 998); sub != nil {
		subject = *sub
	}
	count := 0
	for _, a := range m.Attachments {
		if a.Rejected == nil {
			count++
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO message (id, direction, local, plus_tag, from_addr, from_name, to_json, cc_json, reply_to_json,
		subject, snippet, text, html, text_truncated, html_truncated, message_id, in_reply_to, refs, auth_results,
		verified, status, raw_key, size, stored_bytes, attachment_count, received_at, read_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		m.ID, m.Direction, m.Local, nullStr(m.PlusTag), strings.ToLower(m.From.Address), nullStr(m.From.Name),
		mustJSON(addrs(m.To)), mustJSON(addrs(m.Cc)), mustJSON(addrs(m.ReplyTo)),
		subject, SnippetOf(m.Text, m.HTML), strPtr(text), strPtr(html), b2i(textCut), b2i(htmlCut),
		nullStr(m.MessageID), nullStr(m.InReplyTo), nullStr(m.References), nullStr(m.AuthResults),
		b2i(m.Verified), m.Status, nullStr(m.RawKey), m.Size, m.StoredBytes, count, m.ReceivedAt, m.ExpiresAt)
	if err != nil {
		return Summary{}, err
	}
	for _, a := range m.Attachments {
		if _, err := tx.Exec(`INSERT INTO attachment (message_id, idx, filename, content_type, size, file_key, content_id, disposition, rejected)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.ID, a.Idx, strPtr(a.Filename), a.ContentType, a.Size, nullStr(a.Key),
			strPtr(a.ContentID), strPtr(a.Disposition), strPtr(a.Rejected)); err != nil {
			return Summary{}, err
		}
	}
	for _, action := range deliveries {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO delivery (message_id, action, status, attempts, next_at) VALUES (?, ?, 'pending', 0, ?)`,
			m.ID, action, m.ReceivedAt); err != nil {
			return Summary{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	sum, err := s.summary(db, m.ID)
	if err != nil {
		return Summary{}, err
	}
	if m.Direction == "in" {
		s.wake(instanceID, sum)
	}
	return sum, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

const summaryColumns = `id, direction, local, plus_tag, from_addr, from_name, to_json, cc_json, reply_to_json, subject,
  snippet, message_id, in_reply_to, verified, status, size, attachment_count, received_at, read_at, expires_at`

type scanner interface{ Scan(dest ...any) error }

func scanSummary(r scanner, extra ...any) (Summary, error) {
	var m Summary
	var plus, fromName, msgID, inReply sql.NullString
	var toJ, ccJ, rtJ string
	var verified int
	var readAt sql.NullInt64
	dest := []any{&m.ID, &m.Direction, &m.Local, &plus, &m.From.Address, &fromName, &toJ, &ccJ, &rtJ, &m.Subject,
		&m.Snippet, &msgID, &inReply, &verified, &m.Status, &m.Size, &m.AttachmentCount, &m.ReceivedAt, &readAt, &m.ExpiresAt}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return m, err
	}
	m.PlusTag = ns(plus)
	m.From.Name = fromName.String
	m.MessageID, m.InReplyTo = ns(msgID), ns(inReply)
	m.Verified = verified == 1
	if readAt.Valid {
		v := readAt.Int64
		m.ReadAt = &v
	}
	m.To, m.Cc, m.ReplyTo = parseAddrs(toJ), parseAddrs(ccJ), parseAddrs(rtJ)
	return m, nil
}

func ns(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func parseAddrs(s string) []AddressValue {
	out := []AddressValue{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []AddressValue{}
	}
	return out
}

func (s *Store) summary(db *sql.DB, id string) (Summary, error) {
	return scanSummary(db.QueryRow(`SELECT `+summaryColumns+` FROM message WHERE id = ?`, id))
}

// --- reading -------------------------------------------------------------------------------

// List returns one page. Newest first with Cursor; oldest first with After (what a poller wants).
func (s *Store) List(instanceID string, o ListOptions) ([]Summary, *string, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, nil, err
	}
	limit := clampLimit(o.Limit)
	var where []string
	var args []any
	if o.Local != "" {
		where, args = append(where, "local = ?"), append(args, o.Local)
	}
	if o.Direction != "" {
		where, args = append(where, "direction = ?"), append(args, o.Direction)
	}
	if o.Unread {
		where = append(where, "read_at IS NULL AND direction = 'in'")
	}
	asc := o.After != ""
	if asc {
		where, args = append(where, "id > ?"), append(args, o.After)
	} else if o.Cursor != "" {
		where, args = append(where, "id < ?"), append(args, o.Cursor)
	}
	q := `SELECT ` + summaryColumns + ` FROM message`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if asc {
		q += " ORDER BY id ASC LIMIT ?"
	} else {
		q += " ORDER BY id DESC LIMIT ?"
	}
	rows, err := db.Query(q, append(args, limit+1)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		m, err := scanSummary(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var cursor *string
	if len(out) > limit {
		out = out[:limit]
		c := out[len(out)-1].ID
		cursor = &c
	}
	return out, cursor, nil
}

// Get returns one full message, or nil.
func (s *Store) Get(instanceID, id string) (*Record, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	var rec Record
	var text, html, refs, auth, rawKey sql.NullString
	var tt, ht int
	sum, err := scanSummary(db.QueryRow(`SELECT `+summaryColumns+`, text, html, text_truncated, html_truncated, refs, auth_results, raw_key
		FROM message WHERE id = ?`, id), &text, &html, &tt, &ht, &refs, &auth, &rawKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.Summary = sum
	rec.Text, rec.HTML = ns(text), ns(html)
	rec.TextTruncated, rec.HTMLTruncated = tt == 1, ht == 1
	rec.References, rec.AuthResults = ns(refs), ns(auth)
	rec.RawKey = rawKey.String
	rec.Attachments = []Attachment{}
	rows, err := db.Query(`SELECT idx, filename, content_type, size, file_key, content_id, disposition, rejected FROM attachment WHERE message_id = ? ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Attachment
		var fn, key, cid, disp, rej sql.NullString
		if err := rows.Scan(&a.Idx, &fn, &a.ContentType, &a.Size, &key, &cid, &disp, &rej); err != nil {
			rows.Close()
			return nil, err
		}
		a.Filename, a.ContentID, a.Disposition, a.Rejected = ns(fn), ns(cid), ns(disp), ns(rej)
		a.Key = key.String
		rec.Attachments = append(rec.Attachments, a)
	}
	rows.Close()
	rec.Deliveries = []Delivery{}
	drows, err := db.Query(`SELECT action, status, attempts, next_at FROM delivery WHERE message_id = ? ORDER BY action`, id)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var d Delivery
		var next sql.NullInt64
		if err := drows.Scan(&d.Action, &d.Status, &d.Attempts, &next); err != nil {
			return nil, err
		}
		if next.Valid {
			v := next.Int64
			d.NextAt = &v
		}
		rec.Deliveries = append(rec.Deliveries, d)
	}
	return &rec, drows.Err()
}

// MarkRead marks a message read (once) and reports whether it exists.
func (s *Store) MarkRead(instanceID, id string, now int64) (bool, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return false, err
	}
	res, err := db.Exec(`UPDATE message SET read_at = COALESCE(read_at, ?) WHERE id = ?`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ErrShortenNeedsFull is SetExpiry refusing to move an expiry earlier for a caller that may not.
var ErrShortenNeedsFull = errors.New("shortening an expiry needs full access")

// SetExpiry moves one message's expiry. The caller bounds expiresAt (KeepUntil). Unless
// allowShorten, an expiresAt earlier than the current one is refused with ErrShortenNeedsFull and
// nothing changes — the check and the write are one statement. nil when there is no such message.
func (s *Store) SetExpiry(instanceID, id string, expiresAt int64, allowShorten bool) (*Summary, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	res, err := db.Exec(`UPDATE message SET expires_at = ? WHERE id = ? AND (? OR expires_at <= ?)`, expiresAt, id, allowShorten, expiresAt)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	sum, err := s.summary(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrShortenNeedsFull
	}
	return &sum, nil
}

// Remove deletes messages: bytes first, then rows. Returns how many were removed.
func (s *Store) Remove(instanceID string, ids []string) (int, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, id := range ids {
		var raw sql.NullString
		if err := db.QueryRow(`SELECT raw_key FROM message WHERE id = ?`, id).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return removed, err
		}
		keys := []string{raw.String}
		rows, err := db.Query(`SELECT file_key FROM attachment WHERE message_id = ?`, id)
		if err != nil {
			return removed, err
		}
		for rows.Next() {
			var k sql.NullString
			_ = rows.Scan(&k)
			keys = append(keys, k.String)
		}
		rows.Close()
		s.deleteBytes(instanceID, keys)
		for _, q := range []string{`DELETE FROM attachment WHERE message_id = ?`, `DELETE FROM delivery WHERE message_id = ?`, `DELETE FROM message WHERE id = ?`} {
			if _, err := db.Exec(q, id); err != nil {
				return removed, err
			}
		}
		removed++
	}
	return removed, nil
}

// Expire removes what has passed its expiry, and prunes old day counters.
func (s *Store) Expire(instanceID string, now int64) (int, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return 0, err
	}
	rows, err := db.Query(`SELECT id FROM message WHERE expires_at <= ? ORDER BY expires_at LIMIT 500`, now)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	_, _ = db.Exec(`DELETE FROM day_count WHERE day < ?`, UTCDay(now-3*86_400_000))
	return s.Remove(instanceID, ids)
}

// Stats is the instance's message count and stored bytes.
func (s *Store) Stats(instanceID string) (messages int64, storedBytes int64, err error) {
	db, err := s.db(instanceID)
	if err != nil {
		return 0, 0, err
	}
	var b sql.NullInt64
	err = db.QueryRow(`SELECT COUNT(*), SUM(stored_bytes) FROM message`).Scan(&messages, &b)
	return messages, b.Int64, err
}

// --- long-poll -----------------------------------------------------------------------------

// WaitFor returns inbound messages after `after` (for `local`, when given), waiting up to
// timeout for the first one when there is none yet. Empty on timeout.
func (s *Store) WaitFor(ctx context.Context, instanceID, local, after string, limit int, timeout time.Duration) ([]Summary, error) {
	now, _, err := s.List(instanceID, ListOptions{Local: local, After: after, Limit: limit, Direction: "in"})
	if err != nil || len(now) > 0 || timeout <= 0 {
		return now, err
	}
	if timeout > MaxWaitMS*time.Millisecond {
		timeout = MaxWaitMS * time.Millisecond
	}
	w := &waiter{local: local, after: after, ch: make(chan Summary, 1)}
	s.mu.Lock()
	if len(s.waiters[instanceID]) >= MaxWaiters {
		s.mu.Unlock()
		return []Summary{}, nil
	}
	if s.waiters[instanceID] == nil {
		s.waiters[instanceID] = map[*waiter]bool{}
	}
	s.waiters[instanceID][w] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters[instanceID], w)
		s.mu.Unlock()
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case m := <-w.ch:
		if m.ID == "" { // the instance was dropped
			return []Summary{}, nil
		}
		return []Summary{m}, nil
	case <-t.C:
	case <-ctx.Done():
	}
	return []Summary{}, nil
}

func (s *Store) wake(instanceID string, m Summary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.waiters[instanceID] {
		if w.local != "" && w.local != m.Local {
			continue
		}
		if m.ID <= w.after {
			continue
		}
		select {
		case w.ch <- m:
		default:
		}
		delete(s.waiters[instanceID], w)
	}
}

// --- addresses -----------------------------------------------------------------------------

func scanAddress(r scanner) (*AddressRecord, error) {
	var a AddressRecord
	var label sql.NullString
	var meta string
	var reg int
	var disabled sql.NullInt64
	if err := r.Scan(&a.Local, &label, &meta, &reg, &a.CreatedAt, &disabled); err != nil {
		return nil, err
	}
	a.Label = ns(label)
	a.Meta = map[string]any{}
	_ = json.Unmarshal([]byte(meta), &a.Meta)
	if a.Meta == nil {
		a.Meta = map[string]any{}
	}
	a.Registered = reg == 1
	if disabled.Valid {
		v := disabled.Int64
		a.DisabledAt = &v
	}
	return &a, nil
}

const addressColumns = `local, label, meta, registered, created_at, disabled_at`

// GetAddress returns an address's row, or nil.
func (s *Store) GetAddress(instanceID, local string) (*AddressRecord, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	a, err := scanAddress(db.QueryRow(`SELECT `+addressColumns+` FROM address WHERE local = ?`, local))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// PutAddress registers (or labels) an address. Idempotent, keeps the original creation time,
// and re-enables a disabled address.
func (s *Store) PutAddress(instanceID, local string, label *string, meta map[string]any, now int64) (*AddressRecord, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		meta = map[string]any{}
	}
	mj, _ := json.Marshal(meta)
	if _, err := db.Exec(`INSERT INTO address (local, label, meta, registered, created_at, disabled_at) VALUES (?, ?, ?, 1, ?, NULL)
		ON CONFLICT(local) DO UPDATE SET label = excluded.label, meta = excluded.meta, registered = 1, disabled_at = NULL`,
		local, strPtr(label), string(mj), now); err != nil {
		return nil, err
	}
	return s.GetAddress(instanceID, local)
}

// DisableAddress refuses inbound mail to an address and stops it sending; its messages stay.
func (s *Store) DisableAddress(instanceID, local string, now int64) (*AddressRecord, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO address (local, label, meta, registered, created_at, disabled_at) VALUES (?, NULL, '{}', 0, ?, ?)
		ON CONFLICT(local) DO UPDATE SET disabled_at = excluded.disabled_at`, local, now, now); err != nil {
		return nil, err
	}
	return s.GetAddress(instanceID, local)
}

// ListAddresses pages the addresses that carry a setting, in name order.
func (s *Store) ListAddresses(instanceID, cursor string, limit int) ([]*AddressRecord, *string, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, nil, err
	}
	limit = clampLimit(limit)
	rows, err := db.Query(`SELECT `+addressColumns+` FROM address WHERE local > ? ORDER BY local LIMIT ?`, cursor, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []*AddressRecord{}
	for rows.Next() {
		a, err := scanAddress(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, a)
	}
	var next *string
	if len(out) > limit {
		out = out[:limit]
		c := out[len(out)-1].Local
		next = &c
	}
	return out, next, rows.Err()
}

// --- sending -------------------------------------------------------------------------------

// HasWrittenToUs: has `sender` written to this instance in AUTHENTICATED mail — reply mode's test.
func (s *Store) HasWrittenToUs(instanceID, sender string) (bool, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return false, err
	}
	var one int
	err = db.QueryRow(`SELECT 1 FROM message WHERE from_addr = ? AND direction = 'in' AND verified = 1 LIMIT 1`, strings.ToLower(sender)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PrepareSend checks, in one place, that the From address is not disabled, that in reply mode
// every recipient has written to us in authenticated mail, and that the instance's own daily cap
// (when it has one) has room — in which case one slot is taken.
func (s *Store) PrepareSend(instanceID, local string, recipients []string, requireReplied bool, cap *int64, now int64) error {
	a, err := s.GetAddress(instanceID, local)
	if err != nil {
		return err
	}
	if a != nil && a.DisabledAt != nil {
		return common.NewError(403, fmt.Sprintf("address '%s' is disabled", local), "ADDRESS_DISABLED")
	}
	if requireReplied {
		var strangers []string
		for _, r := range recipients {
			ok, err := s.HasWrittenToUs(instanceID, r)
			if err != nil {
				return err
			}
			if !ok {
				strangers = append(strangers, r)
			}
		}
		if len(strangers) > 0 {
			shown := strangers
			more := ""
			if len(shown) > 5 {
				shown, more = shown[:5], ", …"
			}
			return common.NewError(403, "this mailbox only sends to people who have written to it (sendMode 'reply'); "+
				"no authenticated mail from: "+strings.Join(shown, ", ")+more, "NOT_A_REPLY")
		}
	}
	db, err := s.db(instanceID)
	if err != nil {
		return err
	}
	limit := int64(maxSafeInt)
	if cap != nil {
		limit = *cap
	}
	ok, err := s.takeCount(db, "out", now, limit)
	if err != nil {
		return err
	}
	if !ok {
		return common.NewError(429, fmt.Sprintf("this mailbox's daily send limit of %d is reached; it resets at 00:00 UTC", limit), "SEND_LIMIT")
	}
	return nil
}

// ReturnSend gives back a send slot taken for a send that then failed.
func (s *Store) ReturnSend(instanceID string, now int64) {
	if db, err := s.db(instanceID); err == nil {
		_, _ = db.Exec(`UPDATE day_count SET n = MAX(0, n - 1) WHERE kind = 'out' AND day = ?`, UTCDay(now))
	}
}

// --- deliveries ----------------------------------------------------------------------------

// DueDelivery is one pending delivery whose time has come.
type DueDelivery struct {
	MessageID string
	Action    string
	Attempts  int
}

// DueDeliveries are pending deliveries at or past their time.
func (s *Store) DueDeliveries(instanceID string, now int64, limit int) ([]DueDelivery, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT message_id, action, attempts FROM delivery WHERE status = 'pending' AND next_at <= ? ORDER BY next_at LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueDelivery
	for rows.Next() {
		var d DueDelivery
		if err := rows.Scan(&d.MessageID, &d.Action, &d.Attempts); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeliveryResult is what one attempt did.
type DeliveryResult struct {
	MessageID string
	Action    string
	Status    string
	Attempts  int
	NextAt    int64 // 0 = none
}

// RecordDeliveries writes attempt results back.
func (s *Store) RecordDeliveries(instanceID string, results []DeliveryResult) error {
	db, err := s.db(instanceID)
	if err != nil {
		return err
	}
	for _, r := range results {
		var next any
		if r.NextAt != 0 {
			next = r.NextAt
		}
		if _, err := db.Exec(`UPDATE delivery SET status = ?, attempts = ?, next_at = ? WHERE message_id = ? AND action = ?`,
			r.Status, r.Attempts, next, r.MessageID, r.Action); err != nil {
			return err
		}
	}
	return nil
}

// ResetDeliveries replaces a message's deliveries with `actions`, all pending and untried — a
// replay through the current routes.
func (s *Store) ResetDeliveries(instanceID, messageID string, actions []string, now int64) error {
	db, err := s.db(instanceID)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM delivery WHERE message_id = ?`, messageID); err != nil {
		return err
	}
	for _, a := range actions {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO delivery (message_id, action, status, attempts, next_at) VALUES (?, ?, 'pending', 0, ?)`, messageID, a, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- webhook signing key -------------------------------------------------------------------

func newWebhookSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// WebhookSecret is the instance's webhook signing key, minted on first use.
func (s *Store) WebhookSecret(instanceID string) (string, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (k, v) VALUES ('webhook_secret', ?)`, newWebhookSecret()); err != nil {
		return "", err
	}
	var v string
	err = db.QueryRow(`SELECT v FROM meta WHERE k = 'webhook_secret'`).Scan(&v)
	return v, err
}

// RotateWebhookSecret replaces the key; the next delivery is signed with the new one.
func (s *Store) RotateWebhookSecret(instanceID string) (string, error) {
	db, err := s.db(instanceID)
	if err != nil {
		return "", err
	}
	v := newWebhookSecret()
	_, err = db.Exec(`INSERT INTO meta (k, v) VALUES ('webhook_secret', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, v)
	return v, err
}

// --- teardown ------------------------------------------------------------------------------

// Drop removes an instance entirely: its database, its bytes, its waiters.
func (s *Store) Drop(instanceID string) {
	s.mu.Lock()
	for w := range s.waiters[instanceID] {
		select {
		case w.ch <- Summary{}:
		default:
		}
	}
	delete(s.waiters, instanceID)
	if db, ok := s.dbs[instanceID]; ok {
		_ = db.Close()
		delete(s.dbs, instanceID)
	}
	for k := range s.bytes {
		if strings.HasPrefix(k, instanceID+"/") {
			delete(s.bytes, k)
		}
	}
	s.mu.Unlock()
	if !s.memory() {
		_ = os.RemoveAll(s.instDir(instanceID))
	}
}
