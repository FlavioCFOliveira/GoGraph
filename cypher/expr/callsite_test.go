package expr_test

// callsite_test.go — the per-statement call-site memo of rmp #2891.
//
// A registry that hosts the call-site table (the engine's per-statement
// registry) must see each call site resolved once per statement, and a
// statement-constant call evaluated once per statement; every other shape must
// keep the per-evaluation behaviour. The fake host below counts both.
//
// Layer: short.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// countingHost is a FunctionRegistry that hosts a call-site table and counts
// Resolve calls and per-function invocations.
type countingHost struct {
	folds    bool
	sites    sync.Map
	resolves atomic.Int64
	calls    sync.Map // name -> *atomic.Int64
	fail     bool     // "date" returns an error
}

func (h *countingHost) LoadCallSite(key *ast.FunctionInvocation) (any, bool) {
	return h.sites.Load(key)
}

func (h *countingHost) StoreCallSite(key *ast.FunctionInvocation, site any) any {
	actual, _ := h.sites.LoadOrStore(key, site)
	return actual
}

func (h *countingHost) FoldsStatementConstants() bool { return h.folds }

func (h *countingHost) counter(name string) *atomic.Int64 {
	c, _ := h.calls.LoadOrStore(name, new(atomic.Int64))
	return c.(*atomic.Int64)
}

var errDateProbe = errors.New("date probe failure")

func (h *countingHost) Resolve(name string) (expr.BuiltinFn, bool) {
	h.resolves.Add(1)
	switch name {
	case "date", "rand":
		c := h.counter(name)
		return func(args []expr.Value) (expr.Value, error) {
			c.Add(1)
			if h.fail {
				return nil, errDateProbe
			}
			return expr.DateValue{Year: 2025, Month: 1, Day: 1}, nil
		}, true
	}
	return nil, false
}

func call(name string, args ...ast.Expression) *ast.FunctionInvocation {
	return &ast.FunctionInvocation{Name: name, Args: args}
}

// evalN evaluates e n times against host, as n rows of one statement would.
func evalN(t *testing.T, e ast.Expression, host *countingHost, params map[string]expr.Value, n int) {
	t.Helper()
	for range n {
		if _, err := expr.Eval(e, expr.RowContext{"x": expr.IntegerValue(1)}, params, host); err != nil {
			t.Fatalf("Eval: %v", err)
		}
	}
}

func TestCallSite_ResolvedOncePerStatement(t *testing.T) {
	host := &countingHost{}
	// rand() is not statement-constant, so every evaluation calls it; the
	// resolution is still memoised.
	evalN(t, call("RAND"), host, nil, 100)
	if got := host.resolves.Load(); got != 1 {
		t.Errorf("Resolve called %d times for 100 evaluations of one call site, want 1", got)
	}
	if got := host.counter("rand").Load(); got != 100 {
		t.Errorf("rand() invoked %d times, want 100 (it must never be folded)", got)
	}
}

func TestCallSite_StatementConstantCallFolded(t *testing.T) {
	params := map[string]expr.Value{"ref": expr.StringValue("2025-01-01")}
	for _, tc := range []struct {
		name string
		e    ast.Expression
	}{
		{"parameter", call("date", &ast.Parameter{Name: "ref"})},
		{"literal", call("date", &ast.StringLiteral{Value: "2025-01-01"})},
		{"no arguments", call("date")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &countingHost{folds: true}
			evalN(t, tc.e, host, params, 100)
			if got := host.counter("date").Load(); got != 1 {
				t.Errorf("date invoked %d times for 100 evaluations, want 1", got)
			}
		})
	}
}

func TestCallSite_NotFoldedWhenPreconditionFails(t *testing.T) {
	params := map[string]expr.Value{
		"ref": expr.StringValue("2025-01-01"),
		"m":   expr.MapValue{"year": expr.IntegerValue(2025)},
	}
	for _, tc := range []struct {
		name  string
		folds bool
		e     ast.Expression
	}{
		{"registry is not the built-ins", false, call("date", &ast.Parameter{Name: "ref"})},
		{"variable argument", true, call("date", &ast.Variable{Name: "x"})},
		{"map argument value", true, call("date", &ast.Parameter{Name: "m"})},
		{"rand is not statement-constant", true, call("rand")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &countingHost{folds: tc.folds}
			evalN(t, tc.e, host, params, 50)
			name := tc.e.(*ast.FunctionInvocation).Name
			if got := host.counter(name).Load(); got != 50 {
				t.Errorf("%s invoked %d times for 50 evaluations, want 50", name, got)
			}
		})
	}
}

// TestCallSite_FoldIsLazyAndKeepsErrors pins the two properties that make
// folding invisible: a call no evaluation reaches is never invoked, and a
// folded call raises the same error on every evaluation that reaches it.
func TestCallSite_FoldIsLazyAndKeepsErrors(t *testing.T) {
	host := &countingHost{folds: true, fail: true}
	guarded := &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{{
			Condition:  &ast.BoolLiteral{Value: false},
			Consequent: call("date", &ast.StringLiteral{Value: "2025-01-01"}),
		}},
	}
	evalN(t, guarded, host, nil, 10)
	if got := host.counter("date").Load(); got != 0 {
		t.Fatalf("date behind a false CASE branch invoked %d times, want 0", got)
	}
	e := call("date", &ast.StringLiteral{Value: "2025-01-01"})
	for i := range 5 {
		if _, err := expr.Eval(e, nil, nil, host); !errors.Is(err, errDateProbe) {
			t.Fatalf("evaluation %d: err = %v, want %v", i, err, errDateProbe)
		}
	}
	if got := host.counter("date").Load(); got != 1 {
		t.Errorf("failing date invoked %d times for 5 evaluations, want 1", got)
	}
}

// TestCallSite_ConcurrentEvaluationIsSafe evaluates one statement's call sites
// from many goroutines, as the parallel scan tier does. Meaningful under -race.
func TestCallSite_ConcurrentEvaluationIsSafe(t *testing.T) {
	host := &countingHost{folds: true}
	params := map[string]expr.Value{"ref": expr.StringValue("2025-01-01")}
	folded := call("date", &ast.Parameter{Name: "ref"})
	unfolded := call("rand")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				v, err := expr.Eval(folded, nil, params, host)
				if err != nil || v != (expr.DateValue{Year: 2025, Month: 1, Day: 1}) {
					t.Errorf("date($ref) = %v, %v", v, err)
					return
				}
				if _, err := expr.Eval(unfolded, nil, params, host); err != nil {
					t.Errorf("rand(): %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := host.counter("rand").Load(); got != 16*200 {
		t.Errorf("rand() invoked %d times, want %d", got, 16*200)
	}
	// Racing first evaluations may each compute the value before one is stored;
	// after that every evaluation is served from the memo.
	if got := host.counter("date").Load(); got < 1 || got > 16 {
		t.Errorf("date invoked %d times, want between 1 and 16", got)
	}
}
