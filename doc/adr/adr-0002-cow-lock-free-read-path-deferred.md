# ADR-0002: Copy-on-write lock-free read path (deferred)

## Status

Accepted (deferred, not justified by measurement) — rmp gograph #1671,
closed 2026-06-26.

## Context

`docs/isolation-design.md` (design F3) specifies a copy-on-write read path
for the engine: readers would load an `atomic.Pointer[Snapshot]` instead of
taking the visibility barrier `visMu.RLock()`, giving lock-free reads and
strengthening isolation from per-statement read-committed to per-statement
snapshot isolation. The current implementation instead has readers take
`visMu` as a shared `RLock` (lock order `store-mutex → visMu`, matching
`Begin` → `ApplyAtomically`; see `docs/isolation-design.md:274-288`).

The multithread audits of 2026-06-22/23 (`docs/audit-multithread-2026-06-22.md`,
`docs/audit-multithread-2026-06-23.md`) flagged this as the highest-leverage
open concurrency item (M2 / #1671), reporting that a single steady writer
made reads up to 2.4-2.8x slower (`docs/audit-multithread-2026-06-22.md:45-46`,
`docs/audit-performance-2026-06-24.md:69,192`), and recommended the COW
snapshot as "the only sound cure."

Before committing to that multi-sprint rewrite, the recommendation was
tested empirically per the project's measure-to-decide principle: a
read-scaling characterization benchmark
(`graph/lpg/readscale_1671_bench_test.go`) was built and run at 1, 8, 64,
and 256 goroutines, with and without a concurrent writer.

## Decision

GoGraph does **not** implement the copy-on-write `atomic.Pointer[Snapshot]`
read path. Readers continue to take `visMu.RLock()`. The design remains
recorded in `docs/isolation-design.md` (F3) as a specified-but-not-built
option, and the characterization benchmark that disproved its urgency is
kept as a permanent perf-regression gate.

This decision is deferred, not rejected outright: it is revisited if a real
workload demonstrates an actual read bottleneck, as opposed to the
theoretical one the earlier audit projected.

## Rationale

The benchmark showed `visMu.RLock` is a **shared** lock: it does not
serialize concurrent readers against each other. Reader-only scaling is a
bounded plateau, not the "collapses past 4 cores" cliff the 2026-06-22/23
audits' framing implied — measured at 1788 -> 3332 -> 3400 -> 3713 ns/op at
1/8/64/256 goroutines (one cache-line step, then flat). With a background
writer present, the penalty is bounded (4659 -> 3843 ns/op) and does not
worsen as reader count grows.

Against that modest, bounded measured cost, the full COW cut is an
indivisible, multi-thousand-line core-ACID re-architecture: it requires
per-shard structural sharing across roughly a dozen entangled
substructures, including two high-risk in-place-mutation rewrites (the
roaring-bitmap `label.Index` and the `index.Manager` subscriber set). Its
isolation correctness cannot be fully certified by automated gates (TCK,
`-race`) alone — it would need manual, structural review at a scope
disproportionate to the measured gain. The storage-engine-auditor gave only
a conditional go for the full, all-or-nothing cut, not for a partial one.

Given a bounded, tolerable measured penalty and a maximal-risk, all-or-
nothing remedy, the user decision (2026-06-26) was to defer the epic and
keep the disproving benchmark as living evidence, rather than build the
epic to solve a problem the data does not show at the scale tested.

## Consequences

- Reads and writes on `lpg.Graph` continue to share the `visMu` barrier;
  isolation stays at per-statement read-committed, not snapshot isolation.
- No new isolation-correctness surface is introduced, so ADR-0001's ACID
  mandate carries zero incremental risk from this decision.
- The spike's snapshot scaffolding (`snapshot.go`) was dropped as dead
  code; only the benchmark was integrated into the codebase
  (`graph/lpg/readscale_1671_bench_test.go`, part of commit `25524c0` on
  `main`).
- If a real production workload later shows read throughput degrading
  materially beyond what this benchmark characterizes — e.g., the plateau
  no longer holds, or writer presence pushes latency past an acceptable
  bound — the F3 design in `docs/isolation-design.md` is the specified
  starting point for revisiting this decision. Until then, re-proposing
  the COW cut without new measured evidence is re-litigation, not new
  information.

## Evidence / links

- `docs/isolation-design.md` — F3 design specification (not implemented).
- `docs/isolation-design.md:274-288` — current lock order and rationale for
  the existing `visMu`-based read path.
- `docs/audit-multithread-2026-06-22.md:29,45-46,121-160` — original M2/#1671
  finding and "2.6x slower" / "only sound cure" framing.
- `docs/audit-multithread-2026-06-23.md` — follow-up characterization.
- `docs/audit-performance-2026-06-24.md:69,192` — restatement of the
  2.4-2.8x reader-under-writer penalty prior to the disproving benchmark.
- `graph/lpg/readscale_1671_bench_test.go` (commit `25524c0`, `main`) — the
  benchmark that measured the bounded plateau and closed the epic.
- rmp gograph task #1671 — `Copy-on-write atomic.Pointer[Snapshot] lock-free
  read path (isolation-design.md F3)`, status COMPLETED, closed
  2026-06-26T11:21:21Z, completion summary records the full rationale
  above including the storage-engine-auditor's conditional-go-for-full-cut
  finding.
