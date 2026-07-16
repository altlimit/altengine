package altengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// SocketState is the ChannelSocket lifecycle state.
type SocketState string

// Socket states.
const (
	SocketIdle         SocketState = "idle"
	SocketConnecting   SocketState = "connecting"
	SocketOpen         SocketState = "open"
	SocketReconnecting SocketState = "reconnecting"
	SocketClosed       SocketState = "closed"
)

// SocketOptions configures a ChannelSocket.
type SocketOptions struct {
	// GetToken is called before every connection attempt (including
	// reconnects) so tokens are always fresh. Typically ChannelClient.
	// CreateToken, or a call to your own backend.
	GetToken func(ctx context.Context) (*TokenResponse, error)
	// URL is an explicit subscribe URL (wss://…/v1/channel/<instance>/subscribe)
	// when the token response's ws_url isn't used; ?token= is appended
	// automatically.
	URL string
	// Channels is the subset of the token's channels to subscribe on connect
	// (default: all).
	Channels []string
	// OnMessage receives every delivered message. Called from the socket's
	// read goroutine — hand off to your own goroutine for slow work.
	OnMessage func(ChannelMessage)
	// OnState observes lifecycle transitions.
	OnState func(SocketState)
	// OnError observes connection-level errors.
	OnError func(error)
	// PingInterval is the keepalive ping interval (default 30s). A silent
	// connection past ~1.5 intervals is treated as dead and reconnected.
	PingInterval time.Duration
	// BackoffBase/BackoffMax bound the reconnect backoff (defaults 250ms / 30s).
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

type pendingSub struct {
	channels []string
	ch       chan subResult
}

type subResult struct {
	channels []string
	err      error
}

type pubResult struct {
	delivered int
	err       error
}

// ChannelSocket is a managed WebSocket subscriber for altengine channels:
// connect, auto-reconnect with backoff, token re-mint on reconnect,
// resubscribe, and keepalive pings.
type ChannelSocket struct {
	opts SocketOptions

	mu           sync.Mutex
	conn         *websocket.Conn
	state        SocketState
	desired      map[string]struct{}
	explicit     bool
	closed       bool
	attempts     int
	lastActivity time.Time
	pendingSubs  []pendingSub
	pendingPubs  []chan pubResult
	cancelLoop   context.CancelFunc
}

// NewChannelSocket builds a socket; call Connect to dial.
func NewChannelSocket(opts SocketOptions) *ChannelSocket {
	s := &ChannelSocket{opts: opts, state: SocketIdle, desired: map[string]struct{}{}}
	s.explicit = opts.Channels != nil
	for _, c := range opts.Channels {
		s.desired[c] = struct{}{}
	}
	if s.opts.PingInterval == 0 {
		s.opts.PingInterval = 30 * time.Second
	}
	if s.opts.BackoffBase == 0 {
		s.opts.BackoffBase = 250 * time.Millisecond
	}
	if s.opts.BackoffMax == 0 {
		s.opts.BackoffMax = 30 * time.Second
	}
	return s
}

// State returns the current lifecycle state.
func (s *ChannelSocket) State() SocketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *ChannelSocket) setState(st SocketState) {
	if s.state == st {
		return
	}
	s.state = st
	if s.opts.OnState != nil {
		go s.opts.OnState(st)
	}
}

func (s *ChannelSocket) emitError(err error) {
	if s.opts.OnError != nil {
		go s.opts.OnError(err)
	}
}

// Connect dials the socket; it returns once the connection is open. After a
// successful Connect the socket reconnects automatically until Close.
func (s *ChannelSocket) Connect(ctx context.Context) error {
	s.mu.Lock()
	if s.conn != nil {
		s.mu.Unlock()
		return nil
	}
	s.closed = false
	s.setState(SocketConnecting)
	s.mu.Unlock()

	if err := s.dial(ctx); err != nil {
		s.mu.Lock()
		s.setState(SocketClosed)
		s.mu.Unlock()
		return err
	}
	return nil
}

// Close stops reconnecting and closes the connection.
func (s *ChannelSocket) Close() error {
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	s.conn = nil
	if s.cancelLoop != nil {
		s.cancelLoop()
	}
	s.failPendingLocked(errors.New("socket closed"))
	s.setState(SocketClosed)
	s.mu.Unlock()
	if conn != nil {
		return conn.Close(websocket.StatusNormalClosure, "client close")
	}
	return nil
}

// Subscribe adds channels (tracked and re-applied after reconnects) and waits
// for the server's ack, returning the acked channel list.
func (s *ChannelSocket) Subscribe(ctx context.Context, channels []string) ([]string, error) {
	s.mu.Lock()
	for _, c := range channels {
		s.desired[c] = struct{}{}
	}
	s.explicit = true
	conn := s.conn
	if conn == nil || s.state != SocketOpen {
		all := s.desiredLocked()
		s.mu.Unlock()
		return all, nil
	}
	ch := make(chan subResult, 1)
	s.pendingSubs = append(s.pendingSubs, pendingSub{channels: channels, ch: ch})
	s.mu.Unlock()

	if err := s.send(ctx, conn, map[string]any{"type": "subscribe", "channels": channels}); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res.channels, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Unsubscribe removes channels from the subscription (and the resubscribe set).
func (s *ChannelSocket) Unsubscribe(ctx context.Context, channels []string) error {
	s.mu.Lock()
	for _, c := range channels {
		delete(s.desired, c)
	}
	s.explicit = true
	conn := s.conn
	open := s.state == SocketOpen
	s.mu.Unlock()
	if conn == nil || !open {
		return nil
	}
	return s.send(ctx, conn, map[string]any{"type": "unsubscribe", "channels": channels})
}

// Publish sends a message over the socket (requires a ws-capable publish
// token) and waits for the ack, returning the delivered count.
func (s *ChannelSocket) Publish(ctx context.Context, channel string, data any) (int, error) {
	s.mu.Lock()
	conn := s.conn
	if conn == nil || s.state != SocketOpen {
		s.mu.Unlock()
		return 0, errors.New("socket is not open")
	}
	ch := make(chan pubResult, 1)
	s.pendingPubs = append(s.pendingPubs, ch)
	s.mu.Unlock()

	if err := s.send(ctx, conn, map[string]any{"type": "publish", "channel": channel, "data": data}); err != nil {
		return 0, err
	}
	select {
	case res := <-ch:
		return res.delivered, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// --- internals ---

func (s *ChannelSocket) desiredLocked() []string {
	out := make([]string, 0, len(s.desired))
	for c := range s.desired {
		out = append(out, c)
	}
	return out
}

func (s *ChannelSocket) send(ctx context.Context, conn *websocket.Conn, frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

func (s *ChannelSocket) buildURL(tok *TokenResponse) (string, error) {
	raw := tok.WSURL
	if s.opts.URL != "" {
		raw = s.opts.URL
	}
	if raw == "" {
		return "", errors.New("no WebSocket URL: token response had no ws_url and no URL option was set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("token") == "" {
		q.Set("token", tok.Token)
	}
	// Narrow the initial subscription to the explicitly requested subset.
	s.mu.Lock()
	if s.explicit && len(s.desired) > 0 {
		q.Set("channels", strings.Join(s.desiredLocked(), ","))
	}
	s.mu.Unlock()
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *ChannelSocket) dial(ctx context.Context) error {
	tok, err := s.opts.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	u, err := s.buildURL(tok)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)

	loopCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.conn = conn
	s.cancelLoop = cancel
	s.attempts = 0
	s.lastActivity = time.Now()
	s.setState(SocketOpen)
	s.mu.Unlock()

	go s.readLoop(loopCtx, conn)
	go s.pingLoop(loopCtx, conn)
	return nil
}

func (s *ChannelSocket) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			s.onDisconnect(conn, err)
			return
		}
		s.mu.Lock()
		s.lastActivity = time.Now()
		s.mu.Unlock()
		s.handleFrame(data)
	}
}

func (s *ChannelSocket) handleFrame(data []byte) {
	raw := string(data)
	if raw == "pong" || raw == "ping" {
		return
	}
	var frame struct {
		Type      string          `json:"type"`
		Channels  []string        `json:"channels"`
		Channel   string          `json:"channel"`
		Data      json.RawMessage `json:"data"`
		TS        int64           `json:"ts"`
		Delivered int             `json:"delivered"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "subscribed":
		// Only settle the oldest pending subscribe if this ack covers its
		// channels — an unsolicited ack (e.g. for the connect-time ?channels=
		// subscription) arriving late must not steal a Subscribe's resolution.
		acked := make(map[string]struct{}, len(frame.Channels))
		for _, c := range frame.Channels {
			acked[c] = struct{}{}
		}
		s.mu.Lock()
		if len(s.pendingSubs) > 0 {
			head := s.pendingSubs[0]
			covered := true
			for _, c := range head.channels {
				if _, ok := acked[c]; !ok {
					covered = false
					break
				}
			}
			if covered {
				s.pendingSubs = s.pendingSubs[1:]
				head.ch <- subResult{channels: frame.Channels}
			}
		}
		s.mu.Unlock()
	case "published":
		s.mu.Lock()
		if len(s.pendingPubs) > 0 {
			ch := s.pendingPubs[0]
			s.pendingPubs = s.pendingPubs[1:]
			ch <- pubResult{delivered: frame.Delivered}
		}
		s.mu.Unlock()
	case "error":
		msg := strings.Trim(string(frame.Error), `"`)
		err := errors.New(msg)
		// An error ack settles the oldest pending request, if any.
		s.mu.Lock()
		if len(s.pendingPubs) > 0 {
			ch := s.pendingPubs[0]
			s.pendingPubs = s.pendingPubs[1:]
			ch <- pubResult{err: err}
		} else if len(s.pendingSubs) > 0 {
			head := s.pendingSubs[0]
			s.pendingSubs = s.pendingSubs[1:]
			head.ch <- subResult{err: err}
		}
		s.mu.Unlock()
		s.emitError(err)
	default:
		if frame.Type == "" && frame.Channel != "" {
			if s.opts.OnMessage != nil {
				s.opts.OnMessage(ChannelMessage{Channel: frame.Channel, Data: frame.Data, TS: frame.TS})
			}
		}
	}
}

func (s *ChannelSocket) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(s.opts.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			stale := time.Since(s.lastActivity) > s.opts.PingInterval*3/2
			s.mu.Unlock()
			if stale {
				// Dead connection: no traffic (not even pongs). Force a reconnect.
				conn.Close(websocket.StatusCode(4000), "keepalive timeout")
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func (s *ChannelSocket) onDisconnect(conn *websocket.Conn, cause error) {
	s.mu.Lock()
	if s.conn != conn {
		// A newer connection already replaced this one.
		s.mu.Unlock()
		return
	}
	s.conn = nil
	if s.cancelLoop != nil {
		s.cancelLoop()
	}
	s.failPendingLocked(fmt.Errorf("socket closed: %w", cause))
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.setState(SocketReconnecting)
	attempt := s.attempts
	s.attempts++
	s.mu.Unlock()

	delay := s.opts.BackoffMax
	if attempt < 20 { // beyond 2^20 the shift only overflows; it's already capped
		delay = min(s.opts.BackoffBase<<attempt, s.opts.BackoffMax)
	}
	delay = time.Duration(float64(delay) * (0.5 + rand.Float64()*0.5))
	time.AfterFunc(delay, func() {
		s.mu.Lock()
		if s.closed || s.conn != nil {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.dial(ctx); err != nil {
			s.emitError(err)
			s.onDisconnect(nil, err)
		}
	})
}

func (s *ChannelSocket) failPendingLocked(err error) {
	for _, p := range s.pendingSubs {
		p.ch <- subResult{err: err}
	}
	s.pendingSubs = nil
	for _, ch := range s.pendingPubs {
		ch <- pubResult{err: err}
	}
	s.pendingPubs = nil
}
