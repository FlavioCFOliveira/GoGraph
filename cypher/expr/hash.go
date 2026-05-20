package expr

// hash.go — row-level hashing helpers for the Cypher executor.
//
// HashRow produces a single uint64 fingerprint for a Row (slice of Values) by
// combining each element's Hash() using a polynomial rolling hash. It is used
// by the Distinct and EagerAggregation operators to form map keys without
// allocating a composite key string.
//
// Collision probability follows birthday paradox at 64-bit width; operators
// that rely on HashRow must still perform an element-wise equality check before
// declaring two rows equal.
//
// # Concurrency
//
// HashRow is pure and safe for concurrent use.

// HashRow returns a uint64 hash that represents the ordered sequence of values
// in row. Two rows whose elements are pairwise equal (per openCypher equality
// semantics) will have the same hash. The inverse is not guaranteed — callers
// must verify equality after a hash match.
//
// NULL values contribute their own hash (0) to the mix; they participate in
// grouping but never compare equal to any value including another NULL.
func HashRow(row []Value) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	for _, v := range row {
		h = h*prime ^ v.Hash()
	}
	return h
}
