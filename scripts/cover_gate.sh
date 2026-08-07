#!/usr/bin/env bash
# cover_gate.sh - enforce repository coverage thresholds.
#
# Two gates are applied to the library coverage profile (non-library
# subtrees - examples/, cmd/, bench/ - are stripped before computation):
#
#   1. Aggregate statement coverage MUST be >= ${MIN_TOTAL}% (default 85.0).
#   2. Every retained package MUST be >= ${MIN_PER_PKG}% (default 75.0).
#
# Per-package coverage matches the statement-weighted methodology used
# by 'go test -cover': covered_statements / total_statements across
# every block recorded in the cover profile. The unweighted average of
# 'go tool cover -func' is NOT used here because it skews on packages
# that contain zero-statement methods (e.g. the no-op metrics backend).
#
# Exit codes:
#   0 - all gates green
#   1 - at least one gate failed (message on stderr)
#   2 - unexpected internal error
#
# Inputs (all optional):
#   COVER_PROFILE       output path for the raw profile  (default cover.out)
#   COVER_LIB_PROFILE   output path for the filtered profile (default cover.lib.out)
#   COVER_EXCLUDE       extended regex of package paths to drop
#                       (default: github.com/FlavioCFOliveira/GoGraph/(examples|cmd|bench/soak|bench/ldbc|bench/dimacs9|cypher/parser/gen))
#   MIN_TOTAL           aggregate threshold percentage     (default 85.0)
#   MIN_PER_PKG         per-package threshold percentage   (default 75.0)
#   GO                  go binary                          (default go)
#   COVER_TEST_LOG      log of the instrumented test run   (default cover.out.testlog)
#   COVER_FAIL_LOG      log kept when that run FAILS; carries the run's PID so
#                       a later run cannot overwrite it
#                       (default cover.out.failed.$$.log)
#   COVER_FAIL_LINES    cap on the failure summary printed to stderr (default 400)
#
# Compatibility: this script is portable across bash 3.2 (macOS
# default) and modern bash; no associative arrays are used. Numeric
# comparisons are routed through awk to avoid locale-dependent
# decimal separators (some macOS locales emit ',' instead of '.').

set -euo pipefail

GO=${GO:-go}
COVER_PROFILE=${COVER_PROFILE:-cover.out}
COVER_LIB_PROFILE=${COVER_LIB_PROFILE:-cover.lib.out}
COVER_EXCLUDE=${COVER_EXCLUDE:-'github.com/FlavioCFOliveira/GoGraph/(examples|cmd|bench/soak|bench/ldbc|bench/dimacs9|bench/cypher_ldbc|cypher/parser/gen)'}
MIN_TOTAL=${MIN_TOTAL:-85.0}
MIN_PER_PKG=${MIN_PER_PKG:-75.0}

# Packages kept in the AGGREGATE coverage figure but exempt from the
# per-package floor, because part of their code is structurally impossible to
# line-cover. internal/crashpoint is production crash-injection instrumentation
# (imported by store/wal and store/checkpoint); its sole uncovered statements
# are the `syscall.Kill(SIGKILL); select{}` firing body — the process dies
# before the Go runtime can flush coverage counters, so that block can never be
# credited by any test. Its non-firing guard/arming logic IS covered (60% is
# the hard ceiling). The firing path is verified behaviourally by the
# subprocess crash-injection tests (internal/crashpoint, store/recovery).
COVER_PKG_FLOOR_EXEMPT=${COVER_PKG_FLOOR_EXEMPT:-'github.com/FlavioCFOliveira/GoGraph/internal/crashpoint'}

# Force a deterministic numeric locale so awk prints '.' as the
# decimal separator regardless of the user's locale.
export LC_ALL=C

echo "==> generating coverage profile: ${COVER_PROFILE}"
# The instrumented test run's stdout (per-package ok/FAIL summary and any
# "--- FAIL" detail) is captured to a log instead of discarded, so that a test
# failure during profile generation is surfaced in CI rather than swallowed
# (a silent ">/dev/null" previously hid the cause of any failure here).
COVER_TEST_LOG=${COVER_TEST_LOG:-"${COVER_PROFILE}.testlog"}

# Where a FAILING run's log is kept. Distinct from COVER_TEST_LOG and carrying
# this run's PID, so that re-running the gate to chase a rare failure cannot
# destroy the record of it (rmp #2347).
COVER_FAIL_LOG=${COVER_FAIL_LOG:-"${COVER_PROFILE}.failed.$$.log"}
# Line cap on the failure summary printed to stderr. The complete log is always
# preserved and its path always printed, so this bounds noise, never evidence.
COVER_FAIL_LINES=${COVER_FAIL_LINES:-400}
# This script's own directory, so it can find failblocks.awk regardless of the
# caller's working directory.
_cover_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# CONCURRENCY SAFETY (rmp #2268). Every path this script writes used to be a
# fixed name in the repository root, so two `make ci` runs in the same checkout
# interleaved their writes into one profile; the gate then died parsing a
# package name spliced from both ("gigithub.com/..."), reporting a corrupt
# profile as a coverage failure. Each run now writes to paths carrying its own
# PID and publishes them with an atomic same-directory rename, so a concurrent
# run can at worst overwrite a COMPLETE profile with another COMPLETE profile,
# never splice two together. The published names are unchanged, so anything
# that reads cover.out afterwards still works.
_cover_tmp_suffix=".tmp.$$"
_cover_profile_tmp="${COVER_PROFILE}${_cover_tmp_suffix}"
_cover_testlog_tmp="${COVER_TEST_LOG}${_cover_tmp_suffix}"
_cover_lib_tmp="${COVER_LIB_PROFILE}${_cover_tmp_suffix}"
cleanup_cover_tmp() {
  rm -f "${_cover_profile_tmp}" "${_cover_testlog_tmp}" "${_cover_lib_tmp}" \
     "${COVER_PROFILE}.pub.$$" "${COVER_LIB_PROFILE}.pub.$$"
}
trap cleanup_cover_tmp EXIT
# -coverpkg=./... attributes coverage of EVERY package to whichever test
# exercises it, not just that package's own _test.go files. The query engine
# (cypher/...) is validated overwhelmingly by the openCypher TCK suite and the
# integration tests that live in OTHER packages; without -coverpkg those hits
# are discarded and the engine packages read far below their true coverage.
# Crediting cross-package coverage is the accurate measure of how well the
# library is tested. The trade-off is a slower instrumented run, hence the
# generous timeout.
if ! "${GO}" test -coverpkg=./... -coverprofile="${_cover_profile_tmp}" -covermode=atomic -timeout=20m ./... >"${_cover_testlog_tmp}" 2>&1; then
  # PRESERVE THE EVIDENCE BEFORE PRINTING ANYTHING (rmp #2347). The log used to
  # be published to ${COVER_TEST_LOG}, which the NEXT run - green or not -
  # overwrites. A rare failure was therefore destroyed by the very re-run
  # performed to investigate it, which is exactly what happened to the ST3
  # durability sighting of 2026-08-07. A failing log now also lands under a
  # name carrying this run's PID, which no later run can clobber.
  cp -f "${_cover_testlog_tmp}" "${COVER_TEST_LOG}" 2>/dev/null || true
  mv -f "${_cover_testlog_tmp}" "${COVER_FAIL_LOG}" || true

  echo "cover_gate: 'go test' failed during coverage profile generation; failing output:" >&2
  # Whole failure BLOCKS, not just their first lines. See scripts/failblocks.awk
  # for what the previous line-pattern grep silently discarded.
  awk -v max="${COVER_FAIL_LINES}" -f "${_cover_script_dir}/failblocks.awk" "${COVER_FAIL_LOG}" >&2 || true
  echo "cover_gate: complete go test output preserved at: ${COVER_FAIL_LOG}" >&2
  exit 1
fi
mv -f "${_cover_testlog_tmp}" "${COVER_TEST_LOG}"

echo "==> filtering non-library packages: ${COVER_EXCLUDE}"
# Every gate below reads the run's OWN temporary profiles. The published names
# are written only once both are complete, so no reader in this script can ever
# observe a profile a concurrent run is midway through replacing.
{
  head -n1 "${_cover_profile_tmp}"
  grep -E -v "${COVER_EXCLUDE}" "${_cover_profile_tmp}" | tail -n +2
} > "${_cover_lib_tmp}"
# Publish via copy-then-rename: `cp` alone is not atomic, so a concurrent reader
# of cover.out could otherwise observe a half-written file.
cp -f "${_cover_profile_tmp}" "${COVER_PROFILE}.pub.$$" && mv -f "${COVER_PROFILE}.pub.$$" "${COVER_PROFILE}"
cp -f "${_cover_lib_tmp}" "${COVER_LIB_PROFILE}.pub.$$" && mv -f "${COVER_LIB_PROFILE}.pub.$$" "${COVER_LIB_PROFILE}"
# From here on the gates read the temporaries, which nothing else can touch.
COVER_LIB_PROFILE="${_cover_lib_tmp}"

total_pct=$("${GO}" tool cover -func="${COVER_LIB_PROFILE}" | awk '/^total:/ { sub("%", "", $NF); print $NF }')
if [[ -z "${total_pct}" ]]; then
  echo "cover_gate: failed to parse aggregate coverage from ${COVER_LIB_PROFILE}" >&2
  exit 2
fi

echo "==> aggregate library coverage: ${total_pct}%"

# Compute per-package statement-weighted coverage directly from the
# raw block records in the profile. Each non-header line has the
# format "pkg/path/file.go:line.col,line.col stmts hits"; we sum
# stmts per package and stmts*(hits>0?1:0) per package.
per_pkg=$(
  awk '
    NR == 1 { next }              # skip "mode: atomic" header
    NF < 3 { next }
    {
      # With -coverpkg the same block can appear once per test binary that
      # instrumented it. Deduplicate by block id first (taking the max hit
      # count across binaries) so a block is counted exactly once per package;
      # otherwise the per-package denominator is inflated N-fold. This also
      # works correctly for a non -coverpkg profile (each block appears once).
      key   = $1                  # "pkg/path/file.go:line.col,line.col"
      stmts = $(NF - 1) + 0
      hits  = $NF + 0
      blkstmts[key] = stmts       # identical across duplicate entries
      if (hits > blkhits[key]) blkhits[key] = hits
    }
    END {
      for (key in blkstmts) {
        loc = key
        # Drop ":line.col,line.col" suffix to get pkg/path/file.go.
        sub(":[0-9]+\\.[0-9]+,[0-9]+\\.[0-9]+", "", loc)
        n = split(loc, parts, "/")
        pkg = parts[1]
        for (i = 2; i < n; i++) pkg = pkg "/" parts[i]
        total[pkg] += blkstmts[key]
        if (blkhits[key] > 0) covered[pkg] += blkstmts[key]
      }
      for (p in total) {
        if (total[p] == 0) {
          printf "%s 100.0\n", p
        } else {
          printf "%s %.1f\n", p, (covered[p] * 100.0) / total[p]
        }
      }
    }
  ' "${COVER_LIB_PROFILE}" | sort
)

echo "==> per-library-package coverage:"
echo "${per_pkg}" | awk '{ printf "    %-40s %s%%\n", $1, $2 }'

failed=$(
  echo "${per_pkg}" \
    | awk -v threshold="${MIN_PER_PKG}" -v exempt="${COVER_PKG_FLOOR_EXEMPT}" '
        $2 + 0 < threshold + 0 {
          if (exempt != "" && $1 ~ exempt) {
            print "    (exempt from floor) " $1 " " $2 "%" > "/dev/stderr"
            next
          }
          print $1 " " $2 "%"
        }'
)

agg_ok=$(awk -v a="${total_pct}" -v b="${MIN_TOTAL}" 'BEGIN { print (a + 0 < b + 0) ? 0 : 1 }')
status=0
if [[ "${agg_ok}" != "1" ]]; then
  echo "cover_gate: aggregate coverage ${total_pct}% < ${MIN_TOTAL}%" >&2
  status=1
fi
if [[ -n "${failed}" ]]; then
  fail_count=$(echo "${failed}" | wc -l | tr -d ' ')
  echo "cover_gate: ${fail_count} package(s) below ${MIN_PER_PKG}%:" >&2
  echo "${failed}" | awk '{ print "    " $0 }' >&2
  status=1
fi

if [[ "${status}" == "0" ]]; then
  echo "cover_gate: OK (aggregate ${total_pct}% >= ${MIN_TOTAL}%, all packages >= ${MIN_PER_PKG}%)"
fi
exit "${status}"
