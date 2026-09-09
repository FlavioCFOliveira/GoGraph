#!/usr/bin/env python3
"""fielddiff.py — exported STRUCT FIELDS of exported types, per package, diffed
between two trees. A top-level-declaration scan cannot see these, and adding one
is a MINOR trigger under docs/semver.md (and breaks an UNKEYED composite literal).
Relies on gofmt: `type X struct {` at column 0, fields at one tab, closing `}` at
column 0.
"""
import os, re, sys
TYPESTRUCT = re.compile(r'^type\s+([A-Z]\w*)(?:\[[^\]]*\])?\s+struct\s*\{')
FIELD      = re.compile(r'^\t([A-Z]\w*)(?:,\s*[A-Z]\w*)*\s+(\S.*?)\s*(?://.*)?$')
EMBED      = re.compile(r'^\t(\*?[A-Z]\w*(?:\.\w+)?)\s*(?://.*)?$')

SKIP_DIR = {'testdata','.git','dist','examples','cmd','bench','internal'}

def surface(root):
    out = set()
    for dp, dns, fns in os.walk(root):
        dns[:] = [d for d in dns if d not in SKIP_DIR and not d.startswith('.')]
        rel = os.path.relpath(dp, root); rel = '' if rel == '.' else rel
        if rel.startswith('cypher/parser/gen') or rel.startswith('cypher/tck'): continue
        for fn in fns:
            if not fn.endswith('.go') or fn.endswith('_test.go'): continue
            lines = open(os.path.join(dp, fn), encoding='utf-8', errors='replace').read().split('\n')
            i = 0
            while i < len(lines):
                m = TYPESTRUCT.match(lines[i])
                if not m: i += 1; continue
                tname = m.group(1); i += 1
                while i < len(lines) and not lines[i].startswith('}'):
                    fm = FIELD.match(lines[i])
                    if fm:
                        out.add(f'{rel or "root"}.{tname}.{fm.group(1)} {fm.group(2)}')
                    else:
                        em = EMBED.match(lines[i])
                        if em: out.add(f'{rel or "root"}.{tname}.<embed {em.group(1)}>')
                    i += 1
                i += 1
    return out

a, b = surface(sys.argv[1]), surface(sys.argv[2])
print(f'arm A exported struct fields: {len(a)}')
print(f'arm B exported struct fields: {len(b)}')
add, rem = sorted(b - a), sorted(a - b)
print(f'\n### ADDED fields ({len(add)})')
for s in add: print('  +', s)
print(f'\n### REMOVED/CHANGED fields ({len(rem)})')
for s in rem: print('  -', s)
