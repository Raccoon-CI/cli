package main

import (
	"flag"
	"fmt"
	"github.com/Raccoon-CI/runner/pkg/cliutil"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Raccoon-CI/lib/pkg/affected"
	"github.com/Raccoon-CI/lib/pkg/workflow"
)

// affectedJobs implements `raccoonci affected`: which jobs the changes
// concern, and why.
func affectedJobs(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("affected", flag.ExitOnError)
	var ef cliutil.EventFlags
	ef.Register(fs, true)
	pkgs := fs.Bool("packages", false, "also list the workspace packages found and what they depend on")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci affected [flags] <workflow.yml>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	w, err := workflow.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	repo, found := cliutil.RepoRoot(path)
	if !found {
		repo = cliutil.GitTopLevel(filepath.Dir(path))
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(repo, cliutil.AbsPath(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is outside its repository %s", path, repo)
	}
	rel = filepath.ToSlash(rel)

	ev := ef.Event(repo, ef.Name)
	cfg, err := affected.LoadConfig(root.FS())
	if err != nil {
		return err
	}
	if ev.Files == nil {
		fmt.Fprintln(out, "changed files: not known (every job is affected)")
	} else {
		fmt.Fprintf(out, "changed files: %d\n", len(ev.Files))
	}
	if *pkgs {
		for _, p := range affected.Packages(root.FS()) {
			fmt.Fprintf(out, "package %s (%s) %s depends on %v\n", p.Dir, p.Kind, p.Name, p.Deps)
		}
	}
	decisions := affected.Decide(affected.Input{Workflow: w, WorkflowPath: rel, Files: ev.Files, Repo: root.FS(), Config: cfg})
	for _, j := range w.Jobs {
		d := decisions[j.ID]
		switch safe, why := affected.Skippable(j); {
		case d.Affected:
			fmt.Fprintf(out, "run   %s: %s\n", j.ID, d.Reason)
		case !safe:
			fmt.Fprintf(out, "run   %s: not affected (%s), but %s, so it is not left out\n", j.ID, d.Reason, why)
		default:
			fmt.Fprintf(out, "skip  %s: %s\n", j.ID, d.Reason)
		}
	}
	return nil
}
