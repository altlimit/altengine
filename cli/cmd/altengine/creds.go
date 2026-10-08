package main

import (
	"fmt"
	"os"

	"github.com/altlimit/altengine/cli/internal/hosted"
)

// keyEnv is the documented API-key variable, the one the SDKs read. legacyKeyEnv is what older
// releases of this CLI read; it still works, so a shell set up for either is fine.
const (
	keyEnv       = "ALTENGINE_API_KEY"
	legacyKeyEnv = "ALTENGINE_KEY"
)

// hostedCreds resolves the service URL and API key for a hosted command. A flag wins over the
// environment, so a scripted call can override an ambient key.
func hostedCreds(urlFlag, keyFlag string) (baseURL, apiKey string, err error) {
	baseURL, apiKey = urlFlag, keyFlag
	if baseURL == "" {
		baseURL = os.Getenv("ALTENGINE_URL")
	}
	if apiKey == "" {
		apiKey = os.Getenv(keyEnv)
	}
	if apiKey == "" {
		apiKey = os.Getenv(legacyKeyEnv)
	}
	if baseURL == "" {
		return "", "", fmt.Errorf("no service URL: pass --url or set ALTENGINE_URL")
	}
	if err := hosted.CheckURL(baseURL); err != nil {
		return "", "", err
	}
	if apiKey == "" {
		return "", "", fmt.Errorf("no API key: pass --key or set %s", keyEnv)
	}
	return baseURL, apiKey, nil
}
