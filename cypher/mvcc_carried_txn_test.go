package cypher_test

// mvcc_carried_txn_test.go — rmp #2320's acceptance gates: the Cypher write path
// CARRIES its transaction, so no write on it resolves a commit record through the
// graph's ambient slot, and every version of one statement lands on ONE record.
//
// # Why these two assertions and not "the tests are green"
//
// The defect this task closes is invisible to a green suite, which is exactly how
// it shipped: with one write bracket open at a time the ambient slot always names
// the writer's own transaction, so the shortcut is indistinguishable from the
// correct thing. It becomes wrong only under two overlapping brackets, and then it
// is wrong SILENTLY — every field involved is atomic, so -race says nothing, and
// the damage (a statement split across two commit records) is only observable to a
// reader that happens to snapshot between the two commit instants.
//
// So the gates assert the MECHANISM rather than a downstream symptom:
//
//   - TestWritePath_ResolvesNoCommitRecordThroughTheAmbientSlot counts ambient
//     resolutions across a representative write surface and requires ZERO;
//   - TestWritePath_OneStatementLandsOnOneCommitRecord plants a DECOY transaction
//     on the slot and requires the statement to ignore it.
//
// Both fail against a build where the threading is bypassed. That the counter
// moves when a write does resolve through the slot is pinned inside lpg by
// TestWriteView_CarriesTheTransactionRatherThanResolvingIt, because no exported
// write path resolves through the slot any more (rmp #2967); see
// TestAmbientSlotGate_NoExportedWriteResolvesThroughTheSlot below.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// writeSurface is the set of statements the gates drive. It is deliberately broad
// rather than minimal: the threading had to reach node labels, node properties,
// node existence, the adjacency, the per-pair relationship type and the columnar
// edge properties, and three of those (topology, per-slot type, columnar edge
// property) had NO transaction-carrying form before this task because they write
// through the adjacency rather than through a node-side store.
var writeSurface = []string{
	`CREATE (:Acct {id:'a', bal:100})`,
	`CREATE (:Acct {id:'b', bal:100})`,
	`MATCH (a:Acct {id:'a'}) SET a.bal = a.bal - 10, a.note = 'debited'`,
	`MATCH (a:Acct {id:'a'}), (b:Acct {id:'b'}) CREATE (a)-[:PAID {amt:10}]->(b)`,
	`MATCH (a:Acct {id:'a'})-[r:PAID]->(b:Acct {id:'b'}) SET r.amt = 11, r.ok = true`,
	`MATCH (a:Acct {id:'a'}) SET a:Settled`,
	`MATCH (a:Acct {id:'a'}) REMOVE a:Settled`,
	`MATCH (a:Acct {id:'a'}) REMOVE a.note`,
	`MERGE (c:Acct {id:'c'}) ON CREATE SET c.bal = 0`,
	`MATCH (a:Acct {id:'a'})-[r:PAID]->() DELETE r`,
	`MATCH (c:Acct {id:'c'}) DELETE c`,
}

// TestWritePath_ResolvesNoCommitRecordThroughTheAmbientSlot is acceptance
// criterion 1: no write on the autocommit Cypher path resolves its commit record
// through the graph's ambient slot.
//
// [lpg.Graph.AmbientVersionResolutions] counts exactly that resolution, and it is
// counted on the ambient branch only — a threaded write never enters it — so the
// assertion is a direct observation and the instrument costs the measured path
// nothing.
func TestWritePath_ResolvesNoCommitRecordThroughTheAmbientSlot(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	before := g.AmbientVersionResolutions()
	for _, q := range writeSurface {
		r, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if err := drain(r); err != nil {
			t.Fatalf("%s: drain: %v", q, err)
		}
	}
	if n := g.AmbientVersionResolutions() - before; n != 0 {
		t.Fatalf("the write path resolved %d version(s) through the ambient slot; "+
			"every write must CARRY its transaction (rmp #2320). With two brackets open "+
			"an ambient resolution adopts a concurrent transaction's commit record and "+
			"splits one statement across two commit instants", n)
	}
}

// TestAmbientSlotGate_NoExportedWriteResolvesThroughTheSlot is the gate's
// companion: no write an exported API makes resolves its commit record through
// the slot while an unrelated exclusive bracket is published on it. A direct
// Go-API mutator runs as an implicit transaction of its own (rmp #2947), and a
// raw adjacency write is its own transaction too (rmp #2967); either one joining
// the bracket would be lost if the bracket aborted. A write made with NO bracket
// open is not an ambient resolution either: it takes a fresh commit timestamp of
// its own.
//
// The counter's liveness — that it MOVES when a write does resolve through the
// slot, so a zero means "threaded" and not "the instrument is dead" — can no
// longer be shown from outside lpg, because no exported write path resolves
// through the slot. It is pinned inside lpg by
// TestWriteView_CarriesTheTransactionRatherThanResolvingIt.
func TestAmbientSlotGate_NoExportedWriteResolvesThroughTheSlot(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})

	// Half one: no bracket open. An untransacted write is NOT an ambient
	// resolution.
	before := g.AmbientVersionResolutions()
	mustSetProp(t, g, "n", 1)
	mustSetProp(t, g, "n", 2)
	if n := g.AmbientVersionResolutions() - before; n != 0 {
		t.Fatalf("a write made outside any bracket counted %d ambient resolution(s); "+
			"it resolves no transaction at all and must not be conflated with one that "+
			"adopts a live transaction's record", n)
	}

	// Half two: a bracket IS open and the write does not carry it. This is the
	// resolution rmp #2320 removed from the engine's write path, and the counter
	// must see it. The bracket is EXCLUSIVE because only an exclusive bracket
	// claims the ambient slot since rmp #2947; a shared one leaves an
	// untransacted write untransacted.
	open := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.ApplyAtomically(func() error {
			close(open)
			<-release
			return nil
		})
	}()
	<-open
	before = g.AmbientVersionResolutions()
	mustSetProp(t, g, "n", 3)
	mustSetProp(t, g, "n", 4)
	direct := g.AmbientVersionResolutions() - before
	before = g.AmbientVersionResolutions()
	if err := g.AdjList().AddEdge("n", "m", 1); err != nil {
		t.Fatalf("raw AddEdge: %v", err)
	}
	got := g.AmbientVersionResolutions() - before
	close(release)
	<-done

	if direct != 0 {
		t.Fatalf("a direct write made while an unrelated bracket was open counted %d ambient "+
			"resolution(s): it joined that bracket's transaction, so the bracket's abort "+
			"would withdraw a write its caller was told had succeeded (rmp #2947)", direct)
	}
	if got != 0 {
		t.Fatalf("a raw adjacency write made while an unrelated bracket was open counted %d "+
			"ambient resolution(s): it joined that bracket's transaction, so the bracket's "+
			"abort would lose a write its caller was told had succeeded (rmp #2967)", got)
	}
}

// mustSetProp writes one node property through the direct Go API, which carries no
// transaction.
func mustSetProp(t *testing.T, g *lpg.Graph[string, float64], key string, v int64) {
	t.Helper()
	if err := g.SetNodeProperty(key, "v", lpg.Int64Value(v)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
}

// TestWritePath_OneStatementLandsOnOneCommitRecord is acceptance criterion 3, and
// it is the assertion that actually reproduces the shipped defect's mechanism.
//
// A DECOY write transaction is opened on the graph and left open, so the ambient
// slot names it for the whole statement. Any write that resolves through the slot
// adopts the decoy's commit record instead of its own — precisely what two
// overlapping brackets did — and since the decoy NEVER COMMITS, anything that
// landed on its record is invisible to every reader, forever. So the statement's
// effect being wholly visible afterwards, while the decoy is still open, is a
// direct proof that no part of it resolved through the slot.
//
// The other half is atomicity in the other direction: a snapshot taken BEFORE the
// statement must see none of it, which is what fails if a write published at an
// instant earlier than its own transaction's commit.
//
// The decoy runs on its own goroutine so the statement's bracket is not nested
// inside it, and writes nothing, so the only effect it has on the run is the slot
// it publishes.
func TestWritePath_OneStatementLandsOnOneCommitRecord(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	if r, err := eng.RunAny(context.Background(), `CREATE (:Acct {id:'a', bal:100})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	} else if err := drain(r); err != nil {
		t.Fatalf("seed drain: %v", err)
	}
	// The node's storage KEY, which the engine mints and which is not the `id`
	// property. The snapshot accessors are keyed by it, so it has to be resolved
	// rather than assumed.
	nodeKey := soleNodeKey(t, g)

	// A snapshot from BEFORE the statement. It must see the seeded state and none
	// of what follows, whatever the slot says.
	beforeSnap := g.BeginRead()
	defer g.EndRead(beforeSnap)
	if b, ok := g.ReadAt(beforeSnap).GetNodeProperty(nodeKey, "bal"); !ok {
		t.Fatal("the pre-statement snapshot cannot see the seeded property; " +
			"the test is not observing what it thinks it is")
	} else if got, _ := b.Int64(); got != 100 {
		t.Fatalf("pre-statement bal = %d, want the seeded 100", got)
	}

	decoyOpen := make(chan struct{})
	decoyRelease := make(chan struct{})
	decoyDone := make(chan struct{})
	go func() {
		defer close(decoyDone)
		_ = g.ApplyVersioned(func(lpg.WriteTx) error {
			close(decoyOpen)
			<-decoyRelease
			return nil
		})
	}()
	<-decoyOpen
	// A shared bracket does not claim the ambient slot (rmp #2947), so nothing
	// can resolve through the decoy at all; the assertions below still prove the
	// statement's own atomicity and visibility.
	if g.AmbientWriteTx() != (lpg.WriteTx{}) {
		t.Fatal("a shared write bracket claimed the ambient slot: a direct write made " +
			"anywhere in the process would now join that transaction")
	}
	// Released exactly once however this test exits, so a failure cannot leak the
	// bracket and wedge the package's remaining tests on the schema barrier.
	released := false
	releaseDecoy := func() {
		if released {
			return
		}
		released = true
		close(decoyRelease)
		<-decoyDone
	}
	defer releaseDecoy()

	const stmt = `MATCH (a:Acct {id:'a'}) SET a.bal = 55, a.tag = 'x' ` +
		`CREATE (a)-[:SELF {k:1}]->(a)`
	r, err := eng.RunAny(context.Background(), stmt, nil)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	if err := drain(r); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// A snapshot from AFTER the statement committed, taken while the decoy is
	// STILL open — so anything that landed on the decoy's record is invisible here.
	afterSnap := g.BeginRead()
	after := g.ReadAt(afterSnap)
	bal, okBal := after.GetNodeProperty(nodeKey, "bal")
	tag, okTag := after.GetNodeProperty(nodeKey, "tag")
	selfEdge := after.HasEdge(nodeKey, nodeKey)
	edgeProps := after.EdgeProperties(nodeKey, nodeKey)
	g.EndRead(afterSnap)

	if !okBal || !okTag {
		t.Fatalf("the statement's property writes are not both visible after it committed "+
			"(bal present=%v, tag present=%v); a write resolved through the ambient slot and "+
			"published on the decoy transaction's record, which never commits (rmp #2320)", okBal, okTag)
	}
	if got, _ := bal.Int64(); got != 55 {
		t.Fatalf("bal = %d, want 55", got)
	}
	if got, _ := tag.String(); got != "x" {
		t.Fatalf("tag = %q, want %q", got, "x")
	}
	if !selfEdge {
		t.Fatal("the statement's self-edge is not visible after it committed; its ADJACENCY " +
			"write did not publish with the rest of its statement — the store that had no " +
			"transaction-carrying form at all before rmp #2320")
	}
	if _, ok := edgeProps["k"]; !ok {
		t.Fatalf("the statement's columnar edge property is not visible after it committed "+
			"(got %v); that store also writes through the adjacency entry", edgeProps)
	}

	// The pre-statement snapshot must still see NONE of it, and its own seeded
	// value unchanged.
	before := g.ReadAt(beforeSnap)
	if _, ok := before.GetNodeProperty(nodeKey, "tag"); ok {
		t.Fatal("a snapshot taken before the statement observed its property write; " +
			"the statement's versions became visible at an instant earlier than its commit")
	}
	if before.HasEdge(nodeKey, nodeKey) {
		t.Fatal("a snapshot taken before the statement observed its edge")
	}
	if b, ok := before.GetNodeProperty(nodeKey, "bal"); !ok {
		t.Fatal("the pre-statement snapshot lost the seeded property")
	} else if got, _ := b.Int64(); got != 100 {
		t.Fatalf("pre-statement bal = %d after the statement, want the seeded 100", got)
	}
}

// soleNodeKey returns the storage key of the only node in g, failing the test when
// there is not exactly one. The Cypher engine mints node keys itself, so a test
// that wants to use the snapshot accessors — which are keyed by them — has to
// resolve the key rather than assume it matches a property.
func soleNodeKey(t *testing.T, g *lpg.Graph[string, float64]) string {
	t.Helper()
	var keys []string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		keys = append(keys, key)
		return true
	})
	if len(keys) != 1 {
		t.Fatalf("want exactly one node, got %d: %v", len(keys), keys)
	}
	return keys[0]
}

// drain consumes and closes a result, which for a write is what fsyncs or rolls
// back, so it must always run.
func drain(r *cypher.Result) error {
	for r.Next() { // drain the stream
	}
	if err := r.Err(); err != nil {
		_ = r.Close()
		return fmt.Errorf("stream: %w", err)
	}
	return r.Close()
}
