# Production-Readiness Audit — GoGraph — 2026-07-19

**Goal.** Determine whether the entire module is ready to operate flawlessly in
extremely demanding production environments.

**Scope.** A whole-module, multi-dimensional production-readiness evaluation
across the project's decision framework — **correctness → security → speed** —
plus the ACID and reliability/concurrency mandates. Baseline commit `8f25220`
(`main`; `v0.8.1` + 54 unreleased commits — the columnar value-model epic and the
2026-07 prod-readiness cycles sit on top of the last published release). This is
an **evaluation only**: no source was modified. `origin/main` is one commit behind
HEAD; the last published tag is `v0.8.1`.

**Method.** Five independent specialist sub-agents audited in parallel (with the
user's explicit prior authorisation, per CLAUDE.md), each read-only,
evidence-based, and adversarially self-verifying:

- `cypher-expert-consultant` — openCypher correctness & TCK conformance.
- `storage-engine-auditor` — ACID (atomicity, consistency, isolation, durability)
  & crash recovery.
- `security-researcher` — attack surface, untrusted-input parsing, DoS, auth.
- `concurrency-architect` — races, goroutine leaks, bounded resources, contention.
- `rust-perf-engineer` — hot-path allocation/CPU/cache methodology on the Go
  benchmarks.

The orchestrator ran the authoritative gate (`make ci`) and the crash-injection
battery, and synthesised the single verdict below. Every finding was required to
be traced to source (`file:line`) and adversarially confirmed before being
reported; plausible-but-unconfirmed items are marked as such.

---

## Baseline gates (@8f25220)

| Gate | Result |
|---|---|
| `make ci` (tidy · fmt · vet · build · `-race` short · lint · cover-gate) | **green** (real exit 0) |
| openCypher TCK (`TestTCKExecution`) | **3897/3897 scenarios, 0 failed / 0 undefined / 0 pending; 16006/16006 steps** |
| `go test -race ./...` (short layer, 111 packages) | **0 data races, 0 failures** |
| ACID crash-injection (`internal/crashinject`, `internal/crashpoint`, `store/recovery`) | **green** (`store/recovery` 53.9 s) |
| `golangci-lint run ./...` | 0 issues |
| Coverage gate | **aggregate 86.6 %** (≥ 85 %), every package ≥ 75 % |
| `govulncheck ./...` | **clean** — no vulnerabilities |
| `goleak` in every goroutine-spawning package | present |

**Verdict summary.** No CRITICAL or HIGH finding exists in any dimension. Every
hard gate is green. The module is **PRODUCTION-READY for extremely demanding
environments**, subject to a small set of non-blocking MEDIUM/LOW improvements
captured below.

| Dimension | Verdict | Blocking? |
|---|---|---|
| openCypher correctness | READY-WITH-CAVEATS | No |
| ACID / durability | READY-WITH-CAVEATS | No |
| Security | READY | No |
| Concurrency / reliability under load | READY | No |
| Performance | GOOD-WITH-OPPORTUNITIES | No |

---

## Findings (severity-ranked, all non-blocking)

### Correctness

- **C-1 — MEDIUM (actionable; only silent-wrong-result risk).** Runtime
  non-boolean operands of `AND`/`OR`/`XOR`/`NOT` are silently coerced instead of
  raising a `TypeError`. Confirmed empirically: `RETURN $x AND true` with `$x = 5`
  returns `true` (openCypher: type error); `AND` and `OR` coerce inconsistently.
  Reachable in production under schema drift / heterogeneous data (e.g.
  `MATCH (n) WHERE n.active AND n.verified` when `active` was stored as an
  integer). The TCK covers only *literal* boolean operands, so the 100 % gate does
  not catch it. Evidence: `cypher/expr/eval.go:930-991`; sema guard is
  literal-only (`cypher/sema/errors.go:196-202`). Fix: runtime guard in
  `eval3VLAND/OR/XOR` + unary `NOT` mapping a non-null non-`BoolValue` operand to
  `InvalidArgumentType`, preserving short-circuit for genuinely-boolean cases, plus
  a regression test. Well-scoped, correctness-first.

- **C-2 — MEDIUM (tracked, non-result).** TCK error-*type* and error-*phase* are
  not verified by the conformance gate — it proves error scenarios raise *some*
  error, not the exact openCypher type/phase. `~122/695` error scenarios raise the
  exact type today; this is a known, ratcheting baseline
  (`cypher/tck/error_fidelity_test.go`). Result-correctness is unaffected and the
  execution-level TCK mandate is not violated. Action: continue ratcheting; document
  the error-taxonomy divergence in the user manual.

- **C-3 — LOW (doc).** Stale docstring on the exported `BuildPlan` describes
  `Selection`/`Expand` as stubs; both are fully implemented via `buildOperator`
  (`cypher/api.go:6025-6031`). Violates the doc-fidelity rule. Fix: delete/replace
  the stale matrix.

- **C-4 — LOW (doc, intentional).** `CREATE (n:L $m)` with `$m = null` creates an
  empty labelled node rather than erroring — a deliberate, documented, TCK-neutral,
  user-approved divergence (rmp #2036). Not a bug. Recommend documenting the Neo4j
  divergence in the user manual.

### ACID / durability

- **A-1 — MEDIUM (scoped; doc/hook).** Secondary hash/B-tree index (`CREATE INDEX`)
  maintenance is structurally Cypher-path-only: `txn.Store` holds only `*lpg.Graph`,
  no `index.Manager` handle, so a raw Go-API `txn.Tx` write cannot update secondary
  indexes — mixing raw `txn` writes with Cypher-created indexes leaves the index
  stale until restart (recovery rebuilds it). **Not reachable when the durable store
  is driven exclusively through `cypher.Engine`** (the intended path); label bitmaps
  are maintained directly by `lpg`. Evidence: `store/txn/txn.go`,
  `cypher/api.go:3984`. Fix: document the mixed-usage hazard on `Store.Graph()` /
  the `txn` package doc, or route index maintenance through a store-level hook.

- **A-2 — LOW (doc).** The bare `NewEngineWithStore` constructor does not
  re-register recovered constraints/indexes; mitigated by
  `NewEngineWithStoreAndConstraints` / `NewEngineWithStoreAndSchema` (which do) and
  by logged auto-registration. Action: ensure embedders use the schema-aware
  constructor after recovery; document.

### Security

- **S-1 — LOW / operational.** The engine-wide *aggregate* inbound-memory ceiling
  auto-engages only when `GOMEMLIMIT` is set; with neither `GOMEMLIMIT` nor
  `Options.MaxInboundDecodeBytes` it resolves to disabled, so aggregate transient
  inbound memory is bounded only by `MaxConnections × (16 MiB + 128 MiB)`. Per-
  connection caps are always on; the outbound side is independently bounded. The
  mechanism already exists. Fix: document as a hardening default in `SECURITY.md`
  (set `GOMEMLIMIT`, or `Options.MaxInboundDecodeBytes` + `GlobalMaxResultBytes`).
  Evidence: `bolt/server/serve.go:295`.

- **S-2 — INFO.** Invariant `panic()`s in the columnar chunk engine were **proven
  unreachable** from attacker-controlled query text/parameters (empirical PoC run
  and removed) and are contained by the serve-loop `recover()` boundary — a panic
  logs and closes one connection, never crashing the server (the CLAUDE.md-
  sanctioned recovery exception). Evidence: `cypher/exec/chunk.go`,
  `bolt/server/serve.go:667`.

### Performance

- **P-1 — HIGH-LEVERAGE opportunity (confirmed by measurement; correctness-safe).**
  The unlabeled full-scan projection reverts to a **boxed `[]Row`** path above the
  50k parallel threshold: `MATCH (n) WHERE n.v>=0 RETURN n.v` measures **89
  allocs/op at 2k** (columnar) but **182,558 allocs/op at 60k**
  (`ParallelScanProject`, boxed). `tryBuildParallelScanProject` is tried before
  `tryBuildColumnarFilterChain`. Worse under load: at N ≥ cores concurrent queries
  the `ParallelGovernor` grants 1 worker each → serial execution *with full boxing*
  (~19.8 MB churned/query → GC pressure), strictly worse than the columnar serial
  path there. This hits precisely the "extreme-demand" regime. Fix: make morsel
  workers columnar (fill typed `Chunk`s, box at sink), or reorder the planner to
  prefer the columnar chain. Correctness-neutral (order-independent full scan; box-
  at-sink stays inside the visibility barrier). Evidence: `cypher/api.go:6356`,
  `cypher/exec/parallel_scan_project.go:324-380`, `cypher/exec/parallel_governor.go:57`.

- **P-2 — MEDIUM (confirmed).** The serial `count`/all-node-count branch boxes every
  scanned node (`cypher/exec/scan_all.go:97`); count-pushdown already exists for the
  parallel/label path. `EngReadCount` 2k = 1,811 allocs vs pushdown 31. Fix: extend
  count-pushdown to the serial branch. Correctness-safe.

- **P-3 / P-4 — MEDIUM (confirmed; overlaps deferred #1704 P4/P5).** Filter-into-
  aggregate and relationship/entity materialization box per row; the columnar
  pipeline terminates at `Expand` (3 boxed IDs/edge; `Expand1Hop` 960k rows ≈ 4.08M
  allocs). These are the deferred `#1704 P4/P5` cluster that needs the pinned-
  snapshot isolation foundation first — sequence accordingly.

- **P-5 — LOW.** Per-query map allocators in analytics (Yen's K-shortest-paths,
  Leiden aggregate, `Distinct_10k` hash-set key boxing). Algorithm-inherent; low
  leverage.

### Hygiene (not defects)

- **H-1.** `bench/cypher` `Rel*` benchmarks fail because their seed loop issues
  repeated `CREATE (a)-[:LIKES]->(b)` on a non-multigraph graph, which the engine
  *correctly* rejects (ACID consistency). The module is right; the benchmark harness
  is missing `adjlist.Config{Multigraph:true}`. These benchmarks currently gate
  nothing. Fix the harness.

- **H-2.** `docs/benchmarks/cypher-scale.md` and
  `docs/benchmarks/chunk-1822-baseline-engreadproject.txt` predate the columnar
  work (they still show the old 5309/119807/184479 numbers). Refresh so the on-disk
  regression baselines reflect current reality.

---

## What was verified SOLID (do not churn)

- **Correctness.** Exact cross-type INTEGER↔FLOAT equivalence across the 2^53
  boundary, propagated through `=`/`<>`/`IN`/HashJoin/DISTINCT/EagerAggregation
  grouping/list-map (single comparator). 3VL Kleene logic, null propagation,
  aggregation determinism/emptiness, ORDER BY totality (NaN/null/mixed-type),
  list/map 3VL equality (false-beats-null), arithmetic overflow guards. The new
  columnar value-model (#1704/#2049/#2045) is soundly constructed and gated to
  degrade byte-identically off the fast path.
- **ACID.** Group-commit with `OpCommit`-marker-gated recovery and a TxnSeq suffix
  filter (aborted prefixes never fused); engine durable-then-visible commit inside
  `ApplyAtomically` (WAL fsync before index-buffer commit; undo-log rollback on
  failure — no reader can observe a non-durable/aborted write); fsyncgate-hardened
  `poison()`; parent-dir fsync on create + torn-tail discard; CRC32C per frame with
  bounded eager allocation; DDL survival across checkpoint gated on WAL health.
  Isolation (single-writer `visMu` barrier + read-committed `BeginReadTx`) is sound
  and faithfully documented.
- **Security.** ~16 prior hardening cycles hold: index-payload OOM, value-depth SO
  guard (128), snapshot-index decoder bounds (128 MiB budget), Bolt RESET auth-
  bypass, `tx_timeout` overflow, `O_NOFOLLOW`, `=~` anchoring, GraphML XXE, CSV cap.
  TLS `MinVersion` 1.2, no `InsecureSkipVerify`. SHOW…YIELD tail re-parsed through
  the same grammar with a one-CALL guard (no injection).
- **Concurrency.** Zero races, zero leaks. `atomic.Pointer` COW for tombstones and
  registries (lock-free reads), shared `ParallelGovernor` caps intra-query worker
  explosion, bounded resources everywhere (conn sem 1024, trigger buffer 4, plan-
  cache LRU 1024, result budget, morsel channels, bulk workers 16), context-
  cancellable writer acquire, recover-log-terminate panic boundaries.
- **Performance.** Commit path 8 allocs/op with group-commit amortizing fsync
  (256 goroutines → 32 µs); `graph/lpg` / `adjlist` exemplary (`HasEdge` 0 allocs,
  lock-free COW reads, SoA columns, inline edge labels, ~61.8 B/edge); search &
  centrality tight (`Brandes` 10 allocs/op invariant 2k→10k, direction-optimizing
  BFS 0 allocs / ~10× top-down); columnar read path excellent on covered shapes
  (`EngReadProject` 5309→89, labeled scale scans 147 allocs at 120k).

---

## Deferred (previously reviewed, user-accepted)

The following remain deferred by prior user decision and are **not** readiness
blockers: `#1704 P4/P5` + `#1825`/`#1826` (entity lazy-columns + result streaming —
need the pinned-snapshot isolation foundation, `docs/isolation-design.md`);
`#1712`/`#1827` (async-checkpoint determinism); `#1714` (Leiden serial); `#1718`
(Stoer-Wagner dense trade-off); `#1879` (needs design); `#2046`/`#2047`/`#2048`
(CALL{}/COLLECT{}/POINT portability). P-3/P-4 above overlap the `#1704 P4/P5`
cluster.

---

## Bottom line

GoGraph is **ready to operate in extremely demanding production environments.**
Both non-negotiable invariants hold with measured evidence (openCypher TCK
3897/3897; ACID crash battery green; zero races/leaks; coverage 86.6 %;
`govulncheck` clean). There is **no CRITICAL or HIGH finding in any dimension.**
The highest-value follow-ups are a single well-scoped correctness fix (C-1, the
only silent-wrong-result path) and a single high-leverage, correctness-safe
performance fix (P-1, which bites exactly at the target scale); the remainder are
documentation, operational-hardening, and tracked/deferred items.
