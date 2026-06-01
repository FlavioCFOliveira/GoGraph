package csrfile

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestCSRFile_SectionAlignment verifies that every typed section in
// the file starts at an offset that is a multiple of [Alignment] (64),
// and that the tail CRC offset is at least 8-byte aligned. Both the
// in-memory Header values and the raw file bytes are checked.
func TestCSRFile_SectionAlignment(t *testing.T) {
	t.Parallel()

	// Build a small directed graph so the edge and vertex sections are
	// both non-empty and the writer must actually lay them out.
	g, err := shapegen.Grid(4, 4, true).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	path := filepath.Join(t.TempDir(), "alignment.csr")
	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	h := r.Header()

	// The header constant stored in the file must match the build constant.
	if h.Alignment != Alignment {
		t.Errorf("Header.Alignment = %d, want %d", h.Alignment, Alignment)
	}

	// Every typed section must start on a Alignment-byte boundary.
	if h.VerticesOffset%Alignment != 0 {
		t.Errorf("VerticesOffset %d not %d-byte aligned", h.VerticesOffset, Alignment)
	}
	if h.EdgesOffset%Alignment != 0 {
		t.Errorf("EdgesOffset %d not %d-byte aligned", h.EdgesOffset, Alignment)
	}
	// WeightsOffset is 0 for unweighted files; if present it must be aligned.
	if h.WeightsOffset != 0 && h.WeightsOffset%Alignment != 0 {
		t.Errorf("WeightsOffset %d not %d-byte aligned", h.WeightsOffset, Alignment)
	}
	// TailCRC is a uint32; the spec requires it to be at least 8-byte
	// aligned (AlignUp rounds the preceding section up to Alignment which
	// is a multiple of 8).
	if h.TailCRCOffset%8 != 0 {
		t.Errorf("TailCRCOffset %d not 8-byte aligned", h.TailCRCOffset)
	}

	// Verify the same invariants hold in the raw byte layout of the
	// mmap'd file — this catches any divergence between the cached
	// header and the on-disk content.
	if len(r.mm) == 0 {
		t.Fatal("mmap region is empty")
	}
	rawH, decErr := DecodeHeader(r.mm[:HeaderSize])
	if decErr != nil {
		t.Fatalf("DecodeHeader from raw bytes: %v", decErr)
	}
	if rawH.VerticesOffset%Alignment != 0 {
		t.Errorf("raw VerticesOffset %d not %d-byte aligned", rawH.VerticesOffset, Alignment)
	}
	if rawH.EdgesOffset%Alignment != 0 {
		t.Errorf("raw EdgesOffset %d not %d-byte aligned", rawH.EdgesOffset, Alignment)
	}
	if rawH.WeightsOffset != 0 && rawH.WeightsOffset%Alignment != 0 {
		t.Errorf("raw WeightsOffset %d not %d-byte aligned", rawH.WeightsOffset, Alignment)
	}
	if rawH.TailCRCOffset%8 != 0 {
		t.Errorf("raw TailCRCOffset %d not 8-byte aligned", rawH.TailCRCOffset)
	}
}
