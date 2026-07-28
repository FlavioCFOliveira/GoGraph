# Final-projection elision — the stacked `Project` pair (rmp #2239)

**Date:** 2026-07-28 · **Sprint:** 327 · **Host:** Apple M4 (10 cores, 32 GB), Go toolchain as
pinned by the module.

## 1. What was wrong

`buildPlanEngine` laid its final column passthrough on top of the projection it had just
built for the `ir.Project` node, so a single `RETURN` produced two stacked `Project`
operators:

```
Project
└─ Project
   └─ NodeByLabelScan [Person]
```

The logical plan (`Engine.ExplainLogical`) carried a single `Projection`, so the second
operator was introduced by the physical build. It was invisible until #2222 made `Explain`
render the physical tree.

The defect was wider than the reported case. Before the fix, all of these rendered a
stacked pair: `RETURN n`, `WHERE … RETURN n`, `RETURN count(*)`, `MATCH (a)-[r]->(b) RETURN
a, b`, `UNWIND … RETURN x`, `RETURN n ORDER BY …`, `RETURN n LIMIT n`, `RETURN n SKIP n`.

## 2. The fix, and why the obvious form of it is unsafe

The passthrough is elided when the child already emits exactly the result columns, decided
by `exec.EmitsExactly`.

The tempting test — the pre-existing `isIdentityPassthrough`, which checks that every output
column *i* reads input column *i* — is **not sufficient**, and acting on it alone would have
been a correctness bug. A variable-to-index schema records where a column *sits*, not how
*wide* the row is. `MATCH (a)-[r]->(b:P) RETURN a, r` maps `a`→0 and `r`→1 and so satisfies
it, yet the row arriving from the expand also carries `b`; eliding there would have emitted
a third column the query never projected. `EmitsExactly` asks the child for its declared
**arity** instead, which closes the gap. `TestProjectElision_KeepsPassthroughWhenItWouldWiden`
pins that case.

The elision also descends through operators that re-emit their input row unchanged in width
and column order — `Distinct`, `Eager`, `Filter`, `Limit`, `Skip`, `Sort` — so `RETURN n
LIMIT 3` collapses as readily as a bare `RETURN n`. Each was read before being admitted to
the whitelist; an operator absent from it stops the descent, so a shape-changing operator
cannot be walked through by omission.

`EmitsExactly` lives in `cypher/exec` rather than in `cypher` because a profiled build wraps
every operator, and only that package can see through the wrapper. A walk that could not
unwrap answered differently under `PROFILE` than under `EXPLAIN` —
`TestProfile_PlanShapeIsIdenticalProfiledOrNot` caught exactly that during development, which
is the divergence `Profiler` documents as forbidden.

## 3. Measurement

`bench/cypher_scale`, 120 000-node seed graph, `-benchmem -count=6`, compared with
`benchstat`. The baseline arm is the same binary with the elision disabled, so nothing but
this change differs.

| Benchmark | sec/op | B/op | allocs/op |
|---|---|---|---|
| `CountAllPersons` | 1.580 µ → 1.514 µ · **−4.21 %** (p=0.002) | 2.156 Ki → 1.977 Ki · **−8.33 %** (p=0.002) | 31 → 27 · **−12.90 %** (p=0.002) |
| `FilterProject` | 14.62 m → 14.86 m · ~ (p=0.818) | 7.756 Mi → 7.756 Mi · ~ | 144 → 144 · ~ (p=1.000) |
| `ReturnWholeNode` | 26.70 m → 26.76 m · ~ (p=0.132) | 82.61 Mi → 82.63 Mi · ~ | 723.7 k → 723.7 k · ~ |
| `ReturnWholeNodeLimited` | 19.45 m → 19.48 m · ~ (p=0.937) | 71.87 Mi → 71.90 Mi · ~ | 723.7 k → 723.7 k · ~ |

`BenchmarkReturnWholeNode` and `BenchmarkReturnWholeNodeLimited` were added by this task:
the whole-node return is the shape the defect hits hardest and it was previously unmeasured.

### The task's stated premise was too strong, and the measurement corrects it

The finding asserted that "every row therefore passes through a projection twice, on the
hottest path in the engine". The first half is true; the implied cost is not. On the two
large-result shapes the win is **statistically indistinguishable from zero** — 723.7 k
allocs/op either way.

The reason is that the elided operator was an *identity* projection: `Project.Next` reuses a
per-operator `outBuf` and each item copies `row[idx]` without allocating, so a second pass
over an already-materialised row costs a slice copy that is lost in the noise of node-value
boxing, which dominates those benchmarks at ~724 k allocs per operation.

The win is therefore in **plan construction**, not per row: one fewer operator to build and
one fewer `[]ProjectionItem` to allocate shows up as −12.90 % allocs on a query whose result
is a single row and whose cost is consequently dominated by building the plan.

The durable value of the fix is correctness of what the user is shown — `EXPLAIN` and
`PROFILE` no longer report an operator that does no work, and `PROFILE` no longer attributes
time to it.

### An allocation the first attempt added

The first implementation asserted `interface{ Columns() []string }` and compared the returned
slice. `Project.Columns` materialises a fresh `[]string` on every call, and `EmitsExactly`
runs on every read-plan build, so this added exactly one heap allocation per plan built —
visible as `FilterProject` 144 → 145 allocs/op (p=0.032). Replacing the getter with the
non-allocating `columnsAre(cols []string) bool` removed it and improved
`CountAllPersons` from −9.68 % to −12.90 % allocs. The interim numbers are retained here
because they are the reason the comparison is a method rather than a getter.

## 4. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate (aggregate 86.9 % ≥ 85.0 %).
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined** (baseline 3897).
- `TestProjectElision_SingleReturnRendersOneProject` fails on the pre-fix behaviour in 9 of 9
  shapes and passes after, verified by disabling the elision and re-running.
