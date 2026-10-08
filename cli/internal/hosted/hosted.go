// Package hosted is the one HTTP client the CLI's hosted-service commands (deploy, functions,
// static, automation) share: the URL check, the bearer header and the error envelope.
package hosted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

// Do sends one JSON request and decodes the JSON answer into out (when non-nil).
func (c Client) Do(method, path string, body any, out any) error {
	if err := CheckURL(c.BaseURL); err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	full := strings.TrimRight(c.BaseURL, "/") + path
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	req, err := http.NewRequest(method, full, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, full, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
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
