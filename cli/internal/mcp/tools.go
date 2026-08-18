package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	"github.com/altlimit/altengine/cli/internal/control"
)

// services an agent can create and address. Derived from the registry rather than written out
// again — a hand-written copy of this list is how containers ended up uncreatable through MCP
// while being perfectly creatable everywhere else.
var services = control.Services

// Bounds on what a tool returns.
//
// Lower than the REST default on purpose, and identical to hosted. An oversized result does
// not fail — it quietly fills the model's context and degrades every step that follows,
// which is much harder to notice than an error.
const (
	defaultLimit = 25
	maxLimit     = 200
)

// tool is one MCP tool.
//
// `props` is not decoration: it is the list of argument names the tool accepts, and an
// argument outside it is REFUSED rather than ignored. That behavior is load-bearing —
// hosted shipped a tool that took `filters` and handed it to an engine reading `where`, so
// the filter vanished and the query returned every document while looking like it had
// worked. A model guessing a plausible-but-wrong argument name is the normal case, not an
// edge case, and naming the valid arguments back lets it correct itself in one step.
type tool struct {
	name        string
	title       string
	description string
	required    []string
	props       map[string]any
	annotations map[string]any
	// confirmable tools refuse to run without `confirm: true`.
	confirmable bool
	run         func(h *Handler, r *http.Request, args map[string]any) (any, error)
}

func (t tool) schema() map[string]any {
	in := map[string]any{"type": "object", "properties": t.props}
	if len(t.required) > 0 {
		in["required"] = t.required
	}
	ann := map[string]any{"title": t.title}
	for k, v := range t.annotations {
		ann[k] = v
	}
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": in,
		"annotations": ann,
	}
}

func toolSchemas() []map[string]any {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, registry[n].schema())
	}
	return out
}

// callTool runs a tool and shapes the result the way MCP expects.
//
// A tool FAILURE is a result carrying isError, not a JSON-RPC error. The distinction is
// easy to get backwards and matters: a protocol error is handled by the client and never
// reaches the model, whereas an isError result is handed to it so it can read what went
// wrong and try something else. "Your query named a field that does not exist" is
// information the model needs.
func (h *Handler) callTool(r *http.Request, name string, args map[string]any) map[string]any {
	t, ok := registry[name]
	if !ok {
		return toolError("no such tool '" + name + "'. Call tools/list to see what is available.")
	}
	if args == nil {
		args = map[string]any{}
	}
	if msg := unknownArgs(t, args); msg != "" {
		return toolError(msg)
	}
	if t.confirmable && args["confirm"] != true {
		return toolError(t.name + " is destructive and was not run: pass \"confirm\": true to proceed. Check first that this is the right target.")
	}
	for _, req := range t.required {
		if _, ok := args[req]; !ok {
			return toolError(fmt.Sprintf("%s: missing required argument '%s'", t.name, req))
		}
	}
	v, err := t.run(h, r, args)
	if err != nil {
		return toolError(err.Error())
	}
	return toolResult(v)
}

func unknownArgs(t tool, args map[string]any) string {
	var bad []string
	for k := range args {
		if _, ok := t.props[k]; !ok {
			bad = append(bad, "'"+k+"'")
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Strings(bad)
	valid := make([]string, 0, len(t.props))
	for k := range t.props {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	plural := ""
	if len(bad) > 1 {
		plural = "s"
	}
	return fmt.Sprintf("%s: unknown argument%s %s. Valid arguments: %s. Nothing was run — a silently ignored argument would have returned a wrong answer that looked right.",
		t.name, plural, strings.Join(bad, ", "), strings.Join(valid, ", "))
}

func toolError(msg string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}, "isError": true}
}

func toolResult(v any) map[string]any {
	text, ok := v.(string)
	if !ok {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return toolError("result could not be encoded: " + err.Error())
		}
		text = string(b)
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

// --- in-process dispatch -------------------------------------------------

// serve issues a request against the emulator's own routes, in process. Same technique the
// function stubs use: no socket, no network, and therefore no chance of a tool behaving
// differently from the REST call it maps to.
func (h *Handler) serveInternal(r *http.Request, method, path string, body any) (map[string]any, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	// Any bearer works: the emulator's auth store is dev-open. Carrying the caller's own
	// header through keeps the local logs honest about who asked.
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = "Bearer emulator-internal"
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			// An HTML body here means the path fell through to the console catch-all —
			// i.e. the route does not exist. Say that, rather than "unreadable response".
			return nil, fmt.Errorf("no such endpoint %s %s (the emulator returned a non-JSON response)", method, path)
		}
	}
	if rec.Code >= 400 {
		return nil, fmt.Errorf("%s", errMessage(decoded, rec.Code))
	}
	return decoded, nil
}

func errMessage(decoded map[string]any, status int) string {
	if e, ok := decoded["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		msg, _ := e["message"].(string)
		if code != "" && msg != "" {
			return code + ": " + msg
		}
		if msg != "" {
			return msg
		}
	}
	return fmt.Sprintf("request failed with status %d", status)
}

// --- argument helpers ----------------------------------------------------

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// nsSegment maps the `namespace` argument to its URL segment. A path cannot carry an empty
// segment (routers collapse "//"), so the default namespace is spelled `_default` on the
// wire — the same reserved word the REST API uses.
func nsSegment(args map[string]any) string {
	ns := argString(args, "namespace")
	if ns == "" || ns == "_default" {
		return "_default"
	}
	return url.PathEscape(ns)
}

func boundedLimit(args map[string]any) int {
	v, ok := args["limit"].(float64)
	if !ok {
		return defaultLimit
	}
	n := int(v)
	if n < 1 {
		return 1
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// instanceOf resolves an instance argument (a name, as everywhere else) to its registry
// row, so a tool can fail with "no such instance" before issuing a request.
func (h *Handler) instanceOf(service, name string) (*control.Instance, error) {
	if name == "" {
		return nil, fmt.Errorf("instance is required")
	}
	if in := h.reg.Get(service, name); in != nil {
		return in, nil
	}
	if in := h.reg.GetByID(service, name); in != nil {
		return in, nil
	}
	return nil, fmt.Errorf("%s instance '%s' not found. Call list_instances to see what exists", service, name)
}

func serviceArg(args map[string]any) (string, error) {
	s := argString(args, "service")
	for _, known := range services {
		if s == known {
			return s, nil
		}
	}
	return "", fmt.Errorf("service must be one of: %s", strings.Join(services, ", "))
}

// --- the registry --------------------------------------------------------

var registry = map[string]tool{}

func register(ts ...tool) {
	for _, t := range ts {
		if _, dup := registry[t.name]; dup {
			panic("duplicate MCP tool: " + t.name)
		}
		registry[t.name] = t
	}
}

var (
	instanceArg = map[string]any{"type": "string", "description": "Instance name (as shown by list_instances)."}
	nsArg       = map[string]any{"type": "string", "description": "Namespace. Omit for the default."}
	serviceEnum = map[string]any{"type": "string", "enum": services}
	readOnly    = map[string]any{"readOnlyHint": true, "idempotentHint": true}
	writes      = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false}
)

func init() {
	register(
		tool{
			name:  "whoami",
			title: "Who am I",
			description: "What this credential is: which organization it acts on, what it may do, and the org's instance quota. " +
				"Call this first if unsure what is possible.",
			props:       map[string]any{},
			annotations: readOnly,
			run: func(h *Handler, _ *http.Request, _ map[string]any) (any, error) {
				return map[string]any{
					"org": map[string]any{"id": "local", "name": "local development"},
					// Reported as full so an agent does not waste turns probing what it may
					// do. The emulator has one org and no billing; scopes are a hosted
					// concept and there is nothing here for them to protect.
					"control_scopes": map[string]any{"instances": "full", "functions": "full", "usage": "full"},
					"data_grants":    map[string]any{"*": "full"},
					"environment":    "emulator",
					"note": "Running against `altengine dev`, not the hosted service. Tools, arguments and behavior match " +
						"hosted; usage is not metered and nothing here is billed. Governance (billing, members, API keys) " +
						"is not available over MCP by design, hosted or locally.",
				}, nil
			},
		},

		tool{
			name:  "list_instances",
			title: "List instances",
			description: "List every service instance (search, datastore, channel, auth, functions) with its id, name and creation time. " +
				"Start here: instance NAMES from this call are accepted anywhere a tool asks for an instance.",
			props: map[string]any{
				"service": mergeMap(serviceEnum, map[string]any{"description": "Only this service. Omit for all."}),
			},
			annotations: readOnly,
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				wanted := services
				if argString(args, "service") != "" {
					s, err := serviceArg(args)
					if err != nil {
						return nil, err
					}
					wanted = []string{s}
				}
				out := map[string]any{}
				for _, s := range wanted {
					rows := []map[string]any{}
					for _, in := range h.reg.List(s) {
						rows = append(rows, map[string]any{"id": in.ID, "name": in.Name, "created_at": in.CreatedAt})
					}
					out[s] = rows
				}
				return out, nil
			},
		},

		tool{
			name:  "create_instance",
			title: "Create an instance",
			description: "Create a service instance. The name is how you address it everywhere afterwards and CANNOT be changed later. " +
				"Locally there is no quota; hosted this counts against the organization's instance quota.",
			required: []string{"service", "name"},
			props: map[string]any{
				"service": serviceEnum,
				"name":    map[string]any{"type": "string", "description": "Unique within this service. Lowercase, descriptive, e.g. 'orders'."},
			},
			annotations: writes,
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				service, err := serviceArg(args)
				if err != nil {
					return nil, err
				}
				name := strings.TrimSpace(argString(args, "name"))
				if name == "" {
					return nil, fmt.Errorf("name is required")
				}
				in, err := h.reg.Create(service, name)
				if err != nil {
					return nil, err
				}
				return map[string]any{"service": service, "id": in.ID, "name": in.Name, "created_at": in.CreatedAt}, nil
			},
		},

		tool{
			name:        "get_instance_config",
			title:       "Read instance config",
			description: "Read an instance's full configuration. Call this before changing anything — patch_instance_config merges onto what is here.",
			required:    []string{"service", "instance"},
			props: map[string]any{
				"service":  serviceEnum,
				"instance": map[string]any{"type": "string", "description": "Instance name or id."},
			},
			annotations: readOnly,
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				service, err := serviceArg(args)
				if err != nil {
					return nil, err
				}
				in, err := h.instanceOf(service, argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				cfg := in.Config
				if cfg == nil {
					cfg = map[string]any{}
				}
				return map[string]any{"service": service, "instance": in.Name, "id": in.ID, "config": cfg}, nil
			},
		},

		tool{
			name:  "patch_instance_config",
			title: "Update instance config",
			description: "Merge changes into an instance's configuration. Send ONLY the top-level fields you want to change; everything else is preserved. " +
				"Returns the fields that actually changed. Read get_instance_config first if you need to see the current shape.",
			required: []string{"service", "instance", "changes"},
			props: map[string]any{
				"service":  serviceEnum,
				"instance": map[string]any{"type": "string", "description": "Instance name or id."},
				"changes":  map[string]any{"type": "object", "description": "Top-level config fields to set, e.g. {\"rateLimit\": 600}."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
			// A PATCH, never a PUT. The underlying write REPLACES the config: a caller who
			// sends only the field they meant to change silently wipes every sibling. That
			// has bitten the hosted console twice with a human reading a form; an agent
			// assembling a config from a partial mental model would hit it on its first
			// write, and the damage is silent and arbitrarily bad.
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				service, err := serviceArg(args)
				if err != nil {
					return nil, err
				}
				in, err := h.instanceOf(service, argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				changes, ok := args["changes"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("changes must be an object of top-level config fields")
				}
				current := map[string]any{}
				for k, v := range in.Config {
					current[k] = v
				}
				// Top-level merge only. A deep merge would make it impossible to CLEAR a
				// nested field (an empty array would read as "no change"), and that
				// ambiguity is worse than the limitation.
				merged := map[string]any{}
				for k, v := range current {
					merged[k] = v
				}
				var changed []string
				for k, v := range changes {
					merged[k] = v
					if !jsonEqual(current[k], v) {
						changed = append(changed, k)
					}
				}
				sort.Strings(changed)
				h.reg.SetConfig(in, merged)

				unchanged := []string{}
				for k := range changes {
					if !contains(changed, k) {
						unchanged = append(unchanged, k)
					}
				}
				sort.Strings(unchanged)
				return map[string]any{
					"service": service, "instance": in.Name,
					"changed": changed, "unchanged": unchanged, "config": merged,
				}, nil
			},
		},

		tool{
			name:  "delete_instance",
			title: "Delete an instance",
			description: "PERMANENTLY delete an instance and all of its data. Not recoverable. Requires confirm: true. " +
				"Prefer leaving an unused instance in place unless the user explicitly asked for deletion.",
			required: []string{"service", "instance", "confirm"},
			props: map[string]any{
				"service":  serviceEnum,
				"instance": map[string]any{"type": "string", "description": "Instance name or id."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. Confirms permanent data loss."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false},
			confirmable: true,
			// DELIBERATELY REFUSED, even though deleting locally would be one map write.
			//
			// The hosted server refuses this too: tearing an instance down there means
			// reclaiming per-service storage and stored configuration, which the console
			// owns. Implementing it here would make the emulator MORE permissive than
			// production — the one direction of divergence that actually hurts, because an
			// agent would learn a workflow locally that fails when it matters.
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				service, err := serviceArg(args)
				if err != nil {
					return nil, err
				}
				in, err := h.instanceOf(service, argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("deleting a %s instance is not available over MCP — its teardown is handled by the console. "+
					"Delete '%s' in the admin console (hosted, or the local one at / while `altengine dev` is running)", service, in.Name)
			},
		},

		tool{
			name:  "datastore_query",
			title: "Query datastore",
			description: "Query a datastore collection. Read docs://datastore/query first — a query no index covers is REFUSED, not slow, " +
				"and inequality filters are limited to one field. Results are capped; use the returned cursor for more.",
			required: []string{"instance", "collection"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string"},
				"where": map[string]any{
					"type": "array",
					"description": `Filters, ANDed: [{"field":"status","op":"=","value":"open"}]. Ops: = != < <= > >= in not-in array-contains. ` +
						"Inequalities (< <= > >=) may only be used on ONE field per query, and the first `order` must be that same field.",
					"items": map[string]any{"type": "object"},
				},
				"order":     map[string]any{"type": "array", "description": `[{"field":"created","dir":"desc"}] — dir is "asc" or "desc".`, "items": map[string]any{"type": "object"}},
				"keys_only": map[string]any{"type": "boolean", "description": "Return matching keys only. Cheaper when you do not need the documents."},
				"limit":     map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
				"cursor":    map[string]any{"type": "string", "description": "From a previous response, to fetch the next page."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				// Built field by field, never by forwarding `args` wholesale: the engine
				// reads `where`/`order`, and a body carrying an unrecognised key would be
				// silently ignored rather than refused.
				body := map[string]any{"limit": boundedLimit(args)}
				copyIfPresent(body, args, "where", "order", "keys_only", "cursor")
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/query",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodPost, path, body)
			},
		},

		tool{
			name:  "datastore_put",
			title: "Write documents",
			description: "Insert or replace documents in a collection. Each document is {key?, data}; omit the key to have one generated. " +
				"Replacing an existing key overwrites that document entirely.",
			required: []string{"instance", "collection", "documents"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string"},
				"documents":  map[string]any{"type": "array", "description": `[{"key": "optional-id", "data": { ... }}]`, "items": map[string]any{"type": "object"}},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				docs, ok := args["documents"].([]any)
				if !ok || len(docs) == 0 {
					return nil, fmt.Errorf("documents must be a non-empty array of {key?, data}")
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"documents": docs})
			},
		},

		tool{
			name:  "datastore_list_collections",
			title: "List collections",
			description: "List the collections in a datastore namespace. Collections are created implicitly by the first write, " +
				"so an empty list means nothing has been written here yet.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": instanceArg, "namespace": nsArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				// The collection listing is a control-plane read (there is no /v1 route for
				// it), so this goes through the admin API by instance id.
				path := fmt.Sprintf("/admin/datastore/%s/namespaces/%s/collections", url.PathEscape(in.ID), nsSegment(args))
				res, err := h.serveInternal(r, http.MethodGet, path, nil)
				if err != nil {
					return nil, err
				}
				ns := argString(args, "namespace")
				if ns == "" {
					ns = "(default)"
				}
				return map[string]any{"namespace": ns, "collections": res["collections"]}, nil
			},
		},

		tool{
			name:  "search_query",
			title: "Search an index",
			description: "Run a full-text search. The query is a STRING in App Engine Search syntax, NOT Lucene or SQL — " +
				"read docs://search/query-language before composing one. total_hits is a lower bound unless total_hits_exact is true.",
			required: []string{"instance", "index", "query"},
			props: map[string]any{
				"instance":        instanceArg,
				"namespace":       nsArg,
				"index":           map[string]any{"type": "string"},
				"query":           map[string]any{"type": "string", "description": "e.g. genre:comedy rating > 3"},
				"limit":           map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
				"cursor":          map[string]any{"type": "string"},
				"returned_fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Fetch only these fields."},
				"sort": map[string]any{"type": "array", "items": map[string]any{"type": "object"},
					"description": `[{"expr":"rating","desc":true,"default":0}] — note "desc" is a boolean here, unlike datastore order.`},
				"ids_only": map[string]any{"type": "boolean", "description": "Return matching ids only. Much cheaper when you do not need the fields."},
				// Facets are how an agent finds out what is IN an index without reading all
				// of it — "what genres exist, and how many of each" in one call rather than
				// a scan.
				"facets": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Facet names to return counts for. NOTE facets are a document's separate `facets` array, NOT its fields — a value only appears here if it was written as a facet."},
				"facet_discover":    map[string]any{"type": "number", "description": "Auto-discover the top N facets. Good for exploring an unfamiliar index."},
				"facet_refinements": map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": `[{"name":"genre","value":"scifi"}] — narrow to a facet value.`},
				"total_hits_accuracy": map[string]any{"type": "number",
					"description": "Count matches exactly up to this many (default 20, max 10000). Counting costs, so raise it only when an exact total is actually needed."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				body := map[string]any{"query": argString(args, "query"), "limit": boundedLimit(args)}
				copyIfPresent(body, args, "cursor", "returned_fields", "sort", "ids_only",
					"facets", "facet_discover", "facet_refinements", "total_hits_accuracy")
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/search",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "index")))
				return h.serveInternal(r, http.MethodPost, path, body)
			},
		},

		tool{
			name:  "search_put_documents",
			title: "Index documents",
			description: "Add or replace documents in a search index. Field TYPES are fixed the first time a field is written " +
				"(text, atom, number, date, geo) — 'atom' matches only in full, 'text' is tokenized prose.",
			required: []string{"instance", "index", "documents"},
			props: map[string]any{
				"instance":  instanceArg,
				"namespace": nsArg,
				"index":     map[string]any{"type": "string"},
				"documents": map[string]any{"type": "array", "items": map[string]any{"type": "object"},
					"description": "[{\"id\":\"1\",\"fields\":[{\"name\":\"title\",\"type\":\"text\",\"value\":\"...\"}]," +
						"\"facets\":[{\"name\":\"genre\",\"type\":\"atom\",\"value\":\"comedy\"}]}] — `facets` is OPTIONAL and separate from " +
						"`fields`: only values written as facets are countable by search_query's facet arguments. Writing a value as both is " +
						"normal — one makes it searchable, the other makes it countable."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				docs, ok := args["documents"].([]any)
				if !ok || len(docs) == 0 {
					return nil, fmt.Errorf("documents must be a non-empty array")
				}
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "index")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"documents": docs})
			},
		},

		tool{
			name:  "search_list_indexes",
			title: "List search indexes",
			description: "List the indexes in a search namespace. Indexes are created implicitly by the first document written to them, " +
				"so an empty list means nothing has been indexed yet.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": instanceArg, "namespace": nsArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx", url.PathEscape(in.Name), nsSegment(args))
				res, err := h.serveInternal(r, http.MethodGet, path, nil)
				if err != nil {
					return nil, err
				}
				ns := argString(args, "namespace")
				if ns == "" {
					ns = "(default)"
				}
				return map[string]any{"namespace": ns, "indexes": res["indexes"]}, nil
			},
		},

		tool{
			name:        "functions_list",
			title:       "List functions",
			description: "List the functions deployed to an instance: active version, granted access, schedules and public URL.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": instanceArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodGet, "/v1/functions/"+url.PathEscape(in.Name), nil)
			},
		},

		tool{
			name:  "functions_deploy",
			title: "Deploy a function",
			description: "Deploy a new version of a function. `code` must be ONE self-contained ES module with a default export — " +
				"the server does no bundling and resolves no imports, so inline everything. Read docs://functions/starter for the " +
				"handler shape and the env clients. Omitted fields (grants, schedules) KEEP their current values; pass null to clear.",
			required: []string{"instance", "name", "code"},
			props: map[string]any{
				"instance": instanceArg,
				"name":     map[string]any{"type": "string", "description": "Function name; becomes the first path segment of its URL."},
				"code":     map[string]any{"type": "string", "description": "A complete ES module: export default { async fetch(request, env) { ... } }"},
				"grants": map[string]any{"type": "object",
					"description": `What this function may reach, e.g. {"datastore:orders": "write"}. A service NOT granted is absent from env entirely. ` +
						"Grant the least it needs — a function runs at org level and row rules do not apply to it."},
				"schedules": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Five-field UTC cron expressions, up to 5. Several are allowed because hour and day-of-week are ANDed " +
						"within one expression. Omit to keep existing schedules; pass [] to clear."},
				"activate": map[string]any{"type": "boolean", "description": "Serve this version immediately. Default true."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				if strings.TrimSpace(argString(args, "code")) == "" {
					return nil, fmt.Errorf("code is required: one self-contained ES module")
				}
				body := map[string]any{"name": argString(args, "name"), "code": argString(args, "code")}
				// Forwarded ONLY when present, so "omitted inherits" still holds — sending
				// an explicit null for an absent argument would CLEAR the deployed value.
				copyIfPresent(body, args, "grants", "schedules", "activate")
				res, err := h.serveInternal(r, http.MethodPost, "/v1/functions/"+url.PathEscape(in.Name)+"/deploy", body)
				if err != nil {
					return nil, err
				}
				res["note"] = "Invoke it once over HTTP to check it works before relying on a schedule. Locally it is served at /fn/" +
					in.Name + "/" + argString(args, "name") + " and console.log lands in the `altengine dev` output."
				return res, nil
			},
		},

		tool{
			name:  "functions_errors",
			title: "Read function errors",
			description: "Recent failures for an instance, GROUPED by error rather than one line per failure: message, how often, " +
				"when it started and last happened, and a sample. This is where a broken deploy or a failing scheduled run shows up.",
			required: []string{"instance"},
			props: map[string]any{
				"instance": instanceArg,
				"function": map[string]any{"type": "string", "description": "Only this function. Omit for all."},
				"limit":    map[string]any{"type": "number", "description": "Default 20."},
			},
			annotations: readOnly,
			run: func(h *Handler, _ *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				// The emulator does not aggregate errors — a single-process dev server has
				// somewhere better to put them. Answering with an empty list would be a LIE
				// an agent would read as "nothing is failing", so it says where to look.
				return map[string]any{
					"instance": in.Name,
					"groups":   []any{},
					"note": "Error grouping is a hosted feature. Locally, a failing invocation prints its error and stack " +
						"directly to the `altengine dev` console, along with anything the function logged — look there. " +
						"This tool returns real groups when pointed at the hosted service.",
				}, nil
			},
		},

		tool{
			name:  "usage_summary",
			title: "Usage this month",
			description: "Month-to-date usage and cost for this organization, broken down by service and metric. " +
				"Uses the same rollup as the invoice, so the numbers match what will actually be billed.",
			props:       map[string]any{},
			annotations: readOnly,
			run: func(_ *Handler, _ *http.Request, _ map[string]any) (any, error) {
				// Present, and honest, rather than absent. A tool missing locally would look
				// to an agent like a broken server; a zero would look like free usage.
				return map[string]any{
					"period": "n/a",
					"lines":  []any{},
					"note": "Nothing is metered or billed locally — this is the emulator. Point the same tool at the hosted " +
						"service for real month-to-date usage and cost.",
				}, nil
			},
		},
	)
}

// --- small helpers -------------------------------------------------------

// copyIfPresent forwards only the keys the caller actually sent. Forwarding an absent key
// as null is not the same thing: for functions_deploy, null CLEARS the deployed value.
func copyIfPresent(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}

func mergeMap(base, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
