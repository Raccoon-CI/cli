package ghrunner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"raccoonci/internal/sched"
	"raccoonci/internal/trigger"
	"raccoonci/internal/workflow"
)

// Some jobs are known to be coming: a scheduled workflow at its cron time, the
// jobs that need a job that is running. A runner started for them just before
// they arrive costs a few seconds of a machine and saves the whole start-up, so
// no keep-alive policy is needed for them.

// expectation is a runner that will be wanted at a known time.
type expectation struct {
	key     string
	repo    string
	at      time.Time // when the job is expected to be queued
	count   int
	why     string
	until   time.Time // keep the runner at least until
	started bool

	// To see how good the predictions are: the job that is waited for, what the
	// job medians alone said (at is from the steps), what both said when the
	// runner was started, and whether the outcome has been reported.
	runID      int64
	job        string
	byJob      time.Time
	atStart    time.Time
	byJobStart time.Time
	reported   bool
	// for an expectation made by a push or pull request: the commit, and when
	// the event reached us.
	sha      string
	received time.Time
}

// labelsOf lists the labels a job's runs-on names, when they are literal.
func labelsOf(runsOn any) []string {
	switch v := runsOn.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (s *Service) isOurs(j *workflow.Job) bool {
	for _, l := range labelsOf(j.RunsOn) {
		if strings.HasPrefix(strings.ToLower(l), s.prefix()) {
			return true
		}
	}
	return false
}

// displayName is what the API calls a job: its name, else its id.
func displayName(j *workflow.Job) string {
	if j.Name != "" && !strings.Contains(j.Name, "${{") {
		return j.Name
	}
	return j.ID
}

// matches reports whether an API job (name "build" or "test (1.22, linux)") is an
// instance of the workflow's job.
func matches(apiName string, j *workflow.Job) bool {
	d := displayName(j)
	return apiName == d || strings.HasPrefix(apiName, d+" (")
}

func (s *Service) addExpectation(e *expectation) {
	e.repo = s.poolOf(e.repo) // the pool whose runner is wanted
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.expected {
		if x.key == e.key {
			if !x.started { // seen again, and the moment may have moved
				x.at, x.until, x.byJob, x.count = e.at, e.until, e.byJob, e.count
			}
			return
		}
	}
	s.expected = append(s.expected, e)
}

// isCommit reports whether ref is a full commit id, which never changes.
func isCommit(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// workflowOfRun reads the workflow file a run runs. For most events that is the
// file at the run's commit. A pull request runs the file of its merge commit,
// and pull_request_target and workflow_run the file of the base or default
// branch, none of which is the run's head commit: the ref after the `@` in the
// run's path says which. If that cannot be read the commit is tried, as before.
func (s *Service) workflowOfRun(ctx context.Context, repo string, info RunInfo) *workflow.Workflow {
	path, ref, _ := strings.Cut(info.Path, "@")
	if ref != "" && (strings.HasPrefix(info.Event, "pull_request") || info.Event == "workflow_run" || strings.HasPrefix(ref, "refs/pull/")) {
		if w := s.workflowFile(ctx, repo, path, ref); w != nil {
			return w
		}
	}
	return s.workflowFile(ctx, repo, path, info.HeadSHA)
}

// workflowFile reads and parses a workflow of repo at ref, remembered.
func (s *Service) workflowFile(ctx context.Context, repo, path, ref string) *workflow.Workflow {
	key := repo + "\x00" + path + "\x00" + ref
	if !isCommit(ref) { // a branch or tag moves: what was read is good for a few minutes
		key += fmt.Sprintf("\x00%d", time.Now().Unix()/300)
	}
	s.mu.Lock()
	if w, ok := s.wfCache[key]; ok {
		s.mu.Unlock()
		return w
	}
	s.mu.Unlock()
	var w *workflow.Workflow
	if b, err := s.GH.FileContent(ctx, repo, path, ref); err == nil {
		w, _ = workflow.Parse(b)
	}
	s.mu.Lock()
	if s.wfCache == nil {
		s.wfCache = map[string]*workflow.Workflow{}
	}
	s.wfCache[key] = w
	s.mu.Unlock()
	return w
}

// refreshSchedules looks at the repository's workflows for cron schedules and
// expects a runner at each next fire time.
func (s *Service) refreshSchedules(ctx context.Context, repo string) {
	wfs, err := s.GH.ListWorkflows(ctx, repo)
	if err != nil {
		return
	}
	for _, f := range wfs {
		if f.State != "active" {
			continue
		}
		w := s.workflowFile(ctx, repo, f.Path, "")
		if w == nil {
			continue
		}
		crons, err := trigger.Schedules(w.On)
		if err != nil || len(crons) == 0 {
			continue
		}
		first := 0 // jobs that start the workflow
		for _, j := range w.Jobs {
			if len(j.Needs) == 0 && s.isOurs(j) && s.waitingCanServe(labelsOf(j.RunsOn)) {
				first++
			}
		}
		if first == 0 {
			continue
		}
		for _, expr := range crons {
			c, err := ParseCron(expr)
			if err != nil {
				continue
			}
			next := c.Next(time.Now())
			if next.IsZero() {
				continue
			}
			s.addExpectation(&expectation{
				key:  repo + "|" + f.Path + "|" + expr + "|" + next.Format(time.RFC3339),
				repo: repo, at: next, count: first, why: "schedule " + expr,
				// GitHub starts scheduled runs late, often by minutes.
				until: next.Add(10 * time.Minute),
			})
		}
	}
}

// observeRun learns from the jobs of a run in progress: how long they take, and
// which of its jobs will be queued when the ones running now finish.
func (s *Service) observeRun(ctx context.Context, repo string, run Run, jobs []Job) {
	if s.Durations != nil {
		for _, j := range jobs {
			if j.Status == "completed" && !j.StartedAt.IsZero() {
				s.mu.Lock()
				done := s.recorded[j.ID]
				if s.recorded == nil {
					s.recorded = map[int64]bool{}
				}
				s.recorded[j.ID] = true
				s.mu.Unlock()
				if !done && j.Conclusion == "success" {
					s.Durations.Record(sched.Key(repo, j.Name), j.CompletedAt.Sub(j.StartedAt))
					s.recordSteps(repo, j)
				}
			}
		}
	}
	if run.Status != "in_progress" || s.Durations == nil {
		return
	}
	info, err := s.GH.GetRun(ctx, repo, run.ID)
	if err != nil || info.Path == "" {
		return
	}
	w := s.workflowOfRun(ctx, repo, info)
	if w == nil {
		return
	}
	for _, d := range w.Jobs {
		if len(d.Needs) == 0 || !s.isOurs(d) || !s.waitingCanServe(labelsOf(d.RunsOn)) {
			continue
		}
		listed := false
		for _, j := range jobs {
			listed = listed || matches(j.Name, d)
		}
		s.recordCount(repo, run.ID, d, jobs)
		if listed {
			continue // already queued or running: the scan has it
		}
		var ready, readyJob time.Time
		known := true
		for _, need := range d.Needs {
			nj := w.Job(need)
			if nj == nil {
				known = false
				break
			}
			seen := false
			for _, j := range jobs {
				if !matches(j.Name, nj) {
					continue
				}
				seen = true
				var at time.Time
				if j.Status == "completed" {
					at = j.CompletedAt
					if at.After(readyJob) {
						readyJob = at
					}
				} else {
					est, ok := s.estimatedTotal(repo, j, time.Now())
					if !ok {
						known = false
						break
					}
					start := j.StartedAt
					if start.IsZero() {
						start = time.Now()
					}
					at = start.Add(est)
					if jm, ok := s.Durations.Estimate(sched.Key(repo, j.Name)); ok && start.Add(jm).After(readyJob) {
						readyJob = start.Add(jm)
					}
				}
				if at.After(ready) {
					ready = at
				}
			}
			if !seen || !known {
				known = false // the job it needs has not started: nothing to predict from
				break
			}
		}
		if known && !ready.IsZero() {
			s.track(run.ID, repo, ready)
		}
		if !known || ready.IsZero() {
			continue
		}
		s.addExpectation(&expectation{
			key:  fmt.Sprintf("%s|run|%s|%d", repo, displayName(d), run.ID),
			repo: repo, at: ready, count: s.instancesOf(repo, d), why: "needs of " + displayName(d),
			until: ready.Add(90 * time.Second),
			runID: run.ID, job: displayName(d), byJob: readyJob,
		})
	}
}

// prestart starts the runners that expectations call for, a start-up ahead of
// the time they are expected.
func (s *Service) prestart(ctx context.Context, repo string) {
	lead := time.Duration(s.leadSeconds() * float64(time.Second))
	now := time.Now()
	s.mu.Lock()
	var due []*expectation
	keep := s.expected[:0]
	for _, e := range s.expected {
		if now.After(e.until) {
			continue // too late to matter
		}
		keep = append(keep, e)
		if e.repo == repo && !e.started && now.After(e.at.Add(-lead)) {
			e.started = true
			e.atStart, e.byJobStart = e.at, e.byJob
			due = append(due, e)
		}
	}
	s.expected = keep
	s.mu.Unlock()
	for _, e := range due {
		s.logf("%s: starting %d runner(s) ahead of time (%s, expected %s)", repo, e.count, e.why, e.at.Format("15:04:05"))
		before := s.heldBack.Load()
		for range e.count {
			s.startFor(ctx, repo, nil, 1, now, e.until)
		}
		if held := s.heldBack.Load() - before; held > 0 {
			s.logf("%s: %d of those %d runner(s) not started: the start limit keeps the last starts for jobs that are queued", repo, held, e.count)
		}
	}
}

func (s *Service) leadSeconds() float64 {
	if s.KeepAlive != nil && s.KeepAlive.R > 0 {
		return s.KeepAlive.R
	}
	return 8
}
