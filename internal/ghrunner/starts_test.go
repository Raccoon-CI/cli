package ghrunner

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStartsAreCountedAndTheLastOnesAreKeptForJobs(t *testing.T) {
	s := &Service{StartsPerMinute: 10}
	for i := range 7 { // runners started ahead may use 7 of the 10
		if ok, _ := s.reserveStart(true); !ok {
			t.Fatalf("speculative start %d refused", i+1)
		}
	}
	if ok, wait := s.reserveStart(true); ok || wait <= 0 {
		t.Errorf("an eighth speculative start was allowed (%v, %v): the last three are for jobs", ok, wait)
	}
	for i := range 3 { // jobs have the rest
		if ok, _ := s.reserveStart(false); !ok {
			t.Fatalf("job start %d refused", i+1)
		}
	}
	ok, wait := s.reserveStart(false)
	if ok {
		t.Fatal("an eleventh start in the minute was allowed")
	}
	if wait < 50*time.Second || wait > 63*time.Second {
		t.Errorf("told to wait %v for a start to free up, want about a minute", wait)
	}
	// the window moves
	s.startMu.Lock()
	for i := range s.starts {
		s.starts[i] = s.starts[i].Add(-63 * time.Second)
	}
	s.startMu.Unlock()
	if ok, _ := s.reserveStart(false); !ok {
		t.Error("no start after the minute had passed")
	}
	// no limit
	if ok, _ := (&Service{}).reserveStart(true); !ok {
		t.Error("a start was refused with no limit set")
	}
}

func TestAStartRefusedByInstaBoxHoldsStartsAndOnlyHoldsThem(t *testing.T) {
	s := &Service{StartsPerMinute: 10}
	s.startMu.Lock()
	s.holdUntil = time.Now().Add(time.Hour)
	s.startMu.Unlock()
	if ok, wait := s.reserveStart(false); ok || wait < 59*time.Minute {
		t.Errorf("a start during a hold: %v %v", ok, wait)
	}
}

// A runner takes the oldest job waiting for its labels, not the one it was
// started for. When the runner started for a job has gone and the job is still
// queued it has no runner, and is given another: before, it was left, for ever.
func TestAJobStillQueuedAfterItsRunnerTookAnotherGetsAnotherRunner(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, stillQueued: true, jobs: []Job{
		{ID: 41, RunID: 7, Name: "plan", Status: "queued", Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(20 * time.Second)
	for ib.creates.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n := ib.creates.Load(); n < 2 {
		t.Errorf("%d machines for a job that was still queued after its runner left, want a second one", n)
	}
}

// InstaBox refuses three starts in a row (its limit): the job is not given up on
// after three tries, as one that failed for another reason is, but tried again
// when the hold is over, and gets its machine.
func TestAJobRefusedByTheStartLimitIsTriedAgainNotGivenUp(t *testing.T) {
	old := startHold
	startHold = 100 * time.Millisecond
	defer func() { startHold = old }()
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 41, RunID: 7, Name: "build", Status: "queued", Labels: []string{"raccoon-4vcpu"}, CreatedAt: time.Now()},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ib.refuse.Store(4) // more than the three attempts a job has
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for ib.deletes.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if ib.refused.Load() < 4 {
		t.Fatalf("only %d starts were refused: the test did not set up the limit", ib.refused.Load())
	}
	if ib.deletes.Load() < 1 || ib.creates.Load() < 1 {
		t.Errorf("the job never got a machine after %d refusals (%d created, %d deleted)", ib.refused.Load(), ib.creates.Load(), ib.deletes.Load())
	}
}
