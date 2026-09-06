# rmp #2763 — shortest-path db-hits counter, raw A/B data

> **Note on the distribution archive.** The raw data files this document points at
> live in the GoGraph repository and are deliberately **not** shipped in the release
> tarball, which carries Markdown only (`.goreleaser.yaml`, rmp #2758). Read them at
> the tag in git; this README travels with the archive so the method is recorded even
> where the data is not.


Interleaved A/B measurements for the storage-access counter added to
`exec.ShortestPath` and `exec.AllShortestPaths`.

Benchmarks: `cypher/exec/shortest_path_bench_test.go`, seven arms chosen to bracket
the design — a unit-degree chain (worst case: one charge per slot), a layered BFS at
out-degree 8, and a single 20 000-slot run (best case: one charge per walk).

Method: `rmp2763-ab.sh <armA> <armB> <outA> <outB> <rounds> <tag>` swaps
`cypher/exec/{shortest_path.go,shortest_path_bidir.go,profile.go}` between two saved
variants and alternates A/B/A/B, recording loadavg before and after every invocation
and each `go test` exit code. `-count=1 -benchtime=300ms -benchmem`, no `-race`.

Host: Apple M4, 10 cores, darwin/arm64, go1.27.1. **loadavg 2.4–2.9 throughout — the
host was NOT idle, so no timing claim is made.** Allocation counts are exact and
load-invariant and carry the verdict.

| file | comparison |
|---|---|
| `benchstat-noisefloor.txt` | base vs base, identical source — the calibration. Produced a significant `-1.57%` of its own. |
| `benchstat-counter-off-vs-on.txt` | **the counter alone**: same refactor with and without the `slotsRead +=` charge. |
| `benchstat-base-vs-head.txt` | pre-change baseline vs HEAD. |
| `benchstat-base-vs-refactor-only.txt` | baseline vs HEAD with the charge removed — attributes the allocation delta away from the counter. |
| `benchstat-base-vs-biscan-loopform.txt` | baseline vs baseline + only `biScan`'s loop rewritten — locates the delta. |
| `benchstat-base-vs-hoist-only.txt` | baseline vs baseline + only the loop bound hoisted — refutes the hoist as the cause. |

`exits_*.log` holds every `go test` exit code (all 0); `loadavg_*.log` holds the
before/after load for every invocation.
