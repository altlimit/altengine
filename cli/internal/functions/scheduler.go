package functions

// The local scheduler: runs functions whose cron expression is due.
//
// Hosted, a scheduler runs this once a minute. Locally it is a goroutine on the same
// clock, and the SEMANTICS are what matter — a schedule that behaves
// differently here than in production makes the emulator worse than not having one:
//
//	at-most-once   A run that fails is not retried; the error is logged.
//	at-or-after    A due run may start late, never early.
//	ONE AT A TIME  A run still in flight excludes its own function from the due-set, so a
//	               job slower than its interval never overlaps itself. The occurrences it
//	               runs through are SKIPPED, not queued.
//	BOUNDED        A run is waited on for 30s, then recorded as "timeout" and released; a
//	               tick starts at most 25 due jobs, claiming each only as it starts.
//
// Ticking on the minute boundary (not every 60s from process start) is deliberate: `0 3 *
// * *` should fire at 03:00:00, and a drifting ticker would fire it at whatever offset the
// process happened to start at.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Hosted bounds, mirrored so a schedule that misbehaves there misbehaves here too.
const (
	// How long the scheduler waits on one scheduled run. Past it the run is recorded as
	// "timeout" and its lease is released, so the next occurrence may start.
	scheduledRunTimeout = 30 * time.Second
	// How many of one organization's due jobs one tick starts. The emulator is one
	// organization, so this is the whole tick. What is not started stays due — not
	// advanced, not claimed — and sorts first next tick, being more overdue.
	maxRunsPerOrgPerTick = 25
)

type schedState struct {
	mu         sync.Mutex
	next       map[string]time.Time // fn key -> next due time
	running    map[string]bool      // fn key -> a run is in flight
	lastStatus map[string]string    // fn key -> "http:200", "timeout", "error:..."
	runTimeout time.Duration
}

func newSchedState() *schedState {
	return &schedState{next: map[string]time.Time{}, running: map[string]bool{},
		lastStatus: map[string]string{}, runTimeout: scheduledRunTimeout}
}

// StartScheduler runs the tick loop until ctx is cancelled.
func (h *Handler) StartScheduler(ctx context.Context) {
	if h.sched == nil {
		h.sched = newSchedState()
	}
	go func() {
		for {
			now := time.Now().UTC()
			// Sleep to the next minute boundary rather than for a fixed minute.
			wake := now.Truncate(time.Minute).Add(time.Minute)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(wake)):
				h.runDue(ctx, time.Now().UTC())
			}
		}
	}()
}

func fnKey(instanceID, name string) string { return instanceID + "/" + name }

// runDue starts the functions that are due at `now`, most overdue first, at most
// maxRunsPerOrgPerTick of them.
func (h *Handler) runDue(ctx context.Context, now time.Time) {
	type dueRun struct {
		key  string
		in   *control.Instance
		fn   *Function
		next time.Time
	}
	var due []dueRun
	for _, in := range h.reg.List("functions") {
		cfg := h.store.Config(in.ID)
		if cfg == nil {
			continue
		}
		for _, fn := range cfg.Functions {
			if len(fn.Schedules) == 0 || fn.ActiveVersion == 0 {
				continue
			}
			key := fnKey(in.ID, fn.Name)

			h.sched.mu.Lock()
			// First sighting: schedule forward from now. A function is never run just
			// because the emulator started — only because its expression came due while
			// it was watching.
			next, seen := h.sched.next[key]
			if !seen {
				h.sched.next[key] = Soonest(fn.Schedules, now)
			} else if !next.IsZero() && !now.Before(next) && !h.sched.running[key] {
				due = append(due, dueRun{key, in, fn, next})
			}
			h.sched.mu.Unlock()
		}
	}
	// Most overdue first, so a row passed over by the cap sorts earlier next tick.
	sort.SliceStable(due, func(i, j int) bool { return due[i].next.Before(due[j].next) })
	if len(due) > maxRunsPerOrgPerTick {
		due = due[:maxRunsPerOrgPerTick]
	}

	for _, d := range due {
		h.sched.mu.Lock()
		// Claim only when the run is about to start: advance and mark running in one
		// critical section, so a slow run cannot be started twice, and a row this tick
		// does not reach is never advanced past an occurrence that did not run.
		if h.sched.running[d.key] {
			h.sched.mu.Unlock()
			continue
		}
		h.sched.next[d.key] = Soonest(d.fn.Schedules, now)
		h.sched.running[d.key] = true
		timeout := h.sched.runTimeout
		h.sched.mu.Unlock()

		go func(d dueRun) {
			status := h.runScheduledWithTimeout(ctx, d.in, d.fn, now, timeout)
			h.sched.mu.Lock()
			h.sched.lastStatus[d.key] = status
			delete(h.sched.running, d.key)
			h.sched.mu.Unlock()
		}(d)
	}
}

// runScheduledWithTimeout waits on one scheduled run for at most timeout. A run past it is
// no longer waited for and is recorded as "timeout", releasing its lease; the invocation's
// own wall-clock interrupt ends it.
func (h *Handler) runScheduledWithTimeout(ctx context.Context, in *control.Instance, fn *Function, now time.Time, timeout time.Duration) string {
	done := make(chan string, 1)
	go func() { done <- h.invokeScheduled(ctx, in, fn, now) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s := <-done:
		return s
	case <-timer.C:
		log.Printf("[%s/%s] scheduled run timed out after %s", in.Name, fn.Name, timeout)
		return "timeout"
	}
}

// invokeScheduled runs one function through the ordinary invocation path, returning the
// status recorded for the run.
//
// Same path as an HTTP call on purpose: a scheduled run differs only in `x-ae-trigger`,
// so anything the runtime does for a request (bindings, egress rules, secrets) it does
// here too, and there is no second implementation to keep in sync.
func (h *Handler) invokeScheduled(_ context.Context, in *control.Instance, fn *Function, now time.Time) string {
	cfg := h.store.Config(in.ID)
	src, ok := h.store.Code(in.ID, fn.Name, fn.ActiveVersion)
	if !ok {
		log.Printf("[%s/%s] scheduled run skipped: no code for version %d", in.Name, fn.Name, fn.ActiveVersion)
		return "error:no code"
	}
	// `crons` is ALWAYS a list, matching hosted, so a function that later gains a second
	// schedule does not see the shape of its own payload change.
	body := []byte(fmt.Sprintf(`{"crons":[%s],"scheduled_for":%d}`, quoteList(fn.Schedules), now.UnixMilli()))
	req, _ := http.NewRequest(http.MethodPost, "http://scheduler.local/"+fn.Name, strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "altengine-scheduler")
	req.Header.Set("x-ae-trigger", "cron")
	req.Header.Set("x-ae-request-id", common.UUID())
	req.Header.Set("x-ae-fn", fn.Name)
	req.Header.Set("x-ae-version", strconv.Itoa(fn.ActiveVersion))

	status, _, _, err := h.run(&invocation{
		instanceName: in.Name,
		fn:           fn,
		cfg:          cfg,
		source:       src,
		cacheKey:     fmt.Sprintf("%s:%s:%d", in.ID, fn.Name, fn.ActiveVersion),
		req:          req,
		body:         body,
		bindings:     h.binder,
		logf: func(level, msg string) {
			log.Printf("[%s/%s] cron %s: %s", in.Name, fn.Name, level, msg)
		},
	})
	if err != nil {
		log.Printf("[%s/%s] scheduled run failed: %v", in.Name, fn.Name, err)
		msg := err.Error()
		if len(msg) > 80 {
			msg = msg[:80]
		}
		return "error:" + msg
	}
	log.Printf("[%s/%s] scheduled run -> %d", in.Name, fn.Name, status)
	return fmt.Sprintf("http:%d", status)
}

func quoteList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, strconv.Quote(x))
	}
	return strings.Join(parts, ",")
}
