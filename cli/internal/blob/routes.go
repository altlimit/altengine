// The blob data plane: /v1/blob/{instance}, plus the two paths that actually carry bytes.
//
// Same paths, same shapes and same refusals as hosted, so app code that uploads a file locally
// uploads one there unchanged. THERE IS NO COMMIT ENDPOINT, here or hosted: the upload is
// confirmed by whoever received the bytes, not by the client reporting on itself.
//
// Two paths outside /v1 carry the bytes, and neither takes an API key — the signed URL IS the
// credential, exactly as an object-storage presigned URL is:
//
//	PUT /_blob/{instance}/{id}?exp&sig   the upload target a minted URL points at
//	GET /_blob/{instance}/{id}?exp&sig   a private download
//	GET /blob/{instance}/{name}/{id}     the public object host
//
// The last one stands in for the `{slug}-blob.<suffix>` hostname hosted. A local emulator has no
// wildcard DNS, so the instance moves from the host into the path — the same trade the functions
// plane already makes with /fn/{instance}/{function}.

package blob

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Handler serves the blob data plane.
type Handler struct {
	reg   *control.Registry
	auth  *auth.Store
	store *Store
	sig   *signer
}

func NewHandler(reg *control.Registry, a *auth.Store, store *Store) *Handler {
	return &Handler{reg: reg, auth: a, store: store, sig: newSigner()}
}

func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/blob/{instance}"
	mux.HandleFunc("POST "+p+"/uploads", common.Wrap(h.mint))
	mux.HandleFunc("GET "+p, common.Wrap(h.list))
	// Before {id}, or "delete" is read as a blob id.
	mux.HandleFunc("POST "+p+"/delete", common.Wrap(h.remove))
	mux.HandleFunc("GET "+p+"/{id}", common.Wrap(h.get))
	mux.HandleFunc("POST "+p+"/{id}/public", common.Wrap(h.setPublic))

	mux.HandleFunc("PUT /_blob/{instance}/{id}", common.Wrap(h.upload))
	mux.HandleFunc("GET /_blob/{instance}/{id}", common.Wrap(h.download))
	// A GET pattern also matches HEAD, and ServeContent answers one correctly — so a HEAD
	// registration here would be redundant, not missing.
	mux.HandleFunc("GET /blob/{instance}/{name}/{id}", common.Wrap(h.servePublic))
}

// Drop forgets an instance's objects — called when the instance is deleted, since nothing else
// would ever remove the bytes.
func (h *Handler) Drop(instanceID string) { h.store.Drop(instanceID) }

func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, Config, error) {
	name := r.PathValue("instance")
	id, err := h.auth.Resolve(r)
	if err != nil {
		return nil, Config{}, err
	}
	if err := auth.Require(id, "blob", name, need); err != nil {
		return nil, Config{}, err
	}
	inst := h.reg.GetOrCreate("blob", name)
	return inst, ParseConfig(inst.Config), nil
}

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

func nameSegment(name string) string {
	if name == "" {
		return "file"
	}
	return url.PathEscape(name)
}

func publicURL(r *http.Request, instanceName, name, id string) string {
	return origin(r) + "/blob/" + url.PathEscape(instanceName) + "/" + nameSegment(name) + "/" + id
}

// recordJSON is the wire shape of one object: its row, plus the two things a caller should never
// have to assemble — the blobkey it stores elsewhere, and the public URL when there is one.
func (h *Handler) recordJSON(r *http.Request, inst *control.Instance, rec *Record) map[string]any {
	var pub any
	if rec.Public {
		pub = publicURL(r, inst.Name, rec.Name, rec.ID)
	}
	return map[string]any{
		"id": rec.ID, "name": rec.Name, "size": rec.Size, "content_type": rec.ContentType,
		"etag": rec.ETag, "status": rec.Status, "public": rec.Public,
		"created": rec.Created, "updated": rec.Updated, "meta": rec.Meta,
		"blobkey": "blob:" + inst.ID + ":" + rec.ID,
		"url":     pub,
	}
}

// --- POST /uploads : mint an upload URL -----------------------------------

func (h *Handler) mint(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var req MintRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	rec, err := h.store.Reserve(inst.ID, cfg, req)
	if err != nil {
		return err
	}
	q, exp := h.sig.query(http.MethodPut, inst.ID, rec.ID, UploadTTL)
	common.WriteJSON(w, http.StatusCreated, map[string]any{
		"blobkey":    "blob:" + inst.ID + ":" + rec.ID,
		"id":         rec.ID,
		"upload_url": origin(r) + "/_blob/" + inst.ID + "/" + rec.ID + "?" + q,
		"expires_at": exp,
		// Spelled out because a mismatch is rejected, and hosted the rejection comes back from
		// object storage as a signature error that reads like a clock problem.
		"required_headers": map[string]string{
			"content-length": strconv.FormatInt(rec.Size, 10),
			"content-type":   rec.ContentType,
		},
		"method": "PUT",
	})
	return nil
}

// --- GET / : list ---------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, cursor := h.store.List(inst.ID, r.URL.Query().Get("prefix"), limit, r.URL.Query().Get("cursor"))
	out := make([]map[string]any, 0, len(rows))
	for _, rec := range rows {
		out = append(out, h.recordJSON(r, inst, rec))
	}
	var next any
	if cursor != "" {
		next = cursor
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"blobs": out, "cursor": next})
	return nil
}

// --- GET /{id} : metadata + a short-lived download URL --------------------

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	rec, ok := h.store.Get(inst.ID, r.PathValue("id"))
	// A pending row is not an object. Hosted this is where a read that outran the storage event
	// repairs itself; locally the bytes and the row are promoted by the same call, so pending
	// can only mean the upload never happened — and the honest answer for that is "not found".
	if !ok || rec.Status != "ready" {
		return common.NotFound("blob not found")
	}
	q, exp := h.sig.query(http.MethodGet, inst.ID, rec.ID, DownloadTTL)
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"blob":         h.recordJSON(r, inst, rec),
		"download_url": origin(r) + "/_blob/" + inst.ID + "/" + rec.ID + "?" + q,
		"expires_at":   exp,
	})
	return nil
}

// --- POST /{id}/public : publish or unpublish -----------------------------

func (h *Handler) setPublic(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		Public *bool `json:"public"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	public := body.Public == nil || *body.Public
	rec, ok := h.store.SetPublic(inst.ID, r.PathValue("id"), public)
	if !ok {
		return common.NotFound("blob not found")
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{"blob": h.recordJSON(r, inst, rec)})
	return nil
}

// --- POST /delete ---------------------------------------------------------

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if len(body.IDs) == 0 {
		return common.BadRequest("ids must be a non-empty array")
	}
	if len(body.IDs) > MaxDeleteIDs {
		return common.BadRequest("at most 200 ids per call")
	}
	removed := h.store.Remove(inst.ID, body.IDs)
	common.WriteJSON(w, http.StatusOK, map[string]any{"deleted": len(removed)})
	return nil
}

// --- PUT /_blob/{instance}/{id} : the bytes -------------------------------

// upload receives what a minted URL was minted for.
//
// The size and content type are enforced against the pending row, which is what object storage
// does with the values signed into a real presigned URL. That check is the whole reason the size
// is required at mint time: without it, whoever holds a URL minted for 1 MB could store 5 GB we
// then pay to keep every month. Catching it locally is the point — the same PUT fails hosted.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) error {
	instanceID, id := r.PathValue("instance"), r.PathValue("id")
	if err := h.sig.verify(http.MethodPut, instanceID, id, r.URL.Query()); err != nil {
		return err
	}
	rec, ok := h.store.Pending(instanceID, id)
	if !ok {
		// Either the reservation never existed, or these bytes already arrived. Neither may
		// overwrite an object: a URL is good for one upload, and a second one would change bytes
		// a public URL is already serving under an `immutable` cache header.
		return common.NotFound("this upload URL has already been used, or its reservation is gone")
	}
	if ct := r.Header.Get("Content-Type"); ct != rec.ContentType {
		return common.BadRequest("content-type must be '" + rec.ContentType + "' — it is part of what was signed")
	}
	if r.ContentLength != rec.Size {
		return common.BadRequest("content-length must be " + strconv.FormatInt(rec.Size, 10) +
			" — it is part of what was signed")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, rec.Size+1))
	if err != nil {
		return common.BadRequest("could not read the request body")
	}
	if int64(len(body)) != rec.Size {
		return common.BadRequest("the body was not the length it declared")
	}
	stored, err := h.store.Commit(instanceID, id, body)
	if err != nil {
		return err
	}
	// Object storage answers a PUT with an empty 200 and the digest it computed.
	w.Header().Set("ETag", `"`+stored.ETag+`"`)
	w.WriteHeader(http.StatusOK)
	return nil
}

// --- GET /_blob/{instance}/{id} : a private download ----------------------

func (h *Handler) download(w http.ResponseWriter, r *http.Request) error {
	instanceID, id := r.PathValue("instance"), r.PathValue("id")
	if err := h.sig.verify(http.MethodGet, instanceID, id, r.URL.Query()); err != nil {
		return err
	}
	rec, ok := h.store.Get(instanceID, id)
	if !ok || rec.Status != "ready" {
		return common.NotFound("blob not found")
	}
	body, ok := h.store.Bytes(instanceID, id)
	if !ok {
		return common.NotFound("blob not found")
	}
	h.serveBytes(w, r, rec, body)
	return nil
}

// --- GET /blob/{instance}/{name}/{id} : the public object host ------------

// servePublic is the local stand-in for the public hostname.
//
// NO CREDENTIAL REACHES IT, so a non-public object must 404 exactly like a missing one — a 403
// would confirm the id exists. The name in the path is cosmetic; the id is the lookup key, and a
// request naming the object differently is redirected rather than given a second live URL for
// the same bytes.
func (h *Handler) servePublic(w http.ResponseWriter, r *http.Request) error {
	inst := h.reg.Get("blob", r.PathValue("instance"))
	if inst == nil {
		return notFoundPlain(w)
	}
	rec, ok := h.store.Get(inst.ID, r.PathValue("id"))
	if !ok || !rec.Public || rec.Status != "ready" {
		return notFoundPlain(w)
	}
	if canonical := nameSegment(rec.Name); r.PathValue("name") != canonical {
		http.Redirect(w, r, "/blob/"+url.PathEscape(inst.Name)+"/"+canonical+"/"+rec.ID, http.StatusMovedPermanently)
		return nil
	}
	body, ok := h.store.Bytes(inst.ID, rec.ID)
	if !ok {
		return notFoundPlain(w)
	}
	// public means public; no credentials ride along.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	h.serveBytes(w, r, rec, body)
	return nil
}

// serveBytes writes an object with the headers hosted sets on it. ServeContent handles range
// requests, conditional requests and HEAD, so media seeking works the same way locally.
func (h *Handler) serveBytes(w http.ResponseWriter, r *http.Request, rec *Record, body []byte) {
	head := w.Header()
	head.Set("Content-Type", rec.ContentType)
	head.Set("ETag", `"`+rec.ETag+`"`)
	// A host that serves files someone else uploaded must never be able to run script in the
	// platform's name, and must never be sniffed into something executable.
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if rec.Name != "" {
		head.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(rec.Name))
	}
	// Deliberately NOT `immutable` locally. Hosted that TTL is what makes the public host nearly
	// free; here it would only mean a file you just replaced keeps showing the old bytes in your
	// browser, which is a debugging trap rather than a saving.
	head.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, rec.Name, time.UnixMilli(rec.Updated), bytes.NewReader(body))
}

func notFoundPlain(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "Not found")
	return nil
}
