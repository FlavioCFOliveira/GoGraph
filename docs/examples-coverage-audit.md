# Examples feature-coverage audit (2026-07-13)

This document records the coverage audit that answers one question: do the
examples under `examples/`, taken together, exercise the GoGraph module in
the **full plenitude** of its features — so that running them can surface
module weaknesses (functional completeness, implementation correctness,
resource efficiency) and certify production readiness?

The audit was performed at commit `1dc09ad` by four independent, read-only
specialist passes (Cypher/Bolt, search algorithms, storage/ACID, and the
remaining packages plus cross-cutting correctness), each grounded in the
source (`go doc`, `grep`, file reads) rather than memory, and cross-checked
against the module's own feature surface.

**Verdict:** the examples exercise the *core* model, single-source search,
clean-restart persistence, all four interchange formats, and read-only
Cypher/Bolt very well, with high API-correctness and clean read-concurrency.
They do **not** exercise the plenitude: entire capability families are
demonstrated by no example. The largest gaps are the **Cypher write path**,
**transactional isolation under concurrency**, the **observability/metrics**
surface, and roughly **24 of ~40 exported graph algorithms**.

The act of auditing already surfaced one fragility: the comment at
`examples/24_social_network_cli/cmd_seed.go:100` claimed the Cypher engine
*"cannot express multi-edge CREATE patterns nor MATCH+CREATE-relationship
statements through the WAL-backed planner."* An empirical probe against a
WAL-backed `cypher.Engine` disproved every part of that claim (multi-pattern
`CREATE`, `MATCH`+`CREATE`-relationship, idempotent `MERGE`, and `UNWIND`
batch `CREATE` all succeed and read back correctly). The comment was
corrected to state the real reason (bulk-load simplicity and single-
transaction atomicity), faithful to the code.

---

## Gap inventory (features implemented by the module, exercised by no example)

### A. Cypher engine — write path and advanced read (largest gap)

| Feature | Status | Notes |
|---|---|---|
| `MATCH`+`CREATE`-relationship, multi-pattern `CREATE` | NOT EXERCISED | Verified feasible over the WAL-backed engine. |
| `MERGE` (+`ON CREATE` / `ON MATCH`) | NOT EXERCISED | Idempotency never demonstrated. |
| `SET` / `REMOVE` / `DELETE` / `DETACH DELETE` (Cypher) | NOT EXERCISED | Only Go-API mutation is shown. |
| `UNWIND` | NOT EXERCISED | Verified feasible for batch writes. |
| `avg`/`min`/`max`/`percentileCont`/`percentileDisc`/`stDev` | NOT EXERCISED | Only `count`/`sum`/`collect` appear. |
| `EXISTS{}` / `COUNT{}` / `COLLECT{}` / `CALL{} IN TRANSACTIONS` | NOT EXERCISED | |
| `UNION` / `UNION ALL`, `CASE`, pattern comprehension | NOT EXERCISED | |
| `shortestPath` / `allShortestPaths` | NOT EXERCISED | Cypher-only construct. |
| `id()` / `elementId()` | NOT EXERCISED | |
| Temporal *functions* `date()`/`datetime()`/`duration()` + arithmetic | NOT EXERCISED | Values are stored via the Go API and only read/compared. |
| DDL: `CREATE INDEX` / `CREATE CONSTRAINT` / `DROP` | NOT EXERCISED | Consistency + index-seek performance undriven. |
| `EXPLAIN` (`Engine.Explain`) | NOT EXERCISED | No example proves an index is used. |
| `CALL db.*` introspection procs | NOT EXERCISED | labels / relationshipTypes / propertyKeys / indexes / constraints / schema. |

### B. Bolt protocol

Only auto-commit reads with `NoAuth` are driven (example 23). Unexercised:
explicit `BEGIN`/`COMMIT`/`ROLLBACK`, `ExecuteWrite`, `RESET`, `FAILURE`
classification, `BasicAuthHandler`, bookmarks, TLS + hot reload, `DISCARD`.

### C. Graph algorithms (~24 of ~40 exported are undriven by any example)

Negative-weight SSSP (`BellmanFord` + `ErrNegativeCycle`); all-pairs
(`FloydWarshall` / `JohnsonAPSP` / `DijkstraAPSP`); MST (`PrimMST` /
`KruskalMST`); Euler (`Hierholzer` directed + undirected); flow variants
(`EdmondsKarp`, `PushRelabelMaxFlow`, `MinCostMaxFlow`, `StoerWagner` global
min-cut); centralities `Closeness` / `Harmonic` / `Eigenvector` / `Katz` /
`WeightedBetweenness` / `PersonalisedPushPageRank`; structure `WCC` (+ `Parallel`)
/ `KCore` / `Diameter` / `CountTriangles` / `TransitiveClosure`;
bidirectional (`BiBFS`, `BidirectionalDijkstra`); k-shortest siblings
(`EppsteinKShortest`, `KShortestPathsLoopless`); `DFS`; every intra-query
`*Parallel` variant; and the zero-alloc `*Into` / `NewSSSP` performance API.

### D. Storage / ACID

Concurrent write-transaction **isolation** (multiple writers, reader-vs-writer)
is exercised only in example 25's *test* files, never in an example runtime;
`BeginReadTx` / `BeginTx` (`ExplicitTx`) are entirely undriven; **consistency**
(uniqueness/`CREATE CONSTRAINT`, index correctness, schema durability across
checkpoint via `WithConstraintSpecs`/`WithIndexSpecs`) has zero coverage;
deliberate **atomic rollback** is never demonstrated; a true cross-process
`kill -9` mid-write + recovery lives only in test files (example 17 simulates
a crash in-process); `store/bulk` is exercised by no example.

### E. Cross-cutting

The **metrics / observability** surface (209 latency sites + 681 counter sites
in the library; the public `metrics.NewPrometheusRegistry`/`SetBackend`
facade) is demonstrated by no example — the one CLAUDE.md mandate axis with
no example evidence. `graph/generation` has no caller anywhere in the module.
The standalone index family (`graph/index/btree` RANGE, `hash`, `label`) is
undriven directly. Sustained high-concurrency (the 1/8/64/256/1024-goroutine
load mandate) and at-scale concurrent writes in a runnable `run` are absent.

### F. Module feature gaps (not example gaps — the module does not implement these)

- `POINT` / spatial type and `point()` function: unimplemented.
- `PROFILE`: the `cypher/explain` package exists but is not exposed on the
  public `Engine` (`EXPLAIN` is, via `Engine.Explain`; `PROFILE` is test-only).
- `FOREACH` and general correlated `CALL {}` subqueries: intentionally
  unsupported (example 25's test pins the `FOREACH` rejection). Not gaps.

---

## Correctness / realism notes on the exercised set

The exercised algorithms run on shapes that make their results meaningful
(e.g. example 13 asserts max-flow == min-cut). API correctness is high: no
`interface{}` in hot paths; ignored errors are idiomatic best-effort. Minor
faithfulness nits: `examples/21_typed_recovery` discards the type-assertion
`ok` on typed-property reads; `examples/13_network_reliability` discards the
`Resolve` `ok`. Example 02's "index-backed query" claim is honest but refers
to `graph/query`'s internal roaring bitmaps, not the standalone `graph/index`
API (which no example touches).

The persistence examples certify durability under clean reopen well but do
not exercise isolation-under-concurrency or OS-level crash recovery, so
running them today would not surface an isolation or torn-write defect.

---

## Remediation plan

The gaps were closed as themed sprints in the `gograph` roadmap (271–276),
each example update honouring the [examples standard](examples-standard.md)
five-point rubric (seeded generator, scale knobs, subject-appropriate
evidence, deterministic facts vs `# ` telemetry, a pinning regression test).

## Outcome (2026-07-13)

All six sprints are closed. The examples grew from 26 to **34**:

- **Cypher write path & DDL** (sprint 271) — ex22 now drives the full write
  surface (multi-pattern `CREATE`, `MERGE` +`ON CREATE`/`ON MATCH`, `SET`,
  `REMOVE`, `DELETE`, `DETACH DELETE`, `UNWIND`) plus `shortestPath`
  cross-checked against `BiBFS`; ex25 adds `CREATE INDEX`/`CREATE CONSTRAINT`,
  a `CALL db.*` schema endpoint, and `EXPLAIN`, durable across restart; ex26
  adds aggregation breadth, subqueries, `CASE`, `UNION`, `id()`/`elementId()`
  and temporal functions.
- **Transactions & durability** (sprint 272) — new ex27 certifies isolation;
  ex17 adds a real cross-process `kill -9` recovery; ex04 a deliberate atomic
  rollback; ex18 the `store/bulk` loader.
- **Observability** (sprint 273) — new ex31 wires a Prometheus backend.
- **Algorithm coverage** (sprints 274–275) — new ex28 (Bellman-Ford), ex29
  (APSP), ex30 (MST), ex32 (Euler); ex13 gains WCC + flow variants; ex11 gains
  k-core/triangles/diameter/reachability; ex16 gains four centralities; ex14
  gains bidirectional solvers; ex08 gains personalised PageRank + the stateful
  `PageRanker`; ex20 certifies the intra-query parallel variants and raises the
  concurrency ceiling.
- **Bolt & generation** (sprint 276) — new ex34 (Bolt write/tx/auth/TLS), new
  ex33 (`graph/generation` MVCC), and faithfulness fixes in ex21/ex13.

Exercising the new coverage surfaced and fixed two stale-documentation defects
(the ex24 write-limitation note and the `elementId()` godoc) and filed five
verified module findings in the backlog: `NewEngineWithStore` dropping
recovered schema (`#1981`, documented contract), the silent index desync when
mixing raw `txn.Store` writes with engine indexes (`#1980`), the `CREATE INDEX
IF NOT EXISTS` placement divergence (`#1982`), the empty-graph index-key-type
sharp edge (`#1983`), and the `KShortestPathsLoopless` super-polynomial blowup
on spatial graphs (`#1997`). The compliance gates remain green throughout
(openCypher TCK 100%, `go test ./...` and `go test -race ./examples/...` all
passing).
