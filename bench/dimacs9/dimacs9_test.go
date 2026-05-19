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
