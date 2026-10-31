package ghrunner

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLabelVCPUs(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   int
		ok     bool
	}{
		{[]string{"raccoon-8vcpu"}, 8, true},
		{[]string{"self-hosted", "Raccoon-2VCPU"}, 2, true},
		{[]string{"raccoon-4vcpu", "linux"}, 4, true},
		{[]string{"raccoon-gpu"}, 0, false},
		{[]string{"raccoon-vcpu"}, 0, false},
		{[]string{"raccoon-0vcpu"}, 0, false},
		{[]string{"ubuntu-latest"}, 0, false},
		{nil, 0, false},
	} {
		got, ok := labelVCPUs("raccoon-", c.labels)
		if got != c.want || ok != c.ok {
			t.Errorf("%v: %d, %v; want %d, %v", c.labels, got, ok, c.want, c.ok)
		}
	}
}

func TestWaitingRunnersServeOnlyTheirOwnLabel(t *testing.T) {
	s := &Service{}
	for labels, want := range map[string]bool{
		"raccoon-4vcpu":             true,
		"self-hosted raccoon-4vcpu": true,
		"raccoon-8vcpu":             false,
		"raccoon-4vcpu gpu":         false,
		"ubuntu-latest":             false,
	} {
		if got := s.waitingCanServe(strings.Fields(labels)); got != want {
			t.Errorf("%q: %v, want %v", labels, got, want)
		}
	}
}

// A job that asks for 8 vCPUs gets a machine of 8 vCPUs with the memory that
// size has, and the machine is not one a waiting runner of the default size
// could have stood in for.
func TestJobGetsTheMachineSizeItsLabelNames(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 81, RunID: 7, Name: "big", Status: "queued", Labels: []string{"raccoon-8vcpu"}, CreatedAt: time.Now()},
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
	deadline := time.Now().Add(15 * time.Second)
	for ib.deletes.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if len(ib.requests) != 1 || ib.requests[0].VCPUs != 8 || ib.requests[0].MemMiB != 16384 {
		t.Errorf("machine requests: %+v, want one of 8 vCPUs and 16384 MiB", ib.requests)
	}
}

// The runners kept waiting are of the size their label names, and take the jobs
// with that label.
func TestWaitingRunnerHasTheSizeOfItsLabel(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	s.RunnerLabel, s.Idle = "raccoon-2vcpu", 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for ib.creates.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if len(ib.requests) < 1 || ib.requests[0].VCPUs != 2 || ib.requests[0].MemMiB != 4096 {
		t.Errorf("machine requests: %+v, want the first of 2 vCPUs and 4096 MiB", ib.requests)
	}
	if !s.waitingCanServe([]string{"raccoon-2vcpu"}) || s.waitingCanServe([]string{"raccoon-4vcpu"}) {
		t.Errorf("a waiting runner of raccoon-2vcpu must take that label and not the default one")
	}
}

// A size the organization cannot create: no machine, and the run is cancelled
// instead of waiting for ever.
func TestJobAskingForAnImpossibleSizeCancelsItsRun(t *testing.T) {
	gh := &fakeGitHub{runners: map[int64]int{}, jobs: []Job{
		{ID: 91, RunID: 7, Name: "huge", Status: "queued", Labels: []string{"raccoon-64vcpu"}, CreatedAt: time.Now()},
	}}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	ib := &fakeInstaBox{machines: map[string]bool{}, files: map[string]string{}}
	ibSrv := httptest.NewServer(ib)
	defer ibSrv.Close()
	s := newService(t, ghSrv.URL, ib, ibSrv.URL)
	var logs []string
	s.Logf = func(f string, a ...any) { logs = append(logs, f) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gh.mu.Lock()
		n := len(gh.cancelled)
		gh.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a second attempt would show
	cancel()
	<-done
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.cancelled) != 1 || gh.cancelled[0] != 7 {
		t.Errorf("cancelled runs %v, want [7] once", gh.cancelled)
	}
	if n := ib.creates.Load(); n != 0 {
		t.Errorf("%d machines created for a size that cannot exist", n)
	}
}
