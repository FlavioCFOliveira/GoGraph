package cypher_test

// refill_3121_test.go — regression gate for rmp #3121: after a large rolled-back
// creation, new nodes must not bring back the memory of the released ids.
//
// # What was wrong
//
// The release of rmp #3119 cut the released ids off the end of each mapper
// shard's reverse table but kept the shard's high-water mark, so no id is issued
// twice. The next key interned in a shard therefore took an index far past the
// end of the table, and the table was extended with a hole slot for every index
// in between: 16 B per released id, back again. Measured on the base of this
// change (78039599): a 7.2-million-id rollback left 2.5 MB live, and 1000
// committed creates after it brought the live heap to 124.3 MB.
//
// # What the gate asserts
//
// After a rollback that drew at least refillMinIssued ids and refillCreates
// committed creations, the live heap is within refillSlack +
// refillBytesPerNode x refillCreates + refillBytesPerIssuedID x issued of the
// level before the rollback, and the mapper holds exactly the committed nodes.
// refillBytesPerIssuedID = 2 is eight times below the defect's 16 B per id;
// refillBytesPerNode is generous for what a committed node keeps (its key, its
// forward and reverse entries, its label and property).
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// refillCreates is how many nodes [TestRefillAfterLargeRollbackStaysSmall_3121]
// commits after the rollback, and refillBytesPerNode the heap it allows each of
// them: a committed node legitimately keeps its key, its forward and reverse
// entries and its label and property.
const (
	refillCreates      = 1000
	refillBytesPerNode = 512
	// refillMinIssued is larger than minIssued so that the cost of re-allocating
	// the released slots (16 B per id before rmp #3121) stands well clear of the
	// bound below.
	refillMinIssued = 250_000
	// refillBytesPerIssuedID is what one released id may still cost once new
	// keys arrive. A released id costs nothing once the new keys start a shard's
	// tail; the allowance leaves room for the tail pages the new keys open.
	refillBytesPerIssuedID = 2
	refillSlack            = 1 << 20
)

// TestRefillAfterLargeRollbackStaysSmall_3121 is the gate for rmp #3121; see the
// file comment.
func TestRefillAfterLargeRollbackStaysSmall_3121(t *testing.T) {
	// Not parallel: the gate reads the process heap.
	g := lpg.New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()
	eng := cypher.NewEngine(g)
	const committed = 600
	mustRun3119(t, eng, `UNWIND range(1, 600) AS i CREATE (:N {i: i})`, nil)
	m := g.AdjList().Mapper()

	before := liveHeap()
	issued := cutUntilIssued(t, m, refillMinIssued, func(ctx context.Context) error {
		return runExpectFailure(ctx, t, eng, `MATCH (a), (b), (c) CREATE ()`)
	})
	mustRun3119(t, eng, `UNWIND range(1, $n) AS i CREATE (:R {i: i})`, map[string]any{"n": refillCreates})
	after := liveHeap()
	bound := uint64(refillSlack) + refillBytesPerNode*refillCreates + refillBytesPerIssuedID*issued
	assertReleased(t, eng, g, committed+refillCreates, before, after, bound, issued)
}
