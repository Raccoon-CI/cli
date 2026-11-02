package ghrunner

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Raccoon-CI/raccoonci/pkg/instabox"
)

// A job names the size of the machine it wants in its runs-on label:
// `raccoon-8vcpu` is a machine of 8 vCPUs, with the memory InstaBox gives that
// many (the sizes the organization may create come from the API). A label
// that says no size gets the default size.

// labelVCPUs reads "<prefix><N>vcpu" among labels (compared lower-case).
func labelVCPUs(prefix string, labels []string) (int, bool) {
	for _, l := range labels {
		rest, ok := strings.CutPrefix(strings.ToLower(l), prefix)
		if !ok {
			continue
		}
		digits, ok := strings.CutSuffix(rest, "vcpu")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(digits); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// SizeError says a job asks for a size the organization cannot create. Such a
// job would wait for a runner for ever, so the service says so and cancels its
// run instead of leaving it.
type SizeError struct {
	Label   string
	VCPUs   int
	Allowed []instabox.Size
}

func (e *SizeError) Error() string {
	var have []string
	for _, a := range e.Allowed {
		have = append(have, fmt.Sprintf("%d vCPU / %d GiB", a.VCPUs, a.MemMiB/1024))
	}
	return fmt.Sprintf("%s asks for %d vCPUs, which this InstaBox organization cannot create (it can: %s)", e.Label, e.VCPUs, strings.Join(have, ", "))
}

// machineSize is the size of machine a job's labels ask for; zero means the
// default size.
func (s *Service) machineSize(ctx context.Context, labels []string) (instabox.Size, error) {
	n, ok := labelVCPUs(s.prefix(), labels)
	if !ok {
		return instabox.Size{}, nil
	}
	s.mu.Lock()
	allowed := s.sizes
	s.mu.Unlock()
	if allowed == nil {
		got, err := s.P.Client.AllowedSizes(ctx)
		if err != nil {
			if slices.Contains(labels, s.runnerLabel()) {
				// The default label is the default size: a failure to read the list
				// must not keep those jobs from running.
				s.logf("reading the machine sizes: %v; using the default size", err)
				return instabox.Size{}, nil
			}
			return instabox.Size{}, fmt.Errorf("reading the machine sizes: %w", err)
		}
		allowed = got
		s.mu.Lock()
		s.sizes = got
		s.mu.Unlock()
	}
	var fit []instabox.Size
	for _, a := range allowed {
		if a.VCPUs == n {
			fit = append(fit, a)
		}
	}
	if len(fit) == 0 {
		return instabox.Size{}, &SizeError{Label: s.prefix() + strconv.Itoa(n) + "vcpu", VCPUs: n, Allowed: allowed}
	}
	// Two sizes with the same vCPUs: the one with 2 GiB for each, else the smaller.
	slices.SortFunc(fit, func(a, b instabox.Size) int { return a.MemMiB - b.MemMiB })
	for _, f := range fit {
		if f.MemMiB == 2048*n {
			return f, nil
		}
	}
	return fit[0], nil
}

// waitingCanServe reports whether a runner kept waiting (labels self-hosted
// and the service's runner label) can take a job with these labels: a runner
// takes a job only if it has every label the job asks for.
func (s *Service) waitingCanServe(labels []string) bool {
	own := strings.ToLower(s.runnerLabel())
	for _, l := range labels {
		if l = strings.ToLower(l); l != "self-hosted" && l != own {
			return false
		}
	}
	return true
}
