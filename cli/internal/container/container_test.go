package container

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner stands in for Docker so the lifecycle can be exercised without a daemon. Every
// test below is about a decision made BEFORE the container starts, or about how its ending is
// recorded — none of it needs a real container, and requiring one would mean these bounds went
// untested on any machine without Docker.
type fakeRunner struct {
	mu      sync.Mutex
	started int
	exit    int
	err     error
	block   chan struct{} // when non-nil, Run waits on it (or on ctx)
	avail   bool
}

func (f *fakeRunner) Available() bool { return f.avail }

func (f *fakeRunner) Run(ctx context.Context, spec RunSpec) (int, error) {
	f.mu.Lock()
	f.started++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.exit, f.err
}

func cfgWith(fn func(*Config)) Config {
	c := DefaultConfig()
	c.AllowedImages = []string{"alpine:3"}
	if fn != nil {
		fn(&c)
	}
	return c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestRefusesWithoutDocker(t *testing.T) {
	// Refusing beats pretending. A simulated success is the one behaviour that would make a
	// container emulator useless: the whole point of running one is that something happened.
	s := NewStore(&fakeRunner{avail: false})
	if _, err := s.Launch("i1", cfgWith(nil), LaunchRequest{Image: "alpine:3"}, nil); err == nil {
		t.Fatal("expected a refusal when no runner is available")
	}
}

func TestEmptyAllowlistRunsNothing(t *testing.T) {
	s := NewStore(&fakeRunner{avail: true})
	c := DefaultConfig() // AllowedImages empty
	_, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil)
	if err == nil {
		t.Fatal("an empty allowlist must run nothing")
	}
}

func TestImageMatching(t *testing.T) {
	c := cfgWith(func(c *Config) { c.AllowedImages = []string{"alpine:3", "ghcr.io/me/worker:*"} })
	cases := map[string]bool{
		"alpine:3":                 true,
		"alpine:4":                 false,
		"ghcr.io/me/worker:v2":     true,
		"ghcr.io/me/worker-evil:v": false, // prefix matching must not leak past the repo boundary
		"":                         false,
	}
	for img, want := range cases {
		if got := ImageAllowed(c, img); got != want {
			t.Errorf("ImageAllowed(%q) = %v, want %v", img, got, want)
		}
	}
	if !ImageAllowed(Config{AllowedImages: []string{"*"}}, "anything:latest") {
		t.Error("an explicit * must allow anything")
	}
}

func TestReservedEnvIsRefusedNotDropped(t *testing.T) {
	// Dropping would be quieter and worse: the job runs believing it was given something it
	// was not, and does the wrong thing successfully.
	for _, name := range []string{"AE_JOB_ID", "FLY_API_TOKEN"} {
		if _, err := BuildEnv(map[string]string{name: "x"}, "j1", nil); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, err := BuildEnv(map[string]string{"PATH; rm -rf /": "x"}, "j1", nil); err == nil {
		t.Error("an invalid env var name must be refused")
	}
	env, err := BuildEnv(map[string]string{"FOO": "bar"}, "j7", nil)
	if err != nil || env["FOO"] != "bar" || env["AE_JOB_ID"] != "j7" {
		t.Fatalf("ours must be injected last: %v %v", env, err)
	}
}

// The platform's own variables go in last and are NOT part of the caller's budget: a job must
// not be refused for a credential it did not ask for.
func TestPlatformEnvIsAddedLast(t *testing.T) {
	platform := map[string]string{"AE_BLOB_URL": "http://host.docker.internal:9191/v1/blob/files"}
	env, err := BuildEnv(map[string]string{"BIG": strings.Repeat("x", MaxEnvBytes-10)}, "j1", platform)
	if err != nil {
		t.Fatalf("the platform's variables must not push a job over the caller's cap: %v", err)
	}
	if env["AE_BLOB_URL"] != platform["AE_BLOB_URL"] || env["AE_JOB_ID"] != "j1" {
		t.Fatalf("platform env missing: %v", env)
	}
}

func TestBlobJobEnvOnlyWhenAStoreIsNamed(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:9191/v1/container/jobs", nil)
	if got := BlobJobEnv(Config{}, r); got != nil {
		t.Fatalf("no blobStore means no blob credential at all, got %v", got)
	}
	got := BlobJobEnv(Config{BlobStore: "files"}, r)
	// Loopback is rewritten: inside a container 127.0.0.1 is the container itself.
	if got["AE_BLOB_URL"] != "http://host.docker.internal:9191/v1/blob/files" {
		t.Fatalf("AE_BLOB_URL must be reachable from inside the container: %q", got["AE_BLOB_URL"])
	}
	if got["AE_BLOB_TOKEN"] == "" {
		t.Fatal("AE_BLOB_TOKEN must be set alongside the URL")
	}
}

func TestCostCeilingRefusesBeforeStarting(t *testing.T) {
	// Checked against the WORST case, so a job that might exceed the ceiling never starts —
	// rather than being stopped halfway, which bills for the half.
	f := &fakeRunner{avail: true}
	s := NewStore(f)
	c := cfgWith(func(c *Config) {
		c.MaxTimeoutMS = MaxTimeoutMS
		c.MaxJobCostUSD = 0.01
	})
	if _, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3", Size: "large", TimeoutMS: MaxTimeoutMS}, nil); err == nil {
		t.Fatal("expected a refusal over the per-job ceiling")
	}
	if f.started != 0 {
		t.Fatal("nothing may start when the ceiling refuses it")
	}
	// And the timeout the refusal names must actually fit — across every size and a spread of
	// budgets, because what this guards against is arithmetic: floor the wall-clock instead of
	// the billable seconds and the answer comes back a fraction of a second too long, which
	// then rounds UP into an extra billable second.
	for _, size := range []string{"small", "medium", "large"} {
		for _, budget := range []float64{0.01, 0.05, 0.137, 1, 7.5} {
			fits := MaxTimeoutMSForBudget(size, budget)
			if WorstCaseHoldUSD(size, fits) > budget+1e-9 {
				t.Fatalf("%s @ $%v: the timeout the refusal offers (%dms) does not fit the budget", size, budget, fits)
			}
		}
	}
}

func TestTimeoutCannotExceedTheInstanceMaximum(t *testing.T) {
	s := NewStore(&fakeRunner{avail: true})
	c := cfgWith(func(c *Config) { c.MaxTimeoutMS = 10_000 })
	if _, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3", TimeoutMS: 60_000}, nil); err == nil {
		t.Fatal("a job may ask for less than the instance maximum, never more")
	}
}

func TestConcurrencyCap(t *testing.T) {
	f := &fakeRunner{avail: true, block: make(chan struct{})}
	s := NewStore(f)
	c := cfgWith(func(c *Config) { c.MaxConcurrent = 2 })
	for i := 0; i < 2; i++ {
		if _, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil); err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	// Refused, not queued — the hosted service answers the same way, because a queue would
	// hide an instance that is permanently over its limit.
	if _, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil); err == nil {
		t.Fatal("expected the third launch to be refused")
	}
	close(f.block)
}

func TestRecordsExitCodeAndCost(t *testing.T) {
	f := &fakeRunner{avail: true, exit: 0}
	s := NewStore(f)
	job, err := s.Launch("i1", cfgWith(nil), LaunchRequest{Image: "alpine:3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ := s.Get("i1", job.ID); return j.Status != "running" })
	j, _ := s.Get("i1", job.ID)
	if j.Status != "done" || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("expected a clean finish, got %+v", j)
	}
	if j.CostUSD <= 0 || j.EndedAt == nil {
		t.Fatalf("a finished job must carry a cost and an end time: %+v", j)
	}
}

func TestNonZeroExitIsAFailure(t *testing.T) {
	f := &fakeRunner{avail: true, exit: 137}
	s := NewStore(f)
	job, _ := s.Launch("i1", cfgWith(nil), LaunchRequest{Image: "alpine:3"}, nil)
	waitFor(t, func() bool { j, _ := s.Get("i1", job.ID); return j.Status != "running" })
	j, _ := s.Get("i1", job.ID)
	if j.Status != "failed" || j.ExitCode == nil || *j.ExitCode != 137 {
		t.Fatalf("a non-zero exit is a failed job, not a finished one: %+v", j)
	}
}

func TestCancelStopsAndIsIdempotent(t *testing.T) {
	f := &fakeRunner{avail: true, block: make(chan struct{})}
	s := NewStore(f)
	c := cfgWith(nil)
	job, _ := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil)

	j, canceled := s.Cancel("i1", c, job.ID)
	if !canceled || j.Status != "canceled" {
		t.Fatalf("expected a cancel, got %+v", j)
	}
	cost, ended := j.CostUSD, j.EndedAt

	// Cancelling again does nothing — and, critically, does not re-bill. The runner's own
	// goroutine is also racing to record this ending; whichever arrives first wins.
	j2, canceled2 := s.Cancel("i1", c, job.ID)
	if canceled2 {
		t.Fatal("cancelling a finished job must report false")
	}
	if j2.CostUSD != cost || *j2.EndedAt != *ended {
		t.Fatal("a second cancel must not rewrite or re-bill the job")
	}
	close(f.block)
}

func TestBillingIsCappedAtTheTimeout(t *testing.T) {
	// A job noticed late must not bill for how long it took to notice.
	s := NewStore(&fakeRunner{avail: true})
	c := cfgWith(nil)
	s.jobs["i1"] = []*Job{{
		ID: "j1", Instance: "i1", Status: "running", Image: "alpine:3", Size: "small",
		TimeoutMS: 10_000, StartedAt: time.Now().UnixMilli() - 3_600_000,
		holdUSD: WorstCaseHoldUSD("small", 10_000),
	}}
	s.finish("i1", c, "j1", "failed", nil, "timed out")
	j, _ := s.Get("i1", "j1")
	want := RunCostUSD(RunSecondsFor("small", 10_000)) + PricePer1000Jobs/1000
	if j.CostUSD != want {
		t.Fatalf("billed %v, want %v (elapsed must clamp to the timeout)", j.CostUSD, want)
	}
}

func TestBillingNeverExceedsTheHold(t *testing.T) {
	// The ceiling has to be real, not decorative: whatever measurement says, the bill cannot
	// exceed what was authorised at launch.
	s := NewStore(&fakeRunner{avail: true})
	s.jobs["i1"] = []*Job{{
		ID: "j1", Instance: "i1", Status: "running", Size: "large",
		TimeoutMS: 600_000, StartedAt: time.Now().UnixMilli() - 600_000, holdUSD: 0.002,
	}}
	s.finish("i1", DefaultConfig(), "j1", "done", intp(0), "")
	j, _ := s.Get("i1", "j1")
	if j.CostUSD > 0.002 {
		t.Fatalf("billed %v, above the hold of 0.002", j.CostUSD)
	}
}

func TestRunSecondsScaleWithSize(t *testing.T) {
	if got := RunSecondsFor("small", 10_000); got != 10 {
		t.Fatalf("small: %d", got)
	}
	if got := RunSecondsFor("large", 10_000); got != 120 { // factor 12
		t.Fatalf("large: %d", got)
	}
	if got := RunSecondsFor("small", 1); got != 1 {
		t.Fatalf("a container was still created and destroyed: %d", got)
	}
}

func TestCompletionCallbackFiresAfterTheJobIsRecorded(t *testing.T) {
	f := &fakeRunner{avail: true}
	s := NewStore(f)
	var seen *Job
	var mu sync.Mutex
	s.OnComplete(func(_ string, _ Config, j *Job) {
		mu.Lock()
		defer mu.Unlock()
		seen = j
	})
	c := cfgWith(func(c *Config) { c.OnComplete = "jobs/on-done" })
	job, _ := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return seen != nil })
	mu.Lock()
	defer mu.Unlock()
	if seen.ID != job.ID || seen.Status == "running" {
		t.Fatalf("the callback must see a FINISHED job: %+v", seen)
	}
}

func TestNoCallbackWhenNoneIsConfigured(t *testing.T) {
	s := NewStore(&fakeRunner{avail: true})
	called := false
	s.OnComplete(func(string, Config, *Job) { called = true })
	job, _ := s.Launch("i1", cfgWith(nil), LaunchRequest{Image: "alpine:3"}, nil)
	waitFor(t, func() bool { j, _ := s.Get("i1", job.ID); return j.Status != "running" })
	time.Sleep(20 * time.Millisecond)
	if called {
		t.Fatal("no completion function configured, so nothing may be called")
	}
}

func TestRunnerFailureIsAFailedJobNotAStuckOne(t *testing.T) {
	f := &fakeRunner{avail: true, err: errors.New("no such image")}
	s := NewStore(f)
	job, _ := s.Launch("i1", cfgWith(nil), LaunchRequest{Image: "alpine:3"}, nil)
	waitFor(t, func() bool { j, _ := s.Get("i1", job.ID); return j.Status != "running" })
	j, _ := s.Get("i1", job.ID)
	if j.Status != "failed" || j.Error == "" {
		t.Fatalf("a runner that cannot start must fail the job with a reason: %+v", j)
	}
}

func TestListIsNewestFirstAndFilters(t *testing.T) {
	f := &fakeRunner{avail: true}
	s := NewStore(f)
	c := cfgWith(nil)
	var ids []string
	for i := 0; i < 3; i++ {
		j, err := s.Launch("i1", c, LaunchRequest{Image: "alpine:3"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
		waitFor(t, func() bool { g, _ := s.Get("i1", j.ID); return g.Status != "running" })
		time.Sleep(2 * time.Millisecond) // distinct started_at for the cursor
	}
	jobs, _ := s.List("i1", "", 10, 0)
	if len(jobs) != 3 || jobs[0].ID != ids[2] {
		t.Fatalf("expected newest first, got %d jobs starting %s", len(jobs), jobs[0].ID)
	}
	if got, _ := s.List("i1", "running", 10, 0); len(got) != 0 {
		t.Fatalf("status filter returned %d running jobs", len(got))
	}
}

func TestParseConfigClampsRatherThanTrusts(t *testing.T) {
	c := ParseConfig(map[string]any{
		"maxTimeoutMs":  float64(MaxTimeoutMS * 10),
		"maxConcurrent": float64(9999),
		"allowedImages": []any{"alpine:3", 7},
	})
	if c.MaxTimeoutMS != MaxTimeoutMS || c.MaxConcurrent != MaxConcurrency {
		t.Fatalf("out-of-range stored values must clamp: %+v", c)
	}
	if len(c.AllowedImages) != 1 {
		t.Fatalf("a non-string image entry must be dropped: %+v", c.AllowedImages)
	}
}

func intp(v int) *int { return &v }

// A job's image is caller-supplied, and an instance whose allowlist is "*" accepts anything.
// Docker reads an argument beginning with "-" as a FLAG and keeps taking flags until the first
// positional argument — so an image of "--privileged" with a cmd of
// ["-v","/:/host","alpine","chroot","/host","sh","-c",...] was a root shell on the developer's
// own machine, from a job request.
func TestImageRefCannotBeADockerFlag(t *testing.T) {
	bad := []string{
		"--privileged",
		"-v",
		"--rm",
		" --privileged",
		"alpine:3 --privileged",
		"alpine;rm -rf /",
		"",
	}
	for _, img := range bad {
		if ValidImageRef(img) {
			t.Errorf("ValidImageRef(%q) = true, want false", img)
		}
	}
	good := []string{
		"alpine",
		"alpine:3",
		"ghcr.io/me/worker:v2",
		"registry.example.com/team/app:1.2.3",
		"alpine@sha256:" + strings.Repeat("a", 64),
	}
	for _, img := range good {
		if !ValidImageRef(img) {
			t.Errorf("ValidImageRef(%q) = false, want true", img)
		}
	}
}

// The allowlist is not the only gate: "*" means the caller chooses the image, so Launch has to
// refuse an argument-shaped one on its own.
func TestLaunchRefusesAFlagShapedImageEvenWithWildcardAllowlist(t *testing.T) {
	s := NewStore(&fakeRunner{avail: true})
	c := cfgWith(func(c *Config) { c.AllowedImages = []string{"*"} })
	if _, err := s.Launch("i1", c, LaunchRequest{Image: "--privileged"}, nil); err == nil {
		t.Fatal("a flag-shaped image must be refused even when every image is allowed")
	}
}
