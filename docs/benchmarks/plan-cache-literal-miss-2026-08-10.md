# What a plan-cache miss costs, and what it does not (2026-08-10)

rmp #2393 asked whether GoGraph's plan cache — which keys on the **raw** query
text, with no literal extraction anywhere in `cypher/` — penalises a workload that
inlines literals instead of binding parameters. Both Memgraph and Neo4j strip
literals before keying, so the gap is real; the question was its size.

The task was explicitly measurement-first: *"the size of the win is currently
UNQUANTIFIED and must not be assumed … If the miss cost is small the task closes
on that evidence with no code change."* This is that measurement.

## Method

Three arms, one process, interleaved by `go test -bench`, medians of **5**. Each
arm differs from the hit arm in exactly one respect.

| Arm | Query | Distinct texts | Cache outcome |
|---|---|---:|---|
| `BenchmarkPlanCacheHit_Parameterised` | `MATCH (n:Account {id: $id}) RETURN n.id` | 1 | hit |
| `BenchmarkPlanCacheMiss_InlinedLiteral` | `… {id: <N>} … /*i*/` | **2048** | **miss** |
| `BenchmarkPlanCacheMiss_InlinedLiteralNoComment` | `… {id: <N>} …` | 64 | **hit** (control) |

The miss arm cycles **2048** distinct texts against a 1024-entry LRU
(`DefaultPlanCacheCapacity`). That is deliberate and asserted by
`TestPlanCacheLiteralArmsAreValid`: with fewer distinct texts than the cache
holds, every lookup after the first pass would **hit** and the arm would silently
stop measuring a miss while still reporting a plausible number.

The graph is only 64 nodes, on purpose: the cost under study is the front end
(parse → semantic analysis → IR → plan build), so execution must not bury it.
`TestPlanCacheLiteralArmsAreValid` also checks that both arms return the **same,
non-zero** row count — otherwise they would not be the same workload.

## Result

| Arm | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| hit (parameterised) | 33 482 | 17 100 | 90 |
| **miss (2048 distinct texts)** | **53 804** | **34 674** | **408** |
| control (inlined, 64 texts → hits) | 33 272 | 16 844 | 98 |

**A miss costs 1.61× the latency, 2.03× the bytes and 4.53× the allocations of a
hit** — an extra **20.3 µs, 17.6 KB and 318 allocations** per query.

## The control is the finding

**Inlining a literal costs nothing by itself.** The control arm inlines its
literals exactly as a generated workload would, but over only 64 distinct texts,
so its lookups hit — and it measures **0.994× the latency** and **1.089× the
allocations** of the parameterised arm. Statistically identical.

So #2393's stated premise — *"a workload that inlines literals instead of binding
parameters misses on every statement"* — **is only true when the workload's
distinct-statement count exceeds the cache capacity.** Below 1024 distinct texts,
inlining is free. That is a materially narrower exposure than the task assumed,
and it changes who is affected:

- **Not affected:** any workload whose set of distinct statement texts is bounded
  below 1024, however many literals it inlines. This includes every GoGraph
  example and the TCK.
- **Affected:** workloads whose statement texts are effectively unbounded —
  generated Cypher inlining high-cardinality identifiers, ad-hoc console traffic,
  ORM-style emission without parameter binding. Those pay 1.61× per query *and*
  churn the LRU, evicting the plans of statements that would otherwise hit.

## Decision

**Closed on this evidence; literal extraction is filed separately (rmp #2399).**

The penalty is real but conditional, and extraction is a front-end architecture
change: a normalisation pass must rewrite literal tokens to synthetic parameters
*before* the cache key, and must be **suppressed** wherever openCypher semantics
depend on the literal rather than its value. That carries genuine risk against the
3897-scenario TCK baseline, needs its own design and its own gate, and does not
belong bolted onto a certification cycle.

Raising `DefaultPlanCacheCapacity` was considered and rejected as the primary
answer: it moves the cliff rather than removing it, and trades memory for hit rate
on a cache whose entries hold plan trees.

## Reproduce

```bash
go test -run=^$ -bench=BenchmarkPlanCache -benchmem -count=5 ./cypher/

# The arms' validity is a test, not a comment:
go test -run=TestPlanCacheLiteralArmsAreValid -v ./cypher/
```

Environment: Apple M4 (10 cores), `darwin/arm64`, macOS 26.5.2, go1.26.5.
