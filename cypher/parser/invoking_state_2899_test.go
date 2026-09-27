package parser

// invoking_state_2899_test.go — rmp #2899.
//
// Every parser rule context records the ATN state its caller was in when it
// invoked the rule. antlr4-go v4.13.1 follows that chain whenever it needs the
// real outer context — full-context (LL) prediction after an SLL conflict
// (predictionContextFromRuleContext) and error recovery
// (DefaultErrorStrategy.getErrorRecoverySet) — and asserts, unchecked, that the
// state's first transition is a *RuleTransition. Generated rule bodies satisfy
// it by calling p.SetState(N) before every sub-rule call. The hand-written
// bodies in gen-patches.patch (MultiPartQ, ReduceExpression and the reduce
// alternative of Atom) did not, so the callee inherited the rule's start
// state, whose transition is epsilon, and the first LL fallback beneath a WITH
// or inside reduce() panicked: `MATCH (a),(b) WITH a, b,
// size([(a)-[]-(b)-[]-(a) | 1]) AS n RETURN n` failed with "interface
// conversion: antlr.Transition is *antlr.EpsilonTransition".
//
// The structural test asserts the invariant itself on every context of every
// parse, so it fails on a missing SetState even where no LL fallback happens
// to occur; the behavioural test asserts the reported queries parse.
//
// Layer: short.

import (
	"reflect"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen"
)

// invokingState2899Queries reach every hand-written rule call site: both
// readingStatement loops, updatingStatement, withSt and singlePartQ of
// MultiPartQ, and every sub-rule of ReduceExpression. Each carries the cyclic
// two-hop pattern comprehension that forces an LL fallback beneath it.
var invokingState2899Queries = []string{
	"MATCH (a),(b) WITH a, b, size([(a)-[]-(b)-[]-(a) | 1]) AS n RETURN n",
	"MATCH (a),(b) WITH a, b, [(a)-[]-(b)-[]-(a) | 1] AS n RETURN n",
	"WITH [(a)--(b)--(a) | 1] AS n RETURN n",
	"WITH 1 AS x WHERE size([(a)--(b)--(a) | 1]) > 0 RETURN x",
	"MATCH (a) WITH a MATCH (b) WITH a, b, [(a)--(b)--(a) | 1] AS n RETURN n",
	"MATCH (a) WITH a MATCH (b) RETURN [(a)--(b)--(a) | 1] AS n",
	"WITH 1 AS x UNWIND [(a)--(b)--(a) | 1] AS y RETURN y",
	"WITH 1 AS x CREATE (a) SET a.v = size([(a)--(b)--(a) | 1]) WITH a RETURN a",
	"MATCH (a) WITH a MATCH (b) CREATE (c) WITH c RETURN c",
	"RETURN reduce(acc = 0, x IN [(a)--(b)--(a) | 1] | acc + x) AS r",
	"RETURN reduce(acc = [(a)--(b)--(a) | 1], x IN [1] | acc) AS r",
	"RETURN reduce(acc = 0, x IN [1] WHERE x > 0 | acc + size([(a)--(b)--(a) | 1])) AS r",
	"WITH [1, 2] AS l RETURN reduce(s = 0, x IN l | s + x) AS r",
}

// invokedRule reads the rule a state's first transition invokes, or reports
// false when that transition is not a *antlr.RuleTransition. antlr4-go keeps
// the ATN state table and the transition fields unexported, so they are read
// by reflection; only integers and types are read, nothing is written. A
// runtime whose layout no longer matches fails the test rather than passing it.
func invokedRule(t *testing.T, atn *antlr.ATN, state int) (rule int, ok bool) {
	t.Helper()
	states := reflect.ValueOf(atn).Elem().FieldByName("states")
	if !states.IsValid() || state < 0 || state >= states.Len() {
		t.Fatalf("ATN state %d not addressable (runtime layout changed?)", state)
	}
	trs := states.Index(state).Elem().Elem().FieldByName("transitions")
	if !trs.IsValid() || trs.Len() == 0 {
		t.Fatalf("ATN state %d has no readable transitions (runtime layout changed?)", state)
	}
	tr := trs.Index(0).Elem()
	if tr.Type() != reflect.TypeFor[*antlr.RuleTransition]() {
		return 0, false
	}
	return int(tr.Elem().FieldByName("ruleIndex").Int()), true
}

// invokingStateChecker asserts, on entry to every rule, that the context's
// invoking state is a rule-invocation state for that same rule, and that the
// runtime can build the context's full prediction context — the call that
// panicked. reduceExpression has no ATN rule of its own and is invoked from
// the functionInvocation alternative of atom, whose follow state is the end of
// atom: the one sanctioned mismatch.
type invokingStateChecker struct {
	antlr.BaseParseTreeListener
	t      *testing.T
	query  string
	parser *gen.CypherParser
	seen   int
}

func (c *invokingStateChecker) EnterEveryRule(ctx antlr.ParserRuleContext) {
	if ctx.GetParent() == nil {
		return // the start rule: nobody invoked it
	}
	c.seen++
	names := c.parser.GetRuleNames()
	rule := names[ctx.GetRuleIndex()]
	atn := c.parser.GetATN()
	n := ctx.GetInvokingState()
	got, ok := invokedRule(c.t, atn, n)
	if !ok {
		c.t.Errorf("%q: %s context's invoking state %d is not a rule-invocation state", c.query, rule, n)
		return
	}
	want := ctx.GetRuleIndex()
	if want == gen.CypherParserRULE_reduceExpression {
		want = gen.CypherParserRULE_functionInvocation
	}
	if got != want {
		c.t.Errorf("%q: %s context's invoking state %d invokes %s, want %s", c.query, rule, n, names[got], names[want])
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				c.t.Errorf("%q: full-context lookahead from %s panicked: %v", c.query, rule, r)
			}
		}()
		atn.NextTokensInContext(atn.DecisionToState[0], ctx)
	}()
}

// checkInvokingStates parses query, prepared as [ParseStatement] prepares it,
// with the default error strategy and the checker attached, and returns the number of contexts it inspected.
func checkInvokingStates(t *testing.T, query string) int {
	t.Helper()
	// The text pipeline of ParseStatement: the shortestPath() rewrite, then
	// the normalizers.
	text, _ := rewriteShortestPath(query)
	stream, lexErrs := lexNormalized(applyNormalizers(text))
	if len(lexErrs.errs) > 0 {
		t.Fatalf("%q: lex error: %v", query, lexErrs.errs[0])
	}
	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	errs := &errorListener{}
	p.AddErrorListener(errs)
	c := &invokingStateChecker{t: t, query: query, parser: p}
	p.AddParseListener(c)
	// Errorf, not Fatalf: every query is checked, so one run reports every
	// defective call site.
	if _, err := recoverParseScript(p); err != nil {
		t.Errorf("%q: %v", query, err)
	}
	if len(errs.errs) > 0 {
		t.Errorf("%q: syntax error: %v", query, errs.errs[0])
	}
	return c.seen
}

// TestHandWrittenRuleInvokingStates asserts the invoking-state invariant over
// the #2899 queries and over every statement of the examples corpus.
func TestHandWrittenRuleInvokingStates(t *testing.T) {
	t.Parallel()
	t.Run("reported", func(t *testing.T) {
		t.Parallel()
		for _, q := range invokingState2899Queries {
			// Non-vacuity: a parse that entered no sub-rule checked nothing.
			if n := checkInvokingStates(t, q); n == 0 {
				t.Errorf("%q: no rule context was inspected", q)
			}
		}
	})
	t.Run("corpus", func(t *testing.T) {
		t.Parallel()
		for _, st := range readCorpus(t).Statements {
			checkInvokingStates(t, st.Text)
		}
	})
}

// TestParse_CyclicPatternComprehensionBeneathHandWrittenRule asserts that the
// #2899 queries parse through the public entry points.
func TestParse_CyclicPatternComprehensionBeneathHandWrittenRule(t *testing.T) {
	t.Parallel()
	for _, q := range invokingState2899Queries {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q): %v", q, err)
		}
		if _, errs := ParseStrict(q); len(errs) > 0 {
			t.Errorf("ParseStrict(%q): %v", q, errs)
		}
	}
}
