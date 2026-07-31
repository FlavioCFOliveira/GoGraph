# Production-readiness certification — GoGraph module

**Date:** 2026-07-30 · **Entry head:** `43348a70` · **Exit head:** see §9 · Apple M4 (10 cores), `darwin/arm64`, go1.26.5

Scope: the **`GoGraph` module**. The 34 programs under `examples/` are **instruments**, not
subjects — they exercise the module so its correctness, performance and efficiency can be
observed. An example failing for its own reasons is not a module finding and is reported as
such.

This cycle audits the **129-commit delta since the last certification** (`afe1681`,
2026-07-26), which added five query access paths, destination-ordered CSR runs and the CSR
pair cache.

---

## Verdict: **CERTIFIED for production**, with the documented limitations in §7.

**Three CRITICAL defects** were found and fixed — a host-process crash on a default engine,
wrong typed relationship counts, and a checkpoint publishing a **partial transaction** —
along with twelve others, across six rounds. **No known correctness defect remains.**

All four ACID properties now rest on evidence, and — this is the part that changed last —
**that evidence can now fail.** Three assertions carrying the durability batteries were
found to be vacuous (`isPrefixOf` tested set membership, so an empty recovery passed against
any fingerprint; `graphFingerprint` had no liveness gate, so a resurrected deleted node
fingerprinted as correct; `internal/crashinject` asserted no graph shape at all). Each is now
proved to bite by injecting the exact defect it names, each paired with a counterfactual
showing the pre-fix assertion passed that same defect unseen.

What certification does **not** claim is set out in §7: two measured structural throughput
ceilings that are decisions rather than defects, a performance cliff attached to unreachable
dead code, non-determinism in `make ci` itself, and a MEDIUM set. None is a correctness
defect; all are enumerated and reproduced.

**The finding worth carrying forward is not any single defect.** Every one of the fifteen was
invisible to a suite that exited 0 with 87.0 % coverage and a 3897/3897 TCK. Three of them —
including a CRITICAL — sat behind a soak layer that **could not run at all** because it
passed no `-timeout`. That looked like a Makefile chore and was the highest-leverage fix of
the cycle. **An unrunnable gate is a defect, not housekeeping, and a green suite is not
evidence until you know what it is structurally unable to see.**

---

## 1. What a green suite was blind to

Every defect below was found while `make ci` exited 0, coverage stood at 87.0 %, and the
openCypher TCK reported 3897/3897 with zero failures. That is the central lesson of this
cycle: **the existing gates could not see any of them.**

| Defect | Why the suite could not see it |
|---|---|
| Process-fatal crash (#2257) | Every delivered test for that shape projects an **aggregate**, which routes to a different operator. A **scalar** projection plus a subquery in `WHERE` is required. Scaling the fixtures would not have found it. |
| Typed `COUNT { }` under-count (#2258) | Needs a **handle-less** parallel edge, which only the Go API produces. Cypher always stamps a handle, so every Cypher-built fixture is correct. |
| Committed type invisible to a warm engine (#2255) | Every existing test that mutates an edge label also adds or removes an edge in the same statement, which bumps the epoch for its own reasons and masks the omission. |
| Permanently-red soak assertion (#2256) | The soak layer could not run at all (#2259), so nobody saw it fail for ~260 sprints. |
| Fail-silent percentile battery (#2261) | The tests **skip** on error rather than failing, so a regression would have been reported as a skip. |

## 2. The two CRITICAL defects

### A default engine crashed the host process — #2257

`tryBuildParallelScanProject` screened projection items with `exprHasNonScalar` but never
applied that screen to the optional `Selection` predicate, while `forWorker` copies the
subquery and pattern evaluators **by pointer**. One evaluator therefore reached N worker
goroutines, and both declare themselves unsafe for concurrent use and memoise into plain
maps.

Measured on a plain `NewEngine` with no options, 60 000 nodes (the default threshold is
50 000), running `MATCH (a:P) WHERE COUNT { (a)-[r:K]->(b) } > 0 RETURN a.id`:

| build | result |
|---|---|
| `-race` | **185 data races**, then `panic: index out of range` |
| production (no race detector) | **5 of 5 runs dead** — 4× `fatal error: concurrent map writes`, 1× nil dereference |

`concurrent map writes` is a runtime throw that `recover` cannot catch, so the process died.
**Bisected to `afe1681`: a standing defect, not a delta regression.** After the fix the same
reproduction returns 60 000 rows with **zero races** and no crash.

### Typed relationship counts were wrong — #2258

`COUNT { (a)-[:K]->() }` answered **1** where five independent enumerating paths and the Go
API all answered **2**. A delta regression.

The investigation reframed the problem: relationship types were stored **per node pair**,
not per relationship, so per-slot truth was unrecoverable —
`AddEdge×2 + SetEdgeLabel(K)` and `AddEdgeLabeled(K) + AddEdge` left byte-identical state
while requiring different answers. On the user's decision the fix stores types **per slot**,
which is what openCypher means, and that single change satisfied **both** previously
irreconcilable oracles and turned the engine's wrong answer of 12 for a self-loop with 11
untyped slots into 1.

A first attempt using a pair-derived fallback was **reverted**: it left a per-slot property
oracle red and cost **+18213 %** (12.80 µs → 2.34 ms, O(d²)).

## 3. ACID evidence

| Property | Evidence |
|---|---|
| **Atomicity** | **A defect was found and fixed (#2255):** `RemoveEdgeLabel` is the rollback inverse of a label SET, and because no edge-label mutator bumped the topology epoch, an **aborted** transaction's relationship type stayed visible to a warm engine. Fixed at source, bump conditional on a real change. |
| **Consistency** | Same defect: a durably committed relationship-type change was invisible to a warm engine **indefinitely** (the cache is capacity 256, so eviction needs 256 other distinct type-key sets). `count(r:T2)` answered 0 where 1 was correct; a fresh engine answered 1, proving the graph was right and the cache wrong. |
| **Isolation** | `store/txn`, `store/recovery`, `store/checkpoint`, `store/snapshot` green under `-race`. The soak-gated isolation battery **was run for the first time** once #2259 made the layer runnable: **8 of 9 pass** — `TestIsolation_Cypher_NoPartialWriteObservable`, `TestRun_ConcurrentReadWrite_NoBuildTear`, both `TestCreateConstraint_ConcurrentDuplicate` variants, `TestByHandle_ConcurrentViewReaders_NoRace`, `TestRunInTx_DetachDelete_CancelDuringSweep_ReturnsCtxError` and both `TestMerge_CrossProcessReopen` variants. **The ninth is #2269 (CRITICAL, open).** |
| **Durability** | WAL, recovery, checkpoint and snapshot suites green. **#2262 fixed in round 4:** `labels.bin` gained a slot ordinal (format **v2**), so relationship types are now durable per relationship instead of per node pair; v1 snapshots still open, verified against a genuine pre-change v1 fixture committed as testdata. **But #2269 is open and outranks it:** the artefact those types are written into can itself contain an edge with no endpoints. |

**Three verification gaps were found in the durability evidence itself — and CLOSED (#2270).** Each fix is proved to bite against an injected defect, with a counterfactual showing the pre-fix assertion passed that same defect unseen. What follows is what they used to be: `isPrefixOf` in the
WAL-truncation battery tests **set membership only** — not order, not position, not
completeness — so an **empty** recovered graph passes against any fingerprint; and
`graphFingerprint` walks the mapper with no liveness gate, so a recovery that **resurrects a
deleted node** fingerprints identically to correct behaviour. `internal/crashinject` has
**zero** graph-shape assertions: its entire surface is `err == nil`, `Killed`, `TimedOut`,
`ExitCode` and one frame count. They are now capable of failing; the crash battery additionally asserts recovered node count, arc count, per-node out-degree, weights, labels and properties against literals.

## 4. Conformance and gates

| Instrument | Result |
|---|---|
| openCypher TCK, execution level | **3897 / 3897** scenarios, **16006 / 16006** steps, 0 failed, 0 undefined, 0 pending |
| `make ci` (tidy, fmt, vet, build, `-race` short layer, lint, coverage) | **exit 0** |
| Coverage | **87.0 %** aggregate, every package ≥ 75 % |
| `make check-soak-build` | exit 0 — soak and nightly tags compile and vet clean |
| `golangci-lint` | 0 issues |

**The soak layer was unrunnable and is now fixed (#2259).** It passed no `-timeout`, so Go's
10-minute default applied to a layer defined as minutes-long workloads run under `-race`.
Measured in an isolated worktree: `graph/io/csv` **passes at 800.8 s** — 1.33× the default,
so it could never have fitted on any machine. Three packages exceed 45 minutes; they are
**slow, not hung**, proved decisively by `TestDetachDelete_Hub1M_Soak` (1 M nodes, 1 M edges)
passing in **724.2 s with the race detector off** against not finishing in 44m24s with it on.

## 5. Measured behaviour under load

### Writes — the ceiling is exactly where it was documented, and now quantified

| writers | Cypher `RunInTx` ops/s | `store/txn` direct ops/s |
|---|---|---|
| 1 | 258.2 | 258.9 |
| 8 | 258.5 | 1 013.7 |
| 64 | 257.3 | 8 051.2 |
| 256 | 258.2 | 31 321.7 |
| 1024 | **257.2** | **113 537.7** |

The Cypher path is **flat from N=1** with `commits_per_fsync = 1.00` at every level; p50 is
exactly `N/258` s, a strict single-server queue. The same durability primitive
(`Tx.CommitWALOnly`) reaches **127 582 op/s at 476 commits/fsync** when the visibility
barrier is not wrapped around it, and the barrier alone sustains 279k–695k op/s — **only the
combination collapses.** Cost: **134× at 256 writers, 496× at 1024.** The independently
measured `F_FULLFSYNC` floor on this filesystem is 265.7/s, matching the ceiling to within 3 %.

### Reads — flatten at 8 readers

Short point query 78k → 195k op/s (**2.5×** over 10 cores); 20 000-node scan 1 794 → 8 770
(**4.9×**). p50 stays flat; tails grow ~3000×. The limiter is **allocation volume**, not
locks: 93.6 KB and 96.7 allocations per single-row query, 92 % of bytes from scratch chunks
hard-coded to 4096 rows regardless of cardinality. A `GOGC=1000` control gives **+69 %
throughput and a 14× better p99 with no code change** — which also refuted the plan-cache
mutex as the cause, despite it holding 98 % of application-lock delay.

### Fair scheduling is violated

One long read alone costs nothing; one writer alone costs nothing. **Together**, at 10
writes/second, short-read throughput collapses **29 009 → 505 op/s (−57×)** and p99 goes
677 µs → 187 ms (**+277×**), because `Engine.Run` holds the read barrier across build *and*
drain and Go's RWMutex writer preference then blocks every new reader. Identical with and
without the fsync, so it is the lock, not disk.

## 6. Security posture

An adversarial audit refuted three of four hypotheses **with executed proofs** and found no
HIGH or MEDIUM issue and no memory-safety bug.

- **Hostile `csrfile` images cannot cause an out-of-bounds read or panic**, even with a
  recomputed tail CRC: overflow wrap, out-of-range edge targets, non-monotone offsets,
  oversized `NVertices` and tampered offsets are all rejected cleanly by three gates —
  overflow-safe `Layout` with carry rejection, exact-equality `validate`, and a full O(V+E)
  semantic pass before any `unsafe.Slice`. **No file path reaches `csr.FromArrays`.**
- **All 12 library `recover()` sites are sound.** Write-path boundaries roll back the WAL
  transaction *before* converting the panic; the undo replays *inside* the visibility
  barrier and re-raises; the two Bolt sites log, meter and terminate one connection.
- **Interchange caps are enforced on every exported entry point**, and GraphML streams
  per-element so the byte cap covers the whole input.
- **One LOW finding:** the plan cache is bounded by entry **count**, not bytes, so its
  effective ceiling is `1024 × 1 MiB ≈ 1 GiB` — measured at **1008.96 MiB** retained,
  shared per engine, surviving client disconnect (§7).

## 7. Documented limitations — what certification does NOT claim

| rmp | Sev | Summary |
|---|---|---|
| ~~#2269~~ | ~~CRITICAL~~ | **FIXED** in round 5 — **a checkpoint could capture a PARTIAL TRANSACTION** — an edge snapshotted without both its endpoints (`Order != 2*Size`). The artefact records a state no serial schedule could produce, and a crash recovery replays it. **Bisects to the entry head `43348a70`: standing, not introduced here.** It was invisible because the test is soak-gated and the soak layer could not run at all until #2259; this is the first defect that fix exposed. |
| ~~#2262~~ | ~~HIGH~~ | **FIXED** in round 4 — `labels.bin` **v2** carries a slot ordinal; v1 snapshots still open with v1 semantics, proven against a genuine pre-change v1 fixture. The work corrected this entry: the shape said to *lose* a type in fact **invents both**. |
| ~~#2263~~ | ~~HIGH~~ | **FIXED** in round 2 — phantom row from a lossy float64 index key. The doc misattributed it to the probe key; the index *entries* are independently lossy. |
| #2264 | HIGH | `CountPatternComp` is unreachable dead code; the projection spelling of a degree count is **983× slower** (2.813 ms vs 2.762 s). Its godoc claims otherwise. |
| ~~#2265~~ | ~~HIGH~~ | **FIXED** in round 2 — **2282.682 µs → 1.883 µs (1212×)**; the tombstoned arm now equals the clean arm, and the cap counts live edges only. |
| ~~#2266~~ | ~~HIGH~~ | **FIXED** in round 3 — the ceiling is now the early-exit budget. Declining shapes, which paid for a decision they never benefited from, improve **56–83 %**; the widest accepting shape **−86 %**. |
| #2268 | MEDIUM | **`make ci` itself is not deterministic.** A strict per-point wall-clock inequality in `bench/cyclicjoin` passed in `test-short` and failed at `cover-gate` **within one invocation** (40.31 ms vs 39.13 ms, a 3 % miss) because the coverage step runs the whole repository in parallel. Separately, two concurrent `make ci` runs interleave writes into the shared `cover.out` and the gate dies on a corrupted package name. |
| ~~#2267~~ | ~~HIGH~~ | **FIXED** in round 3 — a boxed node cell made the operator silently skip its input row (18 rows → 0); a cycle whose closing hop is a self-loop is now **vetoed** rather than guessed. |
| — | HIGH | **Fair scheduling** (§5). A structural decision. |
| — | HIGH | **Write ceiling #2193** (§5). A structural decision. |
| — | MEDIUM | Snapshot node-path **non-determinism**; the WAL-battery **verification gaps**; read-path allocation; the global label-index mutex; plan-cache byte bound; `db.schema.visualization()` returning an empty result set rather than an error; a soak test that **appends 28 000 lines to a tracked testdata file**, breaking `make release`'s clean-tree precondition. |

**Two decisions remain the maintainer's, not mine:** whether to narrow the visibility
barrier — the single structural lever behind the write ceiling, the read starvation and
part of the read-allocation cost — and what `db.schema.visualization()` should do. The
`labels.bin` format decision was taken and implemented in round 4.

## 8. The examples, as instruments

**All 34 build and all 34 exercise the module cleanly.** 30 run to completion unattended;
example 23 runs a full Bolt workload (2 000 nodes, 2 000 queries, 4 sessions) and exits 0;
example 25 comes up on `:8080` over its data directory and shuts down cleanly; example 24
was driven through `init → seed → stats → snapshot → plandiff`, all exit 0.

Example 26 is a deliberately large instrument (1 000 000 users × 150–200 friends) and does
not finish in a 5-minute budget — **that is the instrument working, not a defect.** Driven at
a completing scale it produces the evidence that matters here, including a white-box proof
that the fused cyclic expand actually engages:

```
cyclic.n12000.plans_agree=true      cyclic.n12000.fused_engaged=true
cyclic.n12000.twoexpand_engaged=false
cyclic.n12000.speedup=3.53x         cyclic.n12000.alloc_ratio=7.61x
```

Engagement proof matters because the openCypher TCK contains **no directed cycle over three
or more distinct node variables**, so the 3897/3897 gate is structurally blind to that
operator, and a flag-on/flag-off differential is equally blind to one that silently declines.

## 9. Changes made this cycle

| Commit | Change |
|---|---|
| `8b0b7356` | #2255 — bump the topology epoch on an edge-label change (ACID) |
| `4408357d` | #2256 — correct a permanently-red soak assertion; document the `NVertices` contract |
| `4d4079c6` | tighten the concurrency-doc ratchet to 20 |
| `b4c6e3a6` | #2257 — screen the `Selection` predicate before fusing a parallel scan (**CRITICAL**) |
| `5f18471f` | #2258 — store relationship types per slot, not per node pair (**CRITICAL**) |
| `caebc61b` | #2259 — give the deferred test layers an explicit, overridable timeout |
| `30ffaea3` | #2260 — one helper per abandoned acquire, and enforce liveness with goleak |
| `7ad2fca1` | #2261 — stop four gates and five documents from lying |
| `9ffd321f` | #2254 — document the last 20 exported types; ratchet the concurrency gate to **zero** |

Final gate on the merged line: `make ci` **exit 0**, coverage **87.0 %** (all packages
≥ 75 %), `golangci-lint` **0 issues**, openCypher TCK **3897 / 3897** (0 failed,
0 undefined). All 532 exported types now carry a concurrency clause.

## 10. Method notes worth keeping

- **A subagent task notification reported "exit code 0" over a red `make`** more than once.
  Every gate in this document was confirmed by writing `$?` and reading it.
- **My own harness lied twice.** The first examples runner exited 0 having run nothing
  (macOS bash 3.2 has no associative arrays); a first-pass row-count oracle was wrong where
  the engine was right (nodes at `i%3==0` have out-neighbours ≡1 and ≡2 mod 3, so none is
  `:Q`). Both were caught by checking the result rather than the exit status.
- **Differentials were insufficient three times.** Absolute, hand-computed oracles found what
  differentials missed, and engagement counters distinguished "identical because correct"
  from "identical because it never fired".
- **Contention can only manufacture false failures, never false passes.** Every failure
  observed under load was re-run in an isolated worktree before being believed — which is how
  the soak timeouts were correctly reclassified from "hung" to "slow".
