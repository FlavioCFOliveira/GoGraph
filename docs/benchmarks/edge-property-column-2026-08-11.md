# Merging the columnar edge-property backings (rmp #2406)

**Sprint** 339 · **Date** 2026-08-11

Remediation of finding F5 of
[`../memory-vs-neo4j-memgraph-2026-08-11.md`](../memory-vs-neo4j-memgraph-2026-08-11.md): the
columnar edge-property tier stores a value optimally — **7.95 B for an `int64`, the payload and
nothing else** — but instantiated a **240-byte `edgePropColumn` once per (source node, property
key)**, which is 34 B/edge at degree 8 and 144 B/edge at degree 2.

## 1. What the 240 bytes were

Nine slice headers, 216 of the 240. A column has exactly ONE live value backing, so for a dense
fully-present `int64` column eight of those nine headers were nil — the struct paid 192 bytes to
hold nils.

Four of them are **8-byte-word, pointer-free** and mutually exclusive:

| backing | held | now |
|---|---|---|
| `i64 []int64` | `PropInt64` values | `nums`, two's-complement |
| `f64 []float64` | `PropFloat64` values | `nums`, via `math.Float64bits` |
| `boolBits []uint64` | one bit per slot | `nums`, unchanged |
| `packed []uint64` | frame-of-reference date residuals | `nums`, unchanged |

They are now one `nums []uint64`. The merge is **free**: `int64`↔`uint64` is the two's-complement
identity, and `Float64bits`/`Float64frombits` is an exact bijection including NaN payloads and
negative zero. It also collapses eight typed helpers (`cloneI64`, `growF64`, `spliceI64`, …) onto
the `uint64` forms that already existed.

`days []int32` was deliberately **left out**: it is 4 bytes per slot, and folding it into a
`[]uint64` would double the date column the sprint-222 design specifically optimised.
`str []string` and `boxed []PropertyValue` carry pointers and cannot share a word backing.

## 2. Result

`unsafe.Sizeof(edgePropColumn)` **240 → 168 bytes** (−30 %).

Marginal cost of one `int64` edge property, measured at constant 4 000 000 edges over three
degrees so the per-edge and per-source-node terms separate (`bench/memprobe`):

| degree | before | after |
|---:|---:|---:|
| 2 | 144.01 | **112.03** |
| 8 | 42.01 | **33.98** |
| 32 | 16.50 | **14.50** |

Refitting `cost = a + b/degree`:

| term | before | after |
|---|---:|---:|
| **a** — per edge, the payload | 7.95 B | **8.00 B** (unchanged, as intended) |
| **b** — per (source node, key) | 272 B | **208 B** (−23.5 %) |

The refit predicts degree 8 at 34.01 against 33.98 measured.

**It is also faster.** Interleaved A/B, five rounds, of the columnar benchmarks:
sec/op geomean **−4.48 %**, B/op geomean **−9.98 %**, **allocations unchanged** — a smaller struct
is cheaper to zero and to copy on every copy-on-write rebuild, and the integer casts are free.

## 3. The acceptance criteria this does NOT meet, and why

Task #2406 asked for the per-edge cost at degree 8 to drop **below 20 B** and `b` **below 120 B**.
Delivered: 33.98 and 208. The `a ≤ 8.5 B` criterion is met.

The gap is structural, not effort. After the merge the struct is
`nums`(24) + `days`(24) + `str`(24) + `boxed`(24) + `valid`(24) + `idx`(24) + 24 of scalars. The
five remaining slice headers are **not** mutually reducible the way the four merged ones were:

- `days` is 4-byte and merging it would regress the date column;
- `str` and `boxed` carry pointers;
- `valid` (dense-only) and `idx` (sparse-only) *are* mutually exclusive, but have different element
  widths, so merging them would widen a sparse column's index from 4 to 8 bytes per present slot.

Reaching `b < 120` needs **type erasure** — one `data any`, or an `unsafe.Pointer` plus the kind
tag as `index.NodeSet` already does in this tree. Both were considered and neither was taken here:

- `data any` costs a heap-allocated slice header and an indirection **on every value access**, in
  the one tier whose entire purpose is vectorised access.
- `unsafe.Pointer` avoids both, but the backings are `append`-ed at 15 sites and `cap`-checked at
  5, so it needs pointer/len/cap write-backs at each — a materially more delicate change than the
  mechanical, provably value-preserving merge landed here.

That decision belongs to whoever picks the task up next, with these numbers in hand. **The honest
framing is that this delivered a third of the available win at a fraction of the risk**, and the
remaining two thirds is a design choice about dispatch cost, not more of the same work.

## 4. Context: how much this matters now

Stated so the number is not read as more than it is. On the **Go API**, where this tier is the
whole property cost, an `int64` edge property went 42.01 → 33.98 B/edge at degree 8 (−19 %), and
at degree 2 — where the per-source-node term dominates — 144.01 → 112.03 (−22 %).

Through **Cypher**, a property-carrying relationship costs 976.36 B/edge after sprint 339's other
two changes, so this tier is about 3.5 % of it. The remaining bulk is the per-instance and
per-handle property stores, which is the same class of finding as #2401 and is what #2403 would
settle.

## 5. What was verified

- `make ci` exit 0, TCK 3897/3897, coverage 87.1 %.
- The tier's own suites — dense, sparse COO, frame-of-reference bit-packed dates, the validity
  bitmap, the fused insertion path and the sparse-recovery ACID test — all green, including the
  columnar property oracle. Those tests reach into the backings directly and were updated with the
  fields they assert on, so they still pin the physical representation rather than only the
  public behaviour.
