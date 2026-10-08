package datastore

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

var autoIDs = []string{"uuid", "scattered", "serial", "manual"}

var keyByPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// ValidateConfig is the datastore instance's config check on a write, with the hosted rules.
func ValidateConfig(cfg map[string]any) error {
	if v, present := cfg["autoId"]; present && v != nil {
		s, _ := v.(string)
		ok := false
		for _, a := range autoIDs {
			ok = ok || s == a
		}
		if !ok {
			return common.BadRequest("config.autoId must be one of " + strings.Join(autoIDs, ", "))
		}
	}
	return common.FirstErr(
		common.CheckBool(cfg, "autoIndex", true),
		common.CheckRateLimit(cfg, "rateLimit"),
		common.CheckRegion(cfg),
		validateLiveConfig(cfg["live"]),
	)
}

func validateLiveConfig(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return common.BadRequest("config.live must be an object or null")
	}
	if ci, _ := o["channelInstance"].(string); strings.TrimSpace(ci) == "" {
		return common.BadRequest("config.live.channelInstance (a channel instance name) is required")
	}
	cols, present := o["collections"]
	if !present || cols == nil {
		return nil
	}
	cm, ok := cols.(map[string]any)
	if !ok {
		return common.BadRequest("config.live.collections must be an object keyed by collection")
	}
	for coll, spec := range cm {
		sm, ok := spec.(map[string]any)
		if !ok {
			return common.BadRequest(fmt.Sprintf("config.live.collections['%s'] must be an object", coll))
		}
		if kb, present := sm["keyBy"]; present && kb != nil {
			if s, ok := kb.(string); !ok || !keyByPath.MatchString(s) {
				return common.BadRequest(fmt.Sprintf("config.live.collections['%s'].keyBy must be a field path", coll))
			}
		}
	}
	return nil
}
