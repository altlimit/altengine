package datastore

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/altlimit/altengine/cli/internal/control"
)

// The datastore→channel live bridge. After a COMMITTED write to a collection listed in the
// instance's `live` config, a minimal change event (`{op, keys, ns, collection}` — never a
// document body) is published to the configured channel instance on channel
// `<collection>.<keyBy-value>`, or `<collection>` when the collection has no `keyBy`.
// Subscribers re-fetch the keys through the normal access-controlled read path, so a live
// event can never leak a document the subscriber couldn't have read.
//
// Best-effort by design: the write has already committed and the datastore is the source of
// truth, so a missing or misconfigured channel instance is skipped silently rather than
// failing the write.
//
// The publisher is a narrow interface rather than the channel package itself: the datastore
// must not depend on the channel service.

// LivePublisher publishes one message to a channel instance.
type LivePublisher interface {
	Publish(instanceID, channel string, data json.RawMessage) (int, error)
}

// LiveConfig is an instance's `live` config blob.
type LiveConfig struct {
	ChannelInstance string
	Collections     map[string]string // collection -> keyBy field path ("" = no partition)
}

// parseLive tolerantly reads the `live` key of an instance config blob. Returns nil when
// live publishing isn't configured.
func parseLive(cfg map[string]any) *LiveConfig {
	m, ok := cfg["live"].(map[string]any)
	if !ok {
		return nil
	}
	name, _ := m["channelInstance"].(string)
	if strings.TrimSpace(name) == "" {
		return nil
	}
	out := &LiveConfig{ChannelInstance: strings.TrimSpace(name), Collections: map[string]string{}}
	if colls, ok := m["collections"].(map[string]any); ok {
		for coll, spec := range colls {
			keyBy := ""
			if sm, ok := spec.(map[string]any); ok {
				keyBy, _ = sm["keyBy"].(string)
			}
			out.Collections[coll] = keyBy
		}
	}
	return out
}

// extractKeyBy reads a dot-path scalar out of a document body for the channel partition.
// A missing path or a non-scalar value means that document emits no event.
func extractKeyBy(data json.RawMessage, path string) (string, bool) {
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return "", false
	}
	v := lookupField(doc, path)
	if v == nil {
		return "", false
	}
	s, ok := scalarText(v)
	return s, ok
}

func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	}
	if n, ok := toNumber(v); ok {
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10), true
		}
		return strconv.FormatFloat(n, 'f', -1, 64), true
	}
	return "", false
}

// emitLive publishes change events for a committed write. Never returns an error: the
// write is already durable and a live notification is strictly advisory.
func (h *Handler) emitLive(inst *control.Instance, ns, collection, op string, docs []LiveDoc) {
	if h.Live == nil || len(docs) == 0 {
		return
	}
	live := parseLive(inst.Config)
	if live == nil {
		return
	}
	keyBy, listed := live.Collections[collection]
	if !listed {
		return
	}
	ch := h.Reg.GetOrCreate("channel", live.ChannelInstance)
	if ch == nil {
		return
	}
	// One publish per distinct channel: group the changed keys by their partition.
	groups := map[string][]string{}
	var order []string
	for _, d := range docs {
		channel := collection
		if keyBy != "" {
			part, ok := extractKeyBy(d.Data, keyBy)
			if !ok {
				continue // the keyBy field is absent on this document
			}
			channel = collection + "." + part
		}
		if _, seen := groups[channel]; !seen {
			order = append(order, channel)
		}
		groups[channel] = append(groups[channel], d.Key)
	}
	for _, channel := range order {
		payload, err := json.Marshal(map[string]any{
			"op": op, "keys": groups[channel], "ns": ns, "collection": collection,
		})
		if err != nil {
			continue
		}
		_, _ = h.Live.Publish(ch.ID, channel, payload)
	}
}
