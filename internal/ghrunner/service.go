package ghrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"raccoonci/internal/instabox"
)

const (
	jitFile = "/data/home/.raccoonci-jit" // where the runner's one-time config is put on the machine
	runner  = "/opt/actions-runner"       // the template's Actions runner
)

// Service watches repositories for jobs that ask for one of our labels and
// runs each on a machine of its own.
//
// A job that finds a runner already waiting starts within about a second, but
// a waiting runner is a running machine. A job that finds none makes the
// service start a machine and register a runner for it, which takes a few
// seconds longer. Idle says how many runners to keep waiting per repository.
type Service struct {
	GH *Client
	P  *instabox.Provider
	// Repos are the repositories to watch (owner/name).
	Repos []string
	// LabelPrefix selects the jobs: those with a runs-on label starting with it
	// (compared lower-case). Default "raccoon-".
	LabelPrefix string
	// RunnerLabel is the label of the runners started to wait or to serve a job
	// that has none better. Default LabelPrefix + "4vcpu".
	RunnerLabel string
	// Idle is how many runners to keep waiting, per repository. Zero starts
	// runners only when a job is waiting.
	Idle int
	// Poll is how often a repository is asked for waiting jobs (answers that
	// have not changed are free). Default 1 s. A webhook would replace this.
	Poll time.Duration
	// MaxJobs caps machines at once. Default 8.
	MaxJobs int
	// IdleTimeout is how long a runner started for a job may wait for it before
	// it is given up on. Default 3 minutes. (Waiting runners kept by Idle have
	// no limit.)
	IdleTimeout time.Duration
	// JobTimeout is the longest a job may run. Default 6 hours (GitHub's limit).
	JobTimeout time.Duration
	// Logf receives one line per event. May be nil.
	Logf func(format string, args ...any)

	mu        sync.Mutex
	served    map[int64]*serving // job id -> who is on it
	live      map[*liveRunner]bool
	launching map[string]int // repo -> runners being started
	sem       chan struct{}
	wg        sync.WaitGroup
	topUpMu   sync.Mutex
	topUpDue  bool
}

// serving remembers that a job has been dealt with.
type serving struct {
	attempts int
	at       time.Time
	waiting  bool // an idle runner is expected to take it
}

// liveRunner is a runner on a machine of ours.
type liveRunner struct {
	repo     string
	id       int64
	machine  string
	use      *instabox.Usage
	name     string
	started  time.Time
	resumed  bool
	busy     bool // it has a job
	jobBased bool // started for a job (given up on if no job comes)
}

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Service) prefix() string {
	if s.LabelPrefix != "" {
		return strings.ToLower(s.LabelPrefix)
	}
	return "raccoon-"
}

func (s *Service) runnerLabel() string {
	if s.RunnerLabel != "" {
		return s.RunnerLabel
	}
	return s.prefix() + "4vcpu"
}

// Run serves until ctx ends, then waits for the runners in flight (their
// machines are deleted even then).
func (s *Service) Run(ctx context.Context) error {
	if len(s.Repos) == 0 {
		return errors.New("ghrunner: no repositories to watch")
	}
	n := s.MaxJobs
	if n <= 0 {
		n = 8
	}
	s.sem = make(chan struct{}, n)
	s.served = map[int64]*serving{}
	s.live = map[*liveRunner]bool{}
	s.launching = map[string]int{}
	for _, r := range s.Repos {
		s.wg.Go(func() { s.watch(ctx, r) })
	}
	s.requestTopUp(ctx)
	<-ctx.Done()
	s.wg.Wait()
	s.P.Wait()
	return nil
}

func (s *Service) watch(ctx context.Context, repo string) {
	poll := s.Poll
	if poll <= 0 {
		poll = time.Second
	}
	s.logf("watching %s for jobs labelled %s* (keeping %d runners waiting)", repo, s.prefix(), s.Idle)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		s.scan(ctx, repo)
		s.keepIdle(ctx, repo)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// idleCount is how many runners of repo are waiting for a job (or being
// started, which will be).
func (s *Service) idleCount(repo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.launching[repo]
	for l := range s.live {
		if l.repo == repo && !l.busy {
			n++
		}
	}
	return n
}

// keepIdle starts runners until Idle of them wait.
func (s *Service) keepIdle(ctx context.Context, repo string) {
	for need := s.Idle - s.idleCount(repo); need > 0 && ctx.Err() == nil; need-- {
		s.startFor(ctx, repo, nil, 1, time.Now())
	}
}

// scan starts a runner for every waiting job of ours that no waiting runner is
// expected to take.
func (s *Service) scan(ctx context.Context, repo string) {
	runs, err := s.GH.ActiveRuns(ctx, repo)
	if err != nil {
		if ctx.Err() == nil {
			s.logf("%s: listing runs: %v", repo, err)
		}
		return
	}
	free := s.idleCount(repo)
	for _, run := range runs {
		jobs, err := s.GH.RunJobs(ctx, repo, run.ID)
		if err != nil {
			if ctx.Err() == nil {
				s.logf("%s: listing jobs of run %d: %v", repo, run.ID, err)
			}
			continue
		}
		for _, j := range jobs {
			if j.Status != "queued" || !j.HasLabelPrefix(s.prefix()) {
				continue
			}
			s.mu.Lock()
			sv := s.served[j.ID]
			switch {
			case sv != nil && !sv.waiting:
				s.mu.Unlock() // a runner was started for it
				continue
			case sv != nil && time.Since(sv.at) < 20*time.Second:
				s.mu.Unlock() // a waiting runner should take it any moment
				continue
			case sv == nil && free > 0:
				free-- // an idle runner will take it
				s.served[j.ID] = &serving{at: time.Now(), waiting: true}
				s.mu.Unlock()
				continue
			}
			// Not taken: start a runner for it.
			attempts := 0
			if sv != nil {
				attempts = sv.attempts
			}
			if attempts >= 3 {
				s.mu.Unlock()
				continue
			}
			s.served[j.ID] = &serving{attempts: attempts, at: time.Now()}
			s.mu.Unlock()
			job := j
			s.startFor(ctx, repo, &job, attempts+1, time.Now())
		}
	}
}

// startFor starts a machine and a runner in the background: for job if given,
// else to wait.
func (s *Service) startFor(ctx context.Context, repo string, job *Job, attempt int, seen time.Time) {
	s.mu.Lock()
	s.launching[repo]++
	s.mu.Unlock()
	s.wg.Go(func() {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			s.mu.Lock()
			s.launching[repo]--
			s.mu.Unlock()
			return
		}
		l, tl, err := s.launch(ctx, repo, job, attempt, seen)
		s.mu.Lock()
		s.launching[repo]--
		if err == nil {
			s.live[l] = true
		}
		s.mu.Unlock()
		what := "waiting runner"
		if job != nil {
			what = fmt.Sprintf("job %d (%s)", job.ID, job.Name)
		}
		if err != nil {
			s.logf("%s %s: %v (attempt %d)", repo, what, err, attempt)
			if job != nil {
				s.mu.Lock()
				s.served[job.ID] = &serving{attempts: attempt, at: time.Time{}}
				s.mu.Unlock()
			}
			return
		}
		s.logf("%s %s: runner up (machine %s in %.1fs, runner %.1fs after seen)", repo, what,
			map[bool]string{true: "resumed", false: "created"}[tl.resumed], tl.ready.Sub(tl.seen).Seconds(), tl.started.Sub(tl.seen).Seconds())
		outcome := s.follow(ctx, l)
		s.retire(l)
		if job != nil {
			final, _ := s.GH.GetJob(context.WithoutCancel(ctx), repo, job.ID)
			s.logf("%s %s: %s; queued→started %.1fs, ran %.1fs, %s", repo, what, outcome,
				final.StartedAt.Sub(final.CreatedAt).Seconds(), final.CompletedAt.Sub(final.StartedAt).Seconds(), final.Conclusion)
		} else {
			s.logf("%s %s %s: %s", repo, what, l.name, outcome)
		}
	})
}

// timeline is what start-up looked like.
type timeline struct {
	seen, ready, started time.Time
	resumed              bool
}

// launch gets a machine, registers a runner for it and starts the runner.
func (s *Service) launch(ctx context.Context, repo string, job *Job, attempt int, seen time.Time) (*liveRunner, timeline, error) {
	tl := timeline{seen: seen}
	labels := []string{"self-hosted", s.runnerLabel()}
	tag := fmt.Sprintf("idle-%d", time.Now().UnixNano()%1e9)
	if job != nil {
		labels = append([]string{"self-hosted"}, job.Labels...)
		tag = fmt.Sprintf("%d-%d", job.ID, attempt)
	}
	bg := context.WithoutCancel(ctx)

	type jitResult struct {
		jit JIT
		err error
	}
	jitCh := make(chan jitResult, 1)
	go func() {
		jit, err := s.GH.GenerateJIT(ctx, repo, "rc-"+tag, labels)
		jitCh <- jitResult{jit, err}
	}()
	id, use, err := s.P.Acquire(ctx, "rc-gh-"+tag)
	jr := <-jitCh
	undo := func() {
		if id != "" {
			s.P.Release(bg, id, use)
		}
		if jr.err == nil && jr.jit.RunnerID != 0 {
			s.GH.DeleteRunner(bg, repo, jr.jit.RunnerID)
		}
		s.requestTopUp(bg)
	}
	switch {
	case err != nil:
		undo()
		return nil, tl, fmt.Errorf("getting a machine: %w", err)
	case jr.err != nil:
		undo()
		return nil, tl, fmt.Errorf("registering a runner: %w", jr.err)
	}
	tl.ready, tl.resumed = time.Now(), use.Resumed
	if err := s.P.Client.PutFile(ctx, id, jitFile, "0600", []byte(jr.jit.Config)); err != nil {
		undo()
		return nil, tl, fmt.Errorf("sending the runner its config: %w", err)
	}
	// The config is read from the file: it must not be on a command line, in
	// the machine's exec history or in the log.
	start := fmt.Sprintf(`cd %s && (setsid nohup ./run.sh --jitconfig "$(cat %s)" >/data/home/runner.log 2>&1 </dev/null &)`, runner, jitFile)
	if code, out, err := s.P.Client.Exec(ctx, id, start); err != nil || code != 0 {
		undo()
		return nil, tl, fmt.Errorf("starting the runner: exit %d %q: %v", code, strings.TrimSpace(out), err)
	}
	tl.started = time.Now()
	return &liveRunner{repo: repo, id: jr.jit.RunnerID, machine: id, use: use, name: "rc-" + tag,
		started: tl.started, resumed: tl.resumed, jobBased: job != nil}, tl, nil
}

// follow watches the runner until its job is over: GitHub removes a
// just-in-time runner when it is. It records when the runner has a job. It
// returns "done", or why it gave up.
func (s *Service) follow(ctx context.Context, l *liveRunner) string {
	idle := s.IdleTimeout
	if idle <= 0 {
		idle = 3 * time.Minute
	}
	limit := s.JobTimeout
	if limit <= 0 {
		limit = 6 * time.Hour
	}
	began := time.Now()
	var busySince time.Time
	for {
		select {
		case <-ctx.Done():
			return "cancelled"
		case <-time.After(time.Second):
		}
		r, err := s.GH.GetRunner(ctx, l.repo, l.id)
		switch {
		case errors.Is(err, NotFound):
			return "done"
		case err != nil:
			continue
		case r.Busy && busySince.IsZero():
			busySince = time.Now()
			s.mu.Lock()
			l.busy = true
			s.mu.Unlock()
		}
		if busySince.IsZero() && l.jobBased && time.Since(began) > idle {
			return fmt.Sprintf("gave up: the runner took no job in %v", idle)
		}
		if !busySince.IsZero() && time.Since(busySince) > limit {
			return "gave up: the job ran past the time limit"
		}
	}
}

// retire deletes a runner's machine and registration.
func (s *Service) retire(l *liveRunner) {
	s.mu.Lock()
	delete(s.live, l)
	s.mu.Unlock()
	bg := context.Background()
	s.P.Release(bg, l.machine, l.use)
	s.GH.DeleteRunner(bg, l.repo, l.id)
	s.requestTopUp(bg)
}

// requestTopUp refills the paused spares in the background, one refill at a
// time.
func (s *Service) requestTopUp(ctx context.Context) {
	if s.P.Spares == nil {
		return
	}
	s.topUpMu.Lock()
	if s.topUpDue {
		s.topUpMu.Unlock()
		return
	}
	s.topUpDue = true
	s.topUpMu.Unlock()
	s.wg.Go(func() {
		defer func() {
			s.topUpMu.Lock()
			s.topUpDue = false
			s.topUpMu.Unlock()
		}()
		if err := s.P.Spares.TopUp(context.WithoutCancel(ctx)); err != nil {
			s.logf("paused spare machines: %v", err)
		}
	})
}
