# Production-readiness audit under extreme hostility and load — 2026-07-02 (R2)

**Scope.** A second full-team, evidence-based audit of the **GoGraph module**
(only the module — the `examples/` and `bench/` trees are exercisers, not
deliverables) to determine what remained before the module can operate in
production under **extreme hostility** (adversarial/untrusted network clients,
query text, import files, and store files; resource-exhaustion / DoS) and
**extreme load** (sustained high concurrency, memory pressure, saturation),
across three axes: **functional completeness, performance, and security**. It
follows the 2026-07-01 audit (sprint 257, `docs/audit-prod-readiness-2026-07-01.md`),
which certified the module ready after closing ten findings.

**Method.** Eleven specialist agents ran in two phases. Phase 1: six domain
auditors — Cypher functional/robustness, storage + ACID durability vs an
adversarial disk, graph-algorithm correctness + complexity, concurrency +
resource bounds, performance under load, and a security review of every
untrusted-input boundary — each produced structured findings with a concrete
`file:line` and a reproducible failure scenario, and were explicitly directed to
find what the six prior reliability rounds and the 2026-07-01 audit MISSED,
especially at the intersection of hostility AND load. Phase 2: every
CRITICAL/HIGH/MEDIUM finding was handed to an independent adversarial verifier
instructed to **refute** it against the actual code and the openCypher TCK /
WAL-recovery sources. Only findings that survived verification were remediated.
The two compliance mandates were treated as inviolable throughout: **100%
openCypher TCK (3897 scenarios)** and **100% ACID**.

## Verdict

**Ready for production.** Two of the six domains were certified with **zero
findings** on entry — **storage / ACID durability** ("CERTIFIED — no defects
found") and **concurrency / resource bounds** ("CLEAN — zero findings"). The
remaining four domains surfaced **five findings that collapse to four distinct
defects** (the pre-parse-guard gap was reported independently by the Cypher and
Security auditors). All five survived adversarial verification (0 refuted); all
four defects are TCK-neutral DoS/OOM hardening of the same class the project has
consistently treated as HIGH (#1828–#1830), and all four are now fixed, each with
its own regression test, with the TCK held at 100.0% / 3897 and the full
`-race`/lint/staticcheck/govulncheck gate green.

## Findings and remediation (rmp sprint 258, #1839–#1842)

| # | Sev | Domain(s) | Finding | Fix | Commit |
|---|-----|-----------|---------|-----|--------|
| #1839 | MEDIUM¹ | Cypher + Security | The pre-parse operator guard (`cypher/parser/guard.go`) counted only arithmetic/boolean operators, so a byte-tight chain of **comparison / string-list / null-predicate** operators (`=`, `<`, `>`, `<=`, `>=`, `<>`, `=~`, `IN`, `CONTAINS`, `STARTS WITH`, `ENDS WITH`, `IS`) bypassed it and forced the ANTLR parser + visitor to build a ~500k-node AST (~0.9 s CPU, ~1.2 GB transient) uninterruptibly — the unfixed sibling of #1831. Reported independently by two auditors. | Extend `countBinaryOpNormal` to count the comparison/predicate class toward `maxBinaryOpTokens` using the same false-positive-free discipline proven for `-`/`*`: the unambiguous two-byte tokens `<=`/`>=`/`<>`/`=~` unconditionally; single `<`/`>` with arrow-exclusion; single `=` only operand-adjacent (so `p=(…)` / `SET n={…}` are never counted); predicate keywords word-bounded. | `368de47` |
| #1840 | HIGH | Graph algorithms | The **exhaustive** `shortestPath()`/`allShortestPaths()` path-predicate search (`cypher/exec/shortest_path.go`) — entered when the pattern carries a whole-path `WHERE` predicate — enumerated relationship-unique paths in a frontier bounded **only by `ctx`**. A hostile unsatisfiable predicate over an unbounded pattern on a dense graph drove super-exponential frontier growth whose PEAK MEMORY no deadline bounds. | Give the exhaustive search the identical `VarLengthExpand` budget: a finite hop ceiling (`defaultMaxUnboundedHops`) when the pattern is unbounded, plus per-input-row and aggregate per-query edge-traversal caps returning `ErrVarLenCapExceeded`, incremented alongside the existing `ctx` poll. | `0476281` |
| #1841 | HIGH | Performance | The pipeline-breaking operators (`Sort`, `Distinct`, `Eager`, `EagerAggregation`, `HashJoin`) bounded only the **row/group COUNT**, never the estimated bytes. A few rows carrying large values (e.g. a 9M-element list per row, under the per-eval cap) could hold tens of GiB while the count stayed far below its cap; the engine's aggregate-byte budget is charged only at the drain, which runs strictly AFTER a breaker finishes buffering. | Thread the engine byte budget into every breaker via a shared `byteBudget` helper (`WithByteBudget`), charging `estimateRowSize` per buffered row/group and returning its typed memory-cap sentinel (the new `ErrHashJoinMemoryExceeded` for `HashJoin`, which had no cap at all) — the same pattern #1830 used for `ParallelScanProject`. | `62a22d4` |
| #1842 | MEDIUM | Performance | The result-memory budget was strictly **per query**; the Bolt server admits up to 1024 connections bounded only by cursor count. N concurrent clients could each materialise a per-query-capped result whose SUM exhausts the host — a load-dependent memory-DoS the per-query cap alone cannot stop. | Add an engine-wide ceiling (`EngineOptions.GlobalMaxResultBytes`): a shared atomic counter every `Result` books its estimated materialised size into (flushed in ~1 MiB chunks to bound contention) and returns on `Close`, rejecting a materialisation that would cross the ceiling with the transient `ErrGlobalMemoryExceeded`. The zero-value default derives the ceiling from GOMEMLIMIT (half when set, else unlimited), so protection is default-on precisely when the operator has declared a memory budget and never rejects a legitimate workload on a host whose memory the module cannot know. | `f1fbffd` |

¹ #1839 was reported HIGH by the security auditor and HIGH→MEDIUM by the Cypher
auditor's own verifier, to stay consistent with the module's precedent: the
identical arithmetic case (#1831/F4) measured the same ~0.9 s / ~1.2 GB
single-query profile and was itself rated MEDIUM. It is a straight
incomplete-coverage bypass of a shipped control, which speaks to fix urgency, not
blast radius. Fixed regardless — cheap and TCK-neutral.

### Findings refuted by adversarial verification

None. All five reported findings survived (0 refuted, 0 downgraded to
NOT_A_BUG).

## Domain verdicts (post-remediation)

- **Storage / ACID durability** — CERTIFIED, zero findings. Every critical path
  (WAL CRC + torn-frame detection, 3-phase non-blocking checkpoint, atomic WAL
  truncation, bounds-checked on-disk formats) traced end-to-end against the code
  and the crash-injection battery.
- **Concurrency / resource bounds** — CLEAN, zero findings. Every
  hostility-and-load-reachable concurrent path audited; the Bolt server bounds
  connections/deadlines/in-flight, goroutine spawn is cores-bounded and
  goleak-covered.
- **Graph algorithms** — PRODUCTION-READY after #1840, the sole finding.
- **Performance / graceful degradation** — PRODUCTION-READY after #1841 (byte-blind
  breakers) and #1842 (global ceiling).
- **Cypher (functional + query robustness)** — PRODUCTION-HARDENED after #1839.
- **Security (untrusted-input boundaries)** — PRODUCTION-READY; the one finding
  (#1839, the guard gap) closed; every other boundary already hardened.

## Gate evidence

- `go build ./...`, `go vet ./...`, `gofmt`/`goimports`: clean.
- `go test -race ./...` (all packages, includes the ACID crash-injection battery
  and WAL-recovery tests): 0 races.
- openCypher TCK: **100.0% / 3897**.
- `golangci-lint run ./...`, `staticcheck ./...`: clean.
- `govulncheck ./...`: no vulnerabilities.
- coverage gate (`scripts/cover_gate.sh`): OK — aggregate ≥ 85%, every package ≥ 75%.

Every fix carries its own regression test, so each gate above is also that
finding's guard.
