#!/usr/bin/env python3
"""loadsampler.py — ONE long-lived sampler for the whole campaign.

Appends `<epoch> <load1> <load5> <load15>` every INTERVAL seconds. One process for
the entire run rather than one per invocation, because a sampler that forks a
process per sample is itself load, and the thing being measured is load.

Sliced per invocation afterwards by loadstats.py, using the start/end epochs the
runner records in exits.log. That gives a real min/max/mean DURING each arm, which
before/after brackets alone cannot: a bracket cannot see a spike that begins and
ends inside the invocation, and this project has already had an interleaved A/B
ruined by exactly such a spike.
"""
import os, sys, time
out = sys.argv[1]
interval = float(sys.argv[2]) if len(sys.argv) > 2 else 5.0
with open(out, 'a', buffering=1) as fh:
    while True:
        l1, l5, l15 = os.getloadavg()
        fh.write(f"{time.time():.3f} {l1:.2f} {l5:.2f} {l15:.2f}\n")
        time.sleep(interval)
