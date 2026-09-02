// env.automation, which is the one stub the emulator declares and cannot run.
//
// A run drives a real desktop. There is no local stand-in for that, so the binding exists for
// its grant levels — shared with the hosted stubs through conformance/fn-bindings.json — and
// says plainly why nothing happens. Both halves are worth a test: a stub that vanished locally
// would look like a missing grant, and a refusal that arrived before the grant check would tell
// a read-granted function it was blocked by the emulator when production would have blocked it
// on permission.

package functions

import (
	"strings"
	"testing"
)

func TestAutomationStubIsPresentButNotEmulated(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "fleet", `export default { async fetch(request, env) {
		const present = typeof env.automation?.run === "function";
		let message = "";
		try {
			await env.automation.run({ instance: "office" }, { script: "export-invoices" });
			message = "STARTED-A-RUN";
		} catch (e) { message = e.message; }
		return Response.json({ present, message });
	} };`, map[string]string{"automation": "write"})

	res := invoke(t, mux, "/fn/main/fleet")
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := jsonBody(t, res.Body.String())
	if got["present"] != true {
		t.Fatal("a granted service must be on env, or the gap reads as a missing grant")
	}
	message, _ := got["message"].(string)
	if !strings.Contains(message, "enrolled machines") {
		t.Fatalf("expected the not-emulated explanation, got %q", message)
	}
	// The two things that do work. Without them the message leaves someone with a script and
	// nowhere to run it.
	if !strings.Contains(message, "altengine automation") {
		t.Fatalf("the refusal must say what to do instead, got %q", message)
	}
}

// The grant is checked FIRST, so a level that production would refuse is refused here for that
// reason rather than being masked by the emulator's own limitation.
func TestAutomationStubChecksTheGrantBeforeRefusing(t *testing.T) {
	mux := newTestServer(t)
	deployFn(t, mux, "reporter", `export default { async fetch(request, env) {
		try {
			await env.automation.run({ instance: "office" }, { script: "export-invoices" });
			return new Response("STARTED-A-RUN-ON-A-READ-GRANT", { status: 500 });
		} catch (e) { return new Response(e.message); }
	} };`, map[string]string{"automation": "read"})

	body := invoke(t, mux, "/fn/main/reporter").Body.String()
	if strings.Contains(body, "STARTED-A-RUN") {
		t.Fatal("read-only reporting access must not be able to drive a desktop")
	}
	if strings.Contains(body, "enrolled machines") {
		t.Fatalf("a permission failure was reported as an emulator limitation: %q", body)
	}
}
