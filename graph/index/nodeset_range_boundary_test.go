package index

import (
	"math"
	"testing"
)

// nodeset_range_boundary_test.go — regression cover for #2607.
//
// AddRange and RemoveRange are documented over the CLOSED interval [from, to],
// but roaring64's own range API is half-open and its element type is uint64, so
// there is no value that can name "one past MaxUint64". The conversion used to
// be a plain `to+1`, which wraps to 0 at the top of the range; roaring returns
// immediately when start >= end, so the whole call became a silent no-op. The
// bug was invisible from the query surface and the two tiers disagreed about it,
// because the inline branches of RemoveRange filter on the closed interval
// directly and were always correct.
//
// Every case below is anchored at the top of the uint64 range and paired with a
// max-1 control that the defective code already answered correctly, so a failure
// localises to the boundary rather than to range handling in general.

// bitmapTierSet returns a NodeSet holding ids that is guaranteed to be on the
// roaring tier, without going through AddRange (the method under test).
func bitmapTierSet(t *testing.T, ids ...uint64) *NodeSet {
	t.Helper()
	var s NodeSet
	// AddRange is the only entry point that promotes without crossing
	// smallSetMax, so promote by adding a spread of ids and then removing the
	// ones that are not wanted. This keeps the fixture independent of the
	// method under test.
	for i := uint64(0); i <= smallSetMax; i++ {
		s.Add(i)
	}
	if s.tag() != stateBitmap {
		t.Fatalf("fixture did not reach the bitmap tier: tag = %d", s.tag()>>0)
	}
	for i := uint64(0); i <= smallSetMax; i++ {
		s.Remove(i)
	}
	for _, id := range ids {
		s.Add(id)
	}
	if s.tag() != stateBitmap {
		t.Fatalf("fixture left the bitmap tier: tag = %d", s.tag()>>0)
	}
	return &s
}

// inlineTierSet returns a NodeSet holding ids on an inline (non-bitmap) tier.
func inlineTierSet(t *testing.T, ids ...uint64) *NodeSet {
	t.Helper()
	if len(ids) > smallSetMax {
		t.Fatalf("fixture of %d ids cannot stay inline (smallSetMax = %d)", len(ids), smallSetMax)
	}
	var s NodeSet
	for _, id := range ids {
		s.Add(id)
	}
	if s.tag() == stateBitmap {
		t.Fatalf("fixture reached the bitmap tier with %d ids", len(ids))
	}
	return &s
}

func TestNodeSetAddRangeAtMaxUint64(t *testing.T) {
	const max = uint64(math.MaxUint64)

	tests := []struct {
		name string
		from uint64
		to   uint64
		want []uint64
	}{
		{
			name: "six ids ending at MaxUint64",
			from: max - 5,
			to:   max,
			want: []uint64{max - 5, max - 4, max - 3, max - 2, max - 1, max},
		},
		{
			// The control the defective code already answered correctly.
			name: "control: five ids ending at MaxUint64-1",
			from: max - 5,
			to:   max - 1,
			want: []uint64{max - 5, max - 4, max - 3, max - 2, max - 1},
		},
		{
			name: "degenerate single id at MaxUint64",
			from: max,
			to:   max,
			want: []uint64{max},
		},
		{
			name: "control: degenerate single id below the top",
			from: max - 1,
			to:   max - 1,
			want: []uint64{max - 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s NodeSet
			s.AddRange(tt.from, tt.to)

			if got, want := s.Cardinality(), uint64(len(tt.want)); got != want {
				t.Errorf("Cardinality = %d, want %d", got, want)
			}
			got := s.ToArray()
			if len(got) != len(tt.want) {
				t.Fatalf("ToArray = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("ToArray[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
			for _, id := range tt.want {
				if !s.Contains(id) {
					t.Errorf("Contains(%d) = false, want true", id)
				}
			}
		})
	}
}

func TestNodeSetRemoveRangeAtMaxUint64BothTiers(t *testing.T) {
	const max = uint64(math.MaxUint64)

	// Five members straddling the top of the range. Small enough to stay inline,
	// so the same membership can be presented on either tier.
	members := []uint64{max - 4, max - 3, max - 2, max - 1, max}

	tests := []struct {
		name string
		from uint64
		to   uint64
		want []uint64
	}{
		{
			name: "remove the top four",
			from: max - 3,
			to:   max,
			want: []uint64{max - 4},
		},
		{
			// The control the defective code already answered correctly.
			name: "control: remove three below the top",
			from: max - 3,
			to:   max - 1,
			want: []uint64{max - 4, max},
		},
		{
			name: "remove every member, ending at MaxUint64",
			from: max - 4,
			to:   max,
			want: nil,
		},
		{
			name: "remove the single top id",
			from: max,
			to:   max,
			want: []uint64{max - 4, max - 3, max - 2, max - 1},
		},
	}

	tiers := []struct {
		name string
		make func(*testing.T, ...uint64) *NodeSet
	}{
		{"inline", inlineTierSet},
		{"bitmap", bitmapTierSet},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Both tiers must answer identically: the same logical operation on
			// the same membership cannot depend on state the public surface
			// does not expose.
			results := make(map[string][]uint64, len(tiers))
			for _, tier := range tiers {
				s := tier.make(t, members...)
				nowEmpty := s.RemoveRange(tt.from, tt.to)

				got := s.ToArray()
				results[tier.name] = got

				if len(got) != len(tt.want) {
					t.Errorf("%s tier: ToArray = %v (len %d), want %v (len %d)",
						tier.name, got, len(got), tt.want, len(tt.want))
				} else {
					for i := range tt.want {
						if got[i] != tt.want[i] {
							t.Errorf("%s tier: ToArray[%d] = %d, want %d", tier.name, i, got[i], tt.want[i])
						}
					}
				}
				if want := len(tt.want) == 0; nowEmpty != want {
					t.Errorf("%s tier: RemoveRange reported nowEmpty = %v, want %v", tier.name, nowEmpty, want)
				}
			}

			inline, bitmap := results["inline"], results["bitmap"]
			if len(inline) != len(bitmap) {
				t.Fatalf("tier divergence: inline = %v, bitmap = %v", inline, bitmap)
			}
			for i := range inline {
				if inline[i] != bitmap[i] {
					t.Fatalf("tier divergence at %d: inline = %v, bitmap = %v", i, inline, bitmap)
				}
			}
		})
	}
}
