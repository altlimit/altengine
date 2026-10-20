package channel

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// How a subscribe upgrade carries its token, as on the hosted service.
//
// A browser offers two subprotocols — `altengine.v1` and `altengine.token.<token>` — and the
// server answers with `altengine.v1`, never the protocol that carries the token. Other
// clients may send `Authorization: Bearer <token>`. `?token=` still works but is deprecated:
// each such upgrade is answered with Deprecation, Sunset and Link headers.

const (
	wsProtocol            = "altengine.v1"
	wsTokenProtocolPrefix = "altengine.token."
	queryTokenSunset      = "Sun, 31 Jan 2027 00:00:00 GMT"
	queryTokenDocs        = "https://altengine.net/docs/channel#token-auth"
)

// Where an upgrade's token came from.
const (
	srcProtocol       = "protocol"
	srcBearerProtocol = "bearer-protocol" // the older `bearer, <token>` subprotocol form
	srcHeader         = "header"
	srcQuery          = "query"
)

type upgradeAuth struct {
	token  string
	source string
	echo   string // subprotocol to answer with ("" for none); never the token
}

func offeredProtocols(r *http.Request) []string {
	var out []string
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// upgradeToken picks the upgrade's token. Precedence: the `altengine.token.` subprotocol,
// the older `bearer, <token>` subprotocol, `Authorization: Bearer` (a channel token, not an
// API key), then `?token=`. A token subprotocol offered without `altengine.v1` is refused:
// there would be nothing safe to echo. A nil result with a nil error means no token at all.
func upgradeToken(r *http.Request) (*upgradeAuth, error) {
	offered := offeredProtocols(r)
	echo := ""
	if contains(offered, wsProtocol) {
		echo = wsProtocol
	}
	for _, p := range offered {
		if strings.HasPrefix(p, wsTokenProtocolPrefix) {
			if echo == "" {
				return nil, common.NewError(400, "offer the "+wsProtocol+" subprotocol alongside "+wsTokenProtocolPrefix+"<token>", "INVALID_ARGUMENT")
			}
			tok := strings.TrimPrefix(p, wsTokenProtocolPrefix)
			if tok == "" {
				return nil, nil
			}
			return &upgradeAuth{token: tok, source: srcProtocol, echo: echo}, nil
		}
	}
	for i, p := range offered {
		if p == "bearer" && i+1 < len(offered) {
			e := echo
			if e == "" {
				e = "bearer"
			}
			return &upgradeAuth{token: offered[i+1], source: srcBearerProtocol, echo: e}, nil
		}
	}
	if m := bearerHeader(r); m != "" && strings.Count(m, ".") == 2 {
		return &upgradeAuth{token: m, source: srcHeader, echo: echo}, nil
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return &upgradeAuth{token: q, source: srcQuery, echo: echo}, nil
	}
	return nil, nil
}

func bearerHeader(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// setQueryTokenDeprecation adds the headers every `?token=` upgrade is answered with.
func setQueryTokenDeprecation(h http.Header) {
	h.Set("Deprecation", "true")
	h.Set("Sunset", queryTokenSunset)
	h.Set("Link", "<"+queryTokenDocs+`>; rel="deprecation"`)
}

// wsConnectFields are the connection fields of a token mint response: `ws_endpoint` +
// `protocols` for `new WebSocket(ws_endpoint, protocols)`, and the deprecated `ws_url`
// (the token in the query string), kept for clients that still open it.
func wsConnectFields(scheme, host, instance, token string) map[string]any {
	endpoint := scheme + "://" + host + "/v1/channel/" + url.PathEscape(instance) + "/subscribe"
	return map[string]any{
		"ws_url":      endpoint + "?token=" + url.QueryEscape(token),
		"ws_endpoint": endpoint,
		"protocols":   []string{wsProtocol, wsTokenProtocolPrefix + token},
	}
}
