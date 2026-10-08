package server

import (
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// An instance's configured rate limit (requests per minute) is enforced here as it is hosted: a
// token bucket per (instance, plane) holding a minute's worth, refilled at rpm/60 per second, and
// a 429 RATE_LIMITED past it. Only the instance's own limit applies; the hosted plan ceiling does
// not. An instance without one is not limited.

type bucket struct {
	tokens float64
	at     time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// take spends one token from key's bucket, reporting whether the request may proceed.
func (l *limiter) take(key string, rpm int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	capacity := float64(rpm)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, at: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(capacity, b.tokens+now.Sub(b.at).Seconds()*capacity/60)
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateTarget names the instance a request is limited against and the config field holding its
// limit, or ok=false for a route that is not limited — the same routes the hosted service limits.
func rateTarget(r *http.Request) (service, instance, plane, field string, ok bool) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method == http.MethodOptions || len(segs) < 2 {
		return "", "", "", "", false
	}
	if segs[0] == "fn" {
		return "functions", segs[1], "functions", "rateLimit", true
	}
	if segs[0] != "v1" || len(segs) < 3 {
		return "", "", "", "", false
	}
	service, instance = segs[1], segs[2]
	switch service {
	case "datastore", "search", "blob", "functions":
		return service, instance, service, "rateLimit", true
	case "container":
		// Hosted, only a launch is limited.
		return service, instance, service, "rateLimit", len(segs) == 3 && r.Method == http.MethodPost
	case "channel":
		if len(segs) > 3 && segs[3] == "subscribe" {
			return service, instance, "connect", "connectRateLimit", true
		}
		return service, instance, "channel", "publishRateLimit", true
	}
	return "", "", "", "", false
}

func rateMW(reg *control.Registry, l *limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service, name, plane, field, ok := rateTarget(r)
		if ok {
			// Never creates an instance: one that does not exist yet has no limit configured.
			if in, _ := reg.ResolveAddressed(service, name); in != nil {
				if rpm, set := common.Int(in.Config()[field]); set && rpm > 0 && !l.take(in.ID+":"+plane, rpm) {
					common.WriteError(w, common.NewError(http.StatusTooManyRequests, "rate limit exceeded — slow down", "RATE_LIMITED"))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
