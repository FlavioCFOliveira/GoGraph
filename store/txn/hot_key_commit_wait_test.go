package txn_test

import (
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCommit_HotKeyCommittersAreServedFairly pins rmp #2965 round 5, finding
// R5-F2. Eight store committers and two direct writers hammer ONE node. Each
// commit holds its claim across its fsync, so per-object serialisation is
// inherent; what must hold is that the waiters are served in turn. Before the
// fix they polled, the luckiest poller won, and committers exhausted the 1 s
// budget and were refused: the audit measured p99 = 1.000 s at 8 and 32
// committers. Now every committer commits, none is refused, and the slowest
// commit waits for roughly one fsync per committer ahead of it.
func TestCommit_HotKeyCommittersAreServedFairly(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	seed := st.Begin()
	_ = seed.AddNode("a")
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	const committers, directs = 8, 2
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var refused, directRefused atomic.Int64
	perCommitter := make([]int, committers)
	lat := make([][]time.Duration, committers)
	for i := 0; i < committers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := int64(0); ; v++ {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				tx := st.Begin()
				_ = tx.SetNodeProperty("a", "v", lpg.Int64Value(int64(i)<<32|v))
				err := tx.Commit()
				lat[i] = append(lat[i], time.Since(t0))
				if err != nil {
					refused.Add(1)
					continue
				}
				perCommitter[i]++
			}
		}()
	}
	for i := 0; i < directs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := int64(0); ; v++ {
				select {
				case <-stop:
					return
				default:
				}
				if g.SetNodeProperty("a", "d", lpg.Int64Value(v)) != nil {
					directRefused.Add(1)
				}
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	var all []time.Duration
	for i := range lat {
		all = append(all, lat[i]...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	p99 := all[(len(all)-1)*99/100]
	t.Logf("commits per committer %v; refused %d; direct refused %d; p50 %v p99 %v max %v",
		perCommitter, refused.Load(), directRefused.Load(), all[len(all)/2], p99, all[len(all)-1])
	if refused.Load() != 0 || directRefused.Load() != 0 {
		t.Errorf("refusals: %d commits, %d direct writes; want none", refused.Load(), directRefused.Load())
	}
	for i, n := range perCommitter {
		if n == 0 {
			t.Errorf("committer %d never committed in 1.5 s: starved", i)
		}
	}
	if p99 >= 500*time.Millisecond {
		t.Errorf("commit p99 = %v, want well below the 1 s wait budget", p99)
	}
}
