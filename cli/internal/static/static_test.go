package static

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func site(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "index.html"), "<h1>hi</h1>")
	write(t, filepath.Join(root, "assets", "app-4f2a91bc.js"), "console.log(1)")
	write(t, filepath.Join(root, "assets", "style.css"), "body{}")
	// A dotted directory, which is the case a naive "skip dotfiles" rule breaks.
	write(t, filepath.Join(root, ".well-known", "security.txt"), "Contact: mailto:x@y.z")
	return root
}

func TestWalkHashesEveryFileAtItsServedPath(t *testing.T) {
	files, err := Walk(site(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range files {
		got[f.Path] = f
	}
	for _, want := range []string{"/index.html", "/assets/app-4f2a91bc.js", "/assets/style.css", "/.well-known/security.txt"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %s (walked: %v)", want, keys(got))
		}
	}
	// Paths are ROOTED and slash-separated whatever the host filesystem uses, because they are
	// URL paths, not file paths.
	for p := range got {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
			t.Fatalf("path %q is not a rooted URL path", p)
		}
	}
	sum := sha256.Sum256([]byte("<h1>hi</h1>"))
	if got["/index.html"].Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("index.html hash = %s", got["/index.html"].Hash)
	}
	if got["/index.html"].Size != int64(len("<h1>hi</h1>")) {
		t.Fatalf("index.html size = %d", got["/index.html"].Size)
	}
}

// The rule that keeps `.well-known/` working — ACME challenges, security.txt,
// apple-app-site-association. A CLI that skipped dotfiles would break certificate renewal on a
// customer's custom domain, and they would find out when the certificate expired.
func TestWalkDoesNotSkipDotfiles(t *testing.T) {
	root := site(t)
	write(t, filepath.Join(root, ".nojekyll"), "")
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range files {
		if f.Path == "/.nojekyll" {
			found = true
		}
	}
	if !found {
		t.Fatal("/.nojekyll was skipped; dotfiles are part of the site")
	}
}

func TestWalkRefusesASymlinkOutOfTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on windows")
	}
	root := site(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	write(t, outside, "id_rsa")
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Skip(err)
	}
	_, err := Walk(root)
	if err == nil {
		t.Fatal("deployed a symlink pointing outside the site root")
	}
	// The error must NAME the file — a refusal you cannot act on is only slightly better than
	// the silent publish it prevents.
	if !strings.Contains(err.Error(), "leak.txt") {
		t.Fatalf("error does not name the file: %v", err)
	}
}

func TestWalkFollowsASymlinkInsideTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on windows")
	}
	root := site(t)
	if err := os.Symlink(filepath.Join(root, "index.html"), filepath.Join(root, "home.html")); err != nil {
		t.Skip(err)
	}
	files, err := Walk(root)
	if err != nil {
		t.Fatalf("a symlink within the site is legitimate: %v", err)
	}
	var home, index File
	for _, f := range files {
		switch f.Path {
		case "/home.html":
			home = f
		case "/index.html":
			index = f
		}
	}
	if home.Path == "" {
		t.Fatal("/home.html was skipped")
	}
	// Same bytes, same hash — which is what makes it ONE upload for both paths.
	if home.Hash != index.Hash {
		t.Fatalf("same content hashed differently: %s vs %s", home.Hash, index.Hash)
	}
}

func TestWalkRefusesAFileAndAnEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := Walk(root); err == nil {
		t.Fatal("an empty directory is not a deployable site")
	}
	f := filepath.Join(root, "index.html")
	write(t, f, "x")
	// Pointing at the file rather than the directory is the commonest mistake, and the message
	// has to say which one it wanted.
	_, err := Walk(f)
	if err == nil || !strings.Contains(err.Error(), "DIRECTORY") {
		t.Fatalf("want a message naming the output directory, got %v", err)
	}
}

func TestManifestKeysOnTheServedPath(t *testing.T) {
	files, err := Walk(site(t))
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest(files)
	e, ok := m["/index.html"]
	if !ok {
		t.Fatalf("manifest keys: %v", mkeys(m))
	}
	if e.Hash == "" || e.Size == 0 {
		t.Fatalf("manifest entry is incomplete: %+v", e)
	}
}

// Two paths holding identical bytes must collapse to one upload — the property that makes a
// redeploy cheap, and the reason the upload map is keyed by hash rather than by path.
func TestIdenticalFilesShareOneHash(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "same")
	write(t, filepath.Join(root, "nested", "b.txt"), "same")
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	byHash := map[string]File{}
	for _, f := range files {
		byHash[f.Hash] = f
	}
	if len(files) != 2 {
		t.Fatalf("walked %d files", len(files))
	}
	if len(byHash) != 1 {
		t.Fatalf("identical bytes produced %d hashes", len(byHash))
	}
}

func TestWithinIsALabelBoundary(t *testing.T) {
	// "/site-backup" must not read as being inside "/site", or a sibling directory becomes
	// deployable through a symlink.
	if within("/site", "/site-backup/x") {
		t.Fatal("/site-backup is not inside /site")
	}
	if !within("/site", "/site/x") {
		t.Fatal("/site/x is inside /site")
	}
}

func keys(m map[string]File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mkeys(m map[string]manifestEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
