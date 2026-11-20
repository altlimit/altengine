package main

// `altengine dev deploy <manifest>` and `altengine dev --deploy <manifest>`: a "Deploy to
// AltEngine" manifest applied to the local emulator, with the hosted deploy page's rules.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/manifest"
)

const devDeployUsage = `usage: altengine dev deploy [flags] <manifest.json | http(s)://…/altengine.json>

Plans a Deploy to AltEngine manifest against a running emulator and applies it.

  --name app              app name (default: the manifest's name)
  --var k=v               a value for one of the manifest's vars; repeat
  --secret key/NAME=value a secret for instance <key>; repeat
  --bind key=instance     the existing instance a 'uses' entry means; repeat
  --adopt                 take over existing instances that have the app's names
  --confirm-escalation    allow an update that asks for more access to bound instances
  --confirm-source        allow an update from a different place than the install came from
  --dry-run               print the plan and change nothing
  --url u                 the emulator (default http://127.0.0.1:9191, env ALTENGINE_DEV_URL)
  --json                  print the response as JSON

Exits non-zero when the plan has problems or the deploy fails.`

// deployFlags are the flags `dev deploy` and `dev --deploy` share.
type deployFlags struct {
	name                          *string
	vars, secrets, binds          stringList
	adopt, confirmEsc, confirmSrc *bool
}

func addDeployFlags(fs *flag.FlagSet) *deployFlags {
	f := &deployFlags{}
	f.name = fs.String("name", "", "app name for the manifest (default: the manifest's name)")
	fs.Var(&f.vars, "var", "a manifest var, k=v; repeat")
	fs.Var(&f.secrets, "secret", "a secret, key/NAME=value; repeat")
	fs.Var(&f.binds, "bind", "a uses binding, key=instance; repeat")
	f.adopt = fs.Bool("adopt", false, "take over existing instances that have the app's names")
	f.confirmEsc = fs.Bool("confirm-escalation", false, "allow an update that asks for more access")
	f.confirmSrc = fs.Bool("confirm-source", false, "allow an update from a different manifest source")
	return f
}

// request builds the deploy request. A file path is made absolute here: the emulator may run in
// another directory.
func (f *deployFlags) request(ref string) (manifest.Request, error) {
	req := manifest.Request{Name: *f.name, Adopt: *f.adopt, ConfirmEscalation: *f.confirmEsc, ConfirmSource: *f.confirmSrc}
	if manifest.IsURL(ref) {
		req.Manifest = ref
	} else {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return req, err
		}
		req.Manifest = abs
	}
	pairs := func(list []string, what string) (map[string]string, error) {
		out := map[string]string{}
		for _, kv := range list {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("bad --%s %q: expected key=value", what, kv)
			}
			out[strings.TrimSpace(k)] = v
		}
		return out, nil
	}
	var err error
	if req.Vars, err = pairs(f.vars, "var"); err != nil {
		return req, err
	}
	if req.Bindings, err = pairs(f.binds, "bind"); err != nil {
		return req, err
	}
	sec, err := pairs(f.secrets, "secret")
	if err != nil {
		return req, err
	}
	req.Secrets = map[string]map[string]string{}
	for k, v := range sec {
		key, name, ok := strings.Cut(k, "/")
		if !ok || key == "" || name == "" {
			return req, fmt.Errorf("bad --secret %q: expected key/NAME=value (key is the functions instance's key in the manifest)", k+"=…")
		}
		if req.Secrets[key] == nil {
			req.Secrets[key] = map[string]string{}
		}
		req.Secrets[key][name] = v
	}
	return req, nil
}

func devDeployCmd(args []string) {
	if len(args) > 0 && isHelp(args[0]) {
		subUsage(devDeployUsage, args)
	}
	fs := flag.NewFlagSet("dev deploy", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, devDeployUsage) }
	df := addDeployFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	base := fs.String("url", envOr("ALTENGINE_DEV_URL", "http://127.0.0.1:9191"), "the running emulator")
	asJSON := jsonFlag(fs)
	_ = fs.Parse(flagsFirst(fs, args))
	if fs.NArg() != 1 {
		subUsage(devDeployUsage, nil)
	}
	req, err := df.request(fs.Arg(0))
	if err != nil {
		fail(err)
	}
	req.DryRun = *dryRun

	body, _ := json.Marshal(req)
	client := &http.Client{Timeout: 10 * time.Minute}
	res, err := client.Post(strings.TrimRight(*base, "/")+"/__deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		fail(fmt.Errorf("cannot reach the emulator at %s (is `altengine dev` running?): %w", *base, err))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var resp manifest.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		fail(fmt.Errorf("%s/__deploy answered %d with something that is not a deploy response: %s", *base, res.StatusCode, strings.TrimSpace(string(raw))))
	}
	if *asJSON {
		printJSON(resp)
	} else {
		printDeploy(os.Stdout, &resp)
	}
	if resp.Error != nil || res.StatusCode >= 400 || (resp.Plan != nil && len(resp.Plan.Problems) > 0) {
		os.Exit(1)
	}
}

// printDeploy prints a plan, its result and its error for a person.
func printDeploy(w io.Writer, resp *manifest.Response) {
	if p := resp.Plan; p != nil {
		title := p.AppName
		if title == "" {
			title = "(app name not known yet)"
		}
		fmt.Fprintf(w, "%s — %s, from %s\n", title, p.Mode, p.Manifest.Source)
		if p.Manifest.Version != nil {
			fmt.Fprintf(w, "  version %s\n", *p.Manifest.Version)
		}
		for _, v := range p.Vars {
			val := "(none)"
			if v.Value != nil {
				val = *v.Value
			}
			fmt.Fprintf(w, "  var %s = %s\n", v.Name, val)
		}
		for _, i := range p.Instances {
			url := ""
			if i.URL != nil {
				url = "  " + *i.URL
			}
			fmt.Fprintf(w, "  %-8s %-10s %s%s\n", i.Action, i.Service, i.Name, url)
			for _, c := range i.Changes {
				fmt.Fprintf(w, "             - %s\n", c)
			}
		}
		for _, b := range p.Bindings {
			target := "(unbound)"
			if b.Instance != nil {
				target = b.Instance.Name
			}
			fmt.Fprintf(w, "  uses     %-10s %s → %s (%s)\n", b.Service, b.Key, target, b.Level)
		}
		for _, s := range p.Secrets {
			state := "needs a value"
			if s.Given {
				state = "given"
			} else if s.Stored {
				state = "stored"
			} else if !s.Required {
				state = "optional"
			}
			fmt.Fprintf(w, "  secret   %s/%s: %s\n", s.Key, s.Name, state)
		}
		section := func(name string, list []string) {
			if len(list) == 0 {
				return
			}
			fmt.Fprintf(w, "%s:\n", name)
			for _, l := range list {
				fmt.Fprintf(w, "  - %s\n", l)
			}
		}
		section("warnings", p.Warnings)
		section("escalations (need --confirm-escalation)", p.Escalations)
		if p.SourceChanged != nil {
			fmt.Fprintf(w, "source changed: %s → %s (needs --confirm-source)\n", p.SourceChanged.From, p.SourceChanged.To)
		}
		section("problems", p.Problems)
	}
	if r := resp.Result; r != nil {
		fmt.Fprintf(w, "deployed %s\n", r.AppName)
		for _, i := range r.Instances {
			url := ""
			if i.URL != nil {
				url = "  " + *i.URL
			}
			fmt.Fprintf(w, "  %-10s %-24s %s%s\n", i.Service, i.Name, i.ID, url)
		}
		for _, f := range r.Functions {
			if f.Skipped {
				fmt.Fprintf(w, "  function %s/%s unchanged (v%d)\n", f.Instance, f.Name, f.Version)
			} else {
				fmt.Fprintf(w, "  function %s/%s v%d\n", f.Instance, f.Name, f.Version)
			}
		}
	} else if resp.Error == nil && resp.Plan != nil && len(resp.Plan.Problems) == 0 {
		fmt.Fprintln(w, "dry run: nothing was changed")
	}
	if e := resp.Error; e != nil {
		fmt.Fprintf(w, "error: %s: %s\n", e.Code, e.Message)
	}
}
