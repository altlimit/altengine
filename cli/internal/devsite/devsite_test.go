// What a visitor sees, asserted against a real directory on disk.
//
// These mirror the hosted service's serve-path tests deliberately: the value of this package is
// that the local answer and the deployed answer agree, so the cases worth pinning are the ones
// where they could quietly diverge — resolution order, the trailing-slash redirect, the SPA
// fallback, content types, and 304s.
package devsite

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// site writes the files and returns a server with SPA explicitly OFF. The resolution tests are
// about resolution; leaving detection to run would make half of them depend on whether the
// fixture happens to look like a client-routed build. Detection has its own test.
func site(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	spa := false
	return siteWith(t, files, &spa)
}

func siteWith(t *testing.T, files map[string]string, spa *bool) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Options{Dir: dir, SPA: spa, CleanURLs: true, NotFound: "/404.html"})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func get(t *testing.T, s *Server, path string, headers ...string) *http.Response {
	t.Helper()
	return do(t, s, http.MethodGet, path, headers...)
}

func do(t *testing.T, s *Server, method, path string, headers ...string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b := make([]byte, 4096)
	n, _ := res.Body.Read(b)
	return string(b[:n])
}

var pages = map[string]string{
	"/index.html":             "home",
	"/about.html":             "about",
	"/docs/index.html":        "docs",
	"/assets/app-4f2a91bc.js": "console.log(1)",
	"/404.html":               "missing",
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/a//b", "/a/b", true},
		{"/a/./b", "/a/b", true},
		{"/a/b/../c", "/a/c", true},
		{"/hello%20world.html", "/hello world.html", true},
		// Web paths are case-sensitive; a site may legitimately serve /About.html.
		{"/About.html", "/About.html", true},
		// The trailing slash is the difference between a page and a directory.
		{"/docs/", "/docs/", true},
		{"/docs", "/docs", true},
		{"/", "/", true},
		// Cannot be walked out of the site root.
		{"/../../etc/passwd", "/etc/passwd", true},
		{"/a/../../..", "/", true},
		// Not a path at all.
		{"/%E0%A4%A", "", false},
		{"/a\x00b", "", false},
		{"/a\nb", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizePath(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("NormalizePath(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestResolution(t *testing.T) {
	s, _ := site(t, pages)

	t.Run("index for the root and for a directory", func(t *testing.T) {
		if got := body(t, get(t, s, "/")); got != "home" {
			t.Errorf("/ = %q", got)
		}
		if got := body(t, get(t, s, "/docs/")); got != "docs" {
			t.Errorf("/docs/ = %q", got)
		}
	})

	t.Run("a directory missing its slash redirects rather than serving", func(t *testing.T) {
		// Serving /docs/index.html at /docs would make every relative link on the page resolve
		// one level too high. 302 rather than the hosted 301 on purpose: a permanent redirect a
		// browser has memorized outlives the local mistake that caused it.
		res := get(t, s, "/docs")
		if res.StatusCode != http.StatusFound {
			t.Fatalf("status %d, want 302", res.StatusCode)
		}
		if loc := res.Header.Get("Location"); loc != "/docs/" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("the redirect keeps the query", func(t *testing.T) {
		res := get(t, s, "/docs?page=2")
		if loc := res.Header.Get("Location"); loc != "/docs/?page=2" {
			t.Errorf("Location = %q, want the query preserved", loc)
		}
	})

	t.Run("clean URLs, on and off", func(t *testing.T) {
		if got := body(t, get(t, s, "/about")); got != "about" {
			t.Errorf("/about = %q", got)
		}
		off, _ := site(t, pages)
		off.cleanURLs = false
		res := get(t, off, "/about")
		if res.StatusCode != 404 || body(t, res) != "missing" {
			t.Errorf("with cleanUrls off, /about = %d %q", res.StatusCode, body(t, res))
		}
	})

	t.Run("an exact file beats anything clever", func(t *testing.T) {
		res := get(t, s, "/assets/app-4f2a91bc.js")
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
	})

	t.Run("a miss gets the custom page AND a 404 status", func(t *testing.T) {
		res := get(t, s, "/nope")
		if res.StatusCode != 404 {
			t.Errorf("status %d, want 404", res.StatusCode)
		}
		if got := body(t, res); got != "missing" {
			t.Errorf("body %q, want the custom 404 page", got)
		}
	})

	t.Run("a bare 404 when the site has no 404 page", func(t *testing.T) {
		bare, _ := site(t, map[string]string{"/index.html": "home"})
		res := get(t, bare, "/nope")
		if res.StatusCode != 404 {
			t.Errorf("status %d", res.StatusCode)
		}
	})

	t.Run("a directory with no index is not listed", func(t *testing.T) {
		// Go's file server would happily render an index of it. A static host must not.
		s2, _ := site(t, map[string]string{"/index.html": "home", "/private/secret.txt": "s3cret"})
		res := get(t, s2, "/private/")
		if res.StatusCode != 404 {
			t.Fatalf("status %d, want 404", res.StatusCode)
		}
		if strings.Contains(body(t, res), "secret.txt") {
			t.Error("the directory was listed")
		}
	})
}

func TestSPA(t *testing.T) {
	t.Run("unmatched paths get the shell with a 200", func(t *testing.T) {
		// A client-routed app's unmatched path is one of ITS routes. A 404 here breaks deep
		// links and tells crawlers the app's pages do not exist.
		spa := true
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("shell"), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := New(Options{Dir: dir, SPA: &spa, CleanURLs: true})
		if err != nil {
			t.Fatal(err)
		}
		res := get(t, s, "/dashboard/settings")
		if res.StatusCode != 200 || body(t, res) != "shell" {
			t.Errorf("got %d %q, want 200 shell", res.StatusCode, body(t, res))
		}
	})

	t.Run("the shell wins over the 404 page when both could apply", func(t *testing.T) {
		spa := true
		s, _ := siteWith(t, map[string]string{"/index.html": "shell", "/404.html": "missing"}, &spa)
		res := get(t, s, "/route")
		if res.StatusCode != 200 || body(t, res) != "shell" {
			t.Errorf("got %d %q", res.StatusCode, body(t, res))
		}
	})
}

func TestSPAFallbackVsMissingSubresource(t *testing.T) {
	// A missing script answered with the shell is a 200 nobody can act on: the browser reports
	// `Unexpected token '<'`, which reads as a syntax error in your own code. The extension cannot
	// decide it — /edit/something.js is a legitimate route in a client-routed app. The browser
	// says which it is, and locally is where a missing file should be loudest.
	spa := true
	files := map[string]string{"/index.html": "shell", "/404.html": "missing"}

	t.Run("a navigation gets the shell whatever the path looks like", func(t *testing.T) {
		s, _ := siteWith(t, files, &spa)
		for _, p := range []string{"/dashboard", "/edit/something.js", "/files/report.pdf"} {
			res := get(t, s, p, "Sec-Fetch-Dest", "document")
			if res.StatusCode != 200 || body(t, res) != "shell" {
				t.Errorf("%s: got %d %q, want 200 shell", p, res.StatusCode, body(t, res))
			}
		}
	})

	t.Run("a subresource the browser asked for by name 404s", func(t *testing.T) {
		s, _ := siteWith(t, files, &spa)
		for _, dest := range []string{"script", "style", "image", "font", "empty"} {
			res := get(t, s, "/assets/gone-4f2a91bc.js", "Sec-Fetch-Dest", dest)
			if res.StatusCode != 404 {
				t.Errorf("dest %s: got %d, want 404", dest, res.StatusCode)
			}
		}
	})

	t.Run("a real file is still served to a subresource request", func(t *testing.T) {
		s, _ := siteWith(t, map[string]string{"/index.html": "shell", "/app.js": "code"}, &spa)
		res := get(t, s, "/app.js", "Sec-Fetch-Dest", "script")
		if res.StatusCode != 200 || body(t, res) != "code" {
			t.Errorf("got %d %q, want 200 code", res.StatusCode, body(t, res))
		}
	})

	t.Run("older browsers are read from Accept", func(t *testing.T) {
		s, _ := siteWith(t, files, &spa)
		res := get(t, s, "/edit/something.js", "Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		if res.StatusCode != 200 {
			t.Errorf("navigation by Accept: got %d, want 200", res.StatusCode)
		}
		res = get(t, s, "/assets/gone-4f2a91bc.js", "Accept", "*/*")
		if res.StatusCode != 404 {
			t.Errorf("subresource by Accept: got %d, want 404", res.StatusCode)
		}
	})

	t.Run("clients that send neither fall back to the extension", func(t *testing.T) {
		// curl and CI checks. A missing bundle should still read as missing; anything
		// unrecognized stays permissive.
		s, _ := siteWith(t, files, &spa)
		if res := get(t, s, "/assets/gone-4f2a91bc.js"); res.StatusCode != 404 {
			t.Errorf("missing bundle: got %d, want 404", res.StatusCode)
		}
		if res := get(t, s, "/dashboard"); res.StatusCode != 200 {
			t.Errorf("route: got %d, want 200", res.StatusCode)
		}
		if res := get(t, s, "/users/john.doe"); res.StatusCode != 200 {
			t.Errorf("route with a dot: got %d, want 200", res.StatusCode)
		}
	})
}

func TestDetectSPA(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"a client-routed build has one page", map[string]string{
			"/index.html": "x", "/assets/app-1a2b3c4d.js": "y"}, true},
		{"a 404.html means the build has a considered answer for a miss", map[string]string{
			"/index.html": "x", "/404.html": "y"}, false},
		{"a generated site has a page per route", map[string]string{
			"/index.html": "x", "/about.html": "y", "/docs/index.html": "z"}, false},
		{"nothing to serve", map[string]string{"/robots.txt": "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, dir := site(t, c.files)
			got, why := DetectSPA(Scan(dir))
			if got != c.want {
				t.Errorf("DetectSPA = %v (%s), want %v", got, why, c.want)
			}
			if why == "" {
				t.Error("the reason is logged to the developer; it must not be empty")
			}
		})
	}

	t.Run("an explicit flag beats the detection in both directions", func(t *testing.T) {
		// Detection is a guess. A guess that cannot be overridden is worse than no guess.
		for _, want := range []bool{true, false} {
			dir := t.TempDir()
			for _, f := range []string{"index.html", "about.html"} { // would detect false
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s, err := New(Options{Dir: dir, SPA: &want})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.isSPA(); got != want {
				t.Errorf("SPA = %v, want the flag's %v", got, want)
			}
		}
	})
}

func TestConditionalRequests(t *testing.T) {
	s, _ := site(t, pages)
	res := get(t, s, "/")
	etag := res.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || len(etag) != 66 {
		t.Fatalf("ETag = %q, want a quoted sha256", etag)
	}

	t.Run("an exact match is a 304", func(t *testing.T) {
		if got := get(t, s, "/", "If-None-Match", etag).StatusCode; got != 304 {
			t.Errorf("status %d, want 304", got)
		}
	})
	t.Run("a different tag is not", func(t *testing.T) {
		if got := get(t, s, "/", "If-None-Match", `"nope"`).StatusCode; got != 200 {
			t.Errorf("status %d, want 200", got)
		}
	})
	t.Run("the 404 page revalidates too, keeping its 404", func(t *testing.T) {
		miss := get(t, s, "/nope")
		tag := miss.Header.Get("ETag")
		if got := get(t, s, "/nope", "If-None-Match", tag).StatusCode; got != 304 {
			t.Errorf("status %d, want 304", got)
		}
	})

	t.Run("the matcher", func(t *testing.T) {
		tag := `"` + strings.Repeat("a", 64) + `"`
		cases := []struct {
			inm  string
			want bool
		}{
			{tag, true},
			{"W/" + tag, true},                     // every ETag here is a content hash
			{`"other", ` + tag + `, "more"`, true}, // what a browser actually sends
			{"*", true},
			{`"different"`, false},
			{`"` + strings.Repeat("a", 20) + `"`, false}, // a prefix is not a match
			{"", false},
		}
		for _, c := range cases {
			if got := NotModified(c.inm, tag); got != c.want {
				t.Errorf("NotModified(%q) = %v, want %v", c.inm, got, c.want)
			}
		}
		if NotModified(tag, "") {
			t.Error("no ETag can never match")
		}
	})
}

func TestContentTypeAndImmutability(t *testing.T) {
	// The table is a copy of the hosted one rather than Go's mime database, which varies by
	// machine: a font that serves as octet-stream locally and font/woff2 in production is a bug
	// you can only find in production.
	for path, want := range map[string]string{
		"/index.html":       "text/html; charset=utf-8",
		"/app.js":           "text/javascript; charset=utf-8",
		"/f.woff2":          "font/woff2",
		"/site.webmanifest": "application/manifest+json",
		"/m.wasm":           "application/wasm",
		"/LICENSE":          "application/octet-stream",
		"/thing.unknown":    "application/octet-stream",
	} {
		if got := ContentTypeFor(path); got != want {
			t.Errorf("ContentTypeFor(%q) = %q, want %q", path, got, want)
		}
	}

	// Being wrong in the two directions is not symmetric: a wrongly-mutable file revalidates,
	// while a wrongly-immutable one is stale in every visitor's browser for a year. So the rule
	// only fires on evidence that a build tool put a content hash in the name.
	for path, want := range map[string]bool{
		"/assets/app-4f2a91bc.js":    true,
		"/assets/index.a1b2c3d4.css": true,
		"/logo-2024.png":             false, // too short to be a fingerprint
		"/about-something.html":      false, // no digit
		"/app.js":                    false,
	} {
		if got := IsImmutablePath(path); got != want {
			t.Errorf("IsImmutablePath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestNoCachingLocally(t *testing.T) {
	// Hosted, a fingerprinted asset gets a year and `immutable`. Locally that is a file stuck in
	// the developer's browser that nothing on this side can clear, so every response revalidates.
	s, _ := site(t, pages)
	for _, p := range []string{"/", "/assets/app-4f2a91bc.js"} {
		if cc := get(t, s, p).Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", p, cc)
		}
	}
}

func TestReadsDiskEveryRequest(t *testing.T) {
	// The one deliberate difference from hosted: there is no deployment, so a rebuild is picked
	// up with no deploy step and no restart.
	s, dir := site(t, map[string]string{"/index.html": "v1"})
	first := get(t, s, "/")
	if got := body(t, first); got != "v1" {
		t.Fatalf("body %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("version two"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := get(t, s, "/")
	if got := body(t, second); got != "version two" {
		t.Errorf("body %q, want the rebuilt file", got)
	}
	if first.Header.Get("ETag") == second.Header.Get("ETag") {
		t.Error("the ETag did not move with the content")
	}
}

func TestDirectoryAppearingLater(t *testing.T) {
	// `altengine dev` is routinely started before the first build. That must not be a startup
	// error, and the message must not read as a routing bug in the emulator.
	base := t.TempDir()
	dist := filepath.Join(base, "dist")
	s, err := New(Options{Dir: dist, CleanURLs: true})
	if err != nil {
		t.Fatalf("a missing directory must not fail startup: %v", err)
	}
	res := get(t, s, "/")
	if res.StatusCode != 404 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(body(t, res), "no build output") {
		t.Errorf("body %q does not say why", body(t, res))
	}

	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("built"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := body(t, get(t, s, "/")); got != "built" {
		t.Errorf("after the build, / = %q", got)
	}
}

func TestCannotEscapeTheRoot(t *testing.T) {
	s, dir := site(t, pages)
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("s3cret"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("traversal", func(t *testing.T) {
		for _, p := range []string{"/../secret.txt", "/a/../../secret.txt", "/%2e%2e/secret.txt"} {
			res := get(t, s, p)
			if strings.Contains(body(t, res), "s3cret") {
				t.Errorf("%s escaped the root", p)
			}
		}
	})

	t.Run("a symlink pointing out of the tree", func(t *testing.T) {
		// A dist directory on a developer's machine is a short walk from an SSH key, and
		// --host 0.0.0.0 makes that reachable from the network.
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need a privilege on Windows")
		}
		if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
			t.Fatal(err)
		}
		res := get(t, s, "/leak.txt")
		if strings.Contains(body(t, res), "s3cret") {
			t.Error("a symlink out of the root was served")
		}
	})
}

func TestMethodsAndHead(t *testing.T) {
	s, _ := site(t, pages)

	t.Run("HEAD carries the headers and no body", func(t *testing.T) {
		res := do(t, s, http.MethodHead, "/")
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		if res.Header.Get("ETag") == "" {
			t.Error("no ETag on a HEAD")
		}
		if got := body(t, res); got != "" {
			t.Errorf("HEAD returned a body: %q", got)
		}
	})

	t.Run("a write method is refused with Allow", func(t *testing.T) {
		res := do(t, s, http.MethodPost, "/")
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status %d", res.StatusCode)
		}
		if res.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("Allow = %q", res.Header.Get("Allow"))
		}
	})
}

func TestDotfilesAreServed(t *testing.T) {
	// .well-known carries ACME challenges, apple-app-site-association and security.txt. A server
	// that quietly skips dotfiles breaks them in a way that is discovered weeks later.
	s, _ := site(t, map[string]string{
		"/index.html":               "home",
		"/.well-known/security.txt": "Contact: mailto:x@example.com",
	})
	res := get(t, s, "/.well-known/security.txt")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(body(t, res), "Contact:") {
		t.Error("the dotfile path was not served")
	}
}

func TestSummaryCountsWhatDeploysImmutably(t *testing.T) {
	// The thing a local server can tell you that a deploy cannot: whether your build's filenames
	// will let the edge hold them.
	s, _ := site(t, map[string]string{
		"/index.html":             "home",
		"/assets/app-4f2a91bc.js": "fingerprinted, so the edge can hold it for a year",
		"/assets/style.css":       "not fingerprinted, so it revalidates on every visit",
	})
	joined := strings.Join(s.Summary(), "\n")
	if !strings.Contains(joined, "3 files") {
		t.Errorf("summary %q does not count the files", joined)
	}
	if !strings.Contains(joined, "1 of 2 non-page files") {
		t.Errorf("summary %q does not report immutability", joined)
	}
}
