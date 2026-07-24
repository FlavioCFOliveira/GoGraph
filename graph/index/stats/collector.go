package stats

import "sync/atomic"

// Domain is a caller-assigned tag identifying one comparable value-domain of a
// property (for example numeric versus string). A [Stats] holds at most one
// [Histogram] per domain, because a single histogram is only meaningful over
// values a single orderability comparator totally orders.
type Domain uint8

// Stats is the per-(label, property) bundle of approximate estimators plus the
// staleness bookkeeping. It is built off the write path by a rebuild scan and
// published through a [Collector]; once published its estimators are immutable
// and its counters are mutated only through the atomic Record* methods.
//
// A Stats is safe for concurrent use: the estimator fields are read-only after
// construction and the dirty / delete counters are atomics.
type Stats[T any] struct {
	NDV *HLL
	MCV *MCVList[T]

	// hists holds one histogram per comparable value-domain of the property.
	hists map[Domain]*Histogram[T]

	// g0 is the graph generation, and n0 the exact live count of the label, at
	// the moment this Stats was built — the (g0, N0) stamp the staleness model
	// measures drift against (design docs/statistics-design.md §2).
	g0 uint64
	n0 int64
	b  int // histogram bucket count B, mirrored for the certified 1/B error term

	// dirty is Δ: the number of property writes to this (label, property) since
	// the build, bumped O(1) on the write path. deletes counts the subset that
	// removed or overwrote a value — the direction that makes NDV over-estimate,
	// because HLL cannot lower a register maximum.
	dirty   atomic.Int64
	deletes atomic.Int64
}

// Input carries the freshly-built estimators and the generation stamp for a
// single (label, property) into [NewStats].
type Input[T any] struct {
	NDV        *HLL
	MCV        *MCVList[T]
	Histograms map[Domain]*Histogram[T]
	Generation uint64
	LabelCount int64 // N0: the exact live count of the label at build time
	Buckets    int   // the histogram target bucket count B
}

// NewStats assembles a Stats from freshly-built estimators. The dirty and delete
// counters start at zero.
func NewStats[T any](in Input[T]) *Stats[T] {
	return &Stats[T]{
		NDV:   in.NDV,
		MCV:   in.MCV,
		hists: in.Histograms,
		g0:    in.Generation,
		n0:    in.LabelCount,
		b:     in.Buckets,
	}
}

// Histogram returns the histogram for domain d and true when one exists.
func (s *Stats[T]) Histogram(d Domain) (*Histogram[T], bool) {
	h, ok := s.hists[d]
	return h, ok
}

// Generation returns the g0 stamp: the graph generation at build time.
func (s *Stats[T]) Generation() uint64 { return s.g0 }

// LabelCount returns the N0 stamp: the exact live label count at build time.
func (s *Stats[T]) LabelCount() int64 { return s.n0 }

// Buckets returns the histogram bucket count B, the source of the 1/B error term.
func (s *Stats[T]) Buckets() int { return s.b }

// RecordWrite bumps Δ by one — the single O(1) atomic the write path may pay.
func (s *Stats[T]) RecordWrite() { s.dirty.Add(1) }

// RecordDelete bumps the delete counter by one (a value removed or overwritten).
func (s *Stats[T]) RecordDelete() { s.deletes.Add(1) }

// Delta returns the current Δ (writes since build).
func (s *Stats[T]) Delta() int64 { return s.dirty.Load() }

// Deletes returns the current delete count since build.
func (s *Stats[T]) Deletes() int64 { return s.deletes.Load() }

// NeedsRebuildForDeletes reports whether the delete count has exceeded the given
// tolerance fraction of the build-time label count, the threshold past which the
// HLL's inability to delete makes its NDV estimate untrustworthy (an over-
// estimate). A non-positive n0 forces a rebuild (nothing was summarised, or the
// stamp is degenerate).
func (s *Stats[T]) NeedsRebuildForDeletes(tol float64) bool {
	n0 := s.n0
	if n0 <= 0 {
		return true
	}
	return float64(s.Deletes()) > tol*float64(n0)
}

// Key identifies a statistics bundle by its interned (label, property) ids.
type Key struct {
	Label uint32
	Prop  uint32
}

// collectorSnap is an immutable published snapshot of a [Collector]. Swapping the
// pointer to a fresh snapshot is the only mutation of the map, so readers never
// need a lock.
type collectorSnap[T any] struct {
	byKey        map[Key]*Stats[T]
	labelsByProp map[uint32][]uint32
}

// Collector is the lock-free-read registry of per-(label, property) [Stats]. A
// rebuild publishes a fresh immutable snapshot with [Collector.Publish]; every
// read loads the current snapshot pointer atomically and consults its immutable
// map, so readers never block writers or each other. The per-[Stats] staleness
// counters are mutated in place through the atomic Record* methods.
//
// Collector is safe for concurrent use. The zero value is not usable; construct
// one with [NewCollector].
type Collector[T any] struct {
	snap atomic.Pointer[collectorSnap[T]]
}

// NewCollector returns an empty Collector holding no statistics. Every lookup on
// an empty Collector misses, so a consumer falls back to its exact-count plan
// until the first [Collector.Publish].
func NewCollector[T any]() *Collector[T] {
	c := &Collector[T]{}
	c.snap.Store(&collectorSnap[T]{
		byKey:        map[Key]*Stats[T]{},
		labelsByProp: map[uint32][]uint32{},
	})
	return c
}

// Publish atomically replaces the Collector's contents with byKey. It derives the
// property→labels index the write path consults for staleness attribution, then
// swaps in the new immutable snapshot in one atomic store. Concurrent readers see
// either the whole previous snapshot or the whole new one, never a mix.
func (c *Collector[T]) Publish(byKey map[Key]*Stats[T]) {
	labelsByProp := make(map[uint32][]uint32)
	for k := range byKey {
		labelsByProp[k.Prop] = append(labelsByProp[k.Prop], k.Label)
	}
	c.snap.Store(&collectorSnap[T]{byKey: byKey, labelsByProp: labelsByProp})
}

// Lookup returns the Stats for (label, prop) and true when present. It is a
// lock-free read of the current snapshot.
func (c *Collector[T]) Lookup(label, prop uint32) (*Stats[T], bool) {
	st, ok := c.snap.Load().byKey[Key{Label: label, Prop: prop}]
	return st, ok
}

// TrackedLabelsForProp returns the labels for which prop currently has
// statistics, so the write path can attribute a property write to each affected
// (label, property) bundle. The returned slice belongs to the immutable snapshot
// and must not be mutated. It is a lock-free read.
func (c *Collector[T]) TrackedLabelsForProp(prop uint32) []uint32 {
	return c.snap.Load().labelsByProp[prop]
}

// Tracking reports whether the Collector holds any statistics. A false result
// lets the write path skip its staleness bookkeeping entirely with a single
// atomic load.
func (c *Collector[T]) Tracking() bool {
	return len(c.snap.Load().byKey) > 0
}

// Size reports the number of (label, property) bundles currently held.
func (c *Collector[T]) Size() int {
	return len(c.snap.Load().byKey)
}

// RecordWrite bumps Δ for (label, prop) when a bundle exists (a no-op otherwise).
func (c *Collector[T]) RecordWrite(label, prop uint32) {
	if st, ok := c.Lookup(label, prop); ok {
		st.RecordWrite()
	}
}

// RecordDelete bumps the delete counter for (label, prop) when a bundle exists.
func (c *Collector[T]) RecordDelete(label, prop uint32) {
	if st, ok := c.Lookup(label, prop); ok {
		st.RecordDelete()
	}
}
