# MVCC write-contention arms — baseline at 78f610e3 (2026-08-08)

The baseline every contention task in sprint 336 compares against (rmp #2359). Written
down rather than remembered, so a later claim is measured against a figure and not a
recollection.

## How these numbers were produced

- `BenchmarkWriteContention` in `bench/mvccwrite`, arms `create-labelled-node`,
  `update-property`, `label-add-remove`, `mixed`, `create-edge`.
- **Interleaved**: two `go test -c` binaries alternated inside one loop, **n = 10** each,
  compared with `benchstat`. An across-time comparison on this host has been shown
  worthless.
- **No `-race`.** Host load averages 9.16 / 9.13 / 10.17 at the time of the run, which
  matters for the self-control below and is why it was run at all.
- Apple M4, 10 cores, `darwin/arm64`, Go toolchain as vendored.
- `create-edge` was excluded at first and is now included; see the note at the end for the
  fixture change that made it measurable.

## The harness is validated, not assumed

Both binaries were **byte-identical copies**, so every row must read `~`. Every row does:

```
                                    ctlA          ctlB        vs base
create-labelled-node/writers=1    2.794µ ± 2%   2.781µ ± 1%   ~ (p=0.197 n=10)
create-labelled-node/writers=4    1.339µ ± 4%   1.331µ ± 2%   ~ (p=0.148 n=10)
create-labelled-node/writers=16   1.301µ ± 1%   1.306µ ± 1%   ~ (p=0.469 n=10)
create-labelled-node/writers=32   1.309µ ± 1%   1.302µ ± 2%   ~ (p=0.515 n=10)
update-property/writers=1         3.111µ ± 2%   3.107µ ± 1%   ~ (p=0.383 n=10)
update-property/writers=4         1.768µ ± 2%   1.766µ ± 1%   ~ (p=0.927 n=10)
update-property/writers=16        1.616µ ± 2%   1.625µ ± 2%   ~ (p=0.286 n=10)
update-property/writers=32        1.517µ ± 2%   1.512µ ± 2%   ~ (p=0.516 n=10)
label-add-remove/writers=1        4.459µ ± 1%   4.451µ ± 1%   ~ (p=0.697 n=10)
label-add-remove/writers=4        3.089µ ± 1%   3.091µ ± 2%   ~ (p=0.853 n=10)
label-add-remove/writers=16       3.009µ ± 1%   3.009µ ± 2%   ~ (p=0.985 n=10)
label-add-remove/writers=32       2.825µ ± 1%   2.845µ ± 1%   ~ (p=0.092 n=10)
mixed/writers=1                   5.954µ ± 2%   5.885µ ± 1%   ~ (p=0.123 n=10)
```

Zero phantom rows at load ≈ 9. That is the property to re-check before trusting any
future comparison: this project has previously had a byte-identical control report **22
of 36** flat-by-construction rows as significant, which manufactured two phantom
regressions.

## The baseline

`ns/op` is per commit. `scaling` is commits/s at N writers over commits/s at 1 writer;
total work is constant across writer counts, so it equals `ns/op(1) / ns/op(N)`.

| arm | 1 | 4 | 16 | 32 | scaling @32 |
|---|---:|---:|---:|---:|---:|
| `create-labelled-node` | 2794 | 1339 | 1301 | 1309 | **2.13×** |
| `update-property` | 3111 | 1768 | 1616 | 1517 | **2.05×** |
| `label-add-remove` | 4459 | 3089 | 3009 | 2825 | **1.58×** |
| `mixed` | 5954 | 2083 | 1965 | 1990 | **2.99×** |
| `create-edge` | 13820 | 7901 | 8221 | 8567 | **1.61×** |

`allocs/op` is **independent of the writer count** on all five — 43/42/41/41,
52/53/53/53, 62/64/65/66, 56/56/55/55, 167/168/161/161. That independence is the invariant that says the
fixture is not leaking into the measurement, and its violation is what exposed three
successive artefacts while these arms were being built.

`create-labelled-node`'s 2.13× reproduces `BenchmarkWriteScaling`'s figure, which is the
cross-check that the new harness measures the same thing the old one did.

## What the numbers say

- The ceiling is ≈ 2× on the single-object arms and is reached by **4 writers**; 16 and 32
  add essentially nothing. Whatever binds it is not relieved by more writers.
- `label-add-remove` is the worst scaler (1.58×) and the most expensive per commit. It is
  two statements, so per-statement it is ~1.4 µs — comparable to the others — but it pays
  the label store twice plus a deferred index removal.
- `mixed` scales best (2.99×) because it spreads across three substores instead of
  hammering one, which is consistent with the deficit being per-substore shared state
  rather than a single global lock.

## `create-edge` — why it was excluded, and what fixed it

Its `allocs/op` originally varied with the writer count (1118 → 804 → 329 → 215), failing
the independence invariant. The arm joined two POOLED nodes, so pairs repeated and it
accumulated **parallel edges** — as many as `b.N / writers` per pair — which made per-op
cost a function of the writer count rather than of contention.

Creating **both endpoints fresh** per iteration fixed it: `allocs/op` is now 167/168/161/161,
flat, and the arm is in the table above. Adjacency, the per-edge side stores and the count
store are all still exercised — the three structures the arm exists for — while no node's
degree and no pair's edge count grows with `b.N`. It is the most expensive arm because its
unit is two nodes plus an edge.

Its 1.61× is the second-worst scaler after `label-add-remove`, which is consistent with the
edge write touching the most shared structures per commit.

## Discipline for the tasks that compare against this

Four fixture artefacts were found while building these arms, two of which masqueraded as
engine defects:

1. an unindexed lookup whose cost tracked the seeded node count;
2. a Cypher `CREATE INDEX` that is **string-keyed**, while the pool was keyed on an
   integer — so `tryNewHashSeek` correctly declined the seek and every mutating arm was
   a label scan;
3. per-node churn varying with the writer count (proposed, tested, **refuted**);
4. dependent statements issued through bare `eng.RunInTx`, which livelocked at 16 writers
   and was filed as an engine defect (rmp #2368) before being traced to the arm — the two
   statements need **one `Session`** to get read-your-own-writes.

So: re-run the self-control, check `allocs/op` independence, and check `uptime` before
believing any comparison against this table.
