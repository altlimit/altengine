package datastore

import (
	"encoding/json"
	"net/http"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Write triggers: a committed write to a collection an instance's `triggers` config watches
// becomes a task in a queue. The rules, the matching and the queueing belong to the tasks
// service; this package only says what a write did, AFTER it committed, and never waits for or
// hears back from the sink — a write returns exactly what it would without a trigger.
//
// The sink is a narrow interface rather than the tasks package itself: the datastore must not
// depend on the tasks service.

// WriteChange is what one write did to one document. Before is the document it replaced, where
// the write already reads it (row-scoped writes, transactions).
type WriteChange struct {
	Collection string
	Key        string
	Op         string // create | update | delete
	Doc        json.RawMessage
	Before     json.RawMessage
}

// TriggerSink receives committed writes for an instance's trigger rules.
type TriggerSink interface {
	// DatastoreWatches reports whether any rule could match a write to this collection — asked
	// before the write, so a collection nobody watches pays nothing for the bookkeeping.
	DatastoreWatches(in *control.Instance, namespace, collection string) bool
	// DatastoreWrite hands over a committed write's changes. It must not block.
	DatastoreWrite(in *control.Instance, namespace string, changes []WriteChange, writerDepth int)
}

// WithTriggers enables write triggers.
func (h *Handler) WithTriggers(t TriggerSink) *Handler { h.Triggers = t; return h }

func (h *Handler) watches(inst *control.Instance, ns string, collections ...string) bool {
	if h.Triggers == nil {
		return false
	}
	for _, c := range collections {
		if h.Triggers.DatastoreWatches(inst, ns, c) {
			return true
		}
	}
	return false
}

func (h *Handler) fireTriggers(r *http.Request, inst *control.Instance, ns string, changes []WriteChange) {
	if h.Triggers == nil || len(changes) == 0 {
		return
	}
	h.Triggers.DatastoreWrite(inst, ns, changes, common.TriggerDepth(r.Context()))
}

// existingDocs reads the documents a write is about to touch, keyed by their key.
func existingDocs(store *Store, collection string, keys []any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(keys) == 0 {
		return out
	}
	docs, err := store.BatchGet(collection, keys)
	if err != nil {
		return out
	}
	for _, d := range docs {
		out[d.Key] = d.Data
	}
	return out
}
