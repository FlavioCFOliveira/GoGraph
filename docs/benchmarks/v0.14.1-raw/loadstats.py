#!/usr/bin/env python3
"""loadstats.py — per-invocation load distribution, sliced from the campaign sampler.

Reads exits.log's `T0=<epoch> T1=<epoch>` fields and loadsamples.txt, and prints one
row per invocation: n samples, min, max, mean of the 1-minute load average DURING
that invocation. A verdict published without these has no thresholds behind it, and
docs/test-layers.md already excludes a figure for that omission.

Usage: loadstats.py <exits.log> <loadsamples.txt> [--per-block]
"""
import sys, re, statistics
from collections import defaultdict

exits, samples = sys.argv[1], sys.argv[2]
per_block = '--per-block' in sys.argv

pts = []
for ln in open(samples):
    f = ln.split()
    if len(f) >= 2:
        try: pts.append((float(f[0]), float(f[1])))
        except ValueError: pass
pts.sort()

rows = []
pat = re.compile(r'round=(\d+) block=(\S+) arm=(\S+) EXIT=(\d+) SECONDS=(\d+) T0=([\d.]+) T1=([\d.]+)')
for ln in open(exits):
    m = pat.search(ln)
    if not m: continue
    r, blk, arm, ec, secs, t0, t1 = m.groups()
    t0, t1 = float(t0), float(t1)
    win = [v for t, v in pts if t0 <= t <= t1]
    rows.append(dict(round=int(r), block=blk, arm=arm, exit=int(ec), secs=int(secs),
                     n=len(win), lo=min(win) if win else None,
                     hi=max(win) if win else None,
                     mean=statistics.fmean(win) if win else None))

if per_block:
    agg = defaultdict(list)
    for x in rows:
        if x['n']: agg[x['block']] += [x['lo'], x['hi'], x['mean']]
    print(f"{'block':<12}{'invocations':>12}{'min':>8}{'max':>8}{'mean':>8}")
    for blk in sorted(agg):
        inv = [x for x in rows if x['block'] == blk and x['n']]
        los = [x['lo'] for x in inv]; his = [x['hi'] for x in inv]; mns = [x['mean'] for x in inv]
        print(f"{blk:<12}{len(inv):>12}{min(los):>8.2f}{max(his):>8.2f}{statistics.fmean(mns):>8.2f}")
    allv = [v for x in rows if x['n'] for v in (x['lo'], x['hi'])]
    mns  = [x['mean'] for x in rows if x['n']]
    print(f"\n{'CAMPAIGN':<12}{len(rows):>12}{min(allv):>8.2f}{max(allv):>8.2f}{statistics.fmean(mns):>8.2f}")
    unsampled = [x for x in rows if not x['n']]
    if unsampled:
        print(f"\nWARNING: {len(unsampled)} invocation(s) carry NO load sample — their figures have no conditions.")
else:
    print(f"{'rnd':>4}{'block':<12}{'arm':<5}{'exit':>5}{'secs':>6}{'n':>5}{'min':>8}{'max':>8}{'mean':>8}")
    for x in rows:
        lo = f"{x['lo']:.2f}" if x['n'] else '-'
        hi = f"{x['hi']:.2f}" if x['n'] else '-'
        mn = f"{x['mean']:.2f}" if x['n'] else '-'
        print(f"{x['round']:>4}{x['block']:<12}{x['arm']:<5}{x['exit']:>5}{x['secs']:>6}{x['n']:>5}{lo:>8}{hi:>8}{mn:>8}")
