# Planner Statistics Foundation — Design (P4, F5b)

Status: design spike (#2096, sprint 308). No production code changed by this
document. Specialist input: ai-mathematician (error bounds, the promotion
inequality, staleness) grounding the graph-theory-expert F5 ranking. Date
2026-07-24.

## 0. The one structural fact

Every selectivity is `S = C/N`. GoGraph already maintains **exact, always-fresh**
population counts (`N(label)`, `E(relType)`) in the count-store, so in every `S`
the **denominator N is exact** — only the numerator (in-range count, or NDV) is
ever approximate. This makes the promotion rule (§3) provably safe (an absolute
numerator error maps directly to an absolute error on `S`, no denominator
uncertainty) and lets staleness be measured as `Δ/N` against an exact `N`.

## 1. Structures and parameters

| Structure | Choice | Error bound | Memory / (label,prop) |
|---|---|---|---|
| **NDV** | HyperLogLog, `m = 2^12 = 4096` regs, 6-bit, 64-bit hash, HLL++ sparse + bias | rel. std err `1.04/√m = 1.625%` | 3 KiB (6-bit packed) |
| **Range** | **equi-depth** histogram, `B = 256` buckets | abs. selectivity err `≤ 1/B = 0.39%` per boundary (distribution-free) | 4 KiB (256 × [boundary, cumCount]) |
| **MCV** | **exact top-k, k = 32** (min-heap over the rebuild scan) — NOT Count-Min | exact (0 error) | ~0.5 KiB |
| **Staleness** | per-(label,prop) atomic dirty-write counter, reset at rebuild | drift `Δ/N` | 8 B |
| **Break-even** | `b` (CSR random-vs-sequential ratio; exact peephole used 0.10) — **calibrate empirically** | — | — |

Equi-depth is the *gating* histogram because its `1/B` worst-case bound is
**distribution-free** (Piatetsky-Shapiro–Connell SIGMOD'84); equi-width has no
such bound. Isolate the exact-MCV heavy values into singleton buckets so the
`1/B` bound survives skew (the MaxDiff mechanism, Poosala et al. SIGMOD'96). A
MaxDiff variant may be stored for sharper cost *display* but must NOT relax the
gate. NDV must **never** be sampled: Charikar et al. PODS'00 prove any sampler
touching `r` of `n` rows has worst-case ratio-error `≥ √((n−r)/(2r)·ln(1/γ))`
(≈8% of rows for 2×, ≈22% for 1.1×) — HLL touches every row at O(1) and gives a
distribution-free `1.04/√m` instead.

## 2. Off-write-path maintenance

Statistics are built by a **background full scan**, generation-stamped at `(g0,
N0)`, never on the write path (writes stay O(delta), like the count-store). A
per-(label,prop) atomic dirty-counter `Δ` is incremented O(1) on the write path
(the only write-path cost) and reset at rebuild. HLL **insert** is O(1) and
sound inline (registers only rise); HLL **delete is impossible** (cannot lower a
max) → deletes make NDV over-estimate → equality selectivity under-estimate (the
unsafe direction) → force rebuild once `deletes/N > tol` (recommend ~1%). The
histogram is likewise rebuilt (not invertible under deletes); its staleness is
the same `Δ/N` term.

## 3. The estStats promotion rule (load-bearing)

The range-seek `WHERE n.p <op> x` is a no-regression win iff true selectivity
`S_true < b`. With a certified absolute error `δ = δ_hist + δ_stale = 1/B + Δ/N`
on the point estimate `Ŝ` (denominator exact), the **upper-confidence-bound**
rule fires the seek iff:

```
Ŝ + 1/B + Δ/N + m_guard ≤ b
```

Proof: if it holds then `S_true ≤ Ŝ + δ ≤ b`, so the seek is at worst
break-even, a strict win when strict. Gating on the point estimate `Ŝ ≤ b` is
**unsafe** (`S_true` could reach `Ŝ + δ > b`). Cost of safety is negligible:
with `b=0.10, B=256`, the seek fires at `Ŝ ≤ 0.0961` vs the exact peephole's
`≤ 0.10` — a <4% loss of the firing region for a no-regression proof. Use the
**absolute** form (the equi-depth guarantee is natively absolute and
distribution-free); a relative form spuriously tightens near small `S`.

**Staleness → veto.** The firing region `Ŝ ≤ b − 1/B − Δ/N` closes as `Δ` grows;
demote `estStats → estFallback` (and schedule rebuild) when `Δ/N ≥ b − 1/B`
(≈9.6% of the population changed, for `b=0.10, B=256`), plus a wall-clock
max-age backstop.

## 4. Reduced scope — equality is NOT gating (a proof obligation, not tuning)

`Ŝ_eq = 1/NDV` is only the *distribution-average* equality selectivity; a
specific literal `v` can have `freq(v)` arbitrarily far from `N/NDV` under skew
(up to `N`). The HLL error (1.6%) is real but the **uniformity modelling error
is unbounded and dominates**, so no finite `δ` certifies `S_true < b` for a
chosen `v`. Therefore:

- **Range-seek (`n.p <op> x`): FULL estStats scope** — provably safe under the §3
  UCB rule. This is P4's primary consumer.
- **Equality-driven no-regression plans: reduced scope** — admissible only when
  `v` is in the **exact MCV list** (exact per-value count, effectively estExact);
  otherwise `1/NDV` is an `estHeuristic` **non-gating hint** and the planner
  falls back to the exact-count-only equality peephole (already shipped). HLL/NDV
  must never drive an absolute-no-regression equality decision.

The reordering peepholes (P3) are unaffected — they require estExact and never
consume statistics.

## 5. Numerical-stability requirements

1. **HLL harmonic-mean** via register-histogram `Σ_k c_k·2^{−k}` (exact dyadic;
   `math.Ldexp`, never `math.Pow`); guard linear-counting `m·ln(m/V)` against
   `V=0`.
2. **Histogram boundary comparison** uses the engine's openCypher orderability
   comparator (CIP2016-06-14 / `cmpInt64Float64`), NOT raw IEEE `<`: NaN rows are
   counted separately and EXCLUDED from the in-range numerator (a range predicate
   yields null on NaN); `−0.0 == +0.0`; int-vs-float boundaries via the exact
   comparator; one histogram per comparable value-domain.
3. Clamp `Ŝ` and the UCB `min(Ŝ + 1/B + Δ/N, 1)` to `[0,1]`; guard `1/D̂` for
   `D̂=0` and round `D̂` to an integer `≥1`.

## 6. Sub-tasks

- **#2097** — HLL-NDV per (label,prop) inline-insert + exact top-k MCV over the
  background scan; equality selectivity `1/NDV` exposed as `estHeuristic` (hint),
  MCV hits as `estExact`. Ships inert (no gating consumer changes).
- **#2098** — equi-depth histograms (B=256) built off the write path,
  generation-stamped, staleness-gated, with MCV-spike isolation; range
  selectivity exposed with the `1/B + Δ/N` certified error.
- **#2099** — widen the selective range-index seek to fire under the §3 UCB rule
  when a fresh estStats estimate is available (keeping the comparability guard and
  typed-index requirement); stale/heuristic → the exact-count peephole default.
- **#2100** — accuracy tests (HLL error, histogram quantile error vs ground
  truth); a `rapid` property test asserting the UCB gate **never fires when
  `S_true > b`** over skewed columns; staleness→veto test.
- **#2101** — benchmark: zero write regression (dirty-counter only on the write
  path); widened range-seek win on stats-gated ranges; **calibrate `b`
  empirically** against the CSR random-vs-sequential ratio (measure-to-decide).
- **#2102** — docs + observability metrics (stats size, staleness, rebuild
  count) + KG. **#2120** — observe via example 26.

Honest limit (carry to docs): P4 promotes single-column range selectivity to
estStats; equality stays exact-MCV-or-heuristic; multi-join independence is never
promoted (that stays out of every no-regression gate).
