# ADR-0003: Parallel checkpoint component encode (deferred)

## Status

Accepted (deferred) — rmp gograph #1712, filed 2026-06-24, status BACKLOG.

## Context

`store/snapshot`'s `writeSnapshotFullCore` writes a checkpoint's components
(CSR, labels, properties, mapper, tombstones, edge handles, constraints,
indexes) sequentially, each via its own `writeAndSync` call. Each component
takes read-only inputs — the immutable CSR captured under the commit lock
in phase 1, plus concurrent-safe accessors for the live graph — writes a
distinct file, and returns an independent `(size, crc)`: there is no shared
mutable writer state between components. Phase 2 (the actual encode) runs
lock-free, with the commit lock already released, so parallelizing the
per-component writes would shorten the checkpoint window and reduce disk
and GC contention with foreground commits during that window.

`docs/audit-performance-2026-06-24.md:149,153,184` identifies this as
`#1712`, explicitly filed as a bounded improvement to schedule after
`#1707` (the `writeAndSync`/`WriteProperties` cost reduction), not the
earlier, broader-scoped version of the same idea.

Two independent constraints on doing this now were found:

- **DST-simulator byte-determinism.** `#1546` backed the checkpoint path
  with `SimDisk`, whose `SimFileHandle.Write` draws per-sector
  fault-injection from a seeded RNG on every write call. If checkpoint
  writes componentize into concurrent goroutines, the order in which those
  writes draw from the shared seeded RNG becomes dependent on goroutine
  scheduling. Same-seed simulation runs would then no longer be
  byte-reproducible — breaking a core reliability property of the
  deterministic-simulation-testing harness (see `docs/dst.md`), which
  depends on identical seeds producing identical fault sequences and
  outcomes across runs.
- **Low leverage today.** `#1707` already cut the dominant
  `WriteProperties` component cost by roughly 81%, so the checkpoint
  window's remaining sequential cost is smaller than when the
  parallelization idea was first proposed; the checkpoint itself is an
  infrequent, lock-free background operation, not a foreground hot path.

## Decision

GoGraph does **not** parallelize checkpoint component encoding at this
time. `writeSnapshotFullCore` continues to write CSR, labels, properties,
mapper, tombstones, edge handles, constraints, and indexes sequentially in
phase 2.

Parallelizing is accepted as a future improvement, conditioned on an
filesystem seam that lets the checkpoint writer distinguish a real,
concurrent-write-safe OS filesystem (parallel-safe) from `SimDisk`
(sequential-only, to preserve seeded-fault-injection determinism), so that
parallelism is enabled only in production and never inside the DST
harness.

## Rationale

The components are structurally embarrassingly parallel — no shared
mutable state, independent outputs — so the correctness cost of
parallelizing the encode itself is low. The blocking cost is entirely
external: it would silently break the DST simulator's seeded
reproducibility contract, which is load-bearing for the project's crash
and reliability testing (ADR-0001). Trading that away for a bounded,
already-shrunk window-latency win is not justified without either (a) an
fs-seam that keeps `SimDisk` runs strictly sequential while allowing
production to parallelize, or (b) measured evidence that checkpoint-window
latency is an actual pain point, which does not currently exist.

## Consequences

- Checkpoint window latency remains bounded by the sequential sum of
  per-component encode costs, dominated now by whichever component `#1707`
  did not already address.
- The DST simulator's `SimDisk`-backed seeded fault injection
  (`docs/dst.md`) stays byte-reproducible across runs, which the
  crash-injection and recovery battery in ADR-0001 depends on.
- Revisiting this decision requires either building the fs-seam
  (production-only parallel writer vs. simulation-only sequential writer)
  or new evidence that checkpoint latency, not allocation cost, is the
  bottleneck under a real workload.

## Evidence / links

- `store/snapshot/` — `writeSnapshotFullCore` and per-component
  `writeAndSync` implementation.
- `docs/audit-performance-2026-06-24.md:149,153,184` — filing of `#1712`
  as the correctly scoped version of this idea, after `#1707`.
- `docs/dst.md` — deterministic-simulation-testing harness and its
  reliance on seeded, reproducible fault injection.
- rmp gograph task #1712 — `Parallelize checkpoint component encode
  (bounded; AFTER WriteProperties fix 1707)`, status BACKLOG, technical
  requirements record the DST-determinism blocker and the fs-seam
  condition for revisiting verbatim.
