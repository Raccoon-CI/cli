package ghrunner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Raccoon-CI/lib/pkg/trigger"
)

// A push or a pull request is about to start workflows, but GitHub takes a
// moment to make the run and queue its jobs. The event itself arrives at once,
// so a runner for the first jobs of the workflows it will start can be started
// while GitHub is still at it: the workflows are read at the commit, matched
// against the event as GitHub does (`trigger.Match`: branches, tags, paths,
// activity types), and the jobs that need nothing else and carry our label are
// counted (a matrix as many as it has jobs).

// PushEvent is the part of a push delivery the service uses.
type PushEvent struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// PullRequestEvent is the part of a pull_request delivery the service uses.
type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

const zeroSHA = "0000000000000000000000000000000000000000"

// watching looks a repository up by name as GitHub spells it, and gives the
// name we were given, the loop that watches it and the context to work under.
func (s *Service) watching(repo string) (string, chan struct{}, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx == nil {
		return "", nil, nil, false
	}
	for r, ch := range s.wake {
		if strings.EqualFold(r, repo) {
			return r, ch, s.runCtx, true
		}
	}
	return "", nil, nil, false
}

// poke makes the loop of a repository look at its expectations now.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// HandlePush takes a push event. It reports false when the repository is not
// watched or the service is not running.
func (s *Service) HandlePush(ev PushEvent) bool {
	repo, wake, ctx, ok := s.watching(ev.Repository.FullName)
	if !ok {
		return false
	}
	if ev.Deleted || ev.After == "" || ev.After == zeroSHA {
		return true
	}
	received := time.Now()
	s.wg.Go(func() {
		var files []string
		if ev.Before != "" && ev.Before != zeroSHA { // a new branch has nothing to compare with
			files, _ = s.GH.ChangedFiles(ctx, repo, ev.Before, ev.After)
		}
		s.anticipate(ctx, repo, wake, trigger.Event{Name: "push", Ref: ev.Ref, Files: files}, ev.After,
			"push to "+strings.TrimPrefix(ev.Ref, "refs/heads/"), received)
	})
	return true
}

// HandlePullRequest takes a pull_request event.
func (s *Service) HandlePullRequest(ev PullRequestEvent) bool {
	repo, wake, ctx, ok := s.watching(ev.Repository.FullName)
	if !ok {
		return false
	}
	sha := ev.PullRequest.Head.SHA
	if sha == "" {
		return true
	}
	received := time.Now()
	s.wg.Go(func() {
		var files []string
		if base := ev.PullRequest.Base.SHA; base != "" {
			files, _ = s.GH.ChangedFiles(ctx, repo, base, sha)
		}
		s.anticipate(ctx, repo, wake, trigger.Event{Name: "pull_request", Action: ev.Action, BaseRef: "refs/heads/" + ev.PullRequest.Base.Ref, Files: files}, sha,
			fmt.Sprintf("pull request #%d %s", ev.Number, ev.Action), received)
	})
	return true
}

// anticipate starts the runners for the first jobs of the workflows the event
// triggers, read at commit sha.
func (s *Service) anticipate(ctx context.Context, repo string, wake chan struct{}, ev trigger.Event, sha, why string, received time.Time) {
	wfs, err := s.GH.ListWorkflows(ctx, repo)
	if err != nil {
		return
	}
	total := 0
	for _, f := range wfs {
		if f.State != "active" {
			continue
		}
		w := s.workflowFile(ctx, repo, f.Path, sha)
		if w == nil {
			continue
		}
		if res, err := trigger.Match(w.On, ev); err != nil || !res.Triggered {
			continue
		}
		for _, j := range w.Jobs {
			if len(j.Needs) == 0 && s.isOurs(j) && s.waitingCanServe(labelsOf(j.RunsOn)) {
				total += s.instancesOf(repo, j)
			}
		}
	}
	if total == 0 {
		return
	}
	now := time.Now()
	s.addExpectation(&expectation{
		key:  fmt.Sprintf("%s|event|%s|%s", repo, ev.Name, sha),
		repo: repo, at: now, count: min(total, s.maxJobs()), why: why,
		until: now.Add(time.Minute), sha: sha, received: received,
	})
	poke(wake)
}
