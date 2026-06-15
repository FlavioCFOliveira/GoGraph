# Performance & Resource-Consumption Audit — 2026-06-14

Read-only audit (no code changed) of every component and the seams between
them, to find headroom that gives GoGraph more response capacity. Conducted by
five parallel specialist auditors (rust-perf-engineer ×2, graph-theory-expert,
storage-engine-auditor, go-developer), grounded in code evidence (`file:line`)
and the existing benchmark baselines under `docs/benchmarks/`.

**Prime invariant for every finding:** performance must NOT regress, and
openCypher TCK (3897) + ACID must be preserved. Each task carries an explicit
no-regression / TCK / ACID acceptance gate.

All findings are recorded as atomic tasks in the `gograph` rmp roadmap, one task
per finding, across six themed sprints **S-PA1…S-PA6 (#188–#193)**.

> **Remediation status — addressed in v0.3.1 (2026-06-15).** The three top
> claims below were the audit-time state and have since been remediated: the
> optimizer now has a **hash join** for disconnected equi-join patterns (#1506)
> and a **range-predicate B+tree index seek** (#1505); **PageRank** runs a
> parallel pull-formulation over a reverse-CSR on large graphs, bit-identical to
> the serial path (#1513); and the WAL commit path now does **group commit**
> (#1507, ≈ 118× concurrent write throughput). See
> [docs/benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) (rows
> 0006–0016) and [release-notes/v0.3.1.md](../release-notes/v0.3.1.md). The
> findings text below is retained as the audit-time record.

## Independently verified top claims

- **The cost-based optimizer is dead code.** `cypher/ir/rewrite` is imported
  only by tests; `cypher/rewrite_not_wired_test.go` documents it as not wired.
  No `HashJoin`/`Cartesian` operator exists in `cypher/exec|plan` (non-test).
  Consequence: range predicates never use a B-tree index (full-scan + Filter),
  and disconnected MATCH patterns degrade to nested-loop products. Largest
  ceiling; highest TCK risk → spec-led.
- **PageRank is fully sequential** (`search/centrality/pagerank.go:191-199`, zero
  parallelism primitives) — the gap to the GraphBLAS ceiling (1.2× vs BFS 19.8×).
- **No group commit** — `store/txn/txn.go:989-1017` fsyncs once per transaction
  under the cap-1 writer semaphore; throughput is hard-capped at ≈1/fsync-latency
  regardless of core count.

## Findings by sprint

### S-PA1 — Regression-gate foundation (#188)
- **#1497** Add commit/WAL-path benchmarks (zero exist today; prerequisite to gate group commit).
- **#1498** Capture a fresh benchstat baseline for the perf cycle.

### S-PA2 — Cypher execution hot-path allocation (#189)
- **#1499** Column-oriented (SoA) result rows instead of per-row `map[string]Value` (ResultSet.Next 31.7% + Project.Next 22% of IC1 allocs).
- **#1500** Lazy/late node materialization for scalar projections (`upgradeNodeIDToValue` 15.2% + `NodeLabelsByID` 12.4%).
- **#1501** Remove `EvalWith` double-map build.
- **#1502** Eliminate the triple copy of property-map + labels across the lpg→expr→PackStream seam.
- **#1503** Lock-free metadata snapshot (stop per-property/per-label registry Resolve under RWMutex per node).

### S-PA3 — Cost-based optimizer activation (#190)
- **#1504** SPIKE: spec + design to wire the rewrite Driver / cardinality / scan-strategy with a TCK-safety argument.
- **#1505** Range-predicate B+tree index seeks (depends on #1504, #1514).
- **#1506** Hash-join operator for disconnected patterns (depends on #1504).
- **#1525** SPIKE: snapshot-pinned streaming of large result sets vs full materialization under the F3 isolation barrier (depends on #1500).

### S-PA4 — Storage write-throughput (#191)
- **#1507** Group commit / commit coalescing (depends on #1497). **Highest write-throughput lever.**
- **#1508** Non-blocking checkpoint (capture watermark under lock, snapshot lock-free, prefix-truncate WAL).
- **#1509** Remove double-alloc/double-copy per WAL frame; pool encode scratch (depends on #1497).
- **#1510** `fdatasync` for per-commit WAL sync on Linux.
- **#1511** Verify+adopt hardware CRC32C (single-pass).
- **#1512** Bulk loader: pre-size from MaxRows + partitioned parallel ingest.

### S-PA5 — Graph & search algorithm headroom (#192)
- **#1513** Parallelize PageRank SpMV (deterministic pull over reverse-CSR; expected 4–8×).
- **#1514** Replace sorted-array "btree" index with a real cache-friendly B+ tree.
- **#1515** Flatten Brandes predecessor structure into an arena (10–25%).
- **#1516** Public Dijkstra: validate weights once (drop O(E) rescan per call).

### S-PA6 — Bolt / IO / metrics request path (#193)
- **#1517** Wire the existing PackStream Encoder/Decoder pools (per-connection reuse; measured ~2.2× faster, ~9× less garbage per message).
- **#1518** Reuse per-connection response buffer + encoder in `sendResponse` (depends on #1517).
- **#1519** Lock-free Prometheus metrics hot path (interned handles + fast-path sanitize).
- **#1520** Reuse PULL row buffer on the streaming-sink path only.
- **#1521** Avoid full parameter-map copy per RUN (verify engine read-only first).
- **#1522** Decode PULL/DISCARD extra-map without materializing it (keep DoS budgets).
- **#1523** `csv.Write` reusable record + AppendInt.
- **#1524** Fix `metrics.Time` double-wrap in IO.

## Explicitly confirmed already at/near the ceiling (no action)

CSR layout; direction-optimising BFS with bitset frontiers; binary-heap Dijkstra
with `sync.Pool`; parallel-across-sources Brandes; Dinic O(E√V); Johnson-potential
min-cost max-flow; Roaring-bitmap label/hash indexes (no `map[NodeID][]Edge` on
any hot path). WAL durability core (poison-on-fsync-failure, marker-gated atomic
recovery, O(n) replay). Lock-free reads on the immutable snapshot (COW publish,
CSR not rebuilt per write). Bolt connection model (bounded, goleak-clean, context
honoured). PackStream decoder security budgets. IO readers (streaming, bounded,
`ReuseRecord`). Prior Cypher read-path wins (mapper shardFor, NodeID accessors,
skip-empty-propmap, materialize TakeRecord) intact.

## Sequencing

S-PA1 first (gates everything by making changes measurable). Then S-PA4 #1507
(group commit — biggest write lever) and S-PA5 #1513 (parallel PageRank — biggest
read-algorithm lever) and S-PA2 (steady alloc wins) can proceed in parallel.
S-PA3 (optimizer) is the largest ceiling but spec-led and gated on #1504.
