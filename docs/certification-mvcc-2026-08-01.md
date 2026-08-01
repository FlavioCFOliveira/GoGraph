# MVCC certification — GoGraph module

**Date:** 2026-08-01 · **Entry head:** `e0d8e4c7` · **Exit head:** see §7 · Apple M4 (10 cores), `darwin/arm64`

Scope: **the MVCC substrate and every read path that depends on it.** This is deliberately
narrower than a whole-module production-readiness cycle — it does not repeat the hostile,
security, or resource-exhaustion audits of the 2026-07-26 and 2026-07-31 cycles, and it does
not run the nightly layer. What it certifies is the property those cycles left open: that a
read observes one instant of the graph, and that no read takes the visibility barrier.

The 35 programs under `examples/` are **instruments**, not subjects. One was added this
cycle because the existing set could not see the defects below.

---

## Verdict: **MVCC is complete, and certified for production within the envelope in §5.**

The CRITICAL availability defect that blocked the 2026-07-31 certification — **#2274**, where
one long read plus one write starved every short reader for the whole duration of the long
read — **is fixed and re-verified this cycle against the current head.** It is no longer a
function of the analytical query's duration.

Two further **correctness** defect clusters were found, reproduced deterministically, and
fixed. Both were **Isolation violations**, both were **invisible to a fully green test
suite**, and both belonged to **one defect class** that this cycle names and then closes.

**The class: a structure derived from the graph, keyed on the topology epoch alone, is
unsound under MVCC.** The epoch names the *latest* topology; a derived structure under MVCC
also depends on the *reader's instant*. Two readers at one epoch holding different snapshots
are then served each other's structure. Every member of the class has now been audited — see
§3, which lists what was found, what was proved sound, and the one hygiene item left.

---

## 1. What was broken, and how it presented

### #2293 — the CSR pair the read path expands over

`exec.NewExpand` and its siblings receive **only the two CSRs** — no snapshot, no read view —
so nothing downstream re-checks whether an arc was visible to this read. The pair was
nonetheless built from the **present** adjacency while being filtered by liveness resolved
**at the reader's snapshot**, so it belonged to no single instant at all, and it was cached
under the topology epoch.

Three distinct defects followed. Found by chasing a **1-in-40 intermittent** failure in a
pre-existing test:

| # | Defect | Evidence |
|---|---|---|
| 1 | **Queries failed outright** | `cypher: internal panic: runtime error: index out of range [4] with length 4`, **56 times in a 120 ms run**. Two passes over a live adjacency a writer moved between them. Recovered into an error, so the process survived and the query simply returned nothing. |
| 2 | **Committed edges invisible** | The worst offender was not the pair but the **edge-type filter**, a `map` keyed by **arc position**: served against a differently-sized pair it is not stale, it is **misaligned** — position *i* names a different arc. |
| 3 | **Reads observed the future** | Present-topology arcs let a read traverse an edge committed after its snapshot started. |

**Fixing the pair alone made things dramatically worse**: the concurrency regression went from
1 failure in 40 runs to **183 in 200**, because the sibling cache still keyed on the epoch. A
partial fix of this class is worse than none.

### #2294 — pattern predicates and pattern comprehensions

The same class on an independent path. `pattern_eval.go` holds a `ReadView` bound to its
query's snapshot and then read topology through `ReadView.AdjList`, documented as returning
the adjacency **unbound** from that instant. So every `WHERE (a)-[:T]->(b)` and every pattern
comprehension answered from the **present** while the rest of the same query answered from
the snapshot — **one query, two instants.**

Probed before fixing: with a snapshot held across a committed edge between two pre-existing
nodes, the evaluator's source returned **1 neighbour where the versioned read returned 0**.

Nine sites, found by grepping `.AdjList()` on `ReadView` receivers. Seven in `pattern_eval.go`
now use `ReadView.EntryView`; two in `api.go` resolve a relationship's storage direction and
now use `ReadView.HasEdge`.

---

## 2. The fix, and the prior art that chose it

`csr.BuildFromAdjListAsOf` resolves every adjacency entry at the reader's instant through the
accessors MVCC P3b already provided. `csrPairKey{epoch, startTS, versioned}` replaces the
bare epoch for **both** derived caches.

**`startTS` is the load-bearing component.** Writes apply **eagerly** and publish their
commit timestamp afterwards, so the epoch can move *before* the commit is visible: a reader
starting in that window builds a structure without the arc and files it under the
already-moved epoch. Two reads with the same `startTS` see the same set of published commits
by construction, because `startTS` is drawn from the published instant.

The as-of build is also **repeatable**, which the present-reading build is not: the entry
visible at a fixed `startTS` is immutable and pinned by the reclamation horizon, so both
passes resolve the same entry and the tear is gone **by construction, with no lock**.

> An earlier attempt re-took the visibility barrier on a cache miss. **It worked** — and it
> put a shared acquisition back on the read path, partially undoing what #2274 and P4c exist
> to remove. It was withdrawn. **No read path takes the visibility barrier.**

**Prior art, read in source rather than in documentation.** PostgreSQL's
index-scan-then-visibility-check licenses a derived structure that is a **superset** but never
a **subset** — *that option was not open here*, because nothing downstream of the CSR
verifies. Memgraph's `VertexAccessor`, which reconstructs a vertex's edge vectors as of the
transaction from its Delta chain, is the shape that applies: the structure a read expands over
must *be* the reader's topology.

---

## 3. The class, audited to closure

Every unversioned read on `lpg.ReadView` was enumerated and resolved:

| Surface | Verdict | Basis |
|---|---|---|
| CSR pair + edge-type filter | **fixed** (#2293) | four regression tests, each verified to fail on the unfixed build |
| Pattern predicates / comprehensions | **fixed** (#2294) | five regression tests, each verified to fail on the unfixed build |
| Relationship direction probes (`api.go`) | **fixed** (#2294) | same commit |
| `IndexManager`, `NodeIndex` | **sound — measured** | candidates *are* re-checked against the versioned stores: ~2 000 observations per run, five runs under `-race`, **zero** contradictions |
| `LiveOrder` | **sound — gated** | a cardinality estimate; the O(1) count pushdown gates it through `LiveNodeCountExact` |
| `AdjList` | **sound — escape hatch** | both bulk consumers now read through the as-of accessors |
| Registries, `TopoGeneration` | **sound** | not per-object state; no instant applies |
| `EdgeCreateCount` in `edgeInstanceIdxFor` | **hygiene, open (#2295)** | see below |

**#2295 was opened as a BUG and downgraded to hygiene after tracing its consumer** — the
correction matters more than the finding. `totalCreates` comes from the present and is
compared against a snapshot-derived `parallelCount`, but the single consumer uses it **only
as a guard threshold**. A concurrent CREATE makes the guard *decline* (falling back to the
per-pair type union, conservative); a concurrent DELETE makes it *admit*, and the index it
admits is snapshot-derived and looked up in the **versioned** per-instance store — consistent.
The skew never misattributes. What remains is a latent hazard for the next reader of that
code, not a wrong answer.

The `IndexManager` line above is worth singling out: the doc **asserted** a re-check for the
label bitmap and **said nothing** for the property indexes. Both claims were unmeasured, and
unmeasured claims of exactly this shape were what #2293 and #2294 turned out to be. The
assertion is now replaced by evidence, and `readview.go` records it.

---

## 4. Gates and conformance

| Gate | Result |
|---|---|
| `make ci` | **exit 0**, read from the log and not from a task notification |
| Race suite | **118 packages green** under `-race`, 0 failures, 0 data races |
| openCypher TCK | **3897 / 3897** scenarios, **0 failed, 0 undefined** (baseline 3897) |
| `golangci-lint` | **0 issues** |
| Coverage gate | **87.0 %** aggregate (floor 85.0 %), every package ≥ 75.0 % |
| Concurrency regression | 200 consecutive runs under `-race`, **200 pass**, exit code read directly |

### #2274 re-verified against the current head — the soak fairness gate

The gate ran **clean**: not under `-race` (which distorts the latency it measures) and with no
competing load (which would distort it into a false failure).

| Phase | readers = 1 | worst latency | readers = 8 | worst latency |
|---|---|---|---|---|
| baseline (reads only) | 211 554 op/s | 599 µs | 421 793 op/s | 2.831 ms |
| + one long read | 204 733 op/s | 737 µs | 405 626 op/s | 1.946 ms |
| + writer 10/s | 203 560 op/s | 6.378 ms | 430 583 op/s | 4.157 ms |
| **+ long read AND writer** | **110 582 op/s** | **7.196 ms** | **204 024 op/s** | **7.073 ms** |

Collapse **1.91×** and **2.07×** against a 4.0× tolerance. The long read runs for **1 m 42 s**
and the worst short-read latency is **7 ms** — the amplification is no longer a function of
the analytical query's duration, which is precisely what #2274 was. Example 35 reports
`readers_starved=0`, collapse 1.7×, and a worst read that is **1 %** of the analytical query's
duration.

The residual ~2× under the combination is CPU sharing — the long read occupies a core — not
blocking.

---

## 5. The certified envelope

Certified for production **for reads**, including mixed OLTP-plus-analytics workloads, which
is what the previous cycle could not certify.

Two bounds remain, both measured and both on the **write** side or on an extreme shape:

1. **Write throughput does not scale with concurrent writers (#2193).** Commits are
   serialised and the WAL `fsync` happens while the exclusive visibility barrier is held, so
   the group-commit coalescing that `store/txn` already implements is structurally
   unreachable from the engine path. This is a **throughput ceiling, not a correctness or
   availability defect**, and durability is unaffected.

   This cycle **corrected the recorded reason** for the deferral, which had been wrong. The
   old analysis said the barrier could not be released because writes apply eagerly and
   releasing it would publish them before the `fsync` — visible-but-not-durable. **MVCC
   dissolves that**: visibility is gated by `PublishCommitTS`, not by the barrier, so
   apply → release → fsync → publish *is* durable-before-visible. What actually blocks it now
   is **abort semantics and write-write dependencies**: the failure path is a *physical* undo
   that is only sound while no other writer can have built on the rolled-back state, and once
   writers overlap, a writer reading another's uncommitted state needs a cascading-abort rule.
   MVCC supplies the replacement for the first half (`CommitInfo.Abort` marks versions dead
   rather than unwinding them, as PostgreSQL and Memgraph both do); the second half is a
   transaction-manager change. Still **not a bounded change** — but no longer blocked on the
   reverted whole-graph-snapshot work, and that inference is refuted.

2. **Read cost under a *saturating* writer (#2292).** With a writer committing as fast as it
   can and no rate limit, the read pays a version-walk cost. At the **realistic** write rate
   of the fairness harness — 10 writes/s — a concurrent writer costs **2.5 %**. The saturating
   shape is an extreme, and it is a cost of the version walk under permanent churn, **not of
   a lock**: with the reclamation sweep disabled the same benchmark is *slower*, because the
   sweep is what keeps the chains short.

3. **Scope.** This cycle audited MVCC and its read paths. It is **not** a whole-module
   hostile, security, or resource-exhaustion audit, and it did not run the nightly layer.
   Those remain as last certified on 2026-07-31.

---

## 6. Method notes — what this cycle should teach the next one

**Three of my own instruments lied, and each lie was caught only by running it.**

- A `-count=40` run reported `ok` and I read `EXIT=0` — from `tail`, not from `go test`. The
  suite had failed 1 run in 40. **Read the exit code of the process you care about.**
- `make ci` reported **`CI_EXIT=2`** twice while the harness task notification said exit 0.
  Both times the log was the truth.
- Two A/B setups were byte-identical binaries: a `cd` persisted across a compound command, and
  a `git checkout` failed silently because copied files conflicted. Both were caught only by
  printing the **hash** of what was actually built.

**An instrument that cannot fail on the defective build proves nothing.** Example 36 is
validated in **both** directions — against the broken engine and the fixed one — using a
throwaway `git worktree` at the prior head. Three of its own bugs were found that way: it
mislabelled a query *error* as an isolation violation; its headline verdict read green
alongside a non-zero detail count because it consulted only one of its two checks; and two
successive **writer-side** bounds on its churn phase measured the machine instead of the
engine, reporting zero checks under the coverage gate where a single reader query outlasts the
whole window.

**Know what your invariant is structurally unable to see.** Example 36's
acknowledged-commit bracket catches a read landing *outside every legal instant*. It is blind
to a read of the *wrong legal instant*, because the present always sits inside the bracket —
measured: the #2294-broken engine **passed** a full run with `invisible_commits=0` and
`future_reads=0`. What detects that is a **self-contradictory query** needing no external
oracle: expand an arc, then ask a predicate whether that same arc exists. Zero at any single
instant.

**Size a correctness check for the gate that has to run it.** The contradiction query is
O(spokes²); at 200 spokes it took the coverage gate **188 s**, at 80 it takes **3.5 s** — and
detects *more* strongly, because far more checks fit in the same window. A correctness check
nobody can afford to run is not a correctness check.

**Measure end-to-end before paying for a microbenchmark.** The snapshot build costs **+4.89 %**
in isolation and is **statistically indistinguishable** end-to-end (p = 0.55 / 0.22 / 0.84 on
the 960k-edge `cypher_scale` Expand1Hop trio). Routing both build arms through one accessor
read better and cost **13.94 %** purely by losing an inline.

---

## 7. Commits

| Commit | Subject |
|---|---|
| `96b74c61` | `fix(cypher,csr)`: MVCC P4d — version the topology the read path expands over |
| `cc93874d` | `test(cypher)`: pin the DELETE direction of snapshot topology visibility |
| `d2cea85e` | `fix(cypher)`: MVCC P4e — pattern predicates read the reader's instant |
| `71a0541e` | `test(cypher)`: certify the index-seek path against the reader's instant |

Open, tracked in sprint 333: **#2193** (write throughput, not bounded — reason corrected),
**#2292** (read cost under a saturating writer), **#2295** (hygiene, downgraded from BUG with
the trace that justified it).
