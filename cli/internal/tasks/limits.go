// Package tasks emulates the tasks service: queues of work delivered to a function, a channel or
// a signed webhook (push queues), or leased by workers (pull queues), with retries, backoff,
// dead-letter queues and triggers from datastore writes, blob uploads and mailbox routes.
//
// Every bound, validator and message here is the hosted service's, so a queue setting or a task
// accepted here is accepted there, and one refused there is refused here.
package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	// MaxQueues is queues per instance.
	MaxQueues = 50
	// MaxTasksPerAdd is tasks one add call may carry.
	MaxTasksPerAdd = 100
	// MaxPayloadBytes is a task's payload, as JSON.
	MaxPayloadBytes = 128 * 1024
	// MaxParamsBytes is a task's params, as JSON.
	MaxParamsBytes = 4 * 1024
	// MaxResultBytes is how much of a target's answer is kept on the task.
	MaxResultBytes = 16 * 1024
	// MaxDelayMS is how far ahead a task may be scheduled.
	MaxDelayMS = 30 * 86_400_000
	// MaxNameLength is a task name's length.
	MaxNameLength = 500
	// DefaultQueue exists in every instance and cannot be deleted.
	DefaultQueue = "default"

	// MaxInflight is deliveries in flight across one instance at once.
	MaxInflight           = 50
	MaxConcurrentPerQueue = 50
	MaxRatePerSecond      = 100.0
	MaxLeaseCount         = 100
	MaxLeaseSeconds       = 3600
	DefaultLeaseSeconds   = 300
	// DefaultDeadlineSeconds is how long a push delivery is waited for before the attempt counts
	// as failed and is retried — the queue's `deadlineSeconds`, overridable per task. The target
	// is not stopped when it passes; the queue stops waiting. Longer work belongs on a pull queue,
	// whose worker extends its lease for as long as it needs.
	DefaultDeadlineSeconds = 600
	MinDeadlineSeconds     = 15
	MaxDeadlineSeconds     = 840
	// LeaseSlackMS is added to a running task's deadline to make its lease. Past it, the attempt
	// was lost (a restart mid-delivery) and the task is retried like a timeout.
	LeaseSlackMS           = 60_000
	MaxTriggerDepth        = 8
	MaxListLimit           = 200
	defaultListLimit       = 50
	CountCap               = 10_000
	MaxDefers              = 1000
	MaxWebhookURLLength    = 2048
	maxRefLength           = 300
	RetryHeader            = "x-task-retry"
	RetryAfterHeader       = "x-task-retry-after"
	MaxAttemptsHeader      = "x-task-max-attempts"
	DefaultRetentionDays   = 7
	MaxRetentionDays       = 30
	defaultPayloadForModel = 32 * 1024
)

// States a task can be in.
var States = []string{"scheduled", "running", "leased", "done", "failed", "cancelled"}

var (
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	queueNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	fnNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	numericRe   = regexp.MustCompile(`^\d+(\.\d+)?$`)
	digitsRe    = regexp.MustCompile(`^\d+$`)
)

// Target is where a push task is delivered: exactly one field is set.
type Target struct {
	Function string `json:"function,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Webhook  string `json:"webhook,omitempty"`
}

// RetryPolicy bounds a task's attempts.
type RetryPolicy struct {
	MaxAttempts       int `json:"maxAttempts"`
	MinBackoffSeconds int `json:"minBackoffSeconds"`
	MaxBackoffSeconds int `json:"maxBackoffSeconds"`
	MaxDoublings      int `json:"maxDoublings"`
	MaxAgeSeconds     int `json:"maxAgeSeconds"`
}

// QueueSettings is one queue's configuration.
type QueueSettings struct {
	Mode            string      `json:"mode"`
	Target          *Target     `json:"target"`
	RatePerSecond   float64     `json:"ratePerSecond"`
	MaxConcurrent   int         `json:"maxConcurrent"`
	Retry           RetryPolicy `json:"retry"`
	DeadLetterQueue *string     `json:"deadLetterQueue"`
	Paused          bool        `json:"paused"`
	TargetControl   bool        `json:"targetControl"`
	// DeadlineSeconds is how long a push delivery is waited for (15–840, default 600).
	DeadlineSeconds int `json:"deadlineSeconds"`
}

// DefaultRetry is a task's retry policy unless its queue or itself says otherwise.
var DefaultRetry = RetryPolicy{MaxAttempts: 10, MinBackoffSeconds: 10, MaxBackoffSeconds: 3600, MaxDoublings: 8, MaxAgeSeconds: 7 * 86_400}

// DefaultQueueSettings is what a new queue starts with.
func DefaultQueueSettings() QueueSettings {
	return QueueSettings{Mode: "push", RatePerSecond: 10, MaxConcurrent: 5, Retry: DefaultRetry, TargetControl: true, DeadlineSeconds: DefaultDeadlineSeconds}
}

func bad(msg string) error { return common.BadRequest(msg) }

func obj(v any, what string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, bad(what + " must be an object")
	}
	return m, nil
}

func intIn(v any, lo, hi int, what string) (int, error) {
	n, ok := v.(float64)
	if !ok || n != math.Trunc(n) || math.IsInf(n, 0) || n < float64(lo) || n > float64(hi) {
		if i, isInt := v.(int); isInt && i >= lo && i <= hi {
			return i, nil
		}
		return 0, bad(fmt.Sprintf("%s must be an integer from %d to %d", what, lo, hi))
	}
	return int(n), nil
}

func numIn(v any, lo, hi float64, what string) (float64, error) {
	n, ok := common.Number(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < lo || n > hi {
		return 0, bad(fmt.Sprintf("%s must be a number from %s to %s", what, fmtNum(lo), fmtNum(hi)))
	}
	return n, nil
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// ValidateQueueName checks a queue name.
func ValidateQueueName(v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok || !queueNameRe.MatchString(s) {
		return "", bad(what + " must be 1-63 characters of a-z, 0-9, '_' and '-', starting with a letter or digit")
	}
	return s, nil
}

// SplitRef splits `<instance>/<name>` at the FIRST slash — a channel name may contain slashes.
func SplitRef(s string) (instance, name string) {
	at := strings.Index(s, "/")
	if at < 0 {
		return s, ""
	}
	return s[:at], s[at+1:]
}

func ref(v any, what string, checkName func(string) error) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", bad(what + " must be a string '<instance>/<name>'")
	}
	s = strings.TrimSpace(s)
	at := strings.Index(s, "/")
	if at < 1 || at == len(s)-1 {
		return "", bad(what + " must be '<instance>/<name>'")
	}
	if len(s) > maxRefLength {
		return "", bad(what + " is too long")
	}
	if err := checkName(s[at+1:]); err != nil {
		msg := err.Error()
		if e, ok := err.(*common.APIError); ok {
			msg = e.Message
		}
		return "", bad(what + ": " + msg)
	}
	return s, nil
}

// ValidateQueueRef checks `<tasks-instance>/<queue>` — what a trigger rule or a mailbox route names.
func ValidateQueueRef(v any, what string) (string, error) {
	return ref(v, what, func(n string) error {
		_, err := ValidateQueueName(n, "the queue name")
		return err
	})
}

// ValidateWebhookURL: an https URL with no credentials in it. What keeps it from being anyone's
// receiver is the signature on every delivery.
func ValidateWebhookURL(v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", bad(what + " must be an https URL")
	}
	s = strings.TrimSpace(s)
	if len(s) > MaxWebhookURLLength {
		return "", bad(fmt.Sprintf("%s is longer than %d characters", what, MaxWebhookURLLength))
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", bad(what + " must be an https URL")
	}
	if u.Scheme != "https" {
		return "", bad(what + " must be an https URL")
	}
	if u.User != nil {
		return "", bad(what + " may not carry a username or password")
	}
	return s, nil
}

// ValidateTarget checks a push target.
func ValidateTarget(v any, what string) (*Target, error) {
	o, err := obj(v, what)
	if err != nil {
		return nil, err
	}
	kinds := 0
	for _, k := range []string{"function", "channel", "webhook"} {
		if o[k] != nil {
			kinds++
		}
	}
	if kinds != 1 {
		return nil, bad(what + " must have exactly one of 'function', 'channel' or 'webhook'")
	}
	if o["function"] != nil {
		s, err := ref(o["function"], what+".function", func(n string) error {
			if !fnNameRe.MatchString(n) {
				return fmt.Errorf("function name must be 1-63 chars of [a-z0-9_-], starting alphanumeric")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &Target{Function: s}, nil
	}
	if o["channel"] != nil {
		s, err := ref(o["channel"], what+".channel", channel.ValidateChannel)
		if err != nil {
			return nil, err
		}
		return &Target{Channel: s}, nil
	}
	s, err := ValidateWebhookURL(o["webhook"], what+".webhook")
	if err != nil {
		return nil, err
	}
	return &Target{Webhook: s}, nil
}

// TargetLabel is a target as a stable string.
func TargetLabel(t *Target) string {
	switch {
	case t == nil:
		return ""
	case t.Function != "":
		return "function " + t.Function
	case t.Channel != "":
		return "channel " + t.Channel
	}
	return "webhook " + t.Webhook
}

// ValidateRetry checks a retry policy; a field left out keeps base's.
func ValidateRetry(v any, base RetryPolicy, what string) (RetryPolicy, error) {
	if v == nil {
		return base, nil
	}
	o, err := obj(v, what)
	if err != nil {
		return base, err
	}
	r := base
	fields := []struct {
		key    string
		dst    *int
		lo, hi int
	}{
		{"maxAttempts", &r.MaxAttempts, 1, 100},
		{"minBackoffSeconds", &r.MinBackoffSeconds, 1, 86_400},
		{"maxBackoffSeconds", &r.MaxBackoffSeconds, 1, 7 * 86_400},
		{"maxDoublings", &r.MaxDoublings, 0, 16},
		{"maxAgeSeconds", &r.MaxAgeSeconds, 0, 30 * 86_400},
	}
	for _, f := range fields {
		if o[f.key] == nil {
			continue
		}
		n, err := intIn(o[f.key], f.lo, f.hi, what+"."+f.key)
		if err != nil {
			return base, err
		}
		*f.dst = n
	}
	if r.MaxBackoffSeconds < r.MinBackoffSeconds {
		return base, bad(what + ".maxBackoffSeconds must be at least minBackoffSeconds")
	}
	return r, nil
}

// ValidateQueueSettings is the strict check of a queue's settings. `base` is what an omitted
// field keeps — the current settings on an update, the defaults on a create — so a caller
// changing the rate does not reset the target. A PATCH by design.
func ValidateQueueSettings(v any, base QueueSettings, selfName string) (QueueSettings, error) {
	o := map[string]any{}
	if v != nil {
		m, err := obj(v, "queue settings")
		if err != nil {
			return base, err
		}
		o = m
	}
	mode := base.Mode
	if o["mode"] != nil {
		s, _ := o["mode"].(string)
		mode = s
	}
	if mode != "push" && mode != "pull" {
		return base, bad("mode must be 'push' or 'pull'")
	}
	target := base.Target
	if raw, present := o["target"]; present {
		if raw == nil {
			target = nil
		} else {
			t, err := ValidateTarget(raw, "target")
			if err != nil {
				return base, err
			}
			target = t
		}
	}
	dlq := base.DeadLetterQueue
	if raw, present := o["deadLetterQueue"]; present {
		if raw == nil || raw == "" {
			dlq = nil
		} else {
			n, err := ValidateQueueName(raw, "deadLetterQueue")
			if err != nil {
				return base, err
			}
			dlq = &n
		}
	}
	if dlq != nil && selfName != "" && *dlq == selfName {
		return base, bad("a queue cannot be its own dead-letter queue")
	}
	if p := o["paused"]; p != nil {
		if _, ok := p.(bool); !ok {
			return base, bad("paused must be a boolean")
		}
	}
	if p := o["targetControl"]; p != nil {
		if _, ok := p.(bool); !ok {
			return base, bad("targetControl must be a boolean")
		}
	}
	out := QueueSettings{Mode: mode, Target: target, RatePerSecond: base.RatePerSecond, MaxConcurrent: base.MaxConcurrent,
		DeadLetterQueue: dlq, Paused: base.Paused, TargetControl: base.TargetControl, DeadlineSeconds: base.DeadlineSeconds}
	if out.DeadlineSeconds == 0 {
		out.DeadlineSeconds = DefaultDeadlineSeconds
	}
	if o["deadlineSeconds"] != nil {
		n, err := intIn(o["deadlineSeconds"], MinDeadlineSeconds, MaxDeadlineSeconds, "deadlineSeconds")
		if err != nil {
			return base, err
		}
		out.DeadlineSeconds = n
	}
	// A pull queue is never delivered to, so a target on one would be a setting that does nothing.
	if mode == "pull" {
		out.Target = nil
	}
	if o["ratePerSecond"] != nil {
		n, err := numIn(o["ratePerSecond"], 0.01, MaxRatePerSecond, "ratePerSecond")
		if err != nil {
			return base, err
		}
		out.RatePerSecond = n
	}
	if o["maxConcurrent"] != nil {
		n, err := intIn(o["maxConcurrent"], 1, MaxConcurrentPerQueue, "maxConcurrent")
		if err != nil {
			return base, err
		}
		out.MaxConcurrent = n
	}
	r, err := ValidateRetry(o["retry"], base.Retry, "retry")
	if err != nil {
		return base, err
	}
	out.Retry = r
	if b, ok := o["paused"].(bool); ok {
		out.Paused = b
	}
	if b, ok := o["targetControl"].(bool); ok {
		out.TargetControl = b
	}
	return out, nil
}

// --- the target steering its own retries ---------------------------------------------------------
//
// With the queue's `targetControl` on (the default), a target's answer can say:
//
//	Retry-After / x-task-retry-after   seconds, or an HTTP date: when the next attempt runs.
//	x-task-retry: never                fail now.
//	x-task-retry: retry                count this as a failed attempt and retry, even a 2xx.
//	x-task-retry: defer                run again later WITHOUT spending an attempt.
//	x-task-max-attempts: n             this task's attempt limit from now on (1-100).

// TargetControl is what a target's response headers ask for.
type TargetControl struct {
	Retry       string // "", "never", "retry" or "defer"
	RetryAt     int64  // epoch ms, 0 when not given
	MaxAttempts int    // 0 when not given
}

func (c *TargetControl) empty() bool { return c.Retry == "" && c.RetryAt == 0 && c.MaxAttempts == 0 }

// ReadTargetControl reads a target's response headers, or nil when they ask for nothing.
func ReadTargetControl(h http.Header, now int64) *TargetControl {
	out := &TargetControl{}
	switch r := strings.ToLower(strings.TrimSpace(h.Get(RetryHeader))); r {
	case "never", "retry", "defer":
		out.Retry = r
	}
	after := strings.TrimSpace(h.Get(RetryAfterHeader))
	if after == "" {
		after = strings.TrimSpace(h.Get("Retry-After"))
	}
	if after != "" {
		var at int64
		found := false
		if numericRe.MatchString(after) {
			f, _ := strconv.ParseFloat(after, 64)
			at, found = now+int64(math.Round(f*1000)), true
		} else if t, ok := parseDate(after); ok {
			at, found = t, true
		}
		if found {
			if at < now {
				at = now
			}
			if at > now+MaxDelayMS {
				at = now + MaxDelayMS
			}
			out.RetryAt = at
		}
	}
	if m := strings.TrimSpace(h.Get(MaxAttemptsHeader)); m != "" && digitsRe.MatchString(m) {
		if n, err := strconv.Atoi(m); err == nil && n >= 1 && n <= 100 {
			out.MaxAttempts = n
		}
	}
	if out.empty() {
		return nil
	}
	return out
}

// parseDate reads an HTTP date or an ISO 8601 time as epoch ms.
func parseDate(s string) (int64, bool) {
	if t, err := http.ParseTime(s); err == nil {
		return t.UnixMilli(), true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// TaskInput is one task as a caller sends it, validated and normalized.
type TaskInput struct {
	Name    *string
	Target  *Target
	Payload string // JSON text, size-checked
	Params  *string
	RunAt   int64
	Retry   *RetryPolicy
	// DeadlineSeconds overrides the queue's for this task; nil means the queue's.
	DeadlineSeconds *int
}

// ValidateTaskName checks a task name; empty is no name.
func ValidateTaskName(v any, what string) (*string, error) {
	if v == nil || v == "" {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok || len([]rune(s)) > MaxNameLength || !nameRe.MatchString(s) {
		return nil, bad(fmt.Sprintf("%s must be 1-%d characters of A-Z, a-z, 0-9, '_', '.', ':' and '-'", what, MaxNameLength))
	}
	return &s, nil
}

func compactJSON(raw json.RawMessage, max int, what string) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", bad(what + " must be JSON")
	}
	if buf.Len() > max {
		return "", bad(fmt.Sprintf("%s is larger than %d KB; put large data in a blob and pass its key", what, int(math.Round(float64(max)/1024))))
	}
	return buf.String(), nil
}

func decodeAny(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// ValidateTaskInput validates one task. `delay` is seconds from now; `runAt` is epoch ms or an
// ISO time; both is an error; neither is "now".
func ValidateTaskInput(raw json.RawMessage, now int64, what string) (TaskInput, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil || strings.TrimSpace(string(raw))[0] != '{' {
		return TaskInput{}, bad(what + " must be an object")
	}
	present := func(k string) bool {
		v, ok := o[k]
		return ok && strings.TrimSpace(string(v)) != "null"
	}
	if present("delay") && present("runAt") {
		return TaskInput{}, bad(what + ": give delay or runAt, not both")
	}
	runAt := now
	if present("delay") {
		d, ok := decodeAny(o["delay"]).(float64)
		if !ok || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			return TaskInput{}, bad(what + ".delay must be a number of seconds, 0 or more")
		}
		runAt = now + int64(math.Round(d*1000))
	} else if present("runAt") {
		var t int64
		ok := false
		switch v := decodeAny(o["runAt"]).(type) {
		case float64:
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				t, ok = int64(math.Round(v)), true
			}
		case string:
			t, ok = parseDate(v)
		}
		if !ok {
			return TaskInput{}, bad(what + ".runAt must be epoch milliseconds or an ISO 8601 time")
		}
		if t > runAt {
			runAt = t
		}
	}
	if runAt-now > MaxDelayMS {
		return TaskInput{}, bad(what + " is scheduled more than 30 days ahead")
	}
	payloadRaw, ok := o["payload"]
	if !ok {
		return TaskInput{}, bad(what + ".payload is required (send null for none)")
	}
	in := TaskInput{RunAt: runAt}
	var err error
	if in.Name, err = ValidateTaskName(decodeAny(o["name"]), what+".name"); err != nil {
		return TaskInput{}, err
	}
	if present("target") {
		if in.Target, err = ValidateTarget(decodeAny(o["target"]), what+".target"); err != nil {
			return TaskInput{}, err
		}
	}
	if in.Payload, err = compactJSON(payloadRaw, MaxPayloadBytes, what+".payload"); err != nil {
		return TaskInput{}, err
	}
	if present("params") {
		if _, err := obj(decodeAny(o["params"]), what+".params"); err != nil {
			return TaskInput{}, err
		}
		p, err := compactJSON(o["params"], MaxParamsBytes, what+".params")
		if err != nil {
			return TaskInput{}, err
		}
		in.Params = &p
	}
	if present("retry") {
		r, err := ValidateRetry(decodeAny(o["retry"]), DefaultRetry, what+".retry")
		if err != nil {
			return TaskInput{}, err
		}
		in.Retry = &r
	}
	if present("deadlineSeconds") {
		n, err := intIn(decodeAny(o["deadlineSeconds"]), MinDeadlineSeconds, MaxDeadlineSeconds, what+".deadlineSeconds")
		if err != nil {
			return TaskInput{}, err
		}
		in.DeadlineSeconds = &n
	}
	return in, nil
}

// ValidateTaskList reads a task object, an array of tasks, or { tasks: [...] }.
func ValidateTaskList(body json.RawMessage, now int64) ([]TaskInput, error) {
	t := bytes.TrimSpace(body)
	var list []json.RawMessage
	switch {
	case len(t) > 0 && t[0] == '[':
		if err := json.Unmarshal(t, &list); err != nil {
			return nil, bad("send a task object, an array of tasks, or { tasks: [...] }")
		}
	case len(t) > 0 && t[0] == '{':
		var o map[string]json.RawMessage
		if err := json.Unmarshal(t, &o); err != nil {
			return nil, bad("send a task object, an array of tasks, or { tasks: [...] }")
		}
		if inner, ok := o["tasks"]; ok && len(bytes.TrimSpace(inner)) > 0 && bytes.TrimSpace(inner)[0] == '[' {
			if err := json.Unmarshal(inner, &list); err != nil {
				return nil, bad("send a task object, an array of tasks, or { tasks: [...] }")
			}
		} else {
			list = []json.RawMessage{t}
		}
	default:
		return nil, bad("send a task object, an array of tasks, or { tasks: [...] }")
	}
	if len(list) == 0 {
		return nil, bad("no tasks to add")
	}
	if len(list) > MaxTasksPerAdd {
		return nil, bad(fmt.Sprintf("at most %d tasks per add", MaxTasksPerAdd))
	}
	out := make([]TaskInput, 0, len(list))
	for i, raw := range list {
		what := "task"
		if len(list) > 1 {
			what = fmt.Sprintf("tasks[%d]", i)
		}
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
			return nil, bad(what + " must be an object")
		}
		in, err := ValidateTaskInput(raw, now, what)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// NextAttemptAt is when the next attempt is due after `attempts` failures, or 0 when the task is
// spent. The wait doubles maxDoublings times from minBackoffSeconds, then grows linearly by the
// last doubled step, capped at maxBackoffSeconds — App Engine's rule.
func NextAttemptAt(r RetryPolicy, attempts int, createdAt, now int64) int64 {
	if attempts >= r.MaxAttempts {
		return 0
	}
	doublings := attempts - 1
	if doublings > r.MaxDoublings {
		doublings = r.MaxDoublings
	}
	if doublings < 0 {
		doublings = 0
	}
	wait := float64(r.MinBackoffSeconds) * math.Pow(2, float64(doublings))
	if attempts-1 > r.MaxDoublings {
		wait += float64(attempts-1-r.MaxDoublings) * float64(r.MinBackoffSeconds) * math.Pow(2, float64(r.MaxDoublings))
	}
	if wait > float64(r.MaxBackoffSeconds) {
		wait = float64(r.MaxBackoffSeconds)
	}
	at := now + int64(wait*1000)
	if r.MaxAgeSeconds > 0 && at-createdAt > int64(r.MaxAgeSeconds)*1000 {
		return 0
	}
	return at
}

// ParseState reads a `state` filter: "" or "all" is every state.
func ParseState(v string) (string, error) {
	if v == "" || v == "all" {
		return "", nil
	}
	for _, s := range States {
		if v == s {
			return v, nil
		}
	}
	return "", bad("state must be one of: " + strings.Join(States, ", ") + " (or omit it for every state)")
}
