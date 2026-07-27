# Exhaustive audit: GoGraph vs Neo4j vs Memgraph (round 4)

**Date:** 2026-07-27 · **Baseline:** `bd3f1a38` (post sprint 316–325 remediation) ·
**Scope:** every functional area of the module, with priority on the areas round 3 declared
uncovered

This is the fourth comparative evaluation, and the first run *after* a remediation cycle.

| Round | Date | Basis | Verdict it reached |
|---|---|---|---|
| 1 | 2026-07-24 | architectural reasoning | "gaps are BREADTH not core quality" |
| 2 | 2026-07-25 | planner, reasoned from source | join ordering and anchoring lose |
| 3 | 2026-07-26 | **first empirical head-to-head** | refuted round 1 for execution: 30–107× slower |
| **4** | **2026-07-27** | **re-measurement + round 3's declared blind spots** | **see §1** |

Round 3 closed with 20 findings, of which 15 were fixed in sprints 316–325. It also did
something unusually valuable: **every stream published a `NOT INVESTIGATED` section**
enumerating what it had not reached. Round 4 takes those sections as its primary work list,
because that is where undiscovered defects were guaranteed to be. Two of the three
correctness findings below come from there.

---

## 1. Verdict

**Round 3's architectural conclusion survives: no deficit found in either round is
architectural.** The storage engine remains the strongest part of the module, and the one
remediation that targeted a measured shape worked spectacularly — the triangle query went from
**107× the best rival to 5.9×**.

**But the re-measurement is sobering in two ways.** Traversal is unchanged, which is honest
(nothing in sprints 316–325 targeted it). And the bulk load is **unchanged at 35 minutes**,
because round 3 named a cause, the remediation fixed that cause, and the load did not move.
Round 4 isolated the real one by decomposition (§2.1), refuting two of its own intermediate
hypotheses on the way: **the write path builds its plan with every optimisation gate switched
off.** `Engine.Run` sets six gates on its `buildOpts`; `buildPlanWithMutatorFull` sets none of
them, so any statement containing a write clause is planned by the unoptimised planner. The
hash join — which makes the read side O(N + B) instead of O(N·B) — is therefore never
substituted for a write. **Part A is fixed in this cycle** (order-neutral gates threaded
through; min-label re-anchor inside a write, **145×**); part B, admitting the hash join, needs
an order-preservation ruling and is not done blind.

But round 4 found **three correctness defects that no gate in the project can see**, and they
share one root cause with round 3's headline lexer defect: *GoGraph's conformance evidence is
the openCypher TCK, and the TCK does not exercise these surfaces at all.*

- `EXISTS { MATCH … }` — **valid openCypher 2024.3, rejected by GoGraph, documented as
  supported.** The TCK contains **zero** occurrences of `EXISTS {`.
- `SHOW INDEXES` over Bolt returns the right rows with **every column null**, silently.
- On **Windows** the Durability mandate is not established: the directory-fsync that makes
  every crash-atomic rename durable is compiled out to a `return nil`, in four packages.

Under the project's own `correct → secure → fast` order these outrank every performance
number in this report.

The second theme is that **three of round 3's open items were larger than it estimated**, and
each was left open because a stream ran out of budget rather than because it was hard to
find:

- `MERGE` — which stream 6 flagged as "the highest-value follow-up" — **walks every interned
  node in the graph, once per `MERGE`**, consulting neither the index nor the label bitmap,
  and never exiting early. `UNWIND … MERGE` is Θ(B·N).
- `shortestPath()` runs forward-only BFS, and its own source comment defers bidirectional
  search "until a measured benchmark shows the doubling pays off". **Round 3 produced that
  benchmark** — 60×, the worst non-triangle ratio — and `search/bibfs.go` already ships the
  algorithm.
- Stream 8 verified **zero** external claims about either incumbent. Closing that gap from
  primary source yielded the single most useful technique in this report (§P1).

The third theme is that **round 3's own measurement harness carries two stale premises** that
make GoGraph look better on writes than it is and worse on numeric keys than it is (§5). Both
are corrected here.

---

## 2. The empirical re-measurement

Same harness, same scale and same dataset as round 3, so the columns are directly comparable:
`bench/comparison/threeway_test.go`, 20 000 nodes / 199 941 edges, Apple M4 (10 cores, 32 GB),
Neo4j 5.26 Community and Memgraph 2.22.0 under Docker/colima, GoGraph in-process. Median of 9
after 3 warm-ups. **Row counts were cross-checked across all three targets and are identical
for every query.** Full data: `docs/benchmarks/threeway-2026-07-27.md`.

| Query | R3 GoGraph | **R4 GoGraph** | Neo4j | Memgraph | Change | R4 vs best rival |
|---|---|---|---|---|---|---|
| `q10_triangle` | 22.506 s | **1.249 s** | 363 ms | 213 ms | **18.0× better** | 5.9× slower |
| `q15_create` | 14 µs | **5 µs** | 4.679 ms | 413 µs | 2.8× better | **83× faster** |
| `q12_multi_label` | 16 µs | **8 µs** | 1.153 ms | 891 µs | 2.0× better | **111× faster** |
| `q07_global_count` | 540 µs | **516 µs** | 1.357 ms | 1.231 ms | — | **2.4× faster** |
| `q08_group_by` | 2.716 ms | **2.518 ms** | 6.792 ms | 3.025 ms | — | **1.2× faster** |
| `q01b_point_lookup_str` | n/a | **6 µs** | 2.039 ms | 280 µs | new | **47× faster** |
| `q14_property_filter` | n/a | **4.454 ms** | 6.02 ms | 2.899 ms | new | 1.5× slower |
| `q01_point_lookup` (int) | 714 µs | **762 µs** | 1.516 ms | 225 µs | — | 3.4× slower |
| `q02_range_scan` | 4.807 ms | **5.009 ms** | 2.488 ms | 434 µs | — | 11.5× slower |
| `q03_starts_with` | n/a | **5.094 ms** | 1.585 ms | 3.707 ms | new | 3.2× slower |
| `q04_one_hop` | 8.711 ms | **8.451 ms** | 1.613 ms | 253 µs | — | 33× slower |
| `q11_expand_into` | n/a | **8.665 ms** | 1.148 ms | 284 µs | new | 31× slower |
| `q05_two_hop` | 14.046 ms | **13.378 ms** | 1.884 ms | 279 µs | — | 48× slower |
| `q06_varlen_3` | n/a | **16.534 ms** | 3.116 ms | 402 µs | new | 41× slower |
| `q13_shortest_path` | 23.797 ms | **24.51 ms** | 1.302 ms | 377 µs | — | 65× slower |
| `q09_top_k` | 30.782 ms | **30.94 ms** | 3.74 ms | 5.852 ms | — | 8.3× slower |
| **`load`** | **35 m 33 s** | **35 m 10 s** | 3.47 s | 968 ms | **unchanged** | **2 180× slower** |

**One thing moved, and it moved decisively.** The expand-into work (`#2213`) took the triangle
from **107× the best rival to 5.9×** — an 18× improvement on the single worst query shape, and
the clearest vindication of round 3 the remediation produced.

**Nothing else moved.** Traversal (`q04`, `q05`, `q06`, `q11`, `q13`) is within noise of round
3. The per-row execution tax that round 3 named as the cause of those ratios was not the
target of sprints 316–325, and the numbers say so honestly.

**And the load did not move at all** — 35 m 10 s against round 3's 35 m 33 s. That is the
finding this section exists for, because round 3 named a specific cause for it and the
remediation fixed that cause.

### 2.1 The load collapse: round 3's root-cause attribution was wrong

Round 3 concluded: *"`resolveSeekValue` resolves the seek operand from raw SOURCE TEXT →
any UNWIND- or WITH-bound key loses the index. **This**, not the numeric-index issue, is the
primary cause of the load collapse."* Sprints 316–319 fixed exactly that (`#2182`, `#2183`,
commits `8bb6553`, `4c53982`). The load is unchanged. So the attribution was incomplete.

Round 4 isolated it by decomposition (`bench/r4audit/load_test.go`, `load2_test.go`,
`load3_test.go`). Each step holds the 500-row batch fixed and grows only the node population
N, so any growth is attributable to N.

**Step 1 — which clause carries the cost?** (µs per row at N = 16 000)

| Statement | N=2 000 | N=16 000 | growth over 8× N |
|---|---|---|---|
| `UNWIND … MATCH (a {sid:r.ss}), (b {sid:r.ts}) RETURN count(*)` | 3.628 ms | 20.582 ms | 5.7× |
| `UNWIND … CREATE (:Q {v:r.ss})` | 853 µs | 1.141 ms | **flat** |
| **`UNWIND … MATCH (a …), (b …) CREATE (a)-[:K]->(b)`** | **470.959 ms** | **3.825 s** | **8.1×** |

The MATCH costs 20 ms; adding the `CREATE` makes the same statement 3.8 s — **186×** — and
turns it linear in N.

**Step 2 — is it the index, or relationship creation itself?**

| Case | growth over 8× N |
|---|---|
| relationship create, **with** index | 8.1× |
| relationship create, **without** index | 8.1× |
| node create, with index | **0.8× (flat)** |

The index is not the cause: identical with and without. Node creation is flat, so it is not
the write path in general.

**Step 3 — the decisive pair.** One variable changes: whether the seek key is a bound row
value or a literal.

| Case | N=2 000 | N=16 000 | growth |
|---|---|---|---|
| bound-key **read** | 4.100 ms | 10.087 ms | 2.5× |
| literal-key **read** | 332 µs | 580 µs | 1.7× |
| **bound-key write** — `UNWIND … MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)` | 226.370 ms | **1.852 s** | **8.2×** |
| **literal-key write** — `UNWIND … MATCH (a:P {sid: 's5'}) CREATE (a)-[:K]->(:Z)` | 2.175 ms | **1.458 ms** | **0.7× (flat)** |

**The literal-key write is flat and 1 270× faster at N = 16 000.** Relationship creation is
therefore *not* O(N) — with a literal key it costs the same at 2 000 nodes and at 16 000.

**Step 4 — two further hypotheses, both refuted.** The first draft of this section concluded
"the row-bound seek does not reach a statement that also writes". Two more measurements killed
it, and both are recorded because the wrong version was published first:

| Case | seek? | N=2 000 | N=16 000 |
|---|---|---|---|
| per-row key, aggregate read | scan | 1.653 ms | 9.523 ms |
| per-row key, scalar read | scan | 1.246 ms | 10.319 ms |
| per-row key, **write** | scan | 231.772 ms | **1.882 s** |
| literal key, aggregate read | **SEEK** | 148 µs | 303 µs |
| literal key, **write** | **SEEK** | 1.843 ms | **2.038 ms** |

- A **literal** key seeks *inside a write statement* and is flat. So the seek is not withheld
  from writes.
- A **per-row varying** key never seeks — on the read side either. So that is not the
  difference between the two columns.

Both per-row cases scan the same population, yet the write is **178× slower than the read**.
The difference is what happens *after* the seek is declined.

**Root cause, verified at the source and confirmed by the planner's own counter.**
`Engine.Run` — the read path — sets six optimisation gates on its `buildOpts`
(`cypher/api.go:1948-1997`: `hashJoinEnabled`, `hashJoinOrderSafe`, `rangeSeekEnabled`,
`minLabelScanEnabled`, `parallelScanEnabled`, `parallelScanThreshold`).
`buildPlanWithMutatorFull` — the write path — constructs its `buildOpts` with **only**
`maxCollectItems` and `edgeTypeFilterCache` (`cypher/api.go:5153`). Every gate defaults to
`false`. **The write path runs the unoptimised planner.**

`hashJoinBuildCount` confirms it directly:

| Statement | hash join fires |
|---|---|
| `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a.sid` | **yes** |
| `… RETURN count(a)` | **yes** |
| `… CREATE (a)-[:K]->(:Z)` | **no** |
| `… SET a.touched = true` | **no** |

The hash join is what makes the read side O(N + B) instead of O(N·B). It is switched off for
every writing statement, and the CPU profile agrees from the other direction — the time is in
`cypher.populateRowCtx` (29.6 % cumulative), `expr.evalExpr` (24.7 %), `expr.evalBinaryOp`
(24.4 %) and `expr.evalProperty` (19.6 %): per-row predicate evaluation over the whole label
population, repeated for every input row.

The code states the situation as a fact in two places — `range_seek_plan.go:131-135` ("any
build path that does not set `bopts.rangeSeekEnabled`, such as the write path") and
`api.go:5049` ("Every other build path … keeps the serial path") — but **records no rationale
for it anywhere**.

**Status: part A fixed, part B needs a ruling.** The order-neutral gates are now threaded into
the write path (`planGates`, `cypher/api.go`): the min-label re-anchor engages inside a write for
the first time, measured at **2.609 ms → 18 µs (145×)** at 20 000 nodes and 10.568 ms → 86 µs
at 80 000, with the read control unchanged. **Honest caveat: part A does not move the load
number**, because the bulk-load shape needs the hash join, and admitting *that* for writes is
not unconditionally safe — a hash join may build on either arm and so reorder rows, while
`SET` is last-write-wins, so two rows targeting the same node with different values are
order-dependent. The principled fix is to pin the probe side to the outer, order-defining arm,
after which the emitted order is identical to the nested loop and every write clause is safe.
That is part B of `#2225`.

### 2.2 How the three engines actually differ on syntax

Round 3 built its clause comparison from documentation and grammar files. Round 4 asked the
three implementations directly, over the same driver, on the same connection
(`bench/comparison/dialect_test.go`, tag `threeway`):

| Form | GoGraph | Neo4j 5.26 | Memgraph 2.22 |
|---|---|---|---|
| `EXISTS { (a)-[:K]->() }` | ACCEPT | ACCEPT | REJECT |
| `EXISTS { MATCH … RETURN … }` | ACCEPT | ACCEPT | REJECT |
| **`EXISTS { MATCH … }`** (no RETURN) | **REJECT** | ACCEPT | REJECT |
| **`COUNT { MATCH … }`** (no RETURN) | **REJECT** | ACCEPT | REJECT |
| `COLLECT { … }` | REJECT | ACCEPT | REJECT |
| **`CALL { … }` subquery** | **REJECT** | ACCEPT | **ACCEPT** |
| `OFFSET` (SKIP synonym) | REJECT | ACCEPT | REJECT |
| `UNION DISTINCT` | REJECT | ACCEPT | REJECT |
| `EXPLAIN` prefix | REJECT | ACCEPT | ACCEPT |
| `PROFILE` prefix | REJECT | ACCEPT | ACCEPT¹ |
| `SHOW INDEXES` | ACCEPT² | ACCEPT | REJECT |
| quantified path pattern `-[:K]->{1,3}` | REJECT | ACCEPT | REJECT |
| `exists(a.x)` legacy function | REJECT | REJECT | REJECT |
| `ORDER BY … NULLS LAST` | REJECT | **REJECT** | REJECT |

¹ The driver probe reported REJECT, but that is **an artefact of my harness**: Memgraph
executes `PROFILE MATCH (a) RETURN a LIMIT 1` correctly through `mgconsole`, returning
operator, hits and relative time. Recorded as ACCEPT on the strength of the direct check.

² Accepted, and then every column value is null — finding C2.

**Two corrections to round 3 fall out of this table:**

- Round 3 recorded `CALL { }` as absent from Memgraph and therefore rated GoGraph *at parity
  with Memgraph*. **Memgraph 2.22 accepts it.** GoGraph is behind **both** incumbents on
  `CALL { }`, not one, which raises its priority.
- Round 3 recorded `ORDER BY … NULLS FIRST/LAST` as supported by Neo4j. **Neo4j 5.26 rejects
  it**, with a parse error enumerating the valid continuations. It is presumably a Cypher 25
  addition; against the version actually being compared, no engine has it, and GoGraph's
  absence is parity rather than a gap.

Note also what the table says about C1: Memgraph rejects the `RETURN`-less `EXISTS { MATCH … }`
too — but it rejects *every* `EXISTS { }` form, so it simply lacks the feature. GoGraph has the
feature with an incomplete grammar, which is a different and more fixable thing.

---

## 3. Findings

Ranked by the project's decision framework: correctness, then security and availability,
then speed.

### Tier 0 — correctness

#### C1. `EXISTS { MATCH … }` is valid openCypher and GoGraph rejects it — while the documentation claims it works

*Closes the `NOT INVESTIGATED` gap: stream 4 never probed subquery-expression forms.*

`cypher/parser/grammar/CypherParser.g4:359-361`:

```antlr
subqueryExist
    : EXISTS LBRACE (regularQuery | patternWhere) RBRACE
    ;
```

`patternWhere` is a bare pattern with an optional `WHERE`; `regularQuery` requires a
projection. There is no alternative for a `MATCH`-led block **without** `RETURN`, so that
form is a parse error. `subqueryCount` (`:363-365`) has the identical shape and the
identical hole.

Measured boundary (`bench/r4recon/subq_test.go`):

| Form | GoGraph |
|---|---|
| `EXISTS { (a)-[:K]->(:P) }` | ACCEPT |
| `EXISTS { MATCH (a)-[:K]->(b:P) RETURN b }` | ACCEPT |
| `EXISTS { MATCH (a)-[:K]->(b) WITH b … RETURN b }` | ACCEPT |
| `EXISTS { MATCH (a)-[:K]->(b) RETURN b UNION … }` | ACCEPT |
| **`EXISTS { MATCH (a)-[:K]->(:P) }`** | **REJECT** — `unexpected "}"` |
| **`EXISTS { MATCH (a)-[:K]->(b:P) WHERE b.id > 1 }`** | **REJECT** |
| **`COUNT { MATCH (a)-[:K]->(:P) }`** | **REJECT** |

**Why this is a conformance defect, not a breadth gap.** The openCypher 2024.3 BNF
(`grammar/openCypher.bnf`, `github.com/opencypher/openCypher@main` — the same document round
3 cited for `ANY SHORTEST` at lines 319-320) derives the form as valid:

```bnf
<exists expression> ::= EXISTS <left brace> <subquery expression argument> <right brace>   (824-825)
<subquery expression argument> ::= <procedure specification> | <graph pattern>             (827-829)
<procedure specification> ::= <statement block>                                            (8-10)
<statement block> ::= <statement>                                                          (12-13)
<linear statement> ::= <primitive statement>... [ <primitive result statement> ]            (28-29)
```

The result statement is **optional** — the square brackets are the specification's. A
`<statement block>` consisting of one `<match statement>` and no projection is therefore
well-formed, and `EXISTS { MATCH (a)-[:K]->(b) }` is valid openCypher 2024.3.

**Why no gate caught it.** `grep -c "EXISTS *{" cypher/tck/features/` returns **0**. The TCK
does not exercise `EXISTS { }` subqueries at all. This is the same blind spot class as round
3's `ERRCHAR` lexer defect: the TCK runs valid queries over covered surfaces, and neither
defect lives on one.

**The documentation asserts the opposite, and its own example fails.**
`docs/cypher.md:132` lists `` `EXISTS { MATCH … }` `` as supported and publishes
`` WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) } `` as the example;
`docs/cypher.md:1007` repeats the claim in prose. Run verbatim
(`bench/r4recon/docexample_test.go`):

```
query : MATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) } RETURN count(n)
        → cypher: parse: unexpected "}" at 1:49
control: MATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) RETURN m } RETURN count(n)
        → ACCEPT
```

The control isolates the gap to exactly the omitted `RETURN`. This violates the CLAUDE.md
rule that documentation must be faithful to the code.

**Severity: HIGH.** A query ported from Neo4j fails loudly (a parse error, not a wrong
answer), so it is less dangerous than round 3's C1 — but it is a conformance breach against
mandate #1, and the documentation actively misleads.

**And the gate that exists cannot catch this class.** `internal/cypherdocgate` guards
`docs/cypher.md`, but it asserts only that five *tokens* appear in the file (`BeginTx`,
`ExplicitTx`, `MaxResultRows`, `MaxResultBytes`, `MaxCollectItems`). It is a
word-presence check, not an example-executes check, so a documented query that fails to
parse passes it. **The generalisable fix is worth more than the specific one: make the doc
gate run the Cypher examples in `docs/cypher.md` and require them to succeed.** That would
have caught this automatically, and it retires the whole class — today nothing verifies that
any documented example works. Filed separately from C1 for that reason.

**Effort: S–M.** The grammar needs one alternative admitting a `RETURN`-less statement block
inside `EXISTS`/`COUNT`; the translator already handles the semantics for the
`RETURN`-bearing form. Note the ANTLR-regeneration gotcha recorded in project memory:
token-neutral edits are safe, token additions require a full ATN rewrite. This edit is
token-neutral.

#### C2. `SHOW INDEXES` and `SHOW CONSTRAINTS` return all-null rows over Bolt

*Closes the `NOT INVESTIGATED` gap: stream 4 saw this and recorded it as "likely an artefact
of how I read the result … deserves five minutes". It is not an artefact.*

Proven end to end against the official `neo4j-go-driver/v5`, in-process Bolt server
(`bolt/server/zz_r4_show_test.go`):

```
SHOW INDEXES  rows=1  keys=[name state type entityType labelsOrTypes properties]  non-nil values=0
```

One row, six correctly-named columns, **every value null**. Root cause, a two-line chain:

- `cypher/show.go:164` builds a **streaming** result: `exec.Run(ctx, exec.NewStaticRows(rows), cols)`
  passed to `newResult(rs, cols, nil, nil, nil)`.
- `cypher/api.go:3985-4004` `Result.ValueAt` reads only the **materialised** backing store
  (`r.matChunk` / `r.rowSlice`). Its own godoc says it "must only be called after a
  successful `Next` on a materialised result". On a streaming result it returns `nil` — not
  `expr.Null`, a bare nil interface.
- `bolt/server/session.go:1292-1304` reads every column with `ValueAt`, on the stated
  premise that *"the engine result is always materialised"*. For `SHOW` that premise is
  false, and nothing checks it.

**Severity: HIGH.** `SHOW INDEXES` is the standard way for an operator, a driver's tooling,
or Neo4j Browser's `:schema` to inspect a database. Over Bolt, GoGraph answers with a
well-formed row of nulls and no error — fail-silent, which the CLAUDE.md failure-handling
rule forbids outright. The Go API is affected identically for any caller using `ValueAt`;
`Record()` is unaffected, which is why the in-tree tests missed it.

**FIXED** (`#2215`). `ValueAt` now serves a streaming result from the `ResultSet`'s positional
row, which fixes the whole class rather than the one instance — any future non-materialised
plan is safe here. `SHOW INDEXES` over Bolt returns six populated columns where it returned
six nulls; the Go API agrees. Two regression gates: `bolt/server/show_values_e2e_test.go`
proves the contract through the official driver, and `cypher/show_valueat_test.go` pins the
invariant the defect broke — `ValueAt` and `Record` must describe the same row, which is
exactly the comparison that would have caught it. No hot-path cost: benchstat over the Bolt
encode benchmarks reports **−0.49 % geomean sec/op with B/op and allocs/op unchanged**.

#### C3. String ordering diverges from Neo4j for supplementary-plane characters

*Closes the `NOT INVESTIGATED` gap: "ORDER BY collation and string comparison semantics …
not probed".*

`bench/r4recon/lang_test.go` orders a set separating the two candidate collations:

```
gograph    : "Z" "a" "e" "z" "é" "ﬁ" "😀"      == Unicode code-point order
neo4j/JVM  : "Z" "a" "e" "z" "é" "😀" "ﬁ"      == UTF-16 code-unit order
```

GoGraph compares UTF-8 bytes, which is exactly code-point order. Neo4j compares through
Java's `String.compareTo`, which is UTF-16 code-unit order; surrogate pairs (`D800..DFFF`)
sort *below* BMP characters in `E000..FFFF`, so every supplementary-plane character sorts
before `ﬁ` on Neo4j and after it on GoGraph.

**Severity: LOW–MEDIUM, and it needs a ruling rather than a fix.** Code-point order is the
more defensible choice and is what a Go library should do; the openCypher BNF does not
specify a collation. But the divergence is real, silent, and affects any `ORDER BY` over
user text containing emoji. **Recommendation: document it explicitly in `docs/cypher.md`
as a declared divergence rather than change it.** This is a behaviour decision and is
put to the user in §7.

### Tier 1 — durability

#### D1. On Windows the Durability mandate is not established

*Closes the `NOT INVESTIGATED` gap: "Windows durability posture … This deserves its own
audit; it is a potential Durability gap on a supported platform."*

`parentDirFsync` is `func(_ string) error { return nil }` in **four** packages:

- `store/snapshot/parent_fsync_other.go:28`
- `store/wal/parent_fsync_other.go:18`
- `store/recovery/parent_fsync_other.go:19`
- `store/csrfile/parent_fsync_other.go:20`

The module's own comment at `store/snapshot/full.go:710-720` states the failure mode
precisely: without the directory fsync, "a crash after the rename (and after the
checkpointer truncates the WAL) could otherwise leave the published snapshot directory
present but its components missing or zero-length — **total loss of every transaction folded
into the checkpoint**." The mitigation is described, then compiled out on Windows.

Windows is **not** a release target (`.goreleaser.yml:34` — `goos: [linux, darwin]`) and is
not named in `README.md` or `docs/persistence.md`. But GoGraph is a *library*: nothing stops
`GOOS=windows go build`, and `parent_fsync_other.go` exists precisely so it compiles. A user
who does so gets an engine that claims ACID and cannot deliver the D.

**A primitive does exist.** NTFS journals metadata, and `MoveFileEx` with
`MOVEFILE_WRITE_THROUGH` flushes the rename to disk before returning; Go's `os.Rename` uses
`MoveFileEx` **without** that flag.

Three options, and this is a scope decision — see §7.

### Tier 2 — performance levers taken from the incumbents

#### P1. Group commit: take Neo4j's design, including the detail that solves round 3's own objection

Round 3 F(s08)-N2 measured GoGraph **flat at 261 op/s from 1 to 1024 writers** and F(s08)-F5
found that GoGraph's group-commit experiment had "an O(N) thundering-herd wake and collapses
8.8× past its optimum". Round 3 could not compare against either incumbent: stream 8
exhausted its web budget and **verified zero external claims** — its own words, "the largest
gap in this report". That gap is closed here from primary source.

Neo4j 5.26, `community/kernel/…/transaction/log/TransactionLogQueue.java` (pinned at
`eccd584`):

| Element | Neo4j | GoGraph today |
|---|---|---|
| Submission | `MpscUnboundedXaddArrayQueue` — lock-free multi-producer, single-consumer (`:59`) | committer holds the exclusive barrier across its own fsync |
| Batching | one dedicated appender thread drains `CONSUMER_MAX_BATCH = 1024` per force (`:49`, `:222`) | none — one fsync per commit |
| Force | `logFile.locklessForce(logAppendEvent)` (`:231`) — **explicitly lockless** | under the commit lock |
| Rotation | `logRotation.locklessRotateLogIfNeeded(…)` (`:228`) — **explicitly lockless** | checkpoint phase 3 holds the commit lock (round 3 S3/F4) |
| Idle strategy | `SpinParkCombineWaitingStrategy` — spin 1000, then 10 ns park, then 10 ms (`:356-377`) | n/a |

**The detail that matters most.** Round 3 rejected its own group-commit prototype partly on
the O(N) wake. Neo4j does not pay it on the critical path. `TxConsumer.complete()` (`:330-337`)
unparks **exactly one** waiter — the first element of the batch — after handing it the whole
batch array. That waiter, in `getCommittedAppendIndex()` (`:149-161`), unparks the other
N−1 *itself*, after it has already been woken:

```java
public void complete() {
    TxQueueElement first = txElements[0];
    first.elementsToNotify = elements;
    first.appendIndexes = appendIndexes;
    LockSupport.unpark(first.executor);      // ← O(1) on the appender thread
    ...
}
```

The appender's critical section is therefore **O(1) regardless of batch size**; the O(N) wake
is delegated to an application thread and runs in parallel with the appender's next batch.
**This is the specific technique that makes round 3's rejected prototype viable**, and it is
the highest-value single thing this audit found to take from either incumbent.

**Constraint that still governs.** `#2193` is open precisely because group commit moves the
durable flush relative to the visibility barrier, opening a visible-but-not-durable window —
the failure mode `internal/crashinject/` exists to catch. Neo4j's design does not remove
that obligation; it shows how to make the batch cheap once the ordering is proven correct.
Sequence: prove the barrier/flush ordering first, then adopt the queue, then the one-waiter
wake chain.

#### P0. `MERGE` walks every node in the graph, once per `MERGE` — the bulk-load statement is Θ(B·N)

*Closes the `NOT INVESTIGATED` gap stream 6 named "**the highest-value follow-up, because
`UNWIND … MERGE` is *the* bulk-load statement**". It is worse than that stream suspected.*

`cypher/exec/merge_search.go:50-55`, the module's own comment:

> "The function returned by `NewMergeSearchFnFromPattern` **walks every interned node id**,
> resolves the label and property bag, and admits the node iff every label and every
> property matches. **Match scaling is O(N) where N is the number of interned nodes**; the
> cost is acceptable for the typical MERGE workload (small N or label-restricted pattern). A
> future revision may use `labelResolver`'s bitmap intersection to short-circuit label
> scans."

Both search paths — the closure at `:82` and the row-aware `searchMergeNodes` at `:119` —
call `mutator.WalkNodeIDs`. Three separate defects compound:

1. **The index is never consulted.** A hash or btree index on the merge key exists and is
   ignored; this is the same class as round 3's Q1/Q2, which were fixed for `MATCH` and left
   unfixed here.
2. **The label bitmap is never consulted either** — the walk is over *every interned node
   id*, not over the label's posting list, and label matching is done per node afterwards
   (`nodeMatchesAllLabels`). The source comment concedes this.
3. ~~There is no early exit.~~ **Withdrawn — this was wrong, and measurement caught it.**
   Both callbacks `return true` after appending a match (`:97`, `:134`), and the first draft
   of this report called stopping at the first match "one line, an unconditional win". It is
   not: `MERGE` binds **every** matching node, exactly as `MATCH` does. Measured on three
   `:X {v: 1}` nodes, `MERGE (n:X {v: 1}) RETURN n.v` yields **3 rows**. An early exit would
   silently drop rows and break openCypher semantics. The full walk is *correct*; only its
   access path is wrong.

**Consequence.** `UNWIND $rows AS r MERGE (p:Person {id: r.id})` over B rows against N nodes
is **Θ(B·N)**, and it is the idiom every driver's documentation teaches for idempotent bulk
ingest.

**Stated precisely, to avoid double-counting with W1 (§2.1):** the three-way harness loads with
`MATCH … CREATE`, not `MERGE`, so this finding is **not** what the measured 35-minute load
consists of — W1 is. They are two independent defects on the same ingest surface, reached by
different routes: W1 is a plan-time decision that withholds an existing seek from writing
statements, while P0 is a hand-written search that was never wired to any access path at all.
Fixing W1 will not fix P0, and a user who follows the idempotent-ingest advice hits P0 rather
than W1. Both are unremediated by sprints 316–319, which fixed `MATCH` on the read path.

**Contrast.** Neo4j plans `MERGE` as an anti-conditional apply over the same access paths a
`MATCH` gets, so an indexed merge key yields a `NodeUniqueIndexSeek`/`NodeIndexSeek`; Memgraph
likewise routes MERGE's match through its label-property index.

**Recommendation, in cost order:** (a) restrict the enumeration to the label posting list,
which the code already identifies as the fix; (b) route the merge key through the existing
seek machinery. Both narrow *which* candidates are examined while still enumerating all of
them, so both preserve the multi-match binding. **Severity: HIGH.**

*Checked and cleared:* `cypher/exec/create_node.go:363` also calls `WalkNodeIDs`, but it is
`seedGlobalNodeCounter`, guarded by `globalNodeCounterSeededOnce` — once per process,
amortised. Not a defect.

#### P2. Degree rewrites: `COUNT { }` costs 88× a bare scan and should cost O(1)

*Closes the `NOT INVESTIGATED` gap: "CorrelatedApply / RollupApply / subquery execution …
Entirely unexamined."*

Measured per-outer-row cost, `bench/r4recon/percall_test.go`, 1 000 → 8 000 nodes, out-degree
4, best of 5:

| Shape | 1 000 | 8 000 | µs/row @8k | vs bare scan |
|---|---|---|---|---|
| `MATCH (a:P) RETURN count(a)` | 31 µs | 217 µs | 0.027 | 1× |
| `OPTIONAL MATCH (a)-[:K]->(b)` | 410 µs | 3.461 ms | 0.432 | 16× |
| `EXISTS { MATCH (a)-[:K]->(b) RETURN b }` | 480 µs | 4.236 ms | 0.529 | 20× |
| `WHERE (a)-[:K]->(:P)` | 616 µs | 6.042 ms | 0.755 | 28× |
| `[ (a)-[:K]->(b) \| b.id ]` | 1.601 ms | 14.017 ms | 1.752 | 65× |
| **`COUNT { (a)-[:K]->(:P) } > 0`** | 2.163 ms | 19.048 ms | **2.381** | **88×** |

**Every shape is linear in outer rows** — so `CorrelatedApply`'s per-row `inner.Init()` is a
constant tax, not a re-scan. That is a genuinely good result and refutes the obvious
hypothesis. The problem is the size of the constant, and one shape is indefensible:
`COUNT { … } > 0` enumerates every neighbour to compare a count against zero.

Neo4j solves exactly this with a dedicated planner rewriter,
`cypher-planner/…/logical/steps/getDegreeRewriter.scala`:

- `EligibleExistsIRExpression` → `HasDegreeGreaterThan(node, type, dir, 0)`
- `EligibleCountLikeIRExpression` → `GetDegree(node, type, dir)` — which also covers
  `size([(a)-[:R]->() | …])`
- every comparison of a degree against a literal → a short-circuiting `HasDegree*`:
  `GreaterThan(GetDegree(...), limit)` → `HasDegreeGreaterThan`, and symmetrically for
  `<`, `<=`, `>=`, `=`, in both operand orders.

`HasDegreeGreaterThan(n, t, d, k)` stops counting at `k+1`. `COUNT { } > 0` becomes "does
this node have at least one such edge".

**GoGraph cannot express this today: there is no degree primitive at all.**
`grep -rn "Degree(" graph/` returns nothing; `AdjList` exposes `Order()` and `Size()`
(graph-wide) but no per-node degree. The adjacency stores each node's neighbours
contiguously, so degree is O(1) *by construction* — the primitive is free and merely
unexposed.

**Recommendation, two steps:** (1) expose `AdjList.Degree(node, type, dir)`; (2) add the
degree rewrite for `COUNT {…} <cmp> literal`, `EXISTS {…}` and `size([pattern | …])`.
Step 1 is independently useful. Expected: the 88× row collapses toward the 1× baseline for
the `> 0` case, and `EXISTS` follows.

#### P2b. `shortestPath()` runs forward-only BFS — and the module already ships a bidirectional one

*Partially closes the `NOT INVESTIGATED` gap: "`varlen_expand.go` and `shortest_path.go`
(2 311 lines) as runtime paths … I did not profile them, and the cross-stream benchmark
shows var-len at 34× and shortest-path at 60× — the worst non-traversal ratios."*

`cypher/exec/shortest_path.go:19-29` states the choice and, importantly, its exit condition:

> "**Bidirectional BFS** — halves the expected exploration cost when both endpoints are fixed
> and the graph is reasonably regular, but doubles implementation and test surface and confers
> no asymptotic improvement. **Forward-only BFS is kept until a measured benchmark shows the
> doubling pays off in this codebase.**"

**Round 3 supplied that benchmark and nobody connected it to this comment.** `shortestPath`
bounded at ≤ 6 hops measured **23.797 ms against Neo4j's 1.089 ms and Memgraph's 395 µs — 60×,
the worst non-triangle ratio in the whole matrix**, on a graph with average degree 10.

The arithmetic is why. Forward-only BFS to depth *d* with branching factor *b* explores
Θ(b^d); bidirectional explores Θ(b^⌈d/2⌉) from each side. At b ≈ 10 and d = 6 that is 10⁶
versus 2 × 10³. The comment is right that there is no *asymptotic* improvement in the
complexity-class sense, and wrong that this makes it unimportant: halving the exponent is
precisely where three orders of magnitude of constant live.

**GoGraph already contains the algorithm.** `search/bibfs.go:38` exports
`BiBFS[W any](c *csr.CSR[W], src, dst graph.NodeID)`, and `search/bidijkstra.go:33` the
weighted analogue. Both are tested, and `#1685` validated the Cypher operator *against a
BiBFS oracle* — so the two implementations already meet in the test suite but not in the
product.

**Stated honestly, it is not a drop-in.** `search.BiBFS` runs over an immutable `csr.CSR`,
while the Cypher operator works over the mutable adjacency and must additionally honour
tombstones, the relationship-type filter, and relationship-uniqueness; `allShortestPaths`
further needs the multi-predecessor DAG, which a two-sided search makes materially harder to
reconstruct. The *technique* transfers; the function does not. A reasonable scope is
bidirectional search for the single-path `shortestPath()` only, leaving `allShortestPaths`
level-synchronous.

**Severity: HIGH for the measured shape.** The exit condition the source itself set has been
met.

#### P3. `Engine.Explain` describes a plan that is not the one that runs

*Extends round 3's s08-F9 from one instance to the general case.*

`Explain` renders the **logical IR** (`cypher/ir/explain.go`), not the physical plan. Every
physical decision made in `buildOperator` is invisible, and in both directions:

- Round 3 found `Explain` reporting `NodeByIndexSeek` where a label scan ran.
- Round 4 found the reverse. For `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)`,
  `Explain` prints `CartesianProduct`. The runtime counter `hashJoinBuildCount` proves a
  `HashJoin` is substituted. A reader diagnosing this query sees O(n·m) where O(n+m) runs.

Verified at runtime rather than from `Explain` (throwaway in-package probe over
`hashJoinBuildCount`):

| Shape | hash join substituted? |
|---|---|
| `MATCH (a)-[:K]->(b), (c)-[:K]->(b) RETURN count(*)` | yes |
| `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a, b` | yes |
| `… RETURN collect(a.id)` | no — order guard, correct |
| `… RETURN a.id LIMIT 5` | no — bare-LIMIT guard, correct |
| `MATCH (a:P), (b:P) RETURN count(*)` | no — no equi-join key, correct |

**The hash join itself is healthy** and its guards are right; this is a "nothing to take".
The defect is that no user can see any of it. The columnar tier, the parallel tier, the
index-seek precedence and the hash join are all invisible, which is also round 3's s05-F6
("you cannot tell whether the vectorized or parallel tier engaged").

Both incumbents expose the executed plan with per-operator row counts (Neo4j `PROFILE` adds
db-hits; Memgraph `PROFILE` adds relative time). GoGraph has neither a `PROFILE` nor a
faithful `EXPLAIN`. **Severity: MEDIUM-HIGH** — it is the tool a user reaches for when
GoGraph is slow, and it misinforms them.

### Tier 3 — ergonomics and interoperability

#### E1. Typed Go slices are rejected as parameters

`bench/r4recon/lang_test.go`:

| Parameter type | Verdict |
|---|---|
| `int`, `int64`, `int32`, `uint64`, `float64`, `float32`, `string`, `bool`, `nil` | ACCEPT |
| `[]any`, `map[string]any` | ACCEPT |
| **`[]int`, `[]string`, `[]map[string]any`, `[]byte`** | **REJECT** — `unsupported parameter type` |

For a Go library this is a sharp ergonomic edge: `[]string` and `[]int` are what a Go caller
naturally holds, and the `UNWIND $rows AS r` bulk-load idiom needs `[]map[string]any` — the
single most common shape in every driver's documentation.

**FIXED** (`#2219`), except `[]byte`, deliberately. A reflection fallback in the `default` arm
binds any slice and any string-keyed map, recursively, so arbitrary typed nestings now work;
the concrete type switch still serves everything it served before, so **allocs/op and B/op are
identical on every previously-supported shape** (benchstat, six runs each side; sec/op +0.41 %
geomean, the only significant case being +2.7 ns on the shallowest scalar path — the cost of
the depth counter, reported rather than hidden).

Two things the fix did *not* do, both on purpose:

- **`[]byte` still fails, loudly and with a reason.** Bolt has a distinct Bytes wire type and
  `expr` has no value kind for it. Binding `[]byte` as a list of integers would bind
  *something*, but it would not round-trip: a driver that sent Bytes would read back a list.
  That moves the failure from a clear error at bind time to a silent type change at the far
  end, which is worse. Giving GoGraph a real bytes value is a type-system change and belongs
  in its own task.
- The work uncovered that the **pre-existing `[]any` / `map[string]any` recursion was itself
  unbounded** — the depth guard was written for the new reflected path and the old path had
  none. The bound now covers the whole binder, since a parameter is external input and
  guarding one of two paths leaves the class open through the other.

---

## 4. Where GoGraph is ahead — do not take their approach

#### Durability, decisively — and round 3 under-claimed this

Memgraph's authoritative default configuration
(`tests/e2e/configuration/default_config.py:264-268`):

```python
"storage_wal_file_flush_every_n_tx": (
    "100000", "100000",
    "Issue a 'fsync' call after this amount of transactions are written to the WAL file. "
    "Set to 1 for fully synchronous operation.",
),
```

**Memgraph fsyncs once every 100 000 transactions by default.** A crash loses up to 100 000
committed transactions. Durability per commit is opt-in and costs a documented order of
magnitude. GoGraph fsyncs every commit, and on Darwin that is `F_FULLFSYNC` — the strong
barrier, not the weak `fsync` (round 3 s08-F8).

#### Isolation

| | Default isolation | Write skew |
|---|---|---|
| **GoGraph** | serialisable by construction — single writer over an immutable snapshot | structurally impossible |
| Neo4j 5.26 | **read-committed** (Operations Manual, *Database internals*: "The default isolation level is read-committed"); serialisable requires manually acquiring write locks | possible at the default |
| Memgraph 2.22 | snapshot isolation (`default_config.py:95-98`) | Memgraph documents that no level of its own prevents it |

GoGraph offers the strongest default of the three. This is a contract to defend, and it is
the reason the single-writer ceiling is a *ruling* rather than a defect (round 3 s02-F10).

#### Checksums, error fidelity, exactness

Unchanged from round 3 and re-confirmed: snapshot components are checksummed where
Memgraph's are not; torn-tail versus corruption is discriminated; algorithm exactness and
determinism are documented contracts that GDS does not offer.

---

## 5. Corrections to round 3

**M1. The head-to-head compares a GoGraph with no durability against two rivals with WAL
enabled.** `bench/comparison/threeway_test.go:470-473` — `newEmbeddedTarget` builds a bare
`lpg.Graph`; there is no `store.DB`, no WAL, no fsync. Neo4j forces its transaction log per
commit; Memgraph writes a WAL and fsyncs every 100 000 transactions. Round 3 disclosed the
in-process-versus-container asymmetry but **not** this one. Consequences, both directions:

- GoGraph's write wins are overstated. "Single-node write 21× faster" is an in-memory
  mutation measured against two durable engines.
- GoGraph's traversal losses are *understated*, which strengthens round 3's conclusion: it
  lost 30–107× on reads while carrying no durability cost at all.

The harness should grow a fourth GoGraph target backed by `store.DB` so the durability axis
is measured instead of silently removed.

**M2. The harness's reason for joining on a string key is now stale.**
`threeway_test.go:421-424` joins edges on `sid` because "GoGraph's Cypher `CREATE INDEX` can
only build string-keyed indexes". Numeric equality **is** served now: `#2169` rewrites
`n.prop = v` — including the inline `MATCH (a:P {id: 250})` form — into the degenerate closed
range `[v, v]` over the unified float64 companion btree
(`cypher/range_seek_plan.go:640-676`, `cypher/index_binding.go:243-300`). Round 3's Q2 was
implemented. The harness comment should be corrected and a numeric-key load variant added,
or the module keeps being benchmarked around a defect it has fixed.

**But the numeric path is structurally served and empirically slow, and §2 shows the gap for
the first time.** The harness now runs an indexed point lookup on both key types, on the same
data at the same scale:

| Query | GoGraph | Neo4j | Memgraph |
|---|---|---|---|
| `q01b_point_lookup_str` — indexed **string** key | **6 µs** | 2.039 ms | 280 µs |
| `q01_point_lookup` — indexed **numeric** key | **762 µs** | 1.516 ms | 225 µs |

**127× between two indexed point lookups that differ only in key type.** The string form is
the fastest of all three engines by 47×; the numeric form is 3.4× slower than Memgraph. So
`#2169` made the numeric case *correct* — the seek is admitted — without making it *fast*. The
degenerate `[v, v]` range over the companion btree is evidently not reaching the same access
path the string hash index gets. This is a NEW finding that only the two-key comparison could
surface, and it is why M2's harness fix matters beyond tidiness: **it is worth its own
investigation**, and it should be measured before anyone assumes round 3's Q2 is closed on
performance as well as on semantics.

**M3. Stream 4's `SHOW INDEXES` "artefact" was a real defect** — see C2.

**M4. Two of round 3's clause-support claims do not survive contact with the engines** — see
§2.2: Memgraph 2.22 *accepts* `CALL { }` (round 3 said it does not, and rated GoGraph at
parity), and Neo4j 5.26 *rejects* `ORDER BY … NULLS LAST` (round 3 said it supports it).

---

## 6. Coverage: which round-3 blind spots this round closed

| Round-3 declared gap | Round 4 |
|---|---|
| s01 — on-disk format upgrade path never exercised | **CLOSED (partial)** — `csrfile`, `snapshot`, `wal` compat/version-upgrade suites run green. Loading a genuinely old on-disk corpus is still not done. |
| s01 — Windows durability posture | **CLOSED** — finding D1 |
| s03 — behaviour under memory pressure | **CHARACTERISED** — see §7 Q3 |
| s04 — `ORDER BY` collation | **CLOSED** — finding C3 |
| s04 — parameter type coercion | **CLOSED** — finding E1 |
| s04 — `SHOW … YIELD` projection | **CLOSED** — finding C2 |
| s05 — `CorrelatedApply` / subquery execution | **CLOSED** — finding P2 |
| s05 — `ColumnarHashJoin` never benchmarked | **PARTIAL** — trigger correctness verified at runtime (P3); throughput still unbenchmarked |
| s06 — does `MERGE`'s match use the index? | **CLOSED** — finding P0. No: it walks every node. |
| s08 — *all* Neo4j and Memgraph concurrency behaviour | **CLOSED** — finding P1 and §4, from primary source |
| s05 — `shortest_path.go` as a runtime path | **CLOSED** — finding P2b |
| s05 — `varlen_expand.go` as a runtime path | **OPEN** — 34× and still unprofiled |
| s09 — decomposition of the Cypher-ingest figure | **CLOSED** — §2.1 decomposes it to a single cause (W1). The fsync/planning/tx-setup split turned out not to be where the time was: the same statement with a literal key is flat. |

---

## 7. Decisions for the user

Per the decision-autonomy rule, these change behaviour, scope or architecture and are not
mine to take. They are listed one per decision, in priority order.

**Q1 — Windows.** Three options: **(a)** implement `MOVEFILE_WRITE_THROUGH` renames plus a
Windows durability test lane; **(b)** document a platform durability matrix stating plainly
that Durability is guaranteed on Linux and Darwin only; **(c)** make `store.Open` refuse to
run on a platform without a directory-fsync primitive. **Recommended: (b) now, (a) next** —
(b) removes the silent claim immediately at near-zero cost, (a) is the real fix, (c) is
defensible but would break any existing Windows embedder without warning.

**Q2 — string collation (C3).** **(a)** Document code-point order as a declared divergence;
**(b)** switch to UTF-16 code-unit order for Neo4j parity. **Recommended: (a)** — code-point
order is the more principled choice, the openCypher BNF does not specify a collation, and
(b) would make a Go library behave like a JVM artefact for no user benefit.

**Q3 — memory pressure.** GoGraph has no byte budget on the stored graph: only
`Config.MaxShardCapacity` (a slot count) and the per-query `MaxResultBytes`. Memgraph has a
hierarchical `MemoryTracker` and a default `memory_limit` of 100 %/90 % of physical memory,
aborting the offending query with a typed `OutOfMemoryException`; Neo4j caps transaction
memory at 70 % of heap. A GoGraph embedder that overcommits gets an OS OOM kill **of the host
application**. Options: **(a)** expose a resident-bytes estimator and let the host decide;
**(b)** own a global byte budget with a typed error. **Recommended: (a)** — an embeddable
library should not unilaterally own the host's memory policy, and (a) is a prerequisite for
(b) in any case.

---

## 8. Recommended sequence

1. **W1 part B** — admit the hash join for writing statements by pinning the probe side to
   the outer, order-defining arm (§2.1). Part A (the order-neutral gates) is **done**; part B
   is what stands between GoGraph and a 2 180× deficit on the first operation any user
   performs. It is also a prerequisite for judging P0 fairly, since `MERGE` and
   `MATCH … CREATE` share the ingest path.
2. **C2** `SHOW` over Bolt — fail-silent, effort S.
3. **C1** `EXISTS { MATCH … }` — conformance breach; fix the grammar *and* the two
   documentation claims.
4. **Doc gate** — make it execute the examples in `docs/cypher.md` rather than grep for
   tokens (§C1). Ranked here because it retires a whole class rather than one instance, and
   because it is what would have caught C1's documentation half without an auditor.
5. **P0** `MERGE`'s full-graph walk: label posting list → index seek. Both must keep
   enumerating every match, because `MERGE` binds all of them.
6. **M2-spike** why an indexed numeric point lookup is 127× an indexed string one (§5). It is
   a spike, not a fix, and it gates any claim that round 3's Q2 is closed on performance.
7. **Q1** decision, then the Windows work it selects.
8. **P2** degree primitive, then the degree rewrite — the largest measured per-row win
   available.
9. **E1** parameter reflection — effort S, removes a daily friction.
10. **M1/M2** repair the harness before the next round measures around stale premises.
11. **P2b** bidirectional `shortestPath()` — scoped to the single-path operator.
12. **P1** group commit, in the stated order: barrier/flush ordering proof → MPSC queue →
    one-waiter wake chain.
13. **P3** a faithful plan surface (`PROFILE`, or an `Explain` that renders the physical plan).
14. **CALL { }** subquery — round 4 corrected round 3 here: GoGraph is behind *both*
    incumbents, not at parity with Memgraph.

Still open from round 3 and not re-litigated here: `varlen_expand.go` profiling (s05, 34×),
the fsync/planning/tx-setup decomposition of the Cypher-ingest figure (s09), and
`ColumnarHashJoin` throughput (s05).

---

## 9. Method and evidence

- **Primary sources only for incumbent claims.** Neo4j behaviour is cited from
  `github.com/neo4j/neo4j` at pinned SHA `eccd584a64d468af3daeab421478fe78567c518f` and from
  the Operations Manual; Memgraph from `github.com/memgraph/memgraph@master`
  `tests/e2e/configuration/default_config.py`; openCypher from
  `github.com/opencypher/openCypher@main` `grammar/openCypher.bnf`. Nothing about either
  product is asserted from memory — the rule round 3's stream 8 could not honour.
- **Every GoGraph claim carries a `file:line` or a measurement.** Reconnaissance harnesses
  live in `bench/r4recon/` and the Bolt proof in `bolt/server/`.
- **Three of this round's own hypotheses were refuted by measurement and are reported as
  such:** the shared-destination pattern does *not* degenerate to a nested loop (the hash
  join fires), and `CorrelatedApply` does *not* re-scan per outer row (all shapes are
  linear). The first was reached from `Explain` and was wrong precisely because of P3 —
  the audit walked into the defect it then reported. Third, the load decomposition began from
  the hypothesis that relationship creation was itself O(N) — steps 2 and 3 of §2.1 killed it,
  and only the literal-versus-bound pair identified the real cause.
- **One reported REJECT was withdrawn on a second check:** the driver probe said Memgraph
  rejects `PROFILE`; `mgconsole` runs it correctly. A negative result from one access path is
  not a property of the engine.
- Apple M4 (10 cores, 32 GB), Go toolchain as pinned by the module, Neo4j 5.26 Community and
  Memgraph 2.22.0 under Docker/colima.

---

## 10. Where this landed

- Report: this file. Data: [`benchmarks/threeway-2026-07-27.md`](benchmarks/threeway-2026-07-27.md).
- Evidence harness: `bench/r4audit/`, behind the **`r4audit`** build tag; the dialect matrix is
  `bench/comparison/dialect_test.go`, behind `threeway`. The `SHOW` proof started as a
  deliberately-failing tagged test and, now that C2 is fixed, has become the permanent
  regression gate `bolt/server/show_values_e2e_test.go` in the short layer.
- Plan: **rmp sprint 326** (OPEN), 14 tasks — `#2225`, `#2215`, `#2216`, `#2227`, `#2217`,
  `#2226`, `#2218`, `#2219`, `#2220`, `#2221`, `#2222`, `#2223`, `#2224` — ordered as §8.
- Three decisions are **not** pre-empted and carry no task: Q1 Windows durability, Q2
  collation, Q3 memory-pressure policy (§7).
