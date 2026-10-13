package cypher

// pattern_eval_walk_reentry_test.go — rmp #2896.
//
// The incoming leg of the pattern evaluator finds the arcs INTO a node by
// walking every interned node with [graph.Mapper.Walk]. Walk holds each shard's
// read lock for the whole of that shard's iteration, and its contract forbids
// the callback from re-entering the Mapper while a writer may run: once a
// writer's Intern queues on the shard's write lock, sync.RWMutex admits no new
// reader, so a nested read lock on the walked shard blocks forever, and with it
// the writer and every later operation on the shard.
//
// Before #2896 three callbacks broke that contract — matchIncoming,
// collectIncomingCandidates and bfsExpandIncoming each resolved the edge type of
// a hit by KEY, and [lpg.ReadView.EdgeLabels] / [lpg.ReadView.HasEdge] look the
// key up in the Mapper, on the very shard being walked (the key's own shard).
// matchIncoming also recursed into the remaining steps from inside the callback.
//
// This test drives all three legs while a writer interns fresh nodes, and fails
// on a deadline if any of them stops making progress.
//
// Layer: short. goleak-clean: every goroutine is joined before return.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	walkReentryInDegree = 4096            // arcs into the hub: the hits each walk resolves
	walkReentryRun      = 3 * time.Second // how long readers and the writer overlap
	walkReentryDeadline = 60 * time.Second
	walkReentryMaxNodes = 400_000 // bounds the writer's growth
)

func TestPatternEval_IncomingWalkDoesNotReenterMapper(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("h"); err != nil {
		t.Fatalf("AddNode h: %v", err)
	}
	for i := range walkReentryInDegree {
		k := fmt.Sprintf("s%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.AddEdgeLabeled(k, "h", 1, "R"); err != nil {
			t.Fatalf("AddEdgeLabeled %s: %v", k, err)
		}
	}
	row := bindRow(t, g, "h", "h")

	// A type no arc carries makes every walk visit every hit and resolve its
	// type, which maximises the nested lookups the pre-fix callbacks issued.
	one, two := int64(1), int64(2)
	predicate := typedHop("h", "x", "NOPE", ast.RelDirectionIncoming)
	varLen := typedHop("h", "x", "NOPE", ast.RelDirectionIncoming)
	varLen.Head.Next.Relationship.Range = &ast.RangeQuantifier{Min: &one, Max: &two}
	comprehension := &ast.PatternComprehension{
		Pattern:    typedHop("h", "x", "NOPE", ast.RelDirectionIncoming),
		Projection: &ast.IntLiteral{Value: 1},
	}
	probes := []struct {
		name string
		run  func(pe *patternEvaluator) (expr.Value, error)
	}{
		{"matchIncoming", func(pe *patternEvaluator) (expr.Value, error) {
			return pe.EvalPattern(context.Background(), predicate, row, nil)
		}},
		{"bfsExpandIncoming", func(pe *patternEvaluator) (expr.Value, error) {
			return pe.EvalPattern(context.Background(), varLen, row, nil)
		}},
		{"collectIncomingCandidates", func(pe *patternEvaluator) (expr.Value, error) {
			return pe.EvalPatternComp(context.Background(), comprehension, row, nil, nil)
		}},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var evals, interned atomic.Uint64
	errs := make(chan error, 8)

	wg.Add(1)
	go func() { // writer: every AddNode of a fresh key takes a shard's write lock
		defer wg.Done()
		for i := 0; i < walkReentryMaxNodes; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.AddNode(fmt.Sprintf("w%d", i)); err != nil {
				errs <- fmt.Errorf("writer AddNode: %w", err)
				return
			}
			interned.Add(1)
		}
	}()
	for r := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := r; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := probes[i%len(probes)]
				v, err := p.run(newPatternEvaluator(g.ReadAt(nil), 0))
				if err != nil {
					errs <- fmt.Errorf("%s: %w", p.name, err)
					return
				}
				// No arc carries NOPE: the predicate is false and the
				// comprehension empty.
				if b, ok := v.(expr.BoolValue); ok && bool(b) {
					errs <- fmt.Errorf("%s: matched a NOPE arc", p.name)
					return
				}
				if l, ok := v.(expr.ListValue); ok && len(l) != 0 {
					errs <- fmt.Errorf("%s: %d matches over NOPE", p.name, len(l))
					return
				}
				evals.Add(1)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		time.Sleep(walkReentryRun)
		close(stop)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(walkReentryDeadline):
		t.Fatalf("no progress within %v after %d evaluations and %d interned nodes: "+
			"an incoming-leg Walk callback re-entered the Mapper and deadlocked against the writer",
			walkReentryDeadline, evals.Load(), interned.Load())
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Non-vacuity: the readers and the writer must both have run.
	if evals.Load() == 0 || interned.Load() == 0 {
		t.Fatalf("vacuous run: %d evaluations, %d interned nodes", evals.Load(), interned.Load())
	}
	t.Logf("%d evaluations, %d nodes interned concurrently", evals.Load(), interned.Load())
}
