package main

// `altengine automation …` — deploy scripts to enrolled machines, start runs, read the results.
//
// The commands here are deliberately the ones a terminal is better at than a console: bundling
// and uploading a script, kicking off a run and tailing it. Fleet management, enrollment tokens
// and the developer window are all console-only, because each of them is a decision a person
// should make while looking at a list of real machines.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/automation"
	"github.com/altlimit/altengine/cli/internal/deploy"
)

func automationUsage() error {
	return fmt.Errorf(`usage: altengine automation <subcommand> [flags]

  deploy [--name n] [--parallel] [--no-activate] <entry.js>
        Bundle a script and upload it as a new version.
  scripts [name]
        List deployed scripts, or one script's version history.
  activate --version <n> <name>
        Point a script at an already-uploaded version.
  agents
        List the enrolled machines and which are connected right now.
  run [--param k=v] [--agent id] [--label l] [--wait] <script>
        Start a run. --wait follows it to completion and prints its log.
  runs [--status s] [--script n] [--limit n]
        List recent runs.
  logs <run-id>
        Print a run's log.
  get <run-id>
        Print a run and its artifacts, with short-lived download links.
  cancel <run-id>
        Stop a run that is queued or running.

Needs an org API key with access to the automation instance:

  export ALTENGINE_URL=https://api.altengine.net
  export ALTENGINE_KEY=ak_...
  altengine automation deploy --instance fleet --name nightly ./nightly.js`)
}

// resolveAutomation reads the instance from --instance, then ALTENGINE_AUTOMATION_INSTANCE, then
// ALTENGINE_INSTANCE. Its own variable first because someone with both a functions instance and
// an automation instance in one shell should not have to unset one to use the other.
func resolveAutomation(url, key, instance *string) (automation.Config, error) {
	cfg := automation.Config{BaseURL: *url, APIKey: *key, Instance: *instance}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("ALTENGINE_URL")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("ALTENGINE_KEY")
	}
	if cfg.Instance == "" {
		cfg.Instance = os.Getenv("ALTENGINE_AUTOMATION_INSTANCE")
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
		return cfg, fmt.Errorf("no instance: pass --instance or set ALTENGINE_AUTOMATION_INSTANCE")
	}
	return cfg, nil
}

func automationCmd(args []string) {
	if len(args) == 0 {
		fail(automationUsage())
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("automation "+sub, flag.ExitOnError)
	url := fs.String("url", "", "base URL of the altengine service (env ALTENGINE_URL)")
	key := fs.String("key", "", "org API key (env ALTENGINE_KEY)")
	instance := fs.String("instance", "", "automation instance name (env ALTENGINE_AUTOMATION_INSTANCE)")

	switch sub {
	case "deploy":
		automationDeploy(fs, rest, url, key, instance)
	case "scripts":
		automationScripts(fs, rest, url, key, instance)
	case "activate":
		automationActivate(fs, rest, url, key, instance)
	case "agents":
		automationAgents(fs, rest, url, key, instance)
	case "run":
		automationRun(fs, rest, url, key, instance)
	case "runs":
		automationRuns(fs, rest, url, key, instance)
	case "logs":
		automationLogs(fs, rest, url, key, instance)
	case "get":
		automationGet(fs, rest, url, key, instance)
	case "cancel":
		automationCancel(fs, rest, url, key, instance)
	default:
		fail(automationUsage())
	}
}

func automationDeploy(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	name := fs.String("name", "", "script name (defaults to the entry file's base name)")
	minify := fs.Bool("minify", false, "minify the bundle before uploading")
	noActivate := fs.Bool("no-activate", false, "upload the version but keep running the current one")
	parallel := fs.Bool("parallel", false, "this script drives ONLY browsers and HTTP, so it may share a machine")
	exclusive := fs.Bool("exclusive", false, "this script drives the desktop and must have the machine to itself")
	dryRun := fs.Bool("dry-run", false, "bundle and report the size, but do not upload")
	_ = fs.Parse(rest)
	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine automation deploy [flags] <entry.js>"))
	}
	if *parallel && *exclusive {
		fail(fmt.Errorf("--parallel and --exclusive are opposites; pass at most one"))
	}
	entry := fs.Arg(0)
	scriptName := *name
	if scriptName == "" {
		base := filepath.Base(entry)
		scriptName = strings.TrimSuffix(base, filepath.Ext(base))
	}

	// The same bundler functions use. The agent resolves no imports, so a script has to arrive
	// as one flat module — and doing it here means what you ran locally with
	// `altengine-worker run` is byte-for-byte what the machine in the office will run.
	code, err := deploy.Bundle(entry, *minify)
	if err != nil {
		fail(err)
	}
	if *dryRun {
		fmt.Printf("%s: %d bytes bundled (not uploaded)\n", scriptName, len(code))
		return
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}

	// Three states, and the middle one is the important one: NO flag leaves the deployed setting
	// alone. Silently flipping a desktop-driving script to parallel on a routine redeploy would
	// put two of them on one screen, typing into each other's windows.
	var par *bool
	switch {
	case *parallel:
		t := true
		par = &t
	case *exclusive:
		f := false
		par = &f
	}

	res, err := cfg.Deploy(scriptName, code, par, !*noActivate)
	if err != nil {
		fail(err)
	}
	state := "live"
	if !res.Active {
		state = "uploaded, not live"
	}
	fmt.Printf("deployed %s v%d (%d bytes, %s)\n", res.Name, res.Version, res.SizeBytes, state)
}

func automationScripts(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	_ = fs.Parse(rest)
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	if fs.NArg() == 1 {
		versions, active, err := cfg.Versions(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		for _, v := range versions {
			marker := " "
			if v.Version == active {
				marker = "*"
			}
			hash := v.CodeHash
			if len(hash) > 12 {
				hash = hash[:12]
			}
			fmt.Printf("%s v%-4d %8d bytes  %s  %s\n", marker, v.Version, v.SizeBytes,
				time.UnixMilli(v.CreatedAt).Format(time.RFC3339), hash)
		}
		return
	}
	scripts, err := cfg.Scripts()
	if err != nil {
		fail(err)
	}
	if len(scripts) == 0 {
		fmt.Println("no scripts deployed")
		return
	}
	for _, s := range scripts {
		// Exclusive/parallel is shown for every script, not just the unusual one: it decides
		// whether a run waits for the machine, and it is the first thing to check when a fleet
		// is not keeping up.
		mode := "exclusive"
		if s.Parallel {
			mode = "parallel"
		}
		version := fmt.Sprintf("v%d", s.ActiveVersion)
		if s.ActiveVersion == 0 {
			version = "(none live)"
		}
		fmt.Printf("%-28s %-12s %s\n", s.Name, version, mode)
	}
}

func automationActivate(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	version := fs.Int("version", 0, "version to make live")
	_ = fs.Parse(rest)
	if fs.NArg() < 1 || *version < 1 {
		fail(fmt.Errorf("usage: altengine automation activate --version <n> <name>"))
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	if err := cfg.Activate(fs.Arg(0), *version); err != nil {
		fail(err)
	}
	fmt.Printf("%s is now running v%d\n", fs.Arg(0), *version)
}

func automationAgents(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	_ = fs.Parse(rest)
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	agents, err := cfg.Agents()
	if err != nil {
		fail(err)
	}
	if len(agents) == 0 {
		fmt.Println("no machines enrolled — mint an enrollment token in the console")
		return
	}
	for _, a := range agents {
		// "offline" is not an error here and the output should not read like one: office PCs
		// sleep, and a run sent to a sleeping machine waits for it rather than failing.
		state := "offline"
		if a.Online {
			state = "online"
		}
		name := a.Name
		if name == "" {
			name = a.Hostname
		}
		fmt.Printf("%-36s %-20s %-8s %s\n", a.ID, name, state, strings.Join(a.Labels, ","))
	}
}

func automationRun(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	var params stringList
	var labels stringList
	fs.Var(&params, "param", "run parameter as k=v (repeatable)")
	fs.Var(&labels, "label", "require this label on the machine (repeatable, ALL must match)")
	agent := fs.String("agent", "", "pin the run to one agent id")
	wait := fs.Bool("wait", false, "follow the run to completion and print its log")
	_ = fs.Parse(rest)
	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine automation run [flags] <script>"))
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	run, err := cfg.Start(automation.StartOptions{
		Script:  fs.Arg(0),
		Params:  kvMap(params),
		AgentID: *agent,
		Labels:  labels,
	})
	if err != nil {
		fail(err)
	}
	// "queued" is said out loud, because it is a different promise from "running" and the
	// difference is a machine being asleep — which is normal, not a failure.
	if run.Status == "queued" {
		fmt.Fprintf(os.Stderr, "run %s queued — no machine is connected yet; it will start when one is\n", run.ID)
	} else {
		fmt.Fprintf(os.Stderr, "run %s %s on agent %s\n", run.ID, run.Status, run.AgentID)
	}
	if !*wait {
		fmt.Println(run.ID)
		return
	}
	if err := follow(cfg, run.ID); err != nil {
		fail(err)
	}
}

// follow prints a run's log as it arrives and exits non-zero if the run failed.
//
// Polling rather than streaming, deliberately: the log lives on the agent and is PULLED (see the
// control plane's automation/logs.ts), so there is nothing to stream from. The exit code matters
// more than the mechanism — this is the form a CI job uses.
func follow(cfg automation.Config, runID string) error {
	cursor := ""
	seen := map[int64]bool{}
	for {
		page, err := cfg.Logs(runID, cursor)
		if err == nil {
			for _, l := range page.Lines {
				// The tail ring re-serves recent lines on every poll; without this the same
				// line prints once per second for as long as the run lasts.
				k := l.TS<<8 ^ int64(len(l.Msg))
				if seen[k] {
					continue
				}
				seen[k] = true
				fmt.Printf("%s [%s] %s\n", time.UnixMilli(l.TS).Format("15:04:05"), l.Level, l.Msg)
			}
			if page.Dropped > 0 {
				fmt.Fprintf(os.Stderr, "(%d log lines were dropped)\n", page.Dropped)
			}
			if page.Source == "artifact" && page.Cursor != "" {
				cursor = page.Cursor
				continue
			}
		}
		run, artifacts, err := cfg.Get(runID)
		if err != nil {
			return err
		}
		switch run.Status {
		case "queued", "running":
			time.Sleep(2 * time.Second)
			continue
		}
		for _, a := range artifacts {
			fmt.Fprintf(os.Stderr, "%s (%d bytes)\n  %s\n", a.Name, a.SizeBytes, a.URL)
		}
		fmt.Fprintf(os.Stderr, "run %s %s ($%.4f)\n", run.ID, run.Status, run.CostUSD)
		if run.Status != "done" {
			if run.Error != "" {
				fmt.Fprintln(os.Stderr, run.Error)
			}
			// A non-zero exit is the whole point of --wait: this is what a CI job checks.
			os.Exit(1)
		}
		return nil
	}
}

func automationRuns(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	status := fs.String("status", "", "filter by status")
	script := fs.String("script", "", "filter by script name")
	limit := fs.Int("limit", 20, "how many to list")
	_ = fs.Parse(rest)
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	runs, err := cfg.Runs(*limit, *status, *script)
	if err != nil {
		fail(err)
	}
	for _, r := range runs {
		when := time.UnixMilli(r.QueuedAt).Format(time.RFC3339)
		fmt.Printf("%-36s %-9s %-24s v%-4d %s\n", r.ID, r.Status, r.Script, r.Version, when)
	}
}

func automationLogs(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	_ = fs.Parse(rest)
	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine automation logs <run-id>"))
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	cursor := ""
	for {
		page, err := cfg.Logs(fs.Arg(0), cursor)
		if err != nil {
			fail(err)
		}
		for _, l := range page.Lines {
			fmt.Printf("%s [%s] %s\n", time.UnixMilli(l.TS).Format(time.RFC3339), l.Level, l.Msg)
		}
		if page.Dropped > 0 {
			fmt.Fprintf(os.Stderr, "(%d log lines were dropped)\n", page.Dropped)
		}
		if page.Cursor == "" {
			// Said only when it is true. A live tail is not the full record, and printing
			// nothing here would let a partial log read as a complete one.
			if !page.Complete {
				fmt.Fprintln(os.Stderr, "(this run is still going — the complete log is uploaded when it finishes)")
			}
			return
		}
		cursor = page.Cursor
	}
}

func automationGet(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	_ = fs.Parse(rest)
	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine automation get <run-id>"))
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	run, artifacts, err := cfg.Get(fs.Arg(0))
	if err != nil {
		fail(err)
	}
	out, _ := json.MarshalIndent(map[string]any{"run": run, "artifacts": artifacts}, "", "  ")
	fmt.Println(string(out))
}

func automationCancel(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	_ = fs.Parse(rest)
	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine automation cancel <run-id>"))
	}
	cfg, err := resolveAutomation(url, key, instance)
	if err != nil {
		fail(err)
	}
	if err := cfg.Cancel(fs.Arg(0)); err != nil {
		fail(err)
	}
	fmt.Println("cancelled")
}

// kvMap turns repeated k=v flags into a map.
func kvMap(list stringList) map[string]string {
	out := map[string]string{}
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
