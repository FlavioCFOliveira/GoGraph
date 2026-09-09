#!/usr/bin/env python3
import os, sys, re
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import compare, parse, median
OUT='/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad/raw/campaign'
def fmt(v):
    for lim,suf,div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v)<lim: return f'{v/div:.4g} {suf}'
    return f'{v:.4g} ns'

def show(blk, unit='ns/op', floor=True):
    print(f'\n=== {blk}  [{unit}]  A=v0.14.0  B=v0.14.1 ===')
    eff=compare(f'{OUT}/{blk}_A.txt',f'{OUT}/{blk}_B.txt','A','B',units=(unit,))['rows']
    flo={r['bench']:r for r in compare(f'{OUT}/{blk}_B.txt',f'{OUT}/{blk}_B2.txt','B','B2',units=(unit,))['rows']}
    print(f"{'benchmark':<52}{'v0.14.0':>12}{'v0.14.1':>12}{'delta':>9}{'p':>7}   {'B vs B2 floor':>14}")
    for r in sorted(eff,key=lambda x:x['bench']):
        f=flo.get(r['bench'])
        fs=f"{f['delta_pct']:+.2f}% p={f['p']:.3f}" if f and f['p'] is not None else '-'
        p=f"{r['p']:.3f}" if r['p'] is not None else ' n/a'
        print(f"{r['bench']:<52}{fmt(r['a_med']):>12}{fmt(r['b_med']):>12}{r['delta_pct']:>+8.2f}%{p:>7}   {fs:>14}")

for b in ('Lreadtx','Ltxn','search','centrality','txn','rec'):
    show(b)

print('\n=== ckpt — ONE-ARMED: these five benchmarks do not exist at v0.14.0 ===')
print('Absolute v0.14.1 figures, median of 6, with the same-binary floor beside them.')
d,_,_,_ = parse(f'{OUT}/ckpt_B.txt')
flo={r['bench']:r for r in compare(f'{OUT}/ckpt_B.txt',f'{OUT}/ckpt_B2.txt','B','B2',units=('ns/op',))['rows']}
print(f"{'benchmark':<52}{'v0.14.1 median':>16}{'B vs B2 floor':>18}")
for n in sorted(d):
    v=median(d[n]['ns/op']); f=flo.get(n)
    fs=f"{f['delta_pct']:+.2f}% p={f['p']:.3f}" if f and f['p'] is not None else '-'
    print(f"{n:<52}{fmt(v):>16}{fs:>18}")
