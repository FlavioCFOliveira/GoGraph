//go:build soak || nightly

package extern

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/ldbc"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestPageRank_LDBCSf10_Soak verifies that extern.PageRank converges
// on a synthetic LDBC SF10-scale graph (~500k vertices, ~5M edges):
// no error, the ranks slice covers exactly the NodeID space, and total
// mass sums to 1.0 within 1e-6.
//
// # The NodeID space is NVertices-1, not NVertices (rmp #2256)
//
// This assertion was wrong from the day it was written (Sprint 65) and the
// test was therefore permanently red — undetected because the soak layer is
// deliberately not a release gate.
//
// [csrfile.Header.NVertices] is the number of uint64 entries in the file's
// vertices section, which under the CSR convention is MaxNodeID+1. It is NOT a
// count of distinct vertices: measured on a 4-vertex, 4-edge graph it reports
// 257, because the mapper spreads keys across 256 shards. extern.PageRank is
// correct — it computes n = len(verts)-1 and returns a slice indexed by NodeID
// — so len(ranks) == NVertices-1 by construction and comparing against
// NVertices could never pass.
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
	if nv == 0 {
		t.Fatalf("Header().NVertices = 0; the fixture wrote no vertices section")
	}
	// The NodeID space the ranks slice is indexed by.
	wantRanks := nv - 1
	if uint64(len(ranks)) != wantRanks {
		t.Fatalf("len(ranks) = %d, want %d (NVertices-1, the NodeID space)", len(ranks), wantRanks)
	}

	var total float64
	for _, v := range ranks {
		total += v
	}
	if math.Abs(total-1.0) > 1e-6 {
		t.Fatalf("rank sum = %.9f, want 1.0 (±1e-6)", total)
	}
}
