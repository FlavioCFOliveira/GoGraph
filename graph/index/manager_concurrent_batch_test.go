package index_test

// manager_concurrent_batch_test.go — the secondary-index batch publish's
// ordering basis (rmp #2303, MVCC B1, audit finding E13).
//
// [index.Manager.ApplyBatch] is called at the transaction boundary from
// exec.IndexBuffer, today inside the write visibility barrier, so exactly one
// batch is ever in flight. rmp #2304 removes that guarantee, and these tests are
// what has to hold when it does.
//
// The Manager's own subscriber contract already names the one order-sensitive
// case: a subscriber need NOT be order-independent across MULTIPLE changes to the
// SAME property key, because those carry old→new payloads. Everything else — a
// property SET interleaved with a label add/remove, changes to different nodes,
// a replayed change — is required to be idempotent and order-independent.
//
// So the basis has three parts, and this file tests the two that are properties
// of this package:
//
//  1. WITHIN one batch, mutation order is preserved: one buffer belongs to one
//     transaction and ApplyBatch walks the slice in order.
//  2. ACROSS concurrent batches, every sub-index operation is internally locked,
//     so interleaving cannot corrupt a posting list or lose an entry.
//  3. The same-property-key case cannot ARISE between two concurrently-committing
//     transactions, because both writing a property on one node is a write-write
//     conflict (graph/lpg's node-property store, rmp #2300) and one of them
//     aborts before it reaches a buffer. That third part is enforced in
//     graph/lpg, not here, and is tested there.
//
// Part 3 is the load-bearing one and it is a DEPENDENCY, not a property of this
// package: if conflict detection on node properties were ever relaxed to be
// per-(node, key) rather than per-node, this contract would still hold, but if it
// were removed the index could diverge from the graph with nothing here to catch
// it. Recorded so the coupling is visible from both ends.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestManager_ConcurrentApplyBatchLosesNothing drives many goroutines through
// ApplyBatch with no barrier of any kind, each publishing label additions for its
// own disjoint node range, and requires every entry to be present afterwards.
//
// This is the state a serial schedule would produce: the union of every batch.
// A lost entry means an index that under-reports, which surfaces as a query
// silently missing rows — the failure mode an index defect actually has.
func TestManager_ConcurrentApplyBatchLosesNothing(t *testing.T) {
	const (
		writers   = 16
		perWriter = 200
		labelID   = uint32(3)
	)
	mgr := index.NewManager()
	li := label.NewNodeIndex()
	if err := mgr.CreateIndex("l", li); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			// Disjoint node ranges, so nothing here relies on conflict detection:
			// this arm isolates whether concurrent fan-out itself loses work.
			batch := make([]index.Change, 0, perWriter)
			for i := 0; i < perWriter; i++ {
				batch = append(batch, index.Change{
					Op:    index.OpAddNodeLabel,
					Label: labelID,
					Node:  graph.NodeID(w*perWriter + i),
				})
			}
			mgr.ApplyBatch(batch)
		}(w)
	}
	wg.Wait()

	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			id := graph.NodeID(w*perWriter + i)
			if !li.Has(labelID, id) {
				t.Fatalf("node %d is absent from the label index after %d concurrent "+
					"batches: a published entry was lost, so the index under-reports",
					id, writers)
			}
		}
	}
	if got, want := li.Count(labelID), uint64(writers*perWriter); got != want {
		t.Errorf("label index cardinality = %d, want %d — concurrent batches did not "+
			"compose to the union a serial schedule would produce", got, want)
	}
}

// TestManager_ConcurrentApplyBatchAddRemoveSameNodeConverges is the interleaving
// the subscriber contract explicitly permits: changes to DIFFERENT facets, and
// idempotent repeats, applied concurrently to the SAME node.
//
// Every writer adds its own label to one shared node. Label add is idempotent and
// order-independent, so the node must end up carrying every label — not whichever
// writer happened to go last.
func TestManager_ConcurrentApplyBatchAddRemoveSameNodeConverges(t *testing.T) {
	const (
		writers = 16
		node    = graph.NodeID(42)
	)
	mgr := index.NewManager()
	li := label.NewNodeIndex()
	if err := mgr.CreateIndex("l", li); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			lbl := uint32(w)
			// Repeated on purpose: a replayed change must produce no different
			// state, which is the idempotence half of the contract.
			mgr.ApplyBatch([]index.Change{
				{Op: index.OpAddNodeLabel, Label: lbl, Node: node},
				{Op: index.OpAddNodeLabel, Label: lbl, Node: node},
			})
		}(w)
	}
	wg.Wait()

	for w := 0; w < writers; w++ {
		if !li.Has(uint32(w), node) {
			t.Errorf("label %d is absent from node %d: one writer's facet displaced "+
				"another's, so the index depends on the schedule", w, node)
		}
	}
}

// TestManager_ApplyBatchPreservesMutationOrderWithinABatch pins part 1, which is
// the half concurrency cannot help with: a single batch's same-key changes carry
// old→new payloads and must land in mutation order.
//
// It stays true under rmp #2304 for a structural reason worth stating rather than
// assuming: one IndexBuffer belongs to one transaction, so a batch is never
// assembled by two goroutines and ApplyBatch walks it in slice order.
func TestManager_ApplyBatchPreservesMutationOrderWithinABatch(t *testing.T) {
	const (
		node = graph.NodeID(7)
		lbl  = uint32(11)
	)
	mgr := index.NewManager()
	li := label.NewNodeIndex()
	if err := mgr.CreateIndex("l", li); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	// add, add, remove — the final state must be the LAST one, which is only true
	// if the batch is applied in order.
	//
	// THE SEQUENCE MUST NOT BE A PALINDROME, and the first draft of this test was
	// one: add/remove/add reversed is add/remove/add, so it passed against a build
	// with ApplyBatch deliberately iterating the batch BACKWARDS. The differential
	// is what caught it. Both sequences below are asymmetric under reversal.
	mgr.ApplyBatch([]index.Change{
		{Op: index.OpAddNodeLabel, Label: lbl, Node: node},
		{Op: index.OpAddNodeLabel, Label: lbl, Node: node},
		{Op: index.OpRemoveNodeLabel, Label: lbl, Node: node},
	})
	if li.Has(lbl, node) {
		t.Fatal("add/add/remove left the node PRESENT: the batch was not applied in " +
			"mutation order, so an old→new payload sequence would land backwards")
	}

	// And the reverse sequence, so the assertion above cannot pass by a
	// remove-wins-always bug.
	mgr.ApplyBatch([]index.Change{
		{Op: index.OpRemoveNodeLabel, Label: lbl, Node: node},
		{Op: index.OpRemoveNodeLabel, Label: lbl, Node: node},
		{Op: index.OpAddNodeLabel, Label: lbl, Node: node},
	})
	if !li.Has(lbl, node) {
		t.Fatal("remove/remove/add left the node ABSENT: the batch was not applied " +
			"in mutation order")
	}
}
