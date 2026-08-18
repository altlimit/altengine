package channel

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMessageBytes    = 32 * 1024
	maxPresenceMembers = 1000
)

// conn is one client WebSocket connection (many channels per connection).
type conn struct {
	id         string
	ws         *websocket.Conn
	writeMu    sync.Mutex
	allowed    map[string]bool
	subscribed map[string]bool
	pub        string // "", http, ws, all
	presenceID string
	exp        int64
}

func (c *conn) send(msg []byte) bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
		return false
	}
	return true
}

// room holds the subscribers of one (instance, channel).
type room struct {
	conns map[*conn]bool
}

// Hub is the in-process pub/sub topology for all channel instances.
type Hub struct {
	mu    sync.Mutex
	rooms map[string]*room // key: instanceID + ":" + channel
}

func NewHub() *Hub { return &Hub{rooms: map[string]*room{}} }

func roomKey(instanceID, channel string) string { return instanceID + ":" + channel }

// join subscribes a connection to a channel; returns whether presence went 0->1 for
// this member identity in the channel.
func (h *Hub) join(instanceID, channel string, c *conn) (firstForMember bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey(instanceID, channel)
	r := h.rooms[key]
	if r == nil {
		r = &room{conns: map[*conn]bool{}}
		h.rooms[key] = r
	}
	if c.presenceID != "" {
		firstForMember = h.memberCountLocked(r, c.presenceID) == 0
	}
	r.conns[c] = true
	c.subscribed[channel] = true
	return firstForMember
}

// leave unsubscribes a connection; returns whether presence went 1->0 for the member.
func (h *Hub) leave(instanceID, channel string, c *conn) (lastForMember bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey(instanceID, channel)
	r := h.rooms[key]
	if r == nil {
		return false
	}
	delete(r.conns, c)
	delete(c.subscribed, channel)
	if c.presenceID != "" {
		lastForMember = h.memberCountLocked(r, c.presenceID) == 0
	}
	if len(r.conns) == 0 {
		delete(h.rooms, key)
	}
	return lastForMember
}

func (h *Hub) memberCountLocked(r *room, pid string) int {
	n := 0
	for c := range r.conns {
		if c.presenceID == pid {
			n++
		}
	}
	return n
}

// Publish fans a message out to a channel's subscribers, returning the delivered count.
// RoomInfo is one live channel and how many sockets are on it.
type RoomInfo struct {
	Channel     string `json:"channel"`
	Subscribers int    `json:"subscribers"`
}

// Rooms lists an instance's LIVE channels, newest-irrelevant, sorted by name.
//
// "Live" is the whole definition: a channel exists while someone is subscribed to it and stops
// existing when the last socket leaves. There is no registry of channel names because there is
// nothing to register — publishing to a name nobody is listening on is legal and creates
// nothing. That is the same answer hosted gives from its directory.
func (h *Hub) Rooms(instanceID string) []RoomInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := instanceID + ":"
	out := []RoomInfo{}
	for key, r := range h.rooms {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, RoomInfo{Channel: strings.TrimPrefix(key, prefix), Subscribers: len(r.conns)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

func (h *Hub) Publish(instanceID, channel string, data json.RawMessage) (int, error) {
	frame, _ := json.Marshal(map[string]any{
		"channel": channel,
		"data":    rawOrNull(data),
		"ts":      time.Now().UnixMilli(),
	})
	if len(frame) > maxMessageBytes {
		return 0, errMessageTooLarge
	}
	h.mu.Lock()
	r := h.rooms[roomKey(instanceID, channel)]
	var targets []*conn
	if r != nil {
		for c := range r.conns {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	delivered := 0
	for _, c := range targets {
		if c.send(frame) {
			delivered++
		}
	}
	return delivered, nil
}

func rawOrNull(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("null")
	}
	return data
}

// broadcastPresence emits a join/leave event to a channel's subscribers except `except`.
func (h *Hub) broadcastPresence(instanceID, channel, event, id string, except *conn) {
	frame, _ := json.Marshal(map[string]any{
		"type": "presence", "event": event, "channel": channel, "id": id, "ts": time.Now().UnixMilli(),
	})
	h.mu.Lock()
	r := h.rooms[roomKey(instanceID, channel)]
	var targets []*conn
	if r != nil {
		for c := range r.conns {
			if c != except {
				targets = append(targets, c)
			}
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.send(frame)
	}
}

// Member is one presence roster entry.
type Member struct {
	ID          string `json:"id"`
	Connections int    `json:"connections"`
}

// PresenceSnapshot is the server-side presence roster.
type PresenceSnapshot struct {
	Occupancy   int      `json:"occupancy"`
	MemberCount int      `json:"member_count"`
	Members     []Member `json:"members"`
	Truncated   bool     `json:"truncated"`
}

// Presence returns the roster for a channel.
func (h *Hub) Presence(instanceID, channel string) PresenceSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[roomKey(instanceID, channel)]
	snap := PresenceSnapshot{Members: []Member{}}
	if r == nil {
		return snap
	}
	snap.Occupancy = len(r.conns)
	counts := map[string]int{}
	for c := range r.conns {
		if c.presenceID != "" {
			counts[c.presenceID]++
		}
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snap.MemberCount = len(ids)
	for _, id := range ids {
		if len(snap.Members) >= maxPresenceMembers {
			snap.Truncated = true
			break
		}
		snap.Members = append(snap.Members, Member{ID: id, Connections: counts[id]})
	}
	return snap
}
