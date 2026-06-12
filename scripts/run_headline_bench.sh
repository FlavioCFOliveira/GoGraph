#!/usr/bin/env bash
# Runs the headline benchmarks used by the CI regression gate.
# Each benchmark is run with -count=6 — benchstat requires at least 6
# samples to compute a confidence interval; fewer produce "need >= 6
# samples" warnings and suppress the percentage delta entirely.
# -benchmem is included so allocation regressions show alongside ns/op.

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

go test -run='^$' -bench="$REGEX" -benchmem -count=6 "${PACKAGES[@]}"
