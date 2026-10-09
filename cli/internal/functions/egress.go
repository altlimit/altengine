// Outbound HTTP under the same rules the hosted proxy applies.
//
// This is here for PARITY, not protection. Locally, a function could reach the network by
// other means and the emulator is not a sandbox anyway (see the package comment). What
// matters is that a fetch which production will refuse also fails here — otherwise the
// first thing a deploy does is break a call that worked all through development.
//
// The rules, kept identical to the hosted service's:
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
	"unicode/utf8"
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
	if hostInPatterns(host, allowed) {
		return nil
	}
	if len(allowed) == 0 {
		return fmt.Errorf("outbound fetch blocked: no outbound allowlist is configured for this instance")
	}
	return fmt.Errorf("outbound fetch blocked: '%s' is not in this instance's outbound allowlist", host)
}

// hostInPatterns reports whether host (lowercased, no trailing dot) matches one of patterns
// in allowlist syntax. Shared by the allowlist and by a secret's host binding, so the two
// can never disagree.
func hostInPatterns(host string, patterns []string) bool {
	for _, entry := range patterns {
		e := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "*.") {
			// A wildcard covers subdomains but NOT the apex — that needs its own entry,
			// so a wildcard cannot quietly widen to the parent.
			base := e[2:]
			if host != base && strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		if host == e {
			return true
		}
	}
	return false
}

// errEgressRefused is a request that names a known secret somewhere it may not go. Its
// message carries the secret's NAME and the rule, never the value.
type errEgressRefused struct{ msg string }

func (e *errEgressRefused) Error() string { return "outbound fetch blocked: " + e.msg }

// expandSecrets substitutes {{NAME}} in headers, and in the one query parameter a secret
// names for itself.
//
// Where a value may go:
//   - only to the secret's own Hosts (a secret stored before binding has none and keeps
//     expanding into headers toward any allowlisted host until it is bound);
//   - headers;
//   - the query string ONLY in the parameter named by QueryParam — a query value is what an
//     API quotes back in its error, and that error body goes to function code;
//   - never the host, scheme, path or body.
//
// A placeholder for a known secret placed anywhere else refuses the whole request
// (*errEgressRefused). Unknown placeholders are left alone. Single pass, so a secret whose
// value contains {{OTHER}} is not re-expanded. Returns the header names written to and the
// values used, both needed once a redirect appears.
func expandSecrets(rawURL string, header http.Header, secrets map[string]Secret) (string, []string, []string, error) {
	if len(secrets) == 0 {
		return rawURL, nil, nil, nil
	}
	host := ""
	u, perr := url.Parse(rawURL)
	if perr == nil {
		host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	toHost := func(name string, s Secret) error {
		if s.Hosts != nil && !hostInPatterns(host, s.Hosts) {
			return &errEgressRefused{fmt.Sprintf("secret '%s' may only be sent to %s, not '%s'", name, strings.Join(s.Hosts, ", "), host)}
		}
		return nil
	}

	used := map[string]bool{}
	var refusal error
	sub := func(s string, permit func(string, Secret) error) string {
		return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
			name := placeholderRe.FindStringSubmatch(m)[1]
			sec, ok := secrets[name]
			if !ok {
				return m // unknown placeholders are left alone, not an error
			}
			if refusal != nil {
				return m
			}
			if err := permit(name, sec); err != nil {
				refusal = err
				return m
			}
			used[sec.Value] = true
			return sec.Value
		})
	}

	var touched []string
	for k, vals := range header {
		for i, v := range vals {
			if !strings.Contains(v, "{{") {
				continue
			}
			if n := sub(v, toHost); n != v {
				header[k][i] = n
				touched = append(touched, strings.ToLower(k))
			}
		}
	}
	if refusal != nil {
		return rawURL, nil, nil, refusal
	}

	out := rawURL
	if perr == nil {
		q := u.Query()
		changed := false
		for k, vals := range q {
			for i, v := range vals {
				if !strings.Contains(v, "{{") {
					continue
				}
				key := k
				n := sub(v, func(name string, s Secret) error {
					if s.QueryParam != key {
						if s.QueryParam != "" {
							return &errEgressRefused{fmt.Sprintf("secret '%s' may only be sent in a request header or the '%s' query parameter, not '%s'", name, s.QueryParam, key)}
						}
						return &errEgressRefused{fmt.Sprintf("secret '%s' may only be sent in a request header; to send it in a query parameter, name that parameter on the secret (queryParam)", name)}
					}
					return toHost(name, s)
				})
				if n != v {
					q[k][i] = n
					changed = true
				}
			}
		}
		if refusal != nil {
			return rawURL, nil, nil, refusal
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
	return out, touched, values, nil
}

// doEgress performs the checked, expanded request, following redirects manually.
func doEgress(rawURL, method string, header http.Header, body []byte, allowed []string, secrets map[string]Secret) (int, http.Header, []byte, error) {
	// Check BEFORE expanding: a blocked destination must never see a substituted value,
	// not even in a request we then refuse to send.
	if err := checkEgress(rawURL, allowed); err != nil {
		return 0, nil, nil, err
	}
	current, secretHeaders, secretValues, err := expandSecrets(rawURL, header, secrets)
	if err != nil {
		return 0, nil, nil, err
	}
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

		next, err := nextHop(loc, current, secretValues)
		if err != nil {
			return 0, nil, nil, err
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

// nextHop resolves a Location into the next request URL, or refuses it.
//
// A host we just sent a secret to can answer with a Location that CONTAINS it, and the next
// hop is a different origin. Refuse rather than sanitize. The match runs against the decoded
// URL too: a value with `+`, `/` or `=` in it arrives percent-encoded (`sk%2Bab%3D`), possibly
// form-encoded or encoded twice, and a raw comparison never sees it.
func nextHop(loc, current string, secretValues []string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", fmt.Errorf("outbound fetch blocked: redirect to an invalid URL")
	}
	nextURL, err := base.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("outbound fetch blocked: redirect to an invalid URL")
	}
	next := nextURL.String()
	if len(secretValues) > 0 {
		readings := urlReadings(next)
		for _, v := range secretValues {
			for _, r := range readings {
				if strings.Contains(r, v) {
					return "", fmt.Errorf("outbound fetch blocked: redirect target would carry a secret to another host")
				}
			}
		}
	}
	return next, nil
}

// decodeRounds bounds the decoding: a hostile host can encode a value twice (`%252B`).
const decodeRounds = 3

var pctRunRe = regexp.MustCompile(`(?:%[0-9a-fA-F]{2})+`)

// urlReadings is raw as written, and percent-decoded — with and without form encoding's `+`
// for space — until it stops changing. Lenient: a run that does not decode to UTF-8 is left
// as written rather than failing the check.
func urlReadings(raw string) []string {
	decode := func(s string) string {
		return pctRunRe.ReplaceAllStringFunc(s, func(run string) string {
			d, err := url.PathUnescape(run)
			if err != nil || !utf8.ValidString(d) {
				return run
			}
			return d
		})
	}
	seen := map[string]bool{raw: true}
	out := []string{raw}
	frontier := []string{raw}
	for round := 0; round < decodeRounds && len(frontier) > 0; round++ {
		var next []string
		for _, s := range frontier {
			for _, d := range []string{decode(s), decode(strings.ReplaceAll(s, "+", " "))} {
				if !seen[d] {
					seen[d] = true
					out = append(out, d)
					next = append(next, d)
				}
			}
		}
		frontier = next
	}
	return out
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
