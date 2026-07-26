# Stream 5 — Cypher runtime and execution engine

Baseline `6f31f61` (v0.10.0). All GoGraph claims are `file:line` at that commit.
All measurements: **Apple M4, 10 logical cores, darwin/arm64, go1.26.5**, `go test -bench`,
in-process engine, `adjlist.Config{Multigraph:true, Directed:true}`. Method for every
number is stated inline; the harness used is described in "Measurement harness" at the end.

Neo4j source citations are `neo4j/neo4j` branch `2026.06` @ `eccd584a64d468af3daeab421478fe78567c518f`.
Memgraph source citations are `memgraph/memgraph` `master`, fetched 2026-07-25.

---

## Verdict summary

GoGraph's *vectorized tier is real and, where it engages, decisively ahead of both
incumbents* — neither Neo4j nor Memgraph has a columnar chunk pipeline at all (Neo4j's
morsel is a batch of **rows**; Memgraph's `Cursor::Pull(Frame&, ExecutionContext&)` is one
row at a time, and `grep -i "vectoriz|columnar|morsel|batch"` over its 3,218-line
`operator.hpp` returns zero hits). But that tier is wired by **rigid IR spine
pattern-matching**, and I measured the consequence: on identical data returning an
identical result multiset, `MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v` runs in
**8.6 ms / 133 allocs**, while adding the far-node label `:P` — which every node already
carries — makes it **40.7 ms / 218,058 allocs (4.4× slower, 1,640× more allocations)**.
The same cliff fires on `AND`, on `NOT`, on `IN`, on `STARTS WITH`, on `LIMIT`, and on
`ORDER BY`: **10 of 15 ordinary filter/project shapes I probed never touch the columnar
path at all.** Separately, *every* form of intra-query parallelism is gated on the IR leaf
being a bare `*ir.AllNodesScan` (`cypher/api.go:8208`, `cypher/api.go:7832`) — so adding a
label costs another measured **2.0–3.7×**, and real Cypher always has a label.

**The single most valuable lever from the incumbents is Neo4j's `SlottedRow`
(`longs: Array[Long]` + `refs: Array[AnyValue]`, offsets resolved at plan time) and
Memgraph's `Frame` (`utils::pmr::vector<TypedValue>` indexed by `symbol.position()`) —
both of which eliminate the per-row string-keyed rebinding that GoGraph still performs in
`expr.RowContext = map[string]Value` (`cypher/expr/eval.go:30`).** I measured that
mechanism in isolation at **29–70× (68–184 ns/row of pure CPU, zero allocations)**.
Round 1's "positional slot-array RowContext" naming was *imprecise*: `exec.Row` is
**already** a positional slice (`cypher/exec/row.go:44`); the defect is that the executor
re-derives the name→column mapping through a hash map on every row, and that the slot
element type is a Go interface, so every scalar in it heap-boxes.

Ranked, the three highest-ROI levers are **(1) make the columnar recognisers
shape-robust** (admit a `Selection` below `Expand` — the anchor filter, F1b; then
conjunctions, stacked Selections, chunk-transparent `Limit`, F1),
**(2) partition the label scan for parallelism** (Neo4j `PartitionedNodeByLabelScan`,
Memgraph `ScanParallelByLabel`), **(3) positional slot binding + long slots**. All three
are additive and none touches semantics.

**This stream's per-row constants explain the cross-stream traversal deficit.** The
coordinator measured a 1-hop expand at **8.711 ms** against Memgraph's 286 µs and could
attribute only ~700 µs of it to the planner's missing integer-property index. 20 000 nodes
× the **398–511 ns/row** I measured for a row-mode filtered label scan = **7.96–10.2 ms**,
which brackets 8.711 ms. The missing 11–15× over their 35 ns/row iteration estimate is the
per-row tax this report itemises: interface-boxing the NodeID, `clear()`ing a cap-16 pooled
map and rebinding by string hash, re-walking the predicate AST, and boxing the fetched
property. **But this is the constant factor, not the exponent** — the O(|label|) anchor is
the planner's defect and dominates; fixing every runtime lever here takes 8.7 ms to ≈2.8 ms,
still 10× Memgraph. See "Reconciliation with the cross-stream traversal benchmark".

---

## Feature-by-feature comparison

| Feature | GoGraph (`file:line`) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Execution model | Pull/Volcano `Next(out *Row)` (`exec/operator.go:25-43`) **plus** an additive pull-chunk tier `FillChunk` (`exec/produce_results.go:98-106`) | slotted = pull row-by-row; pipelined/parallel = **push**, batched ([runtimes/concepts](https://neo4j.com/docs/cypher-manual/current/planning-and-tuning/runtimes/concepts/)) | pull, one row (`src/query/plan/operator.hpp:79-104`) | **BETTER than Memgraph**, mixed vs Neo4j Enterprise (they push+fuse; GoGraph pulls) | CONFIRMED-R1 |
| Vectorized/columnar tier | Yes — `Chunk`, cap 4096 (`exec/chunk.go:116`); 6 `ChunkProducer`s | No (morsel = row batch, 128/1024 rows, `internal.cypher.pipelined.batch_size_{small,big}`) | No (zero hits) | **BETTER than both** | CONFIRMED-R1 |
| Columnar coverage | 5/15 probed shapes; 1 bare comparison only (`api.go:13248-13268`) | pipelined covers whole plan; unsupported operators fall back **per-operator** via `internal.cypher.pipelined_interpreted_pipes_fallback` | n/a | **WORSE** — GoGraph falls back *whole-plan*, they fall back *per-operator* | **NEW** |
| Per-row variable binding | `map[string]Value`, pooled (`expr/eval.go:30`, `api.go:10760`, `api.go:10829` `populateRowCtx`) | `SlottedRow`: `Array[Long]` + `Array[AnyValue]`, plan-time offsets (`SlottedRow.scala`, `SlotConfiguration.scala`) | `vector<TypedValue>` at `symbol.position()` (`src/query/interpret/frame.hpp`) | **WORSE than both** | STALE-R1 (refined) |
| Scalar representation in a row | `[]expr.Value` — Go interface ⇒ heap box per non-tiny scalar (`exec/scan_label.go:109`) | node/rel ids unboxed in `long[]` (`LongSlot`) | `TypedValue` tagged union **by value** in the vector | **WORSE than both** | **NEW** |
| Intra-query parallelism trigger | automatic, no licence, `LiveOrder() > 50_000` (`api.go:774`) | Enterprise-only, opt-in `CYPHER runtime=parallel`, read-only | Enterprise-only, opt-in `USING PARALLEL EXECUTION`, needs `PARALLEL_EXECUTION` privilege + `priority_queue` scheduler | **BETTER than both** on gating | CONFIRMED-R1 |
| Parallelisable leaves | **bare `AllNodesScan` only** (`api.go:8208`, `api.go:7832`) | `PartitionedNodeByLabelScan` + partitioned index scans | `ScanParallelByLabel`, `ScanParallelByLabelProperties`, `ScanParallelByEdge`, `ScanParallelByEdgeType…` | **WORSE than both** | **NEW** |
| Parallel writes | writes never parallel (single-writer) | parallel runtime **read-only**, `22N46` on write | parallel segment is scan→agg/ORDER BY only | PARITY | CONFIRMED-R1 |
| Parallel result determinism | position-carrying min/max combine, byte-identical incl. int/float ties, ±0, NaN (`exec/parallel_aggregate_scan.go:16-48`) | ordering divergence treated as a **bug** (neo4j#13382, fixed 5.18); no documented guarantee of representation identity | parallel `DISTINCT` ordering bugs fixed in v3.9 (#3815, #3876) | **BETTER than both** | CONFIRMED-R1 |
| Saturation behaviour | `ParallelGovernor` budget = GOMAXPROCS/inflight (`exec/parallel_governor.go:57-74`) + `budget==1` inline short-circuit (`parallel_aggregate_scan.go:290`) | `server.cypher.parallel.worker_limit`; docs warn throughput *decreases* under concurrency | no cross-query governor documented | **BETTER than both** | CONFIRMED-R1 |
| Expression evaluation | AST type-switch re-dispatched per row (`expr/eval.go:487`) | **compiles** expressions; `expressionEngine=default` = compile when hot after `internal.cypher.expression_recompilation_limit=10` uses | AST visitor, no JIT (`src/query/interpret/eval.hpp:275`, zero jit/llvm hits) | PARITY with Memgraph, **WORSE than Neo4j** | **NEW** |
| Operator fusion | none — two adjacent `Selection`s stay two operators (measured, see F1) | fuses `AllNodesScan→Filter→ProduceResult` into one generated operator (`cypher_operator_engine` description) | none | **WORSE than Neo4j Enterprise** | **NEW** |
| Result streaming | **full materialisation** inside `Graph.View` (`api.go:1900`→`api.go:1977`) | streams (reactive / PULL n) | streams lazily, client-paced, explicitly rejected materialising ("not keeping any of the results in memory") | **WORSE than both** | CONFIRMED-R1 |
| Spill to disk | none | none — "the transaction is terminated"; `TransactionOutOfMemoryError` / GQLSTATUS 51N73 | none — query aborts even in `ON_DISK_TRANSACTIONAL` | PARITY | **NEW** (settles an open question) |
| Memory limits | per-result rows 10M (`api.go:3200`), bytes 1 GiB (`api.go:3230`), **engine-wide** aggregate ceiling defaulting to ½ GOMEMLIMIT (`api.go:3270`), plus per-breaker caps (`exec/sort.go:36`, `exec/distinct.go:27`, `exec/eager_aggregation.go:43`) | `dbms.memory.transaction.total.max` (70 % heap), `db.memory.transaction.total.max`, `db.memory.transaction.max` — all Community | `--memory-limit`, `QUERY MEMORY LIMIT n MB`, `PROCEDURE MEMORY LIMIT` (default 100 MB) | **PARITY on results, WORSE on intermediates** — GoGraph has no single pool spanning sort+distinct+join+agg | **NEW** |
| Query timeout | caller `context.Context` (138 `ctx.Err()` sites in `cypher/exec`); Bolt `DefaultTxTimeout = 30 s` (`bolt/server/serve.go:71`) | `db.transaction.timeout`, **default `0s` = disabled** | `--query-execution-timeout-sec`, default 600 | **BETTER** (context-native, and a stricter default) | **NEW** |
| Runtime observability | one counter, `cypher.exec.columnar_filter.batch` (`exec/columnar_filter.go:39`); `Engine.Explain` renders the **logical** tree only | EXPLAIN prints `Runtime PIPELINED`, `Runtime version`, `Batch size 128`, per-operator `Fused in Pipeline N` | `SHOW MEMORY INFO`; `PARALLEL_EXECUTION_FALLBACK` notification when parallelism declines | **WORSE than both** | **NEW** |

---

## Findings

### F1. The columnar chain is wired by rigid IR-spine matching; ordinary Cypher falls off it  [NEW]  (severity: HIGH)

- **What they do.** Neo4j's pipelined runtime does not fall back whole-plan. `internal.cypher.pipelined_interpreted_pipes_fallback` (`GraphDatabaseInternalSettings.java`, `@Internal`) is documented as *"Use interpreted pipes as a fallback **for operators** that do not have a specialized implementation in the pipelined runtime… a subset of whitelisted operators"*, surfaced publicly as `CYPHER interpretedPipesFallback=whitelisted_plans_only` — *"**Parts** of the execution plan can be executed on another runtime."* The unit of degradation is the operator, not the query. Neo4j additionally **fuses** adjacent operators: *"multiple operators such as for example AllNodesScan -> Filter -> ProduceResult can be compiled into a single specialized operator"* (`cypher_operator_engine` `@Description`).
- **What GoGraph does.** The columnar tier is selected by two hard-coded IR spine matchers:
  - `tryBuildColumnarFilterChain` (`cypher/api.go:12630-12691`) requires exactly `Projection → Selection → single-node scan`, `len(schema)==0`, every projection item a scalar property on the **column-0** node, and a predicate accepted by `buildColumnarPredicate`.
  - `tryBuildColumnarExpandFilterChain` (`cypher/api.go:12710-12789`) requires exactly `Projection → Selection → Expand → single-node scan`.
  - `buildColumnarPredicate` (`cypher/api.go:13248-13268`) accepts **one** `*ast.BinaryOp` whose operator is one of `< <= > >= = <>`. There is no conjunction combiner, despite `docs/columnar-deepening-design.md` §2 asserting "predicates arrive pre-combined into one `ChunkPredicate`" — that statement is **not true at v0.10.0**.
- **Evidence (measured).** Engagement probe, 2,000-node graph, counting `cypher.exec.columnar_filter.batch`. **COL = columnar engaged, ROW = not**:

  | Query | Path |
  |---|---|
  | `WHERE n.v > 10 RETURN n.v` | COL |
  | `WHERE n.v > 10 RETURN n.v, n.w` | COL |
  | `WHERE n.v > $p RETURN n.v` | COL |
  | `WHERE n.v > 10 RETURN n.v AS a` | COL |
  | `WHERE n.v > 10 WITH n.v AS x RETURN x` | COL |
  | `WHERE n.v > 10 AND n.v < 100 RETURN n.v` | **ROW** |
  | `WHERE n.v > 10 AND n.w = 3 RETURN n.v` | **ROW** |
  | `WHERE n.v > 10 OR n.w = 3 RETURN n.v` | **ROW** |
  | `WHERE NOT n.v > 10 RETURN n.v` | **ROW** |
  | `WHERE n.v IN [1,2,3] RETURN n.v` | **ROW** |
  | `WHERE n.name STARTS WITH 'p1' RETURN n.v` | **ROW** |
  | `WHERE n.v > 10 RETURN n.v + 1` | **ROW** |
  | `WHERE n.v > 10 RETURN n.v LIMIT 5` | **ROW** |
  | `WHERE n.v > 10 RETURN n.v ORDER BY n.v` | **ROW** |
  | `WHERE n.v > 10 RETURN DISTINCT n.v` | **ROW** |

  Cost, 100 000-node graph, **all seven queries return the identical 98 900-row multiset**, `-benchtime=10x -count=6` (median):

  | Query | sec/op | B/op | allocs/op |
  |---|---|---|---|
  | `WHERE n.v > 10 RETURN n.v` (COL) | **13.8 ms** | 4.18 MB | **103** |
  | `WHERE n.v > 10 AND n.v < 100000 RETURN n.v` | 39.8 ms | 6.90 MB | 347 545 |
  | `WHERE NOT n.v <= 10 RETURN n.v` | 34.6 ms | 5.51 MB | 174 235 |
  | `WHERE n.v > 10 RETURN n.v + 0` | 52.7 ms | 11.52 MB | 323 048 |
  | `WHERE n.v > 10 RETURN n.v LIMIT 100000` | 59.6 ms | 10.93 MB | 248 650 |
  | `WHERE n.v > 10 RETURN n.v, n.w` (COL) | 18.3 ms | 8.28 MB | 143 |

  So a conjunction costs **2.9×** and **3 374× more allocations**; a semantically-inert
  `LIMIT 100000` on a 98 900-row result costs **4.3×** and **2 414× more allocations**.

  The traversal cliff is worse. Same 20 000-node / 60 000-edge graph, identical
  53 400-row result, `-benchtime=8x -count=3`:

  | Query | sec/op | allocs/op |
  |---|---|---|
  | `MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v` | **8.6 ms** | **133** |
  | `MATCH (a:P)-->(m) WHERE m.v > 10 RETURN m.v` | **7.5 ms** | **131** |
  | `MATCH (a)-[:K]->(m) WHERE m.v > 10 RETURN m.v` | **9.0 ms** | **142** |
  | `MATCH (a:P)-[:K]->(m:P) WHERE m.v > 10 RETURN m.v` | **40.7 ms** | **218 058** |

  **Adding a label to the far endpoint costs 4.4× and 1 640× more allocations.** `Engine.Explain`
  shows exactly why — the far-node label becomes a *second stacked* `Selection`:

  ```
  ### MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v
  ProduceResults └─ Projection └─ Selection └─ Expand └─ NodeByLabelScan [a:P]

  ### MATCH (a:P)-[:K]->(m:P) WHERE m.v > 10 RETURN m.v
  ProduceResults └─ Projection └─ Selection └─ Selection └─ Expand └─ NodeByLabelScan [a:P]
  ```
  `sel.Child` is now a `*ir.Selection`, not a `*ir.Expand`, so `tryBuildColumnarExpandFilterChain`
  declines at `cypher/api.go:12735` and the whole chunk chain collapses to row mode.
- **Lever.** Three independent, additive changes, cheapest first:
  1. **Adjacent-`Selection` fusion peephole** in the IR builder: collapse `Selection(p) → Selection(q)` into `Selection(p AND q)`. Pure structural rewrite; conjunction of predicates is commutative/associative under openCypher 3VL only when both are already conjuncts of the same WHERE — which is exactly this case (both came from one `MATCH`). This alone restores the traversal chain the moment (2) lands.
  2. **Conjunction-combining `ChunkPredicate`**: extend `buildColumnarPredicate` / `buildColumnarExpandPredicate` to accept `*ast.BinaryOp{Operator:"AND"}` recursively, returning `(keep, decided)` = AND over the children with the existing "any child undecided ⇒ whole row undecided ⇒ boxed fallback" rule. Also accept `LabelPredicate` (a roaring-bitmap membership test on the raw NodeID — cheaper unboxed than boxed) and `IN` over a scalar literal list. Every non-matching sub-shape keeps the existing byte-identical boxed fallback, so the risk surface is unchanged.
  3. **Chunk-transparent `Limit`/`Skip`**: make them `ChunkProducer`s that clamp `maxRows` and delegate `FillChunk` to the child, so a `LIMIT` at the plan root no longer forces the entire suffix into row mode (`docs/columnar-deepening-design.md` §0's chunk-chain rule makes this the whole cause).
- **TCK/ACID impact.** All three are build-time physical-operator selection only; the logical plan, the row multiset and every value representation are unchanged, and each columnar operator keeps its `docs/columnar-deepening-design.md` §6 reversibility contract (row-mode `Next` byte-identical fallback, `decided=false` ⇒ boxed predicate). Extend the existing differential tests (`cypher/columnar_filter_test.go`, `cypher/exec/expand_columnar_filter_test.go`) to the new shapes: columnar-ON vs row-mode-OFF must produce an identical multiset. No transaction, WAL, or visibility-barrier code is touched, so ACID is untouched. TCK stays 3897/3897 by construction (fallback preserves semantics for anything the fast path declines).
- **Effort.** (1) S. (2) M. (3) M.

---

### F1b. The columnar tier only recognises filters *above* a traversal — the anchor filter, the commonest shape in graph querying, is a permanent blind spot  [NEW]  (severity: HIGH)

- **What GoGraph does.** There are exactly two columnar entry points, both at the
  `ir.Projection` level (`cypher/api.go:6879`, `cypher/api.go:6890`), and both require the
  `Selection` to sit *directly* under the projection:
  - `Projection → Selection → scan` (`tryBuildColumnarFilterChain`, guard at `api.go:12651-12658`);
  - `Projection → Selection → Expand → scan` (`tryBuildColumnarExpandFilterChain`, guard at
    `api.go:12731-12742`). The code's own comment scopes it explicitly to a
    **post-traversal** WHERE: *"a scalar-property projection over a post-traversal WHERE
    (`MATCH (n)-[r]->(p) WHERE p.x > k RETURN p.y`)"* (`api.go:6884-6886`).

  The canonical graph query is the **opposite** shape — filter first, then traverse:
  `MATCH (p:Person {id:$id})-[:KNOWS]->(f) RETURN f.name`, whose IR is
  `Projection → Expand → Selection → NodeByLabelScan`. Here `proj.Child` is an `*ir.Expand`,
  not an `*ir.Selection`, so recogniser 1 declines at `api.go:12651`; and the `Selection` is
  *below* the `Expand`, so recogniser 2 declines at `api.go:12735`. **No anchoring predicate
  is ever vectorized, at any depth.** This is structural, not a shape-coverage gap that F1's
  conjunction work would fix — F1 widens *which predicates* are eligible; F1b is about
  *where in the plan* a predicate may sit.
- **Evidence.** Structural, from the two guards above and the entry-point comment. It is
  corroborated by the cross-stream traversal benchmark — see the reconciliation section
  below, where the anchor scan accounts for essentially the whole measured 1-hop latency.
- **Lever.** Generalise the recognisers from spine matching to a **suffix walk**: starting at
  the sink, descend while each child is chunk-capable, and admit `Selection` at *any* depth in
  that suffix rather than only at depth 1. Concretely, the missing shape is
  `[Projection] → Expand → Selection → scan`: build the scan as a `NodeIDColumnProducer`, wrap
  it in a `ColumnarFilter` (which already accepts any `ChunkProducer` child —
  `exec/columnar_filter.go:79`), and hand *that* to `NewColumnarExpand`, which already requires
  only that its child be a `ChunkProducer` (`exec/expand.go:672-679`). **Every piece already
  exists; only the recogniser refuses to assemble them in this order.** That makes this the
  cheapest high-value change in the report.
- **TCK/ACID impact.** Identical to F1: build-time physical selection only, each operator
  keeps its row-mode byte-identical fallback (`docs/columnar-deepening-design.md` §6). The one
  new obligation is that `ColumnarFilter` below an `Expand` must preserve the Expand's
  input-row cursor semantics — but `Expand.fillChunk`'s two-level cursor already treats its
  child as an opaque `ChunkProducer` (`exec/expand.go:727-748`), so no new invariant is
  introduced. Differential-test columnar-ON vs row-OFF on anchor-filter shapes.
- **Effort.** M (recogniser generalisation; no new operator).

---

### F2. All intra-query parallelism is gated on a bare, unlabelled `AllNodesScan`  [NEW]  (severity: HIGH)

- **What they do.** Both incumbents partition the **label** scan.
  - Neo4j's parallel runtime uses *partitioned* leaf operators; the runtime-concepts page's own EXPLAIN output shows `PartitionedNodeByLabelScan`, and states *"These operators first segment the retrieved data and then operate on each segment in parallel."*
  - Memgraph 3.8's complete parallel operator set (`src/query/plan/operator.hpp:176-191`) is `ParallelMerge, AggregateParallel, OrderByParallel, ScanParallel, ScanParallelByLabel, ScanParallelByLabelProperties, ScanParallelByEdge, ScanParallelByEdgeType, ScanParallelByEdgeTypeProperty{,Value,Range}, ScanParallelByEdgeProperty{,Value,Range}, ScanChunk, ScanChunkByEdge` — i.e. label scans, label+property scans, edge scans and edge-property range scans all partition.
- **What GoGraph does.** Both recognisers require the leaf to be a bare, unlabelled all-nodes scan:
  - `tryBuildParallelAggregateScan`: `scan, isScan := p.Child.(*ir.AllNodesScan)` (`cypher/api.go:8208`).
  - `tryBuildParallelScanProject`: `if _, isScan := scanPlan.(*ir.AllNodesScan); !isScan { return nil, false, nil }` (`cypher/api.go:7832`).
  `docs/parallelism-broadening-design.md` never considers a partitioned label scan — this is an unexplored gap, not a considered-and-rejected decision.
- **Evidence (measured).** 100 000-node graph in which **every node carries `:Person`**, so the labelled and unlabelled forms return identical results. `-benchtime=8x -count=3` (median), plus a control engine built with `EngineOptions{DisableParallelScan: true}` on identical data to prove the gap is parallelism and not label-scan iterator cost:

  | Query | unlabelled (par ON) | labelled (par ON) | unlabelled (par **OFF**) | labelled (par OFF) |
  |---|---|---|---|---|
  | `RETURN min(n.v)` | **18.9 ms** | 69.6 ms | 86.2 ms | 87.5 ms |
  | `RETURN max(n.v)` | **19.2 ms** | 68.2 ms | — | — |
  | `RETURN n.w, min(n.v)` | **36.0 ms** | 73.2 ms | — | — |
  | `WHERE n.v>10 AND n.v<100000 RETURN n.v` | **18.4 ms** | 43.0 ms | 41.0 ms | 51.1 ms |

  Reading: **adding `:Person` costs 2.0–3.7×**. The control settles causality — with
  parallelism disabled the unlabelled form (86.2 ms) matches the labelled form (87.5 ms),
  so the label scan is *not* intrinsically slower; the entire gap is the parallel path
  declining. Conversely, parallelism is worth **~4.6×** on `min` at concurrency 1
  (18.9 ms vs 86.2 ms) — a genuine, large win that ordinary Cypher never receives.
- **Lever.** Add a **partitioned label scan**: teach both recognisers to accept
  `*ir.NodeByLabelScan` and build per-worker morsels from the label's roaring bitmap
  (`graph/lpg` already maintains exact per-label roaring bitmaps — the same structure the
  min-label anchor peephole reads for exact cardinality). The morsel is a contiguous slice
  of the bitmap's ordered iteration, which preserves the *contiguous morsel + base offset*
  property that `ParallelAggregateScan`'s position-carrying min/max combine relies on for
  byte-identical ties (`cypher/exec/parallel_aggregate_scan.go:24-34`). The threshold
  should key on the **label** cardinality, not `LiveOrder()`, or a small label in a huge
  graph will over-parallelise. Second step (larger): partitioned index seek, mirroring
  Memgraph's `ScanParallelByLabelProperties`.
- **TCK/ACID impact.** Worker count never affects results (`exec/parallel_governor.go:36-38`), and the
  determinism obligation is discharged the same way #2111 discharged it: contiguous morsels
  carry a base offset so the global scan index — hence the first-seen tie representative and
  the group emission order — is identical to serial. The whole parallel fan-out already runs
  inside `Graph.View`'s `visMu.RLock` (`parallel_aggregate_scan.go:80-87`), so Isolation is
  unchanged and no worker outlives the barrier. Reuse the existing worker-count and
  partition-boundary sweeps (`cypher/exec/parallel_aggregate_scan_test.go`) plus the
  goleak/`-race` gate. TCK unaffected (no semantic change).
- **Effort.** M.

---

### F3. `expr.RowContext` is still a string-keyed map; both incumbents index positionally  [STALE-R1 → refined]  (severity: HIGH)

Round 1's T1.2 named "positional slot-array RowContext" as the top runtime lever. That
naming is now **stale**: `exec.Row` is already `[]expr.Value` (`cypher/exec/row.go:44`),
backed by a contiguous `RowSlab` arena (`cypher/exec/row.go:78-95`). The defect that
remains is one layer up, and it is two distinct things.

- **What they do.**
  - Neo4j `SlottedRow.scala` — Scaladoc: *"Execution context which uses a slot
    configuration to store values in **two arrays**."*
    ```scala
    final case class SlottedRow(slots: SlotConfiguration) extends CypherRow {
      val longs = new Array[Long](slots.numberOfLongs)
      val refs  = new Array[AnyValue](slots.numberOfReferences)
      override def getLongAt(offset: Int): Long = longs(offset)
    ```
    It replaced `class MapCypherRow(private val m: mutable.Map[String, AnyValue])`
    (`CypherRow.scala`). `SlotConfiguration` is documented as *"**Immutable slot
    configuration.** Contains **array offsets** and type information of the driving table
    columns"*, and `SlotAllocation` runs over the **logical plan** — i.e. offsets are
    resolved at planning time. `LongSlot` vs `RefSlot` (`Slot.scala`) means node and
    relationship ids live **unboxed** in `Array[Long]`; only genuinely polymorphic values
    are boxed `AnyValue`.
  - Memgraph `src/query/interpret/frame.hpp`:
    ```cpp
    class Frame {
      const TypedValue &operator[](const Symbol &symbol) const { return elems_[symbol.position()]; }
      utils::pmr::vector<TypedValue> elems_;
    ```
    A fixed-size, arena-backed positional vector of **value-semantics tagged unions** —
    no per-value heap allocation at all.
- **What GoGraph does.** `type RowContext map[string]Value` (`cypher/expr/eval.go:30`).
  Per row, `populateRowCtx` (`cypher/api.go:10829`) does `for varName, colIdx := range schema { ctx[varName] = … }`
  — a map *iteration* plus one hash *write* per bound variable — and then `evalExpr`
  resolves each `*ast.Variable` with `row[n.Name]`, a hash *read* (`cypher/expr/eval.go:509`).
  The map container itself is pooled (`rowCtxPool`, `cypher/api.go:10760`; acquire/release at
  `10771`/`10808`), so the allocation is already gone — what remains is pure CPU.
- **Evidence (measured).** Isolated microbenchmark of exactly this mechanism, `-count=5`,
  **zero allocations on both sides** (so this is CPU only, not GC):

  | shape | map `RowContext` | positional slot | ratio |
  |---|---|---|---|
  | 1 var, 1 read | 69.1 ns/op | **0.98 ns/op** | **70×** |
  | 1 var, 2 reads | 74.7 ns/op | **1.77 ns/op** | 42× |
  | 4 vars, 4 reads | 118.3 ns/op | **3.23 ns/op** | 37× |
  | 8 vars, 8 reads | 191.0 ns/op | **6.50 ns/op** | 29× |

  Attribution run (1 var, 1 read, `-count=3`) splits the cost:

  | variant | ns/op |
  |---|---|
  | map cap 16 + `clear` (**what the pool does today**) | 67.2 |
  | map cap 16, no `clear` | 39.5 |
  | map cap 1 + `clear` | 46.0 |
  | map cap 1, no `clear` | 44.6 |

  So **~28 ns/row is the `clear()` alone**, because `acquireRowCtx` pools a map pre-sized
  to `rowCtxPoolMaxSchema = 16` (`cypher/api.go:10765`, `10777`) and Go's map `clear` is
  O(capacity), not O(len) — a 1-variable query pays to clear 16 slots on every row. The
  remaining ~39 ns is the map machinery itself. For calibration, the row-path conjunction
  query above costs 39.8 ms / 98 900 rows ≈ **402 ns/row total**, so the RowContext
  plumbing is on the order of **17 % of row-path CPU** at one variable and grows with
  schema width.
- **Lever.** Two tiers.
  - **Tier A (cheap, self-contained):** size the pooled map to the actual schema width —
    bucket `rowCtxPool` by width (1/2/4/8/16) instead of always handing out a cap-16 map.
    Measured headroom ≈ **28 ns/row**, no API change, no semantic surface.
  - **Tier B (the real lever, Neo4j's decision):** resolve variable→column **at plan time**
    and index positionally. Concretely: keep `exec.Row` as the carrier, and replace
    `RowContext` at the `expr` boundary with a small interface (`Lookup(slot int) Value`)
    plus a plan-time resolution pass that stamps each `*ast.Variable` / `*ast.Property`
    receiver with its slot. This is exactly `SlotAllocation` → `SlotConfiguration` →
    `SlottedRow.getRefAt(offset)`. The `map[string]Value` form must survive for the
    dynamic-name paths (`ast.MapProjection`, comprehension iteration variables at
    `expr/eval.go:1749/1857/1934`, `expr/list.go:136`) — those already allocate a fresh
    inner map, so keeping a map-backed implementation of the same interface is the
    natural fallback.
  - **Tier C (separate, see F5):** add the `longs Array[Long]` half.
- **TCK/ACID impact.** Tier A is a pool-sizing change with zero observable behaviour.
  Tier B is a representation change *inside* the evaluator: value semantics, 3VL, and
  every comparison stay in `expr.Equivalent`/`cmpInt64Float64`. The mandated gate is that
  the resolution pass is total — any expression kind it cannot resolve (subquery, pattern
  comprehension, dynamic key) must fall back to the map form, exactly as
  `analyseNodeScalarUse` already bails today (`cypher/api.go:13216`). No graph, WAL, or
  barrier code is involved. TCK 3897/3897 is the acceptance gate.
- **Effort.** Tier A: S. Tier B: L (touches every `expr` entry point and every operator
  that builds a RowContext).

---

### F4. The columnar aggregation fills its **argument** column through the boxed row evaluator  [NEW]  (severity: HIGH, effort S)

- **What they do.** DuckDB's `UpdateStates` and MonetDB/X100's hash aggregation (Boncz et
  al., CIDR 2005) scatter-add over an **unboxed** argument vector — the model
  `docs/columnar-deepening-design.md` §3 specifies for #2104: *"Vectorized argument
  accumulation (the O(input) de-box): … `MIN/MAX` over unboxed `[]int64`/`[]float64`."*
- **What GoGraph does.** `tryBuildColumnarAggInput` gives the **grouping key** an unboxed
  filler — `buildScalarPropertyFiller(nodeCol, propName, g, items[i].Eval)`
  (`cypher/api.go:12963`, implementation at `13579-13598`, which reads the property
  straight off the raw NodeID) — but gives every **aggregate argument** the boxed row
  evaluator unconditionally: `fillers[len(p.GroupBy)+j] = evalPutColumnFiller(items[len(p.GroupBy)+j].Eval)`
  (`cypher/api.go:12989`; `evalPutColumnFiller` at `13146-13155` just calls the row-mode
  `Eval` and `PutValue`s the boxed result). So the §3 "O(input) de-box" is delivered for
  the key and **not** for the argument.
  Additionally, `tryBuildColumnarAggInput` returns early on `len(p.GroupBy) == 0`
  (`cypher/api.go:12940`), so **every global aggregate is fully row-mode**.
- **Evidence (measured).** 100 000-node graph, 7 groups, `-benchtime=5x`:

  | Query | sec/op | allocs/op | allocs **per input row** |
  |---|---|---|---|
  | `RETURN n.w, count(*)` (no argument) | **14.3 ms** | 99 878 | **1.0** |
  | `RETURN n.w, min(n.v)` (property argument) | 84.2 ms | 774 290 | **7.7** |
  | `RETURN n.w, sum(n.v)` (property argument) | 77.2 ms | 774 295 | 7.7 |
  | `RETURN count(*)` (global, O(1) pushdown #2113) | **22.6 µs** | 31 | — |
  | `RETURN sum(n.v)` (global) | 74.5 ms | 774 222 | 7.7 |
  | `RETURN avg(n.v)` (global) | 71.8 ms | 774 221 | 7.7 |

  The 1.0 → 7.7 allocs/row step, and the 14.3 ms → 84.2 ms step, is entirely the argument
  filler: `count(*)` has no argument and therefore no `evalPutColumnFiller`.
- **Lever.** Use `buildScalarPropertyFiller` for aggregate arguments too whenever the
  argument is `node.prop` on a `NodeIDColumnProducer` child — the code, the fallback, and
  the byte-identity argument are all already written for the key path; it is a call-site
  change at `cypher/api.go:12989` plus the same `aggKeyPropertyItem`-style shape check.
  Separately, drop the `len(p.GroupBy) == 0` early return so global aggregates get the
  same unboxed pre-projection (the aggregation itself stays serial where no byte-identical
  combine exists — that is a different axis).
- **TCK/ACID impact.** `buildScalarPropertyFiller` already falls back to the same
  row-at-a-time `Eval` for any non-resolvable NodeID (`cypher/api.go:13590-13596`) and
  `fillScalarProperty` routes anything non-scalar through the canonical `lpgPropToExpr`
  (`13603`), so the value stream is byte-identical. Exact int64 `SUM` semantics
  (CIP2016-06-14) are unaffected — this changes only how the argument **column is
  filled**, not how it is accumulated. Gate with the existing
  `cypher/agg_columnar_grouping_test.go` differential suite extended to argument shapes.
- **Effort.** S.

---

### F5. Row-mode scalars are boxed in a Go interface; Neo4j has long slots, Memgraph has value-semantics unions  [NEW]  (severity: MEDIUM-HIGH)

- **What they do.** Neo4j `Slot.scala` distinguishes `LongSlot` from `RefSlot`, and
  `SlottedRow` stores node/relationship ids in `longs: Array[Long]` with `PRIMITIVE_NULL`
  as the sentinel — **no box for the entity-id column, which every scan produces and every
  operator carries**. Memgraph's `Frame` holds `TypedValue` **by value** in a PMR vector, so
  no scalar ever allocates.
- **What GoGraph does.** `type Row []expr.Value` where `Value` is an interface
  (`cypher/expr/value.go:101`) and `type IntegerValue int64` (`value.go:160`). Converting
  an `int64` ≥ 256 to an interface invokes Go's `runtime.convT64`, which heap-allocates 8
  bytes (values 0–255 come from `runtime.staticuint64s`). Every scan therefore boxes the
  node id once per row:
  ```go
  // cypher/exec/scan_label.go:109
  op.buf[0] = expr.IntegerValue(int64(op.iter.Next()))
  ```
- **Evidence (measured).** `go tool pprof -sample_index=alloc_objects -list` on the
  row-path conjunction query (`-benchtime=200x`, so the query loop dominates the graph
  build) attributes **9 928 855 objects — 25.22 % of all allocated objects in the profile —
  to that single line**. `lpgPropToExpr` (property boxing) is the next at 34.71 %.
  Cross-check: the columnar variant of the same query allocates **103 objects total** for
  98 900 rows, i.e. the box is entirely eliminated when the chunk chain engages.
- **Lever.** Add the long-slot half. Either (a) split `exec.Row` into
  `longs []int64` + `refs []expr.Value` with a plan-time slot type per column (the direct
  Neo4j translation, and the natural pairing with F3 Tier B), or (b) the cheaper Go-native
  option: intern node ids through a package-level `[]IntegerValue` cache for the common
  dense low range, which removes the allocation without a layout change but keeps the
  interface indirection. (a) is the correct end state; (b) is a stop-gap worth measuring
  first because it is ~20 lines.
  **Do not** reach for `unsafe`-tagged unions: Go's escape analysis and GC make a
  `struct{kind uint8; i int64; f float64; s string; ref any}` value-type row (the Memgraph
  shape) a legitimate alternative worth benchmarking — it trades 40–48 B/slot of stack/arena
  for zero heap traffic, which the existing `RowSlab` arena (`exec/row.go:78-95`) is
  already shaped to hold.
- **TCK/ACID impact.** Representation-only; every comparison already routes through
  `expr.Equivalent`/`Value.Equal`, and openCypher's int/float equivalence
  (`cmpInt64Float64`) is unaffected. The risk is aliasing: a value-type row must be copied,
  not referenced, at pipeline breakers — the existing `RowSlab.Reset` zeroing contract
  (`exec/row.go:145-153`) already encodes that discipline.
- **Effort.** (a) L (couples to F3 Tier B). (b) S.

---

### F6. No runtime observability: you cannot tell whether the vectorized or parallel tier engaged  [NEW]  (severity: MEDIUM)

- **What they do.** Neo4j prints the runtime in every plan header — the runtime-concepts
  page's own output shows `Runtime PIPELINED`, `Runtime version`, `Batch size 128`, and
  per-operator `Fused in Pipeline N` vs `In Pipeline 3`. Memgraph emits a
  `PARALLEL_EXECUTION_FALLBACK` notification with an explicit reason string
  (`"Query was not parallelized. Falling back to single threaded execution."`,
  `src/query/interpreter.cpp:3526-3533`).
- **What GoGraph does.** `Engine.Explain` (`cypher/api.go:1993`) renders the **logical**
  IR tree — it shows `Selection`, `Projection`, `NodeByLabelScan` with estimated rows, but
  nothing about which physical operator was built. There is exactly **one** columnar
  engagement counter in the whole engine, `cypher.exec.columnar_filter.batch`
  (`cypher/exec/columnar_filter.go:39`); `ColumnarProject`, `columnarExpand`,
  `ColumnarHashJoin`, the columnar aggregation kernel, `ParallelScanProject` and
  `ParallelAggregateScan` have none. The build counters
  `parallelScanProjectBuildCount` / `parallelAggregateScanBuildCount`
  (`cypher/api.go:7772`, `8158`) are unexported package variables, not metrics.
  (`docs/optimisations.md` line 227 records per-operator engagement counters as tracked
  work item #2123 — still unshipped at `6f31f61`.)
- **Evidence.** I could only establish F1 and F2 by adding a metrics backend and by
  differential benchmarking. A user cannot do that. Every finding in this report is
  invisible from the product surface.
- **Lever.** (1) Render the **physical** plan in `Engine.Explain`/`PROFILE`, annotating
  each operator with its concrete type and a `columnar` / `parallel(workers=N)` /
  `row` tag — Neo4j's `Fused in Pipeline N` is the model. (2) Emit an engagement counter
  per columnar and parallel operator (finishing #2123). (3) Emit a **decline reason** when
  a recogniser rejects a shape (Memgraph's `PARALLEL_EXECUTION_FALLBACK` is the model) —
  this is what turns F1/F2 from an audit finding into something a user self-diagnoses.
- **TCK/ACID impact.** Purely additive diagnostics; `Engine.Explain` is already documented
  as reading counts live without a `View` barrier (`cypher/api.go:2012-2013`). No execution
  path changes. TCK unaffected (EXPLAIN output is not TCK-covered).
- **Effort.** (1) M, (2) S, (3) S.

---

### F7. Results are fully materialised under the visibility barrier; Memgraph explicitly rejected that design  [CONFIRMED-R1]  (severity: MEDIUM — gated on the snapshot root)

- **What they do.** Memgraph's Bolt v4 design post states the goal verbatim: *"keeping the
  old, lazy behaviour while **not keeping any of the results in memory**"* and *"we prepare
  all the necessary resources for the execution and **ask for the next result only when
  it's needed** after which the results are streamed instantly to the client"* — full
  materialisation was rejected as *"too inefficient memory-wise"*. The mechanism
  (`interpreter.cpp` `PullPlan::Pull`) speculatively pulls one extra row, parks it in the
  `Frame`, and replays it at the head of the next `PULL` to answer "is there more?".
- **What GoGraph does.** `Engine.Run` opens `e.g.View(func(){ … })` at `cypher/api.go:1900`
  and calls `r.materialize()` at `cypher/api.go:1977`, inside the closure. The columnar
  drain does the same: `materializeColumnar` (`cypher/api.go:4069`) loops
  `cp.FillChunk(r.matChunk, want)` until the child is exhausted, growing one chunk that
  holds the whole result column-major. Peak engine heap is **O(result size)** on both paths.
  `docs/result-streaming-design.md` is the authoritative record and remains accurate at
  `6f31f61`: releasing `visMu.RLock` mid-drain would permit a torn cross-substructure read,
  and holding it across the consumer would starve the single writer.
- **Evidence.** `docs/result-streaming-design.md` §"Why the barrier cannot simply be
  released mid-drain"; `graph/lpg/lpg.go:411-417` (`View` holds `RLock` for the whole
  closure); the absence of any `type Snapshot` / `atomic.Pointer[Snapshot]` in `graph/`.
- **Lever.** Unchanged from round 1: the per-shard `Snapshot` root behind
  `atomic.Pointer[Snapshot]` (round-1 T1.3 / rmp #1671/#2051), after which streamable
  shapes (plain `MATCH … RETURN <non-aggregating projection>`, no `ORDER BY`/`DISTINCT`/
  aggregate) return a lazy producer over the pin. **Nothing new to take from the
  incumbents' mechanism** — Memgraph can stream because it has MVCC deltas, which round 1
  deliberately rejected (Fekete 2005). What *is* newly usable is Memgraph's
  **speculative-one-row lookahead** for `has_more`: GoGraph's Bolt PULL path
  (`bolt/server/session.go`) currently reads positionally from an already-materialised
  result, and the lookahead is the exact protocol-level primitive it will need on the day
  streaming lands.
- **TCK/ACID impact.** The lever is *gated on* ACID: streaming without the snapshot root
  breaks Isolation, and `cypher.TestIsolation_Cypher_NoPartialWriteObservable` would trip.
  The row cap and byte budget stay as a backstop. Do not ship streaming before the root.
- **Effort.** L (the foundation is multi-task; streaming itself is M on top of it).

---

### F8. Expression evaluation re-dispatches the AST per row; Neo4j compiles, Memgraph does not  [NEW]  (severity: MEDIUM)

- **What they do.** Neo4j compiles expressions. `internal.cypher.expression_engine`
  (`@Internal`) `@Description`: *"Choose the expression engine. **The default is to only
  compile expressions that are hot**, if 'COMPILED' is chosen all expressions will be
  compiled directly and if 'INTERPRETED' is chosen expressions will never be compiled."*
  `internal.cypher.expression_recompilation_limit = 10` — *"Number of uses before an
  expression is considered for compilation"*. Publicly exposed as
  `CYPHER expressionEngine=default|interpreted|compiled`. This applies even to the
  *slotted* runtime in Enterprise (concepts-page footnote).
  Memgraph does **not**: `ExpressionEvaluator : public ExpressionVisitor<TypedValue>`
  (`src/query/interpret/eval.hpp:275`), 42 `Visit` overloads, zero `jit|llvm|codegen`
  hits — it mitigates with a per-row `property_lookup_cache_` and a `FrameChangeCollector`
  that memoises `IN`-list evaluation instead.
- **What GoGraph does.** `evalExpr` (`cypher/expr/eval.go:487`) is a Go type-switch over
  the AST, re-walked for every row via the `newRowPredicate` closure
  (`cypher/api.go:13214-13223` → `evalRowPooled` → `expr.Eval`). Same model as Memgraph.
- **Evidence.** Structural (the type switch is re-entered per row). I did not isolate its
  cost from the RowContext cost in F3 — the two are entangled in the same 402 ns/row.
  **The experiment that would settle it:** build a plan-time closure tree for one
  representative predicate (`n.v > 10 AND n.v < 100`) and benchmark it against
  `expr.Eval` on the same AST at 1/2/4 operand depth, reporting ns/op with allocs held at
  zero. I am not claiming a number I did not measure.
- **Lever.** Go has no JIT, so Neo4j's bytecode generation is not directly portable. The
  portable equivalent — and the standard technique in Go analytics engines — is
  **closure-tree compilation**: walk the AST **once per plan** and emit a tree of
  `func(Row) (Value, error)` closures, so the per-row cost is a chain of direct calls
  instead of a type switch plus interface dispatch. This composes naturally with F3
  Tier B (the closure captures its slot offset instead of a string key), which is why
  F3 should land first. Rank it *after* F1/F2/F4 — those are 2–4× with S/M effort;
  this one is unquantified until the experiment above is run.
- **TCK/ACID impact.** The closure tree must be built from the same `evalExpr` branch
  bodies, not reimplemented, so 3VL, NULL propagation and int/float promotion stay
  byte-identical. Any node kind without a closure form falls back to `evalExpr`.
  TCK 3897/3897 is the gate.
- **Effort.** L.

---

### F9. Memory is bounded per-result and per-breaker, but there is no single pool spanning a query's intermediates  [NEW]  (severity: LOW-MEDIUM)

- **What they do.** Neo4j has a **hierarchy of pools**: `db.memory.transaction.max`
  (per transaction, default 0 = unlimited) ⊂ `db.memory.transaction.total.max` (per
  database) ⊂ `dbms.memory.transaction.total.max` (whole DBMS, default 70 % of heap) — all
  Community-available, all dynamic, and all covering *transaction* memory, i.e. sort
  buffers, hash tables and aggregation state, not just the result. Exceeding any of them
  raises `TransactionOutOfMemoryError` (GQLSTATUS 51N73) and terminates. Memgraph has
  `--memory-limit` plus per-query `QUERY MEMORY LIMIT n MB` enforced through a
  `memory_tracker` installed on the executing thread (`interpreter.cpp` `PullPlan::Pull`).
- **What GoGraph does.** Bounds exist and are good, but they are **disjoint**:
  per-result rows `DefaultMaxResultRows = 10_000_000` (`cypher/api.go:3200`), per-result
  bytes `DefaultMaxResultBytes = 1 GiB` (`api.go:3230`), an engine-wide **result** ceiling
  defaulting to half of `GOMEMLIMIT` (`globalMemBudget`, `api.go:3270`, charged in 1 MiB
  chunks, `api.go:3262`), and independent per-operator caps `DefaultMaxSortRows = 10M`
  (`exec/sort.go:36`), `DefaultMaxDistinct = 10M` (`exec/distinct.go:27`),
  `DefaultMaxGroups = 1M` (`exec/eager_aggregation.go:43`), `MaxCollectItems`. A query that
  sorts 9M rows *and* distincts 9M rows *and* holds 900k groups passes every cap while
  using far more than any one of them admits, and none of it is charged to `globalMemBudget`.
- **Evidence.** Read from the constants above; `globalMemBudget.charge` is called only
  from the two `Result` drain paths (`cypher/api.go:4026`, `4041`).
- **Lever.** Thread the existing `globalMemBudget` (or a sibling `queryMemBudget`) into the
  pipeline breakers so `Sort`, `Distinct`, `EagerAggregation` and `ColumnarHashJoin` charge
  and release against **one** counter, exactly as Neo4j's transaction pool does. The
  charge/release plumbing already exists; only the call sites are missing. Keep the
  per-operator caps as a second-line backstop.
- **TCK/ACID impact.** A new failure mode must map to a *transient* error like the existing
  `ErrGlobalMemoryExceeded` (`api.go:3249`), not a client error, and must trip
  deterministically at the same point on the columnar and row paths — the discipline
  `materializeColumnar` already follows (*"a cap trips at the identical point"*,
  `api.go:4062-4068`). No TCK scenario exercises memory exhaustion, so 3897/3897 is
  unaffected provided the caps stay far above TCK-sized inputs.
- **Effort.** M.

---

### F10. `ParallelGovernor` over-subscribes in the transition zone  [CONFIRMED-R1 / tracked]  (severity: LOW)

`ParallelGovernor.Enter` samples `inflight` once and racily (`exec/parallel_governor.go:59-62`),
so an early query in a partially-loaded system can grab a budget > 1 and oversubscribe
already-busy cores. `docs/benchmarks/history/LEDGER.md` row 0025 records the residual
honestly: at conc = 8 on a 10-core box the parallel arm is ≈ 8.6 % **slower** than serial
in both the pre- and post-#2115 builds. This is already tracked as rmp #2125; I confirm it
is still present at `6f31f61` (the file is unchanged) and I have **nothing to add from the
incumbents** — Neo4j's own guidance is the same admission (*"the overall throughput of the
database may decrease as a result of running many concurrent queries. The parallel runtime
is accordingly not suitable for transactional processing queries with high throughput
workloads"*), and Memgraph documents no cross-query governor at all. GoGraph's governor is
already the most sophisticated of the three; the fix is a policy refinement (hysteresis, or
a CAS-based reservation rather than a post-hoc sample), not a borrowed design.

---

## Reconciliation with the cross-stream traversal benchmark

The coordinator measured GoGraph (embedded, in-process, **no serialisation**) against
Memgraph 2.22 over Bolt in Docker, at 20 000 nodes / 200 k edges. Reproduced:

| Query | GoGraph | Memgraph | ratio |
|---|---|---|---|
| 1-hop expand from a bound node | 8.711 ms | 286 µs | 30× |
| 2-hop DISTINCT | 14.046 ms | 341 µs | 41× |
| var-len 1..3 DISTINCT | 15.113 ms | 442 µs | 34× |
| shortest path ≤6 | 23.797 ms | 395 µs | 60× |
| triangle count | 22.506 s | 210 ms | 107× |
| **global count** | **540 µs** | 1.217 ms | GoGraph 2.3× |
| **multi-label conjunction** | **16 µs** | 862 µs | GoGraph 54× |
| **single-node write** | **14 µs** | 289 µs | GoGraph 21× |

**Nothing here contradicts my findings. The split between the wins and the losses is
exactly the split my findings predict**, and that is the strongest evidence in this report.

### The win/loss boundary is "does this query run a per-row loop in row mode?"

The three queries GoGraph **wins** are precisely the three that never enter a per-row loop:
- *global count* — the O(1) count pushdown (#2113, `exec/all_nodes_count_scan.go`); I measured
  22.6 µs at **100 000** nodes, five times faster than their 540 µs at 20 000, which is
  consistent with their figure carrying harness/parse overhead my steady-state `-bench` loop
  amortises. Either way: no rows are touched.
- *multi-label conjunction* — 16 µs is the min-cardinality label anchor (#2077) resolving
  through roaring bitmaps. No scan at all.
- *single-node write* — no read pipeline.

Every query GoGraph **loses** anchors on `MATCH (label {prop: $param})` and therefore runs a
**full label scan evaluated row-at-a-time**, which is the exact path I measured at
**398–511 ns per scanned row** (`WHERE n.v > 10 AND n.v < 100000 RETURN n.v` over 100 000
nodes: 39.8 ms parallel-eligible, 51.1 ms with `DisableParallelScan`).

### The arithmetic

20 000 nodes × 398–511 ns/row = **7.96 – 10.2 ms**. The measured 1-hop is **8.711 ms** —
inside that interval, near its midpoint. The 2-hop (14.046 ms) is that anchor plus ≈5.3 ms
of second-hop expansion; the var-len 1..3 (15.113 ms) and shortest-path (23.797 ms) are the
same anchor plus their traversals.

So **yes — my row-vs-columnar / RowContext analysis explains the residual the coordinator
could not attribute**, and it explains the gap between their 700 µs estimate and the observed
8.7 ms. Their 700 µs is the cost of *iterating* 20 000 nodes (≈35 ns/row: roaring iteration
plus a tombstone check). What they did not price is what the engine does *per row on top of
that iteration*, which at `6f31f61` is: box the NodeID into an interface
(`exec/scan_label.go:109`, F5 — measured at 25.2 % of all allocated objects), `clear()` a
cap-16 pooled map and rebind the row through string hashing (F3 — measured 67 ns/row at one
variable), re-walk the predicate AST (`expr/eval.go:487`, F8), fetch the property, and box it
through `lpgPropToExpr` (34.7 % of allocated objects). That is the missing **11–15×** between
35 ns/row and 398–511 ns/row.

And the reason none of it is vectorized away is **F1b**: the anchor predicate sits *below* the
`Expand`, and both columnar recognisers require it *above*. This query shape can never reach
the chunk pipeline at v0.10.0 no matter how selective it is.

### What my analysis does **not** explain — stated plainly

1. **The asymptotics, which are the first-order cause.** Even if every runtime lever in this
   report landed, the anchor would still be O(|:Person|) instead of O(1). At my measured
   *columnar* constant (139 ns/row) 20 000 rows is **2.8 ms** — a 3.1× improvement on 8.711 ms,
   and still **10× slower than Memgraph's 286 µs**. **The runtime is the constant factor; the
   planner is the exponent.** The coordinator's finding #2 (integer-valued properties are never
   indexed, so the anchor is a full label scan) is the dominant defect, and my findings are
   multiplicative with it, not a substitute. Fix the index first; fix the constants second.
2. **Triangle count (22.506 s, 107×).** This is not mine. A cyclic 3-pattern executed as
   left-deep binary joins is Θ(m²)-ish, and round 2 already filed both `ExpandInto`
   (quadratic-in-degree today) and WCOJ for exactly this shape. The row-mode per-row tax
   compounds it — every intermediate tuple in that nested loop pays the same ~400 ns — but the
   107× is an algorithmic gap, not a runtime-constant gap. Do not let my findings absorb it.
3. **Parallelism is not the explanation.** F2 does *not* apply here: at 20 000 nodes GoGraph is
   below `DefaultParallelScanThreshold = 50_000` (`cypher/api.go:774`), so no parallel path
   would engage even without labels — and Memgraph 2.22 predates its Parallel Runtime (3.8,
   Enterprise), so it is single-threaded too. **Both engines are single-threaded in this
   benchmark.** F2 remains a large missed opportunity at production scale; it is not a cause
   of this deficit.
4. **I did not run their queries.** Everything above extrapolates my per-row constants,
   measured on a uniform 100 000-node synthetic graph with a 2-predicate integer filter, onto
   their 20 000-node topology. The agreement is close enough to be persuasive but it is
   arithmetic, not a direct measurement.

### The one command that falsifies this

`Engine.Explain("MATCH (p:Person {id:$id})-[:KNOWS]->(f) RETURN f.name", params)`.
If the anchor renders as `NodeIndexSeek`, my arithmetic is wrong and the 8.7 ms is coming from
somewhere I have not looked. If it renders as `NodeByLabelScan` under a `Selection` (which is
what F1b predicts, and what the coordinator's finding #2 independently implies), the
explanation above holds. Second check: re-run the 1-hop with
`EngineOptions{DisableParallelScan:true}` — the time should be **unchanged**, confirming point 3.

---

## Nothing-to-take list

1. **Result spilling to disk.** Neither incumbent spills. Neo4j: *"When any of the limits
   are reached, the transaction is terminated"*; the Aura troubleshooting page's own remedy
   for a sort OOM is *"try running the same query without using a sorting operation like
   ORDER BY or DISTINCT"* — advice no spilling engine would give. Memgraph: *"while
   executing queries, all the graph objects used in the transactions still need to be able
   to fit in the RAM, or Memgraph will throw an exception"*, **even in `ON_DISK_TRANSACTIONAL`
   mode**. GoGraph's fail-fast caps are at parity with the state of the practice for
   graph engines. **Do not build a spill path.** (This settles the brief's "does anything
   spill?" question: no, and neither do they.)
2. **Query-timeout configuration.** Neo4j's `db.transaction.timeout` defaults to `0s` =
   disabled; Memgraph's defaults to 600 s. GoGraph is `context.Context`-native throughout
   (138 `ctx.Err()` sites in `cypher/exec`) with a 30 s Bolt default
   (`bolt/server/serve.go:71`). For an embeddable Go library, threading the caller's
   context is strictly better than a server-global knob. Nothing to take.
3. **Making parallelism opt-in / licensed.** Both incumbents gate it behind Enterprise
   *and* explicit syntax (`CYPHER runtime=parallel`; `USING PARALLEL EXECUTION` plus a
   `PARALLEL_EXECUTION` privilege plus a `priority_queue` scheduler). GoGraph engages
   automatically above a threshold and governs the fleet centrally. Keep it.
4. **Copying Neo4j's parallel-runtime restrictions.** Neo4j's parallel runtime is read-only
   and errors `22N46` on any write *or* on any transaction whose state has changed; it also
   silently demotes to pipelined for 21 named non-thread-safe procedures. GoGraph's
   restriction (writes are single-writer by design) is a consequence of a
   correctness-first architecture, not an unimplemented feature.
5. **Selection vectors in the `Chunk` contract.** `docs/columnar-deepening-design.md` §2's
   rejection still holds: GoGraph's carrier is narrow, the downstream wants dense random
   access, and DuckDB uses selection vectors because its downstream is a long columnar
   scan (Abadi et al., FnT 2012 §4.4). Nothing in this audit overturns it — F1 is a
   *coverage* problem, not a compaction-strategy problem.
6. **Vectorizing the traversal itself.** `docs/columnar-deepening-design.md` §7's reduced
   scope is correct and is corroborated by the incumbents: Memgraph's parallel operator set
   has **no** `ExpandParallel` (`grep "ExpandParallel|ParallelExpand"` → zero hits), and
   Neo4j's parallel runtime partitions leaves, not expansions. Pointer-chasing is not
   scan-heavy. `columnarExpand` (`exec/expand.go:663-748`) is correctly scoped as output
   plumbing.
7. **The columnar tier as a concept.** Worth restating: GoGraph has something neither
   incumbent has. When it engages it is **103 allocations for a 98 900-row projection**
   against 347 545 for the same query written with an `AND`. The finding of this stream is
   not that the tier is wrong — it is that it is switched off by ordinary syntax.

---

## Corrections to prior rounds

- **[STALE] "Neo4j's four runtimes."** There are **three**. The Cypher manual: *"In
  Cypher®, there are three types of runtimes: slotted, pipelined, and parallel"* and *"The
  slotted runtime … replac[ed] the original (and slower) interpreted runtime, **which is
  now retired**."* At source level `CYPHER runtime=interpreted` is silently **aliased to
  slotted** (`CommunityRuntimeFactory.scala`); the real interpreted runtime survives only
  behind an undocumented `runtime=legacy`. Community's default is **slotted**, not
  interpreted; Enterprise's and all Aura tiers' default is **pipelined**.
- **[STALE] Round-1 T1.2 "positional slot-array RowContext".** `exec.Row` is already
  positional (`exec/row.go:44`). The live defects are (a) the per-row string-keyed
  *rebinding* in `expr.RowContext` (F3) and (b) the interface *element type* (F5). Both
  are real; the original framing conflated them.
- **[STALE] `docs/columnar-deepening-design.md` §2's "predicates arrive pre-combined into
  one `ChunkPredicate`".** Not true at v0.10.0 — `buildColumnarPredicate`
  (`cypher/api.go:13248`) matches a single `*ast.BinaryOp` and nothing else. The design
  document should be corrected.
- **[CONFIRMED] Round 1's "GoGraph is ahead of OSS peers (vectorized + parallel
  default-on)"** — confirmed and strengthened: Memgraph's Parallel Runtime is Enterprise-only
  behind `#ifdef MG_ENTERPRISE` (`src/query/plan/parallel_checker.hpp`), needs an explicit
  clause, a privilege and a specific scheduler, and covers only the plan segment between a
  scan and an aggregation/`ORDER BY`. Neo4j's is Enterprise-only and read-only.
- **[CONFIRMED] Round 1's "bit-identical parallel results".** Verified in code: the
  position-carrying min/max combine is documented and reasoned at
  `exec/parallel_aggregate_scan.go:16-48`, resolves int/float, ±0 and NaN ties by lowest
  global scan index, and reproduces serial group-creation order. Both incumbents have
  shipped ordering bugs in their parallel paths (neo4j#13382; memgraph #3815/#3876), so
  this is a genuine differentiator, not table stakes.
- **[CONFIRMED] Round 1's "Neo4j Enterprise Morsel is the best runtime."** Still true on
  *coverage* and *fusion*, and their published number is 56 200 ms → 1 714 ms (≈30×) going
  from pipelined-on-8-CPU to parallel-on-96-CPU. But note their pipelined-vs-slotted claim
  is only *"about twice as fast"* (Neo4j developer blog, Christoffer Bergman) — GoGraph's
  measured columnar-vs-row gap on the shapes where its tier engages is **2.9–4.4×**, i.e.
  larger. The gap to close is coverage, not peak.

---

## Ranked lever list

| # | Lever | Measured / cited justification | Effort | Finding |
|---|---|---|---|---|
| 1 | **Admit `Selection` *below* `Expand`** — vectorize the anchor filter, not only the post-traversal filter | explains the 8.7 ms 1-hop anchor in the cross-stream benchmark; every operator needed already exists, only the recogniser refuses to assemble them | M | **F1b** |
| 2 | Conjunction + label-predicate `ChunkPredicate`, and adjacent-`Selection` fusion | 4.4× / 1 640× allocs on `(a:P)-[:K]->(m:P)`; 2.9× / 3 374× on `AND` | S+M | F1 |
| 2= | Partitioned label scan for `ParallelScanProject` / `ParallelAggregateScan` | 2.0–3.7× lost to a label; parallelism itself worth 4.6× at conc=1 (controlled) | M | F2 |
| 3 | Unboxed aggregate-**argument** filler (+ drop the `len(GroupBy)==0` early return) | 1.0 → 7.7 allocs/row, 14.3 ms → 84.2 ms between `count(*)` and `min(n.v)` | **S** | F4 |
| 4 | Chunk-transparent `Limit`/`Skip` | inert `LIMIT 100000` costs 4.3× / 2 414× allocs | M | F1(3) |
| 5 | Size-bucketed `rowCtxPool` (stop clearing a cap-16 map for a 1-var schema) | 28 ns/row, isolated | **S** | F3-A |
| 6 | Positional slot binding (Neo4j `SlotConfiguration`/`SlottedRow`, Memgraph `Frame`) | 29–70× on the mechanism; 68–184 ns/row | L | F3-B |
| 7 | Runtime observability: physical plan in EXPLAIN + engagement counters + decline reasons | F1 and F2 were undiscoverable without a custom metrics backend | S+M | F6 |
| 8 | Long slots / value-semantics scalars | one scan line = 25.2 % of allocated objects | S(b)/L(a) | F5 |
| 9 | Single query-memory pool across pipeline breakers | Neo4j's three-level transaction pool; GoGraph's caps are disjoint | M | F9 |
| 10 | Closure-tree expression compilation | Neo4j compiles after 10 uses; **cost in GoGraph not yet isolated — run the experiment first** | L | F8 |
| 11 | Result streaming | blocked on the snapshot root (round-1 T1.3); Memgraph's lookahead is the protocol primitive to reuse | L | F7 |

---

## NOT INVESTIGATED

Scoped in the brief but not reached; listed so no reader mistakes silence for a clean bill.

- **`cypher/exec/varlen_expand.go` and `shortest_path.go` (2 311 lines) as runtime paths.**
  I confirmed only that they are *not* `ChunkProducer`s (they are absent from the six
  implementations of `FillChunk`) and that `docs/columnar-deepening-design.md` §4 deliberately
  keeps them row-mode. I did **not** profile them, and the cross-stream benchmark shows
  var-len at 34× and shortest-path at 60× — the worst non-triangle ratios. Someone should
  profile these two files specifically; my per-row constants explain their *anchor*, not
  their traversal loop.
- **`ColumnarHashJoin` (`exec/hash_join_columnar.go`, 568 lines).** Read its interface and
  its `DefaultChunkCapacity` sizing; never benchmarked. `docs/optimisations.md` reports
  −22.4 % allocs / −5.7 % time from #2105 and I did not verify or challenge that.
- **`CorrelatedApply` / `RollupApply` / subquery execution** (`exec/correlated_apply.go`,
  `rollup_apply.go`). Per-outer-row inner-plan re-execution is a classic runtime cliff and
  `EXISTS {}` / `COUNT {}` route through it (`expr.SubqueryEvaluator`, `expr/eval.go:59-66`).
  Entirely unexamined.
- **Write-path execution** (`create_node.go`, `merge*.go`, `set*.go`, ~5 000 lines). The
  cross-stream benchmark shows single-node write *winning* at 14 µs, so I deprioritised it.
  No bulk-write or `UNWIND … CREATE` measurement was taken.
- **The cost of `evalExpr`'s AST re-dispatch in isolation (F8).** Named as an experiment,
  deliberately not claimed as a number.
- **Behaviour under real concurrency.** Every measurement here is concurrency-1. The
  `ParallelGovernor` transition-zone residual (F10) and the columnar tier's behaviour under
  8/64/256 concurrent queries are taken from `docs/benchmarks/history/LEDGER.md` row 0025,
  not re-measured.
- **Bolt-path runtime cost.** `bolt/server/session.go` PULL chunking over a materialised
  result was read for F7 but not benchmarked; that is stream 9's surface.
- **`cypher/funcs` call path.** The brief asked about "the function-call path"; I established
  only that `BuiltinFn = func(args []Value) (Value, error)` (`expr/eval.go:41`) forces a boxed
  `[]Value` per call. No measurement.

---

## Measurement harness

All benchmarks ran from a scratch package `bench/s05probe` inside the module (deleted after
the audit), against engines built with `adjlist.Config{Multigraph:true, Directed:true}`.
Every comparison pair returns a **verified-identical row count** (reported as a custom
`rows/op` metric) so the comparison is apples-to-apples. Columnar engagement was detected by
installing a counting `metrics.Backend` and reading `cypher.exec.columnar_filter.batch`
around each `Engine.Run`. The parallelism control used a second engine over identical data
built with `cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})`.
Allocation attribution used `-memprofile` at `-benchtime=200x` so the query loop dominates
the graph-build allocations, then `go tool pprof -sample_index=alloc_objects -list`.
Raw output: `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/48f01b28-6444-4504-ac86-d3a529405419/scratchpad/audit3/shape.txt`.

**Honest caveats.** (i) All numbers are single-machine, 10-core, concurrency-1; the parallel
findings will compress under saturation exactly as LEDGER row 0025 documents — F2's lever is
worth most on an idle-core box and least on a saturated one. (ii) The F3 microbenchmark
models the RowContext *mechanism*, not a whole predicate evaluation; the 17 %-of-row-path
figure is a division of two independently measured quantities, not a profiler attribution.
(iii) F8's cost is explicitly **not** measured — the experiment to run is named in the
finding. (iv) The synthetic graphs are uniform; a skewed property distribution would change
selectivity but not the engaged-vs-not-engaged conclusion, which is structural.
