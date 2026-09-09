import os,sys
sys.path.insert(0,os.path.dirname(os.path.abspath(__file__)))
from analyse import compare
OUT='/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/7564ade5-31fe-4cf5-9111-9856bea1b376/scratchpad/raw/campaign'
def fmt(v):
    for lim,suf,div in ((1e3,'ns',1),(1e6,'µs',1e3),(1e9,'ms',1e6),(float('inf'),'s',1e9)):
        if abs(v)<lim: return f'{v/div:.4g} {suf}'
    return f'{v:.4g} ns'
print('=== the Expand family — is the regression systematic across K? ===')
eff={r['bench']:r for r in compare(f'{OUT}/exec_A.txt',f'{OUT}/exec_B.txt','A','B',units=('ns/op',))['rows']}
flo={r['bench']:r for r in compare(f'{OUT}/exec_B.txt',f'{OUT}/exec_B2.txt','B','B2',units=('ns/op',))['rows']}
print(f"{'benchmark':<62}{'v0.14.0':>11}{'v0.14.1':>11}{'delta':>9}{'p':>7}{'floor':>9}")
for n in sorted(eff):
    if 'Expand' not in n: continue
    r=eff[n]; f=flo.get(n)
    fs=f"{f['delta_pct']:+.2f}%" if f else '-'
    p=f"{r['p']:.3f}" if r['p'] is not None else ' n/a'
    print(f"{n:<62}{fmt(r['a_med']):>11}{fmt(r['b_med']):>11}{r['delta_pct']:>+8.2f}%{p:>7}{fs:>9}")
print('\n=== the BoundedOrder family — the ORDER BY key hoist (#2662) ===')
for n in sorted(eff):
    if 'BoundedOrder' not in n: continue
    r=eff[n]; f=flo.get(n)
    fs=f"{f['delta_pct']:+.2f}%" if f else '-'
    p=f"{r['p']:.3f}" if r['p'] is not None else ' n/a'
    print(f"{n:<62}{fmt(r['a_med']):>11}{fmt(r['b_med']):>11}{r['delta_pct']:>+8.2f}%{p:>7}{fs:>9}")
