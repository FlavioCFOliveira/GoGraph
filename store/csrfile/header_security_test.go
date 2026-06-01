package csrfile

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// writeValidCSR writes a small, valid weighted-or-unweighted csrfile
// and returns its raw bytes. The returned slice is the full on-disk
// image: header + sections + trailing CRC32C.
func writeValidCSR(t *testing.T) []byte {
	t.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 32; i++ {
		if err := a.AddEdge(i, (i+1)%32, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(i, (i+5)%32, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src := filepath.Join(t.TempDir(), "ok.csr")
	if _, err := WriteToFile[struct{}](src, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	data, err := os.ReadFile(src) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

// resealCRC recomputes the trailing CRC32C of a csrfile image so that
// it is internally consistent again after a header mutation. tailOff
// is the TailCRCOffset the image claims; the CRC is taken over
// data[:tailOff] and written little-endian at data[tailOff:]. When
// tailOff is out of range for data, the image is left untouched (the
// mutation under test is one where validate rejects before the CRC is
// ever consulted, so a correct CRC is unnecessary).
func resealCRC(data []byte, tailOff uint64) {
	// Overflow-safe range check: tailOff is an untrusted, possibly
	// hostile value (e.g. ^uint64(0)), so tailOff+4 must not be
	// computed before the comparison.
	if tailOff > uint64(len(data)) || uint64(len(data))-tailOff < 4 {
		return
	}
	sum := crc32.Update(0, crc32.MakeTable(crc32.Castagnoli), data[:tailOff])
	binary.LittleEndian.PutUint32(data[tailOff:tailOff+4], sum)
}

// openNoPanic opens path inside a recover guard, proving that Open
// never lets a panic escape regardless of how hostile the header is.
func openNoPanic(t *testing.T, path string) (r *Reader, err error) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Open panicked on hostile header: %v", rec)
		}
	}()
	return Open(path)
}

// TestOpen_HostileHeaderRejected mutates each structural header field
// to a hostile value, recomputes a VALID tail CRC over the mutated
// image (so the CRC gate passes and validate is actually exercised),
// and asserts that Open returns a typed ErrFileCorrupted and never
// panics. This is the H1 regression battery: it covers out-of-range
// offsets/counts and the integer-overflow wrap on 8*NEdges.
func TestOpen_HostileHeaderRejected(t *testing.T) {
	t.Parallel()

	base := writeValidCSR(t)
	// Decode the genuine header so mutations are relative to a real,
	// self-consistent layout.
	good, err := DecodeHeader(base)
	if err != nil {
		t.Fatalf("DecodeHeader(base): %v", err)
	}

	// Field byte offsets inside the 64-byte header (see EncodeHeader).
	const (
		offNVertices      = 8  // [8:16]
		offNEdges         = 16 // [16:24]
		offVerticesOffset = 32 // [32:40]
		offEdgesOffset    = 40 // [40:48]
		offWeightsOffset  = 48 // [48:56]
		offTailCRCOffset  = 56 // [56:64]
	)

	// A count chosen so 8*count wraps uint64 back into a small,
	// in-bounds-looking value: 8 * (2^61 + k) == 8*k (mod 2^64).
	// With k small, the wrapped byte length is tiny, so a naive
	// off+8*N <= len(mm) check would pass while unsafe.Slice builds
	// a 2^61-element view far past the mmap — the exact H1 OOB case.
	const wrapNEdges = (uint64(1) << 61) + 4

	// isTypedSentinel reports whether err is one of the package's
	// typed rejection sentinels. Every hostile-header mutation must
	// fail with one of these (and must never panic). Note that
	// clobbering the start of the file via a relocated tail CRC can
	// surface as ErrBadMagic rather than ErrFileCorrupted; both are
	// valid typed rejections.
	isTypedSentinel := func(err error) bool {
		return errors.Is(err, ErrFileCorrupted) ||
			errors.Is(err, ErrBadMagic) ||
			errors.Is(err, ErrUnsupportedVersion) ||
			errors.Is(err, ErrUnsupportedByteOrder) ||
			errors.Is(err, ErrUnknownWeightKind) ||
			errors.Is(err, ErrHeaderTooShort)
	}

	cases := []struct {
		name     string
		fieldOff int
		value    uint64
		// mustBeCorrupted pins the H1-specific cases (out-of-range and
		// integer-overflow) to ErrFileCorrupted; the rest accept any
		// typed sentinel. All must reject and none may panic.
		mustBeCorrupted bool
	}{
		{"NVertices-overflow-wrap", offNVertices, wrapNEdges, true},
		{"NVertices-huge", offNVertices, 1 << 40, true},
		{"NVertices-shifted", offNVertices, good.NVertices + 8, true},
		{"NEdges-overflow-wrap", offNEdges, wrapNEdges, true},
		{"NEdges-near-2pow61", offNEdges, (uint64(1) << 61), true},
		{"NEdges-huge", offNEdges, 1 << 40, true},
		{"NEdges-shifted", offNEdges, good.NEdges + 8, true},
		{"VerticesOffset-zero", offVerticesOffset, 0, true},
		{"VerticesOffset-huge", offVerticesOffset, 1 << 40, true},
		{"VerticesOffset-maxuint", offVerticesOffset, ^uint64(0), true},
		{"EdgesOffset-zero", offEdgesOffset, 0, true},
		{"EdgesOffset-huge", offEdgesOffset, 1 << 40, true},
		{"EdgesOffset-before-vertices", offEdgesOffset, good.VerticesOffset - 64, true},
		{"WeightsOffset-nonzero-when-absent", offWeightsOffset, good.EdgesOffset, true},
		{"WeightsOffset-huge", offWeightsOffset, 1 << 40, true},
		{"TailCRCOffset-zero", offTailCRCOffset, 0, false},
		{"TailCRCOffset-huge", offTailCRCOffset, 1 << 40, true},
		{"TailCRCOffset-maxuint", offTailCRCOffset, ^uint64(0), true},
		{"TailCRCOffset-minus-one-section", offTailCRCOffset, good.TailCRCOffset - 64, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := make([]byte, len(base))
			copy(data, base)
			binary.LittleEndian.PutUint64(data[tc.fieldOff:tc.fieldOff+8], tc.value)

			// Reseal the CRC over the (possibly relocated) tail so the
			// CRC gate cannot be what rejects the file — we want to
			// prove validate does the rejecting. The tail offset is the
			// (possibly mutated) value now in the header.
			tail := binary.LittleEndian.Uint64(data[offTailCRCOffset : offTailCRCOffset+8])
			resealCRC(data, tail)

			path := filepath.Join(t.TempDir(), "hostile.csr")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			r, openErr := openNoPanic(t, path)
			if openErr == nil {
				if r != nil {
					_ = r.Close()
				}
				t.Fatalf("Open accepted hostile header %q; want a typed error", tc.name)
			}
			if tc.mustBeCorrupted {
				if !errors.Is(openErr, ErrFileCorrupted) {
					t.Fatalf("Open(%q) = %v; want errors.Is(ErrFileCorrupted)", tc.name, openErr)
				}
				return
			}
			if !isTypedSentinel(openErr) {
				t.Fatalf("Open(%q) = %v; want a typed csrfile sentinel", tc.name, openErr)
			}
		})
	}
}

// TestOpen_ValidFileStillOpens is the positive regression: a normally
// written file must pass validate and read back its sections intact.
func TestOpen_ValidFileStillOpens(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	const n = 48
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	path := filepath.Join(t.TempDir(), "valid.csr")
	wantHeader, err := WriteToFile[struct{}](path, c)
	if err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open of valid file rejected: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	got := r.Header()
	if got != wantHeader {
		t.Fatalf("header mismatch:\n got %+v\nwant %+v", got, wantHeader)
	}
	if uint64(len(r.Vertices())) != wantHeader.NVertices {
		t.Fatalf("len(Vertices) = %d, want %d", len(r.Vertices()), wantHeader.NVertices)
	}
	if uint64(len(r.Edges())) != wantHeader.NEdges {
		t.Fatalf("len(Edges) = %d, want %d", len(r.Edges()), wantHeader.NEdges)
	}
	// The CSR offsets array is monotonic non-decreasing; a smoke check
	// that the reinterpreted view is coherent.
	verts := r.Vertices()
	for i := 1; i < len(verts); i++ {
		if verts[i] < verts[i-1] {
			t.Fatalf("vertices offsets not monotonic at %d: %d < %d", i, verts[i], verts[i-1])
		}
	}
}

// TestHeaderValidate_OverflowSafeLayout asserts at the unit level that
// Layout refuses to produce a layout for counts whose section sizes
// overflow uint64, and that validate consequently rejects such a
// header. This pins the overflow-safety contract independently of the
// mmap/Open path.
func TestHeaderValidate_OverflowSafeLayout(t *testing.T) {
	t.Parallel()

	overflowCounts := []struct {
		name              string
		nVertices, nEdges uint64
		weight            WeightKind
	}{
		{"NEdges-wrap-on-8x", 0, (uint64(1) << 61) + 1, WeightAbsent},
		{"NVertices-wrap-on-8x", (uint64(1) << 61) + 1, 0, WeightAbsent},
		{"NEdges-max", 0, ^uint64(0), WeightAbsent},
		{"weights-wrap-on-8x", 0, (uint64(1) << 61) + 1, WeightUint64},
	}
	for _, oc := range overflowCounts {
		oc := oc
		t.Run("Layout/"+oc.name, func(t *testing.T) {
			t.Parallel()
			h, total := Layout(oc.nVertices, oc.nEdges, oc.weight)
			if total != 0 || h != (Header{}) {
				t.Fatalf("Layout(%d,%d,%d) = (%+v, %d); want zero header and zero total on overflow",
					oc.nVertices, oc.nEdges, oc.weight, h, total)
			}
		})
	}

	// validate must reject a header carrying overflowing counts even
	// before considering offsets.
	t.Run("validate/overflow", func(t *testing.T) {
		t.Parallel()
		h := Header{NEdges: (uint64(1) << 61) + 1, Weight: WeightAbsent}
		if err := h.validate(1024); !errors.Is(err, ErrFileCorrupted) {
			t.Fatalf("validate(overflow header) = %v; want errors.Is(ErrFileCorrupted)", err)
		}
	})
}
