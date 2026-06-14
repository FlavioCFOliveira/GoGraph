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
# -coverpkg=./... attributes coverage of EVERY package to whichever test
# exercises it, not just that package's own _test.go files. The query engine
# (cypher/...) is validated overwhelmingly by the openCypher TCK suite and the
# integration tests that live in OTHER packages; without -coverpkg those hits
# are discarded and the engine packages read far below their true coverage.
# Crediting cross-package coverage is the accurate measure of how well the
# library is tested. The trade-off is a slower instrumented run, hence the
# generous timeout.
if ! "${GO}" test -coverpkg=./... -coverprofile="${COVER_PROFILE}" -covermode=atomic -timeout=20m ./... >"${COVER_TEST_LOG}" 2>&1; then
  echo "cover_gate: 'go test' failed during coverage profile generation; failing output:" >&2
  grep -E '(^--- FAIL|^FAIL[[:space:]]|panic:|fatal error:|_test\.go:[0-9]+:|signal:|DATA RACE)' "${COVER_TEST_LOG}" | tail -80 >&2 || true
  echo "---- last 40 lines of go test output ----" >&2
  tail -40 "${COVER_TEST_LOG}" >&2
  exit 1
fi

echo "==> filtering non-library packages: ${COVER_EXCLUDE}"
{
  head -n1 "${COVER_PROFILE}"
  grep -E -v "${COVER_EXCLUDE}" "${COVER_PROFILE}" | tail -n +2
} > "${COVER_LIB_PROFILE}"

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
