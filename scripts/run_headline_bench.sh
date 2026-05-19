#!/usr/bin/env bash
# Runs the headline benchmarks used by the CI regression gate.
# Each benchmark is run with -count=5 so benchstat has enough samples
# for a confidence interval; -benchmem is included so allocation
# regressions show up alongside ns/op deltas.

set -euo pipefail

# The set of benchmarks is a deliberately small fixed list — adding
# more here lengthens every PR. Only benchmarks that exercise hot-
# path-critical algorithms belong on this list.
PATTERNS=(
  "BenchmarkDijkstra_PostWarmup"
  "BenchmarkDijkstra_Large"
  "BenchmarkBrandes_RandomGraph"
  "BenchmarkBFSDirectionOpt_PowerLaw"
  "BenchmarkYen_K100"
)

REGEX="^($(IFS='|'; echo "${PATTERNS[*]}"))$"

# The packages that host the headline benchmarks. Keeping the list
# explicit avoids running every benchmark in every package.
PACKAGES=(
  "./search/..."
  "./search/centrality/..."
)

go test -run='^$' -bench="$REGEX" -benchmem -count=5 "${PACKAGES[@]}"
