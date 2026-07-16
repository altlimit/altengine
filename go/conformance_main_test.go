//go:build conformance

// Conformance harness: builds the emulator once and spawns it on a free port
// for the duration of the run. When ALTENGINE_CONFORMANCE_URL is set, the
// suite targets that instead and no emulator is spawned.
//
//	go test -tags conformance ./...
package altengine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	altengine "github.com/altlimit/altengine/go"
)

var (
	baseURL string
	apiKey  string
)

func TestMain(m *testing.M) {
	if url := os.Getenv("ALTENGINE_CONFORMANCE_URL"); url != "" {
		baseURL = url
		apiKey = os.Getenv("ALTENGINE_CONFORMANCE_KEY")
		os.Exit(m.Run())
	}

	bin := filepath.Join(os.TempDir(), "altengine-conformance")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/altengine")
	build.Dir = "../cli"
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building emulator:", err)
		os.Exit(1)
	}

	port := freePort()
	child := exec.Command(bin, "dev", "--memory", "--port", strconv.Itoa(port))
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "starting emulator:", err)
		os.Exit(1)
	}

	baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	apiKey = "conformance-dev-key"
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := http.Get(baseURL + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			child.Process.Kill()
			fmt.Fprintln(os.Stderr, "emulator did not become healthy in 15s")
			os.Exit(1)
		}
		time.Sleep(100 * time.Millisecond)
	}

	code := m.Run()
	child.Process.Kill()
	os.Exit(code)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func client() *altengine.Client {
	return altengine.New(altengine.WithBaseURL(baseURL), altengine.WithAPIKey(apiKey))
}

func isEmulator() bool { return os.Getenv("ALTENGINE_CONFORMANCE_URL") == "" }

func destructiveOk() bool {
	return isEmulator() || os.Getenv("ALTENGINE_CONFORMANCE_DESTRUCTIVE") == "1"
}

// uniq generates a random suffix so suites never collide across runs or
// languages.
func uniq(prefix string) string {
	return fmt.Sprintf("%s-%x%04x", prefix, time.Now().UnixMilli(), rand.IntN(1<<16))
}

func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "conformance", "fixtures", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}
