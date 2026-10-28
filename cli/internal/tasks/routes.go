package tasks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// The tasks data plane, /v1/tasks/{instance} — same paths, shapes and refusals as hosted:
//
//	GET    /                              the instance and its queues
//	GET    /queues                        every queue, with counts by state
//	POST   /queues                        create a queue                           full
//	GET    /queues/{queue}                one queue
//	PATCH  /queues/{queue}                change a queue's settings                full
//	DELETE /queues/{queue}                delete a queue and its tasks             full
//	POST   /queues/{queue}/pause|resume                                            full
//	POST   /queues/{queue}/purge          delete every task in it                  full
//	POST   /queues/{queue}/tasks          add tasks                                write
//	GET    /queues/{queue}/tasks          list (?state, cursor, limit, name)       read
//	POST   /queues/{queue}/lease          lease tasks from a pull queue            write
//	GET    /tasks/{id}                    one task, with its payload and attempts  read
//	DELETE /tasks/{id}                    cancel a scheduled or leased task        write
//	POST   /tasks/cancel                  cancel many: { ids }                     write
//	POST   /tasks/{id}/run                run now; retry a failed one; re-run a done one   write
//	POST   /tasks/{id}/ack                finish a leased task                     write
//	POST   /tasks/{id}/extend             hold a lease longer                      write
//	POST   /tasks/{id}/release            give a leased task back                  write
//
// plus the console's /admin/tasks routes.
//
// ORG API KEYS ONLY, as hosted: a task runs a function with the FUNCTION's grants, so an app's
// end user must never be able to add one. The emulator resolves no identity tokens here at all.

// MaxAddBodyBytes bounds one add: up to 100 tasks.
const MaxAddBodyBytes = 4 * 1024 * 1024

// Handler serves the tasks planes.
type Handler struct {
	svc  *Service
	reg  *control.Registry
	auth *auth.Store
}

// NewHandler builds the handler.
func NewHandler(reg *control.Registry, a *auth.Store, svc *Service) *Handler {
	return &Handler{svc: svc, reg: reg, auth: a}
}

// Register mounts the data plane and the console's routes.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/tasks/{instance}"
	mux.HandleFunc("GET "+p, common.Wrap(h.info))
	mux.HandleFunc("GET "+p+"/queues", common.Wrap(h.listQueues))
	mux.HandleFunc("POST "+p+"/queues", common.Wrap(h.createQueue))
	mux.HandleFunc("GET "+p+"/queues/{queue}", common.Wrap(h.getQueue))
	mux.HandleFunc("PATCH "+p+"/queues/{queue}", common.Wrap(h.patchQueue))
	mux.HandleFunc("DELETE "+p+"/queues/{queue}", common.Wrap(h.deleteQueue))
	mux.HandleFunc("POST "+p+"/queues/{queue}/pause", common.Wrap(h.pause))
	mux.HandleFunc("POST "+p+"/queues/{queue}/resume", common.Wrap(h.resume))
	mux.HandleFunc("POST "+p+"/queues/{queue}/purge", common.Wrap(h.purge))
	mux.HandleFunc("POST "+p+"/queues/{queue}/tasks", common.Wrap(h.addTasks))
	mux.HandleFunc("GET "+p+"/queues/{queue}/tasks", common.Wrap(h.listTasks))
	mux.HandleFunc("POST "+p+"/queues/{queue}/lease", common.Wrap(h.lease))
	mux.HandleFunc("POST "+p+"/tasks/cancel", common.Wrap(h.cancelMany))
	mux.HandleFunc("GET "+p+"/tasks/{id}", common.Wrap(h.getTask))
	mux.HandleFunc("DELETE "+p+"/tasks/{id}", common.Wrap(h.cancelOne))
	mux.HandleFunc("POST "+p+"/tasks/{id}/run", common.Wrap(h.run))
	mux.HandleFunc("POST "+p+"/tasks/{id}/ack", common.Wrap(h.ack))
	mux.HandleFunc("POST "+p+"/tasks/{id}/extend", common.Wrap(h.extend))
	mux.HandleFunc("POST "+p+"/tasks/{id}/release", common.Wrap(h.release))

	h.registerAdmin(mux)
}

func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, error) {
	in, _, err := h.resolveAs(r, need)
	return in, err
}

// resolveAs is resolve, also returning whether the caller may name a given target.
func (h *Handler) resolveAs(r *http.Request, need auth.Level) (*control.Instance, MayTarget, error) {
	name := r.PathValue("instance")
	if name == "" {
		return nil, nil, common.BadRequest("missing tasks instance")
	}
	id, err := h.auth.Resolve(r)
	if err != nil {
		return nil, nil, err
	}
	if err := auth.Require(id, "tasks", name, need); err != nil {
		return nil, nil, err
	}
	return h.reg.GetOrCreate("tasks", name), mayTarget(r, id), nil
}

// mayTarget: a CHANNEL target is a publish, and publishing to a channel takes a `channel` write
// grant on that instance everywhere else. Without this, a credential holding only `tasks` write
// could publish to every channel by naming one as a target. A function target needs no second
// grant — a function is reachable at its public URL by anyone — and a webhook is the caller's own
// URL. The grants are the caller's: a key's, or a function binding's.
func mayTarget(r *http.Request, id *auth.Identity) MayTarget {
	subject := "API key"
	switch common.Caller(r.Context()) {
	case "function":
		subject = "function binding"
	case "mcp":
		subject = "credential"
	}
	return func(t *Target) error {
		if t == nil || t.Channel == "" {
			return nil
		}
		inst, _ := SplitRef(t.Channel)
		have := auth.EffectiveLevel(r, id, "channel", inst)
		if have >= auth.Write {
			return nil
		}
		held := "has no access"
		if have == auth.Read {
			held = "only has 'read'"
		}
		return common.PermissionDenied(fmt.Sprintf("%s lacks 'write' access to channel '%s' (%s)", subject, inst, held))
	}
}

// readRaw reads a JSON body as raw bytes, bounded. An empty body is null.
func readRaw(r *http.Request, max int) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := common.ReadJSONLimited(r, &raw, max); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("null"), nil
	}
	return raw, nil
}

// readObject reads a JSON object body; an empty or non-object body is an empty object.
func readObject(r *http.Request, max int) (map[string]any, error) {
	var m map[string]any
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(max)+1))
	if err != nil {
		return nil, common.BadRequest("could not read request body")
	}
	if len(data) > max {
		return nil, common.NewError(http.StatusRequestEntityTooLarge, "request body exceeds "+strconv.Itoa(max)+" bytes", "INVALID_ARGUMENT")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, common.BadRequest("invalid JSON body")
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func sourceOf(r *http.Request, fallback string) string {
	if c := common.Caller(r.Context()); c == "function" || c == "mcp" {
		return c
	}
	return fallback
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	queues, err := h.svc.store.ListQueues(in.ID)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"name": in.Name, "retentionDays": retentionDays(in), "queues": queues})
	return nil
}

func (h *Handler) listQueues(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	return writeQueues(w, h.svc, in)
}

func writeQueues(w http.ResponseWriter, svc *Service, in *control.Instance) error {
	queues, err := svc.store.ListQueues(in.ID)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"queues": queues})
	return nil
}

func createQueue(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance, may MayTarget) error {
	body, err := readObject(r, common.MaxJSONBody)
	if err != nil {
		return err
	}
	name := ""
	if v := body["name"]; v != nil {
		name = fmt.Sprint(v)
	}
	delete(body, "name")
	q, err := svc.SaveQueue(in, name, body, true, may)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, map[string]any{"queue": q})
	return nil
}

func (h *Handler) createQueue(w http.ResponseWriter, r *http.Request) error {
	in, may, err := h.resolveAs(r, auth.Full)
	if err != nil {
		return err
	}
	return createQueue(w, r, h.svc, in, may)
}

func getQueue(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance) error {
	name, err := ValidateQueueName(r.PathValue("queue"), "queue")
	if err != nil {
		return err
	}
	q, err := svc.store.GetQueue(in.ID, name)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"queue": q})
	return nil
}

func (h *Handler) getQueue(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	return getQueue(w, r, h.svc, in)
}

func patchQueue(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance, patch any, may MayTarget) error {
	if patch == nil {
		body, err := readObject(r, common.MaxJSONBody)
		if err != nil {
			return err
		}
		patch = body
	}
	q, err := svc.SaveQueue(in, r.PathValue("queue"), patch, false, may)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"queue": q})
	return nil
}

func (h *Handler) patchQueue(w http.ResponseWriter, r *http.Request) error {
	in, may, err := h.resolveAs(r, auth.Full)
	if err != nil {
		return err
	}
	return patchQueue(w, r, h.svc, in, nil, may)
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	return patchQueue(w, r, h.svc, in, map[string]any{"paused": true}, nil)
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	return patchQueue(w, r, h.svc, in, map[string]any{"paused": false}, nil)
}

func deleteQueue(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance) error {
	name, err := ValidateQueueName(r.PathValue("queue"), "queue")
	if err != nil {
		return err
	}
	out, err := svc.store.DeleteQueue(in.ID, name)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, out)
	return nil
}

func (h *Handler) deleteQueue(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	return deleteQueue(w, r, h.svc, in)
}

func purge(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance) error {
	name, err := ValidateQueueName(r.PathValue("queue"), "queue")
	if err != nil {
		return err
	}
	out, err := svc.store.PurgeQueue(in.ID, name)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, out)
	return nil
}

func (h *Handler) purge(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	return purge(w, r, h.svc, in)
}

func addTasks(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance, source string, may MayTarget) error {
	body, err := readRaw(r, MaxAddBodyBytes)
	if err != nil {
		return err
	}
	results, err := svc.AddTasks(in, r.PathValue("queue"), body, source, common.TriggerDepth(r.Context()), may)
	if err != nil {
		return err
	}
	status, out := AddResponse(results)
	common.WriteJSON(w, status, out)
	return nil
}

// AddResponse is an add's answer: every task's result — and, when none was added, a 409 with the
// usual error envelope carrying the first refusal, so a client reading only `error` still learns
// why.
func AddResponse(results []AddResult) (int, map[string]any) {
	out := map[string]any{"tasks": results}
	for _, res := range results {
		if res["ok"] == true {
			return 201, out
		}
	}
	for _, res := range results {
		if res["ok"] == false {
			out["error"] = map[string]any{"code": res["code"], "message": res["message"]}
			break
		}
	}
	return 409, out
}

func (h *Handler) addTasks(w http.ResponseWriter, r *http.Request) error {
	in, may, err := h.resolveAs(r, auth.Write)
	if err != nil {
		return err
	}
	return addTasks(w, r, h.svc, in, sourceOf(r, "api"), may)
}

func listTasks(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance) error {
	q := r.URL.Query()
	queue, err := ValidateQueueName(r.PathValue("queue"), "queue")
	if err != nil {
		return err
	}
	state, err := ParseState(q.Get("state"))
	if err != nil {
		return err
	}
	page, err := svc.store.List(in.ID, queue, ListOptions{
		State: state, Cursor: q.Get("cursor"), Name: q.Get("name"),
		Limit: common.PageLimit(q.Get("limit"), defaultListLimit, MaxListLimit),
	})
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, page)
	return nil
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	return listTasks(w, r, h.svc, in)
}

func (h *Handler) lease(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readObject(r, common.MaxJSONBody)
	if err != nil {
		body = map[string]any{}
	}
	queue, err := ValidateQueueName(r.PathValue("queue"), "queue")
	if err != nil {
		return err
	}
	count, ok := body["count"].(float64)
	if !ok {
		count = 1
	}
	secs, ok := body["leaseSeconds"].(float64)
	if !ok {
		secs = DefaultLeaseSeconds
	}
	tasks, err := h.svc.store.Lease(in.ID, queue, count, secs, h.svc.now())
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"tasks": tasks})
	return nil
}

// IDsOf reads `{ ids: [...] }` or a single `id`.
func IDsOf(body map[string]any) ([]string, error) {
	var ids []any
	if list, ok := body["ids"].([]any); ok {
		ids = list
	} else if body["id"] != nil {
		ids = []any{body["id"]}
	}
	if len(ids) == 0 {
		return nil, common.BadRequest("ids (an array of task ids) is required")
	}
	if len(ids) > 200 {
		return nil, common.BadRequest("at most 200 ids at a time")
	}
	out := make([]string, 0, len(ids))
	for _, v := range ids {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		default:
			b, _ := json.Marshal(x)
			out = append(out, string(b))
		}
	}
	return out, nil
}

func cancelMany(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance) error {
	body, err := readObject(r, common.MaxJSONBody)
	if err != nil {
		return err
	}
	ids, err := IDsOf(body)
	if err != nil {
		return err
	}
	out, err := svc.store.Cancel(in.ID, ids, svc.now(), retentionDays(in))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, out)
	return nil
}

func (h *Handler) cancelMany(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	return cancelMany(w, r, h.svc, in)
}

func getTask(w http.ResponseWriter, r *http.Request, svc *Service, in *control.Instance, id string) error {
	t, err := svc.store.Get(in.ID, id)
	if err != nil {
		return err
	}
	if t == nil {
		return common.NotFound("task not found")
	}
	common.WriteJSON(w, 200, map[string]any{"task": t})
	return nil
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	return getTask(w, r, h.svc, in, r.PathValue("id"))
}

func (h *Handler) cancelOne(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	out, err := h.svc.store.Cancel(in.ID, []string{r.PathValue("id")}, h.svc.now(), retentionDays(in))
	if err != nil {
		return err
	}
	if len(out.Cancelled) == 0 {
		if len(out.Skipped) == 0 || out.Skipped[0].State == nil {
			return common.NotFound("task not found")
		}
		return common.NewError(409, "a "+*out.Skipped[0].State+" task cannot be cancelled", "FAILED_PRECONDITION")
	}
	common.WriteJSON(w, 200, map[string]any{"cancelled": true})
	return nil
}

func runTask(w http.ResponseWriter, svc *Service, in *control.Instance, id string) error {
	t, err := svc.RunTask(in, id)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"task": t})
	return nil
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	return runTask(w, h.svc, in, r.PathValue("id"))
}

func leaseIDOf(body map[string]any) (string, error) {
	s, ok := body["leaseId"].(string)
	if !ok || s == "" {
		return "", common.BadRequest("leaseId (from the lease) is required")
	}
	return s, nil
}

func (h *Handler) ack(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readObject(r, 64*1024)
	if err != nil {
		return err
	}
	leaseID, err := leaseIDOf(body)
	if err != nil {
		return err
	}
	t, err := h.svc.store.Ack(in.ID, r.PathValue("id"), leaseID, ResultText(body["result"]), h.svc.now(), retentionDays(in))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"task": t})
	return nil
}

// ResultText is an ack's result as stored: a string as is, anything else as JSON, null as none.
func ResultText(v any) *string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return &s
	}
	b, _ := json.Marshal(v)
	s := string(b)
	return &s
}

func (h *Handler) extend(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readObject(r, common.MaxJSONBody)
	if err != nil {
		return err
	}
	leaseID, err := leaseIDOf(body)
	if err != nil {
		return err
	}
	secs, ok := body["leaseSeconds"].(float64)
	if !ok {
		secs = DefaultLeaseSeconds
	}
	t, err := h.svc.store.Extend(in.ID, r.PathValue("id"), leaseID, secs, h.svc.now())
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"task": t})
	return nil
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) error {
	in, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readObject(r, common.MaxJSONBody)
	if err != nil {
		return err
	}
	leaseID, err := leaseIDOf(body)
	if err != nil {
		return err
	}
	delay, _ := body["delaySeconds"].(float64)
	var errText *string
	if s, ok := body["error"].(string); ok {
		c, _ := clipBytes(s, 1000)
		errText = &c
	}
	t, err := h.svc.Release(in, r.PathValue("id"), leaseID, delay, errText)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"task": t})
	return nil
}

// --- the console's routes --------------------------------------------------------------------
//
// The console is not a privileged path: every operation goes through the same store and the same
// rules as /v1.

func (h *Handler) registerAdmin(mux *http.ServeMux) {
	a := "/admin/tasks/{id}"
	mux.HandleFunc("GET "+a+"/info", common.Wrap(h.adminInfo))
	mux.HandleFunc("GET "+a+"/queues", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return writeQueues(w, h.svc, in)
	})))
	mux.HandleFunc("POST "+a+"/queues", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		// The console acts for the whole organization: no narrower grant to hold a target to.
		return createQueue(w, r, h.svc, in, nil)
	})))
	mux.HandleFunc("GET "+a+"/queues/{queue}", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return getQueue(w, r, h.svc, in)
	})))
	mux.HandleFunc("PATCH "+a+"/queues/{queue}", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return patchQueue(w, r, h.svc, in, nil, nil)
	})))
	mux.HandleFunc("DELETE "+a+"/queues/{queue}", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return deleteQueue(w, r, h.svc, in)
	})))
	mux.HandleFunc("POST "+a+"/queues/{queue}/pause", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return patchQueue(w, r, h.svc, in, map[string]any{"paused": true}, nil)
	})))
	mux.HandleFunc("POST "+a+"/queues/{queue}/resume", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return patchQueue(w, r, h.svc, in, map[string]any{"paused": false}, nil)
	})))
	mux.HandleFunc("POST "+a+"/queues/{queue}/purge", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return purge(w, r, h.svc, in)
	})))
	mux.HandleFunc("POST "+a+"/queues/{queue}/tasks", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return addTasks(w, r, h.svc, in, "console", nil)
	})))
	mux.HandleFunc("GET "+a+"/queues/{queue}/tasks", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return listTasks(w, r, h.svc, in)
	})))
	mux.HandleFunc("POST "+a+"/tasks/cancel", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return cancelMany(w, r, h.svc, in)
	})))
	mux.HandleFunc("GET "+a+"/tasks/{tid}", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return getTask(w, r, h.svc, in, r.PathValue("tid"))
	})))
	mux.HandleFunc("POST "+a+"/tasks/{tid}/run", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		return runTask(w, h.svc, in, r.PathValue("tid"))
	})))
	mux.HandleFunc("POST "+a+"/webhook-secret/reveal", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		secret, err := h.svc.store.WebhookSecret(in.ID)
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		common.WriteJSON(w, 200, map[string]any{"secret": secret})
		return nil
	})))
	mux.HandleFunc("POST "+a+"/webhook-secret/rotate", common.Wrap(h.admin(func(w http.ResponseWriter, r *http.Request, in *control.Instance) error {
		secret, err := h.svc.store.RotateWebhookSecret(in.ID)
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		common.WriteJSON(w, 200, map[string]any{"secret": secret, "rotated": true})
		return nil
	})))
}

func (h *Handler) adminInst(r *http.Request) (*control.Instance, error) {
	id := r.PathValue("id")
	in := h.reg.GetByID("tasks", id)
	if in == nil {
		in = h.reg.Get("tasks", id)
	}
	if in == nil {
		return nil, common.NotFound("tasks instance not found")
	}
	return in, nil
}

func (h *Handler) admin(fn func(w http.ResponseWriter, r *http.Request, in *control.Instance) error) common.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		in, err := h.adminInst(r)
		if err != nil {
			return err
		}
		return fn(w, r, in)
	}
}

func (h *Handler) adminInfo(w http.ResponseWriter, r *http.Request) error {
	in, err := h.adminInst(r)
	if err != nil {
		return err
	}
	queues, err := h.svc.store.ListQueues(in.ID)
	if err != nil {
		return err
	}
	n, size, err := h.svc.store.Stats(in.ID)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{
		"id": in.ID, "name": in.Name, "created_at": in.CreatedAt, "config": in.Config(),
		"queues": queues, "task_count": n, "index_bytes": size,
	})
	return nil
}
