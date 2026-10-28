// Tasks tools: queues of work an agent can add to, watch, run, and work through.
//
// Two flows. An agent wiring an app: create a queue whose target is a function, add tasks, watch
// them run, read why one failed, retry it. And an agent AS the worker: lease from a pull queue,
// do the work, ack it — a to-do list several agents can share without stepping on each other.
//
// Names, arguments and limits match the hosted tools. Every tool is the /v1 call it maps to,
// dispatched in process, so the queue rules are one implementation.

package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/altlimit/altengine/cli/internal/tasks"
)

// tasksModelPayloadMax is where a payload or result handed back to a model is cut; the whole one
// is always in the console and /v1.
const tasksModelPayloadMax = 32 * 1024

var (
	tasksInstanceArg = map[string]any{"type": "string", "description": "Tasks instance name (as shown by list_instances)."}
	tasksQueueArg    = map[string]any{"type": "string", "description": "Queue name. Every instance has 'default'."}
	tasksTargetArg   = map[string]any{
		"type": "object",
		"description": "Where a push queue delivers: exactly one of { function: '<functions-instance>/<function>' }, { channel: '<channel-instance>/<channel>' }, " +
			"{ webhook: 'https://…' } (signed like every altengine webhook).",
		"properties": map[string]any{"function": map[string]any{"type": "string"}, "channel": map[string]any{"type": "string"}, "webhook": map[string]any{"type": "string"}},
	}
)

func (h *Handler) tasksPath(args map[string]any, suffix string) (string, error) {
	in, err := h.instanceOf("tasks", argString(args, "instance"))
	if err != nil {
		return "", err
	}
	return "/v1/tasks/" + url.PathEscape(in.Name) + suffix, nil
}

// taskForModel cuts a task's payload and result for a model's context.
func taskForModel(v any) any {
	t, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if b, err := json.Marshal(t["payload"]); err == nil && len(b) > tasksModelPayloadMax {
		t["payload"] = string(b[:tasksModelPayloadMax])
		t["payloadTruncated"] = true
	}
	if s, ok := t["result"].(string); ok && len(s) > tasksModelPayloadMax {
		t["result"] = s[:tasksModelPayloadMax]
	}
	return t
}

func tasksForModel(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	for i := range list {
		list[i] = taskForModel(list[i])
	}
	return list
}

func init() {
	register(
		tool{
			name:  "tasks_list_queues",
			title: "List queues",
			description: "Every queue in a tasks instance with its settings (mode, target, rate, concurrency, retry policy, dead-letter queue, paused) and how many tasks " +
				"it holds in each state. A count that reached 10,000 is listed under `capped` and means 'at least'.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": tasksInstanceArg},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues")
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodGet, p, nil)
				if err != nil {
					return nil, err
				}
				in, _ := h.instanceOf("tasks", argString(args, "instance"))
				return map[string]any{"instance": in.Name, "queues": res["queues"]}, nil
			},
		},
		tool{
			name:  "tasks_set_queue",
			title: "Create or change a queue",
			description: "Create a queue (create: true) or change one. Only the settings you pass change. mode 'push' delivers each task to `target` (or the task's own); " +
				"mode 'pull' holds tasks for workers to lease. ratePerSecond (0.01–100) and maxConcurrent (1–50) bound delivery; retry is { maxAttempts, minBackoffSeconds, " +
				"maxBackoffSeconds, maxDoublings, maxAgeSeconds }; deadLetterQueue names another queue that receives a copy of every task that fails; paused stops delivery " +
				"without losing tasks; deadlineSeconds (15–840, default 600) is how long a delivery is waited for before the attempt counts as failed and is retried — " +
				"the target is not stopped.",
			required: []string{"instance", "name"},
			props: map[string]any{
				"instance":        tasksInstanceArg,
				"name":            map[string]any{"type": "string", "description": "Queue name: a-z, 0-9, '_' and '-'."},
				"create":          map[string]any{"type": "boolean", "description": "True to create a new queue; omit to change an existing one."},
				"mode":            map[string]any{"type": "string", "enum": []string{"push", "pull"}},
				"target":          tasksTargetArg,
				"ratePerSecond":   map[string]any{"type": "number"},
				"maxConcurrent":   map[string]any{"type": "integer"},
				"retry":           map[string]any{"type": "object"},
				"deadLetterQueue": map[string]any{"type": []string{"string", "null"}},
				"paused":          map[string]any{"type": "boolean"},
				"targetControl":   map[string]any{"type": "boolean"},
				"deadlineSeconds": map[string]any{"type": "integer"},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				patch := map[string]any{}
				for k, v := range args {
					if k != "instance" && k != "name" && k != "create" {
						patch[k] = v
					}
				}
				name := argString(args, "name")
				if args["create"] == true {
					p, err := h.tasksPath(args, "/queues")
					if err != nil {
						return nil, err
					}
					patch["name"] = name
					return h.serveInternal(r, http.MethodPost, p, patch)
				}
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(name))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodPatch, p, patch)
			},
		},
		tool{
			name:        "tasks_delete_queue",
			title:       "Delete a queue",
			description: "Delete a queue and EVERY task in it, in any state. Not recoverable. The 'default' queue cannot be deleted. Requires confirm: true.",
			required:    []string{"instance", "queue", "confirm"},
			props:       map[string]any{"instance": tasksInstanceArg, "queue": tasksQueueArg, "confirm": map[string]any{"type": "boolean"}},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(argString(args, "queue")))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodDelete, p, nil)
			},
		},
		tool{
			name:  "tasks_purge_queue",
			title: "Purge a queue",
			description: "Delete every task in a queue — scheduled, running, finished — keeping the queue and its settings. A running task finishes, and its outcome is " +
				"not recorded. Not recoverable. Requires confirm: true.",
			required:    []string{"instance", "queue", "confirm"},
			props:       map[string]any{"instance": tasksInstanceArg, "queue": tasksQueueArg, "confirm": map[string]any{"type": "boolean"}},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(argString(args, "queue"))+"/purge")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, http.MethodPost, p, nil)
			},
		},
		tool{
			name:  "tasks_add",
			title: "Add tasks",
			description: "Add one task or up to 100 to a queue. Each task: { payload (any JSON, ≤128 KB, required — null for none), name? (makes the add idempotent: a name " +
				"already used in the queue is refused while its record is kept), delay? (seconds) or runAt? (epoch ms or ISO time, ≤30 days ahead), target? (overrides the " +
				"queue's), params? (small object passed alongside), retry?, deadlineSeconds? (15–840, overrides the queue's) }. A push task runs within about a second of " +
				"becoming due.",
			required: []string{"instance", "queue", "tasks"},
			props: map[string]any{
				"instance": tasksInstanceArg,
				"queue":    tasksQueueArg,
				"tasks":    map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "The tasks to add."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(argString(args, "queue"))+"/tasks")
				if err != nil {
					return nil, err
				}
				// Every name already taken is a 409 whose body is still the per-task answer — which is
				// all the tool returns, as hosted.
				res, err := h.serveInternal(r, http.MethodPost, p, args["tasks"], http.StatusConflict)
				if err != nil {
					return nil, err
				}
				return map[string]any{"tasks": res["tasks"]}, nil
			},
		},
		tool{
			name:  "tasks_list",
			title: "List tasks",
			description: fmt.Sprintf("One page of a queue's tasks, without payloads. state: %s, or omit for all (newest first). Scheduled/running/leased come soonest first; "+
				"finished ones latest first. Pass the returned cursor for the next page.", strings.Join(tasks.States, ", ")),
			required: []string{"instance", "queue"},
			props: map[string]any{
				"instance": tasksInstanceArg,
				"queue":    tasksQueueArg,
				"state":    map[string]any{"type": "string", "enum": tasks.States},
				"name":     map[string]any{"type": "string", "description": "Only the task with this name."},
				"cursor":   map[string]any{"type": "string"},
				"limit":    map[string]any{"type": "integer", "description": "1–200. Default 50, max 200."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(argString(args, "queue"))+"/tasks")
				if err != nil {
					return nil, err
				}
				q := url.Values{}
				for _, k := range []string{"state", "name", "cursor"} {
					if v := argString(args, k); v != "" {
						q.Set(k, v)
					}
				}
				if v, ok := args["limit"].(float64); ok {
					q.Set("limit", fmt.Sprint(v))
				}
				return h.serveInternal(r, http.MethodGet, p+"?"+q.Encode(), nil)
			},
		},
		tool{
			name:        "tasks_get",
			title:       "Get a task",
			description: "One task: its payload, params, target, state, every attempt with its status and error, and the target's answer (`result`, up to 16 KB).",
			required:    []string{"instance", "id"},
			props:       map[string]any{"instance": tasksInstanceArg, "id": map[string]any{"type": "string"}},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/tasks/"+url.PathEscape(argString(args, "id")))
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodGet, p, nil)
				if err != nil {
					return nil, err
				}
				return map[string]any{"task": taskForModel(res["task"])}, nil
			},
		},
		tool{
			name:  "tasks_run",
			title: "Run a task now",
			description: "Run a task now. A scheduled task is moved up; a failed or cancelled one goes back to scheduled with its attempts reset (a retry); a finished one " +
				"is run again as a NEW task with the same payload. A running or leased task is refused.",
			required:    []string{"instance", "id"},
			props:       map[string]any{"instance": tasksInstanceArg, "id": map[string]any{"type": "string"}},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/tasks/"+url.PathEscape(argString(args, "id"))+"/run")
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodPost, p, nil)
				if err != nil {
					return nil, err
				}
				return map[string]any{"task": taskForModel(res["task"])}, nil
			},
		},
		tool{
			name:        "tasks_cancel",
			title:       "Cancel tasks",
			description: "Cancel scheduled or leased tasks (up to 200). A running task cannot be stopped; it is reported under `skipped` with its state.",
			required:    []string{"instance", "ids"},
			props: map[string]any{
				"instance": tasksInstanceArg,
				"ids":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/tasks/cancel")
				if err != nil {
					return nil, err
				}
				list, _ := args["ids"].([]any)
				ids := make([]string, 0, len(list))
				for _, x := range list {
					ids = append(ids, fmt.Sprint(x))
				}
				if len(ids) > 200 {
					ids = ids[:200]
				}
				if len(ids) == 0 {
					return nil, fmt.Errorf("ids is required")
				}
				return h.serveInternal(r, http.MethodPost, p, map[string]any{"ids": ids})
			},
		},
		tool{
			name:  "tasks_lease",
			title: "Lease tasks from a pull queue",
			description: "Take up to `count` (1–100) due tasks from a PULL queue for `leaseSeconds` (1–3600, default 300). Each comes with its payload and a leaseId. " +
				"Finish one with tasks_ack, or give it back with tasks_release; a lease that runs out puts the task back with its attempt counted as failed.",
			required: []string{"instance", "queue"},
			props: map[string]any{
				"instance":     tasksInstanceArg,
				"queue":        tasksQueueArg,
				"count":        map[string]any{"type": "integer"},
				"leaseSeconds": map[string]any{"type": "integer"},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/queues/"+url.PathEscape(argString(args, "queue"))+"/lease")
				if err != nil {
					return nil, err
				}
				body := map[string]any{}
				for _, k := range []string{"count", "leaseSeconds"} {
					if v, ok := args[k].(float64); ok {
						body[k] = v
					}
				}
				res, err := h.serveInternal(r, http.MethodPost, p, body)
				if err != nil {
					return nil, err
				}
				return map[string]any{"tasks": tasksForModel(res["tasks"])}, nil
			},
		},
		tool{
			name:  "tasks_ack",
			title: "Finish a leased task",
			description: "Mark a leased task done, with an optional result (a string or JSON, kept up to 16 KB). The leaseId must be the one tasks_lease returned and the " +
				"lease must not have run out.",
			required: []string{"instance", "id", "leaseId"},
			props: map[string]any{
				"instance": tasksInstanceArg,
				"id":       map[string]any{"type": "string"},
				"leaseId":  map[string]any{"type": "string"},
				"result":   map[string]any{},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/tasks/"+url.PathEscape(argString(args, "id"))+"/ack")
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodPost, p, map[string]any{"leaseId": argString(args, "leaseId"), "result": args["result"]})
				if err != nil {
					return nil, err
				}
				return map[string]any{"task": taskForModel(res["task"])}, nil
			},
		},
		tool{
			name:  "tasks_release",
			title: "Give a leased task back",
			description: "Return a leased task to its queue to be leased again after delaySeconds (default 0). The attempt counts as failed, with `error` recorded; on its " +
				"last attempt the task fails instead.",
			required: []string{"instance", "id", "leaseId"},
			props: map[string]any{
				"instance":     tasksInstanceArg,
				"id":           map[string]any{"type": "string"},
				"leaseId":      map[string]any{"type": "string"},
				"delaySeconds": map[string]any{"type": "number"},
				"error":        map[string]any{"type": "string"},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.tasksPath(args, "/tasks/"+url.PathEscape(argString(args, "id"))+"/release")
				if err != nil {
					return nil, err
				}
				body := map[string]any{"leaseId": argString(args, "leaseId")}
				if v, ok := args["delaySeconds"].(float64); ok {
					body["delaySeconds"] = v
				}
				if v, ok := args["error"].(string); ok {
					body["error"] = v
				}
				res, err := h.serveInternal(r, http.MethodPost, p, body)
				if err != nil {
					return nil, err
				}
				return map[string]any{"task": taskForModel(res["task"])}, nil
			},
		},
		tool{
			name:  "tasks_reveal_webhook_secret",
			title: "Reveal the webhook signing secret",
			description: "Return the secret this instance signs webhook deliveries with. Each delivery carries `x-altengine-timestamp: <ms>` and " +
				"`x-altengine-signature: v1=<hex>`, where hex is HMAC-SHA256 keyed with this whole string over `<timestamp>.<raw body>`. Treat it as a credential.",
			required:    []string{"instance"},
			props:       map[string]any{"instance": tasksInstanceArg},
			annotations: map[string]any{"readOnlyHint": true, "idempotentHint": true, "destructiveHint": false},
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("tasks", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodPost, "/admin/tasks/"+url.PathEscape(in.ID)+"/webhook-secret/reveal", nil)
				if err != nil {
					return nil, err
				}
				return map[string]any{"instance": in.Name, "secret": res["secret"]}, nil
			},
		},
		tool{
			name:  "tasks_rotate_webhook_secret",
			title: "Rotate the webhook signing secret",
			description: "Replace this instance's webhook signing secret and return the new one. The next delivery is signed with it and there is no overlap, so " +
				"update every receiver straight away. Requires confirm: true.",
			required: []string{"instance", "confirm"},
			props: map[string]any{
				"instance": tasksInstanceArg,
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true."},
			},
			annotations: map[string]any{"readOnlyHint": false, "idempotentHint": false, "destructiveHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				in, err := h.instanceOf("tasks", argString(args, "instance"))
				if err != nil {
					return nil, err
				}
				res, err := h.serveInternal(r, http.MethodPost, "/admin/tasks/"+url.PathEscape(in.ID)+"/webhook-secret/rotate", nil)
				if err != nil {
					return nil, err
				}
				return map[string]any{"instance": in.Name, "secret": res["secret"], "rotated": true}, nil
			},
		},
	)
}
