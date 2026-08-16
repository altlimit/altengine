package functions

// Cron parsing and next-run arithmetic.
//
// The point of these is PARITY: an expression must parse here exactly as it does hosted,
// and produce the same next-run. A schedule that works locally and behaves differently in
// production is worse than no local scheduler at all. The two cases that actually caught
// bugs in the hosted implementation are covered explicitly.

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return v.UTC()
}

func nextFor(t *testing.T, expr, from string) string {
	t.Helper()
	f, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	n := NextRun(f, at(t, from))
	if n.IsZero() {
		return ""
	}
	return n.Format(time.RFC3339)
}

func TestParseAccepts(t *testing.T) {
	for _, expr := range []string{"* * * * *", "0 0 * * *", "*/15 * * * *", "30 2 * * 1-5", "0 9,17 1,15 * *"} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("%q should parse: %v", expr, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	// Six fields is the seconds-first variant: rejected rather than reinterpreted, since
	// the tick is per-minute and accepting it would silently give a coarser schedule.
	for _, expr := range []string{"* * * * * *", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "MON * * * *", "5-1 * * * *", "*/0 * * * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("%q should be rejected", expr)
		}
	}
}

func TestDayOfWeekSevenIsSunday(t *testing.T) {
	f, err := ParseCron("0 0 * * 7")
	if err != nil || len(f.DOW) != 1 || f.DOW[0] != 0 {
		t.Fatalf("dow 7 should fold to Sunday, got %v (%v)", f.DOW, err)
	}
	// The bug this guards: normalizing 7 by rewriting the TEXT turns `0-7` (every day)
	// into `0-0` (Sundays only) — a job that then runs once a week instead of daily.
	f, err = ParseCron("0 0 * * 0-7")
	if err != nil || len(f.DOW) != 7 {
		t.Fatalf("0-7 should be every day, got %v (%v)", f.DOW, err)
	}
}

func TestNextRun(t *testing.T) {
	cases := []struct{ expr, from, want string }{
		// Strictly after: a run that re-matched its own due time would fire every tick.
		{"* * * * *", "2026-03-01T00:00:00Z", "2026-03-01T00:01:00Z"},
		{"0 0 * * *", "2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z"},
		{"30 2 * * *", "2026-03-01T05:00:00Z", "2026-03-02T02:30:00Z"},
		{"*/15 * * * *", "2026-03-01T10:07:00Z", "2026-03-01T10:15:00Z"},
		// 2026-03-01 is a Sunday, so the next weekday run is Monday the 2nd.
		{"0 9 * * 1-5", "2026-03-01T00:00:00Z", "2026-03-02T09:00:00Z"},
		// Both day fields restricted: EITHER matching counts.
		{"0 0 1 * 1", "2026-03-01T12:00:00Z", "2026-03-02T00:00:00Z"},
		{"0 0 1 1 *", "2026-06-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		// A leap day must resolve rather than be reported impossible: the horizon has to
		// reach the next leap year, which a 400-day one does not.
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		// Sub-minute input is ignored.
		{"* * * * *", "2026-03-01T00:00:30Z", "2026-03-01T00:01:00Z"},
	}
	for _, c := range cases {
		if got := nextFor(t, c.expr, c.from); got != c.want {
			t.Errorf("next(%q, %s) = %q, want %q", c.expr, c.from, got, c.want)
		}
	}
}

func TestNextRunImpossibleTerminates(t *testing.T) {
	// February 30th. Without a bounded horizon this is an infinite loop.
	if got := nextFor(t, "0 0 30 2 *", "2026-03-01T00:00:00Z"); got != "" {
		t.Errorf("February 30th should never match, got %q", got)
	}
	if _, err := ValidateCron("0 0 30 2 *"); err == nil {
		t.Error("an unsatisfiable expression should be rejected on write")
	}
}

func TestSoonestAcrossExpressions(t *testing.T) {
	// A function is due at the earliest of its expressions.
	got := Soonest([]string{"0 9 * * *", "0 2 * * *"}, at(t, "2026-01-01T00:00:00Z"))
	if want := "2026-01-01T02:00:00Z"; got.Format(time.RFC3339) != want {
		t.Errorf("soonest = %s, want %s", got.Format(time.RFC3339), want)
	}
}

func TestNormalizeSchedules(t *testing.T) {
	// A comma is a LIST INSIDE a field, so it must not be treated as a separator —
	// splitting on it would shred this expression into nonsense.
	out, err := NormalizeSchedules([]string{"0 9,17 * * *"})
	if err != nil || len(out) != 1 || out[0] != "0 9,17 * * *" {
		t.Fatalf("comma is not a separator: %v (%v)", out, err)
	}
	// Newlines are, and blanks/duplicates are dropped rather than erroring.
	out, err = NormalizeSchedules([]string{"0 9 * * 1-5\n\n  0 9 * * 1-5  \n0 12 * * 6"})
	if err != nil || len(out) != 2 {
		t.Fatalf("expected 2 deduped expressions, got %v (%v)", out, err)
	}
	if _, err := NormalizeSchedules([]string{"0 1 * * *\n0 2 * * *\n0 3 * * *\n0 4 * * *\n0 5 * * *\n0 6 * * *"}); err == nil {
		t.Error("more than the cap should be rejected")
	}
	if _, err := NormalizeSchedules([]string{"not a cron"}); err == nil {
		t.Error("a bad expression should be rejected")
	}
}

func TestDecodeSchedulesTriState(t *testing.T) {
	// Absent means INHERIT: a client that predates schedules must not unschedule a job
	// simply by deploying code.
	if got, err := decodeSchedules(nil, nil); got != nil || err != nil {
		t.Errorf("absent should inherit, got %v (%v)", got, err)
	}
	// Explicit null means CLEAR, or a schedule could never be removed.
	got, err := decodeSchedules([]byte("null"), nil)
	if err != nil || got == nil || len(*got) != 0 {
		t.Errorf("null should clear, got %v (%v)", got, err)
	}
	// A bare string and a list are both accepted.
	got, err = decodeSchedules([]byte(`"0 9 * * *"`), nil)
	if err != nil || got == nil || len(*got) != 1 {
		t.Errorf("string form: %v (%v)", got, err)
	}
	got, err = decodeSchedules([]byte(`["0 9 * * *","0 12 * * 6"]`), nil)
	if err != nil || got == nil || len(*got) != 2 {
		t.Errorf("list form: %v (%v)", got, err)
	}
	// The singular alias still works.
	got, err = decodeSchedules(nil, []byte(`"0 9 * * *"`))
	if err != nil || got == nil || len(*got) != 1 {
		t.Errorf("alias: %v (%v)", got, err)
	}
}
