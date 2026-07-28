# Retiring the read path's hash-join order-safety scan — 2026-07-28 (rmp #2234)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harness `bench/r4audit/hj_test.go` and
  `bench/r4audit/hjwalk_test.go`, build tag `r4audit`. Fixture: 200 `:P` nodes with heavily
  repeating `age` keys.
- Best of 200 per column, `-count=1` on every run so nothing comes from the test cache.
- "before" is the tree at `e18c6124`.

## 1. What changed

The planner carried a whole-query IR scan that disabled the hash-join substitution for the ENTIRE
query whenever the plan contained a bare `LIMIT`/`SKIP` without `ORDER BY`, or an arrival-order
aggregation (`collect`, `collect(DISTINCT)`). Those queries silently got the O(n·m) nested loop.

The scan was guarding against a reordering that cannot happen. #2225 part B established — and
`TestHashJoinOrder_SequenceMatchesNestedLoop` pins — that the substitution emits the nested loop's
row sequence **position for position**, not merely its multiset: the planner pins build = inner and
probe = outer at the single construction site, both operators emit probe-major, and within a bucket
the build rows stay in build-insertion order. The scan is deleted, not bypassed, along with its
plan-cache memoisation.

## 2. The verdict table flips two rows

`TestHashJoinTrigger`, all ten shapes:

| shape | before | after |
|---|---|---|
| equijoin-collect | **CartesianProduct (nested loop)** | **HashJoin** |
| equijoin-limit | **CartesianProduct (nested loop)** | **HashJoin** |
| equijoin-entities, -scalars, -count, -sum, -min, -orderby, -distinct, -with-count | HashJoin | HashJoin |

## 3. What that is worth

`TestHashJoinWalkCost`, end to end, 200 `:P` nodes:

| query | before (warm) | after (warm) | change |
|---|---|---|---|
| `… WHERE a.age = b.age RETURN collect(a.id)` | 9.869 ms | **158.083 µs** | **62.4×** |
| deep plan, five `WITH` stages, `ORDER BY` + `LIMIT` + `SKIP` | 10.194 ms | **758.292 µs** | **13.4×** |
| `… WHERE a.age = b.age RETURN a.id, b.id LIMIT 5` | 99.041 µs | **69.542 µs** | **1.42×** |
| `… WHERE a.age = b.age RETURN a.id ORDER BY a.id` (control, fired before and after) | 427.625 µs | 448.291 µs | unchanged |

The `collect` case is the shape of the win: nothing about it is order-sensitive in practice, yet the
scan saw `collect` anywhere in the plan and forfeited the join for the whole query.

**`LIMIT 5` gains the least, and that is expected rather than disappointing.** A small bare `LIMIT`
lets the nested loop stop early, so it never pays the full O(n·m); the hash join still has to build
its table. The win grows with the limit and with the fixture.

## 4. The removed IR walk is worth nothing measurable, and that is the finding

The task's premise was that the scan is "a per-query whole-plan IR walk the read path runs on every
`Run`". That was true when #1719 was filed and false by the time this task ran: **#1719 had already
memoised it onto the plan-cache entry**, so a warm `Run` only read a bool and the walk itself
happened once per plan-cache MISS.

`TestHashJoinWalkCost` separates the two by calling `ClearPlanCache` before each cold iteration. The
only case that can measure the walk cleanly is the `ORDER BY` control — the one shape whose plan is
IDENTICAL before and after, so nothing but the walk differs. Its cold-minus-warm delta, four runs
each:

| | samples (µs) | range |
|---|---|---|
| before | 48.96, 23.42, 28.54, 30.17 | 23.4 – 49.0 |
| after | 25.46, 42.00, 35.25, 23.96 | 24.0 – 42.0 |

Fully overlapping. **No measurable win.** One IR walk over a small plan is a few microseconds inside
a ~480 µs cold build, and warm runs never touched it at all. Recorded here because acceptance
criterion 5 asked for the answer either way, and because the negative result is the useful one: it
says the value of this change is entirely in **which plan gets built**, not in the cost of deciding.

The other three rows' deltas moved too, but they are not evidence about the walk — their plan SHAPE
changed, so the cold column is measuring a different plan build before and after.

## 5. Correctness

`TestHashJoinOrder_SequenceMatchesNestedLoop` compares the full row SEQUENCE, position by position,
between an Engine with the hash join enabled and one built with
`EngineOptions.DisableHashJoin`, over the same fixture — asserting on each case that the join fired
on one arm and not the other, so no case can degrade into comparing a plan with itself. The fixture
repeats join keys heavily (so bucket order is exercised), leaves every 7th node without an `age` (a
NULL key), and gives every 11th an integral float `age` (so cross-type numeric equality puts
unequal-typed, equal-valued rows in one bucket).

Nine cases were added for exactly the shapes the scan used to exclude, plus one control:

- bare `LIMIT` without `ORDER BY`, bare `SKIP`, and both together;
- `collect()`, `collect(DISTINCT)`, and `collect()` over the whole result (one row, maximal order
  exposure);
- bare `LIMIT` and `collect()` each with an `Expand` on an arm, which forces the row-mode
  `exec.HashJoin` rather than `exec.ColumnarHashJoin`, so both operators are covered — asserted per
  case via `hashJoinColumnarBuildCount`;
- `LIMIT` under `ORDER BY` as the control: the scan already permitted it, so it fired before and
  after. It distinguishes "the scan was retired" from "the substitution broke".

**Every one of the nine new cases fails on the pre-#2234 planner**, for the honest reason: the join
does not fire, so there is nothing to compare. That is the regression proof.

A bare `LIMIT` is the sharpest case in the set. openCypher 9 §8.4 leaves WHICH rows come back
unspecified absent `ORDER BY`, so a divergence there would be *conformant* — and still a behaviour
change a user can see. It is the one shape where the order argument has to be exactly right rather
than merely plausible, which is why it is tested in both operator modes.

## 6. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`, cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**.

## Reproduce

```bash
go test -count=1 -tags=r4audit -run 'TestHashJoinTrigger|TestHashJoinWalkCost' -v ./bench/r4audit/
go test -count=1 -run 'TestHashJoinOrder' ./cypher/
```
