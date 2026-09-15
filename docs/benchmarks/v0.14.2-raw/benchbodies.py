#!/usr/bin/env python3
"""benchbodies.py — extract each Benchmark function's body from a package's *_test.go
and report which ones mention a given pattern, so 'which benchmark reaches this code'
is answered from the SOURCE rather than from intuition."""
import re, sys, os, glob
pkgdir, pat = sys.argv[1], sys.argv[2]
rx = re.compile(pat)
hits = []
allb = []
for f in sorted(glob.glob(os.path.join(pkgdir, '*_test.go'))):
    src = open(f, encoding='utf-8', errors='replace').read()
    # split at top-level func declarations
    idx = [m.start() for m in re.finditer(r'(?m)^func ', src)] + [len(src)]
    for i in range(len(idx)-1):
        seg = src[idx[i]:idx[i+1]]
        m = re.match(r'func (Benchmark[A-Za-z0-9_]*)', seg)
        if not m: continue
        allb.append(m.group(1))
        if rx.search(seg):
            hits.append((m.group(1), os.path.basename(f)))
print(f"benchmarks scanned: {len(allb)}   matching /{pat}/ : {len(hits)}")
for n, f in sorted(hits): print(f"  {n:<58} {f}")
