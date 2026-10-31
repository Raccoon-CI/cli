# When to keep idle runners

`raccoonci serve` can keep runners waiting so that a job starts about a second
after it is created, instead of 7 to 10 seconds. A waiting runner is a running
machine, so it costs money while it waits. This note models the choice, compares
policies on real arrival data, and says what to build.

Reproduce: `fetchruns.sh` downloads the creation times of the last 1000 workflow
runs of twelve public repositories (needs `gh`), `trace_stats.py` summarises them,
`idlesim.py [--known-schedule | --pooled]` simulates the policies.

## Measured inputs (2026-10-31, InstaBox `ci` template, 4 vCPU, 8 GiB, 70 GiB disk)

| quantity | value | how |
|---|---|---|
| job created to started, runner already waiting | 1 s (3 of 3) | GitHub's `created_at`/`started_at` |
| job created to started, runner started for it | 7 to 10 s | same; machine 1 to 3.6 s, runner listener 3 s, polling and assignment the rest |
| a paused spare vs a new machine from the `ci` template | resume 1.1 to 3.6 s vs create 1 to 2.6 s | so **paused spares barely help with this template**; the runner start-up is what costs |
| a runner after suspend and resume | online again after 10 to 45 s | so a paused machine cannot hold a waiting runner |
| waiting machine cost c | 1.16e-5 $/s (0.042 $/h, 1.0 $/day) | InstaBox unit prices of 2026-10-29; paused: disks only, about 0.008 $/h (an assumption) |

## Model

A job asks for a runner at time t. If one is waiting it starts after L_hot = 1 s,
else a machine and runner are started for it and it starts after L_cold = 9 s;
Δ = L_cold − L_hot = 8 s. A just-in-time runner takes one job, so after each job
a replacement is started (ready after R = 8 s) if the policy wants one.

For one stream with inter-arrival gaps G, a policy that keeps a runner waiting for
T seconds after a job pays, per job,

    cost(T) = c · E[min(G, R + T)] + v · E[latency(G)],   latency = L_hot if G ≤ R + T, else L_cold

where v is what a second of waiting is worth. Setting the derivative to zero gives
the rule: **keep waiting while the hazard rate of the next arrival, h(t) = f(t)/S(t),
exceeds h\* = c / (Δ · v)**. For Poisson arrivals h is constant, so the answer is
all or nothing: keep a runner forever iff λ > h\*. For bursty arrivals (a push is
followed by another within minutes, then silence) h falls with time, so there is a
finite best TTL: the time at which h drops to h\*. With v = 0.01 $/s, h\* = 0.52 jobs
per hour; with v = 0.001, 5 per hour.

Arrival classes need different treatment:

* **Scheduled** (`on.schedule`): known in advance. Start the runner at T − R; idle cost
  is c·R ≈ 1e-4 $ per job, latency 1 s. No policy needed.
* **Downstream jobs** (`needs:`): the workflow file says they will be queued when their
  needs finish; the upstream job's history says when. Start them at end − R: the same
  c·R for a 8 s gain, worth it for any v above 1e-5 $/s.
* **Everything else** (pushes, pull requests): stochastic and bursty; this is where
  the policy matters. A burst of k workflows needs k runners.

## Real arrivals

Twelve public repositories, last 1000 runs each (bursts = runs within 30 s):

* Busy: cli/cli 4.5 bursts/h, prometheus 3.1, vite 1.4, tokio 1.1; median gap 8 to 18 min.
* Quiet: 0.03 to 0.2 bursts/h, median gap 4 to 17 hours, with strong peaks at exactly
  86400 s (cron). Even there about 10% of gaps are under 5 minutes (follow-up commits).

## Policies compared

Objective J = idle and start-up cost + v · latency, dollars per day, summed over the
twelve repositories (lower is better; the oracle knows every gap in advance and holds 0,
1 or 2 runners per gap, so it is a lower bound no online policy reaches):

| policy | v = 0.001 | v = 0.003 | v = 0.01 | v = 0.03 |
|---|---|---|---|---|
| never keep one | 6.1 | 18.3 | 60.7 | 181.9 |
| always keep one (`-idle`) | 21.2 | 27.9 | 51.6 | 119.1 |
| fixed TTL 5 min | 6.1 | 16.1 | 51.0 | 150.6 |
| fixed TTL 15 min | 6.9 | 15.8 | 46.9 | 135.8 |
| fixed TTL 60 min | 8.7 | 16.1 | 41.8 | 115.3 |
| break-even TTL (ski rental, T\* = vΔ/c) | 6.6 | 15.5 | 42.1 | 112.9 |
| recent-rate threshold | 6.4 | 16.4 | 42.6 | 113.3 |
| empirical TTL (best TTL over the last 100 gaps) | 6.0 | 15.1 | 41.7 | 111.8 |
| empirical window (also a start delay) | 5.3 | 14.8 | 41.3 | 111.9 |
| oracle | 5.0 | 12.9 | 36.2 | 99.2 |

What this says:

* Always keeping a runner is the worst thing to do for quiet repositories (about 1 $/day for
  nothing; J is 1.0 to 2.0 against 0.02 to 0.1) and the best for the busiest ones. No single
  fixed setting is good for all of them, at any v.
* Never keeping one is nearly optimal only when waiting is cheap (v ≤ 0.001).
* Fixed TTLs fit one kind of repository: 5 to 15 minutes for quiet ones (it catches the
  follow-up commit), an hour or more for busy ones.
* **Adaptive policies track the best choice for every repository** and land within 6% to
  13% of the oracle. The simplest one, the break-even TTL, which needs no data at all, is
  within 2% to 5% of the best adaptive one for v ≥ 0.003. Learning the TTL from the repository's
  own recent gaps adds a few percent.
* Treating scheduled runs as known in advance removes them from the stochastic stream and
  lowers every number a little; the ranking does not change.

## Sharing runners across repositories

The same twelve streams, 3.8 days, objective in dollars (per-repository pools of 2 runners
each against one shared pool; shared pools need org-level runners):

| v | never | per repository, adaptive | shared pool of 2 | of 4 | of 8 |
|---|---|---|---|---|---|
| 0.003 | 70.7 | 59.9 | 46.7 | 43.8 | 44.4 |
| 0.01 | 235 | 164 | 142 | 114 | 102 |
| 0.03 | 705 | 436 | 413 | 314 | 268 |

Pooling is the biggest single lever (−30% to −40% against per-repository pools at v ≥ 0.01):
merged streams are dense, so a shared pool is rarely idle and the choice of keep-alive policy
stops mattering (always, break-even and empirical give the same cost). The caveat is that the
streams are overlaid artificially (different repositories, different times), so independence
is assumed.

## What to build

1. **Know the future first** (cost near zero): pre-start for `on.schedule` and for `needs:` jobs.
2. **Adaptive keep-alive for the rest**: start with the break-even TTL; after 20 gaps choose the
   TTL that would have been best over the last 100 (per repository, and per hour of the week once
   there is data); pool size from the burst-size estimate (EWMA, 1 to 4).
3. **Share across repositories** with org-level runners (needs a GitHub App or an `admin:org` token).
4. **Expose a budget, not v**: "at most B dollars per day on waiting runners". A controller sets v so
   that spending stays under B, which spends it where seconds saved per dollar are highest.

## Limits of this study

* v is unknown; the table brackets it. Whether anyone is waiting on a job (a pull request
  check) or not (nightly) changes it by a factor of ten or more.
* L_cold and R are single measurements; arrivals are public repositories, and a private team's
  rhythm (office hours, weekends) may differ. The empirical policies adapt, a fixed one does not.
* A waiting machine is assumed to be billed in full; InstaBox pauses idle machines after 30 minutes
  by default and it is untested whether a machine running a listening runner counts as idle.
* Job DAGs, runtimes and queueing beyond the pool are not simulated.
