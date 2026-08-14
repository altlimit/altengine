package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bundling is the CLI's whole reason to exist on the deploy path: the server resolves no
// imports, so if this does not flatten them the upload is broken at runtime, not here.
func TestBundleFlattensImports(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "util.js", "export function greet(n) { return `hello ${n}`; }\n")
	write(t, dir, "entry.js", "import { greet } from './util.js';\nexport default { fetch: () => new Response(greet('world')) };\n")

	out, err := Bundle(filepath.Join(dir, "entry.js"), false)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if strings.Contains(out, "import ") {
		t.Errorf("bundle still contains an import statement:\n%s", out)
	}
	if !strings.Contains(out, "hello ${") {
		t.Errorf("imported module body missing from bundle:\n%s", out)
	}
	if !strings.Contains(out, "export default") && !strings.Contains(out, "export {") {
		t.Errorf("bundle lost its default export:\n%s", out)
	}
}

func TestBundleReportsSyntaxErrorWithLocation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bad.js", "export default { async fetch( { } };\n")

	_, err := Bundle(filepath.Join(dir, "bad.js"), false)
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	// A filename and line number here is the difference between fixing it locally and
	// discovering it as an opaque 500 on the first request after deploy.
	if !strings.Contains(err.Error(), "bad.js:1:") {
		t.Errorf("error lacks a source location: %v", err)
	}
}

func TestBundleMinifyShrinks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "e.js", "export default {\n  // a comment\n  fetch: function (request) {\n    return new Response('a fairly long body string');\n  },\n};\n")
	plain, err := Bundle(filepath.Join(dir, "e.js"), false)
	if err != nil {
		t.Fatal(err)
	}
	small, err := Bundle(filepath.Join(dir, "e.js"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(small) >= len(plain) {
		t.Errorf("minify did not shrink: %d >= %d", len(small), len(plain))
	}
}

func TestBundleMissingFile(t *testing.T) {
	if _, err := Bundle(filepath.Join(t.TempDir(), "nope.js"), false); err == nil {
		t.Fatal("expected an error for a missing entry file")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
