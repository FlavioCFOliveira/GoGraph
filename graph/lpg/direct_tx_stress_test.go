package lpg_test

// direct_tx_stress_test.go — rmp #2947: direct writes, shared brackets, explicit
// transactions and synchronous reclamation, all at once on one small graph, on
// both shapes. It is the audit's stress run of the rejected point-check design,
// kept: every direct mutator may now wait on another direct write, refuse on an
// explicit one, and abort and withdraw what it wrote, and none of that may
// deadlock, livelock or race. Run it with -race; the soak layer runs it for
// fifteen seconds per shape.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

func TestDirectTx_MixedWritersStress(t *testing.T) {
	for _, directed := range []bool{false, true} {
		t.Run(fmt.Sprintf("directed=%v", directed), func(t *testing.T) {
			directTxStress(t, directed, time.Second)
		})
	}
}

func TestDirectTx_MixedWritersStressSoak(t *testing.T) {
	testlayers.RequireSoak(t)
	for _, directed := range []bool{false, true} {
		t.Run(fmt.Sprintf("directed=%v", directed), func(t *testing.T) {
			directTxStress(t, directed, 15*time.Second)
		})
	}
}

func directTxStress(t *testing.T, directed bool, dur time.Duration) {
	g := lpg.New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	const nodes = 12
	name := func(i int) string { return fmt.Sprintf("n%d", i) }
	for i := range nodes {
		if err := g.AddNode(name(i)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	var ops, refusals atomic.Int64
	var unexpected atomic.Pointer[error]
	note := func(err error) {
		switch {
		case err == nil:
		case errors.Is(err, lpg.ErrDirectWriteConflict):
			refusals.Add(1)
		default:
			unexpected.CompareAndSwap(nil, &err)
		}
	}
	worker := func(f func(r *rand.Rand)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(rand.Uint64(), 1)) //nolint:gosec // test workload RNG
			for time.Now().Before(deadline) {
				f(r)
				ops.Add(1)
			}
		}()
	}
	pick := func(r *rand.Rand) string { return name(r.IntN(nodes)) }
	for range 3 {
		worker(func(r *rand.Rand) { note(g.RemoveAllEdgesFrom(pick(r))) })
		worker(func(r *rand.Rand) { note(g.AddEdge(pick(r), pick(r), 1)) })
		worker(func(r *rand.Rand) { note(g.RemoveEdge(pick(r), pick(r))) })
		worker(func(r *rand.Rand) {
			a, b := pick(r), pick(r)
			note(g.SetEdgeLabel(a, b, "T"))
			note(g.RemoveEdgeLabel(a, b, "T"))
			note(g.SetEdgeProperty(a, b, "p", lpg.Int64Value(1)))
		})
		worker(func(r *rand.Rand) {
			_ = g.ApplyVersioned(func(tx lpg.WriteTx) error {
				wv := g.Writer(tx)
				_ = wv.AddEdge(pick(r), pick(r), 1)
				wv.RemoveEdge(pick(r), pick(r))
				wv.RemoveAllEdgesFrom(pick(r))
				return tx.Err()
			})
		})
		worker(func(r *rand.Rand) {
			tx := g.BeginVersionedTx()
			_ = g.ApplyInVersionedTx(context.Background(), tx, func(tx lpg.WriteTx) error {
				wv := g.Writer(tx)
				_ = wv.AddEdge(pick(r), pick(r), 1)
				wv.RemoveAllEdgesFrom(pick(r))
				_ = wv.SetEdgeLabel(pick(r), pick(r), "U")
				return nil
			})
			time.Sleep(time.Duration(r.IntN(200)) * time.Microsecond)
			g.EndVersionedTx(tx)
		})
		worker(func(r *rand.Rand) {
			note(g.RemoveNode(pick(r)))
			note(g.AddNode(pick(r)))
		})
	}
	worker(func(*rand.Rand) {
		g.ReclaimNow()
		time.Sleep(time.Millisecond)
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(dur + time.Minute):
		t.Fatal("deadlock or livelock: the workers did not finish")
	}
	if p := unexpected.Load(); p != nil {
		t.Fatalf("a direct write failed with something other than a refusal: %v", *p)
	}
	t.Logf("directed=%v ops=%d refusals=%d", directed, ops.Load(), refusals.Load())
}
