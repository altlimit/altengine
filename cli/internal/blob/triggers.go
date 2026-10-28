package blob

import (
	"net/http"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Upload and delete triggers: an object becoming real (its bytes arrived) or being removed
// becomes a task in a queue when the instance's `triggers` config matches it. The rules and the
// queueing belong to the tasks service; this package only says what happened, AFTER it happened,
// and never waits for the sink — an upload or delete answers exactly as it would without one.

// EventURLTTL is how long the download link in an upload event stays valid.
const EventURLTTL = time.Hour

// Change is one object event.
type Change struct {
	Op     string // create | delete
	Record *Record
	// Key is the portable handle, `blob:<instance-id>:<id>`.
	Key string
	// URL is a short-lived download link (create only), so a webhook or a job can fetch the bytes
	// with no credential of its own.
	URL          string
	URLExpiresAt int64
}

// TriggerSink receives an instance's object events.
type TriggerSink interface {
	BlobWatches(in *control.Instance) bool
	// BlobChanged hands over one event. It must not block.
	BlobChanged(in *control.Instance, c Change, writerDepth int)
}

// WithTriggers enables upload and delete triggers.
func (h *Handler) WithTriggers(t TriggerSink) *Handler { h.triggers = t; return h }

func (h *Handler) fireCreate(r *http.Request, instanceID string, rec *Record) {
	if h.triggers == nil || rec == nil {
		return
	}
	in := h.reg.GetByID("blob", instanceID)
	if in == nil || !h.triggers.BlobWatches(in) {
		return
	}
	q, exp := h.sig.query(http.MethodGet, in.ID, rec.ID, EventURLTTL, nil)
	h.triggers.BlobChanged(in, Change{
		Op: "create", Record: rec, Key: "blob:" + in.ID + ":" + rec.ID,
		URL: origin(r) + "/_blob/" + in.ID + "/" + rec.ID + "?" + q, URLExpiresAt: exp,
	}, common.TriggerDepth(r.Context()))
}

func (h *Handler) fireDelete(r *http.Request, in *control.Instance, removed []*Record) {
	if h.triggers == nil || len(removed) == 0 || !h.triggers.BlobWatches(in) {
		return
	}
	for _, rec := range removed {
		// A reservation whose bytes never arrived was never an object; deleting it is not an event.
		if rec.Status != "ready" {
			continue
		}
		h.triggers.BlobChanged(in, Change{Op: "delete", Record: rec, Key: "blob:" + in.ID + ":" + rec.ID}, common.TriggerDepth(r.Context()))
	}
}
