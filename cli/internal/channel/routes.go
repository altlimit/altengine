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
	"github.com/altlimit/altengine/cli/internal/identity"
	"github.com/gorilla/websocket"
)

// Handler serves the channel data plane.
type Handler struct {
	Reg  *control.Registry
	Auth *auth.Store
	Hub  *Hub
	// Ident resolves end-user identity tokens (nil = API keys only).
	Ident *identity.Service
	up    websocket.Upgrader
}

// WithIdentity enables end-user identity tokens on this handler.
func (h *Handler) WithIdentity(svc *identity.Service) *Handler { h.Ident = svc; return h }

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
	mux.HandleFunc("GET "+p+"/rooms", common.Wrap(h.rooms))
	mux.HandleFunc("GET "+p+"/subscribe", common.Wrap(h.subscribe))
}

// rooms lists the instance's live channels. Live is the whole definition: a channel exists
// while someone is subscribed and stops existing when the last socket leaves, so this is a
// snapshot of the topology rather than a stored list of names.
func (h *Handler) rooms(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("instance")
	id, err := h.Auth.Resolve(r)
	if err != nil {
		return err
	}
	if err := auth.Require(id, "channel", name, auth.Read); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("channel", name)
	common.WriteJSON(w, http.StatusOK, map[string]any{"rooms": h.Hub.Rooms(inst.ID)})
	return nil
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
	id, user, err := h.Ident.ResolveRequest(r, h.Auth)
	if err != nil {
		return err
	}
	if user != nil {
		// End-user identity tokens are SUBSCRIBE-ONLY: a browser mints its own token, so
		// letting it also grant itself publish would make the mint the trust boundary.
		need = auth.Read
		pub = ""
	}
	if err := auth.Require(id, "channel", name, need); err != nil {
		return err
	}
	inst := h.Reg.GetOrCreate("channel", name)

	channels, err := validateChannelList(body.Channels)
	if err != nil {
		return err
	}
	if user != nil {
		// Confine the token to the auth instance's channel patterns, interpolated from the
		// VERIFIED identity (e.g. "posts.*", "dm.$auth.uid").
		patterns, err := user.ChannelPatterns(inst.Name)
		if err != nil {
			return err
		}
		for _, ch := range channels {
			if !identity.ChannelAllowed(patterns, ch) {
				return common.PermissionDenied("not permitted to subscribe to: " + ch)
			}
		}
	}
	ttl := clampTTL(body.TTLSeconds)
	exp := time.Now().Unix() + ttl
	pid, err := validatePresenceID(body.PresenceID)
	if err != nil {
		return err
	}
	if user != nil {
		// The presence identity is FORCED to the end user's uid — a client minting its own
		// token must not be able to claim someone else's roster identity.
		pid = user.UID
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

// channelTokenIssuer returns the channel instance that issued a bearer token, or nil when
// the credential isn't a channel-issued JWT (an API key, or a token from another service).
func (h *Handler) channelTokenIssuer(token string) *control.Instance {
	if token == "" || !LooksLikeJWT(token) {
		return nil
	}
	peek := PeekClaims(token)
	if peek == nil || peek.Iss == "" {
		return nil
	}
	return h.Reg.GetByID("channel", peek.Iss)
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
	// A publish-capable subscriber token is a JWT issued by a CHANNEL instance. An end-user
	// identity token is also a JWT, but issued by an auth instance — it falls through to the
	// resolver below (and is refused there) rather than being mistaken for a channel token.
	if resolved := h.channelTokenIssuer(token); resolved != nil {
		claims := VerifyJWT(token, resolved.Secret)
		if claims == nil || !CanPublish(claims, "http") || !contains(claims.Channels, body.Channel) {
			return common.PermissionDenied("token may not publish to this channel over HTTP")
		}
		inst = resolved
	} else {
		id, user, err := h.Ident.ResolveRequest(r, h.Auth)
		if err != nil {
			return err
		}
		// Publishing stays a backend capability: an end-user token subscribes, and writes
		// through the datastore (whose live bridge publishes) instead.
		if user != nil {
			return common.PermissionDenied("publishing requires an org API key or a publish token, not an end-user token")
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
