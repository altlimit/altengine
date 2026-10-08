package common

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A body over the cap is a 413 that says so. It used to be cut at the cap and reported as
// "invalid JSON body", which sends the reader looking for a syntax error.
func TestReadJSONOverTheCapIs413(t *testing.T) {
	big := append([]byte(`{"a":"`), bytes.Repeat([]byte("x"), MaxJSONBody)...)
	big = append(big, []byte(`"}`)...)
	var v map[string]any
	err := ReadJSON(httptest.NewRequest("POST", "/", bytes.NewReader(big)), &v)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %v, want a 413", err)
	}
	if err := ReadJSON(httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"a":1}`))), &v); err != nil {
		t.Fatalf("a small body: %v", err)
	}
}
