package ghrunner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"raccoonci/internal/instabox"
	"raccoonci/internal/sched"
	"raccoonci/internal/workflow"
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
	// Org, if set, registers the runners with this organization instead of with
	// one repository each: a waiting runner then takes a job of any of Repos, so
	// one pool serves them all and keeps fewer machines idle than a pool each.
	// The organization's default runner group must be open to the repositories.
	Org string
	// LabelPrefix selects the jobs: those with a runs-on label starting with it
	// (compared lower-case). Default "raccoon-".
	LabelPrefix string
	// RunnerLabel is the label of the runners started to wait or to serve a job
	// that has none better. Default LabelPrefix + "4vcpu".
	RunnerLabel string
	// Idle is how many runners to keep waiting at least, per repository. Zero
	// starts runners only when a job is waiting, or, with KeepAlive, when its
	// policy or a known job says so.
	Idle int
	// KeepAlive, if set, decides how many more to keep waiting after jobs have
	// come, from how this repository's jobs have been arriving.
	KeepAlive *KeepAlive
	// Durations, if set, remembers how long jobs take, so the jobs that need a
	// running one can be given a runner just before it finishes. Without it
	// only scheduled workflows are anticipated.
	Durations *sched.History
	// Poll is how often a repository is asked for waiting jobs (answers that
	// have not changed are free). Default 1 s. A webhook would replace this.
	Poll time.Duration
	// Webhooks says that GitHub delivers workflow_job events (see Webhook), so
	// the repositories are listed only every SafetyPoll. Default 30 s.
	Webhooks   bool
	SafetyPoll time.Duration
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
	expected  []*expectation
	wfCache   map[string]*workflow.Workflow
	recorded  map[int64]bool           // jobs whose duration is in Durations
	arrived   map[int64]bool           // runs counted as an arrival
	wake      map[string]chan struct{} // repo -> the loop that watches it
	sizes     []instabox.Size          // what the organization may create; read on first use
	tracked   map[int64]trackedRun     // runs with a job waiting for one that is still running
	runCtx    context.Context          // set while Run runs; webhook deliveries start work under it
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
	pool     string
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
	jobID    int64
	jobRepo  string // where the job is, which can differ from repo (an organization)
	// keepUntil: a runner started for a known job is not retired before then.
	keepUntil time.Time
	retire    bool // asked to stop waiting
}

// takenCheckAfter is how long a runner started for a job waits before asking
// whether another runner has taken the job.
var takenCheckAfter = 10 * time.Second

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// poolOf names the pool of runners that serve repo: the repository itself, or
// the organization when runners are shared.
func (s *Service) poolOf(repo string) string {
	if s.Org != "" {
		return OrgScope + s.Org
	}
	return repo
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
	// Everything a webhook delivery touches is set up under the lock, runCtx last:
	// a delivery that arrives before this is done is told the service is not
	// running.
	s.mu.Lock()
	s.sem = make(chan struct{}, n)
	s.served = map[int64]*serving{}
	s.live = map[*liveRunner]bool{}
	s.launching = map[string]int{}
	s.arrived = map[int64]bool{}
	s.wake = map[string]chan struct{}{}
	type loop struct {
		repos []string
		wake  chan struct{}
	}
	var loops []loop
	if s.Org != "" {
		ch := make(chan struct{}, 1)
		for _, r := range s.Repos {
			s.wake[r] = ch
		}
		loops = append(loops, loop{s.Repos, ch}) // one pool: one loop, so its idle runners are counted once
	} else {
		for _, r := range s.Repos {
			ch := make(chan struct{}, 1)
			s.wake[r] = ch
			loops = append(loops, loop{[]string{r}, ch})
		}
	}
	s.runCtx = ctx
	s.mu.Unlock()
	for _, l := range loops {
		s.wg.Go(func() { s.watch(ctx, l.repos, l.wake) })
	}
	s.requestTopUp(ctx)
	<-ctx.Done()
	s.mu.Lock()
	s.runCtx = nil // deliveries from now on are told the service has stopped
	s.mu.Unlock()
	s.wg.Wait()
	s.P.Wait()
	return nil
}

// watch serves repos, which share one pool of runners.
//
// The repositories are listed for waiting jobs every Poll, or, when webhooks
// are delivered, every SafetyPoll (they tell what happened at once, the listing
// is there for a delivery that never came) and whenever one of them asks for it
// through wake. The local state (runners to start ahead of a job, runners to let
// go) is looked at every second or Poll in any case: it costs no request.
func (s *Service) watch(ctx context.Context, repos []string, wake <-chan struct{}) {
	pool := s.poolOf(repos[0])
	poll := s.Poll
	if poll <= 0 {
		poll = time.Second
	}
	scanEvery, tickEvery := poll, poll
	if s.Webhooks {
		scanEvery = s.SafetyPoll
		if scanEvery <= 0 {
			scanEvery = 30 * time.Second
		}
		tickEvery = min(poll, time.Second)
	}
	how := fmt.Sprintf("listing every %v", scanEvery)
	if s.Webhooks {
		how = fmt.Sprintf("webhooks, and a listing every %v", scanEvery)
	}
	s.logf("watching %s for jobs labelled %s* (%s; runners of %s; keeping %d waiting)", strings.Join(repos, ", "), s.prefix(), how, pool, s.Idle)
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	var lastSchedules, lastScan time.Time
	woken := false
	for {
		if time.Since(lastSchedules) > 10*time.Minute {
			lastSchedules = time.Now()
			for _, repo := range repos {
				s.refreshSchedules(ctx, repo)
			}
		}
		if woken || time.Since(lastScan) >= scanEvery {
			lastScan = time.Now()
			for _, repo := range repos {
				s.scan(ctx, repo)
			}
		}
		woken = false
		s.refreshTracked(ctx, repos)
		s.prestart(ctx, pool)
		s.keepIdle(ctx, pool)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
			woken = true
		}
	}
}

// idleCount is how many runners of repo are waiting for a job (or being
// started, which will be).
func (s *Service) idleCount(repo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleCountLocked(repo)
}

// freeLocked is how many waiting runners of pool no job is promised to: the
// ones that are idle or being started, less the jobs they are expected to take
// (a promise lasts 20 s, after which the job is given a runner of its own).
// The caller holds s.mu.
func (s *Service) freeLocked(pool string) int {
	n := s.idleCountLocked(pool)
	for _, sv := range s.served {
		if sv.waiting && sv.pool == pool && time.Since(sv.at) < 20*time.Second {
			n--
		}
	}
	return n
}

func (s *Service) idleCountLocked(repo string) int {
	n := s.launching[repo]
	for l := range s.live {
		if l.repo == repo && !l.busy {
			n++
		}
	}
	return n
}

// idleTarget is how many runners repo should have waiting now.
func (s *Service) idleTarget(repo string) int {
	n := s.Idle
	if s.KeepAlive != nil {
		n = max(n, s.KeepAlive.Target(repo, time.Now()))
	}
	return n
}

// keepIdle brings the runners waiting for repo to the target: more are started,
// and surplus ones that no known job needs any more are let go.
func (s *Service) keepIdle(ctx context.Context, repo string) {
	target := s.idleTarget(repo)
	have := s.idleCount(repo)
	for need := target - have; need > 0 && ctx.Err() == nil; need-- {
		s.startFor(ctx, repo, nil, 1, time.Now(), time.Time{})
	}
	if have <= target {
		return
	}
	// Retire the oldest waiting runners that are past their keep-until time.
	now := time.Now()
	s.mu.Lock()
	var surplus []*liveRunner
	for l := range s.live {
		if l.repo == repo && !l.busy && !l.jobBased && !l.retire && now.After(l.keepUntil) {
			surplus = append(surplus, l)
		}
	}
	slices.SortFunc(surplus, func(a, b *liveRunner) int { return a.started.Compare(b.started) })
	for i := 0; i < len(surplus) && have-i > target; i++ {
		surplus[i].retire = true
	}
	s.mu.Unlock()
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
	for _, run := range runs {
		jobs, err := s.GH.RunJobs(ctx, repo, run.ID)
		if err != nil {
			if ctx.Err() == nil {
				s.logf("%s: listing jobs of run %d: %v", repo, run.ID, err)
			}
			continue
		}
		s.observeRun(ctx, repo, run, jobs)
		s.noteArrival(repo, run, jobs)
		for _, j := range jobs {
			j.Repo = repo
			s.consider(ctx, j)
		}
	}
}

// consider decides what a job needs: nothing when a runner is on it or a
// waiting runner is about to take it, else a runner of its own. It is called
// for the jobs a scan lists and for those a webhook delivers, and may run
// more than once for one job.
func (s *Service) consider(ctx context.Context, j Job) {
	if !j.HasLabelPrefix(s.prefix()) {
		return
	}
	pool := s.poolOf(j.Repo)
	s.reportPrediction(j.Repo, j) // a job a runner was started ahead for may be taken before a scan sees it queued
	s.mu.Lock()
	sv := s.served[j.ID]
	if j.Status != "queued" {
		if sv != nil && sv.waiting {
			delete(s.served, j.ID) // a waiting runner took it: it is busy now, and no longer promised
		}
		s.mu.Unlock()
		return
	}
	switch {
	case sv != nil && !sv.waiting && !sv.at.IsZero():
		s.mu.Unlock() // a runner was started for it
		return
	case sv != nil && sv.waiting && time.Since(sv.at) < 20*time.Second:
		s.mu.Unlock() // a waiting runner should take it any moment
		return
	case sv == nil && s.waitingCanServe(j.Labels) && s.freeLocked(pool) > 0:
		s.served[j.ID] = &serving{at: time.Now(), waiting: true, pool: pool} // an idle runner will take it
		s.mu.Unlock()
		return
	}
	// Not taken: start a runner for it.
	attempts := 0
	if sv != nil {
		attempts = sv.attempts
	}
	if attempts >= 3 {
		s.mu.Unlock()
		return
	}
	s.served[j.ID] = &serving{attempts: attempts, at: time.Now(), pool: pool}
	s.mu.Unlock()
	s.startFor(ctx, pool, &j, attempts+1, time.Now(), time.Time{})
}

// noteArrival tells the keep-alive policy that a run asked for our runners: once
// per run, at the time of its first job, and only for runs somebody started
// (a scheduled run is known in advance and says nothing about activity).
func (s *Service) noteArrival(repo string, run Run, jobs []Job) {
	if s.KeepAlive == nil || run.Event == "schedule" {
		return
	}
	var first time.Time
	for _, j := range jobs {
		if j.HasLabelPrefix(s.prefix()) && (first.IsZero() || j.CreatedAt.Before(first)) {
			first = j.CreatedAt
		}
	}
	if first.IsZero() {
		return
	}
	s.mu.Lock()
	seen := s.arrived[run.ID]
	s.arrived[run.ID] = true
	s.mu.Unlock()
	if !seen {
		s.KeepAlive.Arrival(s.poolOf(repo), first)
	}
}

// startFor starts a machine and a runner of pool (a repository, or an
// organization) in the background: for job if given, else to wait.
func (s *Service) startFor(ctx context.Context, repo string, job *Job, attempt int, seen time.Time, keepUntil time.Time) {
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
			l.keepUntil = keepUntil
			s.live[l] = true
		}
		s.mu.Unlock()
		what := "waiting runner"
		if job != nil {
			what = fmt.Sprintf("job %d (%s)", job.ID, job.Name)
		}
		if err != nil {
			var sizeErr *SizeError
			if errors.As(err, &sizeErr) && job != nil {
				// No machine can ever serve it: say so and cancel its run, rather than
				// leave it queued for ever.
				s.logf("%s %s: %v; cancelling run %d", repo, what, err, job.RunID)
				s.mu.Lock()
				s.served[job.ID] = &serving{attempts: 3, at: time.Now()}
				s.mu.Unlock()
				if cerr := s.GH.CancelRun(context.WithoutCancel(ctx), cmp.Or(job.Repo, repo), job.RunID); cerr != nil {
					s.logf("%s %s: cancelling the run: %v", repo, what, cerr)
				}
				return
			}
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
			final, _ := s.GH.GetJob(context.WithoutCancel(ctx), cmp.Or(job.Repo, repo), job.ID)
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
	var jobID int64
	var jobRepo string
	if job != nil {
		jobID, jobRepo = job.ID, job.Repo
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
	var size instabox.Size
	var sizeErr error
	if job != nil {
		size, sizeErr = s.machineSize(ctx, job.Labels)
	} else {
		// A runner kept waiting is of the size its label names.
		size, sizeErr = s.machineSize(ctx, []string{s.runnerLabel()})
	}
	var id string
	var use *instabox.Usage
	var err error
	if sizeErr == nil {
		id, use, err = s.P.AcquireSize(ctx, "rc-gh-"+tag, size)
	} else {
		err = sizeErr
	}
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
		if sizeErr != nil {
			return nil, tl, sizeErr
		}
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
		started: tl.started, resumed: tl.resumed, jobBased: job != nil, jobID: jobID, jobRepo: cmp.Or(jobRepo, repo)}, tl, nil
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
	var busySince, checked time.Time
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
		s.mu.Lock()
		stop := l.retire
		s.mu.Unlock()
		// Deleting the registration is what makes retiring safe: GitHub refuses it
		// for a runner that has a job, and a runner that is gone can get none.
		if stop && !r.Busy && busySince.IsZero() && s.GH.DeleteRunner(ctx, l.repo, l.id) == nil {
			return "retired: no job expected"
		}
		if busySince.IsZero() && l.jobBased && time.Since(began) > idle {
			return fmt.Sprintf("gave up: the runner took no job in %v", idle)
		}
		// Its job may have gone to another runner (one that was waiting): this one
		// then has nothing to do, and every second of it is paid for.
		if busySince.IsZero() && l.jobBased && !r.Busy && time.Since(began) > takenCheckAfter && time.Since(checked) > 5*time.Second {
			checked = time.Now()
			if j, err := s.GH.GetJob(ctx, l.jobRepo, l.jobID); err == nil && j.Status != "queued" && !r.Busy {
				if s.GH.DeleteRunner(ctx, l.repo, l.id) == nil {
					return "taken by another runner; this one is released"
				}
			}
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
