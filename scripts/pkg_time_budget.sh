#!/usr/bin/env bash
# pkg_time_budget.sh — parse `go test` output on stdin and report packages whose
# wall-clock exceeds the per-package short-layer budget documented in
# docs/test-layers.md and docs/test-battery.md (< 60 s/pkg).
#
# The stream is echoed through VERBATIM, so this sits on a test pipeline without
# swallowing its output, and either format is accepted (detected per line, no
# flag): `go test -json` events, or the plain "ok<TAB>pkg<TAB>0.330s" summary
# lines an ordinary `go test` already prints. Plain is what `make test-short`
# uses, because -json implies -v and would bury the routine run in per-test noise
# while carrying the identical per-package elapsed time.
#
# Usage:
#   go test -race -count=1 ./...        | bash scripts/pkg_time_budget.sh
#   go test -race -count=1 -json ./...  | bash scripts/pkg_time_budget.sh
#
# Environment:
#   SOFT_BUDGET   seconds; packages above this emit a ::warning:: (default 60)
#   HARD_BUDGET   seconds; packages above this fail the gate (exit 1).
#                 0 disables the hard ceiling (warn-only). Default 0.
#   PKG_HARD_BUDGET_OVERRIDES
#                 optional, documented per-package hard-ceiling overrides.
#                 Whitespace- or comma-separated "path-suffix=seconds" entries;
#                 a package whose import path ENDS WITH a key uses that key's
#                 ceiling instead of HARD_BUDGET. A suffix names exactly one
#                 package: substring matching would let "/cypher" also cover
#                 cypher/tck, cypher/ir and bench/cypher_scale. This is a
#                 justified, per-package accommodation for a package that is legitimately
#                 heavy under -race — NOT a blanket relaxation — mirroring
#                 cover_gate.sh's COVER_PKG_FLOOR_EXEMPT. Each override must be
#                 justified in docs/test-layers.md. Example:
#                   PKG_HARD_BUDGET_OVERRIDES="/internal/sim=780 /cypher=360"
#
# Exit codes:
#   0  no package exceeds HARD_BUDGET (or HARD_BUDGET disabled)
#   1  one or more packages exceeded HARD_BUDGET
#   2  no timing data parsed (the stream was empty or malformed). This is a
#      FAILURE, not a pass: a budget gate that silently checked nothing is
#      indistinguishable from one that found nothing, and the whole point of
#      putting it on the routine path is that it cannot quietly stop asserting.
#
# The script never re-runs the tests; it only summarises timings already
# produced by the upstream `go test` invocation whose output it is echoing, so it
# adds no test execution cost of its own. That is what lets it sit on the routine
# gate: enforcing the budget costs one pipe, not a second suite.
set -euo pipefail

export SOFT_BUDGET="${SOFT_BUDGET:-60}"
export HARD_BUDGET="${HARD_BUDGET:-0}"

exec python3 -c '
import json, os, re, sys

soft = float(os.environ.get("SOFT_BUDGET", "60"))
hard = float(os.environ.get("HARD_BUDGET", "0"))

# Parse the documented per-package hard-ceiling overrides. Each entry is a
# "path-substring=seconds" pair; a package whose import path contains the
# substring uses that ceiling instead of the global HARD_BUDGET. See the
# header comment and docs/test-layers.md for the justification requirement.
overrides = []
for tok in os.environ.get("PKG_HARD_BUDGET_OVERRIDES", "").replace(",", " ").split():
    key, sep, val = tok.partition("=")
    if not sep or not key:
        continue
    try:
        overrides.append((key, float(val)))
    except ValueError:
        continue

def hard_for(pkg):
    """Effective hard ceiling for pkg: the first matching override, else HARD_BUDGET.

    A key matches as a SUFFIX of the import path, not as a substring. Substring
    matching silently over-reaches: "/cypher" would also cover ".../cypher/tck",
    ".../cypher/ir" and ".../bench/cypher_scale", handing an unrelated package a
    ceiling nobody measured for it. A suffix names exactly one package, so every
    accommodation stays visible and deliberate.
    """
    for key, secs in overrides:
        if pkg == key or pkg.endswith(key):
            return secs
    return hard

# The go test output is PASSED THROUGH verbatim, so this script can sit on the
# `make test-short` pipeline without swallowing the output of the suite. A filter
# that ate its input would have made every test failure illegible, which is the
# single reason the budget gate could not previously ride on the routine gate.
# Line buffering keeps the output live rather than arriving in one block at the
# end.
sys.stdout.reconfigure(line_buffering=True)

# Both stream formats are accepted, detected per line rather than by a flag:
#   * `go test -json` events (what `make test-short-timings` produces), and
#   * the PLAIN "ok<TAB>pkg<TAB>0.330s" summary lines that an ordinary `go test`
#     already prints.
# Plain mode is what puts this gate on the routine path: `-json` implies -v, so
# piping the everyday `make test-short` through it would bury the run in per-test
# noise. The plain summary carries the identical per-package elapsed time, so the
# gate rides along with NO change to what the developer sees.
plain_re = re.compile(r"^(?:ok|FAIL)\s+(\S+)\s+([0-9.]+)s")

times = {}
for raw in sys.stdin:
    line = raw.strip()
    if not line or line[0] != "{":
        # Not a -json event: echo it verbatim — dropping it would hide exactly
        # the diagnosis the reader needs — and mine it for a plain summary line.
        sys.stdout.write(raw)
        m = plain_re.match(line)
        if m:
            times[m.group(1)] = float(m.group(2))
        continue
    try:
        ev = json.loads(line)
    except ValueError:
        sys.stdout.write(raw)
        continue
    if ev.get("Action") == "output":
        sys.stdout.write(ev.get("Output", ""))
    # Package-level total: a pass/fail event with no "Test" field.
    if ev.get("Action") in ("pass", "fail") and not ev.get("Test") \
            and "Elapsed" in ev and ev.get("Package"):
        times[ev["Package"]] = ev["Elapsed"]

if not times:
    sys.stderr.write("pkg_time_budget: no package timings parsed from the stream "
                     "(neither -json events nor plain ok/FAIL summary lines). "
                     "The budget was NOT checked.\n")
    sys.exit(2)

ordered = sorted(times.items(), key=lambda kv: -kv[1])
print("-- short-layer per-package timings (slowest 10) --")
for pkg, el in ordered[:10]:
    print("  %7.1fs  %s" % (el, pkg))

over_soft = [(p, e) for p, e in ordered if e > soft]
over_hard = [(p, e) for p, e in ordered if hard_for(p) > 0 and e > hard_for(p)]

if over_soft:
    print()
    print("::warning::%d package(s) exceed the %.0fs short-layer budget (docs/test-layers.md):" % (len(over_soft), soft))
    for pkg, el in over_soft:
        print("  %.1fs  %s" % (el, pkg))

if over_hard:
    print("::error::%d package(s) exceed their hard per-package ceiling — split the package or move slow tests to the soak layer:" % len(over_hard))
    for pkg, el in over_hard:
        print("  %.1fs > %.0fs  %s" % (el, hard_for(pkg), pkg))
    sys.exit(1)
'
