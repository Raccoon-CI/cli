package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/instabox"
)

// remoteFlags are the `run` flags that choose where it runs. They are taken
// out of the argument list; everything else is forwarded to the machine's
// `raccoonci run`.
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
			// A flag of the local run that takes its value as the next word
			// keeps it, so the value is not mistaken for the workflow path.
			if !hasVal && !isBool(name) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		}
	}
	return rf, append(rest, args[i:]...), nil
}

func isBool(flagName string) bool { return flagName == "debug" }

// runRemote implements `raccoonci run -on instabox`: the whole workflow runs
// on one new InstaBox machine made from a runner snapshot.
func runRemote(rf remoteFlags, args []string) (bool, error) {
	if rf.on != "instabox" {
		return false, fmt.Errorf("-on %q: only instabox is supported", rf.on)
	}
	// INSTABOX_API_KEY is reserved on InstaBox machines (the MCP uses it);
	// IBX_API_KEY is the name an org Secret can have there.
	key := cmp.Or(os.Getenv("INSTABOX_API_KEY"), os.Getenv("IBX_API_KEY"))
	if key == "" {
		return false, errors.New("-on instabox needs INSTABOX_API_KEY (or IBX_API_KEY) in the environment")
	}
	if rf.snapshot == "" {
		rf.snapshot = os.Getenv("RACCOONCI_INSTABOX_SNAPSHOT")
	}
	if rf.snapshot == "" {
		return false, errors.New("-on instabox needs -snapshot (or RACCOONCI_INSTABOX_SNAPSHOT): the runner snapshot to start machines from")
	}
	if rf.listen == "" || rf.controllerURL == "" {
		return false, errors.New("-on instabox needs -listen ADDR and -controller-url URL: where this process serves the machines, and the https address they reach it at (for example a published port)")
	}
	if !strings.HasPrefix(rf.controllerURL, "https://") {
		return false, errors.New("-controller-url must be https: the machine's token and secrets travel on it")
	}

	// The workflow path is the last argument; secrets are read here and sent
	// with the assignment, the rest of the flags go to the machine as they are.
	if len(args) == 0 {
		return false, errors.New("usage: raccoonci run -on instabox [flags] <workflow.yml>")
	}
	wf, err := filepath.Abs(args[len(args)-1])
	if err != nil {
		return false, err
	}
	repo, ok := repoRoot(wf)
	if !ok {
		repo = gitTopLevel(filepath.Dir(wf))
	}
	rel, err := filepath.Rel(repo, wf)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false, fmt.Errorf("%s is outside its repository %s", wf, repo)
	}
	fwd, secrets, err := takeSecrets(args[:len(args)-1])
	if err != nil {
		return false, err
	}
	fwd = append(fwd, filepath.ToSlash(rel))

	ctl := &dispatch.Controller{}
	ln, err := net.Listen("tcp", rf.listen)
	if err != nil {
		return false, err
	}
	srv := &http.Server{Handler: ctl.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	var suffix [4]byte
	rand.Read(suffix[:])
	name := "rc-" + hex.EncodeToString(suffix[:])
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := &instabox.Provider{
		Client:        &instabox.Client{Key: key, URL: os.Getenv("INSTABOX_API_URL")},
		Controller:    ctl,
		ControllerURL: strings.TrimRight(rf.controllerURL, "/"),
		Snapshot:      rf.snapshot,
		StartAgent:    rf.startAgent,
		Logf:          func(f string, a ...any) { fmt.Fprintf(os.Stderr, "raccoonci: "+f+"\n", a...) },
	}
	res, err := p.Run(ctx, name, dispatch.Job{
		Args:    fwd,
		Secrets: secrets,
		Source:  func(w io.Writer) error { return dispatch.TarDir(w, repo) },
		Log:     os.Stdout,
	})
	if err != nil {
		return false, err
	}
	if res.Error != "" {
		return false, errors.New("the machine could not run the job: " + res.Error)
	}
	return res.ExitCode == 0, nil
}

// takeSecrets removes -secret NAME flags and reads their values from the
// environment (GITHUB_TOKEN too, when set), as a local run does.
func takeSecrets(args []string) (rest []string, secrets map[string]string, err error) {
	secrets = map[string]string{}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		secrets["GITHUB_TOKEN"] = t
	}
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if name != "secret" || !strings.HasPrefix(args[i], "-") {
			rest = append(rest, args[i])
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, nil, errors.New("-secret needs a name")
			}
			i++
			val = args[i]
		}
		v, ok := os.LookupEnv(val)
		if !ok {
			return nil, nil, fmt.Errorf("-secret %s: no such environment variable", val)
		}
		secrets[val] = v
	}
	return rest, secrets, nil
}
