package txn

// Regression gate for rmp #2784: an edge-handle record carrying more labels, or
// more properties, than store/snapshot can capture. Such a record COMMITS
// durably and then makes every checkpoint fail for as long as it lives, because
// phase-1 capture refuses it and the checkpointer returns before phase 3
// truncates the WAL prefix — so the WAL grows without bound.
//
// # What was measured, and what could not be
//
// The mechanism was established end to end against a build with
// store/snapshot's maxPerRecordCount lowered to 64: an edge handle carrying 65
// properties committed, three consecutive checkpoints then failed with
// "capture edgehandles.bin: ... edge handle property count is 65 bytes, maximum
// 64", and the WAL stayed at 3362 bytes across all three. The 64-property
// control committed, checkpointed, and truncated the WAL to 0. Deleting one
// property restored checkpointing and reclaimed the WAL.
//
// The same run AT THE REAL CAP is not in any layer, and deliberately. Building
// a handle that actually HOLDS 1 Mi properties costs on the order of eight hours
// of CPU on an Apple M4 — extrapolated, not guessed: the write path is quadratic
// in the handle's property count, and the measured series
//
//	16_000   6.27 s
//	32_000  25.13 s   (4.01x)
//	64_000 101.81 s   (4.05x)
//	128_000 452.45 s  (4.44x)
//
// puts 1_048_577 between 7.5 h (anchored on the 64 k point) and 8.4 h (on the
// 128 k point). The quadratic is lpg's, not this bound's: the MVCC pre-image
// clonePropBag copies the handle's whole bag on every write, and
// PropertyKeyRegistry.Intern copies the whole registry on every new key. Both
// are out of this task's scope.
//
// So the gate is split, the way rmp #2750's was:
//
//   - the REFUSAL is proven at the real cap, end to end through Commit, because
//     a refused transaction never applies its ops and therefore never pays the
//     quadratic (below);
//   - the NON-over-restriction is proven at the real cap against
//     checkFoldableHandleRecords itself, which is the whole of the decision;
//     that a transaction it admits then commits and checkpoints is proven
//     separately, at ordinary sizes, in store/checkpoint;
//   - that store/snapshot really does write an at-cap record and really does
//     refuse a cap+1 one is proven in
//     store/snapshot.TestEdgeHandleRecord_PerRecordCountBoundary_2784.

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// newHandleRecordTestStore is [newFoldableTestStore] with a weight codec, which
// AddEdgeWithHandle requires.
func newHandleRecordTestStore(t *testing.T) (*Store[string, float64], *lpg.Graph[string, float64], string, func()) {
	t.Helper()
	path := t.TempDir() + "/wal"
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := NewStoreWithOptions[string, float64](g, w, Options[string, float64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewFloat64WeightCodec(),
	})
	return st, g, path, func() { _ = w.Close() }
}

// seedHandleEdge commits one edge carrying a fresh stable handle and returns it.
func seedHandleEdge(t *testing.T, st *Store[string, float64], g *lpg.Graph[string, float64]) uint64 {
	t.Helper()
	tx := st.Begin()
	if err := tx.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := tx.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}
	h := g.NextEdgeHandle()
	if err := tx.AddEdgeWithHandle("a", "b", 1.0, h); err != nil {
		t.Fatalf("AddEdgeWithHandle: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed edge: %v", err)
	}
	return h
}

// TestSnapshotPerRecordCountCapAgreement_2784 is the drift tripwire for the
// number itself. store/txn cannot import store/snapshot, so the cap is declared
// twice; this pins this side to the literal and names the other, and
// store/snapshot's TestSnapshotPerRecordCountCapAgreement_2784 does the
// reciprocal.
func TestSnapshotPerRecordCountCapAgreement_2784(t *testing.T) {
	t.Parallel()

	if maxSnapshotPerRecordCount != 1<<20 {
		t.Fatalf("maxSnapshotPerRecordCount = %d, want 1<<20 (%d). This constant MUST "+
			"equal store/snapshot's maxPerRecordCount (store/snapshot/lenguard.go), which "+
			"is the ceiling readEdgeHandleRecord enforces and the largest per-handle "+
			"record edgehandles.bin can carry. Change both or neither (rmp #2784)",
			maxSnapshotPerRecordCount, 1<<20)
	}
}

// TestCheckSnapshotFoldableCount_Boundary_2784 pins the predicate at its exact
// boundary, so a later edit cannot tighten or loosen it by one unnoticed.
func TestCheckSnapshotFoldableCount_Boundary_2784(t *testing.T) {
	t.Parallel()

	if err := checkSnapshotFoldableCount("probe", 0); err != nil {
		t.Fatalf("checkSnapshotFoldableCount(0) = %v, want nil", err)
	}
	if err := checkSnapshotFoldableCount("probe", maxSnapshotPerRecordCount); err != nil {
		t.Fatalf("over-restricted: a count of exactly maxSnapshotPerRecordCount (%d) was "+
			"refused: %v", maxSnapshotPerRecordCount, err)
	}
	err := checkSnapshotFoldableCount("probe", maxSnapshotPerRecordCount+1)
	if err == nil {
		t.Fatalf("checkSnapshotFoldableCount(%d) = nil; store/snapshot's "+
			"writeEdgeHandleRecord refuses that count, so the record could never be "+
			"captured", maxSnapshotPerRecordCount+1)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
	}
	if got, want := err.Error(), "probe is 1048577 bytes, maximum 1048576"; !strings.Contains(got, want) {
		t.Fatalf("refusal message %q does not name the field and both counts (want it to "+
			"contain %q)", got, want)
	}
}

// TestCommitRefusesOverCapHandlePropertyRecord_2784 is the wiring proof, at the
// REAL cap: a transaction that would leave one edge handle carrying
// maxSnapshotPerRecordCount+1 properties is refused by Commit with the typed
// sentinel, and writes nothing durable.
//
// # Why this fixture is affordable and the at-cap one is not
//
// The refusal happens in appendOnly BEFORE a sequence is minted and BEFORE any
// op is applied, so none of the 1_048_577 staged ops ever reaches lpg. The
// quadratic that makes an at-cap graph cost hours — the MVCC pre-image clone and
// the copy-on-write key registry — lives entirely in the apply, which this test
// never performs. What it does pay is linear: staging the ops and walking them
// once.
//
// # What it looks like without the fix
//
// Before the guard this commit SUCCEEDED. Every checkpoint from then on failed
// with snapshot.ErrFieldTooLong out of phase-1 capture, and the WAL prefix was
// never truncated — so the assertion is on the refusal AND on the WAL being
// untouched, because a guard that refused after writing frames would have
// swapped one unbounded-WAL defect for another.
//
// Note for anyone neutralising the fix to check this test still bites: it does
// NOT fail fast without it. A commit that is admitted goes on to APPLY its
// 1_048_577 ops, which is the quadratic — about 7.6 hours — so the test surfaces
// as a package TIMEOUT rather than as a failed assertion. To exercise the logic
// quickly, lower maxSnapshotPerRecordCount here AND store/snapshot's
// maxPerRecordCount together; the whole file is written in terms of the
// constant. That is how the #2784 neutralisation battery was run.
func TestCommitRefusesOverCapHandlePropertyRecord_2784(t *testing.T) {
	st, g, walPath, cleanup := newHandleRecordTestStore(t)
	defer cleanup()
	h := seedHandleEdge(t, st, g)
	before := walSize(t, walPath)

	tx := st.Begin()
	for i := 0; i <= maxSnapshotPerRecordCount; i++ {
		if err := tx.SetEdgePropertyByHandle("a", "b", h, "k"+strconv.Itoa(i), lpg.Int64Value(1)); err != nil {
			t.Fatalf("SetEdgePropertyByHandle refused while staging at i=%d: %v", i, err)
		}
	}
	err := tx.Commit()
	if err == nil {
		t.Fatalf("Commit ACCEPTED an edge handle carrying %d properties against a %d cap. "+
			"store/snapshot's WriteEdgeHandles refuses that record, so every checkpoint "+
			"fails and the WAL prefix is never truncated (rmp #2784)",
			maxSnapshotPerRecordCount+1, maxSnapshotPerRecordCount)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("Commit refused with %v, which does not wrap txn.ErrFieldTooLong", err)
	}
	if !strings.Contains(err.Error(), "edge handle property count") {
		t.Errorf("refusal %q does not name the quantity it bounds", err.Error())
	}
	if after := walSize(t, walPath); after != before {
		t.Errorf("the refused transaction wrote %d bytes to the WAL; a refusal must make "+
			"nothing durable (was %d, now %d)", after-before, before, after)
	}
	if n := len(g.EdgePropertiesByHandle("a", "b", h)); n != 0 {
		t.Errorf("the refused transaction applied %d properties to the graph; a refusal "+
			"must apply nothing", n)
	}
}

// TestCommitRefusesOverCapHandleLabelRecord_2784 is the same wiring proof on the
// dimension the #2784 lead did not reproduce. store/snapshot bounds the label
// count and the property count of an edge-handle record with the SAME constant
// and the same check, and lpg's per-handle label bag accumulates distinct names
// exactly as the property bag accumulates keys, so the exposure is identical and
// so must the refusal be.
func TestCommitRefusesOverCapHandleLabelRecord_2784(t *testing.T) {
	st, g, walPath, cleanup := newHandleRecordTestStore(t)
	defer cleanup()
	h := seedHandleEdge(t, st, g)
	before := walSize(t, walPath)

	tx := st.Begin()
	for i := 0; i <= maxSnapshotPerRecordCount; i++ {
		if err := tx.SetEdgeLabelByHandle("a", "b", h, "L"+strconv.Itoa(i)); err != nil {
			t.Fatalf("SetEdgeLabelByHandle refused while staging at i=%d: %v", i, err)
		}
	}
	err := tx.Commit()
	if err == nil {
		t.Fatalf("Commit ACCEPTED an edge handle carrying %d labels against a %d cap "+
			"(rmp #2784)", maxSnapshotPerRecordCount+1, maxSnapshotPerRecordCount)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("Commit refused with %v, which does not wrap txn.ErrFieldTooLong", err)
	}
	if !strings.Contains(err.Error(), "edge handle label count") {
		t.Errorf("refusal %q does not name the quantity it bounds", err.Error())
	}
	if after := walSize(t, walPath); after != before {
		t.Errorf("the refused transaction wrote %d bytes to the WAL (was %d, now %d)",
			after-before, before, after)
	}
}

// TestCheckFoldableHandleRecords_AtCapAccepted_2784 is the
// bounds-rather-than-over-restricts control, at the real cap.
//
// It drives the guard rather than Commit, and that is the honest boundary of
// what can be asserted here: a transaction the guard ADMITS goes on to apply its
// ops, and applying 1 Mi properties to one handle costs about 7.6 hours on this
// hardware for reasons that have nothing to do with this bound (see the file
// comment). The guard's verdict is the whole of the decision this fix adds, so
// pinning the verdict at exactly the cap pins the over-restriction risk; that an
// admitted transaction then commits and checkpoints is proven at ordinary sizes
// in store/checkpoint.
func TestCheckFoldableHandleRecords_AtCapAccepted_2784(t *testing.T) {
	st, g, _, cleanup := newHandleRecordTestStore(t)
	defer cleanup()
	h := seedHandleEdge(t, st, g)

	tx := st.Begin()
	for i := 0; i < maxSnapshotPerRecordCount; i++ {
		if err := tx.SetEdgePropertyByHandle("a", "b", h, "k"+strconv.Itoa(i), lpg.Int64Value(1)); err != nil {
			t.Fatalf("staging at i=%d: %v", i, err)
		}
	}
	if err := tx.checkFoldableHandleRecords(); err != nil {
		t.Fatalf("over-restricted: an edge handle at exactly the cap (%d properties) was "+
			"refused: %v. store/snapshot writes that record — see "+
			"store/snapshot.TestEdgeHandleRecord_PerRecordCountBoundary_2784 — so refusing "+
			"it here is a new defect, not a fix (rmp #2784)", maxSnapshotPerRecordCount, err)
	}
	_ = tx.Rollback()
}

// TestCheckFoldableHandleRecords_Exactness_2784 pins the simulation against the
// ways a transaction can reach the SAME final count by different routes.
//
// A guard that counted staged Set ops instead of simulating them would refuse
// every one of the last three cases, each of which leaves a record store/snapshot
// writes without complaint. Over-restriction is a new defect, not a fix.
func TestCheckFoldableHandleRecords_Exactness_2784(t *testing.T) {
	const cap0 = maxSnapshotPerRecordCount

	cases := []struct {
		name        string
		stage       func(tx *Tx[string, float64], h uint64)
		wantRefused bool
	}{
		{
			name: "cap distinct keys is admitted",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, cap0)
			},
		},
		{
			name: "cap+1 distinct keys is refused",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, cap0+1)
			},
			wantRefused: true,
		},
		{
			name: "cap+1 ops over cap distinct keys is admitted (the last one overwrites)",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, cap0)
				_ = tx.SetEdgePropertyByHandle("a", "b", h, "k0", lpg.Int64Value(2))
			},
		},
		{
			name: "cap+1 keys with one deleted again is admitted",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, cap0+1)
				_ = tx.DelEdgePropertyByHandle("a", "b", h, "k0")
			},
		},
		{
			// The pre-retire keys are DISJOINT from the post-retire ones, so the
			// union is cap only if the retire is honoured and cap+10 if it is
			// not. Overlapping ranges here would make the case pass either way —
			// it did, until the #2784 neutralisation battery found the fixture
			// vacuous by removing the retire and seeing nothing fail.
			name: "cap keys after RemoveEdgeInstanceByHandle is admitted",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, 10)
				_ = tx.RemoveEdgeInstanceByHandle("a", "b", h)
				stageProps(tx, h, 1000, 1000+cap0)
			},
		},
		{
			name: "cap keys after RemoveEdgeByHandle is admitted",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, 10)
				_ = tx.RemoveEdgeByHandle("a", "b", h)
				stageProps(tx, h, 1000, 1000+cap0)
			},
		},
		{
			name: "a deleted key re-added still counts once",
			stage: func(tx *Tx[string, float64], h uint64) {
				stageProps(tx, h, 0, cap0+1)
				_ = tx.DelEdgePropertyByHandle("a", "b", h, "k0")
				_ = tx.SetEdgePropertyByHandle("a", "b", h, "k0", lpg.Int64Value(3))
			},
			wantRefused: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, g, _, cleanup := newHandleRecordTestStore(t)
			defer cleanup()
			h := seedHandleEdge(t, st, g)
			tx := st.Begin()
			tc.stage(tx, h)
			err := tx.checkFoldableHandleRecords()
			_ = tx.Rollback()
			switch {
			case tc.wantRefused && err == nil:
				t.Fatalf("the guard ADMITTED a record over the cap")
			case tc.wantRefused && !errors.Is(err, ErrFieldTooLong):
				t.Fatalf("refusal %v does not wrap ErrFieldTooLong", err)
			case !tc.wantRefused && err != nil:
				t.Fatalf("over-restricted: the guard refused a record at or under the cap: %v", err)
			}
		})
	}
}

// stageProps buffers Set ops for keys k<lo>..k<hi-1> on one handle.
func stageProps(tx *Tx[string, float64], h uint64, lo, hi int) {
	for i := lo; i < hi; i++ {
		_ = tx.SetEdgePropertyByHandle("a", "b", h, "k"+strconv.Itoa(i), lpg.Int64Value(1))
	}
}

// TestStagesHandleRecordGrowth_2784 pins the fast path that keeps the guard off
// every commit that cannot fire it.
//
// The check reads graph state, so it must not run for a transaction whose ops
// cannot grow a handle's record. This is the predicate that decides that, and it
// has to answer FALSE for the shrinking by-handle kinds as well as for every
// unrelated one — a predicate that answered TRUE for all of them would be a
// silent cost on the whole write path, and one that answered FALSE for a Set
// kind would be the defect itself.
func TestStagesHandleRecordGrowth_2784(t *testing.T) {
	t.Parallel()

	grows := []OpKind{OpSetEdgePropertyByHandle, OpSetEdgeLabelByHandle}
	inert := []OpKind{
		OpAddNode, OpSetNodeProperty, OpSetNodeLabel, OpAddEdgeWeighted,
		OpSetEdgeProperty, OpSetEdgeLabel, OpAddEdgeH, OpCreateIndex,
		OpDelEdgePropertyByHandle, OpRemoveEdgeInstanceByHandle, OpRemoveEdgeByHandle,
	}
	for _, k := range grows {
		if !stagesHandleRecordGrowth([]Op[string, float64]{{Kind: k}}) {
			t.Errorf("stagesHandleRecordGrowth(%v) = false; that kind grows a handle "+
				"record, so skipping the guard for it reopens rmp #2784", k)
		}
	}
	for _, k := range inert {
		if stagesHandleRecordGrowth([]Op[string, float64]{{Kind: k}}) {
			t.Errorf("stagesHandleRecordGrowth(%v) = true; that kind cannot grow a handle "+
				"record, so the guard would read graph state on a commit that cannot "+
				"fire it", k)
		}
	}
	if stagesHandleRecordGrowth([]Op[string, float64](nil)) {
		t.Error("stagesHandleRecordGrowth(nil) = true")
	}
	// A growing kind is found wherever it sits in the batch.
	batch := []Op[string, float64]{{Kind: OpAddNode}, {Kind: OpSetNodeProperty}, {Kind: OpSetEdgeLabelByHandle}}
	if !stagesHandleRecordGrowth(batch) {
		t.Error("stagesHandleRecordGrowth missed a growing kind that was not first")
	}
}

// walSize reports the current size of the WAL file at path.
func walSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return fi.Size()
}

// TestCheckFoldableHandleRecords_CountsExistingGraphState_2784 proves the guard
// reads the handle's CURRENT bag out of the graph and does not merely count what
// this transaction stages.
//
// That distinction is the whole reason the bound cannot live in the encoder
// beside rmp #2750's. A handle's record accumulates across commits: nothing
// obliges the properties that push it over the cap to arrive together, and
// DefaultMaxTxnOps (16 M) is no bound on a quantity that need not fit in one
// transaction at all. A guard that started from an empty set would admit every
// transaction that staged 1 Mi keys or fewer, however many the handle already
// carried — which is the defect, not the fix.
//
// The cases straddle the boundary from the same seeded state, so the test fails
// if the graph read is dropped (the over-cap case is then admitted) and also if
// it is double-counted (the at-cap case is then refused). BOTH dimensions are
// covered: store/snapshot bounds the label count and the property count with the
// same constant, and lpg accumulates both bags the same way, so a test that
// covered only properties would leave the label read unguarded — which is
// exactly what a neutralisation of the label read proved, silently passing every
// other test in this file.
func TestCheckFoldableHandleRecords_CountsExistingGraphState_2784(t *testing.T) {
	const seeded = 3

	for _, dim := range []struct {
		what  string
		seed  func(tx *Tx[string, float64], h uint64, lo, hi int)
		count func(g *lpg.Graph[string, float64], h uint64) int
	}{
		{
			what:  "properties",
			seed:  func(tx *Tx[string, float64], h uint64, lo, hi int) { stageProps(tx, h, lo, hi) },
			count: func(g *lpg.Graph[string, float64], h uint64) int { return len(g.EdgePropertiesByHandle("a", "b", h)) },
		},
		{
			what:  "labels",
			seed:  func(tx *Tx[string, float64], h uint64, lo, hi int) { stageLabels(tx, h, lo, hi) },
			count: func(g *lpg.Graph[string, float64], h uint64) int { return len(g.EdgeLabelsByHandle("a", "b", h)) },
		},
	} {
		for _, tc := range []struct {
			name        string
			stage       int // fresh names this transaction adds
			wantRefused bool
		}{
			{"lands exactly on the cap", maxSnapshotPerRecordCount - seeded, false},
			{"lands one over the cap", maxSnapshotPerRecordCount - seeded + 1, true},
		} {
			t.Run(dim.what+" "+tc.name, func(t *testing.T) {
				st, g, _, cleanup := newHandleRecordTestStore(t)
				defer cleanup()
				h := seedHandleEdge(t, st, g)

				seed := st.Begin()
				dim.seed(seed, h, 0, seeded)
				if err := seed.Commit(); err != nil {
					t.Fatalf("commit seed %s: %v", dim.what, err)
				}
				if got := dim.count(g, h); got != seeded {
					t.Fatalf("the handle carries %d %s after seeding, want %d — without the "+
						"seed this test cannot tell a graph read from an empty start",
						got, dim.what, seeded)
				}

				// Names disjoint from the seeded ones, so the union is exactly
				// seeded+stage and the boundary is where the case name says.
				tx := st.Begin()
				dim.seed(tx, h, seeded, seeded+tc.stage)
				err := tx.checkFoldableHandleRecords()
				_ = tx.Rollback()

				switch {
				case tc.wantRefused && err == nil:
					t.Fatalf("the guard ADMITTED %d staged %s onto a handle already carrying "+
						"%d, for a record of %d against a cap of %d. It is counting the batch "+
						"and not the graph, so a record that grows across commits never trips "+
						"it (rmp #2784)",
						tc.stage, dim.what, seeded, seeded+tc.stage, maxSnapshotPerRecordCount)
				case tc.wantRefused && !errors.Is(err, ErrFieldTooLong):
					t.Fatalf("refusal %v does not wrap ErrFieldTooLong", err)
				case !tc.wantRefused && err != nil:
					t.Fatalf("over-restricted: %d staged %s onto a handle carrying %d is a "+
						"record of exactly the cap (%d) and store/snapshot writes it: %v",
						tc.stage, dim.what, seeded, maxSnapshotPerRecordCount, err)
				}
			})
		}
	}
}

// stageLabels buffers SetEdgeLabelByHandle ops for L<lo>..L<hi-1> on one handle.
func stageLabels(tx *Tx[string, float64], h uint64, lo, hi int) {
	for i := lo; i < hi; i++ {
		_ = tx.SetEdgeLabelByHandle("a", "b", h, "L"+strconv.Itoa(i))
	}
}
