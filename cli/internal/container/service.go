// Package container emulates the altengine container service: a Docker image run as a
// background job for as long as the work takes.
//
// # WHAT IS FAITHFUL, AND WHAT IS NOT
//
// Faithful: the API — start a job and get its id back immediately, poll it, list, cancel —
// and every bound the hosted service applies before the job starts. The allowed-image list
// (empty means nothing runs), the per-instance timeout, the concurrency cap, the per-job cost
// ceiling checked against the worst case, the reserved AE_/environment-name refusals, and the
// completion callback into a function. A request refused here is refused there, for the same
// reason and with the same message.
//
// NOT faithful, deliberately:
//
//   - Jobs run on YOUR Docker daemon, in whatever isolation you have configured it with. The
//     hosted service runs each job as a single-use machine with no inbound network, no
//     persistent disk and no route back into the platform. Locally a container can reach your
//     network and your daemon can do whatever you have allowed it to. Run images you trust.
//   - Without a working `docker`, the service refuses with 503 rather than pretending a job
//     ran. A simulated success is the one behaviour that would make this useless: the whole
//     point of running a container is that something happened.
//   - There is no size difference. `small`, `medium` and `large` are recorded and billed
//     exactly as hosted, so cost estimates match, but the local container gets whatever your
//     daemon gives it — a job that fits in 512 MB here may not fit hosted.
//   - AE_BLOB_TOKEN bounds nothing. Naming a `blobStore` injects the same two variables a job
//     gets hosted, so an image is written once and runs in both places, but this data plane is
//     dev-open: hosted the token is scoped to one store and refuses delete and publish, and
//     here any bearer works. See blobjob.go.
package container

import (
	"context"
	"fmt"
	"math"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Sizes offered, matching the hosted service. `Factor` scales billed seconds; it is the whole
// reason one rate can cover every size.
type Size struct {
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memory_mb"`
	Factor   int `json:"-"`
}

var Sizes = map[string]Size{
	"small":  {CPUs: 1, MemoryMB: 512, Factor: 1},
	"medium": {CPUs: 2, MemoryMB: 2048, Factor: 4},
	"large":  {CPUs: 4, MemoryMB: 8192, Factor: 12},
}

const (
	DefaultSize      = "small"
	MaxTimeoutMS     = int64(60 * 60 * 1000)
	DefaultTimeoutMS = int64(5 * 60 * 1000)
	MaxConcurrency   = 50

	// Rates, matching the hosted pricing rows. Duplicated here for the same reason the hosted
	// service duplicates them in code: the per-job ceiling has to be computed before the job
	// starts, and a local estimate that disagreed with the real bill would be worse than none.
	PricePer1000RunSeconds = 0.02
	PricePer1000Jobs       = 1.00

	MaxEnvVars    = 32
	MaxEnvBytes   = 8192
	MaxCmdArgs    = 64
	MaxCmdArgLen  = 4096
	MaxListLimit  = 100
	DefaultLimit  = 25
	dockerTimeout = 10 * time.Second
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvPrefixes are refused rather than dropped: a silently ignored variable is a job
// that runs and does the wrong thing successfully.
var reservedEnvPrefixes = []string{"AE_", "FLY_"}

// Config is a container instance's settings.
type Config struct {
	AllowedImages []string `json:"allowedImages"`
	MaxTimeoutMS  int64    `json:"maxTimeoutMs"`
	MaxConcurrent int      `json:"maxConcurrent"`
	MaxJobCostUSD float64  `json:"maxJobCostUsd"`
	OnComplete    string   `json:"onComplete,omitempty"`
	// BlobStore is the blob instance a job launched here may reach, by name. Empty means a job
	// gets no blob credential at all — which is what an instance that has not set it does.
	BlobStore string `json:"blobStore,omitempty"`
}

// DefaultConfig is what a new instance starts with. AllowedImages is EMPTY, and that is the
// point: an allowlist defaulting to "everything" is not an allowlist, and this is the one
// service whose literal description is "run code someone else wrote".
func DefaultConfig() Config {
	return Config{
		AllowedImages: []string{},
		MaxTimeoutMS:  DefaultTimeoutMS,
		MaxConcurrent: 2,
		MaxJobCostUSD: 1,
	}
}

// ParseConfig reads an instance's stored config blob, clamping anything out of range rather
// than trusting it — an out-of-range stored value is a bug somewhere else, and honouring it
// would spend money.
func ParseConfig(raw map[string]any) Config {
	c := DefaultConfig()
	if raw == nil {
		return c
	}
	if v, ok := raw["allowedImages"].([]any); ok {
		// An EMPTY SLICE, never nil: this list is served as `allowed_images`, and a nil slice
		// marshals to `null` where hosted always sends an array. A client that has to read the
		// two differently is a client that behaves differently locally.
		c.AllowedImages = []string{}
		for _, i := range v {
			if s, ok := i.(string); ok && strings.TrimSpace(s) != "" {
				c.AllowedImages = append(c.AllowedImages, strings.TrimSpace(s))
			}
		}
	}
	if v, ok := numOf(raw["maxTimeoutMs"]); ok {
		c.MaxTimeoutMS = int64(math.Min(math.Max(1000, v), float64(MaxTimeoutMS)))
	}
	if v, ok := numOf(raw["maxConcurrent"]); ok {
		c.MaxConcurrent = int(math.Min(math.Max(1, v), MaxConcurrency))
	}
	if v, ok := numOf(raw["maxJobCostUsd"]); ok {
		c.MaxJobCostUSD = math.Max(0.01, v)
	}
	if v, ok := raw["onComplete"].(string); ok {
		c.OnComplete = strings.TrimSpace(v)
	}
	if v, ok := raw["blobStore"].(string); ok {
		c.BlobStore = strings.TrimSpace(v)
	}
	return c
}

func numOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// ImageAllowed reports whether this instance may run the image. An exact tag matches only
// itself; a trailing ":*" allows any tag of one repository — and must not leak past the
// repository boundary, so "ghcr.io/me/worker:*" does not admit "ghcr.io/me/worker-evil:v2".
func ImageAllowed(c Config, image string) bool {
	img := strings.TrimSpace(image)
	if img == "" {
		return false
	}
	for _, pattern := range c.AllowedImages {
		p := strings.TrimSpace(pattern)
		switch {
		case p == "":
		case p == "*":
			return true
		case strings.HasSuffix(p, ":*"):
			if strings.HasPrefix(img, strings.TrimSuffix(p, "*")) {
				return true
			}
		case img == p:
			return true
		}
	}
	return false
}

// RunSecondsFor is billable seconds: wall-clock scaled by size, rounded up — a job that ran
// for a tenth of a second still occupied a container that had to be created and destroyed.
func RunSecondsFor(size string, elapsedMS int64) int64 {
	f := Sizes[size].Factor
	if f == 0 {
		f = 1
	}
	if elapsedMS < 0 {
		elapsedMS = 0
	}
	return int64(math.Ceil(float64(elapsedMS) / 1000 * float64(f)))
}

func RunCostUSD(runSeconds int64) float64 {
	return float64(runSeconds) / 1000 * PricePer1000RunSeconds
}

// WorstCaseHoldUSD is the most a job could possibly cost, from its timeout. The per-job
// ceiling is checked against THIS, so a job that might exceed it never starts.
func WorstCaseHoldUSD(size string, timeoutMS int64) float64 {
	return RunCostUSD(RunSecondsFor(size, timeoutMS)) + PricePer1000Jobs/1000
}

// MaxTimeoutMSForBudget is the longest timeout whose worst case fits a budget. Used to say
// what WOULD have worked, rather than clamping silently to it.
//
// Round DOWN to whole billable seconds BEFORE converting back to a wall-clock timeout.
// Flooring after the division instead lets the answer come back a fraction of a second too
// long, which WorstCaseHoldUSD then rounds up into an extra billable second — so the refusal
// names a timeout that is itself refused. An error telling you to do something that also fails
// is worse than no suggestion at all.
func MaxTimeoutMSForBudget(size string, budgetUSD float64) int64 {
	budget := budgetUSD - PricePer1000Jobs/1000
	if budget <= 0 {
		return 0
	}
	f := Sizes[size].Factor
	if f == 0 {
		f = 1
	}
	maxRunSeconds := math.Floor(budget / PricePer1000RunSeconds * 1000)
	return int64(math.Floor(maxRunSeconds / float64(f) * 1000))
}

// Job is one container run.
type Job struct {
	ID        string  `json:"id"`
	Instance  string  `json:"-"`
	Status    string  `json:"status"` // running | done | failed | canceled
	Image     string  `json:"image"`
	Size      string  `json:"size"`
	ExitCode  *int    `json:"exit_code"`
	TimeoutMS int64   `json:"timeout_ms"`
	Error     string  `json:"error,omitempty"`
	StartedAt int64   `json:"started_at"`
	EndedAt   *int64  `json:"ended_at"`
	CostUSD   float64 `json:"cost_usd"`

	holdUSD float64
	cancel  context.CancelFunc
	logs    *logBuffer
}

func (j *Job) clone() *Job {
	c := *j
	c.cancel = nil
	// The buffer has its own lock and is shared by reference deliberately: copying it would
	// copy a mutex, and a clone handed to a caller is read-only anyway.
	return &c
}

// Runner starts a container and reports how it ended. Swapped out in tests, which is the only
// way to exercise the lifecycle without a Docker daemon.
type Runner interface {
	// Available reports whether jobs can run at all. False => the service refuses honestly.
	Available() bool
	// Run blocks until the container exits, ctx is cancelled, or it fails to start.
	Run(ctx context.Context, spec RunSpec) (exitCode int, err error)
}

// RunSpec is what a runner needs.
type RunSpec struct {
	JobID    string
	Image    string
	Cmd      []string
	Env      map[string]string
	MemoryMB int
	CPUs     int
	// Where the container's output goes. Nil discards it — which is what os/exec does by
	// default, and what this did for every job until logs existed.
	Stdout io.Writer
	Stderr io.Writer
}

// DockerRunner runs jobs on the local Docker daemon.
type DockerRunner struct {
	once sync.Once
	ok   bool
}

func (d *DockerRunner) Available() bool {
	d.once.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
		defer cancel()
		d.ok = exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run() == nil
	})
	return d.ok
}

func (d *DockerRunner) Run(ctx context.Context, spec RunSpec) (int, error) {
	args := []string{
		"run", "--rm",
		"--name", "altengine-" + spec.JobID,
		// The hosted machine has no inbound network and no persistent disk. `--network none`
		// is stricter than hosted (which permits OUTBOUND traffic), so it is deliberately not
		// set — a job that fetches its input would work hosted and fail here, which is the
		// wrong way round for an emulator to differ.
		"--memory", fmt.Sprintf("%dm", spec.MemoryMB),
		"--cpus", fmt.Sprintf("%d", spec.CPUs),
	}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return ee.ExitCode(), nil
		}
		return 0, err
	case <-ctx.Done():
		// Kill the CONTAINER, not just the client process: `docker run` exiting leaves the
		// container running, which is precisely the leak the hosted reaper exists to prevent.
		kctx, kcancel := context.WithTimeout(context.Background(), dockerTimeout)
		defer kcancel()
		_ = exec.CommandContext(kctx, "docker", "kill", "altengine-"+spec.JobID).Run()
		<-done
		return 0, ctx.Err()
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Store holds every instance's jobs and runs them.
type Store struct {
	mu     sync.Mutex
	jobs   map[string][]*Job // instance id -> newest last
	runner Runner
	// onComplete is called with the finished job once it is recorded. The HTTP layer uses it
	// to invoke a function, exactly as hosted does — after the result is durable, so a failing
	// callback cannot lose the result or re-run the job.
	onComplete func(instanceID string, cfg Config, job *Job)
}

func NewStore(runner Runner) *Store {
	if runner == nil {
		runner = &DockerRunner{}
	}
	return &Store{jobs: map[string][]*Job{}, runner: runner}
}

func (s *Store) OnComplete(fn func(instanceID string, cfg Config, job *Job)) { s.onComplete = fn }

// Available reports whether the runner can run anything.
func (s *Store) Available() bool { return s.runner.Available() }

// LaunchRequest is a start-a-job call.
type LaunchRequest struct {
	Image     string            `json:"image"`
	Size      string            `json:"size"`
	Cmd       []string          `json:"cmd"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
}

// BuildEnv validates the caller's variables and appends ours last, so a caller cannot shadow
// them even if the prefix check ever loosens. `platform` is whatever else the platform injects
// beyond the job id — today the blob store's URL and token, when the instance names one
// (blobjob.go). It is added after the caller's budget is checked: a platform credential must not
// be the thing that pushes a job over a limit the caller was told about.
func BuildEnv(in map[string]string, jobID string, platform map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if len(in) > MaxEnvVars {
		return nil, common.BadRequest(fmt.Sprintf("at most %d env vars", MaxEnvVars))
	}
	bytes := 0
	for k, v := range in {
		if !envNameRe.MatchString(k) {
			return nil, common.BadRequest(fmt.Sprintf("invalid env var name '%s'", trunc(k, 40)))
		}
		for _, p := range reservedEnvPrefixes {
			if strings.HasPrefix(k, p) {
				return nil, common.BadRequest(fmt.Sprintf("env var '%s' uses a reserved prefix", k))
			}
		}
		bytes += len(k) + len(v)
		out[k] = v
	}
	if bytes > MaxEnvBytes {
		return nil, common.BadRequest(fmt.Sprintf("env must be under %d bytes", MaxEnvBytes))
	}
	for k, v := range platform {
		out[k] = v
	}
	out["AE_JOB_ID"] = jobID
	return out, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Launch validates everything, starts the container, and returns immediately. `platform` is the
// environment the platform adds to this job — see BlobJobEnv.
func (s *Store) Launch(instanceID string, cfg Config, req LaunchRequest, platform map[string]string) (*Job, error) {
	if !s.runner.Available() {
		return nil, common.NewError(503,
			"container jobs need a running Docker daemon — start Docker and try again", "UNAVAILABLE")
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		return nil, common.BadRequest("image is required")
	}
	if !ImageAllowed(cfg, image) {
		if len(cfg.AllowedImages) == 0 {
			return nil, common.PermissionDenied(
				"this instance has no allowed images — add one in its settings before launching")
		}
		return nil, common.PermissionDenied(fmt.Sprintf("image '%s' is not in this instance's allowed images", image))
	}
	size := req.Size
	if size == "" {
		size = DefaultSize
	}
	if _, ok := Sizes[size]; !ok {
		return nil, common.BadRequest("size must be one of small, medium, large")
	}
	timeout := req.TimeoutMS
	if timeout == 0 {
		timeout = cfg.MaxTimeoutMS
	}
	if timeout < 1000 {
		return nil, common.BadRequest("timeout_ms must be an integer >= 1000")
	}
	if timeout > cfg.MaxTimeoutMS {
		return nil, common.BadRequest(fmt.Sprintf(
			"timeout_ms must be <= %d (this instance's maximum)", cfg.MaxTimeoutMS))
	}
	if len(req.Cmd) > MaxCmdArgs {
		return nil, common.BadRequest(fmt.Sprintf("cmd may have at most %d arguments", MaxCmdArgs))
	}
	for _, a := range req.Cmd {
		if len(a) > MaxCmdArgLen {
			return nil, common.BadRequest("cmd argument too long")
		}
	}
	hold := WorstCaseHoldUSD(size, timeout)
	// The epsilon is float noise, not slack: 0.009 + 0.001 is 0.010000000000000002 in binary
	// floating point, so an exact-fit job would otherwise be refused for exceeding its budget
	// by a quintillionth of a cent.
	if hold > cfg.MaxJobCostUSD+1e-9 {
		fits := MaxTimeoutMSForBudget(size, cfg.MaxJobCostUSD)
		return nil, common.BadRequest(fmt.Sprintf(
			"a %s job running for %ds could cost up to $%.4f, above this instance's maxJobCostUsd ($%g). "+
				"Lower timeout_ms to %ds, use a smaller size, or raise the cap.",
			size, timeout/1000, hold, cfg.MaxJobCostUSD, fits/1000))
	}

	id := "j" + strings.ReplaceAll(common.UUID(), "-", "")
	env, err := BuildEnv(req.Env, id, platform)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	running := 0
	for _, j := range s.jobs[instanceID] {
		if j.Status == "running" {
			running++
		}
	}
	if running >= cfg.MaxConcurrent {
		s.mu.Unlock()
		return nil, common.NewError(429, fmt.Sprintf(
			"this instance already has %d jobs running (maxConcurrent %d)", running, cfg.MaxConcurrent),
			"RESOURCE_EXHAUSTED")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	job := &Job{
		ID:        id,
		Instance:  instanceID,
		Status:    "running",
		Image:     image,
		Size:      size,
		TimeoutMS: timeout,
		StartedAt: time.Now().UnixMilli(),
		holdUSD:   hold,
		cancel:    cancel,
		logs:      newLogBuffer(),
	}
	s.jobs[instanceID] = append(s.jobs[instanceID], job)
	s.mu.Unlock()

	sz := Sizes[size]
	go s.run(ctx, cancel, instanceID, cfg, job, RunSpec{
		JobID: id, Image: image, Cmd: req.Cmd, Env: env, MemoryMB: sz.MemoryMB, CPUs: sz.CPUs,
		Stdout: job.logs.writer("stdout"), Stderr: job.logs.writer("stderr"),
	})
	return job.clone(), nil
}

func (s *Store) run(ctx context.Context, cancel context.CancelFunc, instanceID string, cfg Config, job *Job, spec RunSpec) {
	defer cancel()
	code, err := s.runner.Run(ctx, spec)

	status, exit, msg := "done", &code, ""
	switch {
	case err != nil && ctx.Err() == context.DeadlineExceeded:
		status, exit, msg = "failed", nil, "timed out"
	case err != nil && ctx.Err() == context.Canceled:
		status, exit, msg = "canceled", nil, "canceled"
	case err != nil:
		status, exit, msg = "failed", nil, trunc(err.Error(), 200)
	case code != 0:
		status = "failed"
	}
	s.finish(instanceID, cfg, job.ID, status, exit, msg)
}

// finish records the ending exactly once. The status check is what makes a cancel racing a
// natural exit safe — the same guard the hosted service uses, for the same reason.
func (s *Store) finish(instanceID string, cfg Config, jobID, status string, exit *int, msg string) {
	s.mu.Lock()
	var job *Job
	for _, j := range s.jobs[instanceID] {
		if j.ID == jobID {
			job = j
			break
		}
	}
	if job == nil || job.Status != "running" {
		s.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	// Elapsed is clamped to the timeout the job was AUTHORISED for, so a job noticed late is
	// not billed for how long it took to notice.
	elapsed := now - job.StartedAt
	if elapsed > job.TimeoutMS {
		elapsed = job.TimeoutMS
	}
	runSeconds := RunSecondsFor(job.Size, elapsed)
	cost := RunCostUSD(runSeconds) + PricePer1000Jobs/1000
	if cost > job.holdUSD {
		cost = job.holdUSD
	}
	job.Status = status
	job.ExitCode = exit
	job.Error = msg
	job.EndedAt = &now
	job.CostUSD = cost
	snapshot := job.clone()
	s.mu.Unlock()

	if s.onComplete != nil && cfg.OnComplete != "" {
		s.onComplete(instanceID, cfg, snapshot)
	}
}

// Cancel stops a running job. Idempotent: cancelling a finished job does nothing.
func (s *Store) Cancel(instanceID string, cfg Config, jobID string) (*Job, bool) {
	s.mu.Lock()
	var job *Job
	for _, j := range s.jobs[instanceID] {
		if j.ID == jobID {
			job = j
			break
		}
	}
	if job == nil {
		s.mu.Unlock()
		return nil, false
	}
	if job.Status != "running" {
		out := job.clone()
		s.mu.Unlock()
		return out, false
	}
	cancel := job.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// The runner's goroutine records the ending, but a runner that never started leaves the
	// job running forever — so record it here too. finish() is idempotent, so whichever gets
	// there first wins and the other is a no-op.
	s.finish(instanceID, cfg, jobID, "canceled", nil, "canceled")
	j, _ := s.Get(instanceID, jobID)
	return j, true
}

// Get returns one job.
func (s *Store) Get(instanceID, jobID string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs[instanceID] {
		if j.ID == jobID {
			return j.clone(), true
		}
	}
	return nil, false
}

// List returns jobs newest-first, optionally filtered by status, with a keyset cursor.
func (s *Store) List(instanceID, status string, limit int, before int64) ([]*Job, *int64) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.jobs[instanceID]
	out := []*Job{}
	for i := len(all) - 1; i >= 0; i-- {
		j := all[i]
		if status != "" && j.Status != status {
			continue
		}
		if before > 0 && j.StartedAt >= before {
			continue
		}
		out = append(out, j.clone())
		if len(out) > limit {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
		c := out[len(out)-1].StartedAt
		return out, &c
	}
	return out, nil
}

// RunningCount is what the console shows and what the concurrency cap is measured against.
// Logs returns one page of a job's captured output, oldest first.
//
// Best effort, exactly as hosted: a job we no longer hold (the emulator was restarted, or the
// job predates log capture) gets an empty page with a note rather than an error. The job's own
// record never depends on this.
func (s *Store) Logs(instanceID, jobID string, from int) (LogPage, bool) {
	s.mu.Lock()
	var job *Job
	for _, j := range s.jobs[instanceID] {
		if j.ID == jobID {
			job = j
			break
		}
	}
	s.mu.Unlock()
	if job == nil {
		return LogPage{}, false
	}
	if job.logs == nil {
		return LogPage{Note: "no output was captured for this job"}, true
	}
	return job.logs.page(from), true
}

func (s *Store) RunningCount(instanceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.jobs[instanceID] {
		if j.Status == "running" {
			n++
		}
	}
	return n
}

// Drop forgets an instance's jobs, cancelling anything still running — the local equivalent of
// deleting an instance, where the hosted service destroys the machines first.
func (s *Store) Drop(instanceID string) {
	s.mu.Lock()
	jobs := s.jobs[instanceID]
	delete(s.jobs, instanceID)
	s.mu.Unlock()
	for _, j := range jobs {
		if j.Status == "running" && j.cancel != nil {
			j.cancel()
		}
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
