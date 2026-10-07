//go:build !race

package cypher

// readPathSameLabelAllocCeiling is the third arm of [TestReadPathAllocationCeiling]:
// one cache-hit execution of [readPathAllocQuery] while a node that gained label N
// after the pinning reader began is still unreclaimed (rmp #2776).
//
// It has two values, one per build mode, because allocation counts are not
// invariant under the race detector: the compiler disables optimisations when it
// instruments. Measured in the test's isolated child, Apple M4, Go 1.27.1, on
// this arm exactly (it runs after the second arm, so the unrelated node that arm
// left live is a suspect too):
//
//	build   | before rmp #2776 | after
//	plain   |       35         |  23
//	-race   |       37         |  25
//
// Before, the count cloned the label's image in order to correct it. The drained
// arms read 20 in both modes. What remains above them is the correction's suspect
// sampling and deduplication, which grows with the number of unreclaimed suspects
// and not with the size of the label; lpg's
// TestLabelCountAsOf_CountedCorrectionClonesNothing pins the substrate's share at
// one suspect. Set at the measured values with no headroom, like
// [readPathAllocCeiling].
const readPathSameLabelAllocCeiling = 23
