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

# Laboratory iteration 3, 2026-10-12 — the same sweep at `7b8120df`

Sprint 367, rmp #3102. The iteration-1 sweep was re-run at HEAD `7b8120df` with the same
driver, passes, examples, scales and owner reader, so every row compares 1:1 with sections 2
and 11. **No module code was changed.** Every number below was collected in this session.

## 16. Method and environment (iteration 3)

- Same as sections 1 and 10, with these differences: binaries rebuilt (`LAB_REBUILD=1`,
  37 binaries, 25 s, outside every timed window); `GOCACHE` on its default on-disk location,
  `GOTMPDIR` on disk; graph data under `/Volumes/GoGraphRAM/lab3102` (deleted at the end).
  `$L3` = `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab3`.
- **Peak-RSS harness fix.** `$L3/rss.sh` strips the query with a quoted, literal pattern
  (`${a%" $Q"}`) and runs `set -f` before word splitting, so `[:FRIEND]` is no longer read as
  a glob bracket. `24_query` ran with the exact query and exited 0 (1 432 MiB).
- Host: Apple M4, 10 cores, Darwin 27.0.0 arm64, Go 1.27.2, tree clean at `7b8120df`,
  `GOGRAPH_FSYNC_LATENCY=off`. Load1: start 1.34 (top processes WindowServer 31%,
  `osascript` 26%, `system_profiler` 21%, iTerm 21% of one core); `default` 13:14–13:20
  (to 2.81); `elevated` 13:20–13:29 (2.74); peak RSS 13:29–13:34 (4.07); `contention`
  13:34–13:38 (4.08); A/B repeat 13:39–13:46 (2.43–5.13). Every row ran alone within its pass.
- **A/B and repeat.** `$L3/repeat.sh` ran rows 26 (2 + 2), 35 (3 + 3) and 36 (3 + 3)
  **interleaved**, A = the iteration-2 binaries (`6b399a3b`), B = HEAD, CPU and heap profiles
  only, plus one same-code repeat of rows 03 and 20 at HEAD.
- **Noise.** Same code, same profiles: row 03 50.54 → 49.93 s (−1.2%); row 26 B1/B2 −1.5%,
  A1/A2 −0.6%; row 36 spread 10–14% within an arm; row 20 97.33 → 106.65 s (+9.6%, load1 up
  to 5.13). The `elevated`-vs-`contention` comparison (15 rows above 2 s; the second pass adds
  mutex and block profiling) gives −3.4% to +10.7% per row, aggregate +1.3%. **A single-run
  per-row delta under ±10% is not a finding this iteration**; rows 26, 35 and 36 are judged on
  the interleaved A/B only.

## 17. Manifest `c794a1ed` → `6b399a3b` → `7b8120df`

**Totals (`elevated`, 42 rows).** CPU **595.56 → 443.90 → 429.80 s** (−3.2% vs iteration 2,
−27.8% vs iteration 1); module **383.88 → 249.06 → 233.24 s** (−6.4%, −39.2%); harness
54.57 → 56.03 → 54.54 s; unowned 157.11 → 138.81 → 142.02 s. Allocation **160.96 → 135.67 →
125.22 GiB** (−7.7%, −22.2%); module **144.47 → 119.13 → 108.38 GiB** (−9.0%, −25.0%).
Contention pass: CPU 463.57 → 317.65 → 304.39 s; delay 398.37 → 365.22 → 229.97 s (module
274.86 → 263.05 → 127.99 s, −51% vs iteration 2).

Row 37's `elevated` run failed in iteration 2 and passed here; its `contention` run failed
here on the same intermittent harness check (`ladder.L22 level=8 crash_image_after_checkpoint:
image taken false`). Its passing runs compare: 16.41 s (iteration 2, `contention`) → 16.41 s
(HEAD, `elevated`). Excluding row 37, module CPU 246.76 → 229.91 s (−6.8%) and module
allocation 114.54 → 101.23 GiB (−11.6%). Row 26 accounts for 10.58 s of the 15.82 s module-CPU
drop and 10.81 GiB of the 10.75 GiB module-allocation drop; the remaining per-row CPU deltas
are inside the noise band.

Exit status: every `default` row exited 0 (11 driver `cpu-total-zero` FAILs — instrument,
short runs with no CPU sample; 10 in iteration 2, the set varies run to run); `elevated` 31
exits 1 on the same harness check as iterations 1 and 2; `24_init` `cpu-total-zero`. No row
showed a module defect.

| row | exit d/e (3 points) | CPU s | module CPU s | alloc GiB | module alloc GiB | peak RSS MiB |
|---|---|---|---|---|---|---|
| 01_basic | 0/0 → 0/0 → 0/0 | 1.29 → 1.28 → 1.31 | 0.30 → 0.34 → 0.32 | 1.06 → 1.04 → 1.06 | 0.97 → 0.95 → 0.95 | 474 → 487 → 494 |
| 02_property_graph | 0/0 → 0/0 → 0/0 | 1.56 → 1.56 → 1.61 | 0.43 → 0.47 → 0.45 | 0.72 → 0.72 → 0.74 | 0.71 → 0.71 → 0.72 | 308 → 297 → 303 |
| 03_advanced_algorithms | 0/0 → 0/0 → 0/0 | 45.66 → 49.35 → 50.54 | 45.11 → 49.04 → 50.07 | 0.06 → 0.05 → 0.05 | 0.05 → 0.04 → 0.04 | 33 → 30 → 31 |
| 04_persistence | 0/0 → 0/0 → 0/0 | 3.82 → 3.67 → 3.68 | 2.19 → 2.07 → 2.16 | 1.92 → 1.86 → 1.93 | 1.91 → 1.85 → 1.91 | 292 → 268 → 274 |
| 05_out_of_core | 0/0 → 0/0 → 0/0 | 0.40 → 0.37 → 0.39 | 0.11 → 0.11 → 0.10 | 0.30 → 0.26 → 0.29 | 0.28 → 0.25 → 0.27 | 177 → 181 → 176 |
| 06_csv_import | 0/0 → 0/0 → 0/0 | 1.69 → 1.65 → 1.58 | 1.17 → 1.12 → 1.05 | 1.42 → 1.47 → 1.43 | 1.09 → 1.13 → 1.11 | 593 → 592 → 592 |
| 07_graphml_roundtrip | 0/0 → 0/0 → 0/0 | 2.12 → 2.26 → 2.17 | 1.39 → 1.44 → 1.48 | 1.85 → 1.85 → 1.79 | 1.82 → 1.83 → 1.77 | 592 → 591 → 591 |
| 08_pagerank | 0/0 → 0/0 → 0/0 | 5.97 → 5.97 → 5.98 | 2.29 → 2.34 → 1.88 | 0.68 → 0.69 → 0.70 | 0.62 → 0.64 → 0.65 | 300 → 302 → 299 |
| 09_leiden | 0/0 → 0/0 → 0/0 | 2.96 → 2.89 → 2.93 | 1.31 → 1.26 → 1.37 | 2.21 → 2.15 → 2.12 | 2.20 → 2.15 → 2.12 | 864 → 746 → 742 |
| 10_dimacs9_routing | 0/0 → 0/0 → 0/0 | 2.74 → 2.69 → 2.74 | 2.32 → 2.26 → 2.43 | 1.38 → 1.36 → 1.37 | 1.37 → 1.35 → 1.36 | 244 → 363 → 242 |
| 11_social_network | 0/0 → 0/0 → 0/0 | 129.78 → 3.86 → 3.76 | 129.15 → 2.08 → 1.92 | 0.47 → 0.33 → 0.33 | 0.45 → 0.31 → 0.31 | 218 → 181 → 184 |
| 12_build_dependency | 0/0 → 0/0 → 0/0 | 0.63 → 0.65 → 0.59 | 0.15 → 0.16 → 0.19 | 0.35 → 0.33 → 0.35 | 0.30 → 0.30 → 0.31 | 251 → 250 → 250 |
| 13_network_reliability | 0/0 → 0/0 → 0/0 | 2.90 → 2.81 → 2.90 | 2.35 → 2.20 → 2.35 | 0.07 → 0.07 → 0.07 | 0.05 → 0.05 → 0.05 | 73 → 73 → 73 |
| 14_routing_alternatives | 0/0 → 0/0 → 0/0 | 39.96 → 39.38 → 39.93 | 0.54 → 0.46 → 0.60 | 6.12 → 6.22 → 6.19 | 0.05 → 0.04 → 0.04 | 38 → 37 → 38 |
| 15_task_assignment | 0/0 → 0/0 → 0/0 | 3.26 → 3.30 → 3.39 | 2.80 → 2.81 → 2.93 | 0.30 → 0.30 → 0.28 | 0.20 → 0.21 → 0.19 | 211 → 211 → 211 |
| 16_centrality_analytics | 0/0 → 0/0 → 0/0 | 9.07 → 9.15 → 9.29 | 8.95 → 9.00 → 9.19 | 0.10 → 0.09 → 0.09 | 0.10 → 0.09 → 0.09 | 29 → 29 → 26 |
| 17_transactional_log | 0/0 → 0/0 → 0/0 | 0.53 → 0.51 → 0.48 | 0.40 → 0.41 → 0.40 | 0.50 → 0.16 → 0.17 | 0.49 → 0.16 → 0.17 | 25 → 20 → 20 |
| 18_oocore_pipeline | 0/0 → 0/0 → 0/0 | 1.47 → 1.50 → 1.47 | 0.34 → 0.48 → 0.47 | 0.82 → 0.86 → 0.86 | 0.66 → 0.71 → 0.71 | 344 → 335 → 340 |
| 19_pattern_query | 0/0 → 0/0 → 0/0 | 9.73 → 9.77 → 9.29 | 3.73 → 3.44 → 3.04 | 3.81 → 3.94 → 3.80 | 3.76 → 3.89 → 3.74 | 314 → 320 → 310 |
| 20_concurrent_reads | 0/0 → 0/0 → 0/0 | 114.13 → 100.64 → 97.33 | 61.37 → 57.14 → 52.03 | 33.66 → 17.81 → 18.37 | 32.29 → 16.45 → 17.02 | 145 → 101 → 103 |
| 21_typed_recovery | 0/0 → 0/0 → 0/0 | 11.28 → 11.29 → 11.39 | 8.43 → 8.30 → 8.35 | 2.12 → 2.09 → 2.19 | 2.09 → 2.06 → 2.16 | 372 → 332 → 359 |
| 22_cypher | 0/0 → 0/0 → 0/0 | 4.43 → 4.29 → 4.12 | 2.32 → 2.14 → 2.16 | 1.74 → 1.65 → 1.63 | 1.70 → 1.61 → 1.58 | 555 → 547 → 540 |
| 23_bolt_server | 0/0 → 0/0 → 0/0 | 2.97 → 2.84 → 2.81 | 0.86 → 0.54 → 0.70 | 0.63 → 0.68 → 0.65 | 0.48 → 0.50 → 0.48 | 116 → 118 → 113 |
| 24_init | 0/0 → 0/0 → 0/0 | 0.00 → 0.00 → 0.00 | 0.00 → 0.00 → 0.00 | 0.01 → 0.00 → 0.01 | 0.01 → 0.00 → 0.00 | 11 → 9 → 9 |
| 24_seed | 0/0 → 0/0 → 0/0 | 3.29 → 3.44 → 3.42 | 2.23 → 2.23 → 2.10 | 4.38 → 4.30 → 4.36 | 4.35 → 4.28 → 4.33 | 2122 → 1778 → 2123 |
| 24_stats | 0/0 → 0/0 → 0/0 | 5.60 → 5.84 → 5.81 | 3.31 → 3.62 → 3.48 | 3.87 → 3.98 → 3.95 | 3.87 → 3.98 → 3.95 | 1460 → 1396 → 1431 |
| 24_query | 0/0 → 0/0 → 0/0 | 4.35 → 4.44 → 4.40 | 2.37 → 2.00 → 2.13 | 3.79 → 3.73 → 3.78 | 3.78 → 3.72 → 3.78 | 1958 → 2013 ¹ → 1432 |
| 24_plandiff | 0/0 → 0/0 → 0/0 | 14.84 → 15.14 → 13.11 | 8.74 → 9.99 → 8.22 | 8.84 → 8.82 → 6.21 | 8.83 → 8.81 → 6.20 | 1472 → 1436 → 1423 |
| 24_snapshot | 0/0 → 0/0 → 0/0 | 4.93 → 5.11 → 5.05 | 2.66 → 2.46 → 2.36 | 4.46 → 4.43 → 4.38 | 4.45 → 4.43 → 4.37 | 1454 → 1392 → 1420 |
| 25_software_house_api | 0/0 → 0/0 → 0/0 | 24.23 → 19.25 → 17.93 | 11.29 → 5.44 → 5.08 | 9.55 → 3.48 → 3.41 | 9.52 → 3.45 → 3.38 | n/m |
| 26_social_scale_bench | 0/0 → 0/0 → 0/0 | 77.18 → 72.55 → 59.64 | 49.43 → 50.12 → 39.54 | 32.69 → 32.51 → 21.70 | 32.66 → 32.49 → 21.68 | 3469 → 3558 → 3460 |
| 27_concurrent_txn | 0/0 → 0/0 → 0/0 | 0.86 → 0.86 → 0.84 | 0.39 → 0.30 → 0.36 | 0.56 → 0.56 → 0.56 | 0.52 → 0.52 → 0.53 | 54 → 56 → 56 |
| 28_negative_weights | 0/0 → 0/0 → 0/0 | 9.51 → 9.40 → 9.58 | 9.32 → 9.07 → 9.41 | 1.37 → 1.36 → 1.37 | 1.36 → 1.36 → 1.35 | 965 → 965 → 965 |
| 29_all_pairs | 0/0 → 0/0 → 0/0 | 2.91 → 2.89 → 2.89 | 2.04 → 2.04 → 2.01 | 0.16 → 0.16 → 0.16 | 0.15 → 0.15 → 0.16 | 129 → 129 → 129 |
| 30_min_spanning_tree | 0/0 → 0/0 → 0/0 | 0.44 → 0.44 → 0.45 | 0.16 → 0.15 → 0.15 | 0.42 → 0.38 → 0.41 | 0.31 → 0.28 → 0.30 | 220 → 213 → 215 |
| 31_metrics_observability | 0/1 → 0/1 → 0/1 | 1.08 → 1.18 → 1.34 | 0.25 → 0.31 → 0.48 | 0.71 → 0.71 → 0.71 | 0.65 → 0.64 → 0.65 | 273 → 271 → 262 (exit 1) |
| 32_euler | 0/0 → 0/0 → 0/0 | 4.67 → 4.69 → 4.75 | 0.10 → 0.04 → 0.10 | 7.04 → 7.04 → 7.04 | 0.29 → 0.27 → 0.29 | 7228 → 7228 → 4827 |
| 33_generation_swap | 0/0 → 0/0 → 0/0 | 5.49 → 5.07 → 4.85 | 5.11 → 4.68 → 4.46 | 1.03 → 0.94 → 1.01 | 1.02 → 0.93 → 1.00 | 173 → 183 → 182 |
| 34_bolt_transactions | 0/0 → 0/0 → 0/0 | 1.15 → 1.20 → 1.18 | 0.28 → 0.31 → 0.25 | 0.63 → 0.65 → 0.66 | 0.59 → 0.61 → 0.62 | 270 → 295 → 284 |
| 35_mvcc_mixed_workload | 0/0 → 0/0 → 0/0 | 13.06 → 13.08 → 13.19 | 2.77 → 2.80 → 2.55 | 9.25 → 9.22 → 9.61 | 8.63 → 8.62 → 8.82 | 43 → 43 → 48 |
| 36_mvcc_snapshot_topology | 0/0 → 0/0 → 0/0 | 6.13 → 5.91 → 5.28 | 1.75 → 1.59 → 1.59 | 2.85 → 2.73 → 2.06 | 2.84 → 2.73 → 2.06 | 48 → 47 → 45 |
| 37_mvcc_write_contention | 0/1 → 0/1 → 0/0 | 17.49 → 11.73 → 16.41 ² | 3.67 → 2.30 → 3.33 ² | 7.08 → 4.65 → 7.38 ² | 6.98 → 4.59 → 7.15 ² | 87 → 104 → 112 |

¹ Iteration 2's value is the manual re-run (section 10). ² Iteration 2's `elevated` run
failed early; compare on the passing runs (above). Peak RSS is one run per row: row 32
(7 228 → 4 827 MiB, harness-owned allocation unchanged) and row 24_seed (1 778 → 2 123 MiB,
back to its iteration-1 value) are not attributed. Row 35 runs for a fixed duration, so its
totals are not per-operation; see section 18.

**Interleaved A/B (A = `6b399a3b`, B = `7b8120df`).**

| row | CPU s A → B | module CPU s A → B | alloc GiB A → B | per-operation |
|---|---|---|---|---|
| 26 (n = 2 + 2) | 74.26, 73.84 → 62.41, 61.45 (**−16.4%**) | 47.63, 46.79 → 38.76, 41.90 (−14.6%) | 32.86, 32.80 → 21.58, 21.68 (**−34.1%**) | fixed workload |
| 36 (n = 3 + 3) | 6.09, 5.90, 6.73 → 5.38, 5.00, 5.51 (**−15.1%**) | 1.66 → 1.55 mean (within noise) | 2.80, 2.71, 2.88 → 2.06, 2.01, 2.08 (**−26.7%**) | fixed workload |
| 35 (n = 3 + 3) | 13.35, 13.15, 12.76 → 13.13, 13.07, 12.99 (fixed duration) | — | module 8.25 → 8.32 GiB mean | summed phase throughput 2.21 M → 2.70 M ops/s (**+22.5%**); module allocation per operation **4 012 → 3 304 B (−17.7%)** |

## 18. Verification of the iteration-2 fixes

| commit / task | claim | measured at HEAD (`elevated`, cumulative) | verdict |
|---|---|---|---|
| `4e9c48bb` #3088 `edgeInstanceIdxFor` removed | correctness; the positional fallback goes | `edgeInstanceIdxFor` 1.51 s → **0**; `EdgeCreateCount` (`ReadView` + `Graph`) 1.50 + 1.49 s → **0** | **moved as claimed** (cost side; the correctness regression test is not re-run here) |
| `d56bfbe6` #3089 per-`ReadView` slot index | row 26 type lookup 5.49 → 3.22 s, presence 2.02 → 0.69 s | `EdgeLabelsByID` **6.83 → 3.79 s (−45%)**, `slotLabelsForPair` 6.20 → 0.33 s; `ReadView.GetEdgeProperty` 5.78 → **4.76 s (−18%)** (`edgePropertyBySlots` 3.21 s, `pairIDs` 0.93 s); `ReadView.EdgeHasProperty` 2.30 → **1.81 s (−21%)** (`edgeHasPropertyBySlots` 1.07 s, `pairIDs` 0.37 s); new cost `buildSourceSlots` 1.69 s and **692 MiB** | **type lookup moved as claimed**; presence moved **less** than claimed at the `ReadView` call (1.81 s, not 0.69 s); property read moved partly |
| `7e7b854a` #3090, `ce609ce6` #3093 shared append buffers | row 26 site 5 077 → 1 518 MB | `GrowSlotWithValue` **5 068.9 → 1 542.4 MiB (−70%)** | **moved as claimed** |
| `280ce211` #2732 aggregations do not materialise nodes | count DISTINCT node −74% allocs | `nodePropsToExprMap` **10 984 → 733 MiB, 3.43 → 0.23 s**; `populateRowCtx` 17.23 → 5.61 GiB; row 24_plandiff alloc −30%, row 36 −27% (A/B) | **moved as claimed** |
| `85f8ba86` #3094 lint, dead forward-CSR helpers | no runtime claim | — | not measurable here |
| `b737b3a2`, `3d1be4dd`, `138f7bc3` #3095 | test-only | — | not exercised by examples |
| `3f8bddfc` #3096/#3097 seek once, layout memo | point read −19%, 82 → 61 allocs; example 35 `copySchema` 766 → 50 B/exec | row 35 `copySchema` **1.19 → 0.10 GiB (−92%)** at +22.5% more operations; read build per operation −25%; `indexSeekWouldFire` 0.72 → 1.23 GiB (the seek is now built there and handed off) | **moved as claimed** |
| `7b8120df` #3092 example 20 PageRank budget | block −67% | row 20 `pagerank.go:866` + `:930` block **185.63 → 60.13 s (−68%)**; row 20 CPU 100.36 → 101.81 s, unowned 33.98 → 34.39 s (contention pass); 8-worker throughput 206 → 212 reads/s | **block moved as claimed**; CPU and throughput did **not** move — the removed delay was parked-worker wait, not work |

## 19. Residual module-owned costs at HEAD

**Significance threshold** (as section 13, recomputed on this sweep): **1% of module CPU =
2.33 s**, **1% of module allocation = 1.08 GiB** (`elevated`), **1% of module contention delay
= 1.28 s** (`contention`), at a non-idle site, not inherent to the algorithm or workload.

| # | site | cost (row) | hypothesis | risk | verdict |
|---|---|---|---|---|---|
| **U1** | integer ids boxed into `expr.Value` per row: `cypher/exec/expand.go:1284–1286` (`IntegerValue(srcID/edgeID/dstID)`), `scan_label.go:206`, `scan_all.go:149` | **5.83 GiB (5.4%)** (26, 25, 24_plandiff, 35, 19); `Expand.buildRow` 1.52 s flat (26) | `IntegerValue` is an `int64` stored in the `Value` interface, so every id above 255 is a heap object. **Established by code**; the remedy (ids carried unboxed, e.g. in the `Chunk` `i64` columns) changes the row representation | medium-high; **row-model change — user decision** | **worth a task** |
| **U2** | `cypher.(*Engine).buildReadPhysical` per cached execution (row 35) | **5.55 GiB (5.1%)**: scaffold 1.67 GiB flat, range seek 1.00 GiB, `newRowBindPlan` 0.52 GiB, predicate/projection closures, operator structs | plan-pure separation | per S3–S6 | **worth a task — already backlog S3 (rowbind/closures), S4 (seek memo, ~1.0 GiB here), S5 (scaffold), S6 (operator tree)** |
| **U3** | `cypher.csrPairAndColumnCachedFor` → `buildRelTypeColumn` 5.72 s (`encodeSlotTypes` 2.05 s) | **7.99 s (3.4%), 2.81 GiB (2.6%)** (26) | the CSR pair and type column are cached per graph version; the number of builds per version in row 26 is **not measured**; rebuilds after each write phase would make it inherent | unknown until measured | **worth a measurement task** (count builds per version, then decide) |
| **U4** | `cypher/expr.NewLazyRelationshipValue` (`value.go:809`), one heap object per relationship per row | **2.72 GiB (2.5%)**, 0.66 s (26) | the lazy value is returned by pointer; a value-typed or row-scoped form would remove one allocation per relationship. **Hypothesised** | medium (value lifetime crosses `Result`) | **worth a task** |
| **U5** | cypher ↔ lpg id → key → id round trip in relationship materialisation: `api.go:16869–16870` `Mapper.Resolve` 1.10 s, then `ReadView.pairIDs` `Mapper.Lookup` 1.31 s | **2.41 s (1.03%)** (26) | the caller holds `srcID`/`dstID`; ID-keyed `GetEdgeProperty`/`EdgeHasProperty` variants remove both directions (iteration-2 Q4 remainder). **Established by profile + code flow** | low | **worth a task** (join with U4, same path) |
| U6 | `graph/mvcc.NewCommitInfo` (`mvcc.go:98`) | 1.45 GiB (1.3%) (26 load, 24_*) | one record per committed transaction; lifetime is the version chain's | — | inherent (not ranked) |
| U7 | `graph/lpg.StringValue` (`property.go:118`) | 1.11 GiB (1.02%) (26, 19) | string boxed into `PropertyValue.v`; at threshold | medium (property model) | at threshold; join U1's decision |
| U8 | `cypher/exec.NewChunk` (`chunk.go:176`) | 1.08 GiB (1.00%) (37) | per-statement `i64` column | low | at threshold; insignificant |
| U9 | `ReadView.buildSourceSlots` (#3089 index) | 1.69 s (0.72%), 692 MiB (0.62%) (26) | single-entry index rebuilt on each source switch | low | insignificant |
| U10 | row 37 node-property shard lock `property.go:391/358`, reader `snapshot_read.go:217` | 7.88 s mutex + 5.79 s + 5.04 s block | the deliberate hot set; #3091 measured cross-node collisions below threshold | — | inherent (closed by #3091) |
| U11 | `graph/lpg/mvcc_vacuum.go:717` | 17.32 s block (26, 22, 34) | the vacuum loop's idle `select` | — | idle |
| U12 | row 20 PageRank park `pagerank.go:866/930` | 60.13 s block | parked budgeted workers; CPU and throughput unchanged by #3092 | — | idle |

Re-confirmed and unchanged (inherent or tracked): WAL replay per `24_*` process
(`recovery.go:2207`, 4.47 GiB), row 20 search pools (#2382; `search.go:164` 4.19 GiB),
`adjlist.go:971` copy-on-write entries (2.74 GiB), `txn.go:1379` bulk redo (2.49 GiB),
Brandes (row 03, 50.07 s), PageRank `runRange`, Johnson, WAL group sync in row 21 (7.04 s).
No vacuous code was found in this iteration's sites: every removed path (#3088) shows zero
residual samples, and no new site returns a discarded result.

**Cross-component reading.** Row 26's relationship materialisation remains the largest chain
(`buildRelationshipValueFromRow` 8.84 s, 5.02 GiB, from 15.02 s / 4.58 GiB: CPU −41%, allocation +0.44 GiB, the #3089 index): Expand boxes the
ids (U1) → `cypher` resolves ids to string keys (U5) → `graph/lpg` looks the keys up again
(U5) → the slot index (#3089) → one lazy value object per relationship (U4); the type column
the Expand reads is built in `cypher` from the `graph/csr` pair (U3). The second chain is the
read build (U2, cypher → planner → exec), already designed in `docs/design-plan-pure-separation.md`.

## 20. Significance and campaign verdict

- **Moved this iteration:** module CPU −6.4% (−15.82 s), module allocation −9.0%
  (−10.75 GiB), module contention delay −51% (−135.06 s, parked wait). Beyond the noise band
  only on rows 26, 35, 36 (interleaved A/B) and on the site-level deltas of section 18.
- **Significant residuals not in the backlog:** U1 (5.4% alloc, row-model change), U3 (3.4%
  CPU, measurement first), U4 (2.5% alloc), U5 (1.03% CPU). **In the backlog:** U2 = S3–S6.
- **Verdict: one more optimisation iteration is warranted, narrowly** — on the row-26
  relationship path (U5 + U4 together, low/medium risk; U3 measured first). U1 is the
  largest item but changes the row representation and goes to the user as an architecture
  decision, as S4 and S6 do. Every other module site is below the threshold, idle, or
  inherent; once U3–U5 are done or refused, the campaign has reached diminishing returns
  for example-driven work.

## 21. Limits (iteration 3)

- Noise is wider than in iterations 1 and 2 (load1 up to 5.13): ±10% per single-run row.
- A/B covers rows 26, 35, 36 only. Row 35's per-operation figure assumes equal phase
  durations (the example reports per-phase throughput, not counts).
- U3's build count, U4's and U7's remedies are hypotheses; U1's remedy is not designed.
- Row 25's peak RSS remains unmeasured; peak RSS has no repeat.

## 22. Reproduction (iteration 3)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
$L3/sweep.sh            # default (LAB_REBUILD=1), elevated, rss.sh (fixed), contention
$L3/repeat.sh           # interleaved A/B rows 26, 35, 36; repeats rows 03, 20
$L3/owner/owner $L3/out/elevated/*/{cpu,heap}.pprof > $L3/owner.elevated.tsv
$L3/owner/owner $L3/out/contention/*/*.pprof       > $L3/owner.contention.tsv
$L3/owner/owner $L3/repeat/*/{cpu,heap}.pprof      > $L3/owner.repeat.tsv
python3 $L3/compare3.py <scratchpad>                # section 17 table (lab1, lab2, lab3)
python3 $L3/fn.py '<function regex>' $L2/owner.elevated.tsv $L3/owner.elevated.tsv   # section 18
python3 $L3/agg.py $L3/owner.elevated.tsv alloc_space line 40
python3 $L3/agg.py $L3/owner.contention.tsv delay line 30
python3 $L3/agg.py $L3/owner.repeat.tsv cpu summary
```

# Campaign conclusion, 2026-10-12

Sprint 367, rmp #3107. Built only from recorded evidence: sections 1–22, the commit messages
`c794a1ed..8932f1f3` plus `590d85f3` and `5b3d50af`, and the rmp task logs cited per row. **No new measurement.**

## 23. Final totals

| measure (`elevated` sweep unless stated) | `c794a1ed` → `6b399a3b` → `7b8120df` | change across the sprint | source |
|---|---|---|---|
| module CPU | 383.88 → 249.06 → 233.24 s | −39.2% | §17 |
| module allocation | 144.47 → 119.13 → 108.38 GiB | −25.0% | §17 |
| module contention delay (`contention` pass) | 274.86 → 263.05 → 127.99 s | −53.4% (mostly parked wait, §18) | §17 |
| total CPU / total allocation | 595.56 → 443.90 → 429.80 s / 160.96 → 135.67 → 125.22 GiB | −27.8% / −22.2% | §17 |

After iteration 3 (**paired example-26 runs, not sweep totals**; units as recorded):

| commit / task | example 26, paired | micro-benchmark | source |
|---|---|---|---|
| `a5f2668f` #3104 pooled lazy relationship values | total alloc 22.25/22.31 → 19.51/19.39 GB (−12.6%); lazy-rel alloc 2.8 GB → 0 | GoAPI/lazy −51.60% B/op, −3.41% time | #3104 |
| `8932f1f3` #3106 streamed edge labels | total alloc 19.53/19.38 → 18.59/18.68 GB; label slice 0.64 GB → 0; CPU within run spread, not claimed | GoAPI/multitype −7.10% time, −15.2% B/op | #3106 |

## 24. Cross-component chains

| chain | waste observed | fix (task, commit) | measured after | residual, status |
|---|---|---|---|---|
| relationship materialisation: exec Expand → cypher row build → lpg slot scan / columns → expr values (row 26) | `buildRelationshipValueFromRow` 13.71 s (§3 R2), 14.99 s (§12); `slotLabelsForPair` 6.18 s; `GetEdgePropertyAsOf` 5.78 s (Q3); `edgeInstanceIdxFor` 1.51 s + `EdgeCreateCount` 1.50 + 1.49 s (§18) | #3080 `4d831e8e`; #3088 `4e9c48bb`; #3089 `d56bfbe6`; #3104 `a5f2668f`; #3106 `8932f1f3` | #3080 no move on row 26 (§12); 8.84 s, 5.02 GiB at iteration 3 (§19); lazy values and label slices 0 (§23) | id boxing U1, user-dropped; id→key→id U5 2.41 s, user-dropped (#3105); per-view index build 1.24/1.37 s, inherent (#3106) |
| CSR pair cache → relationship type column | `buildRelTypeColumn` path 7.99 s, 2.81 GiB (U3) | measurement only, #3103 | 7 builds, each the first per Engine; 0 rebuilds (#3103) | inherent |
| MVCC edge write → edge-property columns → pinned readers | `GrowSlotWithValue` 4.95 GiB (Q8) | #3090 `7e7b854a`; #3093 `ce609ce6` | 5 068.9 → 1 542.4 MiB (§18) | 48 B per-version block floor, accepted by the user (#3090) |
| aggregation → node materialisation → lpg properties | `nodePropsToExprMap` 10.73 GiB, 3.43 s (Q6) | #2732 `280ce211` | 733 MiB, 0.23 s (§18) | node references through aggregation not carried (row contract), left as is (#2732) |
| cached read: plan cache → physical build → index seek (row 35) | `buildReadPhysical` 6.35 GiB (R6), 6.18 GiB (Q7) | #3096/#3097 `3f8bddfc` | `copySchema` 1.19 → 0.10 GiB; throughput +22.5%, alloc/op −17.7% (§17, §18) | U2 5.55 GiB, backlog S3–S6 |
| SET → constraint registry → lpg node-property shard locks (row 37) | `reserveSetPropertyGen` 1.79 s mutex + 1.85 s block (R8) | #3082 `546efbe8` | 1.11 s + 1.10 s (§12) | Q11 below threshold; shard lock inherent (U10, #3091) |
| MVCC reclaim → node-property shard locks → readers (row 37) | `reclaimPropVersions` write-locked every shard per pass: 5.9 M write-locks/run, **95.8%** on shards without history; waiters' delay 1.71/1.93 s; abort sweeps 0 ms (#3108) | #3108 `590d85f3` (read-locked pre-check) | waiters' delay median **1187 → 104 ms** (5 paired runs, every new run below every base run) (#3108) | write-locked unlink of real history, inherent (#3108) |
| Cypher introspection and scan → lpg (row 25) | `PropertyKeysInUse` 6.08 s, 1.80 GiB (R5); `AllNodesScan.Init` 4.88 GiB (R7) | #3081 `1a8e25ae` | 0.54 s, < 0.5 MiB; 21 MiB (§12) | `AllNodesScan.Next` boxing (Q10), part of U1 |
| checkpoint → snapshot writer (row 17) | 1 MiB `bufio` writers ≈ 360 MiB (R9) | #3083 `e71407c1` | row 17 0.50 → 0.16 GiB (§12) | none ranked |
| csr projection → search analytics (PageRank, BiBFS) | transpose of a symmetric CSR 13.96 GiB, 3.24 s (R1); `IsSymmetric` 53 MiB (V3); intermediate directed CSR ≈ 0.27 GiB (P2) | #3078 `cfd64290`, `e7b12a60` | transpose 0 on row 20; row 20 alloc −47%; rows 09 + 33 536 → 366 MiB (§12) | one-pass build +27% CPU, accepted by the user (#3078) |
| csr projection → diameter (row 11) | `bfsFarthest` 125.28 s flat (R4) + 9.8 s rescans (R3) | #3079 `4be967d2`; #3085 `6b399a3b` | 1.80 s; row 11 CPU −97% (§12) | Q15 below threshold; 600-cycle +203%, accepted (#3085) |
| concurrent callers → PageRank workers → scheduler (row 20) | 151.04 + 30.70 s block (§4); 185.6 s (Q1) | #3086 `fc0f6807`; #3092 `7b8120df` | 60.13 s block (−68%); CPU and throughput unchanged (§18) | U12 idle wait |
| WAL writer lifecycle → tests' cleanup | segment preparer alive at test end in 18/2 200 subtests (#3095) | #3095 `b737b3a2`, `3d1be4dd`, `138f7bc3` | 0/2 200 (#3095) | 19 sites left with stated reasons (#3095) |

## 25. Cost moved between components

| fix | where the cost went | figures | source |
|---|---|---|---|
| #3082 constraint lock | writers now queue on the node-property shard lock | registry site −39%; row 37 mutex total 15.57 → 15.70 s; shard lock 7.44 → 7.73 s | §12 |
| #3089 slot index | new index build in `graph/lpg` | `buildSourceSlots` 1.69 s, 692 MiB; relationship builder alloc 4.58 → 5.02 GiB | §18, §19 |
| #3096 seek once | the seek is built in the probe and handed off | `indexSeekWouldFire` 0.72 → 1.23 GiB | §18 |
| #3078 one-pass projection | bytes traded for build CPU | −32.44% B/op, +27.09% sec/op | #3078 |
| #3085 BoundingDiameters | vertex-transitive shapes pay more sweeps | 600-cycle 659 → 1 997 µs (+203%) | #3085 |
| #3092 PageRank budget | parked wait removed, work unchanged | block −68%; CPU 100.36 → 101.81 s; 206 → 212 reads/s | §18 |

## 26. Residual ledger

| residual | decision | evidence |
|---|---|---|
| Q1 PageRank budget unused | fixed (#3092); remainder U12 idle | §18 |
| Q2, Q3 per-pair scans | fixed in part (#3089): type 6.83 → 3.79 s, property 5.78 → 4.76 s | §18 |
| Q4 string-key round trips | `EdgeCreateCount` removed (#3088); remainder = U5 | §18 |
| Q5 handle read as position | bug, fixed (#3088) | #3088 |
| Q6, Q8 | fixed (#2732, #3090) | §18 |
| Q7 / U2 read build | S1 + S2 fixed (#3096/#3097); S3–S6 backlog: #3098 S3 row-bind and closures, #3099 S4 access-path cache, #3100 S5 build-object size, #3101 S6 shared operator tree | #2391, #3097; titles of #3099–#3101; sprint 367 DECISION 155 |
| Q9 / U10 shard lock | inherent: cross-node share 1.13/1.35 s instrumented, ≈ 0.6–0.8 s scaled | #3091 |
| whole-shard sweeps (`reclaimPropVersions`, abort-reclaim) | fixed (#3108, `590d85f3`): all of it is `reclaimPropVersions` (abort sweeps 0 ms); 95.8% of 5.9 M write-locks/run hit shards without history; waiters' delay median 1187 → 104 ms (5 paired runs); residual write-locked unlink inherent | #3091, #3108 |
| Q10–Q15, U8, U9 | below threshold | §13, §19 |
| U1 id boxing (5.83 GiB), U7 string boxing (joins U1) | user-dropped (row-model change): measurement recorded, no task | §19; sprint 367 DECISION 155 |
| U3 pair/type column builds | inherent (#3103) | #3103 |
| U4 lazy relationship object | fixed (#3104) | #3104 |
| U5 id → key → id (2.41 s, 1.03%) | user-dropped: needs exported API | #3105 |
| U6 commit records; WAL replay per `24_*`; copy-on-write adjacency; bulk redo; Brandes, PageRank `runRange`, Johnson, WAL group sync | inherent | §7, §19 |
| U11, U12 | idle | §19 |

## 27. Significance verdict

- **Threshold** (iteration 3, §19): 1% of module CPU = **2.33 s**, allocation = **1.08 GiB**,
  contention = **1.28 s**. Noise: ±5% per row in iterations 1–2 (§10), ±10% in iteration 3 (§16).
- **The last changes against it.** #3104 removed 2.8 GB in example 26 (paired), above the
  allocation threshold. #3106 removed ≈ 0.8 GB (paired, about 0.76 GiB), **below** it, and its
  CPU change is inside the run spread (#3106). #3103 found U3 inherent; #3105 dropped U5 at 1.03%.
- **Verdict (iteration 5, §37–§42, full sweep at `c52a6380`): insignificant returns are NOT
  confirmed as stated.** N1 is fixed (#3111, verified sweep-wide and by interleaved A/B) and
  N2 is decided inherent (#3112), and no regression was found; but on HEAD's smaller totals two
  residuals previously decided "below threshold" now sit marginally above the recomputed 1%
  thresholds: **U8**, `exec.NewChunk` (1.11 GiB, 1.08% of module allocation), and **Q12**,
  `lpg.appendSuspects` (1.57 s block, 1.15% of module contention delay). Every other residual
  is fixed, inherent, idle, below threshold, user-dropped (U1, U5, U7) or in the backlog
  (S3–S6, #3098–#3101).
- *Superseded* (iteration 4, §30–§34, full sweep at `ffeebf23`): example-driven optimisation of
  GoGraph had NOT yet reached insignificant returns. The last changes were confirmed at sweep
  level (module allocation −3.0%, no regression) and #3106 and #3108 are each below the 1%
  threshold, but two module residuals above the recomputed allocation threshold (1.05 GiB) were
  never ranked or decided: **N1**, PageRank's per-call rank vectors (2.63 GiB, 2.50%), and
  **N2**, the adjacency reverse-index appends (1.73 GiB, 1.64%). Every other residual is
  inherent, idle, below threshold, user-dropped (U1, U5, U7), or in the backlog as design work
  (S3–S6, #3098–#3101); the whole-shard sweeps noted in #3091 were fixed by #3108.

## 28. Bugs found and claims corrected

| item | what | regression guard | source |
|---|---|---|---|
| **#3088** | a Go-API edge was reported with a Cypher sibling's type after deletes: the stable handle was read as a CSR position | `cypher/edge_handle_instance_test.go` fails at `0dbd9452` | #3088 |
| **#3093** | an aborted append could rewrite an edge-property cell a pinned reader still read (data race, 555 instead of 777) | `graph/lpg/edge_property_fused_rewrite_test.go`, 3 of 4 fail at `7e7b854a` | #3093 |
| **#3095** | test-owned WAL writers outlived their test directory; not introduced by this sprint (26/2 200 at `c794a1ed`) | `TestEnginesB2WALClosedWhenTestEnds_3095` | #3095 |
| #3089 correction | presence 2.02 → 0.69 s **not reproduced**: 2.30 → 1.81 s in the sweep | — | #3089, §18 |
| #3090 correction | premise wrong: values already grew amortised; the copy was the per-version column headers (384 B for ≈ 12 B of value) | — | #3090 |
| #3109 correction | example 37's `ladder.IX11` was a harness check that could not fail on serialisation (a forced-serialisation mutant passed it); present at `c794a1ed`, **not a module regression**; fixed in `5b3d50af` (fixed check 15/15 pass, mutant 15/15 fail) | fixed IX11 check | #3109 |
| other refuted premises | #3080 step (b) O(1) slot read; #2732 original cause; #2391 recorded split; #3091 shard collision | — | task logs |

# Laboratory iteration 4, 2026-10-12 — the same sweep at `ffeebf23`

Sprint 367, rmp #3110. The iteration-3 sweep was re-run at HEAD `ffeebf23` (module code as at
`590d85f3`: #3104 `a5f2668f`, #3106 `8932f1f3`, #3108 `590d85f3`; #3109 `5b3d50af` changes the
example-37 harness only) with the same driver, passes, examples, scales and owner reader, so
every row compares 1:1 with section 17. **No module code was changed.** Every number below was
collected in this session.

## 29. Method and environment (iteration 4)

- Same as section 16, with these differences: `$L4` =
  `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab4`;
  binaries rebuilt (`LAB_REBUILD=1`, 37 binaries, 25 s, outside every timed window); graph data
  under `/Volumes/GoGraphRAM/lab3110` (deleted at the end); `GOCACHE` default (on disk).
- Host: Apple M4, 10 cores, Darwin 27.0.0 arm64, Go 1.27.2, tree clean,
  `GOGRAPH_FSYNC_LATENCY=off`. Load1: start 2.60 (top processes iTerm 30%, `claude` 16%,
  `sysmond` 13%, `osascript` 11%, WindowServer 9% of one core); `default` 15:55–16:01 (to 3.94);
  `elevated` 16:01–16:09 (3.95); peak RSS 16:09–16:14 (4.60); `contention` 16:14–16:19 (3.11);
  A/B repeats 16:19–16:28 (2.78–6.03). Every row ran alone within its pass.
- **A/B.** `$L4/repeat.sh` ran rows 26 (3 + 3, CPU and heap) and 37 (3 + 3, with
  `-contention 1`) **interleaved**, A = the iteration-3 binaries (`7b8120df`), B = HEAD.
  `$L4/repeat2.sh` re-ran the rows the sweep flagged (section 31): 08 and 23 (3 + 3, CPU and
  heap) and the peak RSS of 10 and 32 (3 + 3, `/usr/bin/time -l`), interleaved the same way.
- **Noise.** Same-code spread inside one arm: row 26 module CPU 40.01–44.31 s (A) and
  40.67–49.96 s (B); row 37 total CPU 16.78–24.76 s (B; B3 executed more statements:
  `NewChunk` 2.04 GiB against 1.05–1.11 GiB); rows below 1 s of module CPU move ±30–50% run to
  run (rows 02, 14, 32 fell 31–50% with no change in their code paths). As in iteration 3, **a
  single-run per-row delta under ±10% is not a finding**, and rows under 1 s of module CPU are
  judged on A/B only.

## 30. Totals `c794a1ed` → `6b399a3b` → `7b8120df` → `ffeebf23`

| measure (`elevated` sweep unless stated) | iteration 1 → 2 → 3 → 4 | iteration 4 vs 3 | vs iteration 1 |
|---|---|---|---|
| module CPU | 383.88 → 249.06 → 233.24 → **232.74 s** | −0.2% (inside noise) | −39.4% |
| module allocation | 144.47 → 119.13 → 108.38 → **105.17 GiB** | **−3.0% (−3.21 GiB)** | −27.2% |
| module contention delay (`contention` pass) | 274.86 → 263.05 → 127.99 → **126.17 s** | −1.4% | −54.1% |
| total CPU | 595.56 → 443.90 → 429.80 → 428.03 s | −0.4% | −28.1% |
| total allocation | 160.96 → 135.67 → 125.22 → 121.89 GiB | −2.7% | −24.3% |
| harness / unowned CPU | 54.57 / 157.11 → … → 54.54 / 142.02 → 55.51 / 139.78 s | +1.8% / −1.6% | — |

Row 26 carries the allocation drop: module 21.68 → 18.25 GiB (−3.43 GiB, −15.8%); every other
row's module allocation moved by at most 0.19 GiB (row 20, +1.1%, inside noise). Exit status: every `default` row exited 0
(11 `cpu-total-zero` instrument FAILs, the same 11 rows as iteration 3); `elevated` row 31 exits
1 on the same planner-statistics harness check as iterations 1–3, `24_init` `cpu-total-zero`;
`contention` row 37 exits 1 on the same intermittent check as iteration 3
(`ladder.L22 level=8 crash_image_after_checkpoint: image taken false`), profiles written. No row
showed a module defect. Per-row table: `$L4/compare4.md`.

## 31. Verification of #3104, #3106, #3108 at sweep level

| commit / task | claim (paired, single example) | sweep (`elevated`, owner reader) | interleaved A/B | verdict |
|---|---|---|---|---|
| `a5f2668f` #3104 pooled lazy relationship values | example 26 alloc −12.6%; lazy-rel 2.8 GB → 0 | `NewLazyRelationshipValue` **2 783.3 MiB, 0.66 s → 0 MiB, 0.03 s** | row 26 module alloc 22 211 / 22 134 / 22 189 → 18 716 / 18 599 / 18 678 MiB (**−15.8%, −3.43 GiB**, every B below every A), shared with #3106 | **confirmed** |
| `8932f1f3` #3106 streamed edge labels | label slice 0.64 GB → 0; CPU not claimed | `edgeLabelsBySlots` **614.0 MiB → 0**; `EdgeLabelsByID` 1 347.9 MiB, 3.79 s → 5.5 MiB, 0 s; new `ForEachEdgeLabelByID` 4.11 s, 687.9 MiB (of which `buildSourceSlots` 675.9 MiB, unchanged); `buildRelationshipValueFromRow` 8.88 s, 5.02 GiB → **8.13 s, 1.66 GiB** | included above; row 26 module CPU 40.01 / 44.31 / 42.31 → 41.59 / 49.96 / 40.67 s (no change beyond spread) | **confirmed for allocation**; CPU unchanged, as #3106 stated |
| `590d85f3` #3108 read-locked reclaim pre-check | waiters' delay median 1 187 → 104 ms (instrumented) | `reclaimPropVersions` delay 0.44 s → 0.23 s (single run each) | row 37 `reclaimPropVersions` **mutex delay 0.31 / 0.26 / 0.29 → 0.07 / 0.06 / 0.09 s (−75%)**; block delay 0.20 / 0.24 / 0.19 → 0.19 / 0.18 / 0.21 s (unchanged) | **confirmed in direction**; the profile sees the waiters' share only at the unlock site, so the instrumented −91% is not reproduced by this method; both before and after are below the 1.26 s threshold |

## 32. Rows flagged by the sweep, repeated

| row | sweep, iteration 3 → 4 | interleaved repeat A (`7b8120df`) → B (HEAD) | verdict |
|---|---|---|---|
| 08_pagerank | module CPU 1.88 → 2.21 s (+17.6%) | 2.59 / 2.00 / 2.26 → 2.17 / 1.95 / 1.67 s | **not a regression** (iteration 3's 1.88 s was a low draw; iterations 1–2: 2.29, 2.34 s) |
| 23_bolt_server | module CPU 0.70 → 0.78 s (+11.4%) | 1.05 / 0.82 / 0.78 → 0.72 / 0.78 / 0.85 s | **not a regression** |
| 30_min_spanning_tree | module CPU 0.15 → 0.17 s (+13.3%) | not repeated: 0.02 s is two 10 ms samples | below resolution |
| 10_dimacs9_routing | peak RSS 242 → 387 MiB | 376 / 391 / 366 → 385 / 242 / 386 MiB | **bimodal in both arms** (iteration 2 had 363); not a regression |
| 32_euler | peak RSS 4 827 → 7 228 MiB | 7 228 × 3 → 7 228 × 3 | **iteration 3's 4 827 MiB was the outlier** (iterations 1–2: 7 228); harness-owned |
| 24_seed | peak RSS 2 123 → 1 818 MiB | not repeated | one-run RSS, moves between 1 778 and 2 123 MiB across iterations |

No row regressed beyond noise.

## 33. Residual module-owned costs at HEAD

**Threshold** (recomputed on this sweep): 1% of module CPU = **2.33 s**, of module allocation =
**1.05 GiB**, of module contention delay = **1.26 s**.

| # | site (row) | cost at HEAD | status |
|---|---|---|---|
| **N1** | `search/centrality.PageRankCtx` `pagerank.go:286–287`, the per-call `cur` and `next` rank vectors (row 20) | **2.63 GiB (2.50%)**: 1 366.2 + 1 325.6 MiB (iteration 3: 1 391.4 + 1 350.1 MiB) | **above threshold, never ranked** (present in iterations 2 and 3, not listed in sections 13 or 19). One vector is the returned result; the other is scratch dropped at return. **Hypothesis:** the scratch half, ≈ 1.31 GiB (1.25%), is poolable. Not measured |
| **N2** | `graph/adjlist.(*revIndex).add` `reverse.go:134` (per-destination source-list append) and `:130` (shard array doubling) (rows 26, 09, 01, 33, 32, 18 and others) | **1.73 GiB (1.64%)**: 1 148.7 + 618.6 MiB (iteration 3: 1 174.4 + 595.6 MiB) | **above threshold, never ranked**. Part is the index's own content, part is amortised-growth garbage; the split is **not measured** |
| N3 | `graph/lpg.(*sourceSlots).pairRun` (row 26) | **2.82 s (1.21%)** flat (iteration 3: 2.85 s) | above threshold as a site, never ranked. Three binary searches of the same (source, destination) pair per relationship: labels 1.21 s, property 1.01 s, presence 0.60 s. Removing two of three would save at most ≈ 1.61 s (0.69%): **remedy below threshold** (hypothesised) |
| U1 | id boxing: `expand.go:1284–1286` 3.43 GiB, `scan_label.go:206` 1.44 GiB, `scan_all.go:149` 1.00 GiB | 5.87 GiB (5.6%) | user-dropped (§26) |
| U7 | `lpg.StringValue` `property.go:118` | 1.14 GiB (1.08%) | user-dropped with U1 (§26) |
| U8 | `exec.NewChunk` `chunk.go:176` (row 37) | 1.12 GiB (1.06%); 1.05–2.04 GiB across row-37 repeats (statement count varies) | at threshold; varies with the workload's statement count |
| U2 | `buildReadPhysical` (row 35) | 5.94 GiB cumulative | backlog S3–S6, #3098–#3101 |
| U3 | `csrPairAndColumnCachedFor` (row 26) | 7.51 s, 2.74 GiB | inherent (#3103) |
| U5 | `Mapper.Resolve` 1.43 s + `Mapper.Lookup` 1.75 s | 3.18 s combined | user-dropped (#3105) |
| U6 | `mvcc.NewCommitInfo` | 1.43 GiB (1.36%) | inherent |
| U10 | row 37 node-property shard lock `property.go:391/358`, `snapshot_read.go:217` | 8.03 s mutex, 5.64 + 5.15 s block | inherent (#3091) |
| U11, U12 | vacuum idle `select` `mvcc_vacuum.go:717` 16.82 s; PageRank parked workers `pagerank.go:866/930` 56.95 + 11.29 s | — | idle |
| — | other contention sites above 1.26 s: `diameter.go:203` 2.28 s (worker waits for jobs), `serve.go:1562` 2.18 s (connection `select`), `isolationtest` runner 1.84 s (test harness), `txn.go:2228` `waitApplyTurn` 1.71 s, `mvcc_vacuum.go:755` 1.53 s (backoff timer) | — | idle or ordered-apply wait; unchanged from iteration 3 (2.37, 2.28, 1.80, 1.86, 1.49 s) |
| — | algorithm kernels (flat, no allocation): Brandes 52.22 s, PageRank `runRange` 31.81 s, `dijkHeap.pop` 12.54 s, Johnson 8.91 s, `dijkstraCore` 7.55 s, BFS row 33 3.44 s, `HungarianCtx` 2.88 s, `bfsScratch.run`/`runHarmonic` 2.76/2.74 s; WAL group sync 8.43 s; WAL replay per `24_*`; row 20 search pools (#2382); copy-on-write adjacency `adjlist.go:971` 3.95 GiB (both shapes); bulk redo `txn.go:1379` 2.49 GiB; `johnsonPrepare` 1.23 GiB | — | inherent or tracked, as section 19 |

## 34. Final verdict (iteration 4)

- **Moved since iteration 3:** module allocation −3.0% (−3.21 GiB), all of it row 26 and all of
  it attributed to #3104 and #3106 at site level; module CPU and module contention delay inside
  the noise band. No regression.
- **Every last change was confirmed at sweep level** (section 31); #3106 and #3108 are each
  below the threshold on their own.
- **Verdict: the significance verdict of section 27 is NOT confirmed as stated.** Two module
  residuals are above the 1% allocation threshold and were never ranked or decided: **N1**,
  PageRank's per-call rank vectors (2.63 GiB, 2.50%; poolable scratch ≈ 1.31 GiB, 1.25%,
  hypothesised), and **N2**, the adjacency reverse-index appends (1.73 GiB, 1.64%; the growth
  share unmeasured). N3 (`pairRun`, 1.21% CPU) is above the threshold as a site, but its
  hypothesised remedy is below it. Every other residual is below the threshold, inherent, idle,
  user-dropped (U1, U5, U7) or in the backlog (S3–S6, #3098–#3101).

## 35. Limits (iteration 4)

- A/B covers rows 26, 37, 08, 23 and the peak RSS of rows 10 and 32 only; every other row is
  one run per pass.
- #3108's instrumented figure (waiters' delay) is not reproducible with pprof's mutex profile,
  which charges the delay to the unlocking stack; the sweep verifies direction only.
- N1's and N2's remedies and gains are hypotheses; N2's content-versus-growth split and N3's
  three-search cost per call are read from profiles, not measured by a single-variable change.

## 36. Reproduction (iteration 4)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
$L4/sweep.sh            # default (LAB_REBUILD=1), elevated, rss.sh, contention
$L4/repeat.sh           # interleaved A/B rows 26 and 37 (A = $L3/bin, B = $L4/bin)
$L4/repeat2.sh          # interleaved A/B rows 08, 23 (profiles) and 10, 32 (peak RSS)
$L4/owner/owner $L4/out/elevated/*/{cpu,heap}.pprof > $L4/owner.elevated.tsv
$L4/owner/owner $L4/out/contention/*/*.pprof       > $L4/owner.contention.tsv
$L4/owner/owner $L4/repeat/*/*.pprof               > $L4/owner.repeat.tsv
$L4/owner/owner $L4/repeat2/*/{cpu,heap}.pprof     > $L4/owner.repeat2.tsv
python3 $L4/compare4.py <scratchpad>                # section 30 totals, per-row table
python3 $L3/fn.py '<function regex>' $L3/owner.elevated.tsv $L4/owner.elevated.tsv   # section 31
python3 $L4/linediff.py alloc_space line $L3/owner.elevated.tsv $L4/owner.elevated.tsv 900
python3 $L4/agg.py $L4/owner.elevated.tsv cpu fn 40
python3 $L4/agg.py $L4/owner.contention.tsv delay line 30
python3 $L4/agg.py $L4/owner.repeat.tsv {cpu,alloc_space,delay} summary
```

# Laboratory iteration 5, 2026-10-12 — final confirmation sweep at `c52a6380`

Sprint 367, rmp #3113. The iteration-4 sweep was re-run at HEAD `c52a6380` (module code changed
since `ffeebf23` only in `search/centrality/pagerank.go`, #3111 `29fb8dc5`; `c52a6380` adds a
benchmark only) with the same driver, passes, examples, scales and owner reader, so every row
compares 1:1 with section 30. **No module code was changed.** Every number below was collected
in this session.

## 37. Method and environment (iteration 5)

- Same as section 29, with these differences: `$L5` =
  `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab5`;
  binaries rebuilt (`LAB_REBUILD=1`); graph data under `/Volumes/GoGraphRAM/lab3113` (deleted at
  the end); `GOCACHE` default (on disk).
- Host: Apple M4, 10 cores, Darwin 27.0.0 arm64, Go 1.27.2, tree clean,
  `GOGRAPH_FSYNC_LATENCY=off`. Load1: start 2.23 (top processes iTerm 21%, `osascript` 13% + 12%,
  `sysmond` 13%, `claude` 13%, WindowServer 11% of one core); `default` 18:17–18:23 (to 3.04);
  `elevated` 18:23–18:31 (2.40); peak RSS 18:31–18:37 (2.88); `contention` 18:37–18:41 (3.33);
  A/B repeats 18:41–18:47 (3.05–6.48; row 20 saturates the cores itself). Every row ran alone
  within its pass.
- **A/B**, interleaved, A = the iteration-4 binaries (`ffeebf23`), B = HEAD: `$L5/repeat5.sh`
  ran row 20 (3 + 3, CPU and heap) and the peak RSS of rows 16 and 24 `init` + `seed`
  (3 + 3, `/usr/bin/time -l`); `$L5/repeat6.sh` ran rows 11, 19, 35, 36 (3 + 3, CPU and heap)
  and 37 (3 + 3, `-contention 1`).
- **Noise.** As section 29: a single-run per-row delta under ±10% is not a finding; rows under
  1 s of module CPU are judged on A/B only.

## 38. Totals `c794a1ed` → `6b399a3b` → `7b8120df` → `ffeebf23` → `c52a6380`

| measure (`elevated` sweep unless stated) | iteration 1 → 2 → 3 → 4 → 5 | iteration 5 vs 4 | vs iteration 1 |
|---|---|---|---|
| module CPU | 383.88 → 249.06 → 233.24 → 232.74 → **237.77 s** | +2.2% (no row moved under A/B, section 40) | −38.1% |
| module allocation | 144.47 → 119.13 → 108.38 → 105.17 → **103.20 GiB** | **−1.9% (−1.97 GiB)** | −28.6% |
| module contention delay (`contention` pass) | 274.86 → 263.05 → 127.99 → 126.17 → **136.44 s** | +8.1% (row 37 draw; flat under A/B, section 40) | −50.4% |
| total CPU | 595.56 → 443.90 → 429.80 → 428.03 → 436.41 s | +2.0% | −26.7% |
| total allocation | 160.96 → 135.67 → 125.22 → 121.89 → 119.89 GiB | −1.6% | −25.5% |
| harness / unowned CPU | 54.57 / 157.11 → … → 55.51 / 139.78 → 55.99 / 142.65 s | +0.9% / +2.1% | — |

Row 20 carries the allocation drop: module 17.21 → 15.61 GiB (−1.60 GiB). Exit status: every
`default` row exited 0 (10 `cpu-total-zero` instrument FAILs: the iteration-4 set less row 08);
`elevated` row 31 exits 1 on the same planner-statistics harness check as iterations 1–4,
`24_init` `cpu-total-zero`; every `contention` row exited 0. Repeat `37_B1` exited 1 on the
intermittent `ladder.L22 level=8 crash_image_after_checkpoint` check of iterations 3–4 and
stopped early (4.84 GiB against 7.14–7.50 GiB in the other runs); it is excluded from the A/B.
No row showed a module defect. Per-row table: `$L5/compare5.md`.

## 39. Verification of #3111 at sweep level

| claim (paired, example 20) | sweep (`elevated`, owner reader) | interleaved A/B, row 20 | verdict |
|---|---|---|---|
| scratch allocation 1.35 → 0.62 GB | scratch `pagerank.go:287` **1 325.6 MiB → `acquirePageRankScratch` 654.8 MiB**; result vector `:286` 1 366.2 → `:297` 1 391.4 MiB (unchanged, caller-owned); all of `pagerank.go`, every row: 3 864.4 → 3 153.5 MiB (row 20 3 597.0 → 2 900.6, row 08 264.6 → 249.5) | `PageRankCtx` + `acquirePageRankScratch` 3 661.4 → 2 904.9 MiB per run (**−756.5 MiB, −20.7%**); scratch 1 355.5–1 424.4 → 604.7 / 626.1 / 650.6 MiB; row 20 module alloc 17 399.6 / 17 327.0 / 17 448.9 → 15 888.2 / 15 684.0 / 16 219.7 MiB (**every B below every A**, −1.46 GiB mean) | **confirmed** |
| no ns/op regression | row 20 module CPU 53.84 → 59.18 s (+9.9%, single run) | module CPU 61.83 / 62.85 / 61.06 → 61.94 / 62.61 / 64.27 s (ranges overlap); throughput 49 / 97 reads/s (1 / 2 workers) in all six runs | **no regression** |

Of row 20's −1.46 GiB per run, −0.74 GiB is PageRank; the other −0.71 GiB is in the search
pools whose code did not change (`BFSCtx` −316.7, `acquireDijkstra` −256.0, `dijkHeap.push`
−137.7 MiB per run). **Hypothesis, not established:** fewer collections let more pooled
buffers survive between calls.

## 40. Rows flagged by the sweep, repeated

| row | sweep, iteration 4 → 5 | interleaved repeat A (`ffeebf23`) → B (HEAD) | verdict |
|---|---|---|---|
| 20_concurrent_reads | module CPU +9.9% | section 39 | not a regression |
| 19_pattern_query | module CPU 2.87 → 4.04 s (+40.8%) | 4.15 / 3.91 / 4.98 → 4.03 / 4.22 / 3.93 s | not a regression |
| 35_mvcc_mixed_workload | module CPU 2.33 → 3.14 s (+34.8%) | 3.00 / 2.79 / 3.50 → 2.81 / 2.35 / 2.84 s | not a regression |
| 36_mvcc_snapshot_topology | module CPU 1.20 → 1.45 s (+20.8%) | 1.62 / 1.52 / 2.16 → 1.63 / 1.71 / 1.35 s | not a regression |
| 11_social_network | module CPU 1.95 → 2.34 s (+20.0%) | 2.28 / 2.08 / 2.17 → 2.17 / 2.15 / 2.30 s | not a regression |
| 37_mvcc_write_contention | module contention 33.43 → 42.63 s (block + mutex) | 41.39 / 41.92 / 36.77 → 41.21 / 41.94 s (B1 excluded, section 38); module CPU 3.87 / 3.77 / 3.77 → 3.74 / 3.77 s | not a regression (iteration 4's 33.43 s was a low draw) |
| 16_centrality_analytics | peak RSS 26 → 28 MiB | 26 / 29 / 29 → 27 / 28 / 28 MiB | not a regression |
| 24_seed | peak RSS 1 818 → 2 058 MiB | 1 722 / 1 818 / 2 125 → 1 718 / 2 270 / 2 123 MiB | spread in both arms; not a regression |
| 10_dimacs9_routing | peak RSS 387 → 243 MiB | not repeated | the bimodal row of section 32 |

Rows under 1 s of module CPU that moved more than ±10% (01, 02, 05, 12, 14, 17, 18, 22, 24_snapshot,
27, 32, 34) do not execute `search/centrality`, the only module code changed. **No row regressed.**

## 41. Residual ledger at HEAD

**Threshold** (recomputed on this sweep): 1% of module CPU = **2.38 s**, of module allocation =
**1.03 GiB**, of module contention delay = **1.36 s**.

| # | site (row) | cost at HEAD | decision |
|---|---|---|---|
| N1 | PageRank scratch vector (20) | 654.8 MiB (0.62%) | **fixed** (#3111, section 39) |
| N1 | PageRank result vector `pagerank.go:297` (20) | 1.36 GiB (1.32%) | **inherent**: the returned, caller-owned vector (#3111) |
| N2 | `adjlist.(*revIndex).add` `reverse.go:134` + `:130` | 1.71 GiB (1.66%): 1 167.1 + 585.4 MiB | **inherent** (#3112: doubling growth; two chunked layouts regressed hub DETACH DELETE and in-cache inserts and were reverted; `c52a6380` adds the benchmark only) |
| N3 | `lpg.(*sourceSlots).pairRun` (26) | 2.03 s (0.85%) | **below threshold** |
| **U8** | `exec.NewChunk` `chunk.go:176` (37) | **1.11 GiB (1.08%)**; repeats 1.08–1.14 (A), 1.03 / 1.10 GiB (B) | **above threshold**; decided "below threshold" at iteration 3's 1.08 GiB (§19, §26); not re-decided |
| **Q12** | `lpg.appendSuspects` `mvcc_index.go:944` (37), shard `RLock` walk | **1.57 s block (1.15%)**; repeats 1.54 / 1.36 / 1.46 (A), 1.46 / 1.71 s (B); iteration-4 repeats 1.54–2.05 s | **above threshold**; decided "insignificant" against iteration 2's threshold (§13, §26); not re-decided |
| U1, U7 | id boxing (`expand.go:1284–1286` 3.41 GiB, `scan_label.go:206` 1.43, `scan_all.go:149` 1.02); `lpg.StringValue` 1.17 GiB | 7.03 GiB | **user-dropped** (§26) |
| U5 | id → key → id (26) | `Mapper.Lookup` 1.47 + `Mapper.Resolve` 0.94 s cumulative (2.41 s, 1.01%) | **user-dropped** (#3105) |
| U2 | `buildReadPhysical` (35) | `api.go:2811` 1.73 GiB flat | **backlog** S3–S6, #3098–#3101 |
| U3 | pair/type column builds (26) | — | **inherent** (#3103) |
| U6 | `mvcc.NewCommitInfo` | 1.49 GiB (1.44%) | **inherent** |
| U10 | row 37 shard lock `property.go:391/358`, `snapshot_read.go:217` | 7.85 s mutex, 5.50 + 4.43 s block | **inherent** (#3091) |
| — | `wal.syncToLocked` 1.62 s, `txn.waitApplyTurn` 2.21 s (37) | — | **inherent**: group commit and ordered apply (§4) |
| U11, U12 and idle waits | vacuum `select` 17.28 s; PageRank parked workers `pagerank.go:925/989` 57.27 + 11.46 s; `diameter.go:203` 2.31 s; `serve.go:1562` 2.24 s; `mvcc_vacuum.go:755` 1.66 s; `isolationtest` runner 1.79 + 1.40 s (test harness) | — | **idle** |
| — | WAL replay per `24_*` (`recovery.go:2207` 4.47 GiB, `readFramePayload` 2.57, `wal.Decode` 1.49, `stringCodec.Decode` 1.48, `decodeV3` 1.26 GiB); row 20 search pools (#2382: `search.go:164` 4.07, `dijkstra.go:100` 2.15, `:463/:465` 1.83 + 1.87, `:505` 1.71 GiB); copy-on-write adjacency `adjlist.go:971` 2.77 + 1.15 GiB; bulk redo `txn.go:1379` 2.49 GiB; `johnsonPrepare` 1.23 GiB; kernels (Brandes 50.36 s, PageRank `runRange` 33.69, `dijkHeap.pop` 13.70, `dijkstraCore` 8.88, Johnson 8.84, WAL group sync 8.62, BFS row 33 3.35, `HungarianCtx` 2.82, `bfsScratch` 2.68 / 2.70 s) | — | **inherent or tracked**, as sections 19 and 33 |

## 42. Final verdict (iteration 5)

- **Moved since iteration 4:** module allocation −1.9% (−1.97 GiB), carried by row 20 and
  attributed at site level to #3111; module CPU and module contention delay rose in the single
  sweep run, and every row that moved was flat under interleaved A/B. **No regression.**
- **#3111 is confirmed sweep-wide** (section 39). N2 is decided inherent (#3112).
- **Verdict: insignificant returns are NOT confirmed as stated.** Two residuals previously
  decided "below threshold" are now marginally above the 1% thresholds recomputed on HEAD's
  smaller totals: **U8** `exec.NewChunk` (1.11 GiB, 1.08%) and **Q12** `lpg.appendSuspects`
  (1.57 s block, 1.15%). Every other residual is fixed, inherent, idle, below threshold,
  user-dropped or in the backlog.

## 43. Limits (iteration 5)

- A/B covers rows 11, 16, 19, 20, 24 `init` + `seed`, 35, 36 and 37; every other row is one run
  per pass. Row 37's A/B has two B runs (section 38).
- The threshold is 1% of a total that shrinks as fixes land, so an unchanged site can cross it;
  U8 and Q12 did not grow (U8 1.08 → 1.11 GiB across iterations 3–5, Q12 1.81 s at iteration 2).
- The search-pool share of row 20's drop (section 39) is attributed to no mechanism.

## 44. Reproduction (iteration 5)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
# data dirs -> /Volumes/GoGraphRAM/lab3113 through symlinks, as section 1
$L5/sweep.sh            # default (LAB_REBUILD=1), elevated, rss.sh, contention
$L5/repeat5.sh          # interleaved A/B row 20 (profiles), rows 16 and 24 init+seed (peak RSS)
$L5/repeat6.sh          # interleaved A/B rows 11, 19, 35, 36 (profiles) and 37 (-contention 1)
$L5/owner/owner $L5/out/elevated/*/{cpu,heap}.pprof   > $L5/owner.elevated.tsv
$L5/owner/owner $L5/out/contention/*/*.pprof          > $L5/owner.contention.tsv
$L5/owner/owner $L5/repeat/20_*/{cpu,heap}.pprof      > $L5/owner.repeat.tsv
$L5/owner/owner $L5/repeat/{11,19,35,36,37}_*/*.pprof > $L5/owner.repeat6.tsv
python3 $L5/compare5.py <scratchpad>                  # section 38 totals, per-row table
python3 $L5/linediff.py alloc_space line $L4/owner.elevated.tsv $L5/owner.elevated.tsv 200
python3 $L5/agg.py $L5/owner.elevated.tsv cpu fn 40
python3 $L5/agg.py $L5/owner.contention.tsv delay line 30
python3 $L5/agg.py $L5/owner.repeat6.tsv {cpu,alloc_space,delay} summary
```

# Campaign close, 2026-10-12

## 45. Final significance verdict

Section 42 left two residuals marginally above the 1% thresholds recomputed at `c52a6380`
(module CPU **2.38 s**, allocation **1.03 GiB**, contention delay **1.36 s**). Both were worked
and measured in paired example-37 runs.

| # | site | before | after (paired, example 37) | fix | decision |
|---|---|---|---|---|---|
| **U8** | `exec.NewChunk` | 1.11 GiB (ledger); 1.1–2.2 GiB paired | **0.09–0.15 GiB** | #3114 `3e2f9a2d`: pooled node-id chunks for counted label scans; small filtered count −51% time, −95% B/op | **below threshold** (1.03 GiB) |
| **Q12** | `lpg.appendSuspects` | 1.57 s block (ledger) | block delay **−35%** on the life shards; CPU **−51%** | #3115 `82c0dfb5`: per-shard emptiness flags skip the 92% of shards holding nothing | **below threshold**: ≈1.02 s (derived) against 1.36 s |

- **Q12 is a derived estimate.** ≈1.02 s is the paired −35% applied to the 1.57 s ledger
  figure, not a full-sweep re-measurement. The remaining label-shard wait is inherent writer
  overlap.
- **Verdict:** every residual in section 41 is now fixed, inherent, idle, below threshold,
  user-dropped, or in the backlog (S3–S6, #3098–#3101). **Returns are insignificant; the
  iterative campaign stops.**
- **Limit:** no sweep was run after #3114 and #3115; their effect is established by paired
  example-37 runs only.

## 46. Cross-component result

The synergy evidence is section 24: 13 cross-component chains, each with its fix and measured
after-value. Module totals `c794a1ed` → `c52a6380` (section 38): CPU 383.88 → 237.77 s
(**−38.1%**), allocation 144.47 → 103.20 GiB (**−28.6%**), contention delay 274.86 → 136.44 s
(**−50.4%**).

## 47. Sprint-close gate

- **`make ci` at `7b2fd112`:** every package passed — race detector, openCypher TCK
  **3897/3897**, crash/recovery battery, kgverify **20/20**.
- **`test-short` exited 2** only on hard per-package time ceilings that predate the sprint:
  `internal/sim` 1658.8 s > 780 s, `cypher` 559.6 s > 420 s, `bench/csrorder` 247.2 s > 240 s.
  Tracked in backlog #3067; per CLAUDE.md, test-time budgets do not gate.
- **Test-only gate fixes at close:**

| task | commit | defect |
|---|---|---|
| #3116 | `3e6c7024` | schema-walk gate blind after the layout memo |
| #3117 | `bbbee294` | free-space floor measured the RAM drive instead of the repository volume |
| #3118 | `7b2fd112` | contention ranking compared without a tie tolerance |

- **Merge:** sprint 367 merged into `develop` at `4d1947e8`.

# Laboratory iteration 6, 2026-10-12 — post-fix confirmation sweep at `8b089715`

The iteration-5 sweep (sections 37 and 44) was re-run at HEAD `8b089715` to replace the derived
Q12 estimate of section 45 with a measurement. Module code changed since `c52a6380` only in
#3114 `3e2f9a2d` (`cypher/exec`: pooled node-id chunks) and #3115 `82c0dfb5` (`graph/lpg`:
per-shard emptiness flags in `appendSuspects`). **No module code was changed.** Every number
below was collected in this session.

## 48. Method and environment (iteration 6)

- Same driver, rows, scales, flags and owner reader as section 37, with these differences:
  `$L6` = `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/lab6`;
  binaries rebuilt (`LAB_REBUILD=1`, 37 binaries, 26 s, outside every timed window); graph data
  under `/Volumes/GoGraphRAM/lab6sweep` (deleted at the end); `GOCACHE` default (on disk);
  `GOGRAPH_FSYNC_LATENCY=off`.
- **Passes run:** `elevated` (CPU + heap) and `contention`. The `default` pass and the peak-RSS
  pass were not run (section 51).
- Host: Apple M4, 10 cores, Darwin 27.0.0 arm64, Go 1.27.2, tree clean. Load1: start 1.89 (top
  processes `osascript` 37% + 11%, iTerm 21%, `sysmond` 11%, `claude` 10% of one core);
  `elevated` 23:20–23:29, per-row load1 1.83–3.87; `contention` 23:29–23:33, per-row load1
  2.54–5.25 (row 37 started at 4.97); row-37 A/B 23:33–23:34, load1 4.67–5.60; row-26 A/B
  23:36–23:42, load1 2.55–3.14. At 23:35, after the row-37 A/B, `XProtectRemediator*` processes
  were measured at 99% + 60% of a core; they were gone at 23:36. Their start time is unknown, so
  the `contention` tail and the row-37 A/B may overlap them (section 51). Every row ran alone
  within its pass.
- **A/B**, interleaved, A = the iteration-5 binaries (`c52a6380`), B = HEAD: `$L6/sweep6.sh`
  ran row 37 (3 + 3, `-contention 1`: CPU, heap, block, mutex); `$L6/repeat26.sh` ran row 26
  (3 + 3, CPU and heap) to judge the row-26 sites that moved (section 50). Every run exited 0.
- **Noise.** As section 37: a single-run per-row delta under ±10% is not a finding.
- **Exit status** (read from the manifests): `elevated` row 31 exits 1 on the same
  planner-statistics harness check as iterations 1–5 (identical message), `24_init`
  `cpu-total-zero`; every other `elevated` row and every `contention` row PASS.

## 49. Totals `c794a1ed` → `6b399a3b` → `7b8120df` → `ffeebf23` → `c52a6380` → `8b089715`

| measure (`elevated` sweep unless stated) | iteration 1 → 2 → 3 → 4 → 5 → 6 | iteration 6 vs 5 | vs iteration 1 |
|---|---|---|---|
| module CPU | 383.88 → 249.06 → 233.24 → 232.74 → 237.77 → **233.10 s** | −1.96% | −39.3% |
| module allocation | 144.47 → 119.13 → 108.38 → 105.17 → 103.20 → **102.33 GiB** | −0.85% (−0.87 GiB) | −29.2% |
| module contention delay (`contention` pass) | 274.86 → 263.05 → 127.99 → 126.17 → 136.44 → **134.83 s** | −1.17% | −50.9% |
| total CPU | 595.56 → 443.90 → 429.80 → 428.03 → 436.41 → 428.91 s | −1.72% | −28.0% |
| total allocation | 160.96 → 135.67 → 125.22 → 121.89 → 119.89 → 119.14 GiB | −0.63% | −26.0% |
| harness / unowned CPU | 54.57 / 157.11 → … → 55.99 / 142.65 → 55.77 / 140.04 s | −0.4% / −1.8% | — |

**Thresholds** (recomputed on this sweep): 1% of module CPU = **2.33 s**, of module allocation =
**1.02 GiB**, of module contention delay = **1.35 s**. Row 37 module allocation 7.16 → 6.47 GiB
(−9.6%) carries the allocation drop. Per-row table: `$L6/compare6.md`.

## 50. U8 and Q12 measured; residual ledger at HEAD

**U8** `exec.NewChunk` (#3114):

| source | `c52a6380` | `8b089715` |
|---|---|---|
| sweep, `elevated`, all rows | 1 141.4 MiB (row 37 1 127.5) | **143.8 MiB = 0.14 GiB (0.14%)** (row 37 135.0) |
| row-37 A/B, per run | A 1 234.1 / 1 177.2 / 1 171.2 MiB | B 148.0 / 91.3 / 144.9 MiB |

U8 is **below threshold, confirmed by measurement**; every B run is below every A run.

**Q12** `lpg.appendSuspects` (#3115), row 37. #3115 moved the label-shard `RLock` from
`mvcc_index.go:944` to `:947`; the life-shard `RLock` from `:954` to `:960`; `:971`/`:975` are
the `idxDeferred` mutex `Lock`/`Unlock`.

| measure | `c52a6380` (section 41) | `8b089715` sweep | A/B per run, A (`c52a6380`) → B (HEAD) |
|---|---|---|---|
| label-shard walk (`:944` → `:947`), block — the site section 41 ledgered | 1.57 s | **1.16 s (0.86%)** | 1.97 / 1.67 / 1.81 → 1.36 / 1.13 / 1.34 s |
| life-shard walk (`:954` → `:960`), block | 0.53 s | 0.44 s | 0.43 / 0.26 / 0.80 → 0.58 / 0.49 / 0.22 s |
| `idxDeferred` mutex (`:965/:969` → `:971/:975`), block + mutex | 0.01 + 0.01 s | 0.29 + 0.40 s | 0.16 + 0.09 / 0.11 + 0.16 / 0.05 + 0.05 → 0.05 + 0.05 / 0.06 + 0.06 / 0.95 + 1.00 s |
| whole function, block + mutex | 2.21 s | **2.38 s (1.77%)** | 2.75 / 2.28 / 2.82 → 2.15 / 1.83 / 3.58 s |

- The ledgered site (label-shard walk) fell: every B run is below every A run, and the sweep
  figure is 1.16 s against 1.35 s. The repeats are 1.13–1.36 s: one of three is at the
  threshold (+0.01 s). The ≈1.02 s derived in section 45 was not reproduced; the measured figure
  is 1.16 s.
- The whole function is **above threshold** (2.38 s). Its `idxDeferred` mutex share is bimodal:
  0.01–0.25 s in most runs, about 2 s in one run per arm (iteration 5 B3 1.03 + 1.01 s, this
  A/B B3 0.95 + 1.00 s). The life-shard reduction stated in section 45 (−35%) is not visible
  in this A/B (A 0.26–0.80 s, B 0.22–0.58 s; ranges overlap).

**Ledger re-check** against the section-49 thresholds (sweep figures at HEAD; section 41 for
the iteration-5 values):

| # | site (row) | cost at `8b089715` | vs threshold | decision |
|---|---|---|---|---|
| N1 | PageRank scratch `pagerank.go:423` / result vector `:297` (20) | 629.3 MiB / 1 406.7 MiB (1.34%) | scratch below; result above | fixed / inherent (unchanged) |
| N2 | `revIndex.add` `reverse.go:134` + `:130` | 1 181.7 + 620.7 MiB = 1.76 GiB (1.72%) | above | inherent (#3112, unchanged) |
| **N3** | `lpg.(*sourceSlots).pairRun` (26) | **2.59 s (1.11%)**; A/B A 2.70 / 2.81 / 2.74, B 2.19 / 2.56 / 2.59 s | **above** (5 of 6 repeats ≥ 2.33 s) | decided "below threshold" at 2.03 s (section 41), a low draw: the code is unchanged and the `c52a6380` binaries measure 2.70–2.81 s; **not re-decided** |
| U8 | `exec.NewChunk` (37) | 0.14 GiB | below | **fixed** (#3114, confirmed) |
| Q12 | `appendSuspects` label-shard walk (37) | 1.16 s; repeats 1.13–1.36 s | below in the sweep; marginal in repeats | fixed (#3115) |
| **Q12′** | `appendSuspects`, whole function (37) | **2.38 s (1.77%)** | **above** | never ledgered at function level (2.21 s at iteration 5); **not decided** |
| **Q11** | `reserveSetPropertyGen` `constraints.go:1269–1270` (37) | **1.21 s block + 1.28 s mutex = 2.49 s (1.85%)**; A/B A 1.29 / 2.14 / 1.93, B 2.60 / 1.44 / 1.64 s | **above** (each line alone is below) | decided "insignificant" at 1.11 + 1.10 s (section 13); absent from section 41; iteration 5 already measured 2.19 s; **not re-decided** |
| U1, U7 | id boxing `expand.go:1284–1286` 3.45 GiB, `scan_label.go:206` 1.43, `scan_all.go:149` 1.00; `lpg.StringValue` 1.17 GiB | 7.05 GiB | above | user-dropped (unchanged) |
| **new** | `exec.(*Expand).buildRow` flat CPU (26, 24_plandiff, 25) | **3.12 s (1.34%)**; row 26 A/B A 1.86 / 1.70 / 1.86, B 1.70 / 2.05 / 1.74 s | **above** (2.36 s at iteration 5, 0.99%) | **not decided**; hypothesis, not established: the CPU side of U1 (same function) |
| U5 | `Mapper.Lookup` + `Mapper.Resolve` (26) | 1.34 + 1.15 = 2.49 s cumulative (1.07%) | above | user-dropped (unchanged) |
| U2 | `buildReadPhysical` `api.go:2811` (35) | 1.67 GiB | above | backlog S3–S6 (unchanged) |
| U6 | `mvcc.NewCommitInfo` | 1.45 GiB (1.41%) | above | inherent (unchanged) |
| U10 | shard lock `property.go:391` / `:358`, `snapshot_read.go:217` (37) | 7.13 s mutex, 5.02 + 4.88 s block | above | inherent (unchanged) |
| — | `txn.waitApplyTurn` 2.28 s, `wal.syncToLocked` 1.53 s (37) | — | above | inherent (unchanged) |
| U11, U12, idle | vacuum `select` 17.29 s; PageRank parked `pagerank.go:925/989` 57.12 + 11.39 s; `diameter.go:203` 2.47; `serve.go:1562` 2.16; `mvcc_vacuum.go:755` 1.75; `isolationtest` 1.71 + 1.37 s | — | — | idle (unchanged) |
| — | WAL replay per `24_*` (`recovery.go:2207` 4.47 GiB, `readFramePayload` 2.59, `wal.Decode` 1.52, `stringCodec.Decode` 1.52, `decodeV3` 1.32); search pools (`search.go:164` 3.83, `dijkstra.go:100` 2.18, `:463/:465` 1.79 + 1.82, `:505` 1.77 GiB); `adjlist.go:971` 2.71 + 1.17 GiB; bulk redo `txn.go:1379` 2.49 GiB; `johnsonPrepare` 1.23 GiB; kernels (Brandes 52.11 s, PageRank `runRange` 30.91, `dijkHeap.pop` 18.35, WAL group sync 8.50, `dijkstraCore` 7.60, BFS row 33 3.23, `dijkHeap.push` 3.08, `HungarianCtx` 2.88, `bfsScratch` 2.81 / 2.81 s) | — | — | inherent or tracked (unchanged) |

Row 28's CPU split moved with no change in `search/` since `c52a6380`: `johnsonDijkstraSource`
8.64 → 0.13 s flat, `dijkHeap.pop` 0.51 → 5.83 s, `dijkstraCoreWithWeights` 0.13 → 1.86 s,
`dijkHeap.push` 0.14 → 1.32 s; row 28 module CPU 9.43 → 9.20 s. The work is the same; only its
attribution moved. **Hypothesis, not established:** a different inlining decision in the rebuilt
binary.

No other module-owned site reaches a threshold: the next allocation sites are
`buildRelationshipValueFromRow` `api.go:17020` 0.99 GiB (0.97%), `setEdgeLabelSlotsAtTx`
0.94 GiB and `edgePropCols.clone` 0.93 GiB; every contention line not listed above is below
1.35 s.

## 51. Cross-component check (section 24 chains)

| chain (section 24) | residual figure, `c52a6380` → `8b089715` (sweep) | A/B | moved? |
|---|---|---|---|
| relationship materialisation (26) | `buildRelationshipValueFromRow` cumulative 5.73 → 7.67 s, 0.97 → 0.99 GiB; `slotLabelsForPair` 0.31 → 0.27 s; `GetEdgePropertyAsOf` 0.38 → 0.34 s; `buildSourceSlots` 1.25 → 1.59 s; U5 2.41 → 2.49 s | `buildRelationshipValueFromRow` A 7.28 / 7.79 / 7.43, B 5.22 / 7.32 / 7.77 s; row 26 module CPU A 37.95–40.13, B 37.62–38.83 s | **no** (the sweep's 5.73 s was a low draw) |
| CSR pair cache → type column (26) | `buildRelTypeColumn` cumulative 5.60 → 5.55 s | A 4.02–4.14, B 4.04–4.13 s (row 26 only) | no |
| MVCC edge write → edge-property columns | `edgePropCols.GrowSlot` 264.5 → 250.5 MiB | — | no |
| aggregation → node materialisation | `nodePropsToExprMap` 605.6 → 692.7 MiB, 0.36 → 0.33 s | — | no (below threshold, ±10% row noise) |
| cached read → physical build (35) | `buildReadPhysical` 1.73 → 1.67 GiB; `copySchema` 174.5 → 181.0 MiB | — | no |
| SET → constraint registry → shard locks (37) | Q11 2.19 → 2.49 s; U10 17.78 → 17.03 s | Q11 above | no move; Q11 above threshold (section 50) |
| MVCC reclaim → shard locks (37) | `reclaimPropVersions` 0.23 → 0.25 s | — | no |
| introspection and scan (25) | `PropertyKeysInUse` 0.54 → 0.50 s; `AllNodesScan.Init` 446.0 → 473.5 MiB | — | no |
| checkpoint → snapshot writer (17) | row 17 module allocation 0.17 → 0.19 GiB | — | no |
| csr projection → analytics (20, 09, 33) | transpose 0 → 0; rows 09 + 33 module allocation 3.12 → 3.12 GiB | — | no |
| csr projection → diameter (11) | `bfsFarthest` 1.96 → 2.03 s; row 11 module CPU 2.34 → 2.31 s | — | no |
| callers → PageRank workers (20) | parked block 68.73 → 68.51 s | — | no |
| WAL writer lifecycle → tests | not covered by the sweep | — | not measured |

**No chain moved.** The relationship-materialisation figure changed only in the single sweep
run; the interleaved A/B shows no difference between `c52a6380` and HEAD.

## 52. Verdict (iteration 6)

This section supersedes the verdict of section 45, which remains as written.

- **Moved since iteration 5:** module allocation −0.87 GiB (−0.85%), carried by row 37 and
  attributed at site level to #3114 (`NewChunk` 1 141.4 → 143.8 MiB). Module CPU (−1.96%) and
  module contention delay (−1.17%) are inside single-run noise. **No regression.**
- **U8 is confirmed below threshold by measurement** (0.14 GiB against 1.02 GiB).
- **Q12, the ledgered label-shard walk, is below threshold in the sweep** (1.16 s against
  1.35 s) and marginal in the repeats (1.13 / 1.34 / 1.36 s). The derived ≈1.02 s of section 45
  is replaced by the measured 1.16 s.
- **Verdict: insignificant returns are NOT confirmed by measurement.** Four module-owned
  residuals are above the 1% thresholds recomputed at HEAD, none decided against them:
  - **Q11** `reserveSetPropertyGen` (row 37): 2.49 s block + mutex (1.85%); above threshold in
    every A/B run.
  - **Q12′** `appendSuspects` whole function (row 37): 2.38 s (1.77%), of which the
    `idxDeferred` mutex is bimodal.
  - **`Expand.buildRow` flat CPU**: 3.12 s (1.34%).
  - **N3** `sourceSlots.pairRun` (row 26): 2.59 s (1.11%); its iteration-5 figure (2.03 s)
    was a low draw.
- None of the four grew because of a code change. N3 and Q11 do not move under A/B, and
  `buildRow` does not move on row 26. They cross the threshold because the threshold shrinks
  with the totals, iteration-5 draws were low, and per-line figures were ledgered where the
  site's sum is larger. Every other residual is fixed, inherent, idle, below threshold,
  user-dropped, or in the backlog.

## 53. Limits (iteration 6)

- Only the `elevated` and `contention` passes were run. The `default` pass, the peak-RSS pass
  and the coverage passes were not, so this iteration gives no exit-status or peak-RSS
  evidence for the `default` scales.
- A/B covers rows 26 and 37 (3 + 3 each); every other row is one run per pass.
- The `contention` tail and the row-37 A/B ran at load1 4.67–5.60. `XProtectRemediator`
  processes were seen at 23:35 with an unknown start time. Block delay is wall-clock waiting,
  so Q11, Q12 and Q12′ may be inflated in those runs. The A/B arms interleave, so the
  comparison between them is protected; the absolute figures are not.
- The threshold rule (1% of a shrinking total, one sweep run) puts sites within ±10% of the
  threshold on either side from draw to draw. N3, Q12 and `buildRow` are within that band.
- The row-28 attribution shift and the bimodal `idxDeferred` mutex wait are attributed to no
  mechanism.

## 54. Reproduction (iteration 6)

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
R=/Volumes/GoGraphRAM/lab6sweep
for p in elevated contention; do
  mkdir -p $R/$p/data-24 $R/$p/data-25 $L6/out/$p/25_software_house_api
  ln -sfn $R/$p/data-24 $L6/out/$p/data-24
  ln -sfn $R/$p/data-25 $L6/out/$p/25_software_house_api/data
done
mkdir -p $R/tmp; ln -sfn $R/tmp $L6/out/tmp
$L6/sweep6.sh           # elevated (LAB_REBUILD=1), contention, interleaved A/B row 37 (-contention 1)
$L6/repeat26.sh         # interleaved A/B row 26 (CPU and heap)
$L6/owner/owner $L6/out/elevated/*/{cpu,heap}.pprof   > $L6/owner.elevated.tsv
$L6/owner/owner $L6/out/contention/*/*.pprof          > $L6/owner.contention.tsv
$L6/owner/owner $L6/repeat/37_*/*.pprof               > $L6/owner.repeat.tsv
$L6/owner/owner $L6/repeat/26_*/{cpu,heap}.pprof      > $L6/owner.repeat26.tsv
python3 $L6/compare6.py <scratchpad> > $L6/compare6.md  # section 49 totals, per-row table
python3 $L6/q12.py $L6/owner.repeat.tsv $L6/owner.contention.tsv   # U8, Q12 per run
python3 $L6/chains.py                                  # section 51 figures, lab5 vs lab6
python3 $L6/agg.py $L6/owner.elevated.tsv {alloc_space line,cpu fn,cpu fncum} 40
python3 $L6/agg.py $L6/owner.contention.tsv delay line 40
rm -rf $R
```
