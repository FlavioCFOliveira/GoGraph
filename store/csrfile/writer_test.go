package csrfile

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestWriteToFile_AtomicProducesValidFile(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < 64; i++ {
		if err := a.AddEdge("hub", string(rune('a'+i%26)), int64(i)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "test.csr")
	h, err := WriteToFile(path, c)
	if err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	if h.NVertices != uint64(len(c.VerticesSlice())) {
		t.Fatalf("nVertices = %d, want %d", h.NVertices, len(c.VerticesSlice()))
	}
	// File should exist; .tmp should not.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat path: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp must be gone, got %v", err)
	}
	// Verify header parses back and CRC matches.
	data, err := os.ReadFile(path) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if uint64(len(data)) != h.TailCRCOffset+4 {
		t.Fatalf("file size = %d, want %d", len(data), h.TailCRCOffset+4)
	}
	parsed, err := DecodeHeader(data)
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if parsed != h {
		t.Fatalf("header roundtrip mismatch")
	}
	body := data[:h.TailCRCOffset]
	gotCRC := binary.LittleEndian.Uint32(data[h.TailCRCOffset:])
	wantCRC := crc32.Update(0, castagnoli, body)
	if gotCRC != wantCRC {
		t.Fatalf("tail CRC = %x, want %x", gotCRC, wantCRC)
	}
}

func TestWriteToFile_StructWeightDowngrades(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "test.csr")
	h, err := WriteToFile(path, c)
	if err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	if h.Weight != WeightAbsent {
		t.Fatalf("Weight = %d, want WeightAbsent", h.Weight)
	}
	if h.WeightsOffset != 0 {
		t.Fatalf("WeightsOffset should be 0 for absent weights")
	}
}

func TestWriteToFile_UnsupportedWeightKind(t *testing.T) {
	t.Parallel()
	type CustomWeight struct {
		X complex128
		Y complex128
	}
	a := adjlist.New[string, CustomWeight](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", CustomWeight{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "test.csr")
	_, err := WriteToFile(path, c)
	if !errors.Is(err, ErrUnknownWeightKind) {
		t.Fatalf("expected ErrUnknownWeightKind, got %v", err)
	}
}
