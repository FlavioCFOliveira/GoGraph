package graphml_test

// writer_props_walk_reentry_test.go — rmp #2897.
//
// [graphml.WriteWithPropsCtx] walks the Mapper twice (the key-kind scan and the
// <node> emission) and, before #2897, read each walked node's properties and
// labels BY KEY inside the Walk callback. Those reads Lookup the key in the
// Mapper, on the very shard being walked, which the Walk contract forbids while
// a writer may run: once a writer's Intern queues on the shard's write lock,
// sync.RWMutex admits no new reader, and the nested read lock deadlocks the
// export, the writer and every later operation on the shard.
//
// The writer interns only into one shard, and never grows it past the tallest
// shard, so the export's own NodeID-indexed name table (sized from MaxNodeID
// before its edge walk) is never outgrown: this test isolates the re-entry
// defect from that separate precondition of the exporters.
//
// Layer: short. goleak-clean on success: every goroutine is joined before
// return.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	reentryTallShard   = 0
	reentryTargetShard = 1
	reentryTallHeight  = 12_000 // nodes in the tallest shard: bounds the writer
	reentryTargetStart = 2_000  // nodes in the target shard before the writer runs
	reentryRun         = 3 * time.Second
	reentryDeadline    = 60 * time.Second
	reentryReaders     = 4
)

// mapperShardOfKey reproduces the Mapper's documented string placement
// (unseeded FNV-1a 64, masked to the shard count). The test asserts it against
// a real Mapper before relying on it.
func mapperShardOfKey(k string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(k); i++ {
		h ^= uint64(k[i])
		h *= 1099511628211
	}
	return h & uint64(graph.MapperShardCount()-1)
}

// keysInShard returns n distinct keys with the given prefix that the Mapper
// places in shard.
func keysInShard(prefix string, shard uint64, n int) []string {
	out := make([]string, 0, n)
	for i := 0; len(out) < n; i++ {
		k := fmt.Sprintf("%s%d", prefix, i)
		if mapperShardOfKey(k) == shard {
			out = append(out, k)
		}
	}
	return out
}

func TestWriteWithProps_ConcurrentWriterDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	// The placement model must match the real Mapper, or the bound below is void.
	probe := graph.NewMapper[string]()
	for _, k := range []string{"a", "t17", "tall4242", "w99"} {
		if got, want := graph.MapperShardOf(probe.Intern(k)), mapperShardOfKey(k); got != want {
			t.Fatalf("placement model disagrees with the Mapper for %q: shard %d, model %d", k, got, want)
		}
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, k := range keysInShard("tall", reentryTallShard, reentryTallHeight) {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	for i, k := range keysInShard("t", reentryTargetShard, reentryTargetStart) {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "L"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if err := g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	// Every key the writer interns lands in the target shard, which stays below
	// the tall shard's height.
	fresh := keysInShard("w", reentryTargetShard, reentryTallHeight-reentryTargetStart-1)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var exports, interned atomic.Uint64
	errs := make(chan error, reentryReaders+1)
	// The writer holds back until an export is running: its interning budget is
	// bounded, and a budget spent before any export starts overlaps nothing.
	started := make(chan struct{})
	var startOnce sync.Once

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-started:
		case <-stop:
			return
		}
		for _, k := range fresh {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.AddNode(k); err != nil {
				errs <- fmt.Errorf("writer AddNode %s: %w", k, err)
				return
			}
			interned.Add(1)
		}
	}()
	for range reentryReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				startOnce.Do(func() { close(started) })
				if err := graphml.WriteWithPropsCtx(context.Background(), io.Discard, g); err != nil {
					errs <- fmt.Errorf("WriteWithPropsCtx: %w", err)
					return
				}
				exports.Add(1)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		time.Sleep(reentryRun)
		close(stop)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(reentryDeadline):
		var dump bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
		t.Fatalf("no progress within %v after %d exports and %d interned nodes: a Mapper.Walk "+
			"callback re-entered the Mapper and deadlocked against the writer\n%s",
			reentryDeadline, exports.Load(), interned.Load(), dump.String())
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if exports.Load() == 0 || interned.Load() == 0 {
		t.Fatalf("vacuous run: %d exports, %d interned nodes", exports.Load(), interned.Load())
	}
	t.Logf("%d exports, %d nodes interned concurrently", exports.Load(), interned.Load())
}
