# Stream 4 — Cypher language surface and conformance

**Baseline:** `6f31f61` (v0.10.0). **Date:** 2026-07-25.
**Method:** every GoGraph claim is a measured probe (temporary `zz_probe*_test.go` files driving
`parser.Parse` / `Engine.RunAny` / `Engine.Explain`, since removed) or a `file:line` citation.
Every Neo4j/Memgraph/openCypher claim is a fetched primary source with a URL. Hardware: darwin/arm64,
Go toolchain in-repo, single process.

---

## Verdict summary

GoGraph's openCypher-9 **execution** fidelity is genuinely excellent and, in named and citable places,
**better than Memgraph** — but round 1's framing of *why* is wrong in a way that changes the whole
recommendation. openCypher is **not** frozen at ~2017: the project shipped `2024.1` (2025-04-08),
`2024.2` (2025-07-03) and `2024.3` (2026-03-20), and the GQL alignment landed in the **official BNF
grammar**, not in the TCK. GoGraph's vendored TCK is in fact **byte-identical to openCypher 2024.3**,
and the TCK has changed by exactly *four substantive lines* since `1.0.0-M23` (2023). So "100% TCK" is
an honest, *current* claim — it is simply a **weak proxy for language coverage**, and the gap to Neo4j 5
/ Cypher 25 / GQL is invisible to it by construction.

The single most valuable lever is not "add feature X". It is **replace the parser front-end's grammar
source**. GoGraph does not parse openCypher: it parses a third-party community ANTLR grammar
(`antlr/grammars-v4`, BSD-3, Boris Zhguchev) that the project's own NOTICE says is "**NOT an official
openCypher artefact**" (`cypher/parser/grammar/NOTICE.txt:9-11`), wrapped in **~4,500 lines of
workaround** — 11 pre-lex text rewriters, an 846-line hand-patch of the generated ANTLR code, a 402-line
pre-lex `shortestPath` rewrite, and a 1,255-line hand-written regex DDL parser. openCypher **publishes**
the grammar GoGraph should be using (`grammar/openCypher.bnf`, ISO WG3 BNF, GQL-aligned), and that one
artefact contains label expressions, quantified path patterns, path selectors, `OFFSET` and element-pattern
`WHERE` — i.e. essentially the whole GQL-alignment surface — from a Tier-1 source.

That same foreign grammar is also the direct cause of the one **HIGH-severity correctness defect** this
stream found: the vendored lexer's catch-all `ERRCHAR : . -> channel(HIDDEN)` **silently discards any
character it does not recognise**, so `RETURN 1 != 2` evaluates `1 = 2` and `MATCH (n:!A)` returns exactly
the nodes `MATCH (n:A)` returns. Both are silent wrong answers, and neither is reachable by the TCK.

---

## Feature-by-feature comparison

Legend: **Y** supported · **P** partial · **N** absent. Verdicts are GoGraph vs each engine.

### Clauses

| Clause | GoGraph (file:line / probe) | Neo4j 5 / 25 | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| `MATCH`, `OPTIONAL MATCH` | Y — `CypherParser.g4:105` | Y | Y | PARITY | CONFIRMED-R1 |
| `WHERE` | Y — `CypherParser.g4:174` | Y | Y | PARITY | — |
| `WITH` (+scope barrier) | Y — `CypherParser.g4:64` | Y | Y | PARITY | — |
| `RETURN`, `DISTINCT` | Y | Y | Y | PARITY | — |
| `ORDER BY` / `SKIP` / `LIMIT` | Y — `CypherParser.g4:79,68,72` | Y | Y | PARITY | — |
| `OFFSET` (SKIP synonym) | **N** — probe: `unexpected "OFFSET"` | Y (oC 2024.3 `<offset synonym>`) | N | WORSE vs Neo4j | NEW |
| `ORDER BY … NULLS FIRST/LAST` | **N** — probe rejects | Y | N | WORSE vs Neo4j | NEW |
| `UNWIND` | Y — `CypherParser.g4:109` | Y | Y | PARITY | — |
| `CREATE` | Y — `CypherParser.g4:167` | Y | Y | PARITY | — |
| `MERGE` + `ON CREATE`/`ON MATCH` | Y — `CypherParser.g4:143-147` | Y | Y | PARITY | — |
| `SET` (`=`, `+=`, labels) | Y — `CypherParser.g4:153-159` | Y | Y | PARITY | — |
| `REMOVE` | Y — `CypherParser.g4:124` | Y | Y | PARITY | — |
| `DELETE` / `DETACH DELETE` | Y — `CypherParser.g4:120` | Y | Y | PARITY | — |
| `FOREACH` | Y — local grammar mod, `grammar/README.md:34-40` | Y | Y | PARITY | — |
| `UNION` / `UNION ALL` | Y — `CypherParser.g4:305` | Y | Y | PARITY | — |
| `UNION DISTINCT` | **N** — probe rejects | Y | N | WORSE vs Neo4j | NEW |
| `CALL` procedure (+`YIELD…WHERE`) | Y — `CypherParser.g4:128,53` | Y | Y | PARITY | — |
| `CALL { }` subquery | **N** — `docs/cypher.md:944` | Y | N (`Cypher.g4`) | PARITY vs Memgraph; WORSE vs Neo4j | CONFIRMED-R1 |
| `CALL () { }` (Cypher 25 scope) | N | Y (25) | N | WORSE vs Neo4j | NEW |
| `CALL { } IN TRANSACTIONS` | **N** — `docs/cypher.md:945` | Y | N | PARITY vs Memgraph | CONFIRMED-R1 |
| `CALL { } IN [k] CONCURRENT TRANSACTIONS` | N | Y (25) | N | PARITY vs Memgraph | NEW |
| `… DISJOINT BY {NONE\|AUTO\|(expr,…)}` | N | Y (Cypher-25-only, Neo4j 2026.06) | N | **not applicable** — see F11 | NEW |
| `… ON ERROR RETRY [FOR d SECONDS] [THEN …]` | N | Y (25) | N | **not applicable** — see F11 | NEW |
| `USE` | N | Y | N | PARITY vs Memgraph | — |
| `LOAD CSV` | **N** — zero hits repo-wide | Y | Y (`MemgraphCypher.g4`) | WORSE vs both | STALE-R1 |
| `EXPLAIN` prefix | **N** — method `Engine.Explain` only | Y | Y | WORSE vs both | NEW |
| `PROFILE` | **N** — absent | Y | Y | WORSE vs both | NEW |
| `FINISH` / `INSERT` / `NEXT` / `LET` / `FOR` / `FILTER` | N | Y (Cypher 25) | N | PARITY vs Memgraph | NEW |
| `SHOW INDEXES` / `SHOW CONSTRAINTS` | Y — `cypher/show.go`, `ir/ddl_parser.go:146` | Y | Y | PARITY | — |
| `SHOW FUNCTIONS`/`PROCEDURES`/`TRANSACTIONS`/`DATABASES` | **N** — probe rejects | Y | P | WORSE vs Neo4j | NEW |

### Expressions

| Expression | GoGraph | Neo4j 5 / 25 | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| `CASE` (simple + searched) | Y — `CypherParser.g4:388` | Y | P (docs: no multi-value `WHEN`) | PARITY/BETTER | NEW |
| List comprehension | Y — `CypherParser.g4:376` | Y | Y | PARITY | — |
| Pattern comprehension | Y — `CypherParser.g4:368` | Y | Y | PARITY | — |
| `reduce()` | Y — hand-patch, `CypherParser.g4:236-238` | Y | Y | PARITY | — |
| `ALL/ANY/NONE/SINGLE` | Y — `CypherParser.g4:364` | Y | Y | PARITY | — |
| `EXISTS { }` | Y — `CypherParser.g4:309`; probe OK for both pattern and full-query forms | Y | Y | PARITY | — |
| `COUNT { }` | Y — `CypherParser.g4:313`; probe returns 1/0 | Y | **N** (docs) | **BETTER vs Memgraph** | NEW |
| `COLLECT { }` | **N** — no token, no AST node; probe: parse error | Y | N | WORSE vs Neo4j | NEW |
| Map projection `n{.a, .*}` | Y — `CypherParser.g4:257-273`; probe OK | Y | Y | PARITY | — |
| `STARTS WITH`/`ENDS WITH`/`CONTAINS`/`IN` | Y — `CypherParser.g4:250` | Y | Y | PARITY | — |
| `=~` regex | Y — but via raw-source hack, `visitor.go:2936-2968` | Y | Y | PARITY (fragile) | NEW |
| `IS NULL` / `IS NOT NULL` | Y — `CypherParser.g4:254` | Y | Y | PARITY | — |
| `IS :: T` / `IS TYPED T` | **N** — probe rejects | Y | N (docs: use `valueType()`) | PARITY vs Memgraph | NEW |
| `IS NORMALIZED` | N | Y | N | PARITY vs Memgraph | NEW |
| Label expressions `&`, `\|`, `!`, `%` | **N** — and `!` is **silently dropped** (see F1) | Y (5.0+) | N, only `\|` (`Cypher.g4`) | WORSE vs Neo4j; **defect** | NEW |
| `\|\|` string concat | N | Y (25) | N | PARITY vs Memgraph | NEW |
| `exists(prop)` / `exists(pattern)` fn | **N** — probe: parse error | deprecated/removed in 5 | Y (Pattern fn) | mixed | NEW |
| Pattern predicate in `WHERE` | Y — TCK `Pattern1.feature` green | Y | P | PARITY | — |
| Pattern as projection value | N — deliberate `SemaError`, `visitor.go:934-937` | Y (`size((n)-->())` removed in 5) | P | PARITY | — |

### Patterns

| Feature | GoGraph | Neo4j 5 / 25 | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Var-length `[*]`, `[*1..3]`, `[*0..1]` | Y — probe verified counts | Y | Y | PARITY | — |
| `shortestPath()` / `allShortestPaths()` | Y — `parser/shortestpath.go` pre-lex rewrite; probe returns `length(p)=1` | Y | **N** (docs: use `[*BFS]`) | **BETTER vs Memgraph** | NEW |
| Weighted / k-shortest expansions | N | GDS (Enterprise) | Y (`*WSHORTEST`, `*KSHORTEST`, `*ALLSHORTEST`) | WORSE vs Memgraph | NEW |
| Quantified path patterns `(…){1,3}` | N | Y — **Neo4j 5.9, i.e. Cypher 5** | N (docs) | PARITY vs Memgraph | CONFIRMED-R1 |
| Quantified relationships `-[:T]->{1,3}` | N | Y (same 5.9 line) | N (docs) | PARITY vs Memgraph | NEW |
| Path selectors `ANY k / ALL / ALL SHORTEST / SHORTEST k / SHORTEST k GROUPS` | N | Y — **Neo4j 5.21, i.e. Cypher 5** | N | PARITY vs Memgraph | NEW |
| Element-pattern `WHERE` `(n WHERE n.x>1)` | N | Y | N | PARITY vs Memgraph | NEW |
| Match mode `DIFFERENT RELATIONSHIPS` (the explicit default) | implicit only — default is edge-isomorphism | Y (Cypher 25, 2025.06) | N | PARITY (semantics already match) | NEW |
| Match mode `REPEATABLE ELEMENTS` | **N** — no way to relax edge-uniqueness | Y (Cypher 25, 2025.06) | N | PARITY vs Memgraph | NEW |
| Path mode `ACYCLIC` (no repeated **nodes**) | **N** | Y (Cypher 25, 2026.03) | N | PARITY vs Memgraph | NEW |
| Path modes `TRAIL` / `WALK` (GQL synonyms) | N | Y | N | PARITY vs Memgraph | NEW |
| Named paths, undirected, rel-type disjunction `:A\|B`, `:A\|:B` | Y — probes OK | Y | Y | PARITY | — |
| Relationship uniqueness (edge isomorphism) | Y — TCK-enforced | Y | Y | PARITY | — |

Path-mode sources (canonical slugs, all under `neo4j.com/docs/cypher-manual/current/`):
`patterns/unique-relationship-paths/` (`DIFFERENT RELATIONSHIPS`),
`patterns/repeatable-node-and-relationship-paths/` (`REPEATABLE ELEMENTS`),
`patterns/acyclic-paths/` (`ACYCLIC`).

### Type system

GoGraph's value kinds are exactly **16**: `KindNull, Integer, Float, String, Bool, List, Map, Node,
Relationship, Path` (`cypher/expr/value.go:42-60`) plus `KindDate, LocalDateTime, DateTime, LocalTime,
Time, Duration` (`cypher/expr/temporal.go:47-57`).

| Type | GoGraph | Neo4j 5 | Memgraph | Verdict |
|---|---|---|---|---|
| INTEGER (64-bit, overflow = error) | Y — probe: `ArithmeticOverflow` | Y | Y | PARITY |
| FLOAT (IEEE-754; `1.0/0 = +Inf`) | Y — probe | Y | Y | PARITY |
| STRING / BOOLEAN / LIST / MAP / NULL | Y | Y | Y | PARITY |
| NODE / RELATIONSHIP / PATH | Y | Y | Y | PARITY |
| DATE/LOCALTIME/TIME/LOCALDATETIME/DATETIME/DURATION | Y — full: map ctors, `.realtime()/.statement()/.transaction()`, component access, `date + duration`, `.truncate` (all probe-verified) | Y | Y | PARITY |
| **POINT / spatial** | **N** — `point()` is an unknown function | Y (`point`, `point.distance`, `point.withinBBox`) | Y (`point`, `point.distance`, `point.withinbbox`) | **WORSE vs both** |
| `CLOSED DYNAMIC UNION` / `ANY` / `PROPERTY VALUE` type names | N | Y (25) | N | PARITY vs Memgraph |
| Three-valued logic (`null = null → null`) | Y — probe | Y | Y | PARITY |
| Cross-type numeric equality `1 = 1.0` | Y — probe `true` | Y | Y | PARITY |

---

## Findings

### F1. The lexer silently discards unrecognised characters, producing wrong answers  [NEW]  (severity: **HIGH**)

- **What they do:** Neo4j 5 and Memgraph both reject unknown characters with a syntax error. Neo4j has
  no `!=` operator (`<>` only) and rejects it; Memgraph documents `!` only inside label expressions,
  which it does not support (<https://memgraph.com/docs/querying/differences-in-cypher-implementations>).
- **What GoGraph does:** the vendored lexer ends with a catch-all
  `ERRCHAR : . -> channel(HIDDEN);` — `cypher/parser/grammar/CypherLexer.g4:157`. Any character the
  lexer does not recognise is routed to the **hidden channel** and vanishes before the parser sees it.
  The ANTLR error listener never fires, because no error token is ever produced.
- **Evidence (measured, `parser.Parse` + `ast.Print` + `Engine.RunAny`):**

  | Input | GoGraph AST | GoGraph result | Correct (Neo4j 5) |
  |---|---|---|---|
  | `RETURN 1 != 2` | `RETURN (1 = 2)` | `false` | syntax error |
  | `RETURN 1 !< 2` | `RETURN (1 < 2)` | `true` | syntax error |
  | `RETURN 1 !> 2` | `RETURN (1 > 2)` | — | syntax error |
  | `RETURN 1 !<> 2` | `RETURN (1 <> 2)` | — | syntax error |
  | `RETURN 1 #+ 1` | `RETURN (1 + 1)` | `2` | syntax error |
  | `MATCH (n:!A) RETURN n.n` | `MATCH (n:A) RETURN n.n` | **`"hasA"`** | **`"hasB"`** |
  | `MATCH (n) WHERE n.p !< 1 …` | `WHERE (n.p < 1)` | — | syntax error |

  The last row is the dangerous shape: a Neo4j-5 user writing the standard negated label expression
  `:!A` gets the **exact complement** of the correct answer set, with no warning. Applied to
  `MATCH (n:!Archived) DETACH DELETE n` this deletes precisely the wrong nodes.
- **Lever:** add a pre-lex character guard alongside the existing DoS guard
  (`cypher/parser/guard.go`, already a single O(n) allocation-free pass), rejecting any byte outside the
  Cypher alphabet when it occurs **outside** a string literal, backtick identifier or comment. The
  scanner that must skip those regions already exists and is proven —
  `copyStringOrComment` in `cypher/parser/shortestpath.go`. Emit a `*ParseError` naming the offending
  character and offset. Then, separately, implement `!` properly as label negation (see the extension list).
- **TCK/ACID impact:** **measured TCK-safe.** `!` appears exactly twice in the entire 220-file corpus,
  both times *inside string literals*: `clauses/create/Create4.feature:370`
  (`'Freedom! Forever!'`) and `clauses/with-orderBy/WithOrderBy1.feature:1147` (list element `'!'`).
  A guard that skips string regions cannot see either. No ACID surface is touched — this is parse-time,
  before any transaction begins, and it converts a silent wrong answer into a fail-stop error, which is
  what the project's "fail-stop, never fail-silent" rule requires.
- **Effort:** **S** (guard ~80 LOC + regression tests). This is the highest value-per-line change in the stream.

### F2. openCypher is not frozen — but its TCK is, and GoGraph already has the newest one  [STALE-R1]  (severity: HIGH, informational)

- **Round 1 said:** "'100% TCK' = frozen ~2017 openCypher 9 TCK, ZERO GQL constructs."
- **What is actually true (measured):**

  | openCypher release | Date (GitHub API `published_at`) |
  |---|---|
  | `1.0.0-M23` | 2023-07-11 |
  | `2024.1` | **2025-04-08** |
  | `2024.2` | **2025-07-03** |
  | `2024.3` | **2026-03-20** |

  Release notes: 2024.1 is "the first release on the path of evolving the specification towards
  ISO/IEC 39075 GQL"; 2024.2 added "label expressions grammar", "element pattern where clauses" and
  "quantified path patterns grammar"; 2024.3 added "SHORTEST grammar"
  (<https://github.com/opencypher/openCypher/releases>).
- **But the TCK did not move.** Downloading the release tarballs and diffing:
  - Every ref (`1.0.0-M23`, `2024.1`, `2024.2`, `2024.3`, `master`, `main`) has **exactly 220** feature files.
  - `1.0.0-M23` → `2024.3`: 220 files differ, but *the only substantive change* is **4 diff lines in
    `clauses/with/With1.feature`**; every other difference is the copyright header.
  - **GoGraph's `cypher/tck/features/` is byte-identical to openCypher `2024.3`** — `diff -rq` reports
    **0 differing files**. (It differs from `master` only by that same copyright header.)
- **Consequence — this is the reframing that matters:** GoGraph's "100% TCK, 3897/3897"
  (`cypher/tck/runner_test.go:2030`) is **true against the current openCypher TCK**, not against a
  2017 relic. The claim is honest. What it is *not* is a measure of language coverage: openCypher put
  its entire GQL alignment into `grammar/openCypher.bnf`, and **the TCK exercises none of it**. Every
  GQL construct is therefore TCK-neutral *by construction* — which is good news for the extension plan.
- **Lever:** stop treating the TCK as the conformance target and start treating
  **`openCypher.bnf@2024.3` as the grammar target**, with the TCK as the regression floor it already is.
  Add a CI check that re-downloads the upstream TCK and asserts byte-identity, so the "byte-identical
  to the current release" property is *maintained* rather than coincidental.
- **TCK/ACID impact:** none — this is a claim-hygiene and roadmap-framing change.
- **Effort:** S (the CI identity check); the grammar work is F3.

### F3. GoGraph does not parse openCypher — it parses a third-party grammar, wrapped in ~4,500 LOC of workaround  [NEW]  (severity: HIGH)

- **What they do:** Neo4j generates its parser from its own maintained grammar and publishes the
  language reference from the same source tree (`github.com/neo4j/docs-cypher`). Memgraph maintains
  `src/query/frontend/opencypher/grammar/Cypher.g4` as a first-class artefact and layers its extensions
  in a separate `MemgraphCypher.g4`. **openCypher itself publishes the grammar**, in ISO WG3 BNF —
  the same notation ISO/IEC 39075 GQL uses — precisely so implementers can track GQL
  (`grammar/README.adoc`: "most non-terminals can serve as a pointer into the GQL specification").
- **What GoGraph does:** vendors `antlr/grammars-v4` @ `284602b3f23ca54dc30778204ab7ae9e969145e9`,
  BSD-3, author Boris Zhguchev. The project's own NOTICE states it is "**NOT an official openCypher
  artefact and … not endorsed by the openCypher project or Neo4j**"
  (`cypher/parser/grammar/NOTICE.txt:9-11`). `cypher/parser/shortestpath.go:6-9` records that the
  grammar has "**drifted**" and that regenerating from it "is not behaviour-preserving".
- **Evidence — measured workaround surface (`wc -l`):**

  | Layer | LOC | What it works around |
  |---|---|---|
  | `cypher/parser/normalize.go` | 2,121 | **11** pre-lex text rewriters: `normalizeArithmeticMinus`, `normalizeVarlenBounds`, `normalizeVarlenDotDot`, `normalizeZeroDotFloat`, `normalizeLeadingDotFloat`, `normalizeDoubleNot`, `normalizeCallNoParen`, `normalizeNegHexOct`, `normalizeFloatExpZeroPad`, `validateUnicodeEscapes`, `normalizeSingleQuotes` |
  | `cypher/parser/grammar/gen-patches.patch` | 846 | hand-patches to **generated** ANTLR Go across 5 files |
  | `cypher/parser/shortestpath.go` | 402 | pre-lex rewrite of `shortestPath()`/`allShortestPaths()` |
  | `cypher/ir/ddl_parser.go` + `ddl_show.go` | 1,255 | hand-written **regex** parser for all DDL and SHOW |
  | `cypher/parser/rebalance.go` | 82 | fixes the grammar's wrong operator precedence for `IN`/`CONTAINS`/`STARTS WITH`/`ENDS WITH` vs arithmetic |
  | **Total** | **~4,700** | |

  Plus `=~`, which has **no lexer token at all**: `visitor.go:2936-2968` detects it by inspecting whether
  the raw source character immediately after an `ASSIGN` token is `~`.
- **Lever:** migrate the parser front-end to the **official openCypher 2024.3 BNF**
  (`grammar/openCypher.bnf`, 1,533 lines). This is not merely tidier — it *is* the extension roadmap,
  because that one file already defines, as Tier-1 normative text:
  `<label expression>` with `|`/`&`/`!`/`%` and parentheses (lines 586-608); `<graph pattern quantifier>`
  = `*` `+` `{n}` `{m,n}` and `<quantified path primary>` (lines 355-456); the full path-selector family
  `ALL`/`ANY [k]`/`ALL SHORTEST`/`ANY SHORTEST`/`SHORTEST k`/`SHORTEST k GROUP(S)` (lines 300-334);
  `<offset synonym> ::= SKIP | OFFSET` (lines 246-251); `<element pattern where clause>` and
  `<parenthesized path pattern where clause>` (lines 362-389); and it *retains*
  `<legacy shortest path pattern>` so GoGraph's existing behaviour stays conformant.
  Do it incrementally: keep the ANTLR runtime, port rule-by-rule behind the existing green TCK gate,
  and retire one `normalize.go` rewriter per ported rule (each retirement is independently verifiable).
- **TCK/ACID impact:** the TCK is the regression gate for the migration, run at every step; the
  3897 baseline in `cypher/tck/runner_test.go:2030` must not move. No ACID surface — parse-time only.
- **Effort:** **L** (multi-sprint), but it is the enabler for essentially every other item in the
  extension list, and it removes the class of defect F1 belongs to.

### F4. `docs/cypher.md` claims `COLLECT { }` is supported; it is not  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** `docs/cypher.md:947-949` — "`EXISTS { … }`, `COUNT { … }`, and `COLLECT { … }`
  subquery *expressions* are supported … only the standalone `CALL { … }` subquery *clause* is unsupported."
- **Evidence:** there is **no `COLLECT` token** in `cypher/parser/grammar/CypherLexer.g4`, **no
  `subqueryCollect` rule** in `CypherParser.g4` (contrast `subqueryExist` at :309 and `subqueryCount` at
  :313), and **no `CollectSubquery` AST node** in `cypher/ast/expressions.go` (contrast `ExistsSubquery`
  at :321 and `CountSubquery` at :345). Execution probe:
  `MATCH (n:P) RETURN COLLECT { MATCH (n)-->(m) RETURN m.n }` →
  `cypher: parse: unexpected "(" at 1:35, expected one of {':', '|'}`.
- **Lever:** correct the sentence now (S), and implement `COLLECT { }` (see extension list, rank 5).
- **TCK/ACID impact:** none. This is a direct breach of CLAUDE.md's "Documentation must be **accurate and
  faithful to the code** — never document intent, only what is actually implemented."
- **Effort:** S to fix the doc.

### F5. LOAD CSV is *not* TCK-covered — round 1 had this backwards  [STALE-R1]  (severity: MEDIUM)

- **Round 1 said:** "LOAD CSV *is* TCK-covered while index/constraint/GQL features have no feature files."
- **Measured:** `grep -ril "LOAD CSV" cypher/tck/features/` → **0 files**, **0 lines**.
  Same for `CREATE INDEX`/`CREATE CONSTRAINT` → **0**. The upstream TCK tree confirms it: only
  `clauses/`, `expressions/`, `useCases/` exist, with no load-csv, index, constraint, shortestPath,
  subquery or spatial directory (<https://github.com/opencypher/openCypher/tree/master/tck/features>).
  Nor is LOAD CSV in the openCypher 2024.3 grammar: `grep -ic "LOAD\|CSV" openCypher.bnf` → **0**.
  So LOAD CSV is a **Neo4j/Memgraph vendor extension**, not openCypher, and it is TCK-invisible.
- **What GoGraph does:** no LOAD CSV anywhere (`grep -ril 'load csv'` over the whole repo → 0). CSV
  ingestion exists only as the Go API `graph/io/csv`.
- **Lever:** LOAD CSV is purely additive and cannot touch the TCK. But note the correct justification:
  it is not needed for conformance, it is needed for **usability parity** with both incumbents. Rank it
  on utility, not on conformance. Its transactional shape (`CALL {} IN TRANSACTIONS`) is what makes it
  hard, and that intersects ACID — so ship LOAD CSV *inside a single transaction* first.
- **TCK/ACID impact:** TCK-neutral (0 scenarios). Single-transaction LOAD CSV preserves atomicity
  trivially; batched LOAD CSV must not be shipped until the `IN TRANSACTIONS` semantics are agreed
  with the user, because per-batch commit deliberately *weakens* statement atomicity.
- **Effort:** M (single-transaction), L (batched).

### F6. Function catalogue: the measured gap against Neo4j, grouped  [NEW]  (severity: MEDIUM)

- **What GoGraph has:** **81** registered scalar/list/string/math/temporal functions
  (`grep -rhoE 'Register\("[a-zA-Z_.0-9]+"' cypher/funcs/*.go`, excluding the test-only `myfn`), plus
  **10** aggregates dispatched separately (`cypher/ir/aggregation.go:33-44`).
- **Neo4j's list** (authoritative, from Neo4j's own docs source, `github.com/neo4j/docs-cypher`,
  `modules/ROOT/pages/functions/*.adoc`). Grouped gap:

  | Group | Missing in GoGraph | Neo4j source |
  |---|---|---|
  | **Spatial** (whole category) | `point()`, `point.distance()`, `point.withinBBox()`, `distance()` | `spatial.adoc` |
  | **Scalar** | `nullIf`, `valueType`, `toBooleanOrNull`, `toFloatOrNull`, `toIntegerOrNull`, `toStringOrNull` | `scalar.adoc` |
  | **Predicate** | `isEmpty`, `exists` (property + pattern form), `allReduce` (Cypher 25, 2025.08) | `predicate.adoc` |
  | **String** | `btrim`, `normalize`, `lower`, `upper`, `string.indexOf`, `string.join`, `string.regexReplace` (all three Cypher 25, 2026.05) | `string.adoc` |
  | **Scalar (extra)** | `char_length`, `character_length` | `scalar.adoc` |
  | **Temporal** | `format()` (Cypher 25, 2025.09) | `temporal/format.adoc` |
  | **GQL naming aliases** (Neo4j 2026.02, Cypher 25) | `collect_list`, `percentile_cont`, `percentile_disc`, `stdev_samp`, `stdev_pop`, `path_length`, `zoned_datetime`, `local_datetime`, `local_time`, `zoned_time`, `duration_between` — pure synonyms of functions GoGraph **already has**, so this row is the cheapest conformance win on the page | `aggregating.adoc`, `scalar.adoc`, `temporal/` |
  | **Math (log)** | `ln` | `mathematical-logarithmic.adoc` |
  | **Math (numeric)** | `ceiling`, `round(x, precision)`, `round(x, precision, mode)` | `mathematical-numeric.adoc` |
  | **Math (trig)** | `cot`, `coth`, `cosh`, `sinh`, `tanh`, `haversin` | `mathematical-trigonometric.adoc` |
  | **List (Cypher 25)** | `collect.distinct/flatten/indexOf/insert/max/min/remove/sort` family | `list.adoc` |
  | **Vector** | `vector.similarity.cosine`, `vector.similarity.euclidean` | `vector.adoc` |
  | **Graph / Database** | `graph.names`, `graph.byName`, `graph.propertiesByName`, `graph.byElementId`, `nameFromElementId` | `graph.adoc`, `database.adoc` |
  | **LOAD** | `file()`, `linenumber()` | `load-csv.adoc` |
  | **Temporal** | `date.format` and the Cypher-25 format family | `temporal/format.adoc` |

  Verified by execution probe: every one of the above returns
  `SyntaxError.UnknownFunction` or an arity error (`round(1.234, 2)` →
  "round() takes exactly 1 argument(s), got 2").
- **Where GoGraph is complete and Memgraph is not:** GoGraph implements all four openCypher-9 statistical
  aggregates — `stDev`, `stDevP`, `percentileCont`, `percentileDisc`
  (`cypher/funcs/aggregators.go:472,516,564,648`; probe: `percentileDisc(x,0.5)` over `[1,2,3]` → `2`).
  **Memgraph supports only `avg, collect, count, max, min, sum`** and explicitly not
  `percentileCont`, `percentileDisc`, `stDev`, `stDevP`
  (<https://memgraph.com/docs/querying/functions>). See the Nothing-to-take list.
- **Lever:** add the missing functions in the priority order given in the extension list. The trig/math/
  string/scalar tail is trivially additive; **spatial is a type-system change**, not a function addition.
- **TCK/ACID impact:** **measured TCK-neutral.** Zero TCK scenarios reference `point(`, `round(`, `floor(`,
  `exp(`, `log(`, `sin(`, `pi(`, `id(`, `elementId(`, `reduce(`, `replace(`, `toUpper(`, `left(`, `right(`,
  `trim(`, `timestamp(`, `randomUUID(`, `isNaN(`, `stDev`, `stDevP`. Adding new *names* to the registry
  cannot affect any scenario that does not call them.
- **Effort:** S per function; M for the whole tail; L for spatial (new value kind, new comparability
  rules, new index kind, new WAL/snapshot encoding — this one touches durability, so it is not "just a function").

### F7. Schema DDL is single-property, node-only, two-kind  [NEW]  (severity: MEDIUM)

- **What GoGraph does** (probe against `Engine.RunAny`, and `cypher/ir/ddl_parser.go`):
  - Works: `CREATE INDEX [name] FOR (n:L) ON (n.p) [OPTIONS {indexType:'hash'|'btree'}]`,
    `DROP INDEX name [IF EXISTS]`, `CREATE CONSTRAINT … REQUIRE n.p IS UNIQUE | IS NOT NULL`,
    `DROP CONSTRAINT`, `SHOW INDEXES`, `SHOW CONSTRAINTS` (with `YIELD`/`WHERE`/`RETURN`).
  - Explicitly rejected with typed errors: composite indexes ("composite indexes (multiple properties)
    are not supported"), composite constraints, `IS NODE KEY`, relationship indexes
    (`FOR ()-[r:R]-()`), relationship constraints, `IS :: <TYPE>` property-type constraints.
  - Not parsed at all: `TEXT`/`POINT`/`FULLTEXT`/`VECTOR`/`LOOKUP` index kinds.
- **Neo4j 5:** RANGE (default), TEXT, POINT, LOOKUP, FULLTEXT, VECTOR indexes; UNIQUE, NODE KEY,
  RELATIONSHIP KEY, IS NOT NULL, property-type constraints; composite everywhere.
  **Memgraph:** label, label-property, edge-type, point and vector indexes; explicitly *no*
  relationship constraints ("Relationship constraints … raise errors in Memgraph") and
  "indexes are not created in advance and creating constraints does not imply index creation"
  (<https://memgraph.com/docs/querying/differences-in-cypher-implementations>).
- **Lever:** composite index + NODE KEY is already round-1 T2 item (6) — not re-filed. What round 1
  missed and this stream adds: **relationship property indexes and relationship `IS NOT NULL`
  constraints**, which Neo4j has and Memgraph explicitly does not — a place GoGraph could pass Memgraph
  rather than merely catch up.
- **TCK/ACID impact:** zero TCK scenarios touch DDL, so all of it is TCK-neutral. **Constraints are an
  ACID Consistency invariant**: any new constraint kind must be enforced at commit time, not only at
  the mutating statement (this repo has been bitten by exactly that — see `docs/audit-constraint-readiness-2026-07-13.md`).
- **Effort:** M per index kind; L for relationship constraints (new commit-time sweep).

### F8. Procedure surface is six `db.*` procedures  [NEW]  (severity: LOW-MEDIUM)

- **What GoGraph has:** `db.indexes`, `db.constraints`, `db.labels`, `db.relationshipTypes`,
  `db.propertyKeys`, `db.schema.visualization` (`cypher/procs/builtin_db.go:9-16, 68-75`), plus a
  user-extensible `Registry` (`cypher/procs/registry.go`).
- **Measured absences:** `dbms.components()` → "procs: procedure not found: dbms.components";
  `db.awaitIndexes()` → not found. No `dbms.*` namespace at all.
- **Neo4j:** a large `db.*`/`dbms.*` surface plus APOC. **Memgraph:** `mg.*` plus MAGE.
- **Applicability judgement:** GoGraph is an **embeddable in-process Go library**. Most of `dbms.*`
  (cluster, security, config) is meaningless here, and a Go caller reaches for the Go API, not a
  procedure. But **`dbms.components()` is different**: every official Neo4j driver and every Bolt
  client tool calls it during handshake/version negotiation. Its absence is a *client-compatibility*
  bug wearing a *procedure-coverage* costume, and it belongs to the Bolt stream's remit as much as this one.
- **Lever:** implement `dbms.components()` returning `name/versions/edition`, and
  `db.awaitIndexes()`/`db.awaitIndex()` as no-ops (GoGraph builds indexes synchronously, so "await" is
  vacuously satisfied — `cypher/show.go:42-45` already documents that state is always `ONLINE`).
- **TCK/ACID impact:** zero TCK scenarios reference either. Both are pure reads.
- **Effort:** S.

### F9. `EXPLAIN`/`PROFILE` are not query prefixes; `PROFILE` does not exist  [NEW]  (severity: MEDIUM)

- **What they do:** in both Neo4j and Memgraph, `EXPLAIN <query>` and `PROFILE <query>` are query
  prefixes, so any client — driver, shell, notebook, Bolt tool — can obtain a plan without an API change.
- **What GoGraph does:** `Engine.Explain(query, params)` is a **Go method**; `EXPLAIN MATCH (n) RETURN n`
  is a parse error ("unexpected \"EXPLAIN\" at 1:0"). There is **no** `PROFILE` equivalent — no
  executed-plan operator statistics (rows/db-hits per operator) anywhere.
- **Evidence:** probe; and `Engine.Explain("MATCH (n:P) WHERE n.age > 1 RETURN n", nil)` returns a
  one-line plan `└─ NodeByLabelScan [n:P] (est. rows=0, exact)` — note the predicate does not appear
  as a `Filter` operator in the rendering, which is itself worth a look by the planner stream.
- **Lever:** (a) accept `EXPLAIN`/`PROFILE` as query prefixes routed to the existing explain path —
  trivial, and it is what unlocks plan inspection over Bolt; (b) add `PROFILE` with per-operator row
  counts. (a) is S and high-value; (b) is M.
- **TCK/ACID impact:** `EXPLAIN`/`PROFILE` appear in **zero** TCK scenarios. `EXPLAIN` must not execute
  (no writes, no transaction); `PROFILE` executes and must therefore obey the same transactional path as
  a normal run — no separate commit path, or atomicity is at risk.
- **Effort:** S / M.

### F10. Parse-error quality: one misleading follow-set for every failure  [NEW]  (severity: LOW)

- **What GoGraph does:** essentially every rejection reports the same fixed token set. `SHOW FUNCTIONS`,
  `USE mydb …`, `LOAD CSV …`, `CALL { … }`, `MATCH (n:A&B) …` all yield
  `unexpected "X" at 1:0, expected one of {'CALL', 'YIELD', 'CREATE', 'DELETE', 'DESC', 'DETACH',
  'EXISTS', 'MATCH', 'MERGE', 'ON', 'OPTIONAL', 'ORDER', 'REMOVE', 'RETURN', 'SET', 'SKIP', 'WITH',
  'UNION', 'UNWIND', 'AND', 'FOREACH', ID}`. For `SHOW FUNCTIONS` this is actively wrong — `SHOW` *is*
  accepted, by a different parser (`ir.IsDDL`, `ir/ddl_parser.go:88-95`), just not for `FUNCTIONS`.
- **Neo4j** returns feature-aware messages naming the construct and, in 5.x, a `GqlStatusObject` with a
  standard GQLSTATUS code. **Memgraph** returns rule-specific messages.
- **Lever:** cheap and effective — a small "known construct, not supported" table consulted *before*
  the ANTLR follow-set message is rendered, mapping leading keywords (`LOAD`, `USE`, `SHOW <x>`,
  `CALL {`, `PROFILE`, `EXPLAIN`, `FOREACH`-in-bad-position, `:!`, `:&`) to a one-line
  "not supported in GoGraph; see docs/cypher.md#known-limitations". This is the difference between a
  user filing a bug and a user reading the doc.
- **TCK/ACID impact:** the TCK asserts error *types* (`SyntaxError`), not messages, and the local runner
  is already lenient on message text — so message improvements are free. No ACID surface.
- **Effort:** S.

### F11. `IN CONCURRENT TRANSACTIONS` / `DISJOINT BY` / `ON ERROR RETRY` — a Cypher-25 concurrency surface that solves a problem GoGraph does not have  [NEW]  (severity: LOW — classification finding)

- **What they do:** Neo4j 25's full subquery-in-transactions syntax is considerably larger than the
  `IN TRANSACTIONS OF n ROWS` form round 1 recorded
  (`neo4j/docs-cypher`, `modules/ROOT/pages/subqueries/subqueries-in-transactions.adoc:25-31`):

  ```
  CALL { subQuery } IN [[concurrency] CONCURRENT] TRANSACTIONS
    [OF batchSize ROW[S]]
    [DISJOINT BY {NONE | AUTO | (<expr>, ...)}]
    [REPORT STATUS AS statusVar]
    [ON ERROR {CONTINUE | BREAK | FAIL | RETRY [FOR] [duration SEC[OND[S]]] [THEN {CONTINUE|BREAK|FAIL}]}]
  ```

  `DISJOINT BY` (same file, `:1108-1132`, tagged `label--new-Neo4j-2026.06 label--cypher-25-only`) is
  **batch scheduling for deadlock prevention**, not a path construct: it "ensur[es] that batches sharing
  resources never run at the same time, while batches that do not share resources run concurrently"
  (`:1112`). Three forms — `(<expr>, …)` declares lock-prone resources per row, `AUTO` lets the runtime
  infer them, `NONE` disables scheduling and overrides the database default
  `dbms.cypher.transactions.default_subquery_batch_strategy` (`:1115-1124`). Critically, it
  "**can only be run together with concurrent transactions; it is not valid in a regular
  `CALL { ... } IN TRANSACTIONS` subquery**" (`:1132`).
- **Classification (correcting a plausible mis-filing):** none of this belongs under patterns or
  shortest-path. It is **subquery execution**, and it is Neo4j-proprietary Cypher 25 — `grep -ic` over
  the openCypher 2024.3 BNF returns **0** for `DISJOINT`, `CONCURRENT` and `RETRY`, and there are **0**
  TCK scenarios. It is outside openCypher 9 entirely.
- **Applicability judgement — this is the substantive point:** `DISJOINT BY` exists to schedule
  **concurrent writers** so they do not deadlock. **GoGraph has exactly one writer.** Its transaction
  model is single-writer serializable and retry-free by deliberate design (round 1, area 3; MVCC
  rejected per Fekete 2005). Two write batches therefore *cannot* run concurrently, cannot contend, and
  cannot deadlock — so the entire construct is answering a question GoGraph's architecture has already
  answered a different and stronger way. `ON ERROR RETRY` is likewise a remedy for the optimistic-locking
  aborts GoGraph does not produce.
- **Lever:** **nothing to take** for `DISJOINT BY`, `CONCURRENT`, or `ON ERROR RETRY`, unless and until
  GoGraph adopts concurrent disjoint writers (round 1's area-8 gap) — at which point revisit
  `DISJOINT BY (<expr>, …)` as the *declarative* way to express write-disjointness, which is a genuinely
  good idea worth remembering. The parts of this syntax that *are* worth having if `CALL {}` subqueries
  ever land are the mundane ones: `OF n ROWS`, `REPORT STATUS AS`, `ON ERROR {CONTINUE|BREAK|FAIL}`.
- **TCK/ACID impact:** TCK-neutral (0 scenarios, not in the openCypher grammar). **But the whole family
  sits on the wrong side of the ACID mandate**: `IN TRANSACTIONS` is structurally non-atomic — it splits
  one statement into independently committing inner transactions, so a failure leaves part of the work
  durable. That is a deliberate weakening of Atomicity, and CLAUDE.md's decision-autonomy rule puts it
  squarely in the **"requires an explicit user mandate"** bucket, not the additive-and-safe bucket.
- **Effort:** n/a — recommended against.

### F12. Path modes and match modes: GoGraph has one hard-wired matching semantics and no way to name or vary it  [NEW]  (severity: MEDIUM)

- **What they do:** Neo4j separates *match mode* (which elements may repeat) from *path mode* (a constraint
  on each matched path), and lets the query name both:
  - `DIFFERENT RELATIONSHIPS` — the default made explicit; "does not alter how a graph pattern is
    matched" (`patterns/unique-relationship-paths/`). Cypher 25, Neo4j 2025.06.
  - `REPEATABLE ELEMENTS` — relaxes uniqueness so **both** nodes and relationships may repeat; unbounded
    quantifiers are disallowed in combination, and it cannot be combined with `TRAIL`/`ACYCLIC`
    (`patterns/repeatable-node-and-relationship-paths/`). Cypher 25, Neo4j 2025.06.
  - `ACYCLIC` — no repeated **nodes** within a path, relationships still following the default rule
    (`patterns/acyclic-paths/`). Cypher 25, Neo4j 2026.03; combinable with restrictive selectors such as
    `SHORTEST` only from 2026.05.
  - `TRAIL` / `WALK` — GQL-conformant synonyms: `TRAIL` matches the `DIFFERENT RELATIONSHIPS` constraint,
    `WALK` matches `REPEATABLE ELEMENTS`.
- **What GoGraph does:** exactly one semantics — openCypher edge-isomorphism, relationships unique within
  a `MATCH`, nodes free to repeat — hard-wired and TCK-enforced. There is no syntax to name it, relax it,
  or tighten it to acyclic.
- **Evidence:** GoGraph's default *is* `DIFFERENT RELATIONSHIPS` / `TRAIL`, so the semantics are already
  conformant; what is missing is the vocabulary. Note the openCypher 2024.3 BNF does **not** yet define
  path modes at all — `grep -ic` returns 0 for `TRAIL`, `ACYCLIC` and `WALK`. So unlike label expressions
  and QPPs, this is currently **Neo4j-proprietary Cypher 25**, not openCypher-normative.
- **Lever:** the cheap and genuinely useful half is `ACYCLIC`, which expresses "simple path" — a query
  users otherwise write with an `all(n IN nodes(p) …)` post-filter that the planner cannot push down.
  `DIFFERENT RELATIONSHIPS` and `TRAIL` are one-line no-op synonyms for behaviour GoGraph already has,
  so they are nearly free and buy syntax compatibility. **`REPEATABLE ELEMENTS` / `WALK` should be
  declined for now**: relaxing relationship uniqueness is precisely the invariant the TCK pins, it
  interacts badly with unbounded quantifiers (Neo4j itself forbids the combination), and it would be the
  one item on this list capable of destabilising the 3897 baseline if scoped carelessly.
- **TCK/ACID impact:** `TRAIL`=0, `ACYCLIC`=0 occurrences in the corpus, and `WALK` appears only inside a
  string literal — so the keywords are measured-safe to introduce. The *semantics* stay TCK-safe only
  because the default path is untouched: every existing scenario parses without a mode and keeps
  edge-isomorphism. Read-only; no ACID surface.
- **Effort:** S for `DIFFERENT RELATIONSHIPS`/`TRAIL` (synonyms for existing behaviour); M for `ACYCLIC`.

---

## The prioritised, TCK-safe language-extension list

This is the stream's main deliverable. **Every item below is TCK-neutral** unless the TCK column says
otherwise, and the neutrality is *measured*, not assumed: the keyword-collision scan over all 220 feature
files gives `SHORTEST`=0, `GROUPS`=0, `TRAIL`=0, `ACYCLIC`=0, `NORMALIZED`=0, `INSERT`=0, `FINISH`=0,
`LET`=0, `LOAD`=0 occurrences; `COLLECT` appears 69 times but **only** as the function `collect(` or in
scenario titles, never as an identifier; `OFFSET` appears 6 times but **only** as a property name after
a dot (`d.offset`, `d.offsetMinutes`); `TYPED`, `WALK` and `NEXT` appear only in comments and strings.

| # | Item | Additive? | TCK | ACID | Effort | Why here |
|---|---|---|---|---|---|---|
| **1** | **Reject unrecognised characters at pre-lex (F1)** | **Not purely additive — it *removes* accepted-but-wrong input** | Safe: `!` occurs twice, both inside string literals | none (parse-time) | **S** | Converts a class of silent wrong answers into fail-stop errors. Blocks nothing; unblocks item 3. |
| **2** | **Fix `docs/cypher.md` COLLECT{} claim (F4)** | doc only | none | none | **S** | Mandated accuracy; costs minutes. |
| **3** | **Label expressions `:A&B`, `:A\|B`, `:!A`, `:%`, parens** | Purely additive **once item 1 lands** — before it, `:!A` is silently accepted with the wrong meaning | Node label sets are `(n:A:B)` conjunction in the TCK; `&` `\|` `!` `%` are new syntax with 0 scenarios | none | **M** | Normative in openCypher 2024.3 `<label expression>` (lines 586-608). Neo4j 5.0+. **Memgraph does not have it.** Closes the F1 defect properly. |
| **4** | **`EXPLAIN` / `PROFILE` query prefixes (F9)** | Purely additive | 0 scenarios | `EXPLAIN` must not execute; `PROFILE` reuses the normal tx path | **S** / M | Unlocks plan inspection for every Bolt client with no API change. |
| **5** | **`COLLECT { }` subquery expression** | Purely additive | `COLLECT` never used as an identifier — measured | none | **M** | Neo4j 5.6+. Completes the `EXISTS{}`/`COUNT{}`/`COLLECT{}` trio GoGraph already half-has, and makes `docs/cypher.md:947` true. |
| **6** | **Scalar/predicate function tail: `valueType`, `isEmpty`, `nullIf`, `toXOrNull` ×4** | Purely additive | 0 scenarios reference them | none | **S** | Cheapest breadth per line. `valueType` also gives users the `IS ::` capability without the grammar work. Memgraph has all of these. |
| **7** | **Math/string tail: `ln`, `ceiling`, `cot/coth/cosh/sinh/tanh/haversin`, `btrim`, `lower`, `upper`, `char_length`, `normalize`, `round/2`, `round/3`** | Purely additive (`round` gains overloads; the 1-arg form is unchanged) | `round(`, `floor(`, `exp(`, `log(`, `sin(`, `pi(` all have **0** TCK scenarios | none | **S** | Trivial, and removes a long tail of "unknown function" surprises. |
| **8** | **`dbms.components()`, `db.awaitIndexes()` (F8)** | Purely additive | 0 scenarios | pure reads | **S** | Driver/tooling handshake compatibility, not procedure vanity. |
| **8b** | **GQL naming aliases for functions GoGraph already implements**: `collect_list`, `percentile_cont`, `percentile_disc`, `stdev_samp`, `stdev_pop`, `path_length`, `ceiling`, `ln`, `lower`, `upper`, `zoned_datetime`, `local_datetime`, `local_time`, `zoned_time` | Purely additive — registry aliases to existing implementations | 0 scenarios reference any alias | none | **S** | Highest conformance-per-line item in the whole list: ~14 Neo4j 2026.02 GQL-conformance names, each one line of registry wiring, zero new semantics. |
| **8c** | **Path modes `TRAIL` and `DIFFERENT RELATIONSHIPS` as explicit no-op synonyms; then `ACYCLIC` (F12)** | Synonyms purely additive (they name existing behaviour); `ACYCLIC` adds a new constraint | `TRAIL`=0, `ACYCLIC`=0 occurrences; `WALK` only inside a string literal | none | **S** / M | GoGraph's default already *is* `TRAIL`. `ACYCLIC` expresses "simple path", today only writable as an unpushdownable `all(n IN nodes(p) …)` post-filter. **Decline `REPEATABLE ELEMENTS`/`WALK`** — see F12. |
| **9** | **`OFFSET` as a `SKIP` synonym** | Purely additive | `OFFSET` occurs only as `d.offset` **after a dot** — keep it a non-reserved word usable as a property/identifier | none | **S** | Normative: openCypher 2024.3 `<offset synonym> ::= SKIP \| OFFSET` (lines 246-251). GQL-aligned. |
| **10** | **`UNION DISTINCT`** | Purely additive (explicit synonym of bare `UNION`) | `UNION` semantics unchanged; no scenario writes `UNION DISTINCT` | none | **S** | openCypher 2024.3 `<composite conjunction> ::= UNION [ <set quantifier> ]`. |
| **11** | **Element-pattern `WHERE`: `(n:L WHERE n.x > 1)`** | Purely additive | 0 scenarios; new position for `WHERE` | none | **M** | openCypher 2024.3 `<element pattern where clause>` (line 388). Neo4j 5.x. **Memgraph lacks it.** |
| **12** | **Grammar migration to `openCypher.bnf@2024.3` (F3)** | Behaviour-preserving by construction, verified per-rule against the TCK | The 3897 gate *is* the acceptance test | none | **L** | The enabler. Every item 13-18 becomes cheap once this lands; ~4,700 LOC of workaround retires. |
| **13** | **Quantified path patterns `((a)-[:R]->(b)){1,3}` and `-[:R]->{1,3}`** | Purely additive | 0 scenarios | none | **L** (M after item 12) | openCypher 2024.3 `<graph pattern quantifier>` (445-455). Neo4j 5.9+. **Memgraph lacks it.** Requires group-variable semantics — read GQL §, not Neo4j blog posts. |
| **14** | **Path selectors `ANY [k]`, `ALL`, `ANY SHORTEST`, `ALL SHORTEST`, `SHORTEST k`, `SHORTEST k GROUP(S)`** | Purely additive; the legacy `shortestPath()` form is *retained* by openCypher 2024.3 (`<legacy shortest path pattern>`, line 344) so no existing behaviour changes | `SHORTEST`=0, `GROUPS`=0 occurrences | none | **M** (after 12/13) | Reuses GoGraph's existing, validated BiBFS shortest-path machinery — the grammar is the only new part. |
| **15** | **`IS :: <TYPE>` / `IS TYPED <TYPE>` type predicates** | Purely additive | `TYPED` occurs only in comments | none | **M** | Neo4j 5.9+/25. **Memgraph lacks it** (its docs say "use `valueType()`"), so item 6 delivers 80% of the value at 10% of the cost — do 6 first, 15 later. |
| **16** | **`CALL { }` subquery clause** | Purely additive | 0 scenarios | Correlated subqueries must run inside the *same* transaction — no new commit path | **L** | Round-1 T2 item (7); confirmed still unshipped at `6f31f61`. Note **Memgraph does not have it either**, so this is Neo4j-parity, not table-stakes. |
| **17** | **`LOAD CSV` (single-transaction first) (F5)** | Purely additive | 0 scenarios; not even in the openCypher grammar | Single-tx = trivially atomic. **Do not ship batched/`IN TRANSACTIONS` without an explicit user decision** — per-batch commit deliberately weakens statement atomicity | **M** / L | Usability parity with both incumbents. Reuse `graph/io/csv`. |
| **18** | **POINT type + `point()`, `point.distance()`, `point.withinBBox()` (F6)** | **Not purely additive** — a new value kind changes orderability, equality, `valueType`, property storage, WAL/snapshot encoding and index eligibility | 0 scenarios reference `point(` | **Touches durability**: a new persisted property kind must round-trip through WAL and snapshot, and crash-injection coverage must be extended | **L** | The only category where GoGraph is behind **both** Neo4j and Memgraph. Highest user-visible value of the L-sized items — but sequence it after item 12 and treat it as a storage change, not a function addition. |

**Deliberately excluded**, with reasons: `USE` / composite databases and the `graph.*` function family
(GoGraph is single-graph and embeddable — a server feature is not automatically a gap);
the whole `CALL {} IN [CONCURRENT] TRANSACTIONS` family — including `DISJOINT BY`, `ON ERROR RETRY` and
`REPORT STATUS AS` (F11) — which is a *deliberate ACID trade-off*, not a language gap: it is structurally
non-atomic, so CLAUDE.md's decision-autonomy rule requires an explicit user mandate before any of it is
planned, and `DISJOINT BY` in particular solves a concurrent-writer deadlock problem GoGraph's
single-writer serializable model does not have;
Cypher-25 `FINISH`/`INSERT`/`NEXT`/`LET`/`FOR`/`FILTER` (no openCypher 2024.3 grammar backing —
`grep` returns 0 for all of them — so they are Neo4j-proprietary today);
`vector.similarity.*` (belongs with a vector index, which is round-1 T3);
`USING PERIODIC COMMIT` (removed in Cypher 25 — do not implement a construct Neo4j deleted).

**One correction to the sequencing rationale.** Quantified path patterns landed in **Neo4j 5.9** and path
selectors in **Neo4j 5.21** — both are **Cypher 5**, not Cypher 25. So items 13 and 14 are not
"catching up with a bleeding-edge dialect"; they are gaps against the *stable* Neo4j language that has
been shipping for over two years, and openCypher has since made both normative in the 2024.x grammar.
That raises their priority relative to the genuinely Cypher-25-only items (`IS ::`, `ACYCLIC`, `FOR`/`LET`).

**TCK-CONFLICTING candidates — none found.** Every construct examined is absent from all 220 feature
files. The single item that changes *accepted* behaviour is item 1, and its safety was measured
directly (the only two `!` characters in the corpus are inside string literals). This is the direct
consequence of F2: because openCypher put its GQL work in the grammar and not in the TCK, the entire
GQL surface is TCK-neutral by construction.

---

## Nothing-to-take list — where GoGraph is genuinely ahead

1. **openCypher statistical aggregates — better than Memgraph.** GoGraph implements `stDev`, `stDevP`,
   `percentileCont`, `percentileDisc` (`cypher/funcs/aggregators.go:472,516,564,648`; probe:
   `percentileDisc` over `[1,2,3]` at 0.5 → `2`). Memgraph's aggregation set is exactly
   `avg, collect, count, max, min, sum` — the other four are absent
   (<https://memgraph.com/docs/querying/functions>). Nothing to take: Memgraph is simply less conformant here.

2. **`shortestPath()` / `allShortestPaths()` — better than Memgraph.** GoGraph supports both
   (`cypher/parser/shortestpath.go`; probe returns `length(p)=1` for both). Memgraph's own docs state
   Neo4j "uses `shortestPath()`" while Memgraph "requires built-in traversal syntax like `[*BFS]`"
   (<https://memgraph.com/docs/querying/differences-in-cypher-implementations>). *Partial* counter-lever:
   Memgraph's `*WSHORTEST` / `*KSHORTEST` / `*ALLSHORTEST` cover **weighted** and **k**-shortest, which
   GoGraph does not expose in Cypher — but the right answer is openCypher 2024.3's standard
   `SHORTEST k` selector (item 14), not Memgraph's proprietary syntax.

3. **`COUNT { }` subqueries — better than Memgraph.** GoGraph has it (`CypherParser.g4:313`; probe
   returns 1/0 per row). Memgraph's differences page lists "COUNT/COLLECT subqueries" as unsupported,
   and its `Cypher.g4` has only `EXISTS '{' existsSubquery '}'`. Nothing to take.

4. **Reject the "accept a superset" posture that produced F1.** The tempting "fix" for `:!A` is to make
   the lexer *tolerant* so more Neo4j syntax slides through. That is exactly the decision that created
   the defect. GoGraph's fail-stop discipline is right; the vendored grammar violates it, and the
   remedy is stricter input, not looser.

5. **Reject Memgraph's "constraints do not imply indexes" model.** Memgraph documents that "creating
   constraints does not imply index creation"
   (<https://memgraph.com/docs/querying/differences-in-cypher-implementations>). GoGraph backs a UNIQUE
   constraint with a reserved hash index (`__uniq__<label>.<prop>`, `cypher/ir/ddl_parser.go:34-41`) and
   guards the namespace so a user cannot drop it out from under the constraint. That is the ACID-correct
   design — a uniqueness invariant that is not index-backed is an O(n) check or a silent hole. Nothing to take.

6. **Reject Neo4j's `id()` reuse semantics; keep GoGraph's documented asymmetry.** Neo4j's internal
   `id()` is reused after deletion. GoGraph documents precisely which identifier is stable and which is
   not (`docs/cypher.md:975-1004`: node `id()`/`elementId()` survives a store reopen; relationship
   `id()` is a CSR position and does not). Being *explicit* about an implementation-defined value —
   which the TCK does not constrain, and for which there are **zero** `id()`/`elementId` scenarios in the
   corpus — is better practice than either incumbent's silence. Nothing to take.

7. **Reject `DISJOINT BY` and the concurrent-transactions family outright (F11).** Neo4j 2026.06 added
   `DISJOINT BY {NONE|AUTO|(<expr>,…)}` to schedule concurrent write batches so they do not deadlock
   (`neo4j/docs-cypher`, `subqueries/subqueries-in-transactions.adoc:1108-1132`). GoGraph's single-writer
   serializable model makes concurrent write batches impossible, so there is no deadlock to prevent and
   no retry to schedule — Neo4j is paying syntax to recover a property GoGraph already has structurally.
   Nothing to take *today*; worth revisiting only if concurrent disjoint writers are ever adopted.

8. **Keep the strict `string + number` rule.** `RETURN 'a' + 1 → null` (`docs/cypher.md:955-973`).
   openCypher leaves the coercion underspecified and its own issue tracker calls the conversion
   "internally inconsistent"; GoGraph pins the strict reading with a regression test. Do not adopt
   Neo4j's implicit coercion.

---

## NOT INVESTIGATED — sub-areas this stream did not reach

Stated plainly so the coordinator does not read silence as a clean bill of health:

- **Bolt-level type/value fidelity for the language surface** — how temporals, durations and entity values
  serialise over PackStream. Round 1 already flagged entities-as-maps; I did not re-verify it, and it is
  the Bolt stream's remit.
- **`ORDER BY` collation and string comparison semantics** — GoGraph's ordering of non-ASCII strings vs
  Neo4j's, and whether either matches openCypher's orderability rules. Not probed.
- **Parameter (`$p`) type coercion and the parameter surface** — only trivially exercised.
- **`SHOW … YIELD/WHERE/RETURN` projection correctness** — the probe showed `SHOW INDEXES` returning
  three rows but rendering `<nil>` for every column via `Result.ValueAt`. That is likely an artefact of
  how I read the result rather than a defect, but I did not chase it down, and it deserves five minutes
  from whoever owns `cypher/show.go`.
- **Cypher 25 clauses `FILTER`, `LET`, `FOR`, `NEXT`, `WHEN/THEN/ELSE`, `FINISH`, `INSERT`,
  `NODETACH DELETE`, `SEARCH`** — confirmed to exist in Neo4j and confirmed absent from GoGraph, but I
  did not analyse their semantics deeply enough to size an implementation. None has openCypher 2024.3
  grammar backing (`grep -ic` = 0 for `FINISH`, `INSERT`, `NEXT`, `LET`), so all are Neo4j-proprietary
  today and correctly sit below the openCypher-normative items in the ranking.
- **Neo4j's `IS LABELED` / `PROPERTY_EXISTS` / dynamic label references `$()`/`$any()`/`$all()`** —
  identified as Cypher-25 additions (2026.04, 2026.03, 2025.07 respectively), not evaluated for GoGraph.
- **APOC / GDS / MAGE surface comparison** — deliberately out of scope: all three are plugin ecosystems,
  and GoGraph is an embeddable library whose users call Go, not procedures.

One residual disagreement worth recording: Neo4j's own pattern *reference* page enumerates only
`ANY k` and `ALL SHORTEST`, not `ANY SHORTEST`, as formal grammar. My inclusion of `ANY SHORTEST` in the
path-selector items rests on an independent Tier-1 source — openCypher 2024.3's BNF explicitly defines
`<any shortest path search> ::= ANY SHORTEST [ <path keywords> ]` (line 320-321) — so the citation stands
on its own regardless of how Neo4j documents it.

## Two round-1 conclusions this stream overturns

1. **"GoGraph's TCK is the frozen ~2017 openCypher 9 TCK."** Measured false. It is **byte-identical to
   openCypher 2024.3**, released 2026-03-20 — the newest TCK that exists. What is frozen is the TCK
   *itself* (4 substantive diff lines since 2023), not GoGraph's copy of it. The real problem is that
   openCypher's GQL alignment lives in the **grammar**, which GoGraph does not track at all.

2. **"LOAD CSV *is* TCK-covered."** Measured false — zero feature files, zero lines, and it is not in
   the openCypher 2024.3 grammar either. LOAD CSV is a vendor extension; prioritise it on utility, not
   on conformance.

And one round-1 conclusion this stream **qualifies**: "GoGraph has the strongest openCypher 9 fidelity."
True at the **execution** layer, where the evidence is 3897/3897 against the current TCK and the named
places Memgraph falls short. Not true at the **acceptance** layer: F1 shows GoGraph accepts input
neither openCypher nor Neo4j accepts, and silently computes something else. Conformance is a
two-sided property — accept what the spec accepts, *and reject what it rejects* — and the second side
is currently unguarded.
