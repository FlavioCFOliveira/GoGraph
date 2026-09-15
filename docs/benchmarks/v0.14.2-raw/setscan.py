#!/usr/bin/env python3
"""setscan.py — every top-level func in a package's *_test.go whose BODY contains a
Cypher SET clause (or DROP INDEX), printed with its name, so 'no benchmark reaches
the SET write path' can be checked against the whole file set rather than asserted."""
import re, sys, os, glob
pkgdir = sys.argv[1]; pat = re.compile(sys.argv[2])
for f in sorted(glob.glob(os.path.join(pkgdir, '*_test.go'))):
    src = open(f, encoding='utf-8', errors='replace').read()
    idx = [m.start() for m in re.finditer(r'(?m)^func ', src)] + [len(src)]
    for i in range(len(idx)-1):
        seg = src[idx[i]:idx[i+1]]
        m = re.match(r'func (?:\([^)]*\)\s*)?([A-Za-z0-9_]+)', seg)
        if not m: continue
        if pat.search(seg):
            kind = 'BENCH' if m.group(1).startswith('Benchmark') else ('TEST ' if m.group(1).startswith('Test') else 'helper')
            print(f"  {kind}  {m.group(1):<52} {os.path.basename(f)}")
