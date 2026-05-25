//go:build soak

package extern

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"gograph/bench/ldbc"
	"gograph/internal/testlayers"
	"gograph/store/bulk"
	"gograph/store/csrfile"
)

// TestPageRank_LDBCSf10_Soak verifies that extern.PageRank converges
// on a synthetic LDBC SF10-scale graph (~500k vertices, ~5M edges):
// no error, ranks slice length matches NVertices, and total mass
// sums to 1.0 within 1e-6.
func TestPageRank_LDBCSf10_Soak(t *testing.T) {
	testlayers.RequireSoak(t)

	path := filepath.Join(t.TempDir(), "ldbc_sf10_pr.csr")
	loader := bulk.New(bulk.Options{OutputPath: path, Directed: true})
	ldbc.Synthetic(context.Background(), 500_000, 5_000_000, loader)
	if _, _, err := loader.Finalise(); err != nil {
		t.Fatalf("loader.Finalise: %v", err)
	}

	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	ranks, _, err := PageRank(r, DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}

	nv := r.Header().NVertices
	if uint64(len(ranks)) != nv {
		t.Fatalf("len(ranks) = %d, want %d (NVertices)", len(ranks), nv)
	}

	var total float64
	for _, v := range ranks {
		total += v
	}
	if math.Abs(total-1.0) > 1e-6 {
		t.Fatalf("rank sum = %.9f, want 1.0 (±1e-6)", total)
	}
}
