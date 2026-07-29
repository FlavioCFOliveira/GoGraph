# Expand(Into) for bound destinations, and the symmetric anchor swap

Design record for rmp sprint 314 (task #2148, the SPIKE). It fixes the physical
access path for a hop whose destination variable is already bound, and specifies
the cost model that lets the single-edge anchor-swap peephole become symmetric.

Status: **design accepted, no production code in #2148**. Implementation is
#2149 (the seek) and #2150 (the symmetric swap); validation #2151; measurement
#2152; documentation #2153; end-to-end exercise #2154.

Every number below was measured on this machine (Apple M4, `darwin/arm64`) at
commit `9edebf60`, the sprint-314 base. Nothing in this document is carried over
from the motivating audit without re-measurement, because three of its stated
premises did not survive it.

---

## 1. The premise, re-measured

`docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.3 is the motivating
finding. Its harness is 20 000 `:CyN` nodes, ring-structured out-degree 8 and 64,
running `MATCH (a:CyN)-[:F]->(b:CyN)-[:F]->(a) RETURN count(*)`. That harness was
reproduced exactly.

| Out-degree | Audit, 2026-07-25 | HEAD `9edebf60`, 2026-07-29 | Change |
|---|---|---|---|
| 8 | 625.8 ms · 222 MB · 9.68 M allocs | **71.98 ms · 9.71 MB · 686 K allocs** | 8.7× faster · 22.9× less memory · 14.1× fewer allocs |
| 64 | 41.68 s · 12.80 GB · 577.9 M allocs | **2.981 s · 319 MB · 6.54 M allocs** | 14.0× faster · 40.1× less memory · 88.4× fewer allocs |
| Growth for 8× degree | 66.6× | **41.4×** | — |
| Empirical exponent | **2.02** | **1.79** | — |

`-benchmem`, `-count=3`, spread across the three runs below 0.6%.

### 1.1 What this changes

**The audit's absolute reference points are not reproducible and must not be
used as a baseline.** They predate rmp #2206, which added the `intoCol` filter to
`Expand` — the operator still enumerates the whole neighbour run but discards
non-matching slots *before* building a row. That removed the per-neighbour row
construction and boxing, which is where the 12.8 GB went. It also predates
#2142, which made the read-path forward-position probes O(log d).

Consequently **task #2152's acceptance criterion "Reproduce the audit's
reference points (degree 8: 625.8 ms, 9.68M allocs; degree 64: 41.68 s, 577.9M
allocs) as the disabled baseline" is unattainable by construction.** The
disabled-baseline row to record is the HEAD column above.

**The defect itself is real and survives.** An exponent of 1.79 over 8→64 is
still super-linear: the closing hop still walks Θ(d) slots per input row. What
changed is the magnitude, not the kind.

### 1.2 The honest target exponent, per shape

The sprint description and #2152 both quote "exponent 2.02 → about 1.1". Both
halves need correcting, and the correction is shape-dependent.

For the **2-cycle / mutual-relationship close** — the audit's own shape — hop 1
emits Θ(n·d) rows and hop 2 currently walks d slots for each, so the total is
Θ(n·d²). A seek makes hop 2 Θ(log d + r) per row, so the total becomes
Θ(n·d·(log d + r)). Over the measured range that is an exponent of

```
log( (64·log₂64) / (8·log₂8) ) / log 8  =  log(384/24) / log 8  =  log 16 / log 8  =  1.333
```

**1.33, not 1.1.** The audit's ≈1.1 is the asymptotic limit of `d·log d` for
large d; it is not what the 8→64 range can yield. The target for this sprint is
therefore *"materially below 1.79, approaching 1.33"*, and #2152 must state the
range its fitted exponent covers.

For a **triangle** `(a)-[:K]->(b)-[:K]->(c)-[:K]->(a)`, hop 2 is *open* and
inherently materialises Θ(n·d²) intermediate rows. A seek on hop 3 removes that
hop's Θ(d) factor but cannot reduce an intermediate result the pattern genuinely
has. Measured triangle exponent today is **1.96**, and it will stay near 2 after
the change. This is not a shortfall — it is the true cost of triangle
enumeration under this plan shape, and Neo4j pays it too.

**The headline claim must therefore be made on the 2-cycle/mutual shape, and the
triangle must be reported as bounded by its intermediate cardinality.** Reporting
a single blended exponent across both shapes would misstate the result.

### 1.3 Where the remaining time goes

CPU profile of the degree-64 query (10.21 s of samples, `-benchtime=2x`):

| Symbol | Share | Note |
|---|---|---|
| `Expand.advanceFwdEdge` | 37.9% cum | the enumeration |
| ┗ `Expand.passesFilter` | 18.6% cum | of which `runtime.mapaccess2_fast64` **17.8%** |
| ┗ `Expand.passesRelMorphism` | 16.7% flat | cyphermorphism check |
| `Expand.dstMatchesInto` | 0.98% | the #2206 filter itself |
| `runtime.madvise` | 20.3% | GC returning the 319 MB/op to the OS |

This is the decisive measurement. The per-slot cost is **not** the destination
comparison (1%) — it is the edge-type filter's hash lookup and the
cyphermorphism check, both of which a seek skips entirely on non-matching slots.
Eliminating the enumeration removes work that is currently ~35% of wall time at
degree 64, and the share grows with degree.

It also exposes an unrelated defect worth recording: **`passesFilter` performs a
`map[uint64]string` lookup per edge slot** where the key is a dense CSR
position. A slice or bitmap indexed by position would replace a hash with a load.
That is out of this sprint's scope and belongs in the backlog.

---

## 2. Recognition site (SPIKE question a)

**Decision: reuse the recognition that already exists. Add none.**

The chain is already complete and needs no new detection:

1. `cypher/ir/match.go:1699` — `matchExpandStepBoundWithFrom` sets
   `destRebinding` when the step's destination is a variable the row already
   carries, expands into a synthetic `__anon_N_to_<var>`, and emits an equality
   `Selection` above it.
2. The translator records the bound variable on the IR node as `Expand.IntoVar`,
   so no string parsing is needed downstream.
3. `cypher/api.go:8051` resolves it to a physical column at build time —
   `if intoCol, bound := schema[p.IntoVar]; bound { exp = exp.WithExpandInto(intoCol) }`.

The *logical* fact (this destination is bound) is known in the IR; the *physical*
seek needs the CSR, which only the operator holds. Splitting it exactly there is
the same division already used by the prefix range seek and the min-label scan:
the IR marks intent, the builder resolves columns, the operator owns the access
path. So the answer to "IR versus build-time peephole" is **neither — the
decision is already made in the IR, and only the operator's access path changes.**

---

## 3. The ExpandInto contract (SPIKE question b)

**Decision: realise ExpandInto as a seeking cursor inside `Expand`, not as a new
operator type. Render it as `ExpandInto` in `EXPLAIN`.**

### 3.1 Why not a separate operator

`Expand.loadAdjacency` (`cypher/exec/expand.go:597`) is the single place where a
source's cursor range is set, and it has exactly two call sites — `advanceInput`
(row mode, :592) and `advanceInputChunk` (columnar mode, :879). Narrowing
`[fwdStart, fwdEnd)` there to the destination's run *is* the seek, and it leaves
every downstream behaviour untouched and unduplicated:

- the edge-type filter (`passesFilter`) and its multi-type semantics;
- cyphermorphism (`passesRelMorphism`), including the reverse path's canonical
  forward-position edge ID;
- CREATE-multiplicity re-emission (`maybeQueueMultiplicity`);
- `DirBoth` ordering (forward run then reverse run) and its undirected self-loop
  deduplication (`advanceRevEdge`, :452);
- the per-4096-row cancellation cadence;
- the columnar `FillChunk` path, which shares `advanceFwdEdge`.

A separate operator would have to re-implement all of it. That is precisely the
duplication #2206 avoided when it chose to extend the existing emit gate, and
the reasoning has not changed. The task text for #2149 says "add an ExpandInto
operator to `cypher/exec/`"; the deliverable it actually asks for — a
bound-destination hop that probes instead of enumerating, visible as `ExpandInto`
in `EXPLAIN` (#2153) — is fully met by this shape, with strictly less risk. The
operator's *identity* becomes a mode of `Expand`; its *observable presence* is
the `EXPLAIN` label.

### 3.2 The seek

When `intoCol >= 0` and the bound cell of the current input row resolves to a
bare `NodeID` `D`:

```
fwdStart, fwdEnd = dstRun(fwdEdges, fwdVerts[u], fwdVerts[u+1], D)   // O(log d + r)
revStart, revEnd = dstRun(revEdges, revVerts[u], revVerts[u+1], D)   // DirIn / DirBoth
```

`dstRun` is the shipped, allocation-free helper from `cypher/exec/csrprobe.go:68`.
The forward CSR's run is `(destination, handle)`-ordered unconditionally since
#2141; the reverse CSR's run is `(source, handle)`-ordered — see §3.5.

`dstMatchesInto` **stays in place**. After a successful seek it is a tautology,
but it remains the correctness backstop for every case the seek declines, and it
costs 1% of the profile. Removing it would couple correctness to the seek having
fired.

### 3.3 Parallel-edge identity

Ordering is by destination *first*, so every slot sharing destination `D` forms
one **contiguous run**, ordered by handle within it. `dstRun` returns exactly
that block, so the operator emits **one row per relationship instance**, each
carrying its own CSR position as edge ID and its own handle — never one row per
neighbour, and never a single merged row for a multi-edge pair. Multiplicity `r`
is walked, not searched, which is why the bound is `O(log d + r)` and never worse
than the `O(d)` it replaces, since `r ≤ d`.

### 3.4 Fallback — the seek must decline, never guess

The seek engages only when the bound destination is a resolvable bare `NodeID`.
It falls back to the full range, unchanged, when:

- the bound cell is **NULL** (an `OPTIONAL MATCH` NULL-padded row) — there is no
  key to seek to;
- the bound cell is a **boxed entity** a projection placed there rather than a
  bare id — today's filter already defers to the `Selection` above in this case,
  and the seek must make the same choice;
- the row is **narrower** than `intoCol`;
- the source id is out of the CSR's vertex range.

In every declined case the operator's output stays a **superset** of the correct
result and the equality `Selection` above remains the source of truth. This is
the same seek-superset-plus-residual-refilter discipline the prefix range seek
uses, and it is what makes the change incapable of being a regression.

### 3.5 The reverse direction rests on a by-construction invariant

`graph/csr/csr.go:505` `BuildReverse` **never calls `OrderRuns`**. Its runs are
`(source, handle)`-ordered only *by construction*: pass 2 iterates the source `u`
ascending, and each forward run is already handle-ordered, so the parallel edges
`u→v` scatter into `v`'s reverse run in handle order.

This was **verified empirically**, not assumed: 200 randomised multigraph
fixtures dense in parallel edges, including a handle-0 sentinel slot, asserting
both CSRs' runs are non-decreasing in `(neighbour, handle)`. It passes.

Because the invariant is implicit, a reverse seek makes it **load-bearing**.
#2151 must therefore ship that check as a *permanent* invariant test in
`graph/csr`, so a future change to `BuildReverse`'s scatter order fails a test
rather than silently corrupting a binary search.

The executor's snapshots are the ordered ones: `csrPairFromGraph`
(`cypher/api.go:17260`) builds the forward with `BuildFromAdjListLive` (which
calls `OrderRuns`) and the reverse with `fwd.BuildReverse()`. Note that
`csr.FromArrays` deliberately does *not* order — its contract requires the caller
to call `OrderRuns` — but no read path reaches the executor through it, and the
shipped probes in `csrprobe.go` already depend on the same guarantee, so the seek
adds no new assumption.

### 3.6 A pre-existing gap the seek closes

In columnar mode `op.inputRow` is never populated — `advanceInputChunk` reads the
source from `cScratch` and leaves `inputRow` nil. `dstMatchesInto` therefore hits
its `intoCol >= len(op.inputRow)` guard and returns `true` unconditionally, so
**the #2206 expand-into filter is silently inert on the columnar path today**.
Results are unaffected (the `Selection` above still decides), but the
optimisation is absent.

The seek must read the bound destination mode-aware — from `inputRow` in row
mode, from `cScratch` at column `intoCol`, row `cRow` in chunk mode — which
closes the gap and makes both modes benefit. Results stay byte-identical between
modes, as they already are.

---

## 4. Emission order (SPIKE question c)

**Verdict: the seek is order-*preserving*, not merely multiset-preserving.
`SuppressReorder` is NOT applied to it. It REMAINS applied to the anchor swap.**

### 4.1 The argument

Let a source's run be `[start, end)`, ordered by `(destination, handle)`, and let
`D` be the bound destination.

- Enumerate-and-filter walks positions `start, start+1, …, end-1` ascending and
  emits the subsequence whose destination is `D`.
- Because the run is ordered by destination first, the slots with destination `D`
  are **contiguous**. Call that block `[lo, hi)`.
- The emitted subsequence is therefore exactly `lo, lo+1, …, hi-1`, in ascending
  position order.
- `dstRun` returns precisely `(lo, hi)`, and `advanceFwdEdge` walks it ascending.

So the two paths emit **the same rows, at the same CSR positions, carrying the
same handles, in the same order**. The seek does not permute anything; it skips
positions that would have emitted nothing. This holds for `r > 1` (parallel
edges — the whole run is walked in handle order either way), `r = 0` (both emit
nothing), a self-loop (`u == D`, an ordinary member of `u`'s own run), and
`DirBoth` (the argument applies independently to the forward run and then the
reverse run, and their concatenation order is unchanged).

Because emission order is *identical*, there is no order change for any operator
above to observe, and the order-safety question does not arise. This is the same
reasoning by which the hash-join substitution retired its own order check
(#2234): it emits the nested loop's sequence position for position.

The anchor swap is a different proposition — re-rooting genuinely changes which
endpoint drives the scan and therefore the emitted order — so `SuppressReorder`
stays mandatory there, exactly as shipped.

### 4.2 What the TCK actually observes, measured

Counted in `cypher/tck/features/` at HEAD:

| Comparison form | Scenarios |
|---|---|
| `the result should be, in order` | **87** |
| `the result should be,` (bag) | 1346 |
| `the result should be (ignoring element order for lists)` | 16 |

`cypher/reorder_order_safety.go:24` states 86; the current count is **87**. The
drift is immaterial to the predicate's logic but the comment should be corrected
to a measured figure in #2153.

Two properties confirmed from the runner and features, both load-bearing for the
anchor swap and both unaffected by the seek:

- an unordered result is compared as a **bag**, so permuting it is conformant
  (openCypher 9 "Order of results"; CIP2016-06-14 — order is unspecified absent
  an order-establishing operator);
- an unordered comparison still compares list **cells** exactly, so a `collect()`
  built in arrival order *is* order-sensitive even in an "any order" scenario.
  This is why `SuppressReorder` treats a `collect()`-carrying projection or
  aggregation as an observer (`:83`–`:92`), and why the 16
  `ignoring element order for lists` scenarios are the only exception.

---

## 5. The symmetric anchor swap (SPIKE question d)

### 5.1 What shipped, and why

`computeAnchorSwaps` (`cypher/anchor_swap_plan.go:317`) gates on

```go
if s.exp.Direction != ir.DirectionIncoming { continue }
```

so the peephole only ever flips a written `DirIn` expand **to** `DirOut`, never
the reverse. The reason (recorded at `:29`–`:43`) is a cost-model fidelity
argument, not a correctness one: a `DirIn` expand pays, per in-edge, the cost of
recovering the canonical forward-edge position — `lookupFwdEdgePos`, plus a
second such lookup under a type filter via `reverseEdgePassesFilter`. That
overhead is a function of the *source's out-degree*, which the aggregate
statistic `D(label, relType, dir)` cannot see. An OUT-ward swap *removes* the
overhead, so the modelled win is a lower bound on the real win and cannot
regress. A reverse-introducing swap *adds* an unmodelled cost, so it could.

The file's own header already anticipates this sprint (`:44`–`:56`): since #2142
both lookups are binary searches, so the per-in-edge overhead is
Θ(log(out-degree)) rather than Θ(out-degree), "measured 6.04× cheaper at
out-degree 4096". It is explicit that **lifting the restriction "needs a cost
model that carries the residual log term, not merely the observation that the
term got smaller."** That is what follows.

### 5.2 The revised cost model

Today both directions share one edge constant:

```
cost(anchor) = c_s·N(anchorLabel) + c_e·D(anchorLabel, R, dir)
c_s = 1, c_e = 8, margin = 2
```

The symmetric model splits the edge constant by direction and carries the
residual probe term:

```
cost_out(A→B) = c_s·N(A) + c_out·D(A, R, OUT)

cost_in (B→A) = c_s·N(B) + ( c_in + k·c_p·⌈log₂(1 + D(A, R, OUT))⌉ ) · D(B, R, IN)
```

where

- `c_out`, `c_in` are the per-edge costs of walking one out-edge and one in-edge
  **excluding** forward-position recovery;
- `c_p` is the cost of one binary-search level;
- `⌈log₂(1 + D(A, R, OUT))⌉` is the probe depth, keyed on the **mean out-degree of
  the label at the far end of the in-edge** — the node whose forward range is
  searched;
- `k` is the number of probes per in-edge: **`k = 2`** whenever a type filter is
  present, because `reverseEdgePassesFilter` probes and then `lookupFwdEdgePos`
  (or `…ByHandle`) probes again. A matched anchor site always carries exactly one
  relationship type, so a filter is always present and **`k = 2` for every
  candidate this peephole can see**. `k = 1` is unreachable here and must not be
  assumed as the common case.

`c_out`, `c_in` and `c_p` must be **measured, not carried over**: #2089a's
β_out ≈ 19 ns predates #2142. #2150 calibrates all three on this machine and
records the measurement in a code comment beside the constants, as the task
requires.

### 5.3 The two-sided trustworthiness gate

The swap is admitted only when **every** condition below holds. The first five
are shipped and change in no way; conditions 6 and 7 are new and exist solely to
keep the reverse-introducing direction sound.

1. Exactly one relationship type (`D` is a per-type statistic).
2. Both endpoints' counts `EstExact ∧ ¬dirty`, sampled from **one** count-store
   snapshot. A relabel-dirtied `D` cell still vetoes to the written order.
3. Order-safe: `SuppressReorder(spine) == false`.
4. The shape is a standalone single-label, single-type, single-hop pattern
   (`matchAnchorSite`).
5. Strict win under the margin: `cost(candidate)·margin ≤ cost(written)` **and**
   `cost(candidate) < cost(written)`.
6. **New — the probe-depth input must itself be exact and non-dirty.** The
   reverse cost depends on `D(A, R, OUT)`, a *third* statistic that the OUT-ward
   direction never needed. If that cell is not `EstExact ∧ ¬dirty`, the probe
   depth is unknown, so the candidate is not costable and the swap vetoes.
7. **New — asymmetric margin for the reverse-introducing direction.** An OUT-ward
   swap removes unmodelled cost; an IN-ward swap adds it. The aggregate `D`
   cannot see degree skew, and a hub at the far end makes the true probe depth
   exceed `log₂(1 + mean)`. The IN-ward direction therefore requires the margin
   to be met against a **pessimistic** probe depth, and the OUT-ward direction
   keeps its shipped margin unchanged.

The prior round of this work had its headline lever regress and get backed out
for want of exactly this kind of conservatism. Condition 7's asymmetry is
deliberate: the two directions do not carry symmetric risk, so they must not
carry a symmetric gate. **The swap must remain behind `DisableAnchorSwap`, and
P3's adversarial hub and dirty-veto regression tests must pass unchanged.**

---

## 6. Excluded shapes (SPIKE question e)

| Shape | Disposition | Mechanism |
|---|---|---|
| Variable-length expand `[*1..n]` | excluded | a distinct operator (`ir.VarLengthExpand`); the seek is on `ir.Expand` only. Its neighbour loop is a BFS **enumeration** that must visit every neighbour — a membership probe is the wrong primitive (settled in #2142). |
| `shortestPath` / `allShortestPaths` | excluded | distinct operators (`ir.ShortestPath`); never carry `IntoVar`. |
| `OPTIONAL MATCH` onto an unbound interior | excluded at run time | the NULL-padded cell is not a bare `NodeID`, so §3.4's fallback declines the seek and the full range is walked. No planner-side exclusion needed, and none should be added — the run-time check is the one that is always right. |
| Bound cell holding a boxed entity | excluded at run time | §3.4 fallback; the `Selection` above decides. |
| Self-loop `(a)-[:K]->(a)` | **included** | `D == u`; an ordinary member of `u`'s own ordered run. `DirBoth`'s existing self-loop dedup is unaffected (§4.1). |
| `DirBoth` / undirected | **included** | the argument applies per side; concatenation order unchanged. |
| Non-multigraph (no handle column) | **included** | `dstRun` needs destination ordering only; handles refine order within a run but are not required. |
| CSR from `csr.FromArrays` without `OrderRuns` | not reachable | the read path builds via `BuildFromAdjListLive` + `BuildReverse`; the shipped `csrprobe.go` consumers already assume the same invariant. |

The task text also lists "OPTIONAL MATCH across the boundary onto an unbound
interior" as an exclusion. It is excluded, but note the mechanism differs from
what a planner-side veto would give: the run-time fallback covers it *and* every
other undecidable cell, which a static exclusion could not.

---

## 7. What #2149–#2154 must carry forward

1. The disabled baseline is the **HEAD column of §1**, not the audit's numbers.
   #2152 must record that the audit's reference points are unreproducible and why.
2. The headline claim is **per shape**: 2-cycle/mutual close targets an exponent
   materially below 1.79 approaching **1.33**; the triangle stays near **2.0**,
   bounded by its intermediate cardinality.
3. The reverse-CSR ordering invariant is now **load-bearing** and needs a
   permanent test in `graph/csr` (#2151).
4. `EXPLAIN` must render `ExpandInto` distinctly from `Expand` (#2153), which is
   how the operator's presence stays observable.
5. `reorder_order_safety.go:24`'s "86 in-order scenarios" should read **87**
   (#2153).
6. Backlog, out of scope here: `passesFilter` uses a `map[uint64]string` keyed by
   a dense CSR position — 17.8% of the degree-64 profile in a hash lookup that a
   position-indexed slice or bitmap would make a load.
