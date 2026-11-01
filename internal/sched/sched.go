// Package sched decides which job instances share a machine.
//
// A machine takes seconds to start (InstaBox: 6 to 20 s, longer when many are
// created at once). For an instance that runs only a few seconds that start-up
// is most of the cost, so short instances are packed onto machines that are
// already up or starting, and long ones get a machine each.
//
// How long an instance takes is not known beforehand. History remembers the
// last durations measured for it (on the machine, from the start of the job to
// its end, not counting start-up). An instance with no history counts as long:
// packing a long job next to another costs it CPU time, while giving a short
// job a machine of its own costs one start-up, and the history makes the next
// run right.
package sched

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// keep is how many durations are remembered per instance.
const keep = 5

// History holds the recent durations of job instances, in a file.
type History struct {
	path string
	mu   sync.Mutex
	d    map[string][]float64 // seconds, oldest first
}

// OpenHistory reads the file at path; a missing file is an empty history, a
// damaged one too (it is only an estimate and gets rewritten).
func OpenHistory(path string) (*History, error) {
	h := &History{path: path, d: map[string][]float64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(b, &h.d) != nil || h.d == nil {
		h.d = map[string][]float64{}
	}
	return h, nil
}

// Key names an instance: the workflow file and the instance's name (which has
// the matrix values in it).
func Key(workflow, instance string) string { return workflow + "\x00" + instance }

// Max is the largest of the remembered values of key: what to plan for when
// being short costs far more than having too much.
func (h *History) Max(key string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.d[key]
	if len(s) == 0 {
		return 0, false
	}
	return time.Duration(slices.Max(s) * float64(time.Second)), true
}

// Estimate is the median of the remembered durations of key.
func (h *History) Estimate(key string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := slices.Clone(h.d[key])
	if len(s) == 0 {
		return 0, false
	}
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return time.Duration(m * float64(time.Second)), true
}

// Record remembers a duration and saves the file. The file is replaced
// atomically, so a crash leaves the old one.
func (h *History) Record(key string, d time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := append(h.d[key], d.Seconds())
	if len(s) > keep {
		s = s[len(s)-keep:]
	}
	h.d[key] = s
	b, err := json.Marshal(h.d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(h.path), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), h.path)
}

// Policy says whether an instance is short enough to share a machine.
type Policy struct {
	History *History
	// Startup is how long a machine has taken to come up so far in this run
	// (zero when none has).
	Startup func() time.Duration
	// Factor: an instance is short when its estimate is below Factor times the
	// start-up time. Default 3.
	Factor float64
	// DefaultStartup is used when nothing has been measured. Default 10 s,
	// the middle of what InstaBox showed.
	DefaultStartup time.Duration
}

// Short reports whether the instance should be packed, and the numbers the
// decision rests on (for the log). An instance without history is not short.
func (p *Policy) Short(key string) (short bool, est, limit time.Duration) {
	if p == nil || p.History == nil {
		return false, 0, 0
	}
	est, ok := p.History.Estimate(key)
	if !ok {
		return false, 0, 0
	}
	start := p.DefaultStartup
	if start <= 0 {
		start = 10 * time.Second
	}
	if p.Startup != nil {
		if s := p.Startup(); s > 0 {
			start = s
		}
	}
	f := p.Factor
	if f <= 0 {
		f = 3
	}
	limit = time.Duration(f * float64(start))
	return est < limit, est, limit
}
