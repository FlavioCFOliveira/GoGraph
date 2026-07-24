# Broadening Automatic Intra-Query Parallelism — Design (P6)

Status: design spike (#2111 / #2112 / #2113). No production code is changed by
this document. Specialist inputs: concurrency-architect (admissibility
boundary, deterministic combines, worker/governor contract, proof
obligations); to be complemented by go-developer (idiomatic implementation) and
graph-theory-expert (Expand fan-out correctness) at implementation time. Date
2026-07-24.

This document extends, and must be read alongside:

- `docs/count-store-design.md` and `docs/reordering-design.md` — the
  `SuppressReorder` order-safety predicate reused here.
- The shipped parallel infrastructure it builds on:
  `cypher/exec/parallel_governor.go` (#1705),
  `cypher/exec/parallel_aggregate.go` (#1672),
  `cypher/exec/parallel_scan_project.go` (#1682, #1830),
  `cypher/exec/label_count_scan.go` (#2004).

## 0. Executive recommendation (decisive)

Under the two hard mandates — **determinism** (the deterministic-simulation-
testing battery) and **no-regression** — plus openCypher TCK conformance, the
admissible broadening of automatic intra-query parallelism is:

- **Parallel aggregation (#2111): count + min + max + their `GROUP BY` forms.**
  count is shipped; min/max are added with a **position-carrying combine** that
  is **byte-identical to the serial left-fold, including the tie
  representation**. Group-by merges per-worker group→partial maps
  deterministically per key using the **exact grouping-key comparator**.
- **float `sum`/`avg`, int `sum`, `collect`, `percentileCont`/`percentileDisc`,
  `stdev`/`stdevp` stay SERIAL, permanently** — no deterministic, byte-identical
  combine exists (float non-associativity, order-sensitive integer overflow,
  buffering/order-as-value). See §2.
- **Parallel Expand (#2112) is bench-first.** Its first step is an empirical
  measurement of the fan-out potential on an Expand-heavy workload at high
  concurrency. Implement the operator this cycle only if the win clears a clear
  bar; otherwise defer to the backlog **with the measured evidence** and ship P6
  as parallel-aggregation-only. Either outcome is acceptable and honest.
- **#2066 count-pushdown (#2113):** the serial branch of bare group-by-less
  `count(*)`/`count(v)` over a full node scan reads the maintained live count
  (`LiveOrder()`) in O(1), instead of a full O(N) scan — consistent with the
  parallel count fast path and with `LabelCountScan` (#2004).

The worker model reuses `ParallelGovernor` (no new `N×GOMAXPROCS` spawn), the
`#1830`/`#1841` result byte-budget, and the goleak-clean, context-cancellable
join-inside-the-visibility-barrier lifecycle already proven by the shipped
leaves. Activation reuses `parallelScanThreshold` (default 50 000 live nodes);
small graphs stay serial by construction, so no-regression holds without a
per-query check.

## 1. The determinism obligation, stated precisely

This is the single most important framing correction in the spike, and it is
grounded by reading the DST differential harness itself
(`internal/sim/differential.go`), not assumed.

**The DST differential compares a SORTED MULTISET of STRINGIFIED rows, not raw
emission order.** `canonicalRows` renders each row and calls
`sort.Strings(rows)`; `renderRow` renders each column via `ValueAt(i).String()`.
Queries with an unordered `LIMIT` (`plannerNondeterministicRows`: contains
`LIMIT`, no `ORDER BY`) are compared **count-only**, because openCypher leaves
the surviving subset unspecified.

Two consequences define the admissibility contract for every parallel operator:

1. **Result invariance (the DST / differential guarantee).** The operator's
   result **multiset**, *including each value's representation* (the exact
   string `ValueAt(i).String()` produces), must be invariant under any
   partition boundary, worker count, or scheduling order. This — not emission
   order — is what the differential compares.
2. **Order-safety (the openCypher / TCK guarantee).** Wherever a downstream
   operator turns row order into an *observable result* — a `collect()` (whose
   list element order is a compared value), a non-total `Sort`/`Top` tie-group,
   a bare `LIMIT`/`SKIP` (which rows survive), a positional read — the parallel
   operator's emission order becomes observable and must be **suppressed to the
   serial path**. This is exactly the `SuppressReorder(spine)` predicate from
   the reordering work (`cypher/reorder_order_safety.go`).

The two are distinct and must not be conflated. Emission-order non-determinism
that no operator observes is *conformant and DST-safe* (the differential sorts
it away); it becomes a defect only when an order-observer converts it into a
value or a subset. This is why the shipped `ParallelScanProject` is DST-safe
despite concatenating worker buffers in scheduler-dependent order (§6), and why
parallel Expand — which sits mid-plan under arbitrary consumers — cannot rely on
that and must carry the `SuppressReorder` gate (§4).

## 2. Admissible vs inadmissible aggregation — the boundary, with proof

### 2.1 Admissible (a deterministic, byte-identical combine exists)

| Aggregate | Combine | Determinism justification |
|---|---|---|
| **`count(*)` / `count(v)`** (shipped, #1672) | int64 addition of per-worker partials | int64 addition is associative and commutative (even under two's-complement wraparound); `count`'s NULL-skip is a per-value predicate independent of partition order. The combined total equals the serial single-counter result under **any** partition. |
| **`min` / `max`** (new, position-carrying — §3) | `Compare`-semilattice fold, ties broken by lowest scan index | `Compare` is a total order for sorting; `min`/`max` under it is **associative, commutative, and idempotent** (a semilattice), so the *equivalence class* of the result is partition-invariant. The chosen *representative* among `Compare`-ties is pinned deterministically to the serial one (§3). |
| **`GROUP BY` count / min / max** (new — §3.3) | per-key: the same combines, over merged partial maps keyed by the exact grouping comparator | Group identity uses the serial grouping comparator (not the hash alone); per-key combine is the associative/semilattice combine above. The `(key, value)` **multiset** is partition-invariant. |

### 2.2 Inadmissible (stay serial — no byte-identical combine)

| Aggregate | Why it is inadmissible under §1.1 (not merely under openCypher) |
|---|---|
| **float `sum` / `avg`** | IEEE-754 addition is **non-associative**. A partitioned summation differs from the serial left-fold by the last ULP(s), so the result *value* — and therefore its `String()` — changes with partition boundaries. Conformant to openCypher, but the differential's stringified-value comparison **diverges**. |
| **int `sum`** | `SumAgg.Step` carries an **order-sensitive overflow guard** (`ArithmeticOverflow` fires from the running prefix). Whether it fires depends on the partition boundaries: two partitions can each stay in int64 range while the serial prefix overflows, or vice-versa — an error-vs-value divergence. |
| **`collect`** | The list's **element order is the arrival order**, and the list is a *value*, compared by the differential inside its single row. Schedule-dependent → divergent. Also the value-trap `SuppressReorder` guards against. |
| **`stdev` / `stdevp`** | Welford state (`n`, `mean`, `M2`) admits a mathematically correct parallel merge (Chan–Golub–LeVeque), but the merge is **non-associative in float** — the same last-ULP divergence as `sum`/`avg`. |
| **`percentileCont` / `percentileDisc`** | Buffer-then-sort; requires full materialisation and is order-dependent. No streaming combine. |

### 2.3 The interface decision — no `Combine` on `funcs.Aggregator`

`funcs.Aggregator` deliberately exposes only `Init`/`Step`/`Result` — **no
`Combine`** (`cypher/funcs/aggregators.go`). That omission is a **structural
safety property**: it makes it impossible for a future float aggregate to reach
a parallel merge by accident.

**Adding `Combine` to the interface is rejected.** It would force the
*inadmissible* aggregators (float `sum`/`avg`, `collect`, `stdev`, percentile)
to either implement an unsafe merge — defeating the safety property — or return
"not combinable" at runtime — replacing a compile-time **structural** guarantee
with a checked one that can regress silently.

**Chosen design (mirrors the shipped #1672 / #2004 kernel pattern):**

- Keep `funcs.Aggregator` untouched, and **reuse it for per-worker
  accumulation**. Each worker owns private `funcs.CountAgg` / `MinAgg` / `MaxAgg`
  instances (already documented single-goroutine-safe, one per group per
  column). `Step` does not change.
- Add the parallel merge as a **closed, plan-time-selected combine in package
  `exec`**, admitted *only* for `{count, min, max}` and their group-by forms. An
  aggregate with no admitted combine has no parallel path and is structurally
  serial.
- Physical operator: a single **`ParallelAggregateScan`** leaf that generalises
  `ParallelCountScan`, parameterised by an optional `groupKeyEval` (nil ⇒ global
  aggregate) and a slice of `(argEval, reducerKind ∈ {countStar, count, min,
  max})`, plus the shared `ParallelGovernor` and `WithResultBudget`.

This preserves the no-`Combine` structural safety, reuses every line of the
existing `Step` logic, and keeps the parallel-merge surface minimal and closed.

## 3. The deterministic min/max combine (locked: position-carrying)

`MinAgg`/`MaxAgg` keep the **first-seen** value among `Compare`-ties (the update
is strict `<`/`>`; `cypher/funcs/aggregators.go`). The retained *representation*
is therefore arrival-order-dependent, and it is a compared value:

- `Compare(IntegerValue(1), FloatValue(1.0)) == 0` — the Number tier compares by
  magnitude (`compareNumericCross` → `cmpInt64Float64`), so `1` and `1.0` tie
  yet stringify differently (`"1"` vs `"1.0"`).
- `-0.0` and `+0.0` tie (`cmpFloat64` returns 0) yet can stringify differently.
- Distinct NaN payloads tie.

A value-only parallel combine would therefore be a **false green**: it passes
only when the trace/data never places a mixed-type (or degenerate-float)
representation at the extremum, and diverges the moment it does. The mandate
forbids relying on the trace generator's property distribution.

**Locked decision (Q1): position-carrying combine — byte-identical to serial,
including the tie representation.**

Per-worker partial for `min(e)`: `(pmin expr.Value, pos int64)`, where `pos` is
the **global scan index** of `pmin` (the morsel's base offset plus the local
offset). `splitMorsels` is extended to carry each morsel's base offset. The
worker's update rule for a value `v` at global index `idx`:

```
if pmin == nil ||
   Compare(v, pmin) < 0 ||
   (Compare(v, pmin) == 0 && idx < pos):
       pmin, pos = v, idx
```

The combine folds all partials by `(Compare, then lower pos)`. `max` is
symmetric (`Compare(...) > 0`, same `idx < pos` tie-break).

**Proof of byte-identity to serial.** Morsels are **contiguous** slices of the
deterministically collected `nodeIDs` (the `WalkNodeIDs` order, stable under the
visibility barrier), so `pos` is exactly the serial scan order. The `idx < pos`
tie-break makes each worker's representative the lowest-global-index
`Compare`-minimal among *its* morsels, **independent of the order it dequeued
them**; the fold's `lower pos` tie-break makes the global representative the
lowest-global-index `Compare`-minimal overall. That is precisely the value the
serial `MinAgg` keeps (first-seen in scan order). The result is a pure function
of the set `{(value, index)}`; it is independent of worker count and schedule,
and it is byte-identical to serial for **all** tie cases (mixed int/float,
±0.0, NaN). No change is made to the serial aggregator or its output.

(A canonical value tie-break applied in both serial and parallel would avoid
carrying positions but would *change* today's serial min/max representation — a
small but real TCK/observable risk. It is rejected: position-carrying is the
zero-behaviour-change option, which is the requirement.)

### 3.3 Group-by merge

- **Key identity uses the exact serial grouping comparator, not the hash
  alone.** The engine's grouping buckets on the float64 domain and resolves
  collisions with a comparator; for example int `2^53` and float `2^53` fall in
  the same bucket but are **separate groups**. Because each worker builds its
  partial with the *serial* grouping logic, partials are keyed identically and
  the merge combines same-identity groups. The `(key, aggregated-value)`
  **multiset is partition-invariant.**
- **Bounded resources (mandatory, not optional).** N workers each holding up to
  `DefaultMaxGroups` partial groups gives an **N×MaxGroups** peak before the
  merge. Enforce the group-count cap on the *merged* map **and** bound
  per-worker partials (fail-fast at `MaxGroups` per worker), so peak memory
  stays bounded — the group-by analogue of `WithResultBudget`.
- **Emission order** of the merged groups is unspecified absent `ORDER BY`;
  gate it exactly as §4 gates Expand (`SuppressReorder`), and otherwise emit in
  a deterministic key order so a bare-`LIMIT`-over-group-by remains reproducible
  run-to-run.

## 4. Parallel Expand (#2112) — bench-first, then implement or defer

**Step 1 (empirical gate — do this first).** Measure the parallel-Expand
potential before building the operator. Graph traversal is a pointer-chase; the
fan-out may be memory-latency-bound rather than CPU-bound, in which case
morsel-parallelism yields little net of the governor budget and the
`SuppressReorder`-gated applicability. On an Expand-heavy query at multiple
concurrency levels (1, 8, 64, 256 goroutines) and multiple bound-set sizes,
compare serial vs a morsel-parallel prototype fan-out with `benchstat`
(`-count`, interleaved — see §7). Decision rule:

- **Clears a clear throughput/latency bar** ⇒ implement the operator this cycle
  (steps below).
- **Marginal** ⇒ defer #2112 to the backlog **with the measured evidence**
  attached, and ship P6 as parallel-aggregation-only.

If implemented, the operator's contract is:

**(a) Multiset determinism.** Partition the **bound source set** into contiguous
morsels; each worker expands its sources into a private buffer; concatenate. The
result is a **bag** — the union of per-source expansions is partition-invariant
as a multiset, because each source's edge set is expanded identically regardless
of which worker owns it. Reuse the exact `advanceFwdEdge` / `advanceRevEdge`
decision helpers (`cypher/exec/expand.go`) so DirOut/DirIn/DirBoth, the
edge-type filter, and edge multiplicity are per-morsel identical to serial.

**(b) Order-safety suppression — required.** Build parallel Expand only when
`SuppressReorder(spine) == false` at the Expand's point in the logical plan.
Unlike `ParallelScanProject` (§6), Expand sits mid-plan under arbitrary
consumers; its schedule-dependent emission order can flow into a downstream
`collect` (list element order — a compared value), a non-total sort, a bare
`LIMIT`/`SKIP`, or a positional read. Where suppressed, fall back to serial
Expand (columnar or row). This is the fail-safe default: "when in doubt keep the
deterministic plan — never wrong, only slower."

**(c) Cyphermorphism / uniqueness / multigraph per-instance typing.** Preserved
because each worker drives the same `advance*Edge` helpers over its own sources;
relationship-uniqueness is **per source row (per path)**, never cross-source, so
morsel partitioning cannot violate it, and per-instance reverse-hop typing is
already inside those helpers. Implementation must **verify no cyphermorphism
state is shared across source rows** (any global "seen relationships" set would
need per-worker isolation).

**(d) Reuse the #2106 two-level cursor.** The columnar Expand cursor
(`cypher/exec/expand.go`) already fans a *batch of source rows* (`cRow`) out
into edges with a persistent two-level cursor. Parallel Expand gives each worker
a disjoint contiguous slice of the source batch and runs the existing
`fillChunk` per worker; the cursor is per-operator-instance, so per-worker
instances are naturally isolated. Parallelise the **outer** level (source-row
partition) only; keep the **inner** level (per-source edge fan-out) exactly as
is. No new traversal logic.

**(e) Activation** is keyed on the **bound-set size** (source rows), not total
live nodes — an Expand over a small bound set must stay serial even in a large
graph.

## 5. Worker / governor / cancellation / byte-budget contract (shared, reused)

Every new leaf reuses the shipped contract verbatim — there is no new spawn
model:

- **Bounded pool.** `nWorkers = ParallelGovernor.Enter(len(morsels))` yields
  ≈ `GOMAXPROCS / leaves-in-flight`, clamped to `[1, morsels]`, so N concurrent
  parallel queries do not each spawn `GOMAXPROCS` workers (the `N×GOMAXPROCS`
  oversubscription that regressed large-graph scans past 4 cores under
  concurrent load — 2026-06-24 audit). `Leave()` is called in `Close`, guarded
  by an `entered` flag so it runs exactly once per successful `Enter`.
- **No goroutine leak.** A pre-filled bounded work channel (capacity == morsel
  count) is closed before any worker starts, so no send blocks. Workers exit on
  channel drain, sub-plan error, or `ctx` cancellation; `Close` cancels then
  `wg.Wait`s. Verified with `go.uber.org/goleak` in test teardown.
- **Join inside the visibility barrier.** The join (`wg.Wait`) and the combine
  run on the goroutine that drives `Next`, which the engine drives inside the
  graph's visibility-barrier RLock (`lpg.Graph.View`), so no worker goroutine
  outlives the barrier. The happens-before edge (worker return → `wg.Done` →
  `wg.Wait`) makes the combine race-free with no additional synchronisation.
- **Result byte-budget (#1830 / #1841).** Thread `WithResultBudget(maxRows,
  maxBytes, estimateRow)` into every new leaf; workers check `overResultBudget`
  via shared atomics and stop accumulating at ≈ the budget plus one in-flight
  batch per worker. The drain layer then produces the canonical
  `ErrResultRowsExceeded` / `ErrResultBytesExceeded` from the bounded prefix,
  exactly as on the serial path. For group-by, add the per-worker group-count
  bound of §3.3. Bounded memory under parallelism is preserved.
- **Activation.** Reuse `useParallelScan`: live LPG walker only, live count
  strictly `> parallelScanThreshold` (default `DefaultParallelScanThreshold =
  50 000`). Small graphs stay serial ⇒ no-regression by construction.
- **Context cancellation** is honoured between morsels (workers check
  `ctx.Err()`), inside sub-plan `Next` loops, and before the join in `Next`.

## 6. #2066 count-pushdown on the serial branch (#2113)

**Gap (confirmed by reading the code).** `tryBuildParallelCountScan` declines
below the threshold (`useParallelScan == false`), and the serial
`EagerAggregation`-over-`AllNodesScan` path then performs a **full O(N) scan**
just to count. `LabelCountScan` (#2004) already closed this for single-label
scans; there is **no `AllNodesScan` equivalent**.

**Fix.** Add a serial `AllNodesCountScan` leaf (mirror of `LabelCountScan`) that
reads `LiveOrder()` in O(1), and route bare group-by-less `count(*)` /
`count(v)` over a bare `AllNodesScan` to it on the serial branch (it also
becomes the below-threshold fallback of `tryBuildParallelCountScan`). It is
bit-identical to the serial scan because `WalkNodeIDs` skips tombstones, so the
number of rows a bare `AllNodesScan` emits equals `LiveOrder()`. Reuse
`installCountAggSchema` for the identical downstream schema.

**Second-order consequence (record it).** After #2113, the *bare* shape that
`ParallelCountScan` served is subsumed by the O(1) serial read, so parallel
count adds essentially nothing for the bare shape. The genuinely valuable
"parallel count" is `count` over a **filtered** scan (`count(*) WHERE n.x > k`),
which `ParallelCountScan` does *not* serve today (it declines on any
`Selection`). Fold a count sink into `ParallelScanProject`'s filtered path
rather than growing `ParallelCountScan` — that is where #2111's count
parallelism actually pays.

Note also the latent hardening opportunity in §4(b): `ParallelScanProject`
currently has no `SuppressReorder` gate; it is order-safe only because its
`Projection(scalar) → [Selection] → AllNodesScan` shape structurally excludes
order-observing consumers. #2112 makes the predicate available; adding the gate
to `ParallelScanProject` is small, in-scope hardening.

## 7. Determinism proof obligation + adversarial tests (per parallelisation)

This is a spike; the following are proof obligations the implementation must
discharge, not assumptions:

1. **Baseline + scaling.** `go test -bench -benchmem -count=10`, serial vs
   parallel, at representative live counts (10k / 50k / 200k / 1M), compared
   with `benchstat`. Reject any variant slower than serial at or below the
   threshold. Bench min/max over a **property** (real per-row evaluation — this
   parallelises) as well as count (near-free — likely marginal, which reinforces
   §6's conclusion that the payoff is in the filtered count).
2. **Determinism — adversarial, correctness-by-construction:**
   - **Worker-count sweep** `1 .. 2×GOMAXPROCS` — the result multiset and value
     representations must be invariant.
   - **Morsel-size / partition-boundary sweep** — invariance across chunk
     boundaries.
   - **DST differential.** Extend `internal/sim/differential.go` with a
     `ParallelAggregateVariantPair` (`{ParallelScanThreshold:1}` vs
     `{DisableParallelScan:true}`) alongside the existing
     `ParallelScanVariantPair`. The trace generator **must be forced to place a
     mixed int/float extremum tie** (and ±0.0 / NaN) at the min/max, otherwise a
     value-only combine passes by luck. Same-seed reproduction must be exact.
   - **Expand (if implemented):** traces with a downstream `collect`, bare
     `LIMIT`, and non-total sort must assert the plan **chose serial** (via a
     build-count seam like `parallelScanProjectBuildCount`), proving the
     `SuppressReorder` gate fired.
3. **`go test -race ./...`** and **`goleak`** teardown on every new leaf.
4. **`pprof` mutex / block profiles** to confirm the merge introduces no
   contention (the merge is single-goroutine post-join, so this should be clean;
   verify).

Empirical hygiene (from prior parallel-scaling work): run a background `pgrep`
sampler during any scaling sweep and certify only clean runs — a concurrent
foreign benchmark pinning a core silently corrupts a parallel-scaling sweep; and
measure serial vs parallel **interleaved in one `-count` run**, never as two
before/after files, because thermal drift on a fanless machine confounds a
two-file comparison.

## 8. Sub-tasks

**#2111 — Parallel aggregation (ship this cycle).**

- 2111.1 `exec.ParallelAggregateScan` leaf generalising `ParallelCountScan`;
  reuse `ParallelGovernor` + `WithResultBudget`; goleak- and `-race`-clean.
- 2111.2 Deterministic combines: count (int64 add); **min/max position-carrying
  semilattice** (extend `splitMorsels` to carry the morsel base offset). No
  `Combine` on `funcs.Aggregator` — the plan-selected combine lives in `exec`.
- 2111.3 Group-by partial-map merge under the **exact serial grouping
  comparator**; per-worker group-count bound + merged-map cap (bounded memory).
- 2111.4 Planner recognizer: admit only `{count, min, max}` global + group-by;
  every other aggregate stays serial (structural, no runtime opt-out).
- 2111.5 DST `ParallelAggregateVariantPair` + adversarial mixed int/float, ±0.0,
  NaN extremum traces; worker-count and partition-boundary sweeps; `benchstat`
  no-regression gate.

**#2112 — Parallel Expand (bench-first; implement or defer).**

- 2112.1 **(step 1, empirical gate)** Bench parallel-Expand potential on an
  Expand-heavy query at concurrency 1/8/64/256 and multiple bound-set sizes;
  decide implement-vs-defer against a clear bar; record the evidence either way.
- 2112.2 Partition the bound source set into contiguous morsels; per-worker
  expansion reusing `advance*Edge` + the **#2106 two-level cursor**
  (parallelise the outer level only).
- 2112.3 `SuppressReorder(spine) == false` gate with serial fallback; activation
  keyed on bound-set size.
- 2112.4 Verify cyphermorphism / uniqueness / multigraph per-instance typing are
  per-source-row (no shared "seen" state); governor + byte-budget + goleak +
  `-race`.
- 2112.5 DST traces with downstream `collect` / `LIMIT` / non-total-sort assert
  serial was chosen; multiset-invariance sweeps.
- 2112.6 (in-scope hardening) add the same `SuppressReorder` gate to
  `ParallelScanProject`.

**#2113 — #2066 count-pushdown on the serial branch (ship this cycle).**

- 2113.1 `exec.AllNodesCountScan` reading `LiveOrder()` (mirror
  `LabelCountScan`).
- 2113.2 Route bare group-by-less `count(*)` / `count(v)` over a bare
  `AllNodesScan` to it on the serial branch and as the below-threshold fallback;
  reuse `installCountAggSchema`.
- 2113.3 Fold a count sink into `ParallelScanProject`'s **filtered** path (the
  real parallel-count value after #2066).
- 2113.4 Bit-identical tests on tombstoned graphs (`LiveOrder()` vs scan count)
  + DST.

## 9. Correctness risks to watch during implementation

- **min/max false green.** A value-only combine passes the DST differential
  whenever the data has no mixed-type extremum tie; position-carrying is
  mandatory, and the adversarial trace of §7.2 is what proves it.
- **Group-by memory amplification.** N×MaxGroups peak before merge; the
  per-worker bound of §3.3 is required, not optional.
- **Expand shared state.** Any cross-source cyphermorphism/"seen" set must be
  per-worker; verify before parallelising the fan-out.
- **Emission-order gaps.** `ParallelScanProject`'s order-safety is currently
  incidental (shape-based); parallel Expand and parallel group-by must gate on
  `SuppressReorder` explicitly.
- **Threshold coverage.** Activation must key Expand on bound-set size, not
  total nodes, or a small traversal in a large graph regresses.
