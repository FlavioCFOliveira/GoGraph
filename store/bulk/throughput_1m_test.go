//go:build soak || nightly

package bulk

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// TestLoader_Throughput_1M_Edges verifies that the bulk loader can
// ingest 1 000 000 edges from a 100 000-node graph without error, that
// the resulting CSR is non-empty, and that the written csrfile has a
// non-zero edge count.
func TestLoader_Throughput_1M_Edges(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	const (
		nEdges = 1_000_000
		nNodes = 100_000
	)

	out := filepath.Join(t.TempDir(), "graph.csr")
	l := New(Options{OutputPath: out, Directed: true})

	rng := rand.New(rand.NewPCG(42, 7)) //nolint:gosec // deterministic test RNG
	for i := 0; i < nEdges; i++ {
		src := fmt.Sprintf("node-%d", rng.IntN(nNodes))
		dst := fmt.Sprintf("node-%d", rng.IntN(nNodes))
		if err := l.Add(Edge{Src: src, Dst: dst, Weight: int64(i)}); err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
	}

	rows, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if rows != nEdges {
		t.Fatalf("rows = %d, want %d", rows, nEdges)
	}
	if c.Order() > nNodes {
		t.Fatalf("CSR Order = %d, want <= %d", c.Order(), nNodes)
	}
	if c.Size() == 0 {
		t.Fatalf("CSR Size = 0, want > 0")
	}

	r, err := csrfile.Open(out)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	if r.Header().NEdges == 0 {
		t.Fatalf("csrfile NEdges = 0, want > 0")
	}
}
