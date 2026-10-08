package container

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	maxAllowedImages  = 50
	maxBlobStoreName  = 64
	maxJobCostCeiling = 100
)

var onCompleteRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/[A-Za-z0-9_-]+$`)

// ValidateConfig is the container instance's config check on a write, with the hosted rules:
// a value out of range is refused rather than clamped, so a mistake is reported.
func ValidateConfig(cfg map[string]any) error {
	if v, present := cfg["rateLimit"]; present && v != nil {
		if n, ok := common.Int(v); !ok || n < 1 {
			return common.BadRequest("rateLimit must be a positive integer or null")
		}
	}
	if v, present := cfg["allowedImages"]; present {
		list, ok := v.([]any)
		if !ok {
			return common.BadRequest("allowedImages must be an array of strings")
		}
		for _, i := range list {
			if _, ok := i.(string); !ok {
				return common.BadRequest("allowedImages must be an array of strings")
			}
		}
		if len(list) > maxAllowedImages {
			return common.BadRequest(fmt.Sprintf("at most %d allowed images", maxAllowedImages))
		}
	}
	if n, ok := common.Number(cfg["maxTimeoutMs"]); ok {
		if i, whole := common.Int(n); !whole || i < 1000 || i > MaxTimeoutMS {
			return common.BadRequest(fmt.Sprintf("maxTimeoutMs must be between 1000 and %d", MaxTimeoutMS))
		}
	}
	if n, ok := common.Number(cfg["maxConcurrent"]); ok {
		if i, whole := common.Int(n); !whole || i < 1 || i > MaxConcurrency {
			return common.BadRequest(fmt.Sprintf("maxConcurrent must be between 1 and %d", MaxConcurrency))
		}
	}
	if n, ok := common.Number(cfg["maxJobCostUsd"]); ok && (!(n > 0) || n > maxJobCostCeiling) {
		return common.BadRequest("maxJobCostUsd must be between 0.01 and 100")
	}
	if s := optString(cfg["onComplete"]); s != "" && !onCompleteRe.MatchString(s) {
		return common.BadRequest("onComplete must be '<functions-instance>/<function>'")
	}
	if s := optString(cfg["blobStore"]); s != "" && (len(s) > maxBlobStoreName || strings.ContainsAny(s, " \t\r\n/?#")) {
		return common.BadRequest(fmt.Sprintf("blobStore must be a blob instance name (at most %d characters, no spaces or slashes)", maxBlobStoreName))
	}
	return nil
}

// optString is a config string field as the hosted validator reads it: null or "" is unset.
func optString(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
