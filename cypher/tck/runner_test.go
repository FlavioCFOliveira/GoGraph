package tck_test

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/cucumber/godog"

	"gograph/cypher/tck"
)

// tckExecutionBaseline is the minimum number of passing scenarios the godog
// execution suite must report. Set just below the most recent observed pass
// count so that legitimate ±5-scenario run-to-run variance does not flap the
// gate, but any real regression in execution support fails CI.
//
// History:
//   - 1000: initial gate.
//   - 1145: raised after task #391 wired EagerAggregation argument/group-by
//     AST expressions and TCK value formatting (observed ≈1152±2 over a
//     5-run sample).
//   - 1225: raised after task #392 wired multi-pattern MATCH binding via
//     CorrelatedApply, OPTIONAL MATCH whole-pattern NULL emission via
//     OptionalApply, destination-rebinding equi-join in
//     matchExpandStepBoundWithFrom, and explicit fromVar threading in
//     matchPathPattern (observed ≈1233
//     over a 3-run sample).
//   - 1230: raised after task #393 fixed the VarLengthExpand BFS slice-
//     aliasing hazard (the read frontier and the write target shared a
//     backing slice, silently overwriting unprocessed entries). Observed
//     1234 across a 5-run sample; the gate is set conservatively at 1230
//     to absorb run-to-run variance.
//   - 1370: raised after task #394 added the temporal value kinds (Date,
//     DateTime, LocalDateTime, LocalTime, Time, Duration), their string and
//     map constructors, ISO-8601 arithmetic, and the SOH-tagged PropString
//     bridge that round-trips temporal property values through snapshot+WAL
//     replay. Observed 1374 across a 3-run sample; the gate is set
//     conservatively at 1370 to absorb run-to-run variance.
//   - 1520: raised after task #395 wired the cypher/sema scope analyser
//     into Engine.Run and Engine.RunInTx as a pre-execution gate that
//     short-circuits with a typed *sema.SemanticError before plan build.
//     The wiring required three companion sema enrichments to keep the
//     existing valid-query suite regression-free: (a) clauses are now
//     walked in [ast.Position] order so interleaved WITH / UpdatingClauses
//     respect the openCypher scope rules; (b) WITH * preserves every
//     in-scope binding; (c) ORDER BY / SKIP / LIMIT and WHERE-on-WITH see
//     projection aliases AND the pre-WITH names. The introducer helpers
//     also detect node↔relationship↔path type conflicts on variable
//     reuse and surface them as SyntaxError.VariableTypeConflict.
//     Observed 1527-1528 across a 3-run sample; the gate is set
//     conservatively at 1520 to absorb run-to-run variance.
//   - 1568: raised after task T930 replaced [exec.Merge]'s stub searchFn
//     with a real label+property scan ([exec.NewMergeSearchFnFromPattern]).
//     MERGE on an existing pattern now fires the ON MATCH branch instead of
//     duplicating the node, unlocking the openCypher MERGE*.feature
//     scenarios that rely on idempotent merge semantics.
//     Observed 1571-1573 across a 3-run sample; the gate is set
//     conservatively at 1568 to absorb run-to-run variance.
//   - 1576: raised after task T937 partial closure — buildRowCtx now
//     reconstructs full [expr.RelationshipValue]s (with typed properties)
//     for every variable in bopts.edgeVarMeta, so r.prop expressions in
//     WHERE/RETURN/UNWIND/Filter resolve against the bound edge instead of
//     evaluating to null.
//     Observed 1578-1580 across a 3-run sample; the gate is set
//     conservatively at 1576 to absorb run-to-run variance.
//   - 1628: raised after task T937 partial closure — formatNodeTCK and
//     formatRelTCK now include the property map in the TCK textual form
//     (`({name: 'bar'})`, `[:KNOWS {since: 2020}]`), matching the format
//     the openCypher TCK feature tables expect for full-node and
//     full-relationship comparisons.
//     Observed 1631-1633 across a 3-run sample; the gate is set
//     conservatively at 1628 to absorb run-to-run variance.
//   - 1654: raised after the temporal duration projection functions
//     ([fnDurationInMonths], [fnDurationInDays], [fnDurationInSeconds])
//     accepted a 2-argument form computing the difference between two
//     temporals (delegating to [fnDurationBetween]) and projecting the
//     result to the requested unit. Previously only the 1-argument form
//     (component extraction from an existing Duration) was supported.
//     Observed 1655-1658 across a 3-run sample; the gate is set
//     conservatively at 1654 to absorb run-to-run variance.
//   - 1766: raised after the five `X.truncate(unit, source [, fields])`
//     functions (date / datetime / localdatetime / time / localtime)
//     landed in cypher/funcs/temporal_truncate.go. Each function
//     truncates the source temporal to the start of `unit`
//     (millennium..nanosecond, including weekYear, quarter, week) and
//     then applies the optional MapValue overrides.
//     Observed 1769-1770 across a 3-run sample; the gate is set
//     conservatively at 1766 to absorb run-to-run variance.
//   - 1815: raised after [Engine.RunAny] auto-detected writing clauses
//     (CREATE, MERGE, SET, REMOVE, DELETE, DETACH) via a regex word-
//     boundary scan and routed them through RunInTx instead of failing
//     in buildPlanEngine with "unsupported IR node *ir.Merge" /
//     *ir.SetProperty / *ir.RemoveProperty when wrapped in a read
//     projection. Project also relaxed to accept an empty items slice
//     for WITH * patterns that bind no variables.
//     Observed 1817-1825 across a 5-run sample; the gate is set
//     conservatively at 1815 to absorb run-to-run variance.
//   - 1856: raised after newResult began draining the underlying
//     ResultSet eagerly when the query has no RETURN clause (cols=nil).
//     Write-only queries (CREATE/SET/DELETE/MERGE/REMOVE without a
//     trailing projection) now execute their side effects and surface
//     an empty result iterator, matching the openCypher TCK contract.
//     Observed 1858-1859 across a 3-run sample; the gate is set
//     conservatively at 1856 to absorb run-to-run variance.
//   - 1876: raised after two semantic fixes landed together:
//     (a) the parser recognises that ANTLR tokenises a float literal
//     such as `1.0` as the three tokens `1`, `.`, `0` and reconstructs
//     a FloatLiteral when an integer atom is followed by a single
//     all-digit property accessor — property keys cannot start with a
//     digit in openCypher, so the rewrite is unambiguous; and
//     (b) IntegerValue.Equal and FloatValue.Equal now treat 1 == 1.0
//     as true (numeric cross-type equality), instead of false.
//     Observed 1879-1882 across a 3-run sample; the gate is set
//     conservatively at 1876 to absorb run-to-run variance.
//   - 1882: raised after two reverse-direction relationship fixes
//     landed together: (a) ir.createRelationship now honours
//     ast.RelDirectionIncoming by swapping startVar/endVar so
//     `CREATE (:A)<-[:R]-(:B)` stores the edge B→A (the openCypher
//     semantic for the leftward arrow); and (b) exec.Expand's
//     tryRevEdge now looks up edge types via a fwd-position lookup of
//     the (dst → src) forward edge, instead of unconditionally
//     skipping reverse edges when an edge-type filter is active.
//     Observed 1885-1889 across a 3-run sample; the gate is set
//     conservatively at 1882 to absorb run-to-run variance.
//   - 1893: raised after task T937 partial closure — temporal map-literal
//     constructors date({year:…}), localdate/time/datetime/datetime({...}),
//     duration({…}) are now evaluated at CreateNode/Relationship time
//     instead of returning a hard error. Each constructor compiles its
//     map literal to the canonical ISO-8601 string via
//     mapFieldsTo<Kind>String and delegates to the existing expr.ParseX.
//     Observed 1896-1897 across a 3-run sample; gate set conservatively
//     at 1893 to absorb run-to-run variance.
//   - 1948: raised after task T937 partial closure — WITH-clause
//     OrderBy/Skip/Limit/Distinct were silently dropped by translateWith
//     (the tail applied only to RETURN). applyProjectionTail now wraps
//     both WITH and RETURN with the same Sort/Top/Skip/Limit/Distinct
//     pipeline. +55 scenarios across with-orderBy, with-skip-limit,
//     pattern-comprehension and aggregation suites. Observed 1951
//     across a 3-run sample; gate set conservatively at 1948.
//   - 1955: raised after task T937 partial closure — parsePropValue
//     recognises the "null" literal and returns ErrPropertyValueIsNull;
//     the property-map iterators in parsePropLiteral and
//     parsePropLiteralWithParams skip null entries entirely, matching
//     openCypher CREATE-with-null semantics (the property is not set).
//     Observed 1958 across a 3-run sample.
//   - 1961: raised after task T937 partial closure — buildPlanEngine
//     now accepts *ir.Union and *ir.UnionAll as plan roots and dispatches
//     into each branch recursively. Replaces the "plan root must be
//     ProduceResults, got *ir.Union" hard error with a proper
//     concatenation operator (exec.NewUnion / exec.NewUnionAll).
//     Observed 1964 across a 3-run sample.
//   - 1962: raised after task T937 partial closure — ProcedureCallOp now
//     implements void-procedure passthrough: when the procedure declares
//     no output columns and the impl returns no rows, the driver row is
//     emitted unchanged so downstream operators still see the upstream
//     variable bindings. Previously the loop consumed the driver row
//     silently, breaking the canonical
//     MATCH … CALL <void-proc> RETURN pattern (a side-effect-only step
//     should not erase upstream bindings). Observed 1963 across a 3-run
//     sample; gate set conservatively at 1962.
//   - 1978: raised after task T937 partial closure — three CALL-pipeline
//     enhancements landed together: (a) buildProcedureCallOperator's
//     argEval now recognises primitive literals (quoted strings,
//     integers, floats, booleans, null) via buildProcArgEvaluator +
//     parseProcArgLiteral instead of treating every IR argument as a
//     variable reference (which silently resolved to NULL); (b) the
//     Engine exposes its procs.Registry via the new (*Engine).Procs()
//     accessor; (c) the TCK runner's `there exists a procedure` Gherkin
//     step now parses the signature (with a depth-aware `::` separator
//     that no longer trips on the input-type annotations) and the
//     accompanying table to register a table-driven impl on the
//     scenario's engine. Together these unlock the test.doNothing,
//     test.labels and test.my.proc fixture scenarios across
//     features/clauses/call/Call*. Observed 1980 across a 3-run sample;
//     gate set conservatively at 1978.
//   - 1981: raised after task T937 partial closure — *ir.RollUpApply
//     dispatch now wired into buildOperator: the existing
//     exec.RollUpApply operator is allocated with the routed Argument
//     leaf, and the comprehension's CollectVar is registered in the
//     schema at the outer-width position (inner-only schema entries
//     are pruned post-build because they are not visible downstream).
//     pattern_comprehension.go now also (i) tags the inner Argument
//     for tag-routed lookup, (ii) carries the projection AST via
//     ProjectionItem.Expr so b.name evaluates via expr.Eval, and
//     (iii) passes the comprehension forward as a regular projection
//     item so the final RETURN includes the collected list column.
//     Observed 1979-1983 across an 8-run sample; gate set
//     conservatively at 1979 to absorb the wider run-to-run variance.
//   - 1984: raised after task T937 partial closure — two parser-level
//     fixes for scientific-notation float literals: (a) VisitAtom now
//     reinterprets a Symbol token of shape `<digits>[eE][+-]?<digits>`
//     as a FloatLiteral via looksLikeExponentFloat before falling
//     through to Variable creation (`1e9`, `1E9`, `2e-3` no longer
//     parse as undefined variables); (b) the propertyExpression float
//     reconstruction (which already handled the `1.0 → IntLiteral{1} +
//     Name{"0"}` split) now also accepts the `0e9`-style fractional
//     part so `1.0e9` reconstructs correctly via fmt.Sprintf+ParseFloat.
//     Together these unlock features/expressions/literals/Literals5
//     scientific-notation scenarios. Observed 1986 across a 5-run
//     sample.
//   - 1985: raised after task T937 partial closure — evalSubscript now
//     handles NodeValue and RelationshipValue subscript access:
//     n['name'] and r['since'] return the property value instead of
//     NULL. Refactored into per-container helpers (subscriptList /
//     subscriptMap) to stay under the gocyclo:15 budget. Unlocks
//     features/expressions/graph/Graph7 (dynamic property access).
//     Observed 1985 stable across a 5-run sample.
//   - 2022: raised after task T937 partial closure — formatNanosToTime
//     now elides the seconds field when both seconds and nanoseconds
//     are zero (`12:00` instead of `12:00:00`) — the shortest
//     openCypher textual representation that round-trips. Unlocks
//     features/expressions/temporal/Temporal1, Temporal3 and many
//     temporal-projection scenarios. Observed 2024 across a 5-run
//     sample; gate set conservatively at 2022.
//   - 2179: raised after task T937 partial closure — formatLocalDateTime
//     and formatDateTime extend the same :00-seconds elision through a
//     shared formatHMSNanos helper. `1984-10-11T12:00` and
//     `1984-10-11T12:00+01:00` now produce the canonical shortest form
//     instead of the spurious `:00:00` trailer. Unlocks
//     features/expressions/temporal/Temporal* projections that surface
//     localdatetime/datetime values at the top of the hour or minute.
//     Observed 2181 across a 3-run sample; gate set conservatively at
//     2179.
//   - 2240: raised after task T937 partial closure — fnDurationBetween
//     now accepts mixed-kind inputs. Two date-bearing values (Date,
//     LocalDateTime, DateTime in any combination) project to
//     LocalDateTime and subtract via the wall clock; time-only pairs
//     subtract on the nanosecond-of-day axis; time-only ↔ date-bearing
//     pairs project the date-bearing side to its time-of-day component
//     (the date is dropped, matching the openCypher rule that
//     duration.between with a time-only argument is defined on the
//     time-of-day axis only). Same-kind delegation to the existing
//     Sub* helpers is preserved verbatim. Unlocks
//     features/expressions/temporal/Temporal10 mixed-kind scenarios.
//     Observed 2242 across a 3-run sample; gate set conservatively at
//     2240.
//   - 2258: raised after task T937 partial closure — two coupled fixes:
//     (a) compareValues now falls through to compareSameKind for
//     same-kind Date/LocalDateTime/DateTime/LocalTime/Time/Duration
//     pairs so `<`, `<=`, `>`, `>=` no longer collapse temporal
//     comparisons to NULL; (b) projection-item column headers are
//     computed by a shared projectionColumnName helper used by both
//     projectionItems and projectionsWithComprehensions — for
//     *ast.BinaryOp the outermost parens (which BinaryOp.String()
//     adds for unambiguous re-parsing) are stripped so `RETURN x > d`
//     surfaces the canonical openCypher header `x > d` instead of
//     `(x > d)`. Together these unlock features/expressions/temporal/
//     Temporal7 comparison scenarios across all five temporal kinds.
//     Observed 2260 across a 3-run sample; gate set conservatively at
//     2258.
//   - 2265: raised after task T937 partial closure — evalSlice now
//     propagates NULL when an explicitly-written bound evaluates to
//     NULL (`list[1..null]`, `list[null..3]`, `list[null..null]` all
//     return NULL). The previous implementation silently substituted
//     the default (0 / len(list)) for an evaluated-NULL bound, which
//     yielded the full or partial list instead of NULL. Absent bounds
//     (e.g. `list[..3]`) keep the default-substitution behaviour
//     unchanged. Observed 2262-2266 across an 8-run sample;
//     subsequently lowered to 2260 to absorb the wider variance from
//     the comprehension-driven scenarios.
//   - 2264: raised after task T937 partial closure — ListValue.Equal
//     now implements openCypher 3VL semantics correctly: a single
//     FALSE element comparison short-circuits to FALSE (overrides any
//     earlier NULLs), a mix of TRUEs and NULLs yields NULL, and only
//     all-TRUE yields TRUE. Previously the first NULL element
//     short-circuited the whole list to NULL even when a later
//     element would have produced FALSE. Unlocks features/expressions/
//     list/List3 equality-with-null scenarios. Observed 2266-2269
//     across a 5-run sample.
//   - 2268: raised after task T937 partial closure — the list
//     quantifiers all/any/none/single now partition predicate outcomes
//     into a (trueCount, falseCount, nullCount) tuple instead of the
//     binary (trueCount, sawNull) form. quantifierResult applies the
//     canonical openCypher 3VL rule: a definitive FALSE wins over any
//     NULLs for all/none; a definitive TRUE wins for any/single;
//     otherwise NULL when any null was seen. Previously the all
//     quantifier returned FALSE for an all-null predicate because the
//     `total - trueCount > 0` test conflated falses and nulls. Unlocks
//     features/expressions/quantifier/Quantifier4 [10] all-with-nulls
//     scenarios. Observed 2269-2272 across a 5-run sample.
//   - 2273: raised after task T937 partial closure — precedence-aware
//     column-header builder exprToColumnName replaces the previous
//     "strip outermost parens only" heuristic. Nested arithmetic and
//     boolean expressions like `12 / 4 * 3 - 2 * 4` now produce the
//     unparenthesised canonical header instead of
//     `((12 / 4) * 3) - (2 * 4)`. Operator precedence and
//     left-associativity drive paren-guarding so non-associative
//     siblings (e.g. `a - (b - c)`) still parenthesise correctly.
//     Unlocks features/expressions/mathematical/Mathematical8 and
//     similar precedence scenarios. Observed 2274-2275 across a 5-run
//     sample.
//   - 2308: raised after Audit Cycle 4 (Sprint 82) — two fixes:
//     (a) T957: list literal support in property map parser for CREATE/MERGE
//     ({seasons: [1,2,3,4,5,6,7]}, {list: ['A','B']}, etc.) backed by new
//     lpg.PropList (kind 7) with snapshot/WAL/recovery encode-decode.
//     (b) T958: toString() extended to accept all six openCypher temporal
//     kinds (DATE, DATETIME, LOCALDATETIME, TIME, LOCALTIME, DURATION)
//     returning their canonical ISO-8601 string representation.
//     Net uplift: +35 scenarios. Observed 2305-2308 across a 5-run sample.
//   - 2311: raised after T959+T960 fixes:
//     (a) T959: SetProperty/RemoveProperty NodeID-0 resolution — row bindings
//     that arrive as NodeValue or RelationshipValue are now resolved to their
//     underlying NodeID/edge key, not rejected as non-IntegerValue.
//     (b) T960: runtime evaluation of non-literal property maps in CREATE/MERGE.
//     Expressions like {num: x} or {val: a.id} are now evaluated per-row via a
//     PropsEvalFn closure (buildPropsEvalFn in api.go) instead of failing at
//     plan-construction time. parsePropLiteralDeferred silently defers non-literal
//     values; the physical builder installs the closure when PropertiesExpr is
//     non-nil on the ir.CreateNode/ir.CreateRelationship node.
//     Net uplift: +6 scenarios. Observed 2308-2311 across a 6-run sample.
//     Baseline set conservatively at 2306 to absorb run-to-run variance.
//   - 2378: raised after task T962 — ORDER BY clauses inside WITH are now
//     scope-checked against the merged pre-WITH scope (original names plus
//     new projection aliases), matching the same scope that WHERE-on-WITH
//     already used. Any variable referenced in ORDER BY that is not visible
//     in that merged scope now raises a KindUndefinedVar error, which surfaces
//     as SyntaxError(UndefinedVariable) to callers. The fix is a one-site
//     addition in sema.withClause, symmetric with the existing WHERE check.
//     Unlocks: WithOrderBy1[46] (10 examples), WithOrderBy3[8] (40+ examples)
//     and other scenarios referencing variables dropped by a prior WITH.
//     Net uplift: +72 scenarios. Observed 2382 stable across a 2-run sample;
//     baseline set conservatively at 2378 to absorb run-to-run variance.
//   - 2440: raised after task T961 — pattern predicates in WHERE clauses are
//     now evaluated as existential checks against the live graph. Adds a
//     PatternEvaluator interface and patternEvaluator implementation that walks
//     the LPG adjacency list per row. Supports single-hop directed/undirected/
//     typed patterns (WHERE (a)-[:T]->(b)) and variable-length BFS patterns
//     (WHERE (a)-[:T*]->(b)). Unlocks: expressions/pattern/Pattern1 (scenarios
//     1-9, 12-21), clauses/match-where, and other WHERE-predicate scenarios.
//     Net uplift: +69 scenarios above 2378. Observed 2447-2485 across a 4-run
//     sample; baseline set conservatively at 2440 to absorb run-to-run variance.
//   - 2550: raised after task T940 — ORDER BY / SKIP / LIMIT result-pipeline
//     wiring. Fixes: (1) irSortKeys schema resolution (expr string + AST eval
//     closure fallback), (2) count(*) column name (FunctionInvocation.CountStar
//     flag → "count(*)" instead of "count()"), (3) TCK property-map key
//     normalisation (spaces around colons), (4) TCK column whitespace collapse
//     for headers like "cOuNt( * )", (5) parseCypherLiteral now handles list
//     and map parameter literals. Net uplift: +118 scenarios above 2440.
//     Observed 2558 stable; baseline set conservatively at 2550 to absorb
//     run-to-run variance.
//   - 2860: raised after Sprint 84 audit round 7 — four execution-level
//     fixes landed together: (a) T964 formatFloatTCK falls back to %f
//     when %g produces scientific notation, so float properties render
//     as 0.00002 instead of 2e-05 (matches TCK tables); (b) T965
//     dateFromMap accepts ordinalDay (preferred per openCypher) and
//     dayOfYear (legacy alias), inherits ISO week-year and ISO
//     day-of-week from the base date when {date:..,week:N} omits
//     year/dayOfWeek, preserves month-within-quarter offset from base
//     for {date:..,quarter:N}, and accepts LocalDateTime/DateTime as
//     base via DateFromTime extraction; (c) T966 truncateUnit splits
//     'year' from 'weekYear' (weekYear computes Monday of ISO week 1
//     of the source's ISO year, handling 1984-01-01 → 1983-01-03
//     boundary), and applyOverrides handles the dayOfWeek key by
//     adjusting the reconstructed date by (target - current) weekday
//     difference; (d) T963 sema/analyse.go detects AND/OR/XOR/NOT on
//     statically-typed non-boolean literals (Int/Float/String/List/Map)
//     and emits SyntaxError(InvalidArgumentType) at compile time.
//     Net uplift: +225 scenarios above 2638. Observed 2869 across a
//     3-run sample; gate set conservatively at 2860 to absorb
//     run-to-run variance.
//   - 2985: raised after Sprint 84 audit round 8 — three temporal-stack
//     fixes landed together: (a) T967 timeComponentsFromMap now accepts
//     a {time: <baseValue>} key (LocalTime/Time/LocalDateTime/DateTime)
//     and inherits hour/minute/second/nanosecond before explicit
//     overrides apply; zoneFromMap inherits the offset from a {time}
//     or {datetime} base when no explicit timezone key is given;
//     time()'s map branch likewise inherits the base offset. (b) The
//     applyOverrides function in temporal_truncate.go now decomposes
//     the source's nanosecond-of-second into hierarchical (millisecond-
//     of-second, microsecond-of-millisecond, nanosecond-of-microsecond)
//     components, so an override such as `{nanosecond: 2}` after
//     truncate('millisecond', src) preserves the truncated ms value
//     and just sets the sub-microsecond component (.645000002 instead
//     of the previous .000000002). (c) ParseDateTime / parseOffset
//     now silently strip an optional trailing [IANA/Zone] bracket
//     suffix and, when present and resolvable via time.LoadLocation,
//     honour the named zone instead of the fixed-offset fallback —
//     letting `datetime('2017-10-28T23:00+02:00[Europe/Stockholm]')`
//     parse correctly. Net uplift: +126 scenarios above 2860. Observed
//     2995 across a 3-run sample; gate set conservatively at 2985.
//   - 3045: raised after Sprint 84 audit round 9 — the TCK error
//     assertions (`assertError`, `assertSyntaxError`) now drain the
//     pending lazy result when no error was recorded at execution
//     time, so per-row eval errors surface against the assertion.
//     This unlocks scenarios where the failure is in a row-producing
//     expression (e.g. `RETURN range(0, 0, 0)`, list-index type
//     mismatches, NumberOutOfRange) and the eager-eval path otherwise
//     misses them. Net uplift: +60 scenarios above 2985. Observed
//     3055 across a 3-run sample; gate set conservatively at 3045.
//   - 3055: raised after Sprint 84 audit round 9 follow-on — three
//     coordinated fixes: (a) toInteger("1.7") now falls back to a
//     ParseFloat → math.Trunc path when the integer parse fails, so
//     toInteger of a float-formatted string yields 1 instead of NULL;
//     (b) SubTimes normalises both operands to UTC by subtracting the
//     fixed offset before comparing, so duration.between(time('14:30'),
//     time('16:30+0100')) yields PT1H instead of PT2H; (c) SubDates,
//     SubLocalDateTimes, SubDateTimes decompose their result into
//     calendar-anchored (months, days) plus the wall-clock remainder,
//     producing canonical PnYnMnDTnHnMnS strings; (d) toLocalDateTime
//     for DateTime now re-anchors the wall-clock components in UTC
//     without converting the instant, preserving the local
//     hour/minute/second the duration calculation expects. Net uplift:
//     +9 scenarios above 3055. Observed 3064 across a 3-run sample;
//     gate set conservatively at 3055.
//   - 3065: raised after Sprint 84 audit round 9 follow-on 2 —
//     applyProjectionTail in cypher/ir/with.go reordered to wrap the
//     plan in the canonical openCypher order: DISTINCT → ORDER BY →
//     SKIP → LIMIT. Previously SKIP was applied before ORDER BY,
//     which dropped unsorted rows from the head of the stream before
//     the sort had a chance to order them. Sort+Limit still fuses
//     into Top, but only when SKIP is absent (otherwise Top would
//     discard rows that SKIP should later reveal). Unlocks
//     ReturnSkipLimit1 [1], WithSkipLimit2 [1], and similar
//     ORDER-BY-then-SKIP scenarios. Net uplift: +9 scenarios above
//     3064. Observed 3073 across a 3-run sample; gate set
//     conservatively at 3065.
//   - 3070: raised after Sprint 84 audit round 9 follow-on 3 — Time
//     value comparison in compareSameKind now anchors the comparison
//     on the UTC instant (Nanos − OffsetSec * nsPerSec) so two Time
//     values with different fixed offsets sort by their absolute time
//     instead of their local wall-clock. Raw OffsetSec is retained as
//     a tie-breaker for otherwise-equal instants. Unlocks Sort-by-Time
//     scenarios in WithOrderBy1/2 and similar features. Net uplift:
//     +7 scenarios above 3073. Observed 3080 across a 3-run sample;
//     gate set conservatively at 3070.
//   - 3098: raised after Sprint 84 audit round 9 follow-on 4 —
//     compareSameKind for KindLocalDateTime and KindDateTime now uses
//     t.Compare() instead of t.UnixNano(). UnixNano overflows int64
//     for year 0001 (~6.2e19 ns before epoch) and year 9999
//     (~2.5e20 ns after epoch), producing garbage comparisons that
//     broke ORDER BY against the TCK's extreme-year scenarios.
//     t.Compare is documented to work across all valid time.Time
//     values without overflow. Net uplift: +27 scenarios above 3080.
//     Observed 3107 across a 3-run sample; gate set conservatively
//     at 3098.
//   - 3100: raised after Sprint 84 audit round 9 follow-on 5 —
//     kindOrder re-ordered to match the openCypher 9 cross-type
//     sort order: Map(0) < Node(1) < Relationship(2) < List(3) <
//     Path(4) < String(5) < Boolean(6) < Float(7) < Integer(8) ...
//     Previously Path was first and Map was fourth, which contradicted
//     the order asserted by the WithOrderBy1 [21]/[22] "Sort distinct
//     types" scenarios. Net uplift: +1 scenario above 3107. Observed
//     3108 across a 3-run sample; gate set conservatively at 3100.
//   - 3105: raised after Sprint 84 audit round 9 follow-on 6 —
//     sema/analyse.go checkExpr now flags `RETURN <expr> IN <literal>`
//     where the RHS is a statically-known non-list literal (Integer,
//     Float, String, Boolean, Map) as SyntaxError(InvalidArgumentType)
//     at compile time. Variables, parameters and ListLiteral RHS
//     remain unchecked. The new nonListLiteralKind helper mirrors the
//     existing nonBooleanLiteralKind classifier. Net uplift: +1
//     scenario above 3108. Observed 3109 across a 3-run sample;
//     gate set conservatively at 3105.
//   - 3110: raised after Sprint 84 audit round 9 follow-on 7 —
//     compareValues in cypher/expr/eval.go now compares two ListValues
//     lexicographically with openCypher 3-valued logic via the new
//     compareListWith3VL helper. Previously list-vs-list ordering
//     fell through to "incompatible types" and evalOrdering returned
//     NULL, so `[1, 0] >= [1]` evaluated to NULL instead of TRUE.
//     The new helper short-circuits on a definitive non-equal element
//     and collapses to NULL when only NULL elements differentiate the
//     two lists. Net uplift: +7 scenarios above 3109. Observed 3116
//     across a 3-run sample; gate set conservatively at 3110.
//   - 3120: raised after Sprint 85 (gate raised from 80%→95%) audit
//     round 10 — label predicate expression `(n:Foo:Bar)` now parses
//     into a dedicated ast.LabelPredicate node and evaluates to
//     TRUE / FALSE / NULL per openCypher 3VL semantics
//     (NULL receiver → NULL; non-Node receiver → NULL; otherwise
//     conjunctive label test). Previously parser dropped the labels
//     from PropertyOrLabelExpression so `RETURN (n:Foo)` evaluated as
//     just `n`. Net uplift: +10 scenarios above 3118. Observed 3125-
//     3129 across a 3-run sample; gate set conservatively at 3120.
//   - 3175: raised after Sprint 85 audit round 10 follow-on 1 — four
//     sema enhancements landed together: (a) new KindInvalidAggregation
//     ErrorKind + checkOrderByAggregation surface SyntaxError
//     (InvalidAggregation) when ORDER BY references aggregations and
//     the surrounding projection does not aggregate itself; the new
//     containsAggregation classifier covers count/sum/avg/min/max/
//     collect/stdev/percentile{Cont,Disc}. (b) Bare WHERE pattern
//     predicates (e.g. `WHERE (a)-[r]->(b)`) now use a pure-reference
//     check (pathPatternRefCheck): variables that are not already
//     bound surface UndefinedVariable instead of being silently
//     introduced. (c) WITH/RETURN projection aliases inherit a coarse
//     static type via inferProjectedType — non-graph literals
//     (Int/Float/String/Bool/List/Map) produce a "value" type that
//     conflicts with later `MATCH (n)` introductions, raising
//     VariableTypeConflict. (d) Direct literal property access
//     `RETURN 1.foo` now raises InvalidArgumentType at compile time.
//     Net uplift: +56 scenarios above 3120. Observed 3184 across a
//     3-run sample; gate set conservatively at 3175.
//   - 3200: raised after Sprint 85 audit round 10 follow-on 2 — two
//     property-store fixes for temporal values. (a) cypher/api.go's
//     exprValueToLPGProp now encodes the six temporal expr.Value kinds
//     (Date, LocalDateTime, DateTime, LocalTime, Time, Duration) as
//     SOH-tagged PropString matching the literal-write encoding, so
//     `CREATE ({dates: [date({...})]})` stores the temporal element
//     correctly instead of dropping it (the previous default branch
//     silently skipped temporal values inside an evaluated list, so
//     the round-trip retrieval returned an empty list). (b) The
//     clock-source aliases `<kind>.transaction`, `<kind>.statement`,
//     `<kind>.realtime` (for date, localtime, time, localdatetime,
//     datetime) are now registered as aliases for the 0-arg
//     constructor — in a single-process engine the three clock
//     variants are indistinguishable from time.Now(). Net uplift: +24
//     scenarios above 3184. Observed 3208 across a 3-run sample; gate
//     set conservatively at 3200.
//   - 3205: raised after Sprint 85 audit round 10 follow-on 3 — two
//     execution-side fixes for variable forwarding: (a)
//     CreateRelationship's resolveNodeID now accepts both
//     IntegerValue (the canonical exec encoding) and NodeValue (the
//     form a projection alias carries after `WITH n AS a`) — so
//     `MATCH (n) MATCH (m) WITH n AS a, m AS b CREATE (a)-[:T]->(b)`
//     no longer errors with "variable a is not an IntegerValue"; (b)
//     null endpoint (typically from OPTIONAL MATCH that produced no
//     binding) is signalled via a sentinel error and the operator
//     propagates the row unchanged (with the optional relationship
//     variable bound to NULL) instead of failing. Net uplift: +2
//     scenarios above 3208. Observed 3210 stable across runs; gate
//     set conservatively at 3205.
//   - 3215: raised after the named-path uplift for zero- and
//     fixed-length patterns. The IR translator now wraps every named
//     path that is not a variable-length expansion with a new
//     [ir.NamedPath] pass-through operator carrying the explicit
//     alternating node/rel chain; the physical builder consumes the
//     chain at build time to map each step to the (srcID, edgeID,
//     dstID) triplet emitted by the underlying Expand, and the
//     projection fast path reconstructs an expr.PathValue from those
//     triplets. The TCK comparator gained a formatPathTCK helper that
//     renders a path in `<n0 r0 n1 ... >` form with the relationship
//     direction (-[:T]-> vs <-[:T]-) inferred from each rel's storage
//     StartID/EndID; the chain closure resolves storage direction by
//     probing EdgeLabels in both orientations so the relationship's
//     Type, Properties, and arrow render correctly for both directed
//     and undirected patterns. Net uplift: +11-19 scenarios above
//     3209. Observed 3220-3228 across a 5-run sample; gate set
//     conservatively at 3215 (minimum observed - 5).
//   - 3235: raised after the SET entity-replace / map-merge property
//     semantics uplift. The IR translator now emits a dedicated
//     [ir.SetAllProperties] node for the whole-entity SET forms
//     (`SET n = …` and `SET n += …`), capturing the source as either
//     a bound entity reference, a literal map, or a parameter name.
//     The exec layer's matching [exec.SetAllProperties] operator
//     resolves the target and source bindings per row, snapshots the
//     source's properties via GraphMutator.NodeProperties /
//     EdgeProperties, and writes them to the target with replace or
//     merge semantics. The GraphMutator interface gained
//     EdgeProperties so relationship-copy can read its source. Single-
//     property SET with a null RHS now deletes the property rather
//     than no-op'ing, matching openCypher's null-as-deletion rule.
//     Net uplift: +6-14 scenarios above 3228. Observed 3234-3243
//     across an 8-run sample (some under -race showing the lower
//     end); gate set conservatively at 3229 (minimum observed - 5).
//   - 3260: raised after the undirected self-loop MATCH dedup +
//     type(r) projection fix. The Expand operator now skips its
//     reverse-edge pass when the edge is a self-loop on the current
//     source node under DirBoth, so that openCypher's "each matched
//     edge appears exactly once for an undirected pattern" rule is
//     honoured for self-loops; the analogous skip lives in
//     VarLengthExpand's BFS enqueue path. The downstream Projection's
//     schema-name fast path carves out aliases that map to a bound
//     relationship variable (carried in bopts.edgeVarMeta), so
//     `RETURN type(r) AS r` no longer bypasses evaluation of
//     `type(r)` and returns the relationship type label instead of
//     the raw edge id IntegerValue. Net uplift: +45-49 scenarios
//     above 3226. Observed 3265-3274 across a 7-run sample (one
//     -race outlier at 3265); gate set conservatively at 3260
//     (minimum observed - 5).
//   - 3493: raised after task T986 — cumulative outer-scope variable
//     resolution. Several IR operators (Expand, OptionalExpand,
//     VarLengthExpand) report only their own (RelVar, ToVar) additions
//     from [LogicalPlan.Vars]; a non-recursive `child.Vars()` therefore
//     missed leading-bound nodes (e.g. the NodeByLabelScan beneath a
//     chain of Expands). [newOptionalInnerCtx] and the
//     [matchPattern] boundVars seed now use the new [collectAllVars]
//     helper that walks the whole plan subtree, so OPTIONAL MATCH and
//     multi-pattern MATCH correctly classify the leading node of a
//     subsequent path as "shared with outer" when only its child plan
//     introduced it. Pre-fix the canonical TriadicSelection [11] shape
//     translated to `OptionalApply{outer, Selection{Apply{Argument,
//     Expand→AllNodesScan}}}` (plain Apply with fresh AllNodesScan
//     instead of correlated Expand on top of Argument), so `a` was
//     re-scanned across all nodes and the destination-rebinding
//     equality never satisfied for the canonical row. Post-fix the
//     plan is the expected `OptionalApply{outer,
//     Selection{Expand→Argument}}` and the existing destRebinding
//     equality resolves correctly. Net uplift: +2 to +4 scenarios above
//     3494-3496 pre-fix band. Observed 3498 stable across a 3-run
//     sample; gate set conservatively at 3493 (minimum observed - 5).
//   - 3490: lowered after task T986 follow-on — matchExpandStepBoundWithFrom
//     now applies inline relationship property predicates from
//     RelationshipPattern.Properties via the new [matchApplyRelFilter]
//     helper, so `MATCH (a)-[:T {k: v}]->(b)` correctly filters edges by
//     property value (pre-fix the inline rel property was silently
//     dropped from the plan). Match2 [5] now passes stably. The wider
//     observation window also exposed pre-existing run-to-run flakes
//     (Map3 [1], Merge1 [10], Match7 [29], Set3 [7], MatchWhere1 [11])
//     that bounce in/out of the failure set across runs due to Go map
//     iteration order and aggregation-of-non-grouped-expression
//     non-determinism. Observed 3495-3501 across a 10-run sample (median
//     3497); gate lowered to 3490 (minimum observed - 5) to absorb the
//     wider variance band while still locking in the deterministic +2
//     uplift the T986 + rel-property fixes deliver over the pre-fix
//     3494-3496 band.
//   - 3494: raised after sema function-argument type-check follow-on —
//     checkFunctionArgTypes (defined but previously never called) is
//     now wired into checkExpr's *ast.FunctionInvocation arm and
//     restructured from a permissive must-be-set classifier to a
//     reject-set classifier that only flags Variable args whose static
//     symbol type is definitively wrong (so projection-alias variables
//     with "value" / "any" / "" types fall through unchecked).
//     Coverage extended to length() and size() with graph-element
//     receivers. Unlocks the compile-time InvalidArgumentType
//     expectations for type() on node (Graph4 [7]), labels() on path
//     (Graph3 [8]), length() on node (Path3 [2]), length() on
//     relationship (Path3 [3]), size() on path (List6 [5]) — +5
//     deterministic scenarios. Observed 3499-3503 across a 5-run sample
//     (median 3501); gate set conservatively at 3494 (minimum observed - 5).
//   - 3503: raised after exprToColumnName postfix-unary fix —
//     `IS NULL` / `IS NOT NULL` render as `<operand> <op>` (postfix)
//     instead of `<op><operand>` (prefix), and `NOT` keeps a separating
//     space; previously every UnaryOp prepended the operator to the
//     operand, producing column headers like `IS NULLn.missing` instead
//     of `n.missing IS NULL`. The values themselves were computed
//     correctly, but the TCK comparator matched expected vs actual rows
//     by column header, so the header mismatch surfaced as `[null …]`
//     entries for every expected column. Unlocks the Null1/Null2 family
//     and the broader set of scenarios projecting `<x> IS [NOT] NULL`.
//     Observed 3508-3510 across a 5-run sample (median 3509); gate set
//     conservatively at 3503 (minimum observed - 5).
//   - 3515: raised after translateWith pre-projection WHERE fix —
//     openCypher 9 §5.1.5 specifies that `WITH … WHERE` filters the
//     pre-projection row stream so the predicate can reference both the
//     pre-WITH variables (potentially dropped by the projection) and
//     any new aliases introduced by the projection. Implementation:
//     translateWith now applies the WHERE Selection to `child` BEFORE
//     building the EagerAggregation/Projection, with a new
//     [rewriteWithProjectionAliases] AST rewriter that substitutes any
//     reference to a new projection alias with the alias's source
//     expression so it evaluates against the pre-projection row. Pre-fix
//     the Selection sat ABOVE Projection so references to dropped
//     pre-WITH variables resolved to NULL and the filter let everything
//     through (or no rows for an unrelated reason). Unlocks
//     WithWhere1 [3]/[4], WithWhere7 [1]/[2]/[3] and the broader
//     ExistentialSubquery / aggregation-after-WITH families that depend
//     on pre-WITH scope visibility. Observed 3520-3524 across a 5-run
//     sample (median 3523); gate set conservatively at 3515 (minimum
//     observed - 5).
//   - 3520: raised after exec.Expand.advanceInput now accepts a
//     NodeValue (in addition to the canonical IntegerValue) as the
//     source-column value, so a projected node variable forwarded
//     through a WITH (`MATCH (a:A) WITH a MATCH (a)-->(b)`) seeds the
//     adjacency walk correctly. Pre-fix the projection-output NodeValue
//     failed the input-col type assertion and the Expand silently
//     emitted zero rows. Unlocks With1 [1]/[2] and the broader WITH-
//     forwarding family. Observed 3525-3530 across a 5-run sample
//     (median 3529); gate set conservatively at 3520 (minimum
//     observed - 5).
//   - 3539: raised after projectionItems now honours [ast.Projection.All]
//     and prepends every variable introduced by the child subtree
//     (collected via [collectAllVars]) to the projection. Pre-fix
//     `RETURN *` / `WITH *` translated to a Projection with zero items,
//     so the output had no columns and any row count check failed.
//     Both [translateWith] and [returnClause] now pass child.Vars
//     through the helper. Unlocks With1 [1]/[2], Return7 [1] and the
//     broader RETURN * / WITH * forwarding family. Observed 3544-3548
//     across a 5-run sample (median 3546); gate set conservatively at
//     3539 (minimum observed - 5).
//   - 3544: raised after EagerAggregation now emits one row for an
//     empty pure aggregation (openCypher 9 §3.6: `RETURN count(x)` over
//     empty input returns 0, not zero rows) and after detectAggregation
//     learned to recurse into expression wrappers via the new
//     [containsAggregate] / [extractAggregatesFromExpr] helpers. Items
//     like `$age + avg(x.age) - 1000` now register the nested avg() as
//     a synthetic __agg_N column on the EagerAggregation output and
//     the projection re-evaluates the wrapping arithmetic against the
//     produced row. Pre-fix any aggregate nested inside arithmetic
//     skipped EagerAggregation entirely and produced zero rows. Net
//     +5 to +7 scenarios. Observed 3549-3555 across a 5-run sample
//     (median 3553); gate set conservatively at 3544 (minimum
//     observed - 5).
//   - 3549: raised after exec.Expand.passesFilter (and the symmetric
//     reverseEdgePassesFilter) now decide acceptance by membership in
//     the EdgeTypeFilter map rather than by comparing the looked-up
//     type label against a single op.edgeType. The filter map is
//     populated by api.go::buildEdgeTypeFilter to contain only edges
//     of accepted types, so membership is the correct criterion for
//     both single-type (`[r:KNOWS]`) and multi-type (`[r:KNOWS|HATES]`)
//     patterns. Pre-fix the predicate silently excluded edges of every
//     accepted type other than the first (api.go records relTypes[0]
//     as edgeType), so multi-type relationship patterns only returned
//     matches for the first type. Observed 3554-3559 across a 5-run
//     sample (median 3556); gate set conservatively at 3549.
//   - 3549 (kept): translateWith now distinguishes the two WHERE-on-WITH
//     positions per openCypher semantics: when the projection contains
//     NO aggregates, WHERE filters pre-projection so it can reference
//     pre-WITH variables (5.1.5 — unchanged from the earlier fix); when
//     the projection DOES contain aggregates, WHERE behaves as SQL
//     HAVING and applies AFTER the EagerAggregation+Projection so it
//     can reference aggregate aliases. Pre-fix the unconditional
//     pre-projection move broke `WITH a, count(*) AS c WHERE c > 1`
//     because the alias-rewrite pass substituted c with count(*), which
//     cannot evaluate row-by-row upstream of the aggregator. Observed
//     3554-3561 across a 5-run sample (median 3559); baseline kept at
//     3549 since the min observed only moved by zero.
//   - 3555: raised after standalone-CALL output-column auto-yield fix —
//     when a `CALL ns.name(args)` query has no explicit YIELD, the
//     buildPlanEngine standalone-CALL branch now looks up the
//     procedure's declared output column names from procReg and uses
//     them as the result columns. Pre-fix it returned p.YieldVars
//     verbatim (always [] for no-YIELD), so standalone CALLs produced
//     zero columns and the TCK row-count check saw the result as
//     empty. The buildProcedureCallOperator branch already had the
//     same auto-yield logic for the in-query CALL path; the standalone
//     path simply did not consult the registry. Cross-type identity
//     equality (Node/Relationship/Integer) on top of this yielded the
//     net +4-7 deterministic uplift. Observed 3560-3566 across a 5-run
//     sample (median 3564); gate set conservatively at 3555 (minimum
//     observed - 5).
//   - 3564: raised after OPTIONAL MATCH at query start now always wraps
//     in an OptionalApply over a singleton Argument seed (regardless
//     of whether the pattern contains relationships). openCypher 9
//     §3.2.4 requires at least one NULL-extended row per driving
//     outer row; pre-fix the relationship-bearing branch returned the
//     inner plan unwrapped so `OPTIONAL MATCH ()-[r]->()` on an empty
//     graph produced zero rows instead of one. The inner pattern now
//     uses regular Expand (the OptionalApply provides the NULL
//     emission). Standalone-CALL output-column auto-yield + TCK proc
//     declaration NUMBER/ANY kind parsing landed on top of this for
//     additional uplift. Observed 3569-3572 across a 5-run sample
//     (median 3571); gate set conservatively at 3564 (minimum
//     observed - 5).
//   - 3575: raised after SKIP/LIMIT parameter resolution deferred to
//     physical-build time. ir.Skip and ir.Limit now carry an optional
//     CountExpr AST that the physical builder evaluates via expr.Eval
//     against the query parameters; resolveCountExpr applies the
//     openCypher type-and-range rules and surfaces
//     SyntaxError(InvalidArgumentType) for non-integer values and
//     SyntaxError(NegativeIntegerArgument) for negative integers.
//     Pre-fix intExpr silently returned 0 for any non-literal-integer
//     expression, so `SKIP $p` / `LIMIT $p` collapsed to 0 rows
//     regardless of the parameter value. Unlocks ReturnSkipLimit3 [2],
//     WithSkipLimit3 [2] and the broader SKIP/LIMIT-by-parameter family
//     plus the InvalidArgumentType / NegativeIntegerArgument compile
//     expectations. Observed 3580-3582 across a 5-run sample (median
//     3581); gate set conservatively at 3575 (minimum observed - 5).
//   - 3584: raised after compile-time CALL arity validation,
//     AmbiguousAggregationExpression sema check (wired from both
//     projectionCheck and withClause with per-leaf grouping-key
//     matching per openCypher 9 §5.3.3). Bare-Variable and
//     `recv.key` Property projection items count as grouping keys;
//     compound projection items (BinaryOp arithmetic) do NOT
//     authorise leaf substitution in an aggregating sibling.
//     Observed 3589-3596 across a 5-run sample (median 3593); gate
//     set conservatively at 3584 (minimum observed - 5).
//   - 3593: raised after MergeRelationship — new IR + exec operator
//     handling the canonical single-hop MERGE-of-relationship-between-
//     bound-endpoints shape (`MATCH (a:A), (b:B) MERGE (a)-[r:T]->(b)`).
//     The translator routes the simplest case (one rel hop, both
//     endpoint vars bound by child, no ON CREATE / ON MATCH actions,
//     no inline rel properties, no re-asserted endpoint
//     labels/properties) to a focused exec operator that resolves
//     both endpoint NodeIDs, calls HasEdge, and either tags the
//     existing edge with the requested type (idempotent
//     SetEdgeLabel) or AddEdge + SetEdgeLabel for a fresh edge.
//     Pre-fix the IR translator funnelled every MERGE through the
//     node-only Merge path; relationship-pattern MERGEs returned
//     zero rows because the search-fn scanned nodes, not edges.
//     Unlocks Merge5 [2]/[4]/[5]/[7] and related scenarios.
//     Observed 3598-3604 across a 5-run sample (median 3601); gate
//     set conservatively at 3593 (minimum observed - 5).
//   - 3620: raised after eight targeted execution-level fixes landed
//     across two commits:
//     (a) extractAggregatesFromExpr recurses into MapLiteral /
//         ListLiteral / SliceExpr / CaseExpression so a nested
//         collect()/sum()/count() inside `WITH {key: collect(u)} AS m`
//         is hoisted correctly (Delete5 #5/#6/#7).
//     (b) Procedure args: INTEGER literal accepted where FLOAT is
//         declared, with runtime arg-evaluator coercion (Call3 #5/#6).
//     (c) buildRowCtx reconstructs PathValue from pathVarChain so
//         `WHERE length(p) = 1` operates on the proper Path kind
//         (MatchWhere1 #12/#13).
//     (d) RETURN respects DISTINCT → ORDER BY → SKIP → LIMIT canonical
//         order via applyProjectionTail, so SKIP reads from the
//         ordered stream (ReturnSkipLimit1 #1/#2, ReturnSkipLimit3 #1/#2).
//     (e) Sema flags UndefinedVariable on ORDER BY items that reference
//         pre-projection variables after DISTINCT or aggregation
//         collapses row identity, with collectFreeVarsOutsideProjectedAggs
//         honouring projected aggregate calls (ReturnOrderBy2 #13,
//         ReturnOrderBy6 #4, WithOrderBy4 #19).
//     (f) checkMergeNoRebind rejects bound-variable re-use with new
//         labels/properties (Merge5 #22).
//     (g) MERGE patterns with literal `null` property values fail with
//         MergeReadOwnWrites at runtime (Merge1 #17, Merge5 #29).
//     (h) MERGE ON CREATE/ON MATCH applies SET label items
//         (`SET a:Foo` form), not just `var.key = value` (Merge2 #1,
//         Merge3 #2, plus three related scenarios).
//     Observed 3624-3625 across five-run samples; gate set at 3620
//     to absorb run-to-run variance from order-sensitive scenarios.
//   - 3628: raised after three additional execution-level uplifts:
//     (a) sema's transitive property-access check now distinguishes
//         "scalar" / "list" / "map" projection types and rejects
//         `WITH 123 AS x RETURN x.num`-style accesses at compile
//         time (Map1 #6, Graph6 #9).
//     (b) MergeRelationship binds the relationship variable in the
//         output row via a synthetic RelationshipValue, so count(r),
//         RETURN r, and r.prop downstream of `MERGE (a)-[r:T]->(b)`
//         observe the bound edge (Merge5 #2 / #5–#8, Merge6 #2,
//         Merge7 #3 and related).
//     Observed 3633 stable across five-run samples; gate set at
//     3628 to absorb run-to-run variance.
//   - 3643: raised after two further uplifts:
//     (a) The Sort+LimitExpr fallback in applyProjectionTail now
//         preserves the limit expression (NewLimitExpr) instead of
//         silently using a literal zero limit, so a float-typed
//         parameter surfaces as the documented runtime
//         InvalidArgumentType (ReturnSkipLimit2 #15).
//     (b) sema flags `any/none/all/single(x IN <homogeneous list>
//         WHERE <arithmetic on x>)` at compile time when the list is
//         homogeneously typed under a non-numeric kind. Adds
//         inferListElementType and quantifierPredicateUsesArithOn
//         (Quantifier1-4 #15/#16, twelve scenarios).
//     Observed 3648 stable across five-run samples; gate set at
//     3643 to absorb run-to-run variance.
//   - 3665: raised after four further uplifts:
//     (a) collectActualRows column-name fallback is now
//         case-insensitive in addition to whitespace-insensitive, so
//         TCK fixtures that use mixed-case source spellings (cOuNt,
//         dIstInct) resolve against the canonicalised engine column
//         keys (Return4 #4–#6).
//     (b) Map / node / relationship subscript with a non-string index
//         surfaces MapElementAccessByNonString at runtime instead of
//         silently returning NULL (Map2 #6/#7).
//     (c) buildRowCtx reconstructs a PathValue from pathVarMeta when a
//         named path is bound by a variable-length pattern, so
//         relationships(p) / nodes(p) / length(p) observe the proper
//         Path kind (Path2 #1/#2 and related, ~11 scenarios).
//     (d) Merge.Next iterates one upstream child row per merge cycle
//         instead of running the cycle once in Init, so
//         `MATCH (person) MERGE (city) RETURN person, city` emits one
//         row per Person (Merge2 #5, Merge3 #4, Merge4 #2, Merge6 #6/#7,
//         Merge7 #4/#5, Merge8 #1, Merge9 #1/#4).
//     Observed 3669 stable across five-run samples; gate set at
//     3665 to absorb run-to-run variance.
//   - 3678: raised after three further uplifts:
//     (a) MERGE relationship inline property predicate filters the
//         existing-edge search via matchesRelProps and seeds the
//         freshly created edge with the literal properties; the IR
//         translator no longer falls back to the node-only Merge path
//         for `MERGE (a)-[r:T {k: v}]->(b)` (Merge5 #5/#6/#8/#14).
//     (b) projectionsWithComprehensions hoists NESTED PatternCompre-
//         hensions out of projection items via the new
//         extractNestedPatternComprehensions walker, so
//         `size([(n)-->() | 1])`-style queries no longer surface
//         "unsupported expression type *ast.PatternComprehension"
//         at runtime (List6 #7–#10 and related).
//     (c) translatePatternComprehension now propagates the
//         named-path binding (`p = (n)-->(:B)`) onto the inner
//         PathPattern so matchPattern emits NamedPath; the
//         comprehension's `| p` projection then sees a real
//         PathValue via pathVarMeta / pathVarChain (Pattern2 #1–#3
//         and related, nine scenarios).
//     Observed 3682 stable across five-run samples; gate set at
//     3678 to absorb run-to-run variance.
//   - 3683: raised after two further uplifts:
//     (a) RETURN n.prop AS n bypasses the schema-name fast path so
//         the row carries the projected scalar instead of the bound
//         node (Merge5 #16–#19, Return4 #1).
//     (b) Variable-length relationship pattern accepts a
//         previously-bound list alias without raising
//         VariableTypeConflict, enabling
//         `MATCH ... WITH [r1, r2] AS rs MATCH (a)-[rs*]->(b)`
//         (Match4 #8, Match9 #6/#7).
//     Observed 3688 stable across five-run samples; gate set at
//     3683 to absorb run-to-run variance.
//   - 3697: raised after three further uplifts:
//     (a) buildIRProjection's alias-collision guard now covers map
//         literals too — `WITH {k: m.id} AS m` survives without the
//         schema-name fast path swallowing the bound node
//         (With4 #7).
//     (b) Expand.tryRevEdge now resolves the reverse edge to its
//         forward-CSR position (via lookupFwdEdgePos) when available
//         so cyphermorphism observes the same id across both
//         traversal directions; previously the synthetic
//         `len(fwd) + revPos` id let undirected re-traversals slip
//         through (Match6 #9/#10/#11/#12/#13/#14).
//     (c) formatNodeTCK sorts the node-label slice deterministically
//         before emission so the result-set comparator no longer
//         flips on map iteration order
//         (Graph5 #1/#3 etc., plus a handful of label-list
//         comparisons elsewhere).
//     Observed 3702 stable across five-run samples; gate set at
//     3697 to absorb run-to-run variance.
//   - 3700: raised after two further uplifts:
//     (a) cmpFloat64 sorts NaN after every finite number, so ORDER BY
//         DESC produces the documented null > NaN > finite ordering
//         (ReturnOrderBy1 #12 and related).
//     (b) procs.Signature gained InputNames, populated by the TCK
//         proc-decl parser; CALL with no argument list now binds each
//         declared input from the matching query parameter
//         (Call2 #3).
//     Observed 3705 stable across five-run samples; gate set at
//     3700 to absorb run-to-run variance.
//   - 3712: raised after a foundational projection-schema fix:
//     buildIRProjection now resets the shared schema map to ONLY the
//     output items' aliases / expression-string secondary keys after
//     processing the projection list. The previous behaviour added
//     output names but kept stale upstream entries, so a sequence
//     like `WITH x[0] AS y MATCH (n)` saw an inflated schemaWidth
//     (counting both y and the dropped lst/x slots) and the
//     downstream Apply mis-offset the inner subtree's columns —
//     reading the MATCH-bound node returned null. Unlocks
//     Comparison1 #1, Comparison2 #3 (×4), and adjacent multi-clause
//     scenarios in the same family.
//
//     Two further focused uplifts landed alongside the schema fix:
//     * NaN-vs-number ordering yields FALSE; NaN-vs-non-number
//       yields NULL (Comparison2 #5 examples).
//     * sema/validateRelRange retains the negative-bound checks but
//       drops the Min > Max branch (false-positive against the
//       parser's abs-normalisation pipeline).
//
//     Observed 3716 stable across five-run samples; gate set at
//     3712 to absorb run-to-run variance.
//   - 3715: holds — exec.Eager pipeline barrier and the LIMIT-0-over-
//     writes gate fix Create6 #8 ("Limiting to zero results after
//     creating relationships affects the result set but not the side
//     effects"); the wider LIMIT-N-over-writes wrap regressed
//     Match5 #26's mid-pipeline setup query and is intentionally not
//     applied.
//   - 3760: raised after RemoveProperty learnt to silently skip
//     rows whose target variable is NULL. resolveEntityFromRow
//     and resolveEntityMaybeRel now return the existing
//     errNullTarget sentinel (previously they fell through to a
//     hard error about an unsupported type) and the RemoveProperty
//     Next loop treats that sentinel as a pass-through, matching
//     resolveNodeIDFromRow's contract and the openCypher rule that
//     DELETE / REMOVE / SET on a null target is a silent no-op.
//     Closes the two Remove1 [5]/[6] OPTIONAL MATCH + REMOVE
//     scenarios alongside a small ripple of variance-sensitive
//     adjacent tests.
//     Observed 3760-3768 across a 5-run sample; gate set at 3760.
//   - 3756: raised after durationFromMap stopped truncating sub-
//     second components via the bare int32(sFrac * 1e9) cast.
//     For nanoseconds:1 the intermediate sFrac is 1e-9; the product
//     with 1e9 is 0.99999999… in float64 and the truncating cast
//     dropped the lone nanosecond. math.Round around the same
//     product preserves it without affecting larger components.
//     Closes 8 Temporal8 [6]/[7] add-subtract / multiply-divide
//     duration scenarios where the answer differed only in the
//     sub-second tail.
//     Observed 3757-3764 across a 5-run sample; gate set at 3756.
//   - 3748: raised after ast.FloatLiteral.String() learnt to keep
//     the float discriminator. strconv.FormatFloat with precision -1
//     strips the trailing ".0" for whole-number values, so a CREATE
//     ({price: 10.0}) round-tripped through the writes pipeline
//     ended up calling parsePropValue("10") → Int64Value(10) and
//     subsequent percentileDisc/percentileCont over those properties
//     returned IntegerValue instead of FloatValue. The fix appends
//     ".0" to FormatFloat output whenever the rendered text lacks
//     both a "." and an exponent marker, leaving NaN/Inf untouched
//     (those are not valid Cypher literals). Closes the 3
//     Aggregation6 [1] percentileDisc float-discriminator failures.
//     Observed 3748-3755 across a 5-run sample; gate set at 3748.
//   - 3745: raised after dateFromMap / timeComponentsFromMap learnt
//     to treat the {datetime: ...} key as a date+time base source
//     (previously only {date:..} / {time:..} were consulted, so
//     `datetime({datetime: other})` and `localdatetime({datetime:
//     other})` collapsed to the 1970-01-01T00:00 default). Closes all
//     14 Temporal3 [7]/[10]/[11] failures. The {date:..}/{time:..}
//     explicit keys still win when both are supplied, matching the
//     explicit-over-implicit precedence used elsewhere in the
//     constructor.
//     Observed 3745-3752 across a 6-run sample; gate set at 3745.
//   - 3735: raised after two surgical temporal-truncate fixes that
//     together close every Temporal9 [4]/[5] failure (≈24 scenarios):
//     (a) sourceToTime() for TimeValue/LocalTimeValue rebuilds the
//         internal time.Time from wall-clock components in the
//         destination FixedZone instead of treating Nanos as a UTC
//         instant and shifting via .In(loc), so
//         localtime.truncate('hour', time(+01:00)) returns 12:00 not
//         13:00 and the symmetric -01:00 source returns 12:00 not
//         11:00.
//     (b) applyOverrides() timezone branch now parses '+HH:MM'/'-HH:MM'/
//         'Z'/'UTC' via a shared parseTimezoneString helper alongside
//         time.LoadLocation, so {timezone: '+01:00'} overrides actually
//         take effect instead of silently keeping the source zone.
//     Observed 3737-3740 across a 4-run sample; gate set at 3735 to
//     absorb run-to-run variance.
//
// To raise the baseline after a deliberate uplift in execution support, run
// the suite, read the "<N> scenarios (<P> passed, ...)" summary, and edit
// this constant in a dedicated commit.
const tckExecutionBaseline = 3760

// scenarioSummaryRE matches the godog summary line emitted by the progress
// formatter:
//
//	"1234 scenarios (1005 passed, 229 failed)"
//
// Sub-groups: 1 = total scenarios, 2 = passed scenarios.
var scenarioSummaryRE = regexp.MustCompile(`(\d+)\s+scenarios?\s+\((\d+)\s+passed`)

// TestTCKExecution runs the openCypher TCK feature files through the execution engine.
// It uses godog to parse Gherkin and dispatch step implementations.
//
// The test fails when the number of passing scenarios drops below
// tckExecutionBaseline — locking in the current execution coverage. The
// summary line is captured from the progress formatter so the count is
// observable in source rather than relying on godog's exit status (which
// only encodes pass/fail, not magnitude).
//
// Use -short to run a randomised sample of scenarios; the baseline gate is
// disabled in short mode because the sample is non-deterministic.
func TestTCKExecution(t *testing.T) {
	if testing.Short() {
		t.Log("TCK execution: short mode — running randomised scenario sample")
	}

	// Capture the formatter's stdout so we can parse the summary line for the
	// passing-scenario count after suite.Run returns. NoColors=true is required
	// so the captured text has no ANSI escape sequences inside the digit groups
	// the summary regex looks for.
	var buf bytes.Buffer
	out := io.MultiWriter(os.Stdout, &buf)

	opts := &godog.Options{
		Format:        "progress",
		Paths:         []string{"features"},
		FS:            tck.FeatureFiles(),
		Strict:        false,
		StopOnFailure: false,
		Output:        out,
		NoColors:      true,
		// TestingT is intentionally not set: setting it causes godog to call
		// t.Fail() for every scenario failure. The regression gate below is
		// based on the aggregate passing-scenario count, not per-scenario fail.
	}

	if testing.Short() {
		opts.Randomize = math.MaxInt64
	}

	suite := godog.TestSuite{
		Name:                "openCypher TCK",
		ScenarioInitializer: initScenario,
		Options:             opts,
	}

	status := suite.Run()
	if status != 0 {
		t.Logf("TCK execution: some scenarios failed or were pending (status=%d); see progress output above", status)
	}

	// Skip the gate in short mode: the randomised sample is a different
	// population and its pass count is not comparable to the baseline.
	if testing.Short() {
		return
	}

	total, passed, ok := parseScenarioSummary(buf.Bytes())
	if !ok {
		t.Fatalf("TCK execution: could not locate scenario summary line in formatter output (looked for %q)",
			scenarioSummaryRE.String())
	}
	t.Logf("TCK execution: %d scenarios, %d passed (baseline=%d)", total, passed, tckExecutionBaseline)
	if passed < tckExecutionBaseline {
		t.Errorf("TCK execution regression: %d scenarios passed, baseline=%d", passed, tckExecutionBaseline)
	}
}

// parseScenarioSummary extracts (total, passed, ok) from the formatter output
// emitted by godog's Base.Summary(). Returns ok=false if no summary line is
// present in raw.
func parseScenarioSummary(raw []byte) (total, passed int, ok bool) {
	m := scenarioSummaryRE.FindSubmatch(raw)
	if m == nil {
		return 0, 0, false
	}
	t, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, 0, false
	}
	p, err := strconv.Atoi(string(m[2]))
	if err != nil {
		return 0, 0, false
	}
	return t, p, true
}

// initScenario creates a fresh world per scenario and registers all step
// definitions on it.
func initScenario(sc *godog.ScenarioContext) {
	w := newWorld()

	// ── Given steps ──────────────────────────────────────────────────────────
	sc.Step(`^an empty graph$`, func(ctx context.Context) error {
		return w.givenAnEmptyGraph(ctx)
	})
	sc.Step(`^any graph$`, func(ctx context.Context) error {
		return w.givenAnyGraph(ctx)
	})
	sc.Step(`^the binary-tree-1 graph$`, func(ctx context.Context) error {
		return w.givenBinaryTree1(ctx)
	})
	sc.Step(`^the binary-tree-2 graph$`, func(ctx context.Context) error {
		return w.givenBinaryTree2(ctx)
	})

	// ── And/Background steps ─────────────────────────────────────────────────
	sc.Step(`^having executed:$`, func(ctx context.Context, query *godog.DocString) error {
		return w.havingExecuted(ctx, query)
	})
	sc.Step(`^parameters are:$`, func(ctx context.Context, params *godog.Table) error {
		return w.parametersAre(ctx, params)
	})
	// Procedure existence declarations — no-op stubs (engine resolves at runtime).
	// These steps have a trailing table body (the procedure signature table), so
	// the step function must accept *godog.Table as the second argument.
	sc.Step(`^there exists a procedure (.+)$`, func(ctx context.Context, sig string, table *godog.Table) error {
		return w.thereExistsAProcedure(ctx, sig, table)
	})

	// ── When steps ───────────────────────────────────────────────────────────
	sc.Step(`^executing query:$`, func(ctx context.Context, query *godog.DocString) error {
		return w.whenExecutingQuery(ctx, query)
	})
	sc.Step(`^executing control query:$`, func(ctx context.Context, query *godog.DocString) error {
		return w.whenExecutingControlQuery(ctx, query)
	})

	// ── Then steps — result assertions ───────────────────────────────────────
	sc.Step(`^the result should be empty$`, func(ctx context.Context) error {
		return w.resultShouldBeEmpty(ctx)
	})
	sc.Step(`^the result should be, in any order:$`, func(ctx context.Context, table *godog.Table) error {
		return w.resultShouldBeInAnyOrder(ctx, table)
	})
	sc.Step(`^the result should be, in order:$`, func(ctx context.Context, table *godog.Table) error {
		return w.resultShouldBeInOrder(ctx, table)
	})
	sc.Step(`^the result should be \(ignoring element order for lists\):$`, func(ctx context.Context, table *godog.Table) error {
		return w.resultShouldBeInAnyOrderIgnoringListOrder(ctx, table)
	})
	sc.Step(`^the result should be, in order \(ignoring element order for lists\):$`, func(ctx context.Context, table *godog.Table) error {
		return w.resultShouldBeInOrderIgnoringListOrder(ctx, table)
	})

	// ── And steps — side effects ─────────────────────────────────────────────
	sc.Step(`^no side effects$`, func(ctx context.Context) error {
		return w.noSideEffects(ctx)
	})
	sc.Step(`^the side effects should be:$`, func(ctx context.Context, table *godog.Table) error {
		return w.sideEffectsTable(ctx, table)
	})

	// ── Then steps — error assertions ────────────────────────────────────────
	sc.Step(`^a SyntaxError should be raised at compile time: (.+)$`, func(ctx context.Context, errType string) error {
		return w.syntaxErrorAtCompileTime(ctx, errType)
	})
	sc.Step(`^a SyntaxError should be raised at runtime: (.+)$`, func(ctx context.Context, errType string) error {
		return w.syntaxErrorAtRuntime(ctx, errType)
	})
	sc.Step(`^a TypeError should be raised at runtime: (.+)$`, func(ctx context.Context, errType string) error {
		return w.typeErrorAtRuntime(ctx, errType)
	})
	sc.Step(`^a TypeError should be raised at any time: (.+)$`, func(ctx context.Context, errType string) error {
		return w.typeErrorAtAnyTime(ctx, errType)
	})
	sc.Step(`^a TypeError should be raised at compile time: (.+)$`, func(ctx context.Context, errType string) error {
		return w.typeErrorAtCompileTime(ctx, errType)
	})
	// Generic handler for all other error categories (ArgumentError, SemanticError, etc.).
	// Must be registered AFTER the specific SyntaxError and TypeError steps so the
	// more-specific patterns take precedence when both could match.
	sc.Step(`^a (\w+Error) should be raised at runtime: (.+)$`, func(ctx context.Context, errCategory, errType string) error {
		return w.genericErrorAtRuntime(ctx, errCategory, errType)
	})
	sc.Step(`^a (\w+Error) should be raised at compile time: (.+)$`, func(ctx context.Context, errCategory, errType string) error {
		return w.genericErrorAtCompileTime(ctx, errCategory, errType)
	})
	sc.Step(`^a (\w+Error) should be raised at any time: (.+)$`, func(ctx context.Context, errCategory, errType string) error {
		return w.genericErrorAtRuntime(ctx, errCategory, errType)
	})
}
