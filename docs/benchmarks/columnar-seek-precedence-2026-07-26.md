# Index seek versus columnar execution — measured

**Task #2204** · sprint 322 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=x -bench='BenchmarkNumericEqualitySeek' -benchmem -count=4 ./cypher/`

Benchmarks in `cypher/numeric_equality_seek_bench_test.go` (pre-existing, kept as the
standing gate); regression tests in `cypher/columnar_seek_precedence_test.go`.

---

## 1. An index made a query slower than not having one

The columnar recognisers fire at the `ir.Projection` level, **above** the `Selection`
where `buildOperator` applies the index-seek rewrites. So a recogniser that claimed a
`Selection` over a labelled scan silently discarded any seek that would have fired.

The same predicate, on the same indexed property, with only the RETURN shape differing:

| N (`:Person` population) | `RETURN p` (entity) | `RETURN p.age` (scalar) |
|---|---|---|
| 4 000 | 4.23 µs | **553 µs** |
| 16 000 | 4.15 µs | **2.24 ms** |
| 64 000 | 3.66 ms | **9.67 ms** |

And the scalar arm was **identical with the seek disabled** — 554 µs / 2.27 ms / 9.64 ms —
which proves the seek was never consulted at all rather than consulted and declined.

## 2. The fix

The columnar recogniser now declines when a covering seek would fire, and the query takes
the row-mode seek path. This is the same precedence rule #2186 established for the
min-cardinality label re-anchor and the anchor swap, and it follows from the asymptotics: a
seek is **sublinear** in the label population where the columnar chain is **linear** in it,
so the seek wins by an order of magnitude rather than a constant factor. Columnar execution
removes a constant factor; it must never pre-empt something that removes cardinality.

The probe (`indexSeekWouldFire`) is defined in terms of the **real** rewrites —
`tryBuildIndexSeekFromSelection`, `tryBuildIndexSeekSetFromSelection`,
`buildRangeSeekIfEnabled` — rather than re-deriving their conditions, because a probe that
guessed would drift from what `buildOperator` actually does, and the failure mode of drift
is silently losing the index again. Each mutates the schema map it is handed, so the probe
passes a **copy**, exactly as the columnar recognisers already probe their own shape
against a hypothetical schema.

## 3. Measured after

| N | `RETURN p.age`, seek enabled | before | speed-up |
|---|---|---|---|
| 4 000 | **5.12 µs** | 553 µs | **108×** |
| 16 000 | **4.90 µs** | 2.24 ms | **457×** |
| 64 000 | **3.67 ms** | 9.67 ms | 2.6× |

`ScalarSeek` now tracks `EntitySeek` at every population (5.12/4.90/3.67 against
4.23/4.15/3.66), and `ScalarScan` — the seek-disabled control — is unchanged at
560 µs / 2.26 ms / 9.60 ms, which is what attributes the gain to the seek rather than to
drift.

The 64 000 row is slower on **both** arms; that is a separate pre-existing effect (the
seek's own selectivity gate at that population), unchanged by this task and visible
identically in the entity arm.

## 4. No cost to unindexed workloads

The probe builds throwaway operators to answer a planning question — about **seven
allocations per query BUILD**, never per row. Measured on the columnar shape benchmarks,
whose fixture defines no index, that showed up as 127 → 134 allocations/op.

So the probe is gated on `idxMgr.Count() == 0` first: with no index registered no seek can
fire, and a workload that defines none never pays for the probe at all. `Manager.Count` is
one RLock over a map length. After the gate:

| Benchmark | allocs/op before | after |
|---|---|---|
| `ColumnarShape_ScanBaseline` | 127 | **127** |
| `ColumnarShape_ScanConjunctionSameProp` | 148 | **148** |
| `ColumnarShape_ScanConjunctionTwoProps` | 146 | **146** |
| `ColumnarShape_ScanIn` | 185 | **185** |
| `ColumnarShape_ScanNoLimit` | 126 | **126** |
| `ColumnarShape_ScanInertLimit` | 137 | **137** |

Exactly at baseline. Columnar shape coverage also holds at 9/15 scan and 7/7 hop
(`TestColumnarShapeCoverage`), because the yield fires only when an index genuinely covers
the predicate.

## 5. Correctness

`cypher/columnar_seek_precedence_test.go` asserts behaviourally rather than structurally,
by counting the columnar filter's batches:

- a scalar-projecting equality on an **indexed** property drains **zero** columnar
  batches, and the *same query without an index* still drains some — so the assertion is
  about the index, not about the shape being rejected outright;
- that holds at two populations, so a plan that grew with the population would fail;
- the result multiset is identical between an indexed and an unindexed engine over
  identical data, across five query shapes including a range predicate, a no-match
  equality, and a projection of a *different* property than the indexed one.

The fixture sits at 4 000 nodes deliberately: the range/numeric seek has its own
selectivity gate and declines below it, so at a token population the columnar path
legitimately claims the shape. Using the benchmark's own populations avoids asserting
against a gate rather than against the fix.

Test power verified by mutation: disabling the yield makes both engagement tests fail.

TCK 3897/3897. `make ci` green.
