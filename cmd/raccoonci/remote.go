package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/catalog"
	"github.com/Raccoon-CI/raccoonci/internal/dispatch"
	"github.com/Raccoon-CI/raccoonci/internal/engine"
	"github.com/Raccoon-CI/raccoonci/internal/envsnap"
	"github.com/Raccoon-CI/raccoonci/internal/instabox"
	"github.com/Raccoon-CI/raccoonci/internal/runner"
	"github.com/Raccoon-CI/raccoonci/internal/sched"
	"github.com/Raccoon-CI/raccoonci/internal/value"
	"github.com/Raccoon-CI/raccoonci/internal/wire"
)

// remoteFlags are the `run` flags that choose where it runs. They are taken
// out of the argument list; `run` parses the rest.
type remoteFlags struct {
	on, listen, controllerURL, snapshot string
	startAgent                          bool
}

// splitRemote pulls -on/-listen/-controller-url/-snapshot/-start-agent out of
// the flags before the workflow path.
func splitRemote(args []string) (rf remoteFlags, rest []string, err error) {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		target := map[string]*string{"on": &rf.on, "listen": &rf.listen, "controller-url": &rf.controllerURL, "snapshot": &rf.snapshot}[name]
		switch {
		case name == "start-agent":
			rf.startAgent = !hasVal || val == "true"
		case target != nil:
			if !hasVal {
				if i+1 >= len(args) {
					return rf, nil, fmt.Errorf("-%s needs a value", name)
				}
				i++
				val = args[i]
			}
			*target = val
		default:
			rest = append(rest, args[i])
			// A flag that takes its value as the next word keeps it, so the
			// value is not mistaken for the workflow path.
			if !hasVal && !isBool(name) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		}
	}
	return rf, append(rest, args[i:]...), nil
}

// isBool lists the flags of `run` that take no value, so splitRemote does not
// take the word after one for its value. Keep it in step with run.go.
func isBool(flagName string) bool {
	switch flagName {
	case "debug", "affected", "only-if-triggered", "cache-s3-path-style", "no-pack":
		return true
	}
	return false
}

// check validates the flags of `run -on` and returns the API key.
func (rf *remoteFlags) check() (key string, err error) {
	if rf.on != "instabox" {
		return "", fmt.Errorf("-on %q: only instabox is supported", rf.on)
	}
	// INSTABOX_API_KEY is reserved on InstaBox machines (the MCP uses it);
	// IBX_API_KEY is the name an org Secret can have there.
	key = cmp.Or(os.Getenv("INSTABOX_API_KEY"), os.Getenv("IBX_API_KEY"))
	if key == "" {
		return "", errors.New("-on instabox needs INSTABOX_API_KEY (or IBX_API_KEY) in the environment")
	}
	rf.snapshot = cmp.Or(rf.snapshot, os.Getenv("RACCOONCI_INSTABOX_SNAPSHOT"))
	if rf.snapshot == "" {
		return "", errors.New("-on instabox needs -snapshot (or RACCOONCI_INSTABOX_SNAPSHOT): the runner snapshot to start machines from")
	}
	if rf.listen == "" || rf.controllerURL == "" {
		return "", errors.New("-on instabox needs -listen ADDR and -controller-url URL: where this process serves the machines, and the https address they reach it at (for example a published port)")
	}
	if !strings.HasPrefix(rf.controllerURL, "https://") {
		return "", errors.New("-controller-url must be https: the machines' tokens and secrets travel on it")
	}
	rf.controllerURL = strings.TrimRight(rf.controllerURL, "/")
	return key, nil
}

// reportUsage prints how many machines a run used and for how long, with the
// cost at InstaBox's unit prices as of the date in instabox.InstaBoxPrices (a
// guide: they change, which is why the raw usage can be written to a file for
// pricing later).
func reportUsage(us []instabox.Usage, file string) {
	if file != "" {
		if f, err := os.Create(file); err != nil {
			fmt.Fprintf(os.Stderr, "raccoonci: -usage-file: %v\n", err)
		} else {
			if err := instabox.WriteUsage(f, us); err != nil {
				fmt.Fprintf(os.Stderr, "raccoonci: -usage-file: %v\n", err)
			}
			f.Close()
		}
	}
	if len(us) == 0 {
		return
	}
	s := instabox.InstaBoxPrices.Summarize(us)
	fmt.Fprintf(os.Stderr, "raccoonci: %d machines, %.0f machine-seconds, about $%.4f at InstaBox's unit prices of 2026-10-29 (not counting snapshot storage or egress)\n",
		s.Machines, s.Seconds, s.Cost)
}

// remoteRun sends the job instances of one run to machines, one machine each.
type remoteRun struct {
	provider *instabox.Provider
	// workflow is the workflow's path in the source tree.
	workflow string
	// sourceTar holds the source tree as a .tar.gz, made once for all machines.
	sourceTar string
	github    value.Object
	vars      value.Object
	inputs    value.Object
	secrets   map[string]string
	// jobEnv is what steps need to reach the artifact and cache services,
	// with a token for the job id given.
	jobEnv  func(job string) (map[string]string, error)
	install string
	event   func(runner.Event)

	prefix string
	n      atomic.Int32

	// Environment snapshots (-env-snapshots). snaps is nil when they are off.
	snaps   *envsnap.Index
	cat     *catalog.Catalog
	repoFS  fs.FS
	trusted bool // the event comes from the repository's own code, not a fork's
	mu      sync.Mutex
	claimed map[string]bool // keys being snapshotted now: one machine per key does it

	// Packing: pool is nil when it is off. Instances the policy calls short
	// share the pool's machines; the others get a machine of their own.
	pool   *instabox.Pool
	policy *sched.Policy
}

// packable says whether the instance may share a machine, and why not
// otherwise. Jobs with containers or services start docker networks, and
// publish ports on the machine, which a neighbour's would collide with.
func (rr *remoteRun) packable(inst engine.RemoteInstance, key string) (bool, string) {
	if rr.pool == nil || rr.policy == nil {
		return false, ""
	}
	if inst.Job.Container != nil || inst.Job.Services != nil {
		return false, "uses containers or services"
	}
	short, est, limit := rr.policy.Short(key)
	if !short {
		if est > 0 {
			return false, fmt.Sprintf("took %v, not under %v", est.Round(100*time.Millisecond), limit.Round(time.Second))
		}
		return false, "no history yet"
	}
	return true, fmt.Sprintf("took %v, under %v", est.Round(100*time.Millisecond), limit.Round(time.Second))
}

func (rr *remoteRun) say(format string, args ...any) {
	if rr.provider.Logf != nil {
		rr.provider.Logf(format, args...)
	}
}

// restore finds the environment snapshot for an instance. key is empty when
// the instance cannot use one (nothing to cache, or dependencies not pinned).
func (rr *remoteRun) restore(inst engine.RemoteInstance) (key, from string) {
	if rr.snaps == nil || len(inst.Toolchains) == 0 {
		return "", ""
	}
	k, err := envsnap.Compute(rr.repoFS, rr.cat, rr.provider.Snapshot, inst.Toolchains)
	if err != nil {
		return "", ""
	}
	if e, ok := rr.snaps.Lookup(k.Hash); ok {
		rr.say("%s: starting from environment snapshot %s", inst.Name, e.Snapshot)
		return k.Hash, e.Snapshot
	}
	return k.Hash, ""
}

// keep decides whether a finished instance's machine becomes the environment
// snapshot for key. Only a successful run that started from the runner
// snapshot, on a trusted event and with no secrets in play (a job can leave
// them on disk, and a snapshot copies the disks), and only one machine per key.
func (rr *remoteRun) keep(key string, reader *wire.Reader) string {
	if key == "" {
		return ""
	}
	reader.Flush()
	out, ok, _ := reader.Result()
	if !ok || out.Conclusion != runner.Success || !rr.trusted || len(rr.secrets) > 0 {
		return ""
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.claimed[key] {
		return ""
	}
	if rr.claimed == nil {
		rr.claimed = map[string]bool{}
	}
	rr.claimed[key] = true
	var b [3]byte
	rand.Read(b[:])
	return "rc-env-" + key[:12] + "-" + hex.EncodeToString(b[:])
}

func (rr *remoteRun) kept(key, name string, snapID string, err error) {
	rr.mu.Lock()
	delete(rr.claimed, key)
	rr.mu.Unlock()
	ctx := context.Background()
	if err != nil {
		rr.say("environment snapshot %s not saved: %v", name, err)
		if snapID != "" {
			rr.provider.Client.DeleteSnapshot(ctx, snapID)
		}
		return
	}
	evicted, perr := rr.snaps.Put(key, snapID, rr.provider.Snapshot)
	if perr != nil {
		rr.say("environment snapshot index: %v", perr)
	}
	rr.say("saved environment snapshot %s (%s)", name, snapID)
	for _, e := range evicted {
		if derr := rr.provider.Client.DeleteSnapshot(ctx, e.Snapshot); derr != nil {
			rr.say("could not delete old environment snapshot %s: %v", e.Snapshot, derr)
		}
	}
}

func newRemoteRun(p *instabox.Provider) *remoteRun {
	var b [3]byte
	rand.Read(b[:])
	return &remoteRun{provider: p, prefix: "rc-" + hex.EncodeToString(b[:])}
}

// run is engine.Options.Remote.
func (rr *remoteRun) run(ctx context.Context, inst engine.RemoteInstance) (runner.Result, error) {
	contexts, err := json.Marshal(value.Object{
		{Key: "github", Value: rr.github},
		{Key: "matrix", Value: orEmptyObject(inst.Matrix)},
		{Key: "strategy", Value: orEmptyObject(inst.Strategy)},
		{Key: "needs", Value: orEmptyObject(inst.Needs)},
		{Key: "inputs", Value: orEmptyObject(rr.inputs)},
		{Key: "vars", Value: orEmptyObject(rr.vars)},
	})
	if err != nil {
		return runner.Result{}, err
	}
	// Each instance gets a token of its own. The id is unique per instance (a
	// matrix has many with one job id) and uses only what a token may hold.
	svcEnv, err := rr.jobEnv(fmt.Sprintf("%s-%d", inst.Job.ID, rr.n.Add(1)))
	if err != nil {
		return runner.Result{}, err
	}
	input, err := json.Marshal(wire.InstanceSpec{Protocol: wire.Version, Workflow: rr.workflow, Job: inst.Job.ID, Contexts: contexts,
		ServiceEnv: svcEnv, Install: rr.install})
	if err != nil {
		return runner.Result{}, err
	}
	reader := &wire.Reader{Name: inst.Name, Event: rr.event}
	defer reader.Flush()
	job := dispatch.Job{
		Command: "run-instance",
		Input:   input,
		Secrets: rr.secrets,
		Source: func(w io.Writer) error {
			f, err := os.Open(rr.sourceTar)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		},
		Log: reader,
	}
	key, from := rr.restore(inst)
	histKey := sched.Key(rr.workflow, inst.Name)
	if ok, why := rr.packable(inst, histKey); rr.pool != nil {
		switch {
		case ok && key == "":
			rr.say("%s: shares a machine (%s)", inst.Name, why)
			res, err := rr.pool.Run(ctx, job)
			return rr.finish(histKey, reader, res, err)
		case ok:
			rr.say("%s: gets a machine of its own (it can use an environment snapshot)", inst.Name)
		case why != "":
			rr.say("%s: gets a machine of its own (%s)", inst.Name, why)
		}
	}
	attempt := func(from string) (dispatch.Result, error) {
		opts := &instabox.RunOptions{From: from}
		if key != "" && from == "" {
			var name string
			opts.Keep = func(dispatch.Result) string {
				name = rr.keep(key, reader)
				return name
			}
			opts.Kept = func(id string, err error) { rr.kept(key, name, id, err) }
		}
		return rr.provider.Run(ctx, fmt.Sprintf("%s-%d", rr.prefix, rr.n.Add(1)), job, opts)
	}
	res, err := attempt(from)
	var apiErr *instabox.Error
	if from != "" && errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 404) {
		// The snapshot is gone (deleted elsewhere): forget it and start from the runner snapshot.
		rr.say("environment snapshot %s is gone (%v); starting from the runner snapshot", from, err)
		rr.snaps.Remove(key)
		res, err = attempt("")
	}
	return rr.finish(histKey, reader, res, err)
}

// finish turns what the machine reported into the instance's result, and
// remembers how long a successful job took there.
func (rr *remoteRun) finish(histKey string, reader *wire.Reader, res dispatch.Result, err error) (runner.Result, error) {
	if err != nil {
		return runner.Result{}, err
	}
	if res.Error != "" {
		return runner.Result{}, errors.New("the machine could not run the job: " + res.Error)
	}
	reader.Flush()
	out, ok, err := reader.Result()
	switch {
	case err != nil:
		return runner.Result{}, fmt.Errorf("the machine's result: %w", err)
	case !ok:
		return runner.Result{}, fmt.Errorf("the machine exited with status %d without a result (its output is above)", res.ExitCode)
	}
	if d, ok := reader.Duration(); ok && out.Conclusion == runner.Success && rr.policy != nil {
		if err := rr.policy.History.Record(histKey, d); err != nil {
			rr.say("could not save the job's duration: %v", err)
		}
	}
	return out, nil
}

func orEmptyObject(o value.Object) value.Object {
	if o == nil {
		return value.Object{}
	}
	return o
}

// makeSourceTar archives repo into a file under dir.
// ignoredBy returns a filter for the files git ignores in repo (node_modules,
// build output, local config): a clone on GitHub does not have them, and they
// can be most of the tree. The .git directory stays, so steps that run git
// work. When repo is not a git checkout or git is missing, nothing is skipped.
func ignoredBy(repo string) func(rel string, isDir bool) bool {
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory").Output()
	if err != nil {
		return nil
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ignored[strings.TrimSuffix(p, "/")] = true
		}
	}
	return func(rel string, isDir bool) bool { return ignored[rel] }
}

func makeSourceTar(repo, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "_source.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := dispatch.TarDirSkipping(f, repo, ignoredBy(repo)); err != nil {
		return "", err
	}
	return p, f.Close()
}
