package plan

import (
	"sync/atomic"
	"time"
)

// StatsManager wraps an IndexEstimator with generation-aware cache
// invalidation. When the underlying graph is mutated (Tx commit) or a
// new CSR snapshot is published, the caller should call NotifyRotation.
// On the next planner call, the estimator's avg-out-degree cache is
// discarded and rebuilt lazily from updated statistics.
//
// The recommended pattern is:
//
//	sm := plan.NewStatsManager(est, 100)
//	// … after Publisher.Publish or Tx commit:
//	sm.NotifyRotation()
//	// … after re-populating degree statistics:
//	sm.MarkSeen()
//
// StatsManager is safe for concurrent use.
type StatsManager struct {
	est        *IndexEstimator
	gen        atomic.Uint64 // current generation; incremented by NotifyRotation
	lastSeen   atomic.Uint64 // generation observed during last cache refresh
	staleCount atomic.Uint64 // calls made while stale (for bounded-staleness enforcement)
	maxStale   uint64        // if > 0, force refresh after this many stale calls
	// lastRefreshNs tracks when the cache was last refreshed (for observability).
	lastRefreshNs atomic.Int64
}

// NewStatsManager returns a StatsManager wrapping est.
//
// maxStaleCalls: if non-zero, after this many planner calls with a stale
// cache the manager forces a synchronous cache reset (clearing
// IndexEstimator.degreeCache). Pass 0 to rely solely on NotifyRotation
// + MarkSeen for explicit refresh control.
func NewStatsManager(est *IndexEstimator, maxStaleCalls uint64) *StatsManager {
	sm := &StatsManager{est: est, maxStale: maxStaleCalls}
	sm.lastSeen.Store(0)
	sm.gen.Store(0)
	return sm
}

// NotifyRotation marks the statistics cache as stale. Call this after
// each Tx commit that modifies graph structure or after each CSR snapshot
// rotation via generation.Publisher.Publish.
func (sm *StatsManager) NotifyRotation() {
	sm.gen.Add(1)
}

// IsFresh reports whether the stats cache is up-to-date with the latest
// generation.
func (sm *StatsManager) IsFresh() bool {
	return sm.gen.Load() == sm.lastSeen.Load()
}

// Estimator returns the wrapped IndexEstimator after checking whether the
// avg-out-degree cache is excessively stale. When the cache is stale and
// maxStaleCalls (set in NewStatsManager) is exceeded, the degree cache is
// cleared synchronously before the estimator is returned, and the
// generation counter is advanced to the current one.
//
// Callers must call UpdateDegreeCache on the returned estimator and then
// MarkSeen to re-populate the avg-out-degree cache after a forced reset.
func (sm *StatsManager) Estimator() *IndexEstimator {
	gen := sm.gen.Load()
	last := sm.lastSeen.Load()
	if gen != last {
		count := sm.staleCount.Add(1)
		if sm.maxStale > 0 && count >= sm.maxStale {
			// Force cache reset: clear the avg-out-degree entries.
			sm.est.mu.Lock()
			sm.est.degreeCache = make(map[degreeKey]float64)
			sm.est.mu.Unlock()
			sm.lastSeen.Store(gen)
			sm.staleCount.Store(0)
			sm.lastRefreshNs.Store(time.Now().UnixNano())
		}
	}
	return sm.est
}

// MarkSeen marks the current generation as observed, resetting the stale
// counter. Call this after the planner has re-populated the degree cache
// from updated graph statistics.
func (sm *StatsManager) MarkSeen() {
	sm.lastSeen.Store(sm.gen.Load())
	sm.staleCount.Store(0)
	sm.lastRefreshNs.Store(time.Now().UnixNano())
}

// LastRefresh returns the time at which the cache was last refreshed, or
// the zero Time if it has never been refreshed.
func (sm *StatsManager) LastRefresh() time.Time {
	ns := sm.lastRefreshNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
