#!/usr/bin/env python3
"""analyse.py — integrity check + A/B statistics over the campaign's raw files.

Two jobs, in this order:

 1. INTEGRITY. Every kept line must match a strict result-line regex. A log line
    landing mid-result silently removes samples and benchstat will not tell you.
    Arms must carry the same benchmark set and the same sample count.

 2. STATISTICS. Median, spread, and a Mann-Whitney U p-value per benchmark per metric.
    Reported as a comparison of the medians, with n stated, never a single run.
"""
import os, re, sys, math, json, itertools
from collections import defaultdict

RESULT = re.compile(
    r'^(?P<name>Benchmark\S*)\s+(?P<n>\d+)\s+(?P<rest>.*\S)\s*$')
METRIC = re.compile(r'(?P<val>[-+]?[\d.]+(?:[eE][-+]?\d+)?)\s+(?P<unit>\S+)')

def parse(path):
    """-> {benchname: {unit: [values]}}, plus the integrity tally."""
    data = defaultdict(lambda: defaultdict(list))
    kept = bad = 0
    badlines = []
    if not os.path.exists(path):
        return data, 0, 0, ['<missing file>']
    for raw in open(path, encoding='utf-8', errors='replace'):
        line = raw.rstrip('\n')
        if not line.strip():
            continue
        if line.startswith(('goos:', 'goarch:', 'pkg:', 'cpu:', 'PASS', 'ok ', '---', 'testing:')):
            continue
        m = RESULT.match(line)
        if not m:
            bad += 1
            if len(badlines) < 8: badlines.append(line[:140])
            continue
        name = m.group('name')
        for mm in METRIC.finditer(m.group('rest')):
            try: v = float(mm.group('val'))
            except ValueError: continue
            data[name][mm.group('unit')].append(v)
        kept += 1
    return data, kept, bad, badlines

def median(xs):
    s = sorted(xs); n = len(s)
    return s[n//2] if n % 2 else (s[n//2-1]+s[n//2])/2

def spread(xs):
    """max deviation from the median, as a percentage -- the same shape benchstat prints."""
    if not xs: return 0.0
    m = median(xs)
    if m == 0: return 0.0
    return max(abs(x-m) for x in xs)/abs(m)*100

def mannwhitney_p(a, b):
    """Two-sided Mann-Whitney U, normal approximation with tie correction.
    Small-n exact is not attempted; with n<4 per arm the p-value is reported as None."""
    na, nb = len(a), len(b)
    if na < 4 or nb < 4: return None
    allv = sorted([(v,0) for v in a] + [(v,1) for v in b])
    ranks = [0.0]*len(allv); i = 0; ties = []
    while i < len(allv):
        j = i
        while j+1 < len(allv) and allv[j+1][0] == allv[i][0]: j += 1
        r = (i+j)/2 + 1
        for k in range(i, j+1): ranks[k] = r
        ties.append(j-i+1); i = j+1
    Ra = sum(r for r,(v,g) in zip(ranks, allv) if g == 0)
    Ua = Ra - na*(na+1)/2
    Ub = na*nb - Ua
    U = min(Ua, Ub)
    mu = na*nb/2
    N = na+nb
    tie_term = sum(t**3 - t for t in ties)
    var = na*nb/12 * ((N+1) - tie_term/(N*(N-1))) if N > 1 else 0
    if var <= 0: return 1.0
    z = (U - mu + 0.5)/math.sqrt(var)
    p = 2*(1 - 0.5*(1+math.erf(abs(z)/math.sqrt(2))))
    return min(1.0, max(0.0, p))

def compare(pa, pb, label_a, label_b, units=('ns/op','B/op','allocs/op')):
    da, ka, ba, bla = parse(pa)
    db, kb, bb, blb = parse(pb)
    rep = {'file_a': pa, 'file_b': pb, 'kept_a': ka, 'kept_b': kb,
           'bad_a': ba, 'bad_b': bb, 'badlines_a': bla, 'badlines_b': blb,
           'only_a': sorted(set(da)-set(db)), 'only_b': sorted(set(db)-set(da)),
           'rows': []}
    for name in sorted(set(da) & set(db)):
        for u in units:
            xa, xb = da[name].get(u, []), db[name].get(u, [])
            if not xa or not xb: continue
            ma, mb = median(xa), median(xb)
            delta = (mb-ma)/ma*100 if ma else float('nan')
            rep['rows'].append({
                'bench': name, 'unit': u,
                'a_med': ma, 'b_med': mb, 'a_n': len(xa), 'b_n': len(xb),
                'a_spread': spread(xa), 'b_spread': spread(xb),
                'delta_pct': delta, 'p': mannwhitney_p(xa, xb),
                'a_vals': xa, 'b_vals': xb,
            })
    return rep

if __name__ == '__main__':
    print(json.dumps(compare(sys.argv[1], sys.argv[2], 'A', 'B'), indent=1))
