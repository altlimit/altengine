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
//
// Ticking on the minute boundary (not every 60s from process start) is deliberate: `0 3 *
// * *` should fire at 03:00:00, and a drifting ticker would fire it at whatever offset the
// process happened to start at.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

type schedState struct {
	mu      sync.Mutex
	next    map[string]time.Time // fn key -> next due time
	running map[string]bool      // fn key -> a run is in flight
}

func newSchedState() *schedState {
	return &schedState{next: map[string]time.Time{}, running: map[string]bool{}}
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

// runDue starts every function that is due at `now`.
func (h *Handler) runDue(ctx context.Context, now time.Time) {
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
				h.sched.mu.Unlock()
				continue
			}
			if next.IsZero() || now.Before(next) || h.sched.running[key] {
				h.sched.mu.Unlock()
				continue
			}
			// Claim: advance and mark running in one critical section, so a slow run
			// cannot be started twice.
			h.sched.next[key] = Soonest(fn.Schedules, now)
			h.sched.running[key] = true
			h.sched.mu.Unlock()

			go func(in *control.Instance, fn *Function) {
				defer func() {
					h.sched.mu.Lock()
					delete(h.sched.running, key)
					h.sched.mu.Unlock()
				}()
				h.invokeScheduled(ctx, in, fn, now)
			}(in, fn)
		}
	}
}

// invokeScheduled runs one function through the ordinary invocation path.
//
// Same path as an HTTP call on purpose: a scheduled run differs only in `x-ae-trigger`,
// so anything the runtime does for a request (bindings, egress rules, secrets) it does
// here too, and there is no second implementation to keep in sync.
func (h *Handler) invokeScheduled(_ context.Context, in *control.Instance, fn *Function, now time.Time) {
	cfg := h.store.Config(in.ID)
	src, ok := h.store.Code(in.ID, fn.Name, fn.ActiveVersion)
	if !ok {
		log.Printf("[%s/%s] scheduled run skipped: no code for version %d", in.Name, fn.Name, fn.ActiveVersion)
		return
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
		return
	}
	log.Printf("[%s/%s] scheduled run -> %d", in.Name, fn.Name, status)
}

func quoteList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, strconv.Quote(x))
	}
	return strings.Join(parts, ",")
}
