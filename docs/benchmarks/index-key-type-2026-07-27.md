# Indexed point lookup by key type — measured

**Task #2226** · sprint 326 · 2026-07-27 · Apple M4 (10 cores) ·
`go test -run '^$' -bench 'BenchmarkIndexedPointLookup$' -benchmem -benchtime=100x -count=6 ./cypher/`

Every figure is the median of six runs. The benchmark lives in
`cypher/index_keytype_bench_test.go` and is permanent: it pins all four cells of
the key-type × index-type matrix so the ratio between them cannot silently
regress.

The **before** column is the same tree with the companion build removed from the
hash path only, so the two columns differ by exactly that and nothing else.

---

## 1. The finding inverts the premise of the task

The task was opened on a round-4 measurement reading *"indexed STRING point
lookup 6 µs, indexed NUMERIC point lookup 762 µs — 127× between two lookups
that differ only in the key type"*, and asked why the key type mattered.

It does not. Measuring the full matrix shows the **diagonal works and the
off-diagonal full-scans**, which means the variable is not the key type but
whether the key type matches the index type:

| key | index | before | allocs |
|---|---|---|---|
| string | hash (default) | 3 492 ns | 63 |
| string | btree | 4 267 703 ns | 59 831 |
| numeric | hash (default) | 3 825 694 ns | 59 590 |
| numeric | btree | 5 095 ns | 98 |

An allocation count that tracks the node population (59 590 at n = 20 000) is
the signature of a full label scan; a flat count in the tens is an index seek.

## 2. Root cause

`projectStringPropValue` (`cypher/index_binding.go`) rejects every property kind
other than `PropString`:

```go
if !ok || pv.Kind() != lpg.PropString {
    return "", false
}
```

A hash index therefore admits string keys only. Hash is the **default** index
type (`ir.IndexTypeHash` is the zero value), so

```cypher
CREATE INDEX person_age FOR (n:Person) ON (n.age)
```

on an integer property built an index that **can never hold a single entry**.
`SHOW INDEXES` still reported it:

```
name: "person_age", type: "hash", state: "ONLINE", properties: ["age"]
```

— indistinguishable from a working index. Queries stayed correct, because the
planner declines to seek an index it cannot use and falls back to a scan. The
user simply had an index that did nothing, reported as present and online.

That is the defect worth naming: not a wrong answer, but the **silent absence of
the thing the user asked for**, with a status field that says otherwise.

## 3. The fix

The unified numeric companion btree already existed (#1652) and already made
numeric point lookups fast — it was built only alongside a *btree* CREATE INDEX.
It is now built alongside a hash index too, so the default CREATE INDEX serves
both key types. The equality rewrite (#2169) finds it through its deterministic
internal name, with no user-named btree involved.

Nothing about the hash index changed, no new index kind was introduced, and no
persisted format changed: only the user's own index definition is written to the
WAL, and `registerRecoveredIndexes` re-derives the companion from it — now for
both index kinds.

| key | index | before | after | change |
|---|---|---|---|---|
| numeric | hash, n = 5 000 | 946 691 ns | **5 384 ns** | **176×** |
| numeric | hash, n = 20 000 | 3 825 694 ns | **4 108 ns** | **931×** |
| string | hash, n = 20 000 | 3 492 ns | 3 246 ns | unchanged |
| string | btree, n = 20 000 | 4 267 703 ns | 4 272 902 ns | unchanged |
| numeric | btree, n = 20 000 | 5 095 ns | 3 873 ns | unchanged |

Allocations on the fixed cell fall from 59 590 to **98**, the same flat count the
btree path already had. The change is surgical: only the target cell moves.

The numeric point lookup now matches the string one — 4 108 ns against 3 246 ns
at n = 20 000, within 27 % — which answers the task's question of whether the
gap could be closed. It could, and the residual difference is the btree descent
against a hash probe, not an access-path failure.

## 4. Correctness

`cypher/index_hash_numeric_companion_test.go` pins:

- the companion is built for the default, the explicit hash, and the explicit
  btree index alike;
- a numeric point lookup returns the right row through all three spellings
  (inline map, `WHERE =`, and the explicit degenerate range);
- openCypher value equality across the int/float divide — `7` and `7.0` are one
  index entry, so a lookup by either finds both nodes;
- `DROP INDEX` removes the companion it created, and does **not** remove one a
  surviving index over the same (label, property) still relies on. That last case
  is why `indexCoverage` now counts hash indexes as covering: a btree-only view
  would have stripped the shared companion and silently returned the survivor's
  numeric lookups to full scans. Reverting that one condition fails the test.
- the string path is unaffected, which is acceptance criterion 4.

The companion's WAL-failure unwind drops it only when no other surviving index
covers the pair, and runs after the user index is dropped so the orphan check
does not count the index being unwound as a reason to keep it.

## 5. The mirror-image cell — RESOLVED in #2231 (2026-07-28)

At the time of writing, `string` + `btree` remained a full scan — 4.27 ms at
n = 20 000, unchanged by this work and unchanged before it — because the
degenerate-range rewrite #2169 built for numeric equality had no string
counterpart. It was filed separately rather than left implicit, because the matrix
above makes it visible.

**It has since been fixed.** `extractSingleStringCmp` now accepts `=` and builds
the same degenerate closed range over the bound string btree, sharing the numeric
path's selectivity gate and residual filter: **4 393.8 µs → 5.177 µs at
n = 20 000 (849×)**, with allocations flat in the node population instead of
tracking it. The matrix above is therefore superseded in that one cell; see
[`btree-string-eq-2026-07-28.md`](btree-string-eq-2026-07-28.md) for the current
figures and for two evidence-discipline notes (a differential that had to be
proved non-degenerate, and a fault injection that turned out to be a no-op
because `RangeBound.Include` is metadata only).

## 6. Reproducing

```bash
go test -run '^$' -bench 'BenchmarkIndexedPointLookup$' -benchmem -benchtime=100x -count=6 ./cypher/
```

`BenchmarkIndexedPointLookupWhere` covers the `WHERE` spelling and an explicit
closed range, confirming all three reach the same access path.
