package lpg

// mvcc_gc.go — MVCC P4a (rmp #2288): driving reclamation from the write path.
//
// # Why arming versioning obliges this
//
// P6 (#2283, #2284) built the reclaimers; nothing called them, which was
// harmless only while nothing armed versioning. Arming it without a driver
// would make every modification leak a record until the process exits, and the
// module's bounded-resources mandate does not admit an unbounded structure with
// a follow-up ticket attached. So the driver lands with the arming.
//
// # Where it runs, and why not on a goroutine
//
// It runs on the writer that just committed, at the end of
// [Graph.endWrite], while the barrier is still held. That placement is not a
// convenience:
//
//   - It needs writer exclusion. The adjacency reclaimer takes each shard lock
//     and severs published chains; it is documented as safe against readers but
//     NOT against a concurrent writer, and the barrier is exactly what excludes
//     one.
//   - It self-throttles. A graph that is not being written is not accumulating
//     versions, so it needs no sweep, and a background ticker would wake to do
//     nothing — the module forbids unbounded goroutines and requires every
//     long-lived one to be observable, which is a cost with no benefit here.
//
// PostgreSQL reaches the same place from the opposite direction: its HOT-prune
// path cleans a page during an ordinary access rather than waiting for the
// VACUUM daemon, because the accessor already holds the right lock and already
// paid the cache miss.
//
// # The debt counter
//
// Sweeping on every commit would make a one-property write walk sixty-four
// adjacency shards. Instead each write adds its version count to a debt, and a
// sweep runs only once the debt passes [reclaimThreshold]. The bound that
// follows is stated rather than implied: between sweeps the graph holds at most
// the threshold in versions ABOVE whatever a long-lived reader is legitimately
// holding back, and both quantities are readable — [Graph.VersionCount] for the
// total and [mvcc.Horizon.Active] for the readers responsible.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// reclaimThreshold is how many versions may accumulate between reclamation
// sweeps.
//
// It trades sweep frequency against retained memory. At 4096 the sweep amortises
// to a 64-shard scan per 4096 modifications — under a hundredth of a shard visit
// per write — while the retained ceiling is roughly 4096 records, which at the
// measured 24 to 32 bytes each is around 128 KiB. Both halves of that trade are
// stated so a future change to the constant is made against numbers rather than
// against taste.
const reclaimThreshold = 4096

// reclaimIfDue sweeps the version chains when enough churn has accumulated
// since the last sweep.
//
// The caller must hold the visibility barrier in write mode; see the file
// comment for why that is a requirement and not merely where it happens to be
// called from.
func (g *Graph[N, W]) reclaimIfDue() {
	live := g.VersionCount()
	if live == 0 {
		g.reclaimDebt.Store(0)
		return
	}
	if g.reclaimDebt.Load() < reclaimThreshold {
		return
	}
	g.reclaimDebt.Store(0)
	g.ReclaimNow()
}

// ReclaimNow frees every version no active reader can reach, and returns how
// many records were released.
//
// The watermark is the oldest start timestamp among the readers registered with
// this graph's horizon, falling back to the clock's current value when none is
// registered — at which point every version a completed write superseded is
// unreachable and all of it goes. A reader that could not be registered
// suspends reclamation entirely rather than risk it; see [mvcc.Horizon.Oldest].
//
// It is exported so a caller that knows it has just finished a large mutation
// can settle the debt immediately instead of waiting for the next write, and so
// a test can assert that the substrate returns to zero.
//
// The caller must hold the visibility barrier in write mode, or otherwise
// exclude concurrent writers. Safe against concurrent readers.
func (g *Graph[N, W]) ReclaimNow() int {
	watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
	if watermark == 0 {
		return 0
	}
	return g.ReclaimVersions(watermark) + g.adj.Reclaim(watermark)
}

// VersionCount returns the total number of live version records across every
// store: node-label deltas, node-property deltas and adjacency entry versions.
//
// It is the memory the substrate is responsible for, and the quantity the
// bounded-resources mandate requires be observable rather than merely bounded.
//
// Safe for concurrent use.
func (g *Graph[N, W]) VersionCount() int64 {
	return g.labelDeltaActive.Load() + g.propDeltaActive.Load() + g.adj.VersionCount()
}

// Horizon returns the reader horizon this graph reclaims against.
//
// A reader registers its start timestamp with it for as long as it is active,
// so reclamation can tell which versions are still reachable. It is exported
// because the layer that owns a read's lifetime — the Cypher engine — lives in
// another package.
//
// Safe for concurrent use.
func (g *Graph[N, W]) Horizon() *mvcc.Horizon { return g.horizon }
