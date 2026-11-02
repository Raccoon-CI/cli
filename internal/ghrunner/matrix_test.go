package ghrunner

import (
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/sched"
	"github.com/Raccoon-CI/raccoonci/internal/workflow"
)

func parseJob(t *testing.T, src, id string) *workflow.Job {
	t.Helper()
	w, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	j := w.Job(id)
	if j == nil {
		t.Fatalf("no job %s", id)
	}
	return j
}

func TestInstancesOfAMatrix(t *testing.T) {
	const wf = `
on: push
jobs:
  plan:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  none:
    needs: plan
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  grid:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        os: [a, b]
        n: [1, 2, 3]
    steps: [{run: 'true'}]
  limited:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      max-parallel: 2
      matrix:
        n: [1, 2, 3, 4, 5]
    steps: [{run: 'true'}]
  many:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        n: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]
    steps: [{run: 'true'}]
  shards:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        shard: ${{ fromJSON(needs.plan.outputs.shards) }}
    steps: [{run: 'true'}]
  inside:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.list) }}
    steps: [{run: 'true'}]
`
	hist, err := sched.OpenHistory(t.TempDir() + "/h.json")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Durations: hist}
	for job, want := range map[string]int{
		"none":    1, // no matrix
		"grid":    6, // written out: 2 x 3
		"limited": 2, // five, but two at a time
		"many":    8, // twelve, but the service runs eight machines at most
		"shards":  1, // built at run time, never seen: one
		"inside":  1,
	} {
		if got := s.instancesOf("o/r", parseJob(t, wf, job)); got != want {
			t.Errorf("%s: %d runners, want %d", job, got, want)
		}
	}
	// What earlier runs made stands in for what is built at run time.
	shards := parseJob(t, wf, "shards")
	for _, n := range []int{3, 5, 4} {
		hist.Record(countKey("o/r", shards), time.Duration(n)*time.Second)
	}
	if got := s.instancesOf("o/r", shards); got != 5 {
		t.Errorf("shards after runs of 3, 5 and 4 jobs: %d runners, want the largest, 5", got)
	}
	s.MaxJobs = 2
	if got := s.instancesOf("o/r", shards); got != 2 {
		t.Errorf("shards with room for two machines: %d runners, want 2", got)
	}
}

// The number of jobs a matrix made is remembered once per run, and only for a
// matrix that is built at run time.
func TestCountsOfDynamicMatricesAreRemembered(t *testing.T) {
	const wf = `
on: push
jobs:
  plan:
    runs-on: raccoon-4vcpu
    steps: [{run: 'true'}]
  shards:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        shard: ${{ fromJSON(needs.plan.outputs.shards) }}
    steps: [{run: 'true'}]
  fixed:
    needs: plan
    runs-on: raccoon-4vcpu
    strategy:
      matrix:
        n: [1, 2]
    steps: [{run: 'true'}]
`
	hist, err := sched.OpenHistory(t.TempDir() + "/h.json")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Durations: hist}
	shards, fixed := parseJob(t, wf, "shards"), parseJob(t, wf, "fixed")
	jobs := []Job{{Name: "plan"}, {Name: "shards (1)"}, {Name: "shards (2)"}, {Name: "shards (3)"}, {Name: "fixed (1)"}, {Name: "fixed (2)"}}
	for range 3 { // seen again and again while the jobs run
		s.recordCount("o/r", 77, shards, jobs)
		s.recordCount("o/r", 77, fixed, jobs)
	}
	if est, ok := hist.Estimate(countKey("o/r", shards)); !ok || est != 3*time.Second {
		t.Errorf("remembered count of shards: %v %v, want 3", est, ok)
	}
	if _, ok := hist.Estimate(countKey("o/r", fixed)); ok {
		t.Errorf("the count of a matrix written out was remembered; the workflow says it")
	}
	s.recordCount("o/r", 78, shards, jobs[:3]) // another run, with two shards
	if est, _ := hist.Estimate(countKey("o/r", shards)); est < 2*time.Second || est > 3*time.Second {
		t.Errorf("after a second run: %v", est)
	}
}
