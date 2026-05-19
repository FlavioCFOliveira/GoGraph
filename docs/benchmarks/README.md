# Benchmarks

This directory archives concrete benchmark numbers for every GoGraph
release. The intent is twofold:

1. **Reproducibility** — the numbers in the README and release notes
   come from documented runs on documented hardware, not from
   one-off measurements.
2. **Regression tracking** — a release-over-release comparison
   (e.g. v1.1.0 vs v1.0.1) is one diff against the previous file
   in this directory.

## Convention

Each release ships its own file: `<version>.md`. The file
contains:

- the run environment (OS, CPU, Go version, GOMAXPROCS, frequency
  governor),
- the command line(s) used,
- a table of benchmarks with `ns/op`, `B/op`, `allocs/op`, and (when
  the benchmark is parallel-capable) numbers at multiple
  concurrency levels (1, 8, 64, 256, 1024 goroutines via
  `b.RunParallel` + GOMAXPROCS overrides).

## Releases

- [v1.0.0.md](v1.0.0.md) — headline benchmark numbers for the v1.0.0
  tag.
- [v1.1.0.md](v1.1.0.md) — 30-minute reliability soak run for the
  v1.1.0 tag (canonical 4-hour run tracked for the next release).
- [SOAK.md](SOAK.md) — procedure and acceptance gate for the
  mixed-workload reliability soak.
- [comparison.md](comparison.md) — cross-library performance
  comparison vs NetworkX.
