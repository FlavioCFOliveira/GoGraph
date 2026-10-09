package adjlist

import "sync/atomic"

// removeAllRoundsMax is the most rounds an undirected RemoveAllEdgesFrom took on
// the AdjList the last resetRemoveAllRounds attached it to. The test using it
// does not run in parallel.
var removeAllRoundsMax atomic.Int64

// resetRemoveAllRounds attaches the round counter to a, starting from zero.
func resetRemoveAllRounds[N comparable, W any](a *AdjList[N, W]) {
	removeAllRoundsMax.Store(0)
	a.removeAllRoundsHookForTest = func(r int) {
		for {
			cur := removeAllRoundsMax.Load()
			if int64(r) <= cur || removeAllRoundsMax.CompareAndSwap(cur, int64(r)) {
				return
			}
		}
	}
}

// maxRemoveAllRounds returns the most rounds observed since the reset.
func maxRemoveAllRounds[N comparable, W any](_ *AdjList[N, W]) int64 {
	return removeAllRoundsMax.Load()
}
