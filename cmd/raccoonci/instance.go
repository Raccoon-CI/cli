package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"raccoonci/internal/actions"
	"raccoonci/internal/catalog"
	"raccoonci/internal/engine"
	"raccoonci/internal/local"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/value"
	"raccoonci/internal/wire"
	"raccoonci/internal/workflow"
)

// What a controller and a machine say to each other lives in internal/wire.
type (
	instanceSpec = wire.InstanceSpec
	wireLine     = wire.Line
	wireResult   = wire.Result
	wireReader   = wire.Reader
)

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
	enc := wire.NewEncoder(os.Stdout)
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
			OnEvent:    func(e runner.Event) { enc.Write(wireLine{Event: &e}) },
		},
	}, job, obj("matrix"), obj("strategy"), obj("needs"))

	outputs, err := json.Marshal(res.Outputs)
	if err != nil {
		return err
	}
	enc.Write(wireLine{Result: &wireResult{Conclusion: res.Conclusion, Outputs: outputs, DurationMS: time.Since(began).Milliseconds()}})
	return nil
}
