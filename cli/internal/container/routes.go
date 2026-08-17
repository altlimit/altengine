// The container data plane: /v1/container/{instance}. Same paths, same shapes and same
// refusals as hosted, so app code that starts a job locally starts one there unchanged.

package container

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Handler serves the container data plane.
type Handler struct {
	reg   *control.Registry
	auth  *auth.Store
	store *Store
	// mux is the emulator's own router, used to invoke a completion function in-process —
	// the same technique the functions bindings use, so the callback goes through the real
	// invocation path rather than a second implementation of it.
	mux http.Handler
}

func NewHandler(reg *control.Registry, a *auth.Store, store *Store, mux http.Handler) *Handler {
	h := &Handler{reg: reg, auth: a, store: store, mux: mux}
	store.OnComplete(h.invokeOnComplete)
	return h
}

func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/container/{instance}"
	mux.HandleFunc("POST "+p, common.Wrap(h.launch))
	mux.HandleFunc("GET "+p, common.Wrap(h.list))
	// Before {id}, or "sizes" is read as a job id.
	mux.HandleFunc("GET "+p+"/sizes", common.Wrap(h.sizes))
	mux.HandleFunc("GET "+p+"/{id}", common.Wrap(h.get))
	mux.HandleFunc("GET "+p+"/{id}/logs", common.Wrap(h.logs))
	mux.HandleFunc("POST "+p+"/{id}/cancel", common.Wrap(h.cancel))
}

// resolve authenticates and loads the instance.
//
// ORG API KEYS ONLY, as hosted. There is no identity-token path here and there must not be:
// access rules bound what a user may read and write, and there is no rule language for what a
// user may SPEND. The emulator has no identity wiring for this service at all, which is the
// same answer arrived at by construction.
func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, Config, error) {
	name := r.PathValue("instance")
	id, err := h.auth.Resolve(r)
	if err != nil {
		return nil, Config{}, err
	}
	if err := auth.Require(id, "container", name, need); err != nil {
		return nil, Config{}, err
	}
	inst := h.reg.GetOrCreate("container", name)
	return inst, ParseConfig(inst.Config), nil
}

func (h *Handler) launch(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var req LaunchRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	job, err := h.store.Launch(inst.ID, cfg, req)
	if err != nil {
		return err
	}
	common.WriteJSON(w, http.StatusCreated, map[string]any{"job": job})
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	jobs, cursor := h.store.List(inst.ID, r.URL.Query().Get("status"), limit, before)
	common.WriteJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "cursor": cursor})
	return nil
}

func (h *Handler) sizes(w http.ResponseWriter, r *http.Request) error {
	_, cfg, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, name := range []string{"small", "medium", "large"} {
		s := Sizes[name]
		out = append(out, map[string]any{"name": name, "cpus": s.CPUs, "memory_mb": s.MemoryMB})
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"sizes":          out,
		"allowed_images": cfg.AllowedImages,
		"max_timeout_ms": cfg.MaxTimeoutMS,
		"max_concurrent": cfg.MaxConcurrent,
	})
	return nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	job, ok := h.store.Get(inst.ID, r.PathValue("id"))
	if !ok {
		return common.NotFound("job not found")
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"job": job})
	return nil
}

// logs serves what the job printed. The container's stdout and stderr are captured into a
// bounded per-job buffer; before this they went to /dev/null, so a local `echo` vanished.
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	page, ok := h.store.Logs(inst.ID, r.PathValue("id"), from)
	if !ok {
		return common.NotFound("job not found")
	}
	common.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	job, canceled := h.store.Cancel(inst.ID, cfg, r.PathValue("id"))
	if job == nil {
		return common.NotFound("job not found")
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"job": job, "canceled": canceled})
	return nil
}

// invokeOnComplete calls the instance's completion function, in-process through the
// emulator's own router. Fire-and-forget from the job's point of view: the job is already
// recorded, so a callback that fails cannot lose the result or re-run it.
func (h *Handler) invokeOnComplete(instanceID string, cfg Config, job *Job) {
	if h.mux == nil {
		return
	}
	fnInstance, fnName, ok := splitOnComplete(cfg.OnComplete)
	if !ok {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"job_id":     job.ID,
		"instance":   instanceID,
		"status":     job.Status,
		"exit_code":  job.ExitCode,
		"image":      job.Image,
		"size":       job.Size,
		"started_at": job.StartedAt,
		"ended_at":   job.EndedAt,
		"cost_usd":   job.CostUSD,
		"error":      job.Error,
	})
	req := httptest.NewRequest(http.MethodPost, "/fn/"+fnInstance+"/"+fnName, bytes.NewReader(payload))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer emulator-internal")
	req.Header.Set("user-agent", "altengine-container")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.mux.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
		if rec.Code >= 400 {
			log.Printf("[container] completion function %s returned %d for job %s", cfg.OnComplete, rec.Code, job.ID)
		}
	case <-time.After(30 * time.Second):
		log.Printf("[container] completion function %s timed out for job %s", cfg.OnComplete, job.ID)
	}
}

func splitOnComplete(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' && i > 0 && i < len(s)-1 {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
