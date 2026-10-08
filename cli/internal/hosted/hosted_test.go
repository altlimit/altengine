package hosted

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The API key rides in a header on every request, so a URL that would carry it in clear text over
// the network is refused before anything is sent.
func TestCheckURL(t *testing.T) {
	ok := []string{
		"https://api.altengine.net",
		"https://example.com:8443/base",
		"http://127.0.0.1:9191",
		"http://localhost:9191",
		"http://[::1]:9191",
		"http://app.localhost",
	}
	bad := []string{
		"http://api.altengine.net",
		"http://192.168.1.10:9191",
		"http://10.0.0.1",
		"ftp://api.altengine.net",
		"api.altengine.net",
		"",
		"http://localhost.example.com",
	}
	for _, u := range ok {
		if err := CheckURL(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestDoRefusesPlainHTTPBeforeSending(t *testing.T) {
	err := Client{BaseURL: "http://api.altengine.net", APIKey: "ae_secret"}.Do(http.MethodGet, "/v1/x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"needs full"}}`))
	}))
	defer srv.Close()
	err := Client{BaseURL: srv.URL, APIKey: "k"}.Do(http.MethodGet, "/x", nil, nil)
	he, ok := err.(*Error)
	if !ok || he.Status != 403 || he.Code != "FORBIDDEN" || err.Error() != "needs full (FORBIDDEN)" {
		t.Fatalf("got %#v", err)
	}
}
