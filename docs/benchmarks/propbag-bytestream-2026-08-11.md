# A byte-stream node property bag, after Memgraph (rmp #2408)

**Sprint** 339 · **Date** 2026-08-11 · **Prior art** Memgraph `v3.9.0`, `src/storage/v2/property_store.cpp`

Remediation of finding F4 of
[`../memory-vs-neo4j-memgraph-2026-08-11.md`](../memory-vs-neo4j-memgraph-2026-08-11.md), the
worst ratio in that audit: GoGraph spent **146.66 B on one 9-character node property against
Memgraph's 19.45 B (7.54×)**, and 297.50 B against 36.18 B for a three-property node.

## 1. What was taken from Memgraph, and what was not

Read as prior art under the [inspiration protocol](../../CLAUDE.md#the-inspiration-protocol) and
reimplemented in Go; no source was copied.

**Taken — one self-describing metadata byte per record** (`property_store.cpp:99-205`):

```
metadata: 0bTTTT_IIPP   4-bit type · 2-bit property-id width · 2-bit payload width
then the property id in the narrowest of 1/2/4/8 bytes
then a payload whose shape the type determines
```

with two consequences worth naming: a **boolean's value rides in the payload-width field and
occupies no payload bytes at all**, and an integer is stored at the narrowest width that holds it.

**Not taken — the sort.** Memgraph keeps its records ordered by property id and relies on that for
a merge-style rewrite. Here they are unsorted, because iteration order is not observable — the
public accessors return a `map[string]PropertyValue`, and the snapshot serializer emits
self-describing records whose on-disk order never depended on bag order — and an unsorted stream
makes an append O(1) in the property count.

**Adapted — zig-zag integers.** Memgraph reaches narrow negatives by storing a signed type at each
width. Go's `binary` helpers are unsigned, so the same end is reached by zig-zagging, which keeps
`-1` at one byte instead of eight. `TestPropBagEncoding_UsesNarrowestWidths` pins it.

**Scope.** Only the four scalar kinds stream (`PropString`, `PropInt64`, `PropFloat64`,
`PropBool`). `PropTime`, `PropBytes` and `PropList` are variable-shaped or hold references and
promote to the pre-existing map tier, as does any bag past `smallBagMax`.

## 2. The immutability invariant

Every mutation allocates a **new buffer sized exactly to its contents**; a published buffer is
never written again and none carries spare capacity. That is what licenses `unsafe.String` in the
decoder: a string handed to a caller aliases bytes nothing will ever modify, so **reading a string
property still allocates nothing**, which a copying decoder would have lost.

It also disposes of a hazard spare capacity would create. `propBag` is stored by value and copied
freely; two copies sharing a backing array with room to grow could each append into the same bytes
and disagree about what is there. With exact-size buffers there is nowhere to grow into.

## 3. Result

Live heap for 200 000 bags (`BenchmarkPropBagRepresentation`, `-benchtime 1x`):

| shape | before | after | |
|---|---:|---:|---|
| one 9-character string property | 96.11 B | **48.08 B** | **2.00×** |
| two strings and one integer | 224.0 B | **64.06 B** | **3.50×** |

Whole node through the Go API, 1 000 000 nodes (`bench/memprobe`):

| | before | after |
|---|---:|---:|
| node + 1 label | 210.58 | 210.66 (unchanged, as expected) |
| node + 1 label + 1 property | **375.23** | **327.27** |
| ⇒ the property term | 164.65 | **116.61** |

The three-property shape encodes to **exactly 26 bytes**, the same figure Memgraph's format
produces for it — the arithmetic transferred intact. The remaining 116.61 B of the property term
is the `nodePropShards` map entry (~101 B/node), which this change does not touch; that is #2405.

## 4. The cost, stated plainly

This trades CPU for memory, and the microbenchmarks show the CPU side clearly. Interleaved A/B,
five rounds, `git stash` between arms:

| benchmark | sec/op | allocs/op |
|---|---:|---:|
| `PropRead/deltas=off` | **+147 %** (4.99 → 12.34 ns) | 0 → 0 |
| `PropWrite/nodes=1000000` | +44 % | **3 → 7** |
| `EdgeSideRead_PropertiesByHandle` | +43 % | **2 → 4** |

**None of it survives to the query level.** The same A/B on
`BenchmarkReadPhaseAttribution` — whose query is `MATCH (n:Acct {id: $id}) RETURN n.bal AS b`, so
it both seeks on a property and returns one — gives **geomean −0.28 % on sec/op with allocations
unchanged**, every arm statistically flat. A bag read costs nanoseconds and a query costs
microseconds. Bulk build shows the same: 200 000 three-property bags went 88.2 → 91.2 ms, **+3.4 %**.

The decision recorded, so it can be revisited: a 2.5× microbenchmark regression on a read that is
0.3 % of a query is worth 48–160 B on every property-bearing node. Were a workload found in which
`propBag.get` is the hot path, this is the trade to reopen.

## 5. What was verified

- **Round-trip is bit-exact** for every edge the encoding has: NaN, negative zero, both
  infinities, `MinInt64`, `MaxInt64`, the width boundaries at 127/128/255/256/65535/65536, the
  empty string, a 65 536-byte string, UTF-8, an embedded NUL, and the `\x01`-tagged temporal
  strings the property layer carries. Floats are compared by `Float64bits`, because `NaN != NaN`
  and `-0.0 == 0.0` would let a sign-losing encoder pass.
- **100 rapid-generated bags** are checked against a plain Go map after every operation.
- `make ci` exit 0, TCK 3897/3897, coverage 87.1 %.
- **The tests can fail.** Four mutations were applied; two escaped the first version and both
  taught something:
  - dropping the zig-zag **round-tripped perfectly**, because a symmetric change to encoder and
    decoder is invisible to a round-trip test while doubling the footprint. It is now caught by
    `TestPropBagEncoding_UsesNarrowestWidths`, which asserts encoded **sizes** — the property the
    change exists for.
  - patching a record in place when the new one is the same width escaped because the alias test
    performed an unrelated `set` first, which **reallocated the buffer**, leaving the alias
    pointing at an old array the in-place patch never touched. The same-width overwrite now runs
    first, with nothing between it and the `get`.
  - a float stored at 32-bit precision, and a decoder that ignores the promoted map tier, were
    caught by the first version.
