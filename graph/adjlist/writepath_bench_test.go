package adjlist_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkWritePath_BulkBuild measures the write-path cost of building a hub
// of out-degree d. Each AddEdge republishes the shard's slot array under the
// F3.2 copy-on-write rule, so this gates the write-path non-regression /
// mitigation decision (task #1526).
func BenchmarkWritePath_BulkBuild(b *testing.B) {
	for _, d := range []int{16, 256, 4096} {
		b.Run(benchName(d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a := adjlist.New[int, int64](adjlist.Config{Directed: true})
				for k := 1; k <= d; k++ {
					_ = a.AddEdge(0, k, int64(k))
				}
			}
		})
	}
}

// BenchmarkWritePath_ManyShards measures cost when each edge lands in a
// different source shard (spread sources), so the per-op shard-slice clone is
// over small, growing shards rather than one large hub shard.
func BenchmarkWritePath_ManyShards(b *testing.B) {
	const edges = 4096
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := adjlist.New[int, int64](adjlist.Config{Directed: true})
		for k := 0; k < edges; k++ {
			_ = a.AddEdge(k, k+1, 1)
		}
	}
}

// BenchmarkWritePath_BulkBuildWindowed is the bracketed counterpart of
// BenchmarkWritePath_BulkBuild: the whole build runs inside one commit window
// (BeginCommit/EndCommit), the sanctioned exclusive-build mode. The hub's shard
// is cloned once on first touch and mutated in place thereafter, so cost should
// return to ~baseline (the per-op clone regression is eliminated). This models
// the production Cypher commit path (lpg.ApplyAtomically brackets the window)
// and recovery replay / bulk ingest (one window over the loop).
func BenchmarkWritePath_BulkBuildWindowed(b *testing.B) {
	for _, d := range []int{16, 256, 4096} {
		b.Run(benchName(d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a := adjlist.New[int, int64](adjlist.Config{Directed: true})
				a.BeginCommit()
				for k := 1; k <= d; k++ {
					_ = a.AddEdge(0, k, int64(k))
				}
				a.EndCommit()
			}
		})
	}
}

// BenchmarkWritePath_ManyShardsWindowed brackets the spread-source build in one
// window. Each shard is cloned once on first touch; later same-shard writes
// mutate the builder in place.
func BenchmarkWritePath_ManyShardsWindowed(b *testing.B) {
	const edges = 4096
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := adjlist.New[int, int64](adjlist.Config{Directed: true})
		a.BeginCommit()
		for k := 0; k < edges; k++ {
			_ = a.AddEdge(k, k+1, 1)
		}
		a.EndCommit()
	}
}

func benchName(d int) string {
	switch d {
	case 16:
		return "deg16"
	case 256:
		return "deg256"
	case 4096:
		return "deg4096"
	default:
		return "deg?"
	}
}
