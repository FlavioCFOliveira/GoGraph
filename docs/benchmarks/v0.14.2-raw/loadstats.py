#!/usr/bin/env python3
"""loadstats.py — slice the campaign-long load samples by each invocation's own
T0/T1, so the report can state the load DURING a timing rather than around it.

A before/after bracket cannot see a spike that begins and ends inside an
invocation; this project has already had an interleaved A/B contaminated by
exactly such a spike, so the sampler runs for the whole campaign and the slicing
happens here.
"""
import sys, re
from collections import defaultdict
OUT = sys.argv[1] if len(sys.argv) > 1 else '.'
samples = []
for line in open(f'{OUT}/loadsamples.txt'):
    p = line.split()
    if len(p) >= 2:
        try: samples.append((float(p[0]), float(p[1])))
        except ValueError: pass
samples.sort()
rx = re.compile(r'round=(\d+) block=(\S+) arm=(\S+) EXIT=(\d+) SECONDS=(\d+) T0=([\d.]+) T1=([\d.]+)')
per_block = defaultdict(list); per_round = defaultdict(list); allv = []
rows = []
for line in open(f'{OUT}/exits.log'):
    m = rx.search(line)
    if not m: continue
    rnd, blk, arm, ec, secs, t0, t1 = m.groups()
    t0, t1 = float(t0), float(t1)
    vals = [v for t, v in samples if t0 <= t <= t1]
    if not vals: vals = [float('nan')]
    rows.append((int(rnd), blk, arm, int(ec), int(secs), min(vals), max(vals), sum(vals)/len(vals), len(vals)))
    per_block[blk] += vals; per_round[int(rnd)] += vals; allv += vals
print(f"load samples total: {len(samples)}")
print(f"invocations       : {len(rows)}   non-zero exits: {sum(1 for r in rows if r[3] != 0)}")
print()
print(f"{'block':<12}{'invocations':>12}{'min':>8}{'max':>8}{'mean':>8}")
for blk in sorted(per_block, key=lambda b: sum(per_block[b])/len(per_block[b])):
    v = per_block[blk]
    n = sum(1 for r in rows if r[1] == blk)
    print(f"{blk:<12}{n:>12}{min(v):>8.2f}{max(v):>8.2f}{sum(v)/len(v):>8.2f}")
print(f"{'CAMPAIGN':<12}{len(rows):>12}{min(allv):>8.2f}{max(allv):>8.2f}{sum(allv)/len(allv):>8.2f}")
print()
print(f"{'round':<8}{'min':>8}{'max':>8}{'mean':>8}")
for r in sorted(per_round):
    v = per_round[r]
    print(f"{r:<8}{min(v):>8.2f}{max(v):>8.2f}{sum(v)/len(v):>8.2f}")
print()
print("per-invocation detail")
print(f"{'rnd':<5}{'block':<12}{'arm':<5}{'exit':>5}{'secs':>7}{'load min':>10}{'load max':>10}{'load mean':>11}{'samples':>9}")
for r in sorted(rows):
    print(f"{r[0]:<5}{r[1]:<12}{r[2]:<5}{r[3]:>5}{r[4]:>7}{r[5]:>10.2f}{r[6]:>10.2f}{r[7]:>11.2f}{r[8]:>9}")
