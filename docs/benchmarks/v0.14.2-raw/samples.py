#!/usr/bin/env python3
"""samples.py — print one benchmark's per-round samples, in round order, for all
three arms. A systematic separation that holds in EVERY round is a different kind
of evidence from a median that moved, and this is how it is checked."""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from analyse import parse
SP = '/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/268e8af2-f36a-44f1-a476-9d616baa960f/scratchpad'
OUT = SP + '/raw/campaign'
blk, bench = sys.argv[1], sys.argv[2]
unit = sys.argv[3] if len(sys.argv) > 3 else 'ns/op'
print(f"{blk} / {bench} [{unit}] — per-round samples, in the order they were taken")
for arm in ('A','B','B2'):
    d,_,_,_ = parse(f'{OUT}/{blk}_{arm}.txt')
    vals = d.get(bench, {}).get(unit, [])
    print(f"  {arm:<3} " + '  '.join(f"{v:>12.6g}" for v in vals))
