package lpg

// commit_applier_panic_test.go — how a panicking [CommitApplier] settles the
// transaction it was applying (rmp #2931 re-audit, N2).
//
// Layer: short.
//
// The library does not recover the panic; this harness does, then checks the
// settlement lpg owes on the way out:
//
//   - WAL shape — the commit instant was allocated before the fsync
//     ([Graph.AllocateCommitTS]), so the WAL record is durable and recovery will
//     replay it as committed. The commit must be PUBLISHED, without the
//     applier's work, so memory agrees with the log; aborting it would split
//     durability from atomicity.
//   - In-memory shape — nothing was allocated and nothing is durable. The record
//     must be ABORTED and handed to the reclaimer, so its versions are invisible
//     and the nodes it wrote are writable again.
//
// Either way the frontier keeps moving for later commits.

import (
	"context"
	"testing"
)

// panickingApplier panics in the method named by where.
type panickingApplier struct{ where string }

func (a panickingApplier) CommitApplyShards(CommitNodes) uint64 {
	if a.where == "shards" {
		panic("test: CommitApplyShards")
	}
	return 1
}

func (a panickingApplier) ApplyCommitted(*Snapshot) {
	if a.where == "apply" {
		panic("test: ApplyCommitted")
	}
}

func (panickingApplier) Committed(uint64)  {}
func (panickingApplier) DiscardCommitted() {}

// endPanicking closes tx with a panicking applier registered and reports whether
// the panic surfaced.
func endPanicking(g *Graph[string, float64], tx WriteTx, where string) (panicked bool) {
	tx.SetCommitApplier(panickingApplier{where: where})
	defer func() { panicked = recover() != nil }()
	g.EndVersionedTx(tx)
	return false
}

func TestCommitApplier_PanicSettlesTheTransaction(t *testing.T) {
	for _, wal := range []bool{true, false} {
		for _, where := range []string{"apply", "shards"} {
			name := "mem/" + where
			if wal {
				name = "wal/" + where
			}
			t.Run(name, func(t *testing.T) {
				g := commitTSGraph(t)
				ctx := context.Background()
				// A node committed before, which the panicking transaction relabels.
				if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("n") }); err != nil {
					t.Fatal(err)
				}

				tx := g.BeginVersionedTx()
				var allocated uint64
				if err := g.ApplyInVersionedTx(ctx, tx, func(tx WriteTx) error {
					w := g.Writer(tx)
					if err := w.SetNodeLabel("n", "P"); err != nil {
						return err
					}
					if wal {
						allocated = g.AllocateCommitTS(tx) // the fsync follows
					}
					return nil
				}); err != nil {
					t.Fatalf("ApplyInVersionedTx: %v", err)
				}
				if !endPanicking(g, tx, where) {
					t.Fatal("fixture: the applier did not panic through EndVersionedTx")
				}

				if n := g.MVCCStats().InFlightCommits; n != 0 {
					t.Fatalf("InFlightCommits = %d after the panicking commit, want 0", n)
				}
				if n := g.MVCCStats().ActiveSnapshots; n != 0 {
					t.Fatalf("ActiveSnapshots = %d after the panicking commit, want 0: its horizon slot "+
						"leaked and pins the reclamation watermark (rmp #2936 audit, L2)", n)
				}
				snap := g.BeginRead()
				relabelled := g.ReadAt(snap).HasNodeLabel("n", "P")
				g.EndRead(snap)
				if wal {
					if !relabelled {
						t.Fatal("the durable commit is not visible: memory disagrees with the WAL, which " +
							"recovery replays as committed")
					}
					if now := g.MVCCStats().Now; now < allocated {
						t.Fatalf("frontier = %d, below the durable commit's instant %d", now, allocated)
					}
				} else if relabelled {
					t.Fatal("the in-memory commit whose applier panicked is visible: it was not aborted")
				}

				// Reclaimed and writable again: a later transaction writes the same
				// node, and its commit becomes visible.
				g.ReclaimNow()
				var later uint64
				if err := g.ApplyVersioned(func(tx WriteTx) error {
					if err := g.Writer(tx).SetNodeLabel("n", "Q"); err != nil {
						return err
					}
					later = g.AllocateCommitTS(tx)
					return nil
				}); err != nil {
					t.Fatalf("a later write to the node failed: %v — the panicking transaction's record "+
						"still holds it", err)
				}
				snap = g.BeginRead()
				defer g.EndRead(snap)
				if !g.ReadAt(snap).HasNodeLabel("n", "Q") {
					t.Fatal("the later commit is not visible")
				}
				if now := g.MVCCStats().Now; now < later {
					t.Fatalf("frontier = %d after a later commit at %d: stalled", now, later)
				}
			})
		}
	}
}

// TestCommitApplier_PanicOnTheAutocommitBracketReleasesItsSlot is the same
// horizon check on the autocommit bracket ([Graph.ApplyVersioned]), whose unwind
// also runs through endWrite (rmp #2936 audit, L2).
func TestCommitApplier_PanicOnTheAutocommitBracketReleasesItsSlot(t *testing.T) {
	g := commitTSGraph(t)
	panicked := func() (p bool) {
		defer func() { p = recover() != nil }()
		_ = g.ApplyVersioned(func(tx WriteTx) error {
			if err := g.Writer(tx).AddNode("n"); err != nil {
				return err
			}
			tx.SetCommitApplier(panickingApplier{where: "apply"})
			return nil
		})
		return false
	}()
	if !panicked {
		t.Fatal("fixture: the applier did not panic")
	}
	if n := g.MVCCStats().ActiveSnapshots; n != 0 {
		t.Fatalf("ActiveSnapshots = %d after the panicking autocommit, want 0: its horizon slot leaked", n)
	}
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("m") }); err != nil {
		t.Fatalf("a later autocommit failed: %v", err)
	}
}
