#!/usr/bin/env python3
"""adjudicate.py — the final verdict, calibrated on code that PROVABLY did not change.

The campaign was stopped after 1 complete round + part of a second, so no p-value
exists. The criterion available at n=2 is range disjointness: arm A's whole sample
range lying outside the pooled range of B and B2, which are the SAME BINARY.

That criterion needs calibrating, and this campaign can calibrate it EXACTLY,
because three packages -- search, search/centrality, graph/index/count -- compile
to test binaries that are BYTE-IDENTICAL between v0.14.1 and v0.14.2 (sha256).
Every A-vs-B row they produce is a comparison of a binary against ITSELF across the
arms. Any "separation" they show is a FALSE POSITIVE, by construction.

So: measure the false-positive rate and the worst false-positive magnitude on those
packages, then require a row in a CHANGED package to exceed both before it is called
anything. Nothing else is called a finding; everything in between is reported as
NOT ADJUDICABLE AT THIS POWER, which is what it is.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse
from report import LIMIT, BLOCKS, fmt_t
SP  = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/268e8af2-f36a-44f1-a476-9d616baa960f/scratchpad'
OUT = SP + '/raw/campaign'
IDENTICAL = ['search','centrality','cnt']
CHANGED   = ['harness','exec','cy','bolt']

def samples(blk, arm, unit):
    d,_,_,_ = parse(f'{OUT}/{blk}_{arm}.txt'); k = LIMIT[blk]
    return {n: d[n][unit][:k] for n in d if unit in d[n]}

def scan(blocks, unit):
    out = []
    for blk in blocks:
        A,B,B2 = (samples(blk,a,unit) for a in ('A','B','B2'))
        for n in sorted(set(A)&set(B)&set(B2)):
            a,b,b2 = A[n],B[n],B2[n]
            if not a or not b or not b2: continue
            pool = b+b2; lo,hi = min(pool),max(pool)
            ma = sum(a)/len(a); mp = sum(pool)/len(pool)
            if ma == 0: continue
            d = (mp-ma)/ma*100
            const = len(set(a))==1 and len(set(pool))==1
            if const:
                out.append({'blk':blk,'b':n,'a':ma,'p':mp,'d':d,'gap':0.0,
                            'kind':'constant-differs' if a[0]!=pool[0] else 'constant-equal'})
            elif max(a) < lo:
                out.append({'blk':blk,'b':n,'a':ma,'p':mp,'d':d,'gap':(lo-max(a))/ma*100,'kind':'disjoint'})
            elif min(a) > hi:
                out.append({'blk':blk,'b':n,'a':ma,'p':mp,'d':d,'gap':(min(a)-hi)/ma*100,'kind':'disjoint'})
            else:
                out.append({'blk':blk,'b':n,'a':ma,'p':mp,'d':d,'gap':0.0,'kind':'overlap'})
    return out

def run(unit):
    print('='*118)
    print(f'ADJUDICATION [{unit}] — calibrated on byte-identical binaries')
    print('='*118)
    cal = scan(IDENTICAL, unit)
    fp  = [r for r in cal if r['kind'] == 'disjoint']
    cd  = [r for r in cal if r['kind'] == 'constant-differs']
    print(f"\nCALIBRATION — {len(cal)} rows on packages whose test binary is byte-identical between")
    print(f"the releases (search, search/centrality, graph/index/count). Every one is the SAME CODE.")
    print(f"  rows                      : {len(cal)}")
    print(f"  FALSE 'separations'       : {len(fp)}  ({100*len(fp)/max(1,len(cal)):.0f}% of rows)")
    print(f"  differing 'constants'     : {len(cd)}   <- must be 0, or the constant test is unsound")
    dmax = max((abs(r['d']) for r in fp), default=0.0)
    gmax = max((r['gap'] for r in fp), default=0.0)
    for r in sorted(fp, key=lambda r: -abs(r['d'])):
        print(f"    {r['blk']}/{r['b']:<46}{r['d']:>+8.2f}%  gap {r['gap']:>5.2f}%")
    print(f"\n  BAR = the worst FALSE positive this campaign produced on unchanged code:")
    print(f"        |Δ| > {dmax:.2f}%  AND  gap > {gmax:.2f}%")

    rows = scan(CHANGED, unit)
    keep = [r for r in rows if r['kind'] == 'constant-differs'
            or (r['kind'] == 'disjoint' and abs(r['d']) > dmax and r['gap'] > gmax)]
    grey = [r for r in rows if r['kind'] == 'disjoint' and r not in keep]
    print(f"\nRESULT over {len(rows)} rows in the four changed-binary blocks")
    print(f"  clears the calibration bar          : {len([r for r in keep if r['kind']=='disjoint'])}")
    print(f"  differing per-operation CONSTANTS   : {len([r for r in keep if r['kind']=='constant-differs'])}")
    print(f"  separates but does NOT clear the bar: {len(grey)}   -> NOT ADJUDICABLE at this power")
    print(f"  overlapping ranges                  : {len([r for r in rows if r['kind']=='overlap'])}")
    if keep:
        print(f"\n  {'block':<10}{'benchmark':<46}{'v0.14.1':>12}{'v0.14.2':>12}{'Δ':>9}{'gap':>8}  verdict")
        for r in sorted(keep, key=lambda r: -abs(r['d'])):
            fa = fmt_t(r['a']) if unit=='ns/op' else f"{r['a']:.0f}"
            fb = fmt_t(r['p']) if unit=='ns/op' else f"{r['p']:.0f}"
            v = 'CONSTANT DIFFERS (power-independent)' if r['kind']=='constant-differs' else 'SEPARATION clears calibration'
            print(f"  {r['blk']:<10}{r['b']:<46}{fa:>12}{fb:>12}{r['d']:>+8.2f}%{r['gap']:>7.2f}%  {v}")
    if grey:
        print(f"\n  NOT ADJUDICABLE at this power (separated, but inside the false-positive envelope):")
        for r in sorted(grey, key=lambda r: -abs(r['d']))[:14]:
            fa = fmt_t(r['a']) if unit=='ns/op' else f"{r['a']:.0f}"
            fb = fmt_t(r['p']) if unit=='ns/op' else f"{r['p']:.0f}"
            print(f"  {r['blk']:<10}{r['b']:<46}{fa:>12}{fb:>12}{r['d']:>+8.2f}%{r['gap']:>7.2f}%")
        if len(grey) > 14: print(f"  ... and {len(grey)-14} more")

for u in ('ns/op','allocs/op','B/op'):
    run(u); print()
