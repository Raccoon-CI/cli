package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"raccoonci/internal/cliflag"
	"raccoonci/internal/trigger"
	"raccoonci/internal/workflow"
)

// eventFlags describe the event to match a workflow against, and default to
// what the local checkout says: its branch, and the files changed by its
// last commit.
type eventFlags struct {
	name, action, ref, baseRef, before, cron string
	files                                    cliflag.List
}

func (f *eventFlags) register(fs *flag.FlagSet, withEventName bool) {
	if withEventName {
		fs.StringVar(&f.name, "event", "push", "the event: push, pull_request, workflow_dispatch, schedule, ...")
	}
	fs.StringVar(&f.action, "action", "", "the activity type (opened, labeled, ...); empty means the event's default")
	fs.StringVar(&f.ref, "ref", "", "the ref a push is about, e.g. refs/heads/main or refs/tags/v1 (default: this checkout's branch)")
	fs.StringVar(&f.baseRef, "base-ref", "", "the branch a pull request targets (default: main)")
	fs.StringVar(&f.before, "before", "", "the git revision the changed files are counted from (default: HEAD~1 for a push, the base branch for a pull request)")
	fs.Var(&f.files, "file", "a changed file, instead of asking git (repeatable)")
	fs.StringVar(&f.cron, "cron", "", "for the schedule event: the cron expression that fired")
}

// event builds the trigger.Event, asking git in repo for what was not given.
func (f *eventFlags) event(repo, name string) trigger.Event {
	ev := trigger.Event{Name: name, Action: f.action, Ref: f.ref, BaseRef: f.baseRef, Cron: f.cron}
	if ev.Ref == "" {
		if b := gitOut(repo, "symbolic-ref", "-q", "HEAD"); b != "" {
			ev.Ref = b
		}
	}
	if name == "pull_request" || name == "pull_request_target" {
		if ev.BaseRef == "" {
			ev.BaseRef = "main"
		}
	}
	switch {
	case len(f.files) > 0:
		ev.Files = append([]string{}, f.files...)
	case name == "push":
		before := f.before
		if before == "" {
			before = "HEAD~1"
		}
		ev.Files = gitFiles(repo, before+"..HEAD")
	case name == "pull_request" || name == "pull_request_target":
		base := f.before
		if base == "" {
			base = ev.BaseRef
		}
		ev.Files = gitFiles(repo, base+"...HEAD")
	}
	return ev
}

func gitOut(repo string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitFiles lists the files changed in a revision range, or nil if git cannot
// say (no such revision, not a checkout): the files are then "not known".
func gitFiles(repo, rng string) []string {
	out, err := exec.Command("git", "-C", repo, "diff", "--name-only", "-z", rng).Output()
	if err != nil {
		return nil
	}
	files := []string{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// matchWorkflow implements `raccoonci match`: it prints whether the event
// starts the workflow. ok is false when it does not.
func matchWorkflow(out io.Writer, args []string) (ok bool, err error) {
	fs := flag.NewFlagSet("match", flag.ExitOnError)
	var ef eventFlags
	ef.register(fs, true)
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
	repo, found := repoRoot(path)
	if !found {
		repo = gitTopLevel(filepath.Dir(path))
	}
	res, err := trigger.Match(w.On, ef.event(repo, ef.name))
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
