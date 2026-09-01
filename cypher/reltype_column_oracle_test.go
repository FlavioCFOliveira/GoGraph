package cypher

// reltype_column_oracle_test.go — the PRE-#2251 edge-type filter map, retained
// verbatim as the differential oracle for the slot-aligned type column.
//
// buildEdgeTypeFilterOracle below is exactly the production buildEdgeTypeFilter as
// it stood at commit 35990293, with one edit: the O(V+E) resolution it used to
// carry inline is now called through [forEachResolvedSlotType], which IS that same
// resolution lifted out unchanged. So the oracle and the column share the
// resolution and differ ONLY in what they do with its answer — which is precisely
// the axis a differential test between them can and must isolate.
//
// It lives in a test file because production has no caller for it any more.
// Keeping it is not sentiment: four closed defects (rmp #2258, #2293, TCK
// Match2 [6] and Match7 [29]) were all cases where an arc's type was resolved
// wrongly, and a differential against the implementation that closed them is the
// cheapest possible proof that the replacement did not reopen them.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildEdgeTypeFilterOracle reproduces the retired position-keyed filter map: the
// set of forward CSR positions whose arc carries at least one of relTypes, with an
// empty relTypes meaning "every typed arc".
func buildEdgeTypeFilterOracle(
	g *lpg.ReadView[string, float64], fwdCSR *csr.CSR[float64], relTypes []string,
) map[uint64]string {
	acceptAll := len(relTypes) == 0
	accept := make(map[string]struct{}, len(relTypes))
	for _, t := range relTypes {
		accept[t] = struct{}{}
	}
	filter := make(map[uint64]string)
	forEachResolvedSlotType(g, fwdCSR, func(pos uint64, labels []string) {
		if acceptAll {
			filter[pos] = labels[0]
			return
		}
		for _, lbl := range labels {
			if _, ok := accept[lbl]; ok {
				filter[pos] = lbl
				return
			}
		}
	})
	return filter
}

// columnAdmitOracleDiff returns the forward positions on which the column's
// admission verdict differs from the oracle map's membership, as
// (position, columnSaysAdmit) pairs.
func columnAdmitOracleDiff(
	g *lpg.ReadView[string, float64], fwd, rev *csr.CSR[float64], relTypes []string,
) (diffs []uint64, colAdmit []bool) {
	oracle := buildEdgeTypeFilterOracle(g, fwd, relTypes)
	col := buildRelTypeColumn(g, fwd, rev)
	admit := col.Admit(relTypeCodesFor(g, relTypes))
	for pos := uint64(0); pos < uint64(len(fwd.EdgesSlice())); pos++ {
		_, want := oracle[pos]
		got := admit.Fwd(pos)
		if got != want {
			diffs = append(diffs, pos)
			colAdmit = append(colAdmit, got)
		}
	}
	return diffs, colAdmit
}

// revColumnOracleDiff compares the column's REVERSE verdict for every reverse slot
// against the verdict the pre-#2251 Expand computed for the same slot: recover the
// forward position by handle (falling back to the pair's lower bound) and test the
// oracle map there. It returns one string per divergence.
func revColumnOracleDiff(
	g *lpg.ReadView[string, float64], fwd, rev *csr.CSR[float64], relTypes []string,
) []string {
	oracle := buildEdgeTypeFilterOracle(g, fwd, relTypes)
	col := buildRelTypeColumn(g, fwd, rev)
	admit := col.Admit(relTypeCodesFor(g, relTypes))

	fwdVerts, fwdEdges, fwdHandles := fwd.VerticesSlice(), fwd.EdgesSlice(), fwd.HandlesSlice()
	revVerts, revEdges, revHandles := rev.VerticesSlice(), rev.EdgesSlice(), rev.HandlesSlice()

	var out []string
	for owner := uint64(0); owner+1 < uint64(len(revVerts)); owner++ {
		for revPos := revVerts[owner]; revPos < revVerts[owner+1]; revPos++ {
			d := uint64(revEdges[revPos])
			want := legacyReversePasses(
				fwdVerts, fwdEdges, fwdHandles, revHandles, oracle, d, owner, revPos)
			got, known := admit.Rev(revPos)
			if !known {
				continue // the operator falls back to the legacy path for this slot
			}
			if got != want {
				out = append(out, formatRevDiff(revPos, d, owner, got, want))
			}
		}
	}
	return out
}

// legacyReversePasses is the pre-#2251 Expand.reverseEdgePassesFilter, reduced to
// the parts that decide admission, so the differential compares against the code
// the change replaced rather than against a restatement of it.
func legacyReversePasses(
	fwdVerts []uint64, fwdEdges []graph.NodeID, fwdHandles, revHandles []uint64,
	oracle map[uint64]string, dst, src, revPos uint64,
) bool {
	if dst+1 >= uint64(len(fwdVerts)) {
		return false
	}
	fStart, fEnd := fwdVerts[dst], fwdVerts[dst+1]
	if fwdHandles != nil && revHandles != nil && revPos < uint64(len(revHandles)) {
		if fp := exec.MatchFwdByHandleForTest(
			fwdEdges, fwdHandles, fStart, fEnd, src, revHandles[revPos],
		); fp != exec.UnresolvedFwdPosForTest {
			_, ok := oracle[fp]
			return ok
		}
	}
	fp, ok := exec.FirstDstPosForTest(fwdEdges, fStart, fEnd, src)
	if !ok {
		return false
	}
	_, admitted := oracle[fp]
	return admitted
}

// formatRevDiff renders one reverse-side divergence.
func formatRevDiff(revPos, dst, src uint64, got, want bool) string {
	return fmt.Sprintf("revPos=%d fwdEdge=(%d->%d): column=%v legacy=%v", revPos, dst, src, got, want)
}
