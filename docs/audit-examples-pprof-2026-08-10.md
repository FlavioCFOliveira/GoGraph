# Improvement opportunities found by exercising every example under `pprof` — 2026-08-10

**Entry head:** `cbc45aa2` · sprint 337 · Apple M4 (10 cores, 32 GiB), `darwin/arm64`, go1.26.5

Every one of the 37 programs under `examples/` was driven under `-profile-dir`, and its CPU and heap
profiles read. This is the first sweep able to do that: the capability was only added to 35 of them
in the previous cycle (rmp #2377), and the reference-scale example had still never been profiled at
all.

Findings are ranked by the project's decision framework — **correct → secure → fast → efficient** —
not by size.

---

## Coverage

| | |
|---|---|
| Examples profiled | **37 / 37** |
| Runs producing a valid `cpu.pprof` + `heap.pprof` | **42** — example 24 contributes six, one per subcommand; every other example one |
| Examples with no valid profile | **0** |
| Examples needing non-default arguments | 24 (six subcommands), 25 (server lifetime), 26 (scale) |

Four further runs produced no profile and are each accounted for: examples 24 and 25 invoked with no
arguments exit 2 and print their usage, which is their documented behaviour; and example 26's two
over-budget attempts, below.

Example **24** was driven through `init → seed → stats → query → plandiff → snapshot`, each in its
own process. Example **25** was profiled across its whole server lifetime: startup recovery, a
synthetic seed, about 1 200 HTTP requests, and graceful shutdown on `SIGTERM`.

### Example 26 does not complete at its own default, and that is the headline

`26_social_scale_bench` is the project's reference end-state example. Its default is 1 000 000 users
and about 175 M edges.

| scale | outcome |
|---|---|
| default (1 M users) | killed at **12 min**; never printed past its config block |
| 150 000 users | killed at **7 min**, mid-query-battery — the build alone took **4 m 11 s** |
| 20 000 users | completed in **67 s**, first valid profile pair |

The 150 000-user run got far enough to print its own telemetry before being killed, and that
telemetry is the single most significant piece of evidence this sweep produced:

```
edges.friend=26249942   edges.like=22525717
# build.elapsed=4m11.083s   # build.edge_rate=194261 edges/s
# mem.heap_alloc=1.37 GiB   # mem.total_alloc=331.48 GiB   # mem.sys=47.31 GiB
# bytes_per_edge=30.2
# q.count_friend.latency=43.804822s
# q.count_like.latency=36.181658s
# q.friend_since_filled.latency=51.634874s
```

**331.48 GiB of total allocation to build a graph whose live heap is 1.37 GiB** — a 242× allocation
amplification — and the process asked the OS for **47.31 GiB**. A `count` over 26.2 M relationships
took **43.8 s**.

None of this is one defect, and none of it was visible to any gate, benchmark or test in the module,
because the workload that exhibits it has never run to completion under an instrument. Treat it as a
sprint, not a fix.

---

## 1. FIXED — dynamic chunks were never pre-sized (rmp #2381)

> **SUPERSEDED, in part, by §6 (rmp #2389).** Everything measured below stands and reproduces. What
> it does not say is that the *same* change made a single-row result reserve the whole 4096-row
> capacity, which cost **2.28× throughput** on `examples/35_mvcc_mixed_workload`. The reservation was
> measured on one workload shape and shipped as if it were general. §6 keeps this win and removes
> that cost; read the two together.

*Class: documentation accuracy (correct) + efficiency.*

`NewDynamicChunk`'s godoc opened with "each **pre-sized to capacity rows**" while its own next
paragraph stated "a dynamic column has **no backing at construction**". The two sentences
contradicted each other and the first was false: the constructor allocated nothing, and the `Put`
that committed a column to a type merely flipped the storage tag and appended into a nil slice. Every
fresh dynamic chunk therefore walked `append`'s entire growth series on its first fill.

Measured on one int64 column filled to the default 4096-row capacity, 3 rounds:

| arm | ns/op | B/op | allocs/op |
|---|---|---|---|
| dynamic, before | 16 990 | 128 248 | 16 |
| dynamic, **after** | **11 056** | **32 768** | **1** |
| static `NewChunk` (untouched control) | 12 810 → 12 762 | 32 960 → 32 960 | 3 → 3 |
| dynamic, `Reset` + refill (warm) | 9 885 | 0 | 0 |

The static control is byte-identical before and after, which is what rules out a machine-wide drift
reading. The warm arm stays at zero allocations, confirming the fix is free for pooled chunks.

**The fix** adds one `Chunk.commitDynamic` helper that sets the storage tag *and* allocates the
backing at the chunk's capacity when it has none, and routes all six commit paths through it
(`PutInt64`, `PutFloat64`, `PutString`, `PutBool`, `PutNull`, `PutValue`). `promoteToBoxed` had the
same defect one branch over — it built a slice of length `col.n` mid-fill — and is now sized to the
chunk capacity too. A `cap == 0` guard preserves `Reset`'s backing retention exactly.

### Measured where the finding was made — `examples/23_bolt_server`, interleaved, 3 rounds

The profile that surfaced it: `Chunk.pushI64` was **94.17 MB of the run's 213.42 MB (44.12 %)**,
reached 100 % through `tryBuildColumnarAggInput.evalPutColumnFiller → PutValue → PutInt64`, with
**72.70 %** of the run flowing through `EagerAggregation.consumeChunk`.

At `-nodes 20000 -queries 20000 -sessions 8`, arms alternating every round and never overlapping:

| metric | before | after | change |
|---|---|---|---|
| total allocation | 6.56 – 6.59 GB | **4.63 – 4.82 GB** | **−27.7 %** |
| throughput | 6 813 – 6 843 q/s | **7 401 – 7 474 q/s** | **+8.8 %** |
| latency p50 | 898 – 938 µs | 826 – 871 µs | −8.2 % |
| latency p99 | 3.127 – 3.204 ms | 2.960 – 2.993 ms | −6.1 % |

Every deterministic fact the example pins was **identical across all six runs**.

Pinned by `TestCommitDynamicFirstFillDoesNotAllocate` (12 allocations against 3, pre-fix),
`TestCommitDynamicReuseAllocatesNothing` (the reuse guard) and `TestPromoteToBoxedPreSizes`, all
verified to fail against the pre-fix build in a scratch worktree at `cbc45aa2`. The fourth test this
section originally listed, `TestCommitDynamicPreSizesBacking`, asserted that the committing `Put`
reserves the *whole* capacity — the assertion §6 had to overturn, since it pinned the regression
rather than the fix. It is replaced there by `TestCommitDynamicReservesTheFloorNotTheCapacity`.

---

## 2. OPEN — per-row property materialisation dominates the reference example

*Class: efficiency and speed. Largest measured opportunity. Needs a decision on scope.*

`examples/26_social_scale_bench` at 20 000 users allocates **33.48 GB** in 67 s. Half of it is the
per-row row-context build:

| site | flat | cum |
|---|---|---|
| `cypher.populateRowCtx` | 6 848.66 MB (20.46 %) | **16 595.58 MB (49.57 %)** |
| `cypher.buildRowCtxWithUse` | 973.04 MB | 9 872.21 MB (29.49 %) |
| `cypher.buildEdgeProps` | 2 140.52 MB | 6 226.84 MB (18.60 %) |
| `cypher.edgePropsToExprMap.func1` | 3 367.81 MB (10.06 %) | 3 608.82 MB |
| `cypher.nodePropsToExprMap.func1` | 2 283.55 MB (6.82 %) | 2 577.55 MB |
| `graph/lpg.(*edgePropCols).GrowSlotWithValue` | 2 368.32 MB (7.07 %) | 2 597.92 MB |

Its CPU profile — the first ever taken of this workload — agrees. String-keyed map work is the
largest identifiable cluster, and `madvise` is the GC handing the churn back to the OS:

| symbol | flat | cum |
|---|---|---|
| `runtime.mapaccess2_faststr` | 2.85 % | **12.90 %** |
| `internal/runtime/maps.getWithoutKeySmallFastStr` | 5.42 % | 8.75 % |
| `runtime.madvise` | 6.06 % | 6.06 % |
| `internal/runtime/maps.(*Iter).Next` | 3.49 % | 4.05 % |
| `aeshashbody` | 3.47 % | 3.47 % |
| `graph.fnv1aString` | 2.25 % | 2.25 % |

### The spike answered it, and the answer is not what the profile suggested (rmp #2384)

The question was "why is the lazy materialisation gate not engaging?". **It is engaging.** Counters
placed at every decision point, one 67 s run of example 26 at 20 000 users:

| counter | value |
|---|---|
| relationship row-context materialisations | **17 009 744** |
| … gated presence path taken (`r.k IS NOT NULL`) | 6 511 214 (**38.3 %**) |
| … value path taken (`relUse.keys` non-empty) | 10 498 530 (61.7 %) |
| … fell through for lack of demand info (`scalarUse` nil, or variable unreferenced) | **0** |
| … `needsWholeNode` | **0** |
| map entries returned, whole-map path | 10 498 530 over 10 498 530 calls = **exactly 1.0 per call** |
| property keys demanded, same calls | 10 498 530 = **exactly 1.0 per call** |

**Two hypotheses formed from reading the code were refuted by these counters.** The first: the edge
branch in `populateRowCtx` `continue`s *before* the demand gate that skips variables an expression
never names, so an unreferenced edge variable would build its whole map. Real asymmetry, but
**unreachable** — that case occurred 0 times. The second: `buildEdgeProps` materialises the by-handle
map before its gate and could discard it. Also 0.

**There is no over-fetching.** Entries returned equals keys demanded, exactly 1.0 both ways, because
a `FRIEND` carries exactly one property (`since`). A "fetch only the demanded keys" change would
return precisely nothing here.

**The cost is the container, not the content.** `buildEdgeProps` is 1.99 GB flat / **6.03 GB cum =
18.35 %** of the run, and per-line attribution splits it cleanly:

| path | line | bytes | calls | per call |
|---|---|---|---|---|
| presence | `out = make(expr.MapValue, …)` + `out[k] = relPresencePlaceholder` | 313.51 MB + 1.69 GB = **2.00 GB** | 6 511 214 | **~330 B to answer a boolean** |
| value | `m := edgePropsToExprMap(g, stKey, enKey)` | **4.03 GB** | 10 498 530 | **~412 B to deliver one value** |

So the engine allocates a fresh one-entry Go map **per relationship per row**. The node path avoids
exactly this: `upgradeNodeIDToValuePartial` hands back a lazy value that answers `n.k` without
materialising a map, and it is used 3 102 MB against 17 MB for the eager whole-node path — the node
gate is working. Relationships have no equivalent, because `expr.RelationshipValue` carries its
properties as a `MapValue`.

**Secondary, CPU only:** `EdgePropertiesByHandle` was called on **all 17 009 744** materialisations
and its result was used **0** times (`served_via_by_handle = 0`) — this graph records no by-handle
edge properties. It allocates nothing (it returns nil on an empty result, and never appears in the
heap profile), so this is wasted work rather than wasted memory.

*Instrumentation caveat:* the instrumented build totalled 33.63 GB against 33.48 GB uninstrumented,
and `buildEdgeProps` flat 2 042 MB against 2 141 MB — within 0.5 %, so the attribution above is
representative rather than an artefact of the probe.

**This is where the spike stopped.** Removing the per-row map means changing how a relationship's
properties are represented in the row context, which is a design change; it went to the maintainer,
who approved all three follow-ups (rmp #2386, #2387, #2388), to be taken narrowest first.

### 2a. FIXED — the presence answer is interned, not allocated per row (rmp #2386)

The presence map holds one constant placeholder per **present** key, so for a fixed presence-key set
there are only **2^N distinct answers** — and the row path was allocating a fresh map to express one
of them, ~330 B to deliver a boolean.

All 2^N maps are now precomputed once, at the end of `analyseNodeScalarUse` (after its C1
reconciliation, so the table is indexed by the final key set), and stored read-only on the
`nodeScalarUse`. Being written once at build time and never afterwards is what makes them safe to
hand to the concurrent row workers a parallel scan creates, with no synchronisation. The row selects
by a mask built from the same per-row storage presence checks as before — only the container is
shared. `presenceMaps[0]` stays **nil**, preserving the absent-map (not empty-map) result that
box-at-sink depends on, and the table is bounded at `presenceInternMaxKeys = 4`, above which the
per-row build is used unchanged.

Measured in example 26 at 20 000 users, arms interleaved over 3 rounds and never overlapping:

| metric | before | after |
|---|---|---|
| total allocation | 32.50 – 32.63 GB | **30.50 – 30.76 GB** (**−6.0 %**) |
| `buildEdgeProps` flat | 5.95 – 6.42 % | **0 %** |

The 1.95 GB removed matches the 2.00 GB the spike predicted. Every deterministic fact was identical
across all six runs, and the example's own build-phase telemetry (`mem.total_alloc = 9.50 GiB`) is
byte-identical in both arms — an unchanged control confirming the build path was not touched.

The end-to-end semantics were already pinned by `rel_presence_isnull_test.go`, including the mixed
present/absent case the mask indexing has to get right, and that suite passes unchanged. The new
`presence_intern_internal_test.go` pins the table itself: every subset covered, the absent-map-is-nil
invariant, the stable sorted key ordering, the bound, and that a selection allocates nothing.

---

## 3. PARTLY FIXED — the physical plan is rebuilt on every execution

*Class: efficiency. The `analyseNodeScalarUse` part is fixed (rmp #2383, see §3.1); `copySchema` and
the physical plan itself remain open, the latter as an architecture decision.*

`examples/35_mvcc_mixed_workload` allocates 9 125.86 MB, and **`cypher.(*Engine).buildReadPhysical`
is 5 959.06 MB of it — 65.30 % of the run's entire allocation.**

The *logical* plan is already cached: `planCache` is a bounded LRU keyed by query text, and its entry
already memoises several pure functions of the immutable plan (`paramTypes`, `reorderCandidates`,
`pushedSeekHints`). The *physical* build is per execution by design, because it binds to the live
snapshot.

Two of its largest costs are, however, pure functions of inputs that do not change between executions
of the same cached plan, and are therefore the same class of thing the entry already memoises:

| candidate | cost in example 35 | why it is a candidate |
|---|---|---|
| `analyseNodeScalarUse` | **16.62 %** cum (1 516.73 MB) | a pure function of an immutable AST expression. Every write to a `nodeScalarUse` in the package happens inside the analysis itself — verified by enumerating all field writes — so the result is already treated as immutable by every consumer. |
| `copySchema` | **11.22 %** (1 024.17 MB) | a defensive shallow map copy, taken at 33 call sites per build. |

Memoising `analyseNodeScalarUse` on the plan-cache entry is precedent-backed and result-identical.
Caching the **physical plan** itself would be far larger and is an architecture change — it needs the
maintainer's decision before any work starts.

### Re-measured after §6, and it is now the largest remaining cost

The 65.30 % above was taken at `cbc45aa2`. At HEAD with §6 applied, on the same
`-nodes 3000 -readers 4 -phase-window 3s` run, `buildReadPhysical` is **66.86 % of 47.53 GB** —
`buildIRProjection` 25.58 %, `analyseNodeScalarUse` 18.63 %, `newRowPredicate` 14.07 %. The share is
unchanged because §6 removed a cost that sat *beside* this one, not inside it.

### What the two peer engines do (read at source, to settle the question rather than argue it)

Memgraph `343f7fe3` and Neo4j `eccd584a` (branch 2026.06):

- **Memgraph caches the executable operator tree**, because it has no separate physical plan:
  `LogicalOperator::MakeCursor(...) **const**` (`src/query/plan/operator.hpp:255`) is overridden by
  every operator, and the cache holds
  `LRUCache<HashedString, shared_ptr<PlanWrapper>>` (`cypher_query_interpreter.hpp:162`). The key is
  the **stripped** query text — literals are replaced by fixed tokens
  (`frontend/stripped.hpp:24-27`) and moved into `Parameters` — so `{id:1}` and `{id:2}` are **one**
  entry. Per execution it builds only a cursor and a frame, both from an arena
  (`interpreter.cpp:3642-3644`); the frame's width comes from the **cached** symbol table and slots
  are reached by array index, never by rebuilding a name→index map.
- Memgraph precomputes `required_indices` on the cached plan with a comment that is the exact
  analogue of the case above (`cypher_query_interpreter.hpp:94`): *"A pure function of the plan, so it
  is derived once at construction and reused for the readiness check every time this plan is served."*
- **Neo4j caches the executable plan too** — five layers, the fourth keyed on `LogicalPlan` identity
  and holding a built Pipe tree (`CypherQueryCaches.scala:685`; `InterpretedRuntime.scala:105-141`),
  carrying the warning *"Executable plan for a single cypher query. Warning, this class will get
  cached! Do not leak transaction objects or other resources in here."* Per execution it allocates
  only cursors, the parameter array and a fresh `QueryState`.

**So the boundary both peers draw is the same:** the operator tree, expression trees, slot layout,
output columns and required indices are immutable, cached and shared; the cursor, the row/frame
storage, the transaction view and the parameter **values** are per execution. Per-execution work is
proportional to the data plus a small O(operators) instantiation term — never to the analytical
complexity of the query.

**The root cause here is binding, not Go.** GoGraph's builders *capture* execution state in
build-time closures — `buildIRProjection(items, child, schema, g, params, reg, bopts)` takes both the
read view `g` and `params` — which binds the tree to one execution. Both peers *pass* that state as a
call argument instead (`Pull(Frame&, ExecutionContext&)`), and neither needs a lock, because sharing
is read-only.

**The honest counterweight:** `buildReadPhysical` is genuinely mixed. `e.g.ReadAt(snap)` and the live
cardinality gate `computeReorderSwaps` legitimately depend on the snapshot and must stay per
execution. So the question is not "cache the whole thing" but **separate the plan-pure part from the
execution-bound part** — which is exactly the boundary the peers draw, and exactly why it is an
architecture decision rather than a refactor.

### 3.1 FIXED — `analyseNodeScalarUse` is memoised per cached plan (rmp #2383)

A fifth memoised field on `planCacheEntry`, alongside the four that already carry the comment "pure
function of the plan". Measured on the same `-nodes 3000 -readers 4 -phase-window 3s` run, arms
interleaved over 3 rounds and never overlapping, against the committed `449e8ae4`:

| metric | before | after | change |
|---|---|---|---|
| total allocation | 44.56 / 44.75 / 44.32 GB | **40.55 / 40.33 / 40.23 GB** | **−9.4 %** |
| `phase.baseline.throughput_ops` | 729 757 / 743 050 / 736 218 | **830 183 / 833 752 / 831 964** | **+12.7 %** |
| `analyseNodeScalarUse` (cum) | 18.28 % (8.15 GB) | **absent from the profile** | — |
| `buildReadPhysical` (cum) | 67.15 % | **59.56 %** | −7.6 pp |

Every deterministic fact the example prints is identical in both arms.

**The memo is filled LAZILY, from the build's own call sites, and that is deliberate.** Eager
population at entry creation would have to walk the plan and predict which expressions the build
analyses, duplicating knowledge that lives in the builder — and a position the walk missed would
simply never be memoised, silently. Filling it from the call sites cannot drift. It is therefore the
one field written after `loadOrStore`, which is why it is a `sync.Map` behind a `nodeScalarUseMemo`
rather than a plain map.

#### The first design was WRONG about its own bound, and a verification sweep caught it

That design justified the absence of a ceiling by arguing that "every execution of one cached plan
runs the same build over the same plan, so the set of analysed expressions is identical every time and
the table is complete after the first execution". **That is false on two build paths**, both of which
synthesise a fresh `ast` node per execution and hand it straight to the analyser:

- the **min-label re-anchor** — `minLabelScanEnabled` defaults to on, and it fires on any multi-label
  pattern — builds a residual predicate at `min_label_scan_plan.go:251` and passes it directly to
  `newRowPredicate`;
- the **single-edge anchor swap** builds one at `anchor_swap_plan.go:300`, wraps it in a fresh
  `ir.Selection`, and the ordinary Selection build then reaches `newRowPredicate` with that pointer.

A pointer key can never hit for those, so each execution took a miss **and a store**: the table grew
for as long as the plan stayed cached. **Measured, with the ceiling check removed: 768 executions of
`MATCH (n:Person:Admin) WHERE n.age > 1 RETURN n.name` left 770 entries** — one per execution plus the
two stable ones. That is an unbounded cache, which the bounded-resource rule forbids outright and
which no throughput number justifies; it is a rung-2 defect against a rung-3 win.

`scalarUseMemoMaxEntries` (256) is now the declared ceiling: past it the memo stores nothing and
answers from the live analysis, which is precisely the pre-memo behaviour. The stable shapes are
untouched — their expressions come straight off the cached IR, so they are stored once, far inside the
ceiling, and hit forever. `TestNodeScalarUseMemoIsBoundedForSynthesisedPredicates` runs 3× the ceiling
in executions so the assertion cannot pass vacuously, and it was verified to fail with the check
removed.

**The two synthesis sites are worth fixing on their own account** — a multi-label pattern gets no
benefit from this memo at all today — but that is a separate change to those planner paths, filed
rather than bundled in here.

**Two properties make sharing sound, and both are tested rather than asserted.** Stores go through
`LoadOrStore`, so every caller for one expression gets the same analysis. And a stored analysis is
immutable — the same invariant §2a already relies on when it hands `presenceMaps` to concurrent row
workers without synchronisation.

The oracle for both is deliberately **absolute, not differential**: after the query has run five
times, every memoised entry is recomputed FRESH and compared with `reflect.DeepEqual`. A differential
between two engines would go green if both shared the same corrupted value, and `DeepEqual` is used
instead of a hand-written field comparison so a field added to `nodeScalarUse` later cannot escape the
check. That single assertion covers both requirements: a value that is not what the unmemoised path
would produce, and a value a consumer mutated, both surface as the same inequality.

`TestNodeScalarUseMemoIsConsulted` asserts on the hit/miss counters that the memo is **read**, not
merely populated — a populated-but-ignored memo is invisible to any result-level test, since the
results would be identical and only the win absent. `TestNodeScalarUseMemoObservesBothBailoutStates`
earns its place: it **failed on first run** and showed that no query in the test corpus memoised a
`bailout=true` analysis, because a pattern comprehension in the projection list never reaches the
analysis at all. Moving it into the predicate fixed the coverage. `…ConcurrentExecutions` drives one
cached plan from 16 goroutines and re-checks the shared value afterwards;
`…AbsentFallsBackToTheAnalysis` pins that a nil memo means "compute it", never "there is nothing to
compute", which is what the plan-rendering and write-path builders depend on.

**`copySchema` is NOT addressed here, and memoisation is the wrong tool for it.** It is a shallow map
copy taken mid-build at 33 sites, defensive against the builder's own continuing mutation of its
schema map, so its value is a function of the build's position rather than of the plan alone. The
structural answer is the one Memgraph uses — resolve the column layout once at plan time and reach
slots by array index, never rebuilding a name→index map — and that is part of the architecture
decision above, not a separate memo.

---

## 4. OPEN — example 20 demonstrates the costly PageRank API

*Class: efficiency, and the quality of what the example teaches.*

`examples/20_concurrent_reads` calls the one-shot `centrality.PageRank` inside its per-worker read
loop. The one-shot rebuilds the reverse-CSR transpose on every call, so:

| site | share of example 20's 204.60 MB |
|---|---|
| `search/centrality.pageRankBuildReverseStructure` | **74.32 MB — 36.32 %** |
| `search/centrality.PageRankCtx` (cum) | 95.37 MB — 46.61 % |

The module already ships the reuse path and documents it: `PageRanker` caches the transpose across
runs, and its godoc explicitly says to "give each goroutine its own PageRanker … because the
underlying CSR is immutable and read-only, independent PageRankers over the same snapshot are
race-free" — exactly this example's shape. This is an example-level fix that also corrects what the
example demonstrates.

---

## 5. INFO — default scale defeats CPU attribution

34 of the 37 examples finish in ≤ 2 s at their deterministic default. At 100 Hz that is roughly a
hundred CPU samples, and the aggregate CPU across the whole default sweep put no GoGraph symbol above
**0.11 s**. The heap profiles are informative at default scale because allocation counting does not
depend on run length; the CPU profiles are not. Any future cycle reading CPU from an example must
raise its scale first — as this one did for 26.

---

## 6. FIXED — the pre-size of §1 charged every single-row result for a 4096-row batch (rmp #2389)

*Class: efficiency + bounded resources. Found by re-profiling at HEAD before building on §3.*

**This is a regression the previous cycle shipped, and it was found only because the premise of §3
was re-measured at HEAD instead of being taken from the audit that recorded it.** Re-profiling
`examples/35_mvcc_mixed_workload` put `exec.Chunk.commitDynamic` — the helper §1 introduced — at
**81.45 % of all allocation in the process**, and pushed §3's `buildReadPhysical` from 65.30 % down
to 12.26 % purely by inflating the denominator.

### The mechanism

`NewDynamicChunk` maps a capacity below 1 onto `DefaultChunkCapacity` (4096). `materializeColumnar`
passes `capHint`, which is **0 whenever the plan exposes no `RowCountHint`** — and an indexed point
lookup correctly exposes none, because every operator that can drop rows deliberately withholds one.
Before §1 that 4096 was an inert hint and `append` grew the backing; §1 turned it into an
unconditional eager reservation on the first `Put`.

The hot query is `MATCH (n:Account {id: $id}) RETURN n.balance AS b` — **exactly one row**. Dividing
the profile gives 99 779.09 MB over 3 206 926 allocations = **32 626 B each**, i.e. 4096 × 8 bytes
reserved to hold 8, about 3.2 million times.

### Measured — three arms, interleaved, arms never overlapping, 3 rounds

`NONE` is the pre-§1 shape (no reservation); `OLD` is the shipped `cb7caef7`; `NEW` is the fix.

| example | arm | total allocation | throughput |
|---|---|---|---|
| **35** (`-nodes 3000 -readers 4 -phase-window 3s`) | OLD | 126.92 / 126.97 / 128.24 GB | 345 404 / 357 898 / 350 500 ops/s |
| | **NEW** | **46.41 / 46.73 / 47.50 GB** | **767 889 / 778 929 / 795 826 ops/s** |
| | NONE | 45.90 / 46.40 / 46.67 GB | 786 758 / 779 698 / 805 284 ops/s |
| **23** (`-nodes 20000 -queries 20000 -sessions 8`) | OLD | 4.83 / 4.83 / 4.75 GB | 7 597 / 7 362 / 7 290 q/s |
| | **NEW** | **4.70 / 4.75 / 4.87 GB** | **7 355 / 7 321 / 7 228 q/s** |
| | NONE | 6.64 / 6.57 / 6.58 GB | 7 048 / 6 872 / 6 775 q/s |

Against the shipped build the fix is **−63 % allocation and 2.22× throughput** on example 35, while
example 23 — the workload §1 was measured on — **keeps its win**: `NEW` tracks `OLD` within noise on
both axes and stays clearly ahead of `NONE`. Every deterministic fact both examples print is
identical across every arm (example 23 differs only in log timestamps).

**Both premises were true, which is the point.** §1's win reproduces exactly as recorded, and it is
still catastrophic on the other shape. The distinguishing variable is not the plan, the index or the
hint — both examples run on the same 4096 default with no hint at all — it is simply **how many rows
the chunk actually receives**: example 23 drives thousands per chunk through
`EagerAggregation.consumeChunk`, where removing the reservation moves 636 MB of `commitDynamic` into
2 464 MB of `Chunk.pushI64` growth-copying; example 35 receives one.

### The fix

A `dynamicCommitFloor` of 16 rows is reserved on commit instead of the capacity, and a new `growTo`
escalates a column **straight to the capacity in one step** the first time the floor is exhausted —
so a filling column still costs two allocations rather than a doubling series, and a one-row result
costs 128 bytes rather than 32 KB. `growTo` is inert for statically sized columns (`NewChunk` already
sizes those to capacity) and inert past the capacity, where `append`'s own amortised growth resumes.
`promoteToBoxed` is sized to the same floor.

Benchmarks, `-benchmem`: single row **320 B/op, 3 allocs**; full 4096-row batch **33 088 B/op,
4 allocs** against the shipped 32 960 B in 3 — one extra allocation and a 128-byte copy is the whole
price of the trade.

### What the prior art says, and the rule worth keeping

DuckDB, ClickHouse and Memgraph were read at source
(`e500d778`, `f70042f0`, `343f7fe3`) to settle the design rather than argue it:

- **DuckDB** fixes `STANDARD_VECTOR_SIZE` at 2048 (`common/vector_size.hpp:16`), every
  `DataChunk::Initialize` defaults to it, and the pipeline never passes a capacity
  (`parallel/pipeline_executor.cpp:64,886`). `estimated_cardinality` has three consumers — EXPLAIN,
  **degree of parallelism**, progress — and allocates nothing.
- **ClickHouse** fixes `DEFAULT_BLOCK_SIZE` at 65409 (`Core/Defines.h:32`) and states in the source
  that `IColumn::reserve` "affects performance only (not correctness)" (`Columns/IColumn.h:623`).
  `FilterTransform` filters the first column against a hard upper bound, reads the **exact** surviving
  count, then sizes every remaining column at exactly that (`FilterTransform.cpp:303,307,353`).
- **Memgraph**, the closest architectural peer, has no result buffer at all: its `Frame` is sized by
  **symbol count** — columns, never rows (`query/interpret/frame.hpp:58`), and every `reserve()` in
  `query/plan/operator.cpp` comes from a column count, an actual `.size()`, a fixed batch constant or
  a bound taken from the query text. Its `CostEstimator` output is consumed only to choose between
  candidate plans.

So the rule, which none of the three violates and which is now the review criterion here:
**an allocation width may be derived from a fixed constant, an exactly-known count, or a hard upper
bound — never from a statistical estimate.** GoGraph's `RowCountHint` is documented as a true upper
bound rather than an estimate, so sizing from it stays legitimate; what was not legitimate was
reserving the *fallback default* as though someone had claimed it.

### Pinned by

`TestCommitDynamicReservesTheFloorNotTheCapacity` (all six commit paths) and the amended
`TestPromoteToBoxedPreSizes`, both asserting an **absolute** bound of 64 rows rather than one written
in terms of `dynamicCommitFloor` — a test phrased against the constant moves its own goalposts and
would go green again the moment someone raised the floor back to the capacity. Both were verified to
fail against the shipped behaviour, reproduced in a scratch worktree by setting the floor to
`DefaultChunkCapacity` (reported: "reserved 4096 rows, want at most 64").
`TestCommitDynamicFloorIsBoundedByTheCapacity` pins the other side — a chunk deliberately narrower
than the floor is never widened past the capacity it asked for.
`BenchmarkDynamicChunkSingleRow` and `BenchmarkDynamicChunkFullBatch` pin the two shapes as a pair,
because **either one alone is satisfied by a wrong answer**. Read them in **B/op, not allocs/op**: an
allocation count is blind to this regression — 4096 slots and 16 slots are both exactly one
allocation, and §1 was measured against a count.

### A bounded-resources note, stated without overclaiming

This is a *cost* finding, not a vulnerability, and no exploit is claimed. But the direction is worth
recording under the **secure** rung rather than only the fast one: before the fix, the memory a query
reserved was a function of a number the planner *predicted*, charged in full before a single row
arrived. After it, reservation tracks the rows that actually materialise — the floor is paid up
front, and the capacity only when the rows to fill it exist. A plan reporting a large upper bound
that then yields nothing now costs 128 bytes instead of its whole bound. The engine-wide ceilings
(#1292 row cap, #1328 byte budget, #1842 ~1 MiB charge) were and remain the actual guard; this
narrows what has to be caught by them.

### Left open, deliberately

A result between 17 and 4096 rows escalates to the full capacity on its 17th row, so a 20-row result
still reserves 32 KB. A quadrupling escalation would cut that at the cost of three more allocations
on the full-batch path. **It is not implemented because it is not measured**: no example in the
corpus was shown to produce results in that band, and the two that were measured both sit at the
extremes. Filed as a question for the next cycle rather than guessed at now.

---

## On the security rung of correct → secure → fast → efficient

This sweep produced **no security finding, and it is not evidence of their absence.** A CPU or heap
profile of a benign, seeded workload is not a security instrument: it shows where a cooperative
program spends its time, never what a hostile input could make it do. The one class it could in
principle surface — an allocation whose size is a function of untrusted input with no ceiling —
cannot appear here, because no example feeds itself hostile input. Security assurance for these paths
rests on the dedicated audits (`docs/audit-production-readiness-*`, `docs/security-*`) and on the
existing input ceilings such as the 128 MiB interchange cap, not on anything measured below.

## Checked and clean

- **Retention.** The heap profile is written after `runtime.GC()`, so its `inuse_space` is live heap
  at exit. Across every default-scale run it was **0.5 – 4.0 MB**. No workload retains memory it has
  finished with.
- **All 43 runs exited 0** (after the two examples needing arguments were given them).

## Checked, not concluded

- **`buildEdgeTypeFilter` is 1 872.70 MB (5.59 %) of example 26**, reached entirely through the
  cache's miss path (`edgeTypeFilterFor.func1`). The cache is Engine-shared and keyed on the
  topology epoch, and the example performs writes between its query groups, which would be a
  sufficient explanation — but this was **not confirmed** against the cache's own hit/miss counters
  and should not be recorded as "working as intended" until it is.

## A harness lesson worth keeping

The first sweep bounded each example with `perl -e 'alarm shift; exec @ARGV'`. **That does nothing to
a Go program**: the Go runtime marks `SIGALRM` as `_SigNotify`, so with no goroutine listening the
signal is ignored rather than fatal. Example 26 ran unbounded for 12 minutes under a timeout that had
never been able to fire. Every later run used an explicit `SIGTERM`-then-`SIGKILL` watchdog, and the
watchdog was confirmed to fire.
