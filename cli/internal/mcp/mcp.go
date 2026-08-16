// Package mcp serves the emulator's MCP endpoint: POST /mcp, JSON-RPC 2.0, stateless.
//
// # WHY THE EMULATOR HAS ONE AT ALL
//
// An AI agent building on altengine talks to it over MCP. Without this, the only server an
// agent can develop against is the real one — so every experiment, every wrong guess and
// every throwaway index happens in production, against live data and a live bill. The
// emulator exists precisely so that loop can be run locally, and that argument applies to
// an agent at least as strongly as to a person.
//
// # PARITY IS THE WHOLE POINT
//
// Tool NAMES and ARGUMENT names here must match the hosted server exactly. An agent that
// works out a sequence of calls locally has to be able to run it unchanged against the
// hosted service; a tool named differently, or an argument spelled differently, turns
// "works locally" into a confusing production failure. Where the hosted server refuses
// something, this refuses it too, for the same reason — the divergence that hurts is the
// one where local is MORE permissive.
//
// Conformance keeps the two in step: see conformance/SCENARIOS.md, "MCP".
//
// # HOW TOOLS REACH THE SERVICES
//
// The same way function stubs do (see functions/bindings.go): a synthetic http.Request
// served straight into the emulator's own mux, no socket and no network. So a tool call
// behaves exactly as the REST call it maps to, because it IS that call. The alternative —
// a second implementation against the Go managers — would mean two copies of every rule,
// with the copy nobody exercises being the one agents use.
package mcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// MaxBatch bounds a JSON-RPC batch. Most batches are one message; this is an abuse bound,
// not a capacity plan, and it matches the hosted limit so a client that works here works
// there.
const MaxBatch = 20

// JSON-RPC error codes. The last is ours: -32000..-32099 is the reserved application range.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent => notification, and notifications get no reply
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Handler is the emulator's MCP server.
type Handler struct {
	reg *control.Registry
	mux http.Handler
	// devOpen mirrors the emulator's auth posture. The hosted server requires an API key
	// carrying control scopes; locally any bearer token is accepted, so an agent's config
	// is IDENTICAL in both places apart from the key's value.
	devOpen bool
}

// NewHandler builds the MCP handler. `mux` is the emulator's own router — tool calls are
// dispatched into it in process.
func NewHandler(reg *control.Registry, mux http.Handler, devOpen bool) *Handler {
	return &Handler{reg: reg, mux: mux, devOpen: devOpen}
}

// Register mounts POST /mcp.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", h.serve)
	// A GET is how a client opens a server-initiated SSE stream. There is nothing to push,
	// and saying so is cleaner than holding a stream open forever.
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]any{"code": "METHOD_NOT_ALLOWED", "message": "this MCP server is stateless; POST JSON-RPC to /mcp"},
		})
	})
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	// The header is required even though its value is not checked, so that a client
	// configured for the emulator is configured correctly for hosted too. Discovering the
	// need for a credential only at deploy time is exactly the surprise the emulator is
	// supposed to remove.
	if r.Header.Get("Authorization") == "" {
		common.WriteJSON(w, http.StatusUnauthorized, rpcError(nil, codeInvalidRequest,
			"missing Authorization header: send any bearer token locally; hosted requires an API key with MCP access"))
		return
	}

	body, err := readBody(r)
	if err != nil {
		common.WriteJSON(w, http.StatusBadRequest, rpcError(nil, codeParseError, "invalid JSON"))
		return
	}

	batched := len(body) > 0 && body[0] == '['
	var messages []request
	if batched {
		if err := json.Unmarshal(body, &messages); err != nil {
			common.WriteJSON(w, http.StatusBadRequest, rpcError(nil, codeParseError, "invalid JSON-RPC batch"))
			return
		}
	} else {
		var one request
		if err := json.Unmarshal(body, &one); err != nil {
			common.WriteJSON(w, http.StatusBadRequest, rpcError(nil, codeParseError, "invalid JSON"))
			return
		}
		messages = []request{one}
	}

	if len(messages) > MaxBatch {
		common.WriteJSON(w, http.StatusBadRequest, rpcError(nil, codeInvalidRequest,
			"batch exceeds the limit of 20 messages"))
		return
	}

	responses := make([]any, 0, len(messages))
	for _, m := range messages {
		if m.Method == "" {
			responses = append(responses, rpcError(nil, codeInvalidRequest, "not a JSON-RPC request"))
			continue
		}
		if len(m.ID) == 0 { // notification: no reply, ever
			continue
		}
		responses = append(responses, h.dispatch(r, m))
	}

	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batched {
		common.WriteJSON(w, http.StatusOK, responses)
		return
	}
	common.WriteJSON(w, http.StatusOK, responses[0])
}

func (h *Handler) dispatch(r *http.Request, m request) any {
	switch m.Method {
	case "initialize":
		return rpcResult(m.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			// Only what is actually served. Advertising a capability we do not implement
			// makes a client's first call to it an error rather than a no-op.
			"capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":   map[string]any{"name": "altengine-dev", "title": "altengine (local emulator)", "version": "1"},
			"instructions": "altengine running LOCALLY via `altengine dev`. Same tools as the hosted server, " +
				"same names and arguments, so anything that works here works there. Call whoami first, then " +
				"list_instances. Instance NAMES work anywhere an instance is asked for. Before writing a query, " +
				"read the matching docs:// resource — the search and datastore query languages are App Engine's " +
				"and are not guessable.",
		})

	case "ping":
		return rpcResult(m.ID, map[string]any{})

	case "tools/list":
		return rpcResult(m.ID, map[string]any{"tools": toolSchemas()})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return rpcResult(m.ID, h.callTool(r, p.Name, p.Arguments))

	case "resources/list":
		return rpcResult(m.ID, map[string]any{"resources": resourceList()})

	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(m.Params, &p)
		contents, err := readResource(p.URI)
		if err != nil {
			return rpcError(m.ID, codeInvalidRequest, err.Error())
		}
		return rpcResult(m.ID, map[string]any{"contents": contents})

	case "prompts/list":
		return rpcResult(m.ID, map[string]any{"prompts": promptList()})

	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		got, err := getPrompt(p.Name, p.Arguments)
		if err != nil {
			return rpcError(m.ID, codeInvalidRequest, err.Error())
		}
		return rpcResult(m.ID, got)

	default:
		return rpcError(m.ID, codeMethodNotFound, "unsupported method: "+m.Method)
	}
}

func readBody(r *http.Request) ([]byte, error) {
	var raw json.RawMessage
	if err := common.ReadJSON(r, &raw); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(raw))), nil
}

func rpcResult(id json.RawMessage, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "result": result}
}

func rpcError(id json.RawMessage, code int, message string) any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": message},
	}
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return nil
	}
	return v
}
