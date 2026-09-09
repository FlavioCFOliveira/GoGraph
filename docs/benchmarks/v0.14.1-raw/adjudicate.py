#!/usr/bin/env python3
"""adjudicate.py — every effect row, against the bar for its own population and band.

A delta is a FINDING only when it is significant (p<0.05, Mann-Whitney, n=6/arm) AND
exceeds the same-code envelope for its magnitude band and benchmark kind. Rows that
are significant but inside the envelope are rejected as noise, and said so.

The 10us-1ms bar rests on a SINGLE cross-arm comparison, so anything between Floor 1's
serial figure (2.59%) and that bar (7.87%) is reported INCONCLUSIVE rather than forced
into one verdict or the other.
"""
import os, sys, re, math
from collections import defaultdict
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import compare

OUT='/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad/raw/campaign'
EFFECT=['exec','cy','rec','txn','bolt','search','centrality','Lreadtx','Ltxn']  # cnt is the floor; ckpt is one-armed
HICONC=re.compile(r'-(64|256|1024)$')
BANDS=[(0,100,'< 100 ns'),(100,1e4,'100 ns - 10 us'),(1e4,1e6,'10 us - 1 ms'),(1e6,float('inf'),'> 1 ms')]
BAR_SERIAL={'< 100 ns':1.95,'100 ns - 10 us':2.13,'10 us - 1 ms':7.87,'> 1 ms':1.28}
BAR_HICONC={'100 ns - 10 us':26.13,'10 us - 1 ms':0.75,'< 100 ns':26.13,'> 1 ms':0.75}
GREY={'10 us - 1 ms':(2.59,7.87)}   # Floor-1-serial .. Floor-2 single point

def band(v):
    for lo,hi,n in BANDS:
        if lo<=v<hi: return n
    return '> 1 ms'
def fmt(v):
    for lim,suf,div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v)<lim: return f'{v/div:.4g}{suf}'
    return f'{v:.4g}ns'

allrows=[]
for blk in EFFECT:
    for r in compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=('ns/op',))['rows']:
        r['blk']=blk; allrows.append(r)
print(f'comparable result rows, v0.14.0 vs v0.14.1: {len(allrows)}')
sig=[r for r in allrows if r['p'] is not None and r['p']<0.05]
print(f'rows reaching p<0.05: {len(sig)}')
print()
print(f"{'block':<11}{'benchmark':<58}{'v0.14.0':>11}{'v0.14.1':>11}{'delta':>9}{'p':>7}{'bar':>7}  verdict")
find=[];rej=[];inc=[]
for r in sorted(sig,key=lambda x:x['delta_pct']):
    hi = bool(HICONC.search(r['bench']))
    bn = band(r['a_med']); bar = (BAR_HICONC if hi else BAR_SERIAL)[bn]
    d = abs(r['delta_pct'])
    lo_g,hi_g = GREY.get(bn,(None,None))
    if d>bar: v='FINDING'; find.append((r,bn,bar))
    elif (not hi) and lo_g and lo_g<d<=hi_g: v='inconclusive'; inc.append((r,bn,bar))
    else: v='rejected as noise'; rej.append((r,bn,bar))
    print(f"{r['blk']:<11}{r['bench']:<58}{fmt(r['a_med']):>11}{fmt(r['b_med']):>11}"
          f"{r['delta_pct']:>+8.2f}%{r['p']:>7.3f}{bar:>6.2f}%  {v}")
print(f"\nFINDINGS: {len(find)}   inconclusive: {len(inc)}   rejected as noise: {len(rej)}")

print('\n--- geomean per block (all comparable rows, not only significant ones) ---')
for blk in EFFECT:
    rs=[r for r in allrows if r['blk']==blk and r['a_med']>0 and r['b_med']>0]
    if not rs: continue
    g=math.exp(sum(math.log(r['b_med']/r['a_med']) for r in rs)/len(rs))
    fl=compare(f'{OUT}/{blk}_B.txt', f'{OUT}/{blk}_B2.txt','B','B2',units=('ns/op',))['rows']
    fl=[r for r in fl if r['a_med']>0 and r['b_med']>0]
    gf=math.exp(sum(math.log(r['b_med']/r['a_med']) for r in fl)/len(fl)) if fl else 1.0
    print(f'  {blk:<12} n={len(rs):>4}  effect geomean {(g-1)*100:+7.2f}%   same-binary floor geomean {(gf-1)*100:+7.2f}%')
