package ghrunner

import (
	"testing"
	"time"
)

func feed(p *KeepAlive, repo string, start time.Time, gaps []float64) time.Time {
	t := start
	p.Arrival(repo, t)
	for _, g := range gaps {
		t = t.Add(time.Duration(g * float64(time.Second)))
		p.Arrival(repo, t)
	}
	return t
}

func repeat(n int, g float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = g
	}
	return out
}

func TestNothingIsKeptBeforeTheFirstJob(t *testing.T) {
	p := DefaultKeepAlive(0.01)
	if n := p.Target("o/r", time.Now()); n != 0 {
		t.Errorf("target %d with no history", n)
	}
}

// With no data the break-even time decides: v·Δ/c, about 1.9 hours at 0.01 $/s.
func TestBreakEvenTTLUntilThereIsData(t *testing.T) {
	p := DefaultKeepAlive(0.01)
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	p.Arrival("o/r", t0)
	want := 0.01 * 8 / p.C
	if got := p.TTL("o/r").Seconds(); got < want*0.999 || got > want*1.001 {
		t.Errorf("TTL %.0f s, want %.0f", got, want)
	}
	if n := p.Target("o/r", t0.Add(time.Hour)); n != 1 {
		t.Errorf("an hour after a job: %d", n)
	}
	if n := p.Target("o/r", t0.Add(3*time.Hour)); n != 0 {
		t.Errorf("three hours after a job: %d", n)
	}
	// Cheaper waiting makes the break-even shorter.
	if low := DefaultKeepAlive(0.001); low.TTL("x") >= p.TTL("o/r") {
		t.Error("a lower value of waiting did not shorten the TTL")
	}
}

func TestJobsWithinHalfAMinuteAreOneBurst(t *testing.T) {
	p := DefaultKeepAlive(0.01)
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for i := range 6 {
		p.Arrival("o/r", t0.Add(time.Duration(i)*4*time.Second))
	}
	if g := len(p.repos["o/r"].gaps); g != 0 {
		t.Errorf("%d gaps from one burst", g)
	}
	if k := p.repos["o/r"].k; k != 6 {
		t.Errorf("burst of %d", k)
	}
	// The next burst teaches the typical size; the pool follows it, up to the cap.
	for r := range 5 {
		p.Arrival("o/r", t0.Add(time.Duration(r+1)*20*time.Minute))
		for i := 1; i < 6; i++ {
			p.Arrival("o/r", t0.Add(time.Duration(r+1)*20*time.Minute+time.Duration(i)*time.Second))
		}
	}
	now := t0.Add(5*20*time.Minute + time.Minute)
	if n := p.Target("o/r", now); n != 4 {
		t.Errorf("pool %d for bursts of 6, want the cap of 4", n)
	}
}

// What is learned: a busy repository keeps runners long enough to cover its
// gaps, a quiet one with the odd quick follow-up keeps them a few minutes, and one
// whose jobs never follow each other keeps none.
func TestLearnedTTLFollowsTheRepository(t *testing.T) {
	p := DefaultKeepAlive(0.01)
	t0 := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	feed(p, "busy", t0, repeat(40, 600))
	if ttl := p.TTL("busy").Seconds(); ttl < 600 {
		t.Errorf("busy repository: TTL %.0f s does not cover its 600 s gaps", ttl)
	}

	var quiet []float64
	for i := 0; i < 40; i++ {
		if i%4 == 0 {
			quiet = append(quiet, 200) // a follow-up commit
		} else {
			quiet = append(quiet, 86400)
		}
	}
	feed(p, "quiet", t0, quiet)
	ttl := p.TTL("quiet").Seconds()
	if ttl < 240 || ttl > 900 {
		t.Errorf("quiet repository with follow-ups: TTL %.0f s, want a few minutes", ttl)
	}

	feed(p, "never", t0, repeat(40, 86400))
	if ttl := p.TTL("never").Seconds(); ttl != 0 {
		t.Errorf("a repository whose jobs never follow each other: TTL %.0f s, want none", ttl)
	}
	if n := p.Target("never", t0.Add(40*24*time.Hour+time.Second)); n != 0 {
		t.Errorf("target %d, want none", n)
	}
}
