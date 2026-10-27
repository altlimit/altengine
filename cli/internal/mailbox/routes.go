package mailbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// The mailbox data plane, /v1/mailbox/{instance} — same paths, shapes and refusals as hosted:
//
//	GET    /                                  the instance's addresses and the settings a caller needs
//	GET    /messages                          list; `wait` long-polls for the next inbound message
//	GET    /messages/{id}                     one message, bodies included; `?raw=1` adds a .eml URL
//	GET    /messages/{id}/attachments/{idx}   a short-lived download URL for one attachment
//	POST   /messages/{id}/read                mark read
//	POST   /messages/{id}/reply               reply, threaded
//	POST   /messages/{id}/keep                keep one message `days` (1–90) from now
//	DELETE /messages/{id}                     delete now rather than at expiry
//	POST   /send                              send a new message
//	GET    /addresses                         addresses that carry a setting
//	POST   /addresses                         register (and optionally label) an address
//	DELETE /addresses/{local}                 disable an address
//	POST   /inject                            deliver a raw message without SMTP
//
// plus the signed downloads those URLs point at (/_mailbox/...), and the console's /admin routes.
//
// ORG API KEYS ONLY, as hosted: a mailbox holds third parties' mail and can send as the tenant,
// and there is no row-rule model for it. The emulator has no identity wiring here at all.

const downloadTTL = 5 * time.Minute

// Handler serves the mailbox planes.
type Handler struct {
	svc  *Service
	reg  *control.Registry
	auth *auth.Store
}

// NewHandler builds the handler.
func NewHandler(reg *control.Registry, a *auth.Store, svc *Service) *Handler {
	return &Handler{svc: svc, reg: reg, auth: a}
}

// Register mounts the data plane, the signed downloads and the console's routes.
func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/mailbox/{instance}"
	mux.HandleFunc("GET "+p, common.Wrap(h.info))
	mux.HandleFunc("GET "+p+"/messages", common.Wrap(h.list))
	mux.HandleFunc("GET "+p+"/messages/{id}", common.Wrap(h.get))
	mux.HandleFunc("GET "+p+"/messages/{id}/attachments/{idx}", common.Wrap(h.attachment))
	mux.HandleFunc("POST "+p+"/messages/{id}/read", common.Wrap(h.markRead))
	mux.HandleFunc("POST "+p+"/messages/{id}/reply", common.Wrap(h.reply))
	mux.HandleFunc("POST "+p+"/messages/{id}/keep", common.Wrap(h.keep))
	mux.HandleFunc("DELETE "+p+"/messages/{id}", common.Wrap(h.remove))
	mux.HandleFunc("POST "+p+"/send", common.Wrap(h.send))
	mux.HandleFunc("GET "+p+"/addresses", common.Wrap(h.listAddresses))
	mux.HandleFunc("POST "+p+"/addresses", common.Wrap(h.putAddress))
	mux.HandleFunc("DELETE "+p+"/addresses/{local}", common.Wrap(h.disableAddress))
	mux.HandleFunc("POST "+p+"/inject", common.Wrap(h.inject))

	mux.HandleFunc("GET /_mailbox/{instance}/{id}/raw", common.Wrap(h.downloadRaw))
	mux.HandleFunc("GET /_mailbox/{instance}/{id}/attachments/{idx}", common.Wrap(h.downloadAttachment))

	h.registerAdmin(mux)
}

func (h *Handler) resolve(r *http.Request, need auth.Level) (*control.Instance, error) {
	name := r.PathValue("instance")
	if name == "" {
		return nil, common.BadRequest("missing mailbox instance")
	}
	id, err := h.auth.Resolve(r)
	if err != nil {
		return nil, err
	}
	if err := auth.Require(id, "mailbox", name, need); err != nil {
		return nil, err
	}
	return h.reg.GetOrCreate("mailbox", name), nil
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

func boolQuery(v string) bool { return v == "1" || v == "true" }

// publicAttachment is an attachment as the API returns it: no storage key, whether it is stored.
type publicAttachment struct {
	Attachment
	Stored bool `json:"stored"`
}

// PublicMessage is a message as the API returns it: no storage keys.
type PublicMessage struct {
	Summary
	Text          *string            `json:"text"`
	HTML          *string            `json:"html"`
	TextTruncated bool               `json:"textTruncated"`
	HTMLTruncated bool               `json:"htmlTruncated"`
	References    *string            `json:"references"`
	AuthResults   *string            `json:"authResults"`
	Deliveries    []Delivery         `json:"deliveries"`
	HasRaw        bool               `json:"hasRaw"`
	Attachments   []publicAttachment `json:"attachments"`
}

// Public strips a record to its API shape.
func Public(m *Record) PublicMessage {
	out := PublicMessage{
		Summary: m.Summary, Text: m.Text, HTML: m.HTML, TextTruncated: m.TextTruncated, HTMLTruncated: m.HTMLTruncated,
		References: m.References, AuthResults: m.AuthResults, Deliveries: m.Deliveries, HasRaw: m.RawKey != "",
		Attachments: make([]publicAttachment, 0, len(m.Attachments)),
	}
	if out.Deliveries == nil {
		out.Deliveries = []Delivery{}
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, publicAttachment{Attachment: a, Stored: a.Key != ""})
	}
	return out
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	st := ParseConfig(inst.Config()).Settings
	out := h.svc.addressInfo(inst)
	out["name"] = inst.Name
	out["acceptMode"] = st.AcceptMode
	out["sendMode"] = st.SendMode
	out["retentionDays"] = st.RetentionDays
	// Hosted, this is set when the mailbox's sending was paused for mailing too many addresses
	// that bounced or complained. Locally nothing bounces — no mail leaves — so it is always null.
	out["sendPaused"] = nil
	common.WriteJSON(w, 200, out)
	return nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	local, err := NameFilter(q.Get("to"), SlugOf(inst.Name), h.svc.domain)
	if err != nil {
		return err
	}
	after := ""
	if a := q.Get("after"); a != "" {
		after = ResolveAfter(a, h.svc.now())
	}
	limit := common.PageLimit(q.Get("limit"), defaultListLimit, MaxListLimit)
	direction := q.Get("direction")
	if q.Has("direction") && direction != "in" && direction != "out" {
		return common.BadRequest("direction must be 'in' or 'out'")
	}
	if q.Has("wait") {
		secs, perr := strconv.ParseFloat(strings.TrimSpace(q.Get("wait")), 64)
		if perr != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
			return common.BadRequest("wait must be a number of seconds")
		}
		if after == "" {
			return common.BadRequest("wait needs after=now, a message id, or 0: the point to wait from")
		}
		d := time.Duration(math.Min(secs*1000, MaxWaitMS)) * time.Millisecond
		msgs, err := h.svc.store.WaitFor(r.Context(), inst.ID, local, after, limit, d)
		if err != nil {
			return err
		}
		cursor := after
		if len(msgs) > 0 {
			cursor = msgs[len(msgs)-1].ID
		}
		common.WriteJSON(w, 200, map[string]any{"messages": msgs, "cursor": cursor})
		return nil
	}
	msgs, cursor, err := h.svc.store.List(inst.ID, ListOptions{
		Local: local, After: after, Cursor: q.Get("cursor"), Limit: limit,
		Unread: boolQuery(q.Get("unread")), Direction: direction,
	})
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"messages": msgs, "cursor": cursor})
	return nil
}

func (h *Handler) message(inst *control.Instance, id string) (*Record, error) {
	m, err := h.svc.store.Get(inst.ID, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, common.NotFound("message not found")
	}
	return m, nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	m, err := h.message(inst, r.PathValue("id"))
	if err != nil {
		return err
	}
	out := map[string]any{"message": Public(m)}
	if boolQuery(r.URL.Query().Get("raw")) {
		if m.RawKey == "" {
			return common.NotFound("the raw message was not kept (settings.keepRaw)")
		}
		out["rawUrl"] = h.svc.signedURL(origin(r), inst.ID, m.ID, "raw", downloadTTL)
		out["expiresAt"] = time.Now().Add(downloadTTL).UnixMilli()
	}
	common.WriteJSON(w, 200, out)
	return nil
}

func (h *Handler) attachment(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	m, err := h.message(inst, r.PathValue("id"))
	if err != nil {
		return err
	}
	idx, perr := strconv.Atoi(r.PathValue("idx"))
	var a *Attachment
	if perr == nil {
		for i := range m.Attachments {
			if m.Attachments[i].Idx == idx {
				a = &m.Attachments[i]
			}
		}
	}
	if a == nil {
		return common.NotFound("attachment not found")
	}
	if a.Key == "" {
		why := ""
		if a.Rejected != nil {
			why = " (" + *a.Rejected + ")"
		}
		return common.NotFound("attachment was not stored" + why)
	}
	common.WriteJSON(w, 200, map[string]any{
		"url":         h.svc.signedURL(origin(r), inst.ID, m.ID, strconv.Itoa(a.Idx), downloadTTL),
		"filename":    a.Filename,
		"contentType": a.ContentType,
		"size":        a.Size,
		"expiresAt":   time.Now().Add(downloadTTL).UnixMilli(),
	})
	return nil
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	found, err := h.svc.store.MarkRead(inst.ID, r.PathValue("id"), h.svc.now())
	if err != nil {
		return err
	}
	if !found {
		return common.NotFound("message not found")
	}
	common.WriteJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func readBody(r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	if err := common.ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

// readSendBody reads a send or reply: attachments ride base64 in it, so its cap is the largest
// message base64'd into JSON.
func readSendBody(r *http.Request) (map[string]any, error) {
	body := map[string]any{}
	if err := common.ReadJSONLimited(r, &body, MaxSendRequestBytes); err != nil {
		return nil, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readSendBody(r)
	if err != nil {
		return err
	}
	body["replyToMessageId"] = r.PathValue("id")
	m, err := h.svc.Send(inst, body)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, map[string]any{"message": m})
	return nil
}

// Keep is the one per-message expiry change: /v1, MCP, the function binding and the console.
// Extending needs `write`; moving the expiry EARLIER is a deferred delete, so only a caller that
// could delete (canShorten: `full`, or the console) may.
func (s *Service) Keep(inst *control.Instance, id string, days any, canShorten bool) (*Summary, error) {
	at, err := KeepUntil(days, s.now())
	if err != nil {
		return nil, err
	}
	m, err := s.store.SetExpiry(inst.ID, id, at, canShorten)
	if errors.Is(err, ErrShortenNeedsFull) {
		return nil, common.PermissionDenied("keeping a message for less time than it already has left deletes it sooner, which needs 'full' access; 'write' can only extend")
	}
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, common.NotFound("message not found")
	}
	return m, nil
}

// keep needs `write` to be called at all; whether it may shorten is the caller's real level on
// this instance — a function's grant, when a function made the call.
func (h *Handler) keep(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	id, err := h.auth.Resolve(r)
	if err != nil {
		return err
	}
	canShorten := auth.EffectiveLevel(r, id, "mailbox", r.PathValue("instance")) >= auth.Full
	body, err := readBody(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Keep(inst, r.PathValue("id"), body["days"], canShorten)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"message": m})
	return nil
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Full)
	if err != nil {
		return err
	}
	n, err := h.svc.store.Remove(inst.ID, []string{r.PathValue("id")})
	if err != nil {
		return err
	}
	if n == 0 {
		return common.NotFound("message not found")
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": n})
	return nil
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readSendBody(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Send(inst, body)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, map[string]any{"message": m})
	return nil
}

func (h *Handler) addressJSON(inst *control.Instance, a *AddressRecord) map[string]any {
	b, _ := json.Marshal(a)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	out["address"] = h.svc.addressOf(inst, a.Local)
	return out
}

func (h *Handler) listAddresses(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Read)
	if err != nil {
		return err
	}
	list, cursor, err := h.svc.store.ListAddresses(inst.ID, r.URL.Query().Get("cursor"), common.PageLimit(r.URL.Query().Get("limit"), defaultListLimit, MaxListLimit))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, h.addressJSON(inst, a))
	}
	common.WriteJSON(w, 200, map[string]any{"addresses": out, "cursor": cursor})
	return nil
}

// PutAddress is the one registration rule: /v1, MCP and the console call it.
func (s *Service) PutAddress(inst *control.Instance, localRaw any, labelRaw any, metaRaw any) (*AddressRecord, error) {
	local := ""
	if localRaw == nil || localRaw == "" {
		local = MintLocal()
	} else {
		v, err := ValidateLocal(localRaw, SlugOf(inst.Name))
		if err != nil {
			return nil, err
		}
		local = v
	}
	var label *string
	if labelRaw != nil {
		s, ok := labelRaw.(string)
		if !ok || len([]rune(s)) > 200 {
			return nil, common.BadRequest("label must be a string of at most 200 characters")
		}
		if s != "" {
			label = &s
		}
	}
	meta, _ := metaRaw.(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	if b, _ := json.Marshal(meta); len(b) > 4096 {
		return nil, common.BadRequest("meta must be at most 4 KB of JSON")
	}
	return s.store.PutAddress(inst.ID, local, label, meta, s.now())
}

func (h *Handler) putAddress(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	rec, err := h.svc.PutAddress(inst, body["local"], body["label"], body["meta"])
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, h.addressJSON(inst, rec))
	return nil
}

func (h *Handler) disableAddress(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	local, err := ValidateLocal(r.PathValue("local"), "")
	if err != nil {
		return err
	}
	rec, err := h.svc.store.DisableAddress(inst.ID, local, h.svc.now())
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, rec)
	return nil
}

// readRaw reads a raw message body, refusing to buffer past the message limit.
func readRaw(r *http.Request) ([]byte, error) {
	if r.ContentLength > MaxMessageBytes {
		return nil, reject(rejectTooLarge)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxMessageBytes+1))
	if err != nil {
		return nil, common.BadRequest("could not read request body")
	}
	if len(b) > MaxMessageBytes {
		return nil, reject(rejectTooLarge)
	}
	return b, nil
}

// writeDelivery answers a delivery attempt: the stored message, or the SMTP refusal as 422 —
// the shape hosted inject uses.
func writeDelivery(w http.ResponseWriter, rec *Record, err error, full bool) error {
	var rj *Rejection
	if errors.As(err, &rj) {
		common.WriteJSON(w, 422, map[string]any{"rejected": rj.SMTP})
		return nil
	}
	if err != nil {
		return err
	}
	if full {
		common.WriteJSON(w, 201, map[string]any{"message": Public(rec)})
		return nil
	}
	common.WriteJSON(w, 201, map[string]any{"message": map[string]any{"id": rec.ID, "local": rec.Local, "subject": rec.Subject}})
	return nil
}

// inject delivers a raw RFC 5322 message to `to` (a name, or a full address on this instance)
// through the same admission and ingest as any other mail. Hosted this exists only in local
// development; in the emulator it is always on. `authenticated=1` marks the sender as having
// passed authentication — see authStamp.
func (h *Handler) inject(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.resolve(r, auth.Write)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	to := q.Get("to")
	if to == "" {
		return common.BadRequest("to=<local part> is required")
	}
	raw, err := readRaw(r)
	if err != nil {
		return writeDelivery(w, nil, err, false)
	}
	from := q.Get("from")
	if from == "" {
		from = "sender@example.com"
	}
	rec, err := h.svc.DeliverLocal(inst, to, from, raw, boolQuery(q.Get("authenticated")))
	return writeDelivery(w, rec, err, false)
}

// --- signed downloads ------------------------------------------------------------------------

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, instanceID, key, filename, contentType string) error {
	body, ok := h.svc.store.Bytes(instanceID, key)
	if !ok {
		return common.NotFound("not found")
	}
	head := w.Header()
	head.Set("Content-Type", contentType)
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Always a download: a link to an HTML attachment must never render.
	head.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	head.Set("Cache-Control", "no-store")
	head.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	_, _ = w.Write(body)
	return nil
}

func (h *Handler) downloadRaw(w http.ResponseWriter, r *http.Request) error {
	instID, id := r.PathValue("instance"), r.PathValue("id")
	if err := h.svc.verify(instID, id, "raw", r.URL.Query()); err != nil {
		return err
	}
	m, err := h.svc.store.Get(instID, id)
	if err != nil || m == nil || m.RawKey == "" {
		return common.NotFound("not found")
	}
	return h.serveFile(w, r, instID, m.RawKey, id+".eml", "message/rfc822")
}

func (h *Handler) downloadAttachment(w http.ResponseWriter, r *http.Request) error {
	instID, id, part := r.PathValue("instance"), r.PathValue("id"), r.PathValue("idx")
	if err := h.svc.verify(instID, id, part, r.URL.Query()); err != nil {
		return err
	}
	m, err := h.svc.store.Get(instID, id)
	if err != nil || m == nil {
		return common.NotFound("not found")
	}
	for _, a := range m.Attachments {
		if strconv.Itoa(a.Idx) == part && a.Key != "" {
			name := fmt.Sprintf("attachment-%d", a.Idx)
			if a.Filename != nil {
				name = *a.Filename
			}
			return h.serveFile(w, r, instID, a.Key, name, a.ContentType)
		}
	}
	return common.NotFound("not found")
}

// --- console (/admin) ------------------------------------------------------------------------

// The console's routes. Instance create / list / config / delete are the generic /admin/mailbox
// routes the admin package mounts for every service; these are the mailbox-specific ones. Like
// hosted, the console is not a privileged path: it reads and sends through the same store and the
// same Send as /v1, and delivers test mail through the same inbound path as inject.
func (h *Handler) registerAdmin(mux *http.ServeMux) {
	a := "/admin/mailbox/{id}"
	mux.HandleFunc("GET "+a+"/info", common.Wrap(h.adminInfo))
	mux.HandleFunc("GET "+a+"/messages", common.Wrap(h.adminList))
	mux.HandleFunc("GET "+a+"/messages/{mid}", common.Wrap(h.adminGet))
	mux.HandleFunc("DELETE "+a+"/messages/{mid}", common.Wrap(h.adminDelete))
	mux.HandleFunc("POST "+a+"/messages/{mid}/replay", common.Wrap(h.adminReplay))
	mux.HandleFunc("POST "+a+"/messages/{mid}/reply", common.Wrap(h.adminReply))
	mux.HandleFunc("POST "+a+"/messages/{mid}/keep", common.Wrap(h.adminKeep))
	mux.HandleFunc("POST "+a+"/send", common.Wrap(h.adminSend))
	mux.HandleFunc("POST "+a+"/compose", common.Wrap(h.adminCompose))
	mux.HandleFunc("POST "+a+"/eml", common.Wrap(h.adminEml))
	mux.HandleFunc("GET "+a+"/addresses", common.Wrap(h.adminAddresses))
	mux.HandleFunc("POST "+a+"/addresses", common.Wrap(h.adminPutAddress))
	mux.HandleFunc("DELETE "+a+"/addresses/{local}", common.Wrap(h.adminDisableAddress))
	mux.HandleFunc("POST "+a+"/webhook-secret/reveal", common.Wrap(h.adminRevealSecret))
	mux.HandleFunc("POST "+a+"/webhook-secret/rotate", common.Wrap(h.adminRotateSecret))
	mux.HandleFunc("GET /admin/mailbox-settings", common.Wrap(h.adminSettings))
	mux.HandleFunc("PUT /admin/mailbox-settings", common.Wrap(h.adminSetSettings))
}

func (h *Handler) adminInst(r *http.Request) (*control.Instance, error) {
	id := r.PathValue("id")
	in := h.reg.GetByID("mailbox", id)
	if in == nil {
		in = h.reg.Get("mailbox", id)
	}
	if in == nil {
		return nil, common.NotFound("mailbox instance not found")
	}
	return in, nil
}

func (h *Handler) adminInfo(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	n, b, err := h.svc.store.Stats(inst.ID)
	if err != nil {
		return err
	}
	out := h.svc.addressInfo(inst)
	out["id"], out["name"], out["slug"] = inst.ID, inst.Name, nilIfEmpty(SlugOf(inst.Name))
	out["config"] = ParseConfig(inst.Config())
	out["message_count"], out["stored_bytes"] = n, b
	out["loopback"] = h.svc.Loopback()
	out["org_sends_per_day"] = OrgSendsPerDay
	out["send_paused"] = nil // never locally: see info
	common.WriteJSON(w, 200, out)
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	local, err := NameFilter(q.Get("to"), SlugOf(inst.Name), h.svc.domain)
	if err != nil {
		return err
	}
	direction := q.Get("direction")
	if direction != "in" && direction != "out" {
		direction = ""
	}
	msgs, cursor, err := h.svc.store.List(inst.ID, ListOptions{
		Local: local, Cursor: q.Get("cursor"), Limit: common.PageLimit(q.Get("limit"), defaultListLimit, MaxListLimit),
		Direction: direction, Unread: q.Get("unread") == "1",
	})
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"messages": msgs, "cursor": cursor})
	return nil
}

// adminGet is a message with its download links signed for the console, so the view is one request.
func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	m, err := h.message(inst, r.PathValue("mid"))
	if err != nil {
		return err
	}
	pub := Public(m)
	atts := make([]map[string]any, 0, len(pub.Attachments))
	for _, a := range pub.Attachments {
		b, _ := json.Marshal(a)
		o := map[string]any{}
		_ = json.Unmarshal(b, &o)
		o["url"] = nil
		if a.Stored {
			o["url"] = h.svc.signedURL(origin(r), inst.ID, m.ID, strconv.Itoa(a.Idx), downloadTTL)
		}
		atts = append(atts, o)
	}
	b, _ := json.Marshal(pub)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	out["attachments"] = atts
	res := map[string]any{"message": out}
	if m.RawKey != "" {
		res["rawUrl"] = h.svc.signedURL(origin(r), inst.ID, m.ID, "raw", downloadTTL)
	}
	common.WriteJSON(w, 200, res)
	return nil
}

func (h *Handler) adminDelete(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	n, err := h.svc.store.Remove(inst.ID, []string{r.PathValue("mid")})
	if err != nil {
		return err
	}
	if n == 0 {
		return common.NotFound("message not found")
	}
	common.WriteJSON(w, 200, map[string]any{"deleted": n})
	return nil
}

func (h *Handler) adminReplay(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Replay(inst, r.PathValue("mid"))
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"message": Public(m)})
	return nil
}

func (h *Handler) adminReply(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	body, err := readSendBody(r)
	if err != nil {
		return err
	}
	body["replyToMessageId"] = r.PathValue("mid")
	m, err := h.svc.Send(inst, body)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, map[string]any{"message": m})
	return nil
}

func (h *Handler) adminKeep(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	// The console is the owner's: it may shorten, as hosted.
	m, err := h.svc.Keep(inst, r.PathValue("mid"), body["days"], true)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"message": m})
	return nil
}

func (h *Handler) adminSend(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	body, err := readSendBody(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Send(inst, body)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, map[string]any{"message": m})
	return nil
}

// composeRequest is the console's "Compose test email" form.
type composeRequest struct {
	From          string `json:"from"`
	FromName      string `json:"fromName"`
	To            string `json:"to"`
	Subject       string `json:"subject"`
	Text          string `json:"text"`
	HTML          string `json:"html"`
	Authenticated *bool  `json:"authenticated"`
	Attachments   []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"contentType"`
		Content     string `json:"content"` // base64
	} `json:"attachments"`
}

// adminCompose builds a real MIME message from the form and delivers it through the inbound path:
// to a full address it is delivered exactly as the SMTP listener would (any local mailbox); to a
// bare name, onto this instance as inject does.
func (h *Handler) adminCompose(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	var req composeRequest
	// Attachments ride base64 in the JSON: room for a full-size message plus its encoding.
	if err := common.ReadJSONLimited(r, &req, MaxMessageBytes*4/3+(1<<20)); err != nil {
		return err
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		return common.BadRequest("to is required")
	}
	from := strings.TrimSpace(req.From)
	if from == "" {
		from = "sender@example.com"
	}
	idDomain := "example.com"
	if strings.Contains(from, "@") {
		idDomain = domainOf(from)
	}
	out := OutMessage{
		From: AddressValue{Address: from, Name: req.FromName}, Subject: req.Subject, Text: req.Text, HTML: req.HTML,
		MessageID: "<" + NewMessageID(h.svc.now()) + "@" + idDomain + ">",
	}
	out.To = []AddressValue{{Address: h.headerAddress(inst, to)}}
	for _, a := range req.Attachments {
		b, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			return common.BadRequest(fmt.Sprintf("attachment %q is not valid base64", a.Filename))
		}
		out.Attachments = append(out.Attachments, OutAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: b})
	}
	raw := BuildMessage(out)
	authenticated := req.Authenticated == nil || *req.Authenticated
	var rec *Record
	if strings.Contains(to, "@") {
		rec, err = h.svc.Deliver(to, from, raw, authenticated)
	} else {
		rec, err = h.svc.DeliverLocal(inst, to, from, raw, authenticated)
	}
	return writeDelivery(w, rec, err, true)
}

// headerAddress is what a compose form's To header says: the address as typed, or for a bare
// `name[+tag]` the full address it names on this instance.
func (h *Handler) headerAddress(inst *control.Instance, to string) string {
	if strings.Contains(to, "@") {
		return to
	}
	slug := SlugOf(inst.Name)
	p, ok := ParseLocal(to)
	if slug == "" || !ok {
		return to + "@" + h.svc.domain
	}
	addr := AddressFor(slug, p.Local, h.svc.domain)
	if p.Tag != "" {
		at := strings.LastIndex(addr, "@")
		addr = addr[:at] + "+" + p.Tag + addr[at:]
	}
	return addr
}

// adminEml delivers an uploaded raw .eml, unmodified, through the inbound path.
func (h *Handler) adminEml(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	to := strings.TrimSpace(q.Get("to"))
	if to == "" {
		to = DefaultLocal
	}
	raw, err := readRaw(r)
	if err != nil {
		return writeDelivery(w, nil, err, true)
	}
	from := q.Get("from")
	if from == "" {
		from = "sender@example.com"
	}
	authenticated := q.Get("authenticated") != "0" && q.Get("authenticated") != "false"
	var rec *Record
	if strings.Contains(to, "@") {
		rec, err = h.svc.Deliver(to, from, raw, authenticated)
	} else {
		rec, err = h.svc.DeliverLocal(inst, to, from, raw, authenticated)
	}
	return writeDelivery(w, rec, err, true)
}

func (h *Handler) adminAddresses(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	list, cursor, err := h.svc.store.ListAddresses(inst.ID, r.URL.Query().Get("cursor"), common.PageLimit(r.URL.Query().Get("limit"), defaultListLimit, MaxListLimit))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, h.addressJSON(inst, a))
	}
	common.WriteJSON(w, 200, map[string]any{"addresses": out, "cursor": cursor})
	return nil
}

func (h *Handler) adminPutAddress(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	rec, err := h.svc.PutAddress(inst, body["local"], body["label"], body["meta"])
	if err != nil {
		return err
	}
	common.WriteJSON(w, 201, h.addressJSON(inst, rec))
	return nil
}

func (h *Handler) adminDisableAddress(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	local, err := ValidateLocal(r.PathValue("local"), "")
	if err != nil {
		return err
	}
	rec, err := h.svc.store.DisableAddress(inst.ID, local, h.svc.now())
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, h.addressJSON(inst, rec))
	return nil
}

func (h *Handler) adminRevealSecret(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	secret, err := h.svc.store.WebhookSecret(inst.ID)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	common.WriteJSON(w, 200, map[string]any{"secret": secret})
	return nil
}

func (h *Handler) adminRotateSecret(w http.ResponseWriter, r *http.Request) error {
	inst, err := h.adminInst(r)
	if err != nil {
		return err
	}
	secret, err := h.svc.store.RotateWebhookSecret(inst.ID)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	common.WriteJSON(w, 200, map[string]any{"secret": secret, "rotated": true})
	return nil
}

// adminSettings are the emulator-wide mailbox settings: the mail domain (fixed at start) and
// whether sends to local addresses loop back.
func (h *Handler) adminSettings(w http.ResponseWriter, r *http.Request) error {
	common.WriteJSON(w, 200, map[string]any{"domain": h.svc.domain, "loopback": h.svc.Loopback()})
	return nil
}

func (h *Handler) adminSetSettings(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Loopback *bool `json:"loopback"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Loopback != nil {
		h.svc.SetLoopback(*body.Loopback)
	}
	return h.adminSettings(w, r)
}
