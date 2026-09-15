package main

import (
	"flag"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/altlimit/altengine/cli/internal/static"
)

func staticConfig(fs *flag.FlagSet, url, key, instance *string) (static.Config, error) {
	c, err := resolveConfig(fs, url, key, instance)
	return static.Config{BaseURL: c.BaseURL, APIKey: c.APIKey, Instance: c.Instance}, err
}

func staticCmd(args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("usage: altengine static <deploy|list|rollback|info> [flags]"))
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("static "+sub, flag.ExitOnError)
	url, key, instance := hostedFlags(fs)

	switch sub {
	case "deploy":
		staticDeploy(fs, rest, url, key, instance)

	case "list":
		_ = fs.Parse(rest)
		cfg, err := staticConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		deployments, cursor, active, err := cfg.Deployments("")
		if err != nil {
			fail(err)
		}
		if len(deployments) == 0 {
			fmt.Println("no deployments yet")
			return
		}
		for _, d := range deployments {
			// The id is printed IN FULL, not shortened. `rollback` takes an id, and this is where
			// you get one — a listing that prints a prefix you cannot paste into the next command
			// is a workflow that dead-ends in its own output.
			marker := " "
			if d.ID == active {
				marker = "*"
			}
			msg := d.Message
			if msg == "" {
				msg = "—"
			}
			fmt.Printf("%s %-5s %s  %-18s %5d files  %9s  %s  %-7s %s\n",
				marker, fmt.Sprintf("v%d", d.Number), d.ID, truncate(msg, 18), d.FileCount, humanBytes(d.TotalBytes),
				time.UnixMilli(d.CreatedAt).Format("2006-01-02 15:04"), d.Status, d.URL)
		}
		fmt.Println("\n* = live.  altengine static rollback <id>  to switch to another.")
		// The part of paging that gets skipped: say when this is not the whole answer.
		if cursor != "" {
			fmt.Printf("\n(more deployments — this is the newest %d)\n", len(deployments))
		}

	case "rollback":
		_ = fs.Parse(rest)
		if fs.NArg() < 1 {
			fail(fmt.Errorf("usage: altengine static rollback [flags] <deployment-id>\n" +
				"       (altengine static list shows the ids)"))
		}
		cfg, err := staticConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		// The same call `deploy` finishes with. A rollback uploads nothing: that deployment's
		// files are still stored, so this is a pointer move.
		res, err := cfg.Activate(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		fmt.Printf("rolled back to %s — %d files, %s\n", res.DeploymentID[:12], res.FileCount, humanBytes(res.TotalBytes))
		printLive(res.URL)

	case "info":
		_ = fs.Parse(rest)
		cfg, err := staticConfig(fs, url, key, instance)
		if err != nil {
			fail(err)
		}
		s, err := cfg.Info()
		if err != nil {
			fail(err)
		}
		fmt.Printf("%-14s %s\n", "site", s.Name)
		fmt.Printf("%-14s %s\n", "url", s.URL)
		if s.ActiveDeployment == "" {
			fmt.Printf("%-14s none — nothing is deployed yet\n", "serving")
		} else {
			fmt.Printf("%-14s %s\n", "serving", s.ActiveDeployment)
		}
		fmt.Printf("%-14s %d\n", "deployments", s.Deployments)
		fmt.Printf("%-14s %d (%s, deduplicated)\n", "files", s.Files, humanBytes(s.StoredBytes))

	default:
		fail(fmt.Errorf("unknown subcommand %q (want deploy, list, rollback or info)", sub))
	}
}

func staticDeploy(fs *flag.FlagSet, rest []string, url, key, instance *string) {
	message := fs.String("message", "", "label for this deployment (defaults to the git commit)")
	noActivate := fs.Bool("no-activate", false, "upload the deployment but keep serving the current one")
	dryRun := fs.Bool("dry-run", false, "hash the directory and report what would upload, without uploading")
	_ = fs.Parse(rest)

	if fs.NArg() < 1 {
		fail(fmt.Errorf("usage: altengine static deploy [flags] <directory>\n" +
			"       e.g. altengine static deploy ./dist"))
	}
	dir := fs.Arg(0)

	// Hashing first, and locally: the server can only tell us what it is missing if we tell it
	// what we have, and this is what makes an unchanged asset never upload twice.
	files, err := static.Walk(dir)
	if err != nil {
		fail(err)
	}
	var total int64
	byHash := make(map[string]static.File, len(files))
	for _, f := range files {
		total += f.Size
		// One entry per distinct hash: a file at two paths is one upload.
		byHash[f.Hash] = f
	}
	fmt.Printf("%d files, %s\n", len(files), humanBytes(total))

	if *dryRun {
		fmt.Println("(dry run — nothing uploaded; the server decides which of these it already has)")
		return
	}

	cfg, err := staticConfig(fs, url, key, instance)
	if err != nil {
		fail(err)
	}

	label := *message
	if label == "" {
		label = static.GitDescribe(dir)
	}

	created, err := cfg.Create(files, label)
	if err != nil {
		fail(err)
	}
	reused := created.FileCount - created.MissingCount
	if created.MissingCount == 0 {
		fmt.Printf("nothing to upload — all %d files are already stored\n", created.FileCount)
	} else {
		fmt.Printf("uploading %d files (%d already stored)\n", created.MissingCount, reused)
	}

	// Page until the cursor runs out. A first deploy of a large site has more missing files than
	// one response carries, and activating without uploading the rest fails — so this must not
	// assume the first page was the whole set.
	var done int64
	onDone := func() {
		n := atomic.AddInt64(&done, 1)
		fmt.Printf("\r  %d/%d", n, created.MissingCount)
	}
	uploads, cursor := created.Uploads, created.Cursor
	for {
		if err := static.PutAll(uploads, byHash, onDone); err != nil {
			fmt.Println()
			fail(err)
		}
		if cursor == "" {
			break
		}
		uploads, cursor, err = cfg.UploadPage(created.DeploymentID, cursor)
		if err != nil {
			fmt.Println()
			fail(err)
		}
	}
	if created.MissingCount > 0 {
		fmt.Println()
	}

	if *noActivate {
		fmt.Printf("deployment %s uploaded, not activated\n", created.DeploymentID[:12])
		fmt.Printf("  altengine static rollback %s   # to publish it\n", created.DeploymentID)
		return
	}

	res, err := cfg.Activate(created.DeploymentID)
	if err != nil {
		fail(err)
	}
	fmt.Printf("deployed %s — %d files, %s\n", res.DeploymentID[:12], res.FileCount, humanBytes(res.TotalBytes))
	printLive(res.URL)
}

// printLive states the propagation window rather than implying the deploy is everywhere at once.
// It is atomic per location — a visitor gets one build or the next, never a mix — but a location
// already serving the site can take about a minute to notice.
func printLive(url string) {
	if url != "" {
		fmt.Println(" ", url)
	}
	fmt.Println("  live here now; everywhere within about a minute")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
