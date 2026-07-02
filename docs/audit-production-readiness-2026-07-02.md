# GoGraph — Production-Readiness Audit

**Date:** 2026-07-02
**Commit audited:** `2b2e06d` (branch `main`, clean working tree, synced with `origin/main`)
**Toolchain:** Go 1.26.4, darwin/arm64 (Apple M4, 10-core)
**Method:** Multi-dimensional audit by a team of six specialist sub-agents (Functional × 2, Reliability × 2, Performance, Security), coordinated from the main session. Every conclusion is evidence-based — backed by a gate run, benchmark, profile, crash-injection scenario, differential test against an independent reference, or a proof-of-concept. The two material Functional defects were additionally reproduced independently from the specialist reports.

---

## Overall verdict: CONDITIONALLY READY — one required fix (F1) before an unqualified production stamp

The module's infrastructure is production-grade. Reliability (ACID, durability, crash-safety, concurrency) and Security are both certified with **zero defects**. Performance is production-acceptable with known, non-blocking optimisation headroom. The single item that prevents an unconditional "production-ready" verdict is **F1** — silent loss of parallel edges on the *documented default* engine configuration — which is both a data-integrity defect and a violation of the module's own non-negotiable openCypher-conformance mandate on the shipped default path.

### Baseline gates — all green

| Gate | Result |
| --- | --- |
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `go test ./...` (short layer, 103 packages) | pass |
| `go test -race ./...` | pass — 0 data races, 0 failures |
| `govulncheck ./...` | pass — no vulnerabilities |
| openCypher TCK (`go test ./cypher/tck/...`) | pass — 3897 scenarios; gate `tckExecutionBaseline = 3897`, harness verified non-vacuous |

---

## Per-dimension results

### Reliability — Storage / ACID — READY (0 defects)

The durability, atomicity, isolation, consistency, and on-disk-format paths were independently traced and exercised, and certified sound against authoritative WAL/fsync/recovery practice (RocksDB/PostgreSQL WAL reclamation and group commit, fsyncgate truncation discipline).

Certified sound:
- **Durability** — WAL flush→`dataSync` ordering advances `durableSize` only after fsync; a flush/fsync failure poisons the writer and physically truncates the un-synced suffix. Commit returns only after the fsync covering the `OpCommit` marker (no ack precedes durability). Group commit uses a single leader per fsync round with follower fast-path and fail-all-on-poison.
- **Atomicity** — recovery applies only the committed contiguous `TxnSeq` suffix and discards orphaned prefixes; proven with real SIGKILL crash-injection tests including the post-checkpoint-window edge.
- **Recovery robustness** — torn tail truncates and recovers; over-declared length hiding a CRC-valid frame, CRC/magic/version mismatch, and oversize frame/transaction all fail-stop (`IsClean=false`); replay allocation bounded by the producer op cap.
- **Snapshot / checkpoint** — crash-atomic publish (write+fsync → dir-sync → archive → rename → parent-sync → drop backup); WAL truncation authorised only after re-verifying self-sufficiency under the phase-3 lock, so a DDL committed during the lock-free window blocks truncation.
- **Untrusted on-disk format** — `safeCSRAllocBound` fstats the open `O_NOFOLLOW` fd and bounds allocation to `min(manifest, real)`; every component reader is bounded; manifest capped at 32 MiB with version rejection; all components CRC32C-verified.

Gates run (all green): crash-injection battery (`-tags gograph_crashinject`, real SIGKILL subprocesses), `-race` on the storage/sim packages, and representative sim scenarios (crash-storm, disk-full/ENOSPC, constraint-enforce, index-diversity, type-coverage); harness fidelity checkers proven non-vacuous by mutation.

One INFO-level, non-default-reachable observation (opt-in shard-cap partial in-memory apply, durability preserved) is recorded as documented F5 semantics — not a defect.

### Reliability — Concurrency / Load — READY (0 defects)

Certified sound:
- **Data races / memory model** — `-race` green across the whole concurrency-sensitive surface; lock-free COW publication (adjacency shards, property/label registries, edge-property columns) uses a uniform release-acquire discipline; the tombstone gate is a lock-free `atomic.Int64` fast path.
- **Goroutine lifecycle** — every library-spawned goroutine is owned and joined (Bolt server, checkpoint loop, parallel executor, index backfill, bulk loader, TLS watcher); goleak-covered suites green; no package-init goroutines.
- **Bounded resources / backpressure** — connection semaphore, per-server inbound-decode budget, result-byte ceiling, message-size and per-connection in-flight caps, and tx/statement timeouts are all explicitly bounded and surface typed errors rather than panic/deadlock/silent drop.
- **Contention** — measured read-only scaling ~3.4× from 1→8 cores with flat allocs; mutex/block profiling shows no application lock on the read hot path; under a concurrent writer, readers block only on the ACID isolation barrier (by design), with no deadlock or throughput collapse.
- **Cancellation** — public blocking APIs honour `context.Context`.

Two INFO observations (a documented test-only `statementNow` global overridden per-query in production; an unreachable-in-practice governor-inflight accounting path) — neither is a defect.

### Security — READY (0 new vulnerabilities)

Seventh engagement on a heavily-audited surface. All six security fixes landed earlier today were independently re-verified by code reading, data-flow tracing, and (for the wire layer) an independent proof-of-concept.

Certified sound:
- **Bolt/packstream wire (network-reachable, pre-auth)** — layered bounds: 16 MiB reassembled-message cap (checked before each allocation), per-message wire-byte budget, cumulative 128 MiB decoded-memory budget with overflow-safe charging, and a recursion-depth guard. PoC: a 3.26 MiB list of 1.14M tiny maps is rejected with `ErrDecodedMemoryExceeded` and bounded heap; nested-list and oversize-length-prefix inputs rejected. `-race` clean on the shared-budget CAS path.
- **Snapshot / CSR loader** — the prior CSR-OOM finding is fixed and verified (fstat of the open `O_NOFOLLOW` fd bounds the allocation; no TOCTOU).
- **GraphML import** — stdlib `encoding/xml` default decoder makes XXE and billion-laughs inert; streaming (no whole-DOM amplification), `<key>` cap, truncated-tail rejection, 128 MiB byte cap.
- **CSV / JSONL import** — per-record field cap and per-line/byte caps bound allocation before the stdlib parser.
- **Cypher DoS** — `range()`/list output charged against the per-evaluation element budget, `maxRangeElements` lowered, and an incremental per-row projection byte budget wired at both projection sites — all on by default.

Two carried-forward INFO items are by-design, documented, and consciously accepted (aggregate cross-connection ceilings default-off unless `GOMEMLIMIT`/options set; unseeded FNV shard hash with bounded impact) — configuration guidance, not code defects.

### Performance — READY-WITH-CAVEATS (non-blocking)

Certified sound (measured):
- Search inner loops are genuinely zero-allocation (Dijkstra/BFS-DO 0 B/0 allocs post-warmup; large Dijkstra allocates only the result copy).
- Parallel analytics scale with cores and show no cliff 1→8 (triangles ~3.6×, WCC ~1.7×); PageRank and Floyd-Warshall parallel outputs are bit-identical to serial.
- `metrics.Time` is alloc-free and confined to public entry points.
- Read-path shared-state intern is flat under 128× over-subscription.
- Per-release benchmark discipline (`benchstat` two-sample with p-values, worktree old-vs-new) is exemplary.

Caveats (headroom, not blockers):
- **P1 (MEDIUM)** — the Cypher scan/project path boxes ~1 heap object per scanned entity plus one per projected scalar column (`expr.Value` interface). A `count(*)` over 100k nodes measures ~99.8k allocs and is GC-bound. Bounded and graceful, comparable to peer JVM engines; the top Cypher-throughput lever.
- **P2 (MEDIUM)** — Expand re-materialises edge-label slices per candidate edge for relationship-type filters (~8 allocs/row on a 1-hop pattern).
- **P3 (MEDIUM, regression posture)** — no Cypher-engine benchmark exists above ~1k nodes, so P1/P2 have no regression gate at realistic scale (search does).
- **P4 (LOW)** — some zero-alloc gate tests use node IDs < 256, where Go's `staticuint64s` elides boxing, making those particular assertions vacuous.

### Functional — Graph algorithms — READY-WITH-CAVEATS

Every in-scope algorithm was verified empirically against independent naive references and, for betweenness normalisation, against NetworkX 3.6.1, over thousands of randomised graphs (directed/undirected, weighted/unweighted, multigraph, self-loops, disconnected, `src==dst` cycles, empty/single-node). No CRITICAL/HIGH/MEDIUM correctness defect was found. Parallel variants match their serial counterparts (PageRank and Floyd-Warshall bit-identical; WCC and triangles identical; Brandes within its documented float tolerance).

Caveats (documentation / precondition only):
- **A1 (LOW)** — `search/centrality/brandes.go:19-20` godoc gives an undirected normalisation divisor `((n-1)(n-2)/2)` that is 2× too small a denominator for the ordered-pair raw output, yielding normalised scores that can exceed 1.0. The raw computed value is correct. Fix: use `(n-1)(n-2)` (as the directed case and `WeightedBetweenness` already do), or instruct callers to halve the raw value for the undirected Freeman convention.
- **A2 (LOW)** — `search.CountTriangles` is correct only on simple graphs; on representable non-simple inputs (self-loops or parallel edges) it silently over-counts, and the simple-graph precondition is undocumented. Not reachable via Cypher. Fix: document the precondition or dedup neighbours / skip self-loops.
- INFO: `BellmanFord` exported doc says "textbook O(V·E)" but the implementation is SPFA with an SLF deque (same bound and results); integer cumulative-distance overflow is a documented caller precondition with no production hot-path guard.

### Functional — Cypher conformance & execution — READY-WITH-CAVEATS

The execution engine is deeply openCypher-conformant across a wide semantic probe (three-valued NULL logic, orderability, aggregation over empty input, lists/maps, type coercion, arithmetic edge cases, strings, OPTIONAL MATCH null rows, WITH scope barrier, variable-length paths, shortestPath/allShortestPaths, MERGE, and multigraph reads/writes when configured). The 3897 TCK gate enforces `passed>=3897 AND passed==total AND undefined==0` over the full upstream feature tree with a faithful row/value comparator.

Two real, TCK-invisible defects hold it back from an unqualified READY (both independently reproduced):

**F1 (HIGH) — the documented/default engine configuration silently discards parallel edges.**
- Locations: `graph/adjlist/adjlist.go` (`Multigraph` defaults false → repeated `AddEdge` on the same ordered pair is idempotent); documented default wiring in `examples/22_cypher/main.go:172` and `docs/tier2.md:94` (`adjlist.Config{Directed:true}`, no `Multigraph`); write decision at `cypher/api.go` (`AdjList().Multigraph() || !edgeExisted`); `cypher.NewEngine` has no guard/warning. The TCK harness (`cypher/tck/world_test.go`) sets `Multigraph:true`, which is why the suite stays green while the shipped default path is broken.
- Reproduction (default `adjlist.Config{Directed:true}`, writes via `RunInTx`):
  - `CREATE (a)-[:T1]->(b)` then `CREATE (a)-[:T2]->(b)` between the same pair → total edges = **1** (spec: 2), and the `:T2` edge is absent (0 rows). The second `CREATE` returns success with no error.
  - Same-type parallel `CREATE (a)-[:R]->(b)` twice → total `:R` edges = **1** (spec: 2).
  - With `Multigraph:true` the identical queries return 2 and 1 respectively — the engine is fully correct once configured.
- Impact: openCypher's data model is a multigraph in which `CREATE` always adds a relationship; on the documented default path a `CREATE` is acknowledged but silently stores nothing. This is silent data loss (the worst failure mode for a database) for any model with more than one relationship between a node pair (transactions between accounts, flights between cities, repeated interactions), and a conformance violation of the module's non-negotiable openCypher mandate on the shipped default.
- Note on framing: an edge-deduping *simple-graph mode* is a legitimate library feature; the defect is that the Cypher engine is wired to it by the documentation/examples, where openCypher semantics require multigraph behaviour.
- Recommended options (a design decision for the maintainer): (a) make `cypher.NewEngine` require/default to a multigraph-backed graph, or error/log when handed a non-multigraph adjacency; (b) at minimum fix the user-facing examples and `docs/tier2.md` to set `Multigraph:true`; (c) surface the idempotent drop as a notification rather than silence.

**F2 (MEDIUM) — DISTINCT / grouping / UNION treat integer `1` and float `1.0` as distinct.**
- Location: `cypher/expr/equiv.go`. `Equivalent(IntegerValue(1), FloatValue(1.0))` returns `true` via the `Equal` default branch (line 85), but `EquivalentHash` hashes a `FloatValue` through `math.Float64bits` (line 108) and an `IntegerValue` through `v.Hash()` (line 130), so equivalent values land in different buckets. This breaks the invariant documented at lines 88-90.
- Reproduction: `EquivalentHash(int 1) = 1`, `EquivalentHash(float 1.0) = 4607182419872710656`; `count(DISTINCT [1,1.0,2]) = 3` (spec: 2); `count(DISTINCT [1,1.0]) = 2` (spec: 1); `RETURN 1 AS x UNION RETURN 1.0 AS x` → 2 rows (spec: 1).
- Spec authority: openCypher CIP2016-06-14 (the source cited in `equiv.go`) — numbers of different types are tested for equivalence as if coerced to unlimited-precision decimals, so `1 ≡ 1.0`.
- Impact: silent wrong results (over-count, duplicate "distinct" rows, extra aggregation groups, failed UNION dedup) whenever the same logical numeric value appears as both INTEGER and FLOAT — common with heterogeneous ingestion. Read-path only; systematic direction is over-count, never a false merge.
- Recommended fix: in `EquivalentHash`, canonicalise integral numeric values to one hash domain (hash an integral `FloatValue` within int64 range as the equivalent integer), guarding the float→int precision edge (beyond 2^53); add a regression test alongside `aggregate_equivalence_test.go`.

INFO items: the TCK gate is rigorous on rows/values but does not gate exact openCypher error *types* or property/label side-effect *counters* (a separate error-fidelity gate ratchets error types independently); implicit (un-aliased) column names reflect post-normalisation text (`RETURN age-1` → header `age - 1`) rather than the verbatim source.

---

## Consolidated findings (severity-ranked)

| ID | Severity | Dimension | Summary | Blocking? |
| --- | --- | --- | --- | --- |
| F1 | HIGH | Functional (Cypher) | Silent parallel-edge drop on documented default config; openCypher multigraph violation + silent data loss | Yes |
| F2 | MEDIUM | Functional (Cypher) | `EquivalentHash` int/float mismatch → wrong DISTINCT/grouping/UNION over mixed numerics | Should-fix |
| P1 | MEDIUM | Performance | Cypher scan/project boxes one heap object per entity + per scalar column | No (headroom) |
| P2 | MEDIUM | Performance | Expand re-materialises edge-label slices per candidate edge | No (headroom) |
| P3 | MEDIUM | Performance | No Cypher-engine benchmark above ~1k nodes (regression posture) | No (gap) |
| A1 | LOW | Functional (Algo) | Undirected betweenness normalisation divisor 2× too small in godoc | No (doc) |
| A2 | LOW | Functional (Algo) | `CountTriangles` over-counts on non-simple graphs; precondition undocumented | No (doc) |
| P4 | LOW | Performance | Zero-alloc gate tests use node IDs < 256 (vacuous) | No (hygiene) |
| F3/F4 | INFO | Functional (Cypher) | TCK gate does not assert error-type/side-effect counts; implicit column names use normalised text | No |
| A3/A4 | INFO | Functional (Algo) | BellmanFord doc/impl mismatch (SPFA); integer overflow is a caller precondition | No |

---

## Recommendation

1. **Before an unqualified production deployment:** resolve **F1** (choose one of the design options above). It is the only deploy-blocking finding and is small to fix once the design choice is made.
2. **Soon after:** fix **F2** (clear correctness bug, clear fix).
3. **Track as follow-up (non-blocking):** the performance headroom items (P1/P2/P3/P4) and the algorithm documentation/precondition items (A1/A2, and the INFO items).

The reliability and security posture is production-grade and requires no action.

---

*Audit conducted 2026-07-02 at commit `2b2e06d`. F1 and F2 independently reproduced from the specialist findings. No source files were modified during the audit.*
