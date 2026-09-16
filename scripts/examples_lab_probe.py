#!/usr/bin/env python3
"""examples_lab_probe.py — read the TOTAL out of a pprof artefact, per sample
index, so the sweep driver can tell a real profile from an empty file.

Written for the whole-surface examples laboratory (rmp #2856, sprint 362). The
driver's fail condition is "no profile, or a profile whose total is zero", and
that verdict needs a number, not a file size: `runtime/pprof` happily writes a
well-formed, gzip-compressed, 700-byte protobuf profile containing no samples at
all, and an `ls -l` cannot tell it from a useful one.

Usage:
    scripts/examples_lab_probe.py PROFILE [PROFILE ...]

Prints one line per profile and sample index:

    PROFILE<TAB>INDEX<TAB>TOTAL<TAB>UNIT<TAB>NODES

TOTAL is in the profile's own base unit (nanoseconds for a CPU or contention
profile, bytes for a space index, a plain count otherwise), so the driver
compares numbers rather than parsing "1.23s" against "456MB".

    -nodefraction=0 IS MANDATORY and is passed on every call.

pprof defaults it to 0.005 and then silently drops every node below 0.5% of the
total. Measured in this sprint on one Cypher profile, the default reported
16.04 s against a true 20.70 s — 4.66 s vanished with no warning of any kind.
A driver that inherited the default would under-report every total it checks.

Exit status is 0 when every profile was read, 1 when any could not be, so the
caller can fail a row on the strength of it.
"""

import os
import re
import subprocess
import sys

# "Showing nodes accounting for 1.20s, 100% of 2.35s total"
SHOWING = re.compile(
    r"^Showing nodes accounting for\s+([0-9.]+)([a-zA-Z]*),.*?of\s+([0-9.]+)([a-zA-Z]*)\s+total"
)
# The single-node case prints no "of X total" clause at all.
SHOWING_ALL = re.compile(r"^Showing nodes accounting for\s+([0-9.]+)([a-zA-Z]*),\s*100%")
TOPROW = re.compile(r"^\s*[0-9.]+[a-zA-Z]*\s+[0-9.]+%\s+[0-9.]+%\s+")

TIME = {"ns": 1, "us": 1e3, "µs": 1e3, "ms": 1e6, "s": 1e9, "m": 60e9, "h": 3600e9}
SIZE = {"B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}

# Which sample indices each kind of artefact carries. Asking a CPU profile for
# alloc_space is an error, not an empty answer, so the map has to be right.
INDICES = {
    "cpu.pprof": ["samples", "cpu"],
    "heap.pprof": ["alloc_objects", "alloc_space", "inuse_objects", "inuse_space"],
    "mutex.pprof": ["contentions", "delay"],
    "block.pprof": ["contentions", "delay"],
    "goroutine.pprof": ["goroutine"],
}


def base_unit(unit):
    if unit in TIME:
        return "ns"
    if unit in SIZE:
        return "bytes"
    return "count"


def to_base(num, unit):
    n = float(num)
    if unit in TIME:
        return n * TIME[unit]
    if unit in SIZE:
        return n * SIZE[unit]
    return n


def probe(path, index):
    """Return (total_in_base_unit, base_unit, node_rows) or None on failure."""
    cmd = ["go", "tool", "pprof", "-top", "-nodecount=100000", "-nodefraction=0"]
    if index:
        cmd.append(f"-sample_index={index}")
    cmd.append(path)
    res = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        return None
    total, unit, nodes = None, "", 0
    for line in res.stdout.splitlines():
        m = SHOWING.match(line)
        if m:
            total, unit = m.group(3), m.group(4)
            continue
        m = SHOWING_ALL.match(line)
        if m and total is None:
            total, unit = m.group(1), m.group(2)
            continue
        if TOPROW.match(line):
            nodes += 1
    if total is None:
        # A profile with no samples prints no accounting line at all. That is a
        # zero total, which is a real answer and not a read failure.
        return 0.0, "count", 0
    return to_base(total, unit), base_unit(unit), nodes


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    failed = False
    for path in sys.argv[1:]:
        name = os.path.basename(path)
        if not os.path.exists(path):
            print(f"{path}\t-\tMISSING\t-\t-")
            failed = True
            continue
        for index in INDICES.get(name, [""]):
            got = probe(path, index)
            if got is None:
                print(f"{path}\t{index or 'default'}\tUNREADABLE\t-\t-")
                failed = True
                continue
            total, unit, nodes = got
            print(f"{path}\t{index or 'default'}\t{total:.0f}\t{unit}\t{nodes}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
