package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Raccoon-CI/raccoonci/internal/cliutil"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/actions"
	"github.com/Raccoon-CI/raccoonci/internal/envsnap"
	"github.com/Raccoon-CI/raccoonci/internal/plan"
	"github.com/Raccoon-CI/raccoonci/internal/runnercmd"
	"github.com/Raccoon-CI/raccoonci/internal/scan"
	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
	"github.com/Raccoon-CI/raccoonci/pkg/platform"
	"github.com/Raccoon-CI/raccoonci/pkg/runnerimages"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: raccoonci [-version] [catalog flags] <command> [args]

commands:
  plan [-fetch-actions] <workflow.yml>
                                     parse a workflow and list its jobs, matrix combinations
                                     and what each run needs installed; -fetch-actions
                                     downloads third-party actions to look inside them
  match [flags] <workflow.yml>       say whether an event (push to a branch, a pull request,
                                     changed files...) triggers the workflow's on:
                                     (exit status 1 if not)
  serve -repo owner/name [flags]     run the GitHub Actions jobs that ask for a raccoon-* label
                                     on a fresh InstaBox machine each (GitHub still orchestrates)
  affected [flags] <workflow.yml>    say which jobs the changed files concern, and why
                                     (run -affected leaves out the others)
  run [flags] <workflow.yml>         run a workflow on this machine (raccoonci run -h for flags)
  agent [flags]                      on a machine a controller sends jobs to: wait for
                                     ~/.raccoonci-agent.json, run the job it names
  runner-snapshot [flags]            build the InstaBox snapshot that run -on instabox starts
                                     machines from (needs an API key)
  catalog update                     pull the catalog feed now (needs -catalog-url)
  runner-images update <dir>         fetch what GitHub's Ubuntu runner images preinstall into
                                     snapshot files (uses GITHUB_TOKEN for the API fallback if set)

The catalog is Raccoon CI's knowledge of toolchains, tools, actions, runner
labels and images (see pkg/catalog/catalog.yaml for the format). It is
built up in layers, each on top of the one before:
  built-in               compiled into this binary
  -catalog-url URL       a feed pulled over https and cached on disk, refreshed
                         when a day old; offline, the cached copy is used
                         (env RACCOONCI_CATALOG_URL)
  -catalog-key KEY       base64 ed25519 public key; with it, a feed must carry
                         a valid signature at URL.sig (env RACCOONCI_CATALOG_KEY)
  -catalog file.yaml     a local file
`

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	var cf catalogFlags
	flag.StringVar(&cf.file, "catalog", "", "overlay this catalog file on top")
	flag.StringVar(&cf.url, "catalog-url", os.Getenv("RACCOONCI_CATALOG_URL"), "pull a catalog feed from this https URL")
	flag.StringVar(&cf.key, "catalog-key", os.Getenv("RACCOONCI_CATALOG_KEY"), "require feeds to be signed by this base64 ed25519 public key")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	var err error
	if args := flag.Args(); len(args) == 2 && args[0] == "catalog" && args[1] == "update" {
		if err = updateCatalog(cf); err != nil {
			fmt.Fprintln(os.Stderr, "raccoonci:", err)
			os.Exit(1)
		}
		return
	}
	cat, err := loadCatalog(cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "raccoonci:", err)
		os.Exit(2)
	}

	switch args := flag.Args(); {
	case len(args) == 2 && args[0] == "plan":
		err = printPlan(os.Stdout, args[1], cat, false)
	case len(args) == 3 && args[0] == "plan" && args[1] == "-fetch-actions":
		err = printPlan(os.Stdout, args[2], cat, true)
	case len(args) >= 1 && args[0] == "run":
		var ok bool
		if rf, rest, serr := splitRemote(args[1:]); serr != nil {
			err = serr
		} else if rf.on != "" {
			ok, err = runWorkflow(rest, cat, &rf)
		} else {
			ok, err = runWorkflow(args[1:], cat, nil)
		}
		if err == nil && !ok {
			os.Exit(1)
		}
	case len(args) >= 1 && args[0] == "serve":
		err = serve(args[1:])
	case len(args) >= 1 && args[0] == "affected":
		err = affectedJobs(os.Stdout, args[1:])
	case len(args) >= 1 && args[0] == "match":
		var ok bool
		if ok, err = matchWorkflow(os.Stdout, args[1:]); err == nil && !ok {
			os.Exit(1)
		}
	case len(args) >= 1 && args[0] == "run-instance": // started by the agent
		err = runnercmd.RunInstance(args[1:], cat)
	case len(args) >= 1 && args[0] == "runner-snapshot":
		err = runnercmd.Snapshot(args[1:])
	case len(args) >= 1 && args[0] == "agent":
		err = runnercmd.Agent(args[1:])
	case len(args) == 3 && args[0] == "runner-images" && args[1] == "update":
		err = updateRunnerImages(args[2], cat)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "raccoonci:", err)
		os.Exit(1)
	}
}

func updateRunnerImages(dir string, cat *catalog.Catalog) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := runnerimages.NewFetcher(os.Getenv("GITHUB_TOKEN"))
	f.Catalog = cat
	s := &runnerimages.Snapshot{Aliases: map[string]string{}, Images: map[string]*runnerimages.Defaults{}}
	for _, image := range cat.ImageNames() {
		d, err := f.Latest(ctx, image)
		if err != nil {
			return err
		}
		s.Images[image] = d
		fmt.Printf("%s %s: %v\n", image, d.Version, d.Tools)
	}
	latest, err := f.LatestAlias(ctx)
	if err != nil {
		return err
	}
	if s.Images[latest] == nil {
		return fmt.Errorf("%s points to %s, which is not tracked", cat.ImageAlias, latest)
	}
	s.Aliases[cat.ImageAlias] = latest
	fmt.Printf("%s = %s\n", cat.ImageAlias, latest)
	return s.WriteDir(dir)
}

// printPlan prints a workflow's plan. With fetchActions the third-party actions
// it uses are downloaded (into a temporary directory) so the scan can see what
// their steps need; without it only the workflow's own steps are scanned.
func printPlan(out io.Writer, path string, cat *catalog.Catalog, fetchActions bool) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	w, err := workflow.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	images, err := runnerimages.Embedded()
	if err != nil {
		return err
	}
	opts := plan.Options{Images: images, Catalog: cat}
	if fetchActions {
		dir, err := os.MkdirTemp("", "raccoonci-plan-actions-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		opts.Action = actions.NewStore(dir, os.Getenv("GITHUB_TOKEN")).Lookup(context.Background())
	}
	if root, ok := cliutil.RepoRoot(path); ok {
		r, err := os.OpenRoot(root) // confines reads to the repo, even through symlinks
		if err != nil {
			return err
		}
		defer r.Close()
		opts.Repo = r.FS()
	}
	p, err := plan.Build(w, opts)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	unavailable := 0
	for _, j := range p.Jobs {
		header := j.ID
		if len(j.Needs) > 0 {
			header += " (needs " + strings.Join(j.Needs, ", ") + ")"
		}
		fmt.Fprintln(out, header)
		switch {
		case j.Uses != "":
			fmt.Fprintf(out, "  calls reusable workflow %s\n", j.Uses)
			continue
		case j.DynamicMatrix:
			fmt.Fprintln(out, "  matrix: dynamic, expanded at run time")
		case len(j.Instances) == 1 && len(j.Instances[0].Matrix) == 0:
			fmt.Fprintf(out, "  %s\n", describe(j.Instances[0]))
			printNeeds(out, j.Instances[0], "  ", opts.Repo, cat)
		default:
			fmt.Fprintf(out, "  matrix: %d combinations\n", len(j.Instances))
			for _, inst := range j.Instances {
				b, err := json.Marshal(inst.Matrix)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "    %s  %s\n", b, describe(inst))
				printNeeds(out, inst, "      ", opts.Repo, cat)
			}
		}
		var warnings []string
		for _, inst := range j.Instances {
			if inst.Unavailable() {
				unavailable++
			}
			if inst.Needs != nil {
				for _, w := range inst.Needs.Warnings {
					if !slices.Contains(warnings, w) {
						warnings = append(warnings, w)
					}
				}
			}
		}
		fmt.Fprintf(out, "  %d steps\n", len(j.Steps))
		for _, w := range warnings {
			fmt.Fprintf(out, "  [!] %s\n", w)
		}
	}
	if unavailable > 0 {
		fmt.Fprintf(out, "\nwarning: %d job run(s) need an OS Raccoon CI cannot provide yet; they will fail unless you connect your own machine\n", unavailable)
	}
	return nil
}

// snapshotNote says whether an instance's installed dependencies could be kept
// in a reusable environment snapshot, and what decides when it goes stale.
func snapshotNote(repo fs.FS, cat *catalog.Catalog, inst *plan.Instance) string {
	if repo == nil || inst.Needs == nil {
		return ""
	}
	var toolchains []string
	for _, d := range inst.Needs.Demands {
		if d.Kind == scan.Toolchain {
			toolchains = append(toolchains, d.Name)
		}
	}
	k, err := envsnap.Compute(repo, cat, "", toolchains)
	switch {
	case errors.Is(err, envsnap.ErrNothingToSnapshot) || (err != nil && len(toolchains) == 0):
		return ""
	case err != nil:
		return "environment snapshot: no, " + err.Error()
	}
	var files []string
	for _, p := range k.Parts {
		files = append(files, p.File)
	}
	return "environment snapshot: possible, stale when " + strings.Join(files, ", ") + " changes"
}

func printNeeds(out io.Writer, inst *plan.Instance, indent string, repo fs.FS, cat *catalog.Catalog) {
	if inst.Needs == nil {
		return
	}
	defer func() {
		if note := snapshotNote(repo, cat, inst); note != "" {
			fmt.Fprintf(out, "%s%s\n", indent, note)
		}
	}()
	for _, d := range inst.Needs.Demands {
		what := d.Name
		switch {
		case d.Version != "":
			what += " " + d.Version
		case d.Kind == scan.Toolchain:
			what += " (version known at run time)"
		}
		note := ""
		if d.Implicit {
			note = " [installed for you: GitHub's image preinstalls it]"
		}
		fmt.Fprintf(out, "%sneeds %s %s%s\n%s  because %s\n", indent, d.Kind, what, note, indent, d.Reason)
	}
}

func describe(inst *plan.Instance) string {
	if inst.RunsOnDeferred {
		return "runs-on: decided at run time"
	}
	labels := strings.Join(platform.Labels(inst.RunsOn), ", ")
	if labels == "" { // e.g. a runner group without labels
		b, _ := json.Marshal(inst.RunsOn)
		labels = string(b)
	}
	s := "runs-on: " + labels
	if inst.Unavailable() {
		s += "  [!] " + inst.UnavailableMsg
	}
	return s
}
