package cypher

// result_rows_cap_internal_test.go — white-box tests for the finite default
// result-row cap (#1292). These assert the constructor wiring that a black-box
// test cannot observe: that NewEngine / NewEngineWithStore inherit a *non-zero*
// internal cap (DefaultMaxResultRows) instead of the previously-unbounded zero,
// and that resolveMaxResultRows maps the public EngineOptions.MaxResultRows
// value (zero / sentinel / positive) onto the internal cap correctly.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestResolveMaxResultRows_Policy pins the zero/sentinel/positive mapping that
// resolveMaxResultRows implements. Zero selects the finite default, the
// unlimited sentinel maps to the internal "no limit" zero, and any positive
// value passes through verbatim.
func TestResolveMaxResultRows_Policy(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero selects default", 0, DefaultMaxResultRows},
		{"unlimited sentinel disables cap", MaxResultRowsUnlimited, 0},
		{"positive passes through", 42, 42},
		{"large positive passes through", DefaultMaxResultRows + 1, DefaultMaxResultRows + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxResultRows(tc.in); got != tc.want {
				t.Fatalf("resolveMaxResultRows(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewEngine_DefaultRowCapIsFinite is the core regression for #1292: the
// default constructor must install a non-zero internal cap so an unbounded
// MATCH cannot materialise every row. Pre-fix the field was 0 (unlimited) and
// this assertion failed; post-fix it equals DefaultMaxResultRows.
func TestNewEngine_DefaultRowCapIsFinite(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	eng := NewEngine(g)
	if eng.maxResultRows == 0 {
		t.Fatal("NewEngine installed an unbounded (zero) result-row cap; want a finite default")
	}
	if eng.maxResultRows != DefaultMaxResultRows {
		t.Fatalf("NewEngine maxResultRows = %d, want DefaultMaxResultRows (%d)",
			eng.maxResultRows, DefaultMaxResultRows)
	}
}

// TestNewEngineWithOptions_RowCapWiring covers the public knob's effect on the
// internal field for each interpreted band, including the unlimited opt-out.
func TestNewEngineWithOptions_RowCapWiring(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	cases := []struct {
		name string
		opt  int64
		want int64
	}{
		{"default", 0, DefaultMaxResultRows},
		{"unlimited opt-out", MaxResultRowsUnlimited, 0},
		{"explicit override", 1000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: tc.opt})
			if eng.maxResultRows != tc.want {
				t.Fatalf("maxResultRows = %d, want %d", eng.maxResultRows, tc.want)
			}
		})
	}
}

// TestEngine_ResultRowCap_MirrorsInternalField is the contract test for the
// exported accessor the Bolt server uses to detect an uncapped engine (#1293):
// ResultRowCap must return the resolved internal cap verbatim — a positive value
// when capped and zero when the engine was built unlimited. The black-box
// signature cannot read maxResultRows, so this white-box test pins the mirror.
func TestEngine_ResultRowCap_MirrorsInternalField(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	cases := []struct {
		name string
		opt  int64
		want int64
	}{
		{"default is finite", 0, DefaultMaxResultRows},
		{"unlimited opt-out reports zero", MaxResultRowsUnlimited, 0},
		{"explicit override reported verbatim", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: tc.opt})
			if got := eng.ResultRowCap(); got != tc.want {
				t.Fatalf("ResultRowCap() = %d, want %d", got, tc.want)
			}
			if eng.ResultRowCap() != eng.maxResultRows {
				t.Fatalf("ResultRowCap() = %d must mirror internal maxResultRows = %d",
					eng.ResultRowCap(), eng.maxResultRows)
			}
		})
	}
}
