package io_test

// concurrent_export_test.go — rmp #2902.
//
// Every NodeID-indexed exporter (dot, jsonl, csv, GraphML, and the jsonl and
// GraphML property writers) sizes its name table from MaxNodeID and then fills
// it inside a Mapper.Walk. A node interned between those two reads carries an
// id at or above the table length, and before #2902 the Walk callback indexed
// the table with it and panicked with an index out of range.
//
// The fixture makes that id certain for every concurrent intern: every key the
// writer interns lands in the last Mapper shard, which is also the tallest one,
// so each new id is packed with an intra-shard index equal to the tallest
// height and therefore equals or exceeds any MaxNodeID read before it. Walk
// visits the last shard last, after the padding shards, which widens the window
// between the table sizing and the read of the new id.
//
// Layer: short. goleak-clean on success: every goroutine is joined before
// return.

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	concExportPadNodes    = 30_000           // nodes spread over every shard but the last
	concExportTallHeight  = 1_000            // nodes in the last shard: the tallest one
	concExportWriterKeys  = 20_000           // keys the writer interns, per exporter
	concExportRun         = 2 * time.Second  // minimum run while the writer has budget
	concExportDeadline    = 60 * time.Second // hard bound on one exporter's run
	concExportMinOverlaps = 2                // exports that must overlap node creation
	concExportJoinWait    = 10 * time.Second
	concExportPace        = 50 * time.Microsecond // minimum gap between writer interns
)

// concExportShardOfKey reproduces the Mapper's string placement (unseeded
// FNV-1a 64, masked to the shard count). The test asserts it against a real
// Mapper before relying on it.
func concExportShardOfKey(k string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(k); i++ {
		h ^= uint64(k[i])
		h *= 1099511628211
	}
	return h & uint64(graph.MapperShardCount()-1)
}

// concExportKeys returns n distinct keys with the given prefix whose shard
// satisfies keep.
func concExportKeys(prefix string, n int, keep func(uint64) bool) []string {
	out := make([]string, 0, n)
	buf := []byte(prefix)
	for i := 0; len(out) < n; i++ {
		buf = strconv.AppendInt(buf[:len(prefix)], int64(i), 10)
		if keep(concExportShardOfKey(string(buf))) {
			out = append(out, string(buf))
		}
	}
	return out
}

type concExporter struct {
	name string
	run  func(g *lpg.Graph[string, int64]) error
}

func concExporters() []concExporter {
	ctx := context.Background()
	return []concExporter{
		{"dot.WriteCtx", func(g *lpg.Graph[string, int64]) error {
			return dot.WriteCtx(ctx, io.Discard, g.AdjList())
		}},
		{"jsonl.WriteCtx", func(g *lpg.Graph[string, int64]) error {
			_, err := jsonl.WriteCtx(ctx, io.Discard, g.AdjList())
			return err
		}},
		{"jsonl.WriteWithPropsCtx", func(g *lpg.Graph[string, int64]) error {
			_, err := jsonl.WriteWithPropsCtx(ctx, io.Discard, g)
			return err
		}},
		{"csv.WriteCtx", func(g *lpg.Graph[string, int64]) error {
			_, err := csv.WriteCtx(ctx, io.Discard, g.AdjList(), csv.Options{})
			return err
		}},
		{"graphml.WriteCtx", func(g *lpg.Graph[string, int64]) error {
			return graphml.WriteCtx(ctx, io.Discard, g.AdjList())
		}},
		{"graphml.WriteWithPropsCtx", func(g *lpg.Graph[string, int64]) error {
			return graphml.WriteWithPropsCtx(ctx, io.Discard, g)
		}},
	}
}

// exportNoPanic runs one export and converts a panic into an error.
func exportNoPanic(e concExporter, g *lpg.Graph[string, int64]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panicked: %v", e.name, r)
		}
	}()
	return e.run(g)
}

func TestExporters_ConcurrentNodeCreationDoesNotPanic(t *testing.T) {
	last := uint64(graph.MapperShardCount() - 1)
	probe := graph.NewMapper[string]()
	for _, k := range []string{"a", "pad17", "tall4242", "w99"} {
		if got, want := graph.MapperShardOf(probe.Intern(k)), concExportShardOfKey(k); got != want {
			t.Fatalf("placement model disagrees with the Mapper for %q: shard %d, model %d", k, got, want)
		}
	}
	pad := concExportKeys("pad", concExportPadNodes, func(s uint64) bool { return s != last })
	tall := concExportKeys("tall", concExportTallHeight, func(s uint64) bool { return s == last })
	exporters := concExporters()
	fresh := concExportKeys("w", concExportWriterKeys*len(exporters), func(s uint64) bool { return s == last })

	for i, e := range exporters {
		t.Run(e.name, func(t *testing.T) {
			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			for j, k := range pad {
				if err := g.AddNode(k); err != nil {
					t.Fatalf("AddNode %s: %v", k, err)
				}
				if j > 0 && j%3 == 0 {
					if err := g.AddEdge(pad[j-1], k, int64(j)); err != nil {
						t.Fatalf("AddEdge: %v", err)
					}
				}
			}
			for _, k := range tall {
				if err := g.AddNode(k); err != nil {
					t.Fatalf("AddNode %s: %v", k, err)
				}
			}
			if err := g.SetNodeProperty(pad[0], "k", lpg.Int64Value(1)); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
			keys := fresh[i*concExportWriterKeys : (i+1)*concExportWriterKeys]

			stop := make(chan struct{})
			started := make(chan struct{})
			var wg sync.WaitGroup
			var interned atomic.Uint64
			// inFlight gates the writer to the exports: a budget spent between
			// exports overlaps nothing.
			var inFlight atomic.Bool
			writerErr := make(chan error, 1)
			wg.Add(1)
			go pprof.Do(context.Background(), pprof.Labels("role", "test-2902-concurrent-node-writer"), func(context.Context) {
				defer wg.Done()
				select {
				case <-started:
				case <-stop:
					return
				}
				var last time.Time
				for n := 0; n < len(keys); {
					k := keys[n]
					select {
					case <-stop:
						return
					default:
					}
					// Pace the writer so its budget spans the whole export,
					// including the late table sizing of the property writers.
					if !inFlight.Load() || time.Since(last) < concExportPace {
						runtime.Gosched()
						continue
					}
					last = time.Now()
					if err := g.AddNode(k); err != nil {
						writerErr <- fmt.Errorf("writer AddNode %s: %w", k, err)
						return
					}
					interned.Add(1)
					n++
				}
			})

			a := g.AdjList()
			close(started)
			start := time.Now()
			var exports, overlaps int
			var failure error
			// Run for at least concExportRun, and past it until enough exports
			// have overlapped node creation: an export under the race detector
			// is an order of magnitude slower, so a fixed window would hold a
			// build-dependent number of exports.
			for interned.Load() < uint64(len(keys)) && time.Since(start) < concExportDeadline &&
				(time.Since(start) < concExportRun || overlaps < concExportMinOverlaps) {
				before := a.MaxNodeID()
				inFlight.Store(true)
				err := exportNoPanic(e, g)
				inFlight.Store(false)
				if err != nil {
					failure = err
					break
				}
				exports++
				if a.MaxNodeID() > before {
					overlaps++
				}
			}
			close(stop)
			joined := make(chan struct{})
			go func() { wg.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(concExportJoinWait):
				// A panic inside a Mapper.Walk callback leaves the walked
				// shard read-locked, so the writer can never intern again.
				t.Fatalf("export %d with %d nodes interned concurrently: %v; the writer did not stop within %v",
					exports+1, interned.Load(), failure, concExportJoinWait)
			}
			close(writerErr)
			for err := range writerErr {
				t.Error(err)
			}
			if failure != nil {
				t.Fatalf("export %d with %d nodes interned concurrently: %v", exports+1, interned.Load(), failure)
			}
			if overlaps < concExportMinOverlaps {
				t.Fatalf("vacuous run: %d of %d exports overlapped node creation (want >= %d)",
					overlaps, exports, concExportMinOverlaps)
			}
			t.Logf("%d exports, %d overlapped node creation, %d nodes interned concurrently",
				exports, overlaps, interned.Load())
		})
	}
}
