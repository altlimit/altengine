package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

// healthCmd probes a running emulator's /healthz and exits 0 when it answers.
//
// It exists for container images: the published image is FROM scratch, with no curl or wget to
// run a HEALTHCHECK with, so the binary checks itself.
func healthCmd(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	port := fs.Int("port", 9191, "port the emulator listens on")
	timeout := fs.Duration("timeout", 3*time.Second, "give up after this long")
	_ = fs.Parse(args)
	if err := checkHealth(fmt.Sprintf("http://127.0.0.1:%d/healthz", *port), *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		os.Exit(1)
	}
}

func checkHealth(url string, timeout time.Duration) error {
	resp, err := (&http.Client{Timeout: timeout}).Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
