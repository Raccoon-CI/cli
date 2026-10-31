import glob, os, statistics, datetime, collections

base = "/tmp/claude-1000/-data-workspace-raccoonci/939c54fa-2146-5c42-b12b-5d51edacc123/scratchpad/traces"

def load(path):
    ts = []
    for line in open(path):
        p = line.rstrip("\n").split("\t")
        if not p or not p[0]:
            continue
        ts.append(datetime.datetime.strptime(p[0], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp())
    ts.sort()
    return ts

def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))]

print(f"{'repo':28s} {'runs':>5s} {'days':>6s} {'runs/h':>7s} {'bursts/h':>8s} {'run/burst':>9s} | burst gap: {'p10':>6s} {'p50':>7s} {'p90':>8s} {'p99':>8s}   (seconds, bursts = runs within 30 s merged)")
for f in sorted(glob.glob(base + "/*.tsv")):
    ts = load(f)
    if len(ts) < 50:
        continue
    span = ts[-1] - ts[0]
    # A push starts several workflows at once: merge arrivals within 30 s into one burst.
    bursts = [[ts[0]]]
    for t in ts[1:]:
        if t - bursts[-1][-1] <= 30:
            bursts[-1].append(t)
        else:
            bursts.append([t])
    starts = [b[0] for b in bursts]
    gaps = [b - a for a, b in zip(starts, starts[1:])]
    print(f"{os.path.basename(f)[:-4]:28s} {len(ts):5d} {span/86400:6.1f} {len(ts)/(span/3600):7.2f} {len(bursts)/(span/3600):8.2f} {len(ts)/len(bursts):9.1f} |            {pct(gaps,.1):6.0f} {pct(gaps,.5):7.0f} {pct(gaps,.9):8.0f} {pct(gaps,.99):8.0f}")
