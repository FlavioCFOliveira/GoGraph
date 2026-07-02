# GoGraph — Production-Readiness Audit, Round 2

**Date:** 2026-07-02
**Commit audited:** `ba1450d` (branch `main`, clean working tree, 15 commits ahead of `origin/main`, none pushed)
**Toolchain:** Go 1.26.4, darwin/arm64 (Apple M4, 10-core)
**Method:** Six specialist sub-agents, one per Round 1 area, each independently re-verifying the Round 1 fixes with fresh evidence, checking for regressions, and spending remaining time hunting for new issues — not a checklist re-confirmation. Every conclusion is evidence-based: a gate run, a proof-of-concept, a differential test against an independent oracle, or direct source citation.

---

## Overall verdict: **NOT READY — two new HIGH-severity findings must be fixed first**

Round 1 (report `docs/audit-production-readiness-2026-07-02.md`) found F1 (HIGH) and F2 (MEDIUM) and fixed both in sprint 262 (commits `8bb0356`..`ba1450d`). This Round 2 audit independently re-verified both fixes hold with **zero regressions** — but the deeper, fresh sweep each specialist was asked to perform surfaced **two new HIGH-severity, independently-confirmed defects** that were not visible to Round 1's probes, plus five MEDIUM/LOW findings. The module cannot be called unconditionally production-ready until the two HIGH items are resolved.

### Baseline gates — all green at `ba1450d`

`go build` ✅ · `go vet` ✅ · `go test ./...` (104 packages) ✅ · `go test -race ./...` ✅ (0 races; two isolated flakes, both root-caused to a benign `pprof.Do` label-reset timing window, confirmed transient by repeated runs) · `govulncheck` ✅ · openCypher TCK ✅ 3897/3897

### Round 1 fixes — independently re-verified, zero regressions

| Fix | Re-verification method | Result |
|---|---|---|
| **F1** (parallel-edge guard) | 10 fresh adversarial probes (self-loop, reverse-direction, MERGE idempotency, same-statement collision, UNWIND-driven collision, multigraph, WAL-backed) by cypher-expert-consultant; independent ACID re-proof with 3 new tests incl. a 24-goroutine TOCTOU refutation by storage-engine-auditor; a PoC proving the fail-fast path is flat 15ms regardless of a 200,000× scale increase (cheaper than the old silent-loop behaviour) by security-researcher | **Holds. No regressions. No new attack surface.** |
| **F2** (EquivalentHash) | 10 fresh adversarial probes (huge-integer boundary, NaN, UNION/UNION ALL, hash-collision-safety verification) by cypher-expert-consultant | **Correctness holds** — but see new findings S1/S2 below, both surfaced by the security specialist's fresh sweep of F2's blast radius. |
| **P3** (120k-node benchmark) | Bit-exact reproduction by rust-perf-engineer (119,807 allocs/op exact match) | **Holds.** One doc-accuracy correction needed (see P-doc below). |
| **P4** (alloc-gate fix) | Bit-exact reproduction; `gateAllocNodeIDStart=1000` confirmed ≥256 | **Holds.** |
| **A1** (betweenness doc) | Triple-verified by graph-theory-expert: analytic ground truth (star graphs), 400-graph differential, and a live fetch of NetworkX's actual `_rescale` source confirming an exact match | **Fully accurate.** |
| **A2** (triangle-count doc) | Reproduced the parallel-edge hazard exactly (K4: 6 vs ground-truth 4) | **Accurate for parallel edges.** The doc's "self-loop" half is not substantiated — see INFO-A2 below. |
| **BellmanFord doc** | Line-by-line trace of `bellmanFordCore` confirms SPFA+SLF matches the new doc exactly | **Accurate.** One low-confidence citation nit (see INFO-BF below). |
| **TCK gate-scope doc** | Line-by-line cross-check of every claim against `errors_test.go`/`compare_test.go` | **Fully accurate, no corrections needed.** |
| **#1865** (hash_join sibling bug, tracked not fixed) | Root-cause proof + end-to-end same-graph A/B repro (hash join on: 0 rows silently wrong; off: 1 correct row) | **Confirmed real, still open.** Matches its MEDIUM rating. |

---

## NEW findings this round

### HIGH — `MERGE` with a compound pattern silently drops the second node and the relationship

**Dimension:** Functional (Cypher) · **Specialist:** cypher-expert-consultant · **Verdict impact:** this alone forces the module below READY.

`MERGE (a:Label1 {...})-[:REL]->(b:Label2 {...})` — creating or reusing two connected nodes in one clause, arguably the single most common Cypher graph-building idiom (upsert-style: user→order, post→comment, actor→movie) — creates **only the first node**. The second node and the relationship are silently discarded. No error, no warning, no side-effect signal.

- **Root cause:** `cypher/ir/writes.go`'s `mergeClause`/`mergeSingleHopRel` only routes to the efficient `MergeRelationship` operator when *both* endpoints are already bound by an earlier clause. Any other shape falls back to `exec.Merge`, which is **structurally single-node** (its Go struct has no field for a relationship or a second node at all) — so anything beyond the first node is not rejected, just never built.
- **Reproduced 8+ ways**, including the exact asymmetric "attach new child to existing parent" form and the 2-hop case the source's own comment flags as unhandled ("Multi-hop — handled by the node-only Merge path for now").
- **Why TCK stays 3897/3897:** every scenario across all 9 Merge TCK feature files pre-binds both endpoints (via a preceding `MATCH`/`CREATE`, or by merging each node in its own separate clause) before merging the bare-variable relationship. This exact compound form is a genuine TCK coverage gap, not a TCK violation.
- **Why the existing test suite never caught it:** every one of GoGraph's 25+ `merge_*_test.go` tests uses the same pre-bind idiom.
- **Verified against live Neo4j documentation:** the canonical behaviour is the opposite — MERGE matches or creates the *whole* pattern atomically, never a truncated prefix.
- **Violates:** the module's own "100% ACID Compliant" (Atomicity/Consistency) and "fail-stop, never fail-silent" mandates.

**Recommendation** (not prescribed, a design decision): either (a) properly widen `MergeRelationship`'s fast path and give the fallback operator real multi-element pattern support, matching Neo4j's documented semantics, or (b) as an immediate stopgap, raise a clear compile-time error whenever a MERGE pattern cannot be fully covered by the planner, converting silent data loss into a loud rejection while (a) is built.

### HIGH — `distinctAggregator` has no memory cap, unlike every sibling accumulator

**Dimension:** Security · **Specialist:** security-researcher · **CWE-770/CWE-400, CAPEC-130** · **Verdict impact:** independent HIGH alongside the MERGE finding.

`count/sum/avg/min/max(DISTINCT ...)`'s backing `distinctAggregator.seen` map (`cypher/api.go:6765-6800`) grows once per distinct value seen, forever — no count cap, no byte budget. Every sibling accumulator in the *same file* is bounded: `exec.Distinct` (`DefaultMaxDistinct=10,000,000` + byte budget), `exec.EagerAggregation`/GROUP BY (`DefaultMaxGroups=1,000,000` + byte budget), `collect()` (`DefaultMaxCollectItems=10,000,000`). `distinctAggregator` alone has neither.

- **Measured:** `count(DISTINCT x)` over 3,000,000 plain sequential integers (no hash trick needed) took 13.8× longer and used +406.5MB of heap attributable purely to this missing cap — with no error, no cap firing, far under the 10M/1M thresholds its siblings enforce.
- **Extrapolated** to the module's own already-accepted `range()` cap of 100,000,000 rows: **~13.5GB** of uncapped heap from a single short statement such as `UNWIND range(0, 99999999) AS x RETURN count(DISTINCT x)` — or from an ordinary large-scale `MATCH (n) RETURN count(DISTINCT n.someProp)` with no `range()` involved at all.
- **Reachable** by any caller able to run one autocommit statement — the same privilege level needed to run any query at all; the Bolt server's default statement timeout is a client-controlled knob (0 = no server-side cap unless the operator additionally hardens it), so it does not reliably backstop this by default.
- **Availability impact:** the whole process (all concurrent connections), not just the offending query.

**Recommendation:** give `distinctAggregator` the identical cap-plus-byte-budget pattern its three siblings already implement.

### MEDIUM — F2's fix has a proven, measurable side effect: large-integer hash quality regressed

**Dimension:** Security · **CWE-407 (Inefficient Algorithmic Complexity)**

Pre-F2, `IntegerValue.Hash()` was *mathematically bijective* over the entire int64 domain (proven algebraically and confirmed empirically with zero collisions across 1,000,000 consecutive values near `MaxInt64`). Post-F2, hashing an integer through `hashFloatBits(float64(iv))` is only bijective over float64 bit patterns, which is lossy above 2^53 — 20,000 consecutive integers starting at 2^62 fell into collision buckets of up to 1,025 entries, exactly matching the IEEE-754 ULP-driven theoretical prediction. Measured: 7.7–10.9× slowdown for `count(DISTINCT ...)` over large adjacent integers, reachable via a ~90-byte query (`UNWIND range(2^62, 2^62+999999) AS x RETURN count(DISTINCT x)`). Bounded (int64's range caps the maximum collision-group size at ~1024–2048), not a runaway blowup, and its blast radius is largely subsumed by fixing the distinctAggregator cap above. Recommend fixing the cap first; changing the hash formula itself needs care to avoid reopening the Integer/Float inconsistency F2 fixed, and should go through the same cypher-expert-consultant review F2 got.

### MEDIUM — F2's fix is incomplete: the same bug exists, unfixed, for Integer/NodeValue and Integer/RelationshipValue

**Dimension:** Security · **CWE-697 (Incorrect Comparison)**

`NodeValue.Equal`/`RelationshipValue.Equal` are documented as deliberately symmetric with a raw `IntegerValue`-encoded ID (the same bound node is routinely represented as a bare integer in one row and a full `NodeValue` in another, for performance) — but `EquivalentHash` has no branch for `NodeValue`/`RelationshipValue`/`LazyNodeValue`, so they still hash through their own raw-ID fold while `IntegerValue` now hashes through `hashFloatBits`. Confirmed with a clean, realistic repro:

```cypher
MATCH (n:P {k:'a'}) WITH n, id(n) AS nid UNWIND [n, nid] AS x RETURN count(DISTINCT x) AS c
```

returns `c=2` (should be `1`) — silent over-counting on idiomatic use of the engine's own dual entity/id representation, not a contrived case. **Mechanical, low-risk fix**: mirror F2's exact pattern — add `NodeValue`/`RelationshipValue`/`LazyNodeValue` branches to `EquivalentHash` routing the ID through `hashFloatBits`, with the same pinning-test treatment F2 got.

### MEDIUM — `Result.Err()` can report `context.Canceled` for a write that already durably committed

**Dimension:** Reliability (Concurrency/Load)

For every no-`RETURN` write (`CREATE`/`SET`/`DELETE`/`MERGE`/`REMOVE`/DDL), the eager zero-row drain re-checks `ctx.Err()` *after* all real mutating work — including the durable WAL commit — has already finished, purely for a formal trailing step with no remaining work to abort. Proven with 60 trials of `CREATE INDEX` over 20k nodes with cancellation sampled around the commit boundary: **53/60 trials where the DDL actually committed also reported `Result.Err()==context.Canceled`**. Underlying graph state was correct in all 60 trials — this is not a data-corruption bug — but a caller using the project's own recommended ctx-cancellation pattern cannot distinguish "aborted, nothing happened" from "fully committed, a redundant trailing step raced a cancellation," and a caller that retries on cancellation risks double-applying an already-successful write. Same shared code path structurally implicates plain CREATE/SET too (argued from source, not separately re-run). **Recommendation:** stop consulting `ctx.Err()` in the eager-drain once the statement's real mutating work is done.

### MEDIUM — `RunInTx`'s godoc is factually wrong about rollback support

**Dimension:** Reliability (Storage/ACID)

The exported doc comment claims *"lpg.Graph does not support rollback... partial mutations remain in the graph"* on a write-statement error. This is **false** at current HEAD: task #1282's undo-log mechanism (added after this comment was written, traced via `git log -S` to the exact stale commit `0b547b7`) fully restores pre-statement state via `rollbackUnderBarrier()` — the identical mechanism that makes F1's own fail-fast rejection atomic. Zero code impact, but a real documentation-fidelity defect with a concrete hazard: a future contributor "fixing" working code to match the wrong doc would introduce a genuine Atomicity regression. **Recommendation:** rewrite to describe the actual #1282 undo-log behaviour.

### MEDIUM — `buildEdgeTypeFilter` rebuilds an O(V+E) whole-graph map on every query execution, regardless of selectivity

**Dimension:** Performance

Every relationship-type-filtered pattern (`-[:TYPE]->`) rebuilds its own full CSR and two per-source-node maps from scratch on every `Engine.Run()` call — never amortized by the plan cache, which only caches parse/semantic-analysis, never the physical build. Decisively measured: an 8-row-selective query out of 960,000 possible costs statistically the same (~1.3–1.6s, ~6.96M allocs) as an unfiltered 960,000-row scan. Non-blocking (same tier as the already-tracked P1/P2 boxing costs), but worse-scaling at high QPS since it is a genuinely new, previously-unisolated finding rather than a restatement of P1/P2. **Recommendation:** cache the filter map keyed by (relationship types, a mutation-epoch counter), or maintain a live per-type index incrementally at write time.

### LOW-MEDIUM — parallel index-backfill's cancellation-poll checkpoints are global-index-based, not per-worker

**Dimension:** Reliability (Concurrency/Load)

`processRange`'s poll check (`if i&0xFFF==0`) uses the shared slice's absolute index, not a per-worker-relative one — for a representative 20,000-row/10-worker backfill, this places **zero checkpoints inside 5 of the 10 workers' own ranges**, so those workers cannot see an early cancellation at all. Responsiveness-only: atomicity remains sound (60/60 trials — a half-built index never becomes visible). **Recommendation:** make the check per-worker-relative.

### LOW — `Checkpointer.RunCheckpoint`'s doc overstates concurrency safety

**Dimension:** Reliability (Storage/ACID via Concurrency/Load's fresh sweep)

The doc claims full safety interleaving with the running loop, but phase 2 (the dominant-duration snapshot write) is deliberately lock-free — two concurrent full `RunCheckpoint` calls collide on disk in 10/10 trials with a real filesystem error. **No production impact today** (the only caller, `internal/sim`, is single-goroutine by design), but a live landmine for a future embedder who trusts the doc. A secondary defect in the same code: a concurrent success's `setErr(nil)` can mask a concurrent failure in `Stats().LastError`.

### LOW — two INFO-level documentation-precision notes (Algorithms)

- `search/triangles.go`'s doc says a self-loop **or** a parallel edge causes the over-count; only the parallel-edge half is substantiated (self-loops are structurally always inert against the rank filter, in every configuration tested). Recommend narrowing the claim.
- `search/bellman_ford.go`'s internal `bellmanFordCore` comment attributes SPFA to "Bannister-Eppstein 2012," which — per the paper's own abstract — actually describes a randomized full-pass variant, not the worklist/deque/SLF mechanism GoGraph implements (standard attributions would be Duan 1994 / Bertsekas). Low-confidence (abstract-only review); no correctness claim depends on it.

### LOW — sibling (non-gate) benchmarks in `bench/cypher_alloc` still use small NodeIDs

The P4 fix only updated the 4 pass/fail gates; the informational `Benchmark*` siblings in the same file (`gate500`, NodeIDs 0-499) still sit half inside Go's free-boxing range, understating their own reported baseline. No false correctness assurance (these aren't gates), just an inconsistent convention.

### LOW — documentation attribution error in `docs/benchmarks/cypher-scale.md`

The doc attributes `Expand1Hop`'s cost to "P1 and P2"; independently measured to be 100% P1 — P2's specific mechanism (`buildRelationshipValueFromRow`) is never even triggered by that benchmark's query shape (no bound relationship variable). Confirmed the delta P2 actually costs (+5.000 allocs/row) via a controlled A/B query change.

---

## Consolidated findings table

| ID | Severity | Dimension | Summary | New this round? |
|---|---|---|---|---|
| Merge-compound | **HIGH** | Functional | MERGE silently drops the 2nd node + relationship of a compound pattern | ✅ new |
| Distinct-agg-cap | **HIGH** | Security | `distinctAggregator` has no memory/count cap, unlike its 3 siblings | ✅ new |
| F2-hash-quality | MEDIUM | Security | F2's fix degrades large-integer hash quality (7.7–10.9× slowdown, bounded) | ✅ new |
| F2-incomplete | MEDIUM | Security | Same Equal/Hash bug F2 fixed also affects Integer/Node/Relationship | ✅ new |
| Ctx-after-commit | MEDIUM | Reliability | `Result.Err()` can show `context.Canceled` after a real commit | ✅ new |
| RunInTx-doc | MEDIUM | Reliability | Godoc wrongly claims no rollback support (#1282 added it) | ✅ new |
| EdgeTypeFilter | MEDIUM | Performance | O(V+E) filter rebuild per query, ignores selectivity | ✅ new |
| Backfill-poll | LOW-MED | Reliability | Cancellation-poll misses 5/10 workers' ranges in a repr. case | ✅ new |
| Checkpoint-doc | LOW | Reliability | `RunCheckpoint` doc overstates safety; no live impact today | ✅ new |
| A2-selfloop | LOW/INFO | Functional | Triangle doc over-claims the self-loop half of its hazard | ✅ new |
| BF-citation | LOW/INFO | Functional | Possible wrong paper attribution for SPFA in an internal comment | ✅ new |
| Bench-fixture | LOW | Performance | Sibling (non-gate) benchmarks still use small NodeIDs | ✅ new |
| Bench-doc-attr | LOW | Performance | cypher-scale.md wrongly credits P2 for 100%-P1 cost | ✅ new |
| #1865 | MEDIUM | Functional | hash_join sibling bug (tracked, not fixed) | re-confirmed, pre-existing |

---

## Recommendation

1. **Before any unqualified production deployment:** fix the two HIGH findings — MERGE compound-pattern data loss and `distinctAggregator`'s unbounded memory. Both are well-characterized with clear (if design-requiring) remediation paths.
2. **Should-fix-soon:** the two F2-follow-up MEDIUM findings (hash-quality regression, incomplete fix for Node/Relationship) and the ctx-after-commit MEDIUM finding — all three have a clear, mechanical fix.
3. **Track as follow-up (non-blocking):** the remaining MEDIUM/LOW items (RunInTx doc, EdgeTypeFilter cost, backfill-poll granularity, checkpoint doc, the two algorithm doc-precision notes, the two benchmark-hygiene notes) and the pre-existing `#1865`.

Reliability's core ACID/crash-safety machinery and the algorithm suite remain excellent — nothing here calls that into question. The two HIGH findings are both narrow, well-understood gaps in the Cypher write/aggregation layer, not systemic issues.

---

*Round 2 audit conducted 2026-07-02 at commit `ba1450d`, by six independent specialist sub-agents given full context on Round 1's fixes and explicitly tasked with fresh verification plus new-issue hunting. No source files were modified during the audit; all specialist scratch work lives under the session scratchpad, isolated from the tracked repository via `replace`-directive Go modules.*
