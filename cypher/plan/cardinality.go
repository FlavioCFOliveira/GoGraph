// Package plan provides the cost-based planner for the Cypher executor.
// This file implements selectivity estimation used by the scan-strategy
// selection rule (task-283).
//
// When the underlying graph can rotate (new CSR snapshot published via
// generation.Publisher or a Tx commit that alters graph structure), wrap
// an IndexEstimator with a StatsManager. StatsManager provides
// generation-aware cache invalidation so that avg-out-degree estimates
// remain coherent with the live graph without requiring callers to manage
// the estimator's degree cache directly.
package plan

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// Estimator provides cardinality estimates for physical plan operators.
// All methods return uint64 row-count estimates; callers treat 0 as
// "unknown, assume 1".
type Estimator interface {
	// LabelCount returns the number of nodes carrying the given label.
	LabelCount(label uint32) uint64
	// HashLookupCount returns the estimated number of nodes carrying
	// (property == value) for an exact-match hash index. Returns 0 when
	// no hash index is registered for the property.
	HashLookupCount(property uint32, value any) uint64
	// BTreeRangeCount returns the estimated number of nodes in the range
	// [lo, hi] for a B+ tree index. Returns 0 when no btree index is
	// registered for the property.
	BTreeRangeCount(property uint32, lo, hi string) uint64
	// AvgOutDegree returns the average out-degree of nodes carrying
	// srcLabel, following edges of relType to nodes carrying dstLabel.
	// Returns 1.0 when insufficient data is available.
	AvgOutDegree(srcLabel, relType, dstLabel uint32) float64
}

// distinctValuer is optionally implemented by index.Subscriber
// implementations that expose the count of distinct indexed values.
// Both hash.Index[V] and btree.Index[V] implement this shape, though
// their return types differ (uint64 vs int). We use a uint64 variant
// here; btree.Index[V] exposes DistinctValues() int so we cannot
// use a single interface — see btreeDistinctValuer below.
type distinctValuer interface {
	DistinctValues() uint64
}

// btreeDistinctValuer matches btree.Index[V].DistinctValues() int.
type btreeDistinctValuer interface {
	DistinctValues() int
}

// degreeKey is the cache key for avg-out-degree lookups.
type degreeKey struct{ srcLabel, relType, dstLabel uint32 }

// IndexEstimator implements Estimator by consulting the index.Manager,
// the label.Index, and an avg-out-degree cache computed from the CSR.
//
// IndexEstimator is safe for concurrent use.
type IndexEstimator struct {
	mu          sync.RWMutex
	labelIdx    *label.Index
	idxMgr      *index.Manager
	degreeCache map[degreeKey]float64
}

// NewIndexEstimator returns an estimator backed by the given label index
// and secondary index manager. Both may be nil (returns a zero estimator
// that always returns 0/1.0).
func NewIndexEstimator(labelIdx *label.Index, mgr *index.Manager) *IndexEstimator {
	return &IndexEstimator{
		labelIdx:    labelIdx,
		idxMgr:      mgr,
		degreeCache: make(map[degreeKey]float64),
	}
}

// LabelCount returns the number of nodes carrying the given label.
// Returns 0 when the label index is nil or the label is unknown.
func (e *IndexEstimator) LabelCount(lbl uint32) uint64 {
	if e.labelIdx == nil {
		return 0
	}
	return e.labelIdx.Count(lbl)
}

// HashLookupCount returns an estimate of nodes where the indexed
// property equals value. The estimate is computed as:
//
//	totalNodes / max(1, distinctValues)
//
// where totalNodes is the sum of all label-zero counts (i.e. the count
// stored under label 0 is treated as total-node sentinel; when not
// available the fallback is LabelCount(0)). When no subscriber
// implements the DistinctValues capability, 0 is returned.
//
// The property parameter is accepted for interface conformance and
// future routing once indexes expose their property IDs; currently
// the estimator consults the first subscriber that provides
// a DistinctValues() uint64 capability.
func (e *IndexEstimator) HashLookupCount(_ uint32, _ any) uint64 {
	if e.idxMgr == nil {
		return 0
	}
	names := e.idxMgr.ListIndexes()
	for _, name := range names {
		sub, err := e.idxMgr.GetIndex(name)
		if err != nil {
			continue
		}
		if dv, ok := sub.(distinctValuer); ok {
			distinct := dv.DistinctValues()
			if distinct == 0 {
				distinct = 1
			}
			total := e.LabelCount(0)
			if total == 0 {
				total = 1
			}
			return total / distinct
		}
	}
	return 0
}

// BTreeRangeCount returns an estimate of nodes whose indexed property
// value falls within [lo, hi]. The estimate applies a fixed 30%
// selectivity over the per-subscriber distinct-value count:
//
//	ceil(distinctValues * 0.30)
//
// When a subscriber implements btreeDistinctValuer (btree.Index[V]),
// that count is used. When no such subscriber exists, the fallback is
// LabelCount(0) / 10 (10% global selectivity).
//
// lo and hi are accepted for interface conformance; future
// implementations may use them to compute a tighter histogram bucket.
func (e *IndexEstimator) BTreeRangeCount(_ uint32, _, _ string) uint64 {
	if e.idxMgr == nil {
		return 0
	}
	names := e.idxMgr.ListIndexes()
	for _, name := range names {
		sub, err := e.idxMgr.GetIndex(name)
		if err != nil {
			continue
		}
		if dv, ok := sub.(btreeDistinctValuer); ok {
			distinct := uint64(dv.DistinctValues())
			if distinct == 0 {
				distinct = 1
			}
			// 30% selectivity over distinct values.
			est := (distinct*30 + 99) / 100
			if est == 0 {
				est = 1
			}
			return est
		}
	}
	// Fallback: 10% of total nodes.
	total := e.LabelCount(0)
	if total == 0 {
		return 0
	}
	return (total + 9) / 10
}

// AvgOutDegree returns the cached average out-degree for the triple
// (srcLabel, relType, dstLabel). Returns 1.0 when no cached value is
// available. The cache is populated externally via UpdateDegreeCache
// after a new CSR snapshot is built.
func (e *IndexEstimator) AvgOutDegree(srcLabel, relType, dstLabel uint32) float64 {
	k := degreeKey{srcLabel, relType, dstLabel}
	e.mu.RLock()
	v, ok := e.degreeCache[k]
	e.mu.RUnlock()
	if ok {
		return v
	}
	return 1.0
}

// UpdateDegreeCache updates the avg-out-degree cache for the triple
// (srcLabel, relType, dstLabel). This is called by snapshot rotation
// hooks after a new CSR is built.
func (e *IndexEstimator) UpdateDegreeCache(srcLabel, relType, dstLabel uint32, avg float64) {
	k := degreeKey{srcLabel, relType, dstLabel}
	e.mu.Lock()
	e.degreeCache[k] = avg
	e.mu.Unlock()
}
