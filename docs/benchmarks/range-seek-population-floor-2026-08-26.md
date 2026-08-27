# An indexed equality lookup got 12.6× slower because the graph was SMALLER

**Task:** rmp #2367 · **Date:** 2026-08-26 · **Host:** Apple M4, 10 cores, darwin/arm64, go1.26.5
**Fix:** `cypher/range_seek_plan.go` — `rangeSeekMinLabelPopulation` 1024 → 64

## Summary

`MATCH (n:Account {id: $id}) RETURN n.v` on an indexed property cost **68.7 µs** over a label
of 1023 nodes and **5.5 µs** over one of 1024 — a 12.6× cliff at a constant, with the smaller
graph the slower one. `rangeSeekMinLabelPopulation` suppressed the range-index seek below 1024
nodes on a premise that measurement refutes by more than an order of magnitude.

## The mechanism, established rather than inferred

An earlier version of this ticket asserted "a cost-based planner with a mis-calibrated
crossover" and that claim was **withdrawn** when a search of the planner found no such
threshold. Four further explanations were proposed for the surrounding anomaly and each was
refuted by measurement. So this was settled by plan dumps and a sweep, not by argument.

`cypher/range_seek_plan.go`, `rangeSeekBudget`: when the label population is below
`rangeSeekMinLabelPopulation` it returns `ok == false`, so **no count can change the verdict**
and the range seek is never considered. The plan falls back to `NodeByLabelScan` + `Selection`,
whose cost is linear in the label population.

The key kind decides which index path is even reachable. A Cypher `CREATE INDEX` builds a
**string-keyed** hash index, and `tryNewHashSeek` (`cypher/api.go:12068`) declines a non-string
seek against it — the `#F-CY2` contract. An **integer** key therefore cannot reach the hash
path at all and falls to the range path, which is the population-gated one. That is why the
defect is invisible to a string-keyed fixture and reproduces on the integer key a user
naturally writes.

## Two instrument corrections, recorded because they nearly produced wrong answers

**A logical plan is not evidence about what a write executes.** `Explain` on `SET n.v = $v`
prints *"(logical plan — a writing statement has no physical tree outside a transaction)"*.
Lowering the floor flipped that logical plan to `NodeByIndexRangeScan` at 512 nodes while the
measured cost stayed at 136 µs, linear in the population. The measurement was redone on the
**read** form, which has a physical tree.

**An ascending-only sweep manufactures a warm-up artefact.** The first sweep showed an apparent
anomaly below 128 nodes. Re-run descending, it disappeared: 8 nodes measured 8.9 µs one way and
13.0 µs the other. Nothing is claimed below ~128 nodes; both arms there are dominated by fixed
statement cost and differ by less than the run-to-run spread.

## The sweep — a STEP at the constant, in both directions

Read form, integer key, physical plans, timed work held identical:

| population | shipped (floor 1024) | plan | fixed (floor 64) | plan |
|---:|---:|---|---:|---|
| 8 | 8 906 ns | scan | 12 957 ns | scan |
| 32 | 10 144 | scan | 12 360 | scan |
| 128 | 17 141 | scan | 10 046 | **seek** |
| 256 | 21 936 | scan | 4 153 | **seek** |
| 512 | 39 488 | scan | 5 061 | **seek** |
| **1023** | **68 654** | **scan** | **5 332** | **seek** |
| **1024** | **5 467** | **seek** | 4 987 | seek |
| 2048 | 5 255 | seek | 5 452 | seek |
| 4096 | 5 605 | seek | 7 157 | seek |

Scan cost is linear in the population; seek cost is flat at ~5 µs. The shape is a **step at the
constant**, which is what implicates a decision rather than amortisation.

## Allocations — the same result, immune to machine load

`testing.AllocsPerRun`, allocations per lookup:

| population | floor 1024 | floor 64 |
|---:|---:|---:|
| 64 | 98 | 113 |
| 256 | 106 | 121 |
| 512 | 368 | 127 |
| **1023** | **889** | **124** |
| 1024 | 125 | 125 |
| 4096 | 127 | 123 |

Flat after; **7.1× at the old floor's boundary** before. This is what the regression gate
asserts, for the reason `bench/mvccwrite` already established for #2359: an operation's
allocation count cannot depend on how many peers — or here, how many other nodes — exist.

## Why 64, and why a floor at all

64 is where the measurement stops being able to tell the two paths apart. From 128 nodes upward
the seek is decisively cheaper; below ~128 the per-operation cost is fixed-cost dominated and
the arms differ by less than the run-to-run spread.

A floor is kept rather than removed because below it no count can change the verdict, so
`rangeSeekBudget` refuses to take one. That is the same rule #2380 and #2392 imposed on the
parallel-scan gate: **a gate must cost less than the decision it informs.** Removing the floor
would make every trivial-label equality pay an exact `RangeCount` to decide something it cannot
win.

## Prior art, read from source

Neither reference engine has an analogue of this floor, which is why it is a *floor* and not a
*rule* — a conservative cheap-exit, not a policy about when indexes are useful.

- **PostgreSQL** — `src/backend/optimizer/path/costsize.c` (REL_17_STABLE). `cost_seqscan` and
  `cost_index` contain no minimum table size or row-count floor. The only suppression is
  `disable_cost` (1.0e10) added when the matching `enable_*` GUC is off. Access-path choice is a
  cost comparison from page and tuple estimates; a small table may make an index path *cost*
  more, but nothing rejects it by rule.
- **Neo4j** — `NodeIndexLeafPlanner.scala` (5.x). `findIndexMatches` yields a `NodeIndexMatch`
  for every (label predicate, matching index descriptor, compatible predicate) triple. The whole
  file contains no occurrence of threshold, minimum, population, cardinality, count or size: the
  seek candidate is generated unconditionally and the cost model decides.

Both delegate to a cost model. GoGraph has none — the withdrawn hypothesis in this ticket
confirms there is no such machinery — so this constant *is* the whole decision, and it is now
set from measurement instead of intuition. Building a cost model was considered and rejected as
far beyond what this task scopes.

## What this did NOT fix

An **integer**-keyed property still cannot use the hash path, because a Cypher `CREATE INDEX`
only ever builds a string-keyed hash index. Below the (now much lower) floor an integer equality
still scans, where a string equality seeks at any population. That asymmetry is a separate
question about what `CREATE INDEX` should build and is not touched here.

## Downstream

`bench/mvccwrite`'s `update-property` arm now reports **allocs/op flat at 53–54** across writers
1..32 — i.e. across seeded populations 256..8192 — and ns/op 2810 → 1414, a 1.99× figure that is
a plausible concurrency number rather than the impossible 28× the fixture used to report.

Two comments this work refuted were corrected in the same change: `contention_arms_test.go`
claimed "rmp #2367 is retired by that evidence" (it was not — the string key fixed the *fixture*,
not the finding), and `read_under_writer_test.go` claimed its integer-keyed lookup was a seek
without noting that it is the *range* path and therefore population-gated.

## Reproduce

```
go test ./cypher/ -run 'TestEqualityLookup|TestRangeSeekFloor' -count=1
go test ./cypher/tck/... -count=1
```
