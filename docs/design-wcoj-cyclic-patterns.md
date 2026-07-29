# Worst-case-optimal join for cyclic patterns — design SPIKE

> **rmp #2155** (sprint 315, "Planner R2-P5: worst-case-optimal join for cyclic
> patterns"). Base commit `9a08389bba50a90068bd3c8e7751ea1179acd9d8`, 2026-07-29.
> Consulted: `graph-theory-expert` (mandatory), `cypher-expert-consultant`.
>
> ## VERDICT: **GO**, narrowly scoped — after a self-correction
>
> **This document reached NO-GO in its first two revisions and that verdict was
> WRONG. The error was mine and it was a measurement error, recorded here in full
> because the project's evidence mandate makes an uncorrected verdict worse than a
> late one.**
>
> The NO-GO rested on a typed cyclic pattern measuring 0.61×–0.81× the incumbent.
> **That comparison was not like-for-like**: the binary arm performed *no
> relationship-type checking at all*, while the intersection arm was charged the
> full reverse-side type cost. Correcting it — both arms paying their own real
> costs, and using the `revToFwd` mapping that **already exists** in
> `cypher/exec/revtofwd.go:101` so a reverse type check is an `O(1)` indexed load
> plus a map probe rather than an `O(log d)` `firstDstPos` search — inverts the
> result on the same adversarial clique:
>
> | clique `m` | untyped | **typed, FAIR** | typed via type column |
> |---:|---:|---:|---:|
> | 9 900 | 7.45× | **1.45×** | 4.40× |
> | 22 350 | 7.93× | **1.50×** | 5.05× |
> | 39 800 | 9.01× | **1.56×** | 5.47× |
> | 89 700 | 10.68× | **1.54×** | 6.04× |
>
> Result counts agree in every row and at every size. **There is no regression**,
> so obligation (c) is *met*: the intersection is 1.45×–1.56× faster with existing
> machinery, on the fixture built specifically to strip it of every advantage
> (equal degrees, so §1's work terms tie exactly; every 2-path closing, so the
> §3.1 materialisation saving is zero). The map probe dominates *both* arms and
> merely compresses the win; the slot-aligned type column (§7) is an **amplifier
> to 4.4×–6.0×, not a precondition**.
>
> **What remains unconditionally NO-GO** is the operator *as the sprint framed it*
> — a general worst-case-optimal join. That is structural (§2): every simple cycle
> admits exactly **one** intersection, at the vertex the sprint-314 `ExpandInto`
> seek already occupies, so Leapfrog Triejoin and Generic Join both degenerate on a
> binary relation, and genuine multi-way intersection needs `K4` or denser. The
> thing worth building is far narrower: a **fusion of the last two `Expand`s of a
> cycle** into one intersecting expand, scoped to triangles.
>
> Gating stays runtime per-row on `O(1)` CSR offsets and **never** on the count
> store, which is provably blind here — for the homogeneous triangle the two
> candidate legs carry the *identical* signature (`D(P,K,OUT)` and `D(P,K,IN)`,
> both equal to `E(K)`), and `cypher/anchor_swap_plan.go:454-457` already records
> in-source that `D` cannot see degree skew.
>
> **Revision history of this verdict**, kept deliberately: r1 NO-GO (premise
> refuted, §1) → r2 NO-GO with a "conditional GO after a prerequisite" → **r3 GO,
> narrowly scoped**, once the r1/r2 regression measurement was found to be
> asymmetric (§4.1). Three of my own conclusions were overturned by re-measuring my
> own harness. §7 is retained and reframed: the type column is an amplifier and an
> independent win, not a gate.

## Contents

- [1. The premise as written is refuted](#1-the-premise-as-written-is-refuted)
- [2. There is no operator-shaped gap](#2-there-is-no-operator-shaped-gap)
- [3. What the intersection does win — and where it goes](#3-what-the-intersection-does-win--and-where-it-goes)
- [4. The no-regression gate — and the measurement error that faked a failure](#4-the-no-regression-gate--and-the-measurement-error-that-faked-a-failure)
- [5. Semantics: recoverable, but the surface is larger than assumed](#5-semantics-recoverable-but-the-surface-is-larger-than-assumed)
- [6. The TCK is blind to this operator](#6-the-tck-is-blind-to-this-operator)
- [7. The type column: an independent amplifier](#7-the-type-column-an-independent-amplifier)
- [8. Durable findings and disposition](#8-durable-findings-and-disposition)

---

## 1. The premise as written is refuted

The sprint was opened on the claim that binary-join plans "provably cannot meet
the AGM bound (Atserias–Grohe–Marx, FOCS'08): triangle enumeration over `m` edges
costs `Θ(m²)` … against the `Θ(m^1.5)` worst-case-optimal bound", and that
"§2.3 measured GoGraph currently paying that quadratic price".

Three separate problems, each verified rather than argued.

**§2.3 measured a different mechanism, and it was fixed last sprint.** §2.3's
subject is the *closing hop* — a bound destination that expanded the whole
adjacency, exponent 2.02 **in out-degree**. That is exactly what #2149 fixed; the
closing hop is now `O(log d + r)`. §2.3's numbers are additionally marked
superseded in the audit and documented as non-reproducible at HEAD by
[`bench/expandinto`](../bench/expandinto/expandinto.go), which re-ran the audit's
own harness and found it 8.7×–14.0× faster with 14×–88× fewer allocations.
**§2.3 cannot motivate this sprint: its finding shipped in sprint 314.**

**`Θ(m²)` versus `Θ(m^1.5)` compares two worst-case bounds, not two plans on a
graph.** On a given graph the two plans' work terms are `Σ_v d_in(v)·d_out(v)`
(2-paths enumerated) against `Σ_(a,b)∈E min(d_out(b), d_in(a))` (merge steps).

**Those two terms are *exactly equal* on any regular graph.** For in-degree and
out-degree `d` throughout: the first is `n·d²`; the second is `m·d = n·d²`.
Identical, with no constant-factor slack. **A worst-case-optimal join has no
work-term advantage whatsoever on a uniform-degree graph.** The separation is a
function of degree skew alone.

### 1.1 Measured

Exact combinatorial oracle (both work terms are *counts*, so they carry zero
measurement noise) plus a prototype intersection over the real forward/reverse CSR
arrays, cross-checked against the engine's own `count(*)`. Exponents are
least-squares fits of `log(work)` on `log(m)`, with the same degenerate-input
rejection as `bench/expandinto.FitExponent`. Query:
`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*)`.

| Regime | binary-join work | intersection work | ratio at largest `m` |
|---|---:|---:|---:|
| Uniform degree 9, sweeping `n` (m 18k→486k) | **1.000** | **1.000** | **1.00×** |
| Power-law (Holme–Kim), sweeping `n` (m 32k→864k) | **1.112** | **1.008** | **2.99×** |
| Uniform, fixed `n`, degree 8→128 (m 72k→1.03M) | **2.000** | **2.000** | **1.00×** |

The exponent **is** 2.000 — but only in the *dense* regime, and **there the
intersection is 2.000 too**. Sweeping `n` at fixed degree the same quantity is
**linear**. A claim of "quadratic in `m`" is regime-dependent and meaningless
unless the regime is stated. The only genuine asymptotic separation is on the
skewed fixture, 1.112 against 1.008 — real, monotonic across the sweep (ratio
2.12 → 2.41 → 2.70 → 2.99), and a `~m^0.10` effect that would need `m ≈ 10⁹` to
reach 10×.

### 1.2 And the skew condition is narrower than it looks

The `graph-theory-expert` refuted my own restatement of the skew premise. With
degree exponent `γ` and natural cutoff, `Σ_v d² ≈ n^{1+(3-γ)/(γ-1)}` crosses
`m^{1.5}` at **`γ = 7/3 ≈ 2.333`**. Citation and collaboration graphs commonly sit
at `γ ∈ [2.5, 3.0]`, where today's plan already does **less** work than the
`m^{1.5}` scale. Worse, `Σ_v d_in·d_out` only blows up when in- and out-degree are
**correlated at the middle vertex**: making them independent collapsed that term
by ~560× at `γ = 2.1`, and the canonical dependency/citation shape (heavy
in-degree, small bounded out-degree) yields a **1.00×** opportunity because
`d_out(b)` is already minimal.

So the win requires a **conjunction**: a triangle specifically, `γ < 7/3`, in/out
degree correlation, *and* per-row asymmetry between the two legs. Miss any one and
the benefit is ≈1×.

> The `graph-theory-expert` grounded the `Σ min ≤ O(m^{1.5})` chain in
> Chiba–Nishizeki (SIAM J. Comput. 14(1):210–223, 1985) Lemma 2 + Lemma 1(a),
> giving the sharp constant `√2` rather than 2, and noted the `+n` term matters on
> an LPG where a label scan can leave `n ≫ m`. It also supplied the cleaner
> pointwise fact: since `min(x,y) ≤ (x+y)/2`, **`Σ min ≤ ½·Σ_v d(v)²` on every
> graph** — the intersection is never asymptotically *worse* than the enumeration.
> That is what made the measured regression in §4 surprising enough to be worth
> isolating, since it cannot come from the work term.

---

## 2. There is no operator-shaped gap

The SPIKE asked which algorithm fits: Leapfrog Triejoin (Veldhuizen, ICDT'14),
Generic Join / NPRR (PODS'12), or a specialised triangle intersection. **The
answer is none of them, and the reason is a property of the pattern graph rather
than of the algorithms.**

In a vertex-at-a-time plan over a simple cycle `C_k`, every vertex except the last
has exactly **one** already-bound neighbour; only the final vertex has two. So a
worst-case-optimal join over any simple cycle performs exactly **one**
intersection, at the last vertex — and that is precisely where sprint 314's
bound-destination seek already sits. Confirmed by the engine's own `Explain` at
HEAD:

```
Filter
└─ Expand [ExpandInto seek]     ← the closing hop: already a 2-way intersection,
   └─ Filter                       implemented as "enumerate leg 1, probe leg 2"
      └─ Expand                 ← OPEN: materialises every c ∈ N_out(b)
         └─ Filter
            └─ Expand           ← a→b
               └─ NodeByLabelScan [P]
```

Consequences:

- **LFTJ's index precondition is satisfied but hollow.** LFTJ needs each relation
  trie-indexed in every attribute order the variable order uses. For a *binary*
  relation there are only two orders and GoGraph has both (`fwd`, and `rev` built
  unconditionally at `cypher/api.go:17312`). But with two attributes and one
  relation per pattern edge, LFTJ **degenerates into exactly the 2-way leapfrog
  above**. There is no trie depth to exploit; the machinery buys nothing.
- **Generic Join collapses the same way**, and NPRR's partitioning targets the
  general hypergraph case that binary edge relations never present.
- **Genuine ≥3-way intersection requires a vertex with ≥3 bound neighbours** —
  i.e. `K4` or denser. **Not** cycles of any length, and **not** diamonds (a
  diamond's sink has two). Vanishingly rare in Cypher.
- **A longer cycle adds nothing over the triangle.** For `C_k` the single
  intersection sits at the last vertex and all `k−2` middle hops keep today's
  cardinality. `ρ*(C_k) = k/2` coincides with the best left-deep plan's largest
  intermediate for every `k ≥ 4`, so **the triangle is the only member of the
  cycle family with a real asymptotic gap.**

The `cypher-expert-consultant` reached the same conclusion from the semantic side
independently: the exact formulation is "a **pruning filter in front of the
existing `ExpandInto`**, not a replacement for it". Two consultations converging
on the same structural finding from different directions is the strongest signal
in this SPIKE.

---

## 3. What the intersection does win — and where it goes

The intersection is not worthless. Two effects were measured, and both are real.

**Cost proportional to output rather than to intermediates.** The binary plan
materialises one row per 2-path and discards those that do not close; the
intersection prunes before materialising.

| Fixture | `m` | 2-paths materialised | rows output | ratio | engine alloc | prototype alloc |
|---|---:|---:|---:|---:|---:|---:|
| Uniform | 486k | 4 374 000 | 162 000 | **27.0×** | 259 MB | **0 B** |
| Power-law | 288k | 12 093 612 | 713 346 | **17.0×** | 637 MB | **0 B** |

**A sequential merge instead of a random-access probe per 2-path.** On a clique —
built so that every degree is equal (work terms tie exactly, §1) and every 2-path
closes a triangle (materialisation advantage exactly zero) — the intersection is
still **8.2×–10.3× faster while performing 0.3%–1.0% *more* steps**. That is the
`log d` factor made concrete: the incumbent pays an `O(log d)` binary search per
2-path, the merge pays a sequential advance. Both arms return identical counts.

**And §4 is where both of these go.** Neither survives a typed relationship
pattern, which is why they appear here as characterisation rather than as
justification.

---

## 4. The no-regression gate — and the measurement error that faked a failure

> **RETRACTION.** This section originally reported a 19%–39% regression and was the
> sole basis for a NO-GO verdict. **The measurement was invalid.** Its binary arm
> did no relationship-type checking while the intersection arm paid the full
> reverse-side cost, so it compared an untyped incumbent against a typed
> challenger. §4.2 gives the corrected, like-for-like result: **no regression, and
> a 1.45×–1.56× win.** The invalid figures are kept below rather than deleted,
> because the *shape* of the error is the reusable lesson — an asymmetric cost
> model is far easier to write than to notice.

### 4.1 What was measured, and why it was wrong

The reverse CSR carries **no relationship-type information**, so a typed pattern
must, for every reverse slot it advances over, establish that slot's type.
`Expand.reverseEdgePassesFilter` (`cypher/exec/expand.go:698`) does this by
recovering the slot's *forward* position with an `O(log d)` `firstDstPos` search
and then probing `map[uint64]string` (`cypher/api.go:17422` populates that map with
one entry per accepted slot across the whole graph; `passesFilter`,
`expand.go:803`, probes it per slot).

Charging the intersection that cost while charging the incumbent nothing produced:

| clique `m` | untyped intersection | intersection + reverse check, incumbent UNCHARGED |
|---:|---:|---:|
| 9 900 | 7.45× faster | 0.74× — **invalid** |
| 89 700 | 10.68× faster | 0.63× — **invalid** |

Two things were wrong. First, the asymmetry above. Second, `firstDstPos` is not the
only way to type-check a reverse slot: **`buildRevToFwd`
(`cypher/exec/revtofwd.go:101`) already precomputes a `[]uint64` mapping every
reverse slot to its forward slot in one `O(V+E)` pass**, making the check an `O(1)`
indexed load plus a map probe. The engine already owns the machinery the NO-GO
claimed was missing.

### 4.2 The corrected, like-for-like result

Both arms now pay their own real costs. Today's plan is all-**forward** (scan `a`,
expand `a→b`, expand `b→c`, then seek `a` in `c`'s forward run), so it pays one map
probe per forward slot examined and touches no reverse run. The intersection needs
`N_in(a)`, so it additionally pays a `revToFwd` load plus a map probe per reverse
slot — a cost the incumbent genuinely avoids, and it is charged here in full.

| clique `m` | binary (fair) | intersection (fair) | **speed-up** | counts |
|---:|---:|---:|---:|:--:|
| 9 900 | 14.59 ms | 10.03 ms | **1.45×** | agree |
| 22 350 | 53.56 ms | 35.68 ms | **1.50×** | agree |
| 39 800 | 126.73 ms | 81.11 ms | **1.56×** | agree |
| 89 700 | 476.26 ms | 310.08 ms | **1.54×** | agree |

**Obligation (c) is met.** The intersection is never slower, on the fixture chosen
to be maximally hostile to it. The whole series is internally consistent: 7.4×–10.7×
with no type filter (the pure access-pattern win of §3.2), compressed to ~1.5× when
a per-slot map probe dominates *both* arms, and restored to 4.4×–6.0× when that
probe becomes a slot-aligned column load (§7). The map is the compressor, not the
blocker.

Residual risks, recorded rather than dismissed: behaviour at degree 1–2 is
unmeasured (the `≈16` crossover in
[`design-degree-adaptive-adjacency.md`](design-degree-adaptive-adjacency.md) §2.2
is scan-versus-seek and must not be reused for merge-versus-seek); and
columnar-`ChunkProducer` precedence, where widening a recogniser has twice before
in this project stolen shapes from the columnar chain.

### 4.3 What the original section got right

The count store genuinely **cannot** gate this, and that conclusion survives: for
the homogeneous triangle both candidate legs carry the identical signature
`D(P,K,OUT)` = `D(P,K,IN)` = `E(K)`, so a plan-time gate is asked to distinguish
two alternatives it returns the same number for. Gating must be **runtime per-row
on `O(1)` CSR offsets**, never plan-time on `D`.

The intersection's entire advantage is `O(1)` per advance. But the **reverse CSR
carries no relationship-type information**, so a typed pattern must, for every
reverse slot it advances over, recover that slot's *forward* position with an
`O(log d)` binary search and then probe the type map. Verified in source:

- `Expand.reverseEdgePassesFilter`, `cypher/exec/expand.go:698` — calls
  `firstDstPos`, with the in-source comment "`O(log d)` since rmp #2142".
- `Expand.passesFilter`, `cypher/exec/expand.go:803` — one `map[uint64]string`
  probe per slot, over a map `buildEdgeTypeFilter` (`cypher/api.go:17422`)
  populates with **one entry per accepted slot across the whole graph**.

Measured on the same clique fixture, adding exactly that per-reverse-slot cost and
nothing else (with a filter admitting *every* slot, so the measurement isolates the
lookup mechanism rather than selectivity):

| clique `m` | untyped intersection | **typed intersection** | verdict |
|---:|---:|---:|---|
| 9 900 | 8.39× faster | **0.81×** | 19% slower |
| 22 350 | 8.18× faster | **0.68×** | 32% slower |
| 39 800 | 8.85× faster | **0.69×** | 31% slower |
| 89 700 | 10.25× faster | **0.61×** | 39% slower |

**The mechanism that makes the merge fast is cancelled by the type check it cannot
avoid**, and the result is a regression on the operator's own target shape. Note
what this is *not*: not a constant-factor tuning problem, and not attributable to
the work term — §1.2 records that `Σ min ≤ ½ Σ d²` pointwise, so the work term
*cannot* regress. The regression is entirely in the per-advance cost.

Why no gate excludes it:

- **A plan-time gate is blind.** For the homogeneous triangle both candidate legs
  carry the identical count-store signature — `D(P,K,OUT)` and `D(P,K,IN)`, both
  equal to `E(K)`. The count store returns the *same value* for the two
  alternatives it would be asked to distinguish, and `anchor_swap_plan.go:454-457`
  already records in-source that `D` "is an ESTIMATE, not a bound … **D is an
  aggregate and cannot see degree skew**".
- **A runtime per-row gate on `O(1)` CSR offsets can see the degrees** — that part
  is sound, and it is how a future attempt should be gated — but it cannot see
  *this* cost, because the penalty is per-slot-advance and is paid by whichever
  leg is walked in reverse. Declining the operator whenever any leg is typed
  reduces its scope to untyped cyclic patterns, which are rare in practice.

Two residual risks are recorded rather than resolved, since they no longer change
the verdict: behaviour at degree 1–2 (unmeasured; the `≈16` crossover in
[`design-degree-adaptive-adjacency.md`](design-degree-adaptive-adjacency.md) §2.2
is scan-versus-seek and must not be reused for merge-versus-seek), and
columnar-`ChunkProducer` precedence, where widening a recogniser has twice before
in this project stolen shapes from the columnar chain.

---

## 5. Semantics: recoverable, but the surface is larger than assumed

Recorded because it is durable: **the semantics are not what blocks this.** Every
obligation is satisfiable, and the formulation below is verified against the
engine on nine hand-built graphs. But the surface is materially larger than the
SPIKE brief assumed, and one obligation is broken by the naive formulation.

**A destination-set intersection knows destination identity, not edge identity**,
and that breaks relationship isomorphism in three independent ways:

1. **A named closing relationship variable has nothing to bind.** `r3` in
   `MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a)` must be a concrete edge.
2. **Endpoint-pair collision.** On the minimal graph `x→y, y→z, z→x, x→x` the
   engine returns **3** and a naive intersection returns **4** — the spurious row
   binds `a=b=c=x` and reuses the single self-loop for all three legs. The exact
   correction: legs distinct → `mult(a→b)·mult(b→c)·mult(c→a)`; `a=b=c` with `k`
   self-loops → `k(k−1)(k−2)` ordered distinct triples. Verified — `k=1,2` give 0
   (engine 3) and `k=3` gives 6 (engine 9).
3. **Uniqueness scope is the whole MATCH clause, including sibling patterns.**
   Independently verified against the engine:

   | Query | rows |
   |---|---:|
   | triangle alone | 3 |
   | triangle `, (a)-[:K]->(b)` — **same** clause | **0** |
   | triangle then `MATCH (a)-[:K]->(b)` — separate clause | 3 |
   | triangle `, (a)-[:K]->(b)` with **2 parallel** `a→b` | **2** |

   The last row is decisive: with parallel edges the sibling consumes *one* handle
   and the cycle uses the other, so uniqueness is a **handle-level** fact that
   destination identity cannot decide. **The operator must publish every
   relationship handle it binds as a row column.** A repeated relationship
   variable within one path pattern is by contrast a compile-time error
   (`SyntaxError.RelationshipUniquenessViolation`, verified live), so it needs no
   runtime handling.

**Parallel-edge multiplicity is exactly recoverable** via run lengths, because the
CSR orders each run by the total key `(destination, handle)` — every parallel-edge
group is one contiguous run whose *length is the multiplicity*, locatable with the
existing `dstRun` seek. All nine cases agree with the engine: plain triangle 3;
parallel edges on one/two/three legs 9/18/24; one and two self-loops 3; three
self-loops 9; two triangles sharing an edge 6; no triangle 0.

So the exact formulation is: **intersect the distinct-destination projections to
prune, re-expand each leg's contiguous handle run, cross-product across legs, then
apply pairwise handle-distinctness across every relationship slot in the clause.**
The run-length product shortcut is exact only when no endpoint pair collides *and*
no sibling leg competes; otherwise enumeration is required.

Additional obligations, recorded for any future attempt: veto undirected legs
(`N_out ∪ N_in` is not one contiguous run, so the primitive does not exist there;
self-loops must be counted once, which `expand.go:591-601` exists for and which is
TCK-load-bearing); veto variable-length legs; no fusion across an `OPTIONAL MATCH`
boundary; degenerate to a membership test when the target is already bound; a
predicate on the closing *relationship* cannot be pushed into the intersection.
`OPTIONAL MATCH`'s null-row obligation is satisfied automatically —
`exec.OptionalApply` gates purely on whether the inner subtree yielded a row and
is operator-agnostic.

**A correction to the SPIKE brief**, since it would have misdirected the
implementation: `cypher/exec/cyphermorphism.go`'s `WithCyphermorphism` /
`NewExpandWithOptions` surface is used **only** by
`cypher/exec/operators_test.go`. Production sets the same field via
`ExpandConfig.RelCols` (`cypher/api.go:8062-8072`) and enforces it in
`Expand.passesRelMorphism` (`cypher/exec/expand.go:773`). Planning against the
`cyphermorphism.go` surface would wire into test-only code.

---

## 6. The TCK is blind to this operator

Independently verified by enumerating every `MATCH` pattern in all 220 feature
files: **the TCK contains no directed cycle over three or more distinct node
variables.** The only ≥3-node cycles anywhere are two occurrences of the same
pattern, and every leg is undirected — which §5 vetoes:

```
cypher/tck/features/clauses/match-where/MatchWhere2.feature:47   (a)--(b)--(c)--(d)--(a), (b)--(d)
cypher/tck/features/clauses/with-where/WithWhere2.feature:47     (a)--(b)--(c)--(d)--(a), (b)--(d)
```

Every other cyclic-shaped pattern is a **2-cycle** (`(a)-[:A]->(b), (b)-[:B]->(a)`;
`(n)-->(k)<--(n)`) or a `WHERE` pattern predicate. No cyclic scenario's graph
contains parallel edges.

**So `TCK 3897/3897` would have stayed green whether such an operator were correct
or badly wrong**, and the two areas where it actually breaks — multigraph
multiplicity and endpoint-pair collision — have no coverage at all. Any future
attempt must take its correctness evidence entirely from purpose-built absolute
oracles plus a white-box counter proving the operator fired; a result-identical
optimisation is invisible to a differential test.

> Verified while tracing this: `docs/reordering-design.md` §4's tally of
> `in order:` scenarios — **86 = 65 ORDER BY + 21 procedure** — is **correct**. A
> consultation reported it as 87 = 66 + 21; a direct recount of the feature files
> gives 86 = 65 + 21. Recorded so the correct figure is not "fixed" into an
> incorrect one later.

---

## 7. The type column: an independent amplifier

§4.2 settles that the intersection does not need this to avoid regression. What it
*does* need it for is to stop a per-slot `map[uint64]string` probe from dominating
both plans — and removing that probe is a win far beyond cyclic patterns.

**A slot-aligned relationship-type column** — a `[]uint32` parallel to the edges
array in both directions, replacing the `map[uint64]string` keyed by slot position
— removes it. The `graph-theory-expert` measured the primitive in isolation (Go
1.26 Swiss maps, 4Mi slots, `-count=5`): **25.5 ns/slot for the map against
0.43 ns/slot for a slot-aligned column, ~60×**. That figure is a *micro-benchmark
of the lookup, not an end-to-end GoGraph measurement*, and must be re-measured in
place before being quoted — but the direction is not in doubt, because the map
probe is per-slot on **every typed expand** in the engine, gateless and with no
cost model.

### 7.1 It is an AMPLIFIER, not a precondition — measured both ways

It would have been easy to leave §7 as an assertion that removing the blocker
"should" restore the win. It was measured instead, because the whole point of this
SPIKE is that an unmeasured "should" is what produced the sprint's original
premise.

The counterfactual gives **both** arms the same slot-aligned `[]uint32` type
column — so the type check is one indexed load on each side and the only remaining
difference is enumerate-and-probe against merge — on the same adversarial clique
fixture as §4:

| clique `m` | typed via map + seek (§4) | **typed via column** | counts agree |
|---:|---:|---:|:--:|
| 9 900 | 0.71× — a regression | **4.54× faster** | yes |
| 22 350 | 0.66× | **4.93×** | yes |
| 39 800 | 0.68× | **5.40×** | yes |
| 89 700 | 0.62× | **6.29×** | yes |

Read against §4.2's fair baseline of **1.45×–1.56×**, the column takes the same
fixture to **4.40×–6.04×**. So it roughly **triples an already-positive result**
rather than rescuing a negative one. The residual in both cases is the
memory-access difference of §3.2; the column simply stops the per-slot map probe
from dominating.

That settles the sequencing question the earlier revisions got wrong:

- The closing-leg intersection is **worth building now**, on existing machinery,
  because it does not regress (§4.2) — it does **not** wait on the column.
- The type column remains a **large, gateless, universal win** in its own right —
  it applies to every typed expand in the engine, not only cyclic patterns — and
  it multiplies this operator's benefit when it lands. It is an independent item,
  not a blocker.
- A general worst-case-optimal join stays **unconditionally out of scope** for the
  structural reason in §2.

> **Scope note.** Adding a type column changes the CSR's in-memory representation
> and the engine's hottest read path, which is an architecture decision beyond the
> mandate of this SPIKE. It stays its own item (`rmp` #2251), carrying this
> measurement so it starts from evidence instead of re-deriving it.

---

## 8. Durable findings and disposition

Findings worth keeping regardless of the verdict:

1. The AGM `Θ(m²) → Θ(m^1.5)` framing does not describe GoGraph's cost. The
   per-graph comparison is `Σ_v d_in·d_out` against `Σ min(d_out, d_in)`, and
   **these are equal on any regular graph**. Measured exponents: 1.000 uniform,
   1.112 power-law, 2.000 dense-regime — with the intersection at 1.000, 1.008 and
   2.000 respectively.
2. Every simple cycle admits exactly **one** intersection, at the vertex the
   `ExpandInto` seek already occupies. Multi-way intersection needs `K4`+.
   **This is the structural reason the sprint has no operator-shaped gap, and it
   is why the question should not be re-proposed in this form.**
3. The intersection's win is a `log d` factor plus a 17×–27× reduction in
   materialised intermediates. Measured on the adversarial clique: **7.4×–10.7×
   untyped, 1.45×–1.56× typed like-for-like (§4.2), 4.4×–6.0× with a slot-aligned
   type column (§7.1)**. A per-slot map probe compresses the win in BOTH arms; it
   never inverts it.
7. **An asymmetric cost model faked a NO-GO through two revisions of this
   document** (§4.1): the incumbent was charged no type-check cost while the
   challenger paid it in full. Neither consultation caught it, because both were
   reasoning about the mechanism rather than auditing my arithmetic. **When a
   measurement contradicts a structural argument — here, that `Σ min ≤ ½ Σ d²`
   pointwise, so the work term cannot regress — audit the measurement before
   accepting the contradiction.**
4. The semantics are fully recoverable; §5 records the exact formulation, the
   `k(k−1)(k−2)` self-loop term, and the clause-wide handle-publication
   requirement.
5. The TCK cannot gate this class of work at all (§6).
6. The count store is provably blind on the homogeneous triangle; gating must be
   runtime per-row on CSR offsets, never plan-time on `D`.

**Disposition.** #2155 closes with this NO-GO. #2160 executes its documented
NO-GO branch (this document plus Knowledge Graph provenance). #2156, #2157,
#2158, #2159 and #2161 close unstarted with this reasoning recorded, following the
evidence-based deferral precedent of the parallel-Expand task #2112. The
prerequisite in §7 and the two defects found while tracing §5 are raised as
separate backlog items rather than absorbed here.

**Disposition, as corrected.** The GO is narrow and its scope is exactly §2's
finding: build a **fusion of a cycle's last two `Expand`s** into one intersecting
expand for triangles — not a general worst-case-optimal join, which stays
unconditionally out of scope. #2156 (the primitive) and #2157 (the fused operator)
are therefore live work; #2158, #2159 and #2161 follow them. The slot-aligned type
column (#2251) is an independent, larger, gateless win that multiplies this
operator's benefit but does **not** gate it.
