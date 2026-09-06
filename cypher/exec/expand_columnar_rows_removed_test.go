package exec_test

// expand_columnar_rows_removed_test.go — the rows-removed gate on Expand's
// COLUMNAR path (rmp #2764).
//
// # Why this file exists: a per-site deletion sweep, not a hunch
//
// rmp #2764's counter has eight increment sites. Each was deleted individually and
// the four gate packages re-run. Six deletions were caught. TWO SURVIVED, both on
// the columnar FillChunk path:
//
//	S5  fillOneChunkRow, forward edgeSkip   -> go test ./cypher/ ./cypher/exec/
//	S6  fillOneChunkRow, reverse edgeSkip      ./cypher/explain/ ./bolt/server/  EXIT=0
//
// The reason is precise and worth recording, because it is the same reason the
// reverse-cursor sites went unproved before them. The columnar path shares its
// edge DECISIONS with the row path (advanceFwdEdge / advanceRevEdge) but not its
// ACCOUNTING: fillOneChunkRow charges the rejection itself, at its own two
// `continue` arms. So every gate that drives the row path — which was all of them
// — leaves the columnar charge untouched, and a test that drives the columnar path
// proves nothing unless it reads the REMOVED figure specifically.
// TestProfileDbHits_ColumnarExpandCountsSlotsWalked (rmp #2761) does drive a
// type-filtered expand through FillChunk, and asserts storageAccesses; it says
// nothing about rejections.
//
// # What is asserted, and why through PlanTree
//
// rowsRemovedByFilter and storageAccesses are unexported, so this external test
// reads both off [exec.PlanNode] through a [exec.Profiler] — which is the surface
// every renderer uses, so a counter that is right but unreachable through the
// wrapper fails here too.
//
// Each direction is a SUBJECT/CONTROL pair over one fixture. Both arms walk the
// SAME 100 adjacency slots; only the type filter differs. The control rejects
// nothing and must report `removed=0`, the subject rejects 99, and both must
// satisfy
//
//	DbHits == Rows + RowsRemovedByFilter
//
// whose two sides are counted by unrelated state — the cursor positions and the
// reject branches. A control that reported the subject's figure, or a subject
// whose identity did not close, fails.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// columnarRejectFixture builds a star of `deg` edges between node 0 and nodes
// 1..deg, oriented by `out`, plus a type filter admitting exactly ONE of them.
//
// The filter map is an ACCEPT-LIST keyed by forward-CSR position, not a labelling:
// relTypeAdmitFromPositions (cypher/exec/reltype_admit.go) admits every code that
// appears in the map and leaves every absent position at code 0, which no admit set
// contains. A map naming every position therefore rejects NOTHING — which is what
// allEdgesTypeFilter next door is for, and what a first draft of this fixture did,
// caught by its own non-vacuity guard (both arms emitted 100 rows).
//
// So a hop configured with a non-empty EdgeType walks all `deg` slots and admits
// exactly one, and the same hop with no EdgeType walks the same slots and admits
// all of them. That is the pair: identical walk, different rejection count.
func columnarRejectFixture(deg int, out bool) (fwd, rev *staticCSR, filter map[uint64]string) {
	edges := make([][2]int, 0, deg)
	for i := 1; i <= deg; i++ {
		if out {
			edges = append(edges, [2]int{0, i}) // 0 -> i
		} else {
			edges = append(edges, [2]int{i, 0}) // i -> 0
		}
	}
	fwd, rev = buildStaticPair(deg+1, edges)
	// The LAST forward position, whichever edge that turns out to be: buildCSR
	// orders by source, so the fan-out's positions are one contiguous run out of
	// node 0 and the fan-in's are one per source. Either way exactly one is named.
	filter = map[uint64]string{uint64(len(fwd.EdgesSlice()) - 1): "KNOWS"}
	return fwd, rev, filter
}

// drainColumnarExpandProfiled builds a columnarExpand over the fixture, wraps it
// in a Profiler, drains it column-major, and returns the captured plan node.
//
// It drives FillChunk and NEVER Next, which is what makes it a gate on the
// columnar charge: a counter maintained only on the row path reports 0 here.
// perCall is deliberately small and not a divisor of the walk, so filling stops
// mid-run repeatedly and the fan-out cursor resumes across calls.
func drainColumnarExpandProfiled(t *testing.T, srcID int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, perCall int) exec.PlanNode {
	t.Helper()
	base := exec.NewExpand(&nodeIDChunkSource{ids: []int64{srcID}}, exec.StaticAdjacency(fwd, rev, filter), cfg)
	cp, ok := exec.NewColumnarExpand(base)
	if !ok {
		t.Fatal("NewColumnarExpand: the child was not recognised as a ChunkProducer, " +
			"so this gate would silently measure the ROW path instead")
	}
	p := exec.NewProfiler()
	wrapped := p.WrapChunk(cp)
	if err := wrapped.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := wrapped.NewOutputChunk(exec.DefaultChunkCapacity)
	for {
		n, err := wrapped.FillChunk(dst, perCall)
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		if n < perCall {
			break // n < perCall ⇔ end-of-stream
		}
	}
	tree := exec.PlanTree(wrapped)
	if tree.Name != "columnarExpand" {
		t.Fatalf("the captured node is %q, want columnarExpand — the gate is not "+
			"measuring the operator it names", tree.Name)
	}
	if !tree.Profiled {
		t.Fatal("the captured node carries no measurements")
	}
	return tree
}

// TestColumnarExpandRowsRemoved_CountsWhatFillChunkDiscarded is the gate the
// per-site sweep proved was missing, in both directions.
//
// DirOut exercises fillOneChunkRow's FORWARD arm (site S5) and DirIn its REVERSE
// arm (site S6). The reverse arm additionally reaches
// Expand.reverseEdgePassesFilter, which resolves each reverse slot back to its
// forward position before the type filter can be applied — a path the forward arm
// never touches.
func TestColumnarExpandRowsRemoved_CountsWhatFillChunkDiscarded(t *testing.T) {
	t.Parallel()
	const deg = 100

	for _, dir := range []struct {
		name string
		d    exec.Direction
		out  bool
		arm  string
	}{
		{"DirOut", exec.DirOut, true, "fillOneChunkRow forward arm (site S5)"},
		{"DirIn", exec.DirIn, false, "fillOneChunkRow reverse arm (site S6)"},
	} {
		t.Run(dir.name, func(t *testing.T) {
			fwd, rev, filter := columnarRejectFixture(deg, dir.out)

			for _, perCall := range []int{1, 7, 64, exec.DefaultChunkCapacity} {
				// CONTROL: the same walk with no type filter. It rejects nothing, so it
				// must report a MEASURED zero — not an absent cell, and not the
				// subject's figure.
				ctl := drainColumnarExpandProfiled(t, 0, fwd, rev, filter,
					exec.ExpandConfig{Direction: dir.d, InputCol: 0}, perCall)
				// SUBJECT: the identical walk, admitting one edge in a hundred.
				sub := drainColumnarExpandProfiled(t, 0, fwd, rev, filter,
					exec.ExpandConfig{Direction: dir.d, EdgeType: "KNOWS", InputCol: 0}, perCall)

				// Non-vacuity first: the two arms must really differ in what they EMIT
				// and agree on what they WALKED, or the comparison below proves nothing.
				if ctl.Rows != deg || sub.Rows != 1 {
					t.Fatalf("perCall=%d: the arms emitted %d and %d rows, want %d and 1 — "+
						"the fixture no longer separates the walk from the emission",
						perCall, ctl.Rows, sub.Rows, deg)
				}
				if !ctl.DbHitsKnown || !sub.DbHitsKnown {
					t.Fatalf("perCall=%d: columnarExpand rendered db-hits unknown "+
						"(control %v, subject %v); it embeds *Expand and inherits the counter",
						perCall, ctl.DbHitsKnown, sub.DbHitsKnown)
				}
				if ctl.DbHits != deg || sub.DbHits != deg {
					t.Fatalf("perCall=%d: the arms walked %d and %d slots, want %d each — "+
						"they must walk the SAME run for their rejection counts to be "+
						"comparable", perCall, ctl.DbHits, sub.DbHits, deg)
				}

				// The figure must EXIST on both arms.
				if !ctl.RowsRemovedByFilterKnown || !sub.RowsRemovedByFilterKnown {
					t.Fatalf("perCall=%d: columnarExpand reported NO removed cell "+
						"(control known=%v, subject known=%v). It embeds *Expand and "+
						"implements rowsRemovedCounter, so the figure exists on both arms",
						perCall, ctl.RowsRemovedByFilterKnown, sub.RowsRemovedByFilterKnown)
				}
				if ctl.RowsRemovedByFilter != 0 {
					t.Errorf("perCall=%d: the CONTROL arm reported removed=%d, want 0. It "+
						"admits every edge it walks, so a non-zero here means the charge "+
						"is being taken on a path that emitted", perCall, ctl.RowsRemovedByFilter)
				}
				if want := int64(deg - 1); sub.RowsRemovedByFilter != want {
					t.Errorf("perCall=%d: the SUBJECT arm reported removed=%d, want %d. It "+
						"walked %d slots through %s and admitted one; a 0 here is what "+
						"deleting that increment looks like",
						perCall, sub.RowsRemovedByFilter, want, deg, dir.arm)
				}

				// The identity, on BOTH arms. Its two sides are counted by unrelated
				// state — DbHits from the cursor positions, removed from the reject
				// branches — so it cannot be satisfied by a counter that merely tracks
				// the other.
				for _, arm := range []struct {
					name string
					n    exec.PlanNode
				}{{"control", ctl}, {"subject", sub}} {
					if got := arm.n.Rows + arm.n.RowsRemovedByFilter; got != arm.n.DbHits {
						t.Errorf("perCall=%d %s: rows=%d + removed=%d = %d against dbhits=%d. "+
							"Every slot the columnar cursor consumed is either appended or "+
							"discarded, and the two figures are counted independently",
							perCall, arm.name, arm.n.Rows, arm.n.RowsRemovedByFilter, got, arm.n.DbHits)
					}
				}
			}
		})
	}
}

// TestColumnarExpandRowsRemoved_AgreesWithTheRowPath is the cross-path gate.
//
// The two paths share their edge DECISIONS (advanceFwdEdge / advanceRevEdge) but
// charge the rejection in different places — the row path in tryFwdEdge/tryRevEdge,
// the columnar path in fillOneChunkRow. Two charge sites for one decision is
// exactly the shape that lets the two drift, and nothing else in the suite compares
// them. This does: the same fixture, the same config, driven both ways, must report
// the same figure.
func TestColumnarExpandRowsRemoved_AgreesWithTheRowPath(t *testing.T) {
	t.Parallel()
	const deg = 100

	for _, dir := range []struct {
		name string
		d    exec.Direction
		out  bool
	}{
		{"DirOut", exec.DirOut, true},
		{"DirIn", exec.DirIn, false},
	} {
		t.Run(dir.name, func(t *testing.T) {
			fwd, rev, filter := columnarRejectFixture(deg, dir.out)
			cfg := exec.ExpandConfig{Direction: dir.d, EdgeType: "KNOWS", InputCol: 0}

			// Row path: the same operator driven through Next.
			rowBase := exec.NewExpand(&nodeIDChunkSource{ids: []int64{0}},
				exec.StaticAdjacency(fwd, rev, filter), cfg)
			p := exec.NewProfiler()
			rowOp := p.Wrap(rowBase)
			if _, err := exec.Drain(context.Background(), rowOp); err != nil {
				t.Fatalf("row-mode Drain: %v", err)
			}
			rowNode := exec.PlanTree(rowOp)
			if rowNode.Name != "Expand" {
				t.Fatalf("the row arm captured %q, want Expand", rowNode.Name)
			}

			colNode := drainColumnarExpandProfiled(t, 0, fwd, rev, filter, cfg, 7)

			if !rowNode.RowsRemovedByFilterKnown || !colNode.RowsRemovedByFilterKnown {
				t.Fatalf("one of the paths reported no figure (row known=%v, columnar "+
					"known=%v)", rowNode.RowsRemovedByFilterKnown, colNode.RowsRemovedByFilterKnown)
			}
			if rowNode.RowsRemovedByFilter != colNode.RowsRemovedByFilter {
				t.Errorf("the row path reported removed=%d and the columnar path "+
					"removed=%d for the SAME %d-slot walk under the SAME config. The two "+
					"paths share advanceFwdEdge/advanceRevEdge and differ only in where "+
					"they charge, so a disagreement is one of the two charge sites being "+
					"wrong", rowNode.RowsRemovedByFilter, colNode.RowsRemovedByFilter, deg)
			}
			if want := int64(deg - 1); rowNode.RowsRemovedByFilter != want {
				t.Errorf("both paths agree on removed=%d, but the fixture rejects %d "+
					"— they are consistently wrong, which agreement alone cannot detect",
					rowNode.RowsRemovedByFilter, want)
			}
		})
	}
}
