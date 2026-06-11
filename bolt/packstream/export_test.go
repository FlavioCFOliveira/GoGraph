package packstream

import "math"

// MaxValueDepthForTest exposes the unexported maxValueDepth bound to black-box
// tests in package packstream_test. It exists solely so the nesting-depth
// regression tests can pin their payloads to the exact boundary without
// hard-coding the constant; production code never references it.
func MaxValueDepthForTest() int { return maxValueDepth }

// MaxDecodedCollectionBytesForTest exposes the unexported per-message
// decoded-memory budget so the amplification regression tests can pin their
// payloads to the exact boundary without hard-coding the constant.
func MaxDecodedCollectionBytesForTest() int { return maxDecodedCollectionBytes }

// ListElemCostForTest exposes the decoded cost charged per List element /
// Struct field.
func ListElemCostForTest() int { return listElemCost }

// MapEntryCostForTest exposes the decoded cost charged per Map entry.
func MapEntryCostForTest() int { return mapEntryCost }

// CollectionCostForTest exposes the fixed decoded cost charged per collection.
func CollectionCostForTest() int { return collectionCost }

// ChargeDecodedForTest exposes chargeDecoded so the budget arithmetic
// (boundary, cumulative accounting, failed-charge rollback) can be asserted
// directly and deterministically, without relying on process-global memory
// statistics.
func (d *Decoder) ChargeDecodedForTest(kind string, n, perElem int) error {
	return d.chargeDecoded(kind, n, perElem)
}

// SetUnboundedBudgetForTest reconfigures d as if its source length were
// unknown and lifts the fallback message ceiling to math.MaxInt. The
// wrap-range regression tests (TestWire32LengthPrefixWrapRejectedBeforeCast)
// use it to take the wire byte budget out of the equation: on a 64-bit
// platform a 32-bit length prefix above MaxInt32 cannot wrap the int
// conversion, so with a realistic budget the byte budget would mask the
// pre-conversion validation behind the same ErrLengthExceedsInput error.
// Production code never references this helper.
func (d *Decoder) SetUnboundedBudgetForTest() {
	d.remaining = unknownRemaining
	d.maxMessageBytes = math.MaxInt
}
