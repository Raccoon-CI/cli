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
	"time"

	"raccoonci/internal/dispatch"
)

// agentConfig is the file a controller's provider puts on the machine.
type agentConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// runAgent implements `raccoonci agent`: wait for the config file, then run
// the one job it names and exit. The token is read from a file, never from the
// command line, where other users of the machine could see it.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	config := fs.String("config", "/data/home/.raccoonci-agent.json", "the file holding the controller `url` and `token` (JSON)")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the file to appear")
	dir := fs.String("dir", "", "where the source is unpacked (default: a new temporary directory)")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

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
			return agentConfig{}, fmt.Errorf("%s did not appear within %v", filepath.Clean(path), wait)
		}
		select {
		case <-ctx.Done():
			return agentConfig{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
