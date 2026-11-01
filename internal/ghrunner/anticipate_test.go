package ghrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/instabox"
	"raccoonci/internal/sched"
)

// A GitHub with one run in progress: its job "build" (ours) is running and the job
// "deploy" (ours, needs build) is not queued yet. Runners that are registered stay
// online and idle until they are deleted.
type runInProgress struct {
	mu       sync.Mutex
	started  time.Time
	runners  map[int64]bool
	nextID   int64
	deleted  []int64
	workflow string
	steps    []Step // the steps the running job reports
}

func (f *runInProgress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/repos/o/r/actions/runs":
		runs := []Run{}
		if r.URL.Query().Get("status") == "in_progress" {
			runs = append(runs, Run{ID: 9, Status: "in_progress", Event: "push"})
		}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	case p == "/repos/o/r/actions/runs/9":
		fmt.Fprint(w, `{"id":9,"status":"in_progress","event":"push","path":".github/workflows/ci.yml@refs/heads/main","head_sha":"abc"}`)
	case p == "/repos/o/r/contents/.github/workflows/ci.yml":
		json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(f.workflow))})
	case p == "/repos/o/r/actions/runs/9/jobs":
		json.NewEncoder(w).Encode(map[string]any{"jobs": []Job{
			{ID: 31, RunID: 9, Name: "build", Status: "in_progress", Labels: []string{"raccoon-4vcpu"}, CreatedAt: f.started, StartedAt: f.started, Steps: f.steps},
		}})
	case p == "/repos/o/r/actions/runners/generate-jitconfig":
		f.nextID++
		f.runners[f.nextID] = true
		fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"X"}`, f.nextID)
	case strings.HasPrefix(p, "/repos/o/r/actions/runners/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(p, "/repos/o/r/actions/runners/"), "%d", &id)
		if r.Method == "DELETE" {
			delete(f.runners, id)
			f.deleted = append(f.deleted, id)
			w.WriteHeader(204)
			return
		}
		if !f.runners[id] {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"status":"online","busy":false}`, id)
	default:
		w.WriteHeader(404)
	}
}

func newService(t *testing.T, ghURL string, ib *fakeInstaBox, ibURL string) *Service {
	t.Helper()
	hist, err := sched.OpenHistory(filepath.Join(t.TempDir(), "h.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := &instabox.Provider{
		Client:     &instabox.Client{URL: ibURL, Key: "k", Poll: time.Millisecond},
		Controller: &dispatch.Controller{},
		Template:   "ci",
	}
	return &Service{GH: &Client{Base: ghURL, Token: "t"}, P: p, Repos: []string{"o/r"}, Poll: 10 * time.Millisecond, Durations: hist}
}

// The job "deploy" needs "build". "build" has taken about 2 s before and has been
// running for a second, so a runner is started for "deploy" before it is queued.
func TestRunnerIsStartedForTheJobsThatNeedARunningOne(t *testing.T) {
	gh := &runInProgress{runners: map[int64]bool{}, started: time.Now().Add(-time.Second), workflow: `
on: push
jobs:
  build:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  deploy:
    needs: build
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  elsewhere:
    needs: build
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
`}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.Durations.Record(sched.Key("o/r", "build"), 2*time.Second)
	var mu sync.Mutex
	var log []string
	s.Logf = func(f string, a ...any) { mu.Lock(); log = append(log, fmt.Sprintf(f, a...)); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for ib.creates.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if n := ib.creates.Load(); n != 1 {
		t.Errorf("%d machines created, want 1 (for deploy only, not for the job on another label)", n)
	}
	mu.Lock()
	text := strings.Join(log, "\n")
	mu.Unlock()
	if !strings.Contains(text, "ahead of time (needs of deploy") {
		t.Errorf("no word of the anticipated job:\n%s", text)
	}
}

const chainWorkflow = `
on: push
jobs:
  build:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  deploy:
    needs: build
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
`

// recordBuildSteps remembers a build job of three steps: 1 s, `work` and 1 s.
func recordBuildSteps(s *Service, work time.Duration) {
	for i, c := range []struct {
		name string
		d    time.Duration
	}{{"Set up job", time.Second}, {"work", work}, {"Complete job", time.Second}} {
		s.Durations.Record(stepKey("o/r", "build", Step{Number: i + 1, Name: c.name}), c.d)
	}
}

func runWithSteps(t *testing.T, steps []Step, started time.Time, jobMedian time.Duration, work time.Duration, wait time.Duration) (machines int32, log string) {
	t.Helper()
	return runChain(t, chainWorkflow, steps, started, jobMedian, work, wait)
}

func runChain(t *testing.T, wf string, steps []Step, started time.Time, jobMedian time.Duration, work time.Duration, wait time.Duration) (machines int32, log string) {
	t.Helper()
	gh := &runInProgress{runners: map[int64]bool{}, started: started, workflow: wf, steps: steps}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.Durations.Record(sched.Key("o/r", "build"), jobMedian)
	recordBuildSteps(s, work)
	var mu sync.Mutex
	var lines []string
	s.Logf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	s.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	return ib.creates.Load(), strings.Join(lines, "\n")
}

// The job that is running has done everything but its last step, which takes a
// second: the runner for the job that needs it is started at once, though the
// whole job is usually far longer than it has run.
func TestRunnerStartsWhenTheUpstreamJobIsInItsLastStep(t *testing.T) {
	now := time.Now()
	steps := []Step{
		{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: now.Add(-3 * time.Second), CompletedAt: now.Add(-2 * time.Second)},
		{Number: 2, Name: "work", Status: "completed", Conclusion: "success", StartedAt: now.Add(-2 * time.Second), CompletedAt: now.Add(-time.Second)},
		{Number: 3, Name: "Complete job", Status: "queued"},
	}
	n, log := runWithSteps(t, steps, now.Add(-3*time.Second), 100*time.Second, 2*time.Second, 5*time.Second)
	if n != 1 || !strings.Contains(log, "ahead of time (needs of deploy") {
		t.Errorf("%d machines, log:\n%s\nwant one, started because the upstream job is in its last step", n, log)
	}
}

// Two runners are waiting and four jobs come: the first two are left to the
// waiting runners and each of the others gets a runner of its own at once. A
// runner being started for a job is not free for another, so the fourth job is
// not promised to the one started for the third and left to wait for the
// promise to run out (20 s), which is what happened.
func TestEveryJobWithoutAWaitingRunnerGetsOneAtOnce(t *testing.T) {
	var jobs []Job
	for i := range 4 {
		jobs = append(jobs, Job{ID: int64(51 + i), RunID: 7, Name: fmt.Sprintf("test (%d)", i+1), Status: "queued", Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()})
	}
	gh := &fakeGitHub{runners: map[int64]int{}} // the jobs come after the runners are up
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.Idle = 2
	time.AfterFunc(500*time.Millisecond, func() {
		gh.mu.Lock()
		gh.jobs = jobs
		gh.mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	ib.mu.Lock()
	defer ib.mu.Unlock()
	forJobs := 0
	for _, r := range ib.requests {
		if strings.HasPrefix(r.Name, "rc-gh-") && !strings.HasPrefix(r.Name, "rc-gh-idle") {
			forJobs++
		}
	}
	if forJobs != 2 {
		t.Errorf("%d runners started for jobs within 2.5 s, want 2 (the other two jobs are for the waiting runners)", forJobs)
	}
}

// A job with a matrix of three wants three runners: all are started together,
// not one.
func TestRunnersForAMatrixAreStartedTogether(t *testing.T) {
	now := time.Now()
	steps := []Step{
		{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: now.Add(-3 * time.Second), CompletedAt: now.Add(-2 * time.Second)},
		{Number: 2, Name: "work", Status: "completed", Conclusion: "success", StartedAt: now.Add(-2 * time.Second), CompletedAt: now.Add(-time.Second)},
		{Number: 3, Name: "Complete job", Status: "queued"},
	}
	wf := `
on: push
jobs:
  build:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  test:
    needs: build
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        n: [1, 2, 3]
    steps: [{run: 'true'}]
`
	n, log := runChain(t, wf, steps, now.Add(-3*time.Second), 100*time.Second, 2*time.Second, 5*time.Second)
	if n != 3 {
		t.Errorf("%d machines, want 3 for the three jobs of the matrix; log:\n%s", n, log)
	}
}

// The median says the job is nearly done (10 s, run for 9 s) but it is in a
// step that usually takes a minute: no runner is started for what follows yet.
func TestRunnerWaitsWhenTheUpstreamJobIsInALongStep(t *testing.T) {
	now := time.Now()
	steps := []Step{
		{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: now.Add(-9 * time.Second), CompletedAt: now.Add(-8 * time.Second)},
		{Number: 2, Name: "work", Status: "in_progress", StartedAt: now.Add(-8 * time.Second)},
		{Number: 3, Name: "Complete job", Status: "queued"},
	}
	n, log := runWithSteps(t, steps, now.Add(-9*time.Second), 10*time.Second, time.Minute, 900*time.Millisecond)
	if n != 0 {
		t.Errorf("%d machines started too early, log:\n%s", n, log)
	}
}

// Without a known duration nothing is guessed.
func TestNoRunnerIsStartedWithoutAKnownDuration(t *testing.T) {
	gh := &runInProgress{runners: map[int64]bool{}, started: time.Now(), workflow: `
on: push
jobs:
  build: {runs-on: raccoon-4vcpu, steps: [{run: 'true'}]}
  deploy: {needs: build, runs-on: raccoon-4vcpu, steps: [{run: 'true'}]}
`}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if n := ib.creates.Load(); n != 0 {
		t.Errorf("%d machines created with no history", n)
	}
}

// After a job the policy keeps a runner waiting for the TTL, then lets it go.
func TestKeepAliveStartsARunnerAfterAJobAndRetiresItLater(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 41, RunID: 7, Name: "build", Status: "queued", Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	// A break-even TTL of 6 s: long enough for the job's runner to be taken and
	// a waiting one to follow, short enough for the test to see it retired.
	s.KeepAlive = DefaultKeepAlive(0)
	s.KeepAlive.V = s.KeepAlive.C * 6 / 8
	s.Durations = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(20 * time.Second)
	for ib.creates.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ib.creates.Load() < 2 {
		t.Fatalf("no waiting runner followed the job (%d machines)", ib.creates.Load())
	}
	for ib.deletes.Load() < 2 && time.Now().Before(deadline) { // the job's machine, then the retired one
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if ib.deletes.Load() < 2 {
		t.Errorf("the waiting runner was not retired after its TTL (%d deletes)", ib.deletes.Load())
	}
}

// With an organization the runners are registered with it, not with the
// repository, and the job of a repository is still found and served.
func TestRunnersAreRegisteredWithTheOrganization(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 61, RunID: 7, Name: "build", Status: "queued", Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()},
	}}
	var mu sync.Mutex
	var paths []string
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		// the fake knows one place for runners; the organization's are the same ones
		r.URL.Path = strings.Replace(r.URL.Path, "/orgs/o/actions/runners", "/repos/o/r/actions/runners", 1)
		gh.ServeHTTP(w, r)
	}))
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.Org = "o"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for ib.deletes.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if ib.deletes.Load() < 1 {
		t.Fatal("the job was not served")
	}
	mu.Lock()
	text := strings.Join(paths, "\n")
	mu.Unlock()
	if !strings.Contains(text, "POST /orgs/o/actions/runners/generate-jitconfig") {
		t.Errorf("the runner was not registered with the organization:\n%s", text)
	}
	if strings.Contains(text, "/repos/o/r/actions/runners") {
		t.Errorf("a runner was registered with the repository:\n%s", text)
	}
}

// A job queued, a runner started for it, and then another runner (one that was
// waiting) takes the job: the runner started for it is released at once, not
// after the idle timeout.
type takenJob struct {
	mu      sync.Mutex
	listed  int
	deleted []int64
}

func (f *takenJob) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/repos/o/r/actions/runs":
		runs := []Run{}
		if r.URL.Query().Get("status") == "queued" {
			runs = append(runs, Run{ID: 7, Status: "queued"})
		}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	case p == "/repos/o/r/actions/runs/7/jobs":
		f.listed++
		status := "in_progress"
		if f.listed == 1 {
			status = "queued"
		}
		json.NewEncoder(w).Encode(map[string]any{"jobs": []Job{{ID: 51, RunID: 7, Name: "build", Status: status, Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()}}})
	case p == "/repos/o/r/actions/jobs/51":
		fmt.Fprint(w, `{"id":51,"run_id":7,"name":"build","status":"in_progress"}`)
	case p == "/repos/o/r/actions/runners/generate-jitconfig":
		fmt.Fprint(w, `{"runner":{"id":3},"encoded_jit_config":"X"}`)
	case p == "/repos/o/r/actions/runners/3" && r.Method == "DELETE":
		f.deleted = append(f.deleted, 3)
		w.WriteHeader(204)
	case p == "/repos/o/r/actions/runners/3":
		if len(f.deleted) > 0 {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"id":3,"status":"online","busy":false}`)
	default:
		w.WriteHeader(404)
	}
}

func TestRunnerIsReleasedWhenAnotherRunnerTookItsJob(t *testing.T) {
	old := takenCheckAfter
	takenCheckAfter = 50 * time.Millisecond
	defer func() { takenCheckAfter = old }()
	gh := &takenJob{}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.IdleTimeout = time.Hour // only the release can end it in time
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for ib.deletes.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if ib.deletes.Load() < 1 {
		t.Errorf("the runner whose job another runner took was not released")
	}
}
