#!/usr/bin/env python3
"""report.py — integrity first, then floors, then effects. In that order, deliberately.

Nothing here attributes a delta until the floor for that same metric, measured under
the same conditions in the same rounds, has been printed.

  integrity   every kept line matches ^Benchmark\\S*\\s+\\d+\\s+ ; arms match; n=R exactly
  floor       Floor 1 = B vs B2 (byte-identical binary, same rounds)
              Floor 2 = A vs B on the packages whose test binary is byte-identical
                        BETWEEN the releases -- a cross-arm floor
  bands       both floors stratified by the magnitude of the benchmark itself,
              because a 6% drift means one thing at 1 ms and another at 2 ns
  adjudicate  every p<0.05 row against its own band's same-code envelope
  effects     per-block tables
"""
import os, sys, math
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse, median, spread, mannwhitney_p, compare

SP  = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad'
OUT = SP + '/raw/campaign'

BLOCKS = ['exec','cy','idx','btree','hash','lpg','txn','rec','snap','wal','ckpt','bolt',
          'search','centrality','cnt',
          'Lreadtx','Lcypar','Llpg','Lmvcc','Lprom','Lmapper','Ltxn','Lbolt']
BB2_ONLY = {'ckpt'}                                  # no A arm: the benchmarks are new
IDENTICAL_ACROSS = {'cnt','Lmvcc','Lprom','Lmapper'} # A==B test binary by sha256

BANDS = [(0, 100, '< 100 ns'), (100, 1e4, '100 ns - 10 us'),
         (1e4, 1e6, '10 us - 1 ms'), (1e6, float('inf'), '> 1 ms')]

def band(ns):
    for lo, hi, name in BANDS:
        if lo <= ns < hi: return name
    return '> 1 ms'

def fmt_t(v):
    for lim, suf, div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v) < lim: return f'{v/div:.4g}{suf}'
    return f'{v:.4g}ns'

def rows_for(pairs, unit):
    out = []
    for pa, pb in pairs:
        for r in compare(pa, pb, 'x', 'y', units=(unit,))['rows']:
            out.append(r)
    return out

def integrity(rounds):
    print('='*104)
    print(r'INTEGRITY — every kept line must match ^Benchmark\S*\s+\d+\s+ ; arms must match; n must equal rounds')
    print('='*104)
    print(f"{'block':<11}{'A lines':>8}{'B lines':>8}{'B2 lines':>9}{'A bad':>6}{'B bad':>6}{'B2 bad':>7}"
          f"{'benches':>9}{'n/arm':>10}  verdict")
    allok = True; totals = defaultdict(int); nbad = 0
    for blk in BLOCKS:
        arms = ('B','B2') if blk in BB2_ONLY else ('A','B','B2')
        ds, ks, bs = {}, {}, {}
        for arm in ('A','B','B2'):
            d,k,b,bl = parse(f'{OUT}/{blk}_{arm}.txt'); ds[arm],ks[arm],bs[arm]=d,k,b
            if arm in arms: totals[arm]+=k; nbad += b
        names = set.intersection(*[set(ds[a]) for a in arms])
        ns = sorted({len(ds[a][n].get('ns/op',[])) for n in names for a in arms})
        setmatch = len({frozenset(ds[a]) for a in arms}) == 1
        ok = all(bs[a]==0 for a in arms) and setmatch and ns == [rounds]
        allok &= ok
        print(f"{blk:<11}{ks['A']:>8}{ks['B']:>8}{ks['B2']:>9}{bs['A']:>6}{bs['B']:>6}{bs['B2']:>7}"
              f"{len(names):>9}{str(ns):>10}  {'OK' if ok else 'CHECK'}")
        if not setmatch:
            a0 = arms[0]
            for a in arms[1:]:
                print(f"     only in {a0}: {sorted(set(ds[a0])-set(ds[a]))[:6]}")
                print(f"     only in {a }: {sorted(set(ds[a])-set(ds[a0]))[:6]}")
    print(f"\nresult lines kept: " + ', '.join(f'{a}={n}' for a,n in sorted(totals.items())))
    print(f"malformed lines: {nbad}")
    print(f"ALL BLOCKS CLEAN: {allok}")
    return allok

def floor_summary(label, pairs, unit):
    rows = rows_for(pairs, unit)
    if not rows:
        print(f'  {label}: NO DATA'); return {}
    ds = sorted(abs(r['delta_pct']) for r in rows)
    sig = [r for r in rows if r['p'] is not None and r['p'] < 0.05]
    p95 = ds[int(0.95*(len(ds)-1))]
    print(f"  {label:<44} n={len(rows):>4}  median|Δ|={median(ds):6.2f}%  "
          f"p95|Δ|={p95:6.2f}%  max|Δ|={ds[-1]:6.2f}%  sig@p<0.05={len(sig)}")
    worst = max(rows, key=lambda r: abs(r['delta_pct']))
    aa = fmt_t(worst['a_med']) if unit=='ns/op' else f"{worst['a_med']:.2f}"
    bb = fmt_t(worst['b_med']) if unit=='ns/op' else f"{worst['b_med']:.2f}"
    print(f"      largest drift : {worst['bench']:<50} {aa:>11} -> {bb:<11} "
          f"{worst['delta_pct']:+.2f}%  p={worst['p']}")
    for r in sorted(sig, key=lambda r: -abs(r['delta_pct']))[:8]:
        print(f"      SIGNIFICANT   {r['bench']:<50} {r['delta_pct']:+7.2f}%  p={r['p']:.4f}  "
              f"sprA=±{r['a_spread']:.1f}% sprB=±{r['b_spread']:.1f}%")
    # per band
    per = defaultdict(list)
    for r in rows: per[band(r['a_med'] if unit=='ns/op' else 1e6)].append(r)
    out = {}
    print(f"      {'band':<18}{'n':>5}{'median|Δ|':>11}{'p95|Δ|':>9}{'max|Δ|':>9}{'worst sig':>11}")
    for _,_,name in BANDS:
        rr = per.get(name, [])
        if not rr: continue
        dd = sorted(abs(x['delta_pct']) for x in rr)
        ss = [abs(x['delta_pct']) for x in rr if x['p'] is not None and x['p'] < 0.05]
        p95b = dd[int(0.95*(len(dd)-1))]
        out[name] = {'n':len(rr),'median':median(dd),'p95':p95b,'max':dd[-1],
                     'worst_sig':max(ss) if ss else 0.0}
        print(f"      {name:<18}{len(rr):>5}{median(dd):>10.2f}%{p95b:>8.2f}%{dd[-1]:>8.2f}%"
              f"{(max(ss) if ss else 0.0):>10.2f}%")
    return out

def floors(unit):
    print('\n'+'='*104)
    print('FLOOR 1 — B vs B2: the SAME BYTE-IDENTICAL BINARY against itself, interleaved')
    print('          in the SAME rounds, under the SAME load, as the signal it calibrates.')
    print('='*104); print(f'  [{unit}]')
    f1 = floor_summary('all blocks pooled', [(f'{OUT}/{b}_B.txt', f'{OUT}/{b}_B2.txt') for b in BLOCKS], unit)
    print('\n'+'='*104)
    print('FLOOR 2 — A vs B on the packages whose TEST BINARY is byte-identical BETWEEN')
    print('          the releases. Measured ACROSS the arms, so it catches what a')
    print('          within-arm floor structurally cannot. Every number is noise by')
    print('          construction.')
    print('='*104); print(f'  [{unit}]')
    f2 = floor_summary('identical-binary packages', [(f'{OUT}/{b}_A.txt', f'{OUT}/{b}_B.txt') for b in sorted(IDENTICAL_ACROSS)], unit)
    return f1, f2

def envelopes(f1, f2):
    env = {}
    for _,_,name in BANDS:
        cands = []
        for f in (f1, f2):
            if name in f:
                cands += [f[name]['p95'], f[name]['worst_sig']]
        env[name] = max(cands) if cands else 0.0
    print('\nADJUDICATION BAR per magnitude band = max(p95 same-code drift, worst same-code')
    print('drift that reached p<0.05), taken across BOTH floors:')
    for _,_,name in BANDS:
        print(f'   {name:<18} {env[name]:6.2f}%')
    return env

def adjudicate(env, unit='ns/op'):
    print('\n'+'='*104)
    print(f'ADJUDICATION — every p<0.05 row in an EFFECT block, against its own band bar [{unit}]')
    print('='*104)
    print(f"{'block':<10}{'benchmark':<50}{'A':>11}{'B':>11}{'delta':>9}{'p':>8}{'band':<18}{'bar':>7}  verdict")
    findings, rejected = [], []
    for blk in BLOCKS:
        if blk in BB2_ONLY or blk in IDENTICAL_ACROSS: continue
        for r in compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=(unit,))['rows']:
            if r['p'] is None or r['p'] >= 0.05: continue
            bn = band(r['a_med'] if unit=='ns/op' else 1e6)
            bar = env[bn]
            keep = abs(r['delta_pct']) > bar
            (findings if keep else rejected).append((blk,r,bn,bar))
    for blk,r,bn,bar in sorted(findings+rejected, key=lambda t:-abs(t[1]['delta_pct'])):
        aa = fmt_t(r['a_med']) if unit=='ns/op' else f"{r['a_med']:.0f}"
        bb = fmt_t(r['b_med']) if unit=='ns/op' else f"{r['b_med']:.0f}"
        v = 'FINDING' if abs(r['delta_pct'])>bar else 'rejected as noise'
        print(f"{blk:<10}{r['bench']:<50}{aa:>11}{bb:>11}{r['delta_pct']:>+8.2f}%{r['p']:>8.3f}{bn:<18}{bar:>6.2f}%  {v}")
    print(f"\nrows reaching p<0.05: {len(findings)+len(rejected)}   FINDINGS: {len(findings)}   rejected: {len(rejected)}")
    return findings

def effects(blk, unit='ns/op', bar=None):
    rep = compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=(unit,))
    rows = sorted(rep['rows'], key=lambda r: r['delta_pct'])
    print(f"\n--- {blk} : {unit} : A(v0.14.0) -> B(v0.14.1)  [{len(rows)} rows] ---")
    print(f"{'benchmark':<52}{'v0.14.0':>12}{'v0.14.1':>12}{'delta':>9}{'p':>8}{'sprA':>7}{'sprB':>7}")
    geo = []
    for r in rows:
        a = fmt_t(r['a_med']) if unit=='ns/op' else f"{r['a_med']:.0f}"
        b = fmt_t(r['b_med']) if unit=='ns/op' else f"{r['b_med']:.0f}"
        pp = f"{r['p']:.3f}" if r['p'] is not None else '  n/a'
        print(f"{r['bench']:<52}{a:>12}{b:>12}{r['delta_pct']:>+8.2f}%{pp:>8}"
              f"  ±{r['a_spread']:>4.1f}%  ±{r['b_spread']:>4.1f}%")
        if r['a_med']>0 and r['b_med']>0: geo.append(r['b_med']/r['a_med'])
    if geo:
        g = math.exp(sum(math.log(x) for x in geo)/len(geo))
        print(f"{'GEOMEAN':<52}{'':>12}{'':>12}{(g-1)*100:>+8.2f}%")
    if rep['only_a']: print(f"   only in A: {rep['only_a']}")
    if rep['only_b']: print(f"   only in B: {rep['only_b']}")
    return rows

def floorblock(blk, unit='ns/op'):
    rep = compare(f'{OUT}/{blk}_B.txt', f'{OUT}/{blk}_B2.txt','B','B2',units=(unit,))
    rows = sorted(rep['rows'], key=lambda r: r['delta_pct'])
    geo = [r['b_med']/r['a_med'] for r in rows if r['a_med']>0 and r['b_med']>0]
    g = math.exp(sum(math.log(x) for x in geo)/len(geo)) if geo else 1.0
    print(f"--- {blk} FLOOR (B vs B2, identical binary) [{unit}] geomean={(g-1)*100:+.2f}%  n={len(rows)}")
    return (g-1)*100

if __name__ == '__main__':
    mode = sys.argv[1] if len(sys.argv)>1 else 'all'
    R = int(os.environ.get('ROUNDS','6'))
    if mode in ('all','integrity'): integrity(R)
    if mode in ('all','floor','adjudicate'):
        f1t, f2t = floors('ns/op')
        f1a, f2a = floors('allocs/op')
        env = envelopes(f1t, f2t)
        if mode in ('all','adjudicate'):
            adjudicate(env, 'ns/op')
    if mode == 'effects':
        for blk in sys.argv[2:] or BLOCKS:
            effects(blk); floorblock(blk)
    if mode == 'allocs':
        for blk in sys.argv[2:] or BLOCKS:
            effects(blk, 'allocs/op'); floorblock(blk, 'allocs/op')
