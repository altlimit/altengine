package altengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// httpDoer is the subset of *http.Client the SDK needs.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// transport is the minimal JSON-over-HTTP layer shared by all service clients.
type transport struct {
	baseURL string
	apiKey  string
	client  httpDoer
	timeout time.Duration
	retry   Retry
}

func newTransport(cfg clientConfig) *transport {
	t := &transport{
		baseURL: strings.TrimRight(cfg.baseURL, "/"),
		apiKey:  cfg.apiKey,
		client:  cfg.httpClient,
		timeout: cfg.timeout,
		retry:   cfg.retry,
	}
	if t.client == nil {
		t.client = &http.Client{}
	}
	if t.timeout == 0 {
		t.timeout = 30 * time.Second
	}
	if t.retry.MaxAttempts == 0 {
		t.retry.MaxAttempts = 3
	}
	if t.retry.BaseDelay == 0 {
		t.retry.BaseDelay = 250 * time.Millisecond
	}
	if t.retry.MaxDelay == 0 {
		t.retry.MaxDelay = 4 * time.Second
	}
	return t
}

type request struct {
	method  string
	path    string
	query   url.Values
	body    any
	headers map[string]string
	// noRetry disables retries for non-idempotent endpoints (transaction,
	// publish — a retry would double-apply/double-deliver).
	noRetry bool
}

// do runs a request with the retry policy and decodes the JSON response into out.
func (t *transport) do(ctx context.Context, req request, out any) error {
	attempts := t.retry.MaxAttempts
	if req.noRetry {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		lastErr = t.once(ctx, req, out)
		if lastErr == nil {
			return nil
		}
		var ae *APIError
		retryable := errors.As(lastErr, &ae) && ae.Retryable()
		if !retryable {
			// Network-level failures (no HTTP response) are also retryable.
			var ue *url.Error
			retryable = errors.As(lastErr, &ue)
		}
		if !retryable || attempt >= attempts {
			return lastErr
		}
		delay := min(t.retry.BaseDelay<<(attempt-1), t.retry.MaxDelay)
		if ae != nil && ae.RetryAfter > delay {
			delay = ae.RetryAfter
		}
		// Full jitter keeps concurrent retries from stampeding in unison.
		delay = time.Duration(float64(delay) * (0.5 + rand.Float64()*0.5))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *transport) once(ctx context.Context, req request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	u := t.baseURL + req.path
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	var body *bytes.Reader
	if req.body != nil {
		b, err := json.Marshal(req.body)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, u, body)
	if err != nil {
		return err
	}
	if t.apiKey != "" {
		hr.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	if req.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.headers {
		hr.Header.Set(k, v)
	}

	res, err := t.client.Do(hr)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return toAPIError(res)
	}
	if out == nil || res.StatusCode == 204 {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func toAPIError(res *http.Response) *APIError {
	ae := &APIError{Code: "INTERNAL", Message: "HTTP " + strconv.Itoa(res.StatusCode), Status: res.StatusCode}
	var envelope struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err == nil && envelope.Error.Code != "" {
		ae.Code = envelope.Error.Code
		if envelope.Error.Message != "" {
			ae.Message = envelope.Error.Message
		}
		ae.Details = envelope.Error.Details
	}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil {
			ae.RetryAfter = time.Duration(secs * float64(time.Second))
		}
	}
	return ae
}

// seg encodes one path segment (instance/index/collection/key names).
func seg(s string) string { return url.PathEscape(s) }

// keyString renders a document key: numeric keys are stored as decimal strings.
func keyString(key any) string {
	switch k := key.(type) {
	case string:
		return k
	case int:
		return strconv.Itoa(k)
	case int64:
		return strconv.FormatInt(k, 10)
	case float64:
		return strconv.FormatFloat(k, 'f', -1, 64)
	case json.Number:
		return k.String()
	default:
		b, _ := json.Marshal(k)
		return strings.Trim(string(b), `"`)
	}
}
