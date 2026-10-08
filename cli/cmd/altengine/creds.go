package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/hosted"
	"golang.org/x/term"
)

// keyEnv is the documented API-key variable, the one the SDKs read. legacyKeyEnv is what older
// releases of this CLI read; it still works, so a shell set up for either is fine.
const (
	keyEnv       = "ALTENGINE_API_KEY"
	legacyKeyEnv = "ALTENGINE_KEY"
	// credsEnv overrides where `altengine login` keeps the key.
	credsEnv = "ALTENGINE_CREDENTIALS"
)

// savedCreds is what `altengine login` writes: owner-only, outside any project directory, so a
// key never has to sit in an environment variable, a flag (visible in ps) or shell history.
type savedCreds struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

func credsPath() (string, error) {
	if p := os.Getenv(credsEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user config directory (%v); set %s to a file path", err, credsEnv)
	}
	return filepath.Join(dir, "altengine", "credentials.json"), nil
}

func loadCreds() (savedCreds, error) {
	var c savedCreds
	p, err := credsPath()
	if err != nil {
		return c, nil // no place to have saved one, so none was saved
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("reading %s: %w", p, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s is not valid JSON (%v); run altengine login again", p, err)
	}
	return c, nil
}

// hostedCreds resolves the service URL and API key for a hosted command: a flag, then the
// environment, then what `altengine login` saved, and for the URL finally the hosted service.
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
	if baseURL == "" || apiKey == "" {
		saved, err := loadCreds()
		if err != nil {
			return "", "", err
		}
		if apiKey == "" {
			apiKey = saved.APIKey
			// A saved key goes to the URL it was saved for, unless one was given explicitly.
			if baseURL == "" {
				baseURL = saved.URL
			}
		}
	}
	if baseURL == "" {
		baseURL = hosted.DefaultURL
	}
	if err := hosted.CheckURL(baseURL); err != nil {
		return "", "", err
	}
	if apiKey == "" {
		return "", "", fmt.Errorf("no API key: run `altengine login`, set %s, or pass --key", keyEnv)
	}
	return baseURL, apiKey, nil
}

// loginCmd saves an API key for the hosted commands. The key is read from stdin — without echo
// when stdin is a terminal — never from an argument.
func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	url := fs.String("url", "", "service URL the key is for (default "+hosted.DefaultURL+")")
	_ = fs.Parse(flagsFirst(fs, args))
	base := *url
	if base == "" {
		base = hosted.DefaultURL
	}
	if err := hosted.CheckURL(base); err != nil {
		fail(err)
	}

	var key string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "API key: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fail(err)
		}
		key = string(b)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fail(err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		fail(fmt.Errorf("no key given"))
	}

	p, err := credsPath()
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fail(err)
	}
	b, _ := json.MarshalIndent(savedCreds{URL: base, APIKey: key}, "", "  ")
	if err := common.WriteFileAtomic(p, b, 0o600); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "saved the key for %s to %s\n", base, p)
}

func logoutCmd(args []string) {
	p, err := credsPath()
	if err != nil {
		fail(err)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "removed", p)
}
