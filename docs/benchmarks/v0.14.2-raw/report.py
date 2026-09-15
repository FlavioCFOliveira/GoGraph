#!/usr/bin/env python3
"""report.py — the v0.14.2 analysis, written for the power the campaign ACTUALLY has.

The campaign was stopped by decision after round 1 completed and round 2 reached
the `search` block, so n is 2 per arm for four blocks and 1 per arm for three.
At n<4 a Mann-Whitney U has no usable distribution, so NO p-VALUE IS COMPUTED
ANYWHERE in this report. Saying "p" at n=2 would be worse than saying nothing.

What is computed instead:

  integrity   every kept line matches ^Benchmark\\S*\\s+\\d+\\s+ ; arms carry the
              same benchmark set; the per-arm sample count is printed, per block,
              and every block is truncated to the rounds in which ALL THREE arms
              ran, so no arm is compared against a round its sibling never saw.
  floor       Floor 1 = B vs B2, byte-identical binaries, same rounds, same load.
              Floor 2 = A vs B on the packages whose test binary is byte-identical
                        BETWEEN the releases.
              Both are reported as a distribution of |Δ| with n stated. At this
              power they are a SCREENING envelope, not the v0.14.1 adjudication bar.
  consistency for a two-round block, whether a row's delta keeps its SIGN in both
              rounds. A delta that flips sign between two rounds is noise however
              large it is; one that holds sign in both AND clears the floor is the
              strongest statement this campaign can make.
  constants   allocs/op and B/op are usually per-operation CONSTANTS, not statistics.
              When every sample in both arms is equal and the two constants differ,
              that is a real difference at ANY n -- it does not depend on power.
"""
import os, sys, math
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse, median

SP  = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/268e8af2-f36a-44f1-a476-9d616baa960f/scratchpad'
OUT = SP + '/raw/campaign'

# rounds in which ALL THREE arms of the block completed (from exits.log)
LIMIT = {'harness':2,'exec':2,'cy':2,'bolt':2,'search':1,'centrality':1,'cnt':1}
BLOCKS = ['harness','exec','cy','bolt','search','centrality','cnt']
IDENTICAL_ACROSS = {'search','centrality','cnt'}   # A==B test binary by sha256
CHANGED_BINARY   = {'harness','exec','cy','bolt'}

BANDS = [(0,100,'< 100 ns'),(100,1e4,'100 ns - 10 us'),(1e4,1e6,'10 us - 1 ms'),(1e6,float('inf'),'> 1 ms')]
def band(ns):
    for lo,hi,name in BANDS:
        if lo <= ns < hi: return name
    return '> 1 ms'

def fmt_t(v):
    for lim,suf,div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v) < lim: return f'{v/div:.4g}{suf}'
    return f'{v:.4g}ns'

_cache = {}
def load(blk, arm, unit):
    """Parsed samples, truncated to the block's complete-round count."""
    key = (blk, arm)
    if key not in _cache:
        _cache[key] = parse(f'{OUT}/{blk}_{arm}.txt')
    d, kept, bad, badlines = _cache[key]
    k = LIMIT[blk]
    return {n: d[n][unit][:k] for n in d if unit in d[n]}, kept, bad, badlines

def rows(blk, arm_a, arm_b, unit):
    da, _, _, _ = load(blk, arm_a, unit)
    db, _, _, _ = load(blk, arm_b, unit)
    out = []
    for n in sorted(set(da) & set(db)):
        xa, xb = da[n], db[n]
        if not xa or not xb: continue
        ma, mb = median(xa), median(xb)
        if ma == 0: continue
        per = [(b-a)/a*100 for a, b in zip(xa, xb)] if len(xa) == len(xb) else []
        out.append({'bench':n,'a':xa,'b':xb,'a_med':ma,'b_med':mb,
                    'delta':(mb-ma)/ma*100,'per_round':per,
                    'a_const':len(set(xa))==1,'b_const':len(set(xb))==1})
    return out

def integrity():
    print('='*112)
    print(r'INTEGRITY — every kept line must match ^Benchmark\S*\s+\d+\s+ ; arms must carry the same benchmark set')
    print('='*112)
    print(f"{'block':<12}{'rounds used':>12}{'A lines':>9}{'B lines':>9}{'B2 lines':>10}"
          f"{'malformed':>11}{'benches':>9}{'n/arm after truncation':>24}  verdict")
    allok = True; tot = defaultdict(int); badtot = 0
    for blk in BLOCKS:
        ds, ks, bs = {}, {}, {}
        for arm in ('A','B','B2'):
            d, k, b, _ = load(blk, arm, 'ns/op'); ds[arm], ks[arm], bs[arm] = d, k, b
            tot[arm] += k; badtot += b
        names = set.intersection(*[set(ds[a]) for a in ('A','B','B2')])
        ns = sorted({len(ds[a][n]) for n in names for a in ('A','B','B2')})
        setmatch = len({frozenset(ds[a]) for a in ('A','B','B2')}) == 1
        ok = all(bs[a] == 0 for a in ('A','B','B2')) and setmatch and ns == [LIMIT[blk]]
        allok &= ok
        print(f"{blk:<12}{LIMIT[blk]:>12}{ks['A']:>9}{ks['B']:>9}{ks['B2']:>10}"
              f"{bs['A']+bs['B']+bs['B2']:>11}{len(names):>9}{str(ns):>24}  {'OK' if ok else 'CHECK'}")
    print(f"\nresult lines kept (before truncation): " + ', '.join(f'{a}={n}' for a, n in sorted(tot.items())))
    print(f"malformed lines: {badtot}")
    print(f"ALL BLOCKS CLEAN: {allok}")
    return allok

def floor(label, pairs, unit):
    rr = []
    for blk, a, b in pairs: rr += [(blk, r) for r in rows(blk, a, b, unit)]
    if not rr:
        print(f'  {label}: NO DATA'); return {}
    ds = sorted(abs(r['delta']) for _, r in rr)
    def q(p): return ds[min(len(ds)-1, int(p*(len(ds)-1)))]
    print(f"  {label:<46} n={len(ds):>4}  median|Δ|={median(ds):6.2f}%  p90={q(0.90):6.2f}%  "
          f"p95={q(0.95):6.2f}%  max={ds[-1]:6.2f}%")
    w = max(rr, key=lambda t: abs(t[1]['delta']))
    aa = fmt_t(w[1]['a_med']) if unit == 'ns/op' else f"{w[1]['a_med']:.2f}"
    bb = fmt_t(w[1]['b_med']) if unit == 'ns/op' else f"{w[1]['b_med']:.2f}"
    print(f"      largest same-code drift: {w[0]}/{w[1]['bench']:<44} {aa:>10} -> {bb:<10} {w[1]['delta']:+.2f}%")
    per = defaultdict(list)
    for _, r in rr: per[band(r['a_med'] if unit == 'ns/op' else 1e6)].append(abs(r['delta']))
    env = {}
    print(f"      {'band':<18}{'n':>5}{'median':>10}{'p90':>9}{'p95':>9}{'max':>9}")
    for _, _, name in BANDS:
        v = sorted(per.get(name, []))
        if not v: continue
        p90 = v[min(len(v)-1, int(0.90*(len(v)-1)))]
        p95 = v[min(len(v)-1, int(0.95*(len(v)-1)))]
        env[name] = {'n':len(v),'median':median(v),'p90':p90,'p95':p95,'max':v[-1]}
        print(f"      {name:<18}{len(v):>5}{median(v):>9.2f}%{p90:>8.2f}%{p95:>8.2f}%{v[-1]:>8.2f}%")
    return env

def floors(unit):
    print('\n' + '='*112)
    print(f'FLOOR 1 — B vs B2: the SAME BYTE-IDENTICAL BINARY against itself  [{unit}]')
    print('='*112)
    f1 = floor('changed-binary blocks pooled', [(b,'B','B2') for b in sorted(CHANGED_BINARY)], unit)
    print(f'\nFLOOR 2 — A vs B on packages whose TEST BINARY is byte-identical between the releases  [{unit}]')
    f2 = floor('identical-binary packages (cross-arm)', [(b,'A','B') for b in sorted(IDENTICAL_ACROSS)], unit)
    return f1, f2

def bars(f1, f2):
    """The screening bar is the p95 of same-code |delta| in the row's own magnitude band,
    taken across both floors. The MAXIMUM is deliberately NOT used: at n=2 a single
    same-code outlier (exec/Scan_PerNode at -16.48%) would set a bar that rejects
    everything, which is not a bar, it is a refusal to look. The p95 is stated as a
    SCREENING envelope; clearing it is necessary, never sufficient, and every row that
    clears it is then checked for SIGN CONSISTENCY across rounds and against its OWN
    B-vs-B2 floor."""
    env = {}
    for _, _, name in BANDS:
        c = [f[name]['p95'] for f in (f1, f2) if name in f]
        env[name] = max(c) if c else 0.0
    print('\nSCREENING BAR per magnitude band = p95 of same-code |delta| across BOTH floors.')
    print('At n<=2 this is a SCREENING envelope, NOT the six-round adjudication bar v0.14.1 published.')
    print('Clearing it is necessary, never sufficient: sign consistency and the row own floor decide.')
    for _, _, name in BANDS:
        print(f'   {name:<18} {env[name]:6.2f}%')
    return env

def effects(blk, unit, env):
    rr = rows(blk, 'A', 'B', unit)
    flr = {r['bench']: r for r in rows(blk, 'B', 'B2', unit)}
    rr.sort(key=lambda r: r['delta'])
    k = LIMIT[blk]
    print(f"\n--- {blk} [{unit}]  A(v0.14.1) -> B(v0.14.2),  n={k} per arm,  {len(rr)} rows ---")
    hdr = f"{'benchmark':<44}{'v0.14.1':>11}{'v0.14.2':>11}{'Δ':>9}{'floor B|B2':>12}{'per-round Δ':>22}  verdict"
    print(hdr)
    geo = []
    for r in rr:
        a = fmt_t(r['a_med']) if unit == 'ns/op' else f"{r['a_med']:.0f}"
        b = fmt_t(r['b_med']) if unit == 'ns/op' else f"{r['b_med']:.0f}"
        bar = env.get(band(r['a_med'] if unit == 'ns/op' else 1e6), 0.0)
        fl = flr.get(r['bench'])
        fls = f"{fl['delta']:+.2f}%" if fl else '—'
        pr = ' '.join(f"{d:+.2f}%" for d in r['per_round'])
        held = bool(r['per_round']) and (all(d > 0 for d in r['per_round']) or all(d < 0 for d in r['per_round']))
        ownfloor = abs(fl['delta']) if fl else None
        if unit != 'ns/op' and r['a_const'] and r['b_const']:
            v = 'CONSTANT — equal' if r['a_med'] == r['b_med'] else 'CONSTANT — DIFFERS'
        elif abs(r['delta']) <= bar:
            v = 'within screening floor'
        elif k < 2:
            v = 'NOT ADJUDICABLE — one round only'
        elif not held:
            v = 'NOT ADJUDICABLE — sign flipped between rounds'
        elif ownfloor is not None and abs(r['delta']) > 4*ownfloor and min(abs(d) for d in r['per_round']) > bar:
            v = 'SEPARATION — sign held, every round clears the bar'
        else:
            v = 'NOT ADJUDICABLE at this power'
        print(f"{r['bench']:<44}{a:>11}{b:>11}{r['delta']:>+8.2f}%{fls:>12}{pr:>22}  {v}")
        if r['a_med'] > 0 and r['b_med'] > 0: geo.append(r['b_med']/r['a_med'])
    if geo:
        g = math.exp(sum(math.log(x) for x in geo)/len(geo))
        fg = [flr[x]['b_med']/flr[x]['a_med'] for x in flr if flr[x]['a_med'] > 0 and flr[x]['b_med'] > 0]
        fgm = math.exp(sum(math.log(x) for x in fg)/len(fg)) if fg else 1.0
        print(f"{'GEOMEAN':<44}{'':>11}{'':>11}{(g-1)*100:>+8.2f}%{(fgm-1)*100:>+11.2f}%")

if __name__ == '__main__':
    mode = sys.argv[1] if len(sys.argv) > 1 else 'all'
    if mode in ('all','integrity'): integrity()
    f1t, f2t = floors('ns/op')
    env = bars(f1t, f2t)
    if mode in ('all','allocs'):
        f1a, f2a = floors('allocs/op')
    if mode in ('all','effects'):
        for blk in (sys.argv[2:] or BLOCKS):
            effects(blk, 'ns/op', env)
    if mode == 'allocs':
        for blk in (sys.argv[2:] or BLOCKS):
            effects(blk, 'allocs/op', {})
            effects(blk, 'B/op', {})
