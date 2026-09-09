# Do `EXPLAIN` and `PROFILE` report factually and honestly? — the closing re-run

**Audit date:** 2026-09-05 (measurements taken 2026-09-05/06) · **Tree:**
`feature/355-gograph-execution-reporting`, closing commit
`83ba8d8bf2ddd1380a09305e429345fe92106d7f` · **rmp task:** #2768 (sprint 355)

**This document supersedes [`explain-profile-honesty-audit-2026-09-03.md`](explain-profile-honesty-audit-2026-09-03.md).**
That document audited the tree sprint 355 then changed, and its eight addenda record what
each task claimed at the time it landed. This one re-establishes every classification **by
measurement on the closing tree**, and says plainly where an addendum's claim did not
survive the re-run.

## Why a re-run, and not an edit

The 2026-09-03 document classifies every figure `EXPLAIN` and `PROFILE` print and lists
eleven divergences from Neo4j, Memgraph and PostgreSQL. Tasks #2760 to #2767 changed the
behaviour that document describes. Editing it in place would have left a record whose
figures were part measured on one tree and part on another, with no way for a reader to
tell which — the exact failure mode the sprint exists to eliminate.

So nothing here is carried over. **Every figure below was produced by running the engine at
`83ba8d8b`**, by a harness written for this task and independent of the sprint's own gates.
Where this document and the superseded one disagree, this one is the measurement and the
other is the history.

## Method

### The instrument

An external Go module (`audit2768`) that imports GoGraph through a `replace` directive and
drives the public API only. It is deliberately **not** the sprint's own test suite: those
tests were written by the tasks whose claims are under audit, and a gate that was written
to pass cannot be the evidence that the claim is true. The harness drives 31 query shapes
over 8 graph shapes and records, for every operator in the captured tree, the eight fields
a reader can see — `Name`, `Detail`, `Rows`, `DbHits`, `DbHitsKnown`, `RowsRemovedByFilter`,
`RowsRemovedByFilterKnown`, `Est` — plus the five rendered surfaces verbatim.

The discrimination that makes the classification falsifiable is this: **a db-hits figure
that differs from the operator's own row count cannot have been derived from it.** Every
"MEASURED" verdict below rests on an observed arm where the two differ, quoted with the
arm that produced it. No verdict rests on reading the source, and none rests on the marker
interface an operator claims — the marker is the mechanism, the divergence is the proof.

### Host and quietness

Apple M4, 10 cores, 32 GB, macOS 26.5.2 (darwin arm64), Go 1.27.1. Load average was
between 1.42 and 2.52 throughout — **the host was not quiet**, and this document therefore
contains **no throughput or latency number**. Every figure in it is a count, a glyph, or a
structural comparison, and none of the three is affected by load. This is the same
discipline the superseded audit applied, for the same reason.

### The reference implementations, at the same pinned commits

Re-read in source at the commits the original audit pinned, so the comparison is like for
like. All three were verified with `git rev-parse HEAD` before being read:

| Project | Ref | Commit | State |
|---|---|---|---|
| Neo4j | tag `5.26.16` | `679feffbfb7a9189aba360ea98eef7fc3371e275` | re-read |
| Memgraph | `master` | `cdd8b5e1285f2b4a8ee4c29710aecb7619f3e98d` | **cloned for this task** — no clone existed |
| PostgreSQL | `REL_17_STABLE` | `4639b6cfe3f310b71e1e227dd2a915b053992c9b` | re-read after fetching the pinned object |

Two notes the re-read forced, recorded because they bear on the audit trail:

* **The superseded document cites two different PostgreSQL commits.** Its method table
  (line 30) pins `4639b6cf…`; both the #2762 and #2764 addenda (lines 644, 980) say
  `018bfcfd…`. Every cited PostgreSQL file — `explain.c`, `execParallel.c`, `instrument.c`,
  `execScan.c`, `instrument.h`, `execnodes.h`, `select_parallel.out` — is **byte-identical
  between the two commits**, so no citation is invalidated. It is a documentation
  inconsistency, not a factual error, and it is corrected here by pinning one commit.
* **No Memgraph clone existed** when this task started, so the Memgraph half of the
  original comparison had never been re-verifiable. It has been cloned and re-read.

## 1. The classification, re-derived from observation

110 operator instances across 31 shapes. The classification below is what the harness
**observed**, not what the code claims.

### Figures that are MEASURED

An operator qualifies only if an observed arm reports db-hits that differ from its own rows.

| Operator | Arm that proves it | rows | dbhits |
|---|---|---:|---:|
| `VarLengthExpand` | `MATCH (r:Root)-[*3..3]->(z) RETURN z` on a 200-way broom | 1 | 202 |
| `Expand` | `MATCH (r:Root)-[:KNOWS]->(b) RETURN b`, 1 of 100 slots typed | 1 | 100 |
| `ShortestPath` | `shortestPath((a)-[*]-(b))` on a 60-node chain | 1 | 117 |
| `AllShortestPaths` | `allShortestPaths((a)-[*]-(b))` on a 60-node chain | 1 | 117 |
| `ParallelScanProject` | `MATCH (n:B) WHERE n.v = 0 RETURN n.k + 1`, threshold 10 | 500 | 2000 |
| `ParallelAggregateScan` | `MATCH (n) RETURN n.v AS g, count(*) AS c ORDER BY g` | 4 | 2000 |
| `ParallelCountScan` | `MATCH (n) RETURN count(*)`, threshold 10 | 1 | 2000 |
| `columnarExpand` | `MATCH (a:A)-[:L]->(b:B) WHERE a.tag = 'rare'` | 400 | 400 |

Seven of these eight reported `0` in the superseded audit or were derived from rows. The
three parallel leaves are the sharpest case: each reports exactly `2000`, which is exactly
what the **serial control arm** — the identical query on an identical graph, differing only
in `ParallelScanThreshold`, a setting the reader never chose and cannot see in the output —
reports on its `NodeByLabelScan` / `AllNodesScan`. The two arms agree, which is the whole
point: the figure no longer depends on a planning decision invisible to the reader.

`columnarExpand` is listed on the marker census but its observed arm happens to emit as
many rows as it walked, so this document does **not** claim it measured; it is included for
completeness and flagged in §9.

### Figures that are DERIVED

| Operator | Observation | What it is derived from |
|---|---|---|
| `NodeByLabelScan` | `dbhits == rows` in 23 of 24 instances; the 24th is a `LIMIT 0` arm with `rows=0, dbhits=0` | the operator's own emitted row count, on the `StorageRecordScan` contract |
| `AllNodesScan` | `dbhits == rows` in both instances | the same |
| `Total Rows`, `Total DbHits` | the `Total` row of `ProfileTable` | a sum over every operator; `Total DbHits` inherits every unknown as `+ ?` |
| operator `Name` | the concrete Go type of the value that ran | still the strongest property of the whole surface |

**An honest limit on this table.** No arm was found in which a `StorageRecordScan` leaf
reads more records than it emits, because for those leaves the one-record-per-row identity
genuinely holds — that is what the marker asserts. The consequence is that **derivation is
observationally indistinguishable from a measurement that happens to agree**, and the
rendering does not say which it is. That residue is the surviving half of divergence D1.

### Figures that are ESTIMATED

Now rendered on the **physical** plan, beside the measurement, which is the change #2765
made. Observed provenance across the census:

| Rendering | Meaning | Observed |
|---|---|---|
| `est. rows=N exact` / `Est.Rows` cell `N` | maintained exact count | `NodeByLabelScan` 24/24, `AllNodesScan` 2/2, `Expand` 4/6 |
| `est. rows~N stats` / cell `~N` | histogram- or sample-derived | not reached by these shapes; reached via `Selection` after `RefreshStatistics` |
| `est. rows~N heuristic` | 1/NDV × N over real inputs | not reached by these shapes |
| `Est.Rows` cell `-` | no estimate, or a stale statistic | **every other operator**, 80 of 110 instances |
| column absent entirely | no operator in the plan has an estimate | observed on `MATCH (n:Wide) RETURN count(n)` |

**The estimate provenance machinery remains exemplary and is now visible where it matters.**

> **CORRECTED 2026-09-08 (rmp #2787, v0.14.1).** The paragraph that stood here read: *"The
> `-` is not a mapping failure: `ExplainTable` renders `-` for `Selection`, `Projection`,
> `ProduceResults` and `CartesianProduct` on the **logical** plan too, so the estimator
> itself only attributes cardinality to access-path leaves. That is a pre-existing coverage
> limit that #2765 exposed rather than caused."* The first clause is true of **most** of the
> 80, and remains the right description of them. It is **false of the columnar fusion
> chains**, whose logical node exists, carries a figure, and loses it in the mapping. See
> the [2026-09-08 addendum](#addendum--2026-09-08-rmp-2787-sprint-357) for the disproving
> query pair, the mechanism, and a measured re-partition.

Most of the 80 are a coverage limit of the ESTIMATOR: `ExplainTable` renders `-` for
`Selection`, `Projection`, `ProduceResults` and `CartesianProduct` on the **logical** plan
too, so for those nodes there is no figure on either surface and #2765 exposed the limit
rather than causing it. The exception is the columnar family, where the figure exists on the
logical plan and is lost on the way to the operator. Both are recorded as open items in
§8 (items 6 and 10).

### Figures that are UNCOUNTED — and now say so

The category the superseded audit called "UNCOUNTED but render as `0`". It no longer
renders as `0`.

| Operator | Renders | Why it is unknown |
|---|---|---|
| `Filter` (11 instances), `Project` (27), `ColumnarProject` (8), `Sort` (2), `Top`, `Unwind` | `?` | holds a caller-supplied expression closure that can reach the graph |
| `LabelCountScan` | `?` | answers from a maintained counter, and its fallback resolves a filtered bitmap **below the resolver interface**, where the operator cannot see it (corrected 2026-09-08, #2777) |
| incomplete total | `2000 + ?` | some operators counted, some did not |
| wholly unknown total | `?` | no operator in the plan counted |

Verbatim, on `MATCH (n:Wide) WHERE n.i < 10 RETURN n` over 2000 nodes:

```
+------------------------------+----------+------+----------+---------+-----------+
| Operator                     | Est.Rows | Rows |   DbHits | Removed | Time (ms) |
+------------------------------+----------+------+----------+---------+-----------+
| Project                      |        - |   10 |        ? |         |     0.357 |
| └─ Filter                    |        - |   10 |        ? |    1990 |     0.354 |
|    └─ NodeByLabelScan [Wide] |     2000 | 2000 |     2000 |         |     0.042 |
+------------------------------+----------+------+----------+---------+-----------+
| Total                        |          | 2020 | 2000 + ? |         |     0.357 |
+------------------------------+----------+------+----------+---------+-----------+
```

**One classification moved in a direction the superseded audit did not anticipate.** It
recorded `LabelCountScan` and `AllNodesCountScan` as printing `0`, and argued that "`0` is
defensible here: no records were read". At the closing tree they render `?`, not `0` — they
claim neither `noStorageAccess` nor a counter. Whether `?` or a known `0` is the better
answer for an operator that reads one maintained counter is a real question this sprint did
not settle; it is listed in §9.

> **Settled 2026-09-08 (rmp #2777), and the two leaves parted company.** The question was
> decided by measuring which arm answers on the shapes a real query plans, not by arguing
> about the counter. `AllNodesCountScan` now reports a **PATH-AWARE measured figure** —
> a real `0` for its O(1) counter read, and one db-hit per node id when the counter
> declines and it walks; the walk was measured being taken by a plain
> `MATCH (n) RETURN count(*)` run against a single uncommitted `CREATE` in another
> transaction, which is why a flat `0` was not available to it.
> `LabelCountScan` stays `?`: rmp #2773 made its exec-level bitmap arm unreachable
> (0 entries across seven measured substrate states) but moved the record read down into
> `lpg.Graph.LabelCountAsOf`, which still resolves the filtered bitmap when the churn
> concerns the counted label — measured 14.00 allocs/op there against 0.00 when the churn
> is elsewhere. `ResolveLabelCountAsOf` reports only `(count, ok)`, so the operator cannot
> know which arm answered, and the correction arm reads roaring containers rather than node
> references, which this column has no unit for. See the census in
> `cypher/exec/dbhits_classification_test.go`.

## 2. Verdict on distinguishability

**The rendering now distinguishes what it did not distinguish, and one ambiguity remains.**

The superseded audit's central criticism was that `Rows` (measured), `Time` (measured) and
`DbHits` (measured for one operator, derived for seven, uncounted for at least five) sat in
one table with nothing saying which was which. At the closing tree:

* an uncounted db-hits figure renders `?`, never `0`, and an incomplete total renders
  `x + ?` — the discipline GoGraph already applied to the Bolt page-cache fields, now
  applied to the column where the same ambiguity existed with the opposite sign;
* `Est.Rows` sits **beside** `Rows` in one table over one plan, so an estimate and the
  measurement it predicted can be compared without running two calls and aligning two
  differently-shaped trees by hand;
* `Removed` says directly how many candidate rows a predicate threw away, which is the
  reader-side half of the question `DbHits` answers from the storage side;
* a table drops the `Est.Rows` and `Removed` columns entirely when no operator in the plan
  has one — Neo4j's discipline, and observed here on a count-store plan.

**What remains ambiguous is MEASURED versus DERIVED.** A `NodeByLabelScan` reporting `2000`
and a `ParallelScanProject` reporting `2000` render identically, and the first is its row
count while the second is a real walk. The column now separates *counted* from *not
counted*; it still does not separate *counted* from *inferred*. That is D1's residue, and
it is a smaller and better-defined gap than the one the superseded audit found.

## 3. The three refutations, re-tested

The superseded audit refuted the db-hits model with three measurements. All three were
re-run.

| # | Original refutation | Re-run at `83ba8d8b` | Verdict |
|---|---|---|---|
| 1 | `VarLengthExpand` under-reported a traversal 202× | `[*3..3]` reports `rows=1, dbhits=202`; `[*1..3]` reports `rows=202, dbhits=202`. Both walk the same 202 slots and report the same figure | **fixed** (was already fixed in the audited tree) |
| 2 | a type-filtered single hop under-reported 100× | `-[:KNOWS]->` reports `rows=1, dbhits=100`; `-->` reports `rows=100, dbhits=100`. The same 100-slot walk, the same figure | **fixed by #2761** |
| 3 | the parallel tier reported `0` for a full scan | all three leaves report `2000`, equal to the serial control's `2000` | **fixed by #2762** |

A fourth, which the superseded audit listed under "left unfixed, deliberately":

| `ShortestPath` / `AllShortestPaths` reported `0` | both report `rows=1, dbhits=117` on a 60-node chain | **fixed by #2763** |

## 4. Does `EXPLAIN` show the plan that actually runs?

Unchanged and still yes, with the same single stated exception, re-measured:

```
Statement: CREATE (:Z {v:1})

Engine.Explain (surface 1):
  (logical plan — a writing statement has no physical tree outside a transaction)
  CreateNode

EXPLAIN prefix -> Result.Plan() (surface 6):
  CreateNode                      <- root Name="CreateNode" Detail=""
```

The prefix form still drops the header and the annotations. This is divergence D11 and it
is **unchanged**.

## 5. Divergences D1 to D11 — closing verdicts

| # | Divergence | Verdict | Evidence |
|---|---|---|---|
| **D1** | Db-hits derived from rows, not counted at the storage layer | **NARROWED** | Eight operator kinds now count for real (§1), each proved by an arm where dbhits ≠ rows. Derivation survives only on the access-path leaves, where the one-record-per-row identity is the marker's actual contract. The residue: the rendering does not distinguish a counted figure from an inferred one. The peer correction survives re-reading and is stronger than stated — Memgraph's `ACTUAL HITS` is `actual_hits++` in **both** `ScopedProfile` constructors (`scoped_profile.hpp:58`, `:89`), before `start_time_ = ReadTSC()` and touching no storage, so it counts `Pull()` invocations |
| **D2** | No `Rows Removed by Filter` equivalent | **CLOSED** | A `Removed` column in `ProfileTable` and `removed=N` in the tree. Measured: `Filter` reports `removed=1990` above a 2000-row scan emitting 10; `Expand` reports `removed=99` for a type filter admitting 1 of 100. Reported by `Filter` in 11 of 11 instances and by `Expand` in 6 of 6. PostgreSQL's mechanism re-verified: exactly **19** `Rows Removed by Filter` print sites in `explain.c`, each guarded by `if (plan->qual)` |
| **D3** | Estimated and actual never side by side | **CLOSED**, with a measured coverage limit | `Est.Rows` renders beside `Rows` in one table over one plan, and `est. rows=N exact` leads the parenthesis in the tree. The limit: 80 of 110 observed instances render `-`. **Corrected 2026-09-08 (#2787):** for most of them the estimator attributes cardinality only to access-path leaves, and the `-` is visible on the **logical** plan too — but **not for the columnar fusion chains**, whose `ir.Selection` carries `est. rows=1, exact` on the logical plan and whose `ColumnarFilter` renders nothing. That one IS a mapping loss; see the 2026-09-08 addendum. Recorded as an open item |
| **D4** | A figure that was not counted prints `0` | **CLOSED** | `?` for an uncounted figure, `x + ?` for an incomplete total, `?` for a wholly unknown one, and the column dropped when no operator has the figure. Neo4j's three-level discipline re-verified: `NO_DATA = -1L` (`OperatorProfile.java:59`), argument dropped (`PlanDescriptionBuilder.scala:156-161`), cell blanked (`renderAsTreeTable.scala:415-431`), column removed (`:54`), `"?"` / `"$x + ?"` (`renderSummary.scala:37-44`) |
| **D5** | No `loops` / per-invocation figure | **UNCHANGED** | The token `loops` appears nowhere in any rendered surface. Figures remain lifetime totals across re-`Init`, a self-consistent convention now documented on `RowsRemovedByFilter` as well. PostgreSQL still divides by `nloops` (`explain.c:1844-1847`) |
| **D6** | Parallel tier collapsed to one node | **NARROWED** | The db-hits half is closed (D1). The collapse itself is unchanged: no per-worker rows, and the tokens `Worker`/`worker` appear nowhere in any rendered surface. PostgreSQL's per-worker actuals are computed at `execParallel.c:1039-1043` and printed at `explain.c:1893-1941` |
| **D7** | No memory figure | **UNCHANGED**, and still does not matter | No `Memory`/`memory` token in any rendered surface. Neo4j's `Memory (Bytes)` is `renderAsTreeTable.scala:204` (the superseded audit said `:203`, which is `DB Hits`) |
| **D8** | Bolt `plan` metadata omits `identifiers` | **UNCHANGED** | No `identifiers` key in `bolt/server/plan_meta.go`. The information still exists as `ir.LogicalPlan.Vars`. **Not re-verified against the driver**: `neo4j-go-driver` is not one of the three pinned clones, so the decoder claim is carried from the superseded audit unconfirmed |
| **D9** | Bolt `plan` metadata carries no estimates | **CLOSED**, and GoGraph now leads | `plan_meta.go` publishes `EstimatedRows` (`:91`) and `EstimatedRowsSource` (`:103`), written together at `:161-162`. `EstimatedRows` matches where Neo4j puts it (a plan argument); `EstimatedRowsSource` has **no Neo4j counterpart** — Neo4j publishes the estimate unqualified |
| **D10** | No eager-operator warning | **UNCHANGED**, and still does not matter | Re-verified at 5.26.16: a repository-wide scan finds only `EagerLoadCsvNotification` (`checkForEagerLoadCsv.scala:50`). The peer has no general eager notification either, and GoGraph has no `LOAD CSV` |
| **D11** | `EXPLAIN` on a WRITING statement returns a LOGICAL tree with nothing marking it | **UNCHANGED** | Measured in §4: `Engine.Explain` prints the header, the prefix form renders bare `CreateNode` with `Detail=""`. Two of the seven surfaces still describe the same statement differently, and a Bolt driver still receives logical operator names in a shape indistinguishable from a physical tree |

**Summary: 4 closed (D2, D3, D4, D9), 2 narrowed (D1, D6), 5 unchanged (D5, D7, D8, D10, D11).**
Of the five unchanged, three (D5, D7, D10) were judged not to matter by the superseded audit
and that judgement survives re-reading. The two that matter and did not move are **D8 and
D11**, both Bolt-facing or prefix-facing, and both were explicitly out of scope for this
sprint's tasks.

## 6. The join reorder consumes a measurement — verified by a plan that changed

#2766 is the sprint's only change to what the planner *decides*, so it needs a plan
difference, not a code reading. Two engines, the same graph (400 `:A`, one of which carries
`tag:'rare'`; 400 `:B`), the same query. **The only difference is whether
`RefreshStatistics` has run.** The selective arm is written *second*, so a planner that
cannot tell the arms apart will not move it.

```
Query: MATCH (b:B), (a:A {tag:'rare'}) RETURN a.i, b.i

ARM A — no statistics
  ColumnarProject
    Apply
      NodeByLabelScan [B]   (rows=400,    dbhits=400)
      Filter
        NodeByLabelScan [A] (rows=160000, dbhits=160000)

ARM B — statistics refreshed
  ColumnarProject
    Apply
      Filter
        NodeByLabelScan [A] (rows=400, dbhits=400, est=1/exact)
      NodeByLabelScan [B]   (rows=400, dbhits=400)
```

The **shape** differs, not merely the annotation: arm A drives `:B` and re-scans `:A` once
per outer row; arm B drives the filtered `:A` and scans `:B` once. Counted db-hits fall
from **160 400 to 800**, a reduction of **99.50%**, on a query whose result is identical in
both arms. Both labels have the same live cardinality, so a label-count-only planner cannot
make this choice — it is the property statistics that make the arms distinguishable.

This is the first plan decision in GoGraph driven by a measurement rather than by a
structural rule, and it is confirmed here independently of #2766's own gates.

## 7. What did NOT survive the fresh run

The most valuable output of a re-run. Six items.

### 7.1 The `PlanNode` godoc contradicts a compile-time census in its own package

`cypher/exec/plan.go:71-72` still says db-hits are MEASURED "for an operator implementing
exec's storageAccessCounter — **today [VarLengthExpand]**", and `:86-88` still says
`DbHitsKnown` "is false for an operator that reads storage and claims none of the three
markers — **[ShortestPath], [AllShortestPaths], the morsel-parallel leaves**, the
count-store leaves, …".

Both statements are false at HEAD, and the contradiction is visible **inside the same
package**: `cypher/exec/profile.go:716-731` is a compile-time census listing
`storageAccessCounter` implementations, five of which are the very operators the godoc
names as unknown. It listed **nine** when this audit was written and lists **ten**
since `0e7e982d` (#2777) added `(*AllNodesCountScan)`, which now reports the node
walk its `Init` fallback performs. The measurement agrees with the census and not with the godoc —
`ShortestPath` renders `117`, `AllShortestPaths` renders `117`, and all three parallel
leaves render `2000`.

Written by #2760, never updated by #2761, #2762 or #2763. **No backlog task covers it.**

### 7.2 The brief's summary of the `Removed` rule is not what the code does

#2764 is summarised as reporting rows removed "omitted, never `0`, when an operator rejects
nothing". The observed rule is different, and better:

* an operator with **no rejection mechanism** omits the cell entirely (`NodeByLabelScan`,
  `Project`, every leaf);
* an operator **with** one that rejected nothing prints `removed=0` — measured on
  `MATCH (r:Root)-->(b)`, where `Expand` reports `removed=0` for an untyped walk.

`PlanNode.RowsRemovedByFilterKnown`'s own godoc states the second case correctly and gives
the reason (PostgreSQL suppresses a zero in text mode; GoGraph prints it because a filter
that rejected nothing is exactly the finding a reader of a slow plan wants). The
summary is what is wrong, not the code — but it is the summary that would have been carried
forward.

### 7.3 The most-wrong estimate is the one q-error cannot see

#2765's limitation 3 — columnar fusion chains carry no estimate, so they contribute no
q-error — is confirmed, and its consequence is sharper than recorded. A stale MCV estimate
wrong by 400× produces **no q-error observation at all**, because the shape that exposes it
fuses into a columnar chain:

```
after PROFILE "MATCH (a:A {tag:'rare'}) RETURN a.i"             pairs=0   <- 400x wrong, unseen
after PROFILE "MATCH (a:A {tag:'rare'}), (b:B) RETURN a.i, b.i" pairs=1
after PROFILE "MATCH (a:A) WHERE a.tag = 'rare' RETURN a.i"     pairs=1
```

The blind spot is not a rare corner: the single-pattern filtered match is the commonest
read shape in the language.

### 7.4 Six statements in `optimizer-activation-design.md` are false, not one

The task brief flagged §4. Re-reading found six, two of which predate the sprint:

1. **§4:335, :339-340** — "Join reordering (deferred)" and "Label-ratio selectivity is
   `EstHeuristic` → fails the gate → no reorder today". Both false: the reorder ships and is
   **on by default** (`cypher/api.go:1716`, `joinReorderEnabled: !opts.DisableJoinReorder`),
   and its gate reads the property statistics (`cypher/join_reorder_plan.go:452-507`) under
   the `3.0` margin the document itself prescribes (`:141`).
2. **§2.1:172-179 and §0:62-64** — "today's estimator is entirely `EstFallback` /
   `EstHeuristic` (the fixed 30% range selectivity, the `1.0` default degree, the `1e9`
   AllNodes sandbag)… the planner is therefore **provably inert**". No `1e9` sandbag and no
   fixed 30% selectivity exist anywhere in `cypher/`, and label counts are `estExact`
   unconditionally.
3. **§4:320-327** — the range-seek guard's constants. Shipped values are
   `rangeSeekMaxSelectivity = 0.10` (doc says 0.05) and `rangeSeekMinLabelPopulation = 64`
   (doc says ~1024, a figure `range_seek_plan.go` records as measured false by more than an
   order of magnitude). Pre-sprint.
4. **Header:12, :24-26, :452** — "the logical `cypher/ir/rewrite` Driver remains unwired
   (the guard test stays green)". **`cypher/ir/rewrite/`, `cypher/plan/` and
   `cypher/rewrite_not_wired_test.go` do not exist**; they were deleted in `80954706`
   (2026-06-22). `grep -rln RewritePackageNotWired` over the tree matches only this
   document. Pre-sprint, and it invalidates §1.1, §1.2, §5-Increment-C and the whole
   "guard test" subsection.
5. **§4:369-370, :390** — join reordering sequenced "LAST (and only if justified)" and
   requiring `EnumerateLeftDeep`. It shipped without either.
6. **§4:328-330** — "Kill the `1e9` AllNodes sandbag" reads as outstanding work; it is done.

### 7.5 Three in-code assertions that the statistics are inert

`8bb4b013` corrected the claim in `cypher/stats_build.go` ("Statistics built here DO change
plans, as of rmp #2766") but left the identical claim standing in three other places:

* `cypher/stats_metrics.go:12-13` — "the estimate providers are display-only (consulted by
  EXPLAIN, never by the executed plan)";
* `cypher/api.go:1447` — "the statistics ship INERT — #2099 renders them display-only in
  EXPLAIN";
* `cypher/api.go:6725` — "are inert / display-only".

A fourth, narrower one: `cypher/api.go:1389` still describes `joinReorderEnabled` as gating
"the **count-store-gated** disjoint-component ordering", which since #2766 is only half the
gate.

### 7.6 Citation drift in the superseded document

Re-reading all three references at the pinned commits confirmed the overwhelming majority
of citations, including every load-bearing one. Six are off, none fatally:

| Citation | Problem |
|---|---|
| `explain.c:2176-2178` (the `if (plan->qual)` example) | **wrong lines** — those are `show_upper_qual` Merge Cond / Join Filter lines with no such guard. The *claim* is exactly right: 19 guards for 19 print sites. Correct examples are `:2170-2172` or `:2183-2185` |
| `renderAsTreeTable.scala:203` (`Memory (Bytes)`) | off by one — `:203` is `DB Hits`, `Memory (Bytes)` is `:204` |
| `RecordRelationshipTraversalCursor.java:159-161` | the `tracer.onRelationship()` call is at `:162`. The conclusion — a record read then rejected by the type/direction test is still charged — holds |
| `execParallel.c:1057-1058` | the per-worker copy is the `memcpy` at `:1059` |
| `explain.c:3638-3639` (the zero-suppression comment) | the quoted comment is on `:3637` |
| `operator.cpp:597, 603` | these are the `SCOPED_PROFILE_OP` macro *definitions*; `:610` is the one real placement. The claim is nonetheless well supported — 56 uses in the file |
| `ProfileDbHitsTestBase.scala:172-178` | described too tidily: `sizeHint + 1` for Interpreted/Slotted/Pipelined/1-worker Parallel, `sizeHint` only for multi-worker |

## 8. What this sprint left OPEN

Every item below was **verified to still hold** at `83ba8d8b`.

> **Correction (2026-09-08, v0.14.1):** "verified to still hold" was true at
> `83ba8d8b` and is **no longer true of rows 1 and 2**, both of which were fixed in
> the `v0.14.1` window. Row 1 is closed by `9ec1a6c7` (#2785) and row 2 by
> `29641046` (#2772) — `cypher.statsSnapshotFresh` did not exist at `v0.14.0`
> (`git show v0.14.0:cypher/stats_estimate.go` contains zero occurrences; the
> current file has four) and now demotes a stale most-common-value hit to
> `estFallback`. Rows 10 and 11 already carried in-place corrections under #2787;
> rows 1 and 2 did not, and this note supplies them. The rows are left standing as
> the record of what the audit found, not as a statement of the present tree.

| # | Gap | Evidence at the closing tree | Backlog |
|---|---|---|---|
| 1 | The range estimator reads a **declined** label count as an empty label | `cypher/stats_estimate.go`: `n, _ := src.ResolveLabelCount(label)` discards the second return; the `if n <= 0` guard below then returns `estFallback`. Under a concurrent writer that declines the count, the histogram path goes silently inert | **#2771** |
| 2 | An MCV equality estimate is tagged `estExact` with **no staleness check at all** | **Demonstrated by measurement.** Refresh with one `tag:'rare'` row, add 399 more without refreshing: `ExplainLogical` still prints `Selection (est. rows=1, exact)` while the operator emits 400. In the same plan the sibling `NodeByLabelScan` correctly updates to `est. rows=799` — one estimate live, one stale, **both tagged `exact`** | **#2772** |
| 3 | `exec.RenderPlan` has no caller and renders a plan missing the new columns | `grep -rn 'RenderPlan('` excluding `RenderPlanNode` matches **only its own definition**. Its godoc already says so honestly | **#2770** |
| 4 | `exec.OptionalExpand` is unreachable from any query | **Measured: 0 of 11 OPTIONAL MATCH shapes plan it.** Every one lowers to `OptionalApply(… Expand …)`, and no `ir.OptionalExpand` appeared in any logical plan either. `exec.NewOptionalExpand` is still constructed at `cypher/api.go:10358`, so the operator is *unwired*, not dead | **#2769** |
| 5 | `Expand`'s estimate ignores an intervening `Selection`, biasing its q-error high | Confirmed: `Expand` carries `exact` in 4 of 6 observed instances with no allowance for a filter between it and its input | #2767 finding, no task |
| 6 | Columnar fusion chains carry no estimate, so they contribute no q-error | Confirmed and sharpened — see §7.3. `ColumnarProject` renders `-` in 8 of 8 instances, `ColumnarFilter` likewise | #2765 limitation 3, no task |
| 7 | A `LIMIT 0` `PROFILE` captures the tree with workers still in flight | Measured: `MATCH (n:B) WHERE n.v = 0 RETURN n.k + 1 LIMIT 0` at threshold 10 renders `ParallelScanProject` with `rows=0, dbhits=0` and `DbHitsKnown=true` — a **known zero that is not a fact about the workload** | #2762 finding, no task |
| 8 | The `PlanNode` godoc contradicts its own package's census | §7.1. `cypher/exec/plan.go:71-72`, `:86-88` | **none — new** |
| 9 | Three in-code assertions still say the statistics are inert | §7.5 | **none — new** |
| 10 | Estimate coverage: 80 of 110 operator instances render `-` | §1, §5 D3. **Corrected 2026-09-08 (#2787):** predominantly a property of the estimator, but **not wholly** — the columnar fusion chains lose a figure their logical node does carry, which is a defect of #2765's mapping. Measured re-partition in the 2026-09-08 addendum | documents corrected under **#2787**; the code fix is a separate task |
| 11 | The count-store leaves render `?` rather than a known `0` | §1. **Settled 2026-09-08 (#2777):** measured per arm rather than argued. `AllNodesCountScan` reports a path-aware measured figure (a real `0` on its counter, a full walk when the counter declines — reached by an ordinary count query against one uncommitted `CREATE`); `LabelCountScan` stays `?` because its fallback sits below the resolver interface, which reports only `(count, ok)` | **#2777** |

Items 8 to 10 had **no backlog task** when this audit closed; item 11 was filed as #2777
and closed on 2026-09-08. They were reported here rather than filed, because filing is a
scope decision for the user, not for this audit.

## 9. Limits of this audit

* **Not exhaustive over operators.** 28 operator kinds were observed across the main
  harness's 31 shapes; `columnarExpand` and `ColumnarFilter` were observed in the
  statistics harness instead, and the §1 tables name the arm for each. An operator these
  shapes never planned is not classified here — `ExpandIntersect`, `CSRProbe` and the hash
  joins were not reached.
* **`columnarExpand` is not independently proved MEASURED.** Its observed arm emits as many
  rows as it walked, so the discriminating comparison this document requires was not
  obtained for it. It is on the marker census; that is a source reading, not a measurement.
* **No Bolt-driver round trip.** D8 and D9 were audited in `bolt/server/plan_meta.go`
  source. `neo4j-go-driver` is not among the pinned clones, so the driver-decoder claim
  behind D8 is carried forward **unconfirmed**.
* **The Neo4j evidence is from the Community clone**, so it describes the
  interpreted/slotted runtime. The Enterprise pipelined runtime was not examined.
* **No performance measurement, deliberately.** The host was not quiet (load 1.42–2.52).
  Every figure here is a count or a glyph. The −99.50% in §6 is a **db-hits count ratio**,
  not a timing.
* **The metrics were not observed firing.** `internal/metrics` is import-restricted, so the
  external harness cannot install a recording backend. The claim in §7 that
  `cypher.stats.qerror` fires on a never-refreshed engine rests on the call path
  (`cypher/plan_qerror.go:252-256` has no collector check) **plus** the measured
  precondition that `NodeByLabelScan` carries `exact` on an engine that never refreshed —
  observed in 24 of 24 instances. That is strong, but it is not the metric itself.

## 10. Reproduction

The harness lives outside the repository, in the session scratchpad, as an external module
with a `replace` directive onto this tree:

```bash
# The harness: 31 shapes x 8 graphs, dumping every operator field and every surface
cd <scratchpad>/audit2768
go build -o harness .        && ./harness      > obs.json 2> census.txt
go build -o statsharness ./stats   && ./statsharness   > stats_obs.txt   2>&1
go build -o reorderharness ./reorder && ./reorderharness > reorder_obs.txt 2>&1
go build -o optexpbin ./optexp     && ./optexpbin      > optexp_obs.txt  2>&1
go build -o dcheckbin ./dcheck     && ./dcheckbin      > dcheck_obs.txt  2>&1
python3 census.py > census_final.txt
```

The gates, run on the closing tree, exit status read from inside each log:

```bash
go test -count=1 -run 'TestProfileDbHits_|TestExplainFidelity_|TestProfileRowsRemoved|TestProfileEstimate|TestQError|TestDbHitsClassification' ./cypher/ ./cypher/exec/
#   -> ok cypher 1.075s, ok cypher/exec 0.406s, GO_TEST_EXIT=0

go test -count=1 -v -run TestTCKExecution ./cypher/tck/...
#   -> "3897 scenarios, 3897 passed, 0 failed, 0 undefined, 0 inconclusive (baseline=3897)"
#   -> TCK_EXIT=0

go test -count=1 ./cypher/ ./cypher/exec/ ./cypher/explain/ ./cypher/ir/ ./bolt/server/
#   -> all ok, PKG_EXIT=0
```

`make ci` was **not** run by this task: the full gate is the sprint-close step that follows
it. `go test -race` and `golangci-lint` were likewise not run here, and this document makes
no claim about them.

Reference clones, verified with `git rev-parse HEAD` before reading:

```bash
git -C <scratchpad>/refs/neo4j    rev-parse HEAD  # 679feffbfb7a9189aba360ea98eef7fc3371e275
git -C <scratchpad>/refs/memgraph rev-parse HEAD  # cdd8b5e1285f2b4a8ee4c29710aecb7619f3e98d
git -C <scratchpad>/refs/postgres rev-parse HEAD  # 4639b6cfe3f310b71e1e227dd2a915b053992c9b
```

Environment: Apple M4, 10 cores, 32 GB, macOS 26.5.2 (darwin arm64), Go 1.27.1, load
average 1.42–2.52 throughout.

---

## Addendum — 2026-09-08, rmp #2787 (sprint 357)

**Tree:** `release/0.14.1`, commit `9ec1a6c790c4692669ec0388513614c5688f00ff`. **Host:** Apple
M4, 10 cores, 32 GB, macOS 26.5.2 (darwin arm64), Go 1.27.1, load average 3.48 / 4.77 / 3.98 —
**the host was not quiet**, and, exactly as in the body of this document, every figure below is
a count, a glyph or a structural comparison, so none of them is affected by load. No timing is
claimed here.

### The `-` on a columnar chain IS a mapping loss

Three statements shipped — two in this document, one in `release-notes/v0.14.0.md` — asserting
that the missing estimates on a columnar plan belong to the estimator and not to #2765's
mapping. **They are false for the columnar family**, and a single query pair disproves them.

Seed one `:Component {tag:'rare'}` among 60 `{tag:'common'}`, call `Engine.RefreshStatistics`,
then read the two plans. The two queries differ in **one** thing — what the `RETURN` projects —
so the planner reasons about identical inputs in both:

```
MATCH (c:Component) WHERE c.tag = 'rare' RETURN c        MATCH (c:Component) WHERE c.tag = 'rare' RETURN c.key

  ExplainLogical (IDENTICAL in both):                      ExplainLogical (IDENTICAL in both):
    ProduceResults                                           ProduceResults
    └─ Projection                                            └─ Projection
       └─ Selection (est. rows=1, exact)                        └─ Selection (est. rows=1, exact)
          └─ NodeByLabelScan (est. rows=61, exact)                └─ NodeByLabelScan (est. rows=61, exact)

  Explain (physical):                                      Explain (physical):
    Project                                                  ColumnarProject
    └─ Filter (est. rows=1 exact)                            └─ ColumnarFilter                <- NO ESTIMATE
       └─ NodeByLabelScan (est. rows=61 exact)                  └─ NodeByLabelScan (est. rows=61 exact)
```

The logical node exists. It carries a figure. The figure survives the row lowering and does not
survive the columnar one. The scan carries the same `61 exact` on both physical plans, which
rules out the reading that the two arms were planned against different statistics.

### The mechanism, measured — and where the source comment is imprecise

`cypher/plan_estimate_physical.go:69-72` already says the loss is in the mapping, and is the
statement the three passages should have been aligned to. Its wording is nonetheless not quite
what the build does. Instrumenting the estimate sink directly — the map `buildOperator` fills,
read back before rendering — gives, for the pair above:

| build | operators in the tree | claims in the sink |
|---|---|---|
| row (`RETURN c`) | `Project`, `Filter`, `NodeByLabelScan` | **3** — `NodeByLabelScan` 61 exact, `Filter` 1 exact, `Project` empty |
| columnar (`RETURN c.key`) | `ColumnarProject`, `ColumnarFilter`, `NodeByLabelScan` | **2** — `NodeByLabelScan` 61 exact, `ColumnarProject` empty |

Two claims for three operators. The `ColumnarFilter` is claimed by **nothing**, and the reason
is not that an operator was discarded: `tryBuildColumnarFilterChain` calls `buildOperator` on
`sel.Child` **only** (`cypher/api.go:17171`), and builds `exec.NewColumnarFilter` itself from
`sel.PredicateExpr`. The `ir.Selection` therefore never reaches the one funnel
where a logical node and its operator are both in hand, so **no operator is ever produced for
it to discard**. The same holds for `tryBuildColumnarAggSource`. The comment's "discarded"
wording is accurate for one case only — `tryBuildColumnarExpandFilterChain`, where the
`*exec.Expand` that `buildOperator` produced **is** claimed (`est. rows=60, exact` observed) and
is then replaced by `exec.NewColumnarExpand`, whose identity is not in the map, so the claim is
never looked up.

`ColumnarProject`, by contrast, **is** claimed — by the `ir.Projection`, with an empty estimate,
exactly as the row `Project` is. Its `-` is estimator coverage, not a mapping loss. The loss is
one operator wide per chain, and it lands on the operator that carries the selectivity.

### The re-partition, measured

The published **80 of 110** cannot itself be re-partitioned: the external `audit2768` harness
that produced it did not survive its session, so its 31 shapes over 8 graphs are not
recoverable. A census was therefore built afresh, over a **different and recorded** shape set,
and its totals are **not comparable with 110** — only its proportions carry over. Reported here
because a re-partition asserted without measurement would repeat the error being corrected.

**Census** — 31 distinct shapes over 2 graph fixtures, each run twice (before and after
`RefreshStatistics`) = 62 shape-runs, **228 operator instances**:

| | instances |
|---|---:|
| renders a figure | 71 |
| renders `-` | **157** |
| … of which **claimed with an empty estimate** — ESTIMATOR COVERAGE | **125** |
| … of which **never claimed at all** — NOT MAPPED | **32** |

The 32 unmapped instances, by operator: `ColumnarFilter` 20, `columnarExpand` 4,
`ColumnarProject` 2, `Project` 4, `SingleRow` 2. **26 of the 32 are columnar.** The six that are
not are intermediates built outside `buildOperator`'s funnel; that mechanism is read from the
source (`profileIntermediate`, `cypher/api.go:9792-9834`) and was **not** separately measured
here.

Being unmapped costs a figure only where the logical node had one. That half was measured as a
**single-variable A/B**: the same fixtures, the same shapes, the same estimator, differing only
in whether the columnar recognisers are allowed to fire (`forceColumnarChainDeclineForTest`).

| arm | instances | carry a figure | render `-` |
|---|---:|---:|---:|
| columnar recognisers ON | 228 | 71 | 157 |
| columnar recognisers forced to DECLINE | 230 | **83** | 147 |

**12 estimates are lost purely to the columnar mapping** across this census — every one of them
in a columnar shape, and each visible as a straight substitution in the rendered plan:

```
                                          recognisers ON            recognisers DECLINED
MATCH (a:A) WHERE a.tag='rare' RETURN a.k   ColumnarFilter    ->    Filter (est. rows=1 exact)
MATCH (a:A)-[:L]->(b:B) RETURN b.k          columnarExpand    ->    Expand (est. rows=100 exact)
MATCH (a:A) WHERE a.tag='rare' RETURN count(a)  ColumnarFilter ->   Filter (est. rows=1 exact)
```

So, on this census, the `-` instances partition as **125 estimator coverage · 20 unmapped with
no figure behind them · 12 unmapped with a real figure lost, all columnar**. The proportion that
the corrected text has to carry is that the columnar mapping loss is a small minority of the
`-` cells and is nonetheless real, attributable, and concentrated on the commonest read shape in
the language.

### What this addendum does NOT establish

* It does **not** re-derive the body's 110-instance census; that instrument is gone, and the
  body's figures stand as measured on `83ba8d8b`.
* It does **not** measure the morsel-parallel leaves or the two build-synthesised leaves. Their
  mechanisms are read from `cypher/plan_estimate_physical.go` and are unverified here.
* It changes **no behaviour**. The code fix for the mapping is a separate task.

### Reproduction

The census was an in-package Go test, run and then removed — an instrument, not a gate. Its
binary is as gone as `audit2768`'s, so the **shape list and the fixtures are recorded here**
instead. That is the whole lesson of the 110 figure: a count whose inputs were not written down
cannot be re-partitioned later, only re-measured.

**Fixture A** — 400 `:A` nodes, `tag` = `'rare'` on the first and `'common'` on the rest, `age`
= `i mod 80`, `k` = the node key; 100 `:B` nodes, `v` = `i mod 4`, `k` = the node key; 100
`(:A)-[:L]->(:B)` edges pairing the first 100 of each. **Fixture C** — a 60-node `:N` chain
linked by `[:NEXT]`, `i` = the index.

Each fixture is walked twice, once before `RefreshStatistics` and once after, which is what
makes 31 shapes into 62 shape-runs.

**Shapes on fixture A** (28): `MATCH (a:A) RETURN a` · `… RETURN a.k` · `… WHERE a.tag='rare'
RETURN a` · `… RETURN a.k` · `… RETURN a.k, a.age` · `… WHERE a.age<30 RETURN a` · `… RETURN
a.k` · `… WHERE a.tag='rare' RETURN count(a)` · `MATCH (a:A) RETURN count(a)` · `MATCH (n)
RETURN n` · `MATCH (n) RETURN count(*)` · `MATCH (a:A)-[:L]->(b:B) RETURN b` · `… RETURN b.k` ·
`… WHERE b.v=0 RETURN b.k` · `… WHERE a.tag='rare' RETURN b` · `MATCH (a:A), (b:B) WHERE
a.age=b.v RETURN count(*)` · `MATCH (a:A) RETURN a.k ORDER BY a.k LIMIT 5` · `… WHERE
a.tag='rare' RETURN a.k ORDER BY a.k` · `MATCH (b:B) RETURN b.v AS g, count(*) AS c` · `MATCH
(a:A) WITH a.age AS x WHERE x>40 RETURN x` · `… WHERE a.tag='rare' OR a.age=3 RETURN a` · `…
WHERE a.tag='rare' RETURN a.k SKIP 0 LIMIT 1` · `MATCH (a:A) OPTIONAL MATCH (a)-[:L]->(b:B)
RETURN a, b` · `MATCH (a:A) WHERE NOT (a)-[:L]->() RETURN count(a)` · `UNWIND [1,2,3] AS x
RETURN x` · `MATCH (a:A) WHERE a.k='a0' RETURN a` · `… RETURN a.tag` · `MATCH (a:A) RETURN
DISTINCT a.tag`.

**Shapes on fixture C** (3): `MATCH (a:N {i:0})-[*3..3]->(z) RETURN z` · `MATCH (a:N {i:0}),
(b:N {i:59}) MATCH p = shortestPath((a)-[*]-(b)) RETURN p` · `MATCH (n:N) WHERE n.i>30 RETURN
n.i`.

For each shape the instrument builds the physical plan through the same path
`Engine.explainPhysical` uses — `planEstimatesFor` then `Engine.buildReadPhysical` — walks the
operator tree, and records per operator whether it renders a figure and whether its UNWRAPPED
identity is a key in the sink. The A/B arm repeats the whole thing with
`Engine.forceColumnarChainDeclineForTest` set, which is the only variable that differs.

The DURABLE artefact is the gate `cypher/plan_estimate_columnar_gap_test.go`, which pins the
asymmetry so this claim cannot drift again in either direction — it fails both if the columnar
chain starts carrying an estimate and if the logical node stops carrying one:

```bash
go test -count=1 -run 'TestPlanEstimateColumnarGap' -v ./cypher/   # 3 tests, EXIT=0
```

Its non-vacuity was established by four single-variable neutralisations, each applied to the
production source, run, and reverted: forcing the columnar recognisers to decline (2 failures);
removing the physical `ir.Selection` estimate (2); removing the logical tree's Selection
annotation (1); and **simulating the fix** by recording the Selection's claim on the
`ColumnarFilter` (2). Seven failures over four arms, with all three gates covered.

### Gates run for this addendum

```bash
go build ./...                                            # BUILD_EXIT=0
go vet ./...                                              # VET_EXIT=0
gofmt -l .                                                # empty list
go test -race -count=1 ./cypher/...                       # RACE_EXIT=0, all packages ok
go test -count=1 -v ./cypher/tck/...                      # TCK_EXIT=0
#   -> "TCK execution: 3897 scenarios, 3897 passed, 0 failed, 0 undefined, 0 inconclusive (baseline=3897)"
#   -> "TCK error-type fidelity: 122/695 error scenarios raised the exact expected type (124 classified; baseline=122)"
go test -count=1 ./internal/docscheck/... ./internal/cypherdocgate/... ./internal/scriptgate/...   # DOCS_EXIT=0
golangci-lint run ./...                                   # LINT_EXIT=0, 0 issues
```

`make ci` was **not** run by this task, and neither the soak nor the nightly layer was; this
addendum makes no claim about them.
