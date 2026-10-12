# Design: plan-pure / execution-bound separation of the read build (rmp #2391)

Status: design only (SPIKE). No production code was changed. Sprint 367, residual Q7 of
[`campaign-lab-2026-10-09.md`](campaign-lab-2026-10-09.md) §13. Every number below was
collected at HEAD `138f7bc3`.

## 1. Residual at HEAD

Vehicle: `examples/35_mvcc_mixed_workload -nodes 3000 -readers 4 -phase-window 3s`, two
paired runs, `GOGRAPH_FSYNC_LATENCY=off`, Apple M4 (10 cores), Go 1.27.2, load1 2.5–4.0.
The readers execute the cached point query `MATCH (n:Account {id: $id}) RETURN n.balance AS b`
(plan: Projection → Selection(`n.id = $id`) → NodeByLabelScan; a btree index on `n.id`)
**7.81 M times** per run (object count of the scaffold, `api.go:2801`).

| site (cumulative unless "flat") | run 1 alloc | run 2 alloc | run 1 CPU | run 2 CPU | per execution |
|---|---|---|---|---|---|
| process total | 45.25 GiB | 45.24 GiB | 53.69 s | 53.67 s | — |
| reader goroutine (`measure.func3`) | 43.66 GiB | — | 12.49 s | 11.56 s | — |
| **`buildReadPhysical`** (`api.go:2776`) | **29.59 GiB (65.4%)** | 29.39 GiB | **7.84 s** | 7.21 s | **4.07 KB, 55.7 allocs, ~1.0 µs** |
| └ scaffold `var sc readBuildScaffold` (flat, `api.go:2801`) | 6.51 GiB | 6.53 GiB | 0.31 s | — | 800 B struct → 896 B class |
| └ `buildIRProjection` (`api.go:17513`) | 9.92 GiB | 9.88 GiB | 1.73 s | 1.60 s | 1.36 KB |
| └ `newRowPredicate` (`api.go:19538`) | 5.21 GiB | — | 0.84 s | 0.96 s | 0.72 KB |
| └ **`indexSeekWouldFire` probe** (`api.go:18392`, from `:18482`) | **3.44 GiB** | 3.39 GiB | **2.10 s** | 1.94 s | 473 B, 12.7 allocs |
| └ real seek build: hash attempts (`:11076`) + range seek (`:11150`) | 0.47 + 2.64 GiB | — | 0.77 + 0.95 s | — | — |
| `copySchema` (flat, `api.go:9366`, all callers) | 5.56 GiB | 5.52 GiB | 0.92 s | 0.68 s | 5.9 copies, 765 B |
| label-bitmap clone (`roaring.(*arrayContainer).clone`) | **0** | **0** | **0** | **0** | — |

**What changed since the earlier measurements.** The 2026-09-16 headline (bitmap clone 49.96%
via `ResolveLabelBitmap`) is gone: `82fa809e` (rmp #2863) shares the immutable index bitmap
(`graph/lpg/mvcc_index.go:431`, `BitmapShared`) and copies only when a correction runs
(`mvcc_index.go:443–455`). `ResolveLabelBitmap` costs 0.26 s and 0 B here. The non-cloning
read that AC 4 asks for therefore already exists; no further bitmap step is proposed.

**Today's residual, ranked:** (1) the throwaway seek probe, 2.0 s / 3.4 GiB — the largest
single CPU item of the build, 26% of it; (2) schema snapshots and expression-program
construction (`copySchema`, `newRowSchema`, `newRowBindPlan`, `newRowTypedPredicate`,
`projAliasScalarCols`), ~9.7 GiB / ~1.5 s; (3) the 896 B scaffold, 6.5 GiB; (4) per-execution
access-path resolution against the index catalog (failing hash attempts, `numericBTreeName`,
`ListIndexes`), ~1.0 s / ~1.2 GiB; (5) operator structs proper (`NewNodeByIndexRangeScan`
2.6 GiB including the probe's copy, `NewColumnarProject` 2.0 GiB, `NewFilter` 0.35 GiB).

## 2. Boundary

| class | part | code | why |
|---|---|---|---|
| **plan-pure** (already cached) | logical plan, `containsWrite`, `paramRefs`, reorder / anchor-swap candidates, `scalarUse`, `countVarRewrite`, `pushedSeekHints` | `planCacheEntry` `api.go:5543`; consumed `api.go:2842–2843`, `:2919` | memoised on the entry |
| **plan-pure** (rebuilt per execution) | Engine gate flags copied into `bopts` | `api.go:2848–2950` | constant per Engine |
| plan-pure **per gate vector** | column layout (`schema` map) and every snapshot of it | `copySchema` sites `api.go:17532`, `:18072`, `:19541`; 38 `copySchema` + 38 `schemaWidth` call sites in `cypher/*.go` | a function of the plan and of the swap decisions: `reorderSwap` changes the internal column layout (`api.go:2905–2911`) |
| plan-pure per gate vector | `rowBindPlan.vars` (name → column, bind kind) | `rowbind.go:397` | derived from the layout and the plan-built path/VLE meta maps; already resolved once behind `sync.Once` (`rowbind.go:191`) |
| plan-pure | `projAliasScalarCols` | `api.go:18183–18185` | item names and input-schema NAMES only |
| **catalog-bound** | access-path choice: hash seek, key-set seek, range seek, bound btree | `api.go:14614`, `:14752`, `:14775`; `range_seek_plan.go:845`; `index_binding.go:671` | depends on the index catalog (`graph/index/manager.go:461` CreateIndex, `:474` DropIndex, `build.go:424` FinishBuild); the catalog has **no epoch counter** today |
| **execution-bound** | snapshot and read view | `BeginRead` `api.go:2711`, `ReadAt` `:2789` | one instant per statement (Isolation) |
| execution-bound | index snapshot proof | `api.go:2875` (`indexSeekSnapshotOf`) | rmp #2937: proves the index describes this snapshot |
| execution-bound | frozen `now()` registry | `api.go:2667` | per-statement instant (openCypher) |
| execution-bound | parameter values: evaluator binding, seek bounds | `api.go:2818`, `:2824`; `range_seek_plan.go:907` | values differ per execution |
| execution-bound | live cost gates | `computeReorderSwaps` `api.go:2913`, `computeAnchorSwaps` `:2933`, parallel threshold vs live count | read live counts and parameter values (rmp #2766) |
| execution-bound | statement label-bitmap memo, `ctx` | `read_build_scaffold.go` `init` (memo, `queryCtx`) | per statement |
| execution-bound | operator iterator state | `exec.Operator` `cypher/exec/operator.go:25` (Init/Next/Close); 55 operator types in `cypher/exec` | every operator is a stateful cursor |

**Binding mechanism confirmed.** The tree is bound to one execution by capture: `rowBindPlan`
holds `g` and `bopts` (`rowbind.go:180–181`); `newRowPredicate` and `buildIRProjection`
close over `g`, `params`, `bopts` (43 `func(row exec.Row)` closures in `cypher/*.go`);
`bopts` holds `ctx` and the evaluators. Memgraph (`343f7fe3`, `operator.hpp:255`,
`interpreter.cpp:3642–3644`) and Neo4j (`eccd584a`, `InterpretedRuntime.scala:105–141`) avoid
this by passing per-execution state (frame/`QueryState`) to an immutable tree at cursor time.

## 3. Answers

**AC 3 — column layout at plan time.** Possible, but only per gate vector, because the swap
gates change the layout. The cheapest correct form does not change the `map[string]int`
type: memoise, on the `planCacheEntry`, the immutable products of each snapshot site (the
`rowSchema`, the resolved `boundVar` slice, `projAliasScalarCols`) keyed by plan-node
pointer, used only when the execution's gate vector is empty (no reorder or anchor swap
chosen) and falling back to today's per-execution build otherwise. Changing the schema type
itself (persistent/copy-on-write layout) would touch 64 `schema map[string]int` parameters and
45 in-place writes plus `restoreSchema` (`api.go:9390`) — not justified as a first step.

**AC 4 — immutable shared operator tree.** Feasible only as the Pipe/cursor split of both
references: every operator type separated into an immutable plan node plus a per-execution
cursor, and every row closure re-signed to receive an execution environment
(`rv`, `params`, `reg`, `ctx`, evaluators) instead of capturing it. That is an architecture
change across `cypher/exec` (55 types) and the builder; it needs the user's agreement and is
not the smallest slice. The smallest slice measurable on its own is the **expression program**
of Filter and Projection (step S3): cache the plan-pure half, bind the execution half in one
small per-execution struct.

## 4. Step sequence

Every step is independently revertable. Wins are measured upper bounds of the site's own cost
on this vehicle (profile attribution, **not** A/B); each step must be confirmed by paired,
interleaved runs of example 35 plus `go test -bench` with `benchstat`.

| step | change | expected win vs HEAD residual (per execution) | risk (TCK / ACID) | files |
|---|---|---|---|---|
| **S1** | Stop building the seek twice. When `indexSeekWouldFire` builds an access path, hand the operator and the schema entries it wrote to the serial Selection build (an execution-scoped one-slot handoff on `bopts`, keyed by `*ir.Selection`), so `buildOperatorRec` (`api.go:11076`, `:11150`) consumes it instead of rebuilding; the probe is still defined by the real rewrites (`api.go:18379–18390` rationale kept). | ≤ 2.0 s / 3.4 GiB here; ≤ 473 B, 12.7 allocs, ~0.26 µs per execution of an indexed point read (~26% of build CPU) | low: same operator, built once; nothing shared across executions. Must assert the handed-off schema delta equals what the serial path would write | `cypher/api.go` (`indexSeekWouldFire`, `tryBuildColumnarFilterChain`, `buildOperatorRec` Selection case; also the second caller `:18878`) |
| **S2** | Memoise the expression-program products per plan node on `planCacheEntry` (S3's precondition and AC 3): `rowSchema` snapshots, resolved `boundVar`s, `projAliasScalarCols`; only for an empty gate vector; published once (`sync.Once`/`atomic.Pointer`) and never written after publication. | ≤ ~1.3 KB, ~6–8 allocs, ~1.5 s / ~9.7 GiB here (`copySchema` 5.6, `projAliasScalarCols` 2.0, `newRowTypedPredicate` 1.7, `newRowSchema` 0.35 GiB) | medium. TCK: layout must equal the per-execution one (differential test over the TCK corpus, memo on vs off). Isolation: shared objects must be read-only — `-race` test with 64 concurrent executions of one entry | `cypher/api.go` (`buildIRProjection`, `newRowPredicate`), `cypher/rowbind.go` (split `vars` from `g`/`bopts`), `planCacheEntry` |
| S3 | Split `rowBindPlan` / predicate / projection closures into a cached program plus a per-execution binding (`g`, `params`, `bopts`) in one small struct per operator | the closure and `rowBindPlan` allocations left after S2 (`newRowBindPlan` 2.1 GiB, closures ~1.0 GiB) | medium; same tests as S2 | `cypher/rowbind.go`, `cypher/api.go` |
| S4 | Access-path memo across executions: add an index-catalog epoch (bumped by CreateIndex, DropIndex, FinishBuild) and memoise the decision "which access path, which index" per Selection and parameter kind; bound values, `idxSnap`, and the #2814 pending-delta check stay per execution | ~1.0 s / ~1.2 GiB here (failing hash attempts, `numericBTreeName`, `ListIndexes`) | **medium-high**: a stale epoch read seeks a dropped index and can LOSE rows (Consistency/Isolation); the epoch must be read under the same discipline as the snapshot proof | `graph/index/manager.go`, `graph/index/build.go`, `cypher/api.go`, `cypher/range_seek_plan.go` |
| S5 | Shrink the scaffold below the 640 B class by moving the Engine-constant gate fields of `buildOpts` (392 B) behind one shared pointer | ≤ 256 B per execution (~2 GiB here); hypothesis — the fields movable without touching closure reads are not yet counted | low | `cypher/api.go` (`buildOpts`), `cypher/read_build_scaffold.go` |
| S6 | Full Pipe/cursor split (shared immutable operator tree) | the remainder (operator structs ~5 GiB here, scaffold) | high; **architecture change — user decision** | `cypher/exec/*` (55 types), builder |

## 5. Recommendation

Implement **S1** in this sprint: largest CPU item of today's residual, smallest change, no
cross-execution sharing, so no Isolation exposure. Then **S2** if the sprint has room: it is the
AC 3 answer and the precondition for S3. S4 and S6 go to the user first (S4 for its Consistency
exposure, S6 as an architecture change).

## 6. Limits and reproduction

- One vehicle, one query shape (indexed point read). Shapes with swap gates fire take the S2
  fallback and gain nothing from it; their share is not measured here.
- Wins are profile-attributed upper bounds, not A/B results. Two runs; run-to-run spread of
  the cited sites is under 4% in bytes and under 10% in CPU.
- The scaffold size (800 B) and its parts (`buildOpts` 392 B, `patternEvaluator` 160 B,
  `subqueryEvaluator` 104 B, `rowBindPlan` 112 B) were read with a throwaway `unsafe.Sizeof`
  test, deleted after the run.
- `Result.materialize` (4.2 GiB) and `newResultWithLimit` (3.0 GiB) are outside the build and
  outside this design.

```bash
go build -o $S/bin/ex35 ./examples/35_mvcc_mixed_workload
GOGRAPH_FSYNC_LATENCY=off TMPDIR=/Volumes/GoGraphRAM/<sub> $S/bin/ex35 -nodes 3000 -readers 4 \
  -phase-window 3s -profile-dir $S/run1        # repeat as run2
go tool pprof -nodefraction=0 -sample_index=alloc_space -top -cum $S/run1/heap.pprof
go tool pprof -nodefraction=0 -peek 'cypher.indexSeekWouldFire$|cypher.buildOperatorRec$' $S/run1/cpu.pprof
go tool pprof -nodefraction=0 -sample_index=alloc_space -list 'cypher.buildIRProjection$' $S/run1/heap.pprof
```

`$S` = `/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/755b6bf2-7deb-4d58-a983-07059400361e/scratchpad/unit2391`.
