package exec

// produce_results_initclose_internal_test.go — regression gate for the
// 2026-06-25 reliability audit finding #1760: when the root plan.Init returns
// an error, exec.Run must still call plan.Close(), so a child operator whose
// own Init already succeeded and acquired a resource (ParallelGovernor slot +
// spawned workers) is released. Otherwise the caller's deferred Close no-ops
// (it short-circuits on rs.closed), permanently leaking the governor slot and
// the worker goroutines.
//
// White-box (package exec) so it can read the unexported governor counter.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// idWalker is a minimal nodeWalker over a contiguous [0,n) NodeID range.
type idWalker struct{ n int }

func (w idWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for i := 0; i < w.n; i++ {
		if !fn(graph.NodeID(i)) {
			return
		}
	}
}

// blockingSub is a subplan operator whose Next blocks until its context is
// cancelled — modelling a worker that is mid-flight when the plan is torn down.
type blockingSub struct{ ctx context.Context } //nolint:containedctx // test stub

func (s *blockingSub) Init(ctx context.Context) error { s.ctx = ctx; return nil }
func (s *blockingSub) Next(_ *Row) (bool, error) {
	<-s.ctx.Done()
	return false, s.ctx.Err()
}
func (s *blockingSub) Close() error { return nil }

// initFailParent models a binary-style parent (e.g. HashJoin) whose Init first
// initialises a child that succeeds and acquires resources, then fails on a
// second step — exactly the shape that left the child's Close uncalled.
type initFailParent struct{ child Operator }

var errParentInit = errors.New("parent init failed after child init succeeded")

func (p *initFailParent) Init(ctx context.Context) error {
	if err := p.child.Init(ctx); err != nil {
		return err
	}
	return errParentInit
}
func (p *initFailParent) Next(_ *Row) (bool, error) { return false, nil }
func (p *initFailParent) Close() error              { return p.child.Close() }

// TestRun_InitError_ReleasesChildResources is the #1760 gate. With the fix,
// Run calls plan.Close() on the Init error, so the governor returns to 0 and
// the workers are joined; goleak confirms no goroutine survives.
func TestRun_InitError_ReleasesChildResources(t *testing.T) {
	defer goleak.VerifyNone(t)

	gov := &ParallelGovernor{}
	leaf := NewParallelScanProject(
		idWalker{n: 8},
		func(_ []graph.NodeID) (Operator, error) { return &blockingSub{}, nil },
		2, // small morsel → several workers, all blocked in Next
		gov,
	)
	parent := &initFailParent{child: leaf}

	rs := Run(context.Background(), parent, []string{"x"})
	if rs.Err() == nil {
		t.Fatal("Run: expected an init error, got nil")
	}

	// The fix runs plan.Close() inside Run; the governor slot must be released.
	if got := gov.inflight.Load(); got != 0 {
		t.Fatalf("ParallelGovernor.inflight leaked: got %d, want 0 (#1760 — exec.Run did not Close the plan on Init error)", got)
	}

	// A real caller's deferred Close must remain a safe idempotent no-op.
	if err := rs.Close(); err != nil {
		t.Fatalf("rs.Close after init error: %v", err)
	}
}
