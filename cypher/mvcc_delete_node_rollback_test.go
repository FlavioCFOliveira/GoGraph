package cypher_test

// mvcc_delete_node_rollback_test.go — rmp #2726: the NODE mirror of rmp #2694
// and rmp #2725.
//
// # The sentence, one entity kind over
//
// rmp #2694 gave the bulk edge removal its refusal and gated the undo journal
// on it; rmp #2725 did the same for the per-edge inbound removal. Both left the
// contract written down at [lpg.WriteView.RemoveEdge]: *a caller that journals
// an inverse MUST gate the journal entry on whether the removal was applied.*
//
// The NODE removal was never converted. [lpg.WriteView.RemoveNode] stayed void,
// so both mutator adapters pre-probed with IsTombstoned, called a void
// RemoveNode whose refusal was therefore invisible, and journalled the inverse
// off the PROBE rather than the OUTCOME.
//
// # The defect
//
// Transaction B is DOOMED — by a write-write conflict on some other node — and
// then deletes node X. [lpg.Graph.removeNodeInfo] refuses the death claim,
// strictly BEFORE any mutation (rmp #2444), so B changes nothing; that half was
// always correct. B's adapter could not see the refusal, because RemoveNode was
// void, so it recorded an undo inverse anyway. Then:
//
//	A deletes X for real and commits → X is dead, labels and properties gone.
//	B rolls back → its inverse REVIVES X, which it never deleted.
//
// # Why B is doomed on an UNRELATED node, and why that is not incidental
//
// The obvious shape — A and B racing to delete X itself — does NOT reproduce,
// measured: whichever delete runs first tombstones X in the present, so the
// loser's own `IsTombstoned` pre-probe reads `wasLive == false` and suppresses
// the inverse. The defect needs X to be STILL LIVE at the moment the refusal
// happens, and an already-doomed transaction is what produces a refusal on a
// live node. That is also the shape the DST found: the counters at detection on
// crash seed 790 read `TxRolledBack:0, TxConflicted:4, TxDoomed:2` — a
// conflict-conceded rollback, not a voluntary one.
//
// X comes back as a BARE node: alive in the present with no life record, no
// label and no properties, because A's committed statement removed them and
// [lpg.Graph.revive] restores only what the label bag still holds. The DST
// reports exactly that shape — `engine holds a non-Person node
// (name="<unnamed>", tombstoned=false existsPresent=true) the workload never
// creates` — on crash seed 790.
//
// # The SECOND symptom, which the matrix also caught
//
// The bogus revival CONSUMES the tombstone, so a legitimate inverse that runs
// after it finds nothing to revive and silently does nothing:
// [lpg.Graph.revive] only records a birth when it observes the node tombstoned.
// The A=rollback/B=rollback/B-finishes-first arm therefore LOST X altogether at
// `13467da4` — B's spurious revive cleared the tombstone, and A's real one, the
// one that should have restored the node, became a no-op. Three of these eight
// arms were red.
//
// # Why B's commit must be refused
//
// B hit a write-write conflict — on C's property store here, on whatever store
// the schedule chose in the DST — so it can no longer commit: reporting success
// would be a delete silently lost. That is pre-existing behaviour (the refusal
// is recorded on the transaction by [writeCtx.conflictErr] however the caller
// treats the return) and the matrix below pins it.

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMVCCDeleteNodeRollback_DoesNotResurrectPeerDeletedNode is the
// reproduction, over every combination of outcome and finish order.
//
// Committed state is nodes X, K and C, with an arc K→X so the DETACH half runs
// too. B is doomed on C and then deletes X, which refuses; A deletes X for real.
// Only A can change anything, so the expected end state depends ONLY on A — and
// B's rollback must not put X back.
//
// The UNCONTENDED inverse is covered by the A=rollback arms: A's own rollback
// must restore X whole, label and property included, or the fix has traded this
// defect for its mirror (a legitimate inverse skipped).
func TestMVCCDeleteNodeRollback_DoesNotResurrectPeerDeletedNode(t *testing.T) {
	ctx := context.Background()
	for _, aCommits := range []bool{false, true} {
		for _, bCommits := range []bool{false, true} {
			for _, aFirst := range []bool{true, false} {
				name := fmt.Sprintf("A=%s/B=%s/%s",
					detachRollbackOutcome(aCommits), detachRollbackOutcome(bCommits),
					map[bool]string{true: "A-finishes-first", false: "B-finishes-first"}[aFirst])
				t.Run(name, func(t *testing.T) {
					g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
					eng := cypher.NewEngine(g)
					if _, err := eng.RunInTxAny(ctx,
						"CREATE (:P {n:'K'})-[:R]->(:P {n:'X'}), (:P {n:'C'})", nil); err != nil {
						t.Fatalf("seed: %v", err)
					}
					// Captured BEFORE the deletes: the substrate assertion needs
					// X's NodeID, and the winner's commit removes the `n` property
					// that identifies it.
					idX := deleteNodeRollbackIDOf(t, g, "X")
					_, removedBefore, _, _ := g.SideEffectCounters()

					txA, err := eng.NewSession().BeginTx(ctx)
					if err != nil {
						t.Fatalf("begin A: %v", err)
					}
					txB, err := eng.NewSession().BeginTx(ctx)
					if err != nil {
						t.Fatalf("begin B: %v", err)
					}
					// B is doomed on an UNRELATED node, which is what makes X still
					// LIVE when B's delete runs — the difference that matters. Had
					// the two raced on X itself, A's delete would have tombstoned it
					// first and B's own IsTombstoned pre-probe would have suppressed
					// the inverse, hiding the defect.
					if _, err := txA.ExecAny("MATCH (c:P {n:'C'}) SET c.v = 1", nil); err != nil {
						t.Fatalf("A set: %v", err)
					}
					// Expected to conflict; the transaction is doomed either way and
					// the engine may report it here or at commit.
					_, _ = txB.ExecAny("MATCH (c:P {n:'C'}) SET c.v = 2", nil)
					// The refused delete: X is live, so B's pre-probe says so, and
					// removeNodeInfo refuses the death claim before mutating anything.
					_, _ = txB.ExecAny("MATCH (n:P {n:'X'}) DETACH DELETE n", nil)
					// A's delete is the real one.
					if _, err := txA.ExecAny("MATCH (n:P {n:'X'}) DETACH DELETE n", nil); err != nil {
						t.Fatalf("A detach delete: %v", err)
					}

					finishA := func() {
						if aCommits {
							if err := txA.Commit(); err != nil {
								t.Fatalf("A, which conflicts with nobody, was refused: %v", err)
							}
							return
						}
						if err := txA.Rollback(); err != nil {
							t.Fatalf("A rollback: %v", err)
						}
					}
					finishB := func() {
						if bCommits {
							// B is doomed on C and its delete of X was refused; it
							// must be told so rather than reporting a delete it
							// never performed.
							if err := txB.Commit(); err == nil {
								t.Fatal("B committed after a write-write conflict and a " +
									"DETACH DELETE its own doom refused: the delete is " +
									"silently lost")
							}
							return
						}
						if err := txB.Rollback(); err != nil {
							t.Fatalf("B rollback: %v", err)
						}
					}
					if aFirst {
						finishA()
						finishB()
					} else {
						finishB()
						finishA()
					}

					// The whole-graph scan, which is the DST's own probe: every
					// live node must still be a :P carrying its name.
					wantNames := []string{`"C"`, `"K"`}
					if !aCommits {
						wantNames = []string{`"C"`, `"K"`, `"X"`}
					}
					if got := deleteNodeRollbackNames(t, eng); fmt.Sprint(got) != fmt.Sprint(wantNames) {
						t.Errorf("live node names = %v, want %v", got, wantNames)
					}
					// A live node the label scan does not serve is the exact shape
					// rmp #2726 reports: alive, unlabelled, unnamed.
					if got := deleteNodeRollbackUnlabelled(t, eng); len(got) != 0 {
						t.Errorf("engine holds %v live non-:P node(s); a rolled-back "+
							"refusal resurrected a node it never deleted", got)
					}
					// The arc must not outlive its endpoint. The DST reported a
					// dangling arc alongside the bare node on seed 790, and the two
					// travel together: an arc into a node the graph has deleted is
					// unservable while the node stays dead and becomes visible the
					// moment a bogus inverse revives it.
					wantEdges := []string{}
					if !aCommits {
						wantEdges = []string{`"K"->"X"`}
					}
					if got := detachRollbackEdges(t, eng); fmt.Sprint(got) != fmt.Sprint(wantEdges) {
						t.Errorf("engine edges = %v, want %v", got, wantEdges)
					}
					// The substrate must agree with the query. IsTombstoned is the
					// present-time authority on existence and is what the resurrection
					// actually flips, so a query-only assertion could pass on a
					// half-repaired graph.
					if got, wantDead := g.IsTombstoned(idX), aCommits; got != wantDead {
						t.Errorf("IsTombstoned(X) = %v, want %v", got, wantDead)
					}
					// THE SIDE-EFFECT COUNTER GATES ON THE OUTCOME TOO, and it has
					// to gate WITH the journal: the inverse's DecrNodesRemoved is
					// what reverses this increment, so a counter raised for a
					// retirement whose inverse is (correctly) not journalled stays
					// raised for good. This is the graph-scoped counter the
					// openCypher TCK side-effect comparator reads, so the drift
					// would be observable as a wrong -nodes on a later statement.
					//
					// A=commit is also the NON-VACUITY control for this assertion:
					// exactly one node really was removed, so a build that never
					// counted would fail here rather than passing silently.
					_, removedAfter, _, _ := g.SideEffectCounters()
					wantRemoved := uint64(0)
					if aCommits {
						wantRemoved = 1
					}
					if got := removedAfter - removedBefore; got != wantRemoved {
						t.Errorf("SideEffectCounters nodesRemoved delta = %d, want %d",
							got, wantRemoved)
					}
				})
			}
		}
	}
}

// deleteNodeRollbackNames returns the sorted names of every live node the
// unlabelled whole-graph scan serves.
func deleteNodeRollbackNames(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), "MATCH (n) RETURN n.n AS n", nil)
	if err != nil {
		t.Fatalf("name query: %v", err)
	}
	defer func() { _ = res.Close() }()
	out := []string{}
	for res.Next() {
		out = append(out, fmt.Sprintf("%v", res.Record()["n"]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("name query drain: %v", err)
	}
	slices.Sort(out)
	return out
}

// deleteNodeRollbackUnlabelled returns the ids of every live node the label
// index does not serve — the remnant shape rmp #2726 reports.
func deleteNodeRollbackUnlabelled(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(),
		"MATCH (n) WHERE NOT n:P RETURN id(n) AS id", nil)
	if err != nil {
		t.Fatalf("remnant query: %v", err)
	}
	defer func() { _ = res.Close() }()
	out := []string{}
	for res.Next() {
		out = append(out, fmt.Sprintf("%v", res.Record()["id"]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("remnant query drain: %v", err)
	}
	return out
}

// deleteNodeRollbackIDOf returns the NodeID whose `n` property is name. The
// Cypher engine interns its own node keys, so the mapper cannot be asked for a
// property value.
func deleteNodeRollbackIDOf(t *testing.T, g *lpg.Graph[string, float64], name string) graph.NodeID {
	t.Helper()
	a := g.AdjList()
	for id := graph.NodeID(0); id <= a.MaxNodeID(); id++ {
		v, ok := g.NodePropertyByID(id, "n")
		if !ok {
			continue
		}
		if got, isStr := v.String(); isStr && got == name {
			return id
		}
	}
	t.Fatalf("no node carries n=%q", name)
	return 0
}
