#!/usr/bin/env python3
"""allocs.py — allocation deltas. The same-binary floor for allocs/op is 0.00% at the
median AND the p95 (max 0.14%), so a non-zero delta here is real, not drift.

A 0 -> 0 row yields a NaN percentage, not a movement; filtering on the percentage alone
counted 40 zero-allocation benchmarks as having 'moved'. Compare the medians."""
import os, sys, math
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import compare
OUT='/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad/raw/campaign'
EFFECT=['exec','cy','rec','txn','bolt','search','centrality','Lreadtx','Ltxn']
for unit in ('allocs/op','B/op'):
    print('='*100); print(f'{unit}: rows whose MEDIAN differs between the arms'); print('='*100)
    moved=tot=zero=0
    for blk in EFFECT:
        for r in compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=(unit,))['rows']:
            tot+=1
            if r['a_med']==0 and r['b_med']==0: zero+=1; continue
            if r['a_med']==r['b_med']: continue
            moved+=1
            eq = len(set(r['a_vals']))==1 and len(set(r['b_vals']))==1
            print(f"  {blk:<10}{r['bench']:<58}{r['a_med']:>11.0f} -> {r['b_med']:<11.0f}"
                  f"{r['delta_pct']:>+8.2f}%  {'every sample equal' if eq else 'samples vary'}")
    print(f'  -> {moved} of {tot} rows moved; {zero} are zero-allocation in both arms\n')
