package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/runner"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// One job instance over the wire, the way `run -on instabox` sends it: the
// controller hands a resolved instance (matrix value, needs output) to
// `raccoonci agent`, which runs `run-instance`; events and the result come
// back through wireReader, with outputs and the order of keys intact.
func TestRunInstanceOverTheWire(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "raccoonci")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(src, ".github", "workflows", "w.yml"), []byte(`
on: push
jobs:
  test:
    needs: setup
    runs-on: ubuntu-latest
    outputs:
      seen: ${{ steps.s.outputs.seen }}
    strategy:
      matrix:
        v: [1, 2]
    steps:
      - id: s
        run: |
          echo "seen=v${{ matrix.v }}-${{ needs.setup.outputs.who }}" >> "$GITHUB_OUTPUT"
          echo "hello from ${{ matrix.v }} token=${{ secrets.TOKEN }}"
  setup:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
`), 0o644)

	contexts := `{"github":{"repository":"o/r","event_name":"push"},` +
		`"matrix":{"v":2},"strategy":{"job-index":1,"job-total":2},` +
		`"needs":{"setup":{"result":"success","outputs":{"who":"alice"}}},"inputs":{},"vars":{}}`
	input, _ := json.Marshal(instanceSpec{Workflow: ".github/workflows/w.yml", Job: "test",
		Contexts: json.RawMessage(contexts), Install: "none"})

	c := &dispatch.Controller{}
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()
	var mu sync.Mutex
	var events []runner.Event
	reader := &wireReader{name: "test (2)", event: func(e runner.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }}
	run, err := c.Submit(dispatch.Job{
		Command: "run-instance",
		Input:   input,
		Secrets: map[string]string{"TOKEN": "s3cret"},
		Source:  func(w io.Writer) error { return dispatch.TarDir(w, src) },
		Log:     reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "agent.json")
	b, _ := json.Marshal(agentConfig{URL: srv.URL, Token: run.Token})
	os.WriteFile(cfg, b, 0o600)
	home := t.TempDir()
	cmd := exec.Command(bin, "agent", "-config", cfg, "-wait", "5s")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+home)
	var agentOut lockedBuf
	cmd.Stdout, cmd.Stderr = &agentOut, &agentOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := run.Wait(ctx)
	cmd.Wait()
	reader.Flush()
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res %+v, err %v\nagent:\n%s", res, err, agentOut.String())
	}
	out, ok, err := reader.Result()
	if err != nil || !ok {
		t.Fatalf("no result (%v)\nagent:\n%s", err, agentOut.String())
	}
	if out.Conclusion != runner.Success {
		t.Errorf("conclusion %s", out.Conclusion)
	}
	if v, _ := out.Outputs.Get("seen"); v != "v2-alice" {
		t.Errorf("outputs = %v (matrix and needs must have reached the job)", out.Outputs)
	}
	var lines []string
	mu.Lock()
	for _, e := range events {
		if e.Kind == runner.Output {
			lines = append(lines, e.Text)
		}
	}
	mu.Unlock()
	if !slices.Contains(lines, "hello from 2 token=***") { // the secret arrived, masked
		t.Errorf("step output did not arrive as events: %q", lines)
	}
}

// The real binary, as `raccoonci agent`, runs a workflow a controller sends:
// source tree over the wire, log streamed back, failing step reported as an
// exit code. The workflow only uses run: steps, so no network is needed.
func TestAgentRunsAWorkflowFromAController(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "raccoonci")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(src, ".github", "workflows", "w.yml"), []byte(`
on: push
jobs:
  ok:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo "hello $(cat data.txt) token=${{ secrets.TOKEN }}"
  bad:
    runs-on: ubuntu-latest
    steps:
      - run: exit 7
`), 0o644)
	os.WriteFile(filepath.Join(src, "data.txt"), []byte("from-the-controller"), 0o644)

	c := &dispatch.Controller{}
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	runJob := func(job string) (dispatch.Result, string) {
		var log lockedBuf
		run, err := c.Submit(dispatch.Job{
			Args:    []string{"-install", "none", "-job", job, ".github/workflows/w.yml"},
			Secrets: map[string]string{"TOKEN": "s3cret"},
			Source:  func(w io.Writer) error { return dispatch.TarDir(w, src) },
			Log:     &log,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Forget(run)
		cfg := filepath.Join(t.TempDir(), "agent.json")
		b, _ := json.Marshal(agentConfig{URL: srv.URL, Token: run.Token})
		os.WriteFile(cfg, b, 0o600)
		home := t.TempDir()
		cmd := exec.Command(bin, "agent", "-config", cfg, "-wait", "5s")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+home)
		var agentOut lockedBuf
		cmd.Stdout, cmd.Stderr = &agentOut, &agentOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		res, err := run.Wait(ctx)
		cmd.Wait()
		if err != nil {
			t.Fatalf("%v\nlog:\n%s\nagent:\n%s", err, log.String(), agentOut.String())
		}
		return res, log.String()
	}

	res, log := runJob("ok")
	if res.ExitCode != 0 {
		t.Errorf("ok: %+v\n%s", res, log)
	}
	if !strings.Contains(log, "hello from-the-controller token=***") {
		t.Errorf("the step's output (secret masked) is not in the log:\n%s", log)
	}
	if strings.Contains(log, "s3cret") {
		t.Errorf("a secret reached the controller's log:\n%s", log)
	}
	if res, log := runJob("bad"); res.ExitCode == 0 {
		t.Errorf("a failing job reported success:\n%s", log)
	}
}
