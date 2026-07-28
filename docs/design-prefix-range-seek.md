# Design — `STARTS WITH` prefix range seek

Design record for rmp task **#2126** (sprint 311, *Planner R2-P1: index-backed
string prefix predicate*). It fixes the rewrite rule, proves its soundness,
draws the scope boundary, and states what must be tested — before any
production code is written. **This task changes no production code.**

- **Status** — design accepted; implementation is task #2127.
- **Source finding** — `docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.1.
- **Machinery reused unchanged** — `cypher/range_seek_plan.go` (#1505, #1652,
  #2169, #2231), `cypher/exec/scan_index_btree.go`, `graph/index/btree`.

## 1. The measured gap, re-verified

The audit's numbers date from 2026-07-25; sprints 326 and 327 have landed
since, so the premise was treated as a hypothesis and re-measured on the
current tree before any design work.

Fixture exactly as the audit specifies: 50 000 `:EvPerson` nodes,
`name = "name%05d"`, btree index on `name` built through `CREATE INDEX`;
`STARTS WITH "name002"` against the semantically identical
`>= "name002" AND < "name003"`. Both return **100 rows** (asserted before
timing). Apple M4, `darwin/arm64`, `-benchmem -count=6`.

| Form | sec/op | B/op | allocs/op |
|---|---|---|---|
| `STARTS WITH "name002"` | 11.28 ms | 2 024 599 | 149 829 |
| `>= "name002" AND < "name003"` | **38.23 µs** | **25 494** | **566** |
| **Ratio** | **295×** | **79×** | **265×** |

Plans rendered by `Engine.Explain` (physical):

```
STARTS WITH "name002"              range-equivalent
─────────────────────              ────────────────
ColumnarProject                    ColumnarProject
└─ Filter                          └─ Filter
   └─ NodeByLabelScan [EvPerson]      └─ NodeByIndexRangeScan
                                         [range="name002".."name003"(excl)]
```

**The premise holds.** The audit reported 283× / 275×; the current tree measures
295× / 265×. The allocation count of the prefix form (149 829 ≈ 3 × 50 000)
tracks the label population, which is the signature of a full scan with a
per-row property read.

## 2. The rewrite rule

For a `Selection` whose child is a `NodeByLabelScan` on label `L`, whose
predicate contains the comparison

```
n.prop STARTS WITH <string literal p>
```

and where a **bound string btree** index covers exactly `(L, prop)`, replace the
label-scan child with

```
NodeByIndexRangeScan over [p, succ(p))
```

`succ(p)` is the least string strictly greater than every string having `p` as a
prefix (§3). The original predicate is **always** retained as the residual
`Filter` above the scan — the caller in `buildOperator` stacks it
unconditionally — so the seek only ever narrows what the filter examines.

The rewrite is a **peephole at plan-build time**. It introduces no statistic, no
new gate, and no new operator.

### 2.1 The upper bound is passed as inclusive, and that is correct

`exec.RangeBound.Include` is **metadata only**.
`cypher/exec/scan_index_btree.go` (#F-EXEC1) documents and implements this:
`NodeByIndexRangeScan` emits the **inclusive `[lo, hi]` superset** the index
returns and cannot enforce an open bound, because it holds a bitmap of NodeIDs
and not the property values behind them. Exact open/closed semantics are the
residual `Filter`'s job.

So the implementation passes `hi = succ(p)` with `Include: false` for
documentation, and the executed scan walks the closed interval `[p, succ(p)]`.
That over-returns **at most the single key equal to `succ(p)`** (§3.2), which the
residual `Filter` removes. This is the same contract the shipped `>=` / `<`
range seek already relies on.

## 3. Constructing the exclusive upper bound

### 3.1 The construction: byte successor

`graph/index/btree` orders keys by `cmp.Compare` / `cmp.Less`
(`graph/index/btree/bplus.go:88-94`), i.e. for `V = string` the **byte-wise
lexicographic order** of Go string comparison. There is no collation and no
Unicode normalisation anywhere in the key path.

Let `p` be the prefix as a byte sequence of length `n`.

```
i    = max { j ∈ [0, n) : p[j] < 0xFF }        (the last byte below 0xFF)
succ = p[0:i] ++ byte(p[i] + 1)                 (length i+1)
```

If no such `i` exists — that is, `p` is empty, or every byte of `p` is `0xFF` —
then **no finite successor exists** and the upper bound is left **unbounded**
(§3.3).

### 3.2 Proof that `[p, succ]` is a superset, and how tight it is

**Superset.** Let `s` satisfy `strings.HasPrefix(s, p)`.

- `p ≤ s`: `p` is a prefix of `s`, and in byte-lexicographic order a prefix is
  never greater than the string it prefixes.
- `s < succ`: `|succ| = i+1` and `|s| ≥ |p| ≥ i+1`. For every `j < i`,
  `succ[j] = p[j] = s[j]`. At position `i`, `succ[i] = p[i] + 1 > p[i] = s[i]`.
  The first differing byte therefore makes `s < succ`. ∎

**Tightness.** The converse very nearly holds: every key `k` with
`p ≤ k < succ` *does* have `p` as a prefix. Bytes `0..i-1` of `k` must equal
`p`'s (any smaller violates `k ≥ p`, any larger violates `k < succ`); byte `i`
must equal `p[i]` for the same reason; and bytes `i+1..n-1` of `p` are all
`0xFF` by the choice of `i`, so `k ≥ p` forces those bytes of `k` to be `0xFF`
as well, hence equal to `p`'s.

Therefore

```
{ k : p ≤ k ≤ succ } = { k : HasPrefix(k, p) } ∪ ({succ} ∩ keys)
```

The closed interval the operator actually walks over-returns **at most one
distinct key value**. Consequences:

- the residual `Filter`'s work is essentially the true match set, not a loose
  superset;
- the selectivity gate's exact count over-counts by at most the cardinality of
  that one key, so the gate stays honest (and marginally conservative), exactly
  as the shipped inclusive-count path already assumes.

### 3.3 Edge cases

| Case | Construction | Behaviour |
|---|---|---|
| **Empty prefix** `''` | no `i` exists → `hi` unbounded | `StringRangeIndex.RangeBitmap` sees `hi == nil` and routes to the index's open-ended `RangeFrom(lo)` (#F-CY1); the gate correspondingly uses `RangeCountFrom`. The range spans every indexed key, so the selectivity gate **vetoes** and the plan stays a label scan. Correct either way: `s STARTS WITH ''` is TRUE for every string (TCK `String8` scenario [3]). |
| **All bytes `0xFF`** | no `i` exists → `hi` unbounded | Open-ended `RangeFrom(p)`. Exact, in fact: any `k ≥ p` must begin with `p` when every byte of `p` is the maximum byte. `0xFF` never occurs in valid UTF-8, so this branch is unreachable for well-formed text — but Go strings admit arbitrary bytes and the engine must not be wrong on them. |
| **Maximum code point** `…U+10FFFF` (bytes `F4 8F BF BF`) | last byte `BF < FF` → `succ = F4 8F BF C0` | A finite successor **does** exist. `succ` is not itself valid UTF-8, which is irrelevant: it is a comparison key, never stored, never returned, never decoded. |
| **Prefix longer than every stored value** | ordinary `succ` | Empty range → the gate's `count != 0` test declines → label scan. Correct, and avoids an index descent that would return nothing. |
| **Multi-byte / combining characters** | ordinary `succ` | Byte order coincides with code-point order in UTF-8, and `strings.HasPrefix` is itself a byte test, so no special handling is needed. `"e" + U+0301` and `U+00E9` are distinct keys under both the predicate and the index — openCypher performs no normalisation. |

### 3.4 Why the byte successor, not a code-point increment

Task #2126's brief proposed incrementing the **last code point**. The byte
successor strictly dominates it and is what the design adopts:

- **It always exists** for any non-empty prefix not consisting solely of `0xFF`.
  A code-point increment has no successor when the last code point is
  `U+10FFFF`, forcing the (much weaker) unbounded fallback for a case the byte
  construction handles exactly.
- **It is tighter.** For `p` ending in `U+00FF` (`C3 BF`), the byte successor is
  `C3 C0` while the code-point successor is `U+0100` = `C4 80`. Both are sound;
  the byte bound is narrower.
- **It needs no UTF-8 reasoning at all** — no decoding, no surrogate or
  plane-boundary special cases, no assumption that the stored value is valid
  UTF-8. Since the ordering basis is bytes and the predicate is bytes, a byte
  construction is the one that matches both without a translation step.

This is the construction used by the prefix-scan bounds of LSM engines whose
key order is likewise byte-lexicographic (RocksDB / LevelDB), for the same
reason.

## 4. openCypher `STARTS WITH` semantics

Established from primary sources — the TCK feature files in
`cypher/tck/features/expressions/string/` and the shipped evaluator
`cypher/expr/eval.go:1523` (`evalStringOp`) — not from memory.

| Input | Result | Source | Effect on the rewrite |
|---|---|---|---|
| both operands strings | `strings.HasPrefix(s, p)` — **byte prefix, case-sensitive, no normalisation** | `eval.go:1537` | The index order (bytes) and the predicate (bytes) share one basis; the match set is a contiguous interval. This is the crux of §3.2. |
| left or right `NULL` | `NULL` (row dropped by `WHERE`) | `String8` [6], [7]; `Precedence4` [4] | Null / missing properties are never indexed (`projectStringPropValue`), so index and predicate exclude them identically. |
| either operand non-string | `NULL` | `String8` [8] — all 36 combinations of `[1, 3.14, true, [], {}, null]` yield `null` | A non-string *property* is excluded from the string btree by `projectStringPropValue`, and excluded from the result by the predicate. Symmetric. A non-string *prefix operand* is declined by the extractor (only `*ast.StringLiteral` is accepted), so the scan+filter path yields the correct empty result. |
| empty prefix `''` | TRUE for every string, including `''` | `String8` [3] | §3.3. |
| prefix is whitespace / newline | ordinary byte prefix | `String8` [4], [5] | No special casing; bytes are bytes. |

**Temporal-tagged strings.** A Cypher temporal is stored as a tagged string
(`\x01`…`\x06` prefix) and `projectStringPropValue` excludes it from the btree.
This is symmetric and therefore safe: read back through Cypher such a property
is a temporal value, not a `StringValue`, so `evalStringOp` returns `NULL` and
the predicate excludes it too. Index and predicate agree.

## 5. Scope boundary — what must NOT be rewritten

### 5.1 Negation and disjunction

`NOT (n.prop STARTS WITH p)` selects the **complement** of the prefix set.
Rewriting it to the prefix range would be a non-superset and would return
catastrophically wrong answers. TCK `String8` scenario [9] pins the exact
expected complement, including that `name: ''` **is** returned while a node with
no `name` is **not** (`NOT NULL` = `NULL`).

The existing extractor structure already excludes this, and the implementation
must not weaken it:

- `extractStringRangePred` accepts only an `*ast.BinaryOp`; `NOT` is an
  `*ast.UnaryOp` (`cypher/ast/expressions.go:119`), so `NOT (…)` at the top of
  the predicate is declined outright.
- The only descent is through a top-level `AND` into two direct comparisons;
  `NOT x AND y` presents an `*ast.UnaryOp` to `extractSingleStringCmp`, which
  declines, and the whole predicate is declined with it.
- `OR` reaches `extractSingleStringCmp` as a `*ast.BinaryOp` whose operator is
  not in the accepted set, and is declined.

The implementation adds `"STARTS WITH"` to the **accepted-operator set of
`extractSingleStringCmp` only**. It must not touch the descent structure. This
is the single most dangerous failure mode of the change and #2128 must carry an
explicit regression test for each of `NOT`, `OR`, and `NOT … AND …`.

### 5.2 Conjunction with another bound stays sound

A prefix yields **both** a lower and an upper bound, like the degenerate `=`
range (#2231). `mergeRangeBounds` keeps one bound from each side of an `AND`,
which is sound in general: every bound it retains is a necessary condition of
its own conjunct and therefore of the conjunction, so the merged interval is
always a superset of the true match set. Examples:

- `p.name STARTS WITH 'ab' AND p.name >= 'aba'` → `['aba', 'ac']` ⊇ the true set.
- `p.name STARTS WITH 'ab' AND p.name >= 'z'` → an empty interval; the true set
  is empty too.

No change is needed here; the property is stated so the test plan can pin it.

### 5.3 `ENDS WITH` and `CONTAINS` admit no range rewrite

**Claim.** For any alphabet with at least two symbols and any non-empty pattern
`x`, the set of strings ending with (respectively containing) `x` is not an
interval of the byte-lexicographic order, and the smallest interval containing
it is unbounded in general.

**Proof.** Take `x = "x"` and any two symbols `a < b`. The keys `"ax"`, `"b"`,
`"bx"` sort as `"ax" < "b" < "bx"`. `"ax"` and `"bx"` end with `x`; `"b"` does
not. Any interval containing both `"ax"` and `"bx"` must contain `"b"`. The
construction generalises: for any candidate interval bounds one can insert
non-matching keys between two matching keys, and by varying the first symbol the
matching keys can be made to straddle arbitrarily much of the key space. Hence
the tightest sound interval degenerates towards `[min, max]`. ∎

A range seek needs only a *superset*, so a rewrite is not *unsound* — it is
*useless*: the only sound interval is (in general) the whole index, which
carries no selectivity, is strictly more expensive than the label scan it would
replace, and would be vetoed by the selectivity gate anyway. `CONTAINS` is the
same argument with `x` anywhere in the key.

Serving these predicates from an index requires a different structure
altogether — a suffix array, an n-gram/trigram index, or Neo4j's dedicated
`TEXT` index. That is out of scope for this sprint and is not proposed here.

## 6. Ordering consistency with the shipped index path

- **One order.** The btree's total order is `cmp.Compare` on Go strings
  (`bplus.go:88`); `strings.HasPrefix` compares the same bytes. The prefix set is
  therefore contiguous **in the index's own order**, which is what makes a single
  seek sufficient. No collation question arises — the same reasoning that let
  #2231 serve string equality as the degenerate range `[v, v]`, and the reason
  round-4 finding C3 (collation for inequality) does not bear on this change.
- **Unbounded-above is open-ended, not a sentinel** (#F-CY1). Where §3.3 leaves
  `hi` unbounded, `StringRangeIndex.RangeBitmap` routes to `RangeFrom` and the
  gate to `RangeCountFrom`, so the counted key space and the walked key space are
  the same. A fixed maximum sentinel would silently drop keys sorting above it;
  that defect is already fixed and the prefix path inherits the fix by using the
  same `hi == nil` signal.
- **Bound indexes only.** `findBoundStringBTree` returns an index only when
  `BoundNode()` reports it is bound to exactly `(label, prop)`; an unbound btree
  is never selected because it is not maintained from the change fan-out. The
  prefix path reuses this lookup verbatim.

## 7. The gate is reused unchanged

No change to `rangeCountWinsFn`, `rangeSeekMaxSelectivity` (0.10), or
`rangeSeekMinLabelPopulation` (1024). The prefix path calls the same gate with
the same closure shape:

- `N_label ≥ 1024`, else scan;
- `budget = 0.10 × N_label`; the btree returns an **exact** in-range count with
  early exit at `budget`;
- fire only when the count is exact, non-zero, and within budget.

Because the count is exact rather than an estimate, the estimate-provenance veto
is satisfied trivially — the same argument #1505 established. The change
therefore lands inside the proven no-regression frame, and cannot regress a
non-selective prefix: `STARTS WITH ''` and any broad prefix keep today's plan.

## 8. Implementation shape (for #2127)

| Concern | Decision |
|---|---|
| Recognition | Add `"STARTS WITH"` to the operator set of `extractSingleStringCmp` in `cypher/range_seek_plan.go`, mapping to `lo = {p, Include:true}`, `hi = {succ(p), Include:false}` (or `hi = nil` when no successor exists). Mirroring is **not** applicable: `STARTS WITH` is not symmetric, so only `n.prop STARTS WITH lit` is accepted — `lit STARTS WITH n.prop` must be declined. |
| Successor helper | A small pure function `prefixSuccessor(p string) (string, bool)` in the same file, `ok == false` when no finite successor exists. Unit-testable in isolation. |
| Operand | `*ast.StringLiteral` only, matching the existing string path (parameters remain out of scope for the string extractor, as for `>=` / `<`). |
| Gate | Unchanged (§7). |
| Flag | New `EngineOptions.DisablePrefixIndexSeek`, defaulting to enabled, threaded as `buildOpts.prefixSeekEnabled` via `planGates.prefixSeek`, exactly mirroring `DisableRangeIndexSeek` / `rangeSeekEnabled` / `planGates.rangeSeek`. A **separate** flag, not a reuse of `DisableRangeIndexSeek`: the differential harness must toggle only the prefix rewrite, leaving the `>=` / `<` seek active in both arms, so the two arms differ in exactly one variable. |
| Residual filter | Unchanged — always retained by `buildOperator`. |
| Write path | Inherits `planGates` the same way `rangeSeek` does (#2225), so a statement carrying a write clause is planned with the rewrite too. |

## 9. Test plan (for #2128) and the TCK's role

**TCK inventory.** `STARTS WITH` appears in three feature files:

| File | Scenarios |
|---|---|
| `expressions/string/String8.feature` | [1] non-proper prefix · [2] beginning of string · [3] empty prefix · [4] leading whitespace · [5] leading newline · [6] no string starts with null · [7] no string does not start with null · [8] non-string operands (36 combinations) · [9] `NOT` with `STARTS WITH` |
| `expressions/string/String11.feature` | [1] prefix + suffix · [2] prefix + suffix + substring |
| `expressions/precedence/Precedence4.feature` | [4] string predicate vs binary boolean operator (plus the null-predicate outlines) |

**The TCK is necessary but not sufficient evidence here.** Every one of these
scenarios matches on an unlabelled `MATCH (a)` over a graph with no indexes, so
none of them plans a `NodeByLabelScan` and none can engage the peephole. TCK
green-ness proves the change did not perturb `STARTS WITH` evaluation; it proves
nothing about the rewrite itself. The **differential harness is the real proof**,
and it must therefore assert that the two arms actually take different plans —
a differential whose arms silently share a plan is degenerate and green for the
wrong reason.

Required coverage:

- **Differential** (rewrite on vs off, byte-identical result multisets, plans
  asserted to differ): empty prefix; prefix longer than every stored value;
  prefix at the maximum code point; all-`0xFF` prefix; multi-byte and combining
  characters; `NULL` and non-string property values; a property with no index; a
  prefix matching zero rows; a prefix matching every row (gate must veto);
  parameterised prefixes (must decline).
- **Property-based** (`pgregory.net/rapid`): for random key sets and random
  prefixes, the rewritten interval selects exactly
  `{k : strings.HasPrefix(k, p)}` ∪ at most `{succ(p)}`, and the end-to-end
  result equals the `strings.HasPrefix` filter.
- **`prefixSuccessor` unit tests**: the table in §3.3, plus the invariants
  `p < succ(p)` and `∀s. HasPrefix(s,p) ⇒ s < succ(p)`.
- **Negative / scope**: `ENDS WITH` and `CONTAINS` still plan a label scan;
  `NOT … STARTS WITH …`, `OR`, and `NOT … AND …` still plan a label scan;
  `lit STARTS WITH n.prop` still plans a label scan.
- **Conformance**: TCK stays 3897/3897; `go test -race` clean; `goleak` clean;
  short-layer package budget under 60 s.

## 10. Risks

| Risk | Mitigation |
|---|---|
| A negated or disjunctive predicate reaches the rewrite | The extractor never descends past a top-level `AND` into anything but a direct comparison (§5.1). Pinned by explicit regression tests. |
| The successor bound misses a match | Proved in §3.2; pinned by unit tests on `prefixSuccessor` and by the rapid property. The residual `Filter` cannot rescue a *miss*, so this proof is the load-bearing one and is deliberately given in full. |
| A non-selective prefix regresses | The exact-count gate declines it (§7); covered by the "prefix matching every row" differential case. |
| The differential is degenerate | Assert the arms take different plans, not merely equal results. |
| Documentation drifts from behaviour | #2130 verifies every statement against the shipped code, with real benchstat figures from #2129. |
