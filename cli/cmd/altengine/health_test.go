package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if err := checkHealth(ok.URL+"/healthz", time.Second); err != nil {
		t.Fatalf("healthy server: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if err := checkHealth(bad.URL+"/healthz", time.Second); err == nil {
		t.Fatal("a 503 should be unhealthy")
	}

	bad.Close()
	if err := checkHealth(bad.URL+"/healthz", time.Second); err == nil {
		t.Fatal("a server that is not listening should be unhealthy")
	}
}
