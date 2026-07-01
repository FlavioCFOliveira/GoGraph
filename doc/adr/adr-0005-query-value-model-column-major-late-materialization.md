# ADR-0005: Query value-model direction: column-major, late materialization

## Status

Accepted (planned) — rmp gograph #1704 (epic) with phased subtasks
#1822-#1826, filed 2026-06-24 / phased 2026-07-01, status BACKLOG.

## Context

Cypher query execution represents every runtime scalar as `expr.Value`, an
interface (`cypher/expr/value.go:101`, `Kind()`/`Equal()`/`Hash()`/
`String()`). Concrete implementations box every `int64` and `float64` that
flows through a row: `lpgPropToExpr` boxes each numeric property read from
storage, and `AllNodesScan.Next` boxes each `NodeID` above the small-int
cache threshold as an `IntegerValue`. After the S-PA7 sound-allocation wins
(#1700 registry COW, #1702 scan arena, #1703 `ResultSet` field reuse), heap
profiling still attributed the majority of remaining per-row allocation
cost in the projection path to this interface boxing — roughly 40% of
projection allocations from `lpgPropToExpr` numeric boxing, roughly 17%
from `AllNodesScan` integer boxing — leaving the `EngReadProject` benchmark
at **5306 allocs/op** even after those fixes (rmp task #1704, functional
requirements).

Two rejected alternatives were considered and set aside before choosing the
approach below:

- **A per-row tagged-union `Value`.** Replacing the interface with a Go
  struct carrying a kind tag plus a union of possible payloads was
  rejected: in Go, a tagged union sized to hold every variant (including a
  `string`/slice header for text, list, map, node/relationship references,
  and multi-field temporal types) runs to roughly 40 bytes per value
  regardless of which variant is active, since Go has no true union
  layout — every arm's storage is reserved whether used or not. This
  trades interface-dispatch and heap-escape cost for wasted storage on
  every row and pressure toward `unsafe` reinterpretation to avoid that
  waste, which conflicts with the module's idiomatic-Go and hot-path
  discipline. It was rejected in favor of an approach that avoids
  per-value boxing entirely rather than shrinking the box.
- **Leaving scalars boxed and only optimizing the boxing call sites.**
  Already attempted (S-PA7); it reduced allocation count but could not
  eliminate the structural per-row interface allocation, because the
  `Value` interface itself is the boxing point — any `int64`/`float64`
  routed through it escapes to the heap. The residual 5306 allocs/op is
  evidence this approach has reached its ceiling.

The winning direction draws on the column-major, late-materialization
architecture used by vectorized analytical engines — DuckDB's `DataChunk`
and the X100/MonetDB/Vectorwise lineage of vectorized execution — where a
batch of rows is represented as parallel typed columns
(`[]int64`, `[]float64`, plus a validity bitmap for `NULL`) and a value is
only converted to its dynamically-typed wire/user-facing representation at
the point where it is actually needed (the sink), not at every intermediate
operator boundary.

## Decision

GoGraph adopts a **column-major, late-materialization** execution model
for scalar values in the Cypher engine, replacing per-row interface boxing
of `int64`/`float64` scalars in the hot read path. Concretely:

1. Introduce a `Chunk` batch type: per output column, a typed backing store
   (`[]int64`, `[]float64`, `[]string`, ...) plus a validity bitmap for
   `NULL`, with `Kind` retained as a column-level tag rather than a
   per-value tag (#1822).
2. Rewire the projection operator's scalar path to write directly into
   `Chunk` typed columns; box into `expr.Value` **only at the sink** — the
   final projection materialization or the Bolt wire encode — never at
   intermediate operator boundaries (#1823).
3. Extend the chunked path to the operators adjacent to projection —
   `Filter`, `WITH` projection, aggregation grouping keys — so that
   converted operator chains do not re-box at each boundary (#1824).
4. Represent node/relationship values as lazy typed columns inside the
   `Chunk` (reusing the existing `LazyNodeValue` deferred-materialization
   semantics), so entity-returning projections (`RETURN n`, `RETURN r`)
   also avoid per-row boxing (#1825).
5. Encode Bolt `RECORD` messages directly from `Chunk` columns in bounded
   morsels, so large result sets stream with bounded peak memory instead
   of materializing the full boxed result set, pairing this with the
   existing `docs/result-streaming-design.md` design (#1826).

Work proceeds phased and reversible behind the existing operator interface:
each phase lands independently, is `benchstat`-gated against the prior
baseline, and must hold TCK 3897 (ADR-0001) including temporal scenarios —
`lpgPropToExpr`'s decoding of SOH-tagged temporal strings
(`decodeTemporalString`) must be preserved exactly, and Bolt wire/`Record()`
bytes must stay byte-identical throughout.

## Rationale

Column-major layout removes the boxing point structurally rather than
optimizing around it: a typed column stores `int64`/`float64` values
contiguously and unboxed, so no per-value heap allocation or interface
dispatch occurs for the scalar itself; the value becomes a `Value` only
once, at the sink, where a dynamic representation is actually required
(user-facing rows, Bolt wire encoding). This is the same structural insight
that lets DuckDB's `DataChunk`-based vectorized execution avoid per-tuple
boxing in an analytical engine — the problem GoGraph's `EngReadProject`
benchmark exhibits is the same one vectorized columnar engines solve by
construction, not a Go-specific quirk. Phasing the rollout per operator
with a `benchstat` gate at each step lets each phase be independently
justified by measurement (per the project's measure-to-decide principle)
rather than committing to the full cross-cutting rewrite atomically.

The tagged-union alternative was rejected because Go's lack of true union
layout makes it a fixed ~40-byte cost per value regardless of the active
variant — worse steady-state memory density than the interface it would
replace — and because closing that gap would require `unsafe`
reinterpretation of the union's backing bytes, which the module's hot-path
discipline permits only when clearly justified; a column-major design
avoids the trade-off entirely by not needing a uniform per-value
representation in the hot path at all.

## Consequences

- The `expr.Value` interface (`cypher/expr/value.go:101`) is retained as
  the dynamically-typed representation at the boundary (sink) and wherever
  values must be treated uniformly regardless of static type — it is not
  removed, only pushed later in the pipeline for hot scalar paths.
- Operators not yet converted keep a row-at-a-time fallback, so the
  migration does not require converting the whole operator set before any
  benefit is realized; correctness at each intermediate phase depends on
  this row/columnar boundary being exact (no partial conversion silently
  changing semantics).
- Every phase must independently satisfy ADR-0001: TCK 3897 including
  temporal scenarios, and no observable change to Bolt wire bytes or the
  ACID read path. This is a performance/memory-layout change, not a
  semantic one, and each phase is expected to be certified as such by the
  cypher-expert-consultant per commit.
- Completing #1822-#1826 is expected to bring `EngReadProject` down from
  ~5306 allocs/op to "low hundreds" (rmp #1704/#1823 acceptance criteria);
  the actual figure at each phase is recorded via `benchstat` against the
  baseline captured in #1822, not asserted here.
- This epic pairs with, but does not depend on, ADR-0002 (COW lock-free
  read path): both target the read path, but the value-model change is
  about representation and allocation, while ADR-0002 is about
  synchronization; they are independently justifiable and independently
  deferred/planned.

## Evidence / links

- `cypher/expr/value.go:101` — the `Value` interface definition (kept as
  the sink-side representation).
- `cypher/exec/row.go` — current row representation that the `Chunk`
  columnar batch type extends/replaces for scalar columns.
- `docs/audit-performance-2026-06-24.md:66,135,149` — de-boxing `lpgPropToExpr`
  and related allocation findings that motivated this epic.
- `docs/result-streaming-design.md` — bounded-morsel streaming design that
  #1826 (Bolt encode from `Chunk` columns) extends.
- rmp gograph task #1704 — `Typed/columnar scalar value representation:
  eliminate per-row interface boxing (value-model epic)`, records the
  5306-allocs/op baseline, the rejected tagged-union alternative, and the
  DuckDB `DataChunk` / late-materialization reference point verbatim.
- rmp gograph tasks #1822-#1826 — the five phased subtasks (`Chunk` type;
  unbox scalar projection; extend to Filter/WITH/aggregation; node/rel lazy
  columns; chunked Bolt streaming), each carrying its own acceptance
  criteria and specialist sign-off requirement (columnar-db-expert,
  cypher-expert-consultant, go-developer, storage-engine-auditor).
