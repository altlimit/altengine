// Package hosted is the one HTTP client the CLI's hosted-service commands (deploy, functions,
// static, automation) share: the URL check, the bearer header, the error envelope and retries.
package hosted

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the hosted service's API origin.
const DefaultURL = "https://api.altengine.net"

// CheckURL refuses a base URL the API key must not be sent to: anything but https, except plain
// http to this machine (an emulator or a local proxy).
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid service URL %q: expected e.g. %s", raw, DefaultURL)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("refusing %s: http:// would send your API key unencrypted — use https:// (plain http is allowed only for localhost)", raw)
	}
	return fmt.Errorf("invalid service URL %q: the scheme must be https", raw)
}

// IsLoopback reports whether host names this machine.
func IsLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Short returns at most n bytes of an id or hash for display; a shorter one is returned whole
// rather than panicking.
func Short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Seg escapes one path segment. Names come from flags and arguments, and a `/` or `?` in one must
// not address a different route.
func Seg(s string) string { return url.PathEscape(s) }

// Client calls the hosted API with an org API key.
type Client struct {
	BaseURL string
	APIKey  string
	// Timeout per request. Generous by default: a large bundle on a slow link is a legitimately
	// slow request, and a timeout leaves someone unsure whether it landed.
	Timeout time.Duration
}

// Error is the hosted service's error envelope.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Code)
	}
	return e.Message
}

// Retries: attempts in all, the first backoff (doubled each time), and the sleep between them
// (replaced in tests).
var (
	MaxAttempts = 4
	BaseBackoff = 500 * time.Millisecond
	Sleep       = time.Sleep
)

// Idempotent reports whether a request with this method may be repeated after an ambiguous
// failure. A POST may deploy twice, so it is not.
func Idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// Retryable reports whether an answer is worth another attempt. 429 and 503 turn a request away
// before it is acted on, so any method retries them; 502 and 504 may come after it was acted on,
// so only a request that is safe to repeat does.
func Retryable(status int, idempotent bool) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return idempotent
	}
	return false
}

// NeverSent reports whether a transport error happened before the request reached the server
// (no connection, no address), which makes a retry safe whatever the method.
func NeverSent(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// Backoff is the wait before attempt+1: BaseBackoff doubled per attempt, or the server's
// Retry-After in seconds, capped at 30s.
func Backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return time.Duration(min(s, 30)) * time.Second
	}
	return BaseBackoff << (attempt - 1)
}

// Do sends one JSON request and decodes the JSON answer into out (when non-nil). A failure the
// service cannot have acted on, or one that is safe to repeat, is retried with backoff.
func (c Client) Do(method, path string, body any, out any) error {
	if err := CheckURL(c.BaseURL); err != nil {
		return err
	}
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	full := strings.TrimRight(c.BaseURL, "/") + path
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	idem := Idempotent(method)
	for attempt := 1; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, full, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			if attempt < MaxAttempts && (idem || NeverSent(err)) {
				Sleep(Backoff(attempt, ""))
				continue
			}
			return fmt.Errorf("%s %s: %w", method, full, err)
		}
		raw, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		if attempt < MaxAttempts && (Retryable(res.StatusCode, idem) || (rerr != nil && idem)) {
			Sleep(Backoff(attempt, res.Header.Get("Retry-After")))
			continue
		}
		if rerr != nil && res.StatusCode < 400 {
			return fmt.Errorf("%s %s: reading the response: %w", method, full, rerr)
		}
		return decode(method, full, res, raw, out)
	}
}

func decode(method, full string, res *http.Response, raw []byte, out any) error {
	if res.StatusCode >= 400 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
			return &Error{Status: res.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
		}
		return &Error{Status: res.StatusCode, Message: fmt.Sprintf("%s %s: %s: %s", method, full, res.Status, strings.TrimSpace(string(raw)))}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}
	}
	return nil
}
