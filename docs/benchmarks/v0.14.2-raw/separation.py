#!/usr/bin/env python3
"""separation.py — the adjudication this campaign's power actually supports.

At n=2 per arm no p-value exists. What DOES exist is a non-parametric statement
that needs no distribution at all:

    B and B2 are the SAME BINARY. Their samples pooled are a direct, in-campaign
    observation of how much this host moves a given benchmark WITHOUT any code
    change, measured in the same rounds and under the same load. If arm A's entire
    sample range lies OUTSIDE that pooled same-code range, the release moved that
    benchmark by more than the same code moved itself.

That is the criterion used below: DISJOINT means max(A) < min(B u B2) or
min(A) > max(B u B2). It is deliberately conservative -- with two samples an arm
it can be satisfied by chance -- so it is reported as a SEPARATION, never as a
p-value, and the gap between the ranges is printed so a reader can judge it.

A metric whose every sample is equal within each arm is a per-operation CONSTANT,
not a statistic; when the two constants differ the difference is real at any n,
and that is reported separately and more strongly.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse
from report import LIMIT, BLOCKS, fmt_t

SP  = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/268e8af2-f36a-44f1-a476-9d616baa960f/scratchpad'
OUT = SP + '/raw/campaign'

def samples(blk, arm, unit):
    d, _, _, _ = parse(f'{OUT}/{blk}_{arm}.txt')
    k = LIMIT[blk]
    return {n: d[n][unit][:k] for n in d if unit in d[n]}

def go(unit, blocks):
    print('='*118)
    print(f'SEPARATION TEST [{unit}] — arm A range vs the pooled same-code range of B u B2')
    print('='*118)
    tot = sep = con = 0
    for blk in blocks:
        A, B, B2 = (samples(blk, a, unit) for a in ('A','B','B2'))
        names = sorted(set(A) & set(B) & set(B2))
        hits = []
        for n in names:
            a, b, b2 = A[n], B[n], B2[n]
            if not a or not b or not b2: continue
            tot += 1
            pool = b + b2
            lo, hi = min(pool), max(pool)
            amin, amax = min(a), max(a)
            const_a, const_p = len(set(a)) == 1, len(set(pool)) == 1
            ma = sum(a)/len(a); mp = sum(pool)/len(pool)
            delta = (mp-ma)/ma*100 if ma else 0.0
            if const_a and const_p:
                if a[0] != pool[0]:
                    hits.append((n, 'CONSTANT DIFFERS', a[0], pool[0], delta, 0.0)); con += 1
                continue
            if amax < lo:
                hits.append((n, 'SEPARATION (slower)', ma, mp, delta, (lo-amax)/ma*100)); sep += 1
            elif amin > hi:
                hits.append((n, 'SEPARATION (faster)', ma, mp, delta, (amin-hi)/ma*100)); sep += 1
        if not hits: continue
        print(f"\n  {blk} — {len(hits)} of {len(names)} rows separate or differ as a constant")
        print(f"    {'benchmark':<44}{'v0.14.1':>12}{'v0.14.2':>12}{'Δ':>9}{'gap':>8}  verdict")
        for n, v, ma, mp, d, gap in sorted(hits, key=lambda h: -abs(h[4])):
            fa = fmt_t(ma) if unit == 'ns/op' else f"{ma:.0f}"
            fb = fmt_t(mp) if unit == 'ns/op' else f"{mp:.0f}"
            print(f"    {n:<44}{fa:>12}{fb:>12}{d:>+8.2f}%{gap:>7.2f}%  {v}")
    print(f"\n  rows compared: {tot}   range-disjoint separations: {sep}   differing constants: {con}")

if __name__ == '__main__':
    unit = sys.argv[1] if len(sys.argv) > 1 else 'ns/op'
    go(unit, BLOCKS)
