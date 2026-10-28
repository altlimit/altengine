package tasks

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// THE ENGINE. A background loop ticks about once a second, and an add wakes it at once, so a task
// with no delay runs within a second of being added — as hosted. Each pass, per instance: drop
// finished records past their retention, recover tasks whose lease passed (a pull worker that
// never answered, or a delivery a restart interrupted — retried like a timeout), then claim what
// is due within the limits (per-queue concurrency, a per-queue token bucket, the instance's
// MaxInflight) and deliver each in its own goroutine. A finished delivery wakes the loop again.
//
// AT-LEAST-ONCE, as hosted: a consumer must be idempotent on `task.id`.

// Service is the tasks service: the store, the engine and delivery.
type Service struct {
	reg    *control.Registry
	store  *Store
	mux    http.Handler
	hub    *channel.Hub
	client *http.Client

	// waitCap, when set, bounds how long any one delivery is waited for — so a test can see a
	// deadline pass without waiting the 15-second minimum. Zero in the emulator.
	waitCap time.Duration

	mu       sync.Mutex
	inflight map[string]map[string]bool // instance id -> task id
	running  map[string]map[string]int  // instance id -> queue -> deliveries in flight
	wg       sync.WaitGroup
	wake     chan struct{}

	// now is the clock, swappable in tests.
	now func() int64
}

// NewService builds the service. mux is the emulator's router, used to call functions in process;
// hub is the channel hub a channel target publishes into.
func NewService(reg *control.Registry, store *Store, mux http.Handler, hub *channel.Hub) *Service {
	return &Service{
		reg: reg, store: store, mux: mux, hub: hub,
		// No client timeout: each delivery waits until its own deadline.
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		inflight: map[string]map[string]bool{},
		running:  map[string]map[string]int{},
		wake:     make(chan struct{}, 1),
		now:      func() int64 { return time.Now().UnixMilli() },
	}
}

// Store is the underlying store.
func (s *Service) Store() *Store { return s.store }

// SetHTTPClient replaces the client webhooks are delivered with (tests).
func (s *Service) SetHTTPClient(c *http.Client) {
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.client = c
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() int64) { s.now = now }

// Drop is the instance-delete hook.
func (s *Service) Drop(instanceID string) { s.store.Drop(instanceID) }

// Wait blocks until the deliveries started so far have finished (tests).
func (s *Service) Wait() { s.wg.Wait() }

// Kick wakes the loop: work may be due now.
func (s *Service) Kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// retentionDays is how long the instance keeps a finished task's record.
func retentionDays(in *control.Instance) int {
	if in == nil {
		return DefaultRetentionDays
	}
	if n, ok := common.Int(in.Config()["retentionDays"]); ok && n >= 1 && n <= MaxRetentionDays {
		return int(n)
	}
	return DefaultRetentionDays
}

// Run drives the engine until ctx is done.
func (s *Service) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.wake:
			}
			s.Tick()
		}
	}()
}

func (s *Service) snapshot(instanceID string) (map[string]bool, map[string]int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := map[string]bool{}
	for k := range s.inflight[instanceID] {
		in[k] = true
	}
	run := map[string]int{}
	for k, v := range s.running[instanceID] {
		run[k] = v
	}
	return in, run, len(in)
}

// Tick is one pass of the engine over every instance.
func (s *Service) Tick() {
	for _, in := range s.reg.List("tasks") {
		s.tickInstance(in)
	}
}

func (s *Service) tickInstance(in *control.Instance) {
	now := s.now()
	keep := retentionDays(in)
	inflight, running, n := s.snapshot(in.ID)
	failed, err := s.store.Housekeep(in.ID, inflight, now, keep)
	if err != nil {
		log.Printf("[tasks] %s: housekeeping: %v", in.Name, err)
		return
	}
	for _, f := range failed {
		logFailed(in.Name, f)
	}
	claimed, err := s.store.Claim(in.ID, running, n, now)
	if err != nil {
		log.Printf("[tasks] %s: claiming: %v", in.Name, err)
		return
	}
	for _, c := range claimed {
		s.start(in, c)
	}
}

func logFailed(instance string, f *FailedTask) {
	log.Printf("[tasks] %s/%s %s failed: %s %s", instance, f.Queue, f.ID, f.Status, f.Error)
}

func (s *Service) start(in *control.Instance, c Claimed) {
	r := c.Row
	s.mu.Lock()
	if s.inflight[in.ID] == nil {
		s.inflight[in.ID] = map[string]bool{}
		s.running[in.ID] = map[string]int{}
	}
	s.inflight[in.ID][r.id] = true
	s.running[in.ID][r.queue]++
	s.mu.Unlock()

	target := r.targetOf()
	if target == nil {
		target = c.Settings.Target
	}
	retry := c.Settings.Retry
	if p := r.retryOf(); p != nil {
		retry = *p
	}
	params := json.RawMessage("null")
	if r.params.Valid && json.Valid([]byte(r.params.String)) {
		params = json.RawMessage(r.params.String)
	}
	env := Envelope{
		ID: r.id, Queue: r.queue, Name: nstr(r.name), Attempt: r.attempts, MaxAttempts: retry.MaxAttempts,
		RunAt: r.runAt, CreatedAt: r.created, Source: r.source, Depth: r.depth, Params: params, DeadLetterOf: nstr(r.dlqOf),
		// From the moment it was claimed: the task's own deadline, or its queue's.
		DeadlineAt: r.started.Int64 + r.deadlineOf(c.Settings)*1000,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight[in.ID], r.id)
			if s.running[in.ID][r.queue]--; s.running[in.ID][r.queue] <= 0 {
				delete(s.running[in.ID], r.queue)
			}
			s.mu.Unlock()
			s.Kick()
		}()
		o := s.deliver(in.ID, target, env, r.payload)
		log.Printf("[tasks] %s/%s %s attempt %d → %s: %s", in.Name, r.queue, r.id, r.attempts, TargetLabel(target), o.Status)
		// The instance may have been deleted meanwhile; its store is gone and so is the row.
		if s.reg.GetByID("tasks", in.ID) == nil {
			return
		}
		f, err := s.store.Settle(in.ID, r.id, r.attempts, o, s.now(), retentionDays(in))
		if err != nil {
			log.Printf("[tasks] %s: recording %s: %v", in.Name, r.id, err)
		}
		if f != nil {
			logFailed(in.Name, f)
		}
	}()
}

// --- what every surface does to an instance ---------------------------------------------------

// MayTarget checks that the caller may name a target — nil for a caller acting for the whole org
// (the console, the engine's own triggers).
type MayTarget func(t *Target) error

// SaveQueue creates (`create`) or changes a queue. A change is a PATCH over the current settings.
func (s *Service) SaveQueue(in *control.Instance, name string, patch any, create bool, may MayTarget) (*QueueInfo, error) {
	qname, err := ValidateQueueName(name, "queue")
	if err != nil {
		return nil, err
	}
	base := DefaultQueueSettings()
	if !create {
		if base, err = s.store.QueueSettingsOf(in.ID, qname); err != nil {
			return nil, err
		}
	}
	settings, err := ValidateQueueSettings(patch, base, qname)
	if err != nil {
		return nil, err
	}
	tb, _ := json.Marshal(settings.Target)
	bb, _ := json.Marshal(base.Target)
	if create || string(tb) != string(bb) {
		if may != nil {
			if err := may(settings.Target); err != nil {
				return nil, err
			}
		}
		if err := s.assertTargetExists(settings.Target); err != nil {
			return nil, err
		}
	}
	info, err := s.store.PutQueue(in.ID, qname, settings, create, s.now())
	if err != nil {
		return nil, err
	}
	// Resuming a queue, raising its rate or giving it a target can make work due now.
	s.Kick()
	return info, nil
}

// assertTargetExists: a target must exist when a QUEUE is given it — the thing a person sets once
// and forgets. A task's own target is checked when it runs.
func (s *Service) assertTargetExists(t *Target) error {
	switch {
	case t == nil || t.Webhook != "":
		return nil
	case t.Function != "":
		inst, _ := SplitRef(t.Function)
		if s.reg.Get("functions", inst) == nil {
			return bad("no functions instance '" + inst + "' in this organization")
		}
	case t.Channel != "":
		inst, _ := SplitRef(t.Channel)
		if s.reg.Get("channel", inst) == nil {
			return bad("no channel instance '" + inst + "' in this organization")
		}
	}
	return nil
}

// AddTasks adds one or many tasks to a queue. body is a task, an array of tasks, or { tasks }.
func (s *Service) AddTasks(in *control.Instance, queue string, body json.RawMessage, source string, depth int, may MayTarget) ([]AddResult, error) {
	items, err := ValidateTaskList(body, s.now())
	if err != nil {
		return nil, err
	}
	if may != nil {
		for _, t := range items {
			if err := may(t.Target); err != nil {
				return nil, err
			}
		}
	}
	q, err := ValidateQueueName(queue, "queue")
	if err != nil {
		return nil, err
	}
	if depth > MaxTriggerDepth {
		depth = MaxTriggerDepth
	}
	return s.add(in, q, items, source, depth)
}

func (s *Service) add(in *control.Instance, queue string, items []TaskInput, source string, depth int) ([]AddResult, error) {
	out, wake, err := s.store.Add(in.ID, queue, items, source, depth, s.now())
	if err != nil {
		return nil, err
	}
	if wake {
		s.Kick()
	}
	return out, nil
}

// Run runs a task now (see Store.Run).
func (s *Service) RunTask(in *control.Instance, id string) (*TaskRecord, error) {
	rec, push, err := s.store.Run(in.ID, id, s.now())
	if err != nil {
		return nil, err
	}
	if push {
		s.Kick()
	}
	return rec, nil
}

// Release gives a leased task back.
func (s *Service) Release(in *control.Instance, id, leaseID string, delaySeconds float64, errText *string) (*TaskRecord, error) {
	rec, failed, err := s.store.Release(in.ID, id, leaseID, delaySeconds, errText, s.now(), retentionDays(in))
	if failed != nil {
		logFailed(in.Name, failed)
	}
	return rec, err
}
