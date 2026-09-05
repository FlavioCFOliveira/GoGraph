package cypher_test

// mvcc_detach_delete_rollback_test.go — rmp #2694: an uncommitted DETACH DELETE
// must not disturb a concurrent transaction's edge, whichever way the two
// transactions end and in whatever order they end.
//
// # The defect
//
// `DETACH DELETE n` removes n's out-edges through the bulk adjacency path, and
// that path did two things it may not do:
//
//  1. It took no write-write claim on n's adjacency, so it stepped over a
//     concurrent transaction's in-flight append and WIPED the whole slot —
//     [lpg.AdjList.RemoveAllEdgesFrom] publishes a nil entry, so the peer's
//     uncommitted arc went with it. The peer's own rollback then had nothing
//     left to withdraw.
//  2. It journalled one undo inverse per out-edge BEFORE the removal and
//     unconditionally, so a refused removal still left inverses behind — and an
//     inverse re-adds its arc.
//
// Together they made a rolled-back edge SURVIVE both rollbacks. Found by the DST
// (`sim.RunMVCCSessions` seed 29 at ticks 60 and 61,
// `[ACID_CONSISTENCY] edge-count mismatch: oracle=3 engine=4`), which is an
// Atomicity violation: work from a transaction that was told it failed stayed in
// the graph.
//
// # Why the raw adjacency is asserted and not only the query result
//
// Half 1 alone leaves the query answer CORRECT and the physical adjacency wrong:
// the resurrected arc is re-added by an aborted transaction, so the entry
// version chain masks it from every snapshot read while
// `AdjList().Size()`, `HasEdge` and every direct traversal in `search/` still
// see it. A Cypher-only oracle passes on that build. Both surfaces are therefore
// asserted, and each half of the fix was reverted in turn to confirm each is
// load-bearing.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// detachRollbackEdges returns every edge the engine reports, as "src->dst" over
// the `n` property, sorted.
func detachRollbackEdges(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(),
		"MATCH (a)-[r]->(b) RETURN a.n AS x, b.n AS y", nil)
	if err != nil {
		t.Fatalf("edge query: %v", err)
	}
	defer func() { _ = res.Close() }()
	out := []string{}
	for res.Next() {
		rec := res.Record()
		out = append(out, fmt.Sprintf("%v->%v", rec["x"], rec["y"]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("edge query: %v", err)
	}
	sort.Strings(out)
	return out
}

// detachRollbackRawEdges reads the PHYSICAL adjacency, bypassing every query and
// snapshot path — this is what `search/` traverses and what `AdjList().Size()`
// counts.
func detachRollbackRawEdges(t *testing.T, g *lpg.Graph[string, float64]) []string {
	t.Helper()
	a := g.AdjList()
	out := []string{}
	for id := graph.NodeID(0); id <= a.MaxNodeID(); id++ {
		nbs, _ := a.LoadEntry(id)
		for _, dst := range nbs {
			out = append(out, fmt.Sprintf("%s->%s",
				detachRollbackName(g, id), detachRollbackName(g, dst)))
		}
	}
	sort.Strings(out)
	return out
}

func detachRollbackName(g *lpg.Graph[string, float64], id graph.NodeID) string {
	v, ok := g.NodePropertyByID(id, "n")
	if !ok {
		return "?"
	}
	return fmt.Sprintf("%v", v)
}

// TestMVCCDetachDeleteRollback_DoesNotResurrectPeerArc is the reproduction.
//
// Committed state is one edge X→Y. Transaction A creates X→Z and does not
// commit; transaction B detach-deletes X and does not commit. The two are then
// finished in every combination of outcome and order.
//
// The expected result depends ONLY on A: B always loses the write-write race on
// X's adjacency, so B changes nothing whatever it does — and B's commit must be
// refused, because a DETACH DELETE that silently did nothing and reported
// success would be a lost update.
func TestMVCCDetachDeleteRollback_DoesNotResurrectPeerArc(t *testing.T) {
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
						"CREATE (x:P {n:'X'})-[:K]->(:P {n:'Y'}), (:P {n:'Z'})", nil); err != nil {
						t.Fatalf("seed: %v", err)
					}

					txA, err := eng.NewSession().BeginTx(ctx)
					if err != nil {
						t.Fatalf("begin A: %v", err)
					}
					if _, err := txA.ExecAny(
						"MATCH (x:P {n:'X'}),(z:P {n:'Z'}) CREATE (x)-[:K]->(z)", nil); err != nil {
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
							// B lost the race on X's adjacency; it must be told so
							// rather than reporting a delete it never performed.
							if err := txB.Commit(); err == nil {
								t.Fatal("B committed a DETACH DELETE that was refused " +
									"on the adjacency: the delete is silently lost")
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

					want := []string{`"X"->"Y"`}
					if aCommits {
						want = []string{`"X"->"Y"`, `"X"->"Z"`}
					}
					if got := detachRollbackEdges(t, eng); fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("engine edges = %v, want %v", got, want)
					}
					// The physical adjacency must agree with the query. Content,
					// never the count alone: a count-only oracle passes while one
					// edge is lost and another duplicated.
					wantRaw := []string{"{X 1}->{Y 1}"}
					if aCommits {
						wantRaw = []string{"{X 1}->{Y 1}", "{X 1}->{Z 1}"}
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

func detachRollbackOutcome(commit bool) string {
	if commit {
		return "commit"
	}
	return "rollback"
}
