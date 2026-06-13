#!/usr/bin/env bash
# pkg_time_budget.sh — parse `go test -json` output on stdin and report
# packages whose wall-clock exceeds the per-package short-layer budget
# documented in docs/test-layers.md and docs/test-battery.md (< 60 s/pkg).
#
# Usage:
#   go test -race -count=1 -json ./... | bash scripts/pkg_time_budget.sh
#
# Environment:
#   SOFT_BUDGET   seconds; packages above this emit a ::warning:: (default 60)
#   HARD_BUDGET   seconds; packages above this fail the gate (exit 1).
#                 0 disables the hard ceiling (warn-only). Default 0.
#
# Exit codes:
#   0  no package exceeds HARD_BUDGET (or HARD_BUDGET disabled)
#   1  one or more packages exceeded HARD_BUDGET
#   2  no timing data parsed (the -json stream was empty or malformed)
#
# The script never re-runs the tests; it only summarises timings already
# produced by an upstream `go test -json` invocation, so it adds no test
# execution cost of its own.
set -euo pipefail

export SOFT_BUDGET="${SOFT_BUDGET:-60}"
export HARD_BUDGET="${HARD_BUDGET:-0}"

exec python3 -c '
import json, os, sys

soft = float(os.environ.get("SOFT_BUDGET", "60"))
hard = float(os.environ.get("HARD_BUDGET", "0"))

times = {}
for line in sys.stdin:
    line = line.strip()
    if not line or line[0] != "{":
        continue
    try:
        ev = json.loads(line)
    except ValueError:
        continue
    # Package-level total: a pass/fail event with no "Test" field.
    if ev.get("Action") in ("pass", "fail") and not ev.get("Test") \
            and "Elapsed" in ev and ev.get("Package"):
        times[ev["Package"]] = ev["Elapsed"]

if not times:
    sys.stderr.write("pkg_time_budget: no package timings parsed from -json stream\n")
    sys.exit(2)

ordered = sorted(times.items(), key=lambda kv: -kv[1])
print("-- short-layer per-package timings (slowest 10) --")
for pkg, el in ordered[:10]:
    print("  %7.1fs  %s" % (el, pkg))

over_soft = [(p, e) for p, e in ordered if e > soft]
over_hard = [(p, e) for p, e in ordered if hard > 0 and e > hard]

if over_soft:
    print()
    print("::warning::%d package(s) exceed the %.0fs short-layer budget (docs/test-layers.md):" % (len(over_soft), soft))
    for pkg, el in over_soft:
        print("  %.1fs  %s" % (el, pkg))

if over_hard:
    print("::error::%d package(s) exceed the hard %.0fs ceiling — split the package or move slow tests to the soak layer" % (len(over_hard), hard))
    sys.exit(1)
'
