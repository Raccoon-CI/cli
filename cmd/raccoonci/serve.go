package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/ghrunner"
	"raccoonci/internal/instabox"
	"raccoonci/internal/sched"
)

// serve implements `raccoonci serve`: GitHub Actions jobs whose runs-on has one
// of our labels are run on a fresh InstaBox machine each, by the GitHub runner
// the machine's template carries. GitHub keeps orchestrating the workflow.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var repos listFlag
	fs.Var(&repos, "repo", "a repository to watch, owner/name (repeatable)")
	org := fs.String("org", "", "register the runners with this organization instead of with a repository each, so one pool of waiting runners serves all the repositories watched (all of the organization's when no -repo is given); needs a token that may manage the organization's runners")
	prefix := fs.String("label-prefix", "raccoon-", "serve the jobs whose runs-on has a label starting with this")
	poll := fs.Duration("poll", time.Second, "how often each repository is asked for waiting jobs")
	template := fs.String("template", "ci", "the InstaBox template machines are made from: Ubuntu with GitHub's Actions runner")
	warm := fs.Int("warm", 0, "keep this many machines paused and ready: a job then starts on a machine that resumes in under a second instead of one that is created")
	idle := fs.Int("idle", 0, "always keep at least this many runners waiting per repository (a waiting runner is a running machine, about $1 a day); usually 0, and -second-value decides")
	secondValue := fs.Float64("second-value", 0.01, "what a second of a job waiting to start is worth, in dollars: it sets how long runners are kept waiting after jobs have come (the break-even time is value x 8 s / the machine's cost per second, learned from each repository's own arrivals after 20 bursts); 0 turns the policy off. Scheduled workflows and jobs that need a running job get a runner just before they are due in any case")
	maxJobs := fs.Int("max-jobs", 8, "how many jobs at once")
	usageFile := fs.String("usage-file", "", "write what each machine used to this file, one JSON object per line, when the service stops")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci serve (-repo owner/name [-repo ...] | -org name) [flags]\n\nThe GitHub token is read from GITHUB_TOKEN (it needs the repo scope, and admin rights on the repositories to register runners); the InstaBox key from INSTABOX_API_KEY or IBX_API_KEY.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if len(repos) == 0 && *org == "" {
		fs.Usage()
		os.Exit(2)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("serve needs GITHUB_TOKEN in the environment")
	}
	gh := &ghrunner.Client{Token: token}
	if *org != "" {
		if len(repos) == 0 {
			all, err := gh.OrgRepos(context.Background(), *org)
			if err != nil {
				return fmt.Errorf("listing the repositories of %s: %w", *org, err)
			}
			repos = all
		}
		for _, r := range repos {
			if !strings.HasPrefix(r, *org+"/") {
				return fmt.Errorf("-repo %s is not in the organization %s", r, *org)
			}
		}
	}
	key := cmp.Or(os.Getenv("INSTABOX_API_KEY"), os.Getenv("IBX_API_KEY"))
	if key == "" {
		return errors.New("serve needs INSTABOX_API_KEY (or IBX_API_KEY) in the environment")
	}

	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.0")+" "+f+"\n", a...) }
	p := &instabox.Provider{
		Client:     &instabox.Client{Key: key, URL: os.Getenv("INSTABOX_API_URL")},
		Controller: &dispatch.Controller{}, // unused: there is no agent on these machines
		Template:   *template,
		Logf:       logf,
	}
	if *warm > 0 {
		p.Spares = &instabox.Spares{P: p, Want: *warm}
	}
	var keep *ghrunner.KeepAlive
	if *secondValue > 0 {
		keep = ghrunner.DefaultKeepAlive(*secondValue)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	durations, err := sched.OpenHistory(filepath.Join(cache, "raccoonci", "serve", "durations.json"))
	if err != nil {
		return err
	}
	s := &ghrunner.Service{
		KeepAlive:   keep,
		Durations:   durations,
		GH:          gh,
		P:           p,
		Repos:       repos,
		Org:         *org,
		LabelPrefix: *prefix,
		Idle:        *idle,
		Poll:        *poll,
		MaxJobs:     *maxJobs,
		Logf:        logf,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = s.Run(ctx)
	reportUsage(p.Usage(), *usageFile)
	return err
}
