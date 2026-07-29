# Worst-case-optimal join for cyclic patterns — design SPIKE

> **rmp #2155** (sprint 315, "Planner R2-P5: worst-case-optimal join for cyclic
> patterns"). Base commit `9a08389bba50a90068bd3c8e7751ea1179acd9d8`, 2026-07-29.
> Consulted: `graph-theory-expert` (mandatory), `cypher-expert-consultant`.
>
> ## VERDICT: **NO-GO**
>
> **The unmet obligation is (c), the no-regression gate.** On a typed cyclic
> pattern — `-[:K]->`, which is what essentially every real Cypher query writes —
> a sorted-set-intersection plan is **measured at 0.61×–0.81× the incumbent's
> speed, a 19%–39% regression**, on the exact shape the operator exists to
> improve. The cause is structural, not an implementation detail: the reverse CSR
> carries **no relationship-type information**, so every reverse-side advance must
> first recover its forward slot position via an `O(log d)` binary search
> (`Expand.reverseEdgePassesFilter`, `cypher/exec/expand.go:698`) and then probe a
> `map[uint64]string`. That is precisely the per-candidate cost the merge exists to
> eliminate, so the merge's only source of advantage — `O(1)` per advance — is
> cancelled by the type check it cannot avoid.
>
> No gate excludes this. The regression is the **default** case rather than an
> edge case, and no plan-time gate can even see it: for the homogeneous triangle
> the two candidate legs carry the *identical* count-store signature
> (`D(P,K,OUT)` and `D(P,K,IN)`, both equal to `E(K)`), and the count store is
> documented in-source as unable to see degree skew
> (`cypher/anchor_swap_plan.go:454-457`). The project mandate forbids shipping an
> optimisation that can be slower than the existing plan when no gate excludes the
> case, so the mandate forbids this operator **as scoped**.
>
> **The verdict has two parts, and only one of them is permanent.**
>
> - **Unconditional NO-GO on the operator as scoped.** A general
>   worst-case-optimal join is not warranted *at all*, for a structural reason
>   (§2): every simple cycle admits exactly **one** intersection, at the vertex
>   the sprint-314 `ExpandInto` seek already occupies, so Leapfrog Triejoin and
>   Generic Join both degenerate on a binary relation and genuine multi-way
>   intersection needs `K4` or denser. This should not be re-proposed.
> - **Conditional GO on the closing-leg intersection, strictly sequenced after one
>   prerequisite.** §7.1 *measures* rather than assumes it: give both plans a
>   slot-aligned `[]uint32` relationship-type column and the same clique fixture
>   inverts from a **0.62×–0.71× regression to a 4.54×–6.29× win that widens with
>   scale**. The blocker is not merely the cause — removing it is **sufficient**.
>
> So this is a **sequencing** finding, not an abandonment. The prerequisite is a
> larger and gateless win in its own right, and it is an architecture change
> beyond this sprint's objective, so it is raised as `rmp` #2251 carrying the
> measurement rather than absorbed here.

## Contents

- [1. The premise as written is refuted](#1-the-premise-as-written-is-refuted)
- [2. There is no operator-shaped gap](#2-there-is-no-operator-shaped-gap)
- [3. What the intersection does win — and where it goes](#3-what-the-intersection-does-win--and-where-it-goes)
- [4. The obligation that fails: the no-regression gate](#4-the-obligation-that-fails-the-no-regression-gate)
- [5. Semantics: recoverable, but the surface is larger than assumed](#5-semantics-recoverable-but-the-surface-is-larger-than-assumed)
- [6. The TCK is blind to this operator](#6-the-tck-is-blind-to-this-operator)
- [7. What would reverse this verdict](#7-what-would-reverse-this-verdict)
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

## 4. The obligation that fails: the no-regression gate

The SPIKE was directed to return NO-GO if no gate can exclude regression. This is
that finding.

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

## 7. What would reverse this verdict

The blocker in §4 is a *missing representation*, not a property of intersection
joins: the reverse CSR has no slot-aligned relationship type, so a typed reverse
advance costs `O(log d)` plus a map probe instead of `O(1)`.

**A slot-aligned relationship-type column** — a `[]uint32` parallel to the edges
array in both directions, replacing the `map[uint64]string` keyed by slot position
— removes it. The `graph-theory-expert` measured the primitive in isolation (Go
1.26 Swiss maps, 4Mi slots, `-count=5`): **25.5 ns/slot for the map against
0.43 ns/slot for a slot-aligned column, ~60×**. That figure is a *micro-benchmark
of the lookup, not an end-to-end GoGraph measurement*, and must be re-measured in
place before being quoted — but the direction is not in doubt, because the map
probe is per-slot on **every typed expand** in the engine, gateless and with no
cost model.

### 7.1 The prerequisite is measured SUFFICIENT, not merely necessary

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

**The verdict inverts on this one representation change**, from a 29%–38%
regression to a 4.5×–6.3× win that widens with scale — and it does so on the
fixture built specifically to strip the intersection of every other advantage
(equal degrees, so the work terms tie exactly; every 2-path closing, so there is
no materialisation saving). The residual is entirely the memory-access difference
of §3, restored once the type check stops being a search.

So the NO-GO of this document is **conditional and sequenced, not permanent**:

- **NO-GO as scoped** — a general worst-case-optimal join operator is not
  warranted at all, for the structural reason in §2 (one intersection per simple
  cycle, at the vertex `ExpandInto` already occupies; multi-way needs `K4`+). That
  part of the verdict is unconditional and should not be re-proposed.
- **The closing-leg intersection is warranted, strictly AFTER the type column**,
  scoped to triangles, gated per-row on `O(1)` CSR offsets, never on `D`.

The correct sequence is therefore: **type column first, then re-open the
closing-leg intersection.** The type column is not a competitor to this work; it
is a larger, gateless win that happens to be its precondition.

> **Scope note.** Adding a type column changes the CSR's in-memory representation
> and the engine's hottest read path, which is an architecture decision beyond the
> mandate of this SPIKE and beyond sprint 315's objective. It is therefore raised
> as its own item (`rmp` #2251) carrying this measurement, rather than absorbed
> here. The measurement is recorded now so that item starts from evidence instead
> of re-deriving it.

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
3. The intersection's win is a `log d` factor (8×–10× measured at clique degrees)
   plus a 17×–27× reduction in materialised intermediates — **both cancelled by
   the reverse-leg type check on any typed pattern (0.61×–0.81×), and both
   RESTORED (4.54×–6.29×) the moment that check becomes a slot-aligned column
   lookup (§7.1).** The blocker is a missing representation, not a property of
   intersection joins.
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

**The one open decision this SPIKE deliberately does not take.** §7.1 establishes
that the closing-leg intersection *is* worth having and that exactly one
representation change unlocks it. Making that change — a slot-aligned type column
in both CSR directions, replacing the per-slot `map[uint64]string` on the engine's
hottest read path — is an **architecture** decision, and it benefits every typed
expand rather than only cyclic patterns. It is therefore out of scope for a SPIKE
whose remit was the cyclic-pattern operator, and it is not absorbed here on the
author's own authority. #2251 carries the full measurement so that decision can be
taken on evidence.
