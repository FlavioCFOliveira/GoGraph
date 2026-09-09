#!/usr/bin/env python3
"""apidiff.py — set-diff of exported top-level Go declarations between two trees.

Relies on gofmt: a top-level declaration begins at column 0. Grouped const/var/type
blocks are followed until their closing paren at column 0. Test files, testdata,
examples/ and cmd/ tools are excluded — only the importable library surface counts
for SemVer.
"""
import os, re, sys, json

FUNC   = re.compile(r'^func\s+([A-Z]\w*)\s*[\(\[]')
METH   = re.compile(r'^func\s+\((?:\w+\s+)?\*?([A-Z]\w*)(?:\[[^\]]*\])?\)\s+([A-Z]\w*)\s*[\(\[]')
SINGLE = re.compile(r'^(type|const|var)\s+([A-Z]\w*)\b')
GROUP  = re.compile(r'^(type|const|var)\s*\($')
GITEM  = re.compile(r'^\t([A-Z]\w*)')

def surface(root):
    out = set()
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames
                       if d not in ('testdata', '.git', 'dist', 'examples', 'cmd', 'bench')
                       and not d.startswith('.')]
        rel = os.path.relpath(dirpath, root)
        if rel == '.': rel = ''
        if rel.startswith('internal') or '/internal' in '/'+rel:
            continue          # internal/ is not part of the public surface
        for fn in filenames:
            if not fn.endswith('.go') or fn.endswith('_test.go'):
                continue
            path = os.path.join(dirpath, fn)
            with open(path, encoding='utf-8', errors='replace') as fh:
                lines = fh.read().split('\n')
            i, kind = 0, None
            while i < len(lines):
                ln = lines[i]
                if kind is not None:
                    if ln.startswith(')'):
                        kind = None
                    else:
                        m = GITEM.match(ln)
                        if m: out.add(f'{rel}.{m.group(1)} ({kind})')
                    i += 1; continue
                m = METH.match(ln)
                if m: out.add(f'{rel}.{m.group(1)}.{m.group(2)} (method)'); i += 1; continue
                m = FUNC.match(ln)
                if m: out.add(f'{rel}.{m.group(1)} (func)'); i += 1; continue
                m = GROUP.match(ln)
                if m: kind = m.group(1); i += 1; continue
                m = SINGLE.match(ln)
                if m: out.add(f'{rel}.{m.group(2)} ({m.group(1)})'); i += 1; continue
                i += 1
    return out

a, b = surface(sys.argv[1]), surface(sys.argv[2])
print(f'arm A exported decls: {len(a)}')
print(f'arm B exported decls: {len(b)}')
print()
add = sorted(b - a); rem = sorted(a - b)
print(f'### ADDED in B ({len(add)})')
for s in add: print('  +', s)
print()
print(f'### REMOVED in B ({len(rem)})')
for s in rem: print('  -', s)
