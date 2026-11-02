package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Raccoon-CI/raccoonci/internal/cliutil"
	"github.com/Raccoon-CI/raccoonci/internal/engine"
	"github.com/Raccoon-CI/raccoonci/internal/envsnap"
	"github.com/Raccoon-CI/raccoonci/internal/runner"
	"github.com/Raccoon-CI/raccoonci/internal/runnercmd"
	"github.com/Raccoon-CI/raccoonci/internal/sched"
	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
	"github.com/Raccoon-CI/raccoonci/pkg/instabox"
)

// runWorkflow implements `raccoonci run`. The engine is runnercmd.Run; with
// remote set (`-on instabox`), the job graph is still resolved there but every
// job instance runs on a machine of its own, through the session made here.
func runWorkflow(args []string, cat *catalog.Catalog, remote *remoteFlags) (ok bool, err error) {
	var rf remoteRunFlags
	ext := &runnercmd.RunExt{Register: rf.register}
	if remote != nil {
		var apiKey string
		ext.Remote = &runnercmd.RemoteExt{
			Parallel: 8,
			Check: func() (err error) {
				apiKey, err = remote.check()
				return err
			},
			Start: func(in *runnercmd.RemoteStart) (runnercmd.RemoteSession, error) {
				return startRemote(in, remote, &rf, apiKey)
			},
		}
	}
	return runnercmd.Run(args, cat, ext)
}

// remoteRunFlags are the `run` flags that only `-on` uses.
type remoteRunFlags struct {
	usageFile                string
	warm, packSlots, envSnap int
	noPack                   bool
}

func (f *remoteRunFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.usageFile, "usage-file", "", "with -on: write what each machine used (when it was requested, ready and deleted, its size) to this file, one JSON object per line; money is worked out from it, not stored")
	fs.IntVar(&f.warm, "warm", 0, "with -on: keep this many machines paused and ready between runs; a run resumes one (under a second) instead of creating one (6 to 20 seconds), and tops them up when it ends. A paused machine costs its disks only")
	fs.BoolVar(&f.noPack, "no-pack", false, "with -on: give every job instance a machine of its own, even those that took only seconds in earlier runs")
	fs.IntVar(&f.packSlots, "pack-slots", 0, "with -on: how many short job instances share a machine at once (default: its number of vCPUs; 1 runs them one after another)")
	fs.IntVar(&f.envSnap, "env-snapshots", 0, "with -on: keep up to this many environment snapshots per repository: after a successful run, a machine whose job installed dependencies (all lockfiles pinned) is saved, and later runs with the same lockfiles start from it; 0 turns it off")
}

// remoteSession is a remoteRun as a runnercmd.RemoteSession.
type remoteSession struct {
	*remoteRun
	serve     *runnercmd.PublicServe
	usageFile string
}

func (s *remoteSession) Serve() *runnercmd.PublicServe { return s.serve }

func (s *remoteSession) SetJobEnv(f func(job string) (map[string]string, error)) { s.jobEnv = f }

func (s *remoteSession) Run(ctx context.Context, inst engine.RemoteInstance) (runner.Result, error) {
	return s.run(ctx, inst)
}

func (s *remoteSession) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
	s.provider.Wait() // snapshots being saved and machines being deleted
}

// Finish is what follows the answer: housekeeping a user need not wait for.
func (s *remoteSession) Finish(ctx context.Context) {
	if s.pool != nil {
		s.pool.Close()
	}
	s.provider.Wait() // snapshots being saved and machines being deleted
	if s.provider.Spares != nil && ctx.Err() == nil {
		// Housekeeping after the answer: have paused machines ready for the next run.
		if terr := s.provider.Spares.TopUp(ctx); terr != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: paused spare machines: %v\n", terr)
		}
		s.provider.Wait()
	}
	reportUsage(s.provider.Usage(), s.usageFile)
}

func startRemote(in *runnercmd.RemoteStart, remote *remoteFlags, rf *remoteRunFlags, apiKey string) (runnercmd.RemoteSession, error) {
	ctl := &dispatch.Controller{}
	serve := &runnercmd.PublicServe{Listen: remote.listen, PublicURL: remote.controllerURL,
		Mount: map[string]http.Handler{"/v1/": ctl.Handler()}}
	rr := newRemoteRun(&instabox.Provider{
		Client:        &instabox.Client{Key: apiKey, URL: os.Getenv("INSTABOX_API_URL")},
		Controller:    ctl,
		ControllerURL: remote.controllerURL,
		Snapshot:      remote.snapshot,
		StartAgent:    remote.startAgent,
		Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, "raccoonci: "+f+"\n", a...) },
	})
	rel, err := filepath.Rel(in.Repo, cliutil.AbsPath(in.Path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%s is outside its repository %s", in.Path, in.Repo)
	}
	rr.workflow, rr.github, rr.vars, rr.inputs = filepath.ToSlash(rel), in.Github, in.Vars, in.Inputs
	rr.secrets, rr.install, rr.event = in.Secrets, in.Install, in.Event
	sess := &remoteSession{remoteRun: rr, serve: serve, usageFile: rf.usageFile}
	ok := false
	defer func() {
		if !ok {
			sess.Close()
		}
	}()
	if rf.warm > 0 {
		rr.provider.Spares = &instabox.Spares{P: rr.provider, Want: rf.warm}
	}
	if !rf.noPack {
		histDir := filepath.Join(in.CacheDir, "sched")
		hist, err := sched.OpenHistory(filepath.Join(histDir, cliutil.RepoDirName(in.Repository)+".json"))
		if err != nil {
			return nil, err
		}
		rr.pool = &instabox.Pool{P: rr.provider, Slots: rf.packSlots, Prefix: rr.prefix + "p"}
		rr.policy = &sched.Policy{History: hist, Startup: rr.provider.StartupTime}
	}
	if rf.envSnap > 0 {
		idxDir := filepath.Join(in.CacheDir, "envsnap")
		if err := os.MkdirAll(idxDir, 0o755); err != nil {
			return nil, err
		}
		if rr.snaps, err = envsnap.Open(filepath.Join(idxDir, cliutil.RepoDirName(in.Repository)+".json"), rf.envSnap); err != nil {
			return nil, err
		}
		rr.cat, rr.repoFS = in.Catalog, in.RepoFS
		// A fork's pull request runs its author's code; what it leaves on the
		// disk must not seed later runs.
		rr.trusted = !strings.HasPrefix(in.EventName, "pull_request")
	}
	if rr.sourceTar, err = makeSourceTar(in.Repo, in.WorkDir); err != nil {
		return nil, fmt.Errorf("packing the source: %w", err)
	}
	ok = true
	return sess, nil
}
