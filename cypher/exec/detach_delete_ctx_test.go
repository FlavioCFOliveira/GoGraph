package exec_test

// detach_delete_ctx_test.go — sprint #157, task #1308.
//
// White-box regression test for the in-sweep ctx.Err() check added to the
// DetachDelete edge-removal loops. DetachDelete.Next removes every incident
// edge of a node inside a single Next() call; the operator's per-Next ctx
// check (at the top of Next) fires once per node, so a supernode's O(degree)
// sweep previously ran with no further cancellation check.
//
// The test drives op.Next directly (NOT exec.Drain, whose own pre-Next ctx
// check would mask the operator's internal check) with a context that lets the
// per-Next guard pass and reports cancelled on the next poll — the first
// in-sweep poll. The operator must abort the sweep and return the context
// error having removed ZERO edges; pre-fix it removed every edge and returned
// (true, nil).
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// cancelAfterNPolls returns nil for its first n Err() calls and
// context.Canceled on every call thereafter. The ctx.Err() calls preceding the
// edge sweep are deterministic: (1) DetachDelete.Next's per-node guard at the
// top of Next, then (2) the child sliceOperator.Next's own ctx guard as it
// emits the driving row. n == 2 therefore lets both pass and trips on call 3 —
// the first in-sweep poll (swept == 0) — isolating the in-sweep check with no
// dependence on scheduling or sweep speed.
type cancelAfterNPolls struct {
	context.Context
	n     int64
	calls atomic.Int64
}

func (c *cancelAfterNPolls) Err() error {
	if c.calls.Add(1) <= c.n {
		return nil
	}
	return context.Canceled
}

// TestDetachDelete_Next_CancelDuringEdgeSweep verifies that cancellation during
// the incident-edge sweep of a high-degree node is observed by the in-sweep
// poll: op.Next returns context.Canceled and no edge was removed.
func TestDetachDelete_Next_CancelDuringEdgeSweep(t *testing.T) {
	t.Parallel()

	mut := newStubMutator()
	hubID := mustAddNode(t, mut, "hub")
	const leaves = 10_000
	for i := 0; i < leaves; i++ {
		leaf := fmt.Sprintf("leaf%d", i)
		mustAddNode(t, mut, leaf)
		mustAddEdge(t, mut, "hub", leaf, 0) // outgoing spoke hub→leaf
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(hubID))}
	op := exec.NewDetachDelete("n", schema, newSliceOperator(row), mut)

	ctx := &cancelAfterNPolls{Context: context.Background(), n: 2}
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = op.Close() }()

	var out exec.Row
	ok, err := op.Next(&out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next during sweep: ok=%v err=%v; want err matching context.Canceled", ok, err)
	}
	// The in-sweep poll trips on the first edge (swept == 0), so the sweep
	// must abort before removing any edge. If the check were absent the whole
	// sweep would complete and every spoke would be gone.
	removed := 0
	for i := 0; i < leaves; i++ {
		if !mut.HasEdge("hub", fmt.Sprintf("leaf%d", i)) {
			removed++
		}
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d/%d edges before honouring cancellation; want 0", removed, leaves)
	}
}

// TestDetachDelete_Next_LiveContext_SweepsAll is the non-cancelled control: with
// a live context the operator sweeps every incident edge and emits its row, so
// the in-sweep poll must not perturb the normal full-sweep result.
func TestDetachDelete_Next_LiveContext_SweepsAll(t *testing.T) {
	t.Parallel()

	mut := newStubMutator()
	hubID := mustAddNode(t, mut, "hub")
	const leaves = 10_000
	for i := 0; i < leaves; i++ {
		leaf := fmt.Sprintf("leaf%d", i)
		mustAddNode(t, mut, leaf)
		mustAddEdge(t, mut, "hub", leaf, 0)
	}

	schema := map[string]int{"n": 0}
	row := exec.Row{expr.IntegerValue(int64(hubID))}
	op := exec.NewDetachDelete("n", schema, newSliceOperator(row), mut)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("live ctx: DetachDelete emitted %d rows; want 1", len(rows))
	}
	for i := 0; i < leaves; i++ {
		if mut.HasEdge("hub", fmt.Sprintf("leaf%d", i)) {
			t.Fatalf("live ctx: edge hub→leaf%d should have been removed", i)
		}
	}
}
