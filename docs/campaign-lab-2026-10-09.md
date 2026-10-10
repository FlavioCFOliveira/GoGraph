# Laboratory iteration 1, 2026-10-09 — every example profiled at `c794a1ed`

Sprint 367, rmp #3077 (SPIKE). Every program under `examples/` was profiled at HEAD
`c794a1ed` (sprint 366 merged: directed multigraph storage only). **No module code was
changed.** Every number below was collected in this session. Findings rank module-owned waste
only; code in an example's `package main` is reported separately and not ranked.

## 1. Method

- **Driver.** `scripts/examples-lab.sh`, unchanged, three passes:
  `default` (every row at its default scale; CPU, heap, mutex, block and goroutine profiles,
  `-contention 1`), `elevated` (every row at its elevated scale; CPU and heap only, the
  CPU-attribution pass) and `contention` (the 18 concurrent rows at the elevated scale with
  `-contention 1`). Binaries were built once, outside every timed window (35 s).
- **Peak RSS.** A fourth pass re-ran every elevated row without a profiler under
  `/usr/bin/time -l` and read `maximum resident set size`. Row 25 (an HTTP server driven by
  the driver) was not re-run: its peak RSS is not measured. The heap profiles hold
  cumulative and end-of-run heap only, so peak heap is reported as peak RSS.
- **Ownership.** A purpose-built reader (`$L/owner/main.go`, google/pprof `profile`
  package) charges every sample to its **leaf-most owned frame**: module (any
  `github.com/FlavioCFOliveira/GoGraph/…` function outside `examples/`) or harness
  (`main.*`, `examples/…`). Runtime and standard-library frames are charged to the owned
  frame that called them, so `mallocgc` under a module function is module cost. Samples with
  no owned frame on the stack are **unowned** (scheduler, parking, GC background workers,
  scavenger). Totals are read from the raw profile, never from pprof's text (no node
  dropping). Allocation is `alloc_space`, binary units (MiB/GiB).
- **Attribution.** Premises were re-read in the source at HEAD. A finding is "established"
  only where code flow or an experiment proves it; otherwise "hypothesised".
- **Same-code repeat (noise).** The 15 concurrent rows above 2 s ran twice at the same
  scale (`elevated` and `contention`). Per-row CPU differed by −5.1% to +5.1%; the
  aggregate by −0.3% (461.46 s → 460.05 s). A delta under ±5% on one row is not a finding.

**Environment.** Apple M4, 10 cores, 32 GiB, Darwin 27.0.0 arm64, Go 1.27.2,
`GOMAXPROCS` unset, `GOGRAPH_FSYNC_LATENCY=off`, tree clean at `c794a1ed`. Graph data
(`TMPDIR`, example-24 and example-25 data directories) on the RAM drive
`/Volumes/GoGraphRAM/lab3077` through symlinks; `GOCACHE` on the RAM drive (user
exception for this campaign); binaries, profiles and logs on disk.

**Host load.** Before the sweep: load1 1.81, top process `osascript` 27% of one core.
`default` 19:12–19:17 (load1 1.64–4.03); `elevated` 19:18–19:26 (load1 2.14–8.65; the peaks
follow rows 11 and 20, which saturate the cores themselves); `contention` 19:33–19:38
(load1 2.48–6.90). The peak-RSS pass ran 19:27–19:33 while my own static-analysis runs
(staticcheck, golangci-lint) were active (load1 up to 6.20); RSS does not depend on load.
Every row ran alone within its pass.

**Commands.** `$L` = `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab1`.

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
# data dirs -> RAM drive (profiles stay on disk)
R=/Volumes/GoGraphRAM/lab3077
for p in default elevated contention; do
  mkdir -p $R/$p/data-24 $R/$p/data-25 $L/out/$p/25_software_house_api
  ln -sfn $R/$p/data-24 $L/out/$p/data-24
  ln -sfn $R/$p/data-25 $L/out/$p/25_software_house_api/data
done
mkdir -p $R/default/tmp; ln -sfn $R/default/tmp $L/out/tmp
export GOCACHE=$R/gocache GOGRAPH_FSYNC_LATENCY=off
scripts/examples-lab.sh $L/out default
scripts/examples-lab.sh $L/out elevated
scripts/examples-lab.sh $L/out contention
$L/rss.sh                                             # peak RSS -> $L/rss/summary.tsv
$L/owner/owner $L/out/elevated/*/{cpu,heap}.pprof > $L/owner.elevated.tsv
$L/owner/owner $L/out/default/*/*.pprof           > $L/owner.default.tsv
$L/owner/owner $L/out/contention/*/*.pprof        > $L/owner.contention.tsv
python3 $L/agg.py $L/owner.elevated.tsv cpu summary          # per-row module/harness/unowned
python3 $L/agg.py $L/owner.elevated.tsv alloc_space line 40  # owner file:line ranking
python3 $L/agg.py $L/owner.elevated.tsv cpu fncum 3000       # cumulative per module function
python3 $L/agg.py $L/owner.contention.tsv delay line 30      # contention by owner line
python3 $L/manifest.py $L                                     # the table in section 2
(cd $L/symprobe && go run .)                                  # symmetric-CSR identity probe (V2)
```

## 2. Manifest

Columns 4–8 come from the `elevated` pass (CPU and allocation from the owner reader,
peak RSS from the RSS pass); the two delay columns from the `default` pass (the elevated
contention figures are in section 4). "exit" is read from inside each run's log.

**Totals (`elevated`, 42 rows).** CPU **595.56 s**: module **383.88 s** (64.5%), harness
54.57 s, unowned 157.11 s (scheduler and parking 126.1 s under `runtime.systemstack` /
`runtime.mcall`, GC background marking 14.68 s, `morestack` 13.10 s). Allocation
**160.96 GiB**: module **144.47 GiB** (89.8%), harness 16.31 GiB.

**Exit status.** Every `default` row exited 0. The driver marks 11 `default` rows FAIL
for `cpu-total-zero`: each ran under 10 ms of CPU, below one profiler sample; they are
not failures of the example. Two `elevated` rows exited 1 on a harness self-check, with
both profiles written and used:
- `31_metrics_observability`: the planner-statistics step found no misestimate to score at
  `-services 80000` ("the screen now closes this route too and [staleTier] needs a new
  one", rmp #2795/#2785).
- `37_mvcc_write_contention`: `ladder.L22 crash_image_after_checkpoint: image taken false`.
  The same argv passed in the `contention` pass, so the check is intermittent at this scale.

| row | exit default / elevated | elevated scale | CPU s | module CPU s | alloc GiB | module alloc GiB | peak RSS MiB | mutex delay s (default) | block delay s (default) |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| 01_basic | 0 / 0 | `-nodes 150000` | 1.29 | 0.30 | 1.06 | 0.97 | 474 | 0.000 | 0.000 |
| 02_property_graph | 0 / 0 | `-persons 300000` | 1.56 | 0.43 | 0.72 | 0.71 | 308 | 0.000 | 0.009 |
| 03_advanced_algorithms | 0 / 0 | `-communities 60 -nodes 800` | 45.66 | 45.11 | 0.06 | 0.05 | 33 | 0.000 | 0.000 |
| 04_persistence | 0 / 0 | `-packages 40000` | 3.82 | 2.19 | 1.92 | 1.91 | 292 | 0.000 | 0.040 |
| 05_out_of_core | 0 / 0 | `-nodes 300000` | 0.40 | 0.11 | 0.30 | 0.28 | 177 | 0.000 | 0.000 |
| 06_csv_import | 0 / 0 | `-nodes 200000` | 1.69 | 1.17 | 1.42 | 1.09 | 593 | 0.000 | 0.000 |
| 07_graphml_roundtrip | 0 / 0 | `-nodes 150000` | 2.12 | 1.39 | 1.85 | 1.82 | 592 | 0.000 | 0.000 |
| 08_pagerank | 0 / 0 | `-pages 400000` | 5.97 | 2.29 | 0.68 | 0.62 | 300 | 0.000 | 0.000 |
| 09_leiden | 0 / 0 | `-communities 80 -community-size 400` | 2.96 | 1.31 | 2.21 | 2.20 | 864 | 0.000 | 0.000 |
| 10_dimacs9_routing | 0 / 0 | `-vertices 300000 -edges 1500000` | 2.74 | 2.32 | 1.38 | 1.37 | 244 | 0.000 | 0.000 |
| 11_social_network | 0 / 0 | `-users 100000` | 129.78 | 129.15 | 0.47 | 0.45 | 218 | 0.000 | 0.001 |
| 12_build_dependency | 0 / 0 | `-modules 400000` | 0.63 | 0.15 | 0.35 | 0.30 | 251 | 0.000 | 0.000 |
| 13_network_reliability | 0 / 0 | `-clusters 40 -cluster-size 40` | 2.90 | 2.35 | 0.07 | 0.05 | 73 | 0.000 | 0.000 |
| 14_routing_alternatives | 0 / 0 | `-nodes 20000` | 39.96 | 0.54 | 6.12 | 0.05 | 38 | 0.000 | 0.000 |
| 15_task_assignment | 0 / 0 | `-workers 2500 -tasks 2500` | 3.26 | 2.80 | 0.30 | 0.20 | 211 | 0.000 | 0.000 |
| 16_centrality_analytics | 0 / 0 | `-communities 20 -nodes 250` | 9.07 | 8.95 | 0.10 | 0.10 | 29 | 0.000 | 0.000 |
| 17_transactional_log | 0 / 0 | `-accounts 3000 -transfers 9000` | 0.53 | 0.40 | 0.50 | 0.49 | 25 | 0.000 | 0.120 |
| 18_oocore_pipeline | 0 / 0 | `-nodes 300000` | 1.47 | 0.34 | 0.82 | 0.66 | 344 | 0.000 | 0.000 |
| 19_pattern_query | 0 / 0 | `-nodes 200000` | 9.73 | 3.73 | 3.81 | 3.76 | 314 | 0.000 | 0.024 |
| 20_concurrent_reads | 0 / 0 | `-nodes 60000 -iterations 200` | 114.13 | 61.37 | 33.66 | 32.29 | 145 | 0.038 | 1.371 |
| 21_typed_recovery | 0 / 0 | `-nodes 50000` | 11.28 | 8.43 | 2.12 | 2.09 | 372 | 0.000 | 0.038 |
| 22_cypher | 0 / 0 | `-users 150000` | 4.43 | 2.32 | 1.74 | 1.70 | 555 | 0.000 | 0.006 |
| 23_bolt_server | 0 / 0 | `-nodes 30000 -queries 60000` | 2.97 | 0.86 | 0.63 | 0.48 | 116 | 0.001 | 0.234 |
| 24_init | 0 / 0 | `init` | 0.00 | 0.00 | 0.01 | 0.01 | 11 | 0.000 | 0.000 |
| 24_seed | 0 / 0 | `seed -users 150000 -friends 10` | 3.29 | 2.23 | 4.38 | 4.35 | 2122 | 0.000 | 0.000 |
| 24_stats | 0 / 0 | `stats` | 5.60 | 3.31 | 3.87 | 3.87 | 1460 | 0.000 | 0.000 |
| 24_query | 0 / 0 | `query (25-row FRIEND aggregate)` | 4.35 | 2.37 | 3.79 | 3.78 | 1958 | 0.000 | 0.000 |
| 24_plandiff | 0 / 0 | `plandiff` | 14.84 | 8.74 | 8.84 | 8.83 | 1472 | 0.000 | 0.006 |
| 24_snapshot | 0 / 0 | `snapshot` | 4.93 | 2.66 | 4.46 | 4.45 | 1454 | 0.000 | 0.050 |
| 25_software_house_api | 0 / 0 | `-scale-components 15000 -scale-tasks 12000 -scale-developers 800` | 24.23 | 11.29 | 9.55 | 9.52 | n/m | 0.004 | 24.791 |
| 26_social_scale_bench | 0 / 0 | `(default)` | 77.18 | 49.43 | 32.69 | 32.66 | 3469 | 0.213 | 14.910 |
| 27_concurrent_txn | 0 / 0 | `-accounts 20000 -ops-per-writer 200` | 0.86 | 0.39 | 0.56 | 0.52 | 54 | 0.041 | 0.412 |
| 28_negative_weights | 0 / 0 | `-layers 80 -width 160` | 9.51 | 9.32 | 1.37 | 1.36 | 965 | 0.000 | 0.000 |
| 29_all_pairs | 0 / 0 | `-nodes 1600` | 2.91 | 2.04 | 0.16 | 0.15 | 129 | 0.000 | 0.002 |
| 30_min_spanning_tree | 0 / 0 | `-regions 300 -sites 900` | 0.44 | 0.16 | 0.42 | 0.31 | 220 | 0.000 | 0.000 |
| 31_metrics_observability | 0 / 1 | `-services 80000` | 1.08 | 0.25 | 0.71 | 0.65 | 273 (exit 1) | 0.001 | 0.021 |
| 32_euler | 0 / 0 | `-nodes 300000 -loops 3000` | 4.67 | 0.10 | 7.04 | 0.29 | 7228 | 0.000 | 0.000 |
| 33_generation_swap | 0 / 0 | `-versions 80 -reads-per-reader 2000 -base-nodes 20000` | 5.49 | 5.11 | 1.03 | 1.02 | 173 | 0.003 | 0.009 |
| 34_bolt_transactions | 0 / 0 | `-persons 400000` | 1.15 | 0.28 | 0.63 | 0.59 | 270 | 0.000 | 0.019 |
| 35_mvcc_mixed_workload | 0 / 0 | `(default)` | 13.06 | 2.77 | 9.25 | 8.63 | 43 | 0.614 | 4.273 |
| 36_mvcc_snapshot_topology | 0 / 0 | `-spokes 4000` | 6.13 | 1.75 | 2.85 | 2.84 | 48 | 0.016 | 0.030 |
| 37_mvcc_write_contention | 0 / 1 | `-customers 20000 -ops-per-producer 2000` | 17.49 | 3.67 | 7.08 | 6.98 | 87 (exit 1) | 15.285 | 31.032 |

### Failure classification

| pass | row | driver verdict | class | evidence |
|---|---|---|---|---|
| default | 03, 09, 12, 13, 24_init, 24_seed, 24_stats, 24_query, 28, 30, 32 (11 rows) | FAIL `cpu-total-zero` | **instrument, not a defect** | exit 0 inside each `run.log`; under 10 ms CPU, below one 100 Hz sample |
| elevated | 24_init | FAIL `cpu-total-zero` | instrument, not a defect | exit 0; `init` on an empty directory |
| elevated | 31_metrics_observability | FAIL exit 1 | **harness-owned** | the example's planner-statistics scenario found no misestimate at `-services 80000`; the module behaved as designed (estimate demoted) |
| elevated | 37_mvcc_write_contention | FAIL exit 1 | **harness-owned, intermittent; not attributed** | `ladder.L22 crash_image_after_checkpoint: image taken false`; the same argv passed in `contention`. Whether the image was skipped by the harness's timing or by the store was not established |

No row showed a module defect.

## 3. Ranked findings — module-owned, biggest × simplest first

Denominators: module CPU 383.88 s, module allocation 144.47 GiB (`elevated`). Costs are
cumulative unless marked flat. "Gain" is an estimate: no A/B was run (no module change was
allowed in this task).

| # | site | measured cost (profile, row) | path | fix hypothesis | risk / gates | est. gain |
|---|---|---|---|---|---|---|
| **R1** | `search/centrality/pagerank.go:208` and `:429` (`pageRankBuildReverseStructure`, `:589`) on a CSR built by `graph/csr/csr.go:725` `BuildSymmetric` | **13.96 GiB** alloc, 3.24 s CPU owner-flat; row 20 13.77 GiB = 41% of the row (`elevated/20_concurrent_reads/heap.pprof`) | csr.BuildSymmetric → centrality.PageRank (parallel path) | the transpose of a `BuildSymmetric` result is **bit-identical** to its input (proof V2). Stamp a `symmetric` flag in `BuildSymmetric` and alias `revVerts/revEdges = verts/edges` | low; `search/` only, no TCK, no crash battery; PageRank determinism tests | ≈ 13.9 GiB, ≈ 3.2 s; **established** (identity proven) |
| **R2** | `cypher/api.go:16675` `ets := g.EdgeLabels(stKey, enKey)` → `graph/lpg/edge_labels.go:35` → `graph/lpg/lpg.go:2187` `slotLabelsForPair` (O(deg(src)) scan); with `:16726` `EdgeLabelsByHandle` and `:16733` `edgeInstanceIdxFor` | `buildRelationshipValueFromRow` **13.71 s** = 17.8% of row 26; line 16675 6.81 s, 985 MiB; `:16726` 1.33 s; `:16733` 1.29 s (`elevated/26_social_scale_bench/cpu.pprof`, `-list buildRelationshipValueFromRow`) | Expand → Project → relationship value → lpg per-pair label union | (a) compute `EdgeLabels` only when `!handled` — bit-identical, removes the discarded call for Cypher-created edges (V1); (b) read the type from the slot at the CSR position Expand already resolved (`edgeIDVal`), O(1) instead of O(deg) | (a) low; (b) medium: TCK multigraph scenarios Match2 [6], Match7 [29], MatchWhere1 [11] | (a) unmeasured share (row 26 builds edges through the Go API, where the union is needed); (b) ≈ 6.8 s **hypothesised** |
| **R3** | `search/diameter.go:222–226` ecc rescan, `:266` O(V) reset, `:281` per-neighbour farthest test | line 222 **3.56 s**, 266 1.74 s, 281 4.46 s, flat (row 11, `-list bfsFarthest`) | search.DiameterCtx (iFUB levels) | `ecc = distV[farthest]` (bfsFarthest already tracks the maximum); reset only the vertices in `queue`; track the max distance in a local | none for 222/266 (bit-identical); 281 low | ≈ 5.3 s certain; up to ≈ 4 s more hypothesised |
| **R4** | `search/diameter.go` `bfsFarthest` macro: number of eccentricity BFS sweeps | **125.28 s** flat = 32.6% of module CPU (row 11 only) | DiameterCtx | prior campaign's G2/H1, still open; an algorithm spike with an exactness proof | high (correctness of the bound) | up to ≈ 120 s, **hypothesised** |
| **R5** | `graph/lpg/introspect.go:156` `PropertyKeysInUse`, via `cypher/procs` `dbPropertyKeys` | **6.08 s** = 25% of row 25; 1.80 GiB | `/schema` → `CALL db.propertyKeys()` → full O(V + E) scan per call | per-key usage counts or a per-generation memo (prior H2, still open) | medium; MVCC visibility of the counts | ≈ 6 s |
| **R6** | `cypher/api.go:2743` → `:2797` `buildReadPhysical` on every execution of a cached read | **6.35 GiB**, 1.70 s; row 35 6.11 GiB = 66% of the row; `copySchema` `:9357` 1.06 GiB | plan cache → physical build per run | plan-pure separation (rmp #2391, prior H5) | medium-high; TCK | ≈ 6 GiB, ≈ 1.6 s |
| **R7** | `cypher/exec/scan_all.go:77` `AllNodesScan.Init` materialises every node id | **4.88 GiB**, 1.54 s; row 25 4.40 GiB = 46% of the row | `MATCH (t:Task) RETURN count(t)` → AllNodesScan | streaming scan under the pinned snapshot, or a count-store plan (prior H6) | medium; TCK-observable plan | ≈ 4.8 GiB |
| **R8** | `cypher/exec/constraints.go:1206–1207` `reserveSetPropertyGen` takes the registry-global `r.mu.Lock()` on every property write once **any** UNIQUE / NOT NULL constraint exists, constrained (label, key) or not | contention pass, row 37: **1.79 s mutex + 1.85 s block** | SET → constraint reservation → global mutex | test under `RLock` whether any (label, key) of the write is constrained; take the write lock only then | medium: Consistency/Isolation of constraint enforcement; ACID crash battery not touched | contention only; share of unconstrained writes **unmeasured** |
| **R9** | `store/snapshot/{labels.go:228, properties.go:239, mapper.go:153, mapper.go:293, writer.go:141}` 1 MiB `bufio` writers per checkpoint component | ≈ 360 MiB = 70% of row 17's 0.50 GiB | checkpoint → snapshot writer | size to the component or pool one buffer per checkpoint (prior H7) | low; crash battery (byte stream unchanged) | ≈ 0.35 GiB |

Prior-campaign item H4 (escaped row headers in `Apply`/`Expand`) is **closed at HEAD**:
`5de6174b`; `Apply.Next` now allocates 11.5 MiB flat across the sweep (3.8 GiB before).

## 4. Contention

Elevated `contention` pass, delay summed over goroutines (block includes idle waits).
Module 274.86 s, harness 62.75 s, unowned 60.75 s; mutex delay exists only in row 37
(15.57 s).

| owner site | delay | row | reading |
|---|---|---|---|
| `search/centrality/pagerank.go:748` worker parked on `start[w]`; `:806` coordinator on `done` | 151.04 s + 30.70 s block | 20 | each concurrent reader's one-shot `PageRank` starts `GOMAXPROCS` = 10 workers and a per-iteration channel barrier. Row 20 also spends 43.6 s unowned CPU in `pthread_cond_signal` 14.05 s, `usleep` 9.56 s, `pthread_cond_wait` 9.28 s. **Hypothesis, not established:** nested parallelism oversubscribes the cores when callers are already concurrent; sizing workers to idle Ps (or the stateful `PageRanker`) would remove it |
| `graph/lpg/property.go:391` / `:358` / `:407` `setNodePropertyInfo` node-property shard lock; `graph/lpg/snapshot_read.go:217` reader `RLock` | 8.38 s mutex; 5.41 s + 4.32 s block | 37 | 256 shards (`lpg.go:283`). Row 37 writes a deliberate hot set of shared inventory nodes (README), so same-node contention is by design. Whether any of the delay is shard collision between different nodes is **not established** |
| `cypher/exec/constraints.go:1206–1207` | 1.79 s mutex + 1.85 s block | 37 | R8 |
| `graph/lpg/mvcc_index.go:944` `appendSuspects` | 1.92 s block | 37 | index-suspect list lock under concurrent writers; not analysed further |
| `store/wal/writer.go:1197` `syncToLocked`, `store/txn/txn.go:2227` `waitApplyTurn`, `graph/mvcc/gate.go:222` | 1.43 s, 1.32 s, 1.20 s block | 37 | group commit and ordered apply; inherent to durable commits |
| `graph/lpg/mvcc_vacuum.go:717` `vacuumLoop` | 18.06 s block | 26, 22, 34 | the background vacuum parked on its timer `select`: idle, not contention |
| `search/diameter.go:234` `wg.Wait` | 18.33 s block | 11 | fork-join wait for the eccentricity workers: idle |

## 5. Vacuous and redundant work, with proof

- **V1 — `cypher/api.go:16675` computed then discarded.** `ets := g.EdgeLabels(stKey,
  enKey)` runs unconditionally. Between 16675 and 16726 nothing reads `ets`; when
  `EdgeLabelsByHandle` returns entries (every Cypher-created edge, which always records its
  type by handle — comment at 16711–16724) `ets` is overwritten at 16727, and when the
  positional override fires it is overwritten at 16736. Proof: code flow. Cost of the call:
  6.81 s in row 26; the share spent on Cypher-created edges was not measured.
- **V2 — PageRank transposes a symmetric CSR into a copy of itself.** Probe
  `$L/symprobe/main.go` builds 300 random directed multigraphs (2–61 nodes, up to 6n edges,
  10% self-loops, parallel and reciprocal arcs: 25 092 arcs), applies `BuildSymmetric`, then
  a verbatim copy of `pageRankBuildReverseStructure` and `CSR.BuildReverse`:
  `symmetric_mismatches=0`, `buildreverse_of_symmetric_mismatches=0` (vertices, edges,
  handles and weights), while the directed control differs in 295/300 trials. Every
  analytic example builds its input with `BuildSymmetric` (e.g.
  `examples/20_concurrent_reads/main.go:235`), so the transpose (R1) is a redundant copy.
- **V3 — `search/bibfs.go:63` `IsSymmetric()` per call.** It builds an E-sized hash set
  (`graph/csr/csr.go:810`) only to choose between `c` and `c.BuildReverse()`, which V2 shows
  are bit-identical when `c` is symmetric. Cost: 53.36 MiB of `BiBFSCtx`'s 57.88 MiB and
  0.13 s of its 0.13 s in row 22. Small, but 92% of the function's allocation.

## 6. Post-refactor residue (sprint 366: `fec95062`, `bed4de53`, `5c59f9aa`)

Located with `git show --stat` on the three commits and `git diff` of the touched files,
then confirmed against the profiles.

**Residue with cost.**
- **P1 — a CSR does not record that it is symmetric** (R1, V2, V3). After the refactor the
  only undirected input is the `BuildSymmetric` projection, yet `PageRank` re-derives a
  transpose (13.96 GiB, 3.24 s) and `BiBFS` re-derives symmetry with a hash set (53 MiB,
  0.13 s) on every call. One flag set by `BuildSymmetric` removes both.
- **P2 — undirected analytics build the directed CSR first, then copy it.** Every
  analytic example calls `csr.BuildFromAdjList(a).BuildSymmetric()`; the intermediate
  directed CSR is discarded at once. Measured intermediate allocation in rows that
  symmetrise immediately: 09 130.7 MiB, 33 56.8 MiB, 30 20.6 MiB, 22 16.8 MiB, 15 14.8 MiB,
  32 14.4 MiB, 11 5.8 MiB, 20 5.7 MiB (≈ 0.27 GiB, 0.2% of module allocation); CPU under
  0.2 s. A one-pass `BuildSymmetricFromAdjList` would remove it. Low value.
- **P3 — `examples/13_network_reliability`** builds the same symmetric projection of
  `net.adj` twice (`main.go:258`, `connectivity.go:40`). Harness-owned; cost below one
  sample.

**Checked and clean.** No `Directed()` / `Multigraph()` accessor or mode flag survives in
production code (`git grep`, `.claude/worktrees/` excluded). The remaining `undirected`
identifiers are Cypher pattern semantics (`cypher/ir/plan.go:2097`,
`cypher/exec/merge_relationship.go:107`), undirected analytics over the symmetric
projection (`search/hierholzer_undirected.go`, `search/bibfs.go`), or legacy-store upgrade
(`store/snapshot/legacy_undirected.go`, `store/recovery/legacy_fold.go`,
`store/snapshot/manifest.go:294`). The legacy paths cost nothing measurable
(`fncum` under 0.01 s in every row). The direction-resolution helpers
(`cypher/api.go:16544` `relStoredInverted`, `cypher/exec/set.go:360`
`relStorageDirection`) still serve reverse and undirected **pattern** hops and cost
0.02 s in total. `golangci-lint run --enable-only unused` over `graph/ cypher/ store/
search/ internal/` reports no unused code. `staticcheck -checks U1000` could not run: the
installed staticcheck cannot read Go 1.27 export data ("export data version 5 is greater
than maximum supported version 4").

### Removal candidates

| path | kind | why obsolete | proof | gates touched |
|---|---|---|---|---|
| `bench/memprobe/edge_test.go:99` `TestProbe_EdgesSimple` | test (memory probe, `//go:build threeway`) | measured the simple-graph counterfactual; simple-graph storage was removed | `buildEdges` (`edge_test.go:40`) never reads its `multigraph` parameter (only use is the signature, `grep -n multigraph`), so this probe builds exactly the graph of `TestProbe_EdgesMultigraph` (`:89`) | none (no TCK, no crash battery); KG Test node |
| `bench/memprobe/edge_test.go:40` `buildEdges` parameter `multigraph` | test helper | as above | no read of `multigraph` in the body | none |
| `bench/memprobe/cypher_test.go:138` `TestProbe_CypherEdgesSimpleGraph` | test (memory probe, `threeway`) | measured Cypher edges "with parallel edges disallowed" | `newCypherEngineMode(false)` (`:42`) ignores its argument and builds `adjlist.Config{}`; `cypherSeedNodesOn` (`:63`) only forwards to `cypherSeedNodes`; the body is otherwise identical to `TestProbe_CypherEdges` (`:96`) | none; KG Test node |
| `bench/memprobe/cypher_test.go:42` `newCypherEngineMode` parameter, `:63` `cypherSeedNodesOn` | test helpers | serve only the probe above | `cypherSeedNodesOn` has 2 occurrences: definition and the probe above | none |
| `bolt/server/e2e_shape_roundtrip_test.go:73` field `undirected` and the comments at `:127–131`, `:184–185` | test | described undirected storage ("the adjlist stores two directed arcs per logical edge") | the field is set in the table (`:80–92`) and never read (`grep -n '\.undirected'`: no hit); the comments contradict directed-only storage | none; KG Test node |

### Examined, not listed

- `search/bibfs.go:19` `ErrNotUndirected`: exported, no reference anywhere (`git grep`), but
  it was retired by an earlier `BiBFS` change, not by sprint 366; removal is an API change.
- `internal/shapegen/classic.go:181` `Cycle(n, directed bool)`: with `directed=false` it
  builds the same arcs as `directed=true` and differs only in its size check (n ≥ 3); the
  parameter still states a property of the shape, so it is not proven obsolete.
  `Complete(n, false)` builds a different graph (i < j only) and is in use.
- Handle-0 fallbacks (`cypher/api.go:16544` `relStoredInverted`, `edgeInstanceIdxFor`): every new slot gets a
  handle (`graph/adjlist/adjlist.go:793–798`, rmp #2317, before sprint 366); whether a legacy
  store can still load a handle-0 slot was not traced, so unreachability is not proven.
- Test files named for undirected or reciprocal cases (`cypher/rollback_undirected_edges_test.go`,
  `cypher/set_undirected_reciprocal_test.go`, `cypher/match_undirected_dedup_test.go`, …):
  they assert undirected **pattern** semantics over directed storage, which remain required.

## 7. Items examined and not ranked

- **Harness-owned** (not ranked): `14` `main.kNearest` 35.75 s and 6.07 GiB; `20`
  `main.topKByRank` 9.12 s, 1.36 GiB; `32` `main.generateCycles` 4.41 s, 6.72 GiB (row 32's
  peak RSS 7.06 GiB is the harness's); `35` `main.measure.func3` 0.62 GiB.
- **Row 20 search pools** (`search/search.go:164` BFS frontier 4.86 GiB,
  `search/dijkstra.go:505/479/480` 4.39 GiB, `:463/465` result copies 3.67 GiB): the
  example calls one-shot APIs per read (rmp #2382); the pool-miss mechanism is not
  attributed.
- **Node materialisation** `cypher/api.go:15047/15045` `nodePropsToExprMap` 8.71 GiB
  (rows 26, 24_plandiff, 36): rmp #2732.
- **Copy-on-write adjacency entries** `graph/adjlist/adjlist.go:971` 2.66 GiB (26) and
  1.16 GiB (09): one immutable entry per versioned append, by design.
- **WAL replay per `24_*` subcommand** (`store/recovery/recovery.go:2207` 1.12 GiB each,
  peak RSS 1.46–1.96 GiB): the CLI reopens the store per process; inherent.
- **`store/txn/txn.go:1378` `SetEdgeLabel`** 2.28 GiB in `24_seed`: the redo log of one
  bulk transaction, grown by doubling.
- **Inherent algorithms**: `03` `brandesSource` 43.84 s, `20` PageRank `runRange` 32.19 s,
  `21` WAL group sync 7.36 s (real commits), `28` Johnson 8.46 s.
- **Unowned CPU** 157.11 s (scheduler, parking, `madvise` 38.91 s, GC 14.68 s): cannot be
  charged to one site by a profile.

## 8. Limits

- No A/B: every gain above is an estimate. Only V1 and V2 are established (code flow,
  probe); R2(b), R4, R8 and the row-20 oversubscription are hypotheses.
- Each pass ran once; the noise band is the ±5% same-code repeat of section 1.
- Allocation is sampled at the default `MemProfileRate`.
- Peak heap is not measured; peak RSS stands in. Row 25's peak RSS is not measured.
- The share of V1's discarded `EdgeLabels` calls, the shard-collision share of row 37's
  property-lock contention, and the unconstrained-write share of R8 are not measured.
- `staticcheck` U1000 did not run (tool too old for Go 1.27); `golangci-lint unused`
  covered `graph/ cypher/ store/ search/ internal/` only, not `bolt/`, `bench/` or `examples/`.
- Row 37's L22 failure was not attributed.

## 9. Reproduction

Commands are in section 1. Artefacts under `$L`: manifests
`out/{default,elevated,contention}/manifest.tsv`, per-row profiles and logs
`out/<pass>/<row>/`, owner tables `owner.{default,elevated,contention}.tsv`, peak RSS
`rss/summary.tsv` and `rss/<row>.log`, owner reader `owner/main.go`, aggregation `agg.py`,
table builder `manifest.py`, symmetry probe `symprobe/main.go`, unused-code report
`unused.txt`.

```bash
go tool pprof -nodefraction=0 -list 'cypher.buildRelationshipValueFromRow$' $L/out/elevated/26_social_scale_bench/cpu.pprof
go tool pprof -nodefraction=0 -list 'search.bfsFarthest|search.levelMaxEccentricity.func1' $L/out/elevated/11_social_network/cpu.pprof
go tool pprof -nodefraction=0 -sample_index=alloc_space -list pageRankBuildReverseStructure $L/out/elevated/20_concurrent_reads/heap.pprof
go tool pprof -nodefraction=0 -peek 'PropertyKeysInUse$' $L/out/elevated/25_software_house_api/cpu.pprof
go tool pprof -nodefraction=0 -list reserveSetPropertyGen $L/out/contention/37_mvcc_write_contention/mutex.pprof
```

---

# Laboratory iteration 2, 2026-10-10 — the same sweep at `6b399a3b`

Sprint 367, rmp #3087. The iteration-1 sweep was re-run at HEAD `6b399a3b` with the same
driver, passes, examples, scales and owner reader, so every row compares 1:1 with section 2.
**No module code was changed.** Every number below was collected in this session.

## 10. Method and environment

- Same as section 1, with these differences: binaries rebuilt (`LAB_REBUILD=1`, 37 binaries,
  25 s, outside every timed window); `GOCACHE` on its default on-disk location; graph data
  under `/Volumes/GoGraphRAM/lab3087` (deleted at the end). `$L2` =
  `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab2`.
- Host: Apple M4, 10 cores, Darwin 27.0.0 arm64, Go 1.27.2, tree clean at `6b399a3b`,
  `GOGRAPH_FSYNC_LATENCY=off`. Load1: start 2.80 (top process `osascript`/terminal, under
  20% of one core each); `default` 14:42–14:48 (2.25–4.03); `elevated` 14:48–14:57
  (2.48–3.95); peak RSS 14:57–15:02; `contention` 15:02–15:07 (up to 4.36). Every row ran
  alone within its pass.
- **Same-code repeat (noise).** The 15 concurrent rows above 2 s ran twice at HEAD
  (`elevated` and `contention`): per-row CPU −3.0% to +3.2%, aggregate +1.5%, except row 37
  (+39.9%, see below). With iteration 1's −5.1% to +5.1%, a per-row delta under **±5%** is
  not a finding.
- **Row 37 is not comparable from the `elevated` pass.** Its `elevated` run failed on the
  same intermittent harness check as in iteration 1 (`ladder.L22 level=8
  crash_image_after_checkpoint: image taken false`) and its CPU differs by 40% from the
  passing `contention` run of the same binary. Row 37 is compared on the `contention` pass:
  17.07 s → 16.41 s (−3.9%, within noise).
- ¹ The peak-RSS script's argv rebuild broke `24_query` this time (exit 2: the query text
  was passed twice; `${a% $Q}` treats `[:FRIEND]` as a glob bracket). The row was re-run by
  hand with the exact query (`$L2/rss/24_query.rerun.log`, exit 0).

## 11. Manifest before (`c794a1ed`) → after (`6b399a3b`)

**Totals (`elevated`, 42 rows).** CPU **595.56 s → 443.90 s (−25.5%)**; module
**383.88 s → 249.06 s (−35.1%)**; harness 54.57 → 56.03 s; unowned 157.11 → 138.81 s.
Allocation **160.96 GiB → 135.67 GiB (−15.7%)**; module **144.47 GiB → 119.13 GiB
(−17.5%)**. Contention pass: CPU 463.57 → 317.65 s; delay 398.37 → 365.22 s (module
274.86 → 263.05 s). Row 11 alone accounts for 125.92 s of the 151.66 s CPU drop.

Exit status is unchanged: every `default` row exited 0 (10 driver `cpu-total-zero` FAILs,
instrument, as in section 2); `elevated` 31 and 37 exit 1 on the same harness checks as
iteration 1; `24_init` `cpu-total-zero`. No row showed a module defect.

| row | exit d/e before → after | CPU s | module CPU s | alloc GiB | module alloc GiB | peak RSS MiB |
|---|---|---|---|---|---|---|
| 01_basic | 0/0 → 0/0 | 1.29 → 1.28 (-1%) | 0.30 → 0.34 (+13%) | 1.06 → 1.04 (-2%) | 0.97 → 0.95 (-2%) | 474 → 487 |
| 02_property_graph | 0/0 → 0/0 | 1.56 → 1.56 (+0%) | 0.43 → 0.47 (+9%) | 0.72 → 0.72 (-0%) | 0.71 → 0.71 (-0%) | 308 → 297 |
| 03_advanced_algorithms | 0/0 → 0/0 | 45.66 → 49.35 (+8%) | 45.11 → 49.04 (+9%) | 0.06 → 0.05 (-9%) | 0.05 → 0.04 (-13%) | 33 → 30 |
| 04_persistence | 0/0 → 0/0 | 3.82 → 3.67 (-4%) | 2.19 → 2.07 (-5%) | 1.92 → 1.86 (-3%) | 1.91 → 1.85 (-3%) | 292 → 268 |
| 05_out_of_core | 0/0 → 0/0 | 0.40 → 0.37 (-8%) | 0.11 → 0.11 (+0%) | 0.30 → 0.26 (-12%) | 0.28 → 0.25 (-13%) | 177 → 181 |
| 06_csv_import | 0/0 → 0/0 | 1.69 → 1.65 (-2%) | 1.17 → 1.12 (-4%) | 1.42 → 1.47 (+4%) | 1.09 → 1.13 (+4%) | 593 → 592 |
| 07_graphml_roundtrip | 0/0 → 0/0 | 2.12 → 2.26 (+7%) | 1.39 → 1.44 (+4%) | 1.85 → 1.85 (+0%) | 1.82 → 1.83 (+0%) | 592 → 591 |
| 08_pagerank | 0/0 → 0/0 | 5.97 → 5.97 (+0%) | 2.29 → 2.34 (+2%) | 0.68 → 0.69 (+2%) | 0.62 → 0.64 (+3%) | 300 → 302 |
| 09_leiden | 0/0 → 0/0 | 2.96 → 2.89 (-2%) | 1.31 → 1.26 (-4%) | 2.21 → 2.15 (-2%) | 2.20 → 2.15 (-2%) | 864 → 746 |
| 10_dimacs9_routing | 0/0 → 0/0 | 2.74 → 2.69 (-2%) | 2.32 → 2.26 (-3%) | 1.38 → 1.36 (-1%) | 1.37 → 1.35 (-1%) | 244 → 363 |
| 11_social_network | 0/0 → 0/0 | 129.78 → 3.86 (-97%) | 129.15 → 2.08 (-98%) | 0.47 → 0.33 (-29%) | 0.45 → 0.31 (-30%) | 218 → 181 |
| 12_build_dependency | 0/0 → 0/0 | 0.63 → 0.65 (+3%) | 0.15 → 0.16 (+7%) | 0.35 → 0.33 (-3%) | 0.30 → 0.30 (-2%) | 251 → 250 |
| 13_network_reliability | 0/0 → 0/0 | 2.90 → 2.81 (-3%) | 2.35 → 2.20 (-6%) | 0.07 → 0.07 (+3%) | 0.05 → 0.05 (+6%) | 73 → 73 |
| 14_routing_alternatives | 0/0 → 0/0 | 39.96 → 39.38 (-1%) | 0.54 → 0.46 (-15%) | 6.12 → 6.22 (+2%) | 0.05 → 0.04 | 38 → 37 |
| 15_task_assignment | 0/0 → 0/0 | 3.26 → 3.30 (+1%) | 2.80 → 2.81 (+0%) | 0.30 → 0.30 (+3%) | 0.20 → 0.21 (+5%) | 211 → 211 |
| 16_centrality_analytics | 0/0 → 0/0 | 9.07 → 9.15 (+1%) | 8.95 → 9.00 (+1%) | 0.10 → 0.09 (-10%) | 0.10 → 0.09 (-11%) | 29 → 29 |
| 17_transactional_log | 0/0 → 0/0 | 0.53 → 0.51 (-4%) | 0.40 → 0.41 (+2%) | 0.50 → 0.16 (-68%) | 0.49 → 0.16 (-68%) | 25 → 20 |
| 18_oocore_pipeline | 0/0 → 0/0 | 1.47 → 1.50 (+2%) | 0.34 → 0.48 (+41%) | 0.82 → 0.86 (+5%) | 0.66 → 0.71 (+8%) | 344 → 335 |
| 19_pattern_query | 0/0 → 0/0 | 9.73 → 9.77 (+0%) | 3.73 → 3.44 (-8%) | 3.81 → 3.94 (+3%) | 3.76 → 3.89 (+3%) | 314 → 320 |
| 20_concurrent_reads | 0/0 → 0/0 | 114.13 → 100.64 (-12%) | 61.37 → 57.14 (-7%) | 33.66 → 17.81 (-47%) | 32.29 → 16.45 (-49%) | 145 → 101 |
| 21_typed_recovery | 0/0 → 0/0 | 11.28 → 11.29 (+0%) | 8.43 → 8.30 (-2%) | 2.12 → 2.09 (-1%) | 2.09 → 2.06 (-1%) | 372 → 332 |
| 22_cypher | 0/0 → 0/0 | 4.43 → 4.29 (-3%) | 2.32 → 2.14 (-8%) | 1.74 → 1.65 (-5%) | 1.70 → 1.61 (-5%) | 555 → 547 |
| 23_bolt_server | 0/0 → 0/0 | 2.97 → 2.84 (-4%) | 0.86 → 0.54 (-37%) | 0.63 → 0.68 (+8%) | 0.48 → 0.50 (+4%) | 116 → 118 |
| 24_init | 0/0 → 0/0 | 0.00 → 0.00 | 0.00 → 0.00 | 0.01 → 0.00 | 0.01 → 0.00 | 11 → 9 |
| 24_seed | 0/0 → 0/0 | 3.29 → 3.44 (+5%) | 2.23 → 2.23 (+0%) | 4.38 → 4.30 (-2%) | 4.35 → 4.28 (-2%) | 2122 → 1778 |
| 24_stats | 0/0 → 0/0 | 5.60 → 5.84 (+4%) | 3.31 → 3.62 (+9%) | 3.87 → 3.98 (+3%) | 3.87 → 3.98 (+3%) | 1460 → 1396 |
| 24_query | 0/0 → 0/0 | 4.35 → 4.44 (+2%) | 2.37 → 2.00 (-16%) | 3.79 → 3.73 (-2%) | 3.78 → 3.72 (-2%) | 1958 → 2013 ¹ |
| 24_plandiff | 0/0 → 0/0 | 14.84 → 15.14 (+2%) | 8.74 → 9.99 (+14%) | 8.84 → 8.82 (-0%) | 8.83 → 8.81 (-0%) | 1472 → 1436 |
| 24_snapshot | 0/0 → 0/0 | 4.93 → 5.11 (+4%) | 2.66 → 2.46 (-8%) | 4.46 → 4.43 (-1%) | 4.45 → 4.43 (-1%) | 1454 → 1392 |
| 25_software_house_api | 0/0 → 0/0 | 24.23 → 19.25 (-21%) | 11.29 → 5.44 (-52%) | 9.55 → 3.48 (-64%) | 9.52 → 3.45 (-64%) | n/m → n/m |
| 26_social_scale_bench | 0/0 → 0/0 | 77.18 → 72.55 (-6%) | 49.43 → 50.12 (+1%) | 32.69 → 32.51 (-1%) | 32.66 → 32.49 (-1%) | 3469 → 3558 |
| 27_concurrent_txn | 0/0 → 0/0 | 0.86 → 0.86 (+0%) | 0.39 → 0.30 (-23%) | 0.56 → 0.56 (+0%) | 0.52 → 0.52 (+0%) | 54 → 56 |
| 28_negative_weights | 0/0 → 0/0 | 9.51 → 9.40 (-1%) | 9.32 → 9.07 (-3%) | 1.37 → 1.36 (-0%) | 1.36 → 1.36 (+0%) | 965 → 965 |
| 29_all_pairs | 0/0 → 0/0 | 2.91 → 2.89 (-1%) | 2.04 → 2.04 (+0%) | 0.16 → 0.16 (-3%) | 0.15 → 0.15 (-2%) | 129 → 129 |
| 30_min_spanning_tree | 0/0 → 0/0 | 0.44 → 0.44 (+0%) | 0.16 → 0.15 (-6%) | 0.42 → 0.38 (-10%) | 0.31 → 0.28 (-9%) | 220 → 213 |
| 31_metrics_observability | 0/1 → 0/1 | 1.08 → 1.18 (+9%) | 0.25 → 0.31 (+24%) | 0.71 → 0.71 (-0%) | 0.65 → 0.64 (-1%) | 273 (exit 1) → 271 (exit 1) |
| 32_euler | 0/0 → 0/0 | 4.67 → 4.69 (+0%) | 0.10 → 0.04 (-60%) | 7.04 → 7.04 (-0%) | 0.29 → 0.27 (-7%) | 7228 → 7228 |
| 33_generation_swap | 0/0 → 0/0 | 5.49 → 5.07 (-8%) | 5.11 → 4.68 (-8%) | 1.03 → 0.94 (-9%) | 1.02 → 0.93 (-9%) | 173 → 183 |
| 34_bolt_transactions | 0/0 → 0/0 | 1.15 → 1.20 (+4%) | 0.28 → 0.31 (+11%) | 0.63 → 0.65 (+4%) | 0.59 → 0.61 (+3%) | 270 → 295 |
| 35_mvcc_mixed_workload | 0/0 → 0/0 | 13.06 → 13.08 (+0%) | 2.77 → 2.80 (+1%) | 9.25 → 9.22 (-0%) | 8.63 → 8.62 (-0%) | 43 → 43 |
| 36_mvcc_snapshot_topology | 0/0 → 0/0 | 6.13 → 5.91 (-4%) | 1.75 → 1.59 (-9%) | 2.85 → 2.73 (-4%) | 2.84 → 2.73 (-4%) | 48 → 47 |
| 37_mvcc_write_contention | 0/1 → 0/1 | 17.49 → 11.73 (-33%) | 3.67 → 2.30 (-37%) | 7.08 → 4.65 (-34%) | 6.98 → 4.59 (-34%) | 87 (exit 1) → 104 |

Peak RSS is one run per row with no repeat; a single-row RSS delta (for example row 10
244 → 363 MiB with unchanged allocation) is not attributed.

## 12. Verification of the fixes

| commit / task | claim | measured at HEAD | verdict |
|---|---|---|---|
| `cfd64290` #3078 symmetric mark | PageRank and BiBFS stop transposing a symmetric CSR | `pageRankBuildReverseStructure` row 20 **13 765 MiB, 3.23 s → 0** (only row 08's directed input remains, 184.58 MiB); `PageRankCtx` row 20 17.45 → 3.60 GiB; row 20 alloc 33.66 → 17.81 GiB (−47%), CPU 114.13 → 100.64 s (−12%), peak RSS 145 → 101 MiB; `IsSymmetric` 53.36 MiB / 0.13 s → 0, `BiBFSCtx` 57.88 → 5.10 MiB | **moved as claimed** |
| `e7b12a60` #3078 one-pass `BuildSymmetricFromAdjList` | −32% bytes, +27% build CPU | rows 09 + 33: 536 → 366 MiB (**−32%**); CPU 0.37 → 0.33 s (two to four profiler samples per site: the +27% is below this sweep's resolution) | **bytes moved as claimed**; CPU not resolvable here |
| `4be967d2` #3079, `6b399a3b` #3085 diameter | row 11 diameter 125.6 s → 1.5 s | `bfsFarthest` **125.28 s → 1.80 s**; row 11 CPU 129.78 → 3.86 s (−97%), alloc 0.47 → 0.33 GiB | **moved as claimed** |
| `4d831e8e` #3080 per-pair union only when needed | removes the discarded `EdgeLabels` call | `buildRelationshipValueFromRow` row 26 **13.71 → 14.99 s**; every row-26 relationship takes the `!handled` fallback (`api.go:16797` `EdgeLabelsByID` 6.83 s, `slotLabelsForPair` 6.18 s) | **did not move on the only vehicle**: row 26's edges have no by-handle record, so the union is needed there (as section 3 R2(a) predicted). The saving on Cypher-created edges is not exercised by any example at a measurable scale |
| `1a8e25ae` #3081 key-only `PropertyKeysInUse`, streamed `AllNodesScan` | row 25 scan and introspection | `PropertyKeysInUse` **6.08 s / 1.80 GiB → 0.54 s / under 0.5 MiB**; `AllNodesScan.Init` row 25 **4 401 → 21 MiB**; row 25 module CPU 11.29 → 5.44 s, alloc 9.52 → 3.45 GiB (−64%) | **moved as claimed** |
| `546efbe8` #3082 constraint lock skipped for unconstrained keys | less registry contention in row 37 | `reserveSetPropertyGen` (`constraints.go:1269–1270`) **1.79 s mutex + 1.85 s block → 1.11 s + 1.10 s (−39%)**; row 37 mutex total 15.57 → 15.70 s; node-property shard lock (`property.go:391`) 7.44 → 7.73 s | **moved locally**; row 37's total mutex delay did not fall (the writers now queue on the next lock) |
| `e71407c1` #3083 snapshot buffers sized | ≈ 0.35 GiB off row 17 | row 17 alloc **0.50 → 0.16 GiB (−0.34 GiB, −68%)**; `writeMapperN` 112.7 → 20.9 MiB, `writeAndSync` 86.7 → 13.6 MiB | **moved as claimed** |
| `fc0f6807` #3086 PageRank worker budget | opt-in; not wired into examples | row 20 `pagerank.go` worker park + coordinator 151.04 + 30.70 s → **154.81 + 30.82 s** block | **unchanged, as expected** (no caller uses the budget) |

## 13. Residual module-owned costs at HEAD

**Significance threshold.** A residual is **worth a task** when it costs at least **1% of
module CPU (2.49 s)** or **1% of module allocation (1.19 GiB)** across the `elevated`
sweep, or at least **1% of module contention delay (2.63 s)** in the `contention` pass at a
non-idle site, and the cost is not inherent to the algorithm or the workload. Below all
three it is **insignificant**. Costs are cumulative unless marked flat.

| # | site | cost (row) | hypothesis | risk | verdict |
|---|---|---|---|---|---|
| **Q1** | row 20 one-shot `PageRank` from concurrent readers: `search/centrality/pagerank.go:866` worker park, `:930` barrier | **185.6 s block**; row 20 unowned CPU 33.98 s (contention pass) | nested parallelism: each concurrent caller starts `GOMAXPROCS` workers. The #3086 budget exists but no caller passes it. **Hypothesised** | low (caller-side wiring); examples only, or a default | **worth a task** |
| **Q2** | `cypher/api.go:16797` → `graph/lpg/lpg.go:2205` `slotLabelsForPair` O(deg(src)) scan per relationship value | **6.83 s** (loop line 4.39 s flat), 0.91 GiB (row 26) | the row carries the edge's stable handle (`fwdHandle = edgeIDVal`, `api.go:16719`); a handle → slot lookup would make the type read O(1) for edges with no by-handle record. **Hypothesised** | medium: TCK multigraph scenarios (Match2 [6], Match7 [29], MatchWhere1 [11]) | **worth a task** |
| **Q3** | `graph/lpg` `GetEdgePropertyAsOf` per-pair scan (`:177`) + string-key re-lookup (`:154`, `:158`) | **5.78 s** (row 26) | same O(deg) scan as Q2 for edge properties; caller already holds the ids. **Hypothesised** | medium (same TCK set) | **worth a task** (join with Q2) |
| **Q4** | cypher ↔ lpg string-key round trips: `edgeInstanceIdxFor` → `EdgeCreateCount` `Mapper.Lookup` 1.16 s; `GetEdgePropertyAsOf` Lookup 0.73 s; `api.go:16695–16696` `Mapper.Resolve` 1.38 s | **≈ 3.3 s** (row 26) | the caller holds `stID/enID`; ID-keyed variants remove the Lookups (as #3080 did for labels). **Established by profile + code flow** | low | **worth a task** (join with Q2/Q3) |
| **Q5** | `edgeInstanceIdxFor` (`api.go:17174`) receives the stable handle as `edgePos`, a CSR position | 1.51 s in row 26, all returning at `:17177` (`totalCreates == 0`) | the positional fallback compares a handle against CSR offsets `[start, end)`; when `totalCreates > 0` and a handle falls in range it counts the wrong instance. **Correctness hypothesis, not demonstrated** | correctness; TCK | **worth a task** (correctness first, then cost) |
| **Q6** | `cypher.nodePropsToExprMap` (`api.go:15090`) | **10.73 GiB**, 3.43 s (26, 24_plandiff, 36) | node materialisation; rmp #2732 | medium | **worth a task** (exists: #2732) |
| **Q7** | R6 `cypher/api.go:2822` `buildReadPhysical` per cached execution | **6.18 GiB**, 1.84 s (row 35 6.02 GiB = 65% of the row) | plan-pure separation; rmp #2391 | medium-high; TCK | **worth a task** (exists: #2391) |
| **Q8** | `graph/lpg/edge_property_column.go:315` `GrowSlotWithValue` | **4.95 GiB** (4.35 GiB flat), 0.33 s (row 26 load) | copy-on-write of the whole edge-property column block per appended slot, O(deg) per append. **Hypothesised** | medium: MVCC snapshot immutability | **worth a task** |
| **Q9** | node-property shard lock `graph/lpg/property.go:391/358`, reader `snapshot_read.go:217` | **7.73 s mutex + 5.23 s + 5.15 s block** (row 37) | row 37's deliberate hot set; the share that is shard collision between different nodes is **not measured** | medium; Isolation | **worth a task** only as a measurement (collision share) |
| Q10 | `cypher/exec/scan_all.go:149` `AllNodesScan.Next` boxes each id | 1.02 GiB (0.85%), 1.08 s (25, 19) | `IntegerValue` stored in an interface escapes per row | low | insignificant |
| Q11 | `cypher/exec/constraints.go:1269–1270` residual registry lock | 1.11 s mutex + 1.10 s block (row 37) | constrained writes must serialise | — | insignificant |
| Q12 | `graph/lpg/mvcc_index.go:944` `appendSuspects` | 1.81 s block, 0.42 s CPU, 83 MiB (row 37) | shard `RLock` walk under concurrent writers | low | insignificant |
| Q13 | `graph/lpg/edge_handle_durable.go:376` `EdgeLabelsByHandleIDAsOf` exclusive `sync.Mutex` for a read | 0.13 s mutex + 0.12 s block (row 36), 0.89 s CPU | an `RWMutex` read lock; cost is below every threshold | low | insignificant |
| Q14 | `cypher/exec/constraint_journal.go:162`, `:284` rollback record and `copyLabels` | 9 MiB (row 37) | allocated per reservation | — | insignificant |
| Q15 | `search/diameter.go:197–210` batch pool wake-ups: `pthread_cond_signal` via `goready` | 1.18 s unowned CPU = 31% of row 11; 2.26 s idle block at `:203` | channel handoff per sweep wakes a parked worker; sweep-wide 0.27% | low | insignificant (row-local only) |

Re-confirmed and unchanged from section 7 (inherent or already tracked): WAL replay per
`24_*` process (`store/recovery/recovery.go:2207`, 4.58 GiB over five rows), row 20 search
pools (#2382), `adjlist.go:971` copy-on-write entries (2.72 GiB), `txn.go:1378` bulk redo
log (2.55 GiB, 24_seed), Brandes (51.04 s), PageRank `runRange` (32.44 s), WAL group sync
in row 21 (7.23 s).

**Cross-component reading.** The largest residual chain is row 26's relationship
materialisation: Expand produces ids and a handle, `cypher` resolves the ids to string keys,
then `graph/lpg` re-looks the keys up and scans the source's whole adjacency for the type
(Q2) and again for each property (Q3), and `edgeInstanceIdxFor` scans once more with a
mismatched position (Q4, Q5). `buildRelationshipValueFromRow` alone costs 14.99 s, 30% of row 26's 50.12 s module CPU.

## 14. Limits (iteration 2)

- No A/B: residual gains are estimates. Q4 is established by profile and code flow; Q1–Q3,
  Q5, Q8 and Q9 are hypotheses.
- One run per pass; the noise band is ±5% per row. Row 37 compares on the `contention`
  pass only.
- The +27% build-CPU claim of `e7b12a60` is below this sweep's sampling resolution.
- The #3080 saving on Cypher-created edges is not exercised by any example.
- Row 25's peak RSS remains unmeasured.

## 15. Reproduction (iteration 2)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
$L2/sweep.sh            # default (LAB_REBUILD=1), elevated, rss.sh, contention
(cd $L2/owner && ./owner $L2/out/elevated/*/{cpu,heap}.pprof > $L2/owner.elevated.tsv \
  && ./owner $L2/out/default/*/*.pprof > $L2/owner.default.tsv \
  && ./owner $L2/out/contention/*/*.pprof > $L2/owner.contention.tsv)
python3 $L2/compare.py <scratchpad>          # section 11 table (lab1 vs lab2)
python3 $L2/agg.py $L2/owner.elevated.tsv cpu fncum 9000
python3 $L2/agg.py $L2/owner.elevated.tsv alloc_space line 40
python3 $L2/agg.py $L2/owner.contention.tsv delay line 30
go tool pprof -nodefraction=0 -list 'cypher.buildRelationshipValueFromRow$' $L2/out/elevated/26_social_scale_bench/cpu.pprof
go tool pprof -nodefraction=0 -peek 'GetEdgePropertyAsOf$|EdgeCreateCount$' $L2/out/elevated/26_social_scale_bench/cpu.pprof
```
