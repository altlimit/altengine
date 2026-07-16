package channel

import (
	"regexp"

	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	maxChannelsPerToken = 100
	defaultTTLSeconds   = 3600
	maxTTLSeconds       = 14400
	maxPresenceIDBytes  = 128
)

var (
	channelRe          = regexp.MustCompile(`^[\x20-\x7e]{1,200}$`)
	reservedChannelRe  = regexp.MustCompile(`^__.*__$`)
	presenceIDRe       = regexp.MustCompile(`^[\x20-\x7e]{1,128}$`)
	errMessageTooLarge = common.BadRequest("message exceeds 32768 bytes")
)

func validateChannel(ch string) error {
	if !channelRe.MatchString(ch) || ch[0] == '!' || reservedChannelRe.MatchString(ch) {
		return common.BadRequest("invalid channel name")
	}
	return nil
}

func validateChannelList(v any) ([]string, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil, common.BadRequest("channels must be a non-empty array")
	}
	if len(arr) > maxChannelsPerToken {
		return nil, common.BadRequest("too many channels")
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, common.BadRequest("channels must be strings")
		}
		if err := validateChannel(s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func clampTTL(v any) int64 {
	n, ok := v.(float64)
	if !ok || n <= 0 {
		return defaultTTLSeconds
	}
	if int64(n) > maxTTLSeconds {
		return maxTTLSeconds
	}
	return int64(n)
}

func validatePresenceID(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || !presenceIDRe.MatchString(s) {
		return "", common.BadRequest("invalid presence_id")
	}
	return s, nil
}

// parsePublishMode maps the mint request's `publish` field to a PublishMode string.
func parsePublishMode(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case bool:
		if t {
			return "all", nil
		}
		return "", nil
	case string:
		if t == "http" || t == "ws" || t == "all" {
			return t, nil
		}
	}
	return "", common.BadRequest(`invalid publish value; expected true, false, "http", "ws", or "all"`)
}
