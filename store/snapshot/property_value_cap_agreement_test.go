package snapshot

import "testing"

// TestPropertyValueCapAgreement_2750 is the snapshot side of the drift tripwire
// for the module's cap on a property value.
//
// [maxValueLen] is not only this reader's ceiling any more. Since rmp #2750
// store/txn refuses, at COMMIT, any property value whose SNAPSHOT encoding would
// exceed it — because a value that commits and cannot be folded makes phase-1
// capture fail on every checkpoint attempt, so the WAL prefix is never truncated
// and the WAL grows without bound for as long as that value lives in the graph.
//
// store/txn cannot import this package, so it declares the same number as its
// own maxSnapshotValueLen. Each side pins the literal and names the other; this
// is the half that fails if this constant is moved alone.
func TestPropertyValueCapAgreement_2750(t *testing.T) {
	t.Parallel()

	if maxValueLen != 1<<30 {
		t.Fatalf("maxValueLen = %d, want 1<<30 (%d). This constant MUST equal store/txn's "+
			"maxSnapshotValueLen (store/txn/txn.go), which refuses an unfoldable value at "+
			"commit against this exact number. Change both or neither (rmp #2750)",
			maxValueLen, 1<<30)
	}

	// The per-element widths store/txn's snapshotEncodedScalarLen mirrors. If one
	// of these changes, that mirror is wrong by that many bytes per element and
	// the commit-time guard silently bounds the wrong quantity — which is exactly
	// how the #2750 window opened in the first place.
	if fixed64ValueSize != 8 || timeValueSize != 16 || boolValueSize != 1 {
		t.Fatalf("per-element widths changed (fixed64=%d time=%d bool=%d); store/txn's "+
			"snapshotEncodedScalarLen mirrors these and must be updated with them (rmp #2750)",
			fixed64ValueSize, timeValueSize, boolValueSize)
	}
}
