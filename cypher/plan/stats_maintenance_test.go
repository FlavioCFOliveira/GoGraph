package plan_test

import (
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/plan"
)

// TestStatsManager_FreshAfterMark verifies that a new manager is fresh
// after MarkSeen is called with the current (zero) generation.
func TestStatsManager_FreshAfterMark(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)
	sm := plan.NewStatsManager(est, 0)

	sm.MarkSeen()
	if !sm.IsFresh() {
		t.Fatal("expected fresh after MarkSeen")
	}
}

// TestStatsManager_StaleAfterNotify verifies that NotifyRotation causes
// IsFresh to return false.
func TestStatsManager_StaleAfterNotify(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)
	sm := plan.NewStatsManager(est, 0)

	sm.MarkSeen()
	if !sm.IsFresh() {
		t.Fatal("expected fresh after MarkSeen")
	}

	sm.NotifyRotation()
	if sm.IsFresh() {
		t.Fatal("expected stale after NotifyRotation")
	}
}

// TestStatsManager_BoundedStaleEviction sets maxStaleCalls=3, seeds the
// degree cache, calls Estimator() while stale, and verifies that the
// cache is cleared on the 3rd stale call (eviction boundary).
func TestStatsManager_BoundedStaleEviction(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)
	est.UpdateDegreeCache(1, 2, 3, 4.5) // seed a cached value

	sm := plan.NewStatsManager(est, 3) // evict on 3rd stale call

	sm.NotifyRotation() // mark stale

	// Calls 1 and 2: stale but cache not yet evicted.
	for i := 0; i < 2; i++ {
		e := sm.Estimator()
		if got := e.AvgOutDegree(1, 2, 3); got == 1.0 {
			t.Errorf("call %d: cache evicted prematurely (got default 1.0, want 4.5)", i+1)
		}
	}

	// Call 3: triggers eviction.
	sm.Estimator()

	// Value must now be gone — default 1.0 returned.
	if got := sm.Estimator().AvgOutDegree(1, 2, 3); got != 1.0 {
		t.Errorf("expected default 1.0 after cache eviction, got %f", got)
	}
}

// TestStatsManager_ConcurrentNotify runs 10 goroutines simultaneously
// calling NotifyRotation and Estimator. With -race it verifies no data
// races; with no panics it confirms safe concurrent use.
func TestStatsManager_ConcurrentNotify(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)
	sm := plan.NewStatsManager(est, 5)

	const goroutines = 10
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				sm.NotifyRotation()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = sm.Estimator()
			}
		}()
	}

	wg.Wait() // must not panic or race
}

// TestStatsManager_LastRefresh verifies that LastRefresh returns a
// non-zero time after MarkSeen is called.
func TestStatsManager_LastRefresh(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)
	sm := plan.NewStatsManager(est, 0)

	if !sm.LastRefresh().IsZero() {
		t.Fatal("expected zero time before any MarkSeen")
	}

	before := time.Now()
	sm.MarkSeen()
	after := time.Now()

	lr := sm.LastRefresh()
	if lr.IsZero() {
		t.Fatal("expected non-zero LastRefresh after MarkSeen")
	}
	if lr.Before(before) || lr.After(after) {
		t.Errorf("LastRefresh %v not in expected window [%v, %v]", lr, before, after)
	}
}
