package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"raccoonci/internal/actions"
	"raccoonci/internal/catalog"
	"raccoonci/internal/engine"
	"raccoonci/internal/local"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

// instanceSpec is what a controller sends a machine to run one job instance
// (see engine.RemoteInstance): the instance as the engine resolved it, plus
// the run's contexts. Secrets travel separately, in the environment.
type instanceSpec struct {
	Workflow string `json:"workflow"` // path in the source tree
	Job      string `json:"job"`
	// Contexts is an object with github, matrix, strategy, needs, inputs and
	// vars. JSON object order is kept (value.FromJSON).
	Contexts   json.RawMessage   `json:"contexts"`
	ServiceEnv map[string]string `json:"service_env"`
	Install    string            `json:"install"`
}

// wireLine is one line of the machine's output: an event, the final result,
// or (anything that is not JSON) plain text, which the controller shows as it
// is.
type wireLine struct {
	Event  *runner.Event `json:"event,omitempty"`
	Result *wireResult   `json:"result,omitempty"`
}

type wireResult struct {
	Conclusion runner.Conclusion `json:"conclusion"`
	Outputs    json.RawMessage   `json:"outputs"`
	// DurationMS is how long the job took on the machine, from provisioning
	// to its last step: what the scheduler weighs against a machine's start-up.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// runInstance implements `raccoonci run-instance`, which the agent starts in
// the unpacked source tree: it runs the one job instance read from standard
// input and writes wireLines to standard output.
func runInstance(args []string, cat *catalog.Catalog) error {
	fs := flag.NewFlagSet("run-instance", flag.ExitOnError)
	var secrets listFlag
	fs.Var(&secrets, "secret", "the environment variable `NAME` to pass as secrets.NAME (repeatable)")
	fs.Parse(args)

	var spec instanceSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<20)).Decode(&spec); err != nil {
		return fmt.Errorf("reading the instance: %w", err)
	}
	ctxv, err := value.FromJSON(spec.Contexts)
	if err != nil {
		return fmt.Errorf("contexts: %w", err)
	}
	contexts, ok := ctxv.(value.Object)
	if !ok {
		return errors.New("contexts: not an object")
	}
	obj := func(name string) value.Object {
		v, _ := contexts.Get(name)
		o, _ := v.(value.Object)
		return o
	}
	secretValues := map[string]string{}
	for _, name := range secrets {
		v, ok := os.LookupEnv(name)
		if !ok {
			return fmt.Errorf("secret %s is not in the environment", name)
		}
		secretValues[name] = v
	}

	src, err := os.Getwd()
	if err != nil {
		return err
	}
	wfSrc, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(spec.Workflow)))
	if err != nil {
		return err
	}
	w, err := workflow.Parse(wfSrc)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Workflow, err)
	}
	job := w.Job(spec.Job)
	if job == nil {
		return fmt.Errorf("%s has no job %q", spec.Workflow, spec.Job)
	}
	rt, err := newRunEnv(cat, spec.Install)
	if err != nil {
		return err
	}
	images, err := runnerimages.Embedded()
	if err != nil {
		return err
	}
	github := obj("github")
	repository, _ := github.Get("repository")
	repoName, _ := repository.(string)
	repoDir, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer repoDir.Close()

	workDir := filepath.Join(rt.cacheDir, "runs", fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid()))
	enc := &lineEncoder{out: os.Stdout}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	began := time.Now()
	res := engine.RunInstance(ctx, w, engine.Options{
		Github:    github,
		Secrets:   secretValues,
		Vars:      obj("vars"),
		Inputs:    obj("inputs"),
		Images:    images,
		Catalog:   cat,
		Provision: rt.prov,
		Repo:      repoDir.FS(),
		WorkDir:   workDir,
		Runner: runner.Config{
			ToolCache:  rt.toolCache,
			Actions:    actions.NewStore(filepath.Join(workDir, "_actions"), secretValues["GITHUB_TOKEN"]),
			Node:       rt.node.Binary,
			NodeMusl:   rt.nodeMusl.Binary,
			Mounts:     []string{rt.node.Dir, filepath.Join(workDir, "_actions")},
			Checkout:   local.Checkout(src, repoName),
			ServiceEnv: spec.ServiceEnv,
			RunnerName: "raccoonci-instabox",
			OnEvent:    func(e runner.Event) { enc.write(wireLine{Event: &e}) },
		},
	}, job, obj("matrix"), obj("strategy"), obj("needs"))

	outputs, err := json.Marshal(res.Outputs)
	if err != nil {
		return err
	}
	enc.write(wireLine{Result: &wireResult{Conclusion: res.Conclusion, Outputs: outputs, DurationMS: time.Since(began).Milliseconds()}})
	return nil
}

// lineEncoder writes one JSON line at a time, safe for concurrent use (the
// events of parallel steps come from several goroutines).
type lineEncoder struct {
	mu  sync.Mutex
	out io.Writer
}

func (l *lineEncoder) write(v wireLine) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out.Write(append(b, '\n'))
}

// wireReader turns the controller's copy of a machine's output into events
// and a result. Writes may split lines anywhere.
type wireReader struct {
	name  string // the instance, for lines that are plain text
	event func(runner.Event)

	mu      sync.Mutex
	partial []byte
	result  *wireResult
}

func (r *wireReader) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		r.line(r.partial[:i])
		r.partial = r.partial[i+1:]
	}
	return len(p), nil
}

// Flush handles a last line that has no newline.
func (r *wireReader) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.partial) > 0 {
		r.line(r.partial)
		r.partial = nil
	}
}

func (r *wireReader) line(b []byte) {
	var l wireLine
	if len(b) > 0 && b[0] == '{' && json.Unmarshal(b, &l) == nil && (l.Event != nil || l.Result != nil) {
		if l.Event != nil {
			r.event(*l.Event)
		}
		if l.Result != nil {
			r.result = l.Result
		}
		return
	}
	if len(b) > 0 {
		r.event(runner.Event{Kind: runner.Output, Time: time.Now(), Job: r.name, Text: string(b)})
	}
}

// Duration is how long the job took on the machine, if it said.
func (r *wireReader) Duration() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.result == nil || r.result.DurationMS <= 0 {
		return 0, false
	}
	return time.Duration(r.result.DurationMS) * time.Millisecond, true
}

// Result is the instance's final result, if the machine sent one.
func (r *wireReader) Result() (runner.Result, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.result == nil {
		return runner.Result{}, false, nil
	}
	out := runner.Result{Conclusion: r.result.Conclusion}
	if len(r.result.Outputs) > 0 {
		v, err := value.FromJSON(r.result.Outputs)
		if err != nil {
			return out, true, err
		}
		out.Outputs, _ = v.(value.Object)
	}
	return out, true, nil
}
