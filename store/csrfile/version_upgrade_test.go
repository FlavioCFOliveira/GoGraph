package csrfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestCSRFile_V1Fixture opens the frozen v1 fixture and verifies that
// its Vertices and Edges slices can be iterated without panic. The
// TestCompat_V1FixtureMaps test already checks the header values;
// this test focuses on the read paths that traverse the mmap'd data.
func TestCSRFile_V1Fixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "v1", "sample.csr")
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer func() { _ = r.Close() }()

	h := r.Header()
	if h.NVertices == 0 {
		t.Fatalf("NVertices = 0, want > 0")
	}

	// Traverse every vertex's neighbour range without accessing
	// out-of-bounds indices. This is the minimum panic-freedom proof.
	verts := r.Vertices()
	edges := r.Edges()
	for id := uint64(0); id < h.NVertices; id++ {
		start := verts[id]
		var end uint64
		if id+1 < uint64(len(verts)) {
			end = verts[id+1]
		} else {
			end = uint64(len(edges))
		}
		if end < start {
			t.Errorf("node %d: end %d < start %d (corrupt offsets)", id, end, start)
		}
		// Touch each neighbour entry to detect any mmap fault.
		for i := start; i < end; i++ {
			_ = edges[i]
		}
	}
}

// TestCSRFile_FutureVersionRejected creates a fresh csrfile, patches
// the two-byte version field to CurrentVersion+1 via the Open path,
// and asserts that [Open] (not just [DecodeHeader]) returns
// [ErrUnsupportedVersion]. The TestCompat_FutureVersionRejected test
// already verifies DecodeHeader in isolation; this variant proves that
// the Open entry point surfaces the same error.
func TestCSRFile_FutureVersionRejected(t *testing.T) {
	t.Parallel()

	// Build and write a valid csrfile first.
	g, err := shapegen.Grid(3, 3, false).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	path := filepath.Join(t.TempDir(), "future.csr")
	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	// Read the raw bytes, patch version bytes 4:6 to a value beyond
	// CurrentVersion, and write back.
	data, err := os.ReadFile(path) //nolint:gosec // testdata under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < HeaderSize {
		t.Fatalf("file shorter than HeaderSize")
	}
	bumped := append([]byte(nil), data...)
	// bytes 4:6 are the little-endian version uint16.
	bumped[4] = byte(CurrentVersion + 1)
	bumped[5] = 0
	if err := os.WriteFile(path, bumped, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = Open(path)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Open with future version = %v, want ErrUnsupportedVersion", err)
	}
}
