// Package deploy pushes a function to a hosted altengine functions instance.
//
// The server does no build and resolves no imports: it stores exactly the one ES module
// it is given. So bundling happens HERE — esbuild flattens the entry file and everything
// it imports into a single module before upload. That keeps the runtime free of a
// package resolver, and means what you tested locally is byte-for-byte what runs.
package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	esbuild "github.com/evanw/esbuild/pkg/api"
)

// Config is everything needed to reach an instance. Flags win over environment.
type Config struct {
	BaseURL  string // e.g. https://api.altengine.net
	APIKey   string
	Instance string // the functions instance name, as shown in the console
}

// Bundle flattens entry and its imports into one ES module.
//
// Targeted at the same runtime the hosted service pins, so a syntax level the sandbox
// cannot parse fails here — on your machine, with a filename and line number — rather
// than as an opaque 500 on the first request after deploy.
func Bundle(entry string, minify bool) (string, error) {
	abs, err := filepath.Abs(entry)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("cannot read %s: %w", entry, err)
	}

	result := esbuild.Build(esbuild.BuildOptions{
		EntryPoints:       []string{abs},
		Bundle:            true,
		Format:            esbuild.FormatESModule,
		Platform:          esbuild.PlatformNeutral,
		Target:            esbuild.ES2022,
		MinifyWhitespace:  minify,
		MinifyIdentifiers: minify,
		MinifySyntax:      minify,
		Write:             false,
		// Conditions/MainFields keep resolution to browser-ish, dependency-light
		// packages; a package reaching for node built-ins will fail loudly here, which
		// is correct — there is no node in the sandbox.
		Conditions: []string{"worker", "browser"},
		LogLevel:   esbuild.LogLevelSilent,
	})
	if len(result.Errors) > 0 {
		var b strings.Builder
		for _, e := range result.Errors {
			if e.Location != nil {
				fmt.Fprintf(&b, "\n  %s:%d:%d: %s", e.Location.File, e.Location.Line, e.Location.Column, e.Text)
			} else {
				fmt.Fprintf(&b, "\n  %s", e.Text)
			}
		}
		return "", fmt.Errorf("bundle failed:%s", b.String())
	}
	if len(result.OutputFiles) == 0 {
		return "", fmt.Errorf("bundle produced no output")
	}
	return string(result.OutputFiles[0].Contents), nil
}

type deployBody struct {
	Name   string            `json:"name"`
	Code   string            `json:"code"`
	Grants map[string]string `json:"grants,omitempty"`
	CPUMs  int               `json:"cpuMs,omitempty"`
	// A POINTER so the three states stay distinct on the wire: nil is omitted (the server
	// keeps the deployed schedules), &[] serializes as [] (clear them), and a non-empty
	// list sets them. A plain []string could not express "clear".
	Schedules   *[]string `json:"schedules,omitempty"`
	SubRequests int       `json:"subRequests,omitempty"`
	Activate    *bool     `json:"activate,omitempty"`
}

// Result is the server's answer to a successful deploy.
type Result struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	Active    bool   `json:"active"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c Config) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Generous: a large bundle over a slow link is a legitimate slow request, and a
	// timeout here would leave the user unsure whether the deploy landed.
	client := &http.Client{Timeout: 60 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode >= 400 {
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Message != "" {
			return fmt.Errorf("%s (%s)", ae.Error.Message, ae.Error.Code)
		}
		return fmt.Errorf("%s %s: %s: %s", method, url, res.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}
	}
	return nil
}

// Deploy uploads code as a new version of the named function.
//
// grants is only sent when non-empty: an omitted grants map INHERITS what the function
// already has, so a plain redeploy of source never silently drops a function's access to
// its datastore.
func (c Config) Deploy(fnName, code string, grants map[string]string, schedules *[]string, activate bool) (*Result, error) {
	body := deployBody{Name: fnName, Code: code, Schedules: schedules}
	if len(grants) > 0 {
		body.Grants = grants
	}
	if !activate {
		body.Activate = &activate
	}
	var r Result
	if err := c.do(http.MethodPost, "/v1/functions/"+c.Instance+"/deploy", body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Listing is what /v1/functions/:instance returns.
type Listing struct {
	Instance  string `json:"instance"`
	Host      string `json:"host"`
	Functions []struct {
		Name          string            `json:"name"`
		ActiveVersion int               `json:"active_version"`
		Grants        map[string]string `json:"grants"`
		Schedules     []string          `json:"schedules"`
		CPUMs         int               `json:"cpu_ms"`
		URL           string            `json:"url"`
	} `json:"functions"`
}

// List reports what is currently deployed.
func (c Config) List() (*Listing, error) {
	var l Listing
	if err := c.do(http.MethodGet, "/v1/functions/"+c.Instance, nil, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// Version is one row of deploy history.
type Version struct {
	Version   int    `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
	// The address that runs this version, `{slug}--v{n}-fn`. Empty when the server has no
	// tenant host configured.
	URL string `json:"url"`
}

// Versions lists deploy history for one function, newest first.
func (c Config) Versions(fnName string) ([]Version, int, error) {
	var out struct {
		Versions      []Version `json:"versions"`
		ActiveVersion int       `json:"active_version"`
	}
	if err := c.do(http.MethodGet, "/v1/functions/"+c.Instance+"/"+fnName+"/versions", nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Versions, out.ActiveVersion, nil
}

// Activate points traffic at an already-deployed version. No upload — a rollback moves a
// pointer, so it is fast and cannot fail halfway.
func (c Config) Activate(fnName string, version int) error {
	return c.do(http.MethodPost, "/v1/functions/"+c.Instance+"/"+fnName+"/activate",
		map[string]any{"version": version}, nil)
}

// Pull reads a deployed version's source back.
func (c Config) Pull(fnName string, version int) (string, error) {
	var out struct {
		Code string `json:"code"`
	}
	path := fmt.Sprintf("/v1/functions/%s/%s/versions/%d/code", c.Instance, fnName, version)
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.Code, nil
}
