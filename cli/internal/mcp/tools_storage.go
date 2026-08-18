// Blob and container tools.
//
// THE CONTRACT IS THE NAMES. Every tool here matches the hosted one in name, required
// arguments and argument spelling, because a sequence of calls that works against the emulator
// has to work against production unchanged — and a model does not re-read the schema when a
// call fails, it guesses. `props` is enforced (an unknown argument is refused, not ignored) for
// the same reason.
//
// Two tools carry bytes and are bounded hard for it: `blob_put` writes a small object inline,
// and `blob_upload_url` exists so everything larger never travels through a model's context at
// all.

package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
)

// maxInlineBytes bounds `blob_put`. Matches hosted: this is the one blob path where bytes cross
// the server, and it is also a bound on how much a model can be talked into writing in one call.
const maxInlineBytes = 1024 * 1024

// serveRaw is serveInternal for a body that is NOT JSON — the upload PUT, whose content type is
// signed into the URL and must be sent exactly as minted.
func (h *Handler) serveRaw(r *http.Request, method, path, contentType string, payload []byte) error {
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = "Bearer emulator-internal"
	}
	req.Header.Set("Authorization", auth)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		return fmt.Errorf("%s", errMessage(decoded, rec.Code))
	}
	return nil
}

// blobPath builds a path under an instance, resolving the name first so a typo fails as
// "no such instance" rather than as a 404 from somewhere deeper.
func (h *Handler) blobPath(args map[string]any, suffix string) (string, error) {
	in, err := h.instanceOf("blob", argString(args, "instance"))
	if err != nil {
		return "", err
	}
	return "/v1/blob/" + url.PathEscape(in.Name) + suffix, nil
}

func (h *Handler) containerPath(args map[string]any, suffix string) (string, error) {
	in, err := h.instanceOf("container", argString(args, "instance"))
	if err != nil {
		return "", err
	}
	return "/v1/container/" + url.PathEscape(in.Name) + suffix, nil
}

func init() {
	register(
		tool{
			name:        "blob_list",
			title:       "List objects",
			description: "List stored objects newest-first: id, name, size, content type, whether it is public, and its public URL when it is. Results are capped; use the returned cursor for more.",
			required:    []string{"instance"},
			props: map[string]any{
				"instance": instanceArg,
				"prefix":   map[string]any{"type": "string", "description": "Only objects whose name starts with this."},
				"limit":    map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
				"cursor":   map[string]any{"type": "string", "description": "From a previous response, to fetch the next page."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				q := url.Values{}
				q.Set("limit", fmt.Sprint(boundedLimit(args)))
				if v := argString(args, "prefix"); v != "" {
					q.Set("prefix", v)
				}
				if v := argString(args, "cursor"); v != "" {
					q.Set("cursor", v)
				}
				p, err := h.blobPath(args, "?"+q.Encode())
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", p, nil)
			},
		},
		tool{
			name:        "blob_get",
			title:       "Read an object's metadata",
			description: "An object's metadata plus a short-lived download URL. The URL expires in minutes and is a bearer capability — anyone holding it can read the object until it does, so do not store it.",
			required:    []string{"instance", "id"},
			props: map[string]any{
				"instance": instanceArg,
				"id":       map[string]any{"type": "string", "description": "Blob id (from blob_list)."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.blobPath(args, "/"+url.PathEscape(argString(args, "id")))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", p, nil)
			},
		},
		tool{
			name:  "blob_put",
			title: "Store a small object",
			description: fmt.Sprintf("Store a small object directly — text, or base64 for binary. At most %d bytes; use blob_upload_url for anything larger or for a file you are not holding. Returns the object, with its public URL if it is public.", maxInlineBytes),
			required: []string{"instance", "name", "content"},
			props: map[string]any{
				"instance":     instanceArg,
				"name":         map[string]any{"type": "string", "description": "Filename, e.g. 'logo.svg'. Appears in the public URL."},
				"content":      map[string]any{"type": "string", "description": "The bytes: UTF-8 text, or base64 when encoding is 'base64'."},
				"encoding":     map[string]any{"type": "string", "enum": []string{"utf8", "base64"}, "description": "Default utf8."},
				"content_type": map[string]any{"type": "string", "description": "e.g. 'image/svg+xml'. Defaults by extension, else text/plain."},
				"public":       map[string]any{"type": "boolean", "description": "Serve it from the instance's public URL. Defaults to the instance setting."},
			},
			annotations: writes,
			// Hosted, this writes through the storage binding — there is no REST endpoint for it,
			// and there must not be one here either: an endpoint the emulator has and production
			// lacks is a route that works locally and 404s on deploy. So it is composed from the
			// calls a REST client would actually make.
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				content := argString(args, "content")
				var raw []byte
				if argString(args, "encoding") == "base64" {
					b, err := base64.StdEncoding.DecodeString(content)
					if err != nil {
						return nil, fmt.Errorf("content is not valid base64")
					}
					raw = b
				} else {
					raw = []byte(content)
				}
				if len(raw) > maxInlineBytes {
					return nil, fmt.Errorf("content is %d bytes, over the %d-byte inline limit — use blob_upload_url instead", len(raw), maxInlineBytes)
				}

				mint := map[string]any{"name": argString(args, "name"), "size": len(raw)}
				if v := argString(args, "content_type"); v != "" {
					mint["content_type"] = v
				}
				if v, ok := args["public"]; ok {
					mint["public"] = v
				}
				p, err := h.blobPath(args, "/uploads")
				if err != nil {
					return nil, err
				}
				minted, err := h.serveInternal(r, "POST", p, mint)
				if err != nil {
					return nil, err
				}
				uploadURL, _ := minted["upload_url"].(string)
				headers, _ := minted["required_headers"].(map[string]any)
				ct, _ := headers["content-type"].(string)
				u, err := url.Parse(uploadURL)
				if err != nil || uploadURL == "" {
					return nil, fmt.Errorf("the upload URL was unreadable")
				}
				if err := h.serveRaw(r, "PUT", u.RequestURI(), ct, raw); err != nil {
					return nil, err
				}
				// Read it back so the caller gets the finished record — size and digest measured
				// by whoever received the bytes, not taken from what we sent.
				get, err := h.blobPath(args, "/"+url.PathEscape(fmt.Sprint(minted["id"])))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", get, nil)
			},
		},
		tool{
			name:        "blob_upload_url",
			title:       "Mint an upload URL",
			description: "Get a presigned URL the client PUTs bytes to directly — the bytes never pass through altengine or through you. The exact size and content type are signed into the URL and must match. Nothing else is needed: storage reports the upload, so the object becomes readable on its own.",
			required:    []string{"instance", "name", "size"},
			props: map[string]any{
				"instance":     instanceArg,
				"name":         map[string]any{"type": "string", "description": "Filename. Appears in the public URL."},
				"size":         map[string]any{"type": "number", "description": "The EXACT byte length that will be uploaded."},
				"content_type": map[string]any{"type": "string", "description": "Defaults by extension."},
				"public":       map[string]any{"type": "boolean", "description": "Defaults to the instance setting."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				body := map[string]any{"name": argString(args, "name")}
				for _, k := range []string{"size", "public"} {
					if v, ok := args[k]; ok {
						body[k] = v
					}
				}
				if v := argString(args, "content_type"); v != "" {
					body["content_type"] = v
				}
				p, err := h.blobPath(args, "/uploads")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "POST", p, body)
			},
		},
		tool{
			name:        "blob_set_public",
			title:       "Publish or unpublish an object",
			description: "Make an object world-readable at its public URL, or take it back. Unpublishing clears the edge copies too, but a URL that has been public should be treated as having been seen.",
			required:    []string{"instance", "id"},
			props: map[string]any{
				"instance": instanceArg,
				"id":       map[string]any{"type": "string", "description": "Blob id."},
				"public":   map[string]any{"type": "boolean", "description": "Defaults to true."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				pub := true
				if v, ok := args["public"].(bool); ok {
					pub = v
				}
				p, err := h.blobPath(args, "/"+url.PathEscape(argString(args, "id"))+"/public")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "POST", p, map[string]any{"public": pub})
			},
		},
		tool{
			name:        "blob_delete",
			title:       "Delete objects",
			description: "Permanently delete objects by id. The bytes and the record both go, and any public URL stops working.",
			required:    []string{"instance", "ids", "confirm"},
			props: map[string]any{
				"instance": instanceArg,
				"ids":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Blob ids to delete."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. This cannot be undone."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.blobPath(args, "/delete")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "POST", p, map[string]any{"ids": args["ids"]})
			},
		},

		// --- containers ---------------------------------------------------
		tool{
			name:        "container_run",
			title:       "Start a container job",
			description: "Run a Docker image as a background job. Returns as soon as the machine starts — there is nothing to await. Keep the job id and poll container_get_job, or configure a completion function on the instance. The image must be in the instance's allowed list, which starts EMPTY.",
			required:    []string{"instance", "image"},
			props: map[string]any{
				"instance":   instanceArg,
				"image":      map[string]any{"type": "string", "description": "Docker image reference, e.g. 'alpine:3'."},
				"size":       map[string]any{"type": "string", "enum": []string{"small", "medium", "large"}, "description": "Default small."},
				"cmd":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Overrides the image's command."},
				"env":        map[string]any{"type": "object", "description": "Environment variables. Names beginning AE_ or FLY_ are refused."},
				"timeout_ms": map[string]any{"type": "number", "description": "Kill the job after this long. Capped by the instance's maximum."},
			},
			annotations: writes,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				body := map[string]any{"image": argString(args, "image")}
				for _, k := range []string{"size", "cmd", "env", "timeout_ms"} {
					if v, ok := args[k]; ok {
						body[k] = v
					}
				}
				p, err := h.containerPath(args, "")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "POST", p, body)
			},
		},
		tool{
			name:        "container_list_jobs",
			title:       "List container jobs",
			description: "Jobs for this instance, newest first: status, exit code, how long each ran and what it cost. Filter with status.",
			required:    []string{"instance"},
			props: map[string]any{
				"instance": instanceArg,
				"status":   map[string]any{"type": "string", "enum": []string{"running", "done", "failed", "canceled"}},
				"limit":    map[string]any{"type": "number", "description": fmt.Sprintf("Default %d, max %d.", defaultLimit, maxLimit)},
				"before":   map[string]any{"type": "number", "description": "Cursor from a previous response."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				q := url.Values{}
				q.Set("limit", fmt.Sprint(boundedLimit(args)))
				if v := argString(args, "status"); v != "" {
					q.Set("status", v)
				}
				if v, ok := args["before"].(float64); ok {
					q.Set("before", fmt.Sprintf("%d", int64(v)))
				}
				p, err := h.containerPath(args, "?"+q.Encode())
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", p, nil)
			},
		},
		tool{
			name:        "container_get_job",
			title:       "Read one job",
			description: "Everything recorded about one job: whether it is still running, how it ended, its exit code, how long it ran and what it cost. This is the record — it does not depend on the log service being reachable.",
			required:    []string{"instance", "job_id"},
			props: map[string]any{
				"instance": instanceArg,
				"job_id":   map[string]any{"type": "string", "description": "Job id from container_run or container_list_jobs."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.containerPath(args, "/"+url.PathEscape(argString(args, "job_id")))
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", p, nil)
			},
		},
		tool{
			name:        "container_job_logs",
			title:       "Read a job's output",
			description: "What the job printed, oldest first, with a cursor for more. Best effort: if the output is unavailable this returns an empty page with a note rather than failing, because a job's record does not depend on it.",
			required:    []string{"instance", "job_id"},
			props: map[string]any{
				"instance": instanceArg,
				"job_id":   map[string]any{"type": "string", "description": "Job id."},
				"cursor":   map[string]any{"type": "string", "description": "From a previous response, to continue."},
			},
			annotations: readOnly,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				q := url.Values{}
				if v := argString(args, "cursor"); v != "" {
					q.Set("cursor", v)
				}
				p, err := h.containerPath(args, "/"+url.PathEscape(argString(args, "job_id"))+"/logs?"+q.Encode())
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "GET", p, nil)
			},
		},
		tool{
			name:        "container_cancel_job",
			title:       "Cancel a running job",
			description: "Stop a running job and destroy its machine now. Anything it had not finished writing is lost, and it is billed for the time it ran.",
			required:    []string{"instance", "job_id", "confirm"},
			props: map[string]any{
				"instance": instanceArg,
				"job_id":   map[string]any{"type": "string", "description": "Job id."},
				"confirm":  map[string]any{"type": "boolean", "description": "Must be true. Work in progress is lost."},
			},
			annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true},
			confirmable: true,
			run: func(h *Handler, r *http.Request, args map[string]any) (any, error) {
				p, err := h.containerPath(args, "/"+url.PathEscape(argString(args, "job_id"))+"/cancel")
				if err != nil {
					return nil, err
				}
				return h.serveInternal(r, "POST", p, nil)
			},
		},
	)
}
