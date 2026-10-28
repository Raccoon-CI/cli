package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"raccoonci/internal/catalog"
	"raccoonci/internal/plan"
	"raccoonci/internal/platform"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/scan"
	"raccoonci/internal/workflow"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: raccoonci [-version] [catalog flags] <command> [args]

commands:
  plan <workflow.yml>                parse a workflow and list its jobs, matrix combinations
                                     and what each run needs installed
  run [flags] <workflow.yml>         run a workflow on this machine (raccoonci run -h for flags)
  catalog update                     pull the catalog feed now (needs -catalog-url)
  runner-images update <dir>         fetch what GitHub's Ubuntu runner images preinstall into
                                     snapshot files (uses GITHUB_TOKEN for the API fallback if set)

The catalog is Raccoon CI's knowledge of toolchains, tools, actions, runner
labels and images (see internal/catalog/catalog.yaml for the format). It is
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
		err = printPlan(os.Stdout, args[1], cat)
	case len(args) >= 1 && args[0] == "run":
		var ok bool
		ok, err = runWorkflow(args[1:], cat)
		if err == nil && !ok {
			os.Exit(1)
		}
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

func printPlan(out io.Writer, path string, cat *catalog.Catalog) error {
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
	if root, ok := repoRoot(path); ok {
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
			printNeeds(out, j.Instances[0], "  ")
		default:
			fmt.Fprintf(out, "  matrix: %d combinations\n", len(j.Instances))
			for _, inst := range j.Instances {
				b, err := json.Marshal(inst.Matrix)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "    %s  %s\n", b, describe(inst))
				printNeeds(out, inst, "      ")
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

// repoRoot finds the repository a workflow file belongs to, by its
// .github/workflows location.
func repoRoot(workflowPath string) (string, bool) {
	abs, err := filepath.Abs(workflowPath)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(abs)
	if filepath.Base(dir) != "workflows" || filepath.Base(filepath.Dir(dir)) != ".github" {
		return "", false
	}
	return filepath.Dir(filepath.Dir(dir)), true
}

func printNeeds(out io.Writer, inst *plan.Instance, indent string) {
	if inst.Needs == nil {
		return
	}
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
