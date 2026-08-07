package mvcc

// depthhist.go — the distribution of version-chain depth (rmp #2312).
//
// # Why a distribution and not a mean
//
// A version chain is what a read walks: the reader steps back from the stored
// value through each record newer than its own instant until it reaches one it can
// see. So chain depth is read cost, per object, and the quantity that matters is
// the TAIL — one object with a chain of 200 is a latency spike that a mean over a
// million objects of depth 1 reports as 1.0002.
//
// PostgreSQL's operators read the same shape out of pg_stat_user_tables' dead
// tuples plus pgstattuple's per-page distribution rather than out of an average,
// and Memgraph's `MemoryTracker`/GC metrics likewise report retained deltas rather
// than a per-object mean. Both are read as "is anything holding a long chain",
// which an average cannot answer.
//
// # What is measured, and when
//
// The RETAINED depth: how deep each chain is AFTER the reclaimer has truncated
// everything below the watermark. That is the depth a reader arriving now would
// walk, which is the only depth anyone can act on — the records already
// unreachable are the vacuum's backlog, and [WriteCounts] and the vacuum's own
// series already report that.
//
// It is measured BY the reclaimer, during the walk it already performs. The walk
// visits every chain in the store and already counts its way along it, so the
// measurement is an increment in a register on a goroutine that is off every
// request path. Nothing samples, nothing estimates, and no second traversal
// exists.
//
// # The consequence: a histogram describes the LAST COMPLETE SWEEP of its store
//
// The reclaimer resets the histogram when it starts a store and fills it as it
// goes, so the published distribution is the one the most recent sweep of that
// store observed. A pass that skips a store — the vacuum stops at its per-pass
// record cap, or the store has no live versions at all — leaves that store's
// numbers as its last sweep left them, and a store with nothing live is reset to
// empty rather than left stale.
//
// A concurrent reader can therefore catch a histogram mid-fill and see fewer
// chains than the store holds. That is accepted for telemetry and stated here
// rather than defended against: double-buffering it would cost a second array and
// an atomic swap to make a scrape that happens once a second agree with itself.

import (
	"math/bits"
	"sync/atomic"
)

// DepthBuckets is how many buckets [DepthHist] has. Bucket i holds the chains
// whose retained depth is in [2^i, 2^(i+1)), with the last bucket unbounded above.
const DepthBuckets = 8

// depthLabels names each bucket for the metric series it is published under.
var depthLabels = [DepthBuckets]string{"1", "2_3", "4_7", "8_15", "16_31", "32_63", "64_127", "128_inf"}

// DepthBucketLabel returns the metric-name suffix of bucket i.
func DepthBucketLabel(i int) string { return depthLabels[i] }

// DepthBucketLow returns the smallest depth that falls in bucket i.
func DepthBucketLow(i int) int { return 1 << i }

// DepthHist is a log2-bucketed histogram of retained version-chain depth.
//
// The zero value is ready to use, and means "nothing measured yet" — which is
// distinguishable from "every chain is short", because every bucket is zero rather
// than the first one being large.
//
// Safe for concurrent use: one writer (the single sweeper) and any number of
// readers.
type DepthHist struct {
	b [DepthBuckets]atomic.Uint64
	// deepest is the largest single depth observed since the last reset. The
	// histogram's last bucket is unbounded, so without this a chain of 5000 and a
	// chain of 130 are the same reading — and the whole reason to prefer a
	// distribution over a mean is to see exactly that difference.
	deepest atomic.Uint64
}

// Observe records one chain of the given retained depth. A depth of zero — a chain
// the reclaimer removed entirely — is not a retained chain and is ignored.
func (h *DepthHist) Observe(depth int) {
	if depth <= 0 {
		return
	}
	i := bits.Len(uint(depth)) - 1
	if i >= DepthBuckets {
		i = DepthBuckets - 1
	}
	h.b[i].Add(1)
	if d := uint64(depth); d > h.deepest.Load() {
		// A plain compare-and-store, not a CAS loop: the only writer is the single
		// sweeper, so there is no second writer to lose to, and a reader that
		// catches the intermediate value reads a smaller maximum for one instant.
		h.deepest.Store(d)
	}
}

// Reset clears every bucket. Called by the reclaimer as it starts a store, so the
// histogram describes that store's latest sweep rather than accumulating over the
// life of the process.
func (h *DepthHist) Reset() {
	for i := range h.b {
		h.b[i].Store(0)
	}
	h.deepest.Store(0)
}

// Depths is a readable copy of a [DepthHist].
type Depths struct {
	// Buckets[i] is how many chains had a retained depth in [2^i, 2^(i+1)).
	Buckets [DepthBuckets]uint64
	// Deepest is the largest retained depth observed, exactly.
	Deepest uint64
}

// Chains returns how many chains the histogram counted.
func (d *Depths) Chains() uint64 {
	var n uint64
	for _, v := range d.Buckets {
		n += v
	}
	return n
}

// Add accumulates o into d, so several stores' histograms can be reported as one
// distribution.
func (d *Depths) Add(o Depths) {
	for i := range d.Buckets {
		d.Buckets[i] += o.Buckets[i]
	}
	if o.Deepest > d.Deepest {
		d.Deepest = o.Deepest
	}
}

// Load returns a readable copy.
//
// Safe for concurrent use; see the file comment for what a mid-fill read means.
func (h *DepthHist) Load() Depths {
	var d Depths
	for i := range h.b {
		d.Buckets[i] = h.b[i].Load()
	}
	d.Deepest = h.deepest.Load()
	return d
}
