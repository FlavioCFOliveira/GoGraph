package txn_test

// differential_recovery_test.go — memory must equal recovery after a randomised
// concurrent workload of store commits (ACID audit round 6).
//
// Concurrent workers commit random transactions over a small key domain, so
// that they collide on the same nodes, edges, labels, properties and handles,
// and roll some back. When every worker has finished, the in-memory graph that
// acknowledged those commits is rendered and compared with the graph recovery
// rebuilds from the WAL. Any difference is an acknowledged effect the log did
// not carry, or a logged effect memory did not hold.

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// diffKeys is the size of the node key domain: small, so workers collide.
const diffKeys = 6

func diffKey(i int) string { return fmt.Sprintf("n%d", i) }

func diffProps(m map[string]lpg.PropertyValue) string {
	ks := make([]string, 0, len(m))
	for k, v := range m {
		ks = append(ks, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// diffDump renders the observable state of every key in the domain.
func diffDump(g *lpg.Graph[string, float64]) string {
	var b strings.Builder
	adj := g.AdjList()
	for i := 0; i < diffKeys; i++ {
		k := diffKey(i)
		id, ok := adj.Mapper().Lookup(k)
		alive := ok && !g.IsTombstoned(id)
		fmt.Fprintf(&b, "%s alive=%v", k, alive)
		if !alive {
			b.WriteString("\n")
			continue
		}
		lb := g.NodeLabels(k)
		sort.Strings(lb)
		fmt.Fprintf(&b, " labels=%v props={%s}\n", lb, diffProps(g.NodeProperties(k)))
		nbrs, ws, hs := adj.LoadEntryH(id)
		es := make([]string, 0, len(nbrs))
		for j, d := range nbrs {
			dk, _ := adj.Mapper().Resolve(d)
			dead := ""
			if did, ok := adj.Mapper().Lookup(dk); !ok || g.IsTombstoned(did) {
				dead = "(DEAD)"
			}
			var h uint64
			if j < len(hs) {
				h = hs[j]
			}
			e := fmt.Sprintf("  -> %s%s w=%v", dk, dead, ws[j])
			if h != 0 {
				lbs := g.EdgeLabelsByHandle(k, dk, h)
				sort.Strings(lbs)
				e += fmt.Sprintf(" h=%d hl=%v hp={%s}", h, lbs, diffProps(g.EdgePropertiesByHandle(k, dk, h)))
			}
			es = append(es, e)
		}
		sort.Strings(es)
		for _, e := range es {
			b.WriteString(e + "\n")
		}
		seen := map[string]bool{}
		var pl []string
		for _, d := range nbrs {
			dk, _ := adj.Mapper().Resolve(d)
			if seen[dk] {
				continue
			}
			seen[dk] = true
			el := g.EdgeLabels(k, dk)
			sort.Strings(el)
			pl = append(pl, fmt.Sprintf("  pair %s->%s labels=%v props={%s}\n", k, dk, el, diffProps(g.EdgeProperties(k, dk))))
		}
		sort.Strings(pl)
		for _, x := range pl {
			b.WriteString(x)
		}
	}
	fmt.Fprintf(&b, "hasIndexes=%v hasConstraints=%v constraints=%d\n", g.HasIndexes(), g.HasConstraints(), len(g.StoreConstraints()))
	return b.String()
}

// diffRandomTx buffers one to six random ops on tx. used is the worker's pool
// of recently minted handles, which the by-handle ops address.
func diffRandomTx(r *rand.Rand, tx *txn.Tx[string, float64], handles *atomic.Uint64, used []uint64) []uint64 {
	n := 1 + r.Intn(6)
	key := func() string { return diffKey(r.Intn(diffKeys)) }
	for i := 0; i < n; i++ {
		a, b := key(), key()
		x, y := r.Intn(2), r.Intn(2)
		switch r.Intn(22) {
		case 0, 1:
			_ = tx.AddNode(a)
		case 2:
			_ = tx.RemoveNode(a)
		case 3, 4:
			_ = tx.SetNodeLabel(a, fmt.Sprintf("L%d", x))
		case 5:
			_ = tx.RemoveNodeLabel(a, fmt.Sprintf("L%d", x))
		case 6, 7:
			_ = tx.SetNodeProperty(a, fmt.Sprintf("p%d", x), lpg.Int64Value(int64(y)))
		case 8:
			_ = tx.DelNodeProperty(a, fmt.Sprintf("p%d", x))
		case 9, 10:
			_ = tx.AddEdge(a, b, float64(x))
		case 11:
			_ = tx.RemoveEdge(a, b)
		case 12:
			_ = tx.SetEdgeProperty(a, b, "e", lpg.Int64Value(int64(x)))
		case 13:
			_ = tx.DelEdgeProperty(a, b, "e")
		case 14:
			_ = tx.SetEdgeLabel(a, b, fmt.Sprintf("R%d", x))
		case 15, 16:
			h := handles.Add(1)
			used = append(used, h)
			_ = tx.AddEdgeWithHandle(a, b, 1, h)
		case 17, 18, 19:
			if len(used) == 0 {
				continue
			}
			h := used[r.Intn(len(used))]
			switch r.Intn(5) {
			case 0:
				_ = tx.SetEdgeLabelByHandle(a, b, h, fmt.Sprintf("R%d", x))
			case 1:
				_ = tx.SetEdgePropertyByHandle(a, b, h, "q", lpg.Int64Value(int64(x)))
			case 2:
				_ = tx.DelEdgePropertyByHandle(a, b, h, "q")
			case 3:
				_ = tx.RemoveEdgeInstanceByHandle(a, b, h)
			case 4:
				_ = tx.RemoveEdgeByHandle(a, b, h)
			}
		case 20:
			if r.Intn(4) == 0 {
				_ = tx.CreateIndex(txn.IndexKindHash, "L0", "p0", fmt.Sprintf("ix%d", x))
			} else {
				_ = tx.DropIndex(fmt.Sprintf("ix%d", x))
			}
		case 21:
			if r.Intn(2) == 0 {
				_ = tx.CreateConstraint(txn.ConstraintUnique, "L0", "p1", "c0")
			} else {
				_ = tx.DropConstraint(txn.ConstraintUnique, "L0", "p1", "c0")
			}
		}
	}
	return used
}

// diffRun runs one seed and reports whether memory and recovery diverged.
func diffRun(t *testing.T, seed int64, workers, perWorker int) bool {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// Directed multigraph: what a WAL-only recovery rebuilds.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
	st := txn.NewStoreWithOptions[string, float64](g, w, opts)
	var ok, conflict atomic.Int64
	var otherMu sync.Mutex
	other := map[string]int{}
	var handles atomic.Uint64
	var wg sync.WaitGroup
	for wk := 0; wk < workers; wk++ {
		wg.Add(1)
		go func(wk int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed*1000 + int64(wk))) //nolint:gosec // a reproducible workload, not a secret
			var used []uint64
			for i := 0; i < perWorker; i++ {
				tx := st.Begin()
				used = diffRandomTx(r, tx, &handles, used)
				if r.Intn(10) == 0 {
					_ = tx.Rollback()
					continue
				}
				switch err := tx.Commit(); {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, mvcc.ErrSerializationConflict) || errors.Is(err, lpg.ErrDirectWriteConflict):
					conflict.Add(1)
				default:
					otherMu.Lock()
					other[err.Error()]++
					otherMu.Unlock()
				}
				if len(used) > 64 {
					used = used[len(used)-64:]
				}
			}
		}(wk)
	}
	wg.Wait()
	mem := diffDump(g)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	recd := diffDump(res.Graph)
	if ok.Load() == 0 {
		t.Fatalf("seed %d: no commit succeeded (conflicts=%d, other=%v)", seed, conflict.Load(), other)
	}
	if mem == recd {
		return false
	}
	t.Errorf("seed %d (ok=%d conflict=%d other=%v): MEMORY != RECOVERY\n--- memory ---\n%s--- recovered ---\n%s",
		seed, ok.Load(), conflict.Load(), other, mem, recd)
	return true
}

func diffSweep(t *testing.T, seeds, workers, perWorker int) {
	diverged := 0
	for s := 1; s <= seeds; s++ {
		if diffRun(t, int64(s), workers, perWorker) {
			diverged++
		}
	}
	t.Logf("%d of %d seeds diverged (%d workers x %d transactions)", diverged, seeds, workers, perWorker)
}

// TestDifferential_MemoryEqualsRecovery is the short-layer sweep: a bounded
// number of seeds of the concurrent workload.
func TestDifferential_MemoryEqualsRecovery(t *testing.T) {
	diffSweep(t, 4, 8, 80)
}

// TestDifferential_SequentialMemoryEqualsRecovery runs the same workload from one
// goroutine, where any divergence is an effect-detection defect rather than a
// concurrency one.
func TestDifferential_SequentialMemoryEqualsRecovery(t *testing.T) {
	diffSweep(t, 2, 1, 300)
}

// TestDifferential_MemoryEqualsRecoverySoak is the long sweep: forty seeds at
// twelve workers. Soak layer.
func TestDifferential_MemoryEqualsRecoverySoak(t *testing.T) {
	testlayers.RequireSoak(t)
	diffSweep(t, 40, 12, 150)
}
