# Write-clause classification: regexp → word scan (rmp #2240)

**Date:** 2026-07-28 · **Sprint:** 327 · **Host:** Apple M4 (10 cores, 32 GB), Go toolchain as
pinned by the module.

## 1. What was costly

`QueryHasWritingClause` runs on every `RunAny` / `RunInTxAny` dispatch and decides whether a
statement takes the read path or the single-writer write path. It cost **2.70 µs** for a
70-character read and **1.33 µs** for a short write, plus one heap allocation each. An
indexed point lookup costs roughly 5 µs end to end, so classifying a query could cost a
third of running it.

Two separate causes, only the first of which the finding named:

1. `writingKeywordRE` — `(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`. The regexp engine,
   not the comment/string mask added by #2230: the mask's fast path already allocated
   nothing.
2. `ir.IsDDL`, called immediately before it, uppercased the **whole query** with
   `strings.ToUpper` in order to compare prefixes of at most seventeen bytes — one heap
   allocation per query executed. This was not in the finding; it was found while proving
   acceptance criterion 2, which could not otherwise be met.

## 2. What replaced them

**`containsWritingKeyword`** (`cypher/clause_mask.go`) walks the masked text once, isolates
each maximal run of ASCII word bytes `[0-9A-Za-z_]` — the class Go's regexp `\b` anchors on —
and tests it against the six keywords with a length switch followed by an ASCII-folding
comparison. Nothing is allocated.

The scan is composed **over** `maskNonClauseRegions` rather than fused into it. The finding
suggested fusing the two walks; keeping them separate preserves a single definition of where
the unmaskable regions are, so the classifier and the mask cannot drift apart — and on the
common path the mask returns the query unchanged, so both walks are over the original string
and neither allocates. The fusion would have bought one walk over a string that is already
in cache.

**`ir.hasPrefixFold`** compares a prefix case-insensitively without allocating, deferring to
the exact `strings.HasPrefix(strings.ToUpper(s), upper)` expression it replaced the moment a
non-ASCII byte appears in the prefix window. That fallback is not defensive padding: Unicode
uppercasing maps 'ı' (U+0131) onto ASCII 'I' and 'ſ' (U+017F) onto 'S', and maps 'ß' onto the
**two** bytes "SS", which changes the string's length and shifts every following byte out of
alignment. A byte-wise fold alone would have diverged on all three.

### Case folding was verified, not assumed

The regexp's `(?i)` was checked against `ſet` (U+017F) before the change and **rejected** it,
so the ASCII fold in `containsWritingKeyword` is behaviour-identical rather than a narrowing.
`hasPrefixFold` needs its Unicode fallback because it compares against *prefixes*, where
`ToUpper`'s length changes matter; the keyword scan compares whole words already delimited by
the ASCII word class, where they cannot.

## 3. Measurement

`go test ./cypher/ -bench=BenchmarkQueryHasWritingClause -benchmem -count=6`, compared with
`benchstat`.

| Case | sec/op | B/op | allocs/op |
|---|---|---|---|
| `no_comment_or_string` | 2698.0 n → **123.7 n · −95.42 %** (p=0.002) | 80 → **0 · −100 %** | 1 → **0** |
| `write_no_comment_or_string` | 1326.0 n → **73.51 n · −94.46 %** (p=0.002) | 64 → **0 · −100 %** | 1 → **0** |
| `comment_and_string` | 2037.5 n → **253.5 n · −87.56 %** (p=0.002) | 144 → 80 · −44.44 % | 2 → 1 |
| geomean | 1.939 µ → **132.1 n · −93.19 %** | | |

**21.8× on the dominant path**, and zero allocations wherever the query carries no comment or
quote. The masking path keeps one allocation — the masked copy itself, which is inherent to
masking and unchanged by this work.

## 4. Correctness evidence

Both replacements are held to the implementations they replaced, by differential test rather
than by inspection:

- `TestContainsWritingKeyword_AgreesWithTheRegexpItReplaced` keeps the original regexp alive
  **in the test binary** as the oracle and agrees with it on **462 inputs**, covering case
  variants, word-boundary traps (`PRESET`, `NOMERGE`, `OFFSET`, `SET_`, `1SET`, `n.createdAt`),
  separators, non-ASCII neighbours (`ſet`, `café SET x`, `日本語 CREATE`), and every comment and
  quote form including unterminated ones.
- `TestHasPrefixFold_AgreesWithToUpper` agrees with `strings.HasPrefix(strings.ToUpper(s), p)`
  on **1692 (input, prefix) pairs**, built specifically to attack the byte-wise fold with
  'ı', 'ſ', 'İ' and 'ß'.
- The pre-existing `TestQueryHasWritingClause_IgnoresCommentsAndStrings` passes **unchanged**,
  all 26 cases including the escape-sequence and unterminated-region rows (acceptance
  criterion 3).
- `TestQueryHasWritingClause_FastPathDoesNotAllocate` and `TestIsDDL_DoesNotAllocate` pin the
  zero-allocation property so it cannot silently regress.

## 5. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate (aggregate 86.9 % ≥ 85.0 %).
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined** (baseline 3897).
