# ADR-0004: Leiden parallelism (deferred)

## Status

Accepted (deferred) — rmp gograph #1714, filed 2026-06-24, status BACKLOG,
user decision recorded.

## Context

`search/community/leiden.go` documents an explicit determinism contract:
Leiden's local-move and refinement phases visit nodes in `NodeID` order,
with a fixed "first candidate community wins" tie-break rule for equal
modularity-gain moves. The godoc states this makes the algorithm
"bit-for-bit reproducible" across runs on the same input, same machine, and
same Go toolchain (`search/community/leiden.go:91` and surrounding
comments). By the 2026-06-24 performance audit, Leiden is the module's
heaviest analytic and its last remaining sequential one: 1.43s / 96.5MB / 0
goroutines on the audited workload (`docs/audit-performance-2026-06-24.md:123`).

Parallelizing Leiden's local-moving phase was evaluated as a performance
lever. The state-of-the-art parallel Leiden formulations in the literature
(GVE-Leiden; Sahu, arXiv:2312.13936) use asynchronous, per-thread hash
tables during local-move, and are non-deterministic by construction — this
is the well-known "swap problem" in parallel community detection (see also
Lu/Halappanavar, Grappolo): when two threads concurrently evaluate moves
for adjacent nodes into each other's communities, the result depends on
which thread's move commits first, which is a function of goroutine
scheduling, not input data.

## Decision

GoGraph keeps `Leiden` fully sequential and bit-for-bit deterministic, as
currently documented. A parallel variant is **not** implemented as a
drop-in replacement or as a switch on the existing `Leiden` entry point.

If a parallel variant is built, it is required to be a **separate, new
public API** (e.g. `LeidenParallel`), explicitly documented as
quality-equivalent but **not** deterministic, so that callers who depend on
the existing bit-for-bit contract are never silently affected. Building
that variant remains deferred; `LabelPropagation` — already roughly 2400x
cheaper than Leiden and producing the same `Partition` shape — is the
project's fast-approximate community-detection tier for callers who need
speed over the determinism guarantee today.

## Rationale

Parallel local-moving cannot preserve Leiden's bit-identical serial output
without abandoning the algorithmic technique (async per-thread state) that
makes it fast — the swap problem is structural, not an implementation
defect that better engineering removes. Introducing parallelism into the
existing `Leiden` function would therefore silently break a guarantee its
own godoc promises today, which is a determinism-contract change, not a
performance-only change, and requires explicit user sign-off rather than
being treated as a routine optimization.

The user's recorded decision is to preserve the existing determinism
contract on `Leiden` rather than weaken it, and to rely on
`LabelPropagation` for the fast/approximate tier in the meantime.

## Consequences

- `Leiden` remains single-threaded; its cost does not improve from this
  avenue. Callers needing lower latency on large graphs use
  `LabelPropagation` and accept its coarser community-quality trade-off.
- No change to `search/community/leiden.go`'s public contract or its
  concurrency-safety documentation ("safe to invoke from any number of
  goroutines on a shared CSR" continues to describe concurrent *callers* on
  independent inputs, not internal parallelism within one call).
- This decision does not affect TCK or ACID compliance (ADR-0001): Leiden
  is an analytics algorithm outside Cypher execution and outside the
  transactional write path.
- Revisiting this decision requires: (a) explicit user approval to ship a
  new, separately named, non-deterministic-but-quality-equivalent API, and
  (b) a modularity-within-epsilon quality battery across seeds to certify
  that the async variant's output quality does not regress versus serial
  Leiden, per the acceptance criteria already recorded on the deferred
  ticket.

## Evidence / links

- `search/community/leiden.go:80-99` — determinism contract godoc (node
  order, tie-break rule, bit-for-bit reproducibility claim).
- `docs/audit-performance-2026-06-24.md:123,135,149,153` — Leiden identified
  as the last sequential heavy analytic (1.43s/96.5MB/#1713-#1714).
- Traag et al., 2019, "From Louvain to Leiden: guaranteeing well-connected
  communities" — the serial algorithm GoGraph implements.
- Sahu, "GVE-Leiden: Fast Leiden Algorithm for Community Detection in
  Shared Memory Setting," arXiv:2312.13936 — async parallel formulation and
  its non-determinism.
- Lu, Halappanavar, et al., Grappolo — parallel modularity-based clustering
  literature describing the swap problem.
- rmp gograph task #1714 — `Leiden parallel: separate LeidenParallel with
  documented non-bit-identical contract (USER DECISION)`, status BACKLOG,
  records the rejected-alternative rationale and acceptance criteria for a
  future `LeidenParallel` verbatim.
