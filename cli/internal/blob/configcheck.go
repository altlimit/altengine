package blob

import (
	"fmt"

	"github.com/altlimit/altengine/cli/internal/common"
)

// ValidateConfig is the blob instance's config check on a write, with the hosted rules.
func ValidateConfig(cfg map[string]any) error {
	if v, present := cfg["maxObjectBytes"]; present && v != nil {
		n, ok := common.Int(v)
		if !ok || n < 1 {
			return common.BadRequest("maxObjectBytes must be a positive integer (bytes)")
		}
		if n > MaxObjectBytesCeiling {
			return common.BadRequest(fmt.Sprintf("maxObjectBytes must be <= %d (storage's maximum object size)", MaxObjectBytesCeiling))
		}
	}
	return common.FirstErr(
		common.CheckBool(cfg, "defaultPublic", true),
		common.CheckRateLimit(cfg, "rateLimit"),
		common.CheckRegion(cfg),
	)
}
