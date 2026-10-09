package cypher_test

// direct_write_conflict_test.go — rmp #2947: a direct Go-API write against a
// peer transaction's UNCOMMITTED write on the same node.
//
// Before the fix every case below acknowledged the direct write with nil and
// then lost it when the peer rolled back: the peer's undo restored its
// pre-image over a write the caller had been told succeeded (a dirty write).
// The contract now is that the direct write refuses with
// lpg.ErrDirectWriteConflict, changes nothing, and succeeds once the peer
// commits or rolls back. Each case asserts all three, and the exact final state
// on both peer outcomes.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

type directGraph = lpg.Graph[string, float64]

// directFixture builds n1:L {id:'1', k:'v'} and an unconnected n2.
func directFixture(t *testing.T) (*directGraph, *cypher.Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	for _, err := range []error{
		g.AddNode("n1"),
		g.AddNode("n2"),
		g.SetNodeProperty("n1", "id", lpg.StringValue("1")),
		g.SetNodeProperty("n1", "k", lpg.StringValue("v")),
		g.SetNodeLabel("n1", "L"),
	} {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return g, cypher.NewEngine(g)
}

func execTx(t *testing.T, tx *cypher.ExplicitTx, q string) {
	t.Helper()
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	_ = res.Close()
}

// nodeState renders everything a case can change on n1, so "the refused call
// changed nothing" and "the final state is exact" are both one comparison.
func nodeState(g *directGraph) string {
	k, hasK := g.NodeProperties("n1")["k"]
	labels := g.NodeLabels("n1")
	slices.Sort(labels)
	id, _ := g.AdjList().Mapper().Lookup("n1")
	ks, _ := k.String()
	return fmt.Sprintf("k=%v/%v labels=%v removed=%v", ks, hasK, labels, g.IsTombstoned(id))
}

func requireDirectConflict(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, lpg.ErrDirectWriteConflict) {
		t.Fatalf("direct write against a pending peer returned %v, want ErrDirectWriteConflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) || !c.ConcurrentWriter() {
		t.Fatalf("refusal %v does not carry an in-flight *mvcc.Conflict", err)
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("refusal %v does not match mvcc.ErrSerializationConflict", err)
	}
	if errors.Is(err, lpg.ErrIndexedRawWrite) {
		t.Fatalf("refusal %v is indistinguishable from ErrIndexedRawWrite", err)
	}
}

func TestDirectWrite_RefusesAPendingPeerWrite(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		direct func(g *directGraph) error
		// wantCommit and wantRollback are the exact final states after the peer
		// commits or rolls back and the direct write is retried.
		wantCommit, wantRollback string
	}{
		{
			name:         "property delete vs pending remove",
			peer:         `MATCH (n {id:'1'}) REMOVE n.k`,
			direct:       func(g *directGraph) error { return g.DelNodeProperty("n1", "k") },
			wantCommit:   "k=/false labels=[L] removed=false",
			wantRollback: "k=/false labels=[L] removed=false",
		},
		{
			name:         "property delete vs pending set",
			peer:         `MATCH (n {id:'1'}) SET n.k = 'peer'`,
			direct:       func(g *directGraph) error { return g.DelNodeProperty("n1", "k") },
			wantCommit:   "k=/false labels=[L] removed=false",
			wantRollback: "k=/false labels=[L] removed=false",
		},
		{
			name: "property set vs pending set",
			peer: `MATCH (n {id:'1'}) SET n.k = 'peer'`,
			direct: func(g *directGraph) error {
				return g.SetNodeProperty("n1", "k", lpg.StringValue("direct"))
			},
			wantCommit:   "k=direct/true labels=[L] removed=false",
			wantRollback: "k=direct/true labels=[L] removed=false",
		},
		{
			name:         "label remove vs pending remove",
			peer:         `MATCH (n {id:'1'}) REMOVE n:L`,
			direct:       func(g *directGraph) error { return g.RemoveNodeLabel("n1", "L") },
			wantCommit:   "k=v/true labels=[] removed=false",
			wantRollback: "k=v/true labels=[] removed=false",
		},
		{
			name:         "label set vs pending set",
			peer:         `MATCH (n {id:'1'}) SET n:M`,
			direct:       func(g *directGraph) error { return g.SetNodeLabel("n1", "M") },
			wantCommit:   "k=v/true labels=[L M] removed=false",
			wantRollback: "k=v/true labels=[L M] removed=false",
		},
		{
			name:         "node removal vs pending property set",
			peer:         `MATCH (n {id:'1'}) SET n.k = 'peer'`,
			direct:       func(g *directGraph) error { return g.RemoveNode("n1") },
			wantCommit:   "k=peer/true labels=[L] removed=true",
			wantRollback: "k=v/true labels=[L] removed=true",
		},
		{
			name:         "node removal vs pending relationship create",
			peer:         `MATCH (a {id:'1'}) CREATE (a)-[:R]->(:Other)`,
			direct:       func(g *directGraph) error { return g.RemoveNode("n1") },
			wantCommit:   "k=v/true labels=[L] removed=true",
			wantRollback: "k=v/true labels=[L] removed=true",
		},
		{
			name:         "property set vs pending node delete",
			peer:         `MATCH (n {id:'1'}) DETACH DELETE n`,
			direct:       func(g *directGraph) error { return g.SetNodeProperty("n1", "k", lpg.StringValue("direct")) },
			wantCommit:   "k=direct/true labels=[] removed=true",
			wantRollback: "k=direct/true labels=[L] removed=false",
		},
	}
	for _, c := range cases {
		for _, commit := range []bool{true, false} {
			outcome := "rollback"
			if commit {
				outcome = "commit"
			}
			t.Run(c.name+"/"+outcome, func(t *testing.T) {
				g, eng := directFixture(t)
				tx, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				execTx(t, tx, c.peer)
				before := nodeState(g)

				requireDirectConflict(t, c.direct(g))
				if got := nodeState(g); got != before {
					t.Fatalf("the refused direct write changed state: %s -> %s", before, got)
				}

				if commit {
					err = tx.Commit()
				} else {
					err = tx.Rollback()
				}
				if err != nil {
					t.Fatalf("peer %s: %v", outcome, err)
				}
				// Retryable: once the peer has finished, the same call succeeds.
				// A rolled-back peer's versions are withdrawn by the vacuum, which
				// a reclaim drives to completion here.
				g.ReclaimNow()
				if err := c.direct(g); err != nil {
					t.Fatalf("retry after the peer's %s: %v", outcome, err)
				}
				want := c.wantRollback
				if commit {
					want = c.wantCommit
				}
				if got := nodeState(g); got != want {
					t.Fatalf("final state after peer %s and the retried direct write = %s, want %s",
						outcome, got, want)
				}
			})
		}
	}
}

// TestDirectWrite_UntouchedObjectIsNotCapturedByAnOpenTransaction pins the root
// cause behind rmp #2947: an explicit transaction used to claim the graph's
// ambient stamp slot for its whole life, so a direct write ANYWHERE in the
// process — on an object the transaction never touched — was stamped with that
// transaction's record. It was invisible to new readers until the transaction
// ended, although the caller had been told it committed. A direct write is
// committed the instant it is made, so it must be visible at once and survive
// the unrelated transaction's outcome either way.
func TestDirectWrite_UntouchedObjectIsNotCapturedByAnOpenTransaction(t *testing.T) {
	for _, commit := range []bool{true, false} {
		outcome := "rollback"
		if commit {
			outcome = "commit"
		}
		t.Run(outcome, func(t *testing.T) {
			g, eng := directFixture(t)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			execTx(t, tx, `MATCH (n {id:'1'}) SET n.k = 'peer'`)

			if err := g.SetNodeProperty("n2", "x", lpg.StringValue("direct")); err != nil {
				t.Fatalf("direct write on an untouched node: %v", err)
			}
			if err := g.SetNodeLabel("n2", "D"); err != nil {
				t.Fatalf("direct label on an untouched node: %v", err)
			}
			snap := g.BeginRead()
			_, sawProp := g.ReadAt(snap).NodeProperties("n2")["x"]
			sawLabel := g.ReadAt(snap).HasNodeLabel("n2", "D")
			g.EndRead(snap)
			if !sawProp || !sawLabel {
				t.Fatalf("a reader that began after the direct write saw prop=%v label=%v: the "+
					"write joined the open, unrelated transaction", sawProp, sawLabel)
			}

			if commit {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatalf("peer %s: %v", outcome, err)
			}
			g.ReclaimNow()
			snap = g.BeginRead()
			_, sawProp = g.ReadAt(snap).NodeProperties("n2")["x"]
			sawLabel = g.ReadAt(snap).HasNodeLabel("n2", "D")
			g.EndRead(snap)
			if !sawProp || !sawLabel {
				t.Fatalf("after the unrelated transaction's %s a reader saw prop=%v label=%v",
					outcome, sawProp, sawLabel)
			}
		})
	}
}

// edgeFixture builds n1 {id:'1'} -[:R {k:'v'}]-> n2 {id:'2'} through the
// direct API, on a directed (reverse-indexed) or an undirected graph.
func edgeFixture(t *testing.T, directed bool) (*directGraph, *cypher.Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	for _, err := range []error{
		g.AddNode("n1"),
		g.AddNode("n2"),
		g.SetNodeProperty("n1", "id", lpg.StringValue("1")),
		g.SetNodeProperty("n2", "id", lpg.StringValue("2")),
		g.AddEdge("n1", "n2", 1),
		g.SetEdgeLabel("n1", "n2", "R"),
		g.SetEdgeProperty("n1", "n2", "k", lpg.StringValue("v")),
	} {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return g, cypher.NewEngine(g)
}

// edgeState renders what an edge case can change on n1→n2.
func edgeState(g *directGraph) string {
	k, hasK := g.EdgeProperties("n1", "n2")["k"]
	ks, _ := k.String()
	return fmt.Sprintf("edge=%v k=%v/%v", g.AdjList().HasEdge("n1", "n2"), ks, hasK)
}

// TestDirectWrite_EdgeRefusesAPendingPeerWrite is the probe14 edge matrix
// (rmp #2947): property set and delete and edge removal, against a peer
// transaction's pending write on the same relationship, on a directed and an
// undirected graph. Each refuses while the peer is in flight, changes nothing,
// and succeeds once the peer commits or rolls back.
func TestDirectWrite_EdgeRefusesAPendingPeerWrite(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		direct func(g *directGraph) error
		// Exact final states after the peer's outcome and the retried write.
		wantCommit, wantRollback string
	}{
		{
			name: "property set vs pending set",
			peer: `MATCH (:N1 {id:'1'})-[r]->(:N2 {id:'2'}) SET r.k = 'peer'`,
			direct: func(g *directGraph) error {
				return g.SetEdgeProperty("n1", "n2", "k", lpg.StringValue("direct"))
			},
			wantCommit:   "edge=true k=direct/true",
			wantRollback: "edge=true k=direct/true",
		},
		{
			name:         "property delete vs pending remove",
			peer:         `MATCH (:N1 {id:'1'})-[r]->(:N2 {id:'2'}) REMOVE r.k`,
			direct:       func(g *directGraph) error { return g.DelEdgeProperty("n1", "n2", "k") },
			wantCommit:   "edge=true k=/false",
			wantRollback: "edge=true k=/false",
		},
		{
			name:         "removal vs pending property set",
			peer:         `MATCH (:N1 {id:'1'})-[r]->(:N2 {id:'2'}) SET r.k = 'peer'`,
			direct:       func(g *directGraph) error { return g.RemoveEdge("n1", "n2") },
			wantCommit:   "edge=false k=/false",
			wantRollback: "edge=false k=/false",
		},
	}
	for _, directed := range []bool{true, false} {
		shape := "undirected"
		if directed {
			shape = "directed"
		}
		for _, c := range cases {
			for _, commit := range []bool{true, false} {
				outcome := "rollback"
				if commit {
					outcome = "commit"
				}
				t.Run(shape+"/"+c.name+"/"+outcome, func(t *testing.T) {
					g, eng := edgeFixture(t, directed)
					for _, err := range []error{g.SetNodeLabel("n1", "N1"), g.SetNodeLabel("n2", "N2")} {
						if err != nil {
							t.Fatal(err)
						}
					}
					tx, err := eng.BeginTx(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					execTx(t, tx, c.peer)
					before := edgeState(g)

					requireDirectConflict(t, c.direct(g))
					if got := edgeState(g); got != before {
						t.Fatalf("the refused direct write changed state: %s -> %s", before, got)
					}

					if commit {
						err = tx.Commit()
					} else {
						err = tx.Rollback()
					}
					if err != nil {
						t.Fatalf("peer %s: %v", outcome, err)
					}
					g.ReclaimNow()
					if err := c.direct(g); err != nil {
						t.Fatalf("retry after the peer's %s: %v", outcome, err)
					}
					want := c.wantRollback
					if commit {
						want = c.wantCommit
					}
					if got := edgeState(g); got != want {
						t.Fatalf("final state after peer %s and the retried direct write = %s, want %s",
							outcome, got, want)
					}
				})
			}
		}
	}
}

// TestDirectWrite_ZeroViewReviveRefusesAPendingDelete is audit finding F6: a
// revival through a WriteView over the zero WriteTx ran no check at all, so it
// cleared the tombstone of a node a peer was deleting, and once the peer
// committed the node's existence record said dead while the tombstone set said
// alive. It now runs as a direct write (rmp #2947): refused, changing nothing,
// while the delete is pending, and the delete's outcome stands either way.
func TestDirectWrite_ZeroViewReviveRefusesAPendingDelete(t *testing.T) {
	for _, commit := range []bool{true, false} {
		outcome := "rollback"
		if commit {
			outcome = "commit"
		}
		t.Run(outcome, func(t *testing.T) {
			g, eng := directFixture(t)
			id, _ := g.AdjList().Mapper().Lookup("n1")
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			execTx(t, tx, `MATCH (n:L) DETACH DELETE n`)
			if !g.IsTombstonedStored(id) {
				t.Fatal("setup: the pending delete has not retired the node")
			}

			rerr := zeroViewRevive(g, "n1")
			if !g.IsTombstonedStored(id) {
				t.Error("a zero-view revival undid a pending delete")
			}
			if rerr != nil {
				requireDirectConflict(t, rerr)
			}

			if commit {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatalf("peer %s: %v", outcome, err)
			}
			g.ReclaimNow()
			snap := g.BeginRead()
			exists := g.NodeExistsAsOf(id, snap)
			g.EndRead(snap)
			if exists == g.IsTombstoned(id) {
				t.Fatalf("the existence record (exists=%v) and the tombstone set (tombstoned=%v) disagree",
					exists, g.IsTombstoned(id))
			}
			if exists == commit {
				t.Fatalf("after the delete's %s the node exists=%v", outcome, exists)
			}
			if !commit && !slices.Contains(g.NodeLabels("n1"), "L") {
				t.Fatalf("after the delete's rollback the node lost its label: %v", g.NodeLabels("n1"))
			}
		})
	}
}
