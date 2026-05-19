package ldbc

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRun_SF1Synthetic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	spec := Spec{
		Scale:       ScaleSF1,
		Queries:     16,
		Synthetic:   true,
		OutDir:      dir,
		BulkOutFile: filepath.Join(dir, "snb_sf1.csr"),
	}
	rep, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.IngestTime <= 0 {
		t.Fatalf("IngestTime not recorded")
	}
	if len(SortStats(rep)) == 0 {
		t.Fatalf("no query stats recorded")
	}
}

func TestRun_RealisticNotImplemented(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Spec{Synthetic: false})
	if err == nil {
		t.Fatalf("expected error for non-synthetic mode")
	}
}

// BenchmarkLDBC_SF1_Mixed times the LDBC SNB SF1 synthetic harness
// (mix of BFS / Dijkstra / PageRank queries). Fits within the PR-
// time benchstat budget.
func BenchmarkLDBC_SF1_Mixed(b *testing.B) {
	dir := b.TempDir()
	spec := Spec{
		Scale:       ScaleSF1,
		Queries:     16,
		Synthetic:   true,
		OutDir:      dir,
		BulkOutFile: filepath.Join(dir, "snb_sf1.csr"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := Run(context.Background(), spec)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
	}
}

// BenchmarkLDBC_SF10_Mixed times the harness at SF10 scale (~3 M
// vertices, ~170 M edges). Skipped under -short; opt in with
// `-bench BenchmarkLDBC_SF10_Mixed -benchtime=1x`.
func BenchmarkLDBC_SF10_Mixed(b *testing.B) {
	if testing.Short() {
		b.Skip("LDBC SF10-scale benchmark disabled under -short")
	}
	dir := b.TempDir()
	spec := Spec{
		Scale:       ScaleSF10,
		Queries:     16,
		Synthetic:   true,
		OutDir:      dir,
		BulkOutFile: filepath.Join(dir, "snb_sf10.csr"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := Run(context.Background(), spec)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
	}
}
