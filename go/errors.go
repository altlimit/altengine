package altengine

import (
	"encoding/json"
	"fmt"
	"time"
)

// APIError is a structured altengine API error: the {error:{code,message,details?}}
// envelope plus transport context (HTTP status, Retry-After). Match with
// errors.As:
//
//	var ae *altengine.APIError
//	if errors.As(err, &ae) && ae.Code == "NOT_FOUND" { ... }
type APIError struct {
	// Code is the machine-readable error code (NOT_FOUND, INVALID_ARGUMENT,
	// UNAUTHENTICATED, PERMISSION_DENIED, RATE_LIMITED, QUERY_TOO_COMPLEX,
	// INDEX_REQUIRED, DOCUMENT_TOO_LARGE, INDEX_FULL, ALREADY_EXISTS,
	// PRECONDITION_FAILED, INTERNAL, ...). The set is open — new server codes
	// must not break clients.
	Code    string
	Message string
	Status  int
	// Details is the raw JSON of error.details when present (e.g. the
	// suggested_index of an INDEX_REQUIRED error).
	Details json.RawMessage
	// RetryAfter is the Retry-After header as a duration (0 when absent).
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("altengine: %s (%d): %s", e.Code, e.Status, e.Message)
}

// Retryable reports whether the request can be safely retried after a backoff.
// 429 always carries Retry-After; 502/503/504 are transient. 507 INDEX_FULL is
// NOT retryable.
func (e *APIError) Retryable() bool {
	return e.Status == 429 || e.Status == 502 || e.Status == 503 || e.Status == 504
}

// IsNotFound reports whether err is an APIError with HTTP status 404.
func IsNotFound(err error) bool {
	if ae, ok := err.(*APIError); ok {
		return ae.Status == 404
	}
	return false
}
