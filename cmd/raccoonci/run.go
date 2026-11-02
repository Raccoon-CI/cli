package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"raccoonci/internal/missing"
	"runtime"
	"strings"
	"syscall"
	"time"

	"raccoonci/internal/actions"
	"raccoonci/internal/catalog"
	"raccoonci/internal/cliflag"
	"raccoonci/internal/cliutil"
	"raccoonci/internal/dispatch"
	"raccoonci/internal/engine"
	"raccoonci/internal/envsnap"
	"raccoonci/internal/instabox"
	"raccoonci/internal/local"
	"raccoonci/internal/provision"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnercmd"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/sched"
	"raccoonci/internal/trigger"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

// runWorkflow implements `raccoonci run`. ok is false when the run failed. With
// remote set (`-on instabox`), the job graph is still resolved here but every
// job instance runs on a machine of its own.
func runWorkflow(args []string, cat *catalog.Catalog, remote *remoteFlags) (ok bool, err error) {
	processStart := time.Now()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var jobs, secrets, vars, inputs cliflag.List
	fs.Var(&jobs, "job", "run only this job and the jobs it needs (repeatable)")
	fs.Var(&secrets, "secret", "pass the environment variable `NAME` as secrets.NAME (repeatable; GITHUB_TOKEN is passed when set)")
	fs.Var(&vars, "var", "set vars.`NAME=VALUE` (repeatable)")
	fs.Var(&inputs, "input", "set inputs.`NAME=VALUE` (repeatable)")
	event := fs.String("event", "push", "the event that triggered the run")
	workDir := fs.String("workdir", "", "where jobs run (default: a new directory under the user cache dir)")
	parallel := fs.Int("parallel", max(1, runtime.NumCPU()/2), "how many job runs at once")
	debug := fs.Bool("debug", false, "show ::debug:: output")
	cacheProtocol := fs.String("cache-protocol", "v1", "which actions/cache protocol to offer steps: v1 or v2 (both read and write the same entries)")
	cacheLimitGB := fs.Int("cache-limit-gb", 10, "the most the cache of a repository may hold, in GiB (GitHub allows 10); the least recently used entries are evicted first; 0 means no limit")
	var evFlags cliutil.EventFlags
	evFlags.Register(fs, false)
	onlyIfTriggered := fs.Bool("only-if-triggered", false, "run nothing unless the workflow's on: (branches, tags, paths, types) is triggered by the event, described by -event, -ref, -base-ref, -file ...; by default `run` assumes it is")
	onlyAffected := fs.Bool("affected", false, "leave out the jobs the changed files do not concern (see `raccoonci affected`); the changes come from git, or -file/-before")
	usageFile := fs.String("usage-file", "", "with -on: write what each machine used (when it was requested, ready and deleted, its size) to this file, one JSON object per line; money is worked out from it, not stored")
	var s3f runnercmd.CacheS3Flags
	s3f.Register(fs)
	warm := fs.Int("warm", 0, "with -on: keep this many machines paused and ready between runs; a run resumes one (under a second) instead of creating one (6 to 20 seconds), and tops them up when it ends. A paused machine costs its disks only")
	noPack := fs.Bool("no-pack", false, "with -on: give every job instance a machine of its own, even those that took only seconds in earlier runs")
	packSlots := fs.Int("pack-slots", 0, "with -on: how many short job instances share a machine at once (default: its number of vCPUs; 1 runs them one after another)")
	envSnapshots := fs.Int("env-snapshots", 0, "with -on: keep up to this many environment snapshots per repository: after a successful run, a machine whose job installed dependencies (all lockfiles pinned) is saved, and later runs with the same lockfiles start from it; 0 turns it off")
	toolCacheLimitGB := fs.Int("toolcache-limit-gb", 10, "the most the tool cache (Node.js, Go, Python... installed for jobs) may hold, in GiB; before a run, the versions used longest ago go first, but none used in the last 24 hours; 0 means no limit")
	install := fs.String("install", "toolchains", "what to install for each job before it runs: none; toolchains (Node.js, Go... into the tool cache, which touches nothing else); or all (also apt packages, with sudo)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci run [flags] <workflow.yml>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	parallelSet := false
	fs.Visit(func(f *flag.Flag) { parallelSet = parallelSet || f.Name == "parallel" })
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	if *cacheProtocol != "v1" && *cacheProtocol != "v2" {
		return false, fmt.Errorf("-cache-protocol %q: want v1 or v2", *cacheProtocol)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	w, err := workflow.Parse(src)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	repo, ok := cliutil.RepoRoot(path)
	if !ok {
		repo = cliutil.GitTopLevel(filepath.Dir(path))
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
	varObj, err := cliutil.KeyValues("-var", vars)
	if err != nil {
		return false, err
	}
	inputObj, err := cliutil.KeyValues("-input", inputs)
	if err != nil {
		return false, err
	}
	if *event == "workflow_dispatch" {
		// As on GitHub: declared defaults fill in, required inputs must be
		// given, choices are checked, and booleans and numbers are typed.
		given := map[string]string{}
		for _, f := range inputObj {
			given[f.Key] = f.Value.(string)
		}
		if inputObj, err = trigger.DispatchInputs(w.On, given); err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
	}
	if *onlyIfTriggered {
		res, err := trigger.Match(w.On, evFlags.Event(repo, *event))
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		if !res.Triggered {
			fmt.Printf("%s is not triggered by this event: %s\n", path, res.Reason)
			return true, nil
		}
	}

	installHere := *install
	var apiKey string
	if remote != nil {
		// The machines install what their jobs need; nothing is installed here.
		installHere = "none"
		if err := runnercmd.ValidInstall(*install); err != nil {
			return false, err
		}
		if apiKey, err = remote.check(); err != nil {
			return false, err
		}
		if !parallelSet {
			*parallel = 8
		}
	}
	rt, err := runnercmd.NewRunEnv(cat, installHere)
	if err != nil {
		return false, err
	}
	if installHere != "none" {
		removed, perr := provision.Prune(rt.ToolCache, int64(*toolCacheLimitGB)<<30, 24*time.Hour, time.Now())
		if perr != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: tool cache cleanup: %v\n", perr)
		}
		if len(removed) > 0 {
			fmt.Fprintf(os.Stderr, "raccoonci: tool cache is over %d GiB; removed %s\n", *toolCacheLimitGB, strings.Join(removed, ", "))
		}
	}
	cache := rt.CacheDir
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
	printer := &local.Printer{Out: os.Stdout, Debug: *debug}
	// The scan reads version files from the repo; confine it to the repo, even
	// through symlinks.
	repoDir, err := os.OpenRoot(repo)
	if err != nil {
		return false, err
	}
	defer repoDir.Close()

	var unaffectedFn func(*workflow.Job) (string, bool)
	if *onlyAffected {
		wfRel, err := filepath.Rel(repo, cliutil.AbsPath(path))
		if err != nil || strings.HasPrefix(wfRel, "..") {
			return false, fmt.Errorf("%s is outside its repository %s", path, repo)
		}
		ev := evFlags.Event(repo, *event)
		if unaffectedFn, err = cliutil.Unaffected(w, repoDir.FS(), filepath.ToSlash(wfRel), ev.Files); err != nil {
			return false, err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runID, _ := github.Get("run_id")
	var serve *runnercmd.PublicServe
	var rr *remoteRun
	if remote != nil {
		ctl := &dispatch.Controller{}
		serve = &runnercmd.PublicServe{Listen: remote.listen, PublicURL: remote.controllerURL,
			Mount: map[string]http.Handler{"/v1/": ctl.Handler()}}
		rr = newRemoteRun(&instabox.Provider{
			Client:        &instabox.Client{Key: apiKey, URL: os.Getenv("INSTABOX_API_URL")},
			Controller:    ctl,
			ControllerURL: remote.controllerURL,
			Snapshot:      remote.snapshot,
			StartAgent:    remote.startAgent,
			Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, "raccoonci: "+f+"\n", a...) },
		})
		rel, err := filepath.Rel(repo, cliutil.AbsPath(path))
		if err != nil || strings.HasPrefix(rel, "..") {
			return false, fmt.Errorf("%s is outside its repository %s", path, repo)
		}
		rr.workflow, rr.github, rr.vars, rr.inputs = filepath.ToSlash(rel), github, varObj, inputObj
		rr.secrets, rr.install, rr.event = secretValues, *install, printer.Event
		defer rr.provider.Wait() // snapshots being saved and machines being deleted
		if *warm > 0 {
			rr.provider.Spares = &instabox.Spares{P: rr.provider, Want: *warm}
		}
		if !*noPack {
			histDir := filepath.Join(cache, "sched")
			hist, err := sched.OpenHistory(filepath.Join(histDir, cliutil.RepoDirName(repository.(string))+".json"))
			if err != nil {
				return false, err
			}
			rr.pool = &instabox.Pool{P: rr.provider, Slots: *packSlots, Prefix: rr.prefix + "p"}
			rr.policy = &sched.Policy{History: hist, Startup: rr.provider.StartupTime}
			defer rr.pool.Close()
		}
		if *envSnapshots > 0 {
			idxDir := filepath.Join(cache, "envsnap")
			if err := os.MkdirAll(idxDir, 0o755); err != nil {
				return false, err
			}
			if rr.snaps, err = envsnap.Open(filepath.Join(idxDir, cliutil.RepoDirName(repository.(string))+".json"), *envSnapshots); err != nil {
				return false, err
			}
			rr.cat, rr.repoFS = cat, repoDir.FS()
			// A fork's pull request runs its author's code; what it leaves on the
			// disk must not seed later runs.
			rr.trusted = !strings.HasPrefix(*event, "pull_request")
		}
		if rr.sourceTar, err = makeSourceTar(repo, *workDir); err != nil {
			return false, fmt.Errorf("packing the source: %w", err)
		}
	}
	var missingStore *missing.Store
	var wfID string
	if rel, err := filepath.Rel(repo, cliutil.AbsPath(path)); err == nil && !strings.HasPrefix(rel, "..") {
		wfID = filepath.ToSlash(rel)
	}
	if rt.Prov != nil && remote == nil {
		// Only a local run provisions here; a machine of -on instabox has
		// its own run-instance and is deleted afterwards.
		if missingStore, err = missing.Open(filepath.Join(rt.CacheDir, "missing", cliutil.RepoDirName(repository.(string))+".json")); err != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: remembering missing commands is off: %v\n", err)
			missingStore = nil
		}
	}
	cacheBucket, err := s3f.Store(cliutil.RepoDirName(repository.(string)))
	if err != nil {
		return false, err
	}
	svc, err := runnercmd.LocalServices(filepath.Join(*workDir, "_artifacts"),
		filepath.Join(cache, "actions-cache", cliutil.RepoDirName(repository.(string))), runID.(string), *cacheProtocol == "v2", int64(*cacheLimitGB)<<30, serve, cacheBucket)
	if err != nil {
		return false, fmt.Errorf("artifact service: %w", err)
	}
	defer svc.Stop()
	var remoteFn func(context.Context, engine.RemoteInstance) (runner.Result, error)
	if rr != nil {
		rr.jobEnv = svc.JobEnv
		remoteFn = rr.run
	}

	fmt.Printf("running %s in %s\n", path, *workDir)
	res, err := engine.Run(ctx, w, engine.Options{
		Remote:      remoteFn,
		Unaffected:  unaffectedFn,
		Github:      github,
		Secrets:     secretValues,
		Vars:        varObj,
		Inputs:      inputObj,
		Images:      images,
		Catalog:     cat,
		Provision:   rt.Prov,
		Missing:     missingStore,
		WorkflowID:  wfID,
		Repo:        repoDir.FS(),
		WorkDir:     *workDir,
		Jobs:        jobs,
		MaxParallel: *parallel,
		Runner: runner.Config{
			ToolCache: rt.ToolCache,
			Actions:   actions.NewStore(filepath.Join(*workDir, "_actions"), secretValues["GITHUB_TOKEN"]),
			Node:      rt.Node.Binary,
			NodeMusl:  rt.NodeMusl.Binary,
			// Container jobs reach the Node.js runtimes and the downloaded
			// actions at the same paths as the host.
			Mounts:            []string{rt.Node.Dir, filepath.Join(*workDir, "_actions")},
			Checkout:          local.Checkout(repo, repository.(string)),
			ServiceEnv:        svc.Env,
			ContainerServices: svc.ServeNetwork,
			RunnerName:        "raccoonci-local",
			OnEvent:           printer.Event,
		},
	})
	// The answer is in; what follows (saving snapshots, deleting machines) is
	// housekeeping a user need not wait for, and a benchmark must not count.
	fmt.Fprintf(os.Stderr, "raccoonci: results ready after %.1f s\n", time.Since(processStart).Seconds())
	if rr != nil {
		if rr.pool != nil {
			rr.pool.Close()
		}
		rr.provider.Wait() // snapshots being saved and machines being deleted
		if rr.provider.Spares != nil && ctx.Err() == nil {
			// Housekeeping after the answer: have paused machines ready for the next run.
			if terr := rr.provider.Spares.TopUp(ctx); terr != nil {
				fmt.Fprintf(os.Stderr, "raccoonci: paused spare machines: %v\n", terr)
			}
			rr.provider.Wait()
		}
		reportUsage(rr.provider.Usage(), *usageFile)
	}
	if err != nil {
		return false, err
	}
	fmt.Println()
	for _, j := range res.Jobs {
		if j.Unchanged != "" {
			fmt.Printf("%-8s %s (not run: unaffected by the changes)\n", "-", j.ID)
			continue
		}
		fmt.Printf("%-8s %s\n", j.Conclusion, j.ID)
	}
	return res.Conclusion == runner.Success, nil
}
