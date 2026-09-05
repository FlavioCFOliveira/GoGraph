# Do `EXPLAIN` and `PROFILE` report factually and honestly?

**Audit date:** 2026-09-03 · **Tree:** `feature/353-gograph-optimization-laboratory`, base commit
`c533d3e777251692845ba52435ac33e8412c0808` · **rmp task:** #2720 (sprint 353)

## Why this audit exists

Sprint 353 optimises the module against measurements. `EXPLAIN` and `PROFILE` are the
instruments a user reads to understand a plan. An instrument that reports an inference as a
measurement, or renders a plan that is not the plan that runs, invalidates every conclusion
drawn through it.

The question is not whether the numbers are useful — they are. The question is whether the
**output is honest about what each number is**. This document classifies every figure the
pair prints as **MEASURED**, **DERIVED**, **ESTIMATED** or **UNCOUNTED**, states where the
rendering fails to distinguish them, and records what was corrected and what was left.

## Method

Every claim below was established by running the engine on this machine, not by reading the
code alone. The measurements come from purpose-built query shapes, each with a **control arm**
that makes the subject arm's number falsifiable — a subject and a control that provably do the
same storage work while reporting different figures. The three reference implementations were
read **in source at a pinned commit**, never from documentation:

| Project | Ref | Commit |
|---|---|---|
| Neo4j | tag `5.26.16` | `679feffbfb7a9189aba360ea98eef7fc3371e275` |
| Memgraph | `master` | `cdd8b5e1285f2b4a8ee4c29710aecb7619f3e98d` |
| PostgreSQL | `REL_17_STABLE` | `4639b6cfe3f310b71e1e227dd2a915b053992c9b` |

No throughput number appears in this document. The host was not quiet, and nothing here needs
one: every finding is a count or a structural comparison, neither of which a busy host changes.

## The surfaces

There are seven ways to obtain a plan, plus the Bolt wire. They fall into three groups, and
**within each group they agree**; the disagreements are between groups and are documented as
such.

| # | Surface | Plan kind | Executes? | Figures printed |
|---|---|---|---|---|
| 1 | `Engine.Explain` (`cypher/api.go:2738`) | physical (read) / logical (write) | no | none |
| 2 | `Engine.ExplainLogical` (`cypher/api.go:2934`) | logical | no | `Est.Rows` + provenance |
| 3 | `Engine.ExplainTable` (`cypher/explain_table.go:187`) | logical | no | `Est.Rows` |
| 4 | `Engine.Profile` (`cypher/api.go:2794`) | physical | yes | `rows`, `dbhits`, `time` |
| 5 | `Engine.ProfileTable` (`cypher/explain_table.go:282`) | physical | yes | `Rows`, `DbHits`, `Time (ms)`, `Total` |
| 6 | `EXPLAIN <stmt>` prefix → `Result.Plan()` (`cypher/plan_prefix.go:318`) | physical (read) / logical (write) | no | none |
| 7 | `PROFILE <stmt>` prefix → `Result.Profile()` (`cypher/plan_prefix.go:339`) | physical | yes | `Rows`, `DbHits`, `Time` |
| — | Bolt terminal `SUCCESS` `plan`/`profile` (`bolt/server/plan_meta.go:65`) | as 6 / 7 | as 6 / 7 | `rows`, `dbHits`, `time` |

Surfaces 1, 4, 5, 6 and 7 all render one `exec.PlanNode` tree built by the single
`buildReadPhysical` path, so they cannot name different operators for the same query. Surfaces
2 and 3 share one `explainInputs.walk`, so they cannot disagree either.

## 1. The classification

### Figures that are MEASURED

| Figure | Where it is produced | What is measured |
|---|---|---|
| `rows=` / `Rows` (row mode) | `cypher/exec/profile.go:240` (`p.rows++`) | successful `Next` returns |
| `rows=` / `Rows` (columnar) | `cypher/exec/profile.go:331` (`p.rows += int64(n)`) | rows appended by `FillChunk` |
| `time=` / `Time (ms)` | `cypher/exec/profile.go:238`, `:330` (`p.elapsed += time.Since(start)`) | wall-clock inside the operator's own `Next`/`FillChunk`, **inclusive of children** |
| `Total Time (ms)` | `cypher/explain_table.go:296` | the ROOT node's elapsed time — correct, because the root's time already contains every child's |
| `dbhits` for `VarLengthExpand` | `cypher/exec/varlen_expand.go:866` → `cypher/exec/profile.go:303` | relationship slots the BFS actually enumerated (`cypher/exec/varlen_expand.go:670`) — **new in this audit** |

### Figures that are DERIVED

| Figure | Where it is produced | What it is derived from |
|---|---|---|
| `dbhits` for a marked access path | `cypher/exec/profile.go:305-307` (`return p.rows`) | the operator's own emitted row count, on the contract asserted by `StorageRecordScan` |
| the marked set | `cypher/exec/profile.go:436-442` | `AllNodesScan`, `NodeByLabelScan`, `NodeByIndexSeek`, `NodeByIndexSeekSet`, `NodeByIndexRangeScan`, `Expand`, `OptionalExpand` |
| `Total Rows` | `cypher/explain_table.go:316` | sum over every operator — a cost measure, **not** the result's row count |
| `Total DbHits` | `cypher/explain_table.go:317` | sum over every operator's `DbHits` cell, so it inherits every gap below |
| operator `Name` | `cypher/exec/plan.go:123` (`operatorName`) | the concrete Go type of the operator that ran — the strongest property of the whole surface |
| operator `Detail` | `cypher/exec/plan.go:108` → `cypher/exec/plan_detail.go` | the physical decision (label scanned, bound sought, join build side, tier) |

### Figures that are ESTIMATED

| Figure | Where it is produced | Provenance marker |
|---|---|---|
| `(est. rows=N, exact)` | `cypher/explain_estimate.go:64` | `=` and the literal `exact` |
| `(est. rows~N, stats)` | `cypher/explain_estimate.go:68` | `~` and the literal `stats` |
| `(est. rows~N, stats, err=…)` | `cypher/explain_estimate.go:82` | `~`, `stats`, plus the certified absolute selectivity error |
| `(est. rows~N, heuristic)` | `cypher/explain_estimate.go:70` | `~` and the literal `heuristic` |
| (omitted) | `cypher/explain_estimate.go:71-72` | an absent or stale statistic prints **nothing** rather than a fabricated exact |
| `Est.Rows` cell `N` | `cypher/explain_table.go:100-103` | exact |
| `Est.Rows` cell `~N` | `cypher/explain_table.go:104` | approximate |
| `Est.Rows` cell `-` | `cypher/explain_table.go:97-99` | no estimate is derivable, or the statistic is stale |

**This half of the surface is exemplary.** The estimate machinery carries its provenance into
the rendering at four levels of confidence, marks approximation with a tilde, prints a
certified error term where it has one, and refuses to print a number it cannot stand behind.
Neither Memgraph nor PostgreSQL does anything comparable: Memgraph prints no estimate at all
(`src/query/plan/pretty_print.cpp` contains zero occurrences of `cost`, `cardinal` or
`estimat`), and PostgreSQL's `cost=…rows=…` carries no confidence marker.

### Figures that are UNCOUNTED but render as `0`

| Operator | Reported `DbHits` | What it actually reads |
|---|---|---|
| `ShortestPath`, `AllShortestPaths` | `0` | relationship records, across a bidirectional BFS |
| `ParallelScanProject` | `0` | one node reference per node its workers walk |
| `ParallelAggregateScan`, `ParallelCountScan` | `0` | the whole label, while emitting one row per group |
| `LabelCountScan`, `AllNodesCountScan` | `0` | one maintained counter — **`0` is defensible here**: no records were read |

`0` is also what every pure row transformer reports, honestly. The column has no way to tell
"counted, and it is zero" from "not counted at all".

## 2. Verdict on distinguishability

**The `PROFILE` output does not distinguish its three kinds of figure, and the `PROFILE` and
`EXPLAIN` outputs are never shown together.**

```
| Operator                    | Rows | DbHits | Time (ms) |
```

`Rows` is measured. `Time (ms)` is measured. `DbHits` in the same table is measured for one
operator, derived for seven, and uncounted-rendered-as-zero for at least five. Nothing in the
header, the cell, or the row says which. In the indented renderer the same three sit inside one
parenthesis (`cypher/exec/plan.go:196`):

```
(rows=%d, dbhits=%d, time=%s)
```

Against that, the renderer **does** already draw one honesty distinction, and draws it well:
an operator the instrumentation did not reach is labelled `(not measured)` rather than left
bare (`cypher/exec/plan.go:198`, `cypher/explain_table.go:308`), on the explicit reasoning that
"a bare node in a profiled plan would read as an operator that cost nothing". That is the right
instinct — it is simply not applied to the `DbHits` column, where the identical ambiguity
exists with the opposite sign.

**`Est.Rows` and the measured columns cannot appear in the same table**, because they belong to
two different renderers over two different plans: `FormatPlanTable`
(`cypher/explain/text_tree.go:49-51`: `Operator | Est.Rows | Vars`) renders the LOGICAL plan,
and `FormatReport` (`cypher/explain/profile.go:128-131`: `Operator | Rows | DbHits | Time (ms)`)
renders the PHYSICAL one. So the specific hazard the task anticipated — an estimate and a
measurement rendered alike in one row — **does not occur**. What occurs instead is the opposite
gap: the reader cannot compare them at all without running two calls and aligning two
differently-shaped trees by hand.

## 3. Is the db-hits model correct? — **No. Refuted by measurement.**

The model, stated at `cypher/exec/profile.go` (`StorageRecordScan`) and previously in
`docs/cypher.md`: *"an operator that reads records from storage reads exactly one per row it
emits, so the boundary row count IS the access count."*

### Refutation 1 — a variable-length expansion (202x, since corrected)

Graph: one `:Root` with 200 out-edges, of which exactly one continues into a 3-hop chain. A
level-synchronous BFS bounded at 3 hops enumerates the same 202 relationship slots whatever
emission window it is given; only the rows differ. The `[*1..3]` arm emits one row per enqueued
slot, so **its row count is a measurement, by the engine itself, of the traversal count**.

Before the fix:

```
MATCH (r:Root)-[*1..3]->(z)      VarLengthExpand  rows=202  dbhits=202
MATCH (r:Root)-[*3..3]->(z)      VarLengthExpand  rows=1    dbhits=1     <-- 202 slots read
```

Same BFS, same 202 slots, reported figure moved **202x**. The derived number was tracking rows,
which is what it is, and not storage reads, which is what the column is for.

After the fix (`cypher/exec/varlen_expand.go:866`):

```
MATCH (r:Root)-[*1..3]->(z)      VarLengthExpand  rows=202  dbhits=202
MATCH (r:Root)-[*3..3]->(z)      VarLengthExpand  rows=1    dbhits=202
```

The second line is now the query the column exists to expose: one row returned for 202
relationship reads.

### Refutation 2 — a type-filtered single hop (100x, **not** fixed)

`Expand` walks every slot of the source's adjacency run and rejects the ones the type filter
does not admit (`cypher/exec/expand.go:460`, the `edgeSkip` outcome: *"an edge was consumed but
filtered/morphism-rejected"*). Graph: one `:Root` with 99 `:LIKES` and 1 `:KNOWS` out-edge.

```
MATCH (r:Root)-->(b)             Expand  rows=100  dbhits=100
MATCH (r:Root)-[:KNOWS]->(b)     Expand  rows=1    dbhits=1     <-- same 100-slot walk
```

Neo4j charges the hit regardless of the predicate's outcome — `DefaultNodeCursor.java:199-210`
calls `tracer.onHasLabel(label)` **before** returning `false` for a rejected node — so a
GoGraph reader comparing a filtered plan against a Neo4j one is comparing incompatible
definitions, not just different scales.

### Refutation 3 — the parallel tier reports zero for a full scan

The same query, planned two ways by a threshold the reader did not set:

```
ParallelScanThreshold = 20000 :  NodeByLabelScan [B]                                rows=2000  dbhits=2000
ParallelScanThreshold = 10    :  ParallelScanProject                                rows=2000  dbhits=0
```

2000 nodes walked in both arms; `0` in one of them, in a column where `0` also means "read no
storage".

### Where the model does hold

For `AllNodesScan`, `NodeByLabelScan`, `NodeByIndexSeek`, `NodeByIndexSeekSet` and
`NodeByIndexRangeScan` the identity is exact: each emits one row per bitmap posting it
iterates, with no filtering inside the operator. It also holds for an **unfiltered** `Expand`.
The model is not wrong everywhere; it is wrong where it was stated as universal.

One further precision the code deserves: these leaves emit a NodeID from a bitmap and never
touch a node record — the record is read later, by a property accessor that counts nothing. So
even where the identity holds, the figure counts **access-path postings**, not physical record
fetches. The corrected documentation now says so.

## 4. Does `EXPLAIN` show the plan that actually runs? — **Yes, with one stated exception.**

`Engine.explainPhysical` (`cypher/api.go:2761`) claims to build the tree "exactly as the read
path builds it". Tested against a real execution, using the plan `PROFILE` captured **from the
operators that ran** as the oracle — not against another rendering
(`cypher/explain_fidelity_test.go`):

| Condition | Result |
|---|---|
| cold (empty plan cache) | identical, operator for operator and detail for detail |
| warm (**plan-cache hit**) | identical |
| parameter **bound** (`$a`, equality and range) | identical |
| equi-join subject to hash-join substitution | identical |
| single-hop expansion | identical |
| **parameter UNBOUND** | **DIVERGES** |

The exception, pinned by `TestExplainFidelity_UnboundParameterDivergesFromTheRun`:

```
EXPLAIN MATCH (n:P) WHERE n.age = $a RETURN n.name       (no params)
  ColumnarProject / Filter / NodeByLabelScan [P]

EXPLAIN / PROFILE, same statement, $a = 7
  ColumnarProject / Filter / NodeByIndexRangeScan [range=7..7]
```

`runExplainPrefixed` deliberately does not require parameters, and Neo4j behaves the same way,
so this is not a defect in the decision. It is a defect in the **output**: the reader is shown
a full label scan for a query that will seek, and nothing on the page says a value was missing.
That is exactly the class of surprise sprint 353 already recorded once (a parameter that
full-scanned where the identical literal seeked). It is now documented at the entry point and
pinned by a test; making it visible to the reader is a rendering change and is left as a
recommendation below.

## 5. Comparison with the three reference implementations

### Where GoGraph matches or leads

| Property | GoGraph | Peers |
|---|---|---|
| `EXPLAIN` executes nothing | yes, diverted before any transaction opens (`cypher/plan_prefix.go`), proved by a control arm that mutates | Neo4j `CypherCurrentCompiler.scala:552-563`; Memgraph `interpreter.cpp:4214-4283`; PostgreSQL `explain.c:678-702` |
| Plan names cannot lie about the operator | **stronger than all three** — the name is the concrete Go type of the value that ran (`cypher/exec/plan.go:123`), so a substituted `HashJoin` is named `HashJoin` because it IS one | all three build a plan *description* alongside the executable plan |
| Estimate provenance marked in the output | **unique** — `exact` / `stats` / `heuristic`, `~` for approximate, omitted when stale, with a certified error term | Neo4j prints `Estimated Rows` unqualified; Memgraph prints none; PostgreSQL prints `cost=` unqualified |
| Unmeasured operator labelled | yes, `(not measured)` | Neo4j blanks the cell; PostgreSQL prints `(never executed)` |
| Page-cache fields | **omitted, not zeroed** (`bolt/server/plan_meta.go:29-31`) — GoGraph has no page cache and a `0` would be a measurement claim | exactly Neo4j's and PostgreSQL's discipline (below) |
| Cartesian-product warning | yes, and carried on a prefixed statement (`cypher/notification.go:36`, attached at `cypher/plan_prefix.go:99`) | Neo4j `Clause.scala:842-859` |

The page-cache decision deserves emphasis: it is the *correct* rule, applied correctly, and the
audit's central criticism is that **the same rule is not applied to `DbHits`**. Omitting a
figure you did not measure and printing `0` for a figure you did not count are the same
question answered two ways in one codebase.

### Where GoGraph diverges, and whether it matters

| # | Divergence | Reference evidence | Does it matter here? |
|---|---|---|---|
| D1 | **Db-hits derived from rows, not counted at the storage layer** | Neo4j counts real kernel cursor accesses: `OperatorProfileEvent implements KernelReadTracer`, every read callback increments `dbHit()` (`OperatorProfileEvent.java:24, 44-86`); the hit and row counters are separate fields (`ProfilingTracer.java:114-115, 136-138, 146-148`) | **Partly.** The premise that "the peers count for real" is only half true: **Memgraph's `ACTUAL HITS` is also derived** — `actual_hits++` in the `ScopedProfile` constructor (`scoped_profile.hpp:58, :89`), placed at the head of each cursor's `Pull()` (`operator.cpp:597, 603, 610`), so it counts pull invocations, not storage accesses. Deriving is not unusual; **not saying so in the output is** |
| D2 | **No `Rows Removed by Filter` equivalent** | PostgreSQL `explain.c:3621-3645`, emitted at 19 call sites | **Yes, and it is the sharpest missing figure.** It is precisely the signal that would expose refutation 2: read-but-rejected is invisible in GoGraph |
| D3 | **Estimated and actual never side by side** | Neo4j puts `ESTIMATED_ROWS` and `ROWS` in adjacent columns of one table (`renderAsTreeTable.scala:212`, header asserted verbatim at `RenderAsTreeTableTest.scala:277`); PostgreSQL prints `(cost=…rows=…)` and `(actual …rows=… loops=…)` on the same line (`explain.c:1811` and `:1853`) | **Yes — the largest gap.** This is the signal a reader uses to spot a plan chosen on a bad guess, and GoGraph cannot show it: the two tables render two *different plans* (logical vs physical), so their rows do not correspond one to one |
| D4 | **A figure that was not counted prints `0`** | Neo4j omits at three levels — argument dropped when `value == OperatorProfile.NO_DATA` (`PlanDescriptionBuilder.scala:156-161`, `NO_DATA = -1L` at `OperatorProfile.java:59`), cell blanked (`renderAsTreeTable.scala:415-431`), column removed (`:54`) — and prints `"?"` or `"x + ?"` for an incomplete total (`renderSummary.scala:37-44`). PostgreSQL suppresses every zero-valued buffer field individually under the comment `/* Show only positive counter values. */` (`explain.c:3764-3814`) | **Yes.** Both peers treat "did not measure" as a distinct state with its own glyph. GoGraph has that discipline for page-cache fields and for `(not measured)`, and does not apply it to `DbHits` |
| D5 | **No `loops` / per-invocation figure** | PostgreSQL divides actuals by `nloops` (`explain.c:1844-1847`) so its `rows` is comparable with the per-loop estimate; Neo4j does not print loops either | **Marginal.** GoGraph's inner-side operators report a lifetime total across re-`Init`s, which is a different but self-consistent convention, documented on `Engine.Profile` |
| D6 | **Parallel tier collapsed to one node** | PostgreSQL prints per-worker actuals (`explain.c:1901-1910`) | **Partly.** The collapse itself is a defensible contract, enforced rather than accidental. Its *rows* and *time* are honest totals; only its db-hits are a gap (D4) |
| D7 | **No memory figure** | Neo4j has `Memory (Bytes)` (`renderAsTreeTable.scala:203`); PostgreSQL has `BUFFERS` | **No.** GoGraph enforces result-memory budgets elsewhere; adding a per-operator memory column would need accounting the engine does not keep |
| D8 | **Bolt `plan` metadata omits `identifiers`** | the driver reads `identifiers` as a list of strings (`neo4j-go-driver` v5.28.4, `hydrator.go`; noted in `bolt/server/plan_meta.go:19`) | **Minor.** A driver's `Plan().Identifiers` is empty. The information exists (`ir.LogicalPlan.Vars`, already rendered in `ExplainTable`'s `Vars` column) but is not published |
| D9 | **Bolt `plan` metadata carries no estimates** | Neo4j's `args` carries `EstimatedRows` | **Yes, and it compounds D3.** A driver's `ResultSummary.Plan()` gets operator names and details but no numbers at all |
| D10 | **No eager-operator warning** | only `EagerLoadCsvNotification` exists in Neo4j 5.26 (`checkForEagerLoadCsv.scala:50`); no general eager notification was found | **No.** GoGraph has no `LOAD CSV`, and the peer does not have the general form either |
| D11 | **`EXPLAIN` on a WRITING statement returns a LOGICAL tree with nothing marking it as one** | — | **Yes.** `Engine.Explain` prints the header `(logical plan — a writing statement has no physical tree outside a transaction)` (`cypher/api.go:2751`), but the prefix form (`cypher/plan_prefix.go:146-149`) captures the same walk into an `exec.PlanNode` and drops both the header and the cardinality annotations. Two of the seven surfaces therefore describe the same statement differently, and a Bolt driver receives logical operator names in a shape indistinguishable from a physical tree |

One honesty note about a peer, recorded because it bears on how much weight D1 should carry:
**Memgraph's `ABSOLUTE TIME` is apportioned, not measured** — `AbsoluteTime = RelativeTime *
total_time` (`profile.cpp:38-42`), so its per-operator millisecond figure is the wall-clock
total distributed by a cycle ratio. GoGraph's `time=` is a real `time.Since` per operator.
Memgraph's `RELATIVE TIME` column (`interpreter.cpp:4385`) remains a presentation idea worth
considering: a percentage-of-total column makes the dominant operator visible without
arithmetic, and GoGraph could compute it from figures it already has.

## 6. What was changed

### Fixed

1. **`VarLengthExpand`'s db-hits are now MEASURED.** A new unexported
   `storageAccessCounter` interface (`cypher/exec/profile.go:465`) lets an operator report the
   records it actually read; `dbHits()` consults it before falling back to the derived count
   (`cypher/exec/profile.go:301-309`). `VarLengthExpand` implements it
   (`cypher/exec/varlen_expand.go:866`) from `totalEdgesVisited`, the counter its aggregate
   traversal budget already maintains for #1478, plus a lifetime accumulator carried across
   re-`Init`s (`cypher/exec/varlen_expand.go:366`) so an operator driven once per outer row
   reports its whole traversal and not just the last row's. **The counter already existed and
   is already paid for, so a non-`PROFILE` run gains no work** and the cost-when-off guarantee
   is untouched. `VarLengthExpand`'s `StorageRecordScan` marker was removed, because the
   identity that marker asserts is false for a traversal.
2. **The morsel-parallel leaves declare their gap in the rendered plan.**
   `ParallelScanProject`, `ParallelAggregateScan` and `ParallelCountScan` now carry
   `PlanDetail() == "parallel tier; db-hits not counted"`
   (`cypher/exec/plan_detail.go:116-129`), so the line reads
   `ParallelScanProject [parallel tier; db-hits not counted] (rows=2000, dbhits=0, time=310µs)`.
   The tier was previously unnamed in the plan as well, which is itself a physical decision a
   reader needs.
3. **Every documentation site that stated the model as universal was corrected**:
   `cypher/exec/profile.go` (`StorageRecordScan`, and the `Profiler` header's restatement),
   `cypher/exec/plan.go` (`PlanNode.DbHits`), `cypher/explain_table.go`
   (`ProfileTable`, `ExplainTable`), `cypher/plan_prefix.go` (`runExplainPrefixed`), and
   `docs/cypher.md`. Each now enumerates the exceptions with the measurement behind them.

### Tests that fail on the misleading form

`cypher/profile_dbhits_honesty_test.go`

* `TestProfileDbHits_VarLengthExpandCountsTraversalsNotRows` — the control/subject pair above.
  Mutation-verified: with the measured branch disabled it fails with
  *"reported dbhits=1 for `[*3..3]` and dbhits=202 for `[*1..3]`"*.
* `TestProfileDbHits_TypeFilteredExpandUnderReports` — **pins the known under-report at 1**, in
  both directions, so it cannot be silently re-described as an exact count and so a future fix
  forces the documentation to be updated with it.
* `TestProfileDbHits_ParallelLeafDeclaresItsGap` — asserts the marker, with the sub-threshold
  serial plan as the control that makes the zero a reporting gap rather than a fact about the
  workload. Mutation-verified: blanking the detail string fails it.

`cypher/explain_fidelity_test.go`

* `TestExplainFidelity_MatchesTheTreeThatRuns` — five query shapes × {cold, plan-cache hit},
  each comparing `EXPLAIN`'s tree against the tree `PROFILE` captured **from the run**.
* `TestExplainFidelity_UnboundParameterDivergesFromTheRun` — pins the one divergence, and fails
  if it is closed, so the documentation is reconciled deliberately.

### Left unfixed, deliberately

| Item | Why |
|---|---|
| `Expand`'s type-filter under-report | Correcting it needs a per-slot counter the operator does not maintain. `tryFwdEdge`/`tryRevEdge` advance `op.fwdStart`/`op.revStart` (`cypher/exec/expand.go:668, 720`), so an exact count is reachable in **O(1) per input row** by bracketing the cursor at `loadAdjacency`. That is cheap but not free, and it changes the "no counting CODE AT ALL" property #2222 established. **Recommended, and a decision for the user, not for this audit.** |
| `ShortestPath` / `AllShortestPaths` reporting `0` | Their `totalEdgesTraversed` (`shortest_path.go:177`, `:1295`) is incremented **only** in the exhaustive path-predicate search (`:571`, `:1621`); the bidirectional BFS increments a local `iter` (`:1723`, `:1878`, `:2143`). Wiring it would report an authoritative-looking `0` for the common path — a worse misstatement than the current one, which at least matches every other uncounted operator. Fixing it properly means adding increments to the BFS, i.e. the same decision as above. |
| Rendering `-` instead of `0` for an uncounted `DbHits` | This is the change that would fully answer D4 and it is the audit's **top recommendation**. It changes what `Engine.Profile`, `Engine.ProfileTable` and `Result.Profile()` return, and committed tests assert that output; per the task's own instruction such a change is reported, not made. |
| Marking the EXPLAIN-prefix logical plan (D11) | Same reason. The minimal form is a `Detail` on the root node, which `TestPlanPrefix_WritingStatementRendersLogicalPlan` (`cypher/plan_prefix_test.go:555`) would fail, because it requires every captured operator line to appear verbatim in `ExplainLogical`'s output. **Recommended**, with that test to be updated in the same change. |
| A notification for an `EXPLAIN` planned with unbound parameters | `TestPlanPrefix_ExplainCarriesPlanTimeNotifications` (`cypher/plan_prefix_test.go:405`) pins the contract that a prefixed statement reports the **same** notifications as the un-prefixed run. An EXPLAIN-only notification breaks that contract by design, so it is a scope decision. |
| Publishing `identifiers` and estimates in the Bolt `plan` metadata (D8, D9) | Additive, but it is new figures on the wire rather than a correction, and D9 in particular is the wire half of D3. |

## 7. Limits of this audit

* **Not exhaustive over operators.** The classification covers every operator these query
  shapes planned. `ExpandIntersect`, `CSRProbe` and the columnar variants were observed
  (`columnarExpand` reports db-hits through its embedded `*Expand`, so it inherits refutation
  2) but were not each given a dedicated control arm. An operator that reads storage and
  implements neither interface reports `0`; the audit did not enumerate every such operator.
* **`ParallelAggregateScan` and `ParallelCountScan` were not observed in a rendered plan.**
  Their `0` is established from the interface set, not from a run. Only `ParallelScanProject`
  was reproduced end to end.
* **No Bolt-driver round trip.** The wire metadata was audited in source
  (`bolt/server/plan_meta.go`) against the driver's decoder as transcribed there; no actual
  driver was run against the server.
* **The Neo4j evidence is from the Community clone**, so it describes the interpreted/slotted
  runtime and the shared `runtime-util` tracer. The Enterprise pipelined runtime was not
  examined. One peer claim is a two-file inference rather than an observed run: that Community
  page-cache figures render as `0/0` (`Profiler.scala:161-163` plus the default
  `PageCacheStats(0,0)` at `InterpretedProfileInformation.scala:65`). It is reported as
  indicated, not confirmed.
* **No performance measurement.** The fixes add no work to a non-`PROFILE` run **by
  construction** — the counters they read already existed — but this was established by reading
  the code, not by benchmarking. A benchmark would in any case be uninformative here: there is
  no new instruction on any hot path to measure.

## 8. Reproduction

```bash
# The gates this audit added
go test -count=1 -run 'TestProfileDbHits_'   ./cypher/
go test -count=1 -run 'TestExplainFidelity_' ./cypher/

# The compliance and hygiene gates, all green at the time of writing
go test -count=1 -v -run TestTCKExecution ./cypher/tck/...
#   -> "3897 scenarios, 3897 passed, 0 failed, 0 undefined, 0 inconclusive (baseline=3897)"
go test -count=1 -race ./cypher/...
go test -count=1 ./cypher/ ./cypher/exec/ ./cypher/explain/ ./bolt/server/
golangci-lint run ./cypher/... ./bolt/server/...   # 0 issues
gofmt -l cypher/ cypher/exec/                       # empty
```

Environment: darwin 25.5.0 (arm64), Go toolchain as pinned by `go.mod`. The host was **not**
quiet — the sprint's other specialist was running concurrently — which is why this document
contains no timing comparison. Every figure quoted here is a count or a structural equality,
and neither is affected by load.
