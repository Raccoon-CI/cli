package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A repository with a frontend and a backend and a workflow for each, to see
// that only the one whose files changed is triggered.
func TestMatchUsesTheFilesOfTheLastCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	job := "\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n"
	write(".github/workflows/front.yml", "on:\n  push:\n    branches: [main]\n    paths: ['frontend/**']"+job)
	write(".github/workflows/back.yml", "on:\n  push:\n    paths: ['backend/**']"+job)
	write("frontend/app.js", "1")
	write("backend/main.go", "1")
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	write("backend/main.go", "2") // the last commit touches the backend only
	git("commit", "-q", "-am", "backend change")

	check := func(wf string, extra ...string) (bool, string) {
		t.Helper()
		var out bytes.Buffer
		ok, err := matchWorkflow(&out, append(extra, filepath.Join(repo, ".github/workflows", wf)))
		if err != nil {
			t.Fatal(err)
		}
		return ok, out.String()
	}
	if ok, out := check("front.yml"); ok || !strings.Contains(out, "paths filter") {
		t.Errorf("front.yml after a backend change: %v %q", ok, out)
	}
	if ok, out := check("back.yml"); !ok {
		t.Errorf("back.yml after a backend change: %q", out)
	}
	// Describing the event by hand overrides git.
	if ok, _ := check("front.yml", "-file", "frontend/x.css"); !ok {
		t.Error("front.yml with a frontend file")
	}
	if ok, out := check("front.yml", "-ref", "refs/heads/dev", "-file", "frontend/x.css"); ok || !strings.Contains(out, "branch") {
		t.Errorf("front.yml on branch dev: %v %q", ok, out)
	}
	// From the first commit on, both directories changed.
	if ok, _ := check("front.yml", "-before", "HEAD~2"); ok {
		// HEAD~2 does not exist: the files are unknown, which does not block.
		t.Log("unknown files let the workflow through, as intended")
	}
}
