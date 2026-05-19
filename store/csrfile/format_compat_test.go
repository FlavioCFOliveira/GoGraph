package csrfile

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompat_V1FixtureMaps asserts the frozen v1 csrfile fixture
// committed at store/csrfile/testdata/v1/sample.csr opens under the
// current build with the expected header shape. Regenerate with:
//
//	go run ./cmd/fmtfixture -pkg csrfile
//
// Any mismatch flags an unintended on-disk-format change.
func TestCompat_V1FixtureMaps(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "v1", "sample.csr")
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer func() { _ = r.Close() }()
	h := r.Header()
	if h.Version != CurrentVersion {
		t.Fatalf("Header.Version = %d, want %d", h.Version, CurrentVersion)
	}
	if h.Alignment != Alignment {
		t.Fatalf("Header.Alignment = %d, want %d", h.Alignment, Alignment)
	}
	// Fixture: V=32, E=96, unweighted (BuildFixture uses struct{}).
	if h.NVertices == 0 {
		t.Fatalf("NVertices = 0, want > 0")
	}
	if h.NEdges == 0 {
		t.Fatalf("NEdges = 0, want > 0")
	}
	if h.Weight != WeightAbsent {
		t.Fatalf("Weight = %d, want WeightAbsent (0)", h.Weight)
	}
}

// TestCompat_FutureVersionRejected synthesises a v1 file whose
// version field is bumped past CurrentVersion and verifies the
// header decoder returns ErrUnsupportedVersion.
func TestCompat_FutureVersionRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "v1", "sample.csr")
	data, err := os.ReadFile(path) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < HeaderSize {
		t.Fatalf("fixture shorter than HeaderSize")
	}
	bumped := append([]byte(nil), data...)
	binary.LittleEndian.PutUint16(bumped[4:6], CurrentVersion+1)
	if _, err := DecodeHeader(bumped[:HeaderSize]); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("future-version DecodeHeader = %v, want ErrUnsupportedVersion", err)
	}
}
