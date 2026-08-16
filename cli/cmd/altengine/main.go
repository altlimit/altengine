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
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/deploy"
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
	case "deploy":
		deployCmd(os.Args[2:])
	case "functions":
		functionsCmd(os.Args[2:])
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
  altengine dev [flags]              Start the emulator (data plane + admin console)
  altengine deploy [flags] <file>    Bundle a function and deploy it
  altengine functions <subcommand>   list | versions | rollback | pull
  altengine version                  Print version

Deploying talks to the hosted service and needs an org API key with 'full' access to the
functions instance:

  export ALTENGINE_URL=https://api.altengine.net
  export ALTENGINE_KEY=ak_...
  altengine deploy --instance prod --name hello ./hello.js

Run 'altengine <command> --help' for flags.
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

// resolveConfig builds the hosted-service config from flags, falling back to the
// environment. Flags win, so a scripted deploy can override an ambient key.
func resolveConfig(fs *flag.FlagSet, url, key, instance *string) (deploy.Config, error) {
	cfg := deploy.Config{BaseURL: *url, APIKey: *key, Instance: *instance}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("ALTENGINE_URL")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("ALTENGINE_KEY")
	}
	if cfg.Instance == "" {
		cfg.Instance = os.Getenv("ALTENGINE_INSTANCE")
	}
	if cfg.BaseURL == "" {
		return cfg, fmt.Errorf("no service URL: pass --url or set ALTENGINE_URL")
	}
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("no API key: pass --key or set ALTENGINE_KEY")
	}
	if cfg.Instance == "" {
		return cfg, fmt.Errorf("no instance: pass --instance or set ALTENGINE_INSTANCE")
	}
	return cfg, nil
}

func hostedFlags(fs *flag.FlagSet) (url, key, instance *string) {
	return fs.String("url", "", "base URL of the altengine service (env ALTENGINE_URL)"),
		fs.String("key", "", "org API key with full access to the instance (env ALTENGINE_KEY)"),
		fs.String("instance", "", "functions instance name (env ALTENGINE_INSTANCE)")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func deployCmd(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	url, key, instance := hostedFlags(fs)
	name := fs.String("name", "", "function name (defaults to the entry file's base name)")
	minify := fs.Bool("minify", false, "minify the bundle before uploading")
	noActivate := fs.Bool("no-activate", false, "upload the version but keep serving the current one")
	grantList := fs.String("grants", "", "comma-separated access grants, e.g. datastore:appdb=full,search=read")
	var schedules stringList
	fs.Var(&schedules, "schedule", "UTC cron expression to run this function on; repeat for several (e.g. -schedule '0 9 * * 1-5' -schedule '0 12 * * 6')")
	unschedule := fs.Bool("unschedule", false, "remove this function's schedules")
	dryRun := fs.Bool("dry-run", false, "bundle and report the size, but do not upload")
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine deploy [flags] <entry.js>"))
	}
	entry := fs.Arg(0)

	fnName := *name
	if fnName == "" {
		base := filepath.Base(entry)
		fnName = strings.TrimSuffix(base, filepath.Ext(base))
	}

	code, err := deploy.Bundle(entry, *minify)
	if err != nil {
		fail(err)
	}
	if *dryRun {
		fmt.Printf("%s: %d bytes bundled (not uploaded)\n", fnName, len(code))
		return
	}

	cfg, err := resolveConfig(fs, url, key, instance)
	if err != nil {
		fail(err)
	}
	grants, err := parseGrants(*grantList)
	if err != nil {
		fail(err)
	}

	// Three states, and the difference matters: no flag at all leaves the deployed
	// schedules alone (so a routine redeploy never silently unschedules a job), -schedule
	// sets them, and -unschedule clears them.
	var sched *[]string
	switch {
	case *unschedule:
		empty := []string{}
		sched = &empty
	case len(schedules) > 0:
		list := []string(schedules)
		sched = &list
	}

	res, err := cfg.Deploy(fnName, code, grants, sched, !*noActivate)
	if err != nil {
		fail(err)
	}
	state := "active"
	if !res.Active {
		state = "uploaded, not activated"
	}
	fmt.Printf("deployed %s v%d (%d bytes, %s)\n", res.Name, res.Version, res.SizeBytes, state)

	if list, err := cfg.List(); err == nil {
		for _, f := range list.Functions {
			if f.Name == res.Name && f.URL != "" {
				fmt.Println(f.URL)
			}
		}
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseGrants turns "datastore:appdb=full,search=read" into a grants map. An empty
// string yields nil, which the server reads as "keep whatever this function already
// had" — so a routine redeploy never strips a function's access.
func parseGrants(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad grant %q: expected service[:instance]=level", part)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("bad grant %q: missing service name", part)
		}
		switch v {
		case "read", "write", "full":
		default:
			return nil, fmt.Errorf("bad grant level %q in %q: expected read, write or full", v, part)
		}
		out[k] = v
	}
	return out, nil
}

func functionsCmd(args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("usage: altengine functions <list|versions|rollback|pull> [flags]"))
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("functions "+sub, flag.ExitOnError)
	url, key, instance := hostedFlags(fs)

	switch sub {
	case "list":
		_ = fs.Parse(rest)
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		list, err := cfg.List()
		if err != nil {
			fail(err)
		}
		if len(list.Functions) == 0 {
			fmt.Println("no functions deployed")
			return
		}
		for _, f := range list.Functions {
			fmt.Printf("%-24s v%-4d %s\n", f.Name, f.ActiveVersion, f.URL)
			for _, expr := range f.Schedules {
				fmt.Printf("%-24s   %s UTC\n", "", expr)
			}
		}

	case "versions":
		_ = fs.Parse(rest)
		if fs.NArg() < 1 {
			fail(fmt.Errorf("usage: altengine functions versions [flags] <name>"))
		}
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		versions, active, err := cfg.Versions(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		for _, v := range versions {
			marker := " "
			if v.Version == active {
				marker = "*"
			}
			fmt.Printf("%s v%-4d %8d bytes  %s  %s\n", marker, v.Version, v.SizeBytes,
				time.UnixMilli(v.CreatedAt).Format(time.RFC3339), v.SHA256[:12])
		}

	case "rollback":
		version := fs.Int("version", 0, "version to activate")
		_ = fs.Parse(rest)
		if fs.NArg() < 1 || *version < 1 {
			fail(fmt.Errorf("usage: altengine functions rollback --version <n> <name>"))
		}
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		if err := cfg.Activate(fs.Arg(0), *version); err != nil {
			fail(err)
		}
		fmt.Printf("%s is now serving v%d\n", fs.Arg(0), *version)

	case "pull":
		version := fs.Int("version", 0, "version to fetch (default: the active one)")
		out := fs.String("out", "", "write to this file instead of stdout")
		_ = fs.Parse(rest)
		if fs.NArg() < 1 {
			fail(fmt.Errorf("usage: altengine functions pull [--version n] [--out file] <name>"))
		}
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		fnName := fs.Arg(0)
		v := *version
		if v == 0 {
			list, err := cfg.List()
			if err != nil {
				fail(err)
			}
			for _, f := range list.Functions {
				if f.Name == fnName {
					v = f.ActiveVersion
				}
			}
			if v == 0 {
				fail(fmt.Errorf("function %q is not deployed", fnName))
			}
		}
		code, err := cfg.Pull(fnName, v)
		if err != nil {
			fail(err)
		}
		if *out == "" {
			fmt.Print(code)
			return
		}
		if err := os.WriteFile(*out, []byte(code), 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("wrote %s (v%d, %d bytes)\n", *out, v, len(code))

	default:
		fail(fmt.Errorf("unknown subcommand %q: expected list, versions, rollback or pull", sub))
	}
}
