# Three-way head-to-head with the durability axis restored — 2026-07-27 (rmp #2223)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Neo4j 5.26-community and Memgraph 2.22.0 under Docker;
  GoGraph in-process and behind its own Bolt server.
- 20 000 `:Person` nodes, 199 941 `:KNOWS` edges, `UNWIND` batches of 5 000. Median of 9 after
  3 warm-ups. Dataset, batch size, warm-up and repeat counts are **unchanged** from round 3, so
  prior rounds stay comparable.
- Row counts cross-checked across all six targets and identical for every query — now **enforced**,
  not merely tabulated (see §4).
- Reproduce: `THREEWAY_NODES=20000 go test -tags=threeway -run TestThreeWay -v -timeout 60m ./bench/comparison/`

## 1. The defect this fixes

Round 3 compared a GoGraph with **no durability at all** against two engines writing a log, and
did not say so. `newEmbeddedTarget` built a bare `lpg.Graph` — no `store.DB`, no WAL, no fsync —
while Neo4j forces its transaction log on every commit. The distortion ran both ways: GoGraph's
write wins were overstated, and its traversal losses were **understated**, since it lost 30–107× on
reads while carrying none of the cost its rivals paid.

The harness now runs a fourth GoGraph target backed by a real `store.DB`, paired with the
in-memory one, so durability is *measured* rather than silently removed — the same discipline
`gograph-embedded` versus `gograph-bolt` already applied to transport.

## 2. Durability posture, stated

Every target's posture is now printed before any timing, because an unsourced durability claim is
what made the earlier comparison unfair.

| Target | Durability |
|---|---|
| `gograph-embedded` | **none** — in-memory `lpg.Graph`, no WAL, no fsync |
| `gograph-embedded-durable` | **per commit** — WAL append + fsync before the write is visible |
| `gograph-embedded[id]` | **none** — as above, numeric-key load variant |
| `gograph-bolt` | **none** — Bolt server over an in-memory engine; only transport differs |
| `neo4j-bolt` | **per commit** — transaction log forced on every commit (default) |
| `memgraph-bolt` | **every 100 000 tx** — default `storage_wal_file_flush_every_n_tx=100000` (memgraph/memgraph, `tests/e2e/configuration/default_config.py`); per-commit durability is opt-in |

## 3. The headline result: round 3's write win inverts

| Target | Durability | `q15_create` (single-node write) |
|---|---|--:|
| `gograph-embedded` | none | **5 µs** ← what round 3 reported |
| `gograph-embedded-durable` | per commit | **3.994 ms** |
| `neo4j-bolt` | per commit | 2.039 ms |
| `memgraph-bolt` | every 100 000 tx | 232 µs |

Round 4 recorded `q15_create` as **83× faster than the best rival**. At an equal durability
posture GoGraph is **~2× slower than Neo4j** (3.994 ms vs 2.039 ms). The 5 µs figure was an
in-memory mutation; the 3.994 ms figure is one fsync, and it agrees with the ~3.8 ms per-fsync cost
measured independently on this device in #2221. **The write win was a measurement artefact.**

Bulk load is the opposite case, and the distinction matters:

| Target | Load (20 000 nodes, 199 941 edges) |
|---|--:|
| `memgraph-bolt` | 987 ms |
| `gograph-embedded` | 2.056 s |
| `gograph-embedded[id]` | 2.064 s |
| `gograph-bolt` | 2.344 s |
| **`gograph-embedded-durable`** | **2.564 s** |
| `neo4j-bolt` | 4.054 s |

Durability costs the load only **+25%** (2.056 s → 2.564 s), not the ~800× it costs a single
commit, because the load commits in 5 000-row batches and one fsync is amortised across the whole
batch. GoGraph's load advantage over Neo4j therefore **survives** the correction (2.564 s vs
4.054 s at the same per-commit posture), while its single-write advantage does not. A comparison
that reports only one of those two facts is misleading either way.

## 4. Two further stale premises, both measured

**(a) The string-key join premise was stale — and the obvious correction is also wrong.** The
harness joined edges on the string key because "GoGraph's Cypher CREATE INDEX can only build
string-keyed indexes, so an integer join key would force a full label scan per row on GoGraph
alone". Verified with the physical `Engine.Explain` added in #2222:

- An **inline** numeric key does now reach an index: `MATCH (a:Person {id: 250})` lowers to
  `NodeByIndexRangeScan [range=250..250]` (#2169).
- But the harness's key is bound from an `UNWIND` row, not a literal, and in **that** shape
  *neither* key reaches a per-row index. Both lower to `NodeByLabelScan` feeding a `HashJoin`. On
  the write path `hashJoinBuildCount` fires exactly **2** for both the numeric and the string key
  while loading identical edges.

So the two keys are at parity, for a better reason than an index: the hash join (#2228) subsumes
the per-row lookup entirely for a bulk `UNWIND` load, turning *n* lookups into one scan per side.
Measured rather than asserted: the numeric variant loads in **2.064 s** against the string
variant's **2.056 s** — parity within noise.

**(b) Row counts were never actually cross-checked.** The task assumed they "still" were; the
harness only tabulated them and left the reader to notice a divergence. Two engines returning
different row counts are not running the same query, and a latency table over them compares
nothing. The harness now **fails** on any disagreement, before any timing is compared.

## 5. Baseline stability (AC 4)

The unchanged targets reproduce the round-4 figures, which is what proves the harness change did
not move the baseline:

| | round 4 | this run | |
|---|--:|--:|---|
| `memgraph-bolt` `q10_triangle` | 213 ms | 218 ms | +2.3% |
| `memgraph-bolt` load | 968 ms | 987 ms | +2.0% |
| `neo4j-bolt` `q10_triangle` | 363 ms | 394 ms | +8.5% |
| `neo4j-bolt` load | 3.47 s | 4.054 s | +17% (container variance) |
| `gograph-embedded` `q10_triangle` | 1.249 s | 1.214 s | −2.8% |
| `gograph-embedded` `q15_create` | 5 µs | 5 µs | exact |
| `gograph-embedded` `q12_multi_label` | 8 µs | 8/9 µs | exact |
| `gograph-embedded` `q02_range_scan` | 5.009 ms | 5.002 ms | exact |
| `gograph-embedded` `q09_top_k` | 30.94 ms | 30.52 ms | −1.4% |

Several GoGraph reads are **legitimately faster** than the round-4 baseline, and this is not
harness noise — it is the remediation completed in sprint 326 since that measurement:

| | round 4 | this run | cause |
|---|--:|--:|---|
| `load` | 35 m 10 s | **2.056 s** | #2228 (hash join admitted for writing statements) |
| `q04_one_hop` | 8.451 ms | 2.853 ms | #2225A / #2232 |
| `q11_expand_into` | 8.665 ms | 2.168 ms | #2225A |
| `q05_two_hop` | 13.378 ms | 7.404 ms | #2225A |
| `q13_shortest_path` | 24.51 ms | 13.115 ms | #2220 (bidirectional search) |

The control is the set of queries this sprint did **not** touch — triangle, top-k, range scan,
create, multi-label, point lookups — every one of which reproduces exactly or within 3%.

## 6. What still stands after the correction

The traversal losses are unaffected by durability (the read path does not fsync), so they stand
undiminished, and they are now the honest picture rather than a flattered one:

| Query | GoGraph (durable) | best rival | |
|---|--:|--:|---|
| `q10_triangle` | 1.215 s | 218 ms (Memgraph) | 5.6× slower |
| `q13_shortest_path` | 12.748 ms | 348 µs (Memgraph) | 37× slower |
| `q05_two_hop` | 6.942 ms | 235 µs (Memgraph) | 30× slower |
| `q09_top_k` | 32.699 ms | 3.384 ms (Neo4j) | 9.7× slower |

GoGraph still wins the shapes it was built for — `q12_multi_label` 8 µs against 899 µs,
`q01b_point_lookup_str` 5 µs against 280 µs, `q07_global_count` 561 µs against 1.147 ms,
`q14_property_filter` 4.63 ms against 2.49 ms — and those wins are now measured *with* durability
enabled, so they are real.
