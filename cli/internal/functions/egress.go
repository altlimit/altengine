// Outbound HTTP under the same rules the hosted proxy applies.
//
// This is here for PARITY, not protection. Locally, a function could reach the network by
// other means and the emulator is not a sandbox anyway (see the package comment). What
// matters is that a fetch which production will refuse also fails here — otherwise the
// first thing a deploy does is break a call that worked all through development.
//
// The rules, kept identical to src/functions/egress.ts:
//   - https only
//   - no IP literals (one rule that kills metadata endpoints, loopback and private ranges)
//   - no non-public names (localhost, .internal, .local)
//   - wildcards match a label boundary and NOT the apex
//   - redirects are re-checked at every hop, credentials stripped
//   - `{{SECRET}}` expands only AFTER the destination is approved

package functions

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxRedirects = 3

// Below this length a secret value matches by chance too often for the redirect-leak
// check to be anything but noise.
const minLeakableLen = 8

var placeholderRe = regexp.MustCompile(`\{\{([A-Z][A-Z0-9_]{0,63})\}\}`)

var egressClient = &http.Client{
	Timeout: 20 * time.Second,
	// Manual redirect handling: letting the client follow would apply the allowlist to
	// the FIRST url only, and an allowlisted host answering 302 is the standard way
	// around a check like this.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// checkEgress decides whether user code may fetch rawURL.
func checkEgress(rawURL string, allowed []string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("outbound fetch blocked: not a valid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("outbound fetch blocked: only https is allowed (got %s)", nonEmpty(u.Scheme, "no scheme"))
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("outbound fetch blocked: no host")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("outbound fetch blocked: IP addresses are not allowed; use a hostname")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("outbound fetch blocked: '%s' is not a public hostname", host)
	}
	for _, entry := range allowed {
		e := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "*.") {
			// A wildcard covers subdomains but NOT the apex — that needs its own entry,
			// so a wildcard cannot quietly widen to the parent.
			base := e[2:]
			if host != base && strings.HasSuffix(host, "."+base) {
				return nil
			}
			continue
		}
		if host == e {
			return nil
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("outbound fetch blocked: no outbound allowlist is configured for this instance")
	}
	return fmt.Errorf("outbound fetch blocked: '%s' is not in this instance's outbound allowlist", host)
}

// expandSecrets substitutes {{NAME}} in headers and query VALUES.
//
// Never the host, scheme or path, so an expansion cannot move the request to a different
// origin than the one just approved. Single pass, so a secret whose value contains
// {{OTHER}} is not re-expanded. Returns the header names written to and the values used,
// both needed once a redirect appears.
func expandSecrets(rawURL string, header http.Header, secrets map[string]string) (string, []string, []string) {
	if len(secrets) == 0 {
		return rawURL, nil, nil
	}
	used := map[string]bool{}
	sub := func(s string) string {
		return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
			name := placeholderRe.FindStringSubmatch(m)[1]
			v, ok := secrets[name]
			if !ok {
				return m // unknown placeholders are left alone, not an error
			}
			used[v] = true
			return v
		})
	}

	var touched []string
	for k, vals := range header {
		for i, v := range vals {
			if !strings.Contains(v, "{{") {
				continue
			}
			if n := sub(v); n != v {
				header[k][i] = n
				touched = append(touched, strings.ToLower(k))
			}
		}
	}

	out := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		q := u.Query()
		changed := false
		for k, vals := range q {
			for i, v := range vals {
				if !strings.Contains(v, "{{") {
					continue
				}
				if n := sub(v); n != v {
					q[k][i] = n
					changed = true
				}
			}
		}
		// Only rebuild when something changed: re-encoding normalizes the whole query,
		// which breaks signature-sensitive APIs on requests that had no placeholder.
		if changed {
			u.RawQuery = q.Encode()
			out = u.String()
		}
	}

	values := make([]string, 0, len(used))
	for v := range used {
		if len(v) >= minLeakableLen {
			values = append(values, v)
		}
	}
	return out, touched, values
}

// doEgress performs the checked, expanded request, following redirects manually.
func doEgress(rawURL, method string, header http.Header, body []byte, allowed []string, secrets map[string]string) (int, http.Header, []byte, error) {
	// Check BEFORE expanding: a blocked destination must never see a substituted value,
	// not even in a request we then refuse to send.
	if err := checkEgress(rawURL, allowed); err != nil {
		return 0, nil, nil, err
	}
	current, secretHeaders, secretValues := expandSecrets(rawURL, header, secrets)
	if err := checkEgress(current, allowed); err != nil {
		return 0, nil, nil, err
	}

	for hop := 0; ; hop++ {
		req, err := http.NewRequest(method, current, bytes.NewReader(body))
		if err != nil {
			return 0, nil, nil, fmt.Errorf("outbound fetch blocked: %v", err)
		}
		req.Header = header.Clone()
		res, err := egressClient.Do(req)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("outbound fetch failed: %v", err)
		}
		loc := ""
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			loc = res.Header.Get("Location")
		}
		if loc == "" {
			defer res.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
			return res.StatusCode, res.Header, b, nil
		}
		res.Body.Close()
		if hop >= maxRedirects {
			return 0, nil, nil, fmt.Errorf("outbound fetch blocked: too many redirects")
		}

		base, _ := url.Parse(current)
		nextURL, err := base.Parse(loc)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("outbound fetch blocked: redirect to an invalid URL")
		}
		next := nextURL.String()
		// A host we just sent a secret to can answer with a Location containing it, and
		// the next hop is a different origin. Refuse rather than sanitize.
		for _, v := range secretValues {
			if strings.Contains(next, v) {
				return 0, nil, nil, fmt.Errorf("outbound fetch blocked: redirect target would carry a secret to another host")
			}
		}
		if err := checkEgress(next, allowed); err != nil {
			return 0, nil, nil, err
		}
		// A redirected request is a GET with no body, and drops every credential the
		// caller set — including any header a secret was expanded into.
		method, body, current = http.MethodGet, nil, next
		header = stripSensitive(header, secretHeaders)
	}
}

func stripSensitive(h http.Header, extra []string) http.Header {
	out := h.Clone()
	for _, k := range append([]string{"authorization", "cookie", "proxy-authorization"}, extra...) {
		out.Del(k)
	}
	return out
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// parseURL backs the JS URL class, so the emulator agrees with a real parser.
func parseURL(raw, base string) (map[string]any, error) {
	var u *url.URL
	var err error
	if base != "" {
		b, berr := url.Parse(base)
		if berr != nil {
			return nil, fmt.Errorf("Invalid base URL")
		}
		u, err = b.Parse(raw)
	} else {
		u, err = url.Parse(raw)
	}
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("Invalid URL: %s", raw)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	hash := ""
	if u.Fragment != "" {
		hash = "#" + u.Fragment
	}
	pw, _ := u.User.Password()
	return map[string]any{
		"protocol": u.Scheme + ":",
		"hostname": u.Hostname(),
		"port":     u.Port(),
		"host":     u.Host,
		"origin":   u.Scheme + "://" + u.Host,
		"pathname": path,
		"search":   u.RawQuery,
		"hash":     hash,
		"username": u.User.Username(),
		"password": pw,
	}, nil
}

func b64encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func b64decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("InvalidCharacterError")
	}
	return string(b), nil
}
