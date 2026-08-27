package csrfile

import (
	"errors"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// roundTripWeight publishes a 4-edge graph whose every weight is want, reads the
// file back, and returns the raw weights section together with the header kind.
//
// It asserts the ROUND TRIP rather than merely that the write returned nil: a
// writer that emitted the wrong number of bytes, or the right number of wrong
// ones, would satisfy an error-only check. That is exactly what rmp #2529 was
// about — a kind the documentation claimed and the writer did not deliver.
func roundTripWeight[W comparable](t *testing.T, want W) (WeightKind, []byte) {
	t.Helper()
	const edges = 4
	a := adjlist.New[int, W](adjlist.Config{Directed: true})
	for i := 0; i < edges; i++ {
		if err := a.AddEdge(i, (i+1)%edges, want); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "w.csr")
	hdr, err := WriteToFile(path, csr.BuildFromAdjList(a))
	if err != nil {
		t.Fatalf("WriteToFile[%T]: %v", want, err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open[%T]: %v", want, err)
	}
	defer func() { _ = r.Close() }()

	// COPIED before the deferred Close, deliberately. WeightsRaw returns a view
	// INTO THE MMAP, so returning it and asserting after Close reads unmapped
	// memory — which is a segfault, not a test failure, and cost one here before
	// the copy was added.
	raw := append([]byte(nil), r.WeightsRaw()...)
	if got, wantSize := len(raw), hdr.Weight.Size()*edges; got != wantSize {
		t.Errorf("[%T] weights section is %d byte(s), want %d (%d edges x %d)",
			want, got, wantSize, edges, hdr.Weight.Size())
	}
	return hdr.Weight, raw
}

// assertWeightsRoundTrip checks that every element of the raw section decodes
// back to want, by reinterpreting the bytes as []W — the same view the writer
// produced, read back from disk.
func assertWeightsRoundTrip[W comparable](t *testing.T, want W, raw []byte) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("[%T] no weight bytes to check", want)
	}
	n := len(raw) / int(unsafe.Sizeof(want))
	got := unsafe.Slice((*W)(unsafe.Pointer(&raw[0])), n) //nolint:gosec // the test reads back the same fixed-size view the writer wrote
	for i, g := range got {
		if g != want {
			t.Errorf("[%T] weight[%d] = %v, want %v: the value did not survive the round trip",
				want, i, g, want)
		}
	}
}

// TestWeightKinds_EveryDocumentedKindRoundTrips guards rmp #2529.
//
// weightKindOf advertised int, uint and uintptr and mapped them to the 8-byte
// kind, but the writer persisted through binary.Write, which refuses any slice
// whose element is not fixed-size — so a caller following the documentation got
// `binary.Write: some values are not fixed-sized`, an untyped error from a
// dependency. bool failed the same way. Separately, csrfile refused int8, uint16
// and bool outright while store/snapshot persisted them, so the two durable
// representations of one graph did not accept the same weights.
//
// The set is now reconciled with the snapshot writer, and every member is
// checked here by ROUND TRIP.
func TestWeightKinds_EveryDocumentedKindRoundTrips(t *testing.T) {
	t.Parallel()

	t.Run("1-byte", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			run  func(t *testing.T)
		}{
			{"int8", func(t *testing.T) {
				k, raw := roundTripWeight(t, int8(-7))
				if k != WeightUint8 {
					t.Errorf("kind = %d, want WeightUint8", k)
				}
				assertWeightsRoundTrip(t, int8(-7), raw)
			}},
			{"uint8", func(t *testing.T) {
				k, raw := roundTripWeight(t, uint8(250))
				if k != WeightUint8 {
					t.Errorf("kind = %d, want WeightUint8", k)
				}
				assertWeightsRoundTrip(t, uint8(250), raw)
			}},
			{"bool", func(t *testing.T) {
				k, raw := roundTripWeight(t, true)
				if k != WeightUint8 {
					t.Errorf("kind = %d, want WeightUint8", k)
				}
				assertWeightsRoundTrip(t, true, raw)
			}},
		} {
			t.Run(tc.name, tc.run)
		}
	})

	t.Run("2-byte", func(t *testing.T) {
		t.Parallel()
		k, raw := roundTripWeight(t, int16(-30000))
		if k != WeightUint16 {
			t.Errorf("kind = %d, want WeightUint16", k)
		}
		assertWeightsRoundTrip(t, int16(-30000), raw)

		k, raw = roundTripWeight(t, uint16(65000))
		if k != WeightUint16 {
			t.Errorf("kind = %d, want WeightUint16", k)
		}
		assertWeightsRoundTrip(t, uint16(65000), raw)
	})

	t.Run("platform-dependent widths", func(t *testing.T) {
		t.Parallel()
		// These are the three the godoc advertised and the writer refused.
		k, raw := roundTripWeight(t, int(-1234567))
		if k != WeightUint64 {
			t.Errorf("kind = %d, want WeightUint64", k)
		}
		assertWeightsRoundTrip(t, int(-1234567), raw)

		k, raw = roundTripWeight(t, uint(1234567))
		if k != WeightUint64 {
			t.Errorf("kind = %d, want WeightUint64", k)
		}
		assertWeightsRoundTrip(t, uint(1234567), raw)

		k, raw = roundTripWeight(t, uintptr(999))
		if k != WeightUint64 {
			t.Errorf("kind = %d, want WeightUint64", k)
		}
		assertWeightsRoundTrip(t, uintptr(999), raw)
	})

	t.Run("the kinds that already worked are unchanged", func(t *testing.T) {
		t.Parallel()
		if k, raw := roundTripWeight(t, int32(-9)); k != WeightUint32 {
			t.Errorf("int32 kind = %d, want WeightUint32", k)
		} else {
			assertWeightsRoundTrip(t, int32(-9), raw)
		}
		if k, raw := roundTripWeight(t, float64(2.5)); k != WeightFloat64 {
			t.Errorf("float64 kind = %d, want WeightFloat64", k)
		} else {
			assertWeightsRoundTrip(t, float64(2.5), raw)
		}
	})
}

// TestWeightKinds_UnsupportedIsRefusedByName is the other half of the acceptance
// criterion: what is not persisted must be refused with a TYPED error naming the
// kind, so a caller can discover the limit without reading the source.
//
// It also pins that the refusal is csrfile's own, not an opaque one leaking from
// encoding/binary — which is what a caller used to get for int, uint and uintptr.
func TestWeightKinds_UnsupportedIsRefusedByName(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, string](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, "not a weight"); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	_, err := WriteToFile(filepath.Join(t.TempDir(), "w.csr"), csr.BuildFromAdjList(a))
	if err == nil {
		t.Fatalf("an unsupported weight type was accepted")
	}
	if !errors.Is(err, ErrUnknownWeightKind) {
		t.Errorf("err = %v; want one satisfying errors.Is(err, ErrUnknownWeightKind)", err)
	}
	if got := err.Error(); got == ErrUnknownWeightKind.Error() {
		t.Errorf("err = %q; it must NAME the offending type, or a caller cannot tell which "+
			"weight was refused (rmp #2529)", got)
	}
}

// TestWeightKinds_MatchTheSnapshotWriter is the reconciliation itself, asserted
// rather than left to two lists that already drifted once.
//
// It compares csrfile's kind resolution against the widths store/snapshot's
// csrWeightSize assigns, for every type in the agreed set. The two packages own
// separate on-disk formats and keep separate code; what must agree is the SET
// and the WIDTH, which is what this checks.
func TestWeightKinds_MatchTheSnapshotWriter(t *testing.T) {
	t.Parallel()
	// width per type, as store/snapshot/writer.go csrWeightSize assigns it.
	cases := []struct {
		name  string
		kind  func() (WeightKind, error)
		width int
	}{
		{"struct{}", weightKindOf[struct{}], 0},
		{"int8", weightKindOf[int8], 1},
		{"uint8", weightKindOf[uint8], 1},
		{"bool", weightKindOf[bool], 1},
		{"int16", weightKindOf[int16], 2},
		{"uint16", weightKindOf[uint16], 2},
		{"int32", weightKindOf[int32], 4},
		{"uint32", weightKindOf[uint32], 4},
		{"float32", weightKindOf[float32], 4},
		{"int", weightKindOf[int], 8},
		{"uint", weightKindOf[uint], 8},
		{"uintptr", weightKindOf[uintptr], 8},
		{"int64", weightKindOf[int64], 8},
		{"uint64", weightKindOf[uint64], 8},
		{"float64", weightKindOf[float64], 8},
	}
	for _, tc := range cases {
		k, err := tc.kind()
		if err != nil {
			t.Errorf("%s: csrfile refuses a weight store/snapshot persists: %v", tc.name, err)
			continue
		}
		if got := k.Size(); got != tc.width {
			t.Errorf("%s: csrfile persists it at %d byte(s); store/snapshot's csrWeightSize "+
				"assigns %d. The two durable paths must agree on the WIDTH as well as the set "+
				"(rmp #2529)", tc.name, got, tc.width)
		}
	}
}
