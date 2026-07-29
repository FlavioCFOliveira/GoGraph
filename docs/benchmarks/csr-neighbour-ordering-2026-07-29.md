# Destination-ordered CSR neighbour runs — 2026-07-29 (rmp #2141/#2142/#2143, measured under #2145)

Sprint 313 made every CSR source's neighbour run ordered by the total key
`(destination, handle)`, turned the executor's five forward-position membership
probes into binary searches, and cached the forward/reverse CSR pair per
`Engine`. This document is the measurement of all three, together, against the
pre-sprint tree.

The permanent benchmarks are in [`bench/csrorder/`](../../bench/csrorder). Raw
recordings and benchstat output are in
[`history/csr-neighbour-ordering-2026-07-29/`](history/csr-neighbour-ordering-2026-07-29),
and the curated guard band is
[`history/LEDGER.md`](history/LEDGER.md) rows 0030–0031.

---

## 1. The headline, and the number that should be quoted

| fixture | time | B/op | allocs/op |
|---|---:|---:|---:|
| **Barabási–Albert power law (PRIMARY)** | **−7.37%** | **−58.47%** | −0.00% |
| RMAT scale=16 ef=16 | −28.69% | −58.39% | −0.00% |
| controlled degree 4096 | −81.76% | −56.56% | −0.00% |
| geomean, all 14 traversal benchmarks | −31.77% | −57.58% | +0.00% |

**Quote the power-law row.** The geomean and the degree-4096 row are dominated
by out-degrees that no real property graph reaches, and RMAT overstates the win
by **3.9×**. That gap is not incidental — it is the specific trap
[`design-degree-adaptive-adjacency.md`](../design-degree-adaptive-adjacency.md)
§8 forbids benchmarking into, and it is reproduced here end to end rather than
only as a `costFrac` argument.

So the honest summary is: **a modest win on a realistic graph, a large win on
hub-heavy shapes, and no regression anywhere** — which is what the corrected §2.2
predicted, and nothing like the 30.9× the sprint was opened on.

---

## 2. Per degree, never aggregated

Interleaved A/B, n=10 per arm, every row p=0.000. The arc count is held constant
at 512 Ki by [`csrorder.HubFixtureArcs`](../../bench/csrorder/csrorder.go), so the
number of probes is identical at every degree and only the cost of one probe
varies.

| out-degree | pre-313 | HEAD | time | B/op |
|---:|---:|---:|---:|---:|
| 8 | 260.0 ms | 238.5 ms | **−8.28%** | −59.20% |
| 16 | 234.8 ms | 221.7 ms | **−5.60%** | −58.01% |
| 32 | 227.9 ms | 212.3 ms | −6.83% | −57.26% |
| 64 | 228.9 ms | 209.6 ms | −8.42% | −56.92% |
| 512 | 322.0 ms | 203.7 ms | **−36.75%** | −56.59% |
| 4096 | 1102.1 ms | 201.0 ms | **−81.76%** | −56.56% |

The undirected shape, which runs the forward pass as well, agrees: −10.24%,
−6.49%, −5.68%, −8.75%, −36.21%, −81.74%.

**There is no regression at any degree**, including the 8–16 band where the
probe is at or below its crossover. That is a stronger result than the design
document expected, and the reason is visible in the two columns together: the
`B/op` reduction is degree-INDEPENDENT.

### Attributing the win to the right change

The three commits have different signatures, so the differential can be split
without guessing:

- **The pair cache (#2143) is degree-independent.** `B/op` falls ~57% at every
  degree, and HEAD sits flat at 14.00 MiB regardless of fixture where the
  baseline scaled 32–34 MiB with it. This is the whole story at degrees 8–64,
  and it is why those rows improve at all — the ordering alone could not have
  paid for itself there.
- **The ordering and the O(log d) probe (#2141/#2142) are degree-dependent.**
  They are the additional −28 percentage points at degree 512 and −73 at 4096,
  on top of the cache's baseline.

Reporting the sprint as one number would have credited the ordering with the
cache's win at low degree.

---

## 3. The write-path cost, not hidden

### 3.1 CSR build

Both arms in one binary: the unordered arm reproduces passes 1–2 of
`csr.BuildFromAdjList` through
[`csrorder.UnorderedArrays`](../../bench/csrorder/csrorder.go) and stops before
its pass 3. Median of 10.

| out-degree | unordered | ordered | delta | `OrderRuns` alone | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|---:|
| 8 | 1.911 ms | 4.819 ms | +152.2% | 2.686 ms | 0 | **0** |
| 16 | 1.425 ms | 5.652 ms | +296.6% | 4.202 ms | 0 | **0** |
| 32 | 1.357 ms | 8.861 ms | +553.0% | 7.729 ms | 0 | **0** |
| 64 | 1.144 ms | 10.578 ms | +824.3% | 9.246 ms | 1 024 | 2 |
| 512 | 0.834 ms | 16.524 ms | +1881.0% | 15.333 ms | 8 192 | 2 |
| 4096 | 0.665 ms | 23.018 ms | **+3363.4%** | 22.079 ms | 65 536 | 2 |

**Ordering makes a CSR build 2.5× to 34× more expensive.** This is the real
price of the change and the reason #2143's pair cache is load-bearing rather
than an optimisation: without it, this cost is paid on every query.

Two facts this independently confirms in [`graph/csr/order.go`](../../graph/csr/order.go):

1. **Its allocation contract is exact.** The documented claim is that a CSR whose
   longest run is within `runOrderInsertionCutoff` allocates *nothing at all*.
   The cutoff is 32, and degrees 8–32 allocate zero while degree 64 and above
   allocate exactly two buffers sized to the longest run. The transition lands
   precisely on the constant.
2. **The unordered arm getting cheaper as degree rises is not an anomaly.** The
   sweep holds arcs constant, so a higher degree means fewer sources; 1.911 ms →
   0.665 ms is the O(V) term shrinking, not the O(E) term.

### 3.2 Checkpoint

Interleaved A/B, n=10 per arm, every row p=0.000. This one requires a
cross-commit run: unlike the CSR build, there is no way to ask the snapshot
writer for its pre-#2141 behaviour in-binary, which is why
`BenchmarkSnapshotWrite` lives in its own file that compiles against both trees.

| out-degree | pre-313 | HEAD | delta | absolute |
|---:|---:|---:|---:|---:|
| 8 | 101.2 ms | 112.1 ms | +10.72% | +10.9 ms |
| 16 | 87.84 ms | 99.15 ms | +12.88% | +11.3 ms |
| 32 | 84.46 ms | 102.06 ms | +20.84% | +17.6 ms |
| 64 | 86.40 ms | 113.02 ms | **+30.82%** | +26.6 ms |
| 512 | 164.0 ms | 196.4 ms | +19.74% | +32.4 ms |
| 4096 | 667.8 ms | 706.7 ms | +5.82% | +38.9 ms |
| geomean | 138.7 ms | 161.6 ms | **+16.52%** | |

`B/op` +0.05% and `allocs/op` +0.00%, so this is pure CPU — the ordering pass
plus the canonical label and edge-property collectors #2141 introduced. No
memory cost.

**Read the absolute column, not only the percentage.** The percentage is
non-monotonic and peaks at degree 64, which looks like the cost model
misbehaving. It is not: the absolute delta rises monotonically, +10.9 ms →
+38.9 ms, exactly as O(Σ d log d) predicts. The percentage falls at the top only
because the baseline itself grows faster there (667.8 ms against 86.40 ms).

---

## 4. The probe primitive, and the refuted audit

`BenchmarkProbe_*` makes the #2139 calibration permanent. §12 of the design
document records that the calibration harness was kept outside the working tree,
and §2.4 of
[`audit-planner-vs-neo4j-memgraph-2026-07-25.md`](../audit-planner-vs-neo4j-memgraph-2026-07-25.md)
is refuted partly *because* its harness was also unavailable. A load-bearing
measurement that cannot be re-run is not evidence, so it is now committed.

Median of 10, 64 MiB arena, unsubtracted:

| out-degree | linear hit | binary hit | ratio | §2.2 published ratio |
|---:|---:|---:|---:|---:|
| 8 | 14.30 ns | 19.77 ns | 0.72× | 0.58× |
| **16** | 33.08 ns | 30.42 ns | **1.09×** | **1.01×** |
| 32 | 62.20 ns | 47.92 ns | 1.30× | 1.45× |
| 64 | 102.70 ns | 59.23 ns | 1.73× | 2.04× |
| 512 | 251.00 ns | 106.85 ns | 2.35× | 2.57× |
| 4096 | 1017.00 ns | 159.00 ns | **6.40×** | **6.04×** |

This independently reproduces the corrected §2.2: **the crossover is at
out-degree ≈16 and the win at 4096 is ~6×.** It therefore also independently
refutes audit §2.4, which put the crossover at 64 and claimed 30.9×.

The arena size is a declared parameter, not an incidental one. §2.2 measured
binary search at 7–12 ns per *level* in a cold arena against 1–4 ns/level when
the array is L1-resident, because each level is a dependent load that cannot
prefetch. The real fixtures here are ~4 MiB of edges — L2-resident, which
*flatters* binary search. A 64 MiB arena is the honest regime, and
`BenchmarkProbe_Control` provides the floor to subtract before comparing against
§2.2's control-subtracted figures.

`BenchmarkProbe_Linear_Hit_Unordered` answers the obvious objection to
comparing both algorithms on the ordered array: a scan's cost is
order-independent, so the comparison isolates the algorithm rather than
confounding it with the layout.

---

## 5. Fixture skew is reported with every result

Every benchmark reports its fixture's measured out-degree distribution via
`b.ReportMetric` — `meanDeg`, `maxDeg`, `%vtxT16`, `%edgeT16`, `%costT16`,
`%costT64` — because a traversal number cannot be read without the skew that
produced it. `costFrac` is the share of Σd² above the threshold: the linear-scan
cost model, and the only one of the three fractions that predicts a speed-up.

Measured on the fixtures used above:

| fixture | nodes | arcs | max out | mean/src | costFrac@16 | costFrac@64 |
|---|---:|---:|---:|---:|---:|---:|
| Barabási–Albert n=20k m₀=8 | 20 000 | 319 928 | 719 | 16.00 | 87.22% | 60.79% |
| RMAT scale=16 ef=16 | 65 536 | 955 616 | 6 306 | 23.66 | 99.69% | 97.77% |

The soak-layer `TestDistribution_Reproduces24Table` reproduces design-document
§2.4's published leverage table at its original parameters, and it **confirms
it**: Barabási–Albert n=100k m=8 measures costFrac 89.28%/67.69% against a
published 89.21%/67.18%, and RMAT measures 99.69%/97.79% against 99.68%/97.78%.
So the *leverage* half of §2.4 is sound; only its probe-cost column is refuted.

### A definitional trap found while reproducing it

§2.4's RMAT row publishes "avg out" 14.58, and the reproduction measured 23.66 —
a 1.62× miss that looked like a generator regression. It is not. §2.4 divides
arcs by **every interned node**; the reproduction was dividing by **arc-bearing
sources**, and RMAT is directed and skewed enough that 25 149 of its 65 536 nodes
end up with no out-arc at all. On the Barabási–Albert rows the two definitions
agree exactly, because that generator is undirected and every node carries at
least m₀ arcs — which is precisely why the difference stayed invisible until
RMAT. `DegreeProfile` now carries both means, and the soak test compares against
the all-nodes one.

---

## 6. Evidence discipline: the back-to-back A/B was wrong

**The first measurement of this change was invalid, and its own control caught
it.** `BenchmarkProbe_*` is byte-identical in both trees, so it was included as
an environment control: every row must come out flat. Run back-to-back — each
tree measured to completion in sequence — **22 of its 36 rows came out
"statistically significant", from −11.02% to +4.04% at p ≤ 0.02.**

The consequences were not cosmetic:

- The back-to-back run reported `TraversalReverse/degree=16` at **+2.93%
  (p=0.002)**, which would have been filed as a real low-degree regression. The
  interleaved measurement puts it at **−5.60%**.
- Run 0031's curated guard band reported `cypher_ldbc` geomean **+2.12%** with
  10 of 15 benchmarks "significant" at +1.80…+3.95%. Interleaved, **all 23
  curated benchmarks are statistically flat** (geomeans +0.32% ldbc, +0.98%
  alloc, −1.77% search, +0.07% centrality; `allocs/op` +0.00% exactly).

Both rejected recordings are kept, prefixed `REJECTED-`, so the error is
inspectable rather than merely described.

**This generalises beyond this sprint.**
[`scripts/bench-history.sh`](../../scripts/bench-history.sh) compares each run
against the previous one, which is back-to-back by construction. On this hardware
that manufactures spurious ±2–4% "significant" regressions on the
microsecond-scale curated set. **Any claim that hinges on a few percent needs an
interleaved A/B; the ledger's own deltas are only trustworthy for large effects.**

A second harness defect was hit and fixed while doing this: the interleaved
curated runner initially omitted `bench-history.sh`'s `strip_log_noise` filter,
so a `cypher` WARN line split each benchmark's name from its result and benchstat
rejected all 230 samples. That is the same failure that made a 2026-07-28 run
lose 90 of 92 samples while looking clean — the filter is not optional for any
package that logs.

---

## 7. What is guarded

`bench/csrorder` carries its own correctness guards, because a wrong harness
reports a wrong number with full confidence:

- `TestUnorderedArrays_MatchesOrderedAfterOrdering` — ordering the unordered
  arm's output must reproduce `csr.BuildFromAdjList` array for array, so the
  copied layout logic cannot drift and silently invert the reported cost delta.
- `TestUnorderedArrays_AreActuallyUnordered` — if the fixture's destinations
  arrived ascending, `BenchmarkOrderRuns` would measure the already-ordered fast
  path instead of a real sort.
- `TestSearchFirstDst_MatchesScan` — the binary probe must return the *same*
  slot as the scan it replaces, over a run dense in parallel edges, across every
  sub-range.
- `TestBuildHubGraph_DegreeIsExact` — the swept degree is the degree the fixture
  actually has, and the arc count is constant across the sweep.
- `TestProbeArena_MissKeysAreInRange` — an out-of-range miss key would let both
  algorithms bail early and measure nothing.
- `TestPowerLawFixtureIsSkewed` / `TestRMATOverstatesTheWin` — the primary
  fixture must stay a power law, and RMAT must keep overstating, or the contrast
  it exists to provide is gone.
- The benchmarks themselves assert their work: the hit arm fails unless every
  probe hit, the miss arm unless every probe missed.

The short layer runs in ~4 s. The §2.4 reproduction is soak-gated because the
Barabási–Albert generator is O(n²) by construction.
