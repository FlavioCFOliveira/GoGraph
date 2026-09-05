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
and neither is affected by load. (The rmp #2761 and #2762 addenda below DO contain timing
comparisons; each states its own conditions and its own measured noise floor.)

---

## Addendum — 2026-09-05, rmp #2760 (sprint 355)

This section records what changed after the audit, and one claim above that later
measurement **refuted**. Nothing earlier in the document has been edited: it is the
report of an investigation on a dated tree, and correcting it in place would hide
that the correction happened.

### The top recommendation was implemented

§6 "Left unfixed, deliberately" listed *"Rendering `-` instead of `0` for an
uncounted `DbHits`"* as the audit's top recommendation, deferred because it changes
what `Engine.Profile`, `Engine.ProfileTable` and `Result.Profile()` return. rmp
#2760 made that change, using `?` rather than `-`:

* `exec.PlanNode` gained `DbHitsKnown bool`. `DbHits` is meaningful only when it is
  true; every renderer prints `exec.DbHitsUnknown` (`?`) otherwise.
* An operator's class is decided by which of three marker interfaces it implements:
  `storageAccessCounter` (MEASURED), `StorageRecordScan` (DERIVED), and the new
  `noStorageAccess` (a KNOWN zero). Claiming none is UNKNOWN — the honest default.
* `Total DbHits` sums only the known cells and renders `N + ?` when it summed over a
  gap, which is Neo4j's `TotalHits` form verbatim (`renderSummary.scala`, 5.26.16).
* The Bolt `profile` metadata OMITS `dbHits` for an unknown operator, which is the
  rule §5 identified as already correct for the page-cache fields.
* `cypher/exec/dbhits_classification_test.go` carries the classification of every
  operator in the package, with the reason for each, and fails when the source and
  that census disagree.

### Refuted: `Project` is not a pure row transformer

§1 states: *"`Project`, a pure row transformer, also reports `dbhits=0`, honestly."*
That is **false**, and so is the same assumption about `Filter`.

A GoGraph expression can walk the graph. `cypher/pattern_eval.go`'s
`patternEvaluator` is passed into every per-row evaluation through cypher's
`evalRow` bridge, and its `EvalPattern` / `EvalPatternComp` traverse adjacency
directly. Measured on one `:Root` with 100 `:LIKES` out-edges
(`TestProfileDbHits_ExpressionEvaluationIsUncountedStorage`):

```
MATCH (r:Root) WHERE (r)-[:LIKES]->() RETURN r.k        total db-hits reported: 1
  ColumnarProject / Filter / NodeByLabelScan

MATCH (r:Root)-[:LIKES]->(l) RETURN DISTINCT r.k        total db-hits reported: 101
  Distinct / ColumnarProject / Expand / NodeByLabelScan
```

Both arms return the same one row and both must read the `:Root`'s relationship
slots to do so. The first reports the label scan alone. A pattern comprehension
inside a projection behaves identically —
`RETURN size([(r)-[:LIKES]->(x) | 1])` is evaluated **inside** `Project` (it is not
lowered to a `RollUpApply`) and the plan reports 1 db-hit for 100 slots read.

This is the same class of defect as refutation 3 in §3, in a place the audit did not
look, and it is why `Filter`, `Project`, `Sort`, `Top`, `Unwind`, the hash joins,
`RollUpApply` and `ProcedureCallOp` are all UNKNOWN rather than a known zero: each
holds a caller-supplied expression closure, and none can prove what that closure
reached.

### Still open after #2760

* Deciding the expression-closure operators **per instance** — a `Sort` whose every
  `SortKey.Eval` is nil provably reads nothing, and the planner knows that when it
  builds the operator. rmp #2760 classifies by TYPE, so those instances report `?`.
* `Expand`'s type-filter under-report (§6), the parallel leaves' count and
  `shortestPath`'s count remain uncounted; rmp #2761, #2762 and #2763 exist to
  convert them from UNKNOWN to MEASURED.
* §7's limit stands: `ParallelAggregateScan` and `ParallelCountScan` are still
  classified from the interface set rather than from an observed run.

## Addendum — 2026-09-05, rmp #2761 (sprint 355)

### Refutation 2 is FIXED: `Expand` now reports the slots it walked

§3's refutation 2 recorded a 100x under-report as a standing property, on the
reasoning that correcting it "needs a counter the operator does not have, whose
per-slot increment a non-PROFILE run would pay". **That reasoning was wrong about
the cost, and the correction is now in.**

`Expand` and `OptionalExpand` no longer carry `exec.StorageRecordScan`. Both
implement `storageAccessCounter`, and `columnarExpand` inherits the counter through
its embedded `*Expand`. The measurement §3 published now reads:

```
MATCH (r:Root)-->(b)          Expand  rows=100  dbhits=100
MATCH (r:Root)-[:KNOWS]->(b)  Expand  rows=1    dbhits=100
```

The rows still differ, which is correct — they are different result sets. The
db-hits no longer do, which is the point: both arms walk the same 100-slot CSR run
and reject 99 of them on the relationship type.

**The counter is not a per-slot increment.** The expansion cursors
(`op.fwdStart` / `op.revStart`) already advance exactly one position per slot
consumed, so the count is RECOVERED from the cursor positions by
`Expand.closeSlotWindow` — a fixed two subtractions and an add per INPUT ROW (the
forward cursor and the reverse one), never one per slot — at the two places
a cursor moves without having been walked (`loadAdjacency`, which jumps to the next
source's run, and `Init`, which resets the reverse cursor). The open window is
added at read time, so a walk cut short by a `LIMIT` still reports what it walked,
and the accumulator survives re-`Init` so an operator driven once per outer row
under an `Apply` reports its whole lifetime.

**Exactness was verified, not assumed.** A temporary per-slot probe was added
beside each `op.fwdStart++` / `op.revStart++` with a panic in `storageAccesses`
when the two figures disagreed; it ran green over `./cypher/` and `./cypher/exec/`
in full. The probe was then shown to be discriminating rather than vacuous by
deleting the `loadAdjacency` window close, which produced 36 disagreements in the
same suites. The probe is not kept: it is precisely the per-slot cost the design
avoids. `cypher/exec/expand_slotcount_test.go` keeps the property under an
independent degree-sum oracle.

**Cost:** interleaved A/B over `BenchmarkExpand*` (`cypher/exec`, n=8) and
`BenchmarkExpandInto*` (`cypher`, n=8), Apple M4, go1.27.1, no `-race`. `allocs/op`
and `B/op` are IDENTICAL in every benchmark ("all samples are equal"). `sec/op`
geomean moved -0.56% (exec) and -0.08% (engine), both toward faster and both inside
a noise floor measured in the same rounds from two independent builds of the same
source, which itself produced one "significant" -1.50% (p=0.038). No benchmark
regressed. Raw data: `docs/benchmarks/expand-slot-counter-2026-09-05-raw/`.

### What is DELIBERATELY outside the figure

Two access-path reads are not counted, and both are binary searches rather than
walks:

* `Expand.seekIntoRuns` (the expand-into seek) narrows the cursor to the bound
  destination's contiguous block. The slots it steps over are not read, so charging
  them would report the Θ(d) walk the seek exists to avoid — the counter would make
  an optimisation invisible. Pinned by
  `TestExpandStorageAccesses_SeekIsNotChargedForSlotsItSkipped`, whose control is
  the same query with the seek off.
* `Expand.reverseEdgePassesFilter`'s forward-position recovery probes the
  DESTINATION's forward run when the relationship-type column cannot answer a
  reverse slot directly. Since rmp #2251 the column answers directly whenever the
  pair's transpose was established, so this is the fallback and not the path.

### The definition, read in Neo4j source

§3 cited `DefaultNodeCursor.java:199-210` (`tracer.onHasLabel` before returning
`false`). The closer analogue for a traversal was read for #2761, at the same
pinned commit `679feff` (tag 5.26.16):
`community/record-storage-engine/.../RecordRelationshipTraversalCursor.java:159-161`
calls `tracer.onRelationship(entityReference())` INSIDE the loop
`do { ... } while (!inUse() || (!traversingDenseNode && !selection.test(getType(), ...)))`.
A relationship record read and then rejected by the type-and-direction selection is
therefore still charged. GoGraph's figure now matches that definition.

One structural difference remains and is not a counting difference: Neo4j groups a
DENSE node's relationships by type in the store, so a type-filtered traversal of a
dense node reads fewer records rather than counting fewer. GoGraph's CSR has no
per-type grouping, so it walks the run and counts what it walked.

### Found while doing this, NOT fixed: `exec.OptionalExpand` is unreachable

`exec.OptionalExpand` is built only for an `ir.OptionalExpand`, which
`cypher/ir/match.go:1960` emits only when `matchPattern` is called with
`optional=true` — and its sole caller, `cypher/ir/translator.go:375`, passes `false`
unconditionally. Every `OPTIONAL MATCH` plans an `OptionalApply` over a plain
`Expand` instead, verified by rendering
`OPTIONAL MATCH (r:Root)-[:KNOWS]->(b) RETURN b`. So no `PROFILE` output can
exercise the operator today. Its classification was corrected anyway — it is built,
censused, and would report the moment the translator wires it — and its arm of the
gate is an exec-level test rather than an engine-level `PROFILE`. Whether the
operator should be wired or removed is out of scope for #2761 and is left as a
finding.

### Still open after #2761

* The expression-closure operators are still classified by TYPE, not per instance.
* The parallel leaves (#2762) and `shortestPath` / `allShortestPaths` (#2763)
  remain UNCOUNTED.
* §7's limit stands: `ParallelAggregateScan` and `ParallelCountScan` are still
  classified from the interface set rather than from an observed run.

## Addendum — 2026-09-05, rmp #2762 (sprint 355)

### Refutation 3 is FIXED: the morsel-parallel leaves count their workers' node walk

§3's refutation 3 recorded the same query reporting `dbhits=2000` below the parallel
threshold and `dbhits=0` above it. rmp #2760 turned that `0` into `?`, which stopped it
being a false claim but left it an absent measurement. **It is now a measurement.**

`ParallelScanProject`, `ParallelAggregateScan` and `ParallelCountScan` all implement
`exec.storageAccessCounter` and report the node references their workers consumed. The
same 2000-node fixture, planned both ways by nothing but `ParallelScanThreshold`:

```
ParallelScanThreshold = 20000 :  NodeByLabelScan [B]  rows=2000  dbhits=2000
ParallelScanThreshold = 10    :  ParallelScanProject  rows=2000  dbhits=2000
```

### All three leaves were reached by a REAL query, not by an interface assertion

§7's standing limit — "`ParallelAggregateScan` and `ParallelCountScan` are still
classified from the interface set rather than from an observed run" — is discharged.
Each was planned and PROFILEd end to end, and each subject arm is paired with the
serial plan for the same graph:

| leaf | query | subject (threshold 10) | serial control (threshold 20000) |
|---|---|---|---|
| `ParallelScanProject` | `MATCH (n:B) WHERE n.v = 0 RETURN n.k + 1` | rows=20, **dbhits=2000** | `NodeByLabelScan` rows=2000, dbhits=2000 |
| `ParallelAggregateScan` | `MATCH (n) RETURN n.v AS g, count(*) AS c ORDER BY g` | rows=100, **dbhits=2000** | `AllNodesScan` rows=2000, dbhits=2000 |
| `ParallelCountScan` | `MATCH (n) RETURN count(*)` | rows=1, **dbhits=2000** | see below |

Every subject query was chosen so that **rows ≠ node walk**, which is what makes the
assertion a statement about a counter rather than one a `StorageRecordScan` row
derivation would also satisfy. `RETURN n.k + 1` rather than a bare `RETURN n.k` because
the columnar filter chain claims a plain property projection at every threshold and the
parallel tier would never be reached.

**`ParallelCountScan` has no scanning serial twin, and the gate says so rather than
papering over it.** Below the threshold the same `count(*)` plans an
`AllNodesCountScan`, which answers from the maintained live-node counter in O(1): it
walks nothing, counts nothing, and honestly renders `?`. Its control is therefore the
same whole-graph walk done serially by another query (`MATCH (n) RETURN n.k` →
`AllNodesScan`, dbhits=2000). The test asserts the sub-threshold `count(*)` plan really
is an uncounted `AllNodesCountScan`, so this explanation cannot go stale unnoticed.

### Where the count is taken, and why it is not a per-record atomic

A worker charges a whole MORSEL in one atomic add, immediately after that morsel's
sub-plan `Init` has read it — `AllNodesScan.Init` collects every id it will emit in one
pass, so the figure is known there in O(1) and the row loop needs no increment. With
`DefaultMorselSize` at 1024 that is **one atomic add per 1024 node references**, on a
counter no worker reads back.

This is a correctness requirement of CLAUDE.md mandate 3, not tuning. The sibling
counter on this very operator measured the alternative: before rmp #2649 every produced
row bumped two process-shared atomics, which cost **18.9% of flat CPU** and stopped the
operator scaling past four workers.

Charging at morsel scan-`Init` rather than after the drain also keeps the figure exact
when `ParallelScanProject`'s row loop exits early on the result budget: the records were
read either way.

**A node reference is counted ONCE**, although two phases touch it — the leaf's `Init`
walks the access path on the calling goroutine, and the worker's morsel scan re-reads
the slice `Init` already owns. Charging both would report 2N where the serial plan
reports N for identical work, which is the same threshold-dependent figure this task
removed, wearing a different sign.

### Read in the reference engines: PostgreSQL divides, Neo4j sums

**PostgreSQL** (REL_17_STABLE, commit `018bfcfd9fa4e520970ba3bda370f78bb473c365`) keeps
a per-worker `Instrumentation` array under ANALYZE — each worker calls `InstrEndLoop`
then `InstrAggNode` into its own slot (`execParallel.c:1275`, `:1297`), the leader folds
them into the node's figure (`:1039-1043`) and keeps the per-worker copies (`:1057-1058`)
— and under ANALYZE **plus VERBOSE** emits a separate `Worker N:` sub-entry per worker
(`explain.c:1893-1941`, `:4544-4551`, flushed at `:4595-4614`). Its headline figure is
then a **per-worker average**: `explain.c:1841-1847` computes
`rows = instrument->ntuples / nloops`, and `instrument.c:184-186` has summed both
`ntuples` and `nloops` across participants. `src/test/regress/expected/select_parallel.out:589-620`
shows the consequence — a `Parallel Seq Scan` printing `rows=2000 loops=15` scanned
30 000, where 15 = 3 rescans × (4 workers + leader).

**Neo4j 5.26.16** (commit `679feffbfb7a9189aba360ea98eef7fc3371e275`) sums instead.
`ProfilingTracerData.java:33-41` accumulates `dbHits +=` per operator id,
`PlanDescriptionBuilder.scala:123-141` attaches exactly one `DbHits` argument per plan
node, and `ProfileDbHitsTestBase.scala:172-178` asserts that a parallel all-nodes scan
reports one total equal to the node count. (That clone is community-only, so the
enterprise parallel runtime's own accumulator was not read; the conclusion rests on the
`QueryProfile.operatorProfile(int)` contract, the plan-description builder, and the
runtime spec-suite assertion, all of which are in it.)

**GoGraph follows Neo4j, and neither half of the PostgreSQL design fits.** The
per-worker sub-entries would have nothing to report: cypher's `buildOpts.forWorker`
clears the profiler from the per-worker build options, so no worker measures anything,
and reinstating one would reintroduce the shared-wrapper data race rmp #2664 removed. An
averaged headline would defeat the purpose of the figure — it exists so a reader can
compare the parallel plan against the serial one, and an average is not comparable with
anything. This is stated in `exec.Profiler`'s "parallel tier" section beside the
citations, so the reason survives the next reader.

### Cost: no significant change in any parallel arm, at any concurrency level

Interleaved A/B/C, three arms rotated **within** each round: `base` (worktree at
`e6f6384b`), `base2` (a separately built copy of the same source — the **noise floor**),
and `head`. Apple M4 (10 cores), macOS 26.5.2, go1.27.1, **no `-race`**. Driver:
`rmp2762-parallel-dbhits-ab.sh`; `loadavg` was recorded before all 39 invocations, all of
which exited 0. Raw data, both `benchstat` comparisons per set, the load log and the
driver: `docs/benchmarks/parallel-leaf-dbhits-2026-09-05-raw/`.

*Set A* — `-benchmem -count=1` per round × 5 rounds, `-cpu=1,4,10`, over
`BenchmarkParallelScan_CountBig`, `BenchmarkParallelScanProject_Scan{Big,FilterBig}`,
`BenchmarkParallelAggregate_{MinBig,GroupMinBig}` and
`BenchmarkParallelLabelScan_LabelledProject` (36 benchmarks per round, parallel and
serial arms of each):

* **No parallel arm moved significantly.** Geomean `sec/op` −0.07%, against a noise-floor
  geomean of −0.12%.
* The only three "significant" `sec/op` verdicts are on **serial control** arms
  (`DisableParallelScan`), which this change cannot reach: −1.90%, −1.41%, −1.16%. The
  noise floor produced false positives of the same size on the same kind of arm
  (+1.76%, −0.68%, −1.98%), so ±2% is the floor here.
* `B/op` and `allocs/op`: every delta is ±0.00%; geomeans −0.01% and −0.04%.

*Set B* — the concurrency evidence, `BenchmarkParallelAggregate_Concurrent` at 1, 8 and
64 concurrent queries on ONE shared `exec.ParallelGovernor`, 8 rounds:

| conc | parallel arm, base → head | verdict |
|---|---|---|
| 1 | 17.24m → 17.21m | ~ (p=0.574, n=8) |
| 8 | 136.8m → 136.5m | ~ (p=0.721, n=8) |
| 64 | 860.7m → 861.0m | ~ (p=0.721, n=8) |

The single significant verdict in set B is again a serial control (conc=1, +1.46%,
p=0.007), and the effect geomean (+0.33%) is *inside* the noise floor's own geomean
(+0.47%). `B/op` and `allocs/op` are unchanged at every level.

**Honest qualification.** The host was not idle in the strict sense: the sweep itself
drove the 1-minute load average to 9.6–10.1 on a 10-core machine, and the driver was
niced (NI 5) by the harness. Both conditions applied identically to all three arms,
which were rotated within every round, and the noise floor was measured under exactly
the same conditions — which is what makes the comparison valid rather than the absolute
numbers portable.

### One stated limit: a parent that never pulls a row

`cypher.profileMaterialised` captures the plan tree after the drain and before any
`Close`. A parallel leaf is joined by its own first `Next` (`wg.Wait`), so the figure is
complete for every plan whose parent pulls at least one row — including a `LIMIT 1`,
verified at `dbhits=20000` on a 20 000-node graph. It is **not** complete when the parent
pulls none: `RETURN … LIMIT 0` builds the leaf, `Init` launches its workers, `Limit`
returns false without ever calling the leaf's `Next`, and the capture happens with the
workers still in flight. MEASURED over 40 consecutive PROFILEs: `dbhits=0` every time,
alongside `rows=0` and `time=0s`. Zero is what was observed, not what is guaranteed. This
is a property of WHEN the tree is captured — shared with the rows and time columns — and
not of the counter. It is recorded in `exec.Profiler` rather than left for a reader of an
unexpected figure to discover.

### Corrections this addendum makes to earlier text in this document

* §3 refutation 3's `dbhits=0` and the `[parallel tier; db-hits not counted]` plan detail
  quoted in §6 both describe behaviour that no longer exists. The detail string is now
  `parallel tier; whole phase on one node` — the part that is still true, since the leaf
  really does attribute a whole fused phase to one line with no children to subtract.
* §7's limit "`ParallelAggregateScan` and `ParallelCountScan` are still classified from
  the interface set rather than from an observed run" is discharged, for those two and
  for `ParallelScanProject`.

### Still open after #2762

* `shortestPath` / `allShortestPaths` remain UNCOUNTED (#2763).
* The count-store leaves (`AllNodesCountScan`, `LabelCountScan`) remain UNCOUNTED: each
  answers from an O(1) maintained counter when it can and materialises otherwise, and the
  two paths would need different figures.
* `ExpandIntersect` and `IndexNestedLoopJoin` remain UNCOUNTED.
* The expression-closure operators are still classified by TYPE, not per instance.
* The rendered plan still does not distinguish a MEASURED figure from a DERIVED one
  (rmp #2720's standing limitation of the output).

## Addendum — 2026-09-05, rmp #2763 (sprint 355)

### §6's last "left unfixed, deliberately" entry is FIXED

§6 recorded `ShortestPath` / `AllShortestPaths` reporting `0` (later `?`) as a
deliberate gap, on the reasoning that their only counter — `totalEdgesTraversed` —
covers the exhaustive path-predicate search alone, so wiring it would print an
authoritative-looking `0` for the common path. **Both now report a measurement.**

Both implement `exec.storageAccessCounter` and report the adjacency slots every one
of their searches read. The figure is exact and it is not derivable from anything
else the plan prints: on a 100-way fan whose first mid continues to the
destination, `shortestPath` reports **101 db-hits for one row**.

| query (fan = 100, one mid continues to B) | rows | db-hits before | db-hits now |
|---|---|---|---|
| `shortestPath((a)-[*]->(b))` | 1 | `?` | 101 |
| `shortestPath((a)-[*]->(b))`, fan = 200 | 1 | `?` | 201 |
| `allShortestPaths((a)-[*]->(b))` | 1 | `?` | 101 |
| `allShortestPaths((a)-[*]->(b))`, every mid → B | 100 | `?` | 200 |

The last two rows are the control that refutes a row-derived figure: between them
the rows go 1 → 100 while the walk goes 101 → 200. No scaling of the row count fits
both.

### Where the count is taken, and why it is not per-record

Nine scanning sites, all of the same shape: a bounds guard followed by a complete
walk of one node's adjacency run. **None of them leaves a run early** — every branch
inside is a `continue` — so the number of slots the loop touches is exactly
`verts[node+1] - verts[node]`, and one add per RUN is an exact count rather than an
approximation of one. `ShortestPath.scanRun` computes the loop's own bound and banks
its length in the same expression, so the two cannot drift; it is inlined at all
nine call sites (verified with `-gcflags=-m`).

| operator | sites |
|---|---|
| `ShortestPath` | `biScan` (the two-sided BFS — the common path), `spExpand` (the forward-only fallback), `bfsShortestCycleForward`'s scan, `branchArcs` (DirBoth cycle), `exhArcs` (exhaustive) |
| `AllShortestPaths` | `aspExpand` (level-synchronous BFS), `bfsAllShortestCycle`'s scan, `branchArcs`, `exhArcs` |

`AllShortestPaths` is **not** two-sided: `shortest_path_bidir.go` states that the
two-sided search is deliberately not applied to it, because reconstructing the
multi-predecessor DAG across a meeting point is materially harder. The addendum
records that because the task, and §6 before it, described both operators as
"bidirectional BFS" — only one of them is.

### That the batch charge equals a per-record one was MEASURED, not argued

A temporary per-slot probe was added to all nine loops, incrementing a second
counter on every iteration and panicking on any disagreement with the per-run
charge. Under it, `./cypher/exec/`, `./cypher/`, `./cypher/tck/` and `./bolt/...`
all passed (exit 0) with **zero disagreements**, over 96 operator-lifetime checks at
`Close` (84 of them on a non-zero walk, largest lifetime 620 slots) and up to 370
further checks per process at every `Next`.

The probe was then falsified rather than trusted: halving the charge in
`ShortestPath.scanRun` produced
`panic: rmp2763 PROBE DISAGREEMENT (Next) in ShortestPath: per-run charge=0, per-slot probe=1`,
and halving it in `AllShortestPaths.scanRun` produced
`… in AllShortestPaths: per-run charge=1, per-slot probe=4`. The probe is not
committed: a permanent second counter would cost every ordinary query exactly what
this design refuses.

### `totalEdgesTraversed` is deliberately NOT folded into the figure

The task asked for the existing counter to be folded into one lifetime total. **That
is refuted by the code and was not done**, and the reason is the same one this sprint
exists for. `totalEdgesTraversed` counts the arcs `exhArcs` RETURNED — after the type
filter and after handle de-duplication — over the very runs the new counter charges
the SLOTS of. Adding the two would count one walk twice under two incompatible
definitions and report up to 2x the storage work on the only path where both are
live. The exhaustive search instead contributes through its slot charge like every
other search, so there is one lifetime total with one definition throughout, and the
budget stays a budget.

### The definition: slots READ, not arcs ADMITTED

A slot the relationship-type filter rejects is charged, because it was read before it
could be judged. That is `Expand.storageAccesses`'s definition since #2761 and
Neo4j 5.26.16's (`RecordRelationshipTraversalCursor.next()` calls
`tracer.onRelationship()` inside the `do { … } while (!inUse() || !selection.test(…))`
loop). On a graph where A has 100 `:E` and 100 `:O` out-edges, the typed and untyped
patterns both report **201**; a figure counting admitted arcs would report 101 for the
typed one.

### What is deliberately outside the figure

* `ShortestPath.scanFwdPos` and both operators' `hopForTraversal` — reconstruction-time
  recovery of ONE hop's forward position, running over the found path's ≤ d hops and
  never over the search.
* `buildRevToFwd`, called from `Init`. It walks both CSRs whole to build a position
  table: index construction, not a search read. Charging it would make the figure a
  function of the graph's size rather than of the work the search did.

Both are of the same kind as the property reads no operator's db-hits count.

### Cost: the counter is free; an incidental allocation IMPROVEMENT came with it

Seven purpose-built benchmarks (`cypher/exec/shortest_path_bench_test.go`), chosen to
bracket rather than flatter the design: a unit-degree chain where the per-run charge
degenerates to a per-slot charge (the worst case), a layered BFS at out-degree 8, and
a single 20 000-slot run (the best case), for both operators. Twelve interleaved
A/B rounds each, `-benchtime=300ms`, no `-race`.

**The host was not idle** — loadavg 2.4–2.9 throughout, recorded before and after every
invocation in `loadavg_*.log` — so **no timing claim is made**. The noise floor confirms
why: two builds of *identical* source produced a significant verdict of its own,
`AllShortestPaths_UnitDegreeChain −1.57% (p=0.045, n=12)`.

The verdict therefore rests on allocation counts, which are exact and load-invariant.
Isolating the counter alone (the same refactor with and without the `slotsRead +=`
line):

```
                                       allocs/op OFF   allocs/op ON    vs
ShortestPath_UnitDegreeChain-10          11.90k          11.90k        ~ (p=1.000 n=12) all equal
ShortestPath_Layered-10                   108.0           108.0        ~ (p=1.000 n=12) all equal
ShortestPath_LayeredTyped-10              108.0           108.0        ~ (p=1.000 n=12) all equal
ShortestPath_HighDegreeFan-10             314.0           314.0        ~ (p=1.000 n=12) all equal
AllShortestPaths_UnitDegreeChain-10      5.670k          5.670k        ~ (p=1.000 n=12) all equal
AllShortestPaths_Layered-10              2.938k          2.938k        ~ (p=1.000 n=12) all equal
AllShortestPaths_HighDegreeFan-10        20.32k          20.32k        ~ (p=1.000 n=12) all equal
```

B/op identical too, and every sec/op delta non-significant (geomean +0.07%, inside a
noise floor that produced a significant result on identical source).

Comparing HEAD against the pre-change baseline instead shows allocations **falling**:
`ShortestPath_Layered` and `_LayeredTyped` 126 → 108 (−14.29%, p=0.000, n=12) and
`_HighDegreeFan` 317 → 314 (−0.95%). That is **not** the counter, and the attribution
was established rather than assumed, by three further interleaved arms:

| arm | change from baseline | `ShortestPath_Layered` allocs/op |
|---|---|---|
| baseline | — | 126 |
| hoist only | `end := verts[node+1]` lifted out of the loop condition, early return KEPT | 126 (no change) |
| `biScan` loop form only | single exit, bounds precomputed, no early `return next` | **108** |
| whole refactor, counter removed | all nine sites rewritten | **108** |
| HEAD (counter on) | + the `slotsRead +=` charge | **108** |

So the whole delta comes from `biScan`'s loop form — removing its early `return next`
in favour of a single exit over a precomputed range — and the counter adds nothing on
top. The compiler-level mechanism behind those 18 allocations was **not identified**:
escape analysis and inlining decisions are identical between the arms
(`-gcflags='-m -m'` diff shows only line-number shifts), and a memory profile puts
the difference in `biScan`'s own `pred`/`dist` inserts and `next` append. It is an
improvement, it is reproducible at ±0% across twelve rounds, and it is reported here
rather than claimed as a designed gain.

Raw data, exit codes, loadavg logs and the A/B script:
`docs/benchmarks/shortest-path-dbhits-2026-09-05-raw/`.

### The gates, and the mutations that prove they are exact

Six committed assertions, in `cypher/exec/shortest_path_slotcount_test.go` (definition,
lifetime, fallback parity) and `cypher/profile_dbhits_honesty_test.go` (four
end-to-end `PROFILE` gates). Every one was mutation-verified with a **partial
under-count**, never a zeroing one, so an assertion that only checked for "non-zero"
could not have passed:

| mutation | caught by | message |
|---|---|---|
| halve `ShortestPath.scanRun`'s charge | 6 assertions | `fan=100: storageAccesses() = 50, want 101` |
| halve `AllShortestPaths.scanRun`'s charge | 3 assertions | `storageAccesses() = 4, want 10 (node 0's 9-slot run plus mid 1's one slot)` |
| reset `slotsRead` in both `Init`s | 3 assertions | `k=2: dbhits=11, want 22 … A counter reset in Init reports 11 for every k` |
| refund a type-filtered slot in `biScan` | 2 assertions | `untyped=201 typed=101, want 201 for both` |
| refund a type-filtered slot in `spExpand` | **initially NONE** | — |
| refund a type-filtered slot in `aspExpand` | **initially NONE** | — |

The last two escaped: the typed gates all ran through the two-sided search, so the
forward-only fallback's type-filter branch and `AllShortestPaths`' own were unasserted.
Two arms were added — a typed forward-only fallback, and a typed `allShortestPaths` —
and both mutations are now caught
(`typed(forward-only)=2, want 8`; `untyped=8 typed=2, want 8 for both`). The holes are
recorded here because a mutation that escapes is the only evidence that a gate was
missing.

### Found while doing this, NOT fixed: `AllShortestPaths.Init` rebuilds `revToFwd` per outer row

`ShortestPath.Init` guards the `buildRevToFwd` call with `revPrepared`, added by #2220
after the same rebuild was measured as turning a large win into a 75% regression on the
#2236 benchmark. `AllShortestPaths.Init` has **no such guard**: for `DirIn`/`DirBoth` it
rebuilds the O(E) position table on every `Init`, and `Init` runs once per outer row
under a `CorrelatedApply`. Out of scope for #2763 and reported rather than fixed.

### Still open after #2763

* The count-store leaves (`AllNodesCountScan`, `LabelCountScan`) remain UNCOUNTED.
* `ExpandIntersect` and `IndexNestedLoopJoin` remain UNCOUNTED.
* The expression-closure operators are still classified by TYPE, not per instance.
* The rendered plan still does not distinguish a MEASURED figure from a DERIVED one
  (rmp #2720's standing limitation of the output).

## Addendum — 2026-09-05, rmp #2764 (sprint 355)

### D2 is FIXED: `PROFILE` now says how many rows a filter threw away

§5 recorded divergence **D2** — no `Rows Removed by Filter` equivalent — and called
it *"the sharpest missing figure"*. It is now reported, as `removed=`, by three
operator families: `Filter`, `ColumnarFilter`, and `Expand` / `OptionalExpand` /
the columnar expand.

The gap it closes is not a missing number but a missing *distinction*. Over 1000
`:P` nodes of which exactly 3 carry `age = 7`:

```
Project (rows=3, dbhits=?, time=242µs)
└─ Filter (rows=3, dbhits=?, removed=997, time=239µs)
   └─ NodeByLabelScan [P] (rows=1000, dbhits=1000, time=22µs)
```

and the same query, same answer, once an index on `:P(age)` exists:

```
Project (rows=3, dbhits=?, time=2µs)
└─ Filter (rows=3, dbhits=?, removed=0, time=1µs)
   └─ NodeByIndexRangeScan [range=7..7] (rows=3, dbhits=3, time=0s)
```

Both plans return three rows. Before #2764 the two differed only in an operator
name; now the first says plainly that 997 rows were read to answer with 3, and the
second says the access path removed nothing and the residual `Filter` above it
rejected nothing either. The access path itself carries **no** `removed=` cell in
either plan — a scan and a seek remove no rows, so they have no figure, and the
cell is omitted rather than printed as `0`.

### What PostgreSQL actually does, read at REL_17_STABLE

Read at commit `018bfcfd9fa4e520970ba3bda370f78bb473c365`, not recalled:

| property | PostgreSQL | file:line |
|---|---|---|
| the counter | one `double nfiltered1` on `Instrumentation`, "# of tuples removed by scanqual or joinqual" | `src/include/executor/instrument.h:89` |
| how it is bumped | `InstrCountFiltered1(node, 1)` — **one per rejected tuple**, in the `else` arm of the qual test the executor already branches on | `src/include/nodes/execnodes.h:1223-1227`; `src/backend/executor/execScan.c:255` |
| how many increment sites | 9 across the executor | `execScan.c`, `nodeNestloop.c`, `nodeAgg.c`, `nodeWindowAgg.c`, `nodeMergejoin.c`, `nodeGroup.c` (×2), `nodeHashjoin.c`, `nodeModifyTable.c` |
| how many print sites | **19** for `"Rows Removed by Filter"` (plus 3 `by Index Recheck`, 3 `by Join Filter`, 1 `by Conflict Filter`) | `src/backend/commands/explain.c` |
| when it prints — the plan | every print site is wrapped in `if (plan->qual)`: a node with no filter expression prints **nothing** | e.g. `explain.c:2176-2178` |
| when it prints — the run | `show_instrumentation_count` returns immediately unless `es->analyze && planstate->instrument` | `explain.c:3622, 3628-3629` |
| whether it prints a zero | **no, in text mode** — `/* In text mode, suppress zero counts; they're not interesting enough */` | `explain.c:3638-3639` |
| what value it prints | `nfiltered / nloops` — a per-loop **average**, as a `double` | `explain.c:3641` |

**Followed here:** the concept and the name (`RowsRemovedByFilter` on the Bolt
wire), the increment placed on the branch the operator already takes, and the rule
that an operator which cannot remove rows prints nothing rather than a zero. That
last is also the house rule #2760 established for db-hits, reached by PostgreSQL
through its `if (plan->qual)` guard rather than through a flag.

**Diverged, deliberately, on three points:**

1. **A measured zero IS printed.** PostgreSQL suppresses it in text mode; GoGraph
   prints `removed=0` for an operator that *can* remove rows and removed none.
   "This operator has no filter" and "this filter rejected nothing, so it bought
   you nothing" are different facts, and the second is the finding a reader of the
   index plan above actually wants. Suppressing it would collapse the distinction
   the figure exists to draw.
2. **The figure is a lifetime total, never divided.** GoGraph prints no `loops`
   column and its inner-side operators already report lifetime totals across
   re-`Init` (§5, D5). Dividing this one figure by an invocation count the rest of
   the output does not show would make it incomparable with the `rows` beside it.
3. **There is no plan-wide total.** Db-hits has one because *every* operator is
   classified, so the sum is either complete or explicitly `x + ?`. This figure is
   reported by three families only, and other operators discard rows for reasons it
   would misdescribe. The `Removed` column's `Total` cell is therefore blank.

### `Expand`'s figure covers four fates, and two of them are not "filters"

`Expand`'s `removed=` counts every fate a consumed adjacency slot can meet other
than emission: the relationship-**type** filter (forward and reverse),
**cyphermorphism**, the undirected **self-loop deduplication**, and the
**expand-into destination** comparison. The last two are a de-duplication and a
join condition, which PostgreSQL would not file under `Rows Removed by Filter` —
it splits them across three labels because it prints the qual beside each figure.
GoGraph prints no expression next to an `Expand`, so splitting one operator's
discard count into four unlabelled numbers would answer a question nobody asked.
The divergence is recorded on `Expand.rowsRemovedByFilter` rather than left to be
inferred.

### The figure is EXACT, and that was proved rather than argued

Every counted operator satisfies an identity against a figure counted
**independently**, which is what makes the count falsifiable:

| operator | identity | the other side is counted by |
|---|---|---|
| `Filter` | `rows + removed == rows pulled from the child` | the child's own profiling wrapper |
| `ColumnarFilter` | `appended + removed == source rows examined` | the scratch cursor |
| `Expand` | `rows + removed == dbhits` | the cursor positions (`storageAccesses`, rmp #2761) |

The `Expand` identity is the strongest, because its two sides are derived from
unrelated state: `storageAccesses` reads the cursor positions, `removed` counts
reject branches. A temporary probe asserting
`storageAccesses() == rowsRemovedByFilter() + admitted` on every `Next` and
`FillChunk` return was installed and run over `./cypher/exec/`, `./cypher/`, and
the **full 3897-scenario TCK**: zero disagreements. The probe was then shown not to
be vacuous — deleting one increment produced
`PROBE-2764 Next: slotsRead=2 != rejected=0 + admitted=1` — and removed, with a
repository-wide residue scan confirming nothing was left behind.

### Where the charge is taken: a placement decided by measurement

Every rejection leaves `advanceFwdEdge` / `advanceRevEdge` as the single status
`edgeSkip`, and those two functions have exactly **four** callers. The first
implementation charged inside them, at the five reject branches. It was moved out,
for a measured reason:

`advanceFwdEdge` and `advanceRevEdge` are the innermost code of every expansion,
and *any* statement added to them costs a few percent on a single-hop unfiltered
walk **even when the statement never executes**. Measured: a **dead** counter of
exactly the same shape — an unread `int64` field plus seven increments of it in the
same branches — added to the *unchanged* operator cost **+3.11% (p=0.003, n=8)** on
`BenchmarkExpandDir_InVsOut_Baseline/OUT_deg1_sources`, a benchmark with neither a
type filter nor a morphism, in which no rejection branch is ever taken.

| arm | `OUT_deg1_sources` | `IN_deg1_sources` |
|---|---|---|
| charge inside `advance*Edge` | +4.90% (p=0.000) | +5.35% (p=0.000) |
| **dead counter, same shape, on HEAD** | **+3.11% (p=0.003)** | ~ (p=0.959) |
| charge in the four callers (shipped) | +1.26% (p=0.050) | ~ (p=0.798) |

The shipped placement leaves both advance functions byte-identical to their
pre-#2764 source, and the residual +1.26% sits inside the envelope the dead counter
defines. Inlining decisions are unchanged (`-gcflags=-m`, diffed with line numbers
stripped: the only difference is that the two new accessor methods are inlinable),
and `unsafe.Sizeof` grew by 8 bytes on each of `Expand` (552→560), `Filter` (40→48)
and `ColumnarFilter` (120→128) — no size-class crossing, which the identical
`B/op` confirms.

### Cost: no allocation change anywhere; no significant timing change

`benchstat`, 8 interleaved A/B rounds, `-benchmem -count=1` per round, **no
`-race`**, go1.27.1 darwin/arm64, Apple M4 (10 cores).

`./cypher/exec/`, 11 filter and expand benchmarks: **geomean sec/op −0.01%**, 10 of
11 `~`; the exception is `OUT_deg1_sources` above. **`allocs/op` and `B/op`
identical on every benchmark** ("all samples are equal" on all 11 allocation rows).

`./cypher/`, the 5 columnar-shape filter benchmarks: geomean +0.76%, allocations
identical. That figure is **not attributable to the counting**: the same tree with
every increment deleted measures **+0.61%** against the same baseline, so the
diffuse ~1% is the code the change carries, not the work it does.

**The host was not idle.** Load average ran 1.5–3.2 throughout, with `osascript`,
`iTerm2` and `system_profiler` competing. A same-vs-same noise floor was measured
first and was tight (geomean −0.06%, every benchmark `~`, allocations identical),
which is why the timing figures are reported at all; the allocation figures are
load-invariant and stand unconditionally.

### The mutation that found a real hole

Eleven mutations were run against the new gates. Ten were killed immediately. The
eleventh was not, and it is the valuable one: **deleting all four of `Expand`'s
reverse-cursor increments left `go test ./cypher/ ./cypher/exec/` fully green.**
Half of `Expand`'s rejection accounting was unproved.

Four arms were added to close it, three at engine level and one at exec level:

| branch | query / fixture | rows | db-hits | removed |
|---|---|---|---|---|
| reverse type filter | `MATCH (h:Hub)<-[:KNOWS]-(x)`, 99 `:LIKES` + 1 `:KNOWS` in-edge | 1 | 100 | 99 |
| undirected self-loop dedup | `MATCH (s:Solo)--(x)`, one self-loop + one out-edge | 2 | 3 | 1 |
| reverse cyphermorphism | `MATCH (a:M1)-[r1:E]->(b)--(c)`, one edge | 0 | 1 | 1 |
| expand-into comparison | `newSeekExpand`, seek OFF, bound dst | 1 | 4 | 3 |

The expand-into arm also **checks a claim the code makes about itself**: with the
seek enabled — the default whenever a destination is bound — the cursor is narrowed
to the destination's run before the walk, so the comparison rejects nothing (`4
slots / 3 removed` becomes `1 slot / 0 removed`). That was documented as a property
and is now measured as one.

A second mutation found a weakness in a *test* rather than in the code: an
even/odd predicate rejects on strict alternation, so a counter that counted every
other rejection passed. The columnar exec gates now filter on multiples of 7, which
rejects in runs of six.

### Then a PER-SITE deletion sweep found a second hole, and refuted the method above

The mutations above were mostly **return-value** mutations — `removed / 2`,
`removed - 1`. That method is weaker than it looks, and the weakness is exact: a
return-value mutation **cannot distinguish an increment site that is reached from
one that is not**, because it perturbs the total whatever produced it. Deleting the
increment *at its site* can.

So every one of the eight sites was deleted individually and
`go test -count=1 ./cypher/ ./cypher/exec/ ./cypher/explain/ ./bolt/server/` re-run.
**Two survived — not the one already known.**

| site | where | before | after |
|---|---|---|---|
| S1 | row-mode FORWARD `edgeSkip` (`tryFwdEdge`) | killed | killed |
| S2 | row-mode FORWARD expand-into (`tryFwdEdge`) | killed | killed |
| S3 | row-mode REVERSE `edgeSkip` (`tryRevEdge`) | killed | killed |
| S4 | row-mode REVERSE expand-into (`tryRevEdge`) | killed | killed |
| **S5** | **COLUMNAR FORWARD `edgeSkip` (`fillOneChunkRow`)** | **SURVIVED** | killed |
| **S6** | **COLUMNAR REVERSE `edgeSkip` (`fillOneChunkRow`)** | **SURVIVED** | killed |
| S7 | `Filter.Next` | killed | killed |
| S8 | `ColumnarFilter.FillChunk` | killed | killed |

The reason is structural and worth stating, because it is the same reason the
reverse-cursor sites went unproved before them. `Expand`'s two paths share their
edge **decisions** (`advanceFwdEdge` / `advanceRevEdge`) but not their
**accounting**: `fillOneChunkRow` charges the rejection itself, at its own two
`continue` arms. Every gate that drove the row path therefore left the columnar
charge untouched. `TestProfileDbHits_ColumnarExpandCountsSlotsWalked` (#2761) does
drive a type-filtered expand through `FillChunk` — and asserts `storageAccesses`,
which says nothing about rejections.

Three gates closed it, subject/control in both directions:

* `TestColumnarExpandRowsRemoved_CountsWhatFillChunkDiscarded` (exec) — a
  `columnarExpand` driven through `FillChunk` and never `Next`, across four
  per-call caps, with `DbHits == Rows + RowsRemovedByFilter` asserted on both arms.
* `TestColumnarExpandRowsRemoved_AgreesWithTheRowPath` (exec) — the same fixture
  and config driven both ways. Two charge sites for one decision is exactly the
  shape that lets them drift, and nothing else compared them. It kills S1, S3, S5
  and S6.
* `TestProfileRowsRemoved_ColumnarExpandReportsThroughAPlannedQuery` (engine) —
  four real queries, proving the **planner** reaches both columnar charge sites.

Post-fix sweep, with the exact message each deletion now produces:

| site | first failure |
|---|---|
| S1 | `` type-filtered `-[:KNOWS]->`: Expand reported removed=0, want 99 `` |
| S2 | `rowsRemovedByFilter()=0, want 3. The expand-into comparison is the only branch that can reject here` |
| S3 | `reverse type filter: Expand reported removed=0, want 99` |
| S4 | `rowsRemovedByFilter()=0, want 1` |
| S5 | `forward subject: columnarExpand reported removed=0, want 99` |
| S6 | `reverse subject: columnarExpand reported removed=0, want 99` |
| S7 | `the Filter reported removed=0, want 997. It pulled 1000 rows from the scan and emitted 3` |
| S8 | `the ColumnarFilter reported removed=0, want 997` |

`Filter` and `ColumnarFilter` (S7, S8) were killed by site deletion as well as by
the return-value mutations, so the weaker method was not hiding anything there —
but only the sweep proves that, which is the point.

**The general lesson, recorded because it outlives this task:** a counter with N
increment sites needs N deletions, not one mutation of the value it returns. Where
two execution paths share a decision but charge it separately — as GoGraph's row
and columnar operators do throughout — the number of sites is larger than the
number of *concepts*, and the extra sites are exactly the ones no existing test
reaches.

### A census gate was built, and here is why it was warranted

`cypher/exec/rows_removed_classification_test.go` classifies every operator in the
package as REPORTS or SILENT, with a mandatory reason, derived from the package AST
exactly as `dbhits_classification_test.go` is.

Two states looks too small to need a census. It is warranted because the
**interesting half is the silent one, and the silence is a judgement**. `Limit` and
`Skip` drop rows on a count; `Distinct` and the aggregations collapse them;
`SemiApply` and `AntiSemiApply` drop an outer row on an inner plan's emptiness;
`HashJoin`, `ExpandIntersect` and `IndexNestedLoopJoin` discard candidates. Each
could plausibly have been folded in, and each was excluded for a different reason.
Without the census an exclusion and an oversight look identical — both are an
operator with no marker method. The census entries marked `GAP:` are the ones that
reject by a predicate and are **not yet counted**, listed as gaps rather than
implied:

* `HashJoin` / `ColumnarHashJoin` — PostgreSQL reports these under a *different*
  label (`Rows Removed by Join Filter`), which is the shape a follow-up should take.
* `SemiApply` / `AntiSemiApply` — an `EXISTS` filter by another name; counting it
  needs a decision about whether the figure belongs on the driver or the inner plan.
* `ExpandIntersect`, `IndexNestedLoopJoin` — uncounted, as their db-hits are.
* `ShortestPath` / `AllShortestPaths` / `VarLengthExpand` — each rejects arcs while
  walking; each counts the slots it *read* (#2763, #2761) but not the ones it rejected.
* the three morsel-parallel leaves — their per-morsel sub-plan filters, but the
  sub-plan is not instrumented; a per-morsel fold like #2762's would be needed.

### Found while doing this, NOT fixed: a residual `Filter` above an exact index range

The index plan quoted at the top of this addendum shows
`NodeByIndexRangeScan [range=7..7]` with a `Filter (removed=0)` above it. The range
is a point range that answers `age = 7` exactly, so the `Filter` re-checks a
predicate the access path has already fully applied and rejects nothing. It is a
planner question (#2765–#2767), out of scope here, and reported rather than fixed —
but it is worth recording that **this figure is what made it visible**: before
#2764 the redundant `Filter` was indistinguishable from a selective one.

### Still open after #2764

* The `GAP:` entries in the census above — nine operators that reject by a
  predicate and do not yet report it.
* The count-store leaves (`AllNodesCountScan`, `LabelCountScan`) remain UNCOUNTED
  for db-hits.
* The expression-closure operators are still classified by TYPE, not per instance.
* The rendered plan still does not distinguish a MEASURED db-hits figure from a
  DERIVED one (rmp #2720's standing limitation of the output).

---

## Addendum — 2026-09-05, rmp #2765 (sprint 355)

This section records what changed after the audit and what the work refuted about
its own brief. Nothing earlier in the document has been edited.

### Divergence D3 — "the largest gap" — is CLOSED

§5 recorded D3 as **estimated and actual never side by side**, and called it the
largest gap in the plan surfaces: `Engine.ExplainTable` rendered the LOGICAL plan
with `Est.Rows` and executed nothing, `Engine.ProfileTable` rendered the PHYSICAL
plan with the measured `Rows`, and "their rows do not correspond one to one, so
they cannot simply be placed side by side". A plan chosen on a bad guess was
therefore indistinguishable from one chosen on a good guess.

The planner's estimate now travels onto the PHYSICAL plan node and is rendered
beside the measurement on every physical surface:

* `exec.PlanNode` gained `Est exec.PlanEstimate` — a row count plus its provenance
  (`EstimateExact` / `EstimateStats` / `EstimateHeuristic`, and `EstimateAbsent` as
  the zero value).
* `Engine.ProfileTable` gained an **`Est.Rows` column immediately left of `Rows`**,
  using the cell conventions `ExplainTable` already established: `N` exact, `~N`
  approximate, `-` no estimate. Like the `Removed` column of #2764 it appears only
  when some operator carries the figure, and its `Total` cell is always blank.
* `Engine.Profile`'s indented rendering leads each operator's parenthesis with
  `est. rows=N provenance`, before the measured `rows=`. The `est.` qualifier is
  what stops it being read as a measurement, and it is omitted entirely — never
  zeroed — for an operator with no estimate.
* `Engine.Explain` and the `EXPLAIN` prefix carry it too, which follows
  necessarily: both render the same captured `exec.PlanNode` through the same
  `exec.RenderPlanNode`, and `TestPlanPrefix_ExplainMatchesEngineExplain` requires
  them to agree byte for byte. `Engine.Explain` previously printed no numbers at
  all.

### Divergence D9 is CLOSED

§5's D9 recorded that the Bolt `plan` metadata **carries no estimates**, so a
driver's `ResultSummary.Plan()` received operator names and no numbers. Each plan
node's `args` map now carries `EstimatedRows` — Neo4j's own argument name, read at
5.26.16 — and `EstimatedRowsSource`, which no reference implementation publishes and
which is the provenance §5 credits GoGraph with leading on. Both are OMITTED rather
than zeroed for an operator with no estimate, and both are published for an
`EXPLAIN` as well as a `PROFILE`: an estimate is a prediction made before anything
ran, so an `EXPLAIN` has one and it is the only number an `EXPLAIN` has.

### How the logical estimate is mapped onto a physical operator

The mapping is the hard half of this change, and it is structural rather than
inferred. `buildOperator` is the single funnel every physical operator passes
through on its way out of the recursive lowering, so it is the one place where a
logical node and the operator built from it are both in hand. The rule applied
there is:

> **The first (deepest) logical node whose lowering RETURNS a given operator owns
> it.**

Every operator is CLAIMED — with an estimate when one is derivable and with an
explicitly empty one when it is not — and a claim is never overwritten. That single
rule covers three lowerings that return an operator they did not build, without a
special case for any of them: a `Selection` whose pushed seek hint no seek claimed
returns its child's operator unchanged, a `Selection` over a `shortestPath` fuses
its predicate into the child and returns the child, and an `Expand` built without a
graph returns its child untraversed. In all three the child claimed the operator
first.

### Refuted: the rule is not observable through any query the engine can plan

The brief for #2765 treated the mapping as the task's principal risk. Measured, the
risk is smaller than that and in a different place: **no query the engine can plan
today can be mis-attributed by removing the rule.** Applying a mutation that deletes
first-claim-wins and re-running `./cypher/ ./cypher/exec/ ./cypher/explain/
./bolt/server/` changes no rendered plan, and the reason is structural in each of
the three cases:

* a dropped seek hint's predicate is a correlated key equality — a VARIABLE, not a
  literal or a parameter, on the far side — so `selectionEstimate` declines before
  the rule can matter;
* the `shortestPath` and no-graph cases have a child that is not a scan leaf, which
  `selectionEstimate` and `expandEstimate` also decline.

The rule is therefore defence in depth rather than a live correction, and it is
gated at the level it is implemented (`TestPlanEstimate_AttributionRules`) with that
stated plainly, rather than through a query that cannot exercise it.

### Refuted: reading the estimate through the build's own snapshot LOSES it

The obvious implementation reads the estimate through the resolver the physical
build already holds — the one pinned to the query's MVCC snapshot. That is what the
run measures against, so it looks strictly more correct. It is not usable, and the
reason was measured rather than argued.

`lpgLabelResolver.ResolveLabelCount` answers "EXACT or nothing" and declines the
moment any MVCC history is live, which in a mixed read/write workload is always —
the finding that motivated `ResolveLabelCountBound` in rmp #2392.
`statsRangeEstimateInner` takes that count as its Δ/N denominator with
`n, _ := src.ResolveLabelCount(label)`, does not distinguish the decline from a real
zero, and its `n <= 0` guard then demotes the whole estimate to `estFallback`. Read
through the snapshot resolver, `MATCH (p:Person) WHERE p.age < 30 RETURN p` on a
400-node fixture renders:

```
logical  (ExplainTable)   Selection  Est.Rows = ~84      (stats, err=0.0039)
physical (ProfileTable)   Filter     Est.Rows = -
```

The same node of the same plan, with a figure on one surface and an absence on the
other, the absence appearing and disappearing with unrelated write traffic. The
estimates are therefore read through a LIVE resolver, built exactly as
`Engine.explainInputsFor` builds the one the logical walk reads, so the two surfaces
agree by construction (`TestProfileEstimate_PhysicalAndLogicalAgreeAboutTheSameNode`).
Plan DECISIONS — the min-label re-anchor, the anchor swap, the disjoint reorder —
continue to read the build's pinned snapshot, untouched.

**Found, not fixed:** the `n, _ :=` conflation in `statsRangeEstimateInner` is a
defect in the estimate provider — "cannot answer exactly" and "no live rows" are
different facts and only the second justifies demotion. It belongs to the
planner-statistics work (#2766), because nothing #2765 does may change what the
planner decides.

### Two shapes that CANNOT be mapped, and render `-` on purpose

Both are leaves SYNTHESISED during the build, with no logical node at all:

* the `NodeByIndexRangeScan` a range seek substitutes for a `Selection`'s scan child
  (#1505) — the `ir.NodeByLabelScan` it replaces is never built; and
* the re-anchored scan the minimum-cardinality multi-label rewrite chooses (#2077) —
  the label it scans is picked at BUILD time.

`ExplainTable` shows an estimate for both because that renderer SYNTHESISES a line
for each and calls `rangeSeekLeafEstimate` / `labelScanEstimate` for it. There is no
operator-side equivalent to derive one from, so the physical surfaces render `-`.
Reaching them would need a second estimate-computation site inside each rewrite;
that was not done, because a wrong correspondence between an estimate and an
operator reads as a planner error that never happened, which is worse than an
absence. Two further families are likewise unmapped: the morsel-parallel leaves
(their sub-plan is rebuilt per morsel from fresh IR on a worker goroutine, and the
per-worker build options clear the collector for the same reason they clear the
profiler — rmp #2664's defect with a different field) and the columnar fusion
chains (which build `ColumnarFilter`/`ColumnarProject` directly and discard the
operators `buildOperator` produced).

### Found while doing this: two sim oracles were comparing a data-dependent string

`internal/sim`'s plan-stability checker (`CheckPlanStability`) and the
statistics-refresh report channel (`StatsRegime.PlanChanges`) both compared
`Engine.Explain` renderings for byte equality. With the estimate in that rendering
they immediately reported false positives:

* the plan-stability oracle failed the index-diversity scenario with *"plan drifted
  from its baseline after a plan-cache rebuild"* for a graph that had merely grown
  from 3001 to 3019 nodes — a changed estimate, not a changed plan;
* `PlanChanges` went from 0 to 2 on a statistics rebuild, which is guaranteed once
  the rendering carries a statistics-derived figure, and turned a channel that
  reports *"the planner chose differently"* into one that reports *"my own
  annotation moved"*.

Both now compare `sim.planShape`, the rendering with the estimate annotation
stripped. The estimate is the only data-dependent part of the physical rendering:
operator names are the concrete Go types that were built, and the details are
structural.

### Cost on the ordinary query path

Measured with the project's own `BenchmarkPlanReusePhases` on
cypher-read-label-small (`MATCH (n:N) RETURN count(n) AS c`), two test binaries
built from HEAD and from this tree and run **interleaved** A/B/A, 10 rounds,
`-benchtime=2s`, on an Apple M4 (10 cores, darwin/arm64, go1.27.1), plain build:

| Metric | HEAD | this tree | verdict |
|---|---|---|---|
| `3build` allocs/op | 15.00 ± 0% | 15.00 ± 0% | identical, all samples equal (p=1.000) |
| `4full` allocs/op | 21.00 ± 0% | 21.00 ± 0% | identical, all samples equal (p=1.000) |
| `3build` B/op | 1.469Ki | 1.469Ki | identical, all samples equal |
| `4full` B/op | 2.336Ki | 2.336Ki | identical, all samples equal |

Raw data, every arm and every `benchstat` comparison:
`docs/benchmarks/plan-estimate-physical-2026-09-05-raw/`.

The **noise floor** was measured first, same binary against itself under the same
interleaving: `~ (p=0.739)` and `~ (p=0.670)`, geomean −0.02%.

**No timing claim is made**, because the host was not idle (load average 2.3–4.4
throughout). What was observed, for the record: a reproducible sub-2% difference on
`4full`, which two single-variable experiments failed to attribute to the work —

* adding the per-operator recording branch on top of an otherwise identical tree
  measured as **no difference** (`~ p=0.093`, `~ p=0.481`, geomean +0.13%);
* inserting 160 lines of never-executed code into `api.go` immediately before
  `buildOperatorRec` also measured as **no difference** (`~ p=0.896`, `~ p=0.315`,
  geomean −0.16%), which REFUTES code layout as the explanation at this magnitude;
* `buildOperator` is not inlined in either tree (cost 172 and 255 against a budget
  of 80), so inlining is not the explanation either.

Two changes made in response, each measured: folding the collector's two build-option
fields into ONE pointer (+1.13% → +0.92% geomean) and restoring `buildOperator`'s
single short-circuit early-out so the ordinary query still exits on one chain
(+0.92% → +0.68%). The residual is left, named: merging `buildOpts.profiler` and
`buildOpts.estimates` into one "this build renders its plan" field would make the
struct grow by zero words and the guard identical to HEAD's.

### The gates, and the per-site mutations that prove them

The #2764 addendum's lesson — *"a counter with N increment sites needs N deletions,
not one mutation of the value it returns"* — applies here in a different shape.
There is no counter; there is a MAPPING and a set of rendering rules, and each rule
is a site that can be deleted on its own without any other site noticing. 29
mutations were applied ONE AT A TIME to the working tree, with
`go test -count=1 -p 1 ./cypher/ ./cypher/exec/ ./cypher/explain/ ./bolt/server/`
re-run after each and the tree restored before the next.

**Every one was killed**, and every kill is a behavioural assertion rather than a
compile error. Six of them survived the FIRST sweep and are the reason the sweep was
worth running: `AllNodesScan` and `Expand` had no query planning them (M05, M08),
the two attribution rules had no gate at all (M02, M03), the pass-through-Filter
guard had none (M04), and no test asked what happens to a predicate shape the
estimator does not recognise (M10). Five gates were added to close them.

| # | mutation | first failure |
|---|---|---|
| M01 | delete the recording call in `buildOperator` | `ProfileTable has no Est.Rows and Rows header pair` |
| M02 | remove first-claim-wins | `a second logical node re-attributed an already-claimed operator: {Rows:600 Source:exact} became {Rows:1 Source:heuristic}` |
| M03 | stop writing empty claims | `an operator whose logical node has no estimate was left UNCLAIMED` |
| M04 | drop the pass-through-Filter guard | `a Selection lowered against a walker with no graph was given the estimate {Rows:1 Source:heuristic}` |
| M05 | `AllNodesScan` never estimated | `no Est.Rows column for "MATCH (n) RETURN n"` |
| M06 | `NodeByLabelScan` never estimated | `ProfileTable has no Est.Rows and Rows header pair` |
| M07 | `Selection` never estimated | `the Filter's Est.Rows cell is "-", want a tilde-marked approximation` |
| M08 | `Expand` never estimated | `the Expand's Est.Rows cell is "-", want "3"` |
| M09 | publish a stale statistic as EXACT | `the stale range estimate rendered "84", want "-"` |
| M10 | publish an underivable shape as an exact 0 | `the Filter's Est.Rows cell is "0", want "-"` |
| M11 | read through the build's snapshot resolver | `the fresh range estimate is "-", want a tilde-marked approximation` |
| M12 | `EstRowsCell` prints 0 for no estimate | `the Project's Est.Rows cell is "~0", want "-"` |
| M13 | `EstRowsCell` drops the tilde | `the Filter's Est.Rows cell is "9", want a tilde-marked approximation` |
| M14 | tree prints an estimate for an operator that has none | `plan with DisableParallelScan:true is` … `Project (est. rows=- )` / `└─ LabelCountScan (est. rows=- )`, want the two bare lines |
| M15 | `PlanTreeWithEstimates` does not attach | `ProfileTable has no Est.Rows and Rows header pair` |
| M16 | tree never prints the estimate | `the EXPLAIN rendering does not carry "(est. rows=60 exact)"` |
| M17 | column never rendered | `ProfileTable has no Est.Rows and Rows header pair` |
| M18 | column always rendered | `a plan in which nothing was estimated still rendered an Est.Rows column` |
| M19 | column moved RIGHT of `Rows` | `Est.Rows is column 2 and Rows is column 1` |
| M20 | `ProfileTable` drops the estimate | `ProfileTable has no Est.Rows and Rows header pair` |
| M21 | never published on the wire | `explain published no args map` |
| M22 | published unconditionally | `the un-estimated Project published EstimatedRows=0` |
| M23 | publication gated on `profiled` | `the estimated scan published no EstimatedRows key` |
| M24 | `EXPLAIN`-prefix capture drops it | `rendered plan differs` |
| M25 | `Engine.Explain` drops it | `the EXPLAIN rendering does not carry "(est. rows=60 exact)"` |
| M26 | `PROFILE` drops it | `ProfileTable has no Est.Rows and Rows header pair` |
| M27 | collector never installed | `ProfileTable has no Est.Rows and Rows header pair` |
| M28 | early-out ignores the collector | `the EXPLAIN rendering does not carry "(est. rows=60 exact)"` |
| M29 | `forWorker` stops clearing the collector | `WARNING: DATA RACE` in 6 tests, including `TestProfile_ParallelScanTierIsOneNodeAndRaceFree_2664` |

M29 is the concurrency half and it is the one that could not be gated by an
assertion: with the clearing removed, a morsel-parallel PROFILE writes one shared
map from every worker goroutine. It is killed by `-race` on real planned queries,
which is what shows the clearing is EXERCISED and not merely present.

### Found while doing this, NOT fixed: `TestReadPathAllocationCeiling` is still load-sensitive

`TestReadPathAllocationCeiling` failed **four times** while this task was being
validated, reporting 32.0, 32.0, 38.0 and 32.0 allocations against its ceiling of
20 — a FALSE regression report of the exact shape the gate exists to catch. Three
of the four were under mutations that cannot touch the read path at all (a change
to a `default:` branch of an estimate-conversion switch, a change to a Bolt `if`,
and a change that DISABLES the estimate collector); the fourth was an ordinary
`go test ./cypher/` immediately after a `golangci-lint` run.

What every occurrence has in common is **concurrent compilation**, not load as
such:

* isolated (`-run '^TestReadPathAllocationCeiling$'`), this tree reads exactly
  **20.00**, ten times out of ten;
* 20 consecutive `go test ./cypher/` runs — 10 on this tree and 10 on HEAD,
  interleaved, at load average 3.8–4.4 — produced **zero** failures on either;
* `benchstat` over ten interleaved rounds reports allocs/op IDENTICAL to HEAD, all
  samples equal, p=1.000.

rmp #2753 moved the measurement into a child process to escape sibling test
goroutines, and its own comment records 20.00 isolated against 32.00 contaminated —
32.00 is precisely the number that came back here. The child escapes the siblings
but still shares the machine with whatever else the toolchain is doing.

**Limit of this finding:** the reproducer is concurrent compilation, and it was not
run against HEAD, so this does not establish that HEAD is equally susceptible — only
that this change's allocation count is identical to HEAD's and that the isolated
reading is exactly the calibrated 20.00.

### Still open after #2765

* The two build-synthesised leaves and the two operator families named above carry
  no estimate; the physical `Est.Rows` cell reads `-` for them.
* `statsRangeEstimateInner`'s `n, _ := src.ResolveLabelCount(label)` conflates
  "cannot answer exactly" with "no live rows" (#2766).
* D11 stands: `EXPLAIN` on a WRITING statement still captures a LOGICAL tree, and
  that capture carries no estimates — the estimates on it belong to the logical
  walk, which renders them in words with a certified error term the physical
  renderer cannot reproduce. Nothing marks the captured tree as logical either.
* The `Est.Rows` column carries the number and an approximation marker only; the
  provenance in words remains available on `ExplainLogical` and, since this task, on
  the indented physical renderings and the Bolt `args`.
* `TestReadPathAllocationCeiling` remains sensitive to load outside its own process
  (above).
* One reducible cost is named and not taken: merging `buildOpts.profiler` and
  `buildOpts.estimates` into a single "this build renders its plan" field would make
  the struct grow by zero words and `buildOperator`'s early-out identical to HEAD's.
