package txn_test

// idreserve_test.go — WAL v2 step 4 (rmp #3021 B, docs/design-wal-v2.md §5): node
// id reservations and high-water marks. No id is reissued after a restart if a
// durable record or a checkpoint could name it; a clean restart wastes no id and
// a crash restart at most one batch per shard.
//
// A "crash" is the directory copied while the store is open: every byte a write
// reached the file with is in the copy and nothing still buffered in the process
// is, which is what kill -9 leaves (a process kill loses no page cache).
//
// Layer: short.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

func idOpts() store.Options[string, float64] {
	return store.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func idOpen(t testing.TB, dir string, opts store.Options[string, float64]) *store.Opened[string, float64] {
	t.Helper()
	o, err := store.Open[string, float64](dir, opts)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return o
}

// idCommit commits one transaction creating keys through Tx.Commit.
func idCommit(t testing.TB, st *txn.Store[string, float64], keys ...string) {
	t.Helper()
	tx := st.Begin()
	for _, k := range keys {
		if err := tx.AddNode(k); err != nil {
			_ = tx.Rollback()
			t.Fatalf("AddNode(%q): %v", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// idEager applies key eagerly through lpg, as the Cypher engine does, then
// commits it WAL-only (commit true) or rolls it back.
func idEager(t testing.TB, o *store.Opened[string, float64], key string, commit bool) {
	t.Helper()
	g := o.Graph()
	wtx := g.BeginVersionedTx()
	tx := o.Store().Begin()
	if err := g.Writer(wtx).AddNode(key); err != nil {
		t.Fatalf("lpg AddNode(%q): %v", key, err)
	}
	if err := tx.AddNode(key); err != nil {
		t.Fatalf("txn AddNode(%q): %v", key, err)
	}
	if !commit {
		wtx.Abandon()
		g.EndVersionedTx(wtx)
		_ = tx.Rollback()
		return
	}
	tx.AttachWriteTx(wtx)
	if err := tx.CommitWALOnly(g.AllocateCommitTS(wtx)); err != nil {
		t.Fatalf("CommitWALOnly: %v", err)
	}
	g.EndVersionedTx(wtx)
}

func idOf(t testing.TB, g *lpg.Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(k)
	if !ok {
		t.Fatalf("key %q is not interned", k)
	}
	return id
}

// idCheckpoint runs one checkpoint of o.
func idCheckpoint(t testing.TB, o *store.Opened[string, float64], dir string) {
	t.Helper()
	var mu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// crashCopy copies dir to a fresh directory while its store is open: the crash
// image. It skips the writer's lock file.
func crashCopy(t testing.TB, dir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "crash")
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		if strings.HasSuffix(p, ".lock") {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: test paths under t.TempDir
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // G703: test paths under t.TempDir
	})
	if err != nil {
		t.Fatalf("crash copy: %v", err)
	}
	return dst
}

// marks returns the mapper's per-shard high-water marks.
func marks(g *lpg.Graph[string, float64]) [graph.MapperShards]uint64 {
	return g.AdjList().Mapper().Watermark().Next()
}

// TestIDReserve_NeverReissued_MixedLifecycle drives seeded mixes of commits (both
// commit paths), rollbacks, checkpoints, clean closes and crashes, and checks that
// a new key never receives an id issued earlier to another key once that id is
// protected: its transaction reached durability, or its reservation became
// durable — a later commit's fsync covered it, a checkpoint recorded it, or a
// clean close flushed it. Every committed key keeps its id across every restart.
//
// Before WAL v2 step 4 a restart derived each shard's mark from the ids the log
// placed, so an id issued to a rolled-back transaction above the shard's last
// committed id was reissued.
func TestIDReserve_NeverReissued_MixedLifecycle(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(seed, 3021)) //nolint:gosec // G404: a seeded test schedule
			dir := t.TempDir()
			durable := map[string]graph.NodeID{} // committed key -> id
			owner := map[graph.NodeID]string{}   // protected id -> key
			var issued []graph.NodeID            // this session's unprotected ids
			keyOf := map[graph.NodeID]string{}
			protect := func() {
				for _, id := range issued {
					owner[id] = keyOf[id]
				}
				issued = issued[:0]
			}
			n, reused := 0, 0
			fresh := func(g *lpg.Graph[string, float64], k string) {
				id := idOf(t, g, k)
				if prev, ok := owner[id]; ok && prev != k {
					reused++
					t.Errorf("id %d issued to %q was protected for %q", id, k, prev)
				}
				keyOf[id] = k
				issued = append(issued, id)
			}
			for cycle := 0; cycle < 8; cycle++ {
				o := idOpen(t, dir, idOpts())
				for k, id := range durable {
					if got := idOf(t, o.Graph(), k); got != id {
						t.Errorf("cycle %d: %q has id %d, want %d", cycle, k, got, id)
					}
				}
				for i, ops := 0, 20+rng.IntN(40); i < ops; i++ {
					k := fmt.Sprintf("s%d-c%d-k%d", seed, cycle, n)
					n++
					switch r := rng.IntN(10); {
					case r < 4:
						idCommit(t, o.Store(), k)
						fresh(o.Graph(), k)
						durable[k] = idOf(t, o.Graph(), k)
						protect()
					case r < 6:
						idEager(t, o, k, true)
						fresh(o.Graph(), k)
						durable[k] = idOf(t, o.Graph(), k)
						protect()
					case r < 9:
						idEager(t, o, k, false)
						fresh(o.Graph(), k)
					default:
						idCheckpoint(t, o, dir)
						protect()
					}
				}
				if rng.IntN(2) == 0 {
					if err := o.Close(); err != nil {
						t.Fatalf("close: %v", err)
					}
					protect()
					continue
				}
				crashed := crashCopy(t, dir)
				if err := o.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				dir = crashed
				issued = issued[:0]
			}
			if n == 0 || len(durable) == 0 || len(owner) == len(durable) {
				t.Fatalf("premise: %d keys, %d durable, %d protected; the schedule must protect rolled-back ids", n, len(durable), len(owner))
			}
			if reused > 0 {
				t.Errorf("%d protected ids reissued", reused)
			}
		})
	}
}

// TestIDReserve_CleanRestartWastesNoID: after a clean close every shard resumes at
// exactly its mark before the close — also when a checkpoint recorded the
// reservation limits, which lie above the marks, before the close.
func TestIDReserve_CleanRestartWastesNoID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		checkpoint bool
	}{{"plain", false}, {"checkpoint_before_close", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			o := idOpen(t, dir, idOpts())
			keys := make([]string, 3000)
			for i := range keys {
				keys[i] = fmt.Sprintf("clean-%d", i)
			}
			idCommit(t, o.Store(), keys...)
			idEager(t, o, "clean-rolled-back", false)
			if tc.checkpoint {
				idCheckpoint(t, o, dir)
			}
			before := marks(o.Graph())
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			r := idOpen(t, dir, idOpts())
			defer func() { _ = r.Close() }()
			if after := marks(r.Graph()); after != before {
				for s := range before {
					if after[s] != before[s] {
						t.Errorf("shard %d: mark %d after a clean restart, want %d", s, after[s], before[s])
					}
				}
			}
		})
	}
}

// TestIDReserve_CrashRestartWastesAtMostOneBatch: after a crash every shard
// resumes at or above its mark at the crash, and at most one batch above it.
func TestIDReserve_CrashRestartWastesAtMostOneBatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := idOpen(t, dir, idOpts())
	for b := 0; b < 20; b++ {
		keys := make([]string, 200)
		for i := range keys {
			keys[i] = fmt.Sprintf("crash-%d-%d", b, i)
		}
		idCommit(t, o.Store(), keys...)
	}
	before := marks(o.Graph())
	var batch [graph.MapperShards]uint64
	for s := range batch {
		batch[s] = o.Store().IDBatchForTest(s)
	}
	crashed := crashCopy(t, dir)
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	r := idOpen(t, crashed, idOpts())
	defer func() { _ = r.Close() }()
	after := marks(r.Graph())
	wasted := uint64(0)
	for s := range before {
		if after[s] < before[s] || after[s]-before[s] > batch[s] {
			t.Errorf("shard %d: mark %d after a crash restart, want within [%d, %d]", s, after[s], before[s], before[s]+batch[s])
		}
		wasted += after[s] - before[s]
	}
	if wasted == 0 {
		t.Error("premise: a crash restart wasted no id; the crash image must lack the exact marks")
	}
}

// TestIDReserve_ExhaustionBurstsUnderLatency drives concurrent creation bursts
// into one shard, with seeded fsync latency and concurrent checkpoints, so
// interners exhaust their batch before the prefetch lands and reserve under the
// shard lock while committers hold the WAL writer. It must finish (no deadlock),
// and every committed key keeps a distinct id across a reopen.
func TestIDReserve_ExhaustionBurstsUnderLatency(t *testing.T) {
	t.Parallel()
	const workers, perWorker, perTx = 32, 2, 64
	keys := oneShardKeys(t, workers*perWorker*perTx)
	dir := t.TempDir()
	opts := idOpts()
	opts.SyncLatency = synclatency.ForTest(t)
	o := idOpen(t, dir, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan struct{})
	var cps atomic.Int64
	var cpWG sync.WaitGroup
	cpWG.Add(1)
	go func() {
		defer cpWG.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			var mu sync.Mutex
			cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
				checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
				checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
				checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
			if err := cp.RunCheckpoint(); err != nil {
				t.Errorf("checkpoint: %v", err)
				return
			}
			cps.Add(1)
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				base := (w*perWorker + i) * perTx
				idCommit(t, o.Store(), keys[base:base+perTx]...)
			}
		}(w)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		var b strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
		t.Fatalf("bursts did not finish within 60 s (deadlock?):\n%s", b.String())
	}
	close(done)
	cpWG.Wait()
	want := map[string]graph.NodeID{}
	seen := map[graph.NodeID]string{}
	for _, k := range keys {
		id := idOf(t, o.Graph(), k)
		if prev, dup := seen[id]; dup {
			t.Fatalf("id %d held by %q and %q", id, prev, k)
		}
		seen[id], want[k] = k, id
	}
	shard := int(graph.MapperShardOf(want[keys[0]]))
	if o.Store().IDBatchForTest(shard) <= txn.DefaultIDBatch {
		t.Fatalf("premise: the burst shard's batch stayed %d: no interner exhausted a batch before its prefetch landed", o.Store().IDBatchForTest(shard))
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if cps.Load() == 0 {
		t.Fatal("premise: no checkpoint ran during the bursts")
	}
	r := idOpen(t, dir, idOpts())
	defer func() { _ = r.Close() }()
	for k, id := range want {
		if got := idOf(t, r.Graph(), k); got != id {
			t.Errorf("%q: id %d after reopen, want %d", k, got, id)
		}
	}
}

// oneShardKeys returns n distinct keys in one mapper shard.
func oneShardKeys(t testing.TB, n int) []string {
	t.Helper()
	m := graph.NewMapper[string]()
	byShard := map[uint64][]string{}
	for i := 0; i < 2_000_000; i++ {
		k := fmt.Sprintf("burst-%07d", i)
		s := graph.MapperShardOf(m.Intern(k))
		byShard[s] = append(byShard[s], k)
		if len(byShard[s]) == n {
			return byShard[s]
		}
	}
	t.Fatalf("no shard received %d probe keys", n)
	return nil
}

// TestIDReserve_SynchronousNoGoroutine: reservations run on the interning
// goroutine — interning thousands of keys into one shard starts no goroutine —
// and nothing of the store's id machinery outlives Close. Not parallel, so
// goleak sees only this test's goroutines.
func TestIDReserve_SynchronousNoGoroutine(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	o := idOpen(t, t.TempDir(), idOpts())
	keys := oneShardKeys(t, 2000)
	before := runtime.NumGoroutine()
	for _, k := range keys {
		o.Graph().AdjList().Mapper().Intern(k)
	}
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("interning started goroutines: %d before, %d after", before, after)
	}
	shard := int(graph.MapperShardOf(idOf(t, o.Graph(), keys[0])))
	if lim := o.Graph().AdjList().Mapper().ReservedLimit(shard); lim < uint64(len(keys)) {
		t.Fatalf("premise: shard %d reserved limit %d below the %d keys interned", shard, lim, len(keys))
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIDReserve_FramesAreAFunctionOfTheOperations: two stores given the same
// operations write identical WAL frame sequences — the property the simulator's
// reproducibility tests rely on. A reservation made by a background goroutine
// would land at a scheduling-dependent position.
func TestIDReserve_FramesAreAFunctionOfTheOperations(t *testing.T) {
	t.Parallel()
	run := func() [][]byte {
		dir := t.TempDir()
		o := idOpen(t, dir, idOpts())
		for b := 0; b < 10; b++ {
			keys := make([]string, 100)
			for i := range keys {
				keys[i] = fmt.Sprintf("det-%d-%d", b, i)
			}
			idCommit(t, o.Store(), keys...)
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
		locs, err := waltest.LocateFrames(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatal(err)
		}
		out := make([][]byte, 0, len(locs))
		for _, l := range locs {
			out = append(out, l.Frame.Payload)
		}
		return out
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("frame counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("frame %d differs between identical runs", i)
		}
	}
}
