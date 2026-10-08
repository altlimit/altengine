package common

import (
	"fmt"
	"math"
	"strings"
)

// Strict checks for instance config writes, with the hosted service's rules and messages, so a
// config that saves here saves there. The read path stays tolerant; these run only on a write.

// Regions are the placement hints every service accepts. Locally they change nothing.
var Regions = []string{"auto", "wnam", "enam", "sam", "weur", "eeur", "apac", "apac-ne", "apac-se", "oc", "afr", "me"}

// Int reads a JSON number that is a whole number.
func Int(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(f), true
}

// Number reads a JSON number.
func Number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// CheckRateLimit: absent/null, or a positive integer (requests per minute).
func CheckRateLimit(cfg map[string]any, key string) error {
	v, ok := cfg[key]
	if !ok || v == nil {
		return nil
	}
	if n, ok := Int(v); !ok || n < 1 {
		return BadRequest(fmt.Sprintf("config.%s must be a positive integer (requests per minute)", key))
	}
	return nil
}

// CheckRegion: absent/null, or one of Regions.
func CheckRegion(cfg map[string]any) error {
	v, ok := cfg["region"]
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		for _, r := range Regions {
			if s == r {
				return nil
			}
		}
	}
	return BadRequest("config.region must be one of: " + strings.Join(Regions, ", "))
}

// CheckBool: absent or a boolean, and null too when nullOK (the hosted validators differ per field).
func CheckBool(cfg map[string]any, key string, nullOK bool) error {
	v, ok := cfg[key]
	if !ok || (v == nil && nullOK) {
		return nil
	}
	if _, ok := v.(bool); !ok {
		return BadRequest(fmt.Sprintf("config.%s must be a boolean", key))
	}
	return nil
}

// FirstErr returns the first non-nil error.
func FirstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
