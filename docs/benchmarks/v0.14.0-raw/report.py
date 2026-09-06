#!/usr/bin/env python3
"""report.py — integrity first, then floors, then effects. In that order, deliberately.

Nothing here attributes a delta until the floor for that same metric, measured under the
same conditions in the same rounds, has been printed.
"""
import os, sys, math, json
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse, median, spread, mannwhitney_p, compare

SP = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad'
OUT = SP + '/raw/campaign'

BLOCKS = ['exec','cy','wal','txn','search','centrality',
          'Lreadtx','Lcypar','Llpg','Lmvcc','Lprom','Lmapper','Ltxn','Lbolt']
# packages whose TEST BINARY is byte-identical between the two releases: on these,
# A-vs-B is a floor measured ACROSS the arms, not an effect.
IDENTICAL_ACROSS = {'wal','Lmvcc','Lprom','Lmapper'}

def fmt_t(v):
    """ns -> human"""
    for lim, suf, div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v) < lim: return f'{v/div:.4g}{suf}'
    return f'{v:.4g}ns'

def integrity():
    print('='*100); print('INTEGRITY — every kept line must match ^Benchmark\\S*\\s+\\d+\\s+ ; arms must match')
    print('='*100)
    print(f"{'block':<10} {'A lines':>8} {'B lines':>8} {'B2 lines':>9} {'A bad':>6} {'B bad':>6} {'B2 bad':>7} "
          f"{'benches':>8} {'n/arm':>7}  verdict")
    allok = True
    for blk in BLOCKS:
        ds, ks, bs = {}, {}, {}
        for arm in ('A','B','B2'):
            d,k,b,bl = parse(f'{OUT}/{blk}_{arm}.txt'); ds[arm],ks[arm],bs[arm]=d,k,b
        names = set(ds['A']) & set(ds['B']) & set(ds['B2'])
        ns = set()
        for n in names:
            for arm in ('A','B','B2'):
                ns.add(len(ds[arm][n].get('ns/op',[])))
        setmatch = (set(ds['A'])==set(ds['B'])==set(ds['B2']))
        ok = all(v==0 for v in bs.values()) and setmatch
        allok &= ok
        print(f"{blk:<10} {ks['A']:>8} {ks['B']:>8} {ks['B2']:>9} {bs['A']:>6} {bs['B']:>6} {bs['B2']:>7} "
              f"{len(names):>8} {sorted(ns)!s:>7}  {'OK' if ok else 'CHECK'}")
        if not setmatch:
            print(f"     only in A : {sorted(set(ds['A'])-set(ds['B']))[:5]}")
            print(f"     only in B : {sorted(set(ds['B'])-set(ds['A']))[:5]}")
    print(f"\nALL BLOCKS CLEAN: {allok}")
    return allok

def floor_table(label, pairs, unit='ns/op', thresh=0.05):
    """pairs: list of (fileA, fileB). Prints the distribution of |delta| and the extremes."""
    rows = []
    for pa, pb in pairs:
        rep = compare(pa, pb, 'x', 'y', units=(unit,))
        for r in rep['rows']:
            rows.append(r)
    if not rows:
        print(f'  {label}: NO DATA'); return None
    ds = sorted(abs(r['delta_pct']) for r in rows)
    sig = [r for r in rows if r['p'] is not None and r['p'] < thresh]
    print(f"  {label:<46} n_comparisons={len(rows):>4}  "
          f"median|Δ|={median(ds):.2f}%  p95|Δ|={ds[int(0.95*(len(ds)-1))]:.2f}%  max|Δ|={ds[-1]:.2f}%  "
          f"significant@p<{thresh}={len(sig)}")
    worst = max(rows, key=lambda r: abs(r['delta_pct']))
    print(f"      largest drift: {worst['bench']:<48} {fmt_t(worst['a_med']) if unit=='ns/op' else worst['a_med']:>12} -> "
          f"{fmt_t(worst['b_med']) if unit=='ns/op' else worst['b_med']:<12} {worst['delta_pct']:+.2f}%  p={worst['p']}")
    if sig:
        for r in sorted(sig, key=lambda r: -abs(r['delta_pct']))[:6]:
            print(f"      SIGNIFICANT  {r['bench']:<46} {r['delta_pct']:+7.2f}%  p={r['p']:.4f}  "
                  f"spreadA=±{r['a_spread']:.1f}% spreadB=±{r['b_spread']:.1f}%")
    return {'n':len(rows),'median':median(ds),'p95':ds[int(0.95*(len(ds)-1))],'max':ds[-1],'nsig':len(sig)}

def effects(blk, unit='ns/op', floor=None, top=None):
    rep = compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt', 'A','B', units=(unit,))
    rows = [r for r in rep['rows']]
    rows.sort(key=lambda r: r['delta_pct'])
    print(f"\n--- {blk} : {unit} : A(v0.13.0) -> B(v0.14.0)  [{len(rows)} benchmarks]"
          + (f"  floor=±{floor:.2f}%" if floor else "") + " ---")
    hdr=f"{'benchmark':<52} {'v0.13.0':>12} {'v0.14.0':>12} {'delta':>9} {'p':>7} {'sprA':>6} {'sprB':>6}  verdict"
    print(hdr)
    for r in (rows if top is None else rows[:top]+rows[-top:]):
        sigp = r['p'] is not None and r['p'] < 0.05
        big  = floor is None or abs(r['delta_pct']) > floor
        verdict = 'CHANGE' if (sigp and big) else ('within noise' if not big else ('n.s.' if not sigp else ''))
        a = fmt_t(r['a_med']) if unit=='ns/op' else f"{r['a_med']:.0f}"
        b = fmt_t(r['b_med']) if unit=='ns/op' else f"{r['b_med']:.0f}"
        print(f"{r['bench']:<52} {a:>12} {b:>12} {r['delta_pct']:>+8.2f}% "
              f"{(f'{r[chr(112)]:.4f}' if r['p'] is not None else '  n/a'):>7} "
              f"±{r['a_spread']:>4.1f}% ±{r['b_spread']:>4.1f}%  {verdict}")
    return rows

if __name__ == '__main__':
    mode = sys.argv[1] if len(sys.argv)>1 else 'all'
    if mode in ('all','integrity'):
        integrity()
    if mode in ('all','floor'):
        print('\n'+'='*100)
        print('FLOOR 1 — B vs B2: the SAME BYTE-IDENTICAL BINARY against itself,')
        print('          interleaved in the SAME rounds, under the SAME load, as the signal.')
        print('='*100)
        for u in ('ns/op','allocs/op'):
            print(f'\n  [{u}]')
            floor_table('all blocks pooled', [(f'{OUT}/{b}_B.txt', f'{OUT}/{b}_B2.txt') for b in BLOCKS], unit=u)
        print('\n'+'='*100)
        print('FLOOR 2 — A vs B on the four packages whose TEST BINARY is byte-identical')
        print('          BETWEEN THE RELEASES. Measured ACROSS the arms, so it catches what')
        print('          a within-arm floor structurally cannot.')
        print('='*100)
        for u in ('ns/op','allocs/op'):
            print(f'\n  [{u}]')
            floor_table('identical-binary packages', [(f'{OUT}/{b}_A.txt', f'{OUT}/{b}_B.txt') for b in sorted(IDENTICAL_ACROSS)], unit=u)
