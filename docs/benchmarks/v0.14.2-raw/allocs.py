#!/usr/bin/env python3
"""allocs.py — the allocation table, in its strongest form.

An allocs/op delta is only worth reporting when the samples are constant: "every
sample equal in both arms" is a per-operation CONSTANT, not a median that happened
to move. This prints, per benchmark, whether each arm's six samples are all equal
and whether the two constants differ.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import compare, median
SP = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/268e8af2-f36a-44f1-a476-9d616baa960f/scratchpad'
OUT = SP + '/raw/campaign'

def const(v): return len(set(v)) == 1

for blk in sys.argv[1:]:
    eff = {r['bench']: r for r in compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=('allocs/op',))['rows']}
    byt = {r['bench']: r for r in compare(f'{OUT}/{blk}_A.txt', f'{OUT}/{blk}_B.txt','A','B',units=('B/op',))['rows']}
    flr = {r['bench']: r for r in compare(f'{OUT}/{blk}_B.txt', f'{OUT}/{blk}_B2.txt','B','B2',units=('allocs/op',))['rows']}
    moved = [b for b, r in eff.items() if r['a_med'] != r['b_med']]
    zeroa = [b for b, r in eff.items() if r['a_med'] == 0 and r['b_med'] == 0]
    print(f"\n### {blk}: {len(eff)} benchmarks; {len(moved)} moved an allocation count; "
          f"{len(zeroa)} are zero-allocation in both arms")
    if moved:
        print(f"{'benchmark':<44}{'allocs A':>10}{'allocs B':>10}{'Δ':>7}{'const?':>9}"
              f"{'B/op A':>12}{'B/op B':>12}{'ΔB':>9}{'floor':>8}")
    for b in sorted(moved, key=lambda b: -(eff[b]['b_med']-eff[b]['a_med'])):
        r, rb = eff[b], byt.get(b)
        c = 'BOTH' if const(r['a_vals']) and const(r['b_vals']) else ('A' if const(r['a_vals']) else ('B' if const(r['b_vals']) else 'no'))
        fl = flr.get(b)
        flt = 'same' if fl and fl['a_med'] == fl['b_med'] else (f"{fl['delta_pct']:+.2f}%" if fl else '—')
        ba = f"{rb['a_med']:.0f}" if rb else '—'
        bb = f"{rb['b_med']:.0f}" if rb else '—'
        db = f"{rb['b_med']-rb['a_med']:+.0f}" if rb else '—'
        print(f"{b:<44}{r['a_med']:>10.0f}{r['b_med']:>10.0f}{r['b_med']-r['a_med']:>+7.0f}{c:>9}"
              f"{ba:>12}{bb:>12}{db:>9}{flt:>8}")
    # bytes-only movers
    bmoved = [b for b, r in byt.items() if r['a_med'] != r['b_med'] and b not in moved]
    if bmoved:
        print(f"  bytes moved but allocation count did not, {len(bmoved)} rows:")
        for b in sorted(bmoved, key=lambda b: -abs(byt[b]['delta_pct']))[:12]:
            r = byt[b]
            print(f"    {b:<48}{r['a_med']:>12.0f}{r['b_med']:>12.0f}{r['delta_pct']:>+8.2f}%")
