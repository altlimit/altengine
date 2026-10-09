package functions

// The local scheduler's claim logic, driven directly rather than through the clock.
//
// runDue is called with explicit times so the behaviour is deterministic: waiting on a
// real minute boundary would make this a 60-second test that still could not prove the
// overlap guard.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
)

func schedHandler(t *testing.T) (*Handler, *control.Instance) {
	t.Helper()
	reg, err := control.New("")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h := NewHandler(reg, auth.NewStore(true), memStore(), mux)
	h.Register(mux)
	in := reg.GetOrCreate("functions", "main")
	return h, in
}

// deploySched puts a function with the given schedules straight into the store.
func deploySched(t *testing.T, h *Handler, in *control.Instance, code string, schedules ...string) {
	t.Helper()
	raw := []byte(`[]`)
	if len(schedules) > 0 {
		raw = []byte(`["` + schedules[0] + `"]`)
		if len(schedules) > 1 {
			raw = []byte(`["` + schedules[0] + `","` + schedules[1] + `"]`)
		}
	}
	if _, err := h.store.Deploy(in.ID, DeployRequest{Name: "job", Code: code, Schedules: raw}, DefaultKeepVersions); err != nil {
		t.Fatal(err)
	}
}

// settle waits for in-flight scheduled runs to finish.
func settle(t *testing.T, h *Handler) {
	t.Helper()
	for i := 0; i < 200; i++ {
		h.sched.mu.Lock()
		n := len(h.sched.running)
		h.sched.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("scheduled run never finished")
}

const counterFn = `let runs = 0;
export default { async fetch() { runs++; return new Response(String(runs)); } };`

func TestSchedulerDoesNotRunOnFirstSighting(t *testing.T) {
	// Starting the emulator must not fire every schedule immediately — a job runs because
	// its expression came due while we were watching, not because the process restarted.
	h, in := schedHandler(t)
	deploySched(t, h, in, counterFn, "* * * * *")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.runDue(t.Context(), now)
	settle(t, h)

	h.sched.mu.Lock()
	next, seen := h.sched.next[fnKey(in.ID, "job")]
	h.sched.mu.Unlock()
	if !seen {
		t.Fatal("first sighting should record a next-run")
	}
	if !next.After(now) {
		t.Fatalf("next run %s should be after %s", next, now)
	}
}

func TestSchedulerAdvancesWhenDue(t *testing.T) {
	h, in := schedHandler(t)
	deploySched(t, h, in, counterFn, "* * * * *")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.runDue(t.Context(), t0) // seeds next = t0+1m
	settle(t, h)

	h.runDue(t.Context(), t0.Add(time.Minute))
	settle(t, h)

	h.sched.mu.Lock()
	next := h.sched.next[fnKey(in.ID, "job")]
	h.sched.mu.Unlock()
	if !next.After(t0.Add(time.Minute)) {
		t.Fatalf("a due run should advance next-run past now, got %s", next)
	}
}

func TestSchedulerDoesNotOverlapItself(t *testing.T) {
	// The guard that matters: an every-minute job taking longer than a minute must not
	// accumulate concurrent copies of itself.
	h, in := schedHandler(t)
	deploySched(t, h, in, counterFn, "* * * * *")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.runDue(t.Context(), t0)
	settle(t, h)

	key := fnKey(in.ID, "job")
	// Pin the row as "running", as an in-flight invocation would.
	h.sched.mu.Lock()
	h.sched.running[key] = true
	before := h.sched.next[key]
	h.sched.mu.Unlock()

	h.runDue(t.Context(), t0.Add(10*time.Minute))

	h.sched.mu.Lock()
	after := h.sched.next[key]
	stillRunning := h.sched.running[key]
	h.sched.mu.Unlock()
	if !after.Equal(before) {
		t.Errorf("a held row must not be claimed: next moved %s -> %s", before, after)
	}
	if !stillRunning {
		t.Error("the lease should still be held")
	}

	// Once it finishes, the next tick may run it again.
	h.sched.mu.Lock()
	delete(h.sched.running, key)
	h.sched.mu.Unlock()
	h.runDue(t.Context(), t0.Add(11*time.Minute))
	settle(t, h)
	h.sched.mu.Lock()
	after = h.sched.next[key]
	h.sched.mu.Unlock()
	if !after.After(before) {
		t.Errorf("a released row should be claimed on the next tick, next stayed %s", after)
	}
}

func TestSchedulerIgnoresUnscheduledFunctions(t *testing.T) {
	h, in := schedHandler(t)
	deploySched(t, h, in, counterFn) // no schedules
	h.runDue(t.Context(), time.Now().UTC())
	h.sched.mu.Lock()
	n := len(h.sched.next)
	h.sched.mu.Unlock()
	if n != 0 {
		t.Errorf("an unscheduled function should not enter the due-set, got %d", n)
	}
}

// A scheduled run that outlives its timeout is no longer waited on, but it KEEPS its lease
// until it actually ends — so the next occurrence cannot start a second copy beside it — and
// then records its real status and releases.
func TestSchedulerRunTimeout(t *testing.T) {
	h, in := schedHandler(t)
	deploySched(t, h, in, `export default { async fetch() {
		const end = Date.now() + 300; while (Date.now() < end) {}
		return new Response("late");
	} };`, "* * * * *")
	h.sched.mu.Lock()
	h.sched.runTimeout = 20 * time.Millisecond
	h.sched.mu.Unlock()
	key := fnKey(in.ID, "job")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.runDue(t.Context(), t0)
	h.runDue(t.Context(), t0.Add(time.Minute))
	time.Sleep(150 * time.Millisecond) // well past the timeout, well short of the run
	h.sched.mu.Lock()
	status, running, next := h.sched.lastStatus[key], h.sched.running[key], h.sched.next[key]
	h.sched.mu.Unlock()
	if status != "" || !running {
		t.Fatalf("past the timeout: lastStatus = %q, running = %v; want no status and the lease held", status, running)
	}

	// The next occurrence comes due while it is still going: not claimed.
	h.runDue(t.Context(), t0.Add(2*time.Minute))
	h.sched.mu.Lock()
	after := h.sched.next[key]
	h.sched.mu.Unlock()
	if !after.Equal(next) {
		t.Fatalf("a run past its timeout let the next occurrence claim the row: next %s -> %s", next, after)
	}

	settle(t, h)
	h.sched.mu.Lock()
	status, running = h.sched.lastStatus[key], h.sched.running[key]
	h.sched.mu.Unlock()
	if status != "http:200" || running {
		t.Fatalf("after it ends: lastStatus = %q, running = %v; want its real status and the lease released", status, running)
	}
}

// A claim re-checks that the row is still due and its lease free: a row read as due, then
// claimed and finished by someone else before this claim, is not run again.
func TestSchedulerClaimIsConditional(t *testing.T) {
	s := newSchedState()
	sched := []string{"* * * * *"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.next["k"] = t0
	if _, ok := s.claim("k", sched, t0); !ok {
		t.Fatal("a due, free row was not claimed")
	}
	if _, ok := s.claim("k", sched, t0); ok {
		t.Fatal("a leased row was claimed twice")
	}
	// The first run finishes and releases; a claim from the same stale due-set must not
	// start the same occurrence again.
	delete(s.running, "k")
	if _, ok := s.claim("k", sched, t0); ok {
		t.Fatal("a row no longer due was claimed")
	}
	if _, ok := s.claim("k", sched, t0.Add(time.Minute)); !ok {
		t.Fatal("the next occurrence was not claimed")
	}
}

// One tick starts at most 25 of an organization's due jobs, most overdue first; the rest
// stay due and unclaimed for the next tick instead of being advanced unrun.
func TestSchedulerPerTickCap(t *testing.T) {
	h, in := schedHandler(t)
	const n = maxRunsPerOrgPerTick + 5
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("job%02d", i)
		if _, err := h.store.Deploy(in.ID, DeployRequest{Name: name, Code: counterFn, Schedules: []byte(`["* * * * *"]`)}, DefaultKeepVersions); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.runDue(t.Context(), t0) // first sighting seeds every next-run
	// Make five of them the most overdue: they must be among those started.
	h.sched.mu.Lock()
	for i := 0; i < 5; i++ {
		h.sched.next[fnKey(in.ID, fmt.Sprintf("job%02d", n-1-i))] = t0.Add(-time.Hour)
	}
	h.sched.mu.Unlock()

	tick := t0.Add(time.Minute)
	h.runDue(t.Context(), tick)
	settle(t, h)

	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	advanced := 0
	for i := 0; i < n; i++ {
		key := fnKey(in.ID, fmt.Sprintf("job%02d", i))
		if h.sched.next[key].After(tick) {
			advanced++
			if h.sched.lastStatus[key] != "http:200" {
				t.Errorf("%s lastStatus = %q", key, h.sched.lastStatus[key])
			}
		} else if h.sched.lastStatus[key] != "" {
			t.Errorf("%s was not advanced but has a status %q", key, h.sched.lastStatus[key])
		}
	}
	if advanced != maxRunsPerOrgPerTick {
		t.Fatalf("started %d jobs in one tick, want %d", advanced, maxRunsPerOrgPerTick)
	}
	for i := 0; i < 5; i++ {
		if key := fnKey(in.ID, fmt.Sprintf("job%02d", n-1-i)); !h.sched.next[key].After(tick) {
			t.Errorf("most overdue %s was passed over", key)
		}
	}
}
