// Command altengine runs a local emulator for the altengine services (search,
// datastore, channels) plus an admin console, so applications can develop against the
// altengine APIs without connecting to the real cloud services.
//
//	altengine dev [--port 8080] [--data ./.altengine] [--memory] [--reset]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/altlimit/altengine/cli/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "dev":
		devCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("altengine", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

// version is stamped at release time via -ldflags "-X main.version=v0.2.0".
var version = "dev"

func usage() {
	fmt.Print(`altengine — local emulator for altengine services

Usage:
  altengine dev [flags]      Start the emulator (data plane + admin console)
  altengine version          Print version

Run 'altengine dev --help' for dev flags.
`)
}

func devCmd(args []string) {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	port := fs.Int("port", 9191, "port to listen on")
	host := fs.String("host", "127.0.0.1", "host to bind")
	data := fs.String("data", "./.altengine", "data directory for persistent storage")
	memory := fs.Bool("memory", false, "keep all data in memory (no persistence)")
	reset := fs.Bool("reset", false, "wipe the data directory before starting")
	_ = fs.Parse(args)

	dataDir := *data
	if *memory {
		dataDir = ""
	} else if *reset {
		abs, _ := filepath.Abs(dataDir)
		fmt.Println("resetting data directory:", abs)
		_ = os.RemoveAll(dataDir)
	}

	srv, err := server.New(server.Options{
		Addr:    fmt.Sprintf("%s:%d", *host, *port),
		DataDir: dataDir,
		DevOpen: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error:", err)
		os.Exit(1)
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
