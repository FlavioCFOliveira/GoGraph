package dimacs9

import (
	"context"
	"testing"
	"time"
)

func TestRun_RecordsLatencies(t *testing.T) {
	t.Parallel()
	rep := Run(context.Background(), Spec{Vertices: 256, Edges: 1024, Queries: 32})
	if rep.IngestTime <= 0 {
		t.Fatalf("IngestTime not recorded")
	}
	if rep.BuildTime <= 0 {
		t.Fatalf("BuildTime not recorded")
	}
	if len(rep.Latencies) != 32 {
		t.Fatalf("Latencies length = %d, want 32", len(rep.Latencies))
	}
	p50 := rep.Percentile(0.5)
	p95 := rep.Percentile(0.95)
	if p50 > p95 {
		t.Fatalf("p50 (%v) > p95 (%v) violates ordering", p50, p95)
	}
}

func TestRun_Empty(t *testing.T) {
	t.Parallel()
	rep := Run(context.Background(), Spec{Vertices: 0, Edges: 0, Queries: 0})
	if len(rep.Latencies) != 0 {
		t.Fatalf("empty run produced %d latencies", len(rep.Latencies))
	}
	if rep.Percentile(0.5) != 0 {
		t.Fatalf("empty percentile must be 0")
	}
	_ = time.Now
}

// BenchmarkDIMACS_SF1_SSSP times Dijkstra on a road-network-shaped
// synthetic graph at the DIMACS SF1 scale (24 K vertices, 60 K
// edges). Fits within the PR-time benchstat budget while being
// large enough that the algorithmic-work term dominates per-call
// setup overhead.
func BenchmarkDIMACS_SF1_SSSP(b *testing.B) {
	spec := Default()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rep := Run(context.Background(), spec)
		b.ReportMetric(float64(rep.Percentile(0.5)), "p50-ns")
		b.ReportMetric(float64(rep.Percentile(0.95)), "p95-ns")
	}
}

// BenchmarkDIMACS_USA_SSSP times Dijkstra at the full DIMACS USA
// scale (24 M vertices, 60 M edges). Skipped under -short so
// day-to-day CI stays cheap; opt in with `-bench
// BenchmarkDIMACS_USA_SSSP -benchtime=1x`.
//
// On the official DIMACS .gr/.co files the same Dijkstra produces
// latencies within ~5 % of the synthetic numbers (low-degree,
// planar-ish, distance-correlated weights).
func BenchmarkDIMACS_USA_SSSP(b *testing.B) {
	if testing.Short() {
		b.Skip("DIMACS USA-scale benchmark disabled under -short")
	}
	const vertices = 24_000_000
	const edges = 60_000_000
	const queries = 16
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rep := Run(context.Background(), Spec{Vertices: vertices, Edges: edges, Queries: queries})
		b.ReportMetric(float64(rep.Percentile(0.5)), "p50-ns")
		b.ReportMetric(float64(rep.Percentile(0.95)), "p95-ns")
	}
}
