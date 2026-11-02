package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Raccoon-CI/raccoonci/internal/cliutil"
	"github.com/Raccoon-CI/raccoonci/internal/trigger"
	"github.com/Raccoon-CI/raccoonci/internal/workflow"
)

// matchWorkflow implements `raccoonci match`: it prints whether the event
// starts the workflow. ok is false when it does not.
func matchWorkflow(out io.Writer, args []string) (ok bool, err error) {
	fs := flag.NewFlagSet("match", flag.ExitOnError)
	var ef cliutil.EventFlags
	ef.Register(fs, true)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci match [flags] <workflow.yml>")
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
		return false, err
	}
	w, err := workflow.Parse(src)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	repo, found := cliutil.RepoRoot(path)
	if !found {
		repo = cliutil.GitTopLevel(filepath.Dir(path))
	}
	res, err := trigger.Match(w.On, ef.Event(repo, ef.Name))
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case !res.Triggered:
		fmt.Fprintf(out, "not triggered: %s\n", res.Reason)
	case res.Reason != "":
		fmt.Fprintf(out, "triggered (%s)\n", res.Reason)
	default:
		fmt.Fprintln(out, "triggered")
	}
	return res.Triggered, nil
}
