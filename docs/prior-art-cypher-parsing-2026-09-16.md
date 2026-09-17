# Prior art on fast statement parsing — Neo4j, Memgraph, PostgreSQL

**Task:** rmp #2845, sprint 362, Cypher statement-parsing campaign.
**Read at:** GoGraph `3ab42f2b`. **Date:** 2026-09-16.

This document records what three production engines do to make statement parsing
fast, and which of their techniques apply to the costs GoGraph actually measured
in round 1 (rmp #2844). It proposes nothing and ranks nothing for
implementation; the decision ranking is rmp #2846.

Every claim below was read in source at a pinned commit. Nothing is quoted from
documentation alone, and no reference's own benchmark number is offered as
evidence for GoGraph.

## Sources

| Project | Commit | Date | Scope read |
|---|---|---|---|
| Neo4j | `bbf3b91f46a761809c19dfca0c3a09a6a8ea09e8` | 2026-09-07 | sparse checkout of `community/cypher` |
| Memgraph | `f8ab137e88da3ff0e3dcdd7389fd61e33cb1c631` | 2026-09-15 | full tree |
| PostgreSQL | `a10d14afca3601f519e8ba219627c8c0e9bf72c7` (`REL_17_STABLE`) | 2026-09-16 | full tree |

The ANTLR Go runtime read is `github.com/antlr4-go/antlr/v4@v4.13.1`, the
version pinned in `go.mod`.

## The measured costs this document answers to

From rmp #2844, over an 83-statement corpus taken from the project's own
examples, no `-race`, no build tags:

* **69.2%** of the front end's CPU belongs to the ANTLR runtime and the
  allocation it drives; 16.4% to the generated parser; **14.3%** to GoGraph's
  hand-written code.
* `strip.go:155` runs `strings.ToUpper` on **every identifier token**, which is
  **78.6%** of all objects `StripLiterals` allocates — paid on **every
  execution, plan-cache hits included**.
* Front-end share of a whole query: 0.00–0.67% on a hit, 2.28–82.02% on a miss.
* The front end provokes **1.07x its own CPU again** in background GC marking.

## 1. Front-end shape, side by side

| | Neo4j | Memgraph | PostgreSQL |
|---|---|---|---|
| Generator | ANTLR4 (Java), two grammars | ANTLR4 (C++) | flex + bison, hand-tuned |
| Prediction | **SLL + Bail, retry LL + full errors** | ANTLR default (LL) | LALR(1) |
| Token objects | **`ThinCypherToken` is both `Token` and `TerminalNode`** | stock ANTLR C++ tokens | flex `yytext` slice |
| AST construction | **fused into the parse** | separate visitor pass | bison actions, inline |
| CST lifetime | **freed as it goes** | retained through the visit | no CST |
| Keyword lookup | in the lexer ATN | 256-way trie | **perfect hash + exact verify** |
| Auto-parameterisation | AST rewrite, post-parse | **text strip, pre-parse** | none |
| Fast-path dispatch | **pre-parser, first-token bail** | prefix handling in the stripper | none |

## 2. Neo4j

**Two-stage parsing.** `AntlrAstParser.scala` sets `PredictionMode.SLL` plus a
`BailErrorStrategy`, parses, and on any `NonFatal` throwable rebuilds the token
stream with `fullTokens = true`, switches to `PredictionMode.LL` and the full
error strategy, and parses again. **Every error message a user sees comes from
the second stage.** The first stage is allowed to be wrong, but not wrongly
accepting.

**Thin tokens.** `CypherToken.scala` documents `ThinCypherToken` as *"A slimmer
implementation of Token. Implements both CypherToken and TerminalNode as a
memory optimisation."* It deliberately does not support `getTokenIndex`,
`getParent`, `getChild`, `setParent`. `AstBuildingAntlrParser.createTerminalNode`
returns the token itself when it already is a `TerminalNode`, so the tree
allocates **no wrapper node per token**.

**AST fused with the parse; CST freed during it.** `AstBuildingAntlrParser.scala`
is documented *"Fails fast. Optimised for memory by removing the parse as we
go."* Its `exitRule()` runs the syntax checker and the AST listener on the
context that has just closed, then `localCtx.children = null` when safe. There
is no second tree walk and the complete CST never exists at one time.

**Pre-parser with a first-token bail.** `CypherPreparser.g4` carries the
normative comment *"Preparsing is skipped if the first token is not CYPHER,
EXPLAIN or PROFILE."* The parser is instantiated only when `LA(1)` is one of
those three. It decides four things: execution mode, language version, the
`key=value` settings, and the offset at which the real statement starts.

**It is not free**, and the report says so: the pre-lexer copies the whole query
while rewriting `\uXXXX` escapes and building an offset table, so the pre-parse
is O(n) in a copy plus one lexed token. What it skips is the ANTLR *parser*.

**Five cache layers**, `CypherQueryCaches.scala`: pre-parser cache (**raw
text**), AST cache (**raw statement text + parameter types**), logical-plan
cache (**rewritten AST**), execution-plan cache, executable-query cache.

**Auto-parameterisation is an AST rewrite, not a text strip**
(`literalReplacement.scala`). Extraction is **skipped** under `Limit`, `Skip`,
`GraphPatternQuantifier`, `CountedSelector` and `ContainerIndex(_, StringLiteral)`
— independent corroboration of GoGraph's own reasons for excluding `SKIP`/`LIMIT`
and variable-length bounds. It is **not** skipped under `Return`/`With`, which
Neo4j can afford because column names come from the AST's position information
rather than from surviving text.

**The consequence is decisive:** the AST cache key is raw text, so Neo4j
**re-parses for every distinct literal spelling** and converges them only one
layer later, at the logical plan.

**Negative evidence.** The ANTLR front end is Neo4j's *third* generation. Commits
`dda86a4a` (2024-05-30) and `835ade43` (2024-06-11) record the removal of the
JavaCC parser. **The direction of travel is towards ANTLR, away from a generated
recursive-descent parser.**

## 3. Memgraph — the closest prior art to `strip.go`

**The stripper is hand-written and deliberately does not reuse the ANTLR lexer**
(`src/query/frontend/stripped.cpp`). It is a maximal-munch scanner running
**eleven** matchers at each position and keeping the longest match. That
longest-match rule is how the keyword/identifier boundary is resolved with no
explicit boundary test: `matches` matches the keyword rule at length 5 and the
name rule at length 7, so the name wins. An unmatched byte throws.

**What it strips:** integers to `"0"`, doubles to `"0.0"`, strings to `"\"a\""`,
booleans to `"true"`. `NULL` is deliberately **not** stripped, *"since it can
appear in special expressions like IS NULL and IS NOT NULL"*. The placeholder is
a **canonical literal**, not a parameter reference — the stripped text is
re-lexed as ordinary Cypher and the values travel beside it keyed by **token
position**.

**Keyword matching** is a 256-way trie walked with per-byte folding, allocating
nothing. Computed from the checked-in list: **2,959 keywords, 12,485 trie
nodes**, at roughly 1 KiB per node — about **12 MiB resident, always**. A
branch-free byte step paid for in cache footprint. Its fold uses C `tolower`,
which is locale-dependent — the exact hazard PostgreSQL explicitly refuses.

**The cache key is the stripped text, and the strip runs on every query**, before
the cache is consulted. The cache is *"keyed by text, not hash, so a hash
collision can't return another query's AST"*, with a precomputed FNV-1a as a fast
reject and the full text as the decision.

**Memgraph's cache hit is far more expensive than GoGraph's:** on a hit it copies
five interner maps and **deep-clones the whole AST**, because the visitor
resolves parameters into the AST. GoGraph caches an immutable plan and hands out
one pointer.

**How it strips inside `RETURN` without breaking column names.** The stripper
records the **original, unstripped source text** of every unaliased projection
item, keyed by token position, parsing the `cypherReturn` production by hand with
bracket-depth counting; the result header then looks the name up by position. So
`RETURN 'x'` still reports a column called `'x'`. **This is the exact problem
GoGraph's `strip.go` solves by refusing to hoist in `RETURN`/`WITH`.**

## 4. PostgreSQL — the keyword-lookup answer

**`ScanKeywordLookup`** (`src/common/kwlookup.c`) is three steps and zero
allocations:

1. reject immediately if longer than the longest keyword — *"This saves useless
   hashing and downcasing work on long strings."*
2. a **minimal perfect hash**, generated at build time;
3. verify byte-by-byte, downcasing the input **inline** during the comparison.

The case-folding rationale is normative and explicit: *"we deliberately use a
dumbed-down case conversion that will only translate 'A'-'Z' into 'a'-'z', even
if we are in a locale where tolower() would produce more or different
translations"*, and *"We must not use tolower() since it may produce the wrong
translation in some locales (eg, Turkish)."*

The hash is Czech–Havas–Majewski (1992), emitted as two multiply-and-add passes;
case-insensitivity is `c | 0x20` **in the hash only**, and the generator states
the limit: *"this only works for a strict-ASCII interpretation of case
insensitivity."* All keywords live in one `\0`-separated blob addressed by
`uint16` offsets — **491** entries in PG 17.

**The call site matters as much as the structure.** In `scan.l`, the keyword
lookup runs **first**; only on a miss does the allocating downcase run. The
allocation never happens as part of the keyword test.

**The no-backtracking gate** is enforced at build time: the build fails unless
flex reports no backtracking. PostgreSQL made a performance property of the
scanner a **build-failing invariant rather than a comment**.

**Caching.** `CachedPlanSource` retains the **raw parse tree**, so a replan never
re-runs `raw_parser`. Prepared statements are keyed by **statement name**, not by
query text. Simple-protocol queries re-parse **every time**: PostgreSQL has **no
query-text plan cache and no auto-parameterisation at all**. Its answer to "stop
re-parsing" is "use the extended protocol".

**Negative evidence, both directions.** An array of string pointers for keywords
was *abandoned* for being cache-unfriendly (*"the binary search touched strings
in many different pages"*), replaced by the contiguous blob; binary search itself
was then *abandoned* for the perfect hash, with a measured *"~20%"* drop in raw
parsing time. **That 20% is PostgreSQL's, in C, over 491 keywords. It is not
evidence for GoGraph.**

## 5. The four questions the ranking task asked

### Q1 — Is the 69.2% ANTLR share escapable, and at what cost?

**Partly. The escapes available in Go are a strict subset of Neo4j's.**

* **SLL-first prediction — available, and currently unused.** The Go runtime
  exports `SetPredictionMode` and `NewBailErrorStrategy`, and the generated
  parser exposes `Interpreter`. `NewParserATNSimulator` defaults to
  `PredictionModeLL`, and `cypher/parser/parse.go` never touches it, so
  **GoGraph runs full LL today**. This is the largest structural lever the Go
  runtime does expose. *Verified independently by the coordinator: `go doc`
  confirms `SetPredictionMode` is exported, and two routes confirm zero
  occurrences in `parse.go`.*
* **Thin tokens — NOT available in Go.** `setTokenFactory` is **unexported as a
  method of the `TokenSource` interface itself**, so no type outside package
  `antlr` can implement `TokenSource`. GoGraph can neither install a custom
  `TokenFactory` nor substitute a token source. Every token is a heap-allocated
  `CommonToken`. *Verified verbatim by the coordinator in the module cache.*
* **Fusing the AST build into the parse — half available.** The listener half
  transfers; the CST-freeing half does not, because `ParserRuleContext.children`
  is unexported in the Go runtime. It would mean rewriting all 3,428 lines of
  `visitor.go`.
* **Replacing ANTLR — no supporting prior art exists.** Every Cypher front end
  verified is generated: Neo4j and Memgraph and Kùzu use ANTLR4, Apache AGE uses
  flex+bison, libcypher-parser uses a PEG. **No hand-written recursive-descent
  Cypher parser was found anywhere**, and Neo4j *removed* its recursive-descent
  parser in favour of ANTLR.

### Q2 — Comparing an identifier against a keyword set without allocating

**PostgreSQL's structure is the right model, and for GoGraph it is unusually
safe.**

Go's `strings.ToUpper` returns its input unchanged when the string is ASCII with
no lowercase, and otherwise allocates exactly one `[]byte`. That is precisely the
measured model — the runtime source and the measurement agree mechanistically.

**What makes the ASCII fold exactly equivalent here, not merely nearly so:**
`isIdentStart`/`isIdentChar` accept only `[A-Za-z_]` and `[A-Za-z0-9_]` **bytes**,
so a non-ASCII byte terminates the identifier scan before `ToUpper` is reached.
The Unicode branch is **unreachable from this call site**. The Turkish-locale and
long-s hazards that make ASCII folding a *correctness* argument in PostgreSQL do
not arise; here it is purely a performance argument with **no semantic delta**.
*Verified verbatim by the coordinator.*

**The machinery does not transfer, only the structure.** GoGraph's keyword set is
**24 entries**; a perfect-hash generator and a 12 MiB trie are both
disproportionate. What transfers is: reject on length, fold during the
comparison, never materialise a folded copy, and compare the **whole** token — a
prefix-matching structure without longest-match discipline would silently
reclassify `matches`, `setting`, `ordering` and `forall` as keywords, which is a
semantic change disguised as an optimisation.

### Q3 — What do the references key their caches on, and does any skip stripping on a hit?

| Engine | First cache | Key | Strip on a hit? |
|---|---|---|---|
| Neo4j | pre-parser cache | raw text | no strip exists |
| Neo4j | AST cache | raw statement text + parameter types | n/a — literals extracted after parsing |
| Memgraph | AST cache | **stripped text** | **yes, plus a full AST deep clone** |
| PostgreSQL | prepared statements | **statement name** | no strip exists; simple queries re-parse |
| GoGraph | plan cache | **stripped text** | **yes** |

**It is not inherent to auto-parameterisation; it is inherent to keying the cache
on the stripped text.** Neo4j auto-parameterises and never strips on a hit — but
pays by re-parsing every distinct literal spelling, which is exactly the cost
`strip.go` exists to avoid.

**GoGraph is already the best placed of the three on this axis:** the strip is a
single `IndexByte` fast reject for any quote-free query, and a hit returns one
immutable pointer where Memgraph deep-clones an AST.

The escape the prior art suggests is a **raw-text index in front of the
stripped-text-keyed cache** — structurally what Neo4j's pre-parser to AST cache
layering is. `cypher/api.go` already records a deliberate rejection of the
two-probe variant on a **metrics-fidelity** ground. The prior art does not
overturn that reasoning; it shows the layered shape is conventional and that the
objection is answerable with a separate counter per layer, which is what Neo4j
does.

### Q4 — Is a pre-parser or fast-path dispatch worth it?

Neo4j's decides four things off `LA(1)` and a three-way integer comparison, and
its result is itself cached. libcypher-parser independently reaches the same
design with a second, cheaper grammar.

**For GoGraph the applicability is limited, and this is stated plainly.** GoGraph
has no pre-parser options to strip, and `EXPLAIN`/`PROFILE` are handled by the
main grammar **deliberately** — that is what lets `RETURN explain` parse as an
ordinary statement where a textual leading-keyword scan would misfire. GoGraph
has already applied the principle where it pays most: DDL never reaches the
parser at all.

**The transferable insight is not "add a pre-parser".** It is the shape: *a
cheap, bounded, first-token test that decides whether an expensive stage runs at
all*, with the assumption written down where a future editor will trip over it.

## 6. Candidate techniques, ordered by applicability

Ordered by how directly each bears on the measured costs. **This is an analytical
ordering, not an implementation ranking.**

| | Technique | Targets | Premise holds here? |
|---|---|---|---|
| **C1** | SLL-first prediction with LL fallback | the 53.1% of parse-stage self time that is ANTLR runtime | **yes** — both APIs exported |
| **C2** | Allocation-free keyword test at `strip.go:155` | 78.6% of `StripLiterals` objects, on every cache hit | **yes, exactly** — the ASCII premise is stronger here than in PostgreSQL |
| **C3** | Raw-text index in front of the plan cache | the strip scan paid on every hit | **weak** — an embedded library, not a server with five caches |
| **C4** | Column names from recorded source text, to allow hoisting in `RETURN` | coverage, not CPU — only 5 of 83 statements hoist | **no** — auto-params are name-keyed, not position-keyed |
| **C5** | Fusing the AST build into the parse | the 3,158 ns Visit stage in full | **half** — the memory half is closed in Go |
| **C6** | A no-backtracking scanner as a build-failing invariant | the Lex stage | **no** — the technique is flex-specific; only the discipline transfers |

**C1 caveats that must survive into the ranking.** SLL is not equivalent to LL —
it can reject a valid input, so correctness depends entirely on a faithful retry,
and *every* error path must route through the LL stage. GoGraph's error surface
is elaborate (the `ERRCHAR` sweep, the regex-combiner check, the panic guard for
a known antlr4-go v4.13.1 bug, the expected-token-set enrichment), and a
two-stage scheme must reproduce every diagnosis byte for byte. There is also a
second-order unknown: SLL and LL share the process-wide `decisionToDFA`, and the
mixed-mode population behaviour is not settled by the Java evidence.

**C2 honest scope.** It removes objects, not much wall time: the median strip
stage is 5.4 ns because most statements fast-reject, and only 12 of 83 corpus
statements allocate at all. Its value is larger than its direct nanoseconds
because the front end provokes 1.07x its own CPU again in GC marking, so
warm-path allocations are charged twice.

## 7. Already present in GoGraph — named, not proposed again

* **Literal stripping** (`strip.go`) — closer to Memgraph than to Neo4j.
  Differences verified: GoGraph rewrites to a **named parameter**, Memgraph to a
  **canonical literal placeholder** with values keyed by position; GoGraph hoists
  **strings only**, Memgraph also ints, doubles and booleans; GoGraph restricts
  hoisting to `MATCH`/`WHERE`, Memgraph hoists everywhere and repairs `RETURN`
  names afterwards; **GoGraph has an `IndexByte` fast reject that neither
  reference has**, which is why its median strip cost is 5.4 ns.
* **Plan cache keyed on stripped text** — GoGraph returns one pointer per hit;
  Memgraph deep-clones an AST; Neo4j splits the job across five caches.
  **GoGraph's hit path is the cheapest of the three by construction.**
* **Concurrent-miss collapsing** (`planBuildGroup`) — exists because the
  generated lexer and parser share one process-wide ATN. **No prior-art
  analogue**: a GoGraph-specific answer to a Go-runtime-specific problem.
* **DDL fast-path dispatch** — the same kind of move as Neo4j's first-token bail,
  already applied where it pays most.

## 8. What could not be verified

1. Whether Memgraph ever considered or rejected SLL prediction. No
   `setPredictionMode` and no bail strategy were found where the parser is
   constructed, so it appears to run the C++ default — but whether that is a
   decision or an omission is **unverified**.
2. Any Neo4j commit stating a **measured** speedup for SLL-first or for
   `ThinCypherToken`. Searches returned empty, and an empty search is not
   evidence of absence. **No performance claim is made on Neo4j's behalf.**
3. The design rationale for Neo4j's JavaCC to ANTLR migration. The removal
   commits are verified; the rationale was not found in the sparse checkout.
4. FalkorDB's parser technology — **unassessed**.
5. The exact byte size of a Memgraph trie node. The node count (12,485) is exact;
   the ~12 MiB total is arithmetic from the struct as read, not a measurement.
6. Whether the ANTLR Go runtime's shared `decisionToDFA` behaves well under mixed
   SLL/LL population. **Only measurement in GoGraph can answer this.**

## 9. Licensing

Nothing here proposes reusing code; every candidate is a structural insight to be
reimplemented idiomatically in Go. For the record, the sources read carry
different licences — Neo4j's `community/cypher` mixes Apache-2.0 and GPLv3 files,
Memgraph is BSL 1.1, PostgreSQL is the PostgreSQL Licence. **If any reviewer
concludes that adopting a dependency or reusing source is the better route, that
is the user's decision, not the project's to take silently.**

## 10. Noted outside this campaign's scope

`cypher/parser/shortestpath.go` asserts *"identifiers are ASCII in Cypher"*, a
pre-existing divergence from openCypher's Unicode identifier rule. It is
unrelated to this campaign's costs and changes nothing proposed here — it only
happens to be what makes the C2 ASCII fold **exactly** equivalent rather than
merely near-equivalent.
