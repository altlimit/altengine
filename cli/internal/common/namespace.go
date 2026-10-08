package common

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// NamespaceDefaultSegment is the reserved wire spelling of the default namespace ("") — a URL
// path cannot carry an empty segment — so it can never be a namespace of its own.
const NamespaceDefaultSegment = "_default"

// ValidateNamespace enforces the hosted API's namespace rules, for datastore and search alike:
// printable ASCII, at most 100 bytes; "" (the default namespace) is allowed. Everything keyed on
// (namespace, name) NUL-joins the pair, so a namespace may never contain NUL.
func ValidateNamespace(ns string) error {
	if ns == "" {
		return nil
	}
	if ns == NamespaceDefaultSegment {
		return BadRequest(`"` + NamespaceDefaultSegment + `" is reserved; use the default namespace ("")`)
	}
	if len(ns) > 100 {
		return BadRequest("namespace must be at most 100 bytes")
	}
	for i := 0; i < len(ns); i++ {
		if ns[i] < 0x20 || ns[i] > 0x7e {
			return BadRequest("namespace must contain only printable ASCII characters")
		}
	}
	return nil
}

// fileSafe is the set a namespace may use verbatim in a file name. Lowercase only: on a
// case-insensitive filesystem `Orders` and `orders` would otherwise be one file.
func fileSafe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// legacySafe is the set older emulators used verbatim, uppercase included.
func legacySafe(c byte) bool { return fileSafe(c) || c >= 'A' && c <= 'Z' }

func allBytes(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// windowsDevice reports whether a stem names a Windows device (`nul`, `com1`, `con.backup`…),
// which Windows will not create as a file whatever extension follows.
func windowsDevice(stem string) bool {
	base, _, _ := strings.Cut(strings.ToLower(stem), ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) &&
		base[3] >= '1' && base[3] <= '9'
}

// NamespaceFileName is the file-name stem a namespace's database is stored under. It is
// one-to-one, on case-insensitive filesystems too, so `a b`, `a/b`, `a_b`, `A_b` get four files.
// A name made only of [a-z0-9_.-] that is not a Windows device name is used as is — the stem it
// always had — and any other name is "~" plus its lowercase hex: "~" is never verbatim, and the
// longest valid namespace still fits a file name.
func NamespaceFileName(ns string) string {
	if allBytes(ns, fileSafe) && !windowsDevice(ns) {
		return ns
	}
	return "~" + hex.EncodeToString([]byte(ns))
}

// NamespaceFromFileName maps a stem found on disk back to its namespace: one NamespaceFileName
// produced, or one an older emulator stored verbatim (see NamespaceFile). ok is false for a stem
// neither can have produced.
func NamespaceFromFileName(stem string) (ns string, ok bool) {
	if rest, escaped := strings.CutPrefix(stem, "~"); escaped {
		b, err := hex.DecodeString(rest)
		if err != nil || NamespaceFileName(string(b)) != stem {
			return "", false
		}
		return string(b), true
	}
	if !allBytes(stem, legacySafe) {
		return "", false
	}
	return stem, true
}

// NamespaceFile is the path of a namespace's database in dir, given the stem the store encodes
// it to. A namespace an older emulator stored verbatim and that is escaped now (mixed case, a
// device name) keeps using its old file when that is the one on disk, so its data still opens.
// The match is on the exact name, so on a case-insensitive filesystem `orders.db` is not taken
// for `Orders`.
func NamespaceFile(dir, ns, stem string) string {
	p := filepath.Join(dir, stem+".db")
	if ns == "" || stem == ns {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if allBytes(ns, legacySafe) && exactFileExists(dir, ns+".db") {
		return filepath.Join(dir, ns+".db")
	}
	return p
}

// exactFileExists matches a name exactly, whatever the filesystem's case rules.
func exactFileExists(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}
