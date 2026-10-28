package tasks

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The backoff is App Engine's: doubling from minBackoffSeconds maxDoublings times, then growing
// linearly by the last doubled step, capped at maxBackoffSeconds — the same numbers hosted.
func TestNextAttemptAt(t *testing.T) {
	r := RetryPolicy{MaxAttempts: 10, MinBackoffSeconds: 10, MaxBackoffSeconds: 3600, MaxDoublings: 2, MaxAgeSeconds: 0}
	const now = int64(1_000_000)
	want := map[int]int64{1: 10, 2: 20, 3: 40, 4: 80, 5: 120, 6: 160}
	for attempts, secs := range want {
		if got := NextAttemptAt(r, attempts, now, now); got != now+secs*1000 {
			t.Errorf("after %d failures: %d s, want %d s", attempts, (got-now)/1000, secs)
		}
	}
	if NextAttemptAt(r, 10, now, now) != 0 {
		t.Error("a spent task must not be retried")
	}
	r.MaxBackoffSeconds = 30
	if got := NextAttemptAt(r, 4, now, now); got != now+30_000 {
		t.Errorf("capped wait = %d", got-now)
	}
	r.MaxAgeSeconds = 60
	if NextAttemptAt(r, 1, now-55_000, now) != 0 {
		t.Error("a task past its maximum age must not be retried")
	}
}

func TestReadTargetControl(t *testing.T) {
	const now = int64(1_800_000_000_000)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	if ReadTargetControl(h("content-type", "text/plain"), now) != nil {
		t.Fatal("no control headers must read as nothing asked")
	}
	c := ReadTargetControl(h("x-task-retry", " DEFER ", "retry-after", "1.5", "x-task-max-attempts", "3"), now)
	if c.Retry != "defer" || c.RetryAt != now+1500 || c.MaxAttempts != 3 {
		t.Fatalf("control = %+v", c)
	}
	// x-task-retry-after wins over Retry-After; an HTTP date works; the past is now; 30 days caps it.
	date := time.UnixMilli(now + 60_000).UTC().Format(http.TimeFormat)
	if c := ReadTargetControl(h("x-task-retry-after", date, "retry-after", "5"), now); c.RetryAt != now+60_000 {
		t.Fatalf("date = %+v", c)
	}
	if c := ReadTargetControl(h("retry-after", "0"), now); c.RetryAt != now {
		t.Fatalf("zero = %+v", c)
	}
	if c := ReadTargetControl(h("retry-after", "99999999"), now); c.RetryAt != now+MaxDelayMS {
		t.Fatalf("cap = %+v", c)
	}
	for _, bad := range []string{"0", "101", "two", "-1"} {
		if c := ReadTargetControl(h("x-task-max-attempts", bad), now); c != nil {
			t.Errorf("max attempts %q read as %+v", bad, c)
		}
	}
	if c := ReadTargetControl(h("x-task-retry", "later"), now); c != nil {
		t.Errorf("unknown retry value read as %+v", c)
	}
}

func TestTaskInputNormalizes(t *testing.T) {
	const now = int64(1_800_000_000_000)
	in, err := ValidateTaskInput(json.RawMessage(`{"payload": { "a" : [1, 2] }, "runAt": "2027-01-15T10:00:00Z", "params": {"k": "v"}, "retry": {"maxAttempts": 2}}`), now, "task")
	if err != nil {
		t.Fatal(err)
	}
	if in.Payload != `{"a":[1,2]}` || *in.Params != `{"k":"v"}` || in.Retry.MaxAttempts != 2 || in.Retry.MinBackoffSeconds != 10 {
		t.Fatalf("input = %+v", in)
	}
	if want := time.Date(2027, 1, 15, 10, 0, 0, 0, time.UTC).UnixMilli(); in.RunAt != want {
		t.Fatalf("runAt = %d, want %d", in.RunAt, want)
	}
	// A time in the past is now.
	past, _ := ValidateTaskInput(json.RawMessage(`{"payload": null, "runAt": 5}`), now, "task")
	if past.RunAt != now || past.Payload != "null" {
		t.Fatalf("past = %+v", past)
	}
	if _, err := ValidateTaskInput(json.RawMessage(`{"payload": 1, "retry": {"maxAttempts": 0}}`), now, "task"); err == nil ||
		!strings.Contains(err.Error(), "task.retry.maxAttempts must be an integer from 1 to 100") {
		t.Fatalf("retry bound: %v", err)
	}
}

func TestQueueRefs(t *testing.T) {
	for _, ok := range []string{"jobs/default", " jobs/q-1 "} {
		if _, err := ValidateQueueRef(ok, "queue"); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for ref, msg := range map[string]string{
		"jobs":                          "must be '<instance>/<name>'",
		"/q":                            "must be '<instance>/<name>'",
		"jobs/":                         "must be '<instance>/<name>'",
		"jobs/Q":                        "queue: the queue name must be 1-63 characters",
		"jobs/a/b":                      "the queue name must be",
		"j/" + strings.Repeat("a", 300): "is too long",
	} {
		if _, err := ValidateQueueRef(ref, "queue"); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: %v, want %q", ref, err, msg)
		}
	}
}
