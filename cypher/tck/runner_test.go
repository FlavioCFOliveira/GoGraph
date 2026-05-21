package tck_test

import (
	"context"
	"math"
	"testing"

	"github.com/cucumber/godog"

	"gograph/cypher/tck"
)

// TestTCKExecution runs the openCypher TCK feature files through the execution engine.
// It uses godog to parse Gherkin and dispatch step implementations.
// Pass rate is reported at the end.
//
// The test does not fail the suite when scenarios fail — it is a reporting
// test, not a CI gate. Use -short to run a randomised sample of scenarios.
func TestTCKExecution(t *testing.T) {
	if testing.Short() {
		t.Log("TCK execution: short mode — running randomised scenario sample")
	}

	opts := &godog.Options{
		Format:        "progress",
		Paths:         []string{"features"},
		FS:            tck.FeatureFiles(),
		Strict:        false,
		StopOnFailure: false,
		// TestingT is intentionally not set: setting it causes godog to call
		// t.Fail() for every scenario failure, which would make this test fail
		// the CI suite. This test is a pass-rate reporter, not a strict gate.
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
		// Do not t.Fatal — this is a pass-rate reporter, not a strict gate.
	}
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
