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
//
// To raise the baseline after a deliberate uplift in execution support, run
// the suite, read the "<N> scenarios (<P> passed, ...)" summary, and edit
// this constant in a dedicated commit.
const tckExecutionBaseline = 1955

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
	sc.Step(`^there exists a procedure (.+)$`, func(ctx context.Context, sig string, _ *godog.Table) error {
		return w.thereExistsAProcedure(ctx, sig)
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
