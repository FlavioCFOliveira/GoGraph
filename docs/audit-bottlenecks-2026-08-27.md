# Bottleneck audit — 2026-08-27

**Question:** where are the bottlenecks, and which *small* changes carry the *largest* gains?

**Scope:** the `GoGraph` module only (`graph/`, `cypher/`, `store/`, `search/`, `bolt/`, `ds/`,
`metrics/`). `examples/`, `bench/` and `internal/sim` were used as instruments, never as targets.

**Phase:** diagnosis only. Nothing was optimised. No module source file was modified — the only
tree change is the new instrument at `bench/audit352/`.

**Tree:** branch `feature/352-deep-profiling-and-optimization`, HEAD `473e7323`.

> **Every number in this document was measured in this session, at this HEAD, on this host.**
> No figure is carried forward from the 2026-08-11 peer audit or from any backlog task
> description. Where a prior claim could not be reproduced it is recorded in
> [§5 Refuted premises](#5-refuted-premises), which is as much a result as any finding.

---

## 1. Ranked "small change, big gain" list

Ranked by measured gain × confidence ÷ effort. Every entry names the experiment that produced
its number.

### SCB-1 — Fuse `ORDER BY … SKIP … LIMIT` into `Top`, as `ORDER BY … LIMIT` already is

**Measured gain: 12.13× (877.5 ms → 72.3 ms) and 8.5× less memory, on the most common
pagination shape there is.** Confidence: very high.

`ORDER BY x LIMIT 10` plans as `Top` and costs 72.33 ms. Adding **`SKIP 0`** — a semantic
no-op — collapses the plan to a full `Sort` of all 120 000 rows and costs **877.52 ms**.

| shape | plan | ns/op (median, n=8) | ±% | B/op | allocs/op |
|---|---|---|---|---|---|
| `ORDER BY p.salary LIMIT 10` | `Top` | **72,331,396** | 1.8 | 184 MB | 2,287,781 |
| `ORDER BY p.salary SKIP 0 LIMIT 10` | `Limit→Skip→Sort` | **877,521,250** | 0.7 | 1,571 MB | 12,995,008 |
| `ORDER BY p.salary SKIP 100 LIMIT 10` | `Limit→Skip→Sort` | 876,280,479 | 0.7 | 1,571 MB | 12,995,787 |
| `ORDER BY p.salary SKIP 10000 LIMIT 10` | `Limit→Skip→Sort` | 879,659,875 | 1.3 | 1,571 MB | 12,994,021 |
| `ORDER BY p.salary LIMIT 110` | `Top` | 77,104,316 | 3.4 | 191 MB | 2,342,041 |

The last row is the proof that the fix is cheap: `Top` at **LIMIT 110** costs only 6.6% more
than `Top` at LIMIT 10. So `SKIP 100 LIMIT 10` is exactly `Top(110)` followed by dropping 10 —
**77 ms instead of 876 ms, an 11.4× reduction**, and for `SKIP 0` an exact 12.13×.

**Change:** one planner peephole — rewrite `Limit(k) → Skip(s) → Sort` into
`Skip(s) → Top(s+k)`. Guard: `s+k` must not overflow and must stay below the point where a full
sort is cheaper.
**Risk:** low. `Top` already exists and is already used for the `SKIP`-less form; ordering
semantics under ties must be preserved (the existing `Top` already defines them).
**What would invalidate it:** if `Top`'s tie-breaking differs from `Sort`'s, the rewrite is not
order-preserving for equal keys. `cypher/sort_ties_test.go` and `cypher/order_tie_aggregation_test.go`
are the gate.
**Validation:** those tie tests, plus `TestPaginationPlans` and `BenchmarkPagination` in
`bench/audit352/`.

---

### SCB-2 — Make `COUNT { … }` not quadratic

**Measured gain: 1,203× at 2 000 nodes, 2,484× at 4 000, and the factor doubles with every
doubling of the graph.** Confidence: very high — the complexity class was fitted, not guessed.

`COUNT { MATCH (a)-[:R]->(:P) }` is **quadratic in the graph size**, while every sibling
construct that computes the same thing is linear. The fixture holds out-degree at exactly 1 for
every `n`, so the workload is O(n) by construction; the quadratic growth is entirely the
implementation's.

| shape | n=250 | n=500 | n=1000 | n=2000 | fitted exponent | r² |
|---|---|---|---|---|---|---|
| `plain_match` | 0.017 ms | 0.036 ms | 0.061 ms | 0.135 ms | 0.974 | 0.99477 |
| `optional_match` | 0.131 ms | 0.234 ms | 0.464 ms | 1.019 ms | 0.988 | 0.99565 |
| `EXISTS { }` | 0.112 ms | 0.225 ms | 0.457 ms | 0.970 ms | 1.039 | 0.99972 |
| pattern predicate | 0.125 ms | 0.266 ms | 0.498 ms | 1.059 ms | 1.016 | 0.99872 |
| **`COUNT { }`** | **21.299 ms** | **74.937 ms** | **274.927 ms** | **1167.354 ms** | **1.920** | **0.99894** |

At n=4 000 in the benchmark table: `COUNT { }` = 4,674,448,250 ns/op — **4.67 seconds** —
with 3.71 GB and 16,363,366 allocations per query, i.e. **4,091 allocations per output row** on a
graph whose every node has exactly one outgoing relationship. 4,091 ≈ n, which is the signature
of a full label scan per outer row.

Its plan is bare — `Project → NodeByLabelScan [P]` — so the subquery is evaluated inside the
projection expression per row, with no operator and no correlated seek. `EXISTS { }`, by
contrast, compiles to `SemiApply → CorrelatedApply → Filter → Expand` and is linear.

**Change:** give `COUNT { }` the correlated-apply treatment `EXISTS { }` already has, so the
inner pattern seeks from the bound outer row instead of re-scanning the label.
**Risk:** medium — it is a planner/evaluator change, not a peephole. But `EXISTS { }` is the
working template in-tree.
**What would invalidate it:** nothing measured; the exponent is 1.920 with r²=0.99894 across
four sizes.
**Validation:** `cypher/count_subquery_test.go`, `cypher/count_subquery_where_test.go`, the TCK
suite, and `TestScaling_SubqueryComplexity` in `bench/audit352/` as the regression gate.

---

### SCB-3 — Batch the `ParallelScanProject` result-budget counters per worker

**Measured gain: −26.71% wall clock (p=0.000, n=10) against a 0.51% noise floor — 52× the
noise floor. At 10 cores the isolated effect is −29.87%.** Confidence: very high — attribution
established by a single-variable same-process A/B *and* confirmed by a falsification test.

`exec.(*ParallelScanProject).overResultBudget` (`cypher/exec/parallel_scan_project.go:178`)
increments **two process-shared atomics for every produced row, from every parallel worker**:

```go
over := op.maxRows > 0 && op.sharedRows.Add(1) > op.maxRows
if op.maxBytes > 0 && op.estimateRow != nil {
    if op.sharedBytes.Add(op.estimateRow(row)) > op.maxBytes { over = true }
}
```

Both caps are **on by default** (`resolveMaxResultRows`/`resolveMaxResultBytes`, `cypher/api.go:1463`
and `:1503`, map the zero value to a finite default), so every ordinary `cypher.NewEngine(g)`
query on this path pays them.

In the CPU profile of `MATCH (p:Person) RETURN p.salary`, `sync/atomic.(*Int64).Add` is the
**single largest flat entry at 17.35%**, and `pprof -peek` attributes **100% of it** to
`overResultBudget` (19.46% cumulative).

The public sentinels `MaxResultRowsUnlimited` / `MaxResultBytesUnlimited` toggle each atomic
independently *without touching module source*, giving a genuine single-variable experiment.
`TestResultCapAB_Preconditions` proves all arms compile to the identical plan and ship the
identical 120 000 rows.

| arm | sec/op | vs default |
|---|---|---|
| `ctrl_a` (default) | 16.58m ± 3% | — |
| `rows_off` | 13.55m ± 1% | **−18.27%** (p=0.000) |
| `bytes_off` | 13.47m ± 1% | **−18.76%** (p=0.000) |
| `both_off` | 12.15m ± 8% | **−26.71%** (p=0.000) |
| `ctrl_b` (byte-identical to `ctrl_a`) | 16.49m ± 2% | −0.51% ← **noise floor** |

The two effects are *non-additive* (18.3 + 18.8 → 26.7), which is the signature of cache-line
contention rather than instruction count.

**Falsification test — the attribution is established, not hypothesised.** If the cost is
contention between parallel workers, the gap must grow with worker count and vanish at one:

| GOMAXPROCS | default | both_off | delta | noise floor (ctrl_a vs ctrl_b) |
|---|---|---|---|---|
| 1 | 54.24m | 53.36m | ~0% (p=0.394, **not significant**) | ~ |
| 2 | 28.20m | 27.59m | −2.17% | ~ |
| 4 | 16.41m | 15.68m | −4.45% | ~ |
| 8 | 16.82m | 12.77m | **−24.08%** | ~ |
| 10 | 16.38m | 11.49m | **−29.87%** | −0.48% |

Zero at one worker, monotonically growing to −29.87%. Exactly as predicted.

**A second consequence, visible in the same table:** in the default configuration this query
**stops improving after 4 cores** (16.41 → 16.82 → 16.38 ms), while with the counters removed it
keeps improving (15.68 → 12.77 → 11.49 ms). Cores 5–10 of this machine contribute *nothing* to
the default build of this shape. See §4 for the important hardware caveat on how to read that.

**Change:** per-worker local counters, reconciled into the shared totals every N rows (or once
per morsel) instead of every row. The existing design comment
(`parallel_scan_project.go:135-146`) already tolerates an overshoot of "plus one in-flight batch
per worker", so batched reconciliation sits **inside the guarantee that is already documented**.
**Risk:** low-medium. The counters implement a real memory-safety bound (#1830); the fix must
preserve it. Disabling the caps is *not* the proposed change — the A/B only bounds the ceiling.
**What would invalidate it:** if a batch size large enough to matter pushes the overshoot beyond
what `MaxResultBytes` is meant to guarantee. Choose N so that `workers × N × row_estimate` stays
within the existing tolerance.
**Validation:** `cypher/result_bytes_cap_test.go`, `cypher/result_rows_cap_test.go`,
`cypher/parallel_scan_project_budget_test.go`, plus `BenchmarkResultCapAB` and
`BenchmarkResultCapAB_Procs`.

---

### SCB-4 — Memoise relationship-property materialisation per row

**Measured gain: up to 4.51× on a query that reads the same relationship property five times;
1.46× just to make one property cost no more than the whole relationship.** Confidence: very high.

Reading **one** relationship property costs **more** than returning the **whole** relationship,
and cost grows linearly with the number of property *accesses* — not distinct properties.

| shape (4 000 relationships) | median ms | vs `1_prop` |
|---|---|---|
| `RETURN r` (whole relationship) | 3.107 | 0.76× |
| `RETURN r.w` | 4.113 | 1.00× |
| `RETURN r.w, r.k3` | 7.973 | 1.94× |
| `RETURN r.w, r.k3, r.k4` | 11.049 | 2.69× |
| `RETURN r.w, r.k3, r.k4, r.k1` | 16.609 | 4.04× |
| `RETURN r.w, r.k3, r.k4, r.k1, r.k2` | 18.982 | 4.62× |
| **`RETURN r.w, r.w, r.w, r.w, r.w`** | **18.542** | **4.51×** |

The last row is the clincher: reading the **same** property five times costs 4.51×, essentially
identical to reading five **different** properties (4.62×). There is **no memoisation at all** —
each `r.<prop>` occurrence independently materialises the relationship's entire property map.

Source agrees: `buildRelationshipValueFromRow` (`cypher/api.go:13662-13786`) unconditionally calls
`buildEdgeProps` (`:13776`), whose gate at `cypher/api.go:14017`
(`relUse != nil && !relUse.needsWholeNode && len(relUse.keys) == 0`) excludes exactly the
value-read case, so control falls through to `:14066-14076` and returns the whole map. There is
no `LazyRelationshipValue` sibling to `LazyNodeValue`. A single-key accessor exists
(`graph/lpg/edge_property.go:110`, `GetEdgeProperty`) but **no Cypher read path calls it** — its
only callers are the SET/REMOVE pre-image capture paths.

For contrast, the node side has the lazy path: `MATCH (p:P) RETURN p.sid` is 0.272 ms where the
relationship equivalent is 4.113 ms — **15.1×**.

**Change:** two independent wins, either alone worthwhile —
(a) memoise the materialised map per (row, relationship) so repeated accesses are free;
(b) add a lazy relationship value mirroring `LazyNodeValue`, routing single-key reads to the
existing `GetEdgeProperty`.
**Risk:** low for (a), medium for (b).
**What would invalidate it:** if relationship property maps must be snapshot-consistent *per
access* rather than per row — they are not; the row is already the visibility unit.
**Validation:** `cypher/entity_valued_prop_test.go`, `cypher/edge_property_date_roundtrip_test.go`,
the TCK relationship-property scenarios, plus `TestRelPropertyMaterialisationCount`.

---

### SCB-5 — Hoist `newSchemaWalk` out of the per-row path

**Measured: it is the single largest allocator of objects in the module's read path —
35.1% of all allocated objects on the scan shape, 26.6% on the sort shape — and it performs a
`slices.SortFunc` per output row.** Confidence: high on the cost, medium on the realised time win.

`newSchemaWalk` (`cypher/api.go:7583`) allocates a slice **and sorts it**:

```go
func newSchemaWalk(schema map[string]int) schemaWalk {
	w := make(schemaWalk, 0, len(schema))
	for name, col := range schema { w = append(w, schemaBinding{name: name, col: col}) }
	slices.SortFunc(w, func(a, b schemaBinding) int { return a.col - b.col })
	return w
}
```

It is called **per row** from `evalRowPooled` (`cypher/api.go:13357`) and from
`buildRowCtxWithUse` (`cypher/api.go:13232`).

- `MATCH (p:Person) RETURN p.salary` — `alloc_objects`: `newSchemaWalk` **35.09%** (39.3M objects),
  ahead of `lpg.Int64Value` 17.36%, `AllNodesScan.Next` 17.04%, `lpgPropToExpr` 16.75%.
- `ORDER BY … SKIP 0 LIMIT 10` — `newSchemaWalk` **26.60%**, `buildRowCtxWithUse` 26.37% flat
  (78.62% cum), `populateRowCtx` 26.32%: together **79.29% of all allocated objects**, all under
  `Sort.collectAndSort` (79.46% cum).

**This contradicts a "fixed" item.** Backlog #2415 is recorded as "row-context schema walk
hoisted (−12.6%)". The hoist is real **only** for `newRowPredicate` (`cypher/api.go:16013`,
outside the returned closure). The `evalRowPooled` and `buildRowCtxWithUse` paths still rebuild
the walk — and sort it — on every row. The comment at `cypher/api.go:7565-7579` claiming the walk
"is derived once for the whole execution" is accurate for one of three call sites.

**Change:** hoist the walk to plan-build time for the remaining two sites, as `newRowPredicate`
already does.
**Risk:** low — the schema is fixed for the life of the operator.
**What would invalidate it:** allocation share is not time share. On the shape where boxing was
isolated, removing 240 000 allocations per op produced **no measurable time change** (see
§5, RP-6), so an allocation-count win does not automatically become a wall-clock win. This entry
is ranked on a measured *allocation* share with an *unmeasured* time win, and is placed below
SCB-1…4 for exactly that reason.
**Validation:** an A/B of the hoist itself; `bench/cypher_alloc`'s zero-allocation gates.

---

### SCB-6 — Replace the `map[uint64]string` relationship-type filter with a bitmap

**Measured: the `:KNOWS` type filter costs 143 ns per candidate edge and adds 59.9% to a typed
1-hop traversal — on a graph where every single edge is `:KNOWS`, so the filter rejects nothing.**
Confidence: high.

| shape (960 000 edges) | median ns/op | ±% | allocs/op |
|---|---|---|---|
| `MATCH (a:Person)-->(b:Person) RETURN count(*)` (untyped) | 228,740,312 | 2.1 | 3,115,466 |
| `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN count(*)` | 365,621,944 | 4.1 | 3,115,477 |

Difference: **136.88 ms over 960 000 candidate edges = 142.6 ns per edge**, +59.9%.

The mechanism is a hashed map probe used as a *set membership test*
(`cypher/exec/expand.go:900-909`): `_, ok := op.edgeTypeFilter[pos]` where `edgeTypeFilter` is
`map[uint64]string` keyed by absolute CSR arc position — and the string value is never read. The
CPU profile confirms it: `runtime.mapaccess2_fast64` is **8.99%** on the typed arm versus 2.42%
on the untyped one.

**Change:** a bitmap over the same arc-position space — a branch-free bit test instead of a hash
probe, and ~1 bit per edge instead of a hashed bucket entry. The project already depends on
`roaring`, and already uses bitmaps for label resolution.
**Risk:** low. Same key space, same build point, same LRU caching and the same
`csrPairKey{epoch,startTS,versioned}` validation.
**What would invalidate it:** if arc positions are sparse enough that a dense bitmap wastes more
memory than the map — measure before committing; a roaring bitmap removes that risk.
**Validation:** `cypher/edge_type_filter_cache_test.go`,
`cypher/edge_type_filter_cache_internal_test.go`, `TestCSRPairCache_ConcurrentQueriesAgree`,
plus `BenchmarkExpandShape`.

---

### SCB-7 — Delete `exec.ChunkPool` (vacuous code) and the throwaway template chunk

**Measured gain: zero runtime. This is a hygiene item, ranked last deliberately.**
Confidence: very high on the fact, and on the fact that it is worth nothing in CPU.

`exec.ChunkPool` (`cypher/exec/chunk.go:1129`, with `NewChunkPool` `:1135`, `Get` `:1148`,
`Put` `:1154`) has **zero production call sites** across the whole repository. Its only
references are its own definition and two tests (`chunk_test.go:602-603`, `:629-632`). It even
wires metrics counters `cypher.pool.chunk.get` / `cypher.pool.chunk.put` (`:1149`, `:1155`)
that can never fire outside tests. Its doc comment ("Operators that process a high volume of
batches should obtain chunks from a shared pool") describes an intent no operator honours.

Two adjacent facts, both measured as *small*:

- The aggregation scratch chunk (`cypher/exec/eager_aggregation.go:409`) is allocated **per
  query**, not per chunk and not per row — it is reused across batches via `Reset()` at `:418`.
  So the backlog's "allocated per query instead of pooled" is factually right and
  **practically irrelevant**: one allocation per query.
- `cypher/exec/expand.go:1018` allocates a full-capacity **template chunk purely to read the
  child's column kinds, then discards it** — one wasted chunk per `NewOutputChunk` on the Expand
  path. Per-`Init`, not per-row.

**Change:** delete `ChunkPool` and its metrics, or wire it. Read the kinds without materialising
a chunk.
**Risk:** none for the deletion (nothing calls it).
**Why it is ranked last:** dead code costs zero CPU. It is listed because the hunt found it, not
because removing it will make anything faster.

---

## 2. Ranked bottleneck list

Ranked by measured impact on the workload that exposed it.

| # | Site | Measured share | Profile | Workload | Confidence |
|---|---|---|---|---|---|
| B-1 | `cypher` — `COUNT { }` subquery evaluation (no operator; `Project → NodeByLabelScan`) | **quadratic**, exponent 1.920 (r²=0.99894); 1,203× vs `EXISTS { }` at n=2000 | wall-clock complexity fit | `MATCH (a:P) RETURN a.sid, COUNT { MATCH (a)-[:R]->(:P) }` | very high |
| B-2 | `cypher` — `Sort` reached because `SKIP` blocks `Top` fusion | **12.13×** vs `Top`; 8.5× memory | benchmark, n=8, ±0.7–1.8% | `ORDER BY … SKIP 0 LIMIT 10` | very high |
| B-3 | `cypher/exec/parallel_scan_project.go:178` `overResultBudget` → `sync/atomic.(*Int64).Add` | **17.35% flat CPU** (100% of that symbol); **−26.71%** wall when removed, −29.87% at 10 cores | CPU profile + interleaved A/B | `MATCH (p:Person) RETURN p.salary` | very high |
| B-4 | `cypher/api.go:14066-14076` — whole relationship property map materialised **per property access** | **4.51×** for 5 reads of the same property; one property > whole relationship (1.46×) | wall-clock, medians of 3 | `MATCH ()-[r:R]->() RETURN r.w …` | very high |
| B-5 | `cypher/api.go:7583` `newSchemaWalk` (slice alloc **+ sort** per row) | **35.09%** of allocated objects (scan); **26.60%** (sort) | `alloc_objects` | scan and sort shapes | high (allocation); unmeasured (time) |
| B-6 | `cypher` — name-keyed `RowContext` map traffic (`populateRowCtx`, `mapassign_faststr`, `mapaccess2_faststr`, `memHashAES`) | **~30% CPU** (sort shape), 15.6% (composed DST), 12.8–27.0% (expand) | CPU profiles | every row-path shape measured | high |
| B-7 | `cypher/exec/expand.go:907` `map[uint64]string` type-filter probe | **142.6 ns per candidate edge**, +59.9% on typed traversal; `mapaccess2_fast64` 8.99% | benchmark diff + CPU profile | typed vs untyped 1-hop | high |
| B-8 | `cypher.(*planCache).get` — single global `sync.Mutex` | **99.94% of all mutex contention** (19.50 s of 19.51 s) | mutex profile, 640 goroutines | concurrent reads | high (contention) / **refuted as a throughput limit** — see RP-5 |
| B-9 | `runtime.madvise` via `mheap.allocSpan → sysUsed` | **36.75%** CPU (typed expand), 15.42% (untyped), 11.90% (DST), 9.04% (scan) | CPU profile + `pprof -peek` | allocation-heavy shapes | high — **Darwin-specific, see §4** |
| B-10 | Graph representation — GC mark cost of a merely *resident* graph | **~8 heap objects per node**; 29.53 ms per forced GC at 800k nodes; ~3.93 ns per heap object per cycle | `runtime.GC()` timing + `MemStats` | resident graph, no mutation | high |
| B-11 | `cypher` — aggregation forces the **row** path where the shipping form uses the **columnar** path | **1.72–1.90×** more CPU per node while shipping 35,000× fewer rows | in-process CPU model | `WHERE age>x RETURN count(n)` vs `RETURN n.name` | high |

**Not a bottleneck (measured, reported for completeness):**

- `search/` — `BFS_Chain10M` 47.43 ms at **0 allocs/op**; `Dijkstra_Large` 8.29 ms at 4 allocs/op;
  `Dijkstra_RandomGraph` 339.79 ms at 5 allocs/op. The only GoGraph symbol above the profile
  cutoff was `adjlist.upsertEdgeSlotLocked` at 0.95%, which is fixture *build*. The algorithms
  are not where time goes.
- `store/wal` — `Encode` 500.1 ns/op at **8.22 GB/s**; `Reader_Replay` 2.98 GB/s; `Append`
  encode-only 607.7 ns at 4 KiB (6.76 GB/s). `fsync` is ~3.7–3.9 ms regardless of payload size,
  i.e. device latency, not code. Group commit is excellent: `AppendSync_Batch` batch=1 →
  3.889 ms, batch=1000 → 4.183 ms — **1 000 records for 7.6% more than one**.

---

## 3. State of the art at HEAD — fresh baseline

All measured this session. **The peer audit's fixed/marginal CPU model could not be reproduced
on this host at all** (see §4): it reads cgroup v2 `cpu.stat`, which does not exist on Darwin.
The model below is the in-process equivalent, measured with `getrusage(RUSAGE_SELF)`.

**CPU instrument validated before use** — 1/2/4 busy threads for 400 ms read 0.402 s / 0.800 s /
1.566 s of CPU (ratios 1.01 / 1.00 / 0.98): it tracks parallelism and does not clamp at one core.

### 3.1 Fixed and marginal cost (GOMAXPROCS=1, in-process, no protocol)

| quantity | value | r² |
|---|---|---|
| Fixed cost of accepting a query (`RETURN 1 AS x`) | **1.37 µs/query** | — |
| `UNWIND range(1,k) RETURN i` — produce **and** ship | fixed ≈ 0, marginal **58.7 ns/row** | 0.9998 |
| `UNWIND range(1,k) RETURN count(i)` — produce only | fixed 3.28 µs, marginal **40.1 ns/row** | 1.0000 |
| **Ship cost per row** (difference) | **18.6 ns/row** | — |

Reproduce: `go test -run '^TestCPUModel_FixedAndMarginal$' -v -timeout 30m ./bench/audit352/`

### 3.2 Scan-path primitives (GOMAXPROCS=1, n=50 000, stable across n=5 000)

| arm | plan | ns per scanned node |
|---|---|---|
| `RETURN count(n)` | `ColumnarProject → NodeByLabelScan` | **29.09** |
| `RETURN count(n.age)` | `ColumnarProject → NodeByLabelScan` | **75.05** |
| `WHERE n.age>x RETURN count(n)` | **row** `Filter → Project` | **194.48** |
| `WHERE n.age>x RETURN n.name` (ships 24 999) | `ColumnarFilter → ColumnarProject` | **112.91** |

**Cost of reading one property per scanned node: 45.95 ns/node.**

Reproduce: `go test -run '^TestCPUModel_ScanShapes$' -v -timeout 20m ./bench/audit352/`

### 3.3 Per-row cost — the module's known weak axis, re-derived

Identical plan across all arms (`ColumnarProject → ColumnarFilter → NodeByLabelScan`), asserted by
`TestSweepPreconditions`; rows *produced* fixed at 120 000, rows *shipped* swept 0 → 120 000.

| projected type | fixed (produce) | marginal (ship) | r² (time) | allocs/row | r² (allocs) | bytes/row |
|---|---|---|---|---|---|---|
| small int (18–82) | 8.740 ms → **72.83 ns/node** | **84.94 ns/row** | 0.97923 | **0.0001** | 0.86977 | 44.87 |
| large int (≥100 000) | 8.191 ms → **68.26 ns/node** | **99.85 ns/row** | 0.98307 | **1.0001** | **1.00000** | 52.87 |
| string | 8.674 ms → **72.28 ns/node** | **103.02 ns/row** | 0.99129 | **1.0001** | **1.00000** | 104.10 |

**Interface boxing of an integer costs 14.91 ns/row = 14.9% of the per-row ship cost**, and
exactly 1.0000 allocations per row (r² = 1.00000).

Reproduce: `go test -run='^$' -bench='^BenchmarkShipSweep_' -benchmem -count=8 ./bench/audit352/`

### 3.4 Headline query shapes (120 000 `:Person`, ~960 000 `:KNOWS`, default engine, n=8)

| shape | ns/op | ±% | B/op | allocs/op | allocs per row |
|---|---|---|---|---|---|
| `RETURN count(*)` (`LabelCountScan`, no scan) | 1,428 | 1.4 | 2,168 | 29 | — |
| `RETURN count(p.salary)` (produce 120k, ship 1) | 10,005,868 | 2.2 | 2.0 MB | 239,817 | 2.00 |
| `RETURN p.age` (small int) | 16,453,593 | 3.1 | 39.8 MB | 244,693 | 2.04 |
| `RETURN p.salary` (large int) | 16,416,819 | 2.3 | 41.7 MB | 484,692 | 4.04 |
| `RETURN p.firstName` | 16,641,402 | 2.4 | 43.6 MB | 484,691 | 4.04 |
| `RETURN p` (whole node) | 27,636,826 | 2.2 | 90.5 MB | 1,083,731 | 9.03 |
| `RETURN p.firstName, p.age, p.salary, p.bucket` | 31,431,018 | 2.2 | 93.0 MB | 1,086,973 | 9.06 |
| 1-hop typed expand, `count(*)` (960k edges) | 365,621,944 | 4.1 | 26.9 MB | 3,115,477 | 3.25/edge |
| 1-hop typed expand, ship 1 col | 366,276,806 | 8.3 | 52.9 MB | 960,136 | 1.00 |
| 1-hop typed expand, ship 2 cols | 421,659,084 | 3.7 | 149.9 MB | 1,920,169 | 2.00 |
| 1-hop **untyped** expand, `count(*)` | 228,740,312 | 2.1 | 26.9 MB | 3,115,466 | 3.25/edge |

Note the aggregating twin of the expand (365.6 ms, ships **1** row) costs the **same** as the
columnar form that ships **960 000** rows (366.3 ms) — B-11.

Reproduce: `go test -run='^$' -bench='^(BenchmarkProjectionShape|BenchmarkPagination|BenchmarkExpandShape)$' -benchmem -count=8 ./bench/audit352/`

### 3.5 GC tax of a resident, unchanging graph

Degree 4. Baseline row is the process already holding the 120k-node harness fixture, so the
per-graph figures are *additional* cost.

| nodes | forced GC (median of 5) | heap objects | **objects added per node** | heapAlloc |
|---|---|---|---|---|
| (baseline: fixture only) | 4.447 ms | 964,673 | — | — |
| 50 000 | 5.927 ms | 1,367,611 | 8.06 | 121.3 MB |
| 200 000 | 10.314 ms | 2,568,457 | 8.02 | 197.5 MB |
| 800 000 | 29.530 ms | 7,373,249 | 8.01 | 504.2 MB |

**~8 heap objects per node, and ~3.93 ns of mark time per heap object per GC cycle.** Every
cycle re-traces all of them for as long as the process holds the graph.

Reproduce: `go test -run '^TestGCTax_ResidentGraph$' -v -timeout 30m ./bench/audit352/`

### 3.6 DST — current state of the art

`./simbin 20260827 -swarm -duration=4m -runs=0 -bias -coverage-report -workers=3`

**2 179 runs, 2 177 passes, 2 failures, 9 runs/s, 59/59 coverage buckets exercised, 0 unexplored.**

Both failures are `disk-full`, with reproducer seeds `4916676214232890342` and
`11360545023114334910`. **Neither reproduces in isolation: 0 failures in 6 attempts (3 per
seed).** They are therefore *not* established as engine defects; the likeliest explanation, given
this project's history with `cpu-starvation` clamping process-global `GOMAXPROCS`, is
cross-scenario interference between swarm workers. This warrants a separate correctness
investigation and is out of scope for a performance audit.

**The DST is a correctness vehicle, not a performance vehicle.** At the default `-check-every=1`
the invariant checker issues ~16 additional Cypher probes per simulated operation and sorts the
entire model (`oracle.NodeNames()`, `edgeStates()`) — superlinear in graph size, with probe
queries structurally identical to the workload's own reads. Its durable path is `SimDisk`, an
in-memory simulated filesystem, so any storage timing from it is fictional. The composed profile
in §3.7 is reported with that caveat attached.

### 3.7 Composed-system profile (DST, `BenchmarkSimulatorRun`, 20 000 ticks)

11.90 s/op, 6.09 GB/op, 172,571,994 allocs/op — **checker-inflated, see the caveat above.** The
value here is the *shape*, which corroborates the isolated profiles: scheduler/syscall park-unpark
43.2% (`pthread_cond_signal` 16.99%, `kevent` 14.10%, `pthread_cond_wait` 6.97%), `madvise`
11.90%, **name-keyed map traffic ~15.6%**, `expr.EvalWith` 14.38% cum, `populateRowCtx` 5.86% cum.

### 3.8 Concurrency

`BenchmarkConcurrentRead`, `b.SetParallelism(g)` — note this **multiplies** GOMAXPROCS, so the
arms are 10, 80 and 640 goroutines, not 1/8/64:

| goroutines | ns/op | ±% | allocs/op |
|---|---|---|---|
| 10 | 2,502,444 | 3.9 | 2,466 |
| 80 | 2,480,761 | 2.0 | 2,466 |
| 640 | 2,518,858 | 1.1 | 2,468 |

**From 10 to 640 concurrent readers: latency flat within noise, and allocations per operation
constant** (2,466 / 2,466 / 2,468). That constancy is also the fixture-bug check passing — a
per-op metric that moved with the goroutine count would have indicated a harness defect rather
than a finding. No latency cliff, no per-operation resource growth.

---

## 4. Host state, noise floor, and measurement limits

**Host.** Apple M4, macOS 26.5.2 (25F84), Go 1.27.0 darwin/arm64, 32 GiB RAM.

**⚠ Core topology — this reframes every scaling number.** `hw.ncpu = 10`, but
`hw.perflevel0` = **4 Performance** cores and `hw.perflevel1` = **6 Efficiency** cores. A
"10-core" speedup ceiling near 3× is what this hardware gives for CPU-bound work, and is **not by
itself evidence of a software defect**. Every scaling claim in this report is stated as a
*within-machine A/B* (same hardware, both arms) for exactly that reason.

**Load average**, recorded at each measurement (start → end):

| measurement | loadavg |
|---|---|
| session open | 1.54 |
| result-cap A/B | 1.35 → 7.98 (self-inflicted: the benchmark uses ~6–8 cores) |
| GOMAXPROCS sweep | 2.54 |
| shape sweep | 2.35 |
| GC tax | 2.35 |
| DST swarm | 2.35 → recorded in `dst.host` |

Total non-benchmark process CPU on the host measured at 117% of 1000% available (~1.2 of 10
cores) before the timed runs. This is a workstation, not a dedicated bare-metal server; that is
recorded rather than claimed away.

**Noise floor: 0.51%** — measured as `ctrl_a` vs `ctrl_b`, two **byte-identical**
default-configured arms interleaved in the same process by `-count=10`
(16.58m ± 3% vs 16.49m ± 2%, −0.51%, p=0.043). Per GOMAXPROCS level the floor was `~`
(not significant) at 1/2/4/8 and −0.48% at 10.

**No delta below ~1% is reported in this document as a finding.** Note that the byte-identical
control still produced p=0.043 — which is precisely why the noise *floor* and not the p-value is
the acceptance criterion here.

### What could not be measured

- **The peer fixed/marginal CPU model cannot run on this host.** `bench/comparison/cpu_test.go`
  reads `usage_usec` from a container's cgroup v2 `cpu.stat`; Darwin has no cgroup hierarchy, so
  `HaveCPU` is false and the entire `a + b·K` fit section is empty. The §3.1 model is an
  in-process `getrusage` equivalent — same structure, different instrument, **absolute values not
  comparable** with container-measured peer figures (no Bolt, no protocol, no container).
- **No peer comparison was run.** Neo4j and Memgraph arms need Docker plus a Linux container with
  a bind-mounted cgroup. So this report contains **no** GoGraph-vs-peer number, and none should
  be inferred from it.
- **`runtime.madvise` is a Darwin-specific cost.** `pprof -peek` attributes **100%** of it to
  `mheap.allocSpan → sysUsed → sysUsedOS`, i.e. the *allocation* side (`MADV_FREE_REUSE` on span
  commit), not the scavenger. On Linux `sysUsed` is effectively a no-op, so this cost would
  largely vanish there. Every CPU *share* in this report is inflated by it on this host; the
  portable finding is the underlying allocation rate, not the madvise percentage.
- **The realised time win of SCB-5 was not measured**, only its allocation share. Implementing it
  is diagnosis-out-of-scope; the A/B is the next step.
- **`bolt/` was not profiled in isolation.** It was exercised only indirectly through the DST's
  wire scenarios.
- **`store/checkpoint`, `store/recovery`, `store/snapshot` and bulk import were not profiled.**
  Only `store/wal` was measured.
- **`ds/` and `metrics/` were not measured at all.**
- Only one machine, one OS, one Go version. No result here has been shown to survive a different
  platform.

---

## 5. Refuted premises

Each of these was supplied as a hypothesis to verify. All were tested at HEAD; these did not hold.

**RP-1 — "`bagDecodeAt` ~28.18% cumulative; the byte-stream property bag is UNSORTED ⇒ linear
decode." REFUTED as a bottleneck.**
`bagDecodeAt` does not appear in the top 30 of any CPU profile taken. On the scan shape,
`lpg.(*propBag).get` is 4.80% cumulative and `bagKeyAt` 1.59% flat. The premise is stale in its
mechanism too: `propBag.get` (`graph/lpg/propbag.go:336-355`) walks with `bagKeyAt`, which decodes
only the *key*; `bagDecodeAt` is called exactly **once**, on the hit. And the scan is bounded —
`smallBagMax = 8` (`propbag.go:94`), after which the bag promotes one-way to a map and lookup is
O(1). The module's own benchmarks carry 1–3 properties per node. A bounded ≤8-record key-only
scan is not obviously the wrong structure. The bag is indeed unsorted (insertion order with
recency promotion) — that half of the premise is correct and irrelevant.

**RP-2 — "#2249 `Expand(Into)` is not a real CSR seek." REFUTED.**
`Expand.seekIntoRuns` (`cypher/exec/expand.go:595-614`) narrows the *cursor* via `dstRun`
(`cypher/exec/csrprobe.go:68-75`) → `lowerBoundDst`, a genuine binary search, O(log d + r). It is
on by default (`intoSeek` defaults true, `expand.go:305`), applies to both row and columnar modes
and both directions, and its precondition holds (`csr.OrderRuns` orders runs by
`(destination, handle)`, `graph/csr/order.go:75-92`). There is no separate `ExpandInto` operator —
it is a display name. `lookupFwdEdgePos`, `reverseEdgePassesFilter` and `ExpandIntersect` are all
binary-search-backed too.

**RP-3 — "#2391 `copySchema` is the largest allocation site, 59.56% of example 35 allocation."
REFUTED.**
`copySchema` (`cypher/api.go:7592`) is a **plan-build-time** helper. All 36 call sites are inside
`build*` functions — per plan node, per projection item, or per query; none is per-row. It does
not appear in any allocation profile taken. The genuine per-row allocator on that path is
`newSchemaWalk` (SCB-5 / B-5), which is very likely what the premise misattributed.

**RP-4 — "#2616 subquery projection 115× an equivalent `OPTIONAL MATCH`." NOT EVALUABLE AS
STATED — and the real number is far worse for a different construct.**
The premise does not name the subquery form. Two of them **do not exist at HEAD**: `CALL { }` is
not in the grammar (`cypher/subquery_block_form_test.go:269-270` records it as such), and
`COLLECT { }` is unimplemented — the repo's own `TestCollectSubquery_CollectsInnerMatches`
**skips** on the identical parse error I hit. Of the forms that do exist, `EXISTS { }` (0.970 ms)
and the pattern predicate (1.059 ms) are *at parity* with `OPTIONAL MATCH` (1.019 ms) at n=2000 —
no 115× anywhere. But `COUNT { }` is **1,203×** at the same size and quadratic (SCB-2 / B-1). The
premise's magnitude understates the real defect by an order of magnitude and attaches it to the
wrong construct.

**RP-5 — My own hypothesis: "the plan-cache global mutex should throttle short queries more than
long ones." REFUTED by the experiment I designed to test it.**
The mutex profile is unambiguous about *location*: **99.94%** of all mutex contention
(19.50 s of 19.51 s) is `sync.(*Mutex).Unlock` under `cypher.(*planCache).get` ←
`parseAndAnalyse` ← `runRead`, with `sync.(*Mutex).Lock` a further 21.30% of the block profile at
the same site. The plan cache is a single-`sync.Mutex` LRU (`cypher/plan_cache.go:55-60`) whose
own comment argues the single mutex is acceptable "because plan-cache lookups are not on the
row-level hot path … gating the per-query work that dominates the total runtime by orders of
magnitude".

That comment predicts short queries should suffer. They do not, relative to long ones — speedup
versus GOMAXPROCS=1, one goroutine per P:

| shape | serial ns/op | 2P | 4P | 8P | 10P |
|---|---|---|---|---|---|
| `RETURN 1` (1.4 µs body) | 1,383 | 1.78× | **2.89×** | 2.25× | 2.22× |
| `UNWIND range(1,10)` (2.7 µs body) | 2,726 | 1.89× | **3.24×** | 3.10× | 3.18× |
| scan+filter (8.5 ms body) | 8,517,937 | 1.82× | **3.21×** | 3.02× | **3.37×** |

All three plateau at ~3.2×, and the *short* shapes are not worse than the long one. Given the
4P+6E topology (§4), a ~3.2× ceiling is what this hardware gives. **The plan-cache mutex is where
essentially all contention lives, but it was not shown to limit throughput at any query length
tested**, and the uniform plateau is not distinguishable from hardware asymmetry with these
measurements. B-8 is therefore reported as a contention *location* and a design risk, **not** as a
demonstrated throughput bottleneck. Establishing it either way needs a machine with homogeneous
cores.

**RP-6 — "~13.9% cumulative in interface boxing (`convTstring`, `mallocgc`)." PARTIALLY
CONFIRMED, with an important negative.**
On the columnar sweep the boxing cost is real and cleanly isolated: **14.91 ns/row, 14.9% of the
per-row ship cost**, at exactly 1.0000 allocations/row (r² = 1.00000). That magnitude matches the
premise. **But on the `ParallelScanProject` path it is invisible in time.** `RETURN p.age`
(small int, allocation-free boxing via the runtime's `staticuint64s`) and `RETURN p.salary` (large
int) differ by 239,999 allocations/op and 1.9 MB/op, yet measure **16,453,593 vs 16,416,819 ns/op**
— the *cheaper-allocating* arm is marginally slower, well within the 2.3–3.1% spread. Removing a
quarter-million allocations per operation bought nothing measurable there. Allocation count is
not a proxy for time on this path, which is why SCB-5 is ranked below the wall-clock findings.

**RP-7 — `docs/profiling.md`'s GC-tuning recommendation. REFUTED on this workload.**
The doc recommends `GOMEMLIMIT` and `GOGC` to collapse `madvise` traffic. Measured, five
process-interleaved repetitions per arm on the typed expand:
`GOGC=100` 385.5 ms → `GOGC=400` 391.3 ms (**p=0.548, no change**);
`GOGC=100` 385.5 ms → `GOMEMLIMIT=8GiB` 385.1 ms (**p=1.000, no change**).
The reason is in the profile: `pprof -peek` attributes **100%** of `madvise` to
`mheap.allocSpan → sysUsed → sysUsedOS`, the *allocation* side, not the scavenger. GC tuning
cannot remove span-commit cost. Only allocating less can — which is what the doc calls "the
primary cure", so the direction is right and only the lever is wrong.

**RP-8 — "#2394 `exec.ChunkPool` is dead code; aggregation scratch chunk allocated per query."
CONFIRMED as fact, REFUTED as an opportunity.**
Zero production call sites (SCB-7). But the aggregation scratch chunk is allocated **once per
query** and reused across all batches via `Reset()`, so the wasted work is one allocation per
query — not a bottleneck by any measurement taken. Ranked last deliberately.

**RP-9 — "#2247/#2251 the `edgeTypeFilter` map is rebuilt per candidate edge." PARTIALLY.**
It is *consulted* per candidate edge (a `map[uint64]string` probe, `cypher/exec/expand.go:907`),
at a measured 142.6 ns per edge — that half holds, and is SCB-6/B-7. It is **not rebuilt** per
edge, nor even per query: it is built once per (type-set, CSR-pair-state) and LRU-cached
(`cypher/edge_type_filter_cache.go`, capacity 256), validated by an exact
`csrPairKey{epoch,startTS,versioned}` match. It *is* re-resolved once per outer row under Apply —
one mutex acquire plus an LRU `MoveToFront` per outer row — which the premise does not mention.

**RP-10 — "#2210 / #2411 positional slot binding: binding is by name." PARTIALLY — the phrasing
overstates it.**
Operators already bind **positionally**: `exec.Row` is `[]expr.Value` (`cypher/exec/row.go:44`)
and `NodeByLabelScan`, `Project`, `Expand` and `ColumnarProject` all write by integer column
index. What reintroduces strings is the `Row → RowContext` bridge: `populateRowCtx`
(`cypher/api.go:13385ff`) does one string-keyed map store per bound variable per row, and the
evaluator reads it back by string at `cypher/expr/eval.go:551`. `RowContext` is still
`map[string]Value` (`cypher/expr/eval.go:30`). A pool exists (`cypher/api.go:13284`) but is gated
to `scalarUse != nil` and schemas ≤16 wide. So the *conclusion* holds (B-6) while the mechanism is
narrower than stated.

**Also verified as genuinely fixed (no contradiction found):** #2410 Bolt per-row `write(2)` —
`bolt/chunking.go:315` is `if cw.autoFlush`, and no per-row write appeared in any profile.

---

## 6. Reproduction

**Instrument:** `bench/audit352/` — a new, self-contained exercise harness added by this audit.
It is `gofmt`-clean, `go vet`-clean, and nothing in the module imports it. No module source file
was modified.

Its guards, each answering a trap this project has previously been caught by:

- `TestSweepPreconditions` — fails unless every sweep arm compiles to the **identical** physical
  plan and ships exactly the row count its selectivity implies.
- `runQuery` brackets every timed loop with `Explain` **before and after** and fails on plan
  drift. This is not theoretical: a shape in this package was observed planning as
  `columnarExpand` on a fresh engine and as a row-based `Expand` on a warmed one.
- `TestResultCapAB_Preconditions` — proves the A/B is single-variable.
- `TestCPUInstrumentSanity` — validates the CPU counter against known busy work before any number
  derived from it is believed.
- Fixtures carry both a small integer property (inside the runtime's `staticuint64s` window, where
  boxing is allocation-free) and a large one, so the boxing trap is measured rather than fallen
  into.
- `olsFit` panics on fewer than three points; `cpuMicros` panics rather than returning a sentinel
  drawn from the answer's own value space.

```sh
cd /Users/flaviocfo/dev/xumiga/GoGraph

# Noise floor + the result-budget A/B (SCB-3). Arms interleave under -count.
go test -run='^$' -bench='^BenchmarkResultCapAB$'       -benchmem -count=10 ./bench/audit352/
go test -run='^$' -bench='^BenchmarkResultCapAB_Procs$' -benchmem -count=6  ./bench/audit352/

# Pagination (SCB-1), projection shapes, expand shapes
go test -run='^$' -count=8 -benchmem \
  -bench='^(BenchmarkProjectionShape|BenchmarkPagination|BenchmarkExpandShape)$' ./bench/audit352/

# Per-row ship cost (§3.3)
go test -run='^$' -bench='^BenchmarkShipSweep_' -benchmem -count=8 ./bench/audit352/

# Complexity fit (SCB-2) and relationship-property materialisation (SCB-4)
go test -run '^(TestScaling_SubqueryComplexity|TestRelPropertyMaterialisationCount)$' \
  -v -timeout 40m ./bench/audit352/

# Fresh fixed/marginal CPU model (§3.1, §3.2)
go test -run '^(TestCPUInstrumentSanity|TestCPUModel_FixedAndMarginal|TestCPUModel_ScanShapes)$' \
  -v -timeout 40m ./bench/audit352/

# GC tax of a resident graph (§3.5)
go test -run '^TestGCTax_ResidentGraph$' -v -timeout 30m ./bench/audit352/

# Contention profiles (B-8 / RP-5)
go test -run='^$' -bench='^BenchmarkConcurrentRead$' -benchmem -count=5 \
  -blockprofile=block.pb.gz -mutexprofile=mutex.pb.gz ./bench/audit352/
go test -run='^$' -bench='^BenchmarkPlanCacheScaling$' -count=5 ./bench/audit352/

# Plans and per-operator PROFILE output
go test -run='^(TestLogPlans|TestProfileShapes|TestPaginationPlans|TestExpandShapePlans)$' \
  -v ./bench/audit352/

# search/ and store/wal (§2 "not a bottleneck")
go test -run='^$' -bench='^(BenchmarkDijkstra_Large|BenchmarkBFS_Chain10M|BenchmarkDijkstra_RandomGraph)$' \
  -benchtime=3s -benchmem -cpuprofile=search.cpu.pb.gz ./search/
go test -run='^$' -bench='^(BenchmarkAppend|BenchmarkWriter_AppendSync_Batch|BenchmarkEncode|BenchmarkReader_Replay)$' \
  -benchtime=2s -benchmem ./store/wal/

# DST (§3.6) — build the binary; `go run` swallows a panic's exit 2.
go build -o simbin ./cmd/sim
./simbin 20260827 -swarm -duration=4m -runs=0 -bias -coverage-report -workers=3   # -runs=0 is MANDATORY

# Composed-system profile (§3.7) — checker-inflated, read §3.6 first
go test -run='^$' -bench='^BenchmarkSimulatorRun$' -benchtime=40s -benchmem \
  -cpuprofile=sim.cpu.pb.gz ./internal/sim/
```

**Raw artefacts** (this session's scratchpad — session-local, not in the repo):
`/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/0355df13-ff89-4d09-8a74-917eb9f24306/scratchpad/`
— `prof/` (CPU, alloc, block, mutex profiles + a `.host` loadavg file per capture),
`bench/` (raw benchmark logs and per-arm benchstat inputs), `logs/` (plans, scaling, DST).
Every run writes its own exit status into its log with `echo "EXIT=$?" >> log`; none was read from
a pipeline's exit code.

---

## 7. Suggested next step

SCB-1 and SCB-2 are the two that change orders of magnitude, are confined to the planner, and have
existing in-tree templates (`Top` and `EXISTS { }` respectively). SCB-3 is the largest measured
win on an *ordinary* query and the only one of the top four that touches concurrency correctness,
so it wants its own task with the byte-budget tests as the gate.

Before implementing SCB-5, run the A/B — RP-6 is the standing reminder that an allocation share
on this codebase does not reliably convert into wall clock.
