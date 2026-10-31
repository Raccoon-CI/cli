package ghrunner

import (
	"math"
	"slices"
	"sync"
	"time"
)

// KeepAlive decides, per repository, how many runners to keep waiting.
//
// Model (docs/research/idle-runners): a waiting runner costs C dollars a second
// and starts a job in LHot seconds, a job that finds none starts in LCold; a
// second of waiting is worth V dollars. Keeping a runner for TTL seconds after a
// burst of jobs is worth it while the chance that the next burst arrives in the
// next second, given that it has not yet, stays above C/((LCold−LHot)·V).
// Without knowing the repository that gives the break-even TTL V·Δ/C. After
// enough bursts the TTL is chosen to minimise the cost over the last
// ones: busy repositories get a long TTL, quiet ones a few minutes (long enough
// to catch the follow-up commit) or none.
type KeepAlive struct {
	// C is what a waiting machine costs, dollars per second.
	C float64
	// V is what a second of waiting is worth, dollars.
	V float64
	// R is how long a replacement runner takes to be ready; LHot and LCold are
	// the start-up latencies with and without a waiting runner (seconds).
	R, LHot, LCold float64
	// MaxPool caps the runners kept per repository. Default 4.
	MaxPool int
	// Learn is how many bursts must have been seen before the TTL is learned
	// from them. Default 20.
	Learn int

	mu    sync.Mutex
	repos map[string]*arrivals
}

// DefaultKeepAlive returns a policy with the measured numbers of the InstaBox
// `ci` template (4 vCPU, 8 GiB, 70 GiB of disk at the unit prices of 2026-10-29).
func DefaultKeepAlive(v float64) *KeepAlive {
	return &KeepAlive{C: 4*1.111e-6 + 8*6.25e-7 + 70*3.1e-8, V: v, R: 8, LHot: 1, LCold: 9}
}

// arrivals is what is known of one repository's jobs.
type arrivals struct {
	last   time.Time // start of the latest burst
	k      int       // jobs in it so far
	gaps   []float64 // seconds between burst starts, newest last
	ewmaK  float64   // typical burst size
	bursts int
}

const (
	burstWindow = 30 * time.Second
	historyKeep = 100
)

// Arrival records a job that asked for a runner at t.
func (p *KeepAlive) Arrival(repo string, t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repos == nil {
		p.repos = map[string]*arrivals{}
	}
	a := p.repos[repo]
	if a == nil {
		a = &arrivals{}
		p.repos[repo] = a
	}
	if !a.last.IsZero() && t.Sub(a.last) <= burstWindow {
		a.k++
		return
	}
	if !a.last.IsZero() {
		a.gaps = append(a.gaps, t.Sub(a.last).Seconds())
		if len(a.gaps) > historyKeep {
			a.gaps = a.gaps[len(a.gaps)-historyKeep:]
		}
		if a.ewmaK == 0 {
			a.ewmaK = float64(a.k)
		} else {
			a.ewmaK = 0.7*a.ewmaK + 0.3*float64(a.k)
		}
	}
	a.last, a.k = t, 1
	a.bursts++
}

// TTL is how long runners are kept after the latest burst of repo.
func (p *KeepAlive) TTL(repo string) time.Duration {
	p.mu.Lock()
	a := p.repos[repo]
	var gaps []float64
	if a != nil {
		gaps = slices.Clone(a.gaps)
	}
	p.mu.Unlock()
	return time.Duration(p.ttl(gaps) * float64(time.Second))
}

func (p *KeepAlive) learn() int {
	if p.Learn > 0 {
		return p.Learn
	}
	return 20
}

// grid of TTLs to choose from, seconds.
var ttlGrid = []float64{30, 60, 120, 240, 480, 900, 1800, 3600, 2 * 3600, 4 * 3600, 8 * 3600, 16 * 3600, 26 * 3600}

func (p *KeepAlive) breakEven() float64 { return p.V * (p.LCold - p.LHot) / p.C }

// ttl chooses the TTL: break-even until there is data, then the best over the
// recent gaps (0 meaning keep none).
func (p *KeepAlive) ttl(gaps []float64) float64 {
	if len(gaps) < p.learn() {
		return p.breakEven()
	}
	best, arg := p.cost(gaps, 0), 0.0
	for _, T := range ttlGrid {
		if c := p.cost(gaps, T); c < best {
			best, arg = c, T
		}
	}
	return arg
}

// cost is the mean dollars per burst (waiting machines plus valued latency)
// over gaps if runners are kept T seconds after each burst; T = 0 keeps none.
func (p *KeepAlive) cost(gaps []float64, T float64) float64 {
	var tot float64
	for _, g := range gaps {
		switch {
		case T == 0 || g > T:
			if T > 0 {
				tot += p.C * (T + p.R)
			}
			tot += p.V*p.LCold + p.C*p.LCold
		default:
			tot += p.C*(g+p.R) + p.V*p.LHot
		}
	}
	return tot / float64(len(gaps))
}

// Target is how many runners repo should have waiting at now.
func (p *KeepAlive) Target(repo string, now time.Time) int {
	p.mu.Lock()
	a := p.repos[repo]
	if a == nil || a.last.IsZero() {
		p.mu.Unlock()
		return 0
	}
	last, k, ewma := a.last, a.k, a.ewmaK
	gaps := slices.Clone(a.gaps)
	p.mu.Unlock()
	if now.Sub(last).Seconds() > p.ttl(gaps) {
		return 0
	}
	n := ewma
	if n == 0 || float64(k) > n {
		n = float64(k)
	}
	max := p.MaxPool
	if max <= 0 {
		max = 4
	}
	return int(math.Max(1, math.Min(float64(max), math.Round(n))))
}
