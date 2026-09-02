// Package static deploys a built site to a hosted altengine static instance.
//
// THE UPLOAD IS THREE CALLS AND THE MIDDLE ONE DOES NOT TOUCH THE PLATFORM:
//
//  1. send a manifest — every file's path, size and sha256
//  2. PUT the files it says it does not already have, straight to storage
//  3. activate, which points the site at the new deployment
//
// Files are content-addressed, so step 1 is what makes a redeploy cheap: an unchanged asset is
// never uploaded again, and a docs site where one page changed uploads one page. That is also why
// the hashing happens here rather than on the server — the server can only tell you what it is
// missing if you tell it what you have.
//
// Activation is a pointer move, so a rollback is the same call with an older deployment id and
// costs nothing: that deployment's files are still stored.
package static

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is everything needed to reach an instance. Flags win over environment.
type Config struct {
	BaseURL  string // e.g. https://api.altengine.net
	APIKey   string
	Instance string // the static instance name, as shown in the console
}

// File is one file of a site, as walked from disk.
type File struct {
	// Path as it will be served, always rooted: "/index.html".
	Path string
	// Absolute path on disk, so the upload step can read it back without walking again.
	Local string
	Size  int64
	Hash  string
}

type manifestEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Walk hashes every file under root and returns the manifest to send.
//
// NOTHING IS SKIPPED, and dotfiles least of all. A CLI that quietly ignored them would break
// `.well-known/` — ACME challenges, apple-app-site-association, security.txt — which is exactly
// the kind of file whose absence is discovered weeks later by something else failing. If a file
// is in the build output, it is part of the site.
//
// Symlinks are followed when they resolve inside the root and REFUSED when they do not. Following
// one out of the tree would publish whatever it points at, which on a developer's machine is a
// short walk from an SSH key; refusing names the file rather than silently dropping it.
func Walk(root string) ([]File, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file; point this at your build OUTPUT DIRECTORY (e.g. ./dist)", root)
	}

	var out []File
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil {
				return fmt.Errorf("%s is a broken symlink", rel(absRoot, p))
			}
			if !within(absRoot, target) {
				return fmt.Errorf(
					"%s is a symlink pointing outside %s (to %s) — deploying it would publish a file that is not part of your site",
					rel(absRoot, p), root, target)
			}
			ti, err := os.Stat(target)
			if err != nil || ti.IsDir() {
				return nil // a symlinked directory is walked through its own entries
			}
		}

		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", rel(absRoot, p), err)
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", rel(absRoot, p), err)
		}
		out = append(out, File{
			Path:  "/" + filepath.ToSlash(rel(absRoot, p)),
			Local: p,
			Size:  n,
			Hash:  hex.EncodeToString(h.Sum(nil)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty — nothing to deploy", root)
	}
	// Deterministic order, so two runs over the same tree print the same thing.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return r
}

// within reports whether target is inside root. Compared on cleaned absolute paths with a
// trailing separator, so "/site-backup" is not read as being inside "/site".
func within(root, target string) bool {
	r := filepath.Clean(root) + string(filepath.Separator)
	t := filepath.Clean(target)
	return strings.HasPrefix(t+string(filepath.Separator), r)
}

// Manifest turns walked files into the request body's `files` map.
func Manifest(files []File) map[string]manifestEntry {
	m := make(map[string]manifestEntry, len(files))
	for _, f := range files {
		m[f.Path] = manifestEntry{Hash: f.Hash, Size: f.Size}
	}
	return m
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

// Upload is one presigned PUT the client must perform.
type Upload struct {
	Hash            string            `json:"path_hash"`
	Size            int64             `json:"size"`
	URL             string            `json:"upload_url"`
	Method          string            `json:"method"`
	RequiredHeaders map[string]string `json:"required_headers"`
}

// Created is the answer to opening a deployment.
type Created struct {
	DeploymentID string   `json:"deployment_id"`
	FileCount    int      `json:"file_count"`
	TotalBytes   int64    `json:"total_bytes"`
	MissingCount int      `json:"missing_count"`
	Uploads      []Upload `json:"uploads"`
	Cursor       string   `json:"cursor"`
}

type uploadPage struct {
	Uploads []Upload `json:"uploads"`
	Cursor  string   `json:"cursor"`
}

// Deployment is one row of deploy history.
type Deployment struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"created_at"`
	CreatedBy  string `json:"created_by"`
	Status     string `json:"status"`
	FileCount  int    `json:"file_count"`
	TotalBytes int64  `json:"total_bytes"`
	Message    string `json:"message"`
}

// Activated is the answer to publishing a deployment.
type Activated struct {
	DeploymentID string `json:"deployment_id"`
	FileCount    int    `json:"file_count"`
	TotalBytes   int64  `json:"total_bytes"`
	URL          string `json:"url"`
}

// Create opens a deployment and returns the first page of files to upload.
func (c Config) Create(files []File, message string) (*Created, error) {
	body := map[string]any{"files": Manifest(files)}
	if message != "" {
		body["message"] = message
	}
	var out Created
	if err := c.do(http.MethodPost, "/v1/static/"+c.Instance+"/deployments", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadPage fetches the next page of presigned URLs.
func (c Config) UploadPage(deploymentID, cursor string) ([]Upload, string, error) {
	var out uploadPage
	path := fmt.Sprintf("/v1/static/%s/deployments/%s/uploads?cursor=%s", c.Instance, deploymentID, cursor)
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, "", err
	}
	return out.Uploads, out.Cursor, nil
}

// Activate points the site at a deployment. Also the rollback: an older id is the same call.
func (c Config) Activate(deploymentID string) (*Activated, error) {
	var out Activated
	path := fmt.Sprintf("/v1/static/%s/deployments/%s/activate", c.Instance, deploymentID)
	if err := c.do(http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Deployments lists deploy history, newest first, with the live one named.
func (c Config) Deployments(cursor string) ([]Deployment, string, string, error) {
	var out struct {
		Deployments []Deployment `json:"deployments"`
		Cursor      string       `json:"cursor"`
		Active      string       `json:"active"`
		URL         string       `json:"url"`
	}
	path := "/v1/static/" + c.Instance + "/deployments"
	if cursor != "" {
		path += "?cursor=" + cursor
	}
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, "", "", err
	}
	return out.Deployments, out.Cursor, out.Active, nil
}

// Site is what the instance looks like right now.
type Site struct {
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	URL              string `json:"url"`
	ActiveDeployment string `json:"active_deployment"`
	Deployments      int    `json:"deployments"`
	Files            int    `json:"files"`
	StoredBytes      int64  `json:"stored_bytes"`
}

// Info reports where the site lives and which build is answering.
func (c Config) Info() (*Site, error) {
	var s Site
	if err := c.do(http.MethodGet, "/v1/static/"+c.Instance+"/site", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// UploadParallel is how many files are PUT at once. Enough to keep a link busy on a site of
// small assets, low enough not to look like an attack from one machine.
const UploadParallel = 8

// PutAll uploads a page of files. `byHash` maps a content hash to the local file holding those
// bytes — one file may be at several paths, and only one upload is needed for all of them.
//
// Errors are collected and the FIRST is returned rather than aborting mid-flight, because a
// half-uploaded deployment is not a broken state: it was never activated, nothing points at it,
// and the next run re-uses everything that did land.
func PutAll(uploads []Upload, byHash map[string]File, onDone func()) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, UploadParallel)
	)
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, u := range uploads {
		f, ok := byHash[u.Hash]
		if !ok {
			return fmt.Errorf("server asked for a file we do not have (%s) — re-run the deploy", u.Hash[:12])
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(u Upload, f File) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := put(client, u, f); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			if onDone != nil {
				onDone()
			}
		}(u, f)
	}
	wg.Wait()
	return firstErr
}

func put(client *http.Client, u Upload, f File) error {
	body, err := os.Open(f.Local)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", f.Path, err)
	}
	defer body.Close()

	req, err := http.NewRequest(http.MethodPut, u.URL, body)
	if err != nil {
		return err
	}
	// The size is signed into the URL, so it must be sent exactly. Setting ContentLength rather
	// than a header is what stops Go using chunked transfer encoding, which a presigned PUT
	// rejects with a signature error that says nothing about the cause.
	req.ContentLength = f.Size
	for k, v := range u.RequiredHeaders {
		if strings.EqualFold(k, "content-length") {
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("uploading %s: %w", f.Path, err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("uploading %s: %s", f.Path, res.Status)
	}
	return nil
}

// GitDescribe returns a short commit sha for the working directory, or "" when there is no git
// here. Used as the default deployment label: a list of deployments is only useful if the rows
// can be told apart, and nobody types a message on every deploy.
func GitDescribe(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(out))
	dirty := exec.Command("git", "status", "--porcelain")
	dirty.Dir = dir
	if d, err := dirty.Output(); err == nil && len(strings.TrimSpace(string(d))) > 0 {
		// Says the build did not come from a clean tree, which is the thing you want to know when
		// a deployment behaves unlike the commit it claims to be.
		return sha + "-dirty"
	}
	return sha
}
