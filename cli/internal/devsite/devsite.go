// Package devsite serves a built site directory the way the hosted static service serves a
// deployment, so the local dev loop is the deployed behavior.
//
// THE DIRECTORY IS READ ON EVERY REQUEST. There is no deploy step and no snapshot: rebuild with
// your generator and the next reload has it, delete a file and it is gone. That is the one
// deliberate difference from hosted, where a deployment is immutable and activation is a pointer
// move — locally, freezing the bytes at startup would mean restarting the emulator after every
// build, which is the whole thing this avoids.
//
// Everything a visitor can observe is the hosted behavior: the same path resolution (index files,
// the trailing-slash redirect, clean URLs, SPA fallback, the custom 404 page), the same content
// types, and content-hash ETags. Two things are deliberately NOT reproduced, both because
// reproducing them wastes a developer's afternoon on a local machine:
//
//   - Caching. Hosted, a fingerprinted asset gets a year and `immutable`. Here every response is
//     `no-cache` with an ETag, so a rebuild is always picked up and revalidation is still a 304.
//     An immutable asset stuck in a browser for a year is not something a developer can clear
//     from this side.
//   - The trailing-slash redirect is 302 rather than 301, for the same reason: a permanent
//     redirect a browser has memorized outlives the mistake that caused it.
package devsite

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Options configures a site server. The zero value of SPA means "decide from the directory".
type Options struct {
	// Dir is the build output directory, e.g. ./dist. It need not exist yet — a generator that
	// has not run is a normal state at startup, not an error.
	Dir string
	// SPA overrides the detection when non-nil.
	SPA *bool
	// CleanURLs resolves /about to /about.html. Defaults on, as hosted.
	CleanURLs bool
	// NotFound is the page served for a miss when SPA is off. Defaults to /404.html, as hosted.
	NotFound string
}

// Server serves one directory as a site.
type Server struct {
	dir       string // absolute, symlinks resolved
	display   string // as the user typed it, for messages
	cleanURLs bool
	notFound  string

	mu sync.Mutex
	// spa is decided once the directory exists. Detection is deferred rather than done at
	// startup because `altengine dev` is routinely started before the first build — deciding
	// "not an SPA" against an absent directory would 404 every deep link once it appeared.
	spaSet   bool
	spa      bool
	spaFixed bool // set by Options.SPA: never detect, never log
	etags    map[string]etagEntry
}

type etagEntry struct {
	mod  time.Time
	size int64
	hash string
}

// New builds a site server for dir.
func New(o Options) (*Server, error) {
	abs, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("%s is a file; point --static at your build OUTPUT DIRECTORY (e.g. ./dist)", o.Dir)
	}
	// Resolved once so the per-request containment check compares like with like. A dir that
	// does not exist yet cannot be resolved, and is used as given.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	s := &Server{
		dir:       abs,
		display:   o.Dir,
		cleanURLs: o.CleanURLs,
		notFound:  o.NotFound,
		etags:     map[string]etagEntry{},
	}
	if s.notFound == "" {
		s.notFound = "/404.html"
	}
	if !strings.HasPrefix(s.notFound, "/") {
		s.notFound = "/" + s.notFound
	}
	if o.SPA != nil {
		s.spa, s.spaSet, s.spaFixed = *o.SPA, true, true
	}
	return s, nil
}

// Dir returns the absolute directory being served.
func (s *Server) Dir() string { return s.dir }

// --- the same decisions the hosted service makes at deploy time -------------

// TYPES mirrors the hosted table exactly rather than using Go's mime database, which varies by
// machine (/etc/mime.types) — a font that serves as application/octet-stream locally and
// font/woff2 in production is a bug you can only find in production.
var types = map[string]string{
	"html": "text/html; charset=utf-8",
	"htm":  "text/html; charset=utf-8",
	"css":  "text/css; charset=utf-8",
	"js":   "text/javascript; charset=utf-8",
	"mjs":  "text/javascript; charset=utf-8",
	"json": "application/json; charset=utf-8",
	"map":  "application/json; charset=utf-8",

	"webmanifest": "application/manifest+json",
	"xml":         "application/xml; charset=utf-8",
	"txt":         "text/plain; charset=utf-8",
	"md":          "text/markdown; charset=utf-8",
	"csv":         "text/csv; charset=utf-8",
	"svg":         "image/svg+xml",
	"png":         "image/png",
	"jpg":         "image/jpeg",
	"jpeg":        "image/jpeg",
	"gif":         "image/gif",
	"webp":        "image/webp",
	"avif":        "image/avif",
	"ico":         "image/x-icon",
	"bmp":         "image/bmp",
	"woff":        "font/woff",
	"woff2":       "font/woff2",
	"ttf":         "font/ttf",
	"otf":         "font/otf",
	"eot":         "application/vnd.ms-fontobject",
	"wasm":        "application/wasm",
	"pdf":         "application/pdf",
	"zip":         "application/zip",
	"mp4":         "video/mp4",
	"webm":        "video/webm",
	"ogg":         "audio/ogg",
	"mp3":         "audio/mpeg",
	"wav":         "audio/wav",
	"vtt":         "text/vtt",
}

// ContentTypeFor returns the type the hosted service will send for this path.
func ContentTypeFor(p string) string {
	base := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return "application/octet-stream"
	}
	if t, ok := types[strings.ToLower(base[dot+1:])]; ok {
		return t
	}
	return "application/octet-stream"
}

// isKnownAssetPath reports whether the extension names a type we serve, other than a page. Only
// consulted for clients that do not send Sec-Fetch-Dest — see IsNavigationRequest.
func isKnownAssetPath(p string) bool {
	base := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return false
	}
	ext := strings.ToLower(base[dot+1:])
	if ext == "html" || ext == "htm" {
		return false
	}
	_, ok := types[ext]
	return ok
}

// navigationDests are the destinations that mean "the browser is loading this AS a page". An
// iframe or frame is a navigation of its own; everything else — script, style, image, font,
// empty — is a subresource the page asked for by name.
var navigationDests = map[string]bool{"document": true, "iframe": true, "frame": true}

// IsNavigationRequest reports whether this is a top-level navigation rather than a subresource
// fetch. That is the question the SPA fallback turns on, and nothing else uses it.
//
// /edit/something.js typed into the address bar and /assets/app-4f2a91bc.js referenced by a
// script tag are the same string, so the extension cannot tell them apart: the first is one of
// the app's own routes and must get the shell, the second is a file that is genuinely missing and
// must 404. The browser already says which it is, in a header the page cannot forge.
//
// Three rungs, because not every client sends it: Sec-Fetch-Dest from any modern browser, then
// Accept for older ones that still mark navigations with text/html, then the extension for curl
// and CI checks. Anything unrecognized is a navigation, which is the permissive answer.
func IsNavigationRequest(r *http.Request, p string) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return navigationDests[dest]
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		return true
	}
	return !isKnownAssetPath(p)
}

var fingerprint = regexp.MustCompile(`[.\-_]([A-Za-z0-9]{8,})\.[A-Za-z0-9]+$`)

// IsImmutablePath reports whether the hosted service would let a browser cache this path for a
// year: a delimited run of at least 8 alphanumerics containing a digit, before the extension.
// Nothing is served immutably HERE — this is what the startup summary counts, so a build whose
// fingerprinting is not working is visible before it is deployed.
func IsImmutablePath(p string) bool {
	base := p[strings.LastIndex(p, "/")+1:]
	m := fingerprint.FindStringSubmatch(base)
	return m != nil && strings.ContainsAny(m[1], "0123456789")
}

// NormalizePath decodes and resolves a request path to the form a site is keyed in, or returns
// ok=false for something that is not a path at all. Ported from the hosted service so the two
// agree on what /a/../b and /hello%20world.html mean.
func NormalizePath(raw string) (string, bool) {
	decoded, err := decodePath(raw)
	if err != nil {
		return "", false
	}
	if strings.ContainsAny(decoded, "\x00\r\n") {
		return "", false
	}
	trailing := strings.HasSuffix(decoded, "/") && len(decoded) > 1
	var out []string
	for _, seg := range strings.Split(decoded, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	joined := "/" + strings.Join(out, "/")
	if trailing && joined != "/" {
		joined += "/"
	}
	return joined, true
}

// --- request handling -------------------------------------------------------

type resolution struct {
	kind     string // "file" | "redirect" | "notfound"
	path     string // site path, e.g. /docs/index.html
	local    string // absolute path on disk
	status   int
	location string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, ok := NormalizePath(r.URL.Path)
	if !ok {
		s.plain(w, r, http.StatusNotFound, "Not found")
		return
	}
	if _, err := os.Stat(s.dir); err != nil {
		// The generator has not run yet. Saying so beats a bare 404, which reads as a routing
		// bug in the emulator rather than an empty directory.
		s.plain(w, r, http.StatusNotFound,
			fmt.Sprintf("no build output at %s yet — run your site generator and reload", s.display))
		return
	}

	res := s.resolve(p, IsNavigationRequest(r, p))
	switch res.kind {
	case "redirect":
		// 302, not the hosted 301: see the package comment.
		w.Header().Set("Cache-Control", "no-cache")
		http.Redirect(w, r, res.location+queryOf(r), http.StatusFound)
	case "file":
		s.serveFile(w, r, res)
	default:
		s.plain(w, r, http.StatusNotFound, "Not found")
	}
}

// resolve applies the hosted resolution order against the filesystem: an exact file, then the
// directory index, then the trailing-slash redirect, then clean URLs, then the SPA shell, then
// the custom 404 page.
func (s *Server) resolve(p string, nav bool) resolution {
	// A path ending in "/" is never a file — hosted, no manifest key ends in a slash.
	if !strings.HasSuffix(p, "/") {
		if local, ok := s.file(p); ok {
			return resolution{kind: "file", path: p, local: local, status: 200}
		}
	}

	if strings.HasSuffix(p, "/") {
		if local, ok := s.file(p + "index.html"); ok {
			return resolution{kind: "file", path: p + "index.html", local: local, status: 200}
		}
	} else {
		// Serving /docs/index.html at /docs directly would make every relative link on that page
		// resolve one level too high, so the canonical form is the one with the slash.
		if _, ok := s.file(p + "/index.html"); ok {
			return resolution{kind: "redirect", location: p + "/"}
		}
		if s.cleanURLs {
			if local, ok := s.file(p + ".html"); ok {
				return resolution{kind: "file", path: p + ".html", local: local, status: 200}
			}
		}
	}

	// An unmatched NAVIGATION is one of the app's own routes and gets the shell. A subresource is
	// the opposite case: a script whose bundle is not there, a stylesheet, an image, the app's own
	// fetch to a dead path. Answering those with the shell produces a 200 nobody can act on — the
	// browser reports `Unexpected token '<'`, which reads as a syntax error in your code. Locally
	// that matters more than hosted, because this is where a missing file should be loudest.
	if nav && s.isSPA() {
		if local, ok := s.file("/index.html"); ok {
			return resolution{kind: "file", path: "/index.html", local: local, status: 200}
		}
	}
	if local, ok := s.file(s.notFound); ok {
		return resolution{kind: "file", path: s.notFound, local: local, status: 404}
	}
	return resolution{kind: "notfound"}
}

// file maps a site path to a readable regular file inside the root, or reports that there is
// none. A symlink out of the tree is refused: a dist directory on a developer's machine is a
// short walk from an SSH key, and `--host 0.0.0.0` makes that reachable.
func (s *Server) file(sitePath string) (string, bool) {
	local := filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(sitePath, "/")))
	if !within(s.dir, local) {
		return "", false
	}
	info, err := os.Stat(local)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(local); err != nil || !within(s.dir, resolved) {
		return "", false
	}
	return local, true
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, res resolution) {
	f, err := os.Open(res.local)
	if err != nil {
		s.plain(w, r, http.StatusNotFound, "Not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.plain(w, r, http.StatusNotFound, "Not found")
		return
	}

	etag := `"` + s.etagFor(res.local, info) + `"`
	h := w.Header()
	h.Set("Content-Type", ContentTypeFor(res.path))
	h.Set("ETag", etag)
	// Not the hosted `immutable` — see the package comment. The ETag still makes a reload a 304.
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")

	if NotModified(r.Header.Get("If-None-Match"), etag) {
		h.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if res.status == http.StatusOK {
		// ServeContent for the 200 path only: it handles Range and writes its own status, and
		// the custom 404 page has to keep its 404. A zero modtime suppresses Last-Modified,
		// which the hosted service does not send either.
		http.ServeContent(w, r, res.path, time.Time{}, f)
		return
	}
	w.WriteHeader(res.status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// NotModified reports whether the caller already holds this exact content. `W/` is stripped
// because every ETag here is a content hash, so weak and strong comparison give the same answer.
func NotModified(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, t := range strings.Split(ifNoneMatch, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == want || t == "*" {
			return true
		}
	}
	return false
}

// etagFor returns the file's content hash, recomputed only when its size or mtime has moved.
// The hash rather than the mtime so a rebuild that produces identical bytes still revalidates
// to a 304 — which is most of a rebuild.
func (s *Server) etagFor(local string, info os.FileInfo) string {
	s.mu.Lock()
	e, ok := s.etags[local]
	s.mu.Unlock()
	if ok && e.size == info.Size() && e.mod.Equal(info.ModTime()) {
		return e.hash
	}
	f, err := os.Open(local)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	hash := hex.EncodeToString(sum.Sum(nil))
	s.mu.Lock()
	s.etags[local] = etagEntry{mod: info.ModTime(), size: info.Size(), hash: hash}
	s.mu.Unlock()
	return hash
}

func (s *Server) plain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg)
	}
}

// --- SPA detection and the startup summary ----------------------------------

// Stats is what one walk of the directory can tell a developer before they deploy.
type Stats struct {
	Files     int
	Bytes     int64
	Immutable int // files the hosted service would cache for a year
	Pages     int // .html files
	HasIndex  bool
	Has404    bool
	Missing   bool // the directory does not exist yet
}

// Scan walks the directory once. Cheap enough to run at startup and on the first request after
// the directory appears.
func Scan(dir string) Stats {
	var st Stats
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		st.Missing = true
		return st
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not worth failing startup over
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		site := "/" + filepath.ToSlash(rel)
		st.Files++
		st.Bytes += fi.Size()
		if IsImmutablePath(site) {
			st.Immutable++
		}
		if strings.HasSuffix(site, ".html") {
			st.Pages++
		}
		if site == "/index.html" {
			st.HasIndex = true
		}
		if site == "/404.html" {
			st.Has404 = true
		}
		return nil
	})
	return st
}

// DetectSPA decides whether unmatched paths should get the shell.
//
// A client-routed build emits one page; a generator emits one per route, and a 404.html means it
// has a considered answer for a miss. Guessing wrong in either direction is confusing — an SPA
// whose deep links 404, or a site whose typos silently render the homepage — so the caller logs
// what was decided rather than letting it be silent.
func DetectSPA(st Stats) (bool, string) {
	switch {
	case st.Missing || !st.HasIndex:
		return false, "no index.html"
	case st.Has404:
		return false, "the build has a 404.html"
	case st.Pages == 1:
		return true, "index.html is the only page"
	default:
		return false, fmt.Sprintf("%d pages in the build", st.Pages)
	}
}

// isSPA returns the SPA setting, detecting it the first time the directory is readable. Deferred
// because `altengine dev` is routinely started before the first build.
func (s *Server) isSPA() bool {
	s.mu.Lock()
	if s.spaSet {
		v := s.spa
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()

	st := Scan(s.dir)
	if st.Missing {
		return false
	}
	spa, why := DetectSPA(st)

	s.mu.Lock()
	if !s.spaSet {
		s.spa, s.spaSet = spa, true
		s.mu.Unlock()
		log.Printf("  site spa:       %s (%s) — pass --spa or --spa=false to decide it yourself", onOff(spa), why)
		return spa
	}
	v := s.spa
	s.mu.Unlock()
	return v
}

// Summary is the startup block: what will be served, and what the hosted service will make of it.
func (s *Server) Summary() []string {
	st := Scan(s.dir)
	if st.Missing {
		return []string{fmt.Sprintf("%s does not exist yet — it will be served as soon as your build creates it", s.display)}
	}
	lines := []string{fmt.Sprintf("%d files, %s", st.Files, HumanBytes(st.Bytes))}
	if !s.spaFixed {
		spa, why := DetectSPA(st)
		s.mu.Lock()
		s.spa, s.spaSet = spa, true
		s.mu.Unlock()
		lines = append(lines, fmt.Sprintf("spa %s (%s)", onOff(spa), why))
	} else {
		lines = append(lines, fmt.Sprintf("spa %s (--spa)", onOff(s.spa)))
	}
	// The one thing this can tell you that a local server otherwise cannot: whether your build's
	// filenames will let the edge cache hold them. Assets without a fingerprint revalidate on
	// every visit, which is a build setting, not a platform one.
	assets := st.Files - st.Pages
	if assets > 0 {
		lines = append(lines, fmt.Sprintf("%d of %d non-page files will be cached for a year when deployed", st.Immutable, assets))
	}
	return lines
}

// --- small helpers ----------------------------------------------------------

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// within reports whether target is inside root, compared with a trailing separator so
// "/site-backup" is not read as being inside "/site".
func within(root, target string) bool {
	r := filepath.Clean(root) + string(filepath.Separator)
	t := filepath.Clean(target) + string(filepath.Separator)
	return strings.HasPrefix(t, r)
}

// decodePath percent-decodes a path the way the hosted service's decodeURIComponent does,
// rejecting a malformed escape rather than passing it through.
func decodePath(raw string) (string, error) {
	if !strings.Contains(raw, "%") {
		return raw, nil
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", fmt.Errorf("truncated escape")
		}
		hi, ok1 := unhex(raw[i+1])
		lo, ok2 := unhex(raw[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("invalid escape")
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// HumanBytes formats a size the way the deploy command does, so the two agree.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
