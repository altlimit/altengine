// Package common holds cross-service primitives: the API error envelope, JSON
// request/response helpers, and small id/crypto utilities. Error shapes match the
// hosted altengine API exactly, so client SDKs written against production behave
// identically against the emulator.
package common

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// APIError is the emulator's structured API error. The global handler
// serializes it to {"error":{"code","message","details?}} with the given HTTP status.
type APIError struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// NewError builds an APIError. Code defaults to INVALID_ARGUMENT when empty.
func NewError(status int, message, code string) *APIError {
	if code == "" {
		code = "INVALID_ARGUMENT"
	}
	return &APIError{Status: status, Code: code, Message: message}
}

func (e *APIError) WithDetails(d map[string]any) *APIError { e.Details = d; return e }

// Common constructors matching the hosted API's status/code pairs.
func BadRequest(msg string) *APIError {
	return NewError(http.StatusBadRequest, msg, "INVALID_ARGUMENT")
}
func Unauthenticated(msg string) *APIError {
	return NewError(http.StatusUnauthorized, msg, "UNAUTHENTICATED")
}
func PermissionDenied(msg string) *APIError {
	return NewError(http.StatusForbidden, msg, "PERMISSION_DENIED")
}
func NotFound(msg string) *APIError { return NewError(http.StatusNotFound, msg, "NOT_FOUND") }
func AlreadyExists(msg string) *APIError {
	return NewError(http.StatusConflict, msg, "ALREADY_EXISTS")
}
func Precondition(msg string) *APIError {
	return NewError(http.StatusConflict, msg, "PRECONDITION_FAILED")
}

// WriteError renders any error as the standard envelope. Non-APIError becomes a 500 INTERNAL.
func WriteError(w http.ResponseWriter, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		ae = NewError(http.StatusInternalServerError, "internal error", "INTERNAL")
	}
	if ae.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	body := map[string]any{"error": map[string]any{"code": ae.Code, "message": ae.Message}}
	if ae.Details != nil {
		body["error"].(map[string]any)["details"] = ae.Details
	}
	WriteJSON(w, ae.Status, body)
}

// HandlerFunc is an http handler that returns an error, rendered via WriteError.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Wrap adapts a HandlerFunc to http.HandlerFunc, writing the error envelope on failure.
func Wrap(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, err)
		}
	}
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ReadJSON decodes the request body into v, returning a 400 on malformed JSON.
// An empty body decodes to the zero value (the hosted API is tolerant the same way).
func ReadJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return BadRequest("could not read request body")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return BadRequest("invalid JSON body")
	}
	return nil
}

// versionedLabel is `{name}--v{n}`. A version is a positive integer without leading zeros, so each
// version has exactly one address.
var versionedLabel = regexp.MustCompile(`^(.+)--v([1-9][0-9]{0,8})$`)

// ParseVersionedName splits `myapp--v3` into ("myapp", 3). Hosted this is the instance label of a
// `{slug}--v{n}-{fn|web}` hostname; locally it is the instance segment of a public path, which
// stands in for that hostname. ok is false for anything that is not exactly that shape — `v0`,
// `v03`, a bare `my--app`.
func ParseVersionedName(label string) (base string, version int, ok bool) {
	m := versionedLabel.FindStringSubmatch(label)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

// RandID returns n bytes of randomness as base64url (no padding).
func RandID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// UUID returns a random RFC-4122 v4 UUID string.
func UUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Base62 returns a base62 string of length n (used for search doc ids).
func Base62(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
