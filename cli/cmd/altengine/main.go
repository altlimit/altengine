// Command altengine runs a local emulator for the altengine services plus an admin console, so
// applications can develop against the altengine APIs without the hosted service, and deploys
// functions, static sites and automation scripts to the hosted service.
//
//	altengine dev [--port 9191] [--data ./.altengine] [--memory] [--reset]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/deploy"
	"github.com/altlimit/altengine/cli/internal/hosted"
	"github.com/altlimit/altengine/cli/internal/server"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "dev":
		devCmd(os.Args[2:])
	case "deploy":
		deployCmd(os.Args[2:])
	case "functions":
		functionsCmd(os.Args[2:])
	case "static":
		staticCmd(os.Args[2:])
	case "automation":
		automationCmd(os.Args[2:])
	case "login":
		loginCmd(os.Args[2:])
	case "logout":
		logoutCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("altengine", version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(1)
	}
}

// isHelp reports whether a subcommand position asks for help rather than naming a subcommand.
func isHelp(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help" || arg == "-help"
}

// subUsage prints a command's usage: to stdout and exit 0 when it was asked for, to stderr and
// exit 1 when the command line was wrong.
func subUsage(text string, args []string) {
	if len(args) > 0 && isHelp(args[0]) {
		fmt.Fprintln(os.Stdout, text)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, text)
	os.Exit(1)
}

// version is stamped at release time via -ldflags "-X main.version=v0.2.0".
var version = "dev"

func usage(w io.Writer) {
	fmt.Fprint(w, `altengine — local emulator for altengine services

Usage:
  altengine dev [flags]              Start the emulator (data plane + admin console)
  altengine dev --static ./dist      ...and serve a built site on its own port
  altengine deploy [flags] <file>    Bundle a function and deploy it
  altengine functions <subcommand>   list | versions | rollback | pull | delete
  altengine static <subcommand>      deploy | list | rollback | info
  altengine automation <subcommand>  deploy | scripts | activate | delete | agents | run | runs |
                                     logs | get | cancel | send | env
  altengine login [--url u]          Save an API key for the hosted commands (read from stdin)
  altengine logout                   Forget the saved key
  altengine version                  Print version

Deploying talks to the hosted service and needs an org API key with 'full' access to the
functions instance — saved with 'altengine login', or in ALTENGINE_API_KEY:

  altengine login
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
	reset := fs.Bool("reset", false, "wipe the data directory before starting (asks first on a terminal)")
	yes := fs.Bool("yes", false, "with --reset, do not ask")
	static := fs.String("static", "", "serve this build output directory as a site, e.g. ./dist")
	staticPort := fs.Int("static-port", 0, "port for the site (default: --port + 1)")
	spa := fs.Bool("spa", false, "serve index.html for unmatched paths (default: detected from the directory)")
	// Browser apps on localhost are allowed by default. This is for the case that is not: a
	// phone or a second machine on the LAN pointed at this emulator. It widens who may drive
	// your local data, so it is a flag rather than the default.
	allowOrigin := fs.String("allow-origin", "", "extra browser origin(s) allowed to call the API, comma-separated")
	_ = fs.Parse(flagsFirst(fs, args))

	// Three states, and the third is the point: detection is a guess, and a guess that cannot be
	// overridden is worse than none — both -spa and -spa=false have to beat it.
	var spaOverride *bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "spa" {
			spaOverride = spa
		}
	})
	staticAddr := ""
	if *staticPort != 0 {
		staticAddr = fmt.Sprintf("%s:%d", *host, *staticPort)
	}

	dataDir := *data
	if *memory {
		dataDir = ""
	} else if *reset {
		confirm := func(abs string) bool {
			if *yes || !term.IsTerminal(int(os.Stdin.Fd())) {
				return true
			}
			fmt.Fprintf(os.Stderr, "delete everything in %s? [y/N] ", abs)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
		}
		if err := resetDataDir(dataDir, confirm); err != nil {
			fail(err)
		}
	}

	srv, err := server.New(server.Options{
		Addr:         fmt.Sprintf("%s:%d", *host, *port),
		DataDir:      dataDir,
		DevOpen:      true,
		StaticDir:    *static,
		StaticAddr:   staticAddr,
		StaticSPA:    spaOverride,
		AllowOrigins: splitList(*allowOrigin),
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

// confirmed asks before something destructive. --yes answers for the caller; without it, a
// terminal is asked and anything else (a script, CI) is refused, so a deletion is never the
// unattended default.
func confirmed(yes bool, what string) bool {
	if yes {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "%s: pass --yes to confirm\n", what)
		return false
	}
	fmt.Fprintf(os.Stderr, "%s? [y/N] ", what)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// resetDataDir deletes an emulator data directory, and nothing that is not one.
//
// --data can point anywhere, and --reset used to RemoveAll it unasked: `--data . --reset` deleted
// the project. A directory is wiped only when it holds the emulator's registry (control.json) or
// is empty; confirm is asked first and may refuse.
func resetDataDir(dir string, confirm func(abs string) bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing to reset
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, "control.json")); err != nil && len(entries) > 0 {
		return fmt.Errorf("refusing to reset %s: it has no control.json, so it is not an altengine data directory", abs)
	}
	if !confirm(abs) {
		return fmt.Errorf("not reset")
	}
	fmt.Fprintln(os.Stderr, "resetting data directory:", abs)
	return os.RemoveAll(abs)
}

// resolveConfig builds the hosted-service config from flags, falling back to the
// environment. Flags win, so a scripted deploy can override an ambient key.
func resolveConfig(fs *flag.FlagSet, url, key, instance *string) (deploy.Config, error) {
	cfg := deploy.Config{Instance: *instance}
	var err error
	if cfg.BaseURL, cfg.APIKey, err = hostedCreds(*url, *key); err != nil {
		return cfg, err
	}
	if cfg.Instance == "" {
		cfg.Instance = os.Getenv("ALTENGINE_INSTANCE")
	}
	if cfg.Instance == "" {
		return cfg, fmt.Errorf("no instance: pass --instance or set ALTENGINE_INSTANCE")
	}
	return cfg, nil
}

// hostedFlags declares the flags every hosted command shares; service names the kind of instance
// --instance takes.
func hostedFlags(fs *flag.FlagSet, service string) (url, key, instance *string) {
	url, key = connFlags(fs)
	return url, key, fs.String("instance", "", service+" instance name (env ALTENGINE_INSTANCE)")
}

// connFlags declares --url and --key.
func connFlags(fs *flag.FlagSet) (url, key *string) {
	return fs.String("url", "", "base URL of the altengine service (env ALTENGINE_URL; default "+hosted.DefaultURL+")"),
		fs.String("key", "", "org API key (env ALTENGINE_API_KEY, or saved by altengine login)")
}

const functionsUsage = `usage: altengine functions <subcommand> [flags]

  list
        List deployed functions, their live version, address and schedules.
  versions <name>
        A function's stored versions; * marks the live one.
  rollback --version <n> <name>   (or activate)
        Serve an already-deployed version.
  pull [--version n] [--out file] <name>
        Print a deployed version's source (default: the live one).
  delete [--version n] [--yes] <name>
        Delete a function (it stops serving), or one stored version that is not live.

Every subcommand takes --instance, --url and --key; 'altengine functions <subcommand> -h'
lists its flags.`

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// flagsFirst moves every flag in args ahead of the positionals, so a flag written after the
// file argument is parsed rather than dropped.
//
// Go's flag package stops at the first positional and treats everything after it as more
// positionals, without complaint. `altengine deploy app.js --dry-run` therefore deployed for
// real, and `static deploy ./dist --no-activate` put the build live.
//
// A flag this set does not define is kept among the flags, so Parse reports it by name instead
// of it becoming a second file argument. A literal `--` still ends flag parsing.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		// A bare "-" and a negative number are values, not flags.
		if len(a) < 2 || a[0] != '-' || (a[1] >= '0' && a[1] <= '9') || a[1] == '.' {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		// It takes a value, and the value is the next argument whatever that looks like.
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if len(positional) == 0 {
		return flags
	}
	return append(append(flags, "--"), positional...)
}

func deployCmd(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	url, key, instance := hostedFlags(fs, "functions")
	name := fs.String("name", "", "function name (defaults to the entry file's base name)")
	minify := fs.Bool("minify", false, "minify the bundle before uploading")
	noActivate := fs.Bool("no-activate", false, "upload the version but keep serving the current one")
	grantList := fs.String("grants", "", "comma-separated access grants, e.g. datastore:appdb=full,search=read")
	var schedules stringList
	fs.Var(&schedules, "schedule", "UTC cron expression to run this function on; repeat for several (e.g. -schedule '0 9 * * 1-5' -schedule '0 12 * * 6')")
	unschedule := fs.Bool("unschedule", false, "remove this function's schedules")
	dryRun := fs.Bool("dry-run", false, "bundle and report the size, but do not upload")
	asJSON := jsonFlag(fs)
	_ = fs.Parse(flagsFirst(fs, args))

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
		if *asJSON {
			printJSON(map[string]any{"name": fnName, "size_bytes": len(code), "uploaded": false})
			return
		}
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
	if *asJSON {
		out := map[string]any{"name": res.Name, "version": res.Version, "size_bytes": res.SizeBytes, "active": res.Active}
		if list, err := cfg.List(); err == nil {
			for _, f := range list.Functions {
				if f.Name == res.Name {
					out["url"] = f.URL
				}
			}
		}
		printJSON(out)
		return
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

// jsonFlag declares --json: print the command's result as JSON on stdout instead of text, for
// scripts and CI.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print the result as JSON")
}

// orEmpty keeps an empty list `[]` in JSON output rather than `null`.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fail(err)
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
	if len(args) == 0 || isHelp(args[0]) {
		subUsage(functionsUsage, args)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("functions "+sub, flag.ExitOnError)
	url, key, instance := hostedFlags(fs, "functions")

	switch sub {
	case "list":
		asJSON := jsonFlag(fs)
		_ = fs.Parse(flagsFirst(fs, rest))
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		list, err := cfg.List()
		if err != nil {
			fail(err)
		}
		if *asJSON {
			printJSON(list)
			return
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
		asJSON := jsonFlag(fs)
		_ = fs.Parse(flagsFirst(fs, rest))
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
		if *asJSON {
			printJSON(map[string]any{"versions": versions, "active_version": active})
			return
		}
		for _, v := range versions {
			marker := " "
			if v.Version == active {
				marker = "*"
			}
			fmt.Printf("%s v%-4d %8d bytes  %s  %s  %s\n", marker, v.Version, v.SizeBytes,
				time.UnixMilli(v.CreatedAt).Format(time.RFC3339), hosted.Short(v.SHA256, 12), v.URL)
		}

	// `activate` is the same call under the verb automation uses for it.
	case "rollback", "activate":
		version := fs.Int("version", 0, "version to activate")
		_ = fs.Parse(flagsFirst(fs, rest))
		if fs.NArg() < 1 || *version < 1 {
			fail(fmt.Errorf("usage: altengine functions %s --version <n> <name>", sub))
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
		_ = fs.Parse(flagsFirst(fs, rest))
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

	case "delete":
		version := fs.Int("version", 0, "delete only this stored version (not the live one)")
		yes := fs.Bool("yes", false, "do not ask")
		_ = fs.Parse(flagsFirst(fs, rest))
		if fs.NArg() < 1 {
			fail(fmt.Errorf("usage: altengine functions delete [--version n] [--yes] <name>"))
		}
		cfg, err := resolveConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		fnName := fs.Arg(0)
		if *version > 0 {
			if !confirmed(*yes, fmt.Sprintf("delete v%d of %s", *version, fnName)) {
				fail(fmt.Errorf("not deleted"))
			}
			if err := cfg.DeleteVersion(fnName, *version); err != nil {
				fail(err)
			}
			fmt.Printf("deleted %s v%d\n", fnName, *version)
			return
		}
		if !confirmed(*yes, fmt.Sprintf("delete %s — it stops serving, its schedules stop, and every version is removed", fnName)) {
			fail(fmt.Errorf("not deleted"))
		}
		if err := cfg.Delete(fnName); err != nil {
			fail(err)
		}
		fmt.Printf("deleted %s\n", fnName)

	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		subUsage(functionsUsage, nil)
	}
}

// splitList turns a comma-separated flag value into a trimmed, non-empty list.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
