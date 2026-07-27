# Bidirectional search for shortestPath() — 2026-07-27 (rmp #2220)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harness `bench/r4audit/shortestpath_test.go`, build tag
  `r4audit`. Directed graph of average out-degree 10, the shape the round-3 head-to-head used.
- Best of 5. Each query drives **200 source rows** against one fixed destination.

## 1. Result

At N = 20000, against the forward-only walk measured on the same harness:

| shape | before | after | change |
|---|---|---|---|
| **`shortestPath((a)-[*..6]->(b))`** (untyped) | 190.271 ms | **17.544 ms** | **10.8× faster** |
| `shortestPath((a)-[:K*..6]->(b))` (typed) | 277.660 ms | 273.883 ms | unchanged — falls back |
| `shortestPath((a)-[:K*]->(b))` (typed, unbounded) | 278.473 ms | 274.701 ms | unchanged — falls back |
| `allShortestPaths(…)` (control) | 593.300 ms | 588.405 ms | unchanged — untouched |

At N = 5000 the untyped shape goes 53.316 ms → 6.939 ms (7.7×).

Measured in isolation, driving the two search functions directly over the same CSR, the algorithm
is worth far more than the end-to-end figure shows:

```
n=5000    bidirectional   6.542ms   forward-only   49.505ms   speed-up  7.57x
n=20000   bidirectional   9.261ms   forward-only  165.997ms   speed-up 17.92x
```

## 2. Three measurements that changed the design

**The first benchmark measured nothing.** A single-pair query put the two-sided search at 12.382 ms
against the forward-only walk's 13.268 ms — indistinguishable from noise, because almost all of
that time is fixed per-query setup, not search. Amortising over 200 pairs was the first fix; the
single-pair row is kept in the harness as a caution.

**The second benchmark measured the wrong thing.** With 200 pairs the two were still level at
≈1.19 s, because the query re-scanned the whole label to bind the destination on every source row.
Indexing the destination property and binding it once dropped the query to 277 ms and finally let
the search show.

**Enabling the reverse CSR for `DirOut` was a 26% regression.** A `DirOut` operator never read the
reverse CSR before this change, so `Init` never built the `revToFwd` table. Building it costs
O(E·d) — two million slot comparisons on this fixture — and that exceeded everything the search
saved: 277.7 ms → 351.1 ms end to end, *while the search itself was 17.9× faster*. The table is now
built only for `DirIn`/`DirBoth`, exactly as before; a `DirOut` path resolves its own ≤ d hops
against the forward CSR in O(deg) each.

## 3. Scope, and what is deliberately left out

Delivered: **`DirOut`, untyped, single-path `shortestPath()`**. Everything else keeps the
forward-only walk, which is retained as `bfsShortestPathForward` and is both the fallback and the
differential reference.

- **`allShortestPaths` is untouched**, as the task required: joining a multi-predecessor DAG across
  a meeting point is materially harder and that operator must stay level-synchronous.
- **A type filter disables it.** The filter is keyed by forward edge position, so the backward half
  must map each reverse slot it scans. The prebuilt table is the 26% regression above; resolving
  per-slot is cheap but has an unresolvable case, and being permissive there admitted edges the
  filter excludes — the differential suite caught the two-sided search finding paths the
  forward-only walk correctly rejected. Shipping a check that is either slower than no change or
  occasionally wrong is not an option, so the typed case waits for a reverse type check that is
  both exact and cheap.
- **`DirIn` / `DirBoth` are excluded**, on a finding this task surfaced and did not chase: under a
  type filter the *forward-only* reference returns a path whose hops use an excluded edge. Both
  algorithms agreeing on a path the filter should reject points at the shared reverse-slot type
  check, not at the new code. Widening before that is understood would build on an unexamined
  foundation.
- **A placeholder reverse CSR falls back.** Several in-tree tests construct a `DirOut` operator with
  `buildCSR(n, nil)` as its reverse, because nothing read it. An O(1) shape check turns that into a
  fallback instead of a silent "no path".

## 4. Correctness

`TestBiBFS_DifferentialAgainstForwardOnly` runs every ordered pair of every fixture — chain,
diamond, disconnected, self-loops, parallel edges, multigraph, star, plus six seeded random graphs
— through both algorithms and requires the same reachability and the same path length. It then
**validates the returned path against the graph**, not against the other algorithm: every hop's
recorded forward position must be an edge that genuinely connects the two nodes the hop claims, in
the orientation it records, with no relationship handle repeating.

That last part is load-bearing. The backward half resolves each hop's orientation by a rule that is
the dual of the forward half's, and inverting it yields a path of the *right length* whose hops
hydrate to the wrong relationship instances. Mutation-testing confirms the suite catches it:

```
hop 1 claims 2→3 but its edge (fwdPos 2, dir 1) runs 3→2
```

Relationship-uniqueness holds by construction — a joined walk of minimal length cannot repeat a
vertex, since excising the implied cycle would give a shorter walk — but the join checks it anyway
and abandons a candidate meeting point that fails, falling back to the forward-only search if every
candidate does.

## Reproduce

```bash
go test -tags=r4audit -run 'TestShortestPathBounded' -v -timeout 60m ./bench/r4audit/
go test -run 'TestBiBFS' ./cypher/exec/
```
