# Design — set-at-a-time bitmap intersection access paths

Design record for rmp task **#2132** (sprint 312, *Planner R2-P2: set-at-a-time
bitmap intersection access paths*). It fixes the gate, the cost model, the
precedence order, the verdict on extending the lever to property predicates, and
the snapshot-atomicity argument — before any production code is written.
**This task changes no production code.**

- **Status** — design accepted; implementation is #2133, extension is #2134.
- **Source finding** — `docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.2.
- **Machinery reused unchanged** — `graph/index/label.Index.Intersect`,
  `roaring64.Bitmap.AndCardinality`, `exec.NodeByLabelScan`,
  `cypher/min_label_scan_plan.go` (#2077).

## 1. The measured gap, re-verified

The audit's numbers date from 2026-07-25; sprints 326, 327 and 311 have landed
since, so the premise was re-measured on the current tree before any design work.

Fixture exactly as the audit specifies: `|LabA| = |LabB| = 100 000`,
`|LabA ∩ LabB| = 100`. Apple M4, `darwin/arm64`, `-benchmem -count=6`.

| Path | sec/op | B/op | allocs/op |
|---|--:|--:|--:|
| Shipped: label scan + residual `Filter` (`MATCH (n:LabA:LabB) RETURN count(n)`) | 18.01 ms | 811 821 | 99 821 |
| Access-path floor: one k-way Roaring AND + count | 2.215 µs | 8 880 | **17** |
| **The gate alone**: `AndCardinality`, no materialisation | **424 ns** | **0** | **0** |

The audit reported 18.60 ms / 811 961 / 99 825 and 2.29 µs / 8 880 / 17. **The
premise holds**, essentially to the digit.

Plan confirmed by `Engine.Explain`:

```
Project                                ProduceResults
└─ GlobalAggregateAdapter              └─ Projection
   └─ EagerAggregation                    └─ EagerAggregation
      └─ Project                             └─ Selection
         └─ Filter                              └─ NodeByLabelScan [n:LabA]
            └─ NodeByLabelScan [LabA]                 (est. rows=100000, exact)
```

The planner states, *exactly and with `exact` provenance*, that it is about to
scan 100 000 rows to return 100. The shipped min-label peephole (#2077) does not
fire here and is right not to: it switches only on a **strict** cardinality
improvement, and both labels are 100 000. That is precisely the regime the audit
calls "powerless".

**The third row is new** and did not appear in the audit. `AndCardinality` is the
gate, and it is **free**: 424 ns and **zero allocations** to decide, against 18 ms
of being wrong. This is what makes the decision cost-free rather than a trade.

## 2. The rewrite rule

For a `Selection` whose predicate is a bare `LabelPredicate` over the same node
its child `NodeByLabelScan` binds — the shape the min-label peephole already
recognises — replace the pair with a scan over

```
label.Index.Intersect(L₁, L₂, …, L_k)      ordered by ASCENDING cardinality
```

and **drop the residual label `Filter`**, which the bitmap subsumes (§5).

`Intersect` is already implemented (`graph/index/label/index.go:186`): it holds
`i.mu.RLock()` for the whole k-way AND, materialises a caller-owned bitmap from
the first label (cloning when the source aliases the live bitmap), ANDs the rest
in, and exits early the moment the result is empty.

## 3. The gate

`roaring64.Bitmap.AndCardinality` (verified present in the resolved
`v2.18.2`, `roaring64/roaring64.go:541`) walks the two container arrays by key
with `advanceUntil` skips and accumulates per-container intersection counts. It
**materialises nothing and allocates nothing** — measured 424 ns / 0 B / 0 allocs
on the 100 000 × 100 000 fixture.

So the decision fits GoGraph's established **exact-or-veto** pattern with **no new
statistic**:

- every candidate label's cardinality is already the exact live count the
  min-label peephole reads (`estExact`, #2076), and the estimate-provenance veto
  (`planStaysDefault`) already fails closed on an untrustworthy resolver;
- the intersection cardinality is **exact**, obtained before materialisation.

**The gate fires the AND unless it would lose**, and §4 shows it essentially
cannot. The veto path is not "fall back to a scan" but "fall back to the shipped
min-label plan", which is never worse than today.

### 3.1 k ≥ 3 labels

`AndCardinality` is pairwise. For three or more labels there is no k-way
cardinality primitive, and computing one exactly would require materialising
intermediates — i.e. doing the work the gate exists to avoid. The resolution is
that **`Intersect` already carries its own early empty-exit**, so for k ≥ 3 the
design applies the pairwise gate to the two smallest labels (the tightest
available bound on the k-way result, since
`|L₁ ∩ … ∩ L_k| ≤ |L₁ ∩ L₂|`) and lets `Intersect`'s empty-exit handle the rest.
That is sound because the bound is an **upper** bound: if the two smallest labels
already intersect selectively enough to admit the AND, adding further labels can
only shrink the result.

## 4. The cost model, and the clone the audit warned about

The audit's design caveat — "`Intersect` clones the first bitmap, so the AND is
not free when the intersection is nearly as large as the smaller label" — is real
and was **measured** rather than accepted. On a skewed fixture
(`|LabA| = 100 000`, `|LabC| = 1 000`, overlap 100), the two argument orders do
very different clone work:

| Argument order | clones | sec/op | B/op | allocs/op |
|---|---|--:|--:|--:|
| `Intersect(LabA, LabC)` — large first | the 100 000 bitmap | 3 358 ns | 19 008 | 20 |
| `Intersect(LabC, LabA)` — small first | the 1 000 bitmap | **556 ns** | **2 532** | **14** |

**6.0× faster and 7.5× less memory from argument order alone.** So the design
mandates: **order the label ids by ascending exact cardinality before calling
`Intersect`**. The counts required are the ones the min-label peephole already
computes, so this costs nothing new. The AND is commutative, so the answer is
unchanged — asserted in the spike, and pinned by the differential (#2135).

With smallest-first ordering the AND's cost is `O(|L_min| / 64)` words of clone
plus a container-wise walk, against the shipped plan's `O(|L_min|)` **materialised
rows** each carrying a per-row label re-check. A Roaring word-level AND is orders
of magnitude cheaper per element than materialising a row, which is why:

**There is no break-even where the AND loses.** Even in the worst case
`L_min ⊆ L_other` — the intersection equal to the smaller label, the audit's
feared case — the AND emits the same rows the min-label scan would **and drops
the per-row label re-check**, so it does strictly less work. The measured
2.2 µs / 17 allocs for a 100 000-element AND against 18 ms / 99 821 allocs for the
scan is the same statement quantified.

The gate is therefore retained not to prevent a regression in the AND itself but
because (a) it is free, (b) it keeps the change inside the project's exact-or-veto
discipline, and (c) it is the honest place to fail closed when a cardinality is
not trustworthy. Task #2136 must measure a fixture at the claimed break-even to
confirm this reasoning empirically rather than leave it as an argument.

## 5. Why the residual `Filter` is subsumed — and it is, unlike the property case

This is the load-bearing correctness question, and it was settled by measurement,
not inference. **The label index is maintained on both delete and relabel:**

| Operation | `|L1 ∩ L2|` |
|---|--:|
| three nodes, both labels | 3 |
| after `RemoveNode("n2")` | **2** |
| after `RemoveNodeLabel("n3","L2")` | **1** |

So the bitmap is **authoritative** for label membership *and* for liveness, and the
residual `LabelPredicate` `Filter` re-checks nothing the bitmap has not already
decided. Dropping it is what turns 99 821 allocations into 17 — the win is
precisely the rows never materialised.

**No new exposure.** A node deleted *after* `Init` but before its row is read is
still in the materialised bitmap — but that is exactly the situation a plain
single-label `MATCH (n:L)` is already in today, since `NodeByLabelScan`
materialises its bitmap at `Init` and iterates it with no residual filter at all.
The intersection path inherits the existing, already-accepted contract rather
than widening it.

## 6. Snapshot atomicity

- **The AND is atomic.** `Intersect` holds `i.mu.RLock()` across every label, so
  the k-way result is a consistent image of the label index — not k independently
  sampled bitmaps.
- **The image is a committed state.** Writes update the live Roaring label bitmaps
  *inside* the `ApplyAtomically` / `visMu` window, together with the graph and
  (since F3.4) the secondary indexes, so graph and labels flip together
  (`docs/isolation-design.md` F3.4/F3.6; pinned by
  `lpg.TestIsolation_CrossSubstructure_EdgeImpliesLabels`).
- **It is therefore *more* consistent than today**, not less. The shipped plan
  decides the anchor label at `Init` and re-checks the other labels **live, per
  row**, so a concurrent relabel can interleave *within* one scan. The AND decides
  the whole conjunction at one instant. Cypher's contract is a per-statement
  snapshot (`docs/cypher.md`), which the AND matches more closely than a per-row
  live re-check does.

#2135 must assert this directly: concurrent relabelling must never yield a row
violating the conjunction.

## 7. Precedence

The `Selection` build already runs its peepholes in a fixed order. The
intersection path recognises the **same shape** as the min-label peephole, so it
slots immediately before it:

1. `tryBuildIndexSeekFromSelection` — equality seek; **subsumes** the Selection.
2. `tryBuildIndexSeekSetFromSelection` — key-set seek; also subsumes it.
3. range seek (`buildRangeSeekIfEnabled`) — replaces the scan child, keeps the Filter.
4. **label intersection (new)** — replaces scan **and** Filter.
5. `buildMinLabelScanIfEnabled` (#2077) — re-anchors the scan, keeps the Filter.
6. default `Labels[0]` plan.

Rationale: an equality or key-set seek on an indexed property is more selective
than a label conjunction and already subsumes the Selection, so it must keep
winning — the same precedence #2077 established and for the same reason. The new
path sits above the min-label peephole because it **strictly dominates** it
whenever the gate admits (same rows, no per-row re-check); when the gate vetoes,
control falls through to the min-label peephole unchanged, so the worst case is
exactly today's plan.

The min-label peephole is deliberately **left in place**, not replaced: it still
serves the case where the gate declines, and it is the fallback that makes "never
worse than today" true by construction.

## 8. Verdict on item (d) — conjunctive indexed property predicates: **AFFIRMATIVE**

`RangeBitmap(lo, hi)` also returns a `*roaring64.Bitmap`, so two single-property
indexes compose by intersection. This is admissible, and #2134 should proceed —
**but with one sharp difference from the label case that must be respected.**

A label bitmap is **exact**; a range-index bitmap is a **superset** by explicit
design (`NodeByIndexRangeScan` emits the inclusive `[lo, hi]` superset and relies
on a residual `Filter` for exactness — #F-EXEC1, and the prefix seek of sprint 311
depends on that same contract). Intersection **preserves** the superset property:

> if `Bᵃ ⊇ {n : predᵃ(n)}` and `Bᵇ ⊇ {n : predᵇ(n)}`
> then `Bᵃ ∩ Bᵇ ⊇ {n : predᵃ(n)} ∩ {n : predᵇ(n)}` — the true answer. ∎

So the AND is sound, and `AndCardinality` over the two supersets yields an exact
**upper bound** on the true answer — exactly the conservative gate wanted. But:

- **the residual `Filter` is MANDATORY here**, unlike the label case. Each bitmap
  is only a superset, so the exact predicate must still be re-applied per surviving
  row. Dropping it would be a correctness defect.
- any conjunct with no covering index stays in the residual `Filter` as today;
- both indexes must be bound to the same label, which
  `findBoundStringBTree` / `findBoundNumericBTree` already enforce, so the AND
  cannot cross label populations.

**This is the differentiator to record.** Memgraph needs a dedicated *composite
index* type to answer `WHERE n.a > 1 AND n.b < 9` from indexes; GoGraph composes
**any two single-property indexes** by intersection, with no new index type and no
new statistic. Neither Neo4j nor Memgraph does set-at-a-time label intersection at
all — both scan a label then filter.

## 9. Implementation shape

| Concern | Decision |
|---|---|
| Recognition | A new peephole in `cypher/min_label_scan_plan.go` reusing `pickMinLabel`'s shape recogniser, so both paths agree on what a "bare multi-label LabelPredicate Selection" is. |
| Operator | `exec.NodeByLabelScan` already iterates whatever bitmap its `labelResolver` returns, so the cleanest change is a resolver/operator that yields the **intersected** bitmap for a label LIST. No new iteration machinery. |
| Ordering | Sort candidate label ids by ascending exact cardinality before `Intersect` (§4). |
| Gate | `AndCardinality` on the two smallest labels; exact-or-veto; falls through to #2077. |
| Filter | **Dropped** for labels (§5); **retained** for properties (§8). |
| Flag | `EngineOptions.DisableBitmapIntersection`, default enabled, threaded as `buildOpts.bitmapIntersectEnabled` via `planGates`, mirroring `DisableMinLabelScan`. A separate knob so the differential varies exactly one thing. |
| Empty label | Preserved: a zero-cardinality label makes the conjunction empty; `Intersect` returns an empty bitmap and the scan emits nothing. |
| Determinism | Cardinality ordering is made total by breaking ties on label id then syntactic index, reusing #2077's existing tie-break so plans stay stable run to run. |

## 10. Test plan (#2135) and how the numbers must be reported (#2136)

Required coverage: an empty label; a label absent from the registry; a tiny
intersection; an intersection equal to the smaller label (the claimed break-even);
three or more labels; a multi-label pattern with an inline property predicate,
with `WHERE`, inside `OPTIONAL MATCH`, and inside a pattern that also expands;
nodes relabelled mid-transaction; deleted nodes present in a stale bitmap;
**both argument orders** (the AND is commutative — pin it); a `rapid` property
asserting the planned rows always equal the set-theoretic intersection of label
memberships; and a concurrency assertion under `-race`.

As in sprint 311, the differential must assert the two arms take **different
plans** where the path is meant to fire — a differential whose arms share a plan
is green for the wrong reason — and every case must also be checked against an
**absolute oracle** (the intersection computed directly from the label sets),
because both arms share the same row pipeline and a shared defect is invisible to
an ON/OFF comparison.

**The reported figure must be end-to-end.** The audit itself flagged that its
8127× compares an end-to-end query against a bare access path and overstates the
deliverable. The engine's fixed parse/plan/result overhead does not disappear:
sprint 311 measured a 100-row answer through the full engine at ≈31 µs. Against
18.01 ms that bounds the honest expectation at a **few hundred ×**, and #2136 must
report the end-to-end ratio with that floor stated.

## 11. Risks

| Risk | Mitigation |
|---|---|
| Dropping the residual `Filter` changes an answer | The bitmap is authoritative for labels and liveness — measured, §5 — and the differential + absolute oracle pin it for every input class. |
| The clone makes the AND lose | Ordering smallest-first (§4, measured 6.0×); the gate vetoes to the shipped plan, never to something worse; #2136 measures the break-even fixture. |
| Precedence regression against an existing peephole | §7 fixes the order; an index-seek precedence test is already required by #2133's acceptance criteria. |
| Property-predicate extension drops the mandatory Filter | §8 states it is mandatory and why the label case differs; #2134's differential must include a case where the range bitmap over-returns. |
| Concurrency divergence | §6: the AND is atomic under one RLock and matches the per-statement snapshot contract more closely than the per-row re-check it replaces. |
