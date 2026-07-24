# Columnar/Vectorized Execution Deepening — Design (P5, F3)

Status: design spike (#2103, sprint 309). No production code changed by this
document. Specialist input: columnar-db-expert. Date 2026-07-24.

## 0. The chunk-chain rule (load-bearing)

> Boxing is removed **only on the contiguous `ChunkProducer` suffix** of the
> plan, measured from the sink (`materializeColumnar` asserts the root is a
> `ChunkProducer`) downward. Each `ChunkProducer.FillChunk` must pull its child
> via the **child's `FillChunk`** (type-asserting the child to `ChunkProducer`),
> never via `child.Next`. The first operator that pulls its child row-at-a-time
> re-boxes at that boundary, and everything below it is boxed regardless of how
> it is implemented (the verified #2065 finding).

Corollary — pipeline breakers (aggregation, hash-join build) **split** the
chain: the O(input-size) segment below them must be an unbroken chunk chain
consumed via `FillChunk` (the win); the O(output-size) segment above may be
row-mode (cheap). **Invariant to enforce: every O(input-size) edge of the plan
lies inside an unbroken chunk chain.** Today that holds sink →
`ColumnarProject` → `ColumnarFilter` → scan, and **breaks at `Expand`**.

## 1. Ranked operators by de-box leverage

1. **`Expand` → `ChunkProducer` (the enabler).** Removes nothing on the
   traversal itself (pointer-chasing is not vectorizable — Abadi et al. §5), but
   it is the single break point; keeping it in the chain unlocks unboxed
   post-traversal property reads for every emitted row (O(edges)) in the filter
   and aggregation above it. Load-bearing dependency for #2106 and for
   aggregation over traversal output.
2. **Columnar aggregation argument accumulation (#2104).** Chain-independent,
   high: `SUM/AVG/MIN/MAX` box every argument value today even on the columnar
   path (`consumeChunk` `BoxCell`, O(input)); group-key hashing is already
   unboxed (#2049). Works over any scalar-column `ChunkProducer` child
   (`ColumnarProject` today; `Expand` after the enabler).
3. **Columnar filter over `Expand` output (#2106 proper).** Gated on #1; once
   `Expand` is a `ChunkProducer`, `ColumnarFilter` accepts it with near-zero new
   code (it already takes any `ChunkProducer` child).
4. **Columnar hash-join (#2105).** Lowest for analytic Cypher (joins are the
   least common shape; key boxing is cheap). The real win is killing the
   per-build-row `make(Row)+copy` snapshot, not scan-boxing.

## 2. Compaction, not selection vectors

Keep eager compaction (the current `AppendRowFrom` model); do **not** adopt
selection vectors as a cross-cutting accessor change. GoGraph's carrier is narrow
(NodeID + a few scalar props), predicates arrive pre-combined into one
`ChunkPredicate`, and the downstream (agg hash, property fetch) wants dense
random access — the regime where compaction wins over selection vectors (Abadi
et al. FnT 2012 §4.4; DuckDB uses selection vectors precisely because its
downstream is a long columnar scan, which GoGraph is not). A selection vector
would force a `sel`-indirection into every `Chunk` accessor, colliding with the
drop-in-`Operator` + reversibility contract. Bounded exception: a selection
vector *internal to a fused filter chain only*, bench-gated, never in the public
`Chunk` contract.

## 3. Columnar aggregation (#2104)

- **SoA group state:** map `key → dense group-id`; keep accumulators in
  group-id-indexed parallel arrays (`sum []float64`, `count []int64`, …) instead
  of the current per-group `[]Aggregator` (AoS, a boxed interface per aggregate
  per group). Grouped input is then a **scatter-add**
  `sum[gid[row]] += arg[row]` over the unboxed argument column (MonetDB/X100
  hash-aggregation, Boncz et al. CIDR 2005; DuckDB `UpdateStates`).
- **Vectorized argument accumulation (the O(input) de-box):** `COUNT(*)`=+n;
  `COUNT(expr)`=`bits.OnesCount64` over the argument validity bitmap; `MIN/MAX`
  over unboxed `[]int64`/`[]float64`; `SUM/AVG` over a homogeneous float64 column.
- **Exact int64 SUM/AVG must NOT use a float64 accumulator** (lossy; openCypher
  sums integers exactly). Delegate int-SUM per value to the exact `funcs`
  `Aggregator.Step`, or replicate its overflow contract; gate on a mixed
  int/float `SUM` test that matches the row path bit-for-bit.
- **Buffering aggregators (`collect`, `percentileCont/Disc`)** have no vectorized
  form → delegate to boxed `Step` (their #1841 element budget unchanged).
- **Group-key equivalence unchanged (#2049):** hash through `expr.EquivalentHash`
  (float64-domain fold so `int 1`≡`float 1.0`); collisions/`=` resolve via
  `expr.Equivalent`/`cmpInt64Float64` (exact — `≥2^53` cross-type pairs share a
  bucket but land in separate groups). Box a key once per new group, never per
  row.
- **Pipeline-breaker interaction:** aggregation drains its input via
  `child.FillChunk` (extends the chunk chain to the leaf, consumes it, then
  terminates it) and emits O(groups) rows. Ship row-mode output first (the
  O(input) input-side de-box is the whole win); add a `ChunkProducer` output only
  if a downstream projection over aggregation output profiles hot.

## 4. Columnar filter over Expand output (#2106)

- **Make `Expand` a `ChunkProducer`/`NodeIDColumnProducer`; the filter needs no
  change** (`NewColumnarFilter` already takes any `ChunkProducer` child and
  handles fan-in partial-consume). Output chunk = input passthrough columns ||
  three `int64` columns `srcID,edgeID,dstID` (raw NodeIDs → `NodeIDColumnProducer`
  for the endpoint, so the predicate reads `p`'s NodeID unboxed to fetch `p.x`).
- **Two-level fan-out cursor:** `(input-row index, neighbour offset)`; `FillChunk`
  fills up to `maxRows`, stops mid-input-row when the chunk fills, resumes next
  call (DuckDB partially-consumed-input pattern). Preserve `DirOut/DirIn/DirBoth`,
  edge-type filter, multiplicity.
- **Reject the "chunkify adapter"** (pack `Expand.Next` rows into a chunk): it
  boxes at the `Expand.Next` boundary — the ineffective #2065 hybrid; it moves
  the box, removes nothing.
- `VarLenExpand`/shortest-path stay row-mode (variable-shape boxed `Path`).
- Follow-up (bench-gated): a `CONSTANT` vector column to avoid the per-neighbour
  input-column copy (DuckDB `ConstantVector`).

## 5. Columnar hash-join (#2105)

- **Build (breaker):** drain the build child via `FillChunk`; retain the build
  side **column-major** in an owned buffer and store **row-ids** in the buckets
  (`hash → []int32`), not boxed `Row` snapshots (DuckDB `PhysicalHashJoin`, Velox
  `HashBuild`). Kills the per-build-row `make(Row)+copy` alloc. Vectorized
  NULL/NaN key exclusion (per-column, mirroring `isUnjoinableKey`).
- **Probe (preserving):** stream probe chunks; hash the probe key column unboxed
  (`expr.EquivalentHash`); bucket lookup; verify each match with exact
  `Value.Equal`/`cmpInt64Float64` (never Go `==`, never over-match a `≥2^53`
  collision); copy matched build columns + probe columns into the output chunk
  unboxed (`CopyCellTo`). Late materialization both sides. Output is a
  `ChunkProducer` honouring `buildOnLeft` and the existing order-safety guard.
- Rank last; if time-boxed ship build-side late-mat + streaming probe +
  `ChunkProducer` output, skip probe-side selection-vector cleverness.

## 6. Reversibility / fallback contract (every columnar operator)

1. Drop-in `Operator` with a row-mode `Next` behaviourally identical to the
   operator it replaces (the fallback for any parent/shape not columnar); no
   extra alloc over the plain operator when driven row-mode.
2. Additive `ChunkProducer` (`FillChunk`), wired by the planner only when the
   child qualifies (type-assert at build time). Non-`ChunkProducer` child →
   fall back to the row path.
3. Equivalent by construction: the fast path handles cells it can decide unboxed
   and delegates to the same row-mode primitive (`ProjectionItem.Eval`,
   `FilterFn`, `Aggregator.Step`, `Value.Equal`) for any cell it cannot
   (heterogeneous/promoted/non-scalar/temporal-tagged).
4. Never reimplement openCypher comparison — always `expr.EquivalentHash` /
   `expr.Equivalent` / `cmpInt64Float64`; NULL/NaN excluded exactly.

## 7. Reduced scope (stated plainly)

Traversal itself (`Expand`/`VarLenExpand`/shortestPath) is not columnarizable
(pointer-chase, not scan-heavy) — the `Expand` `ChunkProducer` is output plumbing
to keep the chain unbroken, not a traversal speedup. Entity columns
(`NodeValue`/`RelationshipValue`) stay boxed (pointer-rich, variable-shape).
Exact int64 `SUM` is not float-fast-pathed. Buffering aggregators delegate.

## 8. Sequence and sub-tasks

Recommended order: **#2106-step-1 (Expand `ChunkProducer` enabler) → #2104
(aggregation, now fed by traversal output too) → #2106-step-2 (filter,
near-free) → #2105 (join, last)**. #2104's argument-accumulation win also applies
standalone to non-traversal aggregations over `ColumnarProject`, so it can land
before the enabler for that subset. Each operator honours §6 and is
differential-tested columnar-ON vs row-mode-OFF for a byte-identical result
multiset; TCK stays 3897/3897. Extends the columnar value-model epic #1704.
