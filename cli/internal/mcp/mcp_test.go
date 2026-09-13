package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The invariants worth testing here are the ones whose failure is SILENT.
//
// A tool renamed or an argument respelled does not error locally — it works, and then the
// same call fails against the hosted service, which is the one moment the emulator exists
// to prevent. An ignored argument does not error either: it returns a confident answer to a
// different question.

// call sends one JSON-RPC request and returns the decoded response.
func call(t *testing.T, h *Handler, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.serve(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("undecodable response (status %d): %s", rec.Code, rec.Body.String())
	}
	return out
}

func result(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", resp)
	}
	return r
}

// toolText runs a tool and returns its text content plus whether it was an error result.
func toolText(t *testing.T, h *Handler, name string, args map[string]any) (string, bool) {
	t.Helper()
	r := result(t, call(t, h, "tools/call", map[string]any{"name": name, "arguments": args}))
	isErr, _ := r["isError"].(bool)
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		return "", isErr
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, isErr
}

func newTestHandler() *Handler {
	// A bare mux: the tools that dispatch into it are exercised by the emulator's own
	// end-to-end tests. What is under test here is the protocol and the tool contract.
	return NewHandler(nil, http.NewServeMux(), true)
}

// THE PARITY CONTRACT.
//
// These names are shared with the hosted server and are the whole reason the emulator's MCP
// endpoint is worth having. Changing one here without changing it there — or vice versa —
// means an agent that worked locally breaks in production, so this list is deliberately
// written out rather than derived from the registry it is checking.
//
// Regenerate it from the hosted registry rather than by hand — it went stale once already,
// while hosted grew from 16 tools to 42 and this file went on asserting the old 16.
var hostedTools = map[string][]string{
	"auth_create_user":           {"instance", "fields", "password"},
	"auth_delete_user":           {"instance", "uid", "confirm"},
	"auth_disable_user":          {"instance", "uid", "disabled"},
	"auth_get_rules":             {"instance"},
	"auth_list_users":            {"instance", "q", "limit"},
	"auth_set_rules":             {"instance", "access"},
	"auth_set_user_claims":       {"instance", "uid", "claims"},
	"blob_delete":                {"instance", "ids", "confirm"},
	"blob_get":                   {"instance", "id"},
	"blob_list":                  {"instance", "prefix", "limit", "cursor"},
	"blob_put":                   {"instance", "name", "content", "encoding", "content_type", "public"},
	"blob_set_public":            {"instance", "id", "public"},
	"blob_upload_url":            {"instance", "name", "size", "content_type", "public"},
	"channel_list_rooms":         {"instance", "q", "limit"},
	"channel_presence":           {"instance", "channel"},
	"channel_publish":            {"instance", "channel", "data"},
	"container_cancel_job":       {"instance", "job_id", "confirm"},
	"container_get_job":          {"instance", "job_id"},
	"container_job_logs":         {"instance", "job_id", "cursor"},
	"container_list_jobs":        {"instance", "status", "limit", "before"},
	"container_run":              {"instance", "image", "size", "cmd", "env", "timeout_ms"},
	"container_sizes":            {"instance"},
	"create_instance":            {"service", "name"},
	"datastore_aggregate":        {"instance", "namespace", "collection", "metrics", "where", "group_by"},
	"datastore_create_index":     {"instance", "namespace", "collection", "fields", "unique"},
	"datastore_delete":           {"instance", "namespace", "collection", "keys", "confirm"},
	"datastore_get":              {"instance", "namespace", "collection", "keys"},
	"datastore_list_collections": {"instance", "namespace"},
	"datastore_list_indexes":     {"instance", "namespace", "collection"},
	"datastore_put":              {"instance", "namespace", "collection", "documents"},
	"datastore_query":            {"instance", "namespace", "collection", "where", "order", "keys_only", "limit", "cursor"},
	"delete_instance":            {"service", "instance", "confirm"},
	"functions_delete":           {"instance", "name", "confirm"},
	"functions_delete_version":   {"instance", "name", "version", "confirm"},
	"functions_deploy":           {"instance", "name", "code", "grants", "schedules", "activate"},
	"functions_errors":           {"instance", "function", "limit"},
	"functions_list":             {"instance"},
	"functions_rollback":         {"instance", "name", "version"},
	"functions_versions":         {"instance", "name"},
	"get_instance_config":        {"service", "instance"},
	"list_instances":             {"service"},
	"patch_instance_config":      {"service", "instance", "changes"},
	"search_delete_documents":    {"instance", "namespace", "index", "ids", "confirm"},
	"search_get_documents":       {"instance", "namespace", "index", "ids"},
	"search_get_schema":          {"instance", "namespace", "index"},
	"search_list_indexes":        {"instance", "namespace"},
	"search_put_documents":       {"instance", "namespace", "index", "documents"},
	"search_query":               {"instance", "namespace", "index", "query", "limit", "cursor", "returned_fields", "sort", "ids_only", "facets", "facet_discover", "facet_refinements", "total_hits_accuracy"},
	"usage_summary":              {},
	"whoami":                     {},
}

func TestToolsMatchHostedNames(t *testing.T) {
	for name, args := range hostedTools {
		tl, ok := registry[name]
		if !ok {
			t.Errorf("missing tool %q — hosted exposes it, so an agent will call it here", name)
			continue
		}
		for _, a := range args {
			if _, ok := tl.props[a]; !ok {
				t.Errorf("%s: missing argument %q that hosted accepts", name, a)
			}
		}
	}
	for name := range registry {
		if _, ok := hostedTools[name]; !ok {
			t.Errorf("extra tool %q — a tool that exists only locally teaches a workflow that fails hosted", name)
		}
	}
}

func TestUnknownArgumentIsRefusedNotIgnored(t *testing.T) {
	h := newTestHandler()
	// The bug this guards: hosted shipped datastore_query taking `filters` and handing it to
	// an engine that reads `where`. The filter vanished, every document came back, and it
	// looked like it had worked.
	text, isErr := toolText(t, h, "datastore_query", map[string]any{
		"instance": "x", "collection": "c",
		"filters": []any{map[string]any{"field": "n", "op": ">", "value": 1}},
	})
	if !isErr {
		t.Fatalf("expected an error result, got: %s", text)
	}
	if !strings.Contains(text, "unknown argument 'filters'") {
		t.Errorf("error should name the offending argument, got: %s", text)
	}
	if !strings.Contains(text, "where") {
		t.Errorf("error should name the valid arguments back, got: %s", text)
	}
}

func TestDestructiveToolRefusesWithoutConfirm(t *testing.T) {
	h := newTestHandler()
	text, isErr := toolText(t, h, "delete_instance", map[string]any{"service": "search", "instance": "x"})
	if !isErr || !strings.Contains(text, "confirm") {
		t.Errorf("delete_instance must refuse without confirm, got isErr=%v: %s", isErr, text)
	}
}

func TestDeleteInstanceIsConfirmable(t *testing.T) {
	// It deletes for real, so the confirm gate is the whole safety margin: an agent one
	// hallucinated token away from removing a customer's data must have to say so twice.
	if _, ok := registry["delete_instance"]; !ok {
		t.Fatal("delete_instance is missing")
	}
	if !registry["delete_instance"].confirmable {
		t.Error("delete_instance must be confirmable")
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	// A notification has no id and must produce no reply at all. Every client sends
	// notifications/initialized right after initialize, and answering it confuses some.
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.serve(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("expected 202 for a notification, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("a notification must get no body, got: %s", rec.Body.String())
	}
}

func TestMissingAuthorizationIsRefused(t *testing.T) {
	// Not a security control locally — the emulator is dev-open. It exists so a client
	// configured here is configured correctly for hosted, instead of discovering at deploy
	// time that it needs a credential.
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rec := httptest.NewRecorder()
	h.serve(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Authorization, got %d", rec.Code)
	}
}

func TestBatchIsBounded(t *testing.T) {
	h := newTestHandler()
	msgs := make([]string, MaxBatch+1)
	for i := range msgs {
		msgs[i] = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("["+strings.Join(msgs, ",")+"]"))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.serve(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a batch over the limit must be refused, got %d", rec.Code)
	}
}

func TestBatchReturnsAnArray(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.serve(rec, req)

	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("a batch must answer with an array: %s", rec.Body.String())
	}
	if len(out) != 2 {
		t.Errorf("expected 2 responses, got %d", len(out))
	}
}

func TestInitializeAdvertisesOnlyWhatItServes(t *testing.T) {
	h := newTestHandler()
	r := result(t, call(t, h, "initialize", nil))
	if r["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v", r["protocolVersion"])
	}
	caps, _ := r["capabilities"].(map[string]any)
	for _, want := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[want]; !ok {
			t.Errorf("missing advertised capability %q", want)
		}
	}
}

func TestUnknownToolIsAReadableResult(t *testing.T) {
	h := newTestHandler()
	text, isErr := toolText(t, h, "nope", nil)
	if !isErr || !strings.Contains(text, "no such tool") {
		t.Errorf("expected a readable isError result, got isErr=%v: %s", isErr, text)
	}
}

func TestUnknownMethodIsAProtocolError(t *testing.T) {
	// The inverse of the case above, and the distinction matters: a bad METHOD is the
	// client's problem, whereas a failing TOOL is information the model needs to read.
	h := newTestHandler()
	resp := call(t, h, "does/not/exist", nil)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", resp)
	}
	if int(e["code"].(float64)) != codeMethodNotFound {
		t.Errorf("code = %v, want %d", e["code"], codeMethodNotFound)
	}
}

func TestResourcesGroundTheQueryLanguages(t *testing.T) {
	h := newTestHandler()
	r := result(t, call(t, h, "resources/list", nil))
	list, _ := r["resources"].([]any)
	if len(list) != len(resources) {
		t.Fatalf("listed %d resources, have %d", len(list), len(resources))
	}
	for _, uri := range []string{"docs://search/query-language", "docs://datastore/query", "docs://auth/rules", "docs://functions/starter"} {
		if _, err := readResource(uri); err != nil {
			t.Errorf("missing resource %s", uri)
		}
	}
}

// The grounding resources exist to stop a model guessing. One that teaches a name no tool
// accepts is worse than none at all: it manufactures the failure it was added to prevent.
// Hosted shipped exactly that (docs://datastore/query documented `filters`, and the engine
// reads `where`), so it is checked on both sides.
func TestResourcesTeachRealArgumentNames(t *testing.T) {
	cases := map[string]string{
		"docs://datastore/query":       "datastore_query",
		"docs://search/query-language": "search_query",
	}
	for uri, toolName := range cases {
		contents, err := readResource(uri)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		text := contents[0]["text"].(string)
		tl := registry[toolName]

		// Only the block introduced by "...takes these as arguments" is a declaration of
		// arguments. The search resource also has an indented block of query-language
		// examples, and those are syntax, not argument names.
		lines := strings.Split(text, "\n")
		start := -1
		for i, l := range lines {
			if strings.Contains(l, "takes these as arguments") {
				start = i
				break
			}
		}
		if start < 0 {
			t.Fatalf("%s: no argument block", uri)
		}
		found := 0
		for _, l := range lines[start+1:] {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "  ") {
				break
			}
			name, ok := declaredArg(l)
			if !ok {
				continue
			}
			found++
			if _, ok := tl.props[name]; !ok {
				t.Errorf("%s documents %q but %s does not accept it", uri, name, toolName)
			}
		}
		if found < 4 {
			t.Errorf("%s: only found %d argument names — the scan broke", uri, found)
		}
	}
}

// declaredArg reads "  name   description" — two leading spaces, a name, then either two
// spaces or " / " (the "limit / cursor" form).
func declaredArg(line string) (string, bool) {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return "", false
	}
	rest := line[2:]
	for i, c := range rest {
		if c >= 'a' && c <= 'z' || c == '_' {
			continue
		}
		name := rest[:i]
		if name == "" {
			return "", false
		}
		if strings.HasPrefix(rest[i:], "  ") || strings.HasPrefix(rest[i:], " / ") {
			return name, true
		}
		return "", false
	}
	return "", false
}

func TestPromptsExist(t *testing.T) {
	h := newTestHandler()
	r := result(t, call(t, h, "prompts/list", nil))
	list, _ := r["prompts"].([]any)
	if len(list) != len(prompts) {
		t.Errorf("listed %d prompts, have %d", len(list), len(prompts))
	}
	got, err := getPrompt("add-scheduled-job", map[string]string{"task": "prune old rows"})
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(map[string]any)
	if !strings.Contains(content["text"].(string), "prune old rows") {
		t.Error("prompt did not interpolate its argument")
	}
}

func TestMissingResourceNamesTheAlternatives(t *testing.T) {
	_, err := readResource("docs://nope")
	if err == nil || !strings.Contains(err.Error(), "docs://search/query-language") {
		t.Errorf("a missing resource should list the real ones, got: %v", err)
	}
}

func TestEveryToolIsDescribedUsefully(t *testing.T) {
	// A one-line description is the only thing a model has to choose between tools.
	for name, tl := range registry {
		if len(tl.description) < 80 {
			t.Errorf("%s: description too thin to choose on (%d chars)", name, len(tl.description))
		}
		if tl.title == "" {
			t.Errorf("%s: no title", name)
		}
	}
}

func TestBoundedResultsAreDocumented(t *testing.T) {
	// An oversized result does not error — it quietly fills the model's context and
	// degrades every step after it, which is far harder to notice than a failure.
	for name, tl := range registry {
		p, ok := tl.props["limit"].(map[string]any)
		if !ok {
			continue
		}
		desc, _ := p["description"].(string)
		if !strings.Contains(desc, "max") && !strings.Contains(desc, "Default") {
			t.Errorf("%s: limit is undocumented, so a caller cannot know the bound", name)
		}
	}
}
