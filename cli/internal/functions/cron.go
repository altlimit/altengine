package functions

// Five-field cron expressions, in UTC — the same grammar the hosted scheduler accepts.
//
// This is a deliberate re-implementation rather than an approximation: a schedule that
// parses locally and is rejected on deploy (or worse, runs on a different cadence) makes
// the emulator actively misleading. The two subtleties that bite are called out below.
//
// Supported per field: `*`, a number, `a-b` ranges, `a,b,c` lists, and `*/n` or `a-b/n`
// steps. Deliberately NOT supported: `?`, `L`, `W`, `#`, names (`MON`, `JAN`), and
// seconds. The tick is per-minute, so a seconds field would be a lie.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CronFields is one parsed expression: the set of values each field admits.
type CronFields struct {
	Minute []int
	Hour   []int
	DOM    []int
	Month  []int
	DOW    []int
}

// MaxSchedulesPerFn mirrors the hosted cap.
const MaxSchedulesPerFn = 5

// horizonDays bounds the next-run search. Over four years, so a Feb 29 schedule resolves
// instead of being reported as impossible, while a genuinely unsatisfiable expression
// (`0 0 30 2 *` — February 30th) still terminates.
const horizonDays = 366 * 5

func parseField(spec string, min, max int, field string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		piece := strings.TrimSpace(part)
		if piece == "" {
			return nil, fmt.Errorf("empty value in the %s field", field)
		}
		rangePart, stepPart, hasStep := strings.Cut(piece, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepPart)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("step in the %s field must be a positive integer", field)
			}
			step = n
		}
		var lo, hi int
		switch {
		case rangePart == "*":
			lo, hi = min, max
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			x, err1 := strconv.Atoi(a)
			y, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("the %s field must be numeric", field)
			}
			lo, hi = x, y
		default:
			n, err := strconv.Atoi(rangePart)
			if err != nil {
				return nil, fmt.Errorf("the %s field must be numeric", field)
			}
			// A bare number WITH a step means "from here on" (`5/10`), matching cron.
			lo = n
			hi = n
			if hasStep {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("the %s field must be within %d-%d", field, min, max)
		}
		for v := lo; v <= hi; v += step {
			seen[v] = true
		}
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out, nil
}

// ParseCron parses and validates an expression.
func ParseCron(expr string) (CronFields, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	var f CronFields
	if len(parts) != 5 {
		return f, fmt.Errorf("a schedule must have 5 fields: minute hour day-of-month month day-of-week")
	}
	var err error
	if f.Minute, err = parseField(parts[0], 0, 59, "minute"); err != nil {
		return f, err
	}
	if f.Hour, err = parseField(parts[1], 0, 23, "hour"); err != nil {
		return f, err
	}
	if f.DOM, err = parseField(parts[2], 1, 31, "day-of-month"); err != nil {
		return f, err
	}
	if f.Month, err = parseField(parts[3], 1, 12, "month"); err != nil {
		return f, err
	}
	// Day-of-week accepts 7 as a second spelling of Sunday. Parse with 7 IN RANGE and fold
	// it to 0 afterwards — rewriting the text first turns the perfectly normal `0-7`
	// ("every day") into `0-0` ("Sundays only").
	dow, err := parseField(parts[4], 0, 7, "day-of-week")
	if err != nil {
		return f, err
	}
	folded := map[int]bool{}
	for _, v := range dow {
		if v == 7 {
			v = 0
		}
		folded[v] = true
	}
	f.DOW = f.DOW[:0]
	for v := 0; v <= 6; v++ {
		if folded[v] {
			f.DOW = append(f.DOW, v)
		}
	}
	return f, nil
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// dayMatches applies cron's day rule: when BOTH day-of-month and day-of-week are
// restricted, a match on EITHER counts. Restricting only one means that one must match.
func dayMatches(f CronFields, t time.Time) bool {
	if !contains(f.Month, int(t.Month())) {
		return false
	}
	domAll := len(f.DOM) == 31
	dowAll := len(f.DOW) == 7
	if domAll && dowAll {
		return true
	}
	domHit := contains(f.DOM, t.Day())
	dowHit := contains(f.DOW, int(t.Weekday()))
	if domAll {
		return dowHit
	}
	if dowAll {
		return domHit
	}
	return domHit || dowHit
}

// NextRun is the first time strictly after `after` that the expression matches, or the
// zero time if it never does.
//
// Scans DAY BY DAY, and only within a matching day walks that day's candidate times —
// which are just the parsed hour and minute lists. Minute-by-minute scanning would have to
// walk millions of steps for a rare-but-valid expression like `0 0 29 2 *`.
func NextRun(f CronFields, after time.Time) time.Time {
	// Start from the next whole minute, so a run never re-matches its own due time.
	start := after.UTC().Truncate(time.Minute).Add(time.Minute)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i <= horizonDays; i++ {
		d := day.AddDate(0, 0, i)
		if !dayMatches(f, d) {
			continue
		}
		for _, h := range f.Hour {
			for _, m := range f.Minute {
				t := d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
				if !t.Before(start) {
					return t
				}
			}
		}
	}
	return time.Time{}
}

// ValidateCron checks an expression for storage, rejecting one that can never match.
func ValidateCron(expr string) (string, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return "", fmt.Errorf("schedule must be a cron expression")
	}
	f, err := ParseCron(trimmed)
	if err != nil {
		return "", err
	}
	if NextRun(f, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return "", fmt.Errorf("that schedule never matches a real date")
	}
	return trimmed, nil
}

// Soonest is the earliest next-run across every expression: the function's real due time.
func Soonest(exprs []string, after time.Time) time.Time {
	var best time.Time
	for _, e := range exprs {
		f, err := ParseCron(e)
		if err != nil {
			continue
		}
		next := NextRun(f, after)
		if next.IsZero() {
			continue
		}
		if best.IsZero() || next.Before(best) {
			best = next
		}
	}
	return best
}

// NormalizeSchedules turns deploy input into the stored list.
//
// THE SEPARATOR IS THE NEWLINE, and only the newline: a comma is a LIST inside a cron
// field (`0 9,17 * * *` is 9am and 5pm), so splitting on commas would shred valid
// expressions. Blanks are dropped and duplicates collapsed.
func NormalizeSchedules(lines []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range lines {
		for _, line := range strings.Split(item, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || seen[t] {
				continue
			}
			valid, err := ValidateCron(t)
			if err != nil {
				return nil, fmt.Errorf("schedule %q: %w", t, err)
			}
			seen[t] = true
			out = append(out, valid)
		}
	}
	if len(out) > MaxSchedulesPerFn {
		return nil, fmt.Errorf("at most %d schedules per function", MaxSchedulesPerFn)
	}
	return out, nil
}

// decodeSchedules resolves the deploy body's schedule field into "inherit" (nil) or a
// concrete list.
//
// The three cases are genuinely different and all three happen:
//
//	absent        -> nil, inherit. A client that predates schedules must not unschedule
//	                 a job simply by deploying code.
//	null / []     -> empty list, clear. Otherwise a schedule could never be removed.
//	string / list -> that list, validated.
func decodeSchedules(raw, alias []byte) (*[]string, error) {
	if len(raw) == 0 {
		raw = alias
	}
	if len(raw) == 0 || string(raw) == "null" {
		if len(raw) == 0 {
			return nil, nil // absent: inherit
		}
		empty := []string{}
		return &empty, nil // explicit null: clear
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		out, err := NormalizeSchedules([]string{one})
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("schedules must be a string or a list of strings")
	}
	out, err := NormalizeSchedules(many)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
