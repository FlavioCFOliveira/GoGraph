package cypher

// estimate_test.go — unit tests for the estimate-provenance infrastructure
// (#2076): the estSource trust classification, the estimate.trustworthy /
// planStaysDefault veto, and the exact label-cardinality provider over both its
// zero-alloc ResolveLabelCount path and its bitmap fallback.

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

func TestEstSource_Trustworthy(t *testing.T) {
	tests := []struct {
		name string
		src  estSource
		want bool
	}{
		{"exact is trustworthy", estExact, true},
		{"stats is trustworthy", estStats, true},
		{"heuristic is not trustworthy", estHeuristic, false},
		{"fallback is not trustworthy", estFallback, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimate{rows: 42, source: tt.src}.trustworthy()
			if got != tt.want {
				t.Fatalf("estimate{source:%v}.trustworthy() = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

func TestEstSource_String(t *testing.T) {
	tests := []struct {
		src  estSource
		want string
	}{
		{estExact, "exact"},
		{estStats, "stats"},
		{estHeuristic, "heuristic"},
		{estFallback, "fallback"},
		{estSource(200), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.src.String(); got != tt.want {
			t.Fatalf("estSource(%d).String() = %q, want %q", tt.src, got, tt.want)
		}
	}
}

// TestEstimate_ZeroValueIsExact documents that the zero-value source is estExact
// (iota == 0, per the design reference). The planner therefore NEVER relies on a
// zero-value estimate on a decision path — every estimate is constructed with an
// explicit source via labelCardinalityEstimate. This test pins that contract so
// a future reorder of the enum does not silently change the zero-value meaning.
func TestEstimate_ZeroValueIsExact(t *testing.T) {
	var e estimate
	if e.source != estExact {
		t.Fatalf("zero estimate.source = %v, want estExact (iota 0)", e.source)
	}
}

func TestPlanStaysDefault_Veto(t *testing.T) {
	tests := []struct {
		name string
		path []estimate
		want bool // true => keep default plan (vetoed)
	}{
		{
			name: "empty path is not vetoed",
			path: nil,
			want: false,
		},
		{
			name: "all exact clears the veto",
			path: []estimate{{rows: 10, source: estExact}, {rows: 20, source: estExact}},
			want: false,
		},
		{
			name: "exact + stats clears the veto",
			path: []estimate{{rows: 10, source: estExact}, {rows: 20, source: estStats}},
			want: false,
		},
		{
			name: "a single fallback vetoes the whole path",
			path: []estimate{{rows: 10, source: estExact}, {rows: 0, source: estFallback}},
			want: true,
		},
		{
			name: "a single unvalidated heuristic vetoes the whole path",
			path: []estimate{{rows: 10, source: estExact}, {rows: 5, source: estHeuristic}},
			want: true,
		},
		{
			name: "all untrustworthy vetoes",
			path: []estimate{{rows: 1e9, source: estFallback}, {rows: 3, source: estHeuristic}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planStaysDefault(tt.path...); got != tt.want {
				t.Fatalf("planStaysDefault(%v) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// fakeCounterResolver implements both labelResolverIface and labelCounter,
// modelling the production *lpgLabelResolver's zero-alloc count path.
type fakeCounterResolver struct {
	counts map[string]int64
}

func (f fakeCounterResolver) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	bm := roaring64.New()
	if n, ok := f.counts[name]; ok {
		for i := int64(0); i < n; i++ {
			bm.Add(uint64(i))
		}
	}
	return bm
}

func (f fakeCounterResolver) ResolveLabelCount(name string) (int64, bool) {
	// Mirror lpgLabelResolver: an unknown label is a live count of zero.
	return f.counts[name], true
}

// fakeBitmapOnlyResolver implements only labelResolverIface (no labelCounter),
// forcing labelCardinalityEstimate down its bitmap-cardinality fallback.
type fakeBitmapOnlyResolver struct {
	counts map[string]int64
}

func (f fakeBitmapOnlyResolver) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	bm := roaring64.New()
	if n, ok := f.counts[name]; ok {
		for i := int64(0); i < n; i++ {
			bm.Add(uint64(i))
		}
	}
	return bm
}

func TestLabelCardinalityEstimate(t *testing.T) {
	counter := fakeCounterResolver{counts: map[string]int64{"A": 100, "B": 5, "Empty": 0}}
	bitmapOnly := fakeBitmapOnlyResolver{counts: map[string]int64{"A": 100, "B": 5}}

	tests := []struct {
		name       string
		src        labelResolverIface
		label      string
		wantRows   float64
		wantSource estSource
	}{
		{"counter path, populated label", counter, "A", 100, estExact},
		{"counter path, small label", counter, "B", 5, estExact},
		{"counter path, empty label", counter, "Empty", 0, estExact},
		{"counter path, unknown label => exact zero", counter, "Missing", 0, estExact},
		{"bitmap fallback, populated label", bitmapOnly, "A", 100, estExact},
		{"bitmap fallback, small label", bitmapOnly, "B", 5, estExact},
		{"bitmap fallback, unknown label => exact zero", bitmapOnly, "Missing", 0, estExact},
		{"nil resolver => fallback", nil, "A", 0, estFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := labelCardinalityEstimate(tt.src, tt.label)
			if est.rows != tt.wantRows {
				t.Fatalf("rows = %v, want %v", est.rows, tt.wantRows)
			}
			if est.source != tt.wantSource {
				t.Fatalf("source = %v, want %v", est.source, tt.wantSource)
			}
		})
	}
}

// TestLabelCardinalityEstimate_VetoInteraction proves the exact provider always
// clears the trustworthiness veto (every count it produces is estExact), which
// is what lets the min-label scan deviate from the default plan.
func TestLabelCardinalityEstimate_VetoInteraction(t *testing.T) {
	src := fakeCounterResolver{counts: map[string]int64{"A": 100, "B": 5, "C": 0}}
	path := make([]estimate, 0, 4)
	path = append(path,
		labelCardinalityEstimate(src, "A"),
		labelCardinalityEstimate(src, "B"),
		labelCardinalityEstimate(src, "C"),
	)
	if planStaysDefault(path...) {
		t.Fatal("exact label cardinalities must clear the veto, but the path was vetoed")
	}

	// A nil resolver on the path re-arms the veto.
	path = append(path, labelCardinalityEstimate(nil, "A"))
	if !planStaysDefault(path...) {
		t.Fatal("a fallback estimate on the path must veto to the default plan")
	}
}
