package tasks

import (
	"fmt"

	"github.com/altlimit/altengine/cli/internal/common"
)

// ValidateConfig is the tasks instance's config check on a write, with the hosted rules. Queues
// are NOT config: they are rows of their own, created and changed through their own routes,
// because a queue's settings change while tasks run through it.
func ValidateConfig(cfg map[string]any) error {
	if v, present := cfg["retentionDays"]; present && v != nil {
		if n, ok := common.Int(v); !ok || n < 1 || n > MaxRetentionDays {
			return common.BadRequest(fmt.Sprintf("retentionDays must be an integer from 1 to %d", MaxRetentionDays))
		}
	}
	return common.FirstErr(
		common.CheckRateLimit(cfg, "rateLimit"),
		common.CheckRegion(cfg),
	)
}
