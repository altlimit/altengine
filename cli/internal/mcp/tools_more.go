// The rest of the hosted tool surface: the datastore, search and functions tools the emulator
// was missing, plus channel and auth.
//
// These are not new capability — every one maps onto a route the emulator already served. They
// were simply never written, and the parity test only noticed because its own list of hosted
// tools was regenerated: hosted grew from 16 tools to 42 while this file's ancestor went on
// asserting the old 16 and passing.
//
// Same contract as everywhere else: names and argument spellings match hosted exactly, because
// an agent that gets a call wrong guesses rather than re-reading the schema.

package mcp

import (
	"fmt"
	"net/http"
	"net/url"
)

// authAdminPath builds a control-plane path for an auth instance. Auth's user administration is
// an ADMIN surface, not a data-plane one — the /v1 auth routes are for end users signing
// themselves in, and an agent acting on the org's behalf is not an end user.
func (h *Handler) authAdminPath(args map[string]any, suffix string) (string, error) {
	in, err := h.instanceOf("auth", argString(args, "instance"))
	if err != nil {
		return "", err
	}
	return "/admin/auth/" + url.PathEscape(in.ID) + suffix, nil
}

func init() {
	register(
		// --- datastore ------------------------------------------------------
		tool{
			name:        "datastore_get",
			title:       "Read documents by key",
			description: "Fetch documents by their exact keys. Faster and cheaper than a query when you already know the keys; missing keys are simply absent from the result rather than an error.",
			required:    []string{"instance", "collection", "keys"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string", "description": "Collection name."},
				"keys":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Document keys to fetch."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents/get",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"keys": args["keys"]})
			},
		},
		tool{
			name:        "datastore_delete",
			title:       "Delete documents",
			description: "Permanently delete documents by key. There is no undo and no soft delete — read them first if you might need them.",
			required:    []string{"instance", "collection", "keys", "confirm"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string", "description": "Collection name."},
				"keys":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Document keys to delete."},
				"confirm":    map[string]any{"type": "boolean", "description": "Must be true. This cannot be undone."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/documents/delete",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"keys": args["keys"]})
			},
		},
		tool{
			name:        "datastore_aggregate",
			title:       "Count and summarize documents",
			description: "Run count/sum/avg/min/max over a collection, optionally grouped. Use this instead of paging every document to count them — it is one request and does not fill your context with data you are going to throw away.",
			required:    []string{"instance", "collection", "metrics"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string", "description": "Collection name."},
				"metrics":    map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": `[{"fn":"count"}] or [{"fn":"sum","field":"total","as":"revenue"}]. Functions: count, sum, avg, min, max.`},
				"where":      map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": `Same filters as datastore_query: [{"field":"done","op":"=","value":false}].`},
				"group_by":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Field names to group by."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				// `op` is accepted as an alias for `fn`, matching hosted. Hosted's schema
				// documented `op` while its engine only ever read `fn`, so the tool was
				// unusable exactly as specified; the alias is there because models were told
				// `op` for a long time, and it must be here too or the two disagree.
				body := map[string]any{"metrics": aliasAggregateFn(args["metrics"])}
				for _, k := range []string{"where", "group_by"} {
					if v, ok := args[k]; ok {
						body[k] = v
					}
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/aggregate",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodPost, path, body)
			},
		},

		// --- search ---------------------------------------------------------
		tool{
			name:        "search_delete_documents",
			title:       "Delete documents from an index",
			description: "Remove documents from a search index by id. Deleting every document in an index removes the index itself.",
			required:    []string{"instance", "index", "ids", "confirm"},
			props: map[string]any{
				"instance":  instanceArg,
				"namespace": nsArg,
				"index":     map[string]any{"type": "string", "description": "Index name."},
				"ids":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Document ids to delete."},
				"confirm":   map[string]any{"type": "boolean", "description": "Must be true. This cannot be undone."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents/delete",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "index")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"ids": args["ids"]})
			},
		},

		// --- functions ------------------------------------------------------
		tool{
			name:        "functions_versions",
			title:       "List a function's versions",
			description: "Every deployed version of one function, newest first, and which one is currently serving. Use it to find the version to roll back to.",
			required:    []string{"instance", "name"},
			props: map[string]any{
				"instance": instanceArg,
				"name":     map[string]any{"type": "string", "description": "Function name."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/functions/%s/%s/versions", url.PathEscape(in.Name), url.PathEscape(argString(args, "name")))
				return h.serveInternal(r, http.MethodGet, path, nil)
			},
		},
		tool{
			name:        "functions_rollback",
			title:       "Roll a function back to an earlier version",
			description: "Point a function at a version it was already running. Nothing is rebuilt and no code is uploaded — this only moves which existing version serves traffic, so it is the fast way out of a bad deploy.",
			required:    []string{"instance", "name", "version"},
			props: map[string]any{
				"instance": instanceArg,
				"name":     map[string]any{"type": "string", "description": "Function name."},
				"version":  map[string]any{"type": "number", "description": "Version number from functions_versions."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/functions/%s/%s/activate", url.PathEscape(in.Name), url.PathEscape(argString(args, "name")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"version": args["version"]})
			},
		},
		tool{
			name:        "functions_delete_version",
			title:       "Delete a function version",
			description: "Permanently delete one stored version of a function so it can no longer be activated. Refused for the version being served — activate another one first. Deploying already keeps only the newest versions, so this is for removing a specific one. Requires confirm: true.",
			required:    []string{"instance", "name", "version", "confirm"},
			props: map[string]any{
				"instance": instanceArg,
				"name":     map[string]any{"type": "string", "description": "Function name."},
				"version":  map[string]any{"type": "number", "description": "A version number from functions_versions."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. Guards against a deletion nobody asked for."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/functions/%s/%s/versions/%s", url.PathEscape(in.Name),
					url.PathEscape(argString(args, "name")), url.PathEscape(fmt.Sprint(args["version"])))
				return h.serveInternal(r, http.MethodDelete, path, nil)
			},
		},
		tool{
			name:        "functions_delete",
			title:       "Delete a function",
			description: "Permanently delete a function: it stops serving and running on its schedules immediately, and every stored version of its code is removed. Not recoverable — redeploying the same name starts a new history. Requires confirm: true.",
			required:    []string{"instance", "name", "confirm"},
			props: map[string]any{
				"instance": instanceArg,
				"name":     map[string]any{"type": "string", "description": "Function name."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. Confirms the function and its code are gone for good."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("functions", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/functions/%s/%s", url.PathEscape(in.Name), url.PathEscape(argString(args, "name")))
				return h.serveInternal(r, http.MethodDelete, path, nil)
			},
		},

		// --- channel --------------------------------------------------------
		tool{
			name:        "channel_publish",
			title:       "Publish a message to a channel",
			description: "Send a message to everyone subscribed to a channel right now. Publishing to a channel nobody is listening on is legal and does nothing — messages are not stored and there is no history to catch up on.",
			required:    []string{"instance", "channel", "data"},
			props: map[string]any{
				"instance": instanceArg,
				"channel":  map[string]any{"type": "string", "description": "Channel name, e.g. 'room:42'."},
				"data":     map[string]any{"type": "object", "description": "The message payload, as JSON."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("channel", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := "/v1/channel/" + url.PathEscape(in.Name) + "/publish"
				return h.serveInternal(r, http.MethodPost, path, map[string]any{
					"channel": argString(args, "channel"), "data": args["data"],
				})
			},
		},
		tool{
			name:        "channel_list_rooms",
			title:       "List live channels",
			description: "Channels that have subscribers right now, with how many. A channel exists while someone is listening and stops existing when the last one leaves — so an empty list means nobody is connected, not that nothing was ever published.",
			required:    []string{"instance"},
			props: map[string]any{
				"instance": instanceArg,
				"q":        map[string]any{"type": "string", "description": "Only channels whose name contains this."},
				"limit":    map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("channel", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodGet, "/v1/channel/"+url.PathEscape(in.Name)+"/rooms", nil)
				if err != nil {
					return nil, err
				}
				// Filtering and the cap are applied here rather than in the route: the route
				// answers "what is live", and every caller of it wants the whole picture.
				rooms, _ := res["rooms"].([]any)
				q := argString(args, "q")
				limit := boundedLimit(args)
				out := []any{}
				for _, it := range rooms {
					m, _ := it.(map[string]any)
					name, _ := m["channel"].(string)
					if q != "" && !containsFold(name, q) {
						continue
					}
					if len(out) >= limit {
						break
					}
					out = append(out, m)
				}
				return map[string]any{"rooms": out, "total_live": len(rooms)}, nil
			},
		},

		// --- auth -----------------------------------------------------------
		tool{
			name:        "auth_list_users",
			title:       "List end users",
			description: "End users of an auth instance: uid, identifier, claims, whether they are disabled. These are your app's users, not altengine accounts.",
			required:    []string{"instance"},
			props: map[string]any{
				"instance": instanceArg,
				"q":        map[string]any{"type": "string", "description": "Match against the identifier (email or handle)."},
				"limit":    map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				q := url.Values{}
				q.Set("limit", fmt.Sprint(boundedLimit(args)))
				if v := argString(args, "q"); v != "" {
					q.Set("q", v)
				}
				p, err := h.authAdminPath(args, "/users?"+q.Encode())
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodGet, p, nil)
			},
		},
		tool{
			name:        "auth_create_user",
			title:       "Create an end user",
			description: "Create an account directly, without the sign-up flow — for seeding test users or migrating one in. The fields you pass must satisfy the instance's configured sign-up fields.",
			required:    []string{"instance", "fields", "password"},
			props: map[string]any{
				"instance": instanceArg,
				"fields":   map[string]any{"type": "object", "description": `The sign-up fields, e.g. {"email": "a@example.com", "name": "Ada"}.`},
				"password": map[string]any{"type": "string", "description": "The account's password."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("auth", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				body := map[string]any{"password": argString(args, "password")}
				if f, ok := args["fields"].(map[string]any); ok {
					for k, v := range f {
						body[k] = v
					}
				}
				return h.serveInternal(r, http.MethodPost, "/v1/auth/"+url.PathEscape(in.Name)+"/signup", body)
			},
		},
		tool{
			name:        "auth_set_user_claims",
			title:       "Set a user's claims",
			description: "Replace a user's claims — the values row-level rules read to decide what they may access. Replaces the whole object, so send the claims you want them to end up with, not just the changed ones.",
			required:    []string{"instance", "uid", "claims"},
			props: map[string]any{
				"instance": instanceArg,
				"uid":      map[string]any{"type": "string", "description": "User id from auth_list_users."},
				"claims":   map[string]any{"type": "object", "description": `The complete claims object, e.g. {"role": "admin"}.`},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.authAdminPath(args, "/users/"+url.PathEscape(argString(args, "uid"))+"/claims")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodPut, p, map[string]any{"claims": args["claims"]})
			},
		},
		tool{
			name:        "auth_disable_user",
			title:       "Disable or re-enable a user",
			description: "Lock an account without deleting it: they cannot sign in and existing sessions stop working, but their data and claims are kept. Prefer this to deletion — it is reversible.",
			required:    []string{"instance", "uid"},
			props: map[string]any{
				"instance": instanceArg,
				"uid":      map[string]any{"type": "string", "description": "User id."},
				"disabled": map[string]any{"type": "boolean", "description": "Defaults to true. Pass false to re-enable."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				disabled := true
				if v, ok := args["disabled"].(bool); ok {
					disabled = v
				}
				p, err := h.authAdminPath(args, "/users/"+url.PathEscape(argString(args, "uid"))+"/disabled")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodPut, p, map[string]any{"disabled": disabled})
			},
		},
		tool{
			name:        "auth_delete_user",
			title:       "Delete an end user",
			description: "Permanently delete an account and its sessions. Their documents are NOT deleted — anything keyed by their uid stays behind. Consider auth_disable_user instead, which is reversible.",
			required:    []string{"instance", "uid", "confirm"},
			props: map[string]any{
				"instance": instanceArg,
				"uid":      map[string]any{"type": "string", "description": "User id."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. This cannot be undone."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.authAdminPath(args, "/users/"+url.PathEscape(argString(args, "uid")))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodDelete, p, nil)
			},
		},
		tool{
			name:        "auth_get_rules",
			title:       "Read the access rules",
			description: "The instance's row-level access rules — what an end user's identity token may read and write in datastore and channel. Read these before changing them: a write replaces the whole object.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": instanceArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("auth", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				access, _ := in.Config["access"].(map[string]any)
				if access == nil {
					access = map[string]any{}
				}
				return map[string]any{"instance": in.Name, "access": access}, nil
			},
		},
		tool{
			name:  "auth_set_rules",
			title: "Set the access rules",
			description: "Replace the instance's row-level access rules. REPLACES the whole object — call auth_get_rules first and send back the result with your change applied, or you will silently drop every rule you did not repeat. Rules decide what end users can reach directly, so a mistake here is a data exposure.",
			required:    []string{"instance", "access"},
			props: map[string]any{
				"instance": instanceArg,
				"access":   map[string]any{"type": "object", "description": "The complete access rules object, as returned by auth_get_rules."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("auth", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				access, ok := args["access"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("access must be an object")
				}
				h.reg.SetConfig(in, map[string]any{"access": access})
				return map[string]any{"instance": in.Name, "access": access}, nil
			},
		},
	)
}

// aliasAggregateFn rewrites {"op": ...} to {"fn": ...}, leaving an explicit `fn` alone.
func aliasAggregateFn(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		if _, has := m["fn"]; !has {
			if op, hasOp := m["op"]; hasOp {
				copyM := map[string]any{}
				for k, val := range m {
					copyM[k] = val
				}
				copyM["fn"] = op
				delete(copyM, "op")
				out = append(out, copyM)
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// containsFold is a case-insensitive substring test, for the one place a tool filters names.
func containsFold(s, sub string) bool {
	ls, lsub := []rune(s), []rune(sub)
	if len(lsub) == 0 {
		return true
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(lsub) <= len(ls); i++ {
		match := true
		for j := range lsub {
			if lower(ls[i+j]) != lower(lsub[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
