# Two-sided `shortestPath` for typed, `DirIn` and `DirBoth` searches — 2026-07-28 (rmp #2236)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harness `bench/r4audit/shortestpath_test.go`, build tag
  `r4audit`. Directed graph of average out-degree 10, the shape the round-3 head-to-head used.
- Best of 5. Each query drives **200 source rows** against one fixed destination.
- "before" is the tree at `6140d621` — rmp #2220's two-sided search, which served untyped `DirOut`
  and nothing else. Both columns are measured on the same harness in the same session.

## 1. Result

At N = 20000:

| shape | before | after | change |
|---|---|---|---|
| `shortestPath((a)-[:K*..6]->(b))` typed `DirOut` | 274.359 ms | **21.168 ms** | **13.0×** |
| `shortestPath((a)-[:K*]->(b))` typed unbounded `DirOut` | 273.946 ms | **21.640 ms** | **12.7×** |
| `shortestPath((a)<-[:K*..6]-(b))` typed `DirIn` | 696.143 ms | **21.255 ms** | **32.8×** |
| `shortestPath((a)<-[*..6]-(b))` untyped `DirIn` | 569.240 ms | **18.720 ms** | **30.4×** |
| `shortestPath((a)-[:K*..6]-(b))` typed `DirBoth` | 967.989 ms | **39.126 ms** | **24.7×** |
| `shortestPath((a)-[*..6]->(b))` untyped `DirOut` (control) | 17.231 ms | 17.218 ms | unchanged |
| `allShortestPaths(…)` (control, untouched) | 606.887 ms | 605.376 ms | unchanged |

At N = 5000:

| shape | before | after | change |
|---|---|---|---|
| typed `DirOut`, bounded 6 | 72.761 ms | **7.823 ms** | **9.3×** |
| typed `DirOut`, unbounded | 74.265 ms | **8.414 ms** | **8.8×** |
| typed `DirIn` | 161.123 ms | **7.106 ms** | **22.7×** |
| untyped `DirIn` | 139.530 ms | **6.217 ms** | **22.4×** |
| typed `DirBoth` | 197.046 ms | **9.832 ms** | **20.0×** |
| untyped `DirOut` (control) | 6.805 ms | 6.961 ms | unchanged |

**The typed shape now sits within 1.23× of the untyped one** (21.168 ms against 17.218 ms), the
residual being the per-scanned-edge forward-filter map lookup plus the one-off admit-bitset build.

**No regression on the untyped control.** The two rows differ by 13 µs on a 17 ms query. Across six
runs of the harness in this session the untyped `DirOut` row measured 17.231 ms and 18.843 ms on the
old tree and 17.218 ms, 17.496 ms, 17.679 ms and 18.157 ms on the new one — the new tree's range
sits inside the old tree's, so any difference is run-to-run variance rather than signal.

`allShortestPaths` is the untouched control in the strongest sense: no operator code and no shared
helper changed for it, and it measures within 0.2%.

## 2. Two measurements that changed the design, and one that reversed a conclusion

**`Init` does not run once per query — it runs once per outer ROW.** This is the finding the whole
task turned on. A first attempt built the reverse admit bitset in `ShortestPath.Init` and made the
typed shape *worse*: 286.987 ms → 503.106 ms, a 75% regression. The CPU profile put 61.80% of the
run inside `revTypeAdmitSet`, of which 35% was Go map iteration, and the call graph was
unambiguous:

```
CorrelatedApply.Next → ShortestPath.Init → revTypeAdmitSet → runtime.mapIterNext
```

A `shortestPath` whose endpoints come from an outer pattern sits under a `CorrelatedApply`, which
re-`Init`s its inner plan for every outer row. The 200-row benchmark was therefore building the same
table 200 times. Both reverse-side tables derive only from the CSR snapshots, which are fixed at
construction, so the build is now once per operator (`ShortestPath.revPrepared`).

**That retroactively explains #2220's headline measurement, and refutes its diagnosis.** #2220
recorded "enabling the reverse CSR for `DirOut` was a 26% regression — 277.7 ms → 351.1 ms —
because building `revToFwd` costs O(E·d)". The regression was real; the attribution was wrong. It
was 200 builds, not one expensive build: measured in isolation the table costs 0.71 ms at E = 200k
(§3), so 200 of them account for ~142 ms of the ~73 ms gap and then some. With the build hoisted
out of the per-row path the prebuilt table #2220 rejected on measurement is simply affordable.
**A per-query cost measured inside a correlated inner plan is not a per-query cost.**

**And the third measurement refuted THIS task's own premise.** #2236's technical requirements
called for a reverse type check "that costs O(1) per slot", on the reading that the O(E·d) mapping
was the blocker. It was not, and a replacement built for speed would have been the wrong change —
see §3.

## 3. Obstacle (1): the reverse type check, and why speed was the wrong axis

#2236 required "an exact reverse type check that costs O(1) per slot — for instance a
reverse-position-keyed filter built alongside the forward one at the same cost, rather than derived
from it". The reverse-position-keyed filter is what shipped. The *reason* it shipped is not the one
the requirement anticipated.

A relationship-type filter is keyed by FORWARD edge position (`buildEdgeTypeFilter`), so the half of
the search that scans reverse slots must decide admission for a reverse slot. #2220 raised two
objections to the available mappings: the prebuilt `revToFwd` table was too slow, and the per-slot
forward-CSR scan had an unresolvable case that had to be answered permissively. **Only the second
objection survived.** `BenchmarkRevToFwd` times the two mapping algorithms on the benchmark's own
graph shape, with no query around them:

| n (degree 10) | O(E·d) per-slot scan | O(V+E) transpose replay | replay vs scan |
|---|---|---|---|
| 5000 | 182.7 µs · 401 kB · 1 alloc | 159.9 µs · 844 kB · 3 allocs | 1.14× faster, 2.1× the bytes |
| 20000 | 711.8 µs · 1.61 MB · 1 alloc | 583.5 µs · 3.38 MB · 3 allocs | **1.19× faster**, 2.1× the bytes |

A factor of d = 10 in operation count buys 19% in wall clock, and costs an allocation regression.
The scan's comparisons are sequential array reads at roughly 0.35 ns each; the replay's writes
scatter across destination buckets. **Asymptotics did not decide this one, and a change justified on
speed would have been the wrong change** — it would have taken a documented allocation regression in
three operators for a gain nothing needed, against this project's own rule that a change regressing
allocations/op without justification must not be merged.

What the replay is actually for is EXACTNESS. Every reverse CSR in this module comes from
`csr.CSR.BuildReverse`, a counting-sort transpose that scatters each forward arc into its
destination's bucket, walking the forward CSR in (source ascending, slot order). Replaying that
scatter reproduces the pairing and, crucially, **either validates every slot or declines outright** —
there is no unresolvable case to answer permissively, which is exactly what a type check cannot
tolerate and what let a typed search route over an excluded edge. `revTypeAdmitSet` replays it and
sets a bit per admitted edge, so the per-slot test is one word read and shift.

Being a replay of another function's internals, it validates rather than assumes: matching slice
shapes, the arc must fit its destination bucket, the reverse slot must record the forward slot's
source, and — when both CSRs carry handles — the same handle, which pins WHICH of several parallel
arcs it is. One mismatch declines the replay, and the typed two-sided search then keeps the
forward-only walk.

Consequently the scope is narrow and deliberate:

- **`buildRevToFwd` is unchanged.** The position mapping keeps the per-slot scan, so
  `VarLengthExpand` and `AllShortestPaths` are untouched in behaviour *and* in allocation profile.
  `TestBuildRevToFwd_TransposeMatchesScan` pins the two against each other over every fixture so
  they provably agree wherever both apply.
- The **handle-less** case is covered. The benchmark fixture uses the Go API's `AddEdge`, which
  stamps no handles, so any handle-keyed admission scheme would have missed this measurement
  entirely — a design chosen from the code rather than the fixture would have looked correct and
  measured nothing. The replay is positional and needs no handles; where they exist it uses them
  only to validate.

## 4. Obstacle (2): the shared reverse-slot type check

Resolved first and separately, as #2236's technical requirements ordered, and recorded in
[`shortest-path-type-filter-2026-07-28.md`](shortest-path-type-filter-2026-07-28.md). It was a
defect: the resolution signalled failure by returning its own input, so a slot whose mapping
legitimately equalled its reverse position skipped the filter and was admitted regardless of type.
`DirIn` routed over an excluded edge. Fixing it is what made this widening safe to build on rather
than a wider surface over an unexamined foundation.

## 5. Why no search code changed to widen the direction

`biExpand` was written from the forward/backward duality rather than from the `DirOut` case: it
scans forward-traversed path edges unless `dir` is `DirIn` and reverse-traversed ones unless `dir` is
`DirOut`, and each search picks its CSR by which half it is. For `DirIn` that makes the forward
search read the reverse CSR from `src` and the backward search read the forward CSR from `dst` — the
exact dual — and for `DirBoth` both halves read both. Hop resolution keys off each predecessor
entry's own CSR (`spPredEntry.fwd`), never off `op.dir`. The widening is therefore entirely in
`canBidirectional`.

## 6. Correctness

`TestBiBFS_DifferentialAgainstForwardOnly` now runs **all three directions × both reverse-CSR
bucket orders × four type-filter configurations** over every fixture (chain, diamond, disconnected,
self-loops, parallel edges, multigraph, star, anti-parallel pair, plus six seeded random graphs),
every ordered pair of every fixture — 336 configurations, of which 282 exercise the two-sided search
and 54 the fallback, asserted by coverage counters so neither path can go dead unnoticed.

**It compares three answers, not two.** Both arms now consult one reverse-side type check, so a
defect inside that structure would make both wrong the same way and a two-arm differential would
report agreement — which is how two earlier audits in this project lost a real defect. The third
answer is an absolute oracle: plain BFS over the fixture's raw edge list, honouring direction and
the admitted edge set, reading no CSR, no filter map and no operator state. Every returned path is
additionally validated against the graph — each hop's recorded forward position must be an edge that
genuinely connects the two nodes the hop claims in the orientation it records, no relationship handle
may repeat, and **every hop must use an admitted edge**.

Mutation-tested, both directions:

- making `revTypeAdmitSet` admit every reverse slot (the obstacle-2 defect, relocated into the new
  structure) fails the oracle — `2→0: forward-only reachability=true but the oracle says dist=-1` —
  and fails `TestRevTypeAdmitSet_MarksExactlyTheAdmittedSlots`,
  `TestBiBFS_TypeFilteredSearchIsTwoSidedAndExact` and both obstacle-2 regression tests;
- restoring #2220's gate (`DirOut` and untyped only) fails the differential's admission assertion in
  every widened configuration.

`TestBuildRevToFwd_DeclinesWhatIsNotATranspose` pins the validation against five reverse CSRs —
canonical, edge-list bucket order, empty placeholder, rewritten arc contents, and two handles
swapped within one bucket — and requires `buildRevToFwd` to answer via the scan in each declining
case.

## 7. Scope

- `allShortestPaths` is untouched, as #2236 acceptance criterion 4 requires: no two-sided search is
  admitted into it. It inherits `buildRevToFwd`'s speedup, which changes no answer.
- The placeholder-reverse-CSR fallback still engages
  (`TestBiBFS_FallsBackWithoutAUsableReverseCSR`).
- A typed search over a reverse CSR that is **not** the canonical transpose keeps the forward-only
  walk. The `revToFwd` table would also answer, but it answers permissively where a mapping is
  unknown, and the two halves scan different slots — so the two-sided half could admit an edge the
  forward-only walk never reaches. That is the #2220 symptom and it is not worth reintroducing for a
  fallback that is no faster.
- **Filed, not fixed here:** `AllShortestPaths.Init` and `VarLengthExpand.Init` rebuild `revToFwd`
  on every re-`Init` too, so they pay it per outer row under a `CorrelatedApply` exactly as
  `ShortestPath` did. Same defect class, same one-line remedy; kept out of this task because
  acceptance criterion 4 fences `allShortestPaths` and `VarLengthExpand` is a different operator.

## Reproduce

```bash
go test -tags=r4audit -run 'TestShortestPathBounded' -v -timeout 90m ./bench/r4audit/
go test -run 'TestBiBFS|TestBuildRevToFwd|TestRevTypeAdmitSet|TestShortestPath_TypeFilter' ./cypher/exec/
```
