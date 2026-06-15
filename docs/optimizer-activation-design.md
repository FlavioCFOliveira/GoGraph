# Optimizer Activation Design Spike

Status: design spike — **no production code changed** by this document.
rmp task: #1504 (sprint S-PA3 #190). Downstream increments: #1505 (range index seeks), #1506 (hash join).
Date: 2026-06-15.

> **Implementation note (v0.3.1).** The two downstream increments this spike
> scoped have **shipped**: a physical **hash join** for disconnected equi-join
> patterns (`exec.HashJoin`, #1506) and a **range-predicate B+tree index seek**
> (`NodeByIndexRangeScan`, #1505), both as plan-build physical substitutions
> proven against differential tests to return an identical result multiset. The
> logical `cypher/ir/rewrite` Driver remains **unwired** (the guard test stays
> green). Statements below such as "no hash join exists" describe the
> pre-implementation premise this spike was written against; see
> [docs/benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) rows 0013
> (#1506) and 0014 (#1505) for the measured outcome.

This document evaluates whether — and how — the dormant cost-based optimizer in
`cypher/ir/rewrite/` and `cypher/plan/` can be safely activated in the GoGraph
engine without regressing either compliance mandate (100% openCypher TCK at
3897/3897 scenarios; 100% ACID) and without ever choosing a plan slower than
today's default.

The guard test `cypher/rewrite_not_wired_test.go`
(`TestCypherEngine_RewritePackageNotWired`) remains green: this spike wires
nothing.

---

## 0. Executive recommendation (decisive)

**Full activation is NOT safe now. The physical-operator-selection subset is the
only safe path, and even that must be gated behind a statistics-trustworthiness
veto and conservative safety margins.**

Concretely:

- **Do NOT** wire the logical rewrite `Driver` (`cypher/ir/rewrite/`) into the
  production engine. Predicate pushdown, eager insertion, projection pushdown and
  fusion all carry real or latent result-changing hazards under the current
  string-opaque IR, and several duplicate or conflict with logic the production
  engine already performs ad hoc. Keep them as the documented experimental
  embedder API they are today.
- **DO** pursue physical operator selection only, one operator at a time, each
  gated so that — given today's crude estimator — the planner is **provably
  inert** (it can only ever reproduce today's default plan) and only *earns* the
  right to deviate per-query once a real, statistics-derived estimate exists.
- **Ship order (revised by graph-theory-expert input):**
  1. **Hash join for equi-join predicates across disconnected patterns** (#1506)
     — the only change that is *asymptotically* strictly better
     (`O(|A|+|B|)` vs `O(|A|·|B|)`) and whose trigger is a *structural* fact read
     from the query (presence of `a.prop = b.prop`), not a fragile estimate.
     Lowest estimate-risk, most robust payoff. Ship first.
  2. **Range-predicate btree index seek** (#1505) — correctness-hazardous
     (comparability vs orderability) and dependent on real selectivity
     statistics. Ship second, after the comparability guard and after histogram
     statistics exist; inert until then.
  3. **Logical rewrites / join reordering** — ship last, if ever, and only behind
     the strongest gates. Highest estimate-risk; multiplicative error
     compounding.

The unifying invariant (see §2.1) mechanically enforces no-regression: with
today's all-`Fallback`/`Heuristic` estimator, every gate fails closed, so the
planner reproduces the current plan exactly.

---

## 1. Inventory

### 1.1 `cypher/ir/rewrite/` — logical rewrite framework (UNWIRED)

A bottom-up, fixpoint rewrite `Driver` (`rule.go`, `defaultMaxIter = 16`)
applying an ordered `Registry` of `Rule`s via `WalkAndReplace`. The walker
reconstructs every IR operator with rewritten children and is structurally
complete over the operator set. `doc.go` correctly states the package is
experimental and not in the engine path. Unit tests (`rewrite_test.go`,
`walk_test.go`, `example_test.go`) are thorough on the framework mechanics.

| Rule | What it does | Coverage | Correct / complete / stubbed |
|---|---|---|---|
| `PredicatePushdown` | Commutes `Selection` below `Projection` (only when predicate vars are in the pre-projection scope), `Sort` (always), and another `Selection` (swap). Refuses to push past `Eager`, `Limit`, `Skip`. | Unit-tested per case. | **Algebraically sound but operationally unsafe under the current IR.** Variable-scope analysis is done by *string tokenisation* of an opaque predicate (`extractVarNames`), splitting on non-identifier runes and taking the token before the first dot. This is a heuristic, fail-closed for the empty case but **not robust**: string literals, map keys, function names and label names inside the predicate text can be misclassified as variables, and shadowed/aliased names are invisible. Not safe to trust for a result-identity guarantee. |
| `ProjectionPushdown` | Dead-column elimination: trims `ProjectionItem`s not required by `ProduceResults`; removes an empty `Projection`. | Unit-tested. | **Correct but narrow.** Only handles the `ProduceResults → Projection` and zero-item cases; the doc comment admits the deeper required-set propagation is left to `FusionRules`. Low value in isolation. |
| `FusionRules` | (1) `Distinct(Limit(n,X)) → Top([],n,X)`; (2) double-`Projection` collapse when outer items are simple renames or covered by inner; (3) redundant-`Distinct` removal over `Distinct`/`EagerAggregation`/`Top`. | Unit-tested. | **Hazardous.** (1) `Distinct(Limit)` → `Top([],n)` is **not result-identical**: `LIMIT` then `DISTINCT` keeps the first `n` rows then dedups (≤ n rows, order of arrival); a degenerate `Top` with no sort keys conflates these. Semantics of `DISTINCT … LIMIT` vs `LIMIT … DISTINCT` differ in cardinality. (2) relies on `item.Expression == item.Name` string equality to decide a "simple rename" — brittle. **Treat as stubbed for production.** |
| `EagerInsertion` | Inserts `Eager` barriers before write operators that read the same state (MERGE always; CREATE/DELETE/DETACH DELETE when the subtree contains a read). SET/REMOVE deliberately left non-eager. | Unit-tested. | **Conceptually aligned with Green et al. 2019 §5, but the production engine already enforces write/read isolation through its own transaction-visibility barrier** (the in-barrier durable-then-visible write path, see ACID hardening notes). Running this rule *in addition* risks double-barriers or, worse, a *different* eager placement than the engine assumes. Redundant at best; conflicting at worst. SET/REMOVE case is explicitly incomplete (a stub by the author's own comment). |

**Conclusion on `rewrite/`:** the framework is well-built and well-tested *as a
framework*, but every rule either (a) duplicates logic the engine already does,
(b) depends on string-opaque-IR heuristics that cannot underwrite a
result-identity guarantee, or (c) is semantically unsafe (`FusionRules` #1).
None is a candidate for near-term wiring.

### 1.2 `cypher/plan/` — cost-based physical planner (UNWIRED, built + unit-tested)

| Component | What it does | Coverage | Correct / complete / stubbed |
|---|---|---|---|
| `cardinality.go` (`IndexEstimator`, `Estimator`) | Row-count estimates: `LabelCount` (exact, from the label bitmap), `HashLookupCount = total/distinct`, `BTreeRangeCount = ceil(distinct·0.30)` (fixed 30% selectivity), `AvgOutDegree` (cached or 1.0). Concurrency-safe (RWMutex over the degree cache). | `cardinality_test.go`. | **`LabelCount` is exact and trustworthy. Everything else is a heuristic or a fallback constant.** The 30% range selectivity and the 1.0 default degree are "I don't know" sentinels; the property-ID arguments to `HashLookupCount`/`BTreeRangeCount` are ignored (it consults the *first* index of the matching kind). Not yet histogram-backed. |
| `scan_strategy.go` (`SelectScanStrategy`, `ScanKind`) | Min-cost pick among AllNodes (1.0/row), Label (0.5), hash IndexSeek (0.05), btree RangeScan (0.1). `fallbackAllNodeCount = 1e9` so any real estimate beats an unknown AllNodes. | `scan_strategy_test.go`. | **Mechanically correct; the `1e9` sandbag is dangerous** (lets a non-trustworthy estimate beat AllNodes purely because AllNodes was deliberately inflated — see §4). |
| `index_registry.go` (`IndexRegistry`) | Per-query snapshot of registered indexes classified into label/hash/btree; `ByKind`, `HasHash`, `HasBTree`, `Lookup`. | `index_registry_test.go`. | **Correct and useful.** This is the cleanest reusable piece. |
| `join_enum.go` (`EnumerateLeftDeep`) | Greedy O(n²) left-deep order, n ≤ 8; cheapest leaf first, extend by min `current_rows × (labelRows/totalNodes)`. | `join_enum_test.go`. | **Selectivity is pure label-ratio** — ignores edge connectivity and functional dependencies; the input most prone to large estimation error. No join *algorithm* is chosen (no hash join exists). High estimate-risk. |
| `expand_direction.go` (`SelectExpandDirection`) | IN vs OUT by `srcCount·avgOutDeg` vs `dstCount·avgInDeg`. | `expand_direction_test.go`. | Correct given trustworthy degree stats; with uncached degree (`1.0`) it is a fallback and must not drive a decision. |
| `stats_maintenance.go` (`StatsManager`) | Generation-aware cache invalidation (atomic gen counter, `NotifyRotation`/`MarkSeen`, bounded staleness). Concurrency-safe. | `stats_maintenance_test.go`. | **Correct and necessary** for keeping estimates coherent with CSR snapshot rotation / Tx commits. Reusable as-is. |
| `cache.go` | Plan cache. | `cache_test.go`. | Independent of the wiring question; not on the critical path for this spike. |

### 1.3 What the production engine already does (verified)

- **Equality index seeks are ALREADY solved**, *without* importing `cypher/plan`
  or `cypher/ir/rewrite`. `cypher/api.go` (`tryBuildIndexSeekFromSelection`,
  `tryNewHashSeek`, `tryNamedHashSeek`, `tryAnyHashSeek`) performs an ad-hoc
  rewrite of `Selection(n.prop = v, {AllNodesScan|NodeByLabelScan})` into an
  `exec.NodeByIndexSeek` when a hash index exists. Param typing is index-aware.
- **Range predicates are NOT solved.** `WHERE n.p > x` always runs as
  `NodeByLabelScan + Selection`. The exec operator `NodeByIndexRangeScan`
  (`cypher/exec/scan_index_btree.go`) and its `Int64RangeIndex` adapter exist and
  are unit-tested, but **are never constructed in production** — verified dead
  code on the production path today. This is the concrete win behind #1505.
- **No hash-join operator exists** anywhere in `cypher/exec/`. Disconnected /
  multi-pattern MATCH degrades to a nested-loop product. This is the work behind
  #1506 (a new exec operator, not just wiring).
- The `go list` import graph confirms **neither `cypher/plan` nor
  `cypher/ir/rewrite` is in the production `cypher` package's transitive
  dependencies.**
- **btree index facts** (`graph/index/btree/index.go`), load-bearing for the
  range-seek guard: the index is **typed** — `Index[V cmp.Ordered]`, one Go type
  per index instance (an int64 index and a string index are distinct objects; no
  cross-type comingling of keys). It uses `cmp.Compare`/`cmp.Less`, **not** raw
  `<`/`==`, and has explicit NaN handling (`isNaN`, `nan_total_order_test.go`,
  `float_ordering_test.go`). `cmp.Ordered` admits only numeric and string types
  (no bool, list, map, or null), so the index never has to order incomparable
  types against each other.

---

## 2. The safety invariant

> **An optimizer may change only the PHYSICAL plan (which operators execute),
> never the LOGICAL result: the multiset of result rows AND any ordering or other
> semantics openCypher mandates.**

A physical substitution is *legal* only when it is provably **result-identical**
to the plan it replaces. Below, per proposed wiring, is why it is (or is not)
result-identical to today's behaviour, and which openCypher semantic hazards it
must clear.

### 2.1 The enforcement mechanism: a trustworthiness veto (graph-theory-expert)

Make estimate provenance first-class. Every estimate carries a confidence tag,
e.g.:

```go
type EstSource uint8

const (
	EstExact     EstSource = iota // maintained count (label count, real distinct count)
	EstStats                      // histogram / sampled distribution
	EstHeuristic                  // principled formula over real inputs
	EstFallback                   // a constant standing in for missing data
)

type Estimate struct {
	Rows   float64
	Source EstSource
}
```

**Planner rule (the no-regression invariant):** a non-default physical plan may
be chosen **only when every estimate on its decision path is `EstStats` or
`EstExact` AND the cost comparison clears the operator's safety margin.** Any
`EstFallback` (or unvalidated `EstHeuristic`) anywhere on the candidate path is
an **absolute veto** → fall back to today's plan.

Why this is the linchpin: today's estimator is entirely `EstFallback` /
`EstHeuristic` (the fixed 30% range selectivity, the `1.0` default degree, the
`1e9` AllNodes sandbag). Under this rule the planner is therefore **provably
inert** — it can only reproduce today's default plan — and it lights up one
decision at a time exactly as each real statistic comes online. This is how we
get a strict no-regression guarantee *for free* during rollout.

### 2.2 Range-predicate index seek — result-identity argument and hazards

A btree range seek on `n.p > x` is result-identical to `NodeByLabelScan +
Filter(n.p > x)` **only** when it returns *exactly* the nodes the filter would.
The cypher-expert-consultant identified the decisive hazard:

**Comparability vs orderability (openCypher 9 §3.4; CIP2016-06-14).** openCypher
`>` / `<` use **comparability**: a comparison across different type groups (e.g.
number vs string) yields `null`, and the node is **excluded** by `WHERE` (3VL).
A btree, however, is laid out by **orderability** — a *total* order over all
values. A naive range seek over a btree therefore **over-returns** every
non-matching-type value that happens to sort past the operand. That is a
**correctness bug, not a performance detail.** (This is precisely why the
already-shipped *equality* fast-path is safe — a point probe under equivalence
sidesteps ordering entirely — while range is not.)

Hazards the seek must replicate exactly:

1. **Three-valued logic / missing properties.** Nodes lacking `p`, or with `p IS
   NULL`, must be excluded — identical to the filter. Guard: the index must not
   index null/missing, OR the seek must exclude them.
2. **Cross-type comparison.** `n.p > 5` must exclude string/bool/list-valued `p`.
   Guard (made concrete by §1.3): because GoGraph btree indexes are **typed**
   (`Index[V cmp.Ordered]`, one type per index), a seek on an int64 index
   *physically cannot* return string-typed values — they live in a different
   index object. The remaining requirement is that the predicate operand's type
   matches the index's value type; if it does not, **do not seek** (fall back to
   scan+filter), because `5 > "foo"` must yield `null`/exclude, which the typed
   index cannot express.
3. **Numeric edge cases.** NaN must be excluded (any comparison with NaN is
   `null`); `-0.0 ≡ +0.0`; integer vs float comparison must follow openCypher
   numeric comparison, **not** IEEE total order. Guard: the btree already has
   explicit `isNaN` handling and uses `cmp.Compare`; the seek must additionally
   ensure NaN keys are not returned by a range and that an int-vs-float operand
   uses openCypher numeric semantics (treat with care — a seek on an int64 index
   for a float operand `n.p > 2.5` must still match int 3,4,…; if the index value
   type and operand numeric class diverge in a way the typed index cannot honour,
   fall back).
4. **String ordering.** Must be code-point / UTF-8 order, matching openCypher
   string comparability — consistent with `cmp.Compare` on Go strings (byte =
   code-point order for valid UTF-8).

**Precise guard under which a range seek IS provably result-identical:**

> Use a btree range seek for `n.p <op> operand` **only when ALL hold:**
> (a) a typed btree index exists whose value type is the operand's scalar
> comparability class; (b) the index excludes null/missing properties; (c) the
> seek excludes NaN; (d) the comparator is openCypher-numeric / code-point string
> order (`-0.0 ≡ +0.0`); (e) the operand is a same-class scalar. If any condition
> is unmet, **fall back to scan+filter.**

Verdict (cypher-expert): **physical range seek is UNSAFE by default; SAFE only
under the full guard above.**

### 2.3 Hash join — result-identity and ordering argument

A hash join replacing a nested-loop join changes the *order* in which rows are
emitted. openCypher result order is **unspecified** unless an order-establishing
operator (`ORDER BY`) is present; the TCK marks order-sensitive expectations with
"the result should be, **in order**:" versus a plain "the result should be:".

**Verdict (cypher-expert): hash join is SAFE**, with one guard:

> A hash join may replace a nested-loop join **only when no order-establishing
> operator sits above it that would observe the changed order**, and bare
> `LIMIT`/`SKIP` without `ORDER BY` must be flagged: openCypher leaves the
> *which* rows under an un-ordered `LIMIT` unspecified, so it is conformant but a
> visible behaviour change — handle conservatively (keep nested loop under a bare
> `LIMIT`/`SKIP` to be safe).

**Scope correction (graph-theory-expert):** a hash join does **not** help a
*true* Cartesian product `MATCH (a),(b)` — the output is `|A|·|B|` by definition
regardless of algorithm. A hash join helps **only when there is an equality
predicate `a.prop = b.prop`** joining the otherwise-disconnected patterns; then
`O(|A|+|B|)` build+probe beats `O(|A|·|B|)` nested-loop-with-filter. Non-equality
join predicates (`<`, `<>`) admit no hash key and stay nested-loop. The trigger
is thus a **structural fact** (presence of an equi-join predicate), not an
estimate — which is what makes this the lowest-risk increment.

### 2.4 Write-visibility / Eager — confirmation

**Verdict (cypher-expert): physical-only selection is SAFE for write
visibility.** As long as the logical eager-insertion decisions are left
unchanged (we are NOT wiring `EagerInsertion`), substituting a physical access
path preserves the materialisation boundary the engine already enforces. The one
thing physical selection must never introduce is a **post-write same-index
re-probe** (a Halloween-problem read of an index we are concurrently mutating) —
the guard is: do not select an index seek that reads an index being written by
the same statement.

### 2.5 OPTIONAL MATCH null-rows — confirmation

**Verdict (cypher-expert): SAFE.** When an index seek replaces a scan *inside* an
OPTIONAL MATCH, the null-row synthesis lives in the Optional wrapper
(`OptionalExpand` / `OptionalApply`), not in the access path; the seek only
changes how candidate rows are produced and must still exclude null-property
nodes (same guard as §2.2). The wrapper still emits the null row when the inner
produces nothing.

---

## 3. TCK-safety strategy (how 3897/3897 stays green)

1. **Physical-only first.** Wire cost-based *operator selection* (index range
   seek vs scan; hash join vs nested loop) and **never** the logical rewrite
   `Driver`. Logical rewrites are the ones that can reorder or observe writes;
   physical selection under the §2 guards cannot change the row multiset.
2. **Per-operator opt-in.** Each physical substitution is behind its own guard
   and its own feature flag, defaulting to the trustworthiness veto (§2.1). No
   "turn the optimizer on" master switch.
3. **The planner invariant, enforced in code:** "never pick a plan whose result
   differs." Concretely, a substitution is permitted only when its guard
   (§2.2–§2.5) holds *and* the trustworthiness veto passes; otherwise the default
   plan is used.
4. **Differential testing.** Run **every** TCK scenario twice — optimizer ON vs
   OFF — and assert byte-identical result sets (and identical *order* for
   "in order" scenarios). Add this as a CI matrix dimension. This is the primary
   guarantee that 3897 holds.
5. **EXPLAIN/PROFILE diff harness.** GoGraph already has EXPLAIN plumbing
   (`cypher/ir/explain.go`, `cypher/api.go`). Add a harness that renders the
   chosen physical plan for a query under ON vs OFF and diffs them, so a reviewer
   can see exactly which operator changed and confirm the guard rationale. Useful
   for auditing, secondary to the differential result test.
6. **The existing equality fast-path is the proof of concept**: it already
   substitutes a physical index seek for a scan+filter in production while 3897
   holds — because equality-under-equivalence is order-free and type-safe. The
   range and hash-join increments extend the same discipline under stricter
   guards.

---

## 4. No-performance-regression strategy

The mandate: the planner must **never** choose a plan slower than today's default
for any query. The mechanism is the trustworthiness veto (§2.1) plus
conservative, empirically-grounded margins (graph-theory-expert):

- **Range seek crossover.** A range seek does *random* access into the CSR; a
  label scan is *sequential* and cache-friendly. The index wins only when
  selectivity `S < c_seq/c_rand`. For an in-memory CSR (no disk I/O) the
  random/sequential ratio is ~3×–10× (memory hierarchy), so the *break-even* is a
  *higher* selectivity than a disk DB's ~5–10%. But we must ship **inside** the
  break-even, not at it. Guard: switch to a btree range seek **only when**
  (1) `estimated_selectivity ≤ 0.05` (well below the ~10–30% in-memory break-even,
  so an estimate wrong by up to ~3–5× still lands safe), **and**
  (2) the estimate is `EstStats`/`EstExact` (NOT the fixed-30% fallback), **and**
  (3) `N_label ≥ ~1024` so index-descent overhead is amortised (tiny label sets
  always scan). Note the fixed 30% constant **fails condition (1) by
  construction** (0.30 > 0.05), so the crude estimator can never trigger a range
  seek — correct and harmless until histograms exist.
- **Kill the `1e9` AllNodes sandbag.** AllNodes should carry an **exact** total
  node count (`EstExact`), so it competes honestly and is never beaten by a
  non-trustworthy alternative.
- **Hash join.** Asymptotically strictly better for an equi-join; the only loss
  case is the tiny-input constant factor, neutralised by a **size floor** (use
  hash join only when both sides ≥ ~64, calibrate by benchmark) plus the equi-join
  predicate requirement plus `EstStats`/`EstExact` side cardinalities.
- **Join reordering (deferred).** Errors compound *multiplicatively* along the
  join chain (Ioannidis & Christodoulakis, SIGMOD 1991). Gate hardest: reorder
  only when every selectivity on both the candidate and the written order is
  `EstStats`/`EstExact`, the candidate is `≥ 3×` cheaper, and the chain is short
  (`n ≤ 4`). Label-ratio selectivity is `EstHeuristic` → fails the gate → no
  reorder today. Keep the developer-written order as the safe default.
- **benchstat gate.** Per CLAUDE.md, every structural change runs
  `go test -bench=. -benchmem -count=10` before/after, compared with `benchstat`.
  Add a representative query-mix benchmark (point-seek, range, equi-join,
  disconnected) and require that no query regresses in ns/op or allocs/op without
  documented justification. Calibrate the `c_seq/c_rand` ratio, the 5% threshold,
  the 1024 label floor, and the 64 join floor empirically on the CSR — the
  numbers above are conservative *starting points* to validate, not final tuned
  values.

---

## 5. Incremental rollout

Each increment is independently shippable, self-contained, gated by the
trustworthiness veto, and inert under the current estimator.

### Increment A — Hash join for equi-join across disconnected patterns (#1506) — FIRST

- **New work:** a `NodeHashJoin` exec operator (build hash table on the smaller
  side keyed by the equi-join column, probe with the larger). This is *new code*,
  not just wiring; no such operator exists today.
- **Trigger (structural, not estimated):** two otherwise-disconnected patterns
  with a conjunctive equality predicate `a.prop = b.prop` between them.
- **Guards:** §2.3 (no order-establishing operator above; keep nested loop under
  bare `LIMIT`/`SKIP`); both sides ≥ size floor; side cardinalities
  `EstStats`/`EstExact`.
- **Why first:** asymptotically strictly better; trigger is a query fact, not a
  fragile estimate; lowest estimate-risk.
- **Reuse:** `IndexRegistry`, `LabelCount` (exact), `StatsManager`. Does **not**
  require `EnumerateLeftDeep` (that is join *reordering*, deferred).

### Increment B — Range-predicate btree index seek (#1505) — SECOND

- **Wiring:** extend the existing ad-hoc selection-rewrite path in `cypher/api.go`
  (the same place `tryBuildIndexSeekFromSelection` lives) to recognise
  `Selection(n.p <op> operand, {scan})` with `<op> ∈ {>, >=, <, <=}` and bounded
  ranges, and build the **already-existing** `exec.NodeByIndexRangeScan` /
  `Int64RangeIndex` operator. The rewrite `Driver` stays OFF.
- **Guards:** the full comparability guard §2.2 (typed index of operand's class;
  excludes null/missing; excludes NaN; openCypher-numeric / code-point order;
  same-class operand) AND the no-regression guard §4 (selectivity ≤ 5%,
  `EstStats`/`EstExact`, `N_label ≥ 1024`).
- **Dependency:** delivers value only once real selectivity statistics
  (histograms / distinct-value-aware range estimates) exist; until then the 30%
  fallback keeps it inert (correct, harmless). The histogram work is a
  prerequisite sub-task to schedule under #1505.
- **Reuse:** `SelectScanStrategy` (with the `1e9` sandbag removed and `Estimate`
  provenance added), `IndexRegistry`, `cardinality.go`, `StatsManager`.

### Increment C — Logical rewrites / join reordering — LAST (and only if justified)

- Wire selected rules from `cypher/ir/rewrite/` and/or `EnumerateLeftDeep` **only
  after** A and B have proven the `Estimate{Source}` veto in production, **and
  only** under the strongest gates (§4), **and only** after each rule's
  result-identity hazard (§1.1) is resolved on a non-opaque IR. `FusionRules` #1
  must be fixed or excluded; `EagerInsertion` must be reconciled with the engine's
  existing barrier rather than run alongside it.
- This is where the guard test must change (below). It is plausible this
  increment is **never** justified for the logical rewrites, given the engine
  already handles equality seeks and write barriers natively.

### The guard test (`rewrite_not_wired_test.go`)

- For Increments A and B, **the guard test stays exactly as-is and stays green** —
  neither increment imports `cypher/ir/rewrite`. (Increment B touches
  `cypher/plan`, not `rewrite`; if desired, add a parallel assertion that
  `cypher/plan` *is* now wired.)
- For Increment C (if ever), the guard test must **not be deleted blindly.** It
  should be *transformed* from "rewrite is not imported" into a "**wired safely +
  differentially tested**" gate: assert that whenever `cypher/ir/rewrite` is in
  the import graph, the differential TCK harness (§3.4) is part of the CI matrix
  and green. The protection (no silent behaviour change) is preserved; only the
  mechanism changes from "absent" to "present and proven equivalent."

---

## 6. Specialists' key input (summary)

**cypher-expert-consultant (openCypher / TCK safety):**
- The decisive range-seek hazard is **comparability vs orderability**
  (openCypher 9 §3.4, CIP2016-06-14): `>`/`<` use comparability (cross-type →
  null → excluded), but a btree is laid out by orderability (total order), so a
  naive range seek over-returns — a *correctness* bug. This is exactly why the
  shipped equality fast-path is safe and range is not.
- Verdicts: range seek **UNSAFE by default, SAFE under the full §2.2 guard**;
  hash join **SAFE** (order is unspecified absent `ORDER BY`; flag bare
  `LIMIT`/`SKIP`); physical-only write-visibility **SAFE** (no post-write
  same-index re-probe); OPTIONAL MATCH **SAFE** (null-row synthesis stays in the
  wrapper).
- Citations: openCypher 9 §3.2 (null/3VL), §3.4 (comparability), §8.4 (WHERE),
  §10 (OPTIONAL MATCH); CIP2016-06-14; the TCK "in order:" convention.

**graph-theory-expert (cardinality / join ordering / no-regression):**
- Make estimate **provenance** first-class (`Estimate{Rows, Source}`); a
  `Fallback` estimate anywhere on a candidate path is an **absolute veto**. This
  single rule makes the planner provably inert under today's crude estimator,
  giving no-regression for free during rollout.
- Range-seek crossover `S < c_seq/c_rand`; in-memory ratio ~3×–10× ⇒ break-even
  higher than a disk DB's ~5–10%, but ship at a conservative `S ≤ 0.05` with a
  `N_label ≥ 1024` floor. Kill the `1e9` AllNodes sandbag.
- Hash join helps **only** equi-join across disconnected patterns (not true
  Cartesian); asymptotically strictly better, neutralise the tiny-input loss with
  a size floor (~64). **Ship hash join first** — lowest estimate-risk.
- Join reordering errors compound multiplicatively (Ioannidis & Christodoulakis,
  SIGMOD 1991); gate hardest (all-stats, ≥3× margin, n ≤ 4); ship last.

---

## 7. Verification (this spike)

- `go build ./...` — green (no production imports changed).
- `TestCypherEngine_RewritePackageNotWired` — green (nothing wired).
- This document adds no code; it is a design artefact only.
