package functions

// The local scheduler's claim logic, driven directly rather than through the clock.
//
// runDue is called with explicit times so the behaviour is deterministic: waiting on a
// real minute boundary would make this a 60-second test that still could not prove the
// overlap guard.

import (
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
	h := NewHandler(reg, auth.NewStore(true), NewStore(""), mux)
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
