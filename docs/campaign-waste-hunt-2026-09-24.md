# Waste-hunt campaign, 2026-09-24 — every example profiled, CPU and RAM waste ranked

Sprint 363, rmp #2881 (SPIKE). This document records the profiling sweep and ranks the
waste it found. **No module code was changed.** Findings that sprint 362 already fixed
(the BFS frontier queue, the Dijkstra working set, the Brandes `dv1` hoist, the PageRank
`math.Abs` branch, the label-bitmap image) are not re-ranked; see
[`campaign-whole-surface-2026-09-16.md`](campaign-whole-surface-2026-09-16.md).

## Method

- **Driver.** `scripts/examples-lab.sh` at HEAD `dfeacabf`, two passes: `default` (every row
  at its own default scale, CPU + heap + mutex + block + goroutine profiles, `-contention 1`)
  and `elevated` (every row at its elevated scale, CPU + heap only — the CPU-attribution
  pass). Binaries were built once, outside every timed window (10 s).
- **Reading.** `go tool pprof -nodefraction=0` for every total; no `-show` filter anywhere.
  Allocation is `alloc_space` (cumulative bytes allocated), not live heap. pprof's `MB`/`GB`
  are binary (MiB/GiB).
- **Cross-row totals** sum each function's flat value over all 42 elevated profiles
  (`$L/agg.py`). Ranking is by absolute seconds and bytes, not by share.
- **Premises** were verified in the source at HEAD before a finding was ranked. Runtime
  parking (`findRunnable`, `pthread_cond_*`, `usleep`: 199.5 s cumulative, 152.1 s of it in
  row 36) is excluded from every figure.
- **Harness-owned cost** (code in `package main` of an example) is reported separately and
  not ranked.

**Environment.** Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64, Go 1.27.1,
`GOMAXPROCS` unset. Working tree dirty only in `CLAUDE.md`. No other workload ran during
either pass (host checked before start: load1 1.19, top process iTerm2 at 31% of one core).
Load1 during `elevated` ranged 1.51–5.84; every rise follows a concurrent example of the
sweep itself, and every row ran alone.

**Artefact root** `$L` =
`/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/8b109877-2926-4e22-ad29-9090a7505224/scratchpad/lab-2881/`
— manifests `$L/out/{default,elevated}/manifest.tsv`, per-row profiles and logs
`$L/out/<pass>/<row>/`, undropped pprof listings `$L/reconcile/<pass>.<row>.{cpu,alloc}.{top,cum}`.

## Manifest

42 rows per pass (37 examples; example 24 is six processes). `elapsed` is quantised to the
driver's 5 s poll; an example's own `# *.elapsed` telemetry in its `run.log` is the precise
figure. CPU is the CPU profile total; alloc is `alloc_space`.

| | rows | CPU | alloc | Σ elapsed | wall (incl. probes) |
|---|---|---|---|---|---|
| `default` | 42 | 102.5 s | 63.81 GiB | 301 s | 17:01:40–17:07:35 UTC |
| `elevated` | 42 | 1687.7 s | 387.65 GiB | 1185 s | 17:07:54–17:28:07 UTC |

| row | default: exit / elapsed s / CPU s / alloc | elevated argv | elevated: exit / elapsed s / CPU s / alloc | load1 before→after (elevated) |
|---|---|---|---|---|
| `01_basic` | 0 / 5 / 0.03 / 50.4 MiB | `-nodes 150000` | 0 / 5 / 4.18 / 16.21 GiB | 3.06→2.97 |
| `02_property_graph` | 0 / 5 / 0.01 / 9.5 MiB | `-persons 300000` | 0 / 5 / 2.60 / 2.67 GiB | 2.97→2.81 |
| `03_advanced_algorithms` | 0 / 5 / 0.00 / 6.6 MiB — FAIL: cpu-total-zero | `-communities 60 -nodes 800` | 0 / 50 / 44.22 / 274.1 MiB | 3.87→2.54 |
| `04_persistence` | 0 / 5 / 0.11 / 20.5 MiB | `-packages 40000` | 0 / 165 / 12.09 / 3.18 GiB | 2.54→1.97 |
| `05_out_of_core` | 0 / 5 / 0.00 / 13.8 MiB — FAIL: cpu-total-zero | `-nodes 300000` | 0 / 5 / 3.28 / 8.63 GiB | 1.97→1.98 |
| `06_csv_import` | 0 / 5 / 0.01 / 15.0 MiB | `-nodes 200000` | 0 / 5 / 4.18 / 14.52 GiB | 1.98→2.94 |
| `07_graphml_roundtrip` | 0 / 5 / 0.00 / 8.4 MiB — FAIL: cpu-total-zero | `-nodes 150000` | 0 / 5 / 3.52 / 5.95 GiB | 2.94→2.86 |
| `08_pagerank` | 0 / 5 / 0.01 / 7.9 MiB | `-pages 400000` | 0 / 5 / 11.31 / 15.49 GiB | 2.86→2.79 |
| `09_leiden` | 0 / 5 / 0.00 / 5.4 MiB — FAIL: cpu-total-zero | `-communities 80 -community-size 400` | 0 / 10 / 8.64 / 21.63 GiB | 2.79→2.75 |
| `10_dimacs9_routing` | 0 / 5 / 0.03 / 20.0 MiB | `-vertices 300000 -edges 1500000` | 0 / 5 / 5.61 / 11.76 GiB | 2.75→2.77 |
| `11_social_network` | 0 / 5 / 0.00 / 7.9 MiB — FAIL: cpu-total-zero | `-users 100000` | 0 / 20 / 131.75 / 1.41 GiB | 3.67→5.84 |
| `12_build_dependency` | 0 / 5 / 0.00 / 10.5 MiB — FAIL: cpu-total-zero | `-modules 400000` | 0 / 5 / 5.40 / 9.91 GiB | 5.84→5.38 |
| `13_network_reliability` | 0 / 5 / 0.00 / 5.8 MiB — FAIL: cpu-total-zero | `-clusters 40 -cluster-size 40` | 0 / 5 / 2.88 / 76.3 MiB | 5.38→5.10 |
| `14_routing_alternatives` | 0 / 5 / 0.02 / 10.6 MiB | `-nodes 20000` | 0 / 45 / 40.09 / 5.96 GiB | 5.10→3.45 |
| `15_task_assignment` | 0 / 5 / 0.01 / 9.9 MiB | `-workers 2500 -tasks 2500` | 0 / 5 / 3.37 / 438.2 MiB | 3.45→3.25 |
| `16_centrality_analytics` | 0 / 5 / 0.02 / 8.3 MiB | `-communities 20 -nodes 250` | 0 / 10 / 8.82 / 305.6 MiB | 3.25→3.06 |
| `17_transactional_log` | 0 / 5 / 0.36 / 156.6 MiB | `-accounts 3000 -transfers 9000` | 0 / 55 / 7.74 / 2.67 GiB | 3.06→2.07 |
| `18_oocore_pipeline` | 0 / 5 / 0.02 / 27.0 MiB | `-nodes 300000` | 0 / 5 / 7.89 / 19.78 GiB | 2.07→2.06 |
| `19_pattern_query` | 0 / 5 / 0.06 / 25.8 MiB | `-nodes 200000` | 0 / 5 / 11.83 / 7.57 GiB | 2.54→2.33 |
| `20_concurrent_reads` | 0 / 5 / 0.76 / 206.6 MiB | `-nodes 60000 -iterations 200` | 0 / 25 / 115.70 / 32.71 GiB | 2.33→4.37 |
| `21_typed_recovery` | 0 / 5 / 0.08 / 23.9 MiB | `-nodes 50000` | 0 / 225 / 39.56 / 4.04 GiB | 4.37→2.01 |
| `22_cypher` | 0 / 5 / 0.01 / 16.8 MiB | `-users 150000` | 0 / 10 / 12.96 / 16.09 GiB | 2.01→2.09 |
| `23_bolt_server` | 0 / 5 / 0.12 / 36.4 MiB | `-nodes 30000 -queries 60000` | 0 / 5 / 3.23 / 844.7 MiB | 2.09→2.00 |
| `24_init` | 0 / 5 / 0.00 / 8.2 MiB — FAIL: cpu-total-zero | `init` | 0 / 5 / 0.00 / 6.1 MiB — FAIL: cpu-total-zero | 2.00→1.92 |
| `24_seed` | 0 / 5 / 0.00 / 7.3 MiB — FAIL: cpu-total-zero | `seed -users 150000 -friends 10` | 0 / 5 / 2.54 / 3.39 GiB | 1.92→2.73 |
| `24_stats` | 0 / 5 / 0.00 / 7.4 MiB — FAIL: cpu-total-zero | `stats` (elevated corpus) | 0 / 10 / 8.45 / 4.10 GiB | 2.99→2.76 |
| `24_query` | 0 / 5 / 0.00 / 9.0 MiB — FAIL: cpu-total-zero | `query` (elevated corpus) | 0 / 5 / 5.28 / 3.29 GiB | 2.76→2.62 |
| `24_plandiff` | 0 / 5 / 0.40 / 359.8 MiB | `plandiff` (elevated corpus) | 0 / 20 / 19.35 / 13.34 GiB | 2.62→2.29 |
| `24_snapshot` | 0 / 5 / 0.11 / 129.9 MiB | `snapshot` (elevated corpus) | 0 / 5 / 4.88 / 3.69 GiB | 2.29→2.19 |
| `25_software_house_api` | 0 / 10 / 0.65 / 58.8 MiB | `-scale-components 15000 -scale-tasks 12000 -scale-developers 800` | 0 / 35 / 28.67 / 11.32 GiB | 2.19→1.94 |
| `26_social_scale_bench` | 0 / 91 / 84.52 / 47.56 GiB | (default) | 0 / 90 / 85.19 / 47.60 GiB | 1.94→2.22 |
| `27_concurrent_txn` | 0 / 5 / 1.42 / 6.00 GiB | `-accounts 20000 -ops-per-writer 200` | 0 / 5 / 6.40 / 1.11 GiB | 2.22→2.12 |
| `28_negative_weights` | 0 / 5 / 0.00 / 4.5 MiB — FAIL: cpu-total-zero | `-layers 80 -width 160` | 0 / 15 / 9.69 / 1.31 GiB | 2.12→1.95 |
| `29_all_pairs` | 0 / 5 / 0.02 / 9.9 MiB | `-nodes 1600` | 0 / 5 / 2.90 / 156.2 MiB | 1.95→1.79 |
| `30_min_spanning_tree` | 0 / 5 / 0.00 / 4.9 MiB — FAIL: cpu-total-zero | `-regions 300 -sites 900` | 0 / 5 / 2.99 / 10.22 GiB | 1.79→1.81 |
| `31_metrics_observability` | 0 / 5 / 0.07 / 55.7 MiB | `-services 80000` | **1** / 25 / 23.18 / 9.70 GiB — FAIL: exit=1 | 1.81→1.66 |
| `32_euler` | 0 / 5 / 0.00 / 3.8 MiB — FAIL: cpu-total-zero | `-nodes 300000 -loops 3000` | 0 / 10 / 5.43 / 13.83 GiB | 1.66→1.70 |
| `33_generation_swap` | 0 / 5 / 0.04 / 18.9 MiB | `-versions 80 -reads-per-reader 2000 -base-nodes 20000` | 0 / 5 / 6.26 / 4.54 GiB | 1.70→1.65 |
| `34_bolt_transactions` | 0 / 5 / 0.01 / 8.2 MiB | `-persons 400000` | 0 / 5 / 2.03 / 967.6 MiB | 1.65→1.51 |
| `35_mvcc_mixed_workload` | 0 / 5 / 13.06 / 8.81 GiB | (default) | 0 / 5 / 13.01 / 8.83 GiB | 1.51→1.95 |
| `36_mvcc_snapshot_topology` | 0 / 5 / 0.12 / 42.6 MiB | `-spokes 4000` | **1** / 245 / 968.33 / 48.02 GiB — FAIL: exit=1 | 1.95→5.07 |
| `37_mvcc_write_contention` | 0 / 5 / 0.41 / 39.6 MiB | `-customers 20000 -ops-per-producer 2000` | 0 / 5 / 2.25 / 203.4 MiB | 5.07→4.66 |

### Failures

Every run exited 0 except the two below. The script's other FAIL verdicts are
`cpu-total-zero` — a run shorter than the profiler's 10 ms sampling period, not an error.

| row | pass | error, read from `run.log` |
|---|---|---|
| `31_metrics_observability` | elevated | `run_exit=1`: *"planner statistics workload: PROFILE scored no misestimate: the planner predicted 12 rows for s.tier = 'legacy' and the query returned 3, a 4x miss, and the decommission removed only 0.0% of the 79993 live services — inside the staleness screen's 9.6% firing region — yet no tracked (label, property) statistic was caught … [staleTier] needs a new one (rmp #2795, rmp #2785)"*. A harness assertion at the elevated scale; its profiles are complete and are used below. |
| `36_mvcc_snapshot_topology` | elevated | `run_exit=1`: *"example 36: the self-contradiction query never ran; that half of the check did not happen"* (`reader.contradiction_checks=0`). The same outcome as sprint 362 at `-spokes 4000`; `snapshot_topology_invariant_holds=1`, `read_errors=0`. |
| 14 rows | default | `cpu-total-zero` (listed in the table). `24_init` is also `cpu-total-zero` in `elevated`: `init` has no scale knob. |

### Work per unit, for the fixed-duration rows

| row | work | CPU | alloc | per unit |
|---|---|---|---|---|
| `35_mvcc_mixed_workload` | 4 phases × 0.7 s at 513 540–699 490 ops/s ≈ 1.70 M reads (throughputs in `run.log`) | 13.01 s | 8.83 GiB | ≈ 7.6 µs CPU and ≈ 5.6 KiB per read |
| `36_mvcc_snapshot_topology` | 245 s; `writer.commits_per_s=17`, `writer.churn_deletes=25204`, `reader.observations=2709` | 968.33 s | 48.02 GiB | dominated by a cancelled diagnostic query (see "Not ranked") |
| `17_transactional_log` | 9000 transfers, `checkpoint.count=530` | 7.74 s | 2.67 GiB | ≈ 5.2 MiB per checkpoint of a 396 KiB snapshot (F6) |

## Ranked findings — biggest and simplest first

Each estimate of removable cost is an expectation, not a measurement: no A/B was run in
this task.

### F1 — `adjlist.storeEntry` clones the whole shard slot array on every unbracketed write: 170.6 GiB, 44% of the sweep

**Call site.** `graph/adjlist/adjlist.go:2692`,
`next := &shardSlots{slots: make([]unsafe.Pointer, len(base.slots))}` followed by
`copy(next.slots, base.slots)`.

**Premise, verified at HEAD.** A write with no transaction id and no bulk window is "its own
1-op window: clone-and-publish once (correct, just no dedup)" (the function's own comment).
Every edge insert through the plain `AddEdge`/`AddEdgeLabeledWithProperty` path therefore
copies `len(shard) = V/256` pointers. Total cost is O(E·V/256·8 bytes): quadratic in graph
size. Only `BeginCommit`/`BeginExclusiveBuild` (recovery, bulk import) and transactions
amortise it.

**Evidence.** `storeEntry` flat alloc, summed over the elevated sweep: **174 667 MiB =
170.6 GiB of 387.65 GiB (44.0%)**. Per row: `18` 20.38 GB (96.0% of the row), `09` 19.14 GB
(82.4%), `01` 16.31 GB (93.7%), `08` 15.94 GB (95.8%), `22` 15.39 GB (89.1%), `06` 14.11 GB
(90.5%), `10` 11.22 GB, `30` 10.41 GB, `12` 10.28 GB, `05` 8.99 GB, `32` 7.59 GB; 19 rows above
1 GB. `-list` puts 15.93 GB of `01`'s 16.31 GB on line 2692 alone.

The scaling proves the premise: bytes per inserted edge grow with V, as the model predicts
(8·V/256 per clone).

| row | V | E | storeEntry | bytes / edge | 8·V/256 |
|---|---|---|---|---|---|
| `01_basic` default | 5 000 | 101 974 | 19 MB | 195 B | 156 B |
| `01_basic` elevated | 150 000 | 4 239 078 | 16 308 MB | 4 034 B | 4 688 B |
| `10_dimacs9_routing` elevated | 300 000 | 1 500 000 | 11 217 MB | 7 842 B | 9 375 B |
| `08_pagerank` elevated | 400 000 | 1 599 985 | 15 938 MB | 10 445 B | 12 500 B |

CPU: `storeEntry` cumulative is 8.36 s across rows (0.14–1.25 s per row); the GC mark worker
in the same rows is 0.97–5.02 s (e.g. `22` 5.02 s of 12.96 s, `12` 3.12 s of 5.40 s). That the
GC share is caused by this churn is **hypothesised**, not established.

**Evidence files.** `$L/out/elevated/{01_basic,08_pagerank,09_leiden,18_oocore_pipeline,22_cypher}/heap.pprof`,
listings `$L/reconcile/elevated.*.alloc.top`.

**Estimated removable.** With a two-level slot array (a small directory of fixed-size pages,
copy-on-write of one page per write), the per-write copy falls from 8·V/256 bytes to one page
plus the directory — for a 64-slot page at V = 150 000, about 600 B instead of 4.7 KB, **~85–95%
of the 170.6 GiB**, and the saving grows with V. CPU gain: most of the 8.36 s, plus an
unmeasured share of GC.

**Fix sketch.** Page the shard's slot array; copy-on-write clones only the touched page and
the page directory. Readers keep loading an immutable directory, so the lock-free read
contract (old-or-new, never torn) is unchanged.

**Risk.** High sensitivity, moderate size. This is the MVCC and checkpointer read path
(`storeEntry`'s unwind note; `LoadEntryH`/`WalkEdgeHandles` read under the commit lock, not
`visMu`). ACID isolation depends on older arrays staying frozen. Validation must include the
four tests named in the `storeEntry` doc comment, `go test -race ./graph/adjlist/... ./graph/lpg/...`,
the `store/` crash-recovery battery, and the DST multi-session mode. No TCK surface.

### F2 — `DiameterCtx` runs ≈75 000 full BFS sweeps on a 100 000-vertex social graph: 126.7 s CPU

**Call site.** `search/diameter.go:220` (the parallel `levelMaxEccentricity` worker calling
`bfsFarthest`); root choice at `search/diameter.go:89–110`.

**Premise, verified at HEAD.** The iFUB walk is rooted at `farU`, the **endpoint** of the
2-sweep. The code comment says "centre-most vertex on the lo path (here: farU …)", but `farU`
is peripheral: `ecc(farU) = lo`, so `hi = 2·lo` always exceeds `lo`, and the walk must process
every level down to `k ≈ lo/2 + 1` — most of the graph.

**Evidence.** `11_social_network`, 131.75 s CPU over 19.24 s: `bfsFarthest` 120.85 s flat,
126.70 s cumulative (96.2%); telemetry `diameter.elapsed=18.390376s`, `diameter.lo=hi=18`.
A replica harness (`$L/h11/`: example 11 copied outside the repository, with a counter that
reproduces `DiameterCtx`'s seed, tie-break and level walk) measured the sweep count:
`ifub.peripheral_root_level_sweeps=75030` (levels 17…9 from `farU`; the module's walk, whose
`lo` rises to 18 during the walk, stops one level earlier — 74 991 sweeps), against 100 000
vertices. Level histogram in `$L/h11/run.log`.

Line-level (`-list search.bfsFarthest`): the inner edge loop `for k := verts[v]; k < verts[v+1]`
41.47 s, `dist[nb] = dv+1` 30.39 s, `edges[k]` 16.89 s, the frontier loop 18.74 s. The
per-sweep work is already allocation-free (429 allocations for the whole call).

**A hypothesis refuted here.** Rooting the walk at the midpoint of the `farU…farW` path — the
textbook iFUB start — was measured by the same harness at `central_root_ecc=14`,
`central_root_level_sweeps=49969`: **−33%, not the order of magnitude expected.** The graph
has four planted communities joined by eight bridges (levels 9–10 hold 39 and 50 vertices),
so no single centre is close to everything.

**Estimated removable.** Two micro items are certain and bit-identical:
(a) `levelMaxEccentricity` re-scans the whole `dist` slice after each sweep to find the
eccentricity (`search/diameter.go:222`, 3.30 s flat), although it equals
`dist[farthest]`, which `bfsFarthest` already returns; (b) the O(V) `dist` reset
(`:266`, 1.93 s) is avoidable with a generation-stamped visited array. Together ≈ 5.2 s. The
large item — the sweep count — needs an algorithm change: bound-driven pruning (Takes and
Kosters, *Determining the diameter of small world networks*, CIKM 2011) or bit-parallel
multi-source BFS (Then et al., *The More the Merrier: Efficient Multi-Source Graph
Traversal*, VLDB 2014). Either could remove most of the remaining ~120 s on this shape;
**hypothesised**, needs a spike.

**Risk.** Micro items: none (same result, unexported code). Algorithm change: correctness of
`(lo, hi, exact)` must be proved against the brute-force eccentricity oracle on generated
graphs; no TCK or ACID surface.

### F3 — a write-transaction statement rebuilds the whole-graph CSR pair and relationship-type column: 36.9 s and 30.2 GiB in row 36, 16.9 s in row 31

**Call site.** `cypher/csr_pair_cache.go:425` — `csrPairAndColumnCachedFor` bypasses the
cache whenever `viewCarriesOwnWrites(g)` (`:345`, true for **any** view with a transaction
id), then builds `csrPairFromGraphAt` + `buildRelTypeColumn` (O(V+E)).

**Premise, verified at HEAD.** The bypass is keyed on `snap.TxID() != 0`, not on whether the
transaction has written anything, and a pattern with an anchor and `LIMIT 1` still pays the
whole-graph build. This is rmp #2446's soundness rule, recorded in sprint 362 as R6
(rmp #2866, SPIKE).

**Evidence.**

| row | driver | CPU under the build | alloc under the build |
|---|---|---|---|
| `36_mvcc_snapshot_topology` | `main.runWrite` 857 stacks vs `observeWith` 71 — `MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: N}) DELETE r` | 36.92 s | **30 958 MiB (60.0%)** |
| `31_metrics_observability` | `driveCountStore`: 200 × `MATCH (a:SERVICE)-[:CALLS]->(b:SERVICE) WITH a,b LIMIT 1 CREATE …`, `countstore.write_elapsed=20.026335s` (≈100 ms per write) | **16.94 s of 23.18 s (73.1%)** | 6 515 MiB (62.6%) |
| `26_social_scale_bench` | read battery | 5.28 s | 3 131 MiB |
| `24_stats` | | 3.24 s of 8.45 s | 991 MiB |

(`-focus 'csrPairAndColumnCachedFor|csrPairCachedAt'`.) Inside it in row 36:
`forEachResolvedSlotType` 28.01 s, of which `EdgeLabelsByHandle` 23.12 s; `BuildReverse`
6.89 GB; `buildFromAdjList` 6.36 GB cum; `OrderRuns` 2.43 GB; `Snapshot.visible`'s memo map
6.36 GB (`graph/lpg/snapshot.go:125`, the R8 map, reached here).

**Estimated removable.** Nearly all of it for anchored, limited write statements: ≈ 16 s of
row 31 (its 200 writes would fall from ≈ 20 s to milliseconds) and ≈ 35 s / ≈ 29 GiB of row 36.

**Fix sketch.** Two options, both a design decision for the user under rmp #2866:
(a) let a write view that has **no pending writes yet** use the shared cache — its visibility
equals a pure reader's at the same `(TopoGeneration, startTS)`; narrow, but it misses in
rows 31/36 anyway because each write moves the topology; (b) expand a write view's pattern
over the live adjacency (the `AsOf` neighbour walk) instead of materialising a CSR — the
real fix, broad.

**Risk.** High. rmp #2446 (DST): serving a writer's pair to a reader exposes uncommitted
topology and mislands the position-keyed type filter. Full TCK, DST multi-session, and the
isolation checkers are required.

### F4 — the aggregation pre-projection builds a fresh, unpooled row map per row: 22.2 GiB and 6.86 s in row 26

**Call site.** `cypher/api.go:15227` `buildRowCtxWithUse` (`make(expr.RowContext, …)` per
row), called from `newAggregationEval`'s closure (`cypher/api.go:13058`).

**Premise, verified at HEAD.** The WHERE and scalar-projection closures use the pooled
`pooledRowCtx` (`acquireRowCtx`/`releaseRowCtx`, rmp #1575/#1697); the aggregation
pre-projection does not, "so every value handed to the expression is independently allocated
and may escape".

**Evidence.** `26_social_scale_bench`, `-focus newAggregationEval`: **22 706.89 MiB of
51 108.56 MiB (44.4%)**; CPU 6.86 s (8.1%). Map container share, by line:
`make` at 15228 1.99 GB, first assignment `ctx[b.name] = row[colIdx]` (15467) 7.16 GB — the
small map's group allocated on first insert — and 4.87 GB at 15489; ≈ 14 GB. CPU in map
operations under it: `mapassign_faststr` 2.57 s, `makemap` 1.10 s + 0.18 s. The rest is node
materialisation (`nodePropsToExprMap.func1` 5.57 GB, `upgradeNodeIDToValue` 8.28 GB cum) —
rmp #2732. Also reached in `24_plandiff` (5.00 GB, 35.7%) and `36` (2.56 GB).

**Estimated removable.** The map container: ≈ 14 GiB and ≈ 3.8 s in row 26; ≈ 3 GiB in
`24_plandiff`. Node materialisation is #2732's.

**Fix sketch.** Recycle the map container (`clear`) through the existing pool when
`scalarUse != nil`, keeping the lazy-node arena nil so any node value that escapes into a
group key stays independently owned.

**Risk.** Low-medium: the argument that the map does not escape the evaluator must be made
for aggregation arguments (`collect(n)` returns a value, not the map). Full TCK.

### F5 — `forEachResolvedSlotType` allocates two maps per source vertex: 10.0 GB

**Call site.** `cypher/api.go:22408` and `:22412` — `dstParallelTotal` and `dstSeen`, `make`d
inside the per-source loop.

**Premise, verified at HEAD.** The same function hoists its other scratch
(`slotLabs`, `slotFallbackSeen`) "once for the whole sweep"; these two are rebuilt per
source, and are needed only by the positional fallback, not the handle path.

**Evidence.** Flat 9 991 MiB across rows: `36` 7.32 GB (3.54 + 3.61 GB on the two lines),
`26` 1.83 GB, `24_stats` 0.45 GB. CPU on the two lines ≈ 1.6 s in `36`.

**Estimated removable.** ≈ 9.5 GB and 1–2 s, bit-identical output.

**Fix sketch.** Hoist both maps and reset them per source through a touched-list (the file's
own `slotLabsTouched` pattern), or build them lazily on the first fallback slot of a source.

**Risk.** Low; same component as F3 and batchable with it or ahead of it. `go test ./cypher/...`
plus the full TCK (relationship typing is TCK-observable).

### F6 — the snapshot writer allocates four 1 MiB `bufio` buffers per checkpoint: 2.10 GiB of row 17's 2.80 GiB

**Call site.** `store/snapshot/labels.go:228`, `properties.go:238`, `mapper.go:149`,
`writer.go:141` — `bufio.NewWriterSize(w, 1<<20)`.

**Evidence.** `17_transactional_log`: `bufio.NewWriterSize` 2 153.44 MiB flat (75.0%),
split evenly between `writeLabels`, `writeProperties`, `writeMapperStringN` and
`writeCSRWith`; `checkpoint.count=530`, `checkpoint.snapshot_bytes=395.93 KiB`. CPU 0.02 s.

**Estimated removable.** ≈ 2 GiB in row 17 (allocation only; no measurable CPU).

**Fix sketch.** Size each buffer by min(1 MiB, the section's known or estimated size), or
recycle writers in a bounded pool owned by the checkpointer.

**Risk.** Low — byte stream and flush order unchanged; the crash battery covers it.

### F7 — `db.propertyKeys()` scans every edge-property slot per call: 6.43 s of row 25

**Call site.** `graph/lpg` `(*Graph).PropertyKeysInUse` → `(*edgePropCols).forEachAt` →
`(*edgePropColumn).slotValue`.

**Evidence.** `25_software_house_api`: `handleSchema` 7.80 s of 28.67 s; `dbPropertyKeys`
6.43 s (22.4%), `RelationshipTypesInUse` 1.27 s. 400 `/schema` requests (`http_requests=1200`
in total) → ≈ 16 ms per call on the elevated corpus (15 000 components, 12 000 tasks, 800 developers).

**Estimated removable.** ≈ 6 s in row 25; cost grows with E.

**Fix sketch.** Maintain per-key in-use counts at write time, or memoise the answer per
topology/property generation.

**Risk.** Medium: the "in use" answer must stay snapshot-correct under MVCC.

### F8 — `AllNodesScan.Init` materialises every node id per execution: 4.29 GB in row 25

**Call site.** `cypher/exec/scan_all.go:77`, `op.nodeIDs = append(op.nodeIDs, id)`.

**Evidence.** `25_software_house_api`: 4.29 GB flat (36.2% of the row), driven by
`handleStats` → `countOne` → `MATCH ()-[r:T]->() RETURN count(r) AS n`; CPU only 0.27 s.
Also `19` 0.40 GB.

**Estimated removable.** ≈ 4 GB. A relationship-type count answered from the count store
would remove the scan and the expansion (`Expand.advanceInput` 2.55 GB, `Expand.buildRow`
1.35 GB) altogether.

**Risk.** Medium — streaming changes iteration under concurrent writes; a count-store plan is
a planner change and TCK-observable.

### F9 — the physical plan is rebuilt on every execution of a cached point query: 1.77 s and 5.3 GB in row 35

**Evidence.** `35_mvcc_mixed_workload` (≈ 1.70 M executions of
`MATCH (n:Account {id: $id}) RETURN n.balance AS b`): `buildReadPhysical` 1.77 s cum = 58% of
`runRead`'s 3.07 s; alloc 5 310.99 MiB cum = 56.0% of the row (`copySchema` 1.27 GB,
`buildIRProjection` 2.15 GB cum). ≈ 1.0 µs and ≈ 3.2 KiB per execution.

**Estimated removable.** Up to ≈ 1.7 s and ≈ 5 GB in row 35. This is rmp #2391's subject
(plan-pure separation); sprint 362 measured `buildReadPhysical` at 25.61% elsewhere.

**Risk.** Medium-high: a reused physical plan must not carry per-execution state.

### F10 — per-row re-evaluation of constant temporal expressions and function-name resolution: ≈ 1.6 s in row 26

**Evidence.** `26_social_scale_bench` temporal queries: `funcs.fnDate` 0.72 s (from
`date($ref)`, constant per statement), `nowAwareRegistry.Resolve` 0.87 s cum with
`strings.ToLower` 0.30 s inside `Registry.Resolve` (function-name lookup per call),
`decodeTemporalString` 1.35 s cum (stored temporal decode per row, inherent to the tagged-string
storage contract).

**Estimated removable.** ≈ 1.6 s (constant folding of parameter-only calls; resolving the
function once at plan time). Low risk, TCK-covered semantics unchanged.

## Batching

- **F1** stands alone (`graph/adjlist`), and is the largest single item in the sweep.
- **F2** stands alone (`search/diameter.go`); its two micro items can ship before the spike.
- **F3 + F5** share `cypher`'s CSR-pair/type-column path; F5 is a safe first step inside it.
- **F4 + F10** share the per-row evaluation path (`cypher/api.go`), with #2732 and #2865 nearby.
- **F6** (`store/snapshot`) and **F7/F8** (`graph/lpg`, `cypher/exec`) are independent and small.

## Not ranked

**Harness-owned (examples' `package main`).**

| row | site | cost |
|---|---|---|
| `14_routing_alternatives` | `main.kNearest` → `sort.Slice` | **35.16 s of 40.09 s CPU (87.7%)**, 6.17 GB alloc — the example's evidence is dominated by its own solver |
| `20_concurrent_reads` | `main.topKByRank` → `sort.Slice`; per-worker PageRank (rmp #2382) | 13.50 s CPU; `pageRankBuildReverseStructure` 13.75 GB |
| `32_euler` | `math/rand.(*Rand).Perm` | 6.87 GB alloc, 4.43 s CPU |
| `36_mvcc_snapshot_topology` | `qContradiction` (declared O(spokes²) in its own source), run at `-spokes 4000` and cancelled | `mvcc.Visible` 322.69 s, `supersededAt` 166.30 s, `entryAsOfLoaded` 85.34 s flat — rmp #2860 |

**Inherent or by design.**

- `21_typed_recovery`: 50 000 commits in `build.elapsed=3m39.486117s` (≈ 4.4 ms each);
  `write(2)` from `wal.leadGroupSyncLocked` 27.79 s and `F_FULLFSYNC` 3.47 s. This is the
  durability contract of one commit per source vertex; not waste without a durability decision.
- `04_persistence`: 165 s elapsed, 12.09 s CPU — fsync-bound.
- `03_advanced_algorithms`: `brandesSource` 40.15 s flat — the betweenness work itself (R9 shipped).
- `24_*`: every subcommand replays the WAL on open (`replayWALInto` ≈ 1.2 s CPU, ≈ 1.1 GB each).
- Mutex contention (`default` pass): the largest total is `35` at 0.66 s, 92% of it
  `runtime.unlock`; nothing above 0.3 s is attributable to a module lock.

## Limits

1. **No A/B.** Every "estimated removable" is an expectation. Each fix owes an interleaved
   before/after with `benchstat` and the row re-run.
2. **Single run per pass.** No noise floor was measured for this sweep; the absolute totals
   quoted here are large relative to any plausible run-to-run spread, but small deltas
   (under ~5%) should not be compared across passes.
3. **Allocation is sampled** by the heap profiler at the Go default `runtime.MemProfileRate` (512 KiB mean; `examples/internal/exprof` does not change it); per-line figures are
   estimates, which is why F1's bytes-per-edge sits within ~15–25% of the model rather than on it.
4. **GC attribution is inferred.** Which share of `gcBgMarkWorker` (41.49 s cumulative) each
   allocation site causes is not established by a profile.
5. **No `runtime/trace`, no hardware counters, no contention pass.** Scheduler, GC-pause and
   memory-stall claims are not made.
6. **F2's sweep counts come from a replica,** not from instrumenting the module; the replica
   reproduces seed, tie-break and level rule, and its predicted stop level matches the
   module's `lo=18` result.

## Reproduction

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
L=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/8b109877-2926-4e22-ad29-9090a7505224/scratchpad/lab-2881
scripts/examples-lab.sh "$L/out" default     # manifest: $L/out/default/manifest.tsv
scripts/examples-lab.sh "$L/out" elevated    # manifest: $L/out/elevated/manifest.tsv

go tool pprof -nodefraction=0 -top                             "$L/out/elevated/<row>/cpu.pprof"
go tool pprof -nodefraction=0 -sample_index=alloc_space -top   "$L/out/elevated/<row>/heap.pprof"
go tool pprof -nodefraction=0 -sample_index=alloc_space -list 'AdjList.*storeEntry' "$L/out/elevated/01_basic/heap.pprof"
go tool pprof -nodefraction=0 -focus 'csrPairAndColumnCachedFor|csrPairCachedAt' -top "$L/out/elevated/31_metrics_observability/cpu.pprof"
python3 "$L/agg.py" "$L/reconcile" alloc 0 40   # cross-row flat totals (cpu|alloc, 0=flat 3=cum)
( cd "$L/h11" && ./h11 -users 100000 )          # F2 sweep counter (replica of example 11)
grep '^#\|^run_exit=' "$L/out/<pass>/<row>/run.log"
```

## Iteration 2 — re-profile at `f6b96b15` (rmp #2893)

Sprint 363, rmp #2893 (SPIKE). The same two passes, rerun at HEAD `f6b96b15` after the
iteration-1 fixes (#2882 F1, #2883 F3, #2888/#2889, #2890–#2892 F4/F5/F10, and the rollback
fixes `47334e3b`/`787e674a`). **No module code was changed.** Every number below comes from
this run; "before" is the #2881 sweep at `127c012b` (`$L` above).

**Method.** Unchanged from the first sweep: `scripts/examples-lab.sh` run from the repository
root, passes `default` then `elevated`, fresh binaries (built once, 20 s, outside every timed
window), `go tool pprof -nodefraction=0` for every total, `alloc_space` for allocation.
Runtime parking (`runtime.findRunnable` cumulative: 231.6 s at HEAD, 199.5 s before) is
excluded from every ranked figure; code in an example's `package main` is not ranked.

**Environment.** Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64, Go 1.27.1, `GOMAXPROCS`
unset; working tree dirty only in the project instructions file. Host before start: load1
1.47, top process iTerm2 at 20% of one core; no other workload ran. Load1 during `elevated`
ranged 1.61–5.23; every rise follows a concurrent row of the sweep itself (`11`, `36`).
Passes ran 12:42–13:07 local, 2026-09-27.

**Artefact root** `$M` = the sibling `lab-2893/` of `$L` — manifests
`$M/out/{default,elevated}/manifest.tsv`, driver log `$M/driver.log`, listings
`$M/reconcile/<pass>.<row>.{cpu,alloc}.{top,cum}`, comparison script `$M/cmp.sh`, Neo4j
reference file `$M/CachingExpandInto.java`.

### Manifest, before → after

| | rows | CPU | CPU net of parking | alloc | Σ elapsed |
|---|---|---|---|---|---|
| `default` | 42 | 102.5 → 91.7 s | | 63.81 → 46.95 GiB | 301 → 290 s |
| `elevated` | 42 | 1687.7 → **1472.4 s (−12.8%)** | 1488.2 → **1240.8 s (−16.6%)** | 387.65 → **175.87 GiB (−54.6%)** | 1185 → 1106 s |

**Failures.** The same two genuine failures as before, both example assertions, not module
errors; their profiles are complete and are used below.

| row | pass | error, read from `run.log` |
|---|---|---|
| `31_metrics_observability` | elevated | `run_exit=1`: the same `[staleTier]` planner-statistics assertion as before (rmp #2795, #2785). It now fails at 5 s instead of 25 s because the count-store phase before it fell from `countstore.write_elapsed=20.026335s` to `11.35ms` (F3, #2883). |
| `36_mvcc_snapshot_topology` | elevated | `run_exit=1`: *"the self-contradiction query never ran"* (`reader.contradiction_checks=0`, `read_errors=0`, `snapshot_topology_invariant_holds=1`). Cause: finding G1 below. |
| 13 rows | default | `cpu-total-zero` (runs under the 10 ms sampling period). `05_out_of_core` now registers 10 ms and passes; `24_init` is also `cpu-total-zero` in `elevated`. |

| row | default CPU s | default alloc | elevated CPU s | Δ | elevated alloc | Δ | elevated exit / elapsed s |
|---|---|---|---|---|---|---|---|
| `01_basic` | 0.03 → 0.02 | 50.4 MiB → 36.0 MiB | 4.18 → 1.31 | -69% | 16.21 GiB → 990.4 MiB | -94% | 0 / 5 → 5 |
| `02_property_graph` | 0.01 → 0.01 | 9.5 MiB → 8.9 MiB | 2.60 → 1.70 | -35% | 2.67 GiB → 575.3 MiB | -79% | 0 / 5 → 5 |
| `03_advanced_algorithms` | 0.00 → 0.00 | 6.6 MiB → 4.1 MiB | 44.22 → 45.28 | +2% | 274.1 MiB → 66.1 MiB | -76% | 0 / 50 → 50 |
| `04_persistence` | 0.11 → 0.11 | 20.5 MiB → 23.9 MiB | 12.09 → 11.85 | -2% | 3.18 GiB → 1.63 GiB | -49% | 0 / 165 → 150 |
| `05_out_of_core` | 0.00 → 0.01 | 13.8 MiB → 9.2 MiB | 3.28 → 0.37 | -89% | 8.63 GiB → 263.1 MiB | -97% | 0 / 5 → 5 |
| `06_csv_import` | 0.01 → 0.01 | 15.0 MiB → 11.3 MiB | 4.18 → 1.65 | -61% | 14.52 GiB → 1.39 GiB | -90% | 0 / 5 → 5 |
| `07_graphml_roundtrip` | 0.00 → 0.00 | 8.4 MiB → 9.3 MiB | 3.52 → 2.23 | -37% | 5.95 GiB → 1.77 GiB | -70% | 0 / 5 → 5 |
| `08_pagerank` | 0.01 → 0.01 | 7.9 MiB → 6.2 MiB | 11.31 → 6.14 | -46% | 15.49 GiB → 656.6 MiB | -96% | 0 / 5 → 5 |
| `09_leiden` | 0.00 → 0.00 | 5.4 MiB → 5.4 MiB | 8.64 → 5.85 | -32% | 21.63 GiB → 3.74 GiB | -83% | 0 / 10 → 10 |
| `10_dimacs9_routing` | 0.03 → 0.03 | 20.0 MiB → 14.1 MiB | 5.61 → 2.70 | -52% | 11.76 GiB → 1.28 GiB | -89% | 0 / 5 → 5 |
| `11_social_network` | 0.00 → 0.00 | 7.9 MiB → 8.3 MiB | 131.75 → 124.87 | -5% | 1.41 GiB → 469.1 MiB | -68% | 0 / 20 → 20 |
| `12_build_dependency` | 0.00 → 0.00 | 10.5 MiB → 9.8 MiB | 5.40 → 0.64 | -88% | 9.91 GiB → 340.6 MiB | -97% | 0 / 5 → 5 |
| `13_network_reliability` | 0.00 → 0.00 | 5.8 MiB → 4.9 MiB | 2.88 → 2.88 | +0% | 76.3 MiB → 72.7 MiB | -5% | 0 / 5 → 5 |
| `14_routing_alternatives` | 0.02 → 0.02 | 10.6 MiB → 7.9 MiB | 40.09 → 40.18 | +0% | 5.96 GiB → 5.93 GiB | -1% | 0 / 45 → 45 |
| `15_task_assignment` | 0.01 → 0.01 | 9.9 MiB → 6.4 MiB | 3.37 → 3.41 | +1% | 438.2 MiB → 277.5 MiB | -37% | 0 / 5 → 5 |
| `16_centrality_analytics` | 0.02 → 0.01 | 8.3 MiB → 8.8 MiB | 8.82 → 9.04 | +2% | 305.6 MiB → 163.0 MiB | -47% | 0 / 10 → 10 |
| `17_transactional_log` | 0.36 → 0.41 | 156.6 MiB → 149.9 MiB | 7.74 → 8.45 | +9% | 2.67 GiB → 2.47 GiB | -7% | 0 / 55 → 50 |
| `18_oocore_pipeline` | 0.02 → 0.02 | 27.0 MiB → 22.2 MiB | 7.89 → 1.47 | -81% | 19.78 GiB → 792.7 MiB | -96% | 0 / 5 → 5 |
| `19_pattern_query` | 0.06 → 0.06 | 25.8 MiB → 32.9 MiB | 11.83 → 10.38 | -12% | 7.57 GiB → 3.63 GiB | -52% | 0 / 5 → 5 |
| `20_concurrent_reads` | 0.76 → 0.74 | 206.6 MiB → 180.0 MiB | 115.70 → 111.39 | -4% | 32.71 GiB → 31.96 GiB | -2% | 0 / 25 → 25 |
| `21_typed_recovery` | 0.08 → 0.09 | 23.9 MiB → 20.2 MiB | 39.56 → 41.90 | +6% | 4.04 GiB → 1.91 GiB | -53% | 0 / 225 → 215 |
| `22_cypher` | 0.01 → 0.01 | 16.8 MiB → 14.2 MiB | 12.96 → 4.78 | -63% | 16.09 GiB → 1.67 GiB | -90% | 0 / 10 → 5 |
| `23_bolt_server` | 0.12 → 0.12 | 36.4 MiB → 32.0 MiB | 3.23 → 3.07 | -5% | 844.7 MiB → 597.3 MiB | -29% | 0 / 5 → 5 |
| `24_init` | 0.00 → 0.00 | 8.2 MiB → 10.5 MiB | 0.00 → 0.00 | — | 6.1 MiB → 7.8 MiB | +28% | 0 / 5 → 5 |
| `24_seed` | 0.00 → 0.00 | 7.3 MiB → 9.3 MiB | 2.54 → 2.43 | -4% | 3.39 GiB → 3.42 GiB | +1% | 0 / 5 → 5 |
| `24_stats` | 0.00 → 0.00 | 7.4 MiB → 5.8 MiB | 8.45 → 5.69 | -33% | 4.10 GiB → 3.26 GiB | -20% | 0 / 10 → 5 |
| `24_query` | 0.00 → 0.00 | 9.0 MiB → 10.0 MiB | 5.28 → 4.40 | -17% | 3.29 GiB → 3.08 GiB | -7% | 0 / 5 → 5 |
| `24_plandiff` | 0.40 → 0.36 | 359.8 MiB → 306.9 MiB | 19.35 → 16.18 | -16% | 13.34 GiB → 10.89 GiB | -18% | 0 / 20 → 15 |
| `24_snapshot` | 0.11 → 0.11 | 129.9 MiB → 132.4 MiB | 4.88 → 4.98 | +2% | 3.69 GiB → 3.68 GiB | -0% | 0 / 5 → 5 |
| `25_software_house_api` | 0.65 → 0.64 | 58.8 MiB → 61.3 MiB | 28.67 → 25.79 | -10% | 11.32 GiB → 11.36 GiB | +0% | 0 / 35 → 30 |
| `26_social_scale_bench` | 84.52 → 73.97 | 47.56 GiB → 30.44 GiB | 85.19 → 73.99 | -13% | 47.60 GiB → 30.50 GiB | -36% | 0 / 90 → 80 |
| `27_concurrent_txn` | 1.42 → 1.32 | 6.00 GiB → 6.07 GiB | 6.40 → 6.13 | -4% | 1.11 GiB → 1.17 GiB | +6% | 0 / 5 → 5 |
| `28_negative_weights` | 0.00 → 0.00 | 4.5 MiB → 5.1 MiB | 9.69 → 9.55 | -1% | 1.31 GiB → 1.30 GiB | -1% | 0 / 15 → 15 |
| `29_all_pairs` | 0.02 → 0.02 | 9.9 MiB → 12.0 MiB | 2.90 → 2.86 | -1% | 156.2 MiB → 162.5 MiB | +4% | 0 / 5 → 5 |
| `30_min_spanning_tree` | 0.00 → 0.00 | 4.9 MiB → 4.8 MiB | 2.99 → 0.51 | -83% | 10.22 GiB → 522.6 MiB | -95% | 0 / 5 → 5 |
| `31_metrics_observability` | 0.07 → 0.03 | 55.7 MiB → 44.9 MiB | 23.18 → 1.33 | -94% | 9.70 GiB → 688.0 MiB | -93% | 1 / 25 → 5 |
| `32_euler` | 0.00 → 0.00 | 3.8 MiB → 4.2 MiB | 5.43 → 4.69 | -14% | 13.83 GiB → 6.73 GiB | -51% | 0 / 10 → 10 |
| `33_generation_swap` | 0.04 → 0.05 | 18.9 MiB → 12.2 MiB | 6.26 → 5.29 | -15% | 4.54 GiB → 1.23 GiB | -73% | 0 / 5 → 5 |
| `34_bolt_transactions` | 0.01 → 0.01 | 8.2 MiB → 13.2 MiB | 2.03 → 2.06 | +1% | 967.6 MiB → 957.2 MiB | -1% | 0 / 5 → 5 |
| `35_mvcc_mixed_workload` | 13.06 → 12.98 | 8.81 GiB → 9.15 GiB | 13.01 → 13.08 | +1% | 8.83 GiB → 9.14 GiB | +4% | 0 / 5 → 5 |
| `36_mvcc_snapshot_topology` | 0.12 → 0.09 | 42.6 MiB → 33.7 MiB | 968.33 → 849.35 | -12% | 48.02 GiB → 25.12 GiB | -48% | 1 / 245 → 246 |
| `37_mvcc_write_contention` | 0.41 → 0.44 | 39.6 MiB → 25.3 MiB | 2.25 → 2.52 | +12% | 203.4 MiB → 160.5 MiB | -21% | 0 / 5 → 5 |

Small deltas (under ~10%) are within the unmeasured run-to-run spread of a single-run sweep
and are not findings (Limits, item 2). `35`, `36` and `17` are fixed-duration or fixed-work
rows and are compared per unit:

| row | before | after | per unit |
|---|---|---|---|
| `35_mvcc_mixed_workload` | ≈ 1.70 M reads (4 × 0.7 s at 699 490 / 531 814 / 686 947 / 513 540 ops/s) | ≈ 1.67 M reads (684 143 / 511 209 / 671 163 / 513 933 ops/s) | 7.6 → 7.9 µs CPU and 5.6 → 5.8 KiB per read: +3–4%, inside the noise band; no change claimed |
| `36_mvcc_snapshot_topology` (writer) | `main.runWrite` 61.09 s CPU, `churn_deletes=25204` | 25.13 s, `churn_deletes=28841` | **2.42 → 0.87 ms CPU per churn delete (−64%)**; `runWrite` alloc 47 261 → 24 752 MiB |
| `17_transactional_log` | 530 checkpoints, 2.67 GiB | 492 checkpoints, 2.47 GiB | 5.2 MiB per checkpoint, unchanged; F6 is not yet fixed |

**Iteration-1 fixes, confirmed by the rerun.** F1 (#2882): `storeEntry` no longer appears
in the cross-row allocation top; the eleven F1 rows fell 70–97% in allocation (e.g. `18`
19.78 GiB → 792.7 MiB, `12` 9.91 GiB → 340.6 MiB). F3 (#2883): row 31's count-store writes
went from 20.03 s to 11.35 ms; row 36 `csrPairAndColumnCachedFor` went from 36.92 s to 0.49 s CPU
and from 30 958 to 671 MiB. F4 (#2890): row 26 `newAggregationEval.func1` went from 22 707 to
8 347 MiB, and 8 266 MiB of the remainder is node materialisation (`upgradeNodeIDToValue`,
rmp #2732), so the map-container share is gone. F5: row 26 `forEachResolvedSlotType` went from
1 857.5 to 26.4 MiB.

### Ranked findings at HEAD — biggest and simplest first

Figures are flat unless marked cumulative. "Removable" is an expectation, not a measurement:
no A/B was run.

#### G1 — a pattern predicate between two BOUND nodes calls `edgeMatchesRel` for every neighbour of the start node: 500.5 s CPU, 34.0% of the sweep

**Call site.** `cypher/pattern_eval.go:729–760` `matchOutgoing`: the loop over
`pe.g.EntryView(srcID).Neighbours` calls `pe.edgeMatchesRel(srcKey, dstKey, s.rel)` (`:740`)
**before** `pe.checkEndNode(s.node, dstID, row)` (`:743`), which is where the identity of a
bound end node is compared. Each `edgeMatchesRel` → `ReadView.EdgeLabels` →
`Graph.EdgeLabelsByIDAsOf` → `slotLabelsForPair` → `entrySlotLabels` →
`AdjList.EntrySlotLabelsAsOf` → `entryAsOfLoaded` re-resolves the **start node's whole entry**
by walking its version chain (`graph/adjlist/mvcc_adj.go:185–201`), then scans all of its
slots for the one pair (`graph/lpg/lpg.go:1823–1850`).

**Premise, verified at HEAD.** For row 36's
`MATCH (h:Hub {id: 0})-[r:LINK]->(s:Spoke) WHERE NOT (h)-[:LINK]->(s)`, both `h` and `s` are
bound when the predicate runs, so exactly one neighbour can pass `checkEndNode`. The other
deg(h) − 1 ≈ 4 000 are rejected only after the full labelled-pair lookup. The cost per
predicate is therefore deg(h) × (chain walk + deg(h) slot scan), and the cost per query is
spokes² × chain. The chain is long because the query's own snapshot stays pinned while the
writer churns the hub (28 841 deletes and re-adds in 240 s), so every version committed
since the query began is walked again for every candidate. This is module waste, not example
waste: the example's O(spokes²) comment (`examples/36_mvcc_snapshot_topology/main.go:207`)
assumes one hub walk per row, but the module does deg(h) of them.

**Evidence.** `$M/out/elevated/36_mvcc_snapshot_topology/cpu.pprof`, `-list matchOutgoing`:
line 740 **499.69 s**, line 743 0.17 s, the loop's `mapper.Resolve` 0.06 s. Beneath it:
`entryAsOfLoaded` 484.01 s cumulative, `mvcc.Visible` 289.88 s, `supersededAt` 135.32 s.
The whole reader path (`main.observeWith`) is 502.14 s of 849.35 s. The query never
completed once in 240 s (`reader.contradiction_checks=0`), which is why row 36 fails. The
same shape was 605.5 s at `127c012b`.

**Estimated removable.** ≈ 500 s in row 36: all but one `edgeMatchesRel` per predicate,
≈ 1 − 1/4 000 of 500.5 s. The query would then complete, which changes the row's own mix of
work; the per-predicate cost falls by a factor of ≈ deg(h).

**Fix sketch.** (a) When the step's end node binds a variable present in the row, resolve
that id once and test only the pair (src, bound): one `edgeMatchesRel`, then
`checkNodePattern` and `matchSteps` on that id. Equivalently, move the cheap bound-identity
comparison ahead of `edgeMatchesRel`. Both filters are free of side effects, so the result of
the conjunction is unchanged. (b) Optional next step: resolve each node's as-of entry once per
statement. The snapshot is fixed for the statement, so the resolved entry is immutable, and
this removes the per-row chain walk. (c) Neo4j's Expand(Into) starts the search for connecting
relationships from the endpoint with the lesser degree, and caches degrees per query
(neo4j/neo4j `54a7dcf7c2501b31866199143364c5332da8936f`,
`community/cypher/runtime-util/src/main/java/org/neo4j/internal/kernel/api/helpers/CachingExpandInto.java`,
`connectingRelationships` and `expandFromNodeWithLesserDegree`). An O(min-degree) check here
would need a versioned reverse lookup, because `adjlist.revIndex` is not versioned. Studied for
inspiration only.

**Related, code-read only.** `matchIncoming` (`cypher/pattern_eval.go:762–795`) walks
**every node's** adjacency (`mapper.Walk`) on each evaluation, O(V + E), even when the far
node is bound. No sweep row reaches it, so its cost is unmeasured.

**Risk.** Low for (a): a pure reorder of side-effect-free filters, with bound-variable
semantics unchanged; validate with the full TCK (pattern predicates are TCK-covered) and DST
multi-session. Medium for (b): the memo must be keyed on the statement's snapshot and must not
serve a stale answer to a write view with pending writes of its own.

#### G2 — the full-BFS sweep count of `DiameterCtx`: 113.4 s flat in row 11 (F2, still open)

**Call site.** `search/diameter.go:220` (`bfsFarthest` per level vertex), micro items at
`:222–226` and `:265–267`.

**Premise, re-verified at HEAD.** Unchanged since iteration 1. The eccentricity is re-derived
by scanning the whole `dist` slice after each sweep, although `bfsFarthest` returns the
farthest vertex, and `dist` is reset in O(V) per sweep.

**Evidence.** `$M/out/elevated/11_social_network/cpu.pprof`: `bfsFarthest` 113.38 s flat,
120.57 s cumulative; `levelMaxEccentricity.func1` 3.16 s flat, all on line 222 (3.37 s
cumulative); the reset at line 266 1.62 s.

**Estimated removable.** Micro, certain and bit-identical: ≈ 5.0 s (3.37 + 1.62). Macro,
hypothesised: most of the remaining ≈ 113 s, with bound-driven pruning (Takes and Kosters,
CIKM 2011). This needs a spike; the iteration-1 midpoint-root hypothesis was refuted at −33%.

**Risk.** Micro: none. Macro: `(lo, hi, exact)` must be proved correct against a brute-force
oracle.

#### G3 — residual live-adjacency cost of write transactions in row 36: 10.83 s CPU, ≈ 8.2 GiB

**Call sites.** `cypher/live_adjacency.go:469` `LiveOutRun` → `liveTopoLog.resolveLocked`
(`run.dsts = append(run.dsts[:0], …)` at `:331`, `run.handles` at `:333`, `run.codes = make`
at `:342`, `resolveSourceSlotTypes` at `:357`); `liveTopoLog.beforeAdjWrite` (`:221–223`,
three whole-run copies per write); `Graph.EdgeLabelsByHandleIDAsOf`
(`graph/lpg/edge_handle_durable.go:266`, a fresh `[]string` per slot); `csr.OrderRuns`.

**Premise, verified at HEAD.** `LiveOutRun` receives `dsts/handles/codes` from its caller and
`resolveLocked` appends into them, yet the profile shows the appends allocating: the buffers
arrive without capacity for the hub's run of ≈ 4 000 slots. `beforeAdjWrite` copies the hub's
full last run on every write to it (pre-image capture). For each slot, the type resolver
allocates a `[]string` only for `encodeSlotTypes` to look each name up in the registry again.

**Evidence.** `$M/out/elevated/36_mvcc_snapshot_topology/{cpu,heap}.pprof`: `LiveOutRun`
10.83 s and 5 857 MiB cumulative (`resolveLocked` 2.20 GiB flat: 906 + 891 + 458 MiB on the
three lines); `resolveSourceSlotTypes` 7.22 s; `beforeAdjWrite` 2 294 MiB flat (919 + 903 +
455 MiB); `EdgeLabelsByHandleIDAsOf` 6.24 s, 1 851 MiB; `OrderRuns` 2.64 s, 1 869 MiB.
Iteration 1 recorded `EdgeLabelsByHandleIDAsOf` at 20.74 s / 8 104 MiB and `OrderRuns` at
3.19 s / 2 428 MiB, inside the whole-graph build that #2883 removed.

**Estimated removable.** ≈ 2.2 GiB by reusing run buffers across `LiveOutRun` calls;
≈ 1.8 GiB and part of 6.2 s by resolving label ids to codes directly, with no `[]string`;
≈ 1.9 GiB and 2.6 s by skipping `OrderRuns` when the run is already ordered (whether it is was
not verified). The `beforeAdjWrite` pre-images are required for rollback atomicity; copying
once per transaction instead of once per write is **hypothesised**. Total ≈ 6 GiB and ≈ 7 s,
hypothesised.

**Risk.** Medium: this is the #2883 visibility path and the `47334e3b`/`787e674a` rollback
path. Validate with the full TCK, DST multi-session and the isolation checkers.

#### G4 — deleting and re-adding on a high-degree entry reallocates the whole entry at double capacity: 6.24 GiB in row 36

**Call site.** `graph/adjlist/adjlist.go:1475` `compactEntry` builds each column with
`make(…, len-1)` (capacity = length). The next append to that entry therefore misses the
in-place fast path and takes the slow path at `:1084–1124`, which allocates
`growCap(oldLen) = 2 × oldLen` for every column.

**Premise, verified at HEAD.** The slow path's comment says it "happens only O(log d) times
for a degree-d hub". That holds for append-only growth. Under delete/re-add churn, every
re-add after a compaction takes the slow path and allocates twice the entry's size.

**Evidence.** `$M/out/elevated/36_mvcc_snapshot_topology/heap.pprof`, `-list
upsertEdgeSlotLocked`: 6.24 GiB flat, of which slow-path lines 1087 (`newNb`) 1.76 GiB, 1094
(`newW`) 1.77 GiB, 1115 (`newL`) 922 MiB, plus `newH`; `compactEntry` 3 162 MiB. This is
consistent with 28 841 churn cycles × ≈ 8 000 slots × 28 B ≈ 6.4 GB. Across rows,
`upsertEdgeSlotLocked` is now the largest allocation site (19 113 MiB). Outside row 36 its
dominant line is the fast path's per-append entry header (e.g. `09` line 1078, 2.04 GiB),
which is the MVCC publish unit and is not claimed here.

**Estimated removable.** ≈ 6 GiB in row 36, if the compacted entry keeps its capacity class
(or one slot of headroom) so that the re-add appends in place. The compaction's own copy is
required, because readers of the old version must see it frozen.

**Fix sketch.** Allocate the columns of `compactEntry` with `cap(current.neighbours)` (or
`len-1 + headroom`). Retained memory rises by at most one capacity step per compacted entry;
this trade must be measured on RSS, not assumed.

**Risk.** Low to medium: an in-place append is already sound for older versions, because they
hold a shorter length. Validate with the adjlist race tests, the crash battery and DST.

#### G5 — `db.propertyKeys()` scans every edge-property slot per call: 6.06 s in row 25 (F7, still open)

**Call site.** `graph/lpg/introspect.go:141` `PropertyKeysInUse`, loop at `:166–181`.
Re-verified at HEAD: O(V + E) per call, with no memo.
`$M/out/elevated/25_software_house_api/cpu.pprof`: 6.06 s cumulative (23.5% of the row);
`RelationshipTypesInUse` 0.66 s. Removable ≈ 6 s. Fix and risk as in iteration 1
(MVCC-correct per-key counts or a per-generation memo; medium risk).

#### G6 — the physical plan is rebuilt on every execution of a cached point query: 1.78 s and 5.39 GiB in row 35 (F9, rmp #2391, still open)

`cypher/api.go:2698`/`:2750` `buildReadPhysical` runs per execution.
`$M/out/elevated/35_mvcc_mixed_workload`: 1.78 s cumulative (13.6% of 13.08 s),
**5 522 MiB cumulative (56.3% of the row)**, `copySchema` 1 242 MiB, `buildIRProjection`
2 090 MiB cumulative. Unchanged from iteration 1 (1.77 s, 5 311 MiB). Removable up to
≈ 1.7 s and ≈ 5 GiB; medium-high risk (a reused plan must not carry per-execution state).

#### G7 — `AllNodesScan.Init` materialises every node id per execution: 4.31 GiB in row 25 (F8, still open)

`cypher/exec/scan_all.go:62–85`. `$M/out/elevated/25_software_house_api/heap.pprof`:
`AllNodesScan.Init` 4 416 MiB cumulative (36.2%), CPU 0.15 s, driven by `handleStats` →
`countOne` (9 271 MiB cumulative, 76.0%). Removable ≈ 4 GiB; medium risk (a streaming scan
under concurrent writes, or a count-store plan, which is a planner change and TCK-observable).

#### G8 — four 1 MiB `bufio` writers per checkpoint: 1.93 GiB of row 17's 2.47 GiB (F6, still open)

`store/snapshot/{labels.go:228,properties.go:238,mapper.go:149,writer.go:141}` (and
`mapper.go:289`). Re-verified at HEAD. `bufio.NewWriterSize` 1 977 MiB flat (74.4%) over 492
checkpoints = 4.02 MiB per checkpoint of a 395.86 KiB snapshot. Removable ≈ 1.9 GiB, all of
it allocation; low risk (the byte stream is unchanged; covered by the crash battery).

### Items examined and not ranked

- **Row 26 `buildRelTypeColumn.func1`, 0.90 → 1.86 s.** The data does not support calling this
  a regression: the parent `buildRelTypeColumn` is 3.96 → 3.92 s cumulative. `fa6fc459` moved
  the closure body into `encodeSlotTypes` (the same `reg.Lookup` per name, read at `127c012b`
  and at HEAD), which now carries 1.77 s flat, while F5 removed work elsewhere in
  `forEachResolvedSlotType`. Whether the extraction cost ≈ 1 s (for example, through lost
  inlining) is **unattributed**; a single-run delta of this size needs an interleaved
  benchmark.
- **Row 36 `vacuumLoop`**, 58.49 → 36.54 s cumulative (`severChain` 9.99 s flat). This is the
  MVCC reclaimer; its premise was not examined in this task.
- **Harness-owned, unchanged:** `14` `main.kNearest` (sort) ≈ 35 s; `20`
  `pageRankBuildReverseStructure` 13.78 GiB and `main.topKByRank` 1.42 GiB (per-worker
  PageRank, rmp #2382); `32` `math/rand.Perm` 6.86 GiB, 4.40 s.
- **Node materialisation** (`nodePropsToExprMap.func1` 8 697 MiB across rows; `26`
  5 622 MiB) belongs to rmp #2732.
- **Inherent, unchanged:** `21` fsync-bound commits (`syscall.rawsyscalln` 31.0 s), `03`
  Brandes work (`brandesSource` 42.2 s), WAL replay per `24_*` subcommand
  (`replayWALInto` ≈ 1.15 GiB each).
- **`runtime.madvise`**, 69.4 s across rows (`36` 30.7 s, `26` 11.8 s): the scavenger returning
  memory, a consequence of allocation volume. A profile cannot attribute it to one site.

### Materiality

Denominators: elevated CPU 1 472.4 s in total, **1 240.8 s net of runtime parking**;
allocation 175.87 GiB.

- **Largest removable item: G1, ≈ 500 s = 40.3% of net CPU (34.0% of total).** It is
  material by any threshold, and it is a single, low-risk change.
- **Sum of the top five by CPU** (G1 500 + G3 ≈ 7 + G5 6.1 + G2 micro 5.0 + G6 1.8) ≈
  **520 s = 41.9% of net CPU**. Without G1, the other four sum to ≈ 20 s = **1.6% of net
  CPU**, and each is below 1% on its own. G2's macro item (≈ 113 s, 9.1%, hypothesised) is the
  only other CPU item above that level, and it needs an algorithm spike.
- **Allocation:** the five largest removable items (G4 6.2 + G3 ≈ 6 + G6 5.4 + G7 4.3 + G8
  1.9 GiB) ≈ **23.8 GiB = 13.5% of 175.87 GiB**; the largest single item is 3.5%.
- **Verdict.** After G1, the remaining module CPU waste on the example surface is
  immaterial (< 2% combined), except for the diameter algorithm. Allocation waste is
  moderate (≈ 13.5% combined, no item above 3.5%).

### Recommended next batches

1. **G1** alone, in `cypher/pattern_eval.go`: a short-circuit on the bound end node in
   `matchOutgoing` (and the same guard for `matchIncoming`), with a regression test on a
   hub-and-spokes predicate. Validation: `go test ./cypher/...`, the full TCK, DST
   multi-session, then a rerun of row 36 elevated.
2. **G3 + G4**, the row-36 write path (`cypher/live_adjacency.go`, `graph/adjlist`): run-buffer
   reuse, direct label-id encoding, and `compactEntry` capacity. Validation as in each risk
   line.
3. **G2 micro + G8**: small, independent and bit-identical, so they can ship together.
4. **G5, G6, G7**: each needs a design decision from the user (MVCC-correct introspection
   counts, plan-pure separation under rmp #2391, a count-store or streaming scan).

### Limits of iteration 2

The limits of the first sweep still apply: one run per pass, no noise floor, sampled
allocation, inferred GC attribution. In addition, G1's estimate assumes the bound-end guard
fires on every evaluation, which the code shows but no A/B has measured, and G3's `OrderRuns`
saving assumes the runs are already ordered, which was not verified.

### Reproduction (iteration 2)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph        # the script builds from the CWD
M="$L/../lab-2893"
scripts/examples-lab.sh "$M/out" default
scripts/examples-lab.sh "$M/out" elevated
go tool pprof -nodefraction=0 -list 'patternEvaluator..matchOutgoing' "$M/out/elevated/36_mvcc_snapshot_topology/cpu.pprof"
go tool pprof -nodefraction=0 -sample_index=alloc_space -list upsertEdgeSlotLocked "$M/out/elevated/36_mvcc_snapshot_topology/heap.pprof"
python3 "$M/agg.py" "$M/reconcile" cpu 0 45     # cross-row flat totals (cpu|alloc)
bash "$M/cmp.sh"                                 # before/after figures per finding
```
