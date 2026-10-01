package datastore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/control"
)

type nsPage struct {
	Namespaces []struct {
		Namespace string `json:"namespace"`
		Created   int64  `json:"created_at"`
	} `json:"namespaces"`
	HasMore bool    `json:"has_more"`
	Cursor  *string `json:"cursor"`
}

func nsEnv(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	reg, _ := control.New("")
	mux := http.NewServeMux()
	NewHandler(reg, auth.NewStore(true), NewManager(dir)).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func nsPut(t *testing.T, srv *httptest.Server, ns string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/datastore/appdb/ns/"+ns+"/col/things/documents",
		bytes.NewBufferString(`{"documents":[{"key":"k","data":{"a":1}}]}`))
	req.Header.Set("Authorization", "Bearer devkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put into %s -> %d", ns, resp.StatusCode)
	}
}

func nsList(t *testing.T, srv *httptest.Server, query string) nsPage {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/datastore/appdb/ns"+query, nil)
	req.Header.Set("Authorization", "Bearer devkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("list%s -> %d", query, resp.StatusCode)
	}
	var page nsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("the listing is not the hosted shape: %v", err)
	}
	return page
}

// The listing answered with bare names in alphabetical order while the hosted API answers
// with {namespace, created_at} rows, newest first. Code written against one broke on the other.
func TestNamespaceListingIsTheHostedShape(t *testing.T) {
	srv := nsEnv(t, "")
	for _, ns := range []string{"alpha", "bravo", "charlie"} {
		nsPut(t, srv, ns)
		time.Sleep(2 * time.Millisecond) // distinct creation times, so the order is decided
	}

	page := nsList(t, srv, "")
	if len(page.Namespaces) != 3 {
		t.Fatalf("got %d namespaces, want 3", len(page.Namespaces))
	}
	// Newest first, which here is the reverse of alphabetical.
	for i, want := range []string{"charlie", "bravo", "alpha"} {
		if got := page.Namespaces[i].Namespace; got != want {
			t.Fatalf("position %d is %q, want %q", i, got, want)
		}
		if page.Namespaces[i].Created <= 0 {
			t.Fatalf("%q carries no created_at", want)
		}
	}
	if page.HasMore || page.Cursor != nil {
		t.Fatalf("a complete listing said there was more: has_more=%v cursor=%v", page.HasMore, page.Cursor)
	}

	// The search is by substring and ignores case, as it does hosted.
	if got := nsList(t, srv, "?q=RAV"); len(got.Namespaces) != 1 || got.Namespaces[0].Namespace != "bravo" {
		t.Fatalf("q=RAV matched %v", got.Namespaces)
	}
}

// A listing that says "more" has to say where, or everything past the first page is unreachable.
func TestNamespaceListingPagesWithACursor(t *testing.T) {
	srv := nsEnv(t, "")
	for i := 0; i < 5; i++ {
		nsPut(t, srv, fmt.Sprintf("ns%d", i))
		time.Sleep(2 * time.Millisecond)
	}

	var seen []string
	query := "?limit=2"
	for pages := 0; pages < 5; pages++ {
		page := nsList(t, srv, query)
		for _, n := range page.Namespaces {
			seen = append(seen, n.Namespace)
		}
		if !page.HasMore {
			if page.Cursor != nil {
				t.Fatal("the last page handed back a cursor")
			}
			break
		}
		if page.Cursor == nil {
			t.Fatal("has_more with no cursor")
		}
		query = "?limit=2&cursor=" + *page.Cursor
	}
	want := []string{"ns4", "ns3", "ns2", "ns1", "ns0"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("paged through %v, want each namespace once: %v", seen, want)
	}
}

// The creation time is the namespace's, not the process's: it has to survive a restart, or the
// order — and every cursor a client is holding — changes whenever the emulator is stopped.
func TestNamespaceCreationTimeSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first := NewManager(dir)
	if _, err := first.Open("inst", "kept", ""); err != nil {
		t.Fatal(err)
	}
	before := first.NamespaceEntries("inst")
	if len(before) != 1 || before[0].Created <= 0 {
		t.Fatalf("no creation time recorded: %+v", before)
	}

	time.Sleep(5 * time.Millisecond)
	second := NewManager(dir) // a new process: nothing opened yet, only the file on disk
	after := second.NamespaceEntries("inst")
	if len(after) != 1 || after[0].Namespace != "kept" {
		t.Fatalf("the namespace on disk was not listed: %+v", after)
	}
	if after[0].Created != before[0].Created {
		t.Fatalf("created_at moved across a restart: %d -> %d", before[0].Created, after[0].Created)
	}
}
