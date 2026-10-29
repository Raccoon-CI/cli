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
	"runtime"
	"sync"
	"time"

	"raccoonci/internal/dispatch"
)

// agentConfig is the file a controller's provider puts on the machine.
type agentConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// runAgent implements `raccoonci agent`: wait for the config file, then run
// the one job it names and exit. With -loop it keeps going: after a job the
// config file is gone and the agent waits for the next one, in -slots places at
// once (slot i reads the file dispatch.SlotConfig names), so a machine can run
// several short jobs one after another and side by side. The token is read from
// a file, never from the command line, where other users of the machine could
// see it.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	config := fs.String("config", "/data/home/.raccoonci-agent.json", "the file holding the controller `url` and `token` (JSON)")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the file to appear")
	dir := fs.String("dir", "", "where the source is unpacked (default: a new temporary directory)")
	loop := fs.Bool("loop", false, "keep taking jobs: wait for a new config file after each one, until stopped")
	slots := fs.Int("slots", 0, "with -loop: how many jobs at once (default: the number of CPUs, at most 8)")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *loop {
		return agentLoop(ctx, *config, *slots, *wait)
	}

	cfg, err := waitForConfig(ctx, *config, *wait)
	if err != nil {
		return err
	}
	if *dir == "" {
		if *dir, err = os.MkdirTemp("", "raccoonci-agent-"); err != nil {
			return err
		}
		defer os.RemoveAll(*dir)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return (&dispatch.Agent{URL: cfg.URL, Token: cfg.Token, Dir: *dir, Exe: exe}).Run(ctx)
}

// maxSlots caps how many jobs a looping agent runs at once.
const maxSlots = 8

// agentLoop runs one goroutine per slot; each takes a job at a time from its
// own config file. It returns when ctx ends.
func agentLoop(ctx context.Context, config string, slots int, wait time.Duration) error {
	if slots <= 0 {
		slots = min(runtime.NumCPU(), maxSlots)
	}
	slots = min(slots, maxSlots)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := agentOnce(ctx, path, exe, wait); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "raccoonci agent: %v\n", err)
					select {
					case <-ctx.Done():
					case <-time.After(2 * time.Second):
					}
				}
			}
		}(dispatch.SlotConfig(config, i))
	}
	wg.Wait()
	return nil
}

// agentOnce waits for the config at path, deletes it (so a token is never
// read twice, and is not left on a disk that gets a snapshot), and runs the
// job in a directory of its own.
func agentOnce(ctx context.Context, path, exe string, wait time.Duration) error {
	cfg, err := waitForConfig(ctx, path, wait)
	if err != nil {
		if errors.Is(err, errConfigTimeout) {
			return nil // nothing came; wait again
		}
		os.Remove(path) // unreadable or incomplete: do not read it again
		return err
	}
	os.Remove(path)
	dir, err := os.MkdirTemp("", "raccoonci-agent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	return (&dispatch.Agent{URL: cfg.URL, Token: cfg.Token, Dir: dir, Exe: exe}).Run(ctx)
}

var errConfigTimeout = errors.New("no config appeared")

// waitForConfig polls for the file: the machine may boot before the
// controller has uploaded it.
func waitForConfig(ctx context.Context, path string, wait time.Duration) (agentConfig, error) {
	deadline := time.Now().Add(wait)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			var cfg agentConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				// A half-written file is possible; the upload is not atomic from here.
				if time.Now().After(deadline) {
					return cfg, fmt.Errorf("%s: %w", path, err)
				}
			} else if cfg.URL == "" || cfg.Token == "" {
				return cfg, fmt.Errorf("%s: needs url and token", path)
			} else {
				return cfg, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, io.EOF) {
			return agentConfig{}, err
		}
		if time.Now().After(deadline) {
			return agentConfig{}, fmt.Errorf("%s did not appear within %v: %w", filepath.Clean(path), wait, errConfigTimeout)
		}
		select {
		case <-ctx.Done():
			return agentConfig{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
