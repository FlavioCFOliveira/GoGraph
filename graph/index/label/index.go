// Package label provides a Roaring-bitmap-backed inverted index from
// label identifiers to the NodeIDs that carry them.
//
// The index is the substrate for label-filtered queries such as
// "find every node with label Person and label Active": each label
// is represented by a 64-bit Roaring bitmap, and compound queries
// are answered via bitmap intersection / union, which Roaring
// implements with run-length and array-bitmap hybrids.
//
// Index is safe for concurrent use.
package label

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/graph"
)

// Index maps label identifiers (uint32) to the set of NodeIDs that
// carry them. Different LabelID namespaces (vertices, edges) should
// use distinct Index instances.
type Index struct {
	mu   sync.RWMutex
	bits map[uint32]*roaring64.Bitmap
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{bits: make(map[uint32]*roaring64.Bitmap)}
}

// Add records that node carries label.
func (i *Index) Add(label uint32, node graph.NodeID) {
	i.mu.Lock()
	bm, ok := i.bits[label]
	if !ok {
		bm = roaring64.New()
		i.bits[label] = bm
	}
	bm.Add(uint64(node))
	i.mu.Unlock()
}

// Remove records that node no longer carries label. No-op if absent.
func (i *Index) Remove(label uint32, node graph.NodeID) {
	i.mu.Lock()
	if bm, ok := i.bits[label]; ok {
		bm.Remove(uint64(node))
		if bm.IsEmpty() {
			delete(i.bits, label)
		}
	}
	i.mu.Unlock()
}

// Count returns the number of NodeIDs that carry label.
func (i *Index) Count(label uint32) uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if bm, ok := i.bits[label]; ok {
		return bm.GetCardinality()
	}
	return 0
}

// Has reports whether node carries label.
func (i *Index) Has(label uint32, node graph.NodeID) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	bm, ok := i.bits[label]
	if !ok {
		return false
	}
	return bm.Contains(uint64(node))
}

// Intersect returns a fresh Roaring bitmap containing the NodeIDs
// that carry every supplied label. Calling with no labels returns
// the empty bitmap.
func (i *Index) Intersect(labels ...uint32) *roaring64.Bitmap {
	if len(labels) == 0 {
		return roaring64.New()
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	first, ok := i.bits[labels[0]]
	if !ok {
		return roaring64.New()
	}
	result := first.Clone()
	for _, l := range labels[1:] {
		bm, ok := i.bits[l]
		if !ok {
			return roaring64.New()
		}
		result.And(bm)
		if result.IsEmpty() {
			return result
		}
	}
	return result
}

// Union returns a fresh Roaring bitmap containing the NodeIDs that
// carry any of the supplied labels.
func (i *Index) Union(labels ...uint32) *roaring64.Bitmap {
	result := roaring64.New()
	if len(labels) == 0 {
		return result
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, l := range labels {
		if bm, ok := i.bits[l]; ok {
			result.Or(bm)
		}
	}
	return result
}
