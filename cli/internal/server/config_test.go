package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func adminJSON(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if s, ok := body.(string); ok {
		rdr = bytes.NewReader([]byte(s))
	} else {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func mcpCall(t *testing.T, srv *httptest.Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer dev")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || len(out.Result.Content) == 0 {
		t.Fatalf("%s: undecodable (%v)", name, err)
	}
	return out.Result.Content[0].Text, out.Result.IsError
}

// The console's config PUT replaces the whole config, as hosted: a field it leaves out is gone.
// Locally it used to merge, so a form that forgot a sibling field worked here and wiped that
// field in production.
func TestConfigPutReplaces(t *testing.T) {
	srv := newTestServer(t)
	status, created := adminJSON(t, srv, "POST", "/admin/datastore", map[string]any{"name": "app"})
	if status != 201 {
		t.Fatalf("create: %d %v", status, created)
	}
	id := created["id"].(string)

	status, out := adminJSON(t, srv, "PUT", "/admin/datastore/"+id+"/config",
		map[string]any{"config": map[string]any{"autoIndex": false}})
	if status != 200 {
		t.Fatalf("put: %d %v", status, out)
	}
	cfg := out["config"].(map[string]any)
	if _, kept := cfg["autoId"]; kept || cfg["autoIndex"] != false {
		t.Fatalf("PUT merged instead of replacing: %v", cfg)
	}

	// A bare object is the config itself, and is saved — it used to answer 200 and save nothing.
	status, out = adminJSON(t, srv, "PUT", "/admin/datastore/"+id+"/config", map[string]any{"autoId": "serial"})
	if status != 200 {
		t.Fatalf("bare put: %d %v", status, out)
	}
	if cfg := out["config"].(map[string]any); cfg["autoId"] != "serial" || cfg["autoIndex"] != nil {
		t.Fatalf("bare body not saved as the whole config: %v", cfg)
	}

	if status, _ := adminJSON(t, srv, "PUT", "/admin/datastore/"+id+"/config", `{"config": 5}`); status != 400 {
		t.Errorf("a non-object config = %d, want 400", status)
	}
	if status, _ := adminJSON(t, srv, "PUT", "/admin/datastore/"+id+"/config", ``); status != 400 {
		t.Errorf("an empty body = %d, want 400", status)
	}

	// MCP's patch still merges at the top level.
	if text, isErr := mcpCall(t, srv, "patch_instance_config", map[string]any{
		"service": "datastore", "instance": "app", "changes": map[string]any{"autoIndex": true}}); isErr {
		t.Fatalf("patch: %s", text)
	}
	_, got := adminJSON(t, srv, "GET", "/admin/datastore/"+id, nil)
	if cfg := got["config"].(map[string]any); cfg["autoId"] != "serial" || cfg["autoIndex"] != true {
		t.Fatalf("patch did not merge: %v", cfg)
	}
}
