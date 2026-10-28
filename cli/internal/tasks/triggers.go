package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"

	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/altlimit/altengine/cli/internal/datastore"
	"github.com/altlimit/altengine/cli/internal/mailbox"
)

// Triggers: a datastore write, a blob upload or delete, or a mailbox route becomes a TASK in a
// queue.
//
// THE SOURCE DECIDES WHAT HAPPENED; THE QUEUE DECIDES WHAT TO DO. A rule names a queue, never a
// function: the queue owns the target, the retries and the rate.
//
// BEST EFFORT, NEVER IN THE WRITE'S WAY, as hosted: a write returns what it always returned
// whether or not a rule exists, matches, or can be enqueued. The enqueue runs after the write,
// in the background; one that cannot be enqueued is dropped and logged.
//
// LOOP GUARD. Every event is one deeper than the write that fired it (a function run by a task
// writes at that task's depth). Past MaxTriggerDepth the event is dropped — a function that
// writes the document that triggers it would otherwise run for ever.

const (
	// MaxTriggers is rules per instance.
	MaxTriggers = 20
	// MaxEventDocBytes is one document carried in an event; a larger one is sent as its key alone.
	MaxEventDocBytes = 32 * 1024
	// MaxEventsPerTask is events folded into one task.
	MaxEventsPerTask = 100
	// mailPayloadMax is the largest message a mailbox route hands a queue whole; past it the task
	// carries the summary and the handler reads the rest by id.
	mailPayloadMax = 120 * 1024
	fireAttempts   = 3
)

// DatastoreTrigger is one datastore rule.
type DatastoreTrigger struct {
	Match    DatastoreMatch  `json:"match"`
	Queue    string          `json:"queue"`
	Params   json.RawMessage `json:"params,omitempty"`
	Document *bool           `json:"document,omitempty"`
}

// DatastoreMatch is what a datastore rule matches.
type DatastoreMatch struct {
	Collection string   `json:"collection"`
	Namespace  *string  `json:"namespace,omitempty"`
	Ops        []string `json:"ops,omitempty"`
}

// BlobTrigger is one blob rule.
type BlobTrigger struct {
	Match  BlobMatch       `json:"match"`
	Queue  string          `json:"queue"`
	Params json.RawMessage `json:"params,omitempty"`
}

// BlobMatch is what a blob rule matches.
type BlobMatch struct {
	Prefix      string   `json:"prefix,omitempty"`
	ContentType string   `json:"contentType,omitempty"`
	Ops         []string `json:"ops,omitempty"`
}

func trigOps(v any, allowed []string, what string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	msg := fmt.Sprintf("%s must be a non-empty list of: %s", what, strings.Join(allowed, ", "))
	if !ok || len(list) == 0 {
		return nil, bad(msg)
	}
	var out []string
	seen := map[string]bool{}
	for _, x := range list {
		s, _ := x.(string)
		valid := false
		for _, a := range allowed {
			valid = valid || s == a
		}
		if !valid {
			return nil, bad(msg)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func trigParams(v any, what string) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	o, err := obj(v, what)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(o)
	if len(b) > MaxParamsBytes {
		return nil, bad(what + " is larger than 4 KB")
	}
	return b, nil
}

func trigPattern(v any, what string, max int) (string, error) {
	if v == nil || v == "" {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || len([]rune(s)) > max {
		return "", bad(fmt.Sprintf("%s must be a string of at most %d characters", what, max))
	}
	if strings.TrimSpace(s) == "" {
		return "", bad(what + " is blank; leave it out to match anything")
	}
	return strings.TrimSpace(s), nil
}

func trigList(input any, what string) ([]any, error) {
	if input == nil {
		return nil, nil
	}
	list, ok := input.([]any)
	if !ok {
		return nil, bad(what + " must be an array")
	}
	if len(list) > MaxTriggers {
		return nil, bad(fmt.Sprintf("at most %d triggers", MaxTriggers))
	}
	return list, nil
}

// ValidateDatastoreTriggers is the strict check of a datastore instance's `triggers`.
func ValidateDatastoreTriggers(input any, what string) ([]DatastoreTrigger, error) {
	list, err := trigList(input, what)
	if err != nil {
		return nil, err
	}
	out := []DatastoreTrigger{}
	for i, r := range list {
		w := fmt.Sprintf("%s[%d]", what, i)
		o, err := obj(r, w)
		if err != nil {
			return nil, err
		}
		rawMatch := o["match"]
		if rawMatch == nil {
			rawMatch = map[string]any{}
		}
		m, err := obj(rawMatch, w+".match")
		if err != nil {
			return nil, err
		}
		coll, err := trigPattern(m["collection"], w+".match.collection", 100)
		if err != nil {
			return nil, err
		}
		if coll == "" {
			return nil, bad(w + ".match.collection is required ('*' for every collection)")
		}
		if m["namespace"] != nil {
			if _, ok := m["namespace"].(string); !ok {
				return nil, bad(w + ".match.namespace must be a string")
			}
		}
		if o["document"] != nil {
			if _, ok := o["document"].(bool); !ok {
				return nil, bad(w + ".document must be a boolean")
			}
		}
		t := DatastoreTrigger{Match: DatastoreMatch{Collection: coll}}
		if t.Queue, err = ValidateQueueRef(o["queue"], w+".queue"); err != nil {
			return nil, err
		}
		if ns, ok := m["namespace"].(string); ok {
			if ns == "_default" {
				ns = ""
			}
			t.Match.Namespace = &ns
		}
		if t.Match.Ops, err = trigOps(m["ops"], []string{"create", "update", "delete"}, w+".match.ops"); err != nil {
			return nil, err
		}
		if t.Params, err = trigParams(o["params"], w+".params"); err != nil {
			return nil, err
		}
		if o["document"] == false {
			f := false
			t.Document = &f
		}
		out = append(out, t)
	}
	return out, nil
}

// ValidateBlobTriggers is the strict check of a blob instance's `triggers`.
func ValidateBlobTriggers(input any, what string) ([]BlobTrigger, error) {
	list, err := trigList(input, what)
	if err != nil {
		return nil, err
	}
	out := []BlobTrigger{}
	for i, r := range list {
		w := fmt.Sprintf("%s[%d]", what, i)
		o, err := obj(r, w)
		if err != nil {
			return nil, err
		}
		rawMatch := o["match"]
		if rawMatch == nil {
			rawMatch = map[string]any{}
		}
		m, err := obj(rawMatch, w+".match")
		if err != nil {
			return nil, err
		}
		if p := m["prefix"]; p != nil {
			if s, ok := p.(string); !ok || len([]rune(s)) > 512 {
				return nil, bad(w + ".match.prefix must be a string of at most 512 characters")
			}
		}
		t := BlobTrigger{}
		if t.Queue, err = ValidateQueueRef(o["queue"], w+".queue"); err != nil {
			return nil, err
		}
		if s, ok := m["prefix"].(string); ok {
			t.Match.Prefix = s
		}
		if t.Match.ContentType, err = trigPattern(m["contentType"], w+".match.contentType", 100); err != nil {
			return nil, err
		}
		if t.Match.Ops, err = trigOps(m["ops"], []string{"create", "delete"}, w+".match.ops"); err != nil {
			return nil, err
		}
		if t.Params, err = trigParams(o["params"], w+".params"); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// parseDatastoreTriggers is the tolerant read: a rule list that no longer validates is dropped
// whole rather than half-applied.
func parseDatastoreTriggers(cfg map[string]any) []DatastoreTrigger {
	t, err := ValidateDatastoreTriggers(cfg["triggers"], "triggers")
	if err != nil {
		return nil
	}
	return t
}

func parseBlobTriggers(cfg map[string]any) []BlobTrigger {
	t, err := ValidateBlobTriggers(cfg["triggers"], "triggers")
	if err != nil {
		return nil
	}
	return t
}

// --- config checks: registered on the source services by the server ---------------------------

// CheckQueues: every queue a rule list names must exist — checked at save time, where the person
// who typed it can still fix it, and again when an event fires.
func (s *Service) CheckQueues(refs []string) error {
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		inst, name := SplitRef(ref)
		in := s.reg.Get("tasks", inst)
		if in == nil {
			return bad(fmt.Sprintf("no tasks instance '%s' in this organization", inst))
		}
		if _, err := s.store.GetQueue(in.ID, name); err != nil {
			return bad(fmt.Sprintf("tasks instance '%s' has no queue '%s'", inst, name))
		}
	}
	return nil
}

// ValidateDatastoreConfig checks a datastore config's `triggers`.
func (s *Service) ValidateDatastoreConfig(cfg map[string]any) error {
	rules, err := ValidateDatastoreTriggers(cfg["triggers"], "config.triggers")
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(rules))
	for _, r := range rules {
		refs = append(refs, r.Queue)
	}
	return s.CheckQueues(refs)
}

// ValidateBlobConfig checks a blob config's `triggers`.
func (s *Service) ValidateBlobConfig(cfg map[string]any) error {
	rules, err := ValidateBlobTriggers(cfg["triggers"], "triggers")
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(rules))
	for _, r := range rules {
		refs = append(refs, r.Queue)
	}
	return s.CheckQueues(refs)
}

// ValidateMailboxConfig checks that every queue a mailbox route hands mail to exists. The route
// list's shape is the mailbox's own check, which runs first.
func (s *Service) ValidateMailboxConfig(cfg map[string]any) error {
	routes, err := mailbox.ValidateRoutes(cfg["routes"])
	if err != nil {
		return err
	}
	var refs []string
	for _, r := range routes {
		for _, a := range r.Do {
			if a.Queue != "" {
				refs = append(refs, a.Queue)
			}
		}
	}
	return s.CheckQueues(refs)
}

// NormalizedTriggers rewrites a saved config's `triggers` to their normalized form (trimmed,
// `_default` read as the default namespace, duplicate ops dropped) — what the hosted service
// stores and returns.
func NormalizedTriggers(service string, cfg map[string]any) (any, bool) {
	raw, present := cfg["triggers"]
	if !present || raw == nil {
		return nil, false
	}
	var v any
	var err error
	if service == "datastore" {
		v, err = ValidateDatastoreTriggers(raw, "triggers")
	} else {
		v, err = ValidateBlobTriggers(raw, "triggers")
	}
	if err != nil {
		return nil, false
	}
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out, true
}

// --- matching and payloads ---------------------------------------------------------------------

func datastoreWatches(rules []DatastoreTrigger, namespace, collection string) bool {
	for _, r := range rules {
		if (r.Match.Namespace == nil || *r.Match.Namespace == namespace) && mailbox.GlobMatch(r.Match.Collection, collection) {
			return true
		}
	}
	return false
}

func hasOp(ops []string, op string) bool {
	if len(ops) == 0 {
		return true
	}
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// EventTask is one task an event produces.
type EventTask struct {
	Queue   string
	Payload any
	Params  json.RawMessage
}

type dsEvent struct {
	Op              string          `json:"op"`
	Key             string          `json:"key"`
	Doc             json.RawMessage `json:"doc,omitempty"`
	DocTruncated    bool            `json:"docTruncated,omitempty"`
	Before          json.RawMessage `json:"before,omitempty"`
	BeforeTruncated bool            `json:"beforeTruncated,omitempty"`
}

type dsPayload struct {
	Type       string    `json:"type"`
	Instance   string    `json:"instance"`
	Namespace  string    `json:"namespace"`
	Collection string    `json:"collection"`
	Events     []dsEvent `json:"events"`
}

func boundedDoc(raw json.RawMessage) (json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	if len(raw) <= MaxEventDocBytes {
		return raw, false
	}
	return nil, true
}

// DatastoreEventTasks is the tasks a datastore write produces: per matching rule, one task per
// collection, its events in chunks of at most MaxEventsPerTask that fit a payload.
func DatastoreEventTasks(rules []DatastoreTrigger, sourceName, namespace string, changes []datastore.WriteChange) []EventTask {
	var out []EventTask
	for _, r := range rules {
		byColl := map[string][]datastore.WriteChange{}
		var order []string
		for _, c := range changes {
			if r.Match.Namespace != nil && *r.Match.Namespace != namespace {
				continue
			}
			if !mailbox.GlobMatch(r.Match.Collection, c.Collection) || !hasOp(r.Match.Ops, c.Op) {
				continue
			}
			if _, ok := byColl[c.Collection]; !ok {
				order = append(order, c.Collection)
			}
			byColl[c.Collection] = append(byColl[c.Collection], c)
		}
		for _, coll := range order {
			var chunk []dsEvent
			bytes := 0
			flush := func() {
				if len(chunk) == 0 {
					return
				}
				out = append(out, EventTask{Queue: r.Queue, Params: r.Params,
					Payload: dsPayload{Type: "datastore.write", Instance: sourceName, Namespace: namespace, Collection: coll, Events: chunk}})
				chunk, bytes = nil, 0
			}
			for _, c := range byColl[coll] {
				e := dsEvent{Op: c.Op, Key: c.Key}
				if r.Document == nil || *r.Document {
					e.Doc, e.DocTruncated = boundedDoc(c.Doc)
					if c.Before != nil {
						e.Before, e.BeforeTruncated = boundedDoc(c.Before)
					}
				}
				b, _ := json.Marshal(e)
				if len(chunk) >= MaxEventsPerTask || bytes+len(b) > MaxPayloadBytes-4096 {
					flush()
				}
				chunk = append(chunk, e)
				bytes += len(b)
			}
			flush()
		}
	}
	return out
}

type blobPayload struct {
	Type         string         `json:"type"`
	Instance     string         `json:"instance"`
	ID           string         `json:"id"`
	Key          string         `json:"key"`
	Name         string         `json:"name"`
	Size         int64          `json:"size"`
	ContentType  string         `json:"contentType"`
	ETag         string         `json:"etag"`
	Public       bool           `json:"public"`
	Meta         map[string]any `json:"meta"`
	URL          string         `json:"url,omitempty"`
	URLExpiresAt int64          `json:"urlExpiresAt,omitempty"`
}

// BlobEventTasks is the tasks one blob change produces.
func BlobEventTasks(rules []BlobTrigger, sourceName string, c blob.Change) []EventTask {
	var out []EventTask
	for _, r := range rules {
		if !hasOp(r.Match.Ops, c.Op) {
			continue
		}
		if r.Match.Prefix != "" && !strings.HasPrefix(c.Record.Name, r.Match.Prefix) {
			continue
		}
		if r.Match.ContentType != "" && !mailbox.GlobMatch(r.Match.ContentType, c.Record.ContentType) {
			continue
		}
		meta := c.Record.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		if b, _ := json.Marshal(meta); len(b) > MaxEventDocBytes {
			meta = map[string]any{}
		}
		typ := "blob.delete"
		if c.Op == "create" {
			typ = "blob.create"
		}
		out = append(out, EventTask{Queue: r.Queue, Params: r.Params, Payload: blobPayload{
			Type: typ, Instance: sourceName, ID: c.Record.ID, Key: c.Key, Name: c.Record.Name, Size: c.Record.Size,
			ContentType: c.Record.ContentType, ETag: c.Record.ETag, Public: c.Record.Public, Meta: meta,
			URL: c.URL, URLExpiresAt: c.URLExpiresAt,
		}})
	}
	return out
}

// --- the sinks the source services call ----------------------------------------------------------

// DatastoreWatches implements datastore.TriggerSink: whether any rule could match a write to
// this collection — asked before the write, so a collection nobody watches pays nothing.
func (s *Service) DatastoreWatches(in *control.Instance, namespace, collection string) bool {
	rules := parseDatastoreTriggers(in.Config())
	return len(rules) > 0 && datastoreWatches(rules, namespace, collection)
}

// DatastoreWrite implements datastore.TriggerSink: fire the tasks a committed write produces.
func (s *Service) DatastoreWrite(in *control.Instance, namespace string, changes []datastore.WriteChange, writerDepth int) {
	if len(changes) == 0 {
		return
	}
	tasks := DatastoreEventTasks(parseDatastoreTriggers(in.Config()), in.Name, namespace, changes)
	s.fire("datastore", in.Name, "datastore", writerDepth, tasks)
}

// BlobWatches implements blob.TriggerSink.
func (s *Service) BlobWatches(in *control.Instance) bool {
	return len(parseBlobTriggers(in.Config())) > 0
}

// BlobChanged implements blob.TriggerSink.
func (s *Service) BlobChanged(in *control.Instance, c blob.Change, writerDepth int) {
	tasks := BlobEventTasks(parseBlobTriggers(in.Config()), in.Name, c)
	s.fire("blob", in.Name, "blob", writerDepth, tasks)
}

// EnqueueMail implements mailbox.QueueSink: a mailbox route hands a message to a queue. The task
// is named per message and route, so a retried delivery is refused as a duplicate rather than
// added twice — and that refusal still counts as delivered.
func (s *Service) EnqueueMail(ref, messageID string, payload, summary []byte) (status, why string, permanent bool) {
	inst, name := SplitRef(ref)
	in := s.reg.Get("tasks", inst)
	if in == nil {
		return "", fmt.Sprintf("no tasks instance '%s'", inst), true
	}
	body := payload
	if len(body) > mailPayloadMax {
		body = summary
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ref))
	taskName := fmt.Sprintf("mail-%s-%x", messageID, h.Sum32())
	_, err := s.add(in, name, []TaskInput{{Name: &taskName, Payload: string(body), RunAt: s.now()}}, "mailbox", 1)
	if err != nil {
		var ae *common.APIError
		if errors.As(err, &ae) {
			return "", ae.Message, ae.Status >= 400 && ae.Status < 500
		}
		return "", err.Error(), false
	}
	return "ok:queued", "", false
}

func newEventName(source string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return source + "-" + hex.EncodeToString(b)
}

// fire enqueues each event task in the background. NEVER blocks or fails the caller.
func (s *Service) fire(service, sourceName, source string, writerDepth int, tasks []EventTask) {
	if len(tasks) == 0 {
		return
	}
	depth := writerDepth + 1
	if depth > MaxTriggerDepth {
		var queues []string
		for _, t := range tasks {
			queues = append(queues, t.Queue)
		}
		log.Printf("[triggers] %s/%s: a chain of triggers %d deep was cut (queues %s); a function is likely writing what triggers it",
			service, sourceName, MaxTriggerDepth, strings.Join(queues, ", "))
		return
	}
	now := s.now()
	byQueue := map[string][]TaskInput{}
	var order []string
	for _, t := range tasks {
		payload, err := json.Marshal(t.Payload)
		if err != nil {
			continue
		}
		name := newEventName(source)
		in := TaskInput{Name: &name, Payload: string(payload), RunAt: now}
		if len(t.Params) > 0 {
			p := string(t.Params)
			in.Params = &p
		}
		if _, ok := byQueue[t.Queue]; !ok {
			order = append(order, t.Queue)
		}
		byQueue[t.Queue] = append(byQueue[t.Queue], in)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for _, ref := range order {
			inst, name := SplitRef(ref)
			var lastErr error
			for attempt := 0; attempt < fireAttempts; attempt++ {
				in := s.reg.Get("tasks", inst)
				if in == nil {
					lastErr = fmt.Errorf("no tasks instance '%s'", inst)
					break
				}
				_, lastErr = s.add(in, name, byQueue[ref], source, depth)
				var ae *common.APIError
				// A missing queue or a refused task does not get better by trying again.
				if lastErr == nil || (errors.As(lastErr, &ae) && ae.Status >= 400 && ae.Status < 500) {
					break
				}
			}
			if lastErr != nil {
				log.Printf("[triggers] %s/%s: could not add to %s: %v", service, sourceName, ref, lastErr)
			}
		}
	}()
}
