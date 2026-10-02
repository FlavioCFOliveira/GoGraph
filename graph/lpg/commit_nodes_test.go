package lpg

// commit_nodes_test.go — [CommitNodes.Private], the rule that lets a
// [CommitApplier] leave the nodes a transaction created out of its shard locks
// (rmp #2931 audit, F3).
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// privacyProbe is a CommitApplier that records what CommitNodes reports for a
// fixed set of nodes, and asks for no lock.
type privacyProbe struct {
	ids  map[string]graph.NodeID
	seen map[string]bool
}

func (p *privacyProbe) CommitApplyShards(nodes CommitNodes) uint64 {
	for name, id := range p.ids {
		p.seen[name] = nodes.Private(id)
	}
	return 0
}
func (*privacyProbe) ApplyCommitted(*Snapshot) {}
func (*privacyProbe) Committed(uint64)         {}
func (*privacyProbe) DiscardCommitted()        {}

func TestCommitNodes_PrivateIsExactlyTheNodesBornInTheTransaction(t *testing.T) {
	g := commitTSGraph(t)
	for _, n := range []string{"old", "undone", "dead"} {
		if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode(n) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		_, _ = g.Writer(tx).RemoveNode("dead")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	probe := &privacyProbe{ids: map[string]graph.NodeID{}, seen: map[string]bool{}}
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		w := g.Writer(tx)
		if err := w.AddNode("fresh"); err != nil { // created here
			return err
		}
		if err := w.SetNodeLabel("old", "L"); err != nil { // committed before, modified here
			return err
		}
		_, _ = w.RemoveNode("undone") // deleted and restored here: the undo of a DELETE
		if err := w.AddNode("undone"); err != nil {
			return err
		}
		if err := w.AddNode("dead"); err != nil { // revived here from a committed death
			return err
		}
		for _, n := range []string{"fresh", "old", "undone", "dead"} {
			id, ok := g.AdjList().Mapper().Lookup(n)
			if !ok {
				t.Fatalf("fixture: %q has no id", n)
			}
			probe.ids[n] = id
		}
		tx.SetCommitApplier(probe)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"fresh": true, "old": false, "undone": false, "dead": true}
	for n, w := range want {
		got, ok := probe.seen[n]
		if !ok {
			t.Fatalf("the applier was not asked about %q: CommitApplyShards did not run", n)
		}
		if got != w {
			t.Errorf("Private(%q) = %v, want %v", n, got, w)
		}
	}
	if (CommitNodes{}).Private(probe.ids["fresh"]) {
		t.Error("the zero CommitNodes reported a node private")
	}
}
