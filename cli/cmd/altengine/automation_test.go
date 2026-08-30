package main

import (
	"reflect"
	"testing"
)

func TestParamsBecomeAMap(t *testing.T) {
	got := kvMap(stringList{"branch=dallas", "date=2026-08-30", "empty="})
	want := map[string]string{"branch": "dallas", "date": "2026-08-30", "empty": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNoParamsMeansNoParamsField(t *testing.T) {
	// nil rather than an empty map, so the request omits `params` entirely: a script reading
	// job.params should see what the schedule or the caller actually gave it, not {}.
	if got := kvMap(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if got := kvMap(stringList{"no-equals-sign"}); got != nil {
		t.Fatalf("a malformed param produced %v", got)
	}
}

func TestAValueContainingEqualsSurvives(t *testing.T) {
	// Query strings, base64 and connection strings all contain '='. Splitting on the LAST one
	// would silently truncate them.
	got := kvMap(stringList{"query=a=1&b=2"})
	if got["query"] != "a=1&b=2" {
		t.Fatalf("got %q", got["query"])
	}
}
