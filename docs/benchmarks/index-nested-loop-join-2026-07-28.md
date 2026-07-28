# Index nested-loop join for a per-row-varying key — 2026-07-28 (rmp #2233)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harnesses `bench/r4audit/inlj_test.go` (new) and
  `bench/r4audit/load3_test.go`, build tag `r4audit`. `-count=1` on every run.
- Best of 5 (read) / 3 (write, each needing a fresh graph). B is the UNWIND batch size, N the
  indexed `:P` population.
- "before" is the tree at `3e6d00d8`.

## 1. Result

The shape is the UNWIND-bound equi-join `UNWIND $rows AS r MATCH (b:P) WHERE b.key = r.k …`, which
#2228 collapsed from 35m10s to 2.206s with a hash join. Both plans forced, same harness:

**Read**, `RETURN count(b)`:

| B | N | hash join | index seek | seek is |
|---:|---:|---:|---:|---|
| 500 | 20 000 | 11.262 ms | **203 µs** | **55.5× faster** |
| 500 | 80 000 | 52.970 ms | **205 µs** | **258× faster** |
| 5000 | 20 000 | 12.996 ms | **1.931 ms** | **6.7× faster** |
| 5000 | 80 000 | 58.604 ms | **2.101 ms** | **27.9× faster** |

**Write**, `SET b.touched = r.k`:

| B | N | hash join | index seek | seek is |
|---:|---:|---:|---:|---|
| 500 | 20 000 | 11.439 ms | **840 µs** | **13.6× faster** |
| 500 | 80 000 | 56.756 ms | **1.088 ms** | **52.2× faster** |
| 5000 | 20 000 | 20.146 ms | **7.822 ms** | **2.6× faster** |
| 5000 | 80 000 | 63.144 ms | **8.839 ms** | **7.1× faster** |

The shipped gate awards every one of these cells to the seek.

## 2. The crossover is not where the arithmetic said it was

#2228's decision record deferred this plan on an arithmetic comparison: at B=5000, N=20000,
Θ(N+B) ≈ 25 000 units against Θ(B log N) ≈ 71 500, so the hash join "is ahead here". #2233's
technical requirements inherited that and made it acceptance criterion 2 — *the gate must still pick
the hash join at that shape*.

**Measured, the seek is 6.7× faster at exactly that shape.** `TestIndexNestedLoopCrossover` sweeps
six batch sizes at N=20000 with each plan forced:

| B | B/N | index seek | hash join | seek is |
|---:|---:|---:|---:|---|
| 500 | 0.03 | 277 µs | 11.149 ms | 40.2× faster |
| 5 000 | 0.25 | 2.051 ms | 13.851 ms | 6.8× faster |
| 20 000 | 1.00 | 8.493 ms | 20.553 ms | 2.4× faster |
| 50 000 | 2.50 | 19.669 ms | 33.713 ms | 1.7× faster |
| 200 000 | 10.0 | 80.579 ms | 98.174 ms | 1.2× faster |
| 500 000 | 25.0 | 199.403 ms | 228.651 ms | 1.15× faster |

There is **no crossover in the measured range**. The advantage narrows monotonically — 40.2× down to
1.15× — and never inverts, up to a batch 25× the population.

The reason is that the two "units" are not comparable. Fitting the table:

```
one btree level      ≈  30 ns   — a bounded search inside a cached node
one hash build row   ≈ 550 ns   — allocates and copies a whole row
one hash probe row   ≈ 450 ns
```

A level is worth about 1/15 of a row. At N=20000 that puts 30·log₂N ≈ 429 ns **below** the 450 ns a
probe alone costs, so the seek's per-row price is under the hash join's before the N-row build is
counted at all. The unit-count model implicitly assumed the ratio was 1.

**Acceptance criterion 2 was therefore wrong as written, and was changed on this evidence** rather
than implemented against it. The gate survives — `indexNestedLoopWins` is still the named decision
point, and the regime where the hash join wins is real (the seek's advantage is heading towards 1×
and moves out with index depth, reaching B ≈ 1.1M at N=80000) — but its constant,
`indexSeekLevelsPerHashRow = 15`, is now measured instead of assumed. One ratio rather than three
nanosecond figures, because the absolute times are machine-specific while the ratio is a property of
the two inner loops. `TestIndexNestedLoopCrossover` is in-tree to re-derive it.

## 3. The gate is load-bearing, and the first version of it was catastrophically wrong

#2233 warned: "Do not add the operator without the gate." That was right, for a reason neither the
task nor this author anticipated.

The first implementation admitted the seek whenever a covering numeric index **existed**. A numeric
companion exists for *every* Cypher-created index — including one on a string-valued property, where
it is empty. The three-way load harness (`TestSeekReachesWriteStatements`) joins on the string key
`sid`, so the seek was admitted, every outer row's string key fell through to the operator's scan
fallback, and the shape went Θ(B·N):

| case (N=16000) | before | with the naive gate | regression |
|---|---|---|---|
| bound-key read | 9.157 ms | **3.335 809 s** | **364× slower** |
| bound-key write | 9.651 ms | **3.174 323 s** | **329× slower** |

That is the pre-#1506 nested loop, reintroduced by an optimisation. It was found by measuring the
*existing* harness rather than only the new one.

The fix is a **coverage proof** (`numericIndexCoversScan`): the companion must hold an entry for
every node the inner scan would produce. A node contributes at most one entry, so entries ≥ scanRows
proves every scanned node carries a numeric value for the property. A string-valued property, a
mixed property, or one with any gap is rejected and falls through to the hash join. After the fix:

| case (N=16000) | before | after | change |
|---|---|---|---|
| bound-key read | 9.157 ms | 8.795 ms | unchanged |
| bound-key write | 9.651 ms | 10.036 ms | unchanged |

The proof also closes a Θ(B·N) exposure that would have survived as a "correct but slow" path: with
coverage proved, a key of any non-numeric kind matches nothing, so the operator answers it with no
rows instead of scanning for them (`WithProvenNumericCoverage`). Without that, a batch of B string
keys against a covered numeric column would still have driven B full label scans to return nothing.

## 4. The hybrid, and the one case that still scans

The join key's type is unknown at plan time — `r.a` over an UNWIND may be numeric on one row and a
string on the next — so the operator dispatches per row:

| key | path | cost |
|---|---|---|
| NULL or NaN | emit nothing (`isUnjoinableKey`, shared with the hash join) | O(1) |
| numeric, exactly representable | seek the companion | Θ(log N) |
| any other kind, coverage proved | emit nothing — nothing can match | O(1) |
| integer with \|k\| > 2^53 | scan + filter, i.e. the nested loop for that row | Θ(N) |

The last row is the only remaining scan, and it is the one case where the fallback protects
*correctness* rather than coverage: past 2^53 the int64→float64 conversion rounds, so a seek could
land on a different integer. It is rare enough to be worth Θ(N) and too rare to be worth an exact
re-check path.

## 5. No regression elsewhere

- The three-way load harness is unchanged (§3), and its plan is unchanged: the string key fails the
  coverage proof, so the hash join #2228 installed still serves it.
- `TestHashJoinTrigger`'s verdict table still reports `HashJoin` for all ten read shapes — none of
  them has an UNWIND-bound per-row key, so none is eligible for the seek.
- `TestHashJoinOrder_SequenceMatchesNestedLoop`'s fifteen shapes are unaffected.

## 6. Correctness

`TestIndexNestedLoopJoin_DifferentialAgainstBothPlans` compares **three** answers, not two: the
seek, the hash join, and the plain nested loop — because the seek replaces the hash join, which
replaced the nested loop, and an error common to a pair reads as agreement. The comparison is the
full row SEQUENCE, position by position, since the substitution is order-preserving (outer-major,
ascending node ids within an outer row from every path).

On top of that a **hand-computed match count** is asserted, derived from the fixture's own
value-assignment rule rather than read back through the engine — all three plans share one graph and
one key-equality primitive, and two earlier audits in this project lost a real defect to a
differential whose arms shared the broken code.

Twelve key shapes: integer keys present, a key absent from the index, NULL, NaN, cross-type integer
→ float-valued nodes, cross-type float → integer-valued nodes, a string key, a boolean key, mixed
kinds in one batch, a fractional float, an integer beyond 2^53, and duplicate keys.

`TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage` is the regression gate for §3, over
three uncovered populations, each asserting the hash join fires instead — so a case cannot pass by
failing some unrelated part of the trigger.

Which plan ran is asserted from `indexNestedLoopBuildCount` / `hashJoinBuildCount` in every case, as
acceptance criterion 4 requires: `Engine.Explain` renders the planner's intent, and #2222 found that
intent and the built operator can diverge.

**A defect this suite caught in itself:** an earlier version had two fixture builders, one per arm.
They drifted, and the differential compared answers from two *different graphs* — reporting a row
count mismatch that looked exactly like an operator defect. There is now one builder.

## 7. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`, cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**.

## Reproduce

```bash
go test -count=1 -tags=r4audit -run 'TestIndexNestedLoopJoinBench|TestIndexNestedLoopCrossover|TestSeekReachesWriteStatements|TestHashJoinTrigger' -v ./bench/r4audit/
go test -count=1 -run 'TestIndexNestedLoop|TestHashJoin' ./cypher/
```

The A/B columns come from forcing `indexNestedLoopWins` to `true` and to `false` in turn; the gate
is the only thing that chooses between the two plans, so that is the whole switch.
