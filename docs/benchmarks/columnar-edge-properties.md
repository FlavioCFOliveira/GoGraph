# Benchmarks — columnar edge-property tier (sprint 222)

Memory and build-allocation record for the columnar edge-property tier that
replaced the per-pair `map[edgeKey]propBag` edge-property store. Design:
[`../columnar-edge-properties-design.md`](../columnar-edge-properties-design.md).
This change targets **resident memory** of property-bearing edges; it does not
touch the time-based query benchmarks tracked in
[`history/LEDGER.md`](history/LEDGER.md) (the openCypher TCK is held at 3897/3897
and query results are byte-identical).

> Re-measure on your own hardware before relying on any absolute number; these
> are medians from documented runs, not a guarantee.

## Run environment

| Property | Value |
|----------|-------|
| Date | 2026-06-20 |
| CPU | Apple M4 (10-core) |
| OS / arch | macOS (darwin 25.5.0), darwin/arm64 |
| Go toolchain | go1.26.4 |
| Workload | `examples/26_social_scale_bench` at `-users 20000 -articles 2000 -seed 1` (≈6.5 M edges, every edge carries one ISO-8601 date property) |

## Commits

| Commit | Change |
|--------|--------|
| `30f221e` | design note (decision D1) |
| `225db26` | columnar core — typed columns + validity bitmap; map retired |
| `c9cd23a` | sparse COO columns for low-fill keys |
| `28bd14c` | fused property-at-edge-insertion build path |

## Resident memory (live heap, post-GC `runtime.ReadMemStats`)

| Store | Live heap @ 20k/2k | B/edge | vs. baseline |
|---|---:|---:|---:|
| `map[edgeKey]propBag` (baseline) | ~1020 MiB | ~157 | — |
| Columnar, dense columns (`225db26`) | ~465 MiB | ~74.8 | **−53 %** |
| Columnar + sparse COO (`c9cd23a`) | **~383 MiB** | **~61.8** | **−61 %** |

Extrapolated to the full example-26 specification (~3.25 × 10⁸ edges): the
live-heap ceiling falls from the baseline **~48 GiB to ~20 GiB**.

Attribution (heap profile, `inuse_space`): the baseline's ~128 B/edge property
store was ~65 B map slot + ~31 B one-element slice + ~17 B interface box + ~16 B
value. The columnar tier removes the map key, the slice, and the box; the sparse
COO representation removes the absent-slot string headers for the `since`/`when`
columns, which are each ~50 % sparse within a user's mixed `FRIEND`/`LIKE`
neighbour list.

## Build allocation (`BenchmarkBuild`, `-benchtime=1x -benchmem`, 20k/2k)

The naive columnar build regressed allocation because a per-edge
`SetEdgeProperty` copies the whole column (O(degree) → O(degree²) per source).
The fused `AddEdgeLabeledWithProperty` append path (`28bd14c`) restores it.

| Build path | B/op | allocs/op | ns/op |
|---|---:|---:|---:|
| `map[edgeKey]propBag` (baseline) | ~2.35 GB | ~26.6 M | — |
| Columnar, un-fused `SetEdgeProperty` (regression) | ~54.1 GB | ~90.8 M | ~12.5 s |
| Columnar, fused append (`28bd14c`) | **~4.24 GB** | **~40.3 M** | **~3.0 s** |

Final build allocation is within ~1.8× of the label-only baseline; the residual
is the immutable-snapshot copy-on-write block published per edge (O(1)/edge,
inherent to the lock-free read model).

## Conformance / safety gate

`go test ./...` 98 packages green · `go test -race` on `graph/adjlist`,
`graph/lpg`, `cypher` clean · openCypher TCK `TestTCKExecution` 3897/3897 ·
`golangci-lint` and `staticcheck` 0 issues · storage-engine ACID certification
(value-identity recovery test added).

## Deferred (tracked in the backlog)

- **`int32` epoch-day date column** — storing the date as a typed value rather
  than an ISO string would cut the property column from ~33 to ~4 bytes per
  present slot (measured in spike #1635).
- **`IS NOT NULL` popcount engine fast path** (#1638) — the storage-layer
  validity popcount exists and is tested; wiring it through the Cypher
  relationship path needs a presence-only/lazy read path.
- **Global dense edge-record** — gated on a universal edge id; suits
  edge-centric workloads.
