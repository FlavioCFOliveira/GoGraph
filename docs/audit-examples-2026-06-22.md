# Examples-Driven Module Audit — 2026-06-22

Second read-only, evidence-based audit of the GoGraph **module**, conducted by
exercising its functionalities through the project's `examples/` and the package
benchmark suites, collecting empirical CPU / allocation / memory evidence, and
verifying each candidate with a panel of five specialist sub-agents. The
examples are **instruments**: a finding is attributed to the module only when the
cost lies in GoGraph code, not in an example's generator or harness.

**Baseline:** HEAD `0681e89` — i.e. *after* the eleven optimizations shipped on
2026-06-22 (deadlock fix #1648; count(var) #1654; int32 date column #1649; Bolt
encode/decode/param/row/extra-map pooling #1518/#1517/#1521/#1520/#1522;
lock-free metrics #1519; numeric range index #1652; weightless adjacency #1650).
This audit therefore reports what remains optimizable **after** those landed; it
does not re-raise them.

**Invariants (every candidate preserves them):** openCypher TCK 3897/3897; ACID;
the lock-free immutable-read contract (the whole-`adjEntry` atomic pointer swap —
an atomic-length in-place append was certified NO-GO, #1653); `go test -race`
clean.

**Environment:** Apple M4 (10-core), `go1.26.4`, darwin/arm64.

## Method

Five specialists audited disjoint domains, each exercising the relevant examples
and benchmarks, tracing every candidate to a `file:line` root cause, attributing
module-vs-instrument, and assessing complexity + invariant safety:
graph-theory (search/algorithms/ds), Go-perf (index/IO/wire), cypher
(query engine), storage (persistence/WAL/recovery), columnar (edge-property tier
+ index memory). allocs/op figures are reliable; ns/op under parallel-agent
contention is directional. The headline findings' root causes were independently
re-confirmed against the source.

---

## HIGH — live hot-path allocation (evidence-backed)

**H1 — Scalar functions over a bound entity force full per-row materialization.**
`RETURN id(r)` / `type(r)` / `labels(n)` / `startNode(r)` / `endNode(r)` /
`keys(r)` bypass every bare-variable fast projection path (they are
`*ast.FunctionInvocation`, not `*ast.Variable`) and fall to the general path,
where `analyseNodeScalarUse` (`cypher/api.go` ~6902) classifies a function's
bare-variable argument as `needsWholeNode = true`. That disables the pooled
`RowContext` and eagerly materializes **every variable in scope** per row —
unrelated bound nodes/edges as full `NodeValue`/`RelationshipValue` (+ property
maps) — only to extract one scalar field and discard the rest. Measured (≈800
rows, 3-property edges): `RETURN id(r)` ≈ **24 400 allocs/op** vs `RETURN
count(r)` ≈ 2 120 and even `RETURN r` ≈ 11 000 — i.e. extracting an integer that
already sits in the row costs **2.2× more than returning the whole
relationship**. Fix: teach `analyseNodeScalarUse` to classify field-extractor
functions (`type→needsType`, `id/elementId→the ID already in the row`,
`startNode/endNode→endpoints`, `labels→needsLabels` (flag exists), `keys→key
names`; `properties` keeps full materialization) and have the projection pass
*per-variable* partial use so unrelated vars are skipped. Reuses the existing
`nodeScalarUse`/`upgradeNodeIDToValuePartial` framework. Expected ~**−85% allocs**
(~24k→~3k) on entity-introspection projections — the exact shape visualization
tools and Bolt clients emit. **TCK-safe and TCK-validated:** the suite exercises
`labels(n)` ×9, `type(r)` ×6, `keys(r)` ×6, which currently all take the slow
path, so they both prove the gap and gate the fix. Medium complexity.

**H2 — An index point-seek materializes a full roaring bitmap (+ iterator) to
emit one NodeID.** On the dominant Cypher index-seek shape — `WHERE n.email = $x`
on a unique/sparse property → a *singleton* posting list — `hash`/`btree`
`Lookup` calls `NodeSet.Bitmap()` (`graph/index/nodeset.go:373`), which does
`roaring64.New()` + `Add(single)` for the one element; `NodeByIndexSeek`
(`cypher/exec/scan_index_hash.go:65`) then allocates an iterator over it and
discards the bitmap. Measured `Index_LookupHot` (all-singleton keys): **10
allocs/op, 248 B**. (Note: the bitmap is *not* cloned — `NodeSet` is already
tiered #1584, so the prior "Lookup clones" memory is stale; the cost is singleton
*materialization*, not a clone.) Fix: add a clone-free `NodeSet.ForEach(func(uint64)
bool)` / `AppendTo([]uint64)` borrow API (used under the index read lock) and
route `NodeByIndexSeek` through it — the singleton tier yields its one id with
**zero allocation**. Expected **10→0 allocs** on the dominant seek; directly
benefits the numeric-range and equality seeks (#1505/#1652). Low-moderate
complexity; read-only, no invariant touched.

---

## MEDIUM

**M1 — Snapshot writers still encode with per-field `binary.Write` reflection.**
The CSR codec was de-reflected to zero-copy LE byte views (#1593), but its
sibling writers were not: `store/snapshot/properties.go:272-323`,
`labels.go:157-190`, `edgehandles.go`, `mapper.go` call `binary.Write(tee, LE,
scalar)` once per field per record. In go1.26 each such call boxes the scalar
into `any` (escape → 1 alloc) **and** `make([]byte,n)` (1 alloc) = 2 allocs/field.
Measured (10k nodes / 50k edges): `WriteProperties` **570 080 allocs/op, 46.9 MB**
and `WriteLabels` **300 080 allocs/op, 13.3 MB**, vs `WriteCSR`'s **8 allocs** for
the same edge set. Fix (proven byte-identical, mirrors #1593): fill a fixed stack
array via `binary.LittleEndian.PutUintNN` then one `w.Write(buf[:])` per record —
~5 allocs/rec → 1, 45→11 ns/rec. Format-neutral (`formatVersion` unchanged, CRC
sees identical bytes, publish sequence untouched). Runs on the background
checkpointer (lock-free phase 2), so it doesn't block commits, but its GC
pressure competes with foreground committers for CPU. Low, mechanical. The
symmetric `binary.Read` reflection on the recovery/load side is a lower-priority
fold-in (recovery is one-shot at startup).

**M2 — `RETURN r` / path queries double-allocate the edge-property map per row.**
The projection calls `g.EdgeProperties(...)` (`graph/lpg/edge_property.go:251`),
which allocates a transient `map[string]PropertyValue` via an O(degree) scan, then
the evalFn allocates a **second** `expr.MapValue` and copies every entry through
`lpgPropToExpr` (`cypher/api.go:8606`, repeated in the VLE/path paths). The
intermediate map is pure waste. Fix: an additive `g.ForEachEdgeProperty(srcID,
dstID, visit)` streaming the de-boxed columnar slots so the projection builds the
`expr.MapValue` in the callback. ~−10–15% allocs on `RETURN r`; larger on path
queries; helps example 26. Low-medium; identical values, no invariant.

**M3 — Frame-of-reference + bit-packing for the int32 date column.** ex26's date
column (`graph/lpg/edge_property_column.go:133` `days []int32`) holds ~2 193
distinct values (a 2 192-day window → 12-bit range) stored at full 4 B/edge with
no FOR/bit-packing/dictionary anywhere in the columnar tier. A dense date column
→ `{min int32, width uint8, packed []uint64}` at 12 bits ≈ **1.5 B/edge** (saves
~2.5 B/edge ≈ ~10% of ex26's 24.4 B/edge resident). Apply at `Compact()` (the
freeze pass), gated on the measured range (skip when width ≥ ~24 bits).
**Representation-independent → zero durability/TCK risk**: the on-disk format
serializes the canonical Date string via the public value path and reconstitutes
the column on read, so no snapshot/WAL/recovery change and no `formatVersion`
bump — the same property NodeSet tiering exploits. Mirrors the already-bit-packed
`boolBits` column. Medium. (Honest caveat: the ~8 B/edge Go-runtime overhead —
map buckets, slice headers, size-class rounding — is untouched, so whole-graph
reduction is ~10%, not the raw 4→1.5 ratio.)

**M4 — `btree.BulkLoad` triple-materializes the input.** `graph/index/btree`
builds a throwaway `[]pair` (sorted), then intermediate `keys`/`sets`, then
`bulkPack` copies each into leaf-local slices — three passes over N elements.
Measured `BulkLoad_10M`: **1.31 GB/op, 367 035 allocs/op** ≈ 2.7× the ~480 MB
live floor. Fix: a `BulkLoadSorted(keys, sets)` that skips the `pair` round-trip
for pre-sorted input (Deserialize/rebuild paths), and let `bulkPack` adopt the
caller's slices by 3-index reslice (`keys[i:end:end]`, the discipline `splitLeaf`
already uses) instead of copying. ~−40–50% B/op on the sorted path. Moderate;
under the index write lock, result byte-identical, no invariant.

**M5 — `MinCostMaxFlow` uses `container/heap` with `any`-boxing, re-allocating
the PQ every iteration.** `search/flow/min_cost.go:126` allocates `mcmpPQ` inside
the outer SSP loop, and its `Push(x any)`/`Pop() any` (≈281) box a 16-byte
`mcmfItem` per relaxation push (multi-word value → heap escape) — a direct
"no interface dispatch in inner loops" mandate violation, O(F·E) allocs.
Fix: an inline monomorphic binary heap (clone the existing zero-alloc `dijkHeap`)
hoisted out of the loop (`items[:0]` reset). Pure compute, no invariant. Caveat:
no example uses `MinCostMaxFlow` (shipped API surface, not a live example path),
and there is **no benchmark** (coverage gap to close alongside).

---

## LOW

- **IO serializer per-edge allocation.** DOT (`graph/io/dot/writer.go:103`) does
  two `fmt.Sprintf`/edge (worst); CSV (`writer.go:82`, the known #1523) a
  `FormatInt` + 3-string slice/edge; GraphML a `Sprintf`/edge. Export path, not
  the concurrency hot path. Bundle a hoisted scratch buffer + `strconv.AppendInt`
  across CSV/DOT/GraphML. (JSONL bottoms out in `encoding/json` — leave.)
- **`KShortestPathsLoopless` PQ boxing** (`search/kshortest_loopless.go:204`) —
  de-box only; the per-expansion path `make`+`copy` is inherent to the formulation
  and already DoS-guarded.
- **Leiden per-pass `sigmaTot`/`kvcArr`/`sigmaIn` scratch** (`search/community/
  leiden.go:377/412/503`) — thread through the existing `aggScratch` pool; O(passes)
  allocs, bytes already minimal.
- **Hungarian per-row `minv`/`used`** (`search/hungarian.go:125`) — hoist out of
  the row loop; trivial, but compute-bound (O(V³)) so negligible.
- **Relationship-type label column dictionary/RLE** (ex26: 2 distinct values at
  4 B/edge). ~3.5 B/edge in typed mode, but the column lives in the deliberately
  label-agnostic `graph/adjlist` and is load-bearing for the lock-free read +
  lockstep compaction (and #1655's O(degree) slot-update). **Defer**; if pursued,
  build it as an lpg-owned low-cardinality column inside the existing `AuxColumn`
  block, never in adjlist's flat `labels`.

## Maintenance (not performance)

**The cost-based planner is dead code.** `cypher/plan` (902 LOC, a genuine
cardinality-estimating planner) and `cypher/ir/rewrite` (973 LOC) are imported
only by their own tests (`cypher/ir/rewrite` has an explicit anti-wiring guard).
The live hand-rolled planner already performs the high-value physical rewrites
(hash-join, index/range seek) gated by **exact** cardinality. The one residual
gap — expands are planned in document order with no selectivity-based
leg-reordering (`(a)-[r]->(b:L {indexed})` always drives from `a`) — is real but
resurrecting the planner for it is large/high-risk (cardinality correctness, plan
stability, plan-shape tests) and lazy materialization + 0-alloc neighbour reads
blunt the cost. Recommend **deleting** the ~1 875 LOC as maintenance debt; pursue
selectivity reordering only if a profiled real workload shows an inversion
hot-spot.

---

## Refuted by evidence (recorded so they are not re-raised)

- **`UnionFindGeneric` (151 MB/16 428 allocs)** — instrument-only; **zero**
  production callers; both real users (Kruskal, WCC) already use the dense
  `UnionFindSlice` (9.4 MB/2 allocs). The map version is a correct convenience.
- **`EdmondsKarp` (1 428 allocs)** — the simple O(VE²) reference baseline, not a
  default; the production `MaxFlow` is Dinic (14 allocs). Its allocs are graph
  construction in the timed loop.
- **`Dijkstra_RandomGraph` (91.7 MB/47 allocs)** — the already-praised pooled
  design plus a `benchtime` warmup artifact; steady-state `DijkstraInto` is
  **0 alloc**; the residual is the caller-owned result-copy contract.
- **`Betweenness_Parallel` (147 allocs)** — intrinsic per-worker private buffers
  (bounded by GOMAXPROCS) for the lock-free deterministic reduce; serial is 10.
- **`btree InsertDistinct` (9 296 allocs)** — amortized slice-growth + node
  splits, *not* per-insert (ascending vs random gave the same alloc count); the
  O(n) cost is copies (CPU), not allocation; at floor.
- **packstream codec** — scalar encode/decode is zero-alloc (stack scratch);
  collection boxing + `make([]byte,n)` string decode are inherent to a
  dynamically-typed sum type and budget-guarded; the Encoder/Decoder churn is
  already pooled (#1517/#1518).
- **Index/columnar memory tiering already done** — `NodeSet` (singleton/small/
  bitmap, #1584), `propBag` (#1587), `labelBag` (#1629) are by-value tagged unions
  with zero-alloc singleton tiers; validity bitmap omitted on dense columns
  (Arrow rule); sparse-COO hysteresis correct. At ceiling for ex26's distribution.
- **WAL frame encode, `store/txn` commit-record encode, csrfile mmap read,
  recovery replay** — stack header + pooled scratch (#1509), zero-copy `unsafe`
  mmap views, one-shot startup replay. At ceiling.
- **parser/sema (plan-cached, cold), funcs aggregators (zero per-row alloc),
  `db.*` procs (admin/cold)** — not hot paths.

## Confirmed strengths (re-verified at/near ceiling)

Direction-optimised BFS 0-alloc; pooled Dijkstra/`DijkstraInto` 0-alloc;
lock-free CSR neighbour read; out-of-core mmap (ex05: 88 MB file → ~712 B Go
heap, genuinely zero-copy); the columnar edge-property tier + weightless mode
(ex26 24.4 B/edge, of which 8 B is the irreducible `neighbours` topology); the
group-commit path (the single-writer fsync ceiling is fsync latency, not module
code).

## Backlog mapping

Confirmed module candidates filed as atomic tasks in the `gograph` rmp roadmap
(sprint **S-EA2**), ranked H1, H2 (high); M1–M5 (medium); the LOW set; plus the
dead-planner deletion as a maintenance task. No candidate is implemented — this
is a read-only evaluation; each awaits prioritisation. The single largest
single-threaded-throughput lever remains a batched-commit API (prior audit M3 /
backlog #1512): one `fdatasync` per `tx.Commit` caps serial ingest at ~250 tx/s
(ex04: 25k packages ≈ 100 s); `store/bulk` is a WAL-bypassing read-only loader,
not a recoverable batched-commit path, so the gap stands — it changes atomicity
granularity and must be crash-injection-verified.
