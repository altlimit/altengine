// The tools that close the last gaps between what an agent can SEE and what it can reach.
//
// Each of these existed as a route long before it existed as a tool, and their absence had the
// same shape every time: the agent is told something is wrong and given no way to fix it. It
// could be refused a query for not being index-served with no way to list or add an index; it
// could query a search index without being able to read the field types the query language
// depends on; it could run a container without being able to ask what sizes the instance takes.
//
// Names and argument spellings match hosted exactly — see mcp_test.go, whose hosted list is
// DUMPED from the hosted registry rather than typed out here.

package mcp

import (
	"fmt"
	"net/http"
	"net/url"
)

func init() {
	register(
		// --- search ---------------------------------------------------------
		tool{
			name:  "search_get_schema",
			title: "Get a search index's schema",
			description: "The fields an index actually holds and the TYPE each was indexed as. Read this before writing a search_query: " +
				"the query language treats a field differently depending on whether it is text, atom, number, date or geo — an atom only " +
				"matches whole values, a number accepts range operators, and a text field is tokenized — so a query written without knowing " +
				"the types will silently return nothing rather than error. The schema is derived from the documents that have been indexed; " +
				"a field nobody has set does not appear.",
			required: []string{"instance", "index"},
			props: map[string]any{
				"instance":  instanceArg,
				"namespace": nsArg,
				"index":     map[string]any{"type": "string", "description": "Index name (see search_list_indexes)."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/schema",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "index")))
				return h.serveInternal(r, http.MethodGet, path, nil)
			},
		},
		tool{
			name:  "search_get_documents",
			title: "Fetch search documents by id",
			description: "Fetch indexed documents by their exact ids. Use this when you already know the id — it is a direct lookup, so " +
				"unlike search_query it applies no query parsing, no ranking and no synonyms, and an id that does not exist is simply " +
				"absent from the result rather than an error.",
			required: []string{"instance", "index", "ids"},
			props: map[string]any{
				"instance":  instanceArg,
				"namespace": nsArg,
				"index":     map[string]any{"type": "string", "description": "Index name (see search_list_indexes)."},
				"ids":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Document ids. At most %d per call.", maxLimit)},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("search", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/search/%s/ns/%s/idx/%s/documents/get",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "index")))
				return h.serveInternal(r, http.MethodPost, path, map[string]any{"ids": args["ids"]})
			},
		},

		// --- datastore ------------------------------------------------------
		tool{
			name:  "datastore_list_indexes",
			title: "List datastore indexes",
			description: "The secondary indexes defined on a collection. Datastore refuses any query it cannot serve from an index rather " +
				"than silently scanning, so when datastore_query fails with a not-index-served error this is what tells you which indexes " +
				"exist and datastore_create_index is how you add the missing one. The single-field indexes every document gets are implicit " +
				"and are not listed here.",
			required: []string{"instance", "collection"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string", "description": "Collection name."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/indexes",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				return h.serveInternal(r, http.MethodGet, path, nil)
			},
		},
		tool{
			name:  "datastore_create_index",
			title: "Create a datastore index",
			description: "Define a composite index so a query that needs it can be served. FIELD ORDER MATTERS and is not interchangeable: " +
				"list the equality-filtered fields first, then the field the query sorts or ranges on. An index built in the wrong order will " +
				"not serve the query you built it for. Existing documents are backfilled, so this writes one row per document in the " +
				"collection and bills accordingly.",
			required: []string{"instance", "collection", "fields"},
			props: map[string]any{
				"instance":   instanceArg,
				"namespace":  nsArg,
				"collection": map[string]any{"type": "string", "description": "Collection name."},
				"fields":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Field paths, in index order: equality filters first, then the range/sort field."},
				"unique":     map[string]any{"type": "boolean", "description": "Reject documents that duplicate an existing combination. Default false."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("datastore", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/datastore/%s/ns/%s/col/%s/indexes",
					url.PathEscape(in.Name), nsSegment(args), url.PathEscape(argString(args, "collection")))
				body := map[string]any{"fields": args["fields"], "unique": args["unique"] == true}
				return h.serveInternal(r, http.MethodPost, path, body)
			},
		},

		// --- channel --------------------------------------------------------
		tool{
			name:  "channel_presence",
			title: "Who is in a channel",
			description: "The current roster for one channel: which member ids are connected and how many connections each has. Requires " +
				"presence to be ENABLED on the instance — if it is not, the roster is never tracked, so this reports that rather than " +
				"returning an empty list that would read as 'nobody here'. Members appear only if their token carried a presence identity.",
			required: []string{"instance", "channel"},
			props: map[string]any{
				"instance": instanceArg,
				"channel":  map[string]any{"type": "string", "description": "Channel name (see channel_list_rooms)."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("channel", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				path := fmt.Sprintf("/v1/channel/%s/presence?channel=%s",
					url.PathEscape(in.Name), url.QueryEscape(argString(args, "channel")))
				return h.serveInternal(r, http.MethodGet, path, nil)
			},
		},

		// --- container ------------------------------------------------------
		tool{
			name:  "container_sizes",
			title: "What this instance will run",
			description: "The machine sizes available to container_run, plus the limits this instance enforces: which images are allowed, " +
				"the longest timeout it will accept, and how many jobs may run at once. Read this before container_run rather than guessing " +
				"a size or image — an image outside the allowlist is refused, and a timeout above the maximum is refused rather than clamped.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": instanceArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("container", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodGet, "/v1/container/"+url.PathEscape(in.Name)+"/sizes", nil)
			},
		},
	)
}
