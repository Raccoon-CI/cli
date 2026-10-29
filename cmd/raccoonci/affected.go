package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"raccoonci/internal/affected"
	"raccoonci/internal/workflow"
)

// unaffected builds engine.Options.Unaffected from the changed files: a job is
// left out when none of them concerns it and leaving it out is safe.
func unaffected(w *workflow.Workflow, repo fs.FS, wfPath string, files []string) (func(*workflow.Job) (string, bool), error) {
	cfg, err := affected.LoadConfig(repo)
	if err != nil {
		return nil, err
	}
	decisions := affected.Decide(affected.Input{Workflow: w, WorkflowPath: wfPath, Files: files, Repo: repo, Config: cfg})
	return func(j *workflow.Job) (string, bool) {
		d, ok := decisions[j.ID]
		if !ok || d.Affected {
			return "", false
		}
		if safe, _ := affected.Skippable(j); !safe {
			return "", false
		}
		return "none of the " + fmt.Sprint(len(files)) + " changed files concerns it (" + d.Reason + ")", true
	}, nil
}

// affectedJobs implements `raccoonci affected`: which jobs the changes
// concern, and why.
func affectedJobs(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("affected", flag.ExitOnError)
	var ef eventFlags
	ef.register(fs, true)
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
	repo, found := repoRoot(path)
	if !found {
		repo = gitTopLevel(filepath.Dir(path))
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(repo, absPath(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is outside its repository %s", path, repo)
	}
	rel = filepath.ToSlash(rel)

	ev := ef.event(repo, ef.name)
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
