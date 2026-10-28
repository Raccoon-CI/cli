package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"raccoonci/internal/actions"
	"raccoonci/internal/artifact"
	"raccoonci/internal/blob"
	"raccoonci/internal/catalog"
	"raccoonci/internal/engine"
	"raccoonci/internal/local"
	"raccoonci/internal/nodejs"
	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// runWorkflow implements `raccoonci run`. ok is false when the run failed.
func runWorkflow(args []string, cat *catalog.Catalog) (ok bool, err error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var jobs, secrets, vars, inputs listFlag
	fs.Var(&jobs, "job", "run only this job and the jobs it needs (repeatable)")
	fs.Var(&secrets, "secret", "pass the environment variable `NAME` as secrets.NAME (repeatable; GITHUB_TOKEN is passed when set)")
	fs.Var(&vars, "var", "set vars.`NAME=VALUE` (repeatable)")
	fs.Var(&inputs, "input", "set inputs.`NAME=VALUE` (repeatable)")
	event := fs.String("event", "push", "the event that triggered the run")
	workDir := fs.String("workdir", "", "where jobs run (default: a new directory under the user cache dir)")
	parallel := fs.Int("parallel", max(1, runtime.NumCPU()/2), "how many job runs at once")
	debug := fs.Bool("debug", false, "show ::debug:: output")
	install := fs.String("install", "toolchains", "what to install for each job before it runs: none; toolchains (Node.js, Go... into the tool cache, which touches nothing else); or all (also apt packages, with sudo)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci run [flags] <workflow.yml>")
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
	repo, ok := repoRoot(path)
	if !ok {
		repo = gitTopLevel(filepath.Dir(path))
	}

	secretValues := map[string]string{}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		secretValues["GITHUB_TOKEN"] = t
	}
	for _, name := range secrets {
		v, ok := os.LookupEnv(name)
		if !ok {
			return false, fmt.Errorf("-secret %s: no such environment variable", name)
		}
		secretValues[name] = v
	}
	varObj, err := keyValues("-var", vars)
	if err != nil {
		return false, err
	}
	inputObj, err := keyValues("-input", inputs)
	if err != nil {
		return false, err
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return false, err
	}
	cache = filepath.Join(cache, "raccoonci")
	if *workDir == "" {
		*workDir = filepath.Join(cache, "runs", time.Now().Format("20060102-150405"))
	}
	// Steps run in other directories, so every path handed to them must be
	// absolute.
	if *workDir, err = filepath.Abs(*workDir); err != nil {
		return false, err
	}
	images, err := runnerimages.Embedded()
	if err != nil {
		return false, err
	}

	github := local.Github(repo, path, w.Name, *event)
	if *event == "workflow_dispatch" && len(inputObj) > 0 {
		ev, _ := github.Get("event")
		github = github.Set("event", ev.(value.Object).Set("inputs", inputObj))
	}
	repository, _ := github.Get("repository")
	node := nodejs.New(filepath.Join(cache, "node"))
	node.Catalog = cat
	printer := &local.Printer{Out: os.Stdout, Debug: *debug}

	toolCache := filepath.Join(cache, "toolcache")
	var prov *provision.Provisioner
	switch *install {
	case "none":
	case "toolchains", "all":
		prov = &provision.Provisioner{ToolCache: toolCache, Catalog: cat, Installers: provision.DefaultInstallers(nil)}
		if *install == "all" {
			if prov.Apt, err = provision.NewApt(); err != nil {
				fmt.Fprintf(os.Stderr, "raccoonci: -install=all: %v; missing packages will only be reported\n", err)
			}
		}
	default:
		return false, fmt.Errorf("-install %q: want none, toolchains or all", *install)
	}
	// The scan reads version files from the repo; confine it to the repo, even
	// through symlinks.
	repoDir, err := os.OpenRoot(repo)
	if err != nil {
		return false, err
	}
	defer repoDir.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runID, _ := github.Get("run_id")
	svcEnv, stopArtifacts, err := localArtifacts(filepath.Join(*workDir, "_artifacts"), runID.(string))
	if err != nil {
		return false, fmt.Errorf("artifact service: %w", err)
	}
	defer stopArtifacts()

	fmt.Printf("running %s in %s\n", path, *workDir)
	res, err := engine.Run(ctx, w, engine.Options{
		Github:      github,
		Secrets:     secretValues,
		Vars:        varObj,
		Inputs:      inputObj,
		Images:      images,
		Catalog:     cat,
		Provision:   prov,
		Repo:        repoDir.FS(),
		WorkDir:     *workDir,
		Jobs:        jobs,
		MaxParallel: *parallel,
		Runner: runner.Config{
			ToolCache:  toolCache,
			Actions:    actions.NewStore(filepath.Join(*workDir, "_actions"), secretValues["GITHUB_TOKEN"]),
			Node:       node.Binary,
			Checkout:   local.Checkout(repo, repository.(string)),
			ServiceEnv: svcEnv,
			RunnerName: "raccoonci-local",
			OnEvent:    printer.Event,
		},
	})
	if err != nil {
		return false, err
	}
	fmt.Println()
	for _, j := range res.Jobs {
		fmt.Printf("%-8s %s\n", j.Conclusion, j.ID)
	}
	return res.Conclusion == runner.Success, nil
}

func keyValues(flagName string, list []string) (value.Object, error) {
	var o value.Object
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q: want NAME=VALUE", flagName, kv)
		}
		o = o.Set(k, v)
	}
	return o, nil
}

func gitTopLevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || errors.Is(err, exec.ErrNotFound) {
			abs, _ := filepath.Abs(dir)
			return abs
		}
	}
	return strings.TrimSpace(string(out))
}

// localArtifacts serves the artifact protocol on a loopback port, keeping the
// files under dir, and returns the environment that points steps at it. The
// returned func shuts it down.
func localArtifacts(dir, runID string) (map[string]string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		ln.Close()
		return nil, nil, err
	}
	base := "http://" + ln.Addr().String()
	store, err := blob.NewDir(dir, base, key)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	signer, err := artifact.NewSigner(key)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	svc, err := artifact.New(artifact.Config{Store: store, Signer: signer, BaseURL: base})
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	// One token for the whole run: every job of a local run is the same user.
	env, err := svc.Env(artifact.Scope{Run: runID, Job: "local"}, 24*time.Hour)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(blob.DownloadPrefix, store.Handler())
	mux.Handle("/", svc.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	return env, func() {
		srv.Close()
		svc.Close()
		store.Close()
	}, nil
}
