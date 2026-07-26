# Design: a correlated index seek for row-bound keys

**Spike for rmp #2181** · sprint 319 · 2026-07-26 · baseline `66f0f2a`

Every figure below was measured during this spike on an Apple M4. The
measurement harness was temporary and deleted.

---

## 1. The defect, located exactly

`astLiteralToValue` (`cypher/api.go`) converts an AST leaf to a value and accepts
**literals and `$param` only**. `extractEqFromAST` calls it for the right-hand
side of an equality, so a right-hand side that is a bound `*ast.Variable` — `nm`
in `WITH 'name-1' AS nm MATCH (a:P {name: nm})` — fails to convert and the seek
rewrite declines. `resolveSeekValue` has the same shape one level down: it takes
the seek value as a **string** and parses it as source text, so it cannot express
a value that is only known per row.

There is no per-row seek operator at all. The plan for a row-bound key is
`CartesianProduct(Projection, NodeByLabelScan)` with the key equality left as a
residual filter.

---

## 2. Measurements

20 000-node label, one string property, hash index present. Entity projection
(`RETURN a`) — see §2.1 for why that matters.

| Key form | Plan leaf | N=5 000 | N=10 000 | N=20 000 |
|---|---|---|---|---|
| literal | `NodeByIndexSeek` | 5.03 µs | 4.34 µs | **4.40 µs** |
| `$param` | `NodeByIndexSeek` | 3.47 µs | 3.85 µs | **3.77 µs** |
| `WITH`-bound | scan + CartesianProduct | 2.72 ms | 5.54 ms | **13.37 ms** |

The seek is flat in N. The correlated path is linear in N. **At N=20 000 the gap
is 3 038×.**

### 2.1 A confound that had to be removed first

The same measurement with a **scalar** projection (`RETURN a.id`) showed the
literal case at 204 µs → 415 µs → 895 µs — growing linearly despite the plan
showing `NodeByIndexSeek`. That is not this defect; it is #2204, in which the
columnar scan+filter path claims a scalar-projecting Selection and never consults
the seek at all. Measuring the correlated-seek gap on a scalar projection would
have measured #2204 twice and this defect not at all.

### 2.2 The audit's complexity claim is wrong, and the correction changes the design

The audit recorded Θ(rows·N). Measured, holding N and varying the number of row
keys:

| N | 1 key | 3 keys | 30 keys | 300 keys |
|---|---|---|---|---|
| 5 000 | 2.659 ms | 2.661 ms | 2.759 ms | 2.990 ms |
| 20 000 | 11.787 ms | 12.657 ms | 12.590 ms | 12.248 ms |

A **300× increase in key count costs 12 % at N=5 000 and nothing measurable at
N=20 000.** The cost is **Θ(N + rows)**, not Θ(rows·N): `CartesianProduct`
materialises the label scan **once** per query and joins the row keys against it.

The audit's *conclusion* still holds — this is the root cause of the load deficit,
because a batched bulk load performs one full label scan per batch — but its
*shape* does not, and the difference matters:

- Under Θ(rows·N) the gain from a correlated seek would be ~N per row, i.e.
  unbounded in both.
- Under the measured Θ(N + rows) the gain per query is **N/rows**: 3 000× for one
  key, ~10× for 300 keys at N=20 000, and **nothing** once the key count
  approaches N.

That last point is the design's most important consequence: a correlated seek
must be **cost-gated**, exactly as the range seek already is. An unconditional
rewrite would regress a query whose row keys cover most of the label.

---

## 3. Design

### 3.1 Resolve the key from the expression, not from source text

Replace the string-typed seek value with an expression-typed one.

- `extractEqFromAST` gains a third case: a right-hand side that is a bound
  `*ast.Variable` (or any expression over bound variables) yields a **deferred**
  key rather than a value.
- The IR node carries that expression. `ir.NodeByIndexSeek.Value string` becomes,
  or is joined by, a field holding the AST expression, so the physical build has
  something to evaluate per row instead of something to re-parse.
- `resolveSeekValue`'s string parsing is retained only for the literal and
  `$param` forms it already serves, and is not extended. Parsing source text is
  the mechanism that made this defect possible; the fix is to stop needing it, not
  to teach it more syntax.

### 3.2 Access path: set-at-a-time, not row-at-a-time

**Chosen: set-at-a-time.** Collect the distinct row keys, probe the index once per
distinct key, OR the resulting postings into one bitmap, then join.

Why, with the trade-offs stated rather than asserted:

| | Set-at-a-time (chosen) | Row-at-a-time (`Apply`, Neo4j's shape) |
|---|---|---|
| Index probes | one per **distinct** key | one per **row** |
| Duplicate keys | free — deduplicated before probing | re-probed every time |
| Composes with bitmap intersection | yes — the OR result is a bitmap the sprint-312 label intersection can `And` directly | no — each probe yields rows, not a set |
| Memory | one bitmap over the matched postings | O(1) beyond the current row |
| Pipeline behaviour | a **barrier**: the keys must all arrive before the first probe | streaming; first row can emit immediately |
| Unbounded input | the key set must be bounded | unbounded input is fine |

The barrier is the real cost and it is worth paying here, because the machinery
that makes the set form cheap already exists: `hash.Index.Lookup` returns a
`*roaring64.Bitmap`, and sprint 312's label intersection consumes exactly that. A
row-at-a-time `Apply` would produce rows that then have to be intersected
row-by-row, discarding the bitmap tier the module already has.

The unbounded-input objection is answered by the cost gate rather than by the
shape: an input too large to collect is also an input whose key count approaches
N, where the rewrite must decline anyway.

### 3.3 The cost gate

Reuse the range seek's shape (`rangeSeekMaxSelectivity`, `rangeSeekMinLabelPopulation`)
rather than inventing a second policy:

- decline below a label-population floor, where a scan is a few microseconds;
- decline when the OR-ed posting count exceeds a fraction of the label
  population, because the measured gain vanishes there;
- decline when the key set exceeds a bound, which is the same condition seen from
  the input side.

The posting count is **exact** — it is the cardinality of the OR-ed bitmap, not an
estimate — so the gate needs no estimation margin, which is the same property that
made the range seek's gate provably non-regressing.

### 3.4 Interaction with sprint 312's bitmap intersection

They compose by construction and in one direction only. The correlated seek
produces a node-id bitmap; the multi-label intersection consumes node-id bitmaps.
So `MATCH (a:A:B {k: nm})` becomes

```
And( labelIndex(A), labelIndex(B), Or(seek(k, key) for key in keys) )
```

one intersection over three bitmaps, with no row materialised until the result is
drained. The ordering is a cost question the count store can already answer: the
smallest bitmap first.

What must **not** happen is the seek producing rows that the intersection then
filters, which is what a row-at-a-time shape would force. That is the second
reason for §3.2's choice.

---

## 4. Result-identity: proof sketch

The rewrite replaces, for each row, the predicate `a.k = key` evaluated over every
node of the label with a lookup of `key` in an index over the same label. It is
result-identical if the index's posting set for `key` equals the set of label nodes
satisfying `a.k = key`, for every key the rows can produce.

1. **Population.** A bound hash index over `(label, property)` contains exactly
   the live nodes of that label whose property holds an indexable value, and is
   maintained from the commit fan-out (`index_binding.go`). So membership is
   equivalent for any key of an indexable type.
2. **NULL keys.** A row key of NULL must match nothing: openCypher equality with
   NULL yields NULL, which fails `WHERE`. Nulls are never indexed, so the lookup
   returns the empty set — the same answer. A NULL key must therefore **not** be
   treated as "no key" (which would fall back to a scan and, worse, to a scan whose
   residual filter also returns nothing but at Θ(N)); it is a key that yields
   nothing, and it is skipped in the OR.
3. **Type-mismatch keys.** A key whose type cannot be in this index — an integer
   key against a string-keyed hash index — must contribute nothing, because
   openCypher equality across type groups is FALSE, not NULL, and no string can
   equal an integer. Skipping such a key in the OR is therefore correct, not merely
   convenient. This is the one place where the reasoning differs from the range
   seek's, whose cross-type hazard is about *ordering* rather than equality.
4. **Cross-type numeric equality.** `5 = 5.0` is TRUE in openCypher, so a numeric
   key must not be served by a type-narrow index. Task #2169 established the
   route: numeric equality goes to the float64 btree companion as the degenerate
   range `[v, v]`, whose residual filter applies the exact comparator. A correlated
   numeric key uses that path, not the string hash index.
5. **The residual filter is retained regardless.** As with the range seek, the
   original predicate remains above the access path. So even if the index
   over-returns, the result cannot change; the seek can only narrow what the filter
   examines. Points 1–4 are about not *under*-returning, which the filter cannot
   fix.
6. **Duplicate rows.** Deduplicating keys before probing does not deduplicate
   **rows**: the join must still emit one output row per (input row, matched node)
   pair, or `UNWIND ['a','a'] AS k MATCH (n {p:k})` would lose a row. The
   deduplication is an implementation detail of the probe, not of the join.

---

## 5. Estimated gain

Grounded in §2, per query at N=20 000:

| Row keys | Current (measured) | Correlated seek (projected) | Gain |
|---|---|---|---|
| 1 | 11.79 ms | ~4 µs | **~3 000×** |
| 30 | 12.59 ms | ~120 µs | ~105× |
| 300 | 12.25 ms | ~1.2 ms | ~10× |
| ≥ N/10 | 12 ms | declines to the scan | 1× by design |

The projection assumes one probe per distinct key at the measured single-seek cost
of ~4 µs, plus a bitmap OR whose cost is dominated by the probes. It is an
estimate, not a measurement: the implementation task must measure it.

For the bulk-load idiom the gain compounds differently. A load of 200 000 edges in
batches of 1 000 issues 200 `MATCH` statements; today each performs one full label
scan, so the load pays 200 × Θ(N). With the correlated seek it pays 200 × Θ(1 000
probes). That is the mechanism behind the audit's 2 184× load deficit, and it is
why this task sits on the critical path even though the per-query gain shrinks
with key count.

---

## 6. Recommendation: **GO**

The design reuses three things that already exist and are tested — the hash
index's bitmap postings, the range seek's cost-gate shape, and sprint 312's
intersection — and adds one genuinely new thing, an expression-typed seek key.

Two corrections this spike makes to its own brief, both from measurement:

1. The complexity is **Θ(N + rows)**, not Θ(rows·N). The gain is therefore N/rows
   per query and **vanishes as the key count approaches N**, which makes the cost
   gate mandatory rather than optional. An unconditional rewrite would regress
   wide-key queries.
2. Measuring this defect requires an **entity** projection. On a scalar projection
   the columnar path claims the query and the seek never runs (#2204), so the
   literal "seek" case appears to grow linearly in N. Any benchmark for the
   implementation task must use `RETURN a`, or it will measure #2204 instead.

### Consequences for the rest of sprint 319

- **#2182** (resolve the seek value from the expression) is confirmed as
  specified, and is the prerequisite for everything else.
- **#2183** (the access path) should adopt the set-at-a-time form of §3.2 and
  **must** include the cost gate of §3.3; the gate was not in the task's original
  framing because the Θ(rows·N) figure implied it was unnecessary.
- **#2184** (benchmark) must use an entity projection and must vary the key count,
  not only N, since the gain depends on the ratio.
