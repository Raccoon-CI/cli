"""Which idle-runner policy is best?

Model. A job asks for a runner at time t. If a runner is already waiting (hot)
it starts after L_HOT seconds; otherwise a machine and runner are started for
it, which takes L_COLD. A waiting runner is a running machine: c dollars per
second. A just-in-time runner takes one job, so after each job the policy decides
whether to start another to wait.

Cost of one policy on one stream of arrivals = machine-seconds spent waiting or
starting (valued at c) + v dollars per second of start-up latency, summed over
jobs. Compared per day. The traces are real: the creation times of the last
1000 workflow runs of public repositories. A push often starts several
workflows at once, so runs within 30 s are one burst of k jobs wanting k runners.
"""
import glob, os, math, datetime, sys, collections

BASE = "/tmp/claude-1000/-data-workspace-raccoonci/939c54fa-2146-5c42-b12b-5d51edacc123/scratchpad/traces"
C = 4 * 1.111e-6 + 8 * 6.25e-7 + 70 * 3.1e-8   # 4 vCPU, 8 GiB, 70 GiB of disk, per second
R = 8.0        # from starting a machine to a runner that is waiting
L_HOT = 1.0    # job created -> started, with a runner waiting (measured: 1 s)
L_COLD = 9.0   # job created -> started, machine and runner started for it (measured: 7 to 10 s)
DELTA = L_COLD - L_HOT


def load(path, drop_schedule):
    rows = []
    for line in open(path):
        p = line.rstrip("\n").split("\t")
        if len(p) < 2 or not p[0]:
            continue
        t = datetime.datetime.strptime(p[0], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()
        rows.append((t, p[1]))
    rows.sort()
    sched = [t for t, e in rows if e == "schedule"]
    rest = [t for t, e in rows if e != "schedule"] if drop_schedule else [t for t, _ in rows]
    return rest, sched, rows[0][0], rows[-1][0]


def bursts(ts, window=30.0):
    out = []
    for t in ts:
        if out and t - out[-1][0] <= window:
            out[-1][1] += 1
        else:
            out.append([t, 1])
    return out  # [start, k]


# ---- policies: given the gap history, return (start_after, stay_until) of the warm window after a burst ----

class Never:
    name = "never"
    def window(self, gaps, v, t): return None

class Always:
    name = "always"
    def window(self, gaps, v, t): return (0.0, math.inf)

class FixedTTL:
    def __init__(self, T): self.T = T; self.name = f"ttl {int(T/60)}min"
    def window(self, gaps, v, t): return (0.0, self.T)

class SkiRental:
    """Keep warm for the break-even time vΔ/c: 2-competitive without knowing anything."""
    name = "ski-rental"
    def window(self, gaps, v, t): return (0.0, v * DELTA / C)

GRID = [30, 60, 120, 240, 480, 900, 1800, 3600, 2 * 3600, 4 * 3600, 8 * 3600, 16 * 3600, 26 * 3600]

def expected(gaps, a, b, v):
    """Mean cost+latency per gap if a runner waits from a to b after a burst (a=0: right away)."""
    tot = 0.0
    for g in gaps:
        if g < a:
            tot += v * L_COLD + C * L_COLD          # arrives before the window: starts cold
        elif g <= b:
            tot += C * (g - a + R) + v * L_HOT      # caught by the waiting runner
        else:
            tot += C * (b - a + R) + v * L_COLD + C * L_COLD
    return tot / len(gaps)

class EmpiricalTTL:
    """Choose the TTL that would have been best over the last M gaps of this repository."""
    def __init__(self, M=100): self.M = M; self.name = f"empirical-ttl(M={M})"
    def window(self, gaps, v, t):
        h = gaps[-self.M:]
        if len(h) < 20:
            return (0.0, v * DELTA / C)
        best = min([0.0] + GRID, key=lambda T: expected(h, 0.0, T, v) if T else sum(v * L_COLD + C * L_COLD for _ in h) / len(h))
        return None if best == 0 else (0.0, best)

class EmpiricalWindow:
    """Like EmpiricalTTL but may also wait before starting the runner (a pre-warm window [a, b])."""
    def __init__(self, M=100): self.M = M; self.name = f"empirical-window(M={M})"
    def window(self, gaps, v, t):
        h = gaps[-self.M:]
        if len(h) < 20:
            return (0.0, v * DELTA / C)
        none = sum(v * L_COLD + C * L_COLD for _ in h) / len(h)
        best, arg = none, None
        for a in [0.0] + GRID:
            for b in GRID:
                if b <= a:
                    continue
                e = expected(h, a, b, v)
                if e < best:
                    best, arg = e, (a, b)
        return arg

class RateThreshold:
    """Keep a runner for ever while the recent arrival rate is above the break-even rate, none otherwise."""
    def __init__(self, M=30): self.M = M; self.name = "rate-threshold"
    def window(self, gaps, v, t):
        h = gaps[-self.M:]
        if len(h) < 5:
            return (0.0, v * DELTA / C)
        lam = len(h) / sum(h)
        return (0.0, math.inf) if lam * DELTA * v > C else None


def simulate(arrivals, policy, v, n_pool):
    """arrivals: [(t, k)]. Returns (dollars, latency_seconds_total, jobs)."""
    cost = lat = jobs = 0.0
    runners = []          # start times of runners that exist
    win = None            # (start, end) absolute
    gaps = []
    last = None
    for t, k in arrivals:
        if last is not None:
            gaps.append(t - last)
        # runners whose window has closed were retired at its end
        if win is not None:
            keep = []
            for s in runners:
                if t > win[1]:
                    cost += C * max(0.0, win[1] - s)
                else:
                    keep.append(s)
            runners = keep
        runners.sort()
        served = 0
        for s in runners[:]:
            if served >= k:
                break
            ready = s + R
            wait = max(0.0, ready - t)
            if wait + L_HOT >= L_COLD:
                continue                      # not worth waiting for: start a cold one
            lat += wait + L_HOT
            cost += C * (t + wait - s)
            runners.remove(s)
            served += 1
        lat += (k - served) * L_COLD
        cost += (k - served) * C * L_COLD
        jobs += k
        # the policy decides about the gap that follows
        w = policy.window(gaps, v, t)
        for s in runners:
            cost += C * (t - s)               # a leftover runner is retired now when the new window starts later
        if w is None:
            runners, win = [], None
        else:
            a, b = w
            if a > 0:
                for s in runners:
                    pass
                runners = []
            start = t + a
            win = (start, t + b)
            want = max(1, min(n_pool, k))
            have = len(runners)
            runners += [start] * max(0, want - have) if a == 0 else [start] * want
        last = t
    # end of trace: runners alive are retired at the window end or now
    end_t = arrivals[-1][0]
    for s in runners:
        e = min(win[1], end_t + 3600) if win else end_t
        cost += C * max(0.0, e - s)
    return cost, lat, jobs


def oracle(arrivals, v, n_pool=2):
    """Offline optimum, knowing every gap in advance: for each burst of k jobs choose how many
    runners (0..n_pool) to have been holding through the gap before it."""
    cost = lat = jobs = 0.0
    prev = None
    for t, k in arrivals:
        g = None if prev is None else t - prev
        best = None
        for n in range(0, (n_pool if g is not None else 0) + 1):
            hot = min(n, k)
            c_ = (n * C * (g + R) if n else 0.0) + (k - hot) * C * L_COLD
            l_ = hot * L_HOT + (k - hot) * L_COLD
            if best is None or c_ + v * l_ < best[0] + v * best[1]:
                best = (c_, l_)
        cost += best[0]; lat += best[1]; jobs += k
        prev = t
    return cost, lat, jobs


def main():
    drop = "--known-schedule" in sys.argv
    files = sorted(glob.glob(BASE + "/*.tsv"))
    vs = [0.001, 0.003, 0.01, 0.03]
    pols = [Never(), Always(), FixedTTL(300), FixedTTL(900), FixedTTL(3600), SkiRental(), RateThreshold(), EmpiricalTTL(), EmpiricalWindow()]
    print(f"c = ${C:.2e}/s (${C*3600:.4f}/h), refill R = {R:.0f} s, hot {L_HOT:.0f} s, cold {L_COLD:.0f} s; schedule runs {'known in advance (removed)' if drop else 'treated as unpredictable'}\n")
    for v in vs:
        print(f"=== value of a second of waiting v = ${v}/s; objective J = $ per day (idle machines + start-up) + v x latency per day")
        print(f"{'repo':26s} {'jobs/d':>7s} " + " ".join(f"{p.name[:15]:>15s}" for p in pols) + f" {'oracle':>10s}")
        totals = collections.defaultdict(float)
        for f in files:
            ts, sched, t0, t1 = load(f, drop)
            arr = bursts(ts)
            if len(arr) < 40:
                continue
            days = (arr[-1][0] - arr[0][0]) / 86400
            row = []
            for p in pols:
                cost, lat, jobs = simulate(arr, p, v, 2)
                j = (cost + v * lat) / days
                row.append(j); totals[p.name] += j
            oc, ol, oj = oracle(arr, v)
            oj_ = (oc + v * ol) / days; totals["oracle"] += oj_
            best = min(row)
            print(f"{os.path.basename(f)[:-4][:26]:26s} {sum(k for _, k in arr)/days:7.1f} " + " ".join(f"{x:15.3f}" + ("*" if x == best else " ") for x in row) + f" {oj_:10.3f}")
        print(f"{'TOTAL (sum over repos)':26s} {'':7s} " + " ".join(f"{totals[p.name]:15.3f} " for p in pols) + f" {totals['oracle']:10.3f}\n")

def pooled(v):
    """One pool for all repositories (an org-level runner) against one pool per repository,
    over the last 3.8 days, which every trace covers."""
    streams = {}
    for f in sorted(glob.glob(BASE + "/*.tsv")):
        ts, _, _, _ = load(f, False)
        streams[f] = ts
    res = {}
    for name, p in [("never", Never()), ("ski-rental", SkiRental()), ("empirical-ttl", EmpiricalTTL()), ("always", Always())]:
        sep = 0.0
        merged = []
        for f, ts in streams.items():
            start = ts[-1] - 3.8 * 86400
            window = [t for t in ts if t >= start]
            if len(window) < 5:
                continue
            arr = bursts(window)
            c_, l_, _ = simulate(arr, p, v, 2)
            sep += c_ + v * l_
            merged += [(t - start, k) for t, k in arr]     # all repositories in one frame
        merged.sort()
        out = {}
        for n in (2, 4, 8):
            c_, l_, _ = simulate(merged, p, v, n)
            out[n] = c_ + v * l_
        res[name] = (sep, out)
    return res


if "--pooled" in sys.argv:
    for v in (0.003, 0.01, 0.03):
        print(f"v = ${v}/s: objective over 3.8 days, 12 repositories together")
        for name, (sep, out) in pooled(v).items():
            print(f"  {name:14s} one pool per repository (2 runners each): ${sep:8.2f}   one shared pool of 2: ${out[2]:8.2f}   of 4: ${out[4]:8.2f}   of 8: ${out[8]:8.2f}")
else:
    main()
