package container

import (
	"testing"
)

// started_at is the list cursor. Jobs launched within one millisecond used to share it, and
// paging past the first of them skipped the rest.
func TestPagingNeverSkipsJobsStartedTogether(t *testing.T) {
	s := NewStore(&fakeRunner{avail: true})
	cfg := cfgWith(func(c *Config) { c.MaxConcurrent = 50 })
	for i := 0; i < 7; i++ {
		if _, err := s.Launch("i1", cfg, LaunchRequest{Image: "alpine:3"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	var before int64
	for page := 0; page < 10; page++ {
		jobs, cursor := s.List("i1", "", 2, before)
		for _, j := range jobs {
			if seen[j.ID] {
				t.Fatalf("job %s listed twice", j.ID)
			}
			seen[j.ID] = true
		}
		if cursor == nil {
			break
		}
		before = *cursor
	}
	if len(seen) != 7 {
		t.Fatalf("paged through %d of 7 jobs", len(seen))
	}
}

// The in-memory history is bounded; the oldest finished jobs go first and a running one stays.
func TestHistoryIsPruned(t *testing.T) {
	prev := MaxKeptJobs
	MaxKeptJobs = 3
	t.Cleanup(func() { MaxKeptJobs = prev })

	block := make(chan struct{})
	f := &fakeRunner{avail: true, block: block}
	s := NewStore(f)
	cfg := cfgWith(func(c *Config) { c.MaxConcurrent = 50 })
	first, err := s.Launch("i1", cfg, LaunchRequest{Image: "alpine:3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Everything after the first finishes at once — once the first is actually blocked.
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.started == 1 })
	f.mu.Lock()
	f.block = nil
	f.mu.Unlock()
	for i := 0; i < 5; i++ {
		j, err := s.Launch("i1", cfg, LaunchRequest{Image: "alpine:3"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { got, _ := s.Get("i1", j.ID); return got != nil && got.Status != "running" })
	}
	all, _ := s.List("i1", "", 100, 0)
	if len(all) != 3 {
		t.Fatalf("history holds %d jobs, want 3", len(all))
	}
	if _, ok := s.Get("i1", first.ID); !ok {
		t.Fatal("the running job was pruned")
	}
	close(block)
}
