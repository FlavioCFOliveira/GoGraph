# Admitting the hash join for writing statements — 2026-07-27 (rmp #2225 part B)

Measurement record for the second half of the write-path planner defect. Part A
([`threeway-2026-07-27.md`](threeway-2026-07-27.md) context, rmp #2225) gave the write path the
order-neutral substitutions — the range seek and the min-label re-anchor — and explicitly did
**not** move the bulk-load number. Part B admits the disconnected-equi-join hash join, which is
where that number lived.

- Apple M4 (10 cores, 32 GB), Go 1.26.5, `GOMAXPROCS=10`.
- Neo4j 5.26 Community and Memgraph 2.22.0 under Docker, GoGraph in-process.
- Harnesses: `bench/r4audit/w1partb_test.go` (build tag `r4audit`) and
  `bench/comparison/threeway_test.go` (build tag `threeway`).

## 1. The defect

`buildPlanWithMutatorFull` — the plan builder every statement containing a write clause goes
through — constructed its `buildOpts` with `hashJoinEnabled` and `hashJoinOrderSafe` both false.
The bulk-load idiom

```cypher
UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)
```

lowers to `Selection(a.sid = r.ss, Apply(Unwind, NodeByLabelScan(:P)))`. The key varies per row,
so it reaches no index on either path — `#2182`'s correlated-seek pass only substitutes a
**row-invariant** binding. Both the read and the write form therefore SCAN; the difference was
that the read got the `#1506` hash join (Θ(N+B)) and the write got the nested-loop Cartesian
product (Θ(N·B)).

## 2. Why it was left off, and why that reason did not hold

Part A recorded, and the round-4 audit repeated, that a hash join "may build on either arm and
therefore reorder rows", which would be unsafe for `SET` (last-write-wins). **Measurement
refuted the premise.** GoGraph's hash join never self-selects its build side: the planner pins
`build = apply.Inner`, `probe = apply.Outer` at the single construction site
(`cypher/hash_join_plan.go`), both operators emit probe-major, and within a bucket the build rows
stay in build-insertion (inner-scan) order. The emitted sequence is therefore **row-for-row
identical** to the nested loop, not merely multiset-identical.

The argument is recorded at `hashJoinBuildOnLeft` in `cypher/hash_join_plan.go` and pinned
empirically by `TestHashJoinOrder_SequenceMatchesNestedLoop`, which compares the full row
sequence position-by-position across six shapes covering **both** join operators
(`exec.ColumnarHashJoin` when both arms are bare scans, row-mode `exec.HashJoin` otherwise).

A second risk, not raised by the audit, was checked and cleared: the hash join drains its build
side **once**, whereas `exec.Apply` re-initialises the inner arm per outer row — so a write whose
`CREATE` feeds the build arm's own label could have diverged. It does not; both plans snapshot
the label bitmap at `Init`. Pinned by
`TestWritePathGates_HashJoinWriteResultIdentity/CREATE_feeding_the_build_arm's_own_label`.

## 3. Bound-key write, against its own read control

`bench/r4audit/w1partb_test.go`, batch 500, indexed `:P`, best of 5, fresh engine per repeat.

| case | N=2000 | N=4000 | N=8000 | N=16000 | growth (8× N) |
|---|---|---|---|---|---|
| **before** — bound-key read (control) | 4.319ms | 2.691ms | 5.099ms | 9.797ms | 2.3× |
| **before** — bound-key write | 226.770ms | 454.647ms | 923.972ms | 1.860s | 8.2× |
| **after** — bound-key read (control) | 1.366ms | 2.320ms | 4.757ms | 8.953ms | 6.6× |
| **after** — bound-key write, `CREATE` rel | 2.475ms | 3.390ms | 5.428ms | **9.669ms** | 3.9× |
| **after** — bound-key write, `SET` | 1.810ms | 2.913ms | 4.888ms | 9.436ms | 5.2× |
| **after** — bound-key write, two joins | 3.411ms | 5.659ms | 10.299ms | 18.481ms | 5.4× |

At N=16000 the write is **192× faster** and sits **1.08×** of its read control. Note what the
acceptance criterion means by "flat in N": the cost is still *linear* in N, because the build side
scans the whole `:P` population — exactly as the read control does. What is removed is the N·B
term. The two-join case is the harness's own edge-load shape and pays two builds, hence ≈2× the
single-join case rather than the previous N·B blow-up.

Isolation, same statement with `EngineOptions.DisableHashJoin` (the supported way to obtain the
pre-part-B plan), N=8000:

```
hash join ON 5.426ms   OFF 909.283ms   speed-up 167.6x
```

## 4. Three-way head-to-head at 20 000 nodes

`THREEWAY_NODES=20000` → 20 000 nodes / 199 941 edges, UNWIND batches of 5 000. Row counts
cross-checked identical across all four targets before any timing was compared.

### Load

| Target | round 3 | round 4 | **after part B** |
|---|---|---|---|
| gograph-embedded | 35m33s | 35m10.173s | **2.206s** |
| gograph-bolt | — | — | **2.318s** |
| neo4j-bolt | — | 3.47s | 4.252s |
| memgraph-bolt | — | 968ms | 963ms |

**957× faster**, and GoGraph-embedded now loads this dataset faster than Neo4j. The whole
harness — four targets, load plus 16 queries × (3 warm-ups + 9 repeats) — completed in 28
seconds, against a load phase alone that previously took over half an hour.

The durability caveat from the round-4 record still applies and is **not** repaired by this
change: the GoGraph target is a bare in-memory `lpg.Graph` with no WAL and no fsync, while Neo4j
forces its transaction log per commit and Memgraph writes a WAL. The load figures are not
durability-comparable. Repairing the harness is rmp #2223.

### Query latency (median of 9, after 3 warm-ups)

Unchanged within noise — nothing in this task touches a read plan. Recorded for completeness.

| Query | Description | gograph-embedded | gograph-bolt | neo4j-bolt | memgraph-bolt |
|---|---|---|---|---|---|
| `q01_point_lookup` | Indexed point lookup | 7µs | 63µs | 2.064ms | 268µs |
| `q01b_point_lookup_str` | Point lookup on a STRING key | 7µs | 57µs | 1.942ms | 228µs |
| `q02_range_scan` | Indexed range scan + count | 5.012ms | 5.146ms | 1.686ms | 387µs |
| `q03_starts_with` | STARTS WITH prefix | 4.617ms | 4.758ms | 1.804ms | 3.653ms |
| `q04_one_hop` | 1-hop expand from a bound node | 3.570ms | 2.661ms | 1.551ms | 262µs |
| `q05_two_hop` | 2-hop friends-of-friends, DISTINCT | 8.273ms | 7.476ms | 1.389ms | 263µs |
| `q06_varlen_3` | Variable-length 1..3 DISTINCT | 9.788ms | 9.193ms | 3.912ms | 369µs |
| `q07_global_count` | Global label count | 556µs | 653µs | 1.479ms | 1.057ms |
| `q08_group_by` | Group-by + order + limit | 2.618ms | 2.799ms | 5.233ms | 2.914ms |
| `q09_top_k` | Top-k by unindexed property | 30.305ms | 34.521ms | 3.596ms | 5.934ms |
| `q10_triangle` | Cyclic 3-clique count (WCOJ shape) | 1.322s | 1.274s | 397.677ms | 214.890ms |
| `q11_expand_into` | Both endpoints bound | 2.744ms | 2.909ms | 953µs | 256µs |
| `q12_multi_label` | Multi-label conjunction | 9µs | 90µs | 1.016ms | 790µs |
| `q13_shortest_path` | Unweighted shortest path ≤6 | 12.726ms | 13.660ms | 1.857ms | 316µs |
| `q14_property_filter` | Unindexed property equality scan | 4.550ms | 5.773ms | 4.645ms | 2.799ms |
| `q15_create` | Single-node write | 4µs | 62µs | 1.815ms | 246µs |

## 5. No regression on the read path

`go test -run='^$' -bench=BenchmarkHashJoin -benchmem -count=6 ./bench/cypher_ldbc/`, A/B by
stashing the two production files, `benchstat`:

```
                                           │   before    │             after              │
                                           │   sec/op    │   sec/op     vs base           │
HashJoinDisconnectedEquiJoin_HashJoin-10     587.8µ ± 5%   593.9µ ± 2%  ~ (p=0.699 n=6)
HashJoinDisconnectedEquiJoin_NestedLoop-10   63.11m ± 2%   63.18m ± 1%  ~ (p=0.937 n=6)

                                           │    B/op     │    B/op      vs base           │
HashJoinDisconnectedEquiJoin_HashJoin-10    893.2Ki ± 0%  893.2Ki ± 0%  ~ (p=0.814 n=6)
HashJoinDisconnectedEquiJoin_NestedLoop-10  8.354Mi ± 0%  8.354Mi ± 0%  ~ (p=0.485 n=6)

                                           │  allocs/op  │  allocs/op   vs base           │
HashJoinDisconnectedEquiJoin_HashJoin-10     7.191k ± 0%   7.191k ± 0%  ~ (p=0.545 n=6)
HashJoinDisconnectedEquiJoin_NestedLoop-10   491.4k ± 0%   491.4k ± 0%  ~ (p=0.240 n=6)
```

No significant change in any dimension. The only read-path edit is one atomic increment per
**plan build** (`hashJoinColumnarBuildCount`, the diagnostic seam that lets the order test prove
which operator a case exercised); it is not on any per-row path.

## 6. Decision record — hash join vs index nested-loop join

The task required the choice between the two candidate plans for this shape to be recorded with
its reasoning.

**Neo4j's plan** for a per-row-varying key is an *index nested-loop join*: `Apply` over a
per-row-parameterised `NodeIndexSeek` whose key expression is evaluated against the current row
(`community/cypher/cypher-planner/.../steps/` — the same mechanism `#2182`'s pass imitates for
the row-invariant case). Cost Θ(B log N), order-preserving by construction.

**What was chosen: the hash join, Θ(N+B).** Reasoning:

1. **It is strictly the smaller change.** The operator, its columnar variant, its memory budget,
   its cross-type key semantics and its differential suite all already exist and are already
   exercised on the read path. The index nested-loop join is a new physical operator plus a new
   cost policy to choose between the two.
2. **It does not require an index.** Θ(B log N) is only available when a suitable index exists
   on the property; the hash join serves the unindexed case at the same asymptotic cost. For a
   bulk load — the shape that motivated this — an index on the join key is common but not
   guaranteed.
3. **The measured gap is small at the shape that matters.** At N=20000 and B=5000, Θ(N+B) ≈ 25 000
   units against Θ(B log N) ≈ 5 000 × 14.3 ≈ 71 500 — the hash join is *ahead* here, because the
   batch is large relative to the population. The index nested-loop join wins when B ≪ N
   (B=500: 7 150 against 20 500), which is the small-batch regime.
4. **The two are complementary, not exclusive.** Because (3) cuts both ways, the right long-term
   answer is both plans plus a cardinality gate — which is precisely the cost policy point 1
   defers. The hash join lands the 957× today and does not foreclose it.

The index nested-loop join is therefore **not rejected, only deferred**, and is filed as its own
backlog item.

## 7. What was deliberately not done

The read path still runs its own whole-query order-safety scan (`hashJoinOrderSafe`), which
disables the substitution for a query containing a bare `LIMIT`/`SKIP` or a `collect()`. The
order-preservation proof in §2 makes that scan unnecessary, and retiring it would widen the hash
join's reach on the read path. That is a behaviour change on a path this task was not asked to
touch, so it is filed separately rather than folded in here.

## Reproduce

```bash
go test -tags=r4audit -run 'TestW1PartB' -v -timeout 60m ./bench/r4audit/
go test -run='^$' -bench=BenchmarkHashJoin -benchmem -count=6 ./bench/cypher_ldbc/

docker run -d --rm --name gg-neo4j    -p 7687:7687 -e NEO4J_AUTH=neo4j/gographbench neo4j:5.26-community
docker run -d --rm --name gg-memgraph -p 7688:7687 memgraph/memgraph:2.22.0 --telemetry-enabled=false
THREEWAY_NODES=20000 go test -tags=threeway -run TestThreeWay -v -timeout 90m ./bench/comparison/
```
