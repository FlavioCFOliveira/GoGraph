package snapshot

import (
	"errors"
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshotPerRecordCountCapAgreement_2784 is the snapshot side of the drift
// tripwire for the module's cap on ONE edge-handle record.
//
// [maxPerRecordCount] is not only this package's ceiling any more. Since rmp
// #2784 store/txn refuses, at COMMIT, a transaction that would leave an edge
// handle carrying more labels or more properties than this — because such a
// record commits durably and then makes phase-1 capture fail on every
// checkpoint attempt, so the WAL prefix is never truncated and the WAL grows
// without bound for as long as that record lives in the graph.
//
// store/txn cannot import this package, so it declares the same number as its
// own maxSnapshotPerRecordCount (store/txn/handle_record_count.go). Each side
// pins the literal and names the other; this is the half that fails if this
// constant is moved alone.
func TestSnapshotPerRecordCountCapAgreement_2784(t *testing.T) {
	t.Parallel()

	if maxPerRecordCount != 1<<20 {
		t.Fatalf("maxPerRecordCount = %d, want 1<<20 (%d). This constant MUST equal "+
			"store/txn's maxSnapshotPerRecordCount (store/txn/handle_record_count.go), "+
			"which refuses an unfoldable edge-handle record at commit against this exact "+
			"number. Change both or neither (rmp #2784)",
			maxPerRecordCount, 1<<20)
	}
}

// TestEdgeHandleRecord_PerRecordCountBoundary_2784 pins the writer at the exact
// byte of the boundary, in BOTH directions and on BOTH dimensions.
//
// It is the load-bearing half of the #2784 evidence that store/txn's commit-time
// guard cannot be tested against directly. Proving the commit is refused one
// over the cap is worth nothing unless the snapshot really does refuse there:
// this is the test that says the number store/txn enforces is the number this
// format cannot exceed, and that the record one BELOW it is written rather than
// merely tolerated.
//
// The fixture repeats one interned key and one interned label rather than
// minting a million distinct ones, because the guards under test read only
// len(r.propKeys) and len(r.labels). That keeps the at-cap case to a ~60 MB
// fixture and a ~15 ms write of the real 17_825_824-byte record, instead of the
// several hundred MB a distinct-name fixture would cost to prove the same
// comparison.
func TestEdgeHandleRecord_PerRecordCountBoundary_2784(t *testing.T) {
	t.Parallel()

	write := func(nProps, nLabels int) (int64, error) {
		raw := edgeHandleRaw{src: 1, dst: 2, handle: 7}
		raw.labels = make([]string, nLabels)
		for i := range raw.labels {
			raw.labels[i] = "L"
		}
		raw.propKeys = make([]string, nProps)
		raw.propVals = make([]lpg.PropertyValue, nProps)
		for i := range raw.propKeys {
			raw.propKeys[i] = "k"
			raw.propVals[i] = lpg.Int64Value(int64(i))
		}
		var scratch [28]byte
		return writeEdgeHandleRecord(io.Discard, scratch[:], &raw,
			map[string]uint32{"L": 0}, map[string]uint32{"k": 0})
	}

	for _, tc := range []struct {
		what          string
		props, labels int
		wantRefused   bool
	}{
		{"properties at the cap", maxPerRecordCount, 0, false},
		{"properties one over the cap", maxPerRecordCount + 1, 0, true},
		{"labels at the cap", 0, maxPerRecordCount, false},
		{"labels one over the cap", 0, maxPerRecordCount + 1, true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			n, err := write(tc.props, tc.labels)
			switch {
			case tc.wantRefused && err == nil:
				t.Fatalf("writeEdgeHandleRecord ACCEPTED a record one over the cap "+
					"(props=%d labels=%d, cap %d) and wrote %d bytes. store/txn's "+
					"commit-time guard exists only because this refuses; if it stops "+
					"refusing, that guard now over-restricts (rmp #2784)",
					tc.props, tc.labels, maxPerRecordCount, n)
			case tc.wantRefused && !errors.Is(err, ErrFieldTooLong):
				t.Fatalf("refusal = %v; want one wrapping ErrFieldTooLong", err)
			case !tc.wantRefused && err != nil:
				t.Fatalf("writeEdgeHandleRecord REFUSED a record at exactly the cap "+
					"(props=%d labels=%d, cap %d): %v. store/txn admits this record at "+
					"commit, so refusing it here reopens rmp #2784 from the other side",
					tc.props, tc.labels, maxPerRecordCount, err)
			case !tc.wantRefused && n <= 0:
				t.Fatalf("at-cap record wrote %d bytes; want a real record", n)
			}
		})
	}
}
