package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"raccoonci/internal/instabox"
)

var nameUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// runnerSnapshot builds the snapshot `run -on instabox` starts machines from
// (see instabox.Client.BuildRunnerSnapshot) and prints its id.
func runnerSnapshot(args []string) error {
	fs := flag.NewFlagSet("runner-snapshot", flag.ContinueOnError)
	binary := fs.String("binary", "", "the statically linked raccoonci to install (make runner-binary builds dist/raccoonci-linux-amd64); default: this executable")
	name := fs.String("name", "", "the snapshot's name (default raccoonci-runner-<version>)")
	template := fs.String("template", "universal", "the template to start from")
	replace := fs.String("replace", "", "delete this snapshot id once the new one is ready")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci runner-snapshot [flags]\n\nCreates a machine from the template, installs raccoonci and its agent service, saves a snapshot and deletes the machine.\nThe key is read from INSTABOX_API_KEY (or IBX_API_KEY).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := cmp.Or(os.Getenv("INSTABOX_API_KEY"), os.Getenv("IBX_API_KEY"))
	if key == "" {
		return errors.New("runner-snapshot needs INSTABOX_API_KEY (or IBX_API_KEY) in the environment")
	}
	path := *binary
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		path = self
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := exec.Command(path, "-version").Output()
	if err != nil {
		return fmt.Errorf("%s -version: %w", path, err)
	}
	ver := strings.TrimSpace(string(out))
	if ver == "" || ver == "dev" {
		return errors.New("the binary reports no version (build it with VERSION=<x.y.z or commit>): a snapshot has to say what it holds")
	}
	snapName := *name
	if snapName == "" {
		snapName = "raccoonci-runner-" + strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(ver), "-"), "-")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	c := &instabox.Client{Key: key}
	id, err := c.BuildRunnerSnapshot(ctx, instabox.RunnerSpec{Binary: data, Version: ver, Template: *template, Name: snapName},
		func(f string, a ...any) { fmt.Fprintf(os.Stderr, "raccoonci: "+f+"\n", a...) })
	if err != nil {
		return err
	}
	fmt.Println(id)
	if *replace != "" {
		if err := c.DeleteSnapshot(ctx, *replace); err != nil {
			return fmt.Errorf("the new snapshot %s is ready, but deleting %s failed: %w", id, *replace, err)
		}
		fmt.Fprintf(os.Stderr, "raccoonci: deleted the old snapshot %s\n", *replace)
	}
	return nil
}
