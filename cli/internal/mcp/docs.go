package mcp

import (
	"fmt"
	"strings"
)

// Grounding: the syntax a model cannot guess.
//
// altengine's search query language is App Engine's, and its datastore query shape is App
// Engine's too. A model asked to "search for comedies rated over 3" will confidently
// produce Elasticsearch DSL or SQL, get a 400, and try another dialect. These resources are
// the cheapest fix: the client fetches one when it needs it, and it costs nothing on
// requests that do not.
//
// Kept short on purpose — a resource that restates the whole reference burns the context it
// was meant to save. Each is the part that is NOT guessable, and each must stay in step
// with the hosted server's copy: an argument named here that no tool accepts is worse than
// no documentation at all, because it manufactures the failure it was meant to prevent.

const searchQueryDoc = `# altengine search query language

App Engine Search syntax, NOT Lucene/Elasticsearch DSL and NOT SQL. The query is a STRING.

  up                        bare term (searches every text field)
  title:up                  field scope
  genre = "sci fi"          equality; quote values containing spaces
  rating > 3                numeric comparison (also >=, <, <=, =)
  released < 2011-02-28     date comparison (YYYY-MM-DD)
  a AND b / a OR b          booleans; NOT x and -x negate
  comedy drama              adjacent terms are an implicit AND
  genre:(comedy OR drama)   grouping within a field
  "very important"          phrase
  ~running                  stemmed match (also matches "run", "ran")
  distance(loc, geopoint(37.7, -122.4)) < 1000     geo, in metres

Request shape (search_query tool takes these as arguments):

  query                 the string above
  limit / cursor        pagination. Prefer the cursor over a large offset.
  sort                  [{"expr": "rating", "desc": true, "default": 0}]
                        'default' is required: it is used for documents missing the field.
  returned_fields       ["title"] — fetch only what you need
  ids_only              true returns just the matching ids
  facets                ["genre"] — value counts for these facets
  facet_discover        N — return the top N facets
  facet_refinements     [{"name": "genre", "value": "scifi"}]
  total_hits_accuracy   how far to count exactly (default 20, max 10000)

total_hits is a LOWER BOUND when total_hits_exact is false — render it as "N+", and raise
total_hits_accuracy only when an exact figure is actually needed, because counting costs.

FACETS ARE NOT FIELDS. A document carries 'fields' (searchable) and, optionally, 'facets'
(countable) as two separate arrays. Faceting on a value written only as a field returns
nothing — no error, just an empty result, which reads as "there is no such value". Write it
in both places: one makes it findable, the other makes it countable.

    {"id": "1",
     "fields": [{"name": "genre", "type": "atom", "value": "comedy"}],
     "facets": [{"name": "genre", "type": "atom", "value": "comedy"}]}

Field types, set when the field is first written: text, html, atom, number, date, geo,
tokenprefix, untokenprefix. 'atom' matches only in full — use it for ids and tags, 'text'
for prose.`

const datastoreQueryDoc = `# altengine datastore queries

A document store with App Engine Datastore semantics. Documents are {key, data}; 'data' is
arbitrary JSON.

Query shape (the datastore_query tool takes these as arguments, by these exact names):

  where      [{"field": "status", "op": "=", "value": "open"}]
             ops: = != < <= > >= in not-in array-contains
             NOTE: the field is "where", NOT "filters".
  order      [{"field": "created", "dir": "desc"}]
             NOTE: direction is the "dir" key ("asc" or "desc"), not a boolean flag.
             (Search sort is different — there it IS a boolean. Do not carry it over.)
  limit      bounded; use the returned cursor to continue
  cursor     keyset pagination — always prefer this to a large offset
  keys_only  true returns just the matching keys — cheaper when you do not need the data

THE RULE THAT SURPRISES PEOPLE: a query must be INDEX-SERVED. A filter or ordering that no
index covers is REFUSED rather than run as a scan, because a silent full scan is how a cheap
query becomes an expensive one. Single-field indexes are automatic, and instances with
auto-indexing on will create what a query needs on first use (the response reports it under
auto_indexed). Otherwise the refusal carries details.suggested_index, and the index has to
be created in the console — there is no index-management tool over MCP.
A query filtering AND ordering on different fields needs a composite index.

Inequality filters (< <= > >=) may only be applied to ONE field per query, and the first
'order' must be that same field. This is an index-structure constraint, not a policy.

Transactions and multi-collection joins exist but are ORG-KEY ONLY: row-level access rules
scope to a single collection per request, so anything spanning two collections cannot be
rule-checked and is unreachable from a browser identity token. That is the reason the
functions service exists — put that logic in a function.`

const authRulesDoc = `# altengine auth: access levels and row rules

Two INDEPENDENT mechanisms. Getting the relationship wrong is the most common mistake.

1. level ("read" | "write" | "full") is a CEILING on which endpoints a client may call.
   read  -> queries and gets
   write -> ...and writes
   full  -> ...and DELETES
   Deleting requires "full". Granting "write" to an app that deletes fails no matter what
   the rules say — the rules are never even consulted. This exact confusion broke a real
   app and a published example.

2. Rules gate which ROWS a request may touch, per collection:

     {"read": {"match": {"owner": "$uid"}},
      "write": {"match": {"owner": "$uid"}}}

   $uid is the authenticated user's id; $claims.x reads a custom claim. Several match
   objects OR together (disjunctive normal form). Field-level control masks individual
   fields on read and gates them on write.

A rule scopes ONE collection per request. Any path reaching a second collection (a query
join, a transaction) cannot be rule-checked and is therefore refused for identity tokens.`

const functionStarterDoc = `# altengine function: shape and rules

One pre-bundled ES module, default export with a fetch handler:

    export default {
      async fetch(request, env) {
        const body = await request.json();
        await env.datastore.put({ instance: "orders" }, "events", [{ data: body }]);
        return new Response(JSON.stringify({ ok: true }),
          { headers: { "content-type": "application/json" } });
      },
    };

env holds exactly two things: your secrets, and one client per GRANTED service. A service
you did not grant is ABSENT from env — a missing grant shows up as "env.datastore is
undefined", not as a permission error.

  env.datastore.put(target, collection, [{data}]) / .get / .query / .delete / .transaction
  env.search.put(target, index, docs) / .search(target, index, request)
  env.auth / env.channel similarly; target is {instance, namespace?}
  env.tasks.add(t, queue, task | [tasks])  write — task: {payload, name?, delay? | runAt?, target?, params?}
            .get(t, id) / .list(t, queue, {state, cursor, limit, name})            read
            .run(t, id) / .cancel(t, [ids])                                        write
            .lease(t, queue, {count, leaseSeconds}) / .ack(t, id, leaseId, result?)  write (pull queues)
    A push queue whose target is {"function": "<instance>/<name>"} POSTs
    {"task": {id, queue, name, attempt, maxAttempts, params, ...}, "payload": ...} with
    x-ae-trigger: task. Answer 2xx when done; anything else is retried with backoff (a 4xx
    other than 408/429 fails the task at once). Be idempotent on task.id: delivery is
    at-least-once.

NO OUTBOUND NETWORK by default: fetch() throws unless the host is on the instance's
allowlist. There is no filesystem, no process, and no npm resolution at runtime — the
bundle must be self-contained.

Functions run at ORG level: row rules do NOT apply to them. The grants map is the blast
radius, so grant the least you need. This is exactly why functions exist — a transaction
spanning two collections is unreachable from a browser but fine here.

Schedules: five-field cron expressions in UTC, up to 5 per function. Several are allowed
because hour and day-of-week are ANDed within one expression, so "09:00 weekdays and 12:00
Saturday" is genuinely two. A scheduled run arrives as POST with x-ae-trigger: cron and a
body of {"crons": [...], "scheduled_for": <epoch ms>}. A run still going when the next is
due is SKIPPED, not queued, so a function never overlaps itself. Failures are not retried.

URLs: a function is served at https://<slug>-fn.altengine.app/<name>/... and gets the full
path. To serve a whole site from the root, make one function the instance's DEFAULT:
patch_instance_config {"defaultFunction": "<name>"} (null turns it off). It then answers "/"
and every path whose first segment names no other function, and routes the path itself — so
link to "/about", not "/<name>/about". A named function still wins. /api, /admin and /v1
never reach it (404), so put the site's own endpoints under another prefix. Schedules and
completion callbacks never fall back to it. Deleting the function clears the setting.

Locally, ` + "`altengine dev`" + ` ticks the scheduler once a minute and prints each run. A function
is served at /fn/{instance}/{name}/..., a default function at /fn/{instance}/ and below, and
the path it sees keeps the /fn/{instance} prefix.`

const tasksGuideDoc = `# Tasks: queues of work

A tasks instance holds QUEUES. Every instance has ` + "`default`" + `. A task is one JSON payload (≤128 KB)
delivered to one target, retried until it is accepted or its attempts run out.

## Push queues deliver

Set the queue's target once (` + "`tasks_set_queue`" + `, full):

| target | what happens |
|---|---|
| ` + "`{ \"function\": \"<functions-instance>/<fn>\" }`" + ` | POST in process, ` + "`x-ae-trigger: task`" + `, body ` + "`{ task, payload }`" + ` |
| ` + "`{ \"channel\": \"<channel-instance>/<channel>\" }`" + ` | published as ` + "`{ type: \"task\", task, payload }`" + ` (≤32 KB framed) |
| ` + "`{ \"webhook\": \"https://…\" }`" + ` | POST ` + "`{ task, payload }`" + `, signed like every altengine webhook |

A delivery is waited for ` + "`deadlineSeconds`" + ` (queue setting, overridable per task; 15–840, default
600). Past it the attempt counts as failed and is retried. The target is not stopped; its slot
stays taken and the retry waits until it returns, so ` + "`maxConcurrent`" + ` is never exceeded and a task
never runs beside itself. The timed-out attempt records what the target finally answered.

A 2xx (or a published message) is done. A timeout, 408, 429 or 5xx is retried with backoff
(` + "`min × 2^n`" + `, capped). Any other 4xx fails the task at once — a payload the target refuses does
not improve by retrying. A task with no delay runs within about a second.

` + "`task`" + ` carries ` + "`id, queue, name, attempt, maxAttempts, runAt, createdAt, source, depth, params`" + `.
Delivery is AT LEAST ONCE: a handler must be idempotent on ` + "`task.id`" + `.

## Adding

` + "`tasks_add`" + ` / ` + "`POST /v1/tasks/<instance>/queues/<queue>/tasks`" + ` / ` + "`env.tasks.add(t, queue, task)`" + `:
` + "`{ payload, name?, delay? | runAt?, target?, params?, retry? }`" + `, up to 100 per call.
A ` + "`name`" + ` makes an add idempotent: the same name in the same queue is refused (TASK_EXISTS) for
as long as the finished task's record is kept (the instance's retentionDays, default 7).

## Pull queues are leased

` + "`mode: \"pull\"`" + ` never delivers. A worker calls ` + "`tasks_lease`" + ` (count, leaseSeconds), does the work,
and ` + "`tasks_ack`" + `s with the leaseId it was given — or ` + "`tasks_release`" + `s it. A lease that runs out
counts as a failed attempt and the task goes back.

## Triggers point at a queue

Datastore and blob instances have a ` + "`triggers`" + ` config list; a mailbox route can have a
` + "`{ \"queue\": \"<tasks-instance>/<queue>\" }`" + ` action. Each fires a task whose payload is the event:

    datastore: { type: "datastore.write", instance, namespace, collection, events: [{ op: "create"|"update"|"delete", key, doc?, before? }] }
    blob:      { type: "blob.create" | "blob.delete", instance, id, name, size, contentType, etag, public, meta, url? }

Events are best effort. A write never waits for, or fails because of, a trigger. A chain of
triggers more than 8 deep (a function writing the document that triggers it) is cut.

## Watching and fixing

` + "`tasks_list`" + ` by state (scheduled, running, leased, done, failed, cancelled), ` + "`tasks_get`" + ` for the
attempts and the target's answer, ` + "`tasks_run`" + ` to run one now or retry a failed one,
` + "`tasks_cancel`" + `, ` + "`tasks_purge_queue`" + ` (confirm). Pausing a queue stops delivery; nothing is lost.

Locally, ` + "`altengine dev`" + ` checks each queue about once a second and prints every attempt.`

type resource struct {
	uri, name, title, description, text string
}

var resources = []resource{
	{
		uri: "docs://search/query-language", name: "search-query-language", title: "Search query language",
		description: "The search query grammar and request shape. Read before writing any search query — it is App Engine syntax, not Lucene or SQL.",
		text:        searchQueryDoc,
	},
	{
		uri: "docs://datastore/query", name: "datastore-query", title: "Datastore queries and indexes",
		description: "Filters, ordering, cursors, and the index-served rule that refuses unindexed queries.",
		text:        datastoreQueryDoc,
	},
	{
		uri: "docs://auth/rules", name: "auth-rules", title: "Auth levels and row rules",
		description: "How level (an endpoint ceiling) and row rules (which rows) combine. Read before configuring client access.",
		text:        authRulesDoc,
	},
	{
		uri: "docs://functions/starter", name: "functions-starter", title: "Writing a function",
		description: "Handler shape, the env clients, grants, egress rules, schedules, and serving a whole site from the root with a default function.",
		text:        functionStarterDoc,
	},
	{
		uri: "docs://tasks/guide", name: "tasks-guide", title: "Task queues, targets, retries and triggers",
		description: "How a queue delivers (function, channel, webhook), what a target receives and must answer, retries, names as idempotency keys, " +
			"pull queues and leases, and how datastore/blob/mailbox triggers feed a queue. Read before creating a queue or a trigger.",
		text: tasksGuideDoc,
	},
}

func resourceList() []map[string]any {
	out := make([]map[string]any, 0, len(resources))
	for _, r := range resources {
		out = append(out, map[string]any{
			"uri": r.uri, "name": r.name, "title": r.title,
			"description": r.description, "mimeType": "text/markdown",
		})
	}
	return out
}

func readResource(uri string) ([]map[string]any, error) {
	for _, r := range resources {
		if r.uri == uri {
			return []map[string]any{{"uri": r.uri, "mimeType": "text/markdown", "text": r.text}}, nil
		}
	}
	// Listing the alternatives beats a bare "not found": the client can recover without
	// another round trip to resources/list.
	uris := make([]string, 0, len(resources))
	for _, r := range resources {
		uris = append(uris, r.uri)
	}
	return nil, fmt.Errorf("no resource '%s'. Available: %s", uri, strings.Join(uris, ", "))
}

// --- prompts -------------------------------------------------------------

// The two workflows worth encoding, because both have an ordering that is not obvious and
// is expensive to get wrong.

type promptDef struct {
	name, title, description string
	args                     []map[string]any
	build                    func(map[string]string) string
}

func arg(m map[string]string, k, fallback string) string {
	if v, ok := m[k]; ok && v != "" {
		return v
	}
	return fallback
}

var prompts = []promptDef{
	{
		name: "scaffold-backendless-app", title: "Scaffold a backend-less app",
		description: "Plan and create the altengine instances for a static app that talks directly to the services, with per-user access rules.",
		args: []map[string]any{
			{"name": "description", "description": "What the app does, and who its users are.", "required": true},
		},
		build: func(a map[string]string) string {
			return "Set up altengine for this app: " + arg(a, "description", "(no description given)") + `

Work in this order — each step depends on the one before:

1. Call whoami and list_instances to see what already exists. Reuse before creating.
2. Read docs://datastore/query and docs://auth/rules BEFORE designing anything. The
   index-served rule and the fact that 'level' is a ceiling both change the design, and
   neither is guessable.
3. Decide the collections and, for each, which rows a signed-in user may read and write.
   Express that as row rules keyed on $uid rather than filtering in the client — a client
   filter is a suggestion, a rule is enforced.
4. Create the datastore instance and any indexes the queries need. A query no index covers
   is refused, not slow, so design the indexes with the queries and not afterwards.
5. Configure auth: sign-in methods, then the access rules from step 3.
6. Anything that must touch two collections atomically CANNOT be done from the browser —
   rules scope to one collection per request. Put it in a function (docs://functions/starter).

Explain the access model you chose before creating anything, and say which parts of the app
must live in a function and why.`
		},
	},
	{
		name: "add-scheduled-job", title: "Add a scheduled job",
		description: "Write and deploy a function that runs on a cron schedule.",
		args: []map[string]any{
			{"name": "task", "description": "What the job should do.", "required": true},
			{"name": "when", "description": "How often, in plain words (e.g. 'every night at 3am UTC').", "required": false},
		},
		build: func(a map[string]string) string {
			return `Add a scheduled altengine function.

Task: ` + arg(a, "task", "(not given)") + `
Schedule: ` + arg(a, "when", "(not given — ask, and note schedules are UTC)") + `

1. Read docs://functions/starter first — the handler shape, the env clients, and the fact
   that a function has NO outbound network unless the host is allowlisted.
2. Grant the least the job needs. Grants are the blast radius; a function runs at org level
   and row rules do not apply to it.
3. Schedules are five-field cron in UTC. If the timing needs two different weekday/time
   combinations, that is genuinely two expressions — one expression ANDs hour and
   day-of-week, so "weekdays at 9 and Saturday at noon" cannot be written as one.
4. Deploy with functions_deploy, then invoke it once over HTTP to check it works before
   relying on the timer. Read functions_errors afterwards.

Be explicit about what happens if a run fails: it is NOT retried, and a run still going
when the next is due is skipped rather than queued.`
		},
	},
}

func promptList() []map[string]any {
	out := make([]map[string]any, 0, len(prompts))
	for _, p := range prompts {
		out = append(out, map[string]any{
			"name": p.name, "title": p.title, "description": p.description, "arguments": p.args,
		})
	}
	return out
}

func getPrompt(name string, args map[string]string) (map[string]any, error) {
	for _, p := range prompts {
		if p.name == name {
			return map[string]any{
				"description": p.description,
				"messages": []any{map[string]any{
					"role":    "user",
					"content": map[string]any{"type": "text", "text": p.build(args)},
				}},
			}, nil
		}
	}
	names := make([]string, 0, len(prompts))
	for _, p := range prompts {
		names = append(names, p.name)
	}
	return nil, fmt.Errorf("no prompt '%s'. Available: %s", name, strings.Join(names, ", "))
}
