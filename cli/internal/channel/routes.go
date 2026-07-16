package channel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
	"github.com/gorilla/websocket"
)

// Handler serves the channel data plane.
type Handler struct {
	Reg  *control.Registry
	Auth *auth.Store
	Hub  *Hub
	up   websocket.Upgrader
}

func NewHandler(reg *control.Registry, a *auth.Store, hub *Hub) *Handler {
	return &Handler{
		Reg: reg, Auth: a, Hub: hub,
		up: websocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true }, // local dev
			Subprotocols: []string{"bearer"},
		},
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	p := "/v1/channel/{instance}"
	mux.HandleFunc("POST "+p+"/tokens", common.Wrap(h.tokens))
	mux.HandleFunc("POST "+p+"/publish", common.Wrap(h.publish))
	mux.HandleFunc("GET "+p+"/presence", common.Wrap(h.presence))
	mux.HandleFunc("GET "+p+"/subscribe", common.Wrap(h.subscribe))
}

func presenceEnabled(inst *control.Instance) bool {
	v, _ := inst.Config["presence"].(bool)
	return v
}

// --- POST /tokens ---

func (h *Handler) tokens(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	var body struct {
		Channels   any `json:"channels"`
		TTLSeconds any `json:"ttl_seconds"`
		Publish    any `json:"publish"`
		PresenceID any `json:"presence_id"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	pub, err := parsePublishMode(body.Publish)
	if err != nil {
		return err
	}
	need := auth.Read
	if pub != "" {
		need = auth.Write
	}
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "channel", name, need); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("channel", name)

	channels, err := validateChannelList(body.Channels)
	if err != nil {
		return err
	}
	ttl := clampTTL(body.TTLSeconds)
	exp := time.Now().Unix() + ttl
	pid, err := validatePresenceID(body.PresenceID)
	if err != nil {
		return err
	}
	claims := Claims{Iss: inst.ID, Channels: channels, Exp: exp, Pub: pub, Pid: pid}
	token := SignJWT(claims, inst.Secret)

	scheme := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	wsURL := scheme + "://" + r.Host + "/v1/channel/" + url.PathEscape(inst.Name) + "/subscribe?token=" + url.QueryEscape(token)

	resp := map[string]any{
		"token": token, "expires_at": exp, "channels": channels,
		"publish": pubOrFalse(pub), "ws_url": wsURL,
	}
	if pid != "" {
		resp["presence_id"] = pid
	}
	common.WriteJSON(w, 200, resp)
	return nil
}

func pubOrFalse(p string) any {
	if p == "" {
		return false
	}
	return p
}

// --- POST /publish ---

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	var body struct {
		Channel string          `json:"channel"`
		Data    json.RawMessage `json:"data"`
	}
	if err := common.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Channel == "" {
		return common.BadRequest("channel is required")
	}
	if err := validateChannel(body.Channel); err != nil {
		return err
	}

	token := auth.Bearer(r)
	var inst *control.Instance
	if token != "" && LooksLikeJWT(token) {
		peek := PeekClaims(token)
		if peek == nil || peek.Iss == "" {
			return common.Unauthenticated("invalid token")
		}
		resolved := h.Reg.GetByID("channel", peek.Iss)
		if resolved == nil {
			return common.Unauthenticated("invalid token")
		}
		claims := VerifyJWT(token, resolved.Secret)
		if claims == nil || !CanPublish(claims, "http") || !contains(claims.Channels, body.Channel) {
			return common.PermissionDenied("token may not publish to this channel over HTTP")
		}
		inst = resolved
	} else {
		id, err := h.Auth.Resolve(r)
		if err != nil {
			return err
		}
		if err := auth.Require(id, "channel", name, auth.Write); err != nil {
			return err
		}
		inst = h.Reg.GetOrCreate("channel", name)
	}

	delivered, err := h.Hub.Publish(inst.ID, body.Channel, body.Data)
	if err != nil {
		return err
	}
	common.WriteJSON(w, 200, map[string]any{"delivered": delivered})
	return nil
}

// --- GET /presence ---

func (h *Handler) presence(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		return common.BadRequest("channel is required")
	}
	if err := validateChannel(channel); err != nil {
		return err
	}
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "channel", name, auth.Read); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("channel", name)
	if !presenceEnabled(inst) {
		return common.PermissionDenied("presence is not enabled for this channel instance")
	}
	snap := h.Hub.Presence(inst.ID, channel)
	out := map[string]any{"channel": channel, "occupancy": snap.Occupancy,
		"member_count": snap.MemberCount, "members": snap.Members, "truncated": snap.Truncated}
	common.WriteJSON(w, 200, out)
	return nil
}

// --- GET /subscribe (WebSocket) ---

func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) error {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return common.NewError(426, "expected a WebSocket upgrade", "INVALID_ARGUMENT")
	}
	token := r.URL.Query().Get("token")
	usedSubproto := false
	if token == "" {
		token = subprotocolToken(r)
		usedSubproto = token != ""
	}
	if token == "" {
		return common.Unauthenticated("missing token")
	}
	peek := PeekClaims(token)
	if peek == nil || peek.Iss == "" {
		return common.Unauthenticated("invalid token")
	}
	inst := h.Reg.GetByID("channel", peek.Iss)
	if inst == nil {
		return common.Unauthenticated("invalid token")
	}
	claims := VerifyJWT(token, inst.Secret)
	if claims == nil {
		return common.Unauthenticated("invalid or expired token")
	}

	// Initial channel selection: subset via ?channel=/?channels=, else all in the token.
	requested := r.URL.Query()["channel"]
	if len(requested) == 0 {
		if csv := r.URL.Query().Get("channels"); csv != "" {
			for _, c := range strings.Split(csv, ",") {
				if c = strings.TrimSpace(c); c != "" {
					requested = append(requested, c)
				}
			}
		}
	}
	if len(requested) == 0 {
		requested = claims.Channels
	}
	for _, ch := range requested {
		if !contains(claims.Channels, ch) {
			return common.PermissionDenied("token may not subscribe to: " + ch)
		}
	}

	var respHeader http.Header
	if usedSubproto {
		respHeader = http.Header{"Sec-WebSocket-Protocol": {"bearer"}}
	}
	ws, err := h.up.Upgrade(w, r, respHeader)
	if err != nil {
		return nil // upgrade writes its own response
	}

	pid := ""
	if presenceEnabled(inst) {
		pid = claims.Pid
	}
	c := &conn{
		id: common.UUID(), ws: ws,
		allowed: sliceToSet(claims.Channels), subscribed: map[string]bool{},
		pub: claims.Pub, presenceID: pid, exp: claims.Exp,
	}
	h.runConn(inst, c, requested)
	return nil
}

func (h *Handler) runConn(inst *control.Instance, c *conn, initial []string) {
	defer c.ws.Close()
	// Join initial channels.
	for _, ch := range initial {
		if h.Hub.join(inst.ID, ch, c) && c.presenceID != "" {
			h.Hub.broadcastPresence(inst.ID, ch, "join", c.presenceID, c)
		}
	}
	c.send(subscribedFrame(c))
	defer h.cleanup(inst, c)

	// Force-close at token expiry.
	if c.exp > 0 {
		c.ws.SetReadDeadline(time.Unix(c.exp, 0))
	}
	for {
		_, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		h.handleFrame(inst, c, msg)
	}
}

func (h *Handler) handleFrame(inst *control.Instance, c *conn, msg []byte) {
	var cmd struct {
		Type     string          `json:"type"`
		Channels []string        `json:"channels"`
		Channel  string          `json:"channel"`
		Data     json.RawMessage `json:"data"`
	}
	if json.Unmarshal(msg, &cmd) != nil {
		return
	}
	switch cmd.Type {
	case "publish":
		if c.pub != "ws" && c.pub != "all" {
			c.send(errorFrame("token may not publish over websocket"))
			return
		}
		if !c.allowed[cmd.Channel] {
			c.send(errorFrame("not authorized to publish to this channel"))
			return
		}
		delivered, err := h.Hub.Publish(inst.ID, cmd.Channel, cmd.Data)
		if err != nil {
			c.send(errorFrame("publish failed"))
			return
		}
		c.send(mustJSON(map[string]any{"type": "published", "channel": cmd.Channel, "delivered": delivered}))
	case "subscribe":
		for _, ch := range cmd.Channels {
			if c.allowed[ch] && !c.subscribed[ch] {
				if h.Hub.join(inst.ID, ch, c) && c.presenceID != "" {
					h.Hub.broadcastPresence(inst.ID, ch, "join", c.presenceID, c)
				}
			}
		}
		c.send(subscribedFrame(c))
	case "unsubscribe":
		for _, ch := range cmd.Channels {
			if c.subscribed[ch] {
				if h.Hub.leave(inst.ID, ch, c) && c.presenceID != "" {
					h.Hub.broadcastPresence(inst.ID, ch, "leave", c.presenceID, nil)
				}
			}
		}
		c.send(subscribedFrame(c))
	}
}

func (h *Handler) cleanup(inst *control.Instance, c *conn) {
	for ch := range c.subscribed {
		if h.Hub.leave(inst.ID, ch, c) && c.presenceID != "" {
			h.Hub.broadcastPresence(inst.ID, ch, "leave", c.presenceID, nil)
		}
	}
}

// --- helpers ---

func subscribedFrame(c *conn) []byte {
	chans := make([]string, 0, len(c.subscribed))
	for ch := range c.subscribed {
		chans = append(chans, ch)
	}
	return mustJSON(map[string]any{"type": "subscribed", "channels": chans})
}

func errorFrame(msg string) []byte {
	return mustJSON(map[string]any{"type": "error", "error": msg})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func subprotocolToken(r *http.Request) string {
	proto := r.Header.Get("Sec-WebSocket-Protocol")
	if proto == "" {
		return ""
	}
	parts := strings.Split(proto, ",")
	for i, p := range parts {
		if strings.TrimSpace(p) == "bearer" && i+1 < len(parts) {
			return strings.TrimSpace(parts[i+1])
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sliceToSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}
