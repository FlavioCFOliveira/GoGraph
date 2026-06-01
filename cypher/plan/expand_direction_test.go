package plan_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/plan"
)

// expandEstimator is a configurable Estimator for expand-direction tests.
// AvgOutDegree is looked up by (src, rel, dst) triple.
type expandEstimator struct {
	labelCounts map[uint32]uint64
	degrees     map[[3]uint32]float64
}

func (e *expandEstimator) LabelCount(lbl uint32) uint64 {
	if e.labelCounts == nil {
		return 0
	}
	return e.labelCounts[lbl]
}

func (e *expandEstimator) HashLookupCount(_ uint32, _ any) uint64       { return 0 }
func (e *expandEstimator) BTreeRangeCount(_ uint32, _, _ string) uint64 { return 0 }

func (e *expandEstimator) AvgOutDegree(src, rel, dst uint32) float64 {
	if e.degrees == nil {
		return 1.0
	}
	if v, ok := e.degrees[[3]uint32{src, rel, dst}]; ok {
		return v
	}
	return 1.0
}

func TestSelectExpandDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		srcLabelID   uint32
		relTypeID    uint32
		dstLabelID   uint32
		est          *expandEstimator
		wantDir      plan.ExpandDirection
		wantFrontier float64
	}{
		{
			// Both sides have equal counts and degrees → OUT preferred on tie.
			name:       "symmetric_both_same",
			srcLabelID: 1,
			relTypeID:  10,
			dstLabelID: 2,
			est: &expandEstimator{
				labelCounts: map[uint32]uint64{1: 1000, 2: 1000},
				degrees: map[[3]uint32]float64{
					{1, 10, 2}: 5.0,
					{2, 10, 1}: 5.0,
				},
			},
			wantDir:      plan.ExpandOut,
			wantFrontier: 5000.0, // 1000 * 5.0
		},
		{
			// dst has far fewer nodes → expanding IN (from dst) is cheaper.
			name:       "dst_more_selective",
			srcLabelID: 1,
			relTypeID:  10,
			dstLabelID: 2,
			est: &expandEstimator{
				labelCounts: map[uint32]uint64{1: 10_000, 2: 100},
				degrees: map[[3]uint32]float64{
					{1, 10, 2}: 4.0,
					{2, 10, 1}: 4.0,
				},
			},
			wantDir:      plan.ExpandIn,
			wantFrontier: 400.0, // 100 * 4.0
		},
		{
			// src has far fewer nodes → expanding OUT is cheaper.
			name:       "src_more_selective",
			srcLabelID: 1,
			relTypeID:  10,
			dstLabelID: 2,
			est: &expandEstimator{
				labelCounts: map[uint32]uint64{1: 50, 2: 5_000},
				degrees: map[[3]uint32]float64{
					{1, 10, 2}: 3.0,
					{2, 10, 1}: 3.0,
				},
			},
			wantDir:      plan.ExpandOut,
			wantFrontier: 150.0, // 50 * 3.0
		},
		{
			// No labels → both sides default to count=1, degree=1.0 → tie → OUT.
			name:         "no_labels",
			srcLabelID:   0,
			relTypeID:    0,
			dstLabelID:   0,
			est:          &expandEstimator{},
			wantDir:      plan.ExpandOut,
			wantFrontier: 1.0,
		},
		{
			// Similar sizes but very different avg out-degrees → direction follows
			// the side with the lower frontier (min degree).
			name:       "degree_dominates",
			srcLabelID: 1,
			relTypeID:  10,
			dstLabelID: 2,
			est: &expandEstimator{
				labelCounts: map[uint32]uint64{1: 1000, 2: 1100},
				degrees: map[[3]uint32]float64{
					{1, 10, 2}: 50.0, // frontierOut = 1000 * 50 = 50_000
					{2, 10, 1}: 2.0,  // frontierIn  = 1100 * 2  = 2_200
				},
			},
			wantDir:      plan.ExpandIn,
			wantFrontier: 2200.0,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := plan.SelectExpandDirection(tc.srcLabelID, tc.relTypeID, tc.dstLabelID, tc.est)
			if got.Dir != tc.wantDir {
				t.Errorf("Dir = %v, want %v", got.Dir, tc.wantDir)
			}
			if got.EstimatedFrontier != tc.wantFrontier {
				t.Errorf("EstimatedFrontier = %f, want %f", got.EstimatedFrontier, tc.wantFrontier)
			}
		})
	}
}

// TestSelectExpandDirection_DegreeFloorOne verifies that a degree value below
// 1.0 is clamped to 1.0 and does not produce a negative or zero frontier.
func TestSelectExpandDirection_DegreeFloorOne(t *testing.T) {
	t.Parallel()

	est := &expandEstimator{
		labelCounts: map[uint32]uint64{1: 100, 2: 200},
		degrees: map[[3]uint32]float64{
			{1, 0, 2}: 0.001, // below floor
			{2, 0, 1}: 0.001,
		},
	}
	got := plan.SelectExpandDirection(1, 0, 2, est)
	if got.EstimatedFrontier < 1.0 {
		t.Errorf("EstimatedFrontier = %f; expected >= 1.0 with degree floor", got.EstimatedFrontier)
	}
}
