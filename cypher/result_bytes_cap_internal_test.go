package cypher

// result_bytes_cap_internal_test.go — white-box tests for the aggregate-byte
// budget on result materialisation (#1328). These assert what a black-box test
// cannot observe:
//   - the constructor wiring installs a *finite* default byte budget
//     (DefaultMaxResultBytes) rather than zero (unbounded);
//   - resolveMaxResultBytes maps the public EngineOptions.MaxResultBytes value
//     (zero / sentinel / positive) onto the internal budget correctly;
//   - the per-row size estimator is coarse, monotone in payload size, and
//     allocation-free (the property that makes it safe to run inside the
//     visibility barrier on the hot drain).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestResolveMaxResultBytes_Policy pins the zero/sentinel/positive mapping that
// resolveMaxResultBytes implements, mirroring resolveMaxResultRows.
func TestResolveMaxResultBytes_Policy(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero selects default", 0, DefaultMaxResultBytes},
		{"unlimited sentinel disables budget", MaxResultBytesUnlimited, 0},
		{"positive passes through", 4096, 4096},
		{"large positive passes through", DefaultMaxResultBytes + 1, DefaultMaxResultBytes + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxResultBytes(tc.in); got != tc.want {
				t.Fatalf("resolveMaxResultBytes(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewEngine_DefaultByteBudgetIsFinite is the core regression for #1328: the
// default constructor must install a non-zero internal byte budget so a wide-row
// result under the row cap cannot materialise unbounded bytes. Pre-fix the field
// did not exist / was 0 (unlimited); post-fix it equals DefaultMaxResultBytes.
func TestNewEngine_DefaultByteBudgetIsFinite(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	eng := NewEngine(g)
	if eng.maxResultBytes == 0 {
		t.Fatal("NewEngine installed an unbounded (zero) result-byte budget; want a finite default")
	}
	if eng.maxResultBytes != DefaultMaxResultBytes {
		t.Fatalf("NewEngine maxResultBytes = %d, want DefaultMaxResultBytes (%d)",
			eng.maxResultBytes, DefaultMaxResultBytes)
	}
}

// TestNewEngineWithOptions_ByteBudgetWiring covers the public knob's effect on
// the internal field for each interpreted band, including the unlimited opt-out.
func TestNewEngineWithOptions_ByteBudgetWiring(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	cases := []struct {
		name string
		opt  int64
		want int64
	}{
		{"default", 0, DefaultMaxResultBytes},
		{"unlimited opt-out", MaxResultBytesUnlimited, 0},
		{"explicit override", 65536, 65536},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngineWithOptions(g, EngineOptions{MaxResultBytes: tc.opt})
			if eng.maxResultBytes != tc.want {
				t.Fatalf("maxResultBytes = %d, want %d", eng.maxResultBytes, tc.want)
			}
		})
	}
}

// TestEstimateValueSize_Coarse pins the shape of the per-value estimate: scalars
// cost the flat overhead, a string adds its byte length, and containers add
// their elements' sizes. The exact figures are not a contract (the budget is
// coarse), but the relationships below are: bigger payload ⇒ bigger estimate.
func TestEstimateValueSize_Coarse(t *testing.T) {
	scalar := estimateValueSize(expr.IntegerValue(42))
	if scalar != perValueOverhead {
		t.Fatalf("scalar estimate = %d, want perValueOverhead (%d)", scalar, perValueOverhead)
	}

	short := estimateValueSize(expr.StringValue("abc"))
	long := estimateValueSize(expr.StringValue("abcdefghij"))
	if short != perValueOverhead+3 {
		t.Fatalf("short string estimate = %d, want %d", short, perValueOverhead+3)
	}
	if long <= short {
		t.Fatalf("string estimate not monotone: long %d <= short %d", long, short)
	}

	// A list of three scalars costs its own overhead plus each element's.
	list := estimateValueSize(expr.ListValue{
		expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3),
	})
	if want := perValueOverhead + 3*perValueOverhead; list != want {
		t.Fatalf("list estimate = %d, want %d", list, want)
	}

	// A node with a big string property must out-estimate a bare node.
	bare := estimateValueSize(expr.NodeValue{ID: 1})
	withProp := estimateValueSize(expr.NodeValue{
		ID:         1,
		Labels:     []string{"Person"},
		Properties: expr.MapValue{"name": expr.StringValue("a-long-enough-name")},
	})
	if withProp <= bare {
		t.Fatalf("node estimate not monotone in payload: withProp %d <= bare %d", withProp, bare)
	}
}

// TestEstimateValueSize_NilAndNull verifies the defensive paths: a nil property
// map and the NULL singleton both estimate without panicking.
func TestEstimateValueSize_NilAndNull(t *testing.T) {
	if got := estimateValueSize(expr.Null); got != perValueOverhead {
		t.Fatalf("null estimate = %d, want perValueOverhead (%d)", got, perValueOverhead)
	}
	// A node whose Properties map is nil ranges zero times — no panic, just the
	// node overhead plus the (empty) map overhead.
	n := expr.NodeValue{ID: 7} // Properties is the nil MapValue
	if got := estimateValueSize(n); got != 2*perValueOverhead {
		t.Fatalf("nil-property node estimate = %d, want %d", got, 2*perValueOverhead)
	}
	// A value that is not an expr.Value at all falls into the default branch.
	if got := estimateValueSize("a bare Go string, not an expr.Value"); got != perValueOverhead {
		t.Fatalf("non-expr.Value estimate = %d, want perValueOverhead (%d)", got, perValueOverhead)
	}
}

// TestEstimateRecordSize_AllocationFree is the performance contract that makes
// the byte budget safe to run inside the barrier-held drain: estimating a row's
// size must not allocate. A single regression here (e.g. calling Value.String())
// would show up as a non-zero alloc count.
func TestEstimateRecordSize_AllocationFree(t *testing.T) {
	rec := exec.Record{
		"i":    expr.IntegerValue(1),
		"s":    expr.StringValue("a moderately sized string value"),
		"list": expr.ListValue{expr.IntegerValue(1), expr.StringValue("x")},
		"node": expr.NodeValue{
			ID:         3,
			Labels:     []string{"A", "B"},
			Properties: expr.MapValue{"k": expr.StringValue("v")},
		},
	}
	cols := []string{"i", "s", "list", "node"}
	var sink int64
	allocs := testing.AllocsPerRun(1000, func() {
		sink += estimateRecordSize(cols, rec)
	})
	if allocs != 0 {
		t.Fatalf("estimateRecordSize allocated %.1f times/op, want 0 (must be allocation-free)", allocs)
	}
	if sink == 0 {
		t.Fatal("estimate summed to zero across runs; sink unexpectedly empty")
	}
}
