package cypher_test

// mvcc_detach_delete_inbound_rollback_test.go — rmp #2725: the INBOUND mirror of
// rmp #2694.
//
// # Why a second file for the same sentence
//
// `DETACH DELETE n` sweeps n's incident edges in TWO loops, and they do not
// share a code path:
//
//   - the OUT-edges go through the bulk [lpg.WriteView.RemoveAllEdgesFrom]
//     (cypher/exec/detach_delete.go, `RemoveAllEdgesFrom(nodeKey)`);
//   - the IN-edges go one at a time through [lpg.WriteView.RemoveEdge]
//     (the `for _, src := range incoming` loop just below it).
//
// rmp #2694 fixed the first loop: it gave the bulk removal an adjacency claim
// and made the undo journal conditional on the removal having APPLIED. The
// second loop kept the old shape — a present-state probe before the call, a
// void removal, and an inverse recorded unconditionally — so the identical
// Atomicity violation survived on every arc that pointed INTO the deleted node.
//
// # The defect
//
// Transaction A appends Z→X and does not commit. Transaction B detach-deletes
// X. B's in-edge loop calls RemoveEdge(Z, X); [Graph.removeEdgeInfo] refuses it
// on Z's adjacency claim (rmp #2300) and mutates nothing — that half was always
// correct. The adapter could not see the refusal, because RemoveEdge was void,
// so it recorded an undo inverse anyway. Then:
//
//	A rolls back → withdraws Z→X. Correct.
//	B rolls back → its inverse RE-ADDS Z→X, which it never removed.
//
// The arc now belongs to no transaction. Measured end to end by the DST
// (`sim.RunMVCCSessions` seeds 447 and 760).
//
// # Why the raw adjacency is asserted and not only the query result
//
// Same reason as the #2694 twin, and it is why the leak hid for so long: the
// resurrected arc is re-added by an ABORTED transaction, so the entry version
// chain masks it from every snapshot read while `AdjList().Size()`, `HasEdge`
// and every traversal in `search/` already see it. It becomes visible to Cypher
// too the moment any later transaction appends on the SAME source and publishes
// a real instant over the aborted one. A Cypher-only oracle therefore reports
// the graph as clean for an unbounded time after the corruption.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMVCCDetachDeleteRollback_DoesNotResurrectPeerInboundArc is the
// reproduction, over every combination of outcome and order.
//
// Committed state is one edge Y→X. Transaction A creates Z→X and does not
// commit; transaction B detach-deletes X and does not commit. Both inbound arcs
// therefore go through the per-edge loop, and only the one on Z is contended —
// so the same run also proves the fix does not break the UNCONTENDED inverse
// (Y→X must come back when B rolls back).
//
// The expected result depends ONLY on A: B loses the write-write race on Z's
// adjacency, so B changes nothing net whatever it does, and B's commit must be
// refused rather than reporting a delete it never performed.
func TestMVCCDetachDeleteRollback_DoesNotResurrectPeerInboundArc(t *testing.T) {
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
						"CREATE (:P {n:'Y'})-[:K]->(:P {n:'X'}), (:P {n:'Z'})", nil); err != nil {
						t.Fatalf("seed: %v", err)
					}

					txA, err := eng.NewSession().BeginTx(ctx)
					if err != nil {
						t.Fatalf("begin A: %v", err)
					}
					if _, err := txA.ExecAny(
						"MATCH (x:P {n:'X'}),(z:P {n:'Z'}) CREATE (z)-[:K]->(x)", nil); err != nil {
						t.Fatalf("A create: %v", err)
					}
					txB, err := eng.NewSession().BeginTx(ctx)
					if err != nil {
						t.Fatalf("begin B: %v", err)
					}
					if _, err := txB.ExecAny("MATCH (n:P {n:'X'}) DETACH DELETE n", nil); err != nil {
						t.Fatalf("B detach delete: %v", err)
					}

					finishA := func() {
						if aCommits {
							if err := txA.Commit(); err != nil {
								t.Fatalf("A, the winner of the race, was refused: %v", err)
							}
							return
						}
						if err := txA.Rollback(); err != nil {
							t.Fatalf("A rollback: %v", err)
						}
					}
					finishB := func() {
						if bCommits {
							// B lost the race on Z's adjacency; it must be told so
							// rather than reporting a delete it never performed.
							if err := txB.Commit(); err == nil {
								t.Fatal("B committed a DETACH DELETE whose in-edge " +
									"removal was refused on the adjacency: the delete " +
									"is silently lost")
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

					want := []string{`"Y"->"X"`}
					if aCommits {
						want = []string{`"Y"->"X"`, `"Z"->"X"`}
					}
					if got := detachRollbackEdges(t, eng); fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("engine edges = %v, want %v", got, want)
					}
					// The physical adjacency must agree with the query: the leak is
					// invisible to Cypher until an unrelated later append republishes
					// the entry, so a query-only assertion would pass on the defect.
					wantRaw := []string{"{Y 1}->{X 1}"}
					if aCommits {
						wantRaw = []string{"{Y 1}->{X 1}", "{Z 1}->{X 1}"}
					}
					if got := detachRollbackRawEdges(t, g); fmt.Sprint(got) != fmt.Sprint(wantRaw) {
						t.Errorf("raw adjacency = %v, want %v", got, wantRaw)
					}
					if got, want := g.AdjList().Size(), uint64(len(wantRaw)); got != want {
						t.Errorf("AdjList().Size() = %d, want %d", got, want)
					}
				})
			}
		}
	}
}
