package common

import (
	"encoding/hex"
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

// fileSafe is the set a namespace may use verbatim in a file name.
func fileSafe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// NamespaceFileName is the file-name stem a namespace's database is stored under. It is
// one-to-one, so `a b`, `a/b` and `a_b` get three files. A name made only of [A-Za-z0-9_.-] is
// used as is — the stem it always had, so existing data still opens — and any other name is "~"
// plus its hex: "~" is never verbatim, hex is safe on case-insensitive filesystems, and the
// longest valid namespace still fits a file name.
func NamespaceFileName(ns string) string {
	for i := 0; i < len(ns); i++ {
		if !fileSafe(ns[i]) {
			return "~" + hex.EncodeToString([]byte(ns))
		}
	}
	return ns
}

// NamespaceFromFileName reverses NamespaceFileName. ok is false for a stem it cannot have
// produced.
func NamespaceFromFileName(stem string) (ns string, ok bool) {
	if rest, escaped := strings.CutPrefix(stem, "~"); escaped {
		b, err := hex.DecodeString(rest)
		if err != nil || NamespaceFileName(string(b)) != stem {
			return "", false
		}
		return string(b), true
	}
	if NamespaceFileName(stem) != stem {
		return "", false
	}
	return stem, true
}
