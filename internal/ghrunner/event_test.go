package ghrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A GitHub that knows a repository's workflows and what a commit changed.
type eventGitHub struct {
	*fakeGitHub
	files     []string
	workflows map[string]string // path -> source
	mu2       sync.Mutex
	compared  []string
}

func (f *eventGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/repos/o/r/actions/workflows":
		var list []map[string]any
		for path := range f.workflows {
			list = append(list, map[string]any{"id": 1, "path": path, "state": "active"})
		}
		json.NewEncoder(w).Encode(map[string]any{"workflows": list})
	case strings.HasPrefix(p, "/repos/o/r/contents/"):
		src, ok := f.workflows[strings.TrimPrefix(p, "/repos/o/r/contents/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(src))})
	case strings.HasPrefix(p, "/repos/o/r/compare/"):
		f.mu2.Lock()
		f.compared = append(f.compared, strings.TrimPrefix(p, "/repos/o/r/compare/"))
		f.mu2.Unlock()
		var files []map[string]string
		for _, n := range f.files {
			files = append(files, map[string]string{"filename": n})
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files})
	default:
		f.fakeGitHub.ServeHTTP(w, r)
	}
}

const (
	mainSHA   = "1111111111111111111111111111111111111111"
	beforeSHA = "2222222222222222222222222222222222222222"
)

// runEvent delivers an event to a service watching o/r and says how many
// machines were started within the time given.
func runEvent(t *testing.T, workflows map[string]string, files []string, deliver func(*Service) bool, wait time.Duration) (machines int32, ib *fakeInstaBox, gh *eventGitHub) {
	t.Helper()
	gh = &eventGitHub{fakeGitHub: &fakeGitHub{runners: map[int64]int{}}, files: files, workflows: workflows}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	ib = &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	t.Cleanup(ibSrv.Close)
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) { // the service takes events once it runs
		s.mu.Lock()
		up := s.runCtx != nil
		s.mu.Unlock()
		if up {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !deliver(s) {
		t.Error("the event was not taken")
	}
	time.Sleep(wait)
	cancel()
	<-done
	return ib.creates.Load(), ib, gh
}

func pushEvent(ref, before, after string) PushEvent {
	var ev PushEvent
	ev.Ref, ev.Before, ev.After = ref, before, after
	ev.Repository.FullName = "o/r"
	return ev
}

const pushWorkflow = `
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        n: [1, 2, 3]
    steps: [{run: 'true'}]
  after:
    needs: test
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  elsewhere:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
`

// A push to main starts a workflow whose first job has a matrix of three: three
// runners are started at once, none for the job that needs another or for the one
// on another label.
func TestAPushStartsRunnersForTheFirstJobsOfTheWorkflowsItTriggers(t *testing.T) {
	n, ib, gh := runEvent(t, map[string]string{".github/workflows/ci.yml": pushWorkflow}, []string{"src/a.go"},
		func(s *Service) bool { return s.HandlePush(pushEvent("refs/heads/main", beforeSHA, mainSHA)) }, 1200*time.Millisecond)
	if n != 3 {
		t.Errorf("%d machines for the push, want 3 (the matrix of the first job)", n)
	}
	ib.mu.Lock()
	defer ib.mu.Unlock()
	gh.mu2.Lock()
	defer gh.mu2.Unlock()
	if len(gh.compared) != 1 || gh.compared[0] != beforeSHA+"..."+mainSHA {
		t.Errorf("changed files read as %v, want one comparison of the two commits", gh.compared)
	}
}

func TestAPushThatTriggersNothingStartsNothing(t *testing.T) {
	const pathsOnly = `
on:
  push:
    paths: ['docs/**']
jobs:
  docs:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
`
	const pullRequestOnly = `
on: pull_request
jobs:
  build:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
`
	const otherLabel = `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
`
	const otherSize = `
on: push
jobs:
  build:
    runs-on: raccoon-8vcpu
    steps: [{run: 'true'}]
`
	for name, c := range map[string]struct {
		wf    string
		ref   string
		after string
		files []string
	}{
		"the changed files are not those of the paths filter": {pathsOnly, "refs/heads/main", mainSHA, []string{"src/a.go"}},
		"the workflow is for pull requests":                   {pullRequestOnly, "refs/heads/main", mainSHA, []string{"a"}},
		"the job is on a label that is not ours":              {otherLabel, "refs/heads/main", mainSHA, []string{"a"}},
		"the job asks for a size the waiting runners are not": {otherSize, "refs/heads/main", mainSHA, []string{"a"}},
		"a branch was deleted":                                {otherLabel, "refs/heads/main", zeroSHA, nil},
	} {
		n, _, _ := runEvent(t, map[string]string{".github/workflows/ci.yml": c.wf}, c.files,
			func(s *Service) bool { return s.HandlePush(pushEvent(c.ref, beforeSHA, c.after)) }, 500*time.Millisecond)
		if n != 0 {
			t.Errorf("%s: %d machines started, want none", name, n)
		}
	}
}

func TestAPushToABranchTheWorkflowDoesNotNameStartsNothing(t *testing.T) {
	n, _, _ := runEvent(t, map[string]string{".github/workflows/ci.yml": pushWorkflow}, []string{"a"},
		func(s *Service) bool { return s.HandlePush(pushEvent("refs/heads/feature", beforeSHA, mainSHA)) }, 500*time.Millisecond)
	if n != 0 {
		t.Errorf("%d machines for a push to a branch the workflow's branches filter leaves out", n)
	}
}

func prEvent(action string) PullRequestEvent {
	var ev PullRequestEvent
	ev.Action, ev.Number = action, 12
	ev.PullRequest.Head.SHA = mainSHA
	ev.PullRequest.Base.Ref, ev.PullRequest.Base.SHA = "main", beforeSHA
	ev.Repository.FullName = "o/r"
	return ev
}

func TestAPullRequestStartsRunnersAndOnlyForTheActivitiesThatTriggerIt(t *testing.T) {
	const wf = `
on: pull_request
jobs:
  build:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
`
	for action, want := range map[string]int32{"opened": 1, "synchronize": 1, "reopened": 1, "labeled": 0, "closed": 0, "edited": 0} {
		n, _, _ := runEvent(t, map[string]string{".github/workflows/ci.yml": wf}, []string{"a"},
			func(s *Service) bool { return s.HandlePullRequest(prEvent(action)) }, 600*time.Millisecond)
		if n != want {
			t.Errorf("pull request %s: %d machines, want %d", action, n, want)
		}
	}
}

// An event for a repository that is not watched is not taken, and it is the
// webhook's way of saying so (a 202).
func TestEventsForRepositoriesNotWatchedAreNotTaken(t *testing.T) {
	var ev PushEvent
	ev.Ref, ev.After = "refs/heads/main", mainSHA
	ev.Repository.FullName = "x/y"
	gh := &eventGitHub{fakeGitHub: &fakeGitHub{runners: map[int64]int{}}, workflows: map[string]string{".github/workflows/ci.yml": pushWorkflow}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	if s.HandlePush(ev) { // not running yet
		t.Error("an event was taken before the service ran")
	}
	hook := httptest.NewServer(&Webhook{Secret: []byte("s"), S: s})
	defer hook.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		up := s.runCtx != nil
		s.mu.Unlock()
		if up {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, _ := json.Marshal(ev)
	if code := deliver(t, hook.URL, "s", "push", string(body)); code != http.StatusAccepted {
		t.Errorf("a push of a repository that is not watched got %d, want 202", code)
	}
	body, _ = json.Marshal(pushEvent("refs/heads/main", beforeSHA, mainSHA))
	if code := deliver(t, hook.URL, "s", "push", string(body)); code != http.StatusNoContent {
		t.Errorf("a push of the watched repository got %d, want 204", code)
	}
}
