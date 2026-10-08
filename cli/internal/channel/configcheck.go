package channel

import "github.com/altlimit/altengine/cli/internal/common"

// ValidateConfig is the channel instance's config check on a write, with the hosted rules.
func ValidateConfig(cfg map[string]any) error {
	return common.FirstErr(
		common.CheckBool(cfg, "presence", false),
		common.CheckRateLimit(cfg, "publishRateLimit"),
		common.CheckRateLimit(cfg, "connectRateLimit"),
		common.CheckRegion(cfg),
	)
}
