package exec_test

// reltype_admit_test.go — unit coverage for the two parts of the slot-aligned
// relationship-type column whose correctness rests on a branch that a typical
// fixture never reaches (rmp #2251): the admission mask's above-64 fallback, and
// the reverse column's decline path.
//
// Both are here because "it is obviously fine" is not evidence. The mask's
// single-word fast path covers LabelIDs 0..63 and every ordinary schema stays
// inside it, so the []uint64 fallback would ship untested unless something
// deliberately crossed the boundary. The decline path is worse than untested: it
// is the path a type check goes WRONG on, because the per-slot scan it falls back
// to has an unresolvable case, and answering that case permissively is exactly the
// defect rmp #2236 had to remove.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// code is [lpg.EncodeSlotLabel]: the on-slot encoding of a LabelID, biased by one
// so 0 can mean "this slot carries no relationship type".
func code(labelID uint32) uint32 { return labelID + 1 }

// TestRelTypeMask_HighLabelIDs CHECKS the []uint64 fallback rather than assuming
// it. LabelID space is shared between node labels and relationship types, so a
// schema with a few dozen of each reaches 64 without anything exotic happening.
func TestRelTypeMask_HighLabelIDs(t *testing.T) {
	// One arc per LabelID from 0 to 199, so every arc's code is distinct and the
	// position IS the LabelID. That spans the single-word range, the boundary, and
	// three further words of the fallback.
	const n = 200
	fwdCodes := make([]uint32, n)
	for i := range fwdCodes {
		fwdCodes[i] = code(uint32(i))
	}
	col := exec.NewRelTypeColumn(fwdCodes, nil, nil, nil)

	cases := []struct {
		name    string
		accept  []uint32
		wantIn  []uint64 // positions that must be admitted
		wantOut []uint64 // positions that must be rejected
	}{
		{"below the boundary", []uint32{code(0), code(7), code(63)},
			[]uint64{0, 7, 63}, []uint64{1, 64, 65, 199}},
		{"exactly at the boundary", []uint32{code(64)},
			[]uint64{64}, []uint64{63, 65, 0}},
		{"deep in the fallback", []uint32{code(128), code(199)},
			[]uint64{128, 199}, []uint64{127, 129, 64, 0}},
		{"straddling both halves", []uint32{code(3), code(70), code(150)},
			[]uint64{3, 70, 150}, []uint64{2, 69, 149, 63, 64}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admit := col.Admit(tc.accept)
			if len(tc.wantIn) == 0 {
				t.Fatalf("subtest %q names no admitted position, so it cannot discriminate", tc.name)
			}
			for _, pos := range tc.wantIn {
				if !admit.Fwd(pos) {
					t.Errorf("position %d (LabelID %d) REJECTED, want admitted — the "+
						"[]uint64 fallback is not covering this LabelID", pos, pos)
				}
			}
			for _, pos := range tc.wantOut {
				if admit.Fwd(pos) {
					t.Errorf("position %d (LabelID %d) ADMITTED, want rejected — the mask "+
						"is answering for a LabelID it was never given", pos, pos)
				}
			}
		})
	}

	// A mask naming only a high LabelID that NO arc carries must admit nothing: an
	// unallocated fast-path word must not read as "all set", and an out-of-range
	// fallback word must not read as "present".
	hi := col.Admit([]uint32{code(n)}) // LabelID 200: beyond every arc's code
	for pos := uint64(0); pos < n; pos++ {
		if hi.Fwd(pos) {
			t.Fatalf("a mask naming only LabelID %d admitted position %d", n, pos)
		}
	}
}

// TestRelTypeMask_ZeroAndUnknownAreNeverAccepted pins the two reserved code values.
// Code 0 means "this slot carries no relationship type" and the unknown sentinel
// means "the column could not answer"; neither is a type any pattern can name, so
// neither may ever be admitted — including by a mask that was handed them.
func TestRelTypeMask_ZeroAndUnknownAreNeverAccepted(t *testing.T) {
	fwdCodes := []uint32{0, exec.RelTypeUnknownCode, code(1)}
	col := exec.NewRelTypeColumn(fwdCodes, nil, nil, nil)
	admit := col.Admit([]uint32{0, exec.RelTypeUnknownCode, code(1)})
	if admit.Fwd(0) {
		t.Error("an UNTYPED slot (code 0) was admitted")
	}
	if admit.Fwd(1) {
		t.Error("a slot carrying the unknown sentinel was admitted on the forward side")
	}
	if !admit.Fwd(2) {
		t.Error("the genuinely typed slot was rejected, so this test cannot discriminate")
	}
}

// TestRelTypeColumn_TransposeDeclineIsNotPermissive is the load-bearing test for
// the reverse column's fallback.
//
// When the reverse CSR is not the canonical transpose of the forward one,
// [exec.NewRelTypeColumnFor] cannot use the replay and falls back to the per-slot
// scan, which HAS an unresolvable case. The requirement is that an unresolvable
// slot reports "I do not know" — so the operator recovers the position itself, and
// a two-sided search declines — and NEVER "admitted". A permissive stand-in there
// is indistinguishable from correct until someone counts, which is precisely how
// rmp #2220 routed a typed shortestPath over an excluded edge.
func TestRelTypeColumn_TransposeDeclineIsNotPermissive(t *testing.T) {
	// Forward: 0→1 (type A), 1→2 (type B).
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	fwdCodes := []uint32{code(0), code(1)}

	// Positive control FIRST: the real transpose must be accepted and EXACT, so a
	// decline below is a property of the malformed input and not of the fixture.
	realRev := buildCSR(3, [][2]int{{1, 0}, {2, 1}})
	good := exec.NewRelTypeColumnFor(fwd, realRev, fwdCodes, nil)
	goodAdmit := good.Admit([]uint32{code(0)})
	if !goodAdmit.RevExact() {
		t.Fatal("positive control FAILED: the canonical transpose was not accepted, so a " +
			"decline in the subject arm proves nothing")
	}

	// Subject: a reverse CSR with a THIRD arc, so the edge counts differ and the
	// transpose replay declines outright. One of its slots (2←0) has no forward
	// counterpart at all: node 0 has no arc to node 2.
	badRev := buildCSR(3, [][2]int{{1, 0}, {2, 1}, {2, 0}})
	col := exec.NewRelTypeColumnFor(fwd, badRev, fwdCodes, nil)
	admit := col.Admit([]uint32{code(0), code(1)})

	if admit.RevExact() {
		t.Error("RevExact() is true over a reverse CSR that is not the transpose — a " +
			"two-sided typed search would run on a column that cannot answer every slot")
	}

	unknown, resolved := 0, 0
	revEdges, revVerts := badRev.EdgesSlice(), badRev.VerticesSlice()
	for owner := uint64(0); owner+1 < uint64(len(revVerts)); owner++ {
		for revPos := revVerts[owner]; revPos < revVerts[owner+1]; revPos++ {
			gotAdmit, known := admit.Rev(revPos)
			src := uint64(revEdges[revPos])
			// The slot (owner=2, src=0) has no forward counterpart: node 0's only arc
			// goes to node 1.
			hasCounterpart := owner != 2 || src != 0
			switch {
			case !known:
				unknown++
				if gotAdmit {
					t.Errorf("revPos=%d reported admit=true together with known=false; an "+
						"unresolvable slot must never carry an admission", revPos)
				}
				if hasCounterpart {
					t.Errorf("revPos=%d (%d→%d) is resolvable but the column declined it",
						revPos, src, owner)
				}
			default:
				resolved++
				if !hasCounterpart {
					t.Errorf("revPos=%d (%d→%d) has NO forward counterpart, yet the column "+
						"answered %v for it — that is the permissive answer #2236 removed",
						revPos, src, owner, gotAdmit)
				}
			}
		}
	}
	if unknown == 0 {
		t.Error("no slot was reported unknown, so the decline path was never exercised and " +
			"this test is vacuous")
	}
	if resolved == 0 {
		t.Error("no slot was resolved either, so the fallback is answering nothing at all " +
			"rather than answering exactly what it can")
	}
	t.Logf("decline path: %d slot(s) resolved by the per-slot scan, %d reported unknown",
		resolved, unknown)
}

// TestRelTypeColumn_NilReverseFallsBackRatherThanAdmitting pins the other absent
// case: a caller holding only a forward CSR. Every reverse slot must be unknown.
func TestRelTypeColumn_NilReverseFallsBackRatherThanAdmitting(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	col := exec.NewRelTypeColumnFor(fwd, nil, []uint32{code(0), code(1)}, nil)
	admit := col.Admit([]uint32{code(0), code(1)})
	if admit.RevExact() {
		t.Error("RevExact() is true with no reverse adjacency at all")
	}
	for revPos := uint64(0); revPos < 4; revPos++ {
		if gotAdmit, known := admit.Rev(revPos); known || gotAdmit {
			t.Errorf("revPos=%d answered (admit=%v known=%v) with a nil reverse column",
				revPos, gotAdmit, known)
		}
	}
}

// TestRelTypeColumn_MultiTypeArcPatchList pins the sparse exception map: an arc
// carrying more than one relationship type must be admitted by a pattern naming
// ANY of them, including one that is only in the patch list.
func TestRelTypeColumn_MultiTypeArcPatchList(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 1}})
	// Arc 0 carries A (dense) plus B and C (patch list); arc 1 carries only D.
	fwdCodes := []uint32{code(0), code(3)}
	fwdExtra := map[uint64][]uint32{0: {code(1), code(2)}}
	col := exec.NewRelTypeColumnFor(fwd, rev, fwdCodes, fwdExtra)

	for _, tc := range []struct {
		name   string
		accept []uint32
		want0  bool
		want1  bool
	}{
		{"dense type of the multi-type arc", []uint32{code(0)}, true, false},
		{"first patch type", []uint32{code(1)}, true, false},
		{"second patch type", []uint32{code(2)}, true, false},
		{"the other arc's type only", []uint32{code(3)}, false, true},
		{"a type nothing carries", []uint32{code(9)}, false, false},
		{"disjunction over a patch type and the other arc", []uint32{code(2), code(3)}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admit := col.Admit(tc.accept)
			if got := admit.Fwd(0); got != tc.want0 {
				t.Errorf("forward arc 0: admit=%v want %v", got, tc.want0)
			}
			if got := admit.Fwd(1); got != tc.want1 {
				t.Errorf("forward arc 1: admit=%v want %v", got, tc.want1)
			}
			// The reverse projection must carry the patch list across the transpose:
			// reverse slot 0 is arc 0's counterpart, reverse slot 1 is arc 1's.
			if got, known := admit.Rev(0); !known || got != tc.want0 {
				t.Errorf("reverse slot 0: admit=%v known=%v want admit=%v known=true",
					got, known, tc.want0)
			}
			if got, known := admit.Rev(1); !known || got != tc.want1 {
				t.Errorf("reverse slot 1: admit=%v known=%v want admit=%v known=true",
					got, known, tc.want1)
			}
		})
	}
}
