package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHosted answers the hosted routes a test names, recording each request as "METHOD path".
func fakeHosted(t *testing.T, routes map[string]func(w http.ResponseWriter)) (*[]string, func()) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if h, ok := routes[key]; ok {
			h(w)
			return
		}
		http.NotFound(w, r)
	}))
	isolate(t)
	t.Setenv("ALTENGINE_URL", srv.URL)
	t.Setenv(keyEnv, "ae_test")
	return &seen, srv.Close
}

func body(s string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = w.Write([]byte(s)) }
}

// `automation send` exited 0 when no job received the value, which told a relay it was delivered.
func TestSendFailsWhenNothingIsDelivered(t *testing.T) {
	_, stop := fakeHosted(t, map[string]func(http.ResponseWriter){
		"POST /v1/automation/f/data/otp": body(`{"key":"otp","delivered":0,"runs":[],"unreachable":[],"undetermined":[]}`),
	})
	defer stop()
	code, _, errOut := runMain(t, "automation send --instance f otp 123456")
	if code != 1 || !strings.Contains(errOut, "no job is waiting") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestSendUndeterminedIsNotAFailure(t *testing.T) {
	_, stop := fakeHosted(t, map[string]func(http.ResponseWriter){
		"POST /v1/automation/f/data/otp": body(`{"key":"otp","delivered":0,"undetermined":["r1"]}`),
	})
	defer stop()
	if code, _, errOut := runMain(t, "automation send --instance f otp 123456"); code != 0 {
		t.Fatalf("exit %d (%s): an unconfirmed delivery must not invite a resend", code, errOut)
	}
}

// --wait used to drop a failed log read without a word, so a run whose log could not be read
// looked like a run that printed nothing.
func TestWaitReportsAnUnreadableLog(t *testing.T) {
	_, stop := fakeHosted(t, map[string]func(http.ResponseWriter){
		"POST /v1/automation/f/runs":        body(`{"run":{"id":"r1","status":"running","agent_id":"a1"}}`),
		"GET /v1/automation/f/runs/r1/logs": func(w http.ResponseWriter) { w.WriteHeader(500) },
		"GET /v1/automation/f/runs/r1":      body(`{"run":{"id":"r1","status":"done"},"artifacts":[]}`),
	})
	defer stop()
	code, _, errOut := runMain(t, "automation run --instance f --wait nightly")
	if code != 0 || !strings.Contains(errOut, "could not read the log") || !strings.Contains(errOut, "may be incomplete") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

// A deletion is never the unattended default: without a terminal it needs --yes.
func TestDeleteNeedsConfirmation(t *testing.T) {
	seen, stop := fakeHosted(t, map[string]func(http.ResponseWriter){
		"DELETE /v1/functions/app/hello":            body(`{"deleted":true}`),
		"DELETE /v1/functions/app/hello/versions/2": body(`{"deleted":true}`),
		"DELETE /v1/automation/f/scripts/nightly":   body(`{"deleted":true}`),
	})
	defer stop()
	if code, _, _ := runMain(t, "functions delete --instance app hello"); code != 1 {
		t.Fatalf("delete without --yes: exit %d", code)
	}
	for _, s := range *seen {
		if strings.HasPrefix(s, "DELETE") {
			t.Fatalf("deleted without confirmation: %v", *seen)
		}
	}
	for _, args := range []string{
		"functions delete --instance app --yes hello",
		"functions delete --instance app --version 2 --yes hello",
		"automation delete --instance f --yes nightly",
	} {
		if code, _, errOut := runMain(t, args); code != 0 {
			t.Errorf("%s: exit %d %s", args, code, errOut)
		}
	}
}

// The same operation answers to the same verb in every group.
func TestActivateAndRollbackAreAliases(t *testing.T) {
	seen, stop := fakeHosted(t, map[string]func(http.ResponseWriter){
		"POST /v1/functions/app/hello/activate":          body(`{}`),
		"POST /v1/automation/f/scripts/nightly/activate": body(`{}`),
		"POST /v1/static/web/deployments/d1/activate":    body(`{"deployment_id":"d1"}`),
	})
	defer stop()
	for _, args := range []string{
		"functions activate --instance app --version 2 hello",
		"automation rollback --instance f --version 2 nightly",
		"static activate --instance web d1",
	} {
		if code, _, errOut := runMain(t, args); code != 0 {
			t.Errorf("%s: exit %d %s (%v)", args, code, errOut, *seen)
		}
	}
}
