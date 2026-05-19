package csrfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func writeFixture(t *testing.T) (string, *csr.CSR[int64]) {
	t.Helper()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", 10)
	a.AddEdge("a", "c", 20)
	a.AddEdge("b", "c", 30)
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "fix.csr")
	if _, err := WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	return path, c
}

func TestReader_OpenAndExpose(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	if len(r.Vertices()) == 0 || len(r.Edges()) == 0 {
		t.Fatalf("empty slices: vertices=%d edges=%d", len(r.Vertices()), len(r.Edges()))
	}
	if r.Header().Weight != WeightUint64 {
		t.Fatalf("Weight = %d, want WeightUint64", r.Header().Weight)
	}
	w, ok := r.WeightsUint64()
	if !ok || len(w) != int(r.Header().NEdges) {
		t.Fatalf("WeightsUint64 ok=%v len=%d", ok, len(w))
	}
}

func TestReader_SetHint(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, p := range []AccessPattern{AccessSequential, AccessRandom, AccessWillNeed, AccessDefault} {
		if err := r.SetHint(p); err != nil {
			t.Fatalf("SetHint %v: %v", p, err)
		}
	}
}

func TestReader_CorruptedTailCRC(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	data, err := os.ReadFile(path) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrFileCorrupted) {
		t.Fatalf("expected ErrFileCorrupted, got %v", err)
	}
}

func TestReader_AfterClose(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close.
	if err := r.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}
