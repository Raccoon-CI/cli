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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/engine"
	"raccoonci/internal/instabox"
	"raccoonci/internal/runner"
	"raccoonci/internal/value"
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

func isBool(flagName string) bool { return flagName == "debug" }

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
	env       map[string]string // what steps need to reach the artifact and cache services
	install   string
	event     func(runner.Event)

	prefix string
	n      atomic.Int32
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
	input, err := json.Marshal(instanceSpec{Workflow: rr.workflow, Job: inst.Job.ID, Contexts: contexts,
		ServiceEnv: rr.env, Install: rr.install})
	if err != nil {
		return runner.Result{}, err
	}
	reader := &wireReader{name: inst.Name, event: rr.event}
	defer reader.Flush()
	res, err := rr.provider.Run(ctx, fmt.Sprintf("%s-%d", rr.prefix, rr.n.Add(1)), dispatch.Job{
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
	})
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
	return out, nil
}

func orEmptyObject(o value.Object) value.Object {
	if o == nil {
		return value.Object{}
	}
	return o
}

// makeSourceTar archives repo into a file under dir.
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
	if err := dispatch.TarDir(f, repo); err != nil {
		return "", err
	}
	return p, f.Close()
}
