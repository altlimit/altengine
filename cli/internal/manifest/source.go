package manifest

// Where a manifest and the files it names come from.
//
// Hosted, a manifest is an https URL and every file it names is fetched under the egress rules a
// tenant's function lives under. Locally the manifest is one of two things:
//
//   - a file path. Relative URLs in it (`./dist/web.mjs`, a site's `base`) resolve against the
//     file's directory, and are read from disk;
//   - an http(s) URL. Relative URLs resolve against the URL the bytes came from after redirects,
//     as hosted. Plain http is allowed only to this machine (a dev server serving the build),
//     the local form of hosted's development-only loopback exception.
//
// Either way every module and site file is checked against the sha256 (and, for a site file, the
// size) the manifest gives, and a mismatch stops the deploy, as hosted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/altlimit/altengine/cli/internal/common"
)

const (
	maxRedirects   = 3
	fetchTimeout   = 30 * time.Second
	maxModuleBytes = 5 << 20  // functions.MaxCodeBytes, as hosted
	maxSiteFile    = 25 << 20 // hosted default maxFileBytes
)

// Source is a loaded manifest: where it came from, its bytes and its decoded JSON.
type Source struct {
	// Ref is the manifest's identity: an absolute file path or the URL as given.
	Ref string
	// Host is what a change of source is judged by: the URL's host, or "local" for a file.
	Host  string
	Local bool
	dir   string   // a file manifest's directory
	base  *url.URL // a URL manifest's final URL
	SHA   string
	JSON  any
	Query map[string]string
}

// location is one file a manifest names: on disk, or at a URL.
type location struct {
	path string
	url  *url.URL
}

func (l location) String() string {
	if l.url != nil {
		return l.url.String()
	}
	return l.path
}

func fetchErr(where, reason string) error {
	return common.NewError(422, fmt.Sprintf("could not fetch %s: %s", where, reason), "FETCH_FAILED")
}

func isLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// urlProblem is why a URL will not be fetched, or "".
func urlProblem(u *url.URL) string {
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "plain http is fetched only from this machine (localhost, 127.0.0.1, [::1]); use https"
		}
	default:
		return fmt.Sprintf("only http(s) URLs can be fetched (got %s)", u.Scheme)
	}
	if u.User != nil {
		return "URLs with credentials in them are not fetched"
	}
	if u.Host == "" {
		return "not a valid URL"
	}
	return ""
}

// IsURL reports whether a manifest argument is a URL rather than a file path.
func IsURL(ref string) bool {
	l := strings.ToLower(ref)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

func (d *Deployer) client() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			if p := urlProblem(req.URL); p != "" {
				return fmt.Errorf("%s: %s", req.URL, p)
			}
			// No credentials on any hop.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

// fetchURL GETs a URL under the rules above, reading at most max bytes.
func (d *Deployer) fetchURL(ctx context.Context, u *url.URL, max int64) ([]byte, *url.URL, error) {
	if p := urlProblem(u); p != "" {
		return nil, nil, common.NewError(400, fmt.Sprintf("%s: %s", u, p), "URL_REFUSED")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, fetchErr(u.String(), err.Error())
	}
	req.Header.Set("User-Agent", "altengine-deploy")
	res, err := d.client().Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return nil, nil, fetchErr(u.String(), "timed out")
		}
		return nil, nil, fetchErr(u.String(), err.Error())
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fetchErr(res.Request.URL.String(), fmt.Sprintf("HTTP %d", res.StatusCode))
	}
	if res.ContentLength > max {
		return nil, nil, fetchErr(u.String(), fmt.Sprintf("larger than %d bytes", max))
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, nil, fetchErr(u.String(), err.Error())
	}
	if int64(len(b)) > max {
		return nil, nil, fetchErr(u.String(), fmt.Sprintf("larger than %d bytes", max))
	}
	return b, res.Request.URL, nil
}

func readFileCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fetchErr(path, "no such file")
		}
		return nil, fetchErr(path, err.Error())
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fetchErr(path, err.Error())
	}
	if info.IsDir() {
		return nil, fetchErr(path, "is a directory")
	}
	if info.Size() > max {
		return nil, fetchErr(path, fmt.Sprintf("larger than %d bytes", max))
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fetchErr(path, err.Error())
	}
	if int64(len(b)) > max {
		return nil, fetchErr(path, fmt.Sprintf("larger than %d bytes", max))
	}
	return b, nil
}

// LoadSource reads a manifest from a file path or a URL.
func (d *Deployer) LoadSource(ctx context.Context, ref string) (*Source, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, common.BadRequest("manifest is required: a file path or an http(s) URL")
	}
	src := &Source{Query: map[string]string{}}
	var b []byte
	if IsURL(ref) {
		u, err := url.Parse(ref)
		if err != nil {
			return nil, common.NewError(400, fmt.Sprintf("manifest: '%s' is not a valid URL", ref), "URL_REFUSED")
		}
		got, final, err := d.fetchURL(ctx, u, int64(Limits.Bytes))
		if err != nil {
			return nil, err
		}
		b = got
		src.Ref, src.Host, src.base = ref, u.Host, final
		for k, v := range u.Query() {
			if len(v) > 0 {
				src.Query[k] = v[0]
			}
		}
	} else {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return nil, common.BadRequest(err.Error())
		}
		got, err := readFileCapped(abs, int64(Limits.Bytes))
		if err != nil {
			return nil, err
		}
		b = got
		src.Ref, src.Host, src.Local, src.dir = abs, "local", true, filepath.Dir(abs)
	}
	if !utf8.Valid(b) {
		return nil, common.NewError(422, fmt.Sprintf("%s is not a JSON document", ref), "INVALID_MANIFEST")
	}
	v, err := DecodeJSON(b)
	if err != nil {
		return nil, common.NewError(422, fmt.Sprintf("%s is not a JSON document", ref), "INVALID_MANIFEST")
	}
	sum := sha256.Sum256(b)
	src.SHA, src.JSON = hex.EncodeToString(sum[:]), v
	return src, nil
}

// locate resolves a manifest-relative (or absolute) reference.
func (s *Source) locate(ref string) (location, error) {
	if IsURL(ref) {
		u, err := url.Parse(ref)
		if err != nil {
			return location{}, common.NewError(400, fmt.Sprintf("'%s' is not a valid URL", ref), "INVALID_MANIFEST")
		}
		return location{url: u}, nil
	}
	if s.Local {
		p := ref
		if i := strings.IndexAny(p, "?#"); i >= 0 {
			p = p[:i]
		}
		p, err := url.PathUnescape(p)
		if err != nil {
			return location{}, common.NewError(400, fmt.Sprintf("'%s' is not a valid path", ref), "INVALID_MANIFEST")
		}
		if strings.HasPrefix(p, "/") {
			// Root-relative: against the manifest's directory, never the filesystem root.
			p = strings.TrimLeft(p, "/")
		}
		return location{path: filepath.Join(s.dir, filepath.FromSlash(p))}, nil
	}
	rel, err := url.Parse(ref)
	if err != nil {
		return location{}, common.NewError(400, fmt.Sprintf("'%s' is not a valid URL", ref), "INVALID_MANIFEST")
	}
	return location{url: s.base.ResolveReference(rel)}, nil
}

// child resolves a site path (no leading slash) under a base location.
func (l location) child(rel string) location {
	if l.url != nil {
		return location{url: l.url.ResolveReference(&url.URL{Path: rel})}
	}
	return location{path: filepath.Join(l.path, filepath.FromSlash(rel))}
}

// read returns a location's bytes, at most max.
func (d *Deployer) read(ctx context.Context, l location, max int64) ([]byte, error) {
	if l.url != nil {
		b, _, err := d.fetchURL(ctx, l.url, max)
		return b, err
	}
	return readFileCapped(l.path, max)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashMismatch(msg string) error { return common.NewError(422, msg, "HASH_MISMATCH") }

// readModule reads a function's code and checks it against the manifest.
func (d *Deployer) readModule(ctx context.Context, s *Source, fn string, m ModuleRef) (string, error) {
	if m.Zip != "" {
		return "", common.NewError(400, fmt.Sprintf("function %s: modules inside an archive are not supported yet", fn), "NOT_SUPPORTED")
	}
	loc, err := s.locate(m.URL)
	if err != nil {
		return "", err
	}
	b, err := d.read(ctx, loc, maxModuleBytes)
	if err != nil {
		return "", err
	}
	if sha256Hex(b) != m.SHA256 {
		return "", hashMismatch(fmt.Sprintf("function %s: %s does not match the sha256 in the manifest", fn, loc))
	}
	if !utf8.Valid(b) {
		return "", common.NewError(422, fmt.Sprintf("function %s: %s is not UTF-8 text", fn, loc), "INVALID_MODULE")
	}
	return string(b), nil
}

// siteBase is where a site's files are read from: its `base`, as a directory.
func (s *Source) siteBase(site *SiteDecl) (location, error) {
	b := site.Base
	if !strings.HasSuffix(b, "/") {
		b += "/"
	}
	return s.locate(b)
}

// checkFile reads one site file and checks its size and hash.
func (d *Deployer) checkFile(ctx context.Context, base location, p string, f SiteFile) ([]byte, error) {
	loc := base.child(strings.TrimPrefix(p, "/"))
	max := f.Size
	if max < 1 {
		max = 1
	}
	if max > maxSiteFile {
		return nil, common.NewError(400, fmt.Sprintf("%s: %d bytes is more than the %d a site file may be", p, f.Size, maxSiteFile), "INVALID_ARGUMENT")
	}
	b, err := d.read(ctx, loc, max)
	if err != nil {
		var ae *common.APIError
		if errors.As(err, &ae) && strings.Contains(ae.Message, "larger than") {
			return nil, hashMismatch(fmt.Sprintf("%s: %s does not match the hash and size in the manifest", p, loc))
		}
		return nil, err
	}
	if int64(len(b)) != f.Size || sha256Hex(b) != f.Hash {
		return nil, hashMismatch(fmt.Sprintf("%s: %s does not match the hash and size in the manifest", p, loc))
	}
	return b, nil
}
