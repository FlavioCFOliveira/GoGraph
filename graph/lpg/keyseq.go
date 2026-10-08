package lpg

import (
	"sync"
	"sync/atomic"
)

// KeySequence is a graph's monotonic counter for minting synthetic node keys —
// keys a layer above the graph generates for nodes its caller did not name, such
// as the Cypher engine's anonymous CREATE. One sequence belongs to one [Graph]
// ([Graph.KeySequence]), so every engine writing that graph draws distinct values
// from it, and two graphs in one process mint independently: the keys a graph
// receives are a function of what was done to that graph, never of what other
// graphs in the process did.
//
// The zero value is ready for use. KeySequence is safe for concurrent use by any
// number of goroutines.
type KeySequence struct {
	n    atomic.Uint64
	seed sync.Once
}

// Add advances the sequence by step and returns the new value.
func (s *KeySequence) Add(step uint64) uint64 { return s.n.Add(step) }

// Load returns the current value.
func (s *KeySequence) Load() uint64 { return s.n.Load() }

// SeedOnce calls scan the first time it is invoked on s, and raises the sequence
// to scan's result when that is larger; every later call returns at once. A
// caller uses it to start the sequence past keys the graph already holds (keys
// recovered from disk), so minting rarely meets an occupied key.
func (s *KeySequence) SeedOnce(scan func() uint64) {
	s.seed.Do(func() {
		v := scan()
		for {
			cur := s.n.Load()
			if cur >= v || s.n.CompareAndSwap(cur, v) {
				return
			}
		}
	})
}

// KeySequence returns the graph's synthetic node-key sequence. Safe for
// concurrent use.
func (g *Graph[N, W]) KeySequence() *KeySequence { return &g.keySeq }
