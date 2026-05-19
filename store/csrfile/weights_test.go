package csrfile

import (
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestReader_WeightsRaw_Uint64 verifies that WeightsRaw exposes the
// raw byte view of the weight section for a uint64-weighted file.
func TestReader_WeightsRaw_Uint64(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t) // existing helper from reader_test.go: int64 weights
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	raw := r.WeightsRaw()
	if raw == nil {
		t.Fatal("WeightsRaw returned nil on a weighted file")
	}
	wantLen := int(r.Header().NEdges) * int(r.Header().Weight.Size())
	if len(raw) != wantLen {
		t.Fatalf("WeightsRaw len = %d, want %d", len(raw), wantLen)
	}
	// The float64 view must report not-applicable for uint64 weights.
	if _, ok := r.WeightsFloat64(); ok {
		t.Fatal("WeightsFloat64 must return ok=false for uint64 weights")
	}
}

// TestReader_WeightsRaw_Unweighted verifies that WeightsRaw returns
// nil when the file carries no weights.
func TestReader_WeightsRaw_Unweighted(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", struct{}{})
	a.AddEdge("b", "c", struct{}{})
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "unweighted.csr")
	if _, err := WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if got := r.WeightsRaw(); got != nil {
		t.Fatalf("unweighted WeightsRaw = %v, want nil", got)
	}
	if _, ok := r.WeightsUint64(); ok {
		t.Fatal("WeightsUint64 must return ok=false for unweighted files")
	}
	if _, ok := r.WeightsFloat64(); ok {
		t.Fatal("WeightsFloat64 must return ok=false for unweighted files")
	}
}

// TestReader_WeightsFloat64 builds a float64-weighted file and
// confirms WeightsFloat64 returns a typed slice with the original
// values intact.
func TestReader_WeightsFloat64(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, float64](adjlist.Config{Directed: true})
	a.AddEdge("a", "b", 1.5)
	a.AddEdge("a", "c", 2.5)
	a.AddEdge("b", "c", 3.5)
	c := csr.BuildFromAdjList(a)
	path := filepath.Join(t.TempDir(), "float.csr")
	if _, err := WriteToFile(path, c); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if r.Header().Weight != WeightFloat64 {
		t.Fatalf("Weight = %d, want WeightFloat64", r.Header().Weight)
	}
	got, ok := r.WeightsFloat64()
	if !ok {
		t.Fatal("WeightsFloat64 ok=false on float64-weighted file")
	}
	if len(got) != int(r.Header().NEdges) {
		t.Fatalf("WeightsFloat64 len = %d, want %d", len(got), r.Header().NEdges)
	}
	// The values must match the input multiset (order matches the
	// source-sorted edges layout: a's edges first, then b's).
	want := map[float64]int{1.5: 0, 2.5: 0, 3.5: 0}
	for _, v := range got {
		if _, known := want[v]; !known {
			t.Fatalf("unexpected weight %g in WeightsFloat64", v)
		}
		want[v]++
	}
	for v, n := range want {
		if n != 1 {
			t.Fatalf("weight %g appeared %d times, want 1", v, n)
		}
	}

	// WeightsUint64 must reject a float file.
	if _, ok := r.WeightsUint64(); ok {
		t.Fatal("WeightsUint64 must return ok=false for float64 weights")
	}
}
