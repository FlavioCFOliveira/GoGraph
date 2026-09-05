package exec

import "github.com/FlavioCFOliveira/GoGraph/graph"

// export_reltype_test_support.go — the three unexported CSR-probe primitives the
// parent package's differential oracle needs to reproduce the PRE-#2251 reverse
// admission decision exactly.
//
// They are exported here rather than reimplemented there deliberately: an oracle
// that restates the code it is meant to check proves only that two restatements
// agree. These forward to the very functions [Expand.reverseEdgePassesFilter]
// itself calls.

// UnresolvedFwdPosForTest is [unresolvedFwdPos].
const UnresolvedFwdPosForTest = unresolvedFwdPos

// MatchFwdByHandleForTest is [matchFwdByHandle].
func MatchFwdByHandleForTest(
	fwdEdges []graph.NodeID, fwdHandles []uint64, fStart, fEnd, revUID, handle uint64,
) uint64 {
	return matchFwdByHandle(fwdEdges, fwdHandles, fStart, fEnd, revUID, handle)
}

// FirstDstPosForTest is [firstDstPos].
func FirstDstPosForTest(edges []graph.NodeID, start, end, dst uint64) (uint64, bool) {
	return firstDstPos(edges, start, end, dst)
}

// RelTypeColumnFwdLenForTest reports how many forward slots the column describes.
// It is test-only: a differential in the parent package checks that a column and
// the CSR pair it was built from have the same arc count.
func (c *RelTypeColumn) RelTypeColumnFwdLenForTest() int {
	if c == nil {
		return 0
	}
	return len(c.fwdCodes)
}
