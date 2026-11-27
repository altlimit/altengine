package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/altlimit/altengine/cli/internal/common"
)

// A put holding one document over 1 MiB is refused whole, naming that document, as
// hosted: nothing in the batch is written.
func TestPutRefusesOversizedDocument(t *testing.T) {
	s := openTest(t)
	if _, err := s.Put("ds", []Document{{ID: "seed", Fields: []Field{field("body", "text", "seed")}}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Put("ds", []Document{
		{ID: "ok", Fields: []Field{field("body", "text", "small")}},
		{ID: "big", Fields: []Field{field("body", "text", strings.Repeat("x", maxDocBytes))}},
	})
	var ae *common.APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "DOCUMENT_TOO_LARGE" || !strings.Contains(ae.Message, "document 'big' (position 1)") {
		t.Fatalf("want 400 DOCUMENT_TOO_LARGE naming big, got %v", err)
	}
	got, err := s.GetDocs("ds", []string{"seed", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "seed" {
		t.Fatalf("all-or-nothing: want only seed, got %v", got)
	}
	// Just under the cap still goes in.
	if _, err := s.Put("ds", []Document{{ID: "fits", Fields: []Field{field("body", "text", strings.Repeat("x", maxDocBytes-200))}}}); err != nil {
		t.Fatalf("doc under the cap refused: %v", err)
	}
}
