package ghrunner

import "time"

// InstaBox limits how many machines an organization may start a minute (10 on
// the Pro plan). Past it a start is refused with 429. The service counts its own
// starts so that it does not run into the limit, and keeps the last ones for
// jobs that are queued: a runner started ahead of a job that is not yet queued
// is a guess, and a guess must not leave a real job without a start.

// startWindow is a little longer than the minute the limit speaks of.
const startWindow = 62 * time.Second

// startHold is how long starts wait after InstaBox has refused one (a var so a
// test need not wait).
var startHold = 20 * time.Second

// reserveStart takes one start if there is one to take. A start for a runner
// that is only expected to be wanted (speculative) may not use the share kept
// for jobs. When it refuses it says how long until a start frees up.
func (s *Service) reserveStart(speculative bool) (bool, time.Duration) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	now := time.Now()
	if now.Before(s.holdUntil) { // InstaBox refused one: let the minute pass
		return false, s.holdUntil.Sub(now)
	}
	if s.StartsPerMinute <= 0 {
		return true, 0
	}
	i := 0
	for i < len(s.starts) && now.Sub(s.starts[i]) > startWindow {
		i++
	}
	s.starts = s.starts[i:]
	limit := s.StartsPerMinute
	if speculative {
		limit -= max(1, s.StartsPerMinute*3/10)
		if limit < 1 {
			return false, time.Second
		}
	}
	if len(s.starts) >= limit {
		// the start that has to leave the window before one is free
		wait := s.starts[len(s.starts)-limit].Add(startWindow).Sub(now)
		return false, max(wait, time.Second)
	}
	s.starts = append(s.starts, now)
	return true, 0
}
