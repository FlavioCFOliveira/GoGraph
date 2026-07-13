# Production-Readiness Audit — GoGraph — 2026-07-13

**Scope.** A rigorous, exhaustive, multi-dimensional production-readiness audit of
the GoGraph module across the four dimensions the goal named — **functional
completeness**, **correctness**, **efficiency** (CPU / RAM / storage), and
**security** — exercising the module end to end through the repository's 34
examples and profiling it with `pprof`/benchmarks. Baseline commit `77d8a8c`
(sprints 271–276 closed; examples 26→34). Remediation completed at `49c5aaa`.

**Method.** Five independent specialist sub-agents audited in parallel
(cypher-expert, graph-theory-expert, storage-engine-auditor, security-researcher,
rust-perf-engineer), each read-only, evidence-based, adversarially self-verifying,
and each required to re-evaluate the five open backlog findings (#1980–#1983,
#1997) for production impact and to hunt for new blockers. The orchestrator ran
the invariant/CI gates and exercised every example. Every confirmed blocker was
then remediated under the project's `Specify → Implement → Test → Document`
cycle, each fix carrying a **proven non-vacuous** regression gate (verified to
fail on the pre-fix code) and one commit, with independent specialist
re-certification (GO/NO-GO) of the security fixes.

---

## Baseline gates (@77d8a8c, re-run @49c5aaa)

| Gate | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| openCypher TCK (`TestTCKExecution`) | **3897/3897 scenarios, 0 failed, 0 undefined; 16006/16006 steps** |
| ACID durability (`internal/crashinject`, `store/wal`, `store/recovery`) | clean |
| `go test -race ./...` | **111 packages, 0 data races, 0 failures** |
| doc-freshness / script gates | clean |
| `golangci-lint run ./...` | **0 issues** (2 style regressions cleared this cycle) |
| `govulncheck ./...` | 1 informational (GO-2026-5856, see below) |

**Examples exercised.** 33/34 pass cleanly at their deterministic defaults;
`26_social_scale_bench` was verified separately at 20k-user scale (its default is
a 1M-user stress run). The examples produced faithful correctness evidence:
max-flow suite agreement (Dinic = Edmonds-Karp = Push-Relabel; maxflow = mincut),
three-way APSP agreement (Dijkstra/Floyd-Warshall/Johnson), Prim = Kruskal,
Bellman-Ford vs Johnson oracle, parallel algorithms == serial (betweenness 6.3×),
Eulerian each-edge-once + closure; a WAL ledger conserving debit = credit across
crash recovery; concurrent-transaction conservation with zero lost updates; MVCC
snapshot-swap with no torn reads; all six Bolt wire guarantees; and all twelve
documented observability metrics present.

---

## Dimension verdicts (audit phase)

| Dimension | Verdict | Score | Notes |
|---|---|---|---|
| Functional / Cypher | READY-WITH-RESERVATIONS | completeness 85, correctness 96 | Zero wrong-result defects for any openCypher-9 semantics; full TCK surface green. Reservations are completeness gaps for constructs **outside** the openCypher-9 TCK (FOREACH, `CALL{}`/`COLLECT{}` subqueries, `POINT` type — all TCK-neutral) and one fail-silent write-path edge (F1, fixed). |
| Functional / graph algorithms | READY | completeness 92, correctness 97 | Entire algorithm suite correct across tens of thousands of independent-oracle differential trials (BF/Johnson/Floyd-Warshall/Dinic/EK/Push-Relabel/Prim/Kruskal/Tarjan/Kahn/CountTriangles/Brandes/Hungarian/HopcroftKarp/WCC). No blockers. |
| Correctness / storage / ACID | READY-WITH-RESERVATIONS | 93 | Atomicity, Consistency (storage layer), Isolation (READ-COMMITTED, the documented level), Durability all upheld with evidence. Reservations are two engine-layer consistency footguns (#1980, #1981) that leave durable state correct. |
| Efficiency | READY-WITH-RESERVATIONS | 78 | Search hot loops zero-alloc (Dijkstra 0 B/op post-warmup); lock-free reads scale near-linearly; 55.8 B/edge at scale; no OOM/leak. The one reservation is the tracked, gated `#1704` Cypher-VM interface-boxing ceiling (full-scan analytical Cypher over ~1M edges: 1–4 s). |
| Security | NOT READY (narrowly) → remediated | 65 → cleared | Network/parse/auth surface excellent (Bolt wire hardening, RESET-bypass closed, injection/XXE/billion-laughs immunity, metrics/TLS). Two availability blockers on the value-processing and untrusted-store paths (F1 HIGH, F2 MEDIUM), both fixed this cycle. |

**Overall audit verdict (pre-remediation): NOT production-ready** — one demonstrated
HIGH availability blocker (security F1) plus one MEDIUM (security F2). Both had
small, class-closing fixes.

---

## Blocker adjudication of the five pre-existing open findings

| # | Specialist verdict | Disposition |
|---|---|---|
| #1980 (index desync on mixed write APIs) | MEDIUM, reproduced; durable write correct + self-heals on restart; requires deliberately mixing raw `txn.Store` writes with engine indexes | Non-blocker; remains tracked backlog |
| #1981 (`NewEngineWithStore` over recovered store drops schema) | MEDIUM; schema durably persisted, warned, safe constructors exist | Non-blocker; remains tracked backlog |
| #1982 (`CREATE INDEX <name> IF NOT EXISTS` order) | LOW/MEDIUM, confirmed genuine Neo4j-DDL divergence; TCK-neutral | Non-blocker; remains tracked backlog |
| #1983 (index key type on empty graph) | **Re-evaluated: NOT a correctness defect** — correct results; narrow perf-only limitation (numeric point-equality not index-accelerated), not empty-graph-specific | Downgraded; tracked as a perf note |
| #1997 (`KShortestPathsLoopless` blowup) | MEDIUM, reproduced; a deliberately-exponential best-first enumerator (docstring warns), NOT a broken Yen's; polynomial `YenKShortest` is correct and on the production path | Non-blocker; remains tracked backlog (deprecate/bound the bare entry) |

None of the five pre-existing findings is a production blocker.

---

## Remediation — sprint 277 (5 commits, local; not pushed)

| Task | Fix | Commit | Severity |
|---|---|---|---|
| #1998 | **Value-nesting-depth cap** (security F1). `WriteValue` gains the symmetric `maxValueDepth` guard mirroring the decoder; `cypher/expr` adds `MaxValueDepth` + iterative, aliasing-safe `ExceedsValueDepth`; `reduce()` rejects an over-deep/over-large accumulator with a typed error before it can overflow a downstream walker's stack. | `182c6f4` | HIGH (blocker) |
| #1999 | **Bound `readIndexFile` to the manifest size** (security F2). The one snapshot reader that missed the manifest-size bound now reads through `boundedComponentReader(f, e.Size)` before the CRC check. | `a79bc0d` | MEDIUM (blocker) |
| #2000 | **SET/MERGE-SET RHS fail-stop** (cypher F1). The write-path RHS closures propagate eval errors instead of swallowing them into a silent no-op; `COUNT{}` on a write-path SET now aborts atomically, and arithmetic/type errors on any SET RHS surface. | `3d2c4cc` | MEDIUM |
| #2001 | **golangci-lint 0-issue gate restored** (De Morgan simplification; `runDDL` ctx-first; `bytes.Equal`). | `c81e68a` | style (CI gate) |
| #2002 | **csrfile fuzz seed + reader doc fidelity** (storage F3+F4). Fuzz seed magic corrected and the target now drives the full `openBytes` reinterpretation path; misleading `[Reinterpret]` comments corrected to the actual `unsafe.Slice`. | `49c5aaa` | LOW / INFO |

Every fix carries a regression gate proven to fail on the pre-fix code
(the value-depth guard's removal makes the aliased-explosion test hang; the
SET-RHS swallow silently succeeds; the unbounded index read returns the full
file; the fuzz seed never leaves `ErrBadMagic`).

---

## Non-blocking backlog (tracked, not fixed this cycle)

- **#1980 / #1981** — storage-layer consistency footguns (mixed-write-API index
  desync; plain constructor over a recovered store). Durable state stays correct.
- **#1982** — `CREATE INDEX <name> IF NOT EXISTS` parse-order divergence from Neo4j.
- **#1997** — deprecate or default-bound the unbounded `KShortestPathsLoopless`
  bare entry (the polynomial `YenKShortest` is the production path).
- **Cypher completeness gaps** (TCK-neutral): FOREACH, `CALL{}`/`COLLECT{}`
  subqueries, `POINT`/spatial, `exists()` function form, `SHOW` DDL.
- **Efficiency epics** #1704 (per-scanned-entity interface boxing) and #1838
  (Bolt whole-node re-box); plus count-pushdown eligibility for `count(var)`.
- **Graph LOW documented preconditions**: integer-overflow on SSSP/APSP weight
  sums, directed-graph misuse of undirected algorithms, APSP self-distance of
  isolated nodes, HopcroftKarp partition contract under the shard-aware Mapper.

## Toolchain hygiene (environment action, not a code defect)

`govulncheck` reports **GO-2026-5856 / CVE-2026-42505** (crypto/tls Encrypted
Client Hello de-anonymization), fixed in `go1.26.5`; `go.mod` pins
`toolchain go1.26.4`. It is reachable by symbol via the Bolt TLS path but
**unreachable in practice**: GoGraph never configures ECH. Recommendation: build
a production release against Go ≥ 1.26.5 as routine hygiene. Not a blocker.

---

## Final verdict

All blockers are fixed with independently-certified, non-vacuous regression gates;
the CI lint gate is restored to 0 issues; the two non-negotiable invariants hold.
Final gate suite on `49c5aaa`: **TCK 3897/3897**, `go build`/`go vet` clean,
`go test -race ./...` clean, `golangci-lint` 0 issues, ACID crash-injection
battery clean.

**GoGraph is PRODUCTION-READY across all four audited dimensions at `49c5aaa`**,
with the reservations recorded above carried as an explicitly-tracked,
non-blocking backlog. All commits are local to `main` and **not pushed**, per the
project's merge-≠-push convention.
