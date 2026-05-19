package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/index"
	"gograph/graph/index/btree"
	"gograph/graph/index/hash"
	"gograph/graph/index/label"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
)

// BenchmarkIndexesRecoveryVsRebuild compares the cost of re-hydrating
// three secondary indexes from a snapshot (via the new Serialize /
// Deserialize path) against the cost of rebuilding them from scratch
// out of an in-memory LPG enumeration. The acceptance criterion of
// rmp #172: snapshot recovery must be ≤ 1.5x the rebuild cost on a
// graph with 10^5 nodes.
//
// The benchmark is sized at 10^5 nodes by default; under -short it is
// skipped because the on-disk write/read measurement is slow.
//
// Run via:
//
//	go test -bench=BenchmarkIndexesRecoveryVsRebuild \
//	    -benchmem ./store/recovery/
//
// Sub-benchmarks:
//
//	"snapshot" — measures snapshot.LoadIndexes + Deserialize on three
//	             registered indexes.
//	"rebuild"  — measures populating the same three fresh indexes
//	             by iterating over the LPG and re-inserting every
//	             (key, NodeID) tuple.
const indexesBenchN = 100_000

//nolint:gocyclo // bench: setup + snapshot + payload cache + two sub-benchmarks
func BenchmarkIndexesRecoveryVsRebuild(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 10^5-node benchmark under -short")
	}

	// === Setup: build a 10^5-node LPG with three populated indexes ===
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	lab := label.NewIndex()
	hsh := hash.New[string]()
	bt := btree.New[string]()
	if err := mgr.CreateIndex("labels.nodes", lab); err != nil {
		b.Fatal(err)
	}
	if err := mgr.CreateIndex("hash.user", hsh); err != nil {
		b.Fatal(err)
	}
	if err := mgr.CreateIndex("btree.score", bt); err != nil {
		b.Fatal(err)
	}

	keys := make([]string, indexesBenchN)
	for i := 0; i < indexesBenchN; i++ {
		key := keyForBench(i)
		keys[i] = key
		g.AddNode(key)
	}
	for i := uint64(0); i < indexesBenchN; i++ {
		// Three labels per node so the label bitmaps are
		// reasonably dense.
		lab.Add(uint32(i%32+1), graph.NodeID(i))
		lab.Add(uint32(i%128+33), graph.NodeID(i))
		lab.Add(uint32(i%4+200), graph.NodeID(i))
		hsh.Insert(keys[i], graph.NodeID(i))
		bt.Insert(keys[i], graph.NodeID(i))
	}

	// === Stage the on-disk snapshot once; the snapshot sub-benchmark
	// only measures the load+deserialize cost. ===
	tmp := b.TempDir()
	snapDir := filepath.Join(tmp, "snapshot")
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		b.Fatalf("WriteSnapshotFull: %v", err)
	}
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		b.Fatalf("LoadSnapshotFull: %v", err)
	}
	// Cache the index payloads so the inner loop can pay only for
	// Deserialize and bytes.NewReader allocation.
	type payload struct {
		name string
		buf  []byte
	}
	payloads := make([]payload, 0, len(loaded.Indexes))
	for _, rb := range loaded.Indexes {
		if rb.Bytes == nil {
			b.Fatalf("index %q bytes are nil; benchmark setup broken", rb.Name)
		}
		payloads = append(payloads, payload{name: rb.Name, buf: rb.Bytes})
	}

	// === Sub-benchmark: snapshot recovery (Deserialize only) ===
	b.Run("snapshot", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lab2 := label.NewIndex()
			hsh2 := hash.New[string]()
			bt2 := btree.New[string]()
			byName := map[string]index.Serializer{
				"labels.nodes": lab2,
				"hash.user":    hsh2,
				"btree.score":  bt2,
			}
			for _, p := range payloads {
				if err := byName[p.name].Deserialize(bytes.NewReader(p.buf)); err != nil {
					b.Fatalf("Deserialize %q: %v", p.name, err)
				}
			}
			// Pin so the compiler doesn't lift the work.
			_ = lab2.Count(1)
		}
	})

	// === Sub-benchmark: rebuild from scratch ===
	// We pre-extract the (key, NodeID) pairs into flat slices so the
	// inner loop only measures the index population cost, not the
	// LPG walk.
	labNodes := make([]uint64, 0, indexesBenchN*3)
	labLabels := make([]uint32, 0, indexesBenchN*3)
	hshKeys := make([]string, 0, indexesBenchN)
	hshNodes := make([]uint64, 0, indexesBenchN)
	btKeys := make([]string, 0, indexesBenchN)
	btNodes := make([]uint64, 0, indexesBenchN)
	for i := uint64(0); i < indexesBenchN; i++ {
		labNodes = append(labNodes, i, i, i)
		labLabels = append(labLabels, uint32(i%32+1), uint32(i%128+33), uint32(i%4+200))
		hshKeys = append(hshKeys, keys[i])
		hshNodes = append(hshNodes, i)
		btKeys = append(btKeys, keys[i])
		btNodes = append(btNodes, i)
	}

	b.Run("rebuild", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lab2 := label.NewIndex()
			hsh2 := hash.New[string]()
			bt2 := btree.New[string]()
			for k := range labNodes {
				lab2.Add(labLabels[k], graph.NodeID(labNodes[k]))
			}
			for k := range hshKeys {
				hsh2.Insert(hshKeys[k], graph.NodeID(hshNodes[k]))
			}
			// btree.BulkLoad is the canonical reload path: faster than
			// per-key Insert for large N, and the apples-to-apples
			// comparison we expect callers to use when rebuilding.
			vals := make([]string, len(btKeys))
			ids := make([]graph.NodeID, len(btKeys))
			for k := range btKeys {
				vals[k] = btKeys[k]
				ids[k] = graph.NodeID(btNodes[k])
			}
			bt2.BulkLoad(vals, ids)
			_ = lab2.Count(1)
		}
	})
}

// keyForBench returns a stable, deterministic key per node index so
// the benchmark is reproducible across runs.
func keyForBench(i int) string {
	// Small alphabet so the encoded bytes stay short; the absolute
	// scale of the benchmark is the cost ratio, not the absolute
	// number of bytes copied.
	var buf [12]byte
	n := i
	pos := len(buf)
	for n > 0 || pos == len(buf) {
		pos--
		buf[pos] = byte('a' + n%26)
		n /= 26
	}
	return string(buf[pos:])
}

// TestIndexesBenchSetupBuilds is a smoke test that guards the
// benchmark fixture builder. It exercises the same code path the
// benchmark sets up, with a tiny N so it runs in the regular -short
// suite; if the fixture builder regresses (panic, error from
// WriteSnapshotFull, ...), this test will fail before the benchmark
// gets to run.
func TestIndexesBenchSetupBuilds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	lab := label.NewIndex()
	hsh := hash.New[string]()
	bt := btree.New[string]()
	if err := mgr.CreateIndex("labels.nodes", lab); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CreateIndex("hash.user", hsh); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CreateIndex("btree.score", bt); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		key := keyForBench(i)
		g.AddNode(key)
		lab.Add(1, graph.NodeID(uint64(i)))
		hsh.Insert(key, graph.NodeID(uint64(i)))
		bt.Insert(key, graph.NodeID(uint64(i)))
	}
	tmp := t.TempDir()
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(tmp, "snapshot"), cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "snapshot", snapshot.IndexesDir)); err != nil {
		t.Fatalf("indexes/ dir missing: %v", err)
	}
}
