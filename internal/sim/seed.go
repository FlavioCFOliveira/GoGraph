// Package sim implements a deterministic simulation testing (DST) harness for
// the GoGraph engine, modelled on TigerBeetle's VOPR. The simulator is
// seed-reproducible, single-goroutine, and tick-driven: it drives the real
// cypher.Engine against an in-memory store, maintains a shadow oracle model of
// what the graph must contain, and verifies ACID and graph invariants after
// every operation.
//
// The whole point of the harness is determinism: a given seed always produces
// the exact same sequence of actors, operations, parameters, and injected
// faults, so any violation can be reproduced bit-for-bit from its seed alone.
// To preserve that property, every probabilistic decision anywhere in the
// package must draw from a single [Seed]; no other source of randomness (no
// global math/rand, no time.Now, no map-iteration ordering decisions) may
// influence control flow.
//
// # Concurrency contract
//
// No type in this package is safe for concurrent use. The simulator runs on a
// single goroutine and spawns none; the determinism guarantee depends on a
// single, totally-ordered stream of draws from [Seed]. Sharing any value in
// this package across goroutines is a programmer error.
package sim

import "math/rand/v2"

// seedMix is XORed with the seed value to derive the second PCG stream word, so
// that two simulations seeded with adjacent integers do not share a low-order
// bit pattern in their generator state.
const seedMix uint64 = 0xdeadbeefcafebabe

// Seed is the single source of randomness for an entire simulation. Every
// probabilistic decision — which actor runs, which operation it emits, the
// parameter values it binds, and whether the disk injects a fault — draws from
// one Seed, so the complete simulation is a pure function of the seed value.
//
// Seed wraps a deterministic PCG generator ([math/rand/v2.PCG]) seeded from the
// seed value alone; it never consults the operating system, a global generator,
// or the wall clock. The original value is retained so it can be reported and
// replayed.
//
// # Concurrency contract
//
// Seed is NOT safe for concurrent use. It backs the single-goroutine
// simulation loop and its draw order is load-bearing for reproducibility;
// concurrent draws would interleave non-deterministically and break replay.
type Seed struct {
	val uint64
	rng *rand.Rand
}

// NewSeed returns a Seed whose generator is initialised deterministically from
// val. The two PCG stream words are val and val^seedMix, so distinct seed
// values yield distinct generator states.
func NewSeed(val uint64) *Seed {
	return &Seed{
		val: val,
		//nolint:gosec // G404: deterministic, non-cryptographic PRNG is the
		// entire point — the simulation must be a reproducible function of the
		// seed, which a crypto/rand source would defeat.
		rng: rand.New(rand.NewPCG(val, val^seedMix)),
	}
}

// Value returns the seed value this Seed was constructed with. Printing it lets
// a failing run be replayed exactly.
func (s *Seed) Value() uint64 { return s.val }

// Bool returns true with probability p and false otherwise. p is clamped to
// [0.0, 1.0]: p <= 0 always returns false, p >= 1 always returns true. Each
// call consumes exactly one float64 draw, keeping the draw stream stable
// regardless of p.
func (s *Seed) Bool(p float64) bool {
	switch {
	case p <= 0:
		// Still consume a draw so the stream position is independent of p.
		_ = s.rng.Float64()
		return false
	case p >= 1:
		_ = s.rng.Float64()
		return true
	default:
		return s.rng.Float64() < p
	}
}

// IntN returns a uniform integer in [0, n). It panics if n <= 0, mirroring the
// contract of [math/rand/v2.Rand.IntN].
func (s *Seed) IntN(n int) int { return s.rng.IntN(n) }

// Float64 returns a uniform float64 in [0.0, 1.0).
func (s *Seed) Float64() float64 { return s.rng.Float64() }

// Uint64N returns a uniform unsigned integer in [0, n). It panics if n == 0,
// mirroring the contract of [math/rand/v2.Rand.Uint64N].
func (s *Seed) Uint64N(n uint64) uint64 { return s.rng.Uint64N(n) }

// Pick returns a uniformly-chosen element of items. It panics if items is
// empty, which signals a programmer error (a workload that offers no choices).
func (s *Seed) Pick(items []string) string {
	if len(items) == 0 {
		panic("sim: Seed.Pick on empty slice")
	}
	return items[s.rng.IntN(len(items))]
}

// Shuffle returns a new slice holding the elements of items in a
// deterministically-shuffled order, using an in-place Fisher–Yates pass over
// the copy. The input slice is never mutated. The result is a function of the
// generator state alone, so the same seed produces the same permutation.
func (s *Seed) Shuffle(items []string) []string {
	out := make([]string, len(items))
	copy(out, items)
	for i := len(out) - 1; i > 0; i-- {
		j := s.rng.IntN(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}
