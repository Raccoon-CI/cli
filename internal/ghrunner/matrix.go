package ghrunner

import (
	"fmt"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/sched"
	"github.com/Raccoon-CI/raccoonci/internal/value"
	"github.com/Raccoon-CI/raccoonci/internal/workflow"
)

// A job with a matrix is queued as many jobs as the matrix has combinations, all
// at once, and each wants a runner. How many to start ahead of them: a matrix
// written out in the workflow is counted from the workflow; one that is built at
// run time (`fromJSON(needs.plan.outputs.x)`) is not known until the job that
// builds it has finished, and GitHub's API does not return job outputs, so the
// number of jobs it made on earlier runs stands in for it.

// hasExpression reports whether v holds a ${{ }} anywhere.
func hasExpression(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "${{")
	case []any:
		for _, e := range x {
			if hasExpression(e) {
				return true
			}
		}
	case value.Object:
		for _, f := range x {
			if hasExpression(f.Value) {
				return true
			}
		}
	}
	return false
}

// staticCount is how many jobs d makes when that is written in the workflow.
func staticCount(d *workflow.Job) (int, bool) {
	if d.Strategy == nil || d.Strategy.Matrix == nil {
		return 1, true
	}
	if hasExpression(d.Strategy.Matrix) {
		return 0, false
	}
	combos, err := workflow.ExpandMatrix(d.Strategy.Matrix)
	if err != nil || len(combos) == 0 {
		return 0, false
	}
	return len(combos), true
}

// countKey names the remembered number of jobs of d in a repository. The
// history keeps durations, so a count is kept as that many seconds.
func countKey(repo string, d *workflow.Job) string {
	return sched.Key(repo, "instances\x00"+displayName(d))
}

// instancesOf is how many runners to start ahead for d: its jobs, as many as can
// be queued together (`max-parallel`) and as many machines as the service runs
// at once.
func (s *Service) instancesOf(repo string, d *workflow.Job) int {
	n, exact := staticCount(d)
	if !exact {
		// The largest of the last few counts, not their median: a runner too many
		// waits about 15 s (a fraction of a cent) and a runner too few makes a job
		// wait 7 s for a cold start, which at the default worth of a second is
		// hundreds of times more. On eight real runs of a matrix of 2 to 5 jobs
		// the median left 8 jobs to a cold start and the maximum 3.
		n = 1
		if s.Durations != nil {
			if est, ok := s.Durations.Max(countKey(repo, d)); ok && est >= time.Second {
				n = int(est.Round(time.Second) / time.Second)
			}
		}
	}
	if d.Strategy != nil {
		if f, ok := d.Strategy.MaxParallel.(float64); ok && f >= 1 {
			n = min(n, int(f))
		}
	}
	return max(1, min(n, s.maxJobs()))
}

func (s *Service) maxJobs() int {
	if s.MaxJobs > 0 {
		return s.MaxJobs
	}
	return 8
}

// recordCount remembers how many jobs d made in a run, the first time its jobs
// are seen (GitHub creates a matrix's jobs together). Only a matrix built at run
// time needs it.
func (s *Service) recordCount(repo string, runID int64, d *workflow.Job, jobs []Job) {
	if s.Durations == nil {
		return
	}
	if _, exact := staticCount(d); exact {
		return
	}
	n := 0
	for _, j := range jobs {
		if matches(j.Name, d) {
			n++
		}
	}
	if n == 0 {
		return
	}
	key := fmt.Sprintf("%s|%d|%s", repo, runID, displayName(d))
	s.mu.Lock()
	done := s.countsRecorded[key]
	if s.countsRecorded == nil {
		s.countsRecorded = map[string]bool{}
	}
	s.countsRecorded[key] = true
	s.mu.Unlock()
	if !done {
		s.Durations.Record(countKey(repo, d), time.Duration(n)*time.Second)
	}
}
