# Fused cyclic expand — measurement record

> **rmp #2159** (sprint 315), measuring #2156/#2157/#2158. Machine: Apple M4,
> darwin/arm64, 10 cores, Go 1.26.5. Raw and deltas:
> [`history/cyclic-join-2026-07-30/`](history/cyclic-join-2026-07-30/).
>
> **Both arms run in ONE process**, toggled by
> `EngineOptions.EnableCyclicIntersect`. Sprint 313 established that
> `bench-history.sh` compares runs back-to-back *by construction* and that on this
> machine a byte-identical control produced 22 of 36 "significant" rows spanning
> −11%…+4%. A same-process A/B is immune to that: one binary, one fixture, one
> thermal state, with `-count` interleaving the arms. The consequence for the
> record is that this run takes **no numbered LEDGER row** — see sprint 314's
> unnumbered row for why a heterogeneous set breaks the numbered chain.

## Headline

`n=6` per arm, `p=0.002` on **every** qualifying shape; geomean **−54.24% sec/op**
and **−70.35% B/op**.

| Shape | two-Expand | fused | sec/op | allocs/op |
|---|---:|---:|---:|---:|
| Triangle, uniform d=4 | 16.59 ms | 10.13 ms | **−38.96%** | −63.55% |
| Triangle, uniform d=16 | 103.78 ms | 23.97 ms | **−76.91%** | −91.96% |
| Triangle, uniform d=64 | 1718.7 ms | 145.2 ms | **−91.55%** | **−98.30%** |
| Triangle, power-law | 303.9 ms | 127.9 ms | **−57.91%** | −79.59% |
| 2-cycle, d=16 | 10.266 ms | 4.848 ms | **−52.77%** | −73.69% |
| Square (4-cycle), d=8 | 244.41 ms | 68.90 ms | **−71.81%** | −86.70% |
| Triangle, d=1 | 1897.9 µs | 925.0 µs | **−51.26%** | −61.43% |
| Triangle, d=2 | 10.851 ms | 8.739 ms | −19.47% | −37.98% |
| Triangle, d=3 | 13.244 ms | 9.363 ms | −29.31% | −53.31% |
| **Non-qualifying: labelled** | 471.7 ms | 471.2 ms | **~ (p=0.394)** | ~ (p=0.937) |
| **Non-qualifying: acyclic** | 66.85 ms | 66.57 ms | **~ (p=0.589)** | ~ (p=0.195) |

The two non-qualifying rows are the control for every row above them: both arms
build the *same* plan there, so parity is the correct result and its presence is
what shows the recognition predicate is not touching declined shapes.

## The uniform fixture is the FLOOR, not a flattering case

This matters for reading the table. SPIKE #2155 proved that on a **regular** graph
the two plans' work terms are *exactly equal* — `Σ_v d_in(v)·d_out(v)` for the
binary-join plan and `Σ_(a,b) min(d_out(b), d_in(a))` for the intersection both
reduce to `n·d²`. So the uniform rows have **no skew advantage whatsoever**; what
they measure is purely the constant and the materialisation. The power-law row is
the only one where any work-term separation exists, and it is *smaller* (−57.91%)
than uniform d=64 (−91.55%) — which is the honest shape of this result and the
opposite of what an AGM-framed prediction would say.

## Fitted exponents, and a result stronger than the SPIKE predicted

Dense regime — fixed `n=2000`, degree sweep `{4, 8, 16, 32}`, so `m = n·(d+1)`
grows with degree. The regime is stated because the *same* quantity is quadratic
when degree grows at fixed `n` and linear when `n` grows at fixed degree; an
exponent quoted without its regime is meaningless, which was one of the SPIKE's
three premise refutations.

| Series | m = 10k → 66k | fitted exponent in m |
|---|---|---:|
| two-Expand | 7.98 ms → 170.60 ms | **1.628** |
| fused | 4.95 ms → 24.63 ms | **0.852** |

**This is a genuine exponent separation, and it is NOT the AGM effect.** The
SPIKE's combinatorial oracle predicted both *work terms* at 2.000 in this regime,
and that prediction stands — the work terms really are equal. The wall-clock
exponents separate for a different reason: the binary-join plan's cost is
proportional to the number of **intermediate** rows it materialises (`Θ(#2-paths)`)
while the fused operator's is proportional to its **output** (`Θ(#triangles)`), and
those two quantities have different exponents. So the win is asymptotic in this
regime, but because of row materialisation rather than set-intersection theory.

Two honest qualifications:

- The two-Expand series fits **1.628, not 2.000**. The work-term count is 2.000;
  wall time is lower because per-row cost is not uniform across the sweep. The
  measured figure is reported rather than the predicted one.
- The fused exponent **0.852** is sub-linear in `m` over this sweep, which reflects
  the ring fixture's triangle density rather than a universal property. It should
  not be read as "the operator is sub-linear".

## Residual risks: one resolved, one open

**Small degree — RESOLVED, but the `d=1` row does not mean what it appears to.**

The SPIKE recorded as *unmeasured* whether the merge's setup loses to a trivial scan
at degree 1–2, and flagged that a crossover would require a degree floor. Measured:
degree 1 **−51.26%**, degree 2 −19.47%, degree 3 −29.31%. Every point is a win, so
**no degree floor is needed** and none was added.

**But `d=1` measures the NO-OUTPUT path, not output-producing work**, and the first
draft of this report read it as the latter. A degree-1 ring reaches only `k±1`, so
closing a 3-cycle needs three offsets from `{+1, −1}` summing to `0 mod n`, which is
impossible for `n > 3`: the fixture contains **exactly zero triangles**. The row is
still worth having — "does the merge lose when there is nothing to find?" is
precisely the SPIKE's question, and the answer is no — but it is evidence about the
empty case only.

This was not caught by reading the benchmark output, which looked entirely healthy.
It was caught by `fixture_test.go`, added to this package for exactly that purpose
after sprint 314 recorded a fixture whose far endpoints sat at out-degree zero and
which consequently reported 1.06× for an operator that genuinely worked. That test
now pins the degree-1 triangle count at zero *deliberately*, so the row's meaning
cannot be silently misread again, and requires triangles to exist at every degree
≥ 2. The output-producing small-degree evidence is therefore **d=2 (−19.47%) and
d=3 (−29.31%)**, and those are the figures the no-crossover conclusion rests on.

**Columnar precedence — not exercised, therefore not cleared.** The recogniser
declines unless its child is *exactly* an `*exec.Expand`, which is what prevents it
claiming a shape the `ChunkProducer` chain would take, and it is also why the
declined rows above measure identical plans. No fixture here puts the two
recognisers in genuine competition, so this run does not establish precedence; the
structural veto does, and a dedicated test would be needed to establish it
empirically.

## Recommendation on the default

**Enable by default is supported by these measurements, and I recommend it — with
one caveat about what has NOT been measured.**

For:

- Every qualifying shape wins, at `p=0.002`, across degree 1→64, uniform and
  power-law, 2-cycle, triangle and square. There is no measured crossover.
- The gains are large where it matters and grow with degree (−38.96% → −76.91% →
  −91.55%), which is the signature of removing the `Θ(n·d²)` intermediate
  materialisation rather than a tuned constant.
- Non-qualifying shapes are statistically inert with **allocation counters
  identical**, which is the deterministic evidence this project weights above
  timings.
- Correctness is gated independently of the TCK, which is provably blind here: an
  ordered differential over 12 operator-level fixtures and 15 engine-level shapes,
  each paired with a white-box engagement counter, plus a `rapid` property against
  the binary-join plan, plus TCK 3897/3897 with the operator **enabled**.

Against, and the reason the flag is still opt-in as shipped:

- The labelled-pattern case declines, so the most common real-world spelling
  (`MATCH (a:Person)-[:KNOWS]->…`) gains nothing until the Selection-hoist
  follow-on lands. Enabling by default would advertise a win that many queries do
  not yet get.
- Columnar precedence is unestablished empirically (above).
- The operator falls back under `PROFILE`, so a profiled plan differs from the
  executed one — harmless but surprising, and worth documenting before it becomes
  the default.

**Concretely:** keep `EnableCyclicIntersect` opt-in for now and flip the default in
the same change that lands the Selection hoist, at which point the win covers
labelled patterns and a precedence test can be written against a shape that
actually contends. Nothing in this measurement argues against the operator; the
argument is only about *when* the default flips.
