# String equality on a btree index: seek instead of scan — 2026-07-28 (rmp #2231)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. Harness `cypher/index_keytype_bench_test.go`
  (`BenchmarkIndexedPointLookup`), the same key-type × index-type matrix #2226 introduced.
- `benchstat` over `-count=6`.

## 1. Result

| case | before | after | change |
|---|--:|--:|---|
| **string / btree / n=5000** | 1 063.3 µs | **5.256 µs** | **202× faster** (−99.51%, p=0.002) |
| **string / btree / n=20000** | 4 393.8 µs | **5.177 µs** | **849× faster** (−99.88%, p=0.002) |
| string / hash / n=5000 | 3.484 µs | 3.520 µs | +1.02% (p=0.041) |
| string / hash / n=20000 | 3.465 µs | 3.462 µs | ~ (p=0.509) |
| numeric / hash / n=5000 | 4.679 µs | 4.665 µs | ~ (p=0.818) |
| numeric / btree / n=5000 | 4.647 µs | 4.650 µs | ~ (p=0.855) |
| numeric / hash / n=20000 | 4.633 µs | 4.591 µs | ~ (p=0.065) |
| numeric / btree / n=20000 | 4.599 µs | 4.561 µs | −0.82% (p=0.009) |

`sec/op` geomean −77.86%. The three cells that already worked are unchanged, which is the
no-regression requirement (AC 5).

**Allocations are now flat in the node population** (AC 2) — the signature that distinguishes a
seek from a scan:

| case | before | after |
|---|--:|--:|
| string / btree / n=5000 | 219 364 B/op, 14 831 allocs/op | 13 361 B/op, **107 allocs/op** |
| string / btree / n=20000 | 819 525 B/op | **13 351 B/op** |

Before, the footprint tracked the population (219 k → 819 k as nodes went 5 k → 20 k). After, it is
constant (13 361 → 13 351). That is what proves the label is no longer being walked.

## 2. Cause

`extractSingleStringCmp` rejected `=` outright, while its numeric counterpart accepted it and
degenerated it into the closed range `[v, v]` over the numeric companion btree (#2169). So a user
who asked for a **btree on a string property** — reasonable when the same property also serves
range predicates — got a full label scan for an equality on it, even though
`findBoundStringBTree` could already locate the covering index. The identical predicate written as
`a.sk >= 's10000' AND a.sk <= 's10000'` already seeked.

The fix is one operator: accept `=` and build the degenerate closed range, then hand it to the
**same** `rangeCountWins` selectivity gate and the **same** residual-`Filter` discipline the numeric
path uses. Numeric and string equality now share one gate and one rewrite site.

## 3. Why the degenerate range is exact here, and only here

GoGraph orders strings by code point (UTF-8 byte order), so `[v, v]` selects precisely the keys
whose bytes equal `v`'s: no two distinct strings compare equal under a byte order, so no collation
question arises for **equality**. That reasoning does not extend to inequality over a
collation-sensitive alphabet, which is why only `=` was added and the range operators keep their
existing treatment — round-4 finding C3 leaves the collation ruling open (#2224).

The seek is treated as a **superset** regardless: the residual `Selection` `Filter` is always
retained and re-applies the exact property predicate, so the seek can only narrow what the filter
examines, never change what it admits.

## 4. The temporal hazard was already closed

A Cypher temporal is stored as an SOH-tagged string, so its raw encoded form *is* a string at the
storage layer. The concern was that a pathological string literal could seek-match a node the
scan+filter path rejects. It cannot: the string **btree** uses the very same
`projectStringPropValue` gate as the hash index, which refuses those encodings, so a btree key is
never created for a temporal — already documented as "load-bearing for an ORDERED index" (#1505).
The rewrite inherits the exclusion rather than restating it, and
`TestBTreeStringEq_TemporalIsNotSeekMatchable` pins it: all six temporal tag bytes, the plain ISO
form, and a control proving the date still matches itself.

## 5. Evidence discipline

Two things worth recording, because each nearly produced a worthless test.

**The differential had to be proved non-degenerate.** The scan arm is produced by *parameterising*
the key (which `extractSingleStringCmp` declines) rather than by dropping the index, so both arms
run against the same engine and the same index. But at a few hundred nodes the selectivity gate
keeps the **literal** form on a scan too — so a differential seeded there compares the scan against
itself and passes over any defect. The test now seeds above the gate's floor **and asserts the two
arms take different plans** before comparing rows.

**The first fault injection was a no-op.** Flipping the degenerate bounds to exclusive changed
nothing, because `exec.RangeBound.Include` is *metadata only*: `NodeByIndexRangeScan` always emits
the inclusive `[lo, hi]` superset and leaves open/closed semantics to the residual filter. The
injection that does bite is shifting the range off the key, and the differential fails on it as
required.

Keys covered: present-and-ordinary, a strict prefix of other keys (`s1` against `s10`, `s100`, …),
the empty string, a multi-byte UTF-8 key (`ключ-日本語-🔑`), an absent key, and an absent key beyond
the seeded range.

## 6. Reproduction

```bash
go test ./cypher/ -run '^$' -bench BenchmarkIndexedPointLookup -benchmem -count=6
go test ./cypher/ -run TestBTreeStringEq
```
