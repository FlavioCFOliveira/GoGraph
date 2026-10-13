package snapshot

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildPublishBenchLPG builds a string-keyed account graph shaped like
// examples/17_transactional_log: every node carries a label and two
// properties, every edge a weight. nodes and degree set its size.
func buildPublishBenchLPG(tb testing.TB, nodes, degree int) *lpg.Graph[string, int64] {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{})
	for i := 0; i < nodes; i++ {
		k := "acct-" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Account"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "owner", lpg.StringValue(k)); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeProperty(k, "balance", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < nodes; i++ {
		src := "acct-" + strconv.Itoa(i)
		for d := 1; d <= degree; d++ {
			dst := "acct-" + strconv.Itoa((i*7919+d*13)%nodes)
			if err := g.AddEdge(src, dst, int64(d)); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
		}
	}
	return g
}

// BenchmarkCaptureAndWriteSnapshot measures one checkpoint's snapshot work —
// [CaptureGraph] followed by [WriteCapture] — the path the checkpointer runs
// (rmp #3083). "accounts3k" matches examples/17_transactional_log
// (3000 accounts, 9000 transfers); "accounts50k" has components larger than
// the 1 MiB buffer cap.
func BenchmarkCaptureAndWriteSnapshot(b *testing.B) {
	for _, tc := range []struct {
		name          string
		nodes, degree int
	}{
		{"accounts3k", 3000, 3},
		{"accounts50k", 50_000, 10},
	} {
		b.Run(tc.name, func(b *testing.B) {
			g := buildPublishBenchLPG(b, tc.nodes, tc.degree)
			cs := csr.BuildFromAdjList(g.AdjList())
			dir := filepath.Join(b.TempDir(), "snap")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				capt, err := CaptureGraph[string, int64](g, cs, nil, nil)
				if err != nil {
					b.Fatalf("CaptureGraph: %v", err)
				}
				if err := WriteCapture(dir, capt, nil, nil); err != nil {
					b.Fatalf("WriteCapture: %v", err)
				}
			}
		})
	}
}
