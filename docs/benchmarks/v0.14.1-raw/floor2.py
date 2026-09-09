#!/usr/bin/env python3
"""floor2.py — the noise floor, stratified by BENCHMARK KIND as well as magnitude.

Pooling every comparison into one magnitude band produced a 26.13% bar for the
100ns-10us band, driven entirely by ONE benchmark: BenchmarkReadTx_LockFree-1024,
a b.RunParallel cell at -test.cpu=1024 whose own within-arm spread is +-28.6%.
A bar set by that would reject every real finding in the band.

The cells at 64/256/1024 goroutines are a structurally different population from a
serial benchmark: on a 10-core host they measure inverse AGGREGATE throughput under
oversubscription, where the scheduler's own variance dominates. So they get their own
floor, and findings in them are held to it. That is a statement about which
measurements this host can resolve, not a way of lowering a bar until a result appears.
"""
import os, sys, math, re
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import compare, median

OUT = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad/raw/campaign'
BLOCKS = ['exec','cy','rec','txn','bolt','ckpt','search','centrality','cnt','Lreadtx','Ltxn']
IDENTICAL_ACROSS = {'cnt'}
HICONC = re.compile(r'-(64|256|1024)$')     # oversubscribed ladder cells

BANDS = [(0,100,'< 100 ns'),(100,1e4,'100 ns - 10 us'),(1e4,1e6,'10 us - 1 ms'),(1e6,float('inf'),'> 1 ms')]
def band(v):
    for lo,hi,n in BANDS:
        if lo <= v < hi: return n
    return '> 1 ms'

def rows(pairs, unit):
    out=[]
    for pa,pb in pairs:
        for r in compare(pa,pb,'x','y',units=(unit,))['rows']: out.append(r)
    return out

def table(label, rs, unit):
    if not rs: print(f'  {label}: no data'); return {}
    per=defaultdict(list)
    for r in rs: per[band(r['a_med'] if unit=='ns/op' else 1e6)].append(r)
    print(f'\n  {label}  (n={len(rs)})')
    print(f"    {'band':<18}{'n':>5}{'median':>9}{'p95':>8}{'max':>8}{'worst sig':>11}{'BAR':>8}")
    bars={}
    for _,_,nm in BANDS:
        rr=per.get(nm,[])
        if not rr: continue
        d=sorted(abs(x['delta_pct']) for x in rr)
        sig=[abs(x['delta_pct']) for x in rr if x['p'] is not None and x['p']<0.05]
        p95=d[int(0.95*(len(d)-1))]
        bar=max(p95, max(sig) if sig else 0.0)
        bars[nm]=bar
        print(f"    {nm:<18}{len(rr):>5}{median(d):>8.2f}%{p95:>7.2f}%{d[-1]:>7.2f}%"
              f"{(max(sig) if sig else 0.0):>10.2f}%{bar:>7.2f}%")
    return bars

f1 = rows([(f'{OUT}/{b}_B.txt', f'{OUT}/{b}_B2.txt') for b in BLOCKS], 'ns/op')
f2 = rows([(f'{OUT}/{b}_A.txt', f'{OUT}/{b}_B.txt') for b in sorted(IDENTICAL_ACROSS)], 'ns/op')

ser1=[r for r in f1 if not HICONC.search(r['bench'])]
hi1 =[r for r in f1 if     HICONC.search(r['bench'])]

print('='*96)
print('FLOOR 1 — B vs B2, byte-identical binary, same rounds, same load  [ns/op]')
print('='*96)
b_ser = table('SERIAL and low-concurrency (<=8 goroutines)', ser1, 'ns/op')
b_hi  = table('OVERSUBSCRIBED ladder cells (64/256/1024 goroutines)', hi1, 'ns/op')
print('\n'+'='*96)
print('FLOOR 2 — A vs B on graph/index/count, whose TEST BINARY is byte-identical')
print('          BETWEEN the releases. n is small; it is a cross-CHECK on Floor 1,')
print('          not a substitute for it.')
print('='*96)
b_x = table('cross-arm, identical binary', f2, 'ns/op')

print('\n'+'='*96)
print('THE BARS ACTUALLY USED')
print('='*96)
final={}
for _,_,nm in BANDS:
    cands=[b for b in (b_ser.get(nm), b_x.get(nm)) if b]
    if cands: final[nm]=max(cands)
for nm,v in final.items(): print(f'   serial          {nm:<18}{v:>7.2f}%')
for nm,v in b_hi.items():  print(f'   oversubscribed  {nm:<18}{v:>7.2f}%')

print('\nEvery Floor-1 row that reached p<0.05, with its population:')
for r in sorted([x for x in f1 if x['p'] is not None and x['p']<0.05], key=lambda x:-abs(x['delta_pct'])):
    pop = 'OVERSUBSCRIBED' if HICONC.search(r['bench']) else 'serial'
    print(f"   {r['delta_pct']:+7.2f}%  p={r['p']:.4f}  {pop:<15}{r['bench']}")
