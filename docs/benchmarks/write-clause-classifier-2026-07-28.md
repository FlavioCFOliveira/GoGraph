# The write-clause classifier stops reading comments and strings — 2026-07-28 (rmp #2230)

- Apple M4 (10 cores, 32 GB), Go 1.26.5. `BenchmarkQueryHasWritingClause` in
  `cypher/clause_mask_test.go`, `benchstat` over `-count=6`.

## 1. The defect

`writingKeywordRE` was applied to the **raw** query text, so it matched a keyword wherever it
appeared — including where a clause cannot appear at all:

```
MATCH (n:Person) WHERE n.name = 'CREATE' RETURN count(n)   -> classified as a WRITE
// CREATE nothing here
CALL db.labels() YIELD label                               -> classified as a WRITE
```

The severity is not cosmetic. A misrouted read runs inside a **write** transaction and therefore
serialises on the store's single writer, so one such query throttles exactly the concurrent read
throughput the engine is built for — silently, with nothing surfaced. It was found because adding a
comment to a working `CALL db.labels()` query broke it outright: the misrouted read reached the
write path, where the procedure registry was unreachable (#2229, since fixed).

The docstring already called itself a textual heuristic, and that was never the problem. The
problem was that it inspected regions which cannot hold a clause, which is cheap to exclude.

## 2. The fix

`maskNonClauseRegions` blanks the four regions before the match, in one left-to-right pass with no
regexp. The forms are taken from the lexer grammar rather than from memory
(`cypher/parser/grammar/CypherLexer.g4`):

| token | form | subtlety |
|---|---|---|
| `LINE_COMMENT` | `//` to end of line | — |
| `COMMENT` | `/* … */` | **non-greedy**: ends at the *first* closing pair |
| `CHAR_LITERAL` | `'…'` | backslash `EscapeSequence`, so `'it\'s'` is one string |
| `STRING_LITERAL` | `"…"` | same |
| `ESC_LITERAL` | `` `…` `` | non-greedy, **no** escape sequence — a backslash is an ordinary byte |

Masked bytes become **spaces**, not deletions, so word boundaries either side survive and offsets
into the query are unchanged (pinned by a test on length and newline count).

An unterminated comment or string masks to the end of input. That is the conservative direction
here: the tail is a lexical error the parser rejects anyway, and masking it can only make the
classifier say "read", which fails cleanly on the read path — whereas leaving it unmasked could
route an unparseable statement to the write path and take the single-writer lock to do it.

## 3. Cost (AC 5)

| case | before | after | |
|---|--:|--:|---|
| no comment or string (read) | 2.773 µs | 2.791 µs | +0.67% (p=0.045) |
| no comment or string (write) | 1.363 µs | 1.388 µs | +1.83% (p=0.024) |
| comment and string | 1.654 µs | 2.117 µs | +28.03% (p=0.002) |

| case | allocs/op before | after |
|---|--:|--:|
| no comment or string (read) | 1 | **1** (all samples equal) |
| no comment or string (write) | 1 | **1** (all samples equal) |
| comment and string | 1 | 2 |

The requirement was no material regression on queries containing no comment or string, and there is
none: **allocations are exactly unchanged** and the time cost is under 2%. That cost is understood
rather than mysterious — it is the single `strings.ContainsAny` guard, roughly 20 ns against a
2.8 µs regexp match, which is the 0.67% observed.

The masking path pays +28% (~460 ns) and one copy. A copy is unavoidable once there is something to
mask, and it is only paid by queries that actually contain a comment or a quoted string.

## 4. A finding the measurement produced

The absolute figures are worth more attention than the delta: **classification costs 1.4–2.8 µs on
every `RunAny` / `RunInTxAny` dispatch**, and an indexed point lookup costs about 5 µs end to end —
so classifying a fast query can be a third of its total cost. The expense is the case-insensitive
regexp, not the new mask.

The mask is also the natural place to remove it: one walk could blank the non-clause regions *and*
test the remaining words against the six keywords, fusing two passes into one and deleting the
regexp. Filed as **#2240**.

## 5. Coverage

27 table-driven cases, including every maskable region, four escape-sequence cases (an escaped
delimiter must not end its region early), three unterminated-region cases, genuine writes preceded
by comments that mention no keyword, a mixed read-write statement, and a write whose *string*
mentions a different keyword. Plus a dispatch test asserting a commented `CALL db.labels()` returns
exactly what the uncommented spelling returns through `RunAny`.

## 6. Reproduction

```bash
go test ./cypher/ -run 'TestQueryHasWritingClause|TestMaskNonClauseRegions|TestRunAnyDispatch'
go test ./cypher/ -run '^$' -bench BenchmarkQueryHasWritingClause -benchmem -count=6
```
