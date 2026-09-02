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
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
	mux.HandleFunc("POST "+p+"/uploads/multipart", common.Wrap(h.beginMultipart))
	mux.HandleFunc("POST "+p+"/uploads/multipart/urls", common.Wrap(h.multipartURLs))
	mux.HandleFunc("GET "+p, common.Wrap(h.list))
	// Before {id}, or "delete" is read as a blob id.
	mux.HandleFunc("POST "+p+"/delete", common.Wrap(h.remove))
	mux.HandleFunc("GET "+p+"/{id}", common.Wrap(h.get))
	mux.HandleFunc("POST "+p+"/{id}/public", common.Wrap(h.setPublic))

	mux.HandleFunc("PUT /_blob/{instance}/{id}", common.Wrap(h.upload))
	mux.HandleFunc("GET /_blob/{instance}/{id}", common.Wrap(h.download))
	// Multipart puts its whole vocabulary in the query string rather than the path, so create and
	// complete are one route told apart by what it was asked for, and abort is the DELETE.
	mux.HandleFunc("POST /_blob/{instance}/{id}", common.Wrap(h.objectPost))
	mux.HandleFunc("DELETE /_blob/{instance}/{id}", common.Wrap(h.abortUpload))
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
	q, exp := h.sig.query(http.MethodPut, inst.ID, rec.ID, UploadTTL, nil)
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

// --- multipart : an object bigger than one request can carry ---------------
//
// TWO ROUTES RATHER THAN FOUR, because the upload id does not exist until storage has been asked
// for one: `multipart` signs the create call, and everything after it — the parts, the completion
// and the abort — comes from `multipart/urls` once the client knows the id.
//
// There is no commit endpoint here either. Completing the upload is what stores the object, and
// the row is promoted by the side that received the bytes, exactly as a single PUT is.

func (h *Handler) beginMultipart(w http.ResponseWriter, r *http.Request) error {
	inst, cfg, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var req MintRequest
	if err := common.ReadJSON(r, &req); err != nil {
		return err
	}
	rec, partSize, parts, err := h.store.BeginMultipart(inst.ID, cfg, req)
	if err != nil {
		return err
	}
	q, exp := h.sig.query(http.MethodPost, inst.ID, rec.ID, MultipartTTL, url.Values{"uploads": {""}})
	common.WriteJSON(w, http.StatusCreated, map[string]any{
		"key":       inst.ID + "/" + rec.ID,
		"id":        rec.ID,
		"blobkey":   "blob:" + inst.ID + ":" + rec.ID,
		"part_size": partSize,
		"parts":     parts,
		// POST here with an empty body; the answer is XML naming the UploadId.
		"create_url": origin(r) + "/_blob/" + inst.ID + "/" + rec.ID + "?" + q,
		"expires_at": exp,
	})
	return nil
}

// multipartURLs signs a window of part URLs, plus the two that end the upload either way.
//
// Complete and abort come back with EVERY window rather than on request, because the moment a
// client most needs the abort URL is the moment it has just failed.
func (h *Handler) multipartURLs(w http.ResponseWriter, r *http.Request) error {
	inst, _, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	var body struct {
		ID       string `json:"id"`
		UploadID string `json:"upload_id"`
		From     *int   `json:"from"`
		Count    *int   `json:"count"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	uploadID, err := checkUploadID(body.UploadID)
	if err != nil {
		return err
	}
	// Bounded to a reservation in THIS instance, so a signature is never minted for an object
	// nobody reserved.
	if _, ok := h.store.Get(inst.ID, body.ID); !ok {
		return common.NotFound("blob not found")
	}

	first := clampInt(body.From, 1, MaxParts, 1)
	n := clampInt(body.Count, 1, MaxPartURLsPerRequest, MaxPartURLsPerRequest)
	if n > MaxParts-first+1 {
		n = MaxParts - first + 1
	}
	base := origin(r) + "/_blob/" + inst.ID + "/" + body.ID + "?"
	urls := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		part := first + i
		q, _ := h.sig.query(http.MethodPut, inst.ID, body.ID, MultipartTTL, url.Values{
			"partNumber": {strconv.Itoa(part)},
			"uploadId":   {uploadID},
		})
		urls = append(urls, map[string]any{"part_number": part, "url": base + q})
	}
	done, exp := h.sig.query(http.MethodPost, inst.ID, body.ID, MultipartTTL, url.Values{"uploadId": {uploadID}})
	abort, _ := h.sig.query(http.MethodDelete, inst.ID, body.ID, MultipartTTL, url.Values{"uploadId": {uploadID}})
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"part_urls": urls,
		// POST the CompleteMultipartUpload XML here once every part has an ETag.
		"complete_url": base + done,
		// DELETE here on any failure. Parts of an upload nobody completed still take up room.
		"abort_url":  base + abort,
		"expires_at": exp,
	})
	return nil
}

// clampInt reads a caller-supplied bound. A missing value is a different answer from an
// out-of-range one: defaulting a missing `count` to the minimum would hand back one part URL
// instead of a hundred — an upload that still works and takes a hundred times as many round
// trips to do it.
func clampInt(v *int, lo, hi, fallback int) int {
	if v == nil {
		return fallback
	}
	if *v < lo {
		return lo
	}
	if *v > hi {
		return hi
	}
	return *v
}

// checkUploadID refuses an id that could change which object the signed URLs point at. It is
// echoed into a signed query parameter, so it is input like any other.
func checkUploadID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 512 || !uploadIDOK.MatchString(s) {
		return "", common.BadRequest("upload_id is missing or malformed")
	}
	return s, nil
}

var uploadIDOK = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

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
	q, exp := h.sig.query(http.MethodGet, inst.ID, rec.ID, DownloadTTL, nil)
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
	// A part is selected by the query string, so an ordinary PUT is what is left once no
	// multipart parameter matched.
	if q := r.URL.Query(); q.Get("uploadId") != "" && q.Get("partNumber") != "" {
		return h.uploadPart(w, r, instanceID, id, q)
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

// --- the multipart verbs on the object itself -----------------------------
//
// These stand in for what object storage answers on a presigned URL, in the same S3 shapes: the
// create and complete calls speak XML, and their vocabulary is in the query string rather than
// the path. A client written against hosted talks to these unchanged.

// uploadPart takes one part. No length is bound into a part URL — hosted does not sign one
// either, because only the finished object's size was ever declared.
func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, instanceID, id string, q url.Values) error {
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		return common.BadRequest("partNumber must be a number")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return common.BadRequest("could not read the request body")
	}
	etag, err := h.store.PutPart(instanceID, id, q.Get("uploadId"), n, body)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
	return nil
}

// objectPost is both ends of a multipart upload: `?uploads` creates one, `?uploadId` completes it.
func (h *Handler) objectPost(w http.ResponseWriter, r *http.Request) error {
	instanceID, id := r.PathValue("instance"), r.PathValue("id")
	if err := h.sig.verify(http.MethodPost, instanceID, id, r.URL.Query()); err != nil {
		return err
	}
	q := r.URL.Query()
	if q.Has("uploads") {
		uploadID, err := h.store.CreateUpload(instanceID, id)
		if err != nil {
			return err
		}
		return writeXML(w, "<InitiateMultipartUploadResult><Key>"+xmlEscape(id)+
			"</Key><UploadId>"+xmlEscape(uploadID)+"</UploadId></InitiateMultipartUploadResult>")
	}
	uploadID := q.Get("uploadId")
	if uploadID == "" {
		return common.BadRequest("this URL names no multipart upload")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return common.BadRequest("could not read the request body")
	}
	parts := xmlPartNumbers(string(raw))
	if len(parts) == 0 {
		return common.BadRequest("no parts named")
	}
	rec, err := h.store.CompleteUpload(instanceID, id, uploadID, parts)
	if err != nil {
		return err
	}
	return writeXML(w, "<CompleteMultipartUploadResult><Key>"+xmlEscape(id)+
		"</Key><ETag>&quot;"+xmlEscape(rec.ETag)+"&quot;</ETag></CompleteMultipartUploadResult>")
}

func (h *Handler) abortUpload(w http.ResponseWriter, r *http.Request) error {
	instanceID, id := r.PathValue("instance"), r.PathValue("id")
	if err := h.sig.verify(http.MethodDelete, instanceID, id, r.URL.Query()); err != nil {
		return err
	}
	h.store.AbortUpload(instanceID, id, r.URL.Query().Get("uploadId"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func writeXML(w http.ResponseWriter, body string) error {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
	return nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

var partNumberRe = regexp.MustCompile(`(?s)<Part>.*?<PartNumber>\s*(\d+)\s*</PartNumber>.*?</Part>`)

// xmlPartNumbers reads the part order out of a CompleteMultipartUpload body.
//
// Deliberately not a parser: the document is a flat list of two fields per part, and the ETags in
// it are not checked because this emulator computed them itself and holds the bytes they name.
// The ORDER is the part that matters — it is the client saying how the object goes together.
func xmlPartNumbers(body string) []int {
	var out []int
	for _, m := range partNumberRe.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
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
