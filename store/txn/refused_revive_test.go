package txn_test

// refused_revive_test.go — aborting a revival restores the node's previous life
// exactly: dead and born, never unborn (ACID audit round 6, finding C2).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRefusedRevive_LeavesCommittedDeadNodeDead commits x and removes it, then
// has a store commit that would revive x refused (an explicit transaction holds
// a claim on y, which the same commit writes), and then commits an edge y→x.
// The refused revival must leave x exactly as the removal left it, so the edge
// append does not revive it: memory and recovery must agree that x is dead.
func TestRefusedRevive_LeavesCommittedDeadNodeDead(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, float64](adjlist.Config{})
	opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
	st := txn.NewStoreWithOptions[string, float64](g, w, opts)
	commit := func(f func(tx *txn.Tx[string, float64])) error {
		tx := st.Begin()
		f(tx)
		return tx.Commit()
	}
	if err := commit(func(tx *txn.Tx[string, float64]) { _ = tx.AddNode("x"); _ = tx.AddNode("y") }); err != nil {
		t.Fatal(err)
	}
	if err := commit(func(tx *txn.Tx[string, float64]) { _ = tx.RemoveNode("x") }); err != nil {
		t.Fatal(err)
	}

	etx := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(context.Background(), etx, func(wtx lpg.WriteTx) error {
		return g.Writer(wtx).SetNodeProperty("y", "p", lpg.Int64Value(1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := commit(func(tx *txn.Tx[string, float64]) {
		_ = tx.AddNode("x")
		_ = tx.SetNodeProperty("y", "q", lpg.Int64Value(2))
	}); err == nil {
		t.Fatal("the revival commit was not refused by the explicit transaction's claim on y")
	}
	g.EndVersionedTx(etx)

	xid, _ := g.AdjList().Mapper().Lookup("x")
	if !g.IsTombstoned(xid) {
		t.Fatal("the refused revival left x alive")
	}
	if err := commit(func(tx *txn.Tx[string, float64]) { _ = tx.AddEdge("y", "x", 1) }); err != nil {
		t.Fatal(err)
	}
	memAlive := !g.IsTombstoned(xid)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := res.Graph.AdjList().Mapper().Lookup("x")
	recAlive := !res.Graph.IsTombstoned(rid)
	if memAlive || recAlive {
		t.Errorf("x alive after the edge append: memory=%v recovered=%v, want dead in both", memAlive, recAlive)
	}
}

// TestRefusedFirstCreation_EdgeAppendCreatesTheNode is the case the unborn set
// exists for: a refused FIRST creation leaves x as if it had never existed, so
// a later edge append creates it, in memory exactly as in recovery.
func TestRefusedFirstCreation_EdgeAppendCreatesTheNode(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, float64](adjlist.Config{})
	opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
	st := txn.NewStoreWithOptions[string, float64](g, w, opts)
	commit := func(f func(tx *txn.Tx[string, float64])) error {
		tx := st.Begin()
		f(tx)
		return tx.Commit()
	}
	if err := commit(func(tx *txn.Tx[string, float64]) { _ = tx.AddNode("y") }); err != nil {
		t.Fatal(err)
	}
	etx := g.BeginVersionedTx()
	if err := g.ApplyInVersionedTx(context.Background(), etx, func(wtx lpg.WriteTx) error {
		return g.Writer(wtx).SetNodeProperty("y", "p", lpg.Int64Value(1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := commit(func(tx *txn.Tx[string, float64]) {
		_ = tx.AddNode("x")
		_ = tx.SetNodeProperty("y", "q", lpg.Int64Value(2))
	}); err == nil {
		t.Fatal("the creation commit was not refused by the explicit transaction's claim on y")
	}
	g.EndVersionedTx(etx)
	if err := commit(func(tx *txn.Tx[string, float64]) { _ = tx.AddEdge("y", "x", 1) }); err != nil {
		t.Fatal(err)
	}
	xid, _ := g.AdjList().Mapper().Lookup("x")
	memAlive := !g.IsTombstoned(xid)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := res.Graph.AdjList().Mapper().Lookup("x")
	recAlive := !res.Graph.IsTombstoned(rid)
	if !memAlive || !recAlive {
		t.Errorf("x after the edge append: memory alive=%v recovered alive=%v, want alive in both", memAlive, recAlive)
	}
}
