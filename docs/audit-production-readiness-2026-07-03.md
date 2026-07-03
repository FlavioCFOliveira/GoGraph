# Production-Readiness Audit — GoGraph — 2026-07-03 (Round 3)

**Scope.** A multi-dimensional production-readiness audit of the GoGraph module across
**Functional** (openCypher conformance + graph-algorithm correctness), **Performance**,
**Reliability** (ACID/durability + concurrency), and **Security**. Baseline commit
`0c172a0` (sprint 263 closed); remediation completed at `fe80308`. This is the third
production-readiness round after the 2026-07-02 rounds (sprints 262/263).

**Method.** Six specialist sub-agents audited in parallel, each read-only, evidence-based,
and required to **adversarially self-verify** every finding (prior rounds produced
false-positive claims that were later retracted). Every confirmed finding was then
remediated under the project's `Specify → Implement → Test → Document` cycle, each fix with
a **proven non-vacuous** regression gate (verified to fail on the pre-fix code), one commit
per fix, and an independent specialist certification (GO/NO-GO) of the fix itself.

---

## Baseline gates (@0c172a0 and re-run @fe80308)

`go build` · `go vet` · openCypher TCK (**3897/3897**, baseline held) · `go test -race ./...`
(103 packages, clean) · tagged crash-injection battery (`gograph_crashinject`) · `golangci-lint`
(0 issues) · `govulncheck` (no vulnerabilities). All green.

---

## Dimension verdicts (audit phase)

| Dimension | Verdict | Notes |
|---|---|---|
| Reliability / ACID / durability | **READY** | 0 Critical/High/Medium. Deferred items #1827 (crash-during-async-publish) and #1880 (registry-capture race) both independently disproved as blockers (crash-safe archive-to-`.bak` publish + strictly-ordered WAL truncation; #1880 a fail-stop-safe liveness gap). |
| Reliability / concurrency | **READY** | 0 data races / leaks / unbounded growth / deadlocks. The new #1871 cross-query edge-type-filter cache cleared (bounded LRU, build-outside-lock, read-only shared filter, generation-consistent under the View barrier). |
| Functional / Cypher | **READY** | Core semantics (3VL, cross-type equality, compound equality, aggregation, DISTINCT/grouping, min/max ordering, MERGE integrity) all correct & TCK-conformant. #1865 confirmed **already fixed at HEAD**. Two confirmed non-blocking gaps: #1878, #1875. |
| Functional / graph algorithms | **READY (1 scoped fix)** | Entire suite correct across 4,000+ differential trials; one false positive retracted (Johnson ghost-slot). One confirmed defect: `YenKShortest` on weighted multigraphs. |
| Performance | **READY (acceptable backlog)** | No global lock on a hot path (reads scale ~4× to physical cores), no uncached O(V+E) rebuild, no quadratic hot path, no OOM footprint; steady-state search loops zero-alloc. Per-scanned-entity boxing (#1704) + Bolt re-box (#1838) are throughput/GC ceilings, already tracked & gated — not blockers. |
| Security | **NOT READY (narrowly)** | Network-facing surface (Bolt/Packstream/auth) sound; every confidentiality/integrity/ACID property holds. Two MEDIUM availability defects on the **local on-disk recovery trust boundary** gated the module. |

**Overall audit verdict: NOT production-ready** — three MEDIUM defects blocked release
(two Security, one Functional/Algorithms), all on the module's own declared threat model /
supported input classes. All three had small, class-closing fixes.

---

## Remediation — sprint 264 (14 commits, `9d88478`..`fe80308`, local; not pushed)

### MEDIUM blockers (all specialist-certified GO)

| # | Fix | Commit | Cert |
|---|---|---|---|
| **B1** | `#1882` — forged snapshot CSR `WeightSize` mismatch drove `binary.LittleEndian.Uint64` over a 1-byte slice → **panic at store-open**, outside the recover guards. `ApplyCSRToGraph` now fail-stops with a typed `ErrCorrupted` before any weight decode. | `a53cad2` | storage-engine-auditor + security-researcher |
| **B2** | `#1883` — `embedsValidFrame` advanced one byte at a time and CRC'd each candidate's full declared payload with no work budget → **O(n²) recovery hang** on a crafted WAL (4 MiB → 52.9 s measured). Now caps cumulative CRC input at `2·len(buf)`; on exhaustion returns `true` → `ErrTornFrameMasksData` → recovery fail-stops (the safe direction). | `de0823b` | storage-engine-auditor + security-researcher |
| **B3** | `#1884` — `YenKShortest` returned wrong path order + wrong `Cost` on weighted multigraphs (`buildEdgeIndex` keyed on first-CSR-occurrence while Dijkstra uses the min-weight parallel edge). Now keys on the minimum-weight slot per pair. | `9d88478`, `96b95ba` | graph-theory-expert |

### LOW security hardening (defense-in-depth; security-researcher GO)

| # | Fix | Commit |
|---|---|---|
| LOW-1 `#1885` | index (hash + btree) deserializer `idCount` bound tightened from `len(body)` to `len(body)/8` (each id is 8 wire bytes). | `35dd706` |
| LOW-2 `#1886` | snapshot length-prefixed value reads (properties/mapper/edgehandles) routed through a bounded `readLenPrefixedValue` (eager ≤1 MiB, grow above) instead of `make([]byte, untrustedLen≤1 GiB)`. | `2e4b942` |
| LOW-3 `#1887` | GraphML per-element `<data>` child cap (`ErrTooManyData`), symmetric with the `<key>` cap. | `f82ddb3` |
| LOW-4 `#1888` | JSONL nested-list explicit recursion-depth cap (`ErrListTooDeep`); CSV `MaxBytes`-disables-cap godoc hardened. | `219ae67` |
| LOW-5 `#1889` | WAL frame-payload read bounded by `readFramePayload` (the WAL sibling of LOW-2, found during the sibling-gap hunt); torn-vs-corruption discrimination + CRC preserved (consumed bytes retained on short read). | `9a7718d` |

### Conformance / reliability items closed

| # | Fix | Commit | Cert |
|---|---|---|---|
| `#1878` | Empty relationship-type / node-label names (`[:``]`, `(:``)`) rejected at **every** Cypher pattern site — MATCH/OPTIONAL MATCH/CREATE/MERGE clauses, pattern comprehensions, bare-WHERE predicates, `EXISTS`/`COUNT` pattern-forms, `SET`/`REMOVE` labels, and label-predicate expressions — with a dedicated "must not be empty" error. (An empty backtick type previously collided with the exec no-filter sentinel and matched **every** edge.) | `04a075b`, `3e44878`, `fe80308` | cypher-expert-consultant |
| `#1880` | Snapshot registry-capture race now **self-heals** via a bounded retry (monotonic append-only registries guarantee convergence) instead of aborting the checkpoint; exhaustion falls back to the prior fail-stop (no regression). | `65f390f`, `ef658c6` | storage-engine-auditor + concurrency-architect |
| `#1865` | Confirmed **stale** — already fixed at HEAD (`canonicalKeyHash` delegates to `expr.EquivalentHash`). Closed, no code change. | — | cypher-expert-consultant |

Plus `becca1a` (golangci-lint cleanup of the round's fixes).

### Deferred (tracked, non-blocking)

- `#1875` — MERGE parallel-edge multiplicity under-count (documented limitation; narrow; no corruption).
- `#1890` — end-to-end retry/exhaustion test for the #1880 self-heal (storage-auditor test-depth reservation R2; the fix itself is certified GO on static reasoning + deterministic mechanism tests).
- Pre-existing performance epics `#1704` / `#1838` (interface-boxing) — throughput/GC optimization, not blockers.

---

## Final verdict

All three MEDIUM blockers are fixed and independently certified GO; every LOW hardening item
and both conformance/reliability items are fixed and certified; the one stale backlog item is
closed. Every fix carries a proven non-vacuous regression gate. Full gate suite green on the
final commit `fe80308`: **TCK 3897/3897**, `go test -race ./...` clean, tagged crash-injection
battery clean, `golangci-lint` 0 issues, `govulncheck` clean.

**GoGraph is PRODUCTION-READY across all four audited dimensions at `fe80308`.**

Remaining work is a small, explicitly-tracked, non-blocking backlog (`#1875`, `#1890`, and the
performance-optimization epics). All commits are local to `main` and **not pushed**, per the
project's merge-≠-push convention.
