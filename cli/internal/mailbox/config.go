package mailbox

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
)

// A mailbox instance's config is `{settings, routes}`, as hosted: every scalar in `settings`, the
// ordered trigger list in `routes`. Tolerant read (defaults win, a config that does not parse
// never takes inbound mail down), strict write (a caller setting something wrong is told so).

const (
	DefaultRetentionDays = 7
	MaxRetentionDays     = 90
	// InboundDailyCeiling is inbound messages per instance per UTC day; also the default. A
	// tenant may only lower it.
	InboundDailyCeiling = 5000

	MaxRoutes           = 20
	MaxActionsPerRoute  = 3
	maxPatternLength    = 200
	MaxWebhookURLLength = 2048
)

// Settings are the instance's scalars.
type Settings struct {
	RateLimit       *int64 `json:"rateLimit"`
	Region          string `json:"region"`
	AcceptMode      string `json:"acceptMode"` // any | registered
	InboundDailyCap int64  `json:"inboundDailyCap"`
	RetentionDays   int64  `json:"retentionDays"`
	SendMode        string `json:"sendMode"`     // off | reply | any
	DailySendCap    *int64 `json:"dailySendCap"` // nil = only the org's cap
	KeepRaw         bool   `json:"keepRaw"`
}

// RouteMatch: every given field must hold. Globs are case-insensitive, `*` and `?`.
type RouteMatch struct {
	To            string `json:"to,omitempty"`
	From          string `json:"from,omitempty"`
	Subject       string `json:"subject,omitempty"`
	HasAttachment *bool  `json:"hasAttachment,omitempty"`
}

// Action is one thing a route does: exactly one of Function, Channel or Webhook is set.
type Action struct {
	Function    string `json:"function,omitempty"`
	Channel     string `json:"channel,omitempty"`
	IncludeText bool   `json:"includeText,omitempty"`
	Webhook     string `json:"webhook,omitempty"`
}

// Route is one entry of the ordered trigger list.
type Route struct {
	Match RouteMatch `json:"match"`
	Do    []Action   `json:"do"`
}

// Config is the parsed instance config.
type Config struct {
	Settings Settings `json:"settings"`
	Routes   []Route  `json:"routes"`
}

var (
	acceptModes = []string{"any", "registered"}
	sendModes   = []string{"off", "reply", "any"}
	fnNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// DefaultSettings is what a new instance starts with.
func DefaultSettings() Settings {
	return Settings{
		Region:          "auto",
		AcceptMode:      "any",
		InboundDailyCap: InboundDailyCeiling,
		RetentionDays:   DefaultRetentionDays,
		SendMode:        "reply",
		KeepRaw:         true,
	}
}

// DefaultConfigMap is the registry's seed config for a new mailbox instance.
func DefaultConfigMap() map[string]any {
	return map[string]any{"settings": settingsMap(DefaultSettings()), "routes": []any{}}
}

// NormalizedConfigMap is a config as the hosted service stores it after a write: every setting
// named with its value or default, the routes as validated. A save is followed by this, so
// get_instance_config reads back the same full shape it does hosted.
func NormalizedConfigMap(raw map[string]any) map[string]any {
	c := ParseConfig(raw)
	if c.Routes == nil {
		c.Routes = []Route{}
	}
	b, _ := json.Marshal(map[string]any{"settings": settingsMap(c.Settings), "routes": c.Routes})
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

func settingsMap(s Settings) map[string]any {
	var rl, cap any
	if s.RateLimit != nil {
		rl = *s.RateLimit
	}
	if s.DailySendCap != nil {
		cap = *s.DailySendCap
	}
	return map[string]any{
		"rateLimit": rl, "region": s.Region, "acceptMode": s.AcceptMode,
		"inboundDailyCap": s.InboundDailyCap, "retentionDays": s.RetentionDays,
		"sendMode": s.SendMode, "dailySendCap": cap, "keepRaw": s.KeepRaw,
	}
}

func oneOf(v any, list []string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	for _, x := range list {
		if x == s {
			return s, true
		}
	}
	return "", false
}

func intIn(v any, lo, hi int64) (int64, bool) {
	n, ok := common.Int(v)
	if !ok || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

const maxSafeInt = 1<<53 - 1

// ParseConfig reads an instance's stored config tolerantly: anything missing or malformed is the
// default, and a route that no longer validates is dropped rather than half-applied.
func ParseConfig(raw map[string]any) Config {
	d := DefaultSettings()
	s := d
	o, _ := raw["settings"].(map[string]any)
	if o != nil {
		if n, ok := intIn(o["rateLimit"], 1, maxSafeInt); ok {
			s.RateLimit = &n
		}
		if r, ok := oneOf(o["region"], common.Regions); ok {
			s.Region = r
		}
		if m, ok := oneOf(o["acceptMode"], acceptModes); ok {
			s.AcceptMode = m
		}
		if n, ok := intIn(o["inboundDailyCap"], 1, InboundDailyCeiling); ok {
			s.InboundDailyCap = n
		}
		if n, ok := intIn(o["retentionDays"], 1, MaxRetentionDays); ok {
			s.RetentionDays = n
		}
		if m, ok := oneOf(o["sendMode"], sendModes); ok {
			s.SendMode = m
		}
		if n, ok := intIn(o["dailySendCap"], 0, maxSafeInt); ok {
			s.DailySendCap = &n
		}
		s.KeepRaw = o["keepRaw"] != false
	}
	routes, err := ValidateRoutes(raw["routes"])
	if err != nil {
		routes = nil
	}
	return Config{Settings: s, Routes: routes}
}

// ValidateConfig is the strict check every config write runs (console PUT, MCP patch), with the
// hosted rules and messages: a value out of range is refused, never clamped.
func ValidateConfig(cfg map[string]any) error {
	if _, err := validateSettings(cfg["settings"]); err != nil {
		return err
	}
	_, err := ValidateRoutes(cfg["routes"])
	return err
}

func validateSettings(input any) (Settings, error) {
	s := DefaultSettings()
	if input == nil {
		return s, nil
	}
	o, ok := input.(map[string]any)
	if !ok {
		return s, common.BadRequest("settings must be an object")
	}
	if v := o["acceptMode"]; v != nil {
		m, ok := oneOf(v, acceptModes)
		if !ok {
			return s, common.BadRequest("acceptMode must be one of: " + strings.Join(acceptModes, ", "))
		}
		s.AcceptMode = m
	}
	if v := o["sendMode"]; v != nil {
		m, ok := oneOf(v, sendModes)
		if !ok {
			return s, common.BadRequest("sendMode must be one of: " + strings.Join(sendModes, ", "))
		}
		s.SendMode = m
	}
	if v := o["inboundDailyCap"]; v != nil {
		n, ok := intIn(v, 1, InboundDailyCeiling)
		if !ok {
			return s, common.BadRequest(fmt.Sprintf("inboundDailyCap must be an integer from 1 to %d", InboundDailyCeiling))
		}
		s.InboundDailyCap = n
	}
	if v := o["retentionDays"]; v != nil {
		n, ok := intIn(v, 1, MaxRetentionDays)
		if !ok {
			return s, common.BadRequest(fmt.Sprintf("retentionDays must be an integer from 1 to %d", MaxRetentionDays))
		}
		s.RetentionDays = n
	}
	if v := o["dailySendCap"]; v != nil {
		n, ok := intIn(v, 0, maxSafeInt)
		if !ok {
			return s, common.BadRequest("dailySendCap must be a non-negative integer, or null for the organization's cap")
		}
		s.DailySendCap = &n
	}
	if v := o["keepRaw"]; v != nil {
		if _, ok := v.(bool); !ok {
			return s, common.BadRequest("keepRaw must be a boolean")
		}
	}
	s.KeepRaw = o["keepRaw"] != false
	if v := o["rateLimit"]; v != nil {
		n, ok := common.Int(v)
		if !ok || n < 1 {
			return s, common.BadRequest("rateLimit must be a positive integer (requests per minute)")
		}
		s.RateLimit = &n
	}
	if err := common.CheckRegion(o); err != nil {
		return s, err
	}
	if r, ok := o["region"].(string); ok {
		s.Region = r
	}
	return s, nil
}

// SplitTarget splits `<instance>/<name>` at the FIRST slash: a channel name may contain slashes.
func SplitTarget(s string) (instance, name string) {
	i := strings.Index(s, "/")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func target(v any, what string, checkName func(string) error) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest(what + " must be a string '<instance>/<name>'")
	}
	s = strings.TrimSpace(s)
	at := strings.Index(s, "/")
	if at < 1 || at == len(s)-1 {
		return "", common.BadRequest(what + " must be '<instance>/<name>'")
	}
	if len(s) > maxPatternLength {
		return "", common.BadRequest(what + " is too long")
	}
	if err := checkName(s[at+1:]); err != nil {
		msg := err.Error()
		if e, ok := err.(*common.APIError); ok {
			msg = e.Message
		}
		return "", common.BadRequest(what + ": " + msg)
	}
	return s, nil
}

func functionName(name string) error {
	if !fnNameRe.MatchString(name) {
		return fmt.Errorf("function name must be 1-63 chars of [a-z0-9_-], starting alphanumeric")
	}
	return nil
}

// ValidateWebhookURL: an https URL with no credentials in it. Kept exactly as hosted — locally
// too, so a route that saves here saves there; point it at an https tunnel to reach a local
// receiver.
func ValidateWebhookURL(v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", common.BadRequest(what + " must be an https URL")
	}
	s = strings.TrimSpace(s)
	if len(s) > MaxWebhookURLLength {
		return "", common.BadRequest(fmt.Sprintf("%s is longer than %d characters", what, MaxWebhookURLLength))
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", common.BadRequest(what + " must be an https URL")
	}
	if u.Scheme != "https" {
		return "", common.BadRequest(what + " must be an https URL")
	}
	if u.User != nil {
		return "", common.BadRequest(what + " may not carry a username or password")
	}
	return s, nil
}

func pattern(v any, what string) (string, error) {
	if v == nil || v == "" {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest(what + " must be a string")
	}
	if len(s) > maxPatternLength {
		return "", common.BadRequest(fmt.Sprintf("%s is longer than %d characters", what, maxPatternLength))
	}
	return strings.ToLower(strings.TrimSpace(s)), nil
}

// ValidateRoutes is the strict check of the route list, also reused by the tolerant read.
func ValidateRoutes(input any) ([]Route, error) {
	if input == nil {
		return []Route{}, nil
	}
	// A typed list (from Go code) is normalized through JSON so one validator serves both.
	list, ok := input.([]any)
	if !ok {
		if _, isRoutes := input.([]Route); isRoutes {
			b, _ := json.Marshal(input)
			_ = json.Unmarshal(b, &list)
		} else {
			return nil, common.BadRequest("routes must be an array")
		}
	}
	if len(list) > MaxRoutes {
		return nil, common.BadRequest(fmt.Sprintf("at most %d routes", MaxRoutes))
	}
	out := make([]Route, 0, len(list))
	for i, r := range list {
		where := fmt.Sprintf("routes[%d]", i)
		ro, ok := r.(map[string]any)
		if !ok {
			return nil, common.BadRequest(where + " must be an object")
		}
		m := map[string]any{}
		if raw, present := ro["match"]; present && raw != nil {
			mm, ok := raw.(map[string]any)
			if !ok {
				return nil, common.BadRequest(where + ".match must be an object")
			}
			m = mm
		}
		var route Route
		if v := m["hasAttachment"]; v != nil {
			b, ok := v.(bool)
			if !ok {
				return nil, common.BadRequest(where + ".match.hasAttachment must be a boolean")
			}
			route.Match.HasAttachment = &b
		}
		var err error
		if route.Match.To, err = pattern(m["to"], where+".match.to"); err != nil {
			return nil, err
		}
		if route.Match.From, err = pattern(m["from"], where+".match.from"); err != nil {
			return nil, err
		}
		if route.Match.Subject, err = pattern(m["subject"], where+".match.subject"); err != nil {
			return nil, err
		}
		do, ok := ro["do"].([]any)
		if !ok || len(do) == 0 {
			return nil, common.BadRequest(where + ".do must be a non-empty array of actions")
		}
		if len(do) > MaxActionsPerRoute {
			return nil, common.BadRequest(fmt.Sprintf("%s.do has more than %d actions", where, MaxActionsPerRoute))
		}
		for j, a := range do {
			aw := fmt.Sprintf("%s.do[%d]", where, j)
			ao, ok := a.(map[string]any)
			if !ok {
				return nil, common.BadRequest(aw + " must be an object")
			}
			kinds := 0
			for _, k := range []string{"function", "channel", "webhook"} {
				if ao[k] != nil {
					kinds++
				}
			}
			if kinds != 1 {
				return nil, common.BadRequest(aw + " must have exactly one of 'function', 'channel' or 'webhook'")
			}
			switch {
			case ao["function"] != nil:
				t, err := target(ao["function"], aw+".function", functionName)
				if err != nil {
					return nil, err
				}
				route.Do = append(route.Do, Action{Function: t})
			case ao["webhook"] != nil:
				u, err := ValidateWebhookURL(ao["webhook"], aw+".webhook")
				if err != nil {
					return nil, err
				}
				route.Do = append(route.Do, Action{Webhook: u})
			default:
				if v := ao["includeText"]; v != nil {
					if _, ok := v.(bool); !ok {
						return nil, common.BadRequest(aw + ".includeText must be a boolean")
					}
				}
				t, err := target(ao["channel"], aw+".channel", channel.ValidateChannel)
				if err != nil {
					return nil, err
				}
				route.Do = append(route.Do, Action{Channel: t, IncludeText: ao["includeText"] == true})
			}
		}
		out = append(out, route)
	}
	return out, nil
}

// GlobMatch: `*` any run, `?` one character, everything else literal, case-insensitive.
//
// NOT a regex. A pattern is tenant input and a sender address is the internet's, and a glob
// compiled to a backtracking regex is a CPU-time denial of service on the inbound path. The
// two-pointer match is O(pattern × value) in the worst case and linear in practice.
func GlobMatch(glob, value string) bool {
	p := []rune(strings.ToLower(glob))
	s := []rune(strings.ToLower(value))
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star = pi
			pi++
			mark = si
		case star != -1:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// MatchInput is what route matching reads off a message.
type MatchInput struct {
	Local           string
	From            string
	Subject         string
	AttachmentCount int
}

// MatchRoute returns the first route whose every given field matches, or nil.
func MatchRoute(routes []Route, m MatchInput) *Route {
	for i := range routes {
		r := &routes[i]
		if r.Match.To != "" && !GlobMatch(r.Match.To, m.Local) {
			continue
		}
		if r.Match.From != "" && !GlobMatch(r.Match.From, m.From) {
			continue
		}
		if r.Match.Subject != "" && !strings.Contains(strings.ToLower(m.Subject), r.Match.Subject) {
			continue
		}
		if r.Match.HasAttachment != nil && *r.Match.HasAttachment != (m.AttachmentCount > 0) {
			continue
		}
		return r
	}
	return nil
}

// ActionKey is the stable key a delivery row is stored under: the whole action, options
// included, so a retry does what the first attempt did even after the routes are edited.
func ActionKey(a Action) string {
	switch {
	case a.Function != "":
		return "function:" + a.Function
	case a.Webhook != "":
		return "webhook:" + a.Webhook
	case a.IncludeText:
		return "channel+text:" + a.Channel
	}
	return "channel:" + a.Channel
}

// ParseActionKey is the action a delivery key names; ok=false for a key this version does not
// understand.
func ParseActionKey(key string) (Action, bool) {
	at := strings.Index(key, ":")
	if at < 1 {
		return Action{}, false
	}
	kind, rest := key[:at], key[at+1:]
	switch kind {
	case "function":
		return Action{Function: rest}, true
	case "channel":
		return Action{Channel: rest}, true
	case "channel+text":
		return Action{Channel: rest, IncludeText: true}, true
	case "webhook":
		return Action{Webhook: rest}, true
	}
	return Action{}, false
}
