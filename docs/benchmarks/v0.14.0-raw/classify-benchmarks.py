#!/usr/bin/env python3
"""Classify every Benchmark function in a tree as concurrent or serial.

Concurrent means the benchmark itself creates concurrency, by either
  (a) b.RunParallel(...), or
  (b) a hand-rolled `go func` fan-out (usually behind a WaitGroup).
Only such a benchmark turns -test.cpu=1,8,64,256,1024 into a GOROUTINE ladder.
On a serial benchmark that flag varies GOMAXPROCS and nothing else.
"""
import os, re, sys, json

root = sys.argv[1]
FUNC = re.compile(r'^func (Benchmark\w+)\(([a-zA-Z_]\w*) \*testing\.B\) \{', re.M)

out = {}
for dirpath, dirnames, filenames in os.walk(root):
    dirnames[:] = [d for d in dirnames if d not in ('.git', 'vendor', 'testdata')]
    for fn in filenames:
        if not fn.endswith('_test.go'):
            continue
        p = os.path.join(dirpath, fn)
        try:
            src = open(p, encoding='utf-8', errors='replace').read()
        except OSError:
            continue
        pkg = os.path.relpath(dirpath, root)
        for m in FUNC.finditer(src):
            name, recv = m.group(1), m.group(2)
            # walk braces from the opening brace to find the body
            i = src.index('{', m.start())
            depth, j = 0, i
            while j < len(src):
                if src[j] == '{': depth += 1
                elif src[j] == '}':
                    depth -= 1
                    if depth == 0: break
                j += 1
            body = src[i:j+1]
            rp = f'{recv}.RunParallel(' in body
            gofunc = re.search(r'\bgo\s+func\b', body) is not None
            wg = 'sync.WaitGroup' in body
            out[f'{pkg}::{name}'] = {
                'pkg': pkg, 'name': name, 'file': fn,
                'runparallel': rp, 'gofunc': gofunc, 'waitgroup': wg,
                'concurrent': rp or (gofunc and wg),
            }
print(json.dumps(out, indent=0, sort_keys=True))
