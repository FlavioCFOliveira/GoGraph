package bulk

import (
	"fmt"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/internal/subproc"
	"gograph/store/recovery"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

func init() {
	// Register child handler for T900. The child bulk-loads a
	// deterministic 30-node directed path (0→1→…→29) into a csrfile,
	// constructs an LPG from the same edge list, writes a full snapshot,
	// and creates an empty WAL so recovery.Open finds a valid directory
	// layout. It then prints a golden summary line "order=<n> size=<m>"
	// to stdout before exiting 0.
	subproc.Register("bulk-load-proc", func(args []string) int {
		if len(args) < 1 {
			fmt.Println("bulk-load-proc: missing dir arg")
			return 1
		}
		dir := args[0]
		outPath := filepath.Join(dir, "graph.csr")

		const n = 30
		edges := make([]Edge, 0, n-1)
		for i := 0; i < n-1; i++ {
			edges = append(edges, Edge{
				Src:    fmt.Sprintf("%d", i),
				Dst:    fmt.Sprintf("%d", i+1),
				Weight: int64(i),
			})
		}

		// Phase 1: bulk load.
		l := New(Options{OutputPath: outPath, Directed: true})
		if err := l.AddBatch(edges); err != nil {
			fmt.Printf("bulk-load-proc: AddBatch: %v\n", err)
			return 1
		}
		_, _, err := l.Finalise()
		if err != nil {
			fmt.Printf("bulk-load-proc: Finalise: %v\n", err)
			return 1
		}

		// Phase 2: build an LPG from the same edges and write a full
		// snapshot so recovery.Open can load the graph without a WAL.
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		for _, e := range edges {
			if addErr := g.AddEdge(e.Src, e.Dst, e.Weight); addErr != nil {
				fmt.Printf("bulk-load-proc: AddEdge: %v\n", addErr)
				return 1
			}
		}
		snapCSR := csr.BuildFromAdjList(g.AdjList())
		snapDir := filepath.Join(dir, "snapshot")
		if err := snapshot.WriteSnapshotFull(snapDir, snapCSR, g); err != nil {
			fmt.Printf("bulk-load-proc: WriteSnapshotFull: %v\n", err)
			return 1
		}

		// Phase 3: open and immediately close an empty WAL at dir/wal so
		// recovery.OpenString finds a valid WAL file.
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			fmt.Printf("bulk-load-proc: wal.Open: %v\n", err)
			return 1
		}
		if err := w.Close(); err != nil {
			fmt.Printf("bulk-load-proc: wal.Close: %v\n", err)
			return 1
		}

		fmt.Printf("order=%d size=%d\n", g.AdjList().Order(), g.AdjList().Size())
		return 0
	})
}

// TestBulk_CrossProc_RecoverEqual spawns a child process (proc A) that
// bulk-loads a deterministic 30-node directed path, writes a snapshot
// and an empty WAL, then exits. The parent (proc B) opens the same
// directory via recovery.Open[string, int64] and asserts that Order
// and Size match the golden values printed by proc A.
//
// This validates the full bulk-to-recovery pipeline across an OS
// process boundary: snapshot written by a bulk-load child must be
// fully recoverable by an independent recovery reader.
func TestBulk_CrossProc_RecoverEqual(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	stdout, stderr, err := subproc.Run(t, "bulk-load-proc", dir)
	if err != nil {
		t.Fatalf("proc A failed: %v\nstderr: %s", err, stderr)
	}

	// Parse golden summary from proc A's stdout.
	var goldenOrder, goldenSize uint64
	if _, scanErr := fmt.Sscanf(string(stdout), "order=%d size=%d", &goldenOrder, &goldenSize); scanErr != nil {
		t.Fatalf("could not parse proc A stdout %q: %v", stdout, scanErr)
	}

	// Proc B: open via recovery.Open[string, int64].
	opts := recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	res, err := recovery.Open[string, int64](dir, opts)
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}

	adj := res.Graph.AdjList()
	if got := adj.Order(); got != goldenOrder {
		t.Errorf("Order = %d, want %d (from proc A)", got, goldenOrder)
	}
	if got := adj.Size(); got != goldenSize {
		t.Errorf("Size = %d, want %d (from proc A)", got, goldenSize)
	}
}
