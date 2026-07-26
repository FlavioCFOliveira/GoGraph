package lpg

// edge_property_fused_pack_test.go — rmp #2171.
//
// [edgePropCols.Compact] attempts frame-of-reference date packing via
// maybePackDate, which requires a DENSE column. The fused append path — the one
// taken when an edge is created together with its property in a single call —
// builds and keeps its column in the SPARSE coordinate-list form, and nothing on
// that path ever reshaped it. Packing therefore never fired there at all, even
// for a fully-populated narrow-range column that the set-after path packed
// without difficulty.
//
// Measured before the fix: 30.5% waste at degree 324 and 24.3% at degree 64,
// for byte-identical content. The aggravating detail is that the only production
// caller of the fused path is examples/26_social_scale_bench, so the flagship
// evidence example was measuring the worst of the two paths.
//
// Compact now reshapes before packing. These tests compare the two paths on
// identical content and require the same physical form and the same byte count,
// which is a deterministic assertion rather than a heap sample — the resident
// measurements in edge_property_sparse_mem_test.go are coarse by nature and live
// in the soak layer.
//
// Layer: short.

import (
	"math"
	"testing"
)

// colPhysicalBytes accounts a column's WHOLE physical footprint: the value
// backing, the coordinate-list index when sparse, the validity bitmap when one
// is carried, and the frame-of-reference header when packed.
//
// datesColBytes (edge_property_column_test.go) counts only the value backing,
// which would flatter the sparse form by hiding its idx sidecar — exactly the
// cost this task is about — so the comparison here needs the fuller account.
func colPhysicalBytes(col *edgePropColumn) int {
	n := cap(col.idx)*4 + cap(col.valid)*8
	if col.packedDate {
		return n + cap(col.packed)*8 + forHeaderBytes
	}
	return n + cap(col.days)*4
}

// buildFusedDateColumn builds a block exactly as the fused append path does: the
// first present slot through the aux factory adjlist invokes for a node's first
// edge, then one GrowSlotWithValue per subsequent edge.
func buildFusedDateColumn(t *testing.T, key PropertyKeyID, days []int32) *edgePropCols {
	t.Helper()
	first, ok := newEdgePropColsAux(1, &edgePropPayload{keyID: key, value: dateVal(days[0])}).(*edgePropCols)
	if !ok || first == nil {
		t.Fatal("newEdgePropColsAux did not return a block")
	}
	block := first
	for i := 1; i < len(days); i++ {
		next, ok := block.GrowSlotWithValue(i, &edgePropPayload{keyID: key, value: dateVal(days[i])}).(*edgePropCols)
		if !ok || next == nil {
			t.Fatalf("GrowSlotWithValue returned no block at slot %d", i)
		}
		block = next
	}
	if block.length != len(days) {
		t.Fatalf("fused block length = %d, want %d", block.length, len(days))
	}
	return block
}

// benchDays returns a narrow-range epoch-day series: a ~6-year window whose
// residual fits in 12 bits, which is the example-26 shape and the range the
// byte gate accepts.
func benchDays(n int) []int32 {
	const base = int32(18276) // 2020-01-15
	days := make([]int32, n)
	for i := range days {
		days[i] = base + int32((i*53)%2193)
	}
	return days
}

// TestFusedPack_FusedMatchesSetAfter is the acceptance test for #2171: for
// identical content the fused path must end up in the same physical form, and at
// the same byte count, as the set-after path.
//
// Degrees 64 and 324 are the two the audit measured.
func TestFusedPack_FusedMatchesSetAfter(t *testing.T) {
	t.Parallel()
	for _, degree := range []int{64, 324} {
		days := benchDays(degree)
		t.Run("degree"+itoa(degree), func(t *testing.T) {
			const key = PropertyKeyID(11)

			fused := buildFusedDateColumn(t, key, days)
			setAfter := buildFullDenseDateColumn(t, key, days)

			// Precondition: the two paths really do start in different physical
			// forms, so the comparison below is not vacuous.
			if !findCol(t, fused, key).sparse {
				t.Fatal("fused column is not sparse before Compact; the fixture no longer exercises the defect")
			}
			if findCol(t, setAfter, key).sparse {
				t.Fatal("set-after column is unexpectedly sparse before Compact")
			}

			fc := findCol(t, fused.Compact().(*edgePropCols), key)
			sc := findCol(t, setAfter.Compact().(*edgePropCols), key)

			if !fc.packedDate {
				t.Fatalf("fused column was NOT packed at Compact; before #2171 it stayed sparse "+
					"and unpacked, costing %d bytes against the set-after path's %d",
					colPhysicalBytes(fc), colPhysicalBytes(sc))
			}
			if !sc.packedDate {
				t.Fatal("set-after column was not packed at Compact; the reference path regressed")
			}
			if fc.forWidth != sc.forWidth {
				t.Fatalf("forWidth: fused %d, set-after %d — same content must pack to the same width",
					fc.forWidth, sc.forWidth)
			}
			if fc.forMin != sc.forMin {
				t.Fatalf("forMin: fused %d, set-after %d", fc.forMin, sc.forMin)
			}
			// The fused path must no longer cost MORE than the set-after path.
			// It now costs slightly LESS, and deliberately so rather than by
			// accident: toDense drops the validity bitmap when every slot is
			// present (full == true), whereas the dense build path allocates one
			// regardless, so the set-after column carries words(length) unneeded
			// bytes — 8 at degree 64, 48 at degree 324. That residue is a
			// separate, much smaller inefficiency in the set-after path and is
			// tracked on its own; asserting equality here would wrongly demand
			// that the fused path reproduce it.
			fb, sb := colPhysicalBytes(fc), colPhysicalBytes(sc)
			if fb > sb {
				t.Fatalf("physical bytes: fused %d > set-after %d (%.2f vs %.2f B/edge)",
					fb, sb, float64(fb)/float64(degree), float64(sb)/float64(degree))
			}
			// Guard the claim above: the whole difference must be the bitmap, so
			// the value planes themselves must be byte-identical.
			if fc.forWidth == sc.forWidth {
				if got, want := cap(fc.packed)*8, cap(sc.packed)*8; got != want {
					t.Fatalf("packed value plane differs: fused %d bytes, set-after %d", got, want)
				}
			}
			t.Logf("degree %d: fused %.3f B/edge, set-after %.3f B/edge (forWidth=%d)",
				degree, float64(fb)/float64(degree), float64(sb)/float64(degree), fc.forWidth)

			// Content must survive the added reshape unchanged on both paths.
			for i := 0; i < degree; i++ {
				fv, fok := fc.slotValue(i)
				sv, sok := sc.slotValue(i)
				if fok != sok {
					t.Fatalf("slot %d presence: fused %v, set-after %v", i, fok, sok)
				}
				if !fok {
					continue
				}
				fs, _ := fv.String()
				ss, _ := sv.String()
				if fs != ss {
					t.Fatalf("slot %d value: fused %q, set-after %q", i, fs, ss)
				}
				want := dateVal(days[i])
				ws, _ := want.String()
				if fs != ws {
					t.Fatalf("slot %d value %q, want %q", i, fs, ws)
				}
			}
		})
	}
}

// TestFusedPack_CompactIsIdempotent guards the hazard the fix introduces:
// reshaped() UNPACKS a packed column, so a Compact that reshaped
// unconditionally would unpack on a second freeze and, for a low-fill column,
// could then demote it to sparse and leave packing permanently off. Compact skips
// reshaping an already-packed column for exactly this reason.
func TestFusedPack_CompactIsIdempotent(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(12)
	days := benchDays(324)

	once := buildFusedDateColumn(t, key, days).Compact().(*edgePropCols)
	twice := once.Compact().(*edgePropCols)

	oc := findCol(t, once, key)
	tc := findCol(t, twice, key)
	if !oc.packedDate {
		t.Fatal("first Compact did not pack")
	}
	if !tc.packedDate {
		t.Fatal("second Compact UNPACKED the column; a repeated freeze must be idempotent")
	}
	if oc.forWidth != tc.forWidth || oc.forMin != tc.forMin {
		t.Fatalf("second Compact changed the packing: width %d->%d, min %d->%d",
			oc.forWidth, tc.forWidth, oc.forMin, tc.forMin)
	}
	if got, want := colPhysicalBytes(tc), colPhysicalBytes(oc); got != want {
		t.Fatalf("second Compact changed the footprint: %d -> %d bytes", want, got)
	}
	for i := 0; i < len(days); i++ {
		ov, ook := oc.slotValue(i)
		tv, tok := tc.slotValue(i)
		if ook != tok {
			t.Fatalf("slot %d presence changed on re-Compact", i)
		}
		if !ook {
			continue
		}
		os, _ := ov.String()
		ts, _ := tv.String()
		if os != ts {
			t.Fatalf("slot %d value changed on re-Compact: %q -> %q", i, os, ts)
		}
	}
}

// TestFusedPack_WideRangeStaysUnpacked pins that the reshape did not weaken the
// byte gate: a date range too wide to pack profitably must stay a plain dense
// column on BOTH paths, rather than being packed at a loss.
func TestFusedPack_WideRangeStaysUnpacked(t *testing.T) {
	t.Parallel()
	const key = PropertyKeyID(13)
	const degree = 324
	// The residual must need at least 32 bits for the gate to reject outright, so
	// the range has to span the whole int32 domain. A merely wide range is not
	// enough: 31 bits still undercuts a plain int32 backing and packs profitably,
	// which is correct and was worth discovering — the gate is about bytes saved,
	// not about the range looking narrow.
	days := make([]int32, degree)
	for i := range days {
		if i%2 == 0 {
			days[i] = math.MinInt32
		} else {
			days[i] = math.MaxInt32
		}
	}

	fc := findCol(t, buildFusedDateColumn(t, key, days).Compact().(*edgePropCols), key)
	sc := findCol(t, buildFullDenseDateColumn(t, key, days).Compact().(*edgePropCols), key)

	if fc.packedDate {
		t.Fatal("fused wide-range column was packed; the byte gate must reject it")
	}
	if sc.packedDate {
		t.Fatal("set-after wide-range column was packed; the byte gate must reject it")
	}
	// Both must nevertheless agree on the physical form, which is the point of
	// reshaping at freeze.
	if fc.sparse != sc.sparse {
		t.Fatalf("sparse: fused %v, set-after %v — the two paths must converge", fc.sparse, sc.sparse)
	}
	if fb, sb := colPhysicalBytes(fc), colPhysicalBytes(sc); fb > sb {
		t.Fatalf("physical bytes: fused %d > set-after %d", fb, sb)
	}
	// Both hold the same plain int32 value plane; only the set-after path's
	// unnecessary validity bitmap separates their totals, as in
	// TestFusedPack_FusedMatchesSetAfter.
	if got, want := cap(fc.days)*4, degree*4; got != want {
		t.Fatalf("fused value plane = %d bytes, want %d (one int32 per slot)", got, want)
	}
}
