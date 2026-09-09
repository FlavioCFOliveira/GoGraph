# Knowledge-graph fidelity — measured state, 2026-09-08

Recorded under rmp **#2719** (sprint 357), at `0e7e982d` on `release/0.14.1`.

This is the output of `cmd/kgverify`, written down so the next cycle can measure **drift**
rather than rediscover the gap. Everything below was produced by a command that is in the
repository and can be re-run; nothing here is an estimate.

```bash
make kg-verify                            # the full gate, all 20 checks
go run ./cmd/kgverify -emit packages      # the per-package table reproduced below
go run ./cmd/kgverify -emit absent        # nodes naming a symbol the tree lacks
go run ./cmd/kgverify -emit missing       # declarations with no node
go run ./cmd/kgverify -emit cypher        # statements that close the gap
```

A running graph server is required for every one of them: `rmp graph serve -r gograph`
is the only process that opens the store, and `rmp graph client` is the only way to run a
statement. With nothing listening the client exits 1 and reads nothing.

## What the gate could not do before this task

`cmd/kgverify` **did not run at all**. It shelled out to `rmp graph query`, one of the five
subcommands rmp 1.17.0 removed when it rebuilt `rmp graph` as a server/client pair, so every
invocation exited 3 — *the harness could not conclude*. Because `ci-kg-verify` is a member of
`make ci`, the sprint-close gate had been red for a reason unrelated to the graph, and the
failure was silent in the worst way: exit 3 is neither a pass nor a reported defect.

```
$ go run ./cmd/kgverify
kgverify: reading symbol nodes: … exit status 127: Error: unknown graph subcommand: query
exit status 3
```

Pinned by `TestGraphQueryArgsNameTheClientSubcommand` and, against the live binary,
`TestRmpAcceptsTheGraphSubcommandWeUse`.

## Before and after

| Check | Before | After | Note |
|---|---:|---:|---|
| *(gate runnable at all)* | **no** | **yes** | exit 3 → exit 0 |
| symbol coverage of the tree | 43.1% | **95.7%** | 12 518 → 27 826 declarations with a node |
| `package-symbol-gap` | 97 | **0** | modelled packages at parity: 8 → **105 of 105** |
| `symbol-absent` | 327 | **0** | nodes naming a symbol the tree does not have |
| `symbol-file-absent` | 267 | 40 | residue points into two deleted packages |
| `symbol-pkg-mismatch` | 71 | 68 | the `internal/sim` keying divergence is gone |
| `task-id-duplicated` | 5 | **0** | five ids bound two nodes each |
| `task-stub` | 3 | **0** | empty stubs for 2698, 2699, 2700 |
| `task-status-disagrees-with-rmp` | 9 | **0** | every task re-derived from rmp |
| `provenance-no-node` | 1 712 | 136 | all residue in two unmodelled packages |
| `provenance-no-gitcommit` | 3 | **0** | |
| `edge-off-model` | 62 | 60 | |
| `edge-type-undocumented` | 119 | 118 | |
| `Task` nodes (total) | 495 | **2 672** | 2 660 sprint tasks + 12 backlog-only |
| Graph nodes / edges | 15 454 / 19 515 | **32 578** / 19 116 | |

Counts unchanged by this task, and still open: `symbol-kind-mismatch` 29,
`label-undocumented` 18, `component-path-missing` 25.

> **One row does not reconcile with the committed baseline, and it is recorded as
> unresolved rather than picked.** This table gives `provenance-no-node` an *After*
> of **136**, while `cmd/kgverify/baseline.json` — written in the same task, at the
> same commit — records **160** for that check. The two describe the same check at
> the same tree and cannot both be right. The `-exclude` rationale in the `Makefile`
> quotes a third figure, 1 082, for the count "at the time of writing". Which is
> correct cannot be settled after the fact, because the check's population is
> "declarations in files touched since the merge-base" and both the tree and the
> graph have moved since; re-measuring now answers a different question. **The
> gating number is the one in `baseline.json`**, since that is what the gate
> compares against. Treat 136 as unverified.
>
> **Read the whole table as a measurement of `0e7e982d`, not as a property of the
> branch.** Every figure in it moves with every commit that adds or deletes a
> declaration. Measured again at `efd32fb9`, the release tip — seven commits later,
> with no graph sync in between — the same gate reports symbol coverage **95.4 %**,
> **101** of 105 modelled packages at parity, `symbol-absent` **4**,
> `symbol-kind-mismatch` **30**, `package-symbol-gap` **4**, and
> `provenance-no-node` **257 of 3 137 declarations in 164 touched files**. The four
> `symbol-absent` entries are `Test` nodes naming functions that `e9cf9cb5` deleted
> (`TestHandshakeTimeoutDefaults`, `TestNewServer_DefaultsConnTimeout`,
> `TestNewServer_DefaultStatementTimeoutSecureByDefault`,
> `TestSession_AutocommitCarriesDefaultStmtTimeout`), confirmed absent with
> `git grep`. That is the gate working as designed: `symbol-absent` is
> zero-baselined precisely so a node outliving its declaration turns `make ci` red.

## The three findings, re-measured at HEAD

The task filed three findings from sprint 353. Two had moved and are restated here with the
number actually observed, because acting on a stale premise is how a repair becomes a defect.

**(a) Task coverage — the filed premise was stale, the gap was far larger.** The filing said
`Task` nodes stopped at id 2676 and that only #2698, #2699, #2700 and #2718 had been added by
hand. At HEAD the maximum id was **2777** and **14** of sprint 353's 55 tasks were present, so
the specific claim no longer held. The real figure was worse than the one filed: across every
sprint, **2 177 of 2 660** tasks had no node at all, and the graph's lowest task id was 1358,
so no sprint before ~#195 was modelled. All 2 660 now have a node carrying status, type,
sprint and — where closed — the closing commit.

**(b) `cypher/exec` — confirmed, and the counting method matters.** The filing measured 2 430
declarations against 1 141 nodes. At HEAD: **2 617 declarations against 1 149 nodes** by the
`pkg` property, or **1 091** counted through `(:Package)-[:CONTAINS]->()`, the shape the filing
used. The 58-node difference between those two readings is rmp **#2802** — 249 `Package` stubs
with a null `name` splitting containment — which is why the audit script keys on `pkg` and not
on `CONTAINS`. `cypher/exec` is now at parity: 2 617 declarations, 2 621 nodes, gap 0.

**(c) False claims — confirmed at 327, all removed.** 327 symbol-tier nodes named a
declaration absent from the tree. All 327 were deleted with the 399 edges hanging off them.

## Why a text search cannot do this job

The deletion oracle is `go/parser`, never a text scan, and the difference is measurable rather
than rhetorical. `BenchmarkBarrier_View` has six occurrences in `.go` files and **is not
declared**: all six are comments, one of which says the benchmark moved elsewhere. A `grep`
reports it present; the declaration inventory correctly reports it absent. Deleting a node on
the strength of a text scan would have kept a false claim; keying a *creation* on one is how
the two fabrications in the regression fixture were made.

## An incident, and what it cost

The first attempt at the package repair pass **overwrote `pkg` on 11 652 of 12 677 symbol
nodes** with a single package path, in one statement.

The cause was operator precedence, not the data. `symbolLabelPredicate` returned an
unbracketed `x:Benchmark OR … OR x:Type`, which every existing call site used as an entire
`WHERE` clause. The first caller to conjoin it with an identity test inherited the
reassociation — Cypher binds `AND` tighter than `OR`, so the predicate meant
`x:Benchmark OR … OR (x:Type AND x.name = d.n …)`, matching every node of the first six labels
regardless of the name test. Behind a `SET` driven by `UNWIND`, the last row's value landed on
all of them.

Recovery was complete for 12 657 of 12 677 nodes, because `pkg` is a **derived** property: a Go
symbol's package is fixed by its file's directory, and `file` was untouched. 12 511 file-bearing
nodes were rebuilt from their own `file`; 146 of the 166 file-less nodes were resolved against
the tree by name and receiver, which also restored the `file` they had been missing. The
remaining **20** — file-less, and with no declaration matching their name and receiver — had no
recoverable value, so the false `pkg` was removed rather than left standing. All 20 were among
the ~180 nodes that carried a null `pkg` before the incident, so this is very probably their
prior state; it is recorded as *probable* because it cannot be proven.

Three things now hold that did not before:

- `symbolLabelPredicate` brackets its chain, and `TestSymbolLabelPredicateBracketsItsOrChain`
  plus `TestRepairStatementConjoinsABracketedLabelGroup` fail if the bracket is removed. Both
  were confirmed red against a controlled revert of the fix.
- The invariant is written into `knowledge-model.md` as a query that must return 0, and it
  does: **no symbol node claims a `pkg` that contradicts its `file`'s directory.**
- Every batch write in this task was checked against a predicted counter before the next one
  ran. The three that mattered came out exact: 12 511 × 3 = 37 533 properties for the
  rebuild, 146 × 4 = 584 for the file-less repair, and 15 219 nodes created against a
  15 279-item worklist of which 60 had already been written by the trial statement.

## Scope boundary: what was deliberately left alone

**33 packages holding 1 245 declarations have no symbol node at all** and were not created.
rmp #2719 puts *modelling packages the graph does not already cover* out of scope, so the
parity check gates only the 105 packages that have made a claim. Read the two numbers
together: `package-symbol-gap = 0` says the graph's claims are **complete**, not that the graph
covers the module.

`cmd/kgverify` itself (112 declarations) is in that set, and it is the largest single
contributor to the remaining `provenance-no-node` count.

**rmp #2802 was not touched.** 249 `Package` nodes carry a null `name`/`importPath` and hold
234 edges between them. They name no symbol, so acceptance criterion 2 does not reach them,
and repairing them is a separate filed task in the backlog. The audit script reports them
(as unresolved package keys) and never writes to them.

Unresolved package keys still carried by symbol nodes:

| Key | Nodes | Meaning |
|---|---:|---|
| *(null)* | 8 | no `pkg`, no `file`, no matching declaration |
| `…/cypher/ir/rewrite` | 20 | package deleted in sprint 352 |
| `…/cypher/plan` | 15 | package deleted in sprint 352 |

## Per-package parity — the 105 packages the graph models

`Gap` is the number of declarations with no node, keyed on `(file, name, recv)`. `Nodes` may
exceed `Declarations` where a node exists for a declaration removed from the tree but still
named elsewhere in it.

| Package | Declarations | Nodes | Covered | Gap |
|---|---:|---:|---:|---:|
| `github.com/FlavioCFOliveira/GoGraph/internal/sim` | 4795 | 4795 | 4795 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher` | 4480 | 4483 | 4479 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/exec` | 2617 | 2621 | 2617 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen` | 1993 | 1994 | 1993 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/lpg` | 1496 | 1516 | 1496 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bolt/server` | 790 | 791 | 790 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/ir` | 735 | 735 | 735 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/snapshot` | 681 | 682 | 681 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/shapegen` | 653 | 653 | 653 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/search` | 625 | 629 | 625 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/expr` | 559 | 563 | 559 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/funcs` | 500 | 500 | 500 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/sema` | 444 | 444 | 444 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/recovery` | 405 | 405 | 405 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/txn` | 362 | 365 | 362 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/parser` | 355 | 355 | 355 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/adjlist` | 353 | 354 | 353 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/ast` | 343 | 343 | 343 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/wal` | 225 | 226 | 225 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/mvcc` | 214 | 216 | 214 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index/hash` | 192 | 192 | 192 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index/btree` | 187 | 189 | 187 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bolt/packstream` | 182 | 182 | 182 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/search/centrality` | 176 | 176 | 176 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bolt/proto` | 171 | 171 | 171 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/query` | 171 | 171 | 171 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/25_software_house_api` | 167 | 167 | 167 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/csrfile` | 167 | 167 | 167 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/csr` | 155 | 155 | 155 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/checkpoint` | 154 | 155 | 154 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/tck` | 149 | 149 | 149 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/24_social_network_cli` | 134 | 134 | 134 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/audit352` | 132 | 132 | 132 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index` | 130 | 131 | 130 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/comparison` | 115 | 115 | 115 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index/label` | 105 | 105 | 105 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph` | 102 | 102 | 102 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/io/graphml` | 100 | 101 | 100 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl` | 92 | 92 | 92 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/search/flow` | 90 | 95 | 90 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/soak` | 84 | 84 | 84 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/cypher_ldbc` | 82 | 82 | 82 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/27_concurrent_txn` | 76 | 76 | 76 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/io/csv` | 74 | 74 | 74 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index/count` | 72 | 72 | 72 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/search/community` | 70 | 70 | 70 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/explain` | 67 | 67 | 67 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus` | 66 | 66 | 66 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/mvccwrite` | 64 | 64 | 64 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/csrorder` | 59 | 59 | 59 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/crashinject` | 56 | 57 | 56 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cypher/procs` | 52 | 54 | 52 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store/bulk` | 51 | 51 | 51 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/04_persistence` | 49 | 50 | 49 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/14_routing_alternatives` | 47 | 49 | 47 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/sim` | 46 | 46 | 46 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/metrics` | 46 | 46 | 46 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/13_network_reliability` | 43 | 50 | 43 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/search/extern` | 42 | 42 | 42 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/17_transactional_log` | 41 | 41 | 41 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/testfs` | 40 | 40 | 40 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/20_concurrent_reads` | 39 | 39 | 39 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/22_cypher` | 39 | 41 | 39 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/invariants` | 39 | 39 | 39 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/11_social_network` | 38 | 39 | 38 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/19_pattern_query` | 38 | 39 | 38 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/r4audit` | 36 | 36 | 36 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/ds` | 34 | 34 | 34 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/02_property_graph` | 34 | 35 | 34 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/generation` | 34 | 34 | 34 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/tmphygiene` | 33 | 33 | 33 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/21_typed_recovery` | 32 | 33 | 32 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/16_centrality_analytics` | 31 | 32 | 31 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/01_basic` | 30 | 31 | 30 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/07_graphml_roundtrip` | 30 | 31 | 30 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/23_bolt_server` | 30 | 31 | 30 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema` | 30 | 30 | 30 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/03_advanced_algorithms` | 29 | 30 | 29 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/10_dimacs9_routing` | 28 | 28 | 28 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/08_pagerank` | 27 | 28 | 27 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/12_build_dependency` | 27 | 28 | 27 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/expandinto` | 26 | 26 | 26 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/18_oocore_pipeline` | 26 | 27 | 26 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/36_mvcc_snapshot_topology` | 26 | 26 | 26 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/ldbc` | 25 | 25 | 25 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/io/dot` | 25 | 25 | 25 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/goldens` | 24 | 24 | 24 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/store` | 24 | 24 | 24 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/06_csv_import` | 22 | 23 | 22 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/05_out_of_core` | 21 | 22 | 21 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/graph/io` | 21 | 21 | 21 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/testlayers` | 21 | 21 | 21 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/09_leiden` | 20 | 21 | 20 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/examples/15_task_assignment` | 19 | 20 | 19 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/cypherdocgate` | 19 | 19 | 19 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/cypher_alloc` | 18 | 18 | 18 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/stress` | 18 | 18 | 18 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/crashinject-helper` | 17 | 17 | 17 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/dimacs9` | 15 | 15 | 15 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/subproc` | 15 | 15 | 15 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/crashpoint` | 13 | 14 | 13 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/scenarios` | 12 | 12 | 12 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/bench/rmat` | 9 | 9 | 9 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/internal/sortseam` | 5 | 5 | 5 | 0 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/fmtfixture` | 4 | 4 | 4 | 0 |

## Packages the graph does not model (out of scope, reported for the next cycle)

| Package | Declarations |
|---|---:|
| `github.com/FlavioCFOliveira/GoGraph/bench/contention` | 190 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/kgverify` | 112 |
| `github.com/FlavioCFOliveira/GoGraph/examples/26_social_scale_bench` | 99 |
| `github.com/FlavioCFOliveira/GoGraph/graph/index/stats` | 85 |
| `github.com/FlavioCFOliveira/GoGraph/internal/isolationtest` | 85 |
| `github.com/FlavioCFOliveira/GoGraph/internal/anomaly` | 71 |
| `github.com/FlavioCFOliveira/GoGraph/internal/clock` | 51 |
| `github.com/FlavioCFOliveira/GoGraph/examples/31_metrics_observability` | 48 |
| `github.com/FlavioCFOliveira/GoGraph/store/bulkimport` | 45 |
| `github.com/FlavioCFOliveira/GoGraph/examples/28_negative_weights` | 36 |
| `github.com/FlavioCFOliveira/GoGraph/examples/29_all_pairs` | 32 |
| `github.com/FlavioCFOliveira/GoGraph/examples/30_min_spanning_tree` | 30 |
| `github.com/FlavioCFOliveira/GoGraph/examples/37_mvcc_write_contention` | 28 |
| `github.com/FlavioCFOliveira/GoGraph/bench/cyclicjoin` | 27 |
| `github.com/FlavioCFOliveira/GoGraph/internal/scriptgate` | 25 |
| `github.com/FlavioCFOliveira/GoGraph/bench/memprobe` | 24 |
| `github.com/FlavioCFOliveira/GoGraph/bench/entryheap` | 23 |
| `github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof` | 23 |
| `github.com/FlavioCFOliveira/GoGraph/internal/concurrencydoc` | 22 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/gograph-import` | 20 |
| `github.com/FlavioCFOliveira/GoGraph/examples/32_euler` | 19 |
| `github.com/FlavioCFOliveira/GoGraph/examples/34_bolt_transactions` | 18 |
| `github.com/FlavioCFOliveira/GoGraph/bench/boltwire` | 17 |
| `github.com/FlavioCFOliveira/GoGraph/bench/mtaudit` | 16 |
| `github.com/FlavioCFOliveira/GoGraph/examples/33_generation_swap` | 15 |
| `github.com/FlavioCFOliveira/GoGraph/examples/35_mvcc_mixed_workload` | 15 |
| `github.com/FlavioCFOliveira/GoGraph/cmd/sim-xrelease-helper` | 14 |
| `github.com/FlavioCFOliveira/GoGraph/internal/docscheck` | 13 |
| `github.com/FlavioCFOliveira/GoGraph/metrics` | 13 |
| `github.com/FlavioCFOliveira/GoGraph/bench/cypher_scale` | 11 |
| `github.com/FlavioCFOliveira/GoGraph/bench/cypher_boundkey` | 8 |
| `github.com/FlavioCFOliveira/GoGraph/internal/memlimit` | 7 |
| `github.com/FlavioCFOliveira/GoGraph/bench/comparison/ggserver` | 3 |

## Method notes for whoever re-runs this

- **The declaration key is `(file, name, recv)`, not `(file, name)`.** 531 pairs in this tree
  are two methods of the same name on different receivers in one file, and `func init()` may
  legitimately be declared twice in a file (`cypher/write_property_durability_test.go` does).
  Keyed on `(file, name)` the audit under-reports the gap by 1 680 declarations and a package
  can report parity it has not reached.
- **`MERGE` a method on its receiver too**, or the second of such a pair binds to the first
  and one node is written where two are owed.
- **Read the `counters` block on every write, and predict it first.** A `MATCH` that binds
  nothing returns exit 0 with no `counters` key, so silence is the default failure mode; and
  a counter far *larger* than predicted is how the incident above was caught within one
  statement.
- **Repair before create.** A node whose `pkg` is null or names a deleted package is invisible
  to its real package, so merging that declaration first creates a duplicate instead of
  matching it.
