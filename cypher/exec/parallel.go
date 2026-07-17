package exec

// parallel.go — shared morsel primitives for the parallel scan operators.
//
// The standalone ParallelScan leaf operator that once lived here was dead
// scaffolding (no planner call site; it bypassed the ParallelGovernor and was
// exercised only by a flaky NumGoroutine-based test) and was removed (rmp
// #2019). The live parallel leaves — ParallelScanProject and ParallelCountScan
// (parallel_scan_project.go) — remain and share the two primitives kept here:
// DefaultMorselSize and splitMorsels.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// DefaultMorselSize is the number of NodeIDs processed per worker goroutine
// per scheduling quantum. Sized to fill roughly one or two cache lines of
// work before touching a channel.
const DefaultMorselSize = 1024

// splitMorsels partitions ids into chunks of at most size elements.
func splitMorsels(ids []graph.NodeID, size int) [][]graph.NodeID {
	n := (len(ids) + size - 1) / size
	morsels := make([][]graph.NodeID, 0, n)
	for len(ids) > 0 {
		end := size
		if end > len(ids) {
			end = len(ids)
		}
		morsels = append(morsels, ids[:end])
		ids = ids[end:]
	}
	return morsels
}
