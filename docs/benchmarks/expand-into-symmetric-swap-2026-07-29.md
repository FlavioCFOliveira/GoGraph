# Expand(Into) seek and the symmetric anchor swap — measurements

Sprint 314 (rmp #2149, #2150), measured under #2152 at commit `7c14fd57`.
Apple M4, `darwin/arm64`, Go benchmarks with `-benchmem`.

Harness: [`bench/expandinto/`](../../bench/expandinto). Design and the reasoning
behind each decision: [`design-expand-into-symmetric-swap.md`](../design-expand-into-symmetric-swap.md).

---

## 1. What was measured, and why this way

Both arms of every comparison run **in one process**, toggled by `EngineOptions`,
rather than across two commits. That is deliberate: sprint 313 established
empirically that a back-to-back A/B on this machine manufactures significance — a
byte-identical control produced 22 of 36 "significant" rows spanning −11%…+4% and
invented two phantom regressions. A same-process A/B sees the same binary, the
same fixture and the same thermal state, and `-count` interleaves the arms.

The claim about the closing hop is **asymptotic**, not a constant factor, so the
harness sweeps out-degree and fits the growth exponent from the series. A single
degree cannot demonstrate a change in growth, and a ratio between two endpoints is
at the mercy of whichever endpoint carries the most fixed cost.

## 2. The audit's reference points are not reproducible

`audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.3 is the finding this sprint
was opened on. Its harness — 20 000 `:CyN` nodes, ring-structured out-degree 8 and
64, `MATCH (a:CyN)-[:F]->(b:CyN)-[:F]->(a) RETURN count(*)` — was reproduced
exactly at the sprint-314 base commit `9edebf60`.

| Out-degree | Audit, 2026-07-25 | Sprint-314 base, 2026-07-29 | Change |
|---|---|---|---|
| 8 | 625.8 ms · 222 MB · 9.68 M allocs | **71.98 ms · 9.71 MB · 686 K allocs** | 8.7× faster · 22.9× less memory · 14.1× fewer allocs |
| 64 | 41.68 s · 12.80 GB · 577.9 M allocs | **2.981 s · 319 MB · 6.54 M allocs** | 14.0× faster · 40.1× less memory · 88.4× fewer allocs |
| Growth for 8× degree | 66.6× | **41.4×** | — |
| Empirical exponent | **2.02** | **1.79** | — |

The audit's points predate two changes that already addressed most of what it
measured: **#2206**, which stopped the operator building and boxing a row per
neighbour (that is where the 12.8 GB went), and **#2142**, which made the
read-path forward-position probes O(log d).

**Consequence for the acceptance criteria.** #2152 asked for the audit's reference
points to be reproduced "as the disabled baseline". That is unattainable by
construction — the code that produced them no longer exists. The disabled arm of
the benchmarks below is the honest baseline, and the table above is the record of
why.

**The defect itself was real and did survive.** An exponent of 1.79 is still
super-linear: the closing hop still walked Θ(d) slots per input row. What the two
earlier changes altered was the magnitude, not the kind.

## 3. Where the time was going

CPU profile of the degree-64 closing query at the base commit (10.21 s of samples):

| Symbol | Share | Note |
|---|---|---|
| `Expand.advanceFwdEdge` | 37.9% cum | the enumeration |
| ┗ `Expand.passesFilter` | 18.6% cum | of which `runtime.mapaccess2_fast64` **17.8%** |
| ┗ `Expand.passesRelMorphism` | 16.7% flat | cyphermorphism check |
| `Expand.dstMatchesInto` | 0.98% | the #2206 filter itself |
| `runtime.madvise` | 20.3% | GC returning the 319 MB/op to the OS |

This is what made the seek worth doing, and it is not what the audit's framing
suggested. The per-slot cost was **not** the destination comparison (1%) — it was
the edge-type filter's hash lookup and the cyphermorphism check, both of which a
seek skips entirely on slots that cannot emit.

It also surfaced an unrelated defect, recorded for the backlog: `passesFilter`
performs a `map[uint64]string` lookup per edge slot, keyed by a **dense CSR
position**, where a position-indexed slice or bitmap would be a load.

## 4. Closing hop: the seek against the filter

`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN count(*)`, 20 000 nodes, `-count=6`.
Raw output and benchstat:
[`history/expand-into-symmetric-swap-2026-07-29/`](history/expand-into-symmetric-swap-2026-07-29)
(`ab-sameprocess-head.txt` and `ab-sameprocess.delta.txt`).

Those files are deliberately **outside** the numbered history chain. `bench-history.sh`
takes the immediately-previous numbered file as its benchstat baseline, so a run with a
different benchmark set sitting in the chain leaves the next curated run with no common
benchmark names and an empty comparison — which is exactly what happened when this was
first filed as run 0032. Sprint 313 established the named-directory pattern for the same
reason.

| Benchmark | filter (disabled) | seek (enabled) | Δ time |
|---|---|---|---|
| `ClosingHop/degree8` | 69.06 ms ± 1% | 57.13 ms ± 2% | **−17.27%** (p=0.002) |
| `ClosingHop/degree32` | 526.3 ms ± 3% | 260.2 ms ± 1% | **−50.56%** (p=0.002) |
| `ClosingHop/degree64` | 2 459.7 ms ± 2% | 548.6 ms ± 3% | **−77.69%** (p=0.002) |

The gain **grows with degree** — 1.21× → 2.02× → 4.48× — which is the signature of
Θ(d) becoming O(log d + r) rather than a constant-factor win.

Fitted growth exponent over degrees 8→64 (measured at 3 000 nodes by
`TestClosingHopExponentFalls`, which gates it):

| Arm | Fitted exponent |
|---|---|
| filter (seek disabled) | **1.249** |
| seek (enabled) | **0.809** |

Read the absolute exponents as end-to-end, not as the operator's own: a run's fixed
cost (CSR build, label scan) is itself Θ(n·d), so it dilutes the low degrees and
pulls both fitted values down. What the comparison establishes is the **difference**
between the arms.

**Allocations are flat between the arms** — B/op and allocs/op are statistically
indistinguishable at every degree (p ≥ 0.22). #2206 had already removed the
per-neighbour row construction, so what remained to win was pure CPU. Any claim of
an allocation win on this shape would be false.

## 5. The triangle is bounded by its own intermediate result

`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a)` does gain — −5.86% at degree 4,
−16.61% at 8, −31.00% at 16 (all p=0.002) — but its fitted exponent stays at
**1.562** with the seek enabled and rises towards 2 with degree.

This is not a shortfall. A triangle's middle hop is *open*, so it materialises
Θ(n·d²) intermediate rows however the closing hop executes. The seek removes the
third hop's Θ(d) factor; it cannot remove an intermediate result the pattern
genuinely has, and Neo4j pays the same cost.

So the sprint's headline is stated **per shape**: the 2-cycle / mutual-relationship
close is where the exponent moves. A blended figure across both shapes would
misstate the result. `TestTriangleExponentStaysQuadratic` pins this limit so a
future reader does not expect a win the plan shape cannot deliver.

The sprint description and #2152 both quoted a target of "about 1.1". For the
range measured that figure is arithmetically wrong: `d·log d` over 8→64 gives
`log(384/24)/log 8 = 1.333` exactly. 1.1 is the asymptotic limit of `d·log d` for
large d, not what an 8→64 window can yield.

## 6. Symmetric anchor swap

`MATCH (a:Hub)-[:R]->(b:Leaf)` — a written `DirOut` single edge whose cheaper
anchor requires a reverse expand, so it was vetoed before #2150. The hub has
`hubOut` out-edges; the whole `:Leaf` label has one incoming edge.

| Hub out-degree | written (OUT-only) | swapped (symmetric) | Δ time | Δ B/op | Δ allocs/op |
|---|---|---|---|---|---|
| 1 601 | 105.51 µs ± 1% | 6.62 µs ± 1% | **−93.73%** | −97.41% | **+75.45%** |
| 40 000 | 1 924.61 µs ± 1% | 5.94 µs ± 1% | **−99.69%** | −97.42% | **+75.45%** |

All at p=0.002, `-count=6`. A best-of-5 sweep including a larger fixture measured
12.4× at hub out-degree 1 601, 331.8× at 40 000 and 2 303.7× at 200 000. Row counts
are identical throughout.

The swapped plan's cost is **flat in the hub's degree** because it no longer walks
the hub at all — it scans 50 `:Leaf` nodes and walks the single in-edge — so the
win grows without bound as the hub grows.

### 6.0 The allocation-count regression, and why it is accepted

The swapped plan makes **more allocations of much smaller size**: 193 against 110
per operation (+75.45%) while total bytes fall by 97.4% (361.3 KiB → 9.3 KiB). The
project's mandate is that a change regressing allocs/op must not be merged *without
a documented justification*, so here it is.

The written plan makes few, enormous allocations because it materialises the hub's
whole out-neighbourhood; the swapped plan makes a handful of small ones per scanned
`:Leaf` node. In absolute terms the regression is **83 extra allocations per query**,
fixed and independent of graph size, bought for a 16×–324× reduction in time and a
39× reduction in bytes. It does not grow with the hub's degree, whereas everything
it buys does.

The trade is therefore accepted deliberately. It is recorded here rather than
smoothed over, because the headline time figure would otherwise hide it.

### 6.1 Direction constants, recalibrated

#2089a's `β_out ≈ 19 ns` predates #2142 and was not carried forward. Fixture:
4 000 `:A` and 4 000 `:B` nodes, `a_i → b_{(i+j) mod n}` for `j < d`, so
out-degree(a) = in-degree(b) = d and **both** directions scan n nodes and walk n·d
edges — the only difference is the reverse path's forward-position recovery.

| out-degree d | ns/edge OUT | ns/edge IN | ratio |
|---|---|---|---|
| 4 | 300.16 | 309.96 | 1.03 |
| 16 | 291.37 | 321.58 | 1.10 |
| 64 | 326.11 | 364.72 | 1.12 |
| 256 | 374.44 | 417.77 | 1.12 |
| 1 024 | 398.68 | 459.50 | **1.15** |

Fitting the **difference** of the two arms against `log₂(d)` — the difference,
because per-edge cost grows with d from cache pressure in *both* arms and that
growth would otherwise be charged to the probe:

```
in-edge penalty = 2.01 ns + 5.76 ns · log₂(d)
```

against a marginal out-edge of ≈400 ns. So the reverse overhead is 3% at degree 4
rising to only 15% at degree 1 024, while the admissibility margin already demands
100% headroom.

### 6.2 Why lifting the restriction is sound, not merely cheap

The guarantee does not rest on the probe-depth estimate being tight — it cannot
be, since `D` is an aggregate blind to degree skew *and* the mirror walks in-edges
whose sources may carry any label. It rests on a **structural bound**: a probe
depth is at most 64 levels, so the per-in-edge penalty is at most
`12·64 + 4 = 772 < 800`, one modelled edge. A true in-edge therefore costs under
*twice* a modelled out-edge for any graph, and the 2× margin absorbs it.

That inequality is enforced by a declaration that **fails to compile** if it stops
holding, rather than by a test that could be skipped. It was verified to bite by
deliberately violating it.

This is the change in kind since #2089a: an unmodelled Θ(out-degree) term is
unbounded relative to the edge cost, so no constant margin can absorb it; a
Θ(log out-degree) term is bounded and the margin can.

## 7. Inertness, and one interaction worth knowing

`BenchmarkOpenControl` runs the same two hops with a free destination, where no
bound-destination path exists. Both arms are statistically indistinguishable at
every degree — p = 0.699, 0.589 and 0.699 for degrees 8, 32 and 64 — as required:
the flag must change nothing where it does not apply. That is the control that makes
the closing-hop deltas attributable to the access path rather than to the flag
perturbing something unrelated.

**Widening the anchor swap steals shapes from the columnar chain.** Making the
swap symmetric admits `(a:A)-[:K]->(m:B)` — the *common* written single-edge form
— and re-rooting it breaks the columnar chain, which the count-based cost model
cannot see. This surfaced as two failing columnar shape-identity tests. Measured
at 200 000 nodes on those very queries, swapping beats the columnar plan by
1.31×, 1.32×, 1.83× and 3.12×, so the cost model's choice is right and it was the
test's scope that had to narrow. Recorded here because the cost model remains
blind to execution mode, and a future change to either side should know it.

## 8. Reproducing

```bash
# Short layer: degrees 8, 32, 64 plus the swap and the controls.
go test -run='^$' -bench=. -benchmem -count=6 ./bench/expandinto/

# Soak layer adds out-degree 128 and 256, and hub out-degree 200 000.
go test -run='^$' -bench=. -benchmem -count=6 -tags=soak ./bench/expandinto/

# The exponent gates (short layer, run by `make ci`).
go test -run 'Exponent' -v ./bench/expandinto/
```

The high degrees are soak-gated because the **disabled** arm is the expensive one:
its cost is Θ(n·d²), and at 20 000 nodes out-degree 64 alone is ~2.5 s per
operation. The enabled arm is cheap at every degree, which is the point.

## 9. Curated-suite regression check

Run 0032 of the numbered history is sprint 314's curated check, compared against run
0031 (sprint 313's change commit).

**No regression is evidenced.** The deterministic counters — the ones that do not
depend on timing — are exactly flat: B/op geomean +0.00% on `cypher_alloc` and +0.05%
on `cypher_ldbc`, allocs/op +0.00%. Time geomean is +0.93% and +0.55%, with individual
rows scattering in both directions (IC10 −2.21%, IC9 −1.48%, IC11 −0.87% against
IC3 +2.29%, IC2 +1.95%).

Those sub-1% time deltas are **below the demonstrated validity threshold of this
comparison** and are not read as a regression. `bench-history.sh` compares runs
back-to-back by construction, and sprint 313's byte-identical control produced 22 of 36
"significant" rows spanning −11%…+4% on this machine — see run 0031's own caveat. A
few-percent time claim here would need interleaved measurement; a flat allocation
profile does not, which is why the flat counters carry the verdict.

There is also no mechanism by which the curated set should move: both changes are gated
and inert off their target shapes, which `BenchmarkOpenControl` confirms directly at
p ≈ 0.6–0.7 between arms.
