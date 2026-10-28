package tasks

import (
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

	"github.com/altlimit/altengine/cli/internal/common"
	_ "modernc.org/sqlite"
)

// The store holds, per tasks instance, its queues, every task and every attempt — one SQLite
// database per instance, like the hosted per-instance queue. Every operation on one instance is
// serialized by that instance's lock, so a claim, a settle and an add never interleave: the same
// guarantee the hosted single-threaded queue gives.

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS queue (
  name TEXT PRIMARY KEY, settings TEXT NOT NULL, tokens REAL NOT NULL, refilled_at INTEGER NOT NULL,
  created INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS task (
  id TEXT PRIMARY KEY, queue TEXT NOT NULL, name TEXT, target TEXT, payload TEXT NOT NULL, params TEXT,
  retry TEXT, state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, run_at INTEGER NOT NULL,
  created INTEGER NOT NULL, started INTEGER, finished INTEGER, lease_until INTEGER, lease_id TEXT,
  last_status TEXT, last_error TEXT, result TEXT, keep_until INTEGER, source TEXT NOT NULL,
  depth INTEGER NOT NULL DEFAULT 0, dead_letter_of TEXT, payload_bytes INTEGER NOT NULL DEFAULT 0,
  defers INTEGER NOT NULL DEFAULT 0, deadline INTEGER);
CREATE INDEX IF NOT EXISTS task_due ON task(queue, state, run_at, id);
CREATE INDEX IF NOT EXISTS task_queue_id ON task(queue, id);
CREATE UNIQUE INDEX IF NOT EXISTS task_name ON task(queue, name) WHERE name IS NOT NULL;
CREATE INDEX IF NOT EXISTS task_keep ON task(keep_until) WHERE keep_until IS NOT NULL;
CREATE INDEX IF NOT EXISTS task_lease ON task(lease_until) WHERE lease_until IS NOT NULL;
CREATE TABLE IF NOT EXISTS attempt (
  task_id TEXT NOT NULL, n INTEGER NOT NULL, started INTEGER NOT NULL, ended INTEGER, status TEXT, error TEXT,
  PRIMARY KEY (task_id, n));
`

const taskCols = `id, queue, name, target, payload, params, retry, state, attempts, run_at, created, started, finished,
  lease_until, lease_id, last_status, last_error, result, keep_until, source, depth, dead_letter_of, payload_bytes, defers, deadline`

type instDB struct {
	db *sql.DB
	mu sync.Mutex
}

// Store holds every tasks instance's database.
type Store struct {
	dir string
	mu  sync.Mutex
	dbs map[string]*instDB
}

// NewStore opens the store. dir "" keeps everything in memory.
func NewStore(dir string) *Store {
	return &Store{dir: dir, dbs: map[string]*instDB{}}
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func (s *Store) instDir(instanceID string) string {
	return filepath.Join(s.dir, "tasks", sanitizeRe.ReplaceAllString(instanceID, "_"))
}

func (s *Store) open(instanceID string) (*instDB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dbs[instanceID]; ok {
		return d, nil
	}
	var dsn string
	if s.dir == "" {
		dsn = "file:tq_" + hex.EncodeToString([]byte(instanceID)) + "?mode=memory&cache=shared"
	} else {
		d := s.instDir(instanceID)
		if err := os.MkdirAll(d, common.DataDirPerm); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(d, "tasks.db")
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
	// A database written before tasks had their own deadline; the column already being there is
	// the expected error.
	_, _ = db.Exec(`ALTER TABLE task ADD COLUMN deadline INTEGER`)
	d := &instDB{db: db}
	s.dbs[instanceID] = d
	return d, nil
}

// with runs fn holding the instance's lock, the default queue made sure of.
func (s *Store) with(instanceID string, fn func(db *sql.DB) error) error {
	d, err := s.open(instanceID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UnixMilli()
	def, _ := json.Marshal(DefaultQueueSettings())
	if _, err := d.db.Exec(`INSERT OR IGNORE INTO queue (name, settings, tokens, refilled_at, created, updated) VALUES (?, ?, ?, ?, ?, ?)`,
		DefaultQueue, string(def), 10.0, now, now, now); err != nil {
		return err
	}
	return fn(d.db)
}

// Drop removes an instance entirely.
func (s *Store) Drop(instanceID string) {
	s.mu.Lock()
	if d, ok := s.dbs[instanceID]; ok {
		d.mu.Lock()
		_ = d.db.Close()
		d.mu.Unlock()
		delete(s.dbs, instanceID)
	}
	s.mu.Unlock()
	if s.dir != "" {
		_ = os.RemoveAll(s.instDir(instanceID))
	}
}

// --- ids -------------------------------------------------------------------------------------

// NewTaskID is time-ordered and fixed-width: base36 milliseconds then random, so lexicographic
// order is creation order and an id is a keyset cursor.
func NewTaskID(now int64) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	ts := strconv.FormatInt(now, 36)
	for len(ts) < 9 {
		ts = "0" + ts
	}
	return ts + hex.EncodeToString(b)
}

func newLeaseID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func newWebhookSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// --- shapes ----------------------------------------------------------------------------------

// Counts is a queue's tasks per state, each capped at CountCap.
type Counts struct {
	Scheduled int `json:"scheduled"`
	Running   int `json:"running"`
	Leased    int `json:"leased"`
	Done      int `json:"done"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

func (c *Counts) set(state string, n int) {
	switch state {
	case "scheduled":
		c.Scheduled = n
	case "running":
		c.Running = n
	case "leased":
		c.Leased = n
	case "done":
		c.Done = n
	case "failed":
		c.Failed = n
	case "cancelled":
		c.Cancelled = n
	}
}

// QueueInfo is a queue as every surface returns it.
type QueueInfo struct {
	Name      string        `json:"name"`
	Settings  QueueSettings `json:"settings"`
	CreatedAt int64         `json:"createdAt"`
	UpdatedAt int64         `json:"updatedAt"`
	Deleting  bool          `json:"deleting"`
	Purging   bool          `json:"purging"`
	Counts    Counts        `json:"counts"`
	Capped    []string      `json:"capped"`
}

// TaskSummary is a task without its payload, params or result.
type TaskSummary struct {
	ID           string  `json:"id"`
	Queue        string  `json:"queue"`
	Name         *string `json:"name"`
	State        string  `json:"state"`
	Target       *Target `json:"target"`
	Attempts     int     `json:"attempts"`
	MaxAttempts  int     `json:"maxAttempts"`
	RunAt        int64   `json:"runAt"`
	CreatedAt    int64   `json:"createdAt"`
	StartedAt    *int64  `json:"startedAt"`
	FinishedAt   *int64  `json:"finishedAt"`
	LeaseUntil   *int64  `json:"leaseUntil"`
	LastStatus   *string `json:"lastStatus"`
	LastError    *string `json:"lastError"`
	Source       string  `json:"source"`
	Depth        int     `json:"depth"`
	PayloadBytes int     `json:"payloadBytes"`
	DeadLetterOf *string `json:"deadLetterOf"`
	Defers       int     `json:"defers"`
	// DeadlineSeconds is this task's own deadline; null means its queue's.
	DeadlineSeconds *int64 `json:"deadlineSeconds"`
}

// Attempt is one attempt's record.
type Attempt struct {
	N         int     `json:"n"`
	StartedAt int64   `json:"startedAt"`
	EndedAt   *int64  `json:"endedAt"`
	Status    *string `json:"status"`
	Error     *string `json:"error"`
}

// TaskRecord is a task with everything.
type TaskRecord struct {
	TaskSummary
	Payload    json.RawMessage `json:"payload"`
	Params     json.RawMessage `json:"params"`
	Result     *string         `json:"result"`
	Retry      *RetryPolicy    `json:"retry"`
	AttemptLog []Attempt       `json:"attemptLog"`
}

// LeasedTask is a leased pull task and the token that acknowledges it.
type LeasedTask struct {
	TaskRecord
	LeaseID string `json:"leaseId"`
}

type taskRow struct {
	id, queue, payload, state, source             string
	name, target, params, retry                   sql.NullString
	leaseID, lastStatus, lastError, result, dlqOf sql.NullString
	attempts, depth, payloadBytes, defers         int
	runAt, created                                int64
	started, finished, leaseUntil, keepUntil      sql.NullInt64
	deadline                                      sql.NullInt64
}

// deadlineOf is how long this task's delivery is waited for, in seconds: its own, or the queue's.
func (r *taskRow) deadlineOf(s QueueSettings) int64 {
	if r.deadline.Valid && r.deadline.Int64 > 0 {
		return r.deadline.Int64
	}
	if s.DeadlineSeconds > 0 {
		return int64(s.DeadlineSeconds)
	}
	return DefaultDeadlineSeconds
}

func scanTask(sc interface{ Scan(...any) error }) (*taskRow, error) {
	var r taskRow
	err := sc.Scan(&r.id, &r.queue, &r.name, &r.target, &r.payload, &r.params, &r.retry, &r.state, &r.attempts, &r.runAt,
		&r.created, &r.started, &r.finished, &r.leaseUntil, &r.leaseID, &r.lastStatus, &r.lastError, &r.result, &r.keepUntil,
		&r.source, &r.depth, &r.dlqOf, &r.payloadBytes, &r.defers, &r.deadline)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func queryTasks(db *sql.DB, q string, args ...any) ([]*taskRow, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*taskRow
	for rows.Next() {
		r, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nstr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

func nint(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (r *taskRow) targetOf() *Target {
	if !r.target.Valid {
		return nil
	}
	var t Target
	if json.Unmarshal([]byte(r.target.String), &t) != nil {
		return nil
	}
	return &t
}

func (r *taskRow) retryOf() *RetryPolicy {
	if !r.retry.Valid {
		return nil
	}
	var p RetryPolicy
	if json.Unmarshal([]byte(r.retry.String), &p) != nil {
		return nil
	}
	return &p
}

func (r *taskRow) summary(queueRetry RetryPolicy) TaskSummary {
	max := queueRetry.MaxAttempts
	if p := r.retryOf(); p != nil {
		max = p.MaxAttempts
	}
	return TaskSummary{
		ID: r.id, Queue: r.queue, Name: nstr(r.name), State: r.state, Target: r.targetOf(), Attempts: r.attempts,
		MaxAttempts: max, RunAt: r.runAt, CreatedAt: r.created, StartedAt: nint(r.started), FinishedAt: nint(r.finished),
		LeaseUntil: nint(r.leaseUntil), LastStatus: nstr(r.lastStatus), LastError: nstr(r.lastError), Source: r.source,
		Depth: r.depth, PayloadBytes: r.payloadBytes, DeadLetterOf: nstr(r.dlqOf), Defers: r.defers,
		DeadlineSeconds: nint(r.deadline),
	}
}

type queueRow struct {
	name, settings   string
	tokens           float64
	refilledAt       int64
	created, updated int64
}

// settingsOf re-validates a stored queue's settings against the defaults: a setting tightened
// since it was saved is clamped to what is allowed now rather than half-applied.
func (q *queueRow) settingsOf() QueueSettings {
	var m any
	if json.Unmarshal([]byte(q.settings), &m) != nil {
		s := DefaultQueueSettings()
		s.Paused = true
		return s
	}
	s, err := ValidateQueueSettings(m, DefaultQueueSettings(), q.name)
	if err != nil {
		s = DefaultQueueSettings()
		s.Paused = true
	}
	return s
}

func getQueueRow(db *sql.DB, name string) (*queueRow, error) {
	var q queueRow
	err := db.QueryRow(`SELECT name, settings, tokens, refilled_at, created, updated FROM queue WHERE name = ?`, name).
		Scan(&q.name, &q.settings, &q.tokens, &q.refilledAt, &q.created, &q.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func allQueues(db *sql.DB) ([]*queueRow, error) {
	rows, err := db.Query(`SELECT name, settings, tokens, refilled_at, created, updated FROM queue ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*queueRow
	for rows.Next() {
		var q queueRow
		if err := rows.Scan(&q.name, &q.settings, &q.tokens, &q.refilledAt, &q.created, &q.updated); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}

func queueInfo(db *sql.DB, q *queueRow) (QueueInfo, error) {
	info := QueueInfo{Name: q.name, Settings: q.settingsOf(), CreatedAt: q.created, UpdatedAt: q.updated, Capped: []string{}}
	for _, st := range States {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM task WHERE queue = ? AND state = ? LIMIT ?)`, q.name, st, CountCap).Scan(&n); err != nil {
			return info, err
		}
		info.Counts.set(st, n)
		if n >= CountCap {
			info.Capped = append(info.Capped, st)
		}
	}
	return info, nil
}

func queueNotFound(name string) error {
	return common.NewError(404, fmt.Sprintf("no queue '%s'", name), "QUEUE_NOT_FOUND")
}

// --- queues ----------------------------------------------------------------------------------

// ListQueues is every queue with its counts.
func (s *Store) ListQueues(instanceID string) ([]QueueInfo, error) {
	out := []QueueInfo{}
	err := s.with(instanceID, func(db *sql.DB) error {
		qs, err := allQueues(db)
		if err != nil {
			return err
		}
		for _, q := range qs {
			info, err := queueInfo(db, q)
			if err != nil {
				return err
			}
			out = append(out, info)
		}
		return nil
	})
	return out, err
}

// GetQueue is one queue.
func (s *Store) GetQueue(instanceID, name string) (*QueueInfo, error) {
	var out *QueueInfo
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(name)
		}
		info, err := queueInfo(db, q)
		out = &info
		return err
	})
	return out, err
}

// QueueSettingsOf is a queue's current settings — the base a PATCH validates against.
func (s *Store) QueueSettingsOf(instanceID, name string) (QueueSettings, error) {
	var out QueueSettings
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(name)
		}
		out = q.settingsOf()
		return nil
	})
	return out, err
}

// PutQueue creates (`create`) or changes a queue. settings are already validated.
func (s *Store) PutQueue(instanceID, name string, settings QueueSettings, create bool, now int64) (*QueueInfo, error) {
	var out *QueueInfo
	err := s.with(instanceID, func(db *sql.DB) error {
		existing, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		if create && existing != nil {
			return common.NewError(409, fmt.Sprintf("queue '%s' already exists", name), "ALREADY_EXISTS")
		}
		if !create && existing == nil {
			return queueNotFound(name)
		}
		if existing == nil {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM queue`).Scan(&n); err != nil {
				return err
			}
			if n >= MaxQueues {
				return common.NewError(400, fmt.Sprintf("at most %d queues per instance", MaxQueues), "LIMIT")
			}
		}
		if settings.DeadLetterQueue != nil {
			d, err := getQueueRow(db, *settings.DeadLetterQueue)
			if err != nil {
				return err
			}
			if d == nil {
				return common.NewError(400, fmt.Sprintf("deadLetterQueue '%s' does not exist", *settings.DeadLetterQueue), "INVALID_ARGUMENT")
			}
		}
		b, _ := json.Marshal(settings)
		if existing != nil {
			_, err = db.Exec(`UPDATE queue SET settings = ?, updated = ? WHERE name = ?`, string(b), now, name)
		} else {
			tokens := settings.RatePerSecond
			if tokens < 1 {
				tokens = 1
			}
			_, err = db.Exec(`INSERT INTO queue (name, settings, tokens, refilled_at, created, updated) VALUES (?, ?, ?, ?, ?, ?)`,
				name, string(b), tokens, now, now, now)
		}
		if err != nil {
			return err
		}
		q, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		info, err := queueInfo(db, q)
		out = &info
		return err
	})
	return out, err
}

func deleteTasks(db *sql.DB, where string, args ...any) (int, error) {
	if _, err := db.Exec(`DELETE FROM attempt WHERE task_id IN (SELECT id FROM task WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	res, err := db.Exec(`DELETE FROM task WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteQueue deletes a queue and every task in it. The default queue stays.
func (s *Store) DeleteQueue(instanceID, name string) (map[string]any, error) {
	var out map[string]any
	err := s.with(instanceID, func(db *sql.DB) error {
		if name == DefaultQueue {
			return bad("the default queue cannot be deleted; purge it instead")
		}
		q, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(name)
		}
		qs, err := allQueues(db)
		if err != nil {
			return err
		}
		var dependents []string
		for _, o := range qs {
			if o.name != name {
				if d := o.settingsOf().DeadLetterQueue; d != nil && *d == name {
					dependents = append(dependents, "'"+o.name+"'")
				}
			}
		}
		if len(dependents) > 0 {
			return common.NewError(409, fmt.Sprintf("queue '%s' is the dead-letter queue of %s", name, strings.Join(dependents, ", ")), "QUEUE_IN_USE")
		}
		if _, err := deleteTasks(db, `queue = ?`, name); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM queue WHERE name = ?`, name); err != nil {
			return err
		}
		out = map[string]any{"deleted": true, "pending": false}
		return nil
	})
	return out, err
}

// PurgeQueue deletes every task in a queue, whatever its state. A running one finishes, and its
// outcome is dropped with its row.
func (s *Store) PurgeQueue(instanceID, name string) (map[string]any, error) {
	var out map[string]any
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, name)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(name)
		}
		n, err := deleteTasks(db, `queue = ?`, name)
		if err != nil {
			return err
		}
		out = map[string]any{"purged": n, "pending": false}
		return nil
	})
	return out, err
}

// --- adding ----------------------------------------------------------------------------------

// AddResult is what add reports per task, in request order.
type AddResult map[string]any

// Add inserts tasks into a queue. Returns the results and whether a push queue now has work.
func (s *Store) Add(instanceID, queue string, items []TaskInput, source string, depth int, now int64) ([]AddResult, bool, error) {
	var out []AddResult
	wake := false
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, queue)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(queue)
		}
		st := q.settingsOf()
		for _, t := range items {
			if st.Mode == "pull" && t.Target != nil {
				return bad(fmt.Sprintf("queue '%s' is a pull queue; its tasks have no target", queue))
			}
			if st.Mode == "push" && t.Target == nil && st.Target == nil {
				return common.NewError(400, fmt.Sprintf("queue '%s' has no target; give the task one, or set the queue's", queue), "NO_TARGET")
			}
		}
		accepted := 0
		for _, t := range items {
			id := NewTaskID(now)
			var target, retry any
			if t.Target != nil {
				b, _ := json.Marshal(t.Target)
				target = string(b)
			}
			if t.Retry != nil {
				b, _ := json.Marshal(t.Retry)
				retry = string(b)
			}
			var deadline any
			if t.DeadlineSeconds != nil {
				deadline = *t.DeadlineSeconds
			}
			res, err := db.Exec(`INSERT INTO task (id, queue, name, target, payload, params, retry, state, attempts, run_at, created, source, depth, payload_bytes, deadline)
				VALUES (?, ?, ?, ?, ?, ?, ?, 'scheduled', 0, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				id, queue, strOrNil(t.Name), target, t.Payload, strOrNil(t.Params), retry, t.RunAt, now, source, depth, len(t.Payload), deadline)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				out = append(out, AddResult{"ok": true, "id": id, "name": strOrNil(t.Name), "state": "scheduled", "runAt": t.RunAt})
				accepted++
				continue
			}
			var existing string
			_ = db.QueryRow(`SELECT id FROM task WHERE queue = ? AND name = ?`, queue, *t.Name).Scan(&existing)
			out = append(out, AddResult{
				"ok": false, "code": "TASK_EXISTS",
				"message": fmt.Sprintf("a task named '%s' already exists in queue '%s' (names stay taken until the task's record expires)", *t.Name, queue),
				"id":      existing, "name": *t.Name,
			})
		}
		wake = accepted > 0 && st.Mode == "push" && !st.Paused
		return nil
	})
	return out, wake, err
}

// --- reading ---------------------------------------------------------------------------------

func retryOfQueue(db *sql.DB, queue string) RetryPolicy {
	q, _ := getQueueRow(db, queue)
	if q == nil {
		return DefaultRetry
	}
	return q.settingsOf().Retry
}

func getTaskRow(db *sql.DB, id string) (*taskRow, error) {
	r, err := scanTask(db.QueryRow(`SELECT `+taskCols+` FROM task WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func record(db *sql.DB, r *taskRow) (*TaskRecord, error) {
	rec := &TaskRecord{TaskSummary: r.summary(retryOfQueue(db, r.queue)), Payload: json.RawMessage(r.payload), Result: nstr(r.result), Retry: r.retryOf(), AttemptLog: []Attempt{}}
	if !json.Valid(rec.Payload) {
		rec.Payload = json.RawMessage("null")
	}
	rec.Params = json.RawMessage("null")
	if r.params.Valid && json.Valid([]byte(r.params.String)) {
		rec.Params = json.RawMessage(r.params.String)
	}
	rows, err := db.Query(`SELECT n, started, ended, status, error FROM attempt WHERE task_id = ? ORDER BY n`, r.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attempt
		var ended sql.NullInt64
		var status, e sql.NullString
		if err := rows.Scan(&a.N, &a.StartedAt, &ended, &status, &e); err != nil {
			return nil, err
		}
		a.EndedAt, a.Status, a.Error = nint(ended), nstr(status), nstr(e)
		rec.AttemptLog = append(rec.AttemptLog, a)
	}
	return rec, rows.Err()
}

// Get is one task with its payload and attempts, or nil.
func (s *Store) Get(instanceID, id string) (*TaskRecord, error) {
	var out *TaskRecord
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := getTaskRow(db, id)
		if err != nil || r == nil {
			return err
		}
		out, err = record(db, r)
		return err
	})
	return out, err
}

// ListOptions narrows a listing.
type ListOptions struct {
	State  string
	Cursor string
	Limit  int
	Name   string
}

// TaskPage is one page of a queue's tasks.
type TaskPage struct {
	Tasks  []TaskSummary `json:"tasks"`
	Cursor *string       `json:"cursor"`
}

func decodeCursor(c string) (int64, string, bool) {
	at := strings.Index(c, ".")
	if at <= 0 {
		return 0, "", false
	}
	sort, err := strconv.ParseInt(c[:at], 10, 64)
	id := c[at+1:]
	if err != nil || id == "" {
		return 0, "", false
	}
	return sort, id, true
}

// List is one page of a queue's tasks. With a state, ordered by run time — soonest first for work
// waiting or running, latest first for work that has finished. Without, newest first by creation.
func (s *Store) List(instanceID, queue string, o ListOptions) (*TaskPage, error) {
	var out *TaskPage
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, queue)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(queue)
		}
		retry := q.settingsOf().Retry
		limit := o.Limit
		if limit < 1 {
			limit = defaultListLimit
		}
		if limit > MaxListLimit {
			limit = MaxListLimit
		}
		page := &TaskPage{Tasks: []TaskSummary{}}
		if o.Name != "" {
			rows, err := queryTasks(db, `SELECT `+taskCols+` FROM task WHERE queue = ? AND name = ?`, queue, o.Name)
			if err != nil {
				return err
			}
			for _, r := range rows {
				page.Tasks = append(page.Tasks, r.summary(retry))
			}
			out = page
			return nil
		}
		sortKey, curID, hasCur := decodeCursor(o.Cursor)
		var rows []*taskRow
		if o.State != "" {
			asc := o.State == "scheduled" || o.State == "running" || o.State == "leased"
			dir, cmp := "DESC", "<"
			if asc {
				dir, cmp = "ASC", ">"
			}
			args := []any{queue, o.State}
			after := ""
			if hasCur {
				after = fmt.Sprintf(`AND (run_at %s ? OR (run_at = ? AND id %s ?))`, cmp, cmp)
				args = append(args, sortKey, sortKey, curID)
			}
			args = append(args, limit+1)
			rows, err = queryTasks(db, `SELECT `+taskCols+` FROM task WHERE queue = ? AND state = ? `+after+` ORDER BY run_at `+dir+`, id `+dir+` LIMIT ?`, args...)
		} else {
			args := []any{queue}
			after := ""
			if hasCur {
				after = `AND id < ?`
				args = append(args, curID)
			}
			args = append(args, limit+1)
			rows, err = queryTasks(db, `SELECT `+taskCols+` FROM task WHERE queue = ? `+after+` ORDER BY id DESC LIMIT ?`, args...)
		}
		if err != nil {
			return err
		}
		n := len(rows)
		if n > limit {
			rows = rows[:limit]
		}
		for _, r := range rows {
			page.Tasks = append(page.Tasks, r.summary(retry))
		}
		if n > limit && len(rows) > 0 {
			last := rows[len(rows)-1]
			sort := int64(0)
			if o.State != "" {
				sort = last.runAt
			}
			c := fmt.Sprintf("%d.%s", sort, last.id)
			page.Cursor = &c
		}
		out = page
		return nil
	})
	return out, err
}

// --- changing one task -----------------------------------------------------------------------

func keepUntil(now int64, keepDays int) int64 {
	if keepDays < 1 {
		keepDays = DefaultRetentionDays
	}
	return now + int64(keepDays)*86_400_000
}

// CancelResult is what a cancel reports.
type CancelResult struct {
	Cancelled []string      `json:"cancelled"`
	Skipped   []SkippedTask `json:"skipped"`
}

// SkippedTask is a task a cancel left alone, and why: its state, or null when it does not exist.
type SkippedTask struct {
	ID    string  `json:"id"`
	State *string `json:"state"`
}

// Cancel cancels scheduled or leased tasks. Running ones cannot be stopped mid-delivery.
func (s *Store) Cancel(instanceID string, ids []string, now int64, keepDays int) (*CancelResult, error) {
	out := &CancelResult{Cancelled: []string{}, Skipped: []SkippedTask{}}
	err := s.with(instanceID, func(db *sql.DB) error {
		if len(ids) > MaxListLimit {
			ids = ids[:MaxListLimit]
		}
		for _, id := range ids {
			r, err := getTaskRow(db, id)
			if err != nil {
				return err
			}
			if r == nil || (r.state != "scheduled" && r.state != "leased") {
				sk := SkippedTask{ID: id}
				if r != nil {
					st := r.state
					sk.State = &st
				}
				out.Skipped = append(out.Skipped, sk)
				continue
			}
			if _, err := db.Exec(`UPDATE task SET state = 'cancelled', finished = ?, keep_until = ?, lease_until = NULL, lease_id = NULL, last_status = 'cancelled' WHERE id = ?`,
				now, keepUntil(now, keepDays), id); err != nil {
				return err
			}
			out.Cancelled = append(out.Cancelled, id)
		}
		return nil
	})
	return out, err
}

// Run runs a task now. A scheduled one is moved to now; a failed or cancelled one goes back to
// scheduled with its attempts reset; a finished one is run again as a NEW task (same queue,
// payload, params and target, no name) — its record is history and stays as it was.
func (s *Store) Run(instanceID, id string, now int64) (*TaskRecord, bool, error) {
	var out *TaskRecord
	push := false
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := getTaskRow(db, id)
		if err != nil {
			return err
		}
		if r == nil {
			return common.NotFound("task not found")
		}
		q, err := getQueueRow(db, r.queue)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(r.queue)
		}
		runID := r.id
		switch r.state {
		case "running":
			return common.NewError(409, "the task is running now", "TASK_RUNNING")
		case "leased":
			return common.NewError(409, "the task is leased by a worker; it runs again if the lease expires", "TASK_LEASED")
		case "scheduled":
			_, err = db.Exec(`UPDATE task SET run_at = ? WHERE id = ?`, now, r.id)
		case "failed", "cancelled":
			_, err = db.Exec(`UPDATE task SET state = 'scheduled', attempts = 0, defers = 0, run_at = ?, finished = NULL, keep_until = NULL, last_status = 'requeued' WHERE id = ?`, now, r.id)
		case "done":
			runID = NewTaskID(now)
			_, err = db.Exec(`INSERT INTO task (id, queue, name, target, payload, params, retry, state, attempts, run_at, created, source, depth, payload_bytes, deadline)
				VALUES (?, ?, NULL, ?, ?, ?, ?, 'scheduled', 0, ?, ?, 'rerun', ?, ?, ?)`,
				runID, r.queue, r.target, r.payload, r.params, r.retry, now, now, r.depth, r.payloadBytes, r.deadline)
		}
		if err != nil {
			return err
		}
		push = q.settingsOf().Mode == "push"
		nr, err := getTaskRow(db, runID)
		if err != nil {
			return err
		}
		out, err = record(db, nr)
		return err
	})
	return out, push, err
}

// --- pull queues -----------------------------------------------------------------------------

func clampLease(secs float64) int64 {
	n := int64(secs)
	if n == 0 {
		n = DefaultLeaseSeconds
	}
	if n < 1 {
		n = 1
	}
	if n > MaxLeaseSeconds {
		n = MaxLeaseSeconds
	}
	return n
}

// Lease leases up to count due tasks from a pull queue for leaseSeconds. Each comes with a lease
// id that ack, extend and release must present.
func (s *Store) Lease(instanceID, queue string, count, leaseSeconds float64, now int64) ([]LeasedTask, error) {
	out := []LeasedTask{}
	err := s.with(instanceID, func(db *sql.DB) error {
		q, err := getQueueRow(db, queue)
		if err != nil {
			return err
		}
		if q == nil {
			return queueNotFound(queue)
		}
		st := q.settingsOf()
		if st.Mode != "pull" {
			return common.NewError(400, fmt.Sprintf("queue '%s' is a push queue; its tasks are delivered, not leased", queue), "NOT_PULL_QUEUE")
		}
		if st.Paused {
			return nil
		}
		n := int(count)
		if n < 1 {
			n = 1
		}
		if n > MaxLeaseCount {
			n = MaxLeaseCount
		}
		secs := clampLease(leaseSeconds)
		leaseID := newLeaseID()
		rows, err := queryTasks(db, `UPDATE task SET state = 'leased', attempts = attempts + 1, started = ?, lease_until = ?, lease_id = ?
			WHERE id IN (SELECT id FROM task WHERE queue = ? AND state = 'scheduled' AND run_at <= ? ORDER BY run_at, id LIMIT ?)
			RETURNING `+taskCols, now, now+secs*1000, leaseID, queue, now, n)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := db.Exec(`INSERT OR REPLACE INTO attempt (task_id, n, started, status) VALUES (?, ?, ?, 'leased')`, r.id, r.attempts, now); err != nil {
				return err
			}
		}
		for _, r := range rows {
			rec, err := record(db, r)
			if err != nil {
				return err
			}
			out = append(out, LeasedTask{TaskRecord: *rec, LeaseID: leaseID})
		}
		return nil
	})
	return out, err
}

func leased(db *sql.DB, id, leaseID string, now int64) (*taskRow, error) {
	r, err := getTaskRow(db, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, common.NotFound("task not found")
	}
	if r.state != "leased" || !r.leaseID.Valid || r.leaseID.String != leaseID {
		return nil, common.NewError(409, "this lease is no longer held: it expired, or the task was released or cancelled", "LEASE_LOST")
	}
	if !r.leaseUntil.Valid || r.leaseUntil.Int64 < now {
		return nil, common.NewError(409, "the lease expired", "LEASE_LOST")
	}
	return r, nil
}

// Ack finishes a leased task.
func (s *Store) Ack(instanceID, id, leaseID string, result *string, now int64, keepDays int) (*TaskRecord, error) {
	var out *TaskRecord
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := leased(db, id, leaseID, now)
		if err != nil {
			return err
		}
		var res any
		if result != nil {
			c, _ := clipBytes(*result, MaxResultBytes)
			res = c
		}
		if _, err := db.Exec(`UPDATE task SET state = 'done', finished = ?, keep_until = ?, lease_until = NULL, lease_id = NULL, last_status = 'acked', last_error = NULL, result = ? WHERE id = ?`,
			now, keepUntil(now, keepDays), res, r.id); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE attempt SET ended = ?, status = 'acked' WHERE task_id = ? AND n = ?`, now, r.id, r.attempts); err != nil {
			return err
		}
		nr, err := getTaskRow(db, r.id)
		if err != nil {
			return err
		}
		out, err = record(db, nr)
		return err
	})
	return out, err
}

// Extend holds a lease longer.
func (s *Store) Extend(instanceID, id, leaseID string, leaseSeconds float64, now int64) (*TaskRecord, error) {
	var out *TaskRecord
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := leased(db, id, leaseID, now)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE task SET lease_until = ? WHERE id = ?`, now+clampLease(leaseSeconds)*1000, r.id); err != nil {
			return err
		}
		nr, err := getTaskRow(db, r.id)
		if err != nil {
			return err
		}
		out, err = record(db, nr)
		return err
	})
	return out, err
}

// Release gives a leased task back: scheduled again after delaySeconds, the attempt counted as
// failed (with errText) — or the task fails when it was the last attempt.
func (s *Store) Release(instanceID, id, leaseID string, delaySeconds float64, errText *string, now int64, keepDays int) (*TaskRecord, *FailedTask, error) {
	var out *TaskRecord
	var failed *FailedTask
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := leased(db, id, leaseID, now)
		if err != nil {
			return err
		}
		delay := delaySeconds
		if delay < 0 {
			delay = 0
		}
		if delay > 30*86_400 {
			delay = 30 * 86_400
		}
		why := "released by the worker"
		if errText != nil {
			why = *errText
		}
		at := now + int64(delay*1000)
		failed, err = settle(db, r, Outcome{Status: "released", Error: why}, now, &at, keepDays)
		if err != nil {
			return err
		}
		nr, err := getTaskRow(db, r.id)
		if err != nil {
			return err
		}
		out, err = record(db, nr)
		return err
	})
	return out, failed, err
}

// --- the engine ------------------------------------------------------------------------------

// Outcome is one attempt's result.
type Outcome struct {
	OK        bool
	Status    string
	Error     string
	Permanent bool
	Result    *string
	Control   *TargetControl
}

// FailedTask names a task that has just failed for good — for the log.
type FailedTask struct {
	ID     string
	Queue  string
	Status string
	Error  string
}

// settle records one attempt's outcome on a claimed task: done, scheduled again with backoff, or
// failed (and copied to the dead-letter queue). retryAt overrides the backoff (a pull release).
func settle(db *sql.DB, r *taskRow, o Outcome, now int64, retryAt *int64, keepDays int) (*FailedTask, error) {
	q, err := getQueueRow(db, r.queue)
	if err != nil {
		return nil, err
	}
	var control *TargetControl
	if q != nil && q.settingsOf().TargetControl {
		control = o.Control
	}
	retry := DefaultRetry
	if q != nil {
		retry = q.settingsOf().Retry
	}
	if p := r.retryOf(); p != nil {
		retry = *p
	}
	if control != nil && control.MaxAttempts > 0 && control.MaxAttempts != retry.MaxAttempts {
		retry.MaxAttempts = control.MaxAttempts
		b, _ := json.Marshal(retry)
		if _, err := db.Exec(`UPDATE task SET retry = ? WHERE id = ?`, string(b), r.id); err != nil {
			return nil, err
		}
	}
	result := any(nil)
	if o.Result != nil {
		result = *o.Result
	}
	if control != nil && control.Retry == "defer" {
		at := now + int64(retry.MinBackoffSeconds)*1000
		if control.RetryAt != 0 {
			at = control.RetryAt
		}
		tooOld := retry.MaxAgeSeconds > 0 && at-r.created > int64(retry.MaxAgeSeconds)*1000
		if r.defers < MaxDefers && !tooOld {
			// Not an attempt: the count goes back down, and the deferral is recorded on its own.
			status := o.Status + " deferred"
			if _, err := db.Exec(`UPDATE task SET state = 'scheduled', run_at = ?, attempts = attempts - 1, defers = defers + 1, lease_until = NULL, lease_id = NULL,
				last_status = ?, last_error = NULL, result = ? WHERE id = ?`, at, status, result, r.id); err != nil {
				return nil, err
			}
			_, err := db.Exec(`UPDATE attempt SET ended = ?, status = ? WHERE task_id = ? AND n = ?`, now, status, r.id, r.attempts)
			return nil, err
		}
		why := fmt.Sprintf("deferred %d times", MaxDefers)
		if tooOld {
			why = "deferred past the queue's maxAgeSeconds"
		}
		o = Outcome{Status: o.Status, Error: why, Permanent: true, Result: o.Result}
	} else if o.OK && control != nil && control.Retry == "retry" {
		o = Outcome{Status: o.Status, Error: "the target asked for a retry", Result: o.Result}
	}
	if o.OK {
		if _, err := db.Exec(`UPDATE task SET state = 'done', finished = ?, keep_until = ?, lease_until = NULL, lease_id = NULL, last_status = ?, last_error = NULL, result = ? WHERE id = ?`,
			now, keepUntil(now, keepDays), o.Status, result, r.id); err != nil {
			return nil, err
		}
		_, err := db.Exec(`UPDATE attempt SET ended = ?, status = ? WHERE task_id = ? AND n = ?`, now, o.Status, r.id, r.attempts)
		return nil, err
	}
	errText, _ := clipBytes(o.Error, 1000)
	if _, err := db.Exec(`UPDATE attempt SET ended = ?, status = ?, error = ? WHERE task_id = ? AND n = ?`, now, o.Status, errText, r.id, r.attempts); err != nil {
		return nil, err
	}
	// `never` ends it whatever the status; `retry` keeps a 4xx alive; Retry-After replaces the
	// backoff — still within the attempt limit and the task's maximum age.
	permanent := o.Permanent
	if control != nil && control.Retry == "never" {
		permanent = true
	} else if control != nil && control.Retry == "retry" {
		permanent = false
	}
	backoff := NextAttemptAt(retry, r.attempts, r.created, now)
	var wanted int64
	if retryAt != nil {
		wanted = *retryAt
	} else if control != nil && control.RetryAt != 0 {
		wanted = control.RetryAt
	}
	hasWanted := retryAt != nil || (control != nil && control.RetryAt != 0)
	tooOld := hasWanted && retry.MaxAgeSeconds > 0 && wanted-r.created > int64(retry.MaxAgeSeconds)*1000
	var next int64
	switch {
	case permanent:
		next = 0
	case hasWanted:
		if !tooOld {
			next = wanted
		}
	default:
		next = backoff
	}
	if next != 0 && r.attempts < retry.MaxAttempts {
		_, err := db.Exec(`UPDATE task SET state = 'scheduled', run_at = ?, lease_until = NULL, lease_id = NULL, last_status = ?, last_error = ?, result = ? WHERE id = ?`,
			next, o.Status, errText, result, r.id)
		return nil, err
	}
	if _, err := db.Exec(`UPDATE task SET state = 'failed', finished = ?, keep_until = ?, lease_until = NULL, lease_id = NULL, last_status = ?, last_error = ?, result = ? WHERE id = ?`,
		now, keepUntil(now, keepDays), o.Status, errText, result, r.id); err != nil {
		return nil, err
	}
	if err := deadLetter(db, q, r, now); err != nil {
		return nil, err
	}
	return &FailedTask{ID: r.id, Queue: r.queue, Status: o.Status, Error: errText}, nil
}

// deadLetter copies a failed task into its queue's dead-letter queue, when it has one that still
// exists. A push dead-letter queue delivers to its OWN target: the failed task's target was the
// one that kept failing.
func deadLetter(db *sql.DB, q *queueRow, r *taskRow, now int64) error {
	if q == nil {
		return nil
	}
	dlq := q.settingsOf().DeadLetterQueue
	if dlq == nil {
		return nil
	}
	d, err := getQueueRow(db, *dlq)
	if err != nil || d == nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO task (id, queue, name, target, payload, params, retry, state, attempts, run_at, created, source, depth, dead_letter_of, payload_bytes)
		VALUES (?, ?, NULL, NULL, ?, ?, NULL, 'scheduled', 0, ?, ?, 'deadletter', ?, ?, ?)`,
		NewTaskID(now), *dlq, r.payload, r.params, now, now, r.depth, r.id, r.payloadBytes)
	return err
}

// Claimed is one task the engine has claimed for delivery.
type Claimed struct {
	Row      *taskRow
	Settings QueueSettings
}

// Claim claims as much due push work as the limits allow: per-queue concurrency (`running` per
// queue, from the caller), a token bucket per queue, and the instance's MaxInflight.
func (s *Store) Claim(instanceID string, running map[string]int, inflight int, now int64) ([]Claimed, error) {
	var out []Claimed
	err := s.with(instanceID, func(db *sql.DB) error {
		qs, err := allQueues(db)
		if err != nil {
			return err
		}
		for _, q := range qs {
			if inflight >= MaxInflight {
				return nil
			}
			st := q.settingsOf()
			if st.Mode != "push" || st.Paused {
				continue
			}
			slots := st.MaxConcurrent - running[q.name]
			if free := MaxInflight - inflight; free < slots {
				slots = free
			}
			if slots <= 0 {
				continue
			}
			burst := st.RatePerSecond
			if burst < 1 {
				burst = 1
			}
			tokens := q.tokens + float64(now-q.refilledAt)/1000*st.RatePerSecond
			if tokens > burst {
				tokens = burst
			}
			n := int(tokens)
			if slots < n {
				n = slots
			}
			if n <= 0 {
				continue
			}
			// The lease is the task's own deadline (or the queue's) plus slack: past it, a delivery
			// nothing is waiting on any more was lost and is retried.
			rows, err := queryTasks(db, `UPDATE task SET state = 'running', attempts = attempts + 1, started = ?,
					lease_until = ? + COALESCE(deadline, ?) * 1000 + ?
				WHERE id IN (SELECT id FROM task WHERE queue = ? AND state = 'scheduled' AND run_at <= ? ORDER BY run_at, id LIMIT ?)
				RETURNING `+taskCols, now, now, st.DeadlineSeconds, LeaseSlackMS, q.name, now, n)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				continue
			}
			if _, err := db.Exec(`UPDATE queue SET tokens = ?, refilled_at = ? WHERE name = ?`, tokens-float64(len(rows)), now, q.name); err != nil {
				return err
			}
			for _, r := range rows {
				if _, err := db.Exec(`INSERT OR REPLACE INTO attempt (task_id, n, started, status) VALUES (?, ?, ?, 'running')`, r.id, r.attempts, now); err != nil {
					return err
				}
				out = append(out, Claimed{Row: r, Settings: st})
			}
			inflight += len(rows)
		}
		return nil
	})
	return out, err
}

// Settle records a delivery's outcome, if the row is still that attempt's.
func (s *Store) Settle(instanceID, id string, attempts int, o Outcome, now int64, keepDays int) (*FailedTask, error) {
	var failed *FailedTask
	err := s.with(instanceID, func(db *sql.DB) error {
		r, err := getTaskRow(db, id)
		if err != nil || r == nil || r.state != "running" || r.attempts != attempts {
			// Purged, deleted, or no longer this attempt's.
			return err
		}
		failed, err = settle(db, r, o, now, nil, keepDays)
		return err
	})
	return failed, err
}

// Housekeep drops finished tasks past their retention and recovers tasks whose lease passed with
// nothing delivering them (a pull worker that never answered, or a delivery lost to a restart).
func (s *Store) Housekeep(instanceID string, inflight map[string]bool, now int64, keepDays int) ([]*FailedTask, error) {
	var failed []*FailedTask
	err := s.with(instanceID, func(db *sql.DB) error {
		if _, err := deleteTasks(db, `id IN (SELECT id FROM task WHERE keep_until IS NOT NULL AND keep_until <= ? LIMIT 500)`, now); err != nil {
			return err
		}
		rows, err := queryTasks(db, `SELECT `+taskCols+` FROM task WHERE lease_until IS NOT NULL AND lease_until <= ? AND state IN ('running', 'leased') LIMIT 500`, now)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if inflight[r.id] {
				continue
			}
			why := "the delivery was lost (the queue restarted mid-delivery)"
			if r.state == "leased" {
				why = "the lease expired without an ack"
			}
			f, err := settle(db, r, Outcome{Status: "lost", Error: why}, now, nil, keepDays)
			if err != nil {
				return err
			}
			if f != nil {
				failed = append(failed, f)
			}
		}
		return nil
	})
	return failed, err
}

// NextDue is when the engine should next look at this instance: the soonest scheduled push task
// (or, when one is due but its queue is out of tokens, when the bucket refills), a lease to
// check, or a record to prune. 0 when nothing is waiting.
func (s *Store) NextDue(instanceID string, running map[string]int, now int64) (int64, error) {
	var best int64
	consider := func(at int64) {
		if at > 0 && (best == 0 || at < best) {
			best = at
		}
	}
	err := s.with(instanceID, func(db *sql.DB) error {
		qs, err := allQueues(db)
		if err != nil {
			return err
		}
		for _, q := range qs {
			st := q.settingsOf()
			if st.Mode != "push" || st.Paused {
				continue
			}
			var first sql.NullInt64
			if err := db.QueryRow(`SELECT MIN(run_at) FROM task WHERE queue = ? AND state = 'scheduled'`, q.name).Scan(&first); err != nil {
				return err
			}
			if !first.Valid {
				continue
			}
			at := first.Int64
			if at <= now {
				if running[q.name] >= st.MaxConcurrent {
					continue
				}
				burst := st.RatePerSecond
				if burst < 1 {
					burst = 1
				}
				tokens := q.tokens + float64(now-q.refilledAt)/1000*st.RatePerSecond
				if tokens > burst {
					tokens = burst
				}
				at = now
				if tokens < 1 {
					at = now + int64((1-tokens)/st.RatePerSecond*1000+0.999)
				}
			}
			consider(at)
		}
		var t sql.NullInt64
		if err := db.QueryRow(`SELECT MIN(lease_until) FROM task WHERE lease_until IS NOT NULL`).Scan(&t); err == nil && t.Valid {
			consider(t.Int64 + 1)
		}
		if err := db.QueryRow(`SELECT MIN(keep_until) FROM task WHERE keep_until IS NOT NULL`).Scan(&t); err == nil && t.Valid {
			consider(t.Int64)
		}
		return nil
	})
	return best, err
}

// Stats is the instance's task count and database size.
func (s *Store) Stats(instanceID string) (tasks int, dbBytes int64, err error) {
	err = s.with(instanceID, func(db *sql.DB) error {
		if err := db.QueryRow(`SELECT COUNT(*) FROM task`).Scan(&tasks); err != nil {
			return err
		}
		var pages, size int64
		_ = db.QueryRow(`PRAGMA page_count`).Scan(&pages)
		_ = db.QueryRow(`PRAGMA page_size`).Scan(&size)
		dbBytes = pages * size
		return nil
	})
	return
}

// --- webhook signing key ---------------------------------------------------------------------

// WebhookSecret is the instance's webhook signing key, minted on first use.
func (s *Store) WebhookSecret(instanceID string) (string, error) {
	var v string
	err := s.with(instanceID, func(db *sql.DB) error {
		if _, err := db.Exec(`INSERT OR IGNORE INTO meta (k, v) VALUES ('webhook_secret', ?)`, newWebhookSecret()); err != nil {
			return err
		}
		return db.QueryRow(`SELECT v FROM meta WHERE k = 'webhook_secret'`).Scan(&v)
	})
	return v, err
}

// RotateWebhookSecret replaces the key; the next delivery is signed with the new one.
func (s *Store) RotateWebhookSecret(instanceID string) (string, error) {
	v := newWebhookSecret()
	err := s.with(instanceID, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO meta (k, v) VALUES ('webhook_secret', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, v)
		return err
	})
	return v, err
}
