package ghrunner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Raccoon-CI/cli/internal/sched"
)

// How long a job running now has left is better read from its steps than
// guessed from how long jobs of that name usually take: a job that has done
// everything but its last step is about to finish, whatever the median says.
// The jobs API lists the steps of a job with their status, so each step's
// duration is remembered when a job is done, and the time left of a job in
// progress is what its unfinished steps take (the one it is in less what it has
// run). Without a history for every unfinished step the whole job's median is
// used, as before.

func stepKey(repo, job string, st Step) string {
	return sched.Key(repo, fmt.Sprintf("%s\x00%d %s", job, st.Number, st.Name))
}

// recordSteps remembers how long each step of a finished job took.
func (s *Service) recordSteps(repo string, j Job) {
	if s.Durations == nil {
		return
	}
	for _, st := range j.Steps {
		if st.Status == "completed" && st.Conclusion == "success" && !st.StartedAt.IsZero() && !st.CompletedAt.IsZero() {
			s.Durations.Record(stepKey(repo, j.Name, st), st.CompletedAt.Sub(st.StartedAt))
		}
	}
}

// remaining is how long j still has to run, by its unfinished steps.
func (s *Service) remaining(repo string, j Job, now time.Time) (time.Duration, bool) {
	if len(j.Steps) == 0 {
		return 0, false
	}
	var total time.Duration
	for _, st := range j.Steps {
		if st.Status == "completed" {
			continue
		}
		est, ok := s.Durations.Estimate(stepKey(repo, j.Name, st))
		if !ok {
			return 0, false
		}
		if st.Status == "in_progress" && !st.StartedAt.IsZero() {
			est = max(est-now.Sub(st.StartedAt), 0)
		}
		total += est
	}
	return total, true
}

// estimatedTotal is how long j takes from its start to its end: what it has run
// plus what is left by its steps, or else the median of such jobs.
func (s *Service) estimatedTotal(repo string, j Job, now time.Time) (time.Duration, bool) {
	if rem, ok := s.remaining(repo, j, now); ok {
		var elapsed time.Duration
		if !j.StartedAt.IsZero() {
			elapsed = now.Sub(j.StartedAt)
		}
		return elapsed + rem, true
	}
	return s.Durations.Estimate(sched.Key(repo, j.Name))
}

// reportPrediction, when a job a runner was started ahead for is first seen
// queued, says how far off the two estimates were when that runner was
// started: the one from the steps (what is used) and the one from the whole
// job's median. The error is the estimate minus the time the job was queued
// (GitHub's, to the second): negative, the runner was started that early and
// waited; positive, it was late.
func (s *Service) reportPrediction(repo string, j Job) {
	s.mu.Lock()
	var hit *expectation
	for _, x := range s.expected {
		if x.sha != "" { // made by a push or a pull request: the first job of its run
			if x.sha == j.HeadSHA && x.started && !x.reported {
				x.reported = true
				why, count, received := x.why, x.count, x.received
				s.mu.Unlock()
				s.logf("%s: the first job of the run for %s (%s) was queued %.0f s after the event reached us; runners had been started ahead (%d)", repo, why, j.Name, j.CreatedAt.Sub(received).Seconds(), count)
				return
			}
			continue
		}
		if x.runID == j.RunID && x.started && !x.reported && (j.Name == x.job || strings.HasPrefix(j.Name, x.job+" (")) {
			x.reported = true
			hit = x
			break
		}
	}
	var steps, median time.Duration
	haveMedian := false
	if hit != nil {
		steps = hit.atStart.Sub(j.CreatedAt)
		if haveMedian = !hit.byJobStart.IsZero(); haveMedian {
			median = hit.byJobStart.Sub(j.CreatedAt)
		}
	}
	s.mu.Unlock()
	if hit != nil {
		byMedian := "no median then"
		if haveMedian {
			byMedian = fmt.Sprintf("%+.0f s", median.Seconds())
		}
		s.logf("%s: %s was queued; when its runner was started the estimate was off by %+.0f s by the steps and %s by the job median (estimate minus what happened: negative, the runner was started that early; positive, that late)", repo, j.Name, steps.Seconds(), byMedian)
	}
}

// A run with a job that is waiting for one still running is looked at every
// second, while the usual listing may come only every half minute (webhooks):
// the steps are what tell the moment to start its runner.
type trackedRun struct {
	repo  string
	until time.Time
}

func (s *Service) track(run int64, repo string, ready time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracked == nil {
		s.tracked = map[int64]trackedRun{}
	}
	s.tracked[run] = trackedRun{repo, ready.Add(time.Minute)}
}

// refreshTracked looks again at the runs being followed in repos.
func (s *Service) refreshTracked(ctx context.Context, repos []string) {
	type item struct {
		id   int64
		repo string
	}
	var list []item
	now := time.Now()
	s.mu.Lock()
	for id, t := range s.tracked {
		if now.After(t.until) {
			delete(s.tracked, id)
		} else if slices.Contains(repos, t.repo) {
			list = append(list, item{id, t.repo})
		}
	}
	s.mu.Unlock()
	for _, it := range list {
		jobs, err := s.GH.RunJobs(ctx, it.repo, it.id)
		if err != nil {
			continue
		}
		s.observeRun(ctx, it.repo, Run{ID: it.id, Status: "in_progress"}, jobs)
	}
}
