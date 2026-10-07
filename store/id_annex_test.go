package store_test

// id_annex_test.go — WAL v2 step 3 (rmp #3021 A, docs/design-wal-v2.md §1.2, §1.3,
// §5.5): node ids are identical across a restart.
//
// Layer: short.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// sameShardKeys returns n distinct keys in one mapper shard — the adversarial
// placement graph/security_shard_amplification_test.go uses — found by probing a
// throwaway mapper.
func sameShardKeys(t *testing.T, n int) []string {
	t.Helper()
	m := graph.NewMapper[string]()
	byShard := map[uint64][]string{}
	for i := 0; i < 1_000_000; i++ {
		k := fmt.Sprintf("annex-%07d", i)
		s := graph.MapperShardOf(m.Intern(k))
		byShard[s] = append(byShard[s], k)
		if len(byShard[s]) == n {
			return byShard[s]
		}
	}
	t.Fatalf("no shard received %d probe keys", n)
	return nil
}

// eagerTx is a transaction applied eagerly through lpg and committed WAL-only,
// the shape the Cypher engine commits in.
type eagerTx struct {
	g   *lpg.Graph[string, float64]
	wtx lpg.WriteTx
	tx  *txn.Tx[string, float64]
}

func beginEager(o *store.Opened[string, float64]) *eagerTx {
	return &eagerTx{g: o.Graph(), wtx: o.Graph().BeginVersionedTx(), tx: o.Store().Begin()}
}

func (e *eagerTx) addNode(t *testing.T, k string) {
	t.Helper()
	if err := e.g.Writer(e.wtx).AddNode(k); err != nil {
		t.Fatalf("lpg AddNode(%q): %v", k, err)
	}
	if err := e.tx.AddNode(k); err != nil {
		t.Fatalf("txn AddNode(%q): %v", k, err)
	}
}

func (e *eagerTx) commit(t *testing.T) {
	t.Helper()
	e.tx.AttachWriteTx(e.wtx)
	if err := e.tx.CommitWALOnly(e.g.AllocateCommitTS(e.wtx)); err != nil {
		t.Fatalf("CommitWALOnly: %v", err)
	}
	e.g.EndVersionedTx(e.wtx)
}

func (e *eagerTx) rollback() {
	e.wtx.Abandon()
	e.g.EndVersionedTx(e.wtx)
	_ = e.tx.Rollback()
}

func lookupID(t *testing.T, g *lpg.Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(k)
	if !ok {
		t.Fatalf("key %q is not interned", k)
	}
	return id
}

func reopen(t *testing.T, o *store.Opened[string, float64], dir string) *store.Opened[string, float64] {
	t.Helper()
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return r
}

// TestAnnex_CommitOrderKeepsIDs is the commit-order case: T1 interns K1 and T2
// interns K2 just after it in one shard, and T2 commits first. Before step 3 the
// replay interned K2 first and the two ids swapped.
func TestAnnex_CommitOrderKeepsIDs(t *testing.T) {
	t.Parallel()
	keys := sameShardKeys(t, 2)
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := beginEager(o), beginEager(o)
	t1.addNode(t, keys[0])
	t2.addNode(t, keys[1])
	id1, id2 := lookupID(t, o.Graph(), keys[0]), lookupID(t, o.Graph(), keys[1])
	if id1 >= id2 {
		t.Fatalf("premise: K1=%d must be interned before K2=%d", id1, id2)
	}
	t2.commit(t)
	t1.commit(t)
	r := reopen(t, o, dir)
	defer func() { _ = r.Close() }()
	if got1, got2 := lookupID(t, r.Graph(), keys[0]), lookupID(t, r.Graph(), keys[1]); got1 != id1 || got2 != id2 {
		t.Errorf("ids after reopen K1=%d K2=%d, want %d and %d", got1, got2, id1, id2)
	}
}

// TestAnnex_RollbackLeavesAHole is the rollback case: T1 interns K1 and rolls back,
// T2 creates K2 in the next slot. After a reopen K2 keeps its id and K1's slot is a
// hole.
func TestAnnex_RollbackLeavesAHole(t *testing.T) {
	t.Parallel()
	keys := sameShardKeys(t, 2)
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	t1 := beginEager(o)
	t1.addNode(t, keys[0])
	id1 := lookupID(t, o.Graph(), keys[0])
	t1.rollback()
	commitNodes(t, o.Store(), keys[1])
	id2 := lookupID(t, o.Graph(), keys[1])
	if id2 <= id1 {
		t.Fatalf("premise: K2=%d must follow K1=%d", id2, id1)
	}
	r := reopen(t, o, dir)
	defer func() { _ = r.Close() }()
	if got := lookupID(t, r.Graph(), keys[1]); got != id2 {
		t.Errorf("K2 id after reopen = %d, want %d", got, id2)
	}
	if k, ok := r.Graph().AdjList().Mapper().Resolve(id1); ok {
		t.Errorf("K1's slot %d resolves to %q after reopen; want a hole", id1, k)
	}
}

// TestAnnex_CreateThenDeleteInOneTx: a node created and removed by one transaction
// is dead after the reopen, under the same id, and the ids around it are unchanged.
func TestAnnex_CreateThenDeleteInOneTx(t *testing.T) {
	t.Parallel()
	keys := sameShardKeys(t, 3)
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), keys[0])
	tx := o.Store().Begin()
	if err := tx.AddNode(keys[1]); err != nil {
		t.Fatal(err)
	}
	if err := tx.RemoveNode(keys[1]); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddNode(keys[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want := map[string]graph.NodeID{}
	for _, k := range keys {
		want[k] = lookupID(t, o.Graph(), k)
	}
	liveDead := o.Graph().IsTombstoned(want[keys[1]])
	r := reopen(t, o, dir)
	defer func() { _ = r.Close() }()
	for _, k := range keys {
		if got := lookupID(t, r.Graph(), k); got != want[k] {
			t.Errorf("%q id after reopen = %d, want %d", k, got, want[k])
		}
	}
	if !liveDead || !r.Graph().IsTombstoned(want[keys[1]]) {
		t.Errorf("created-then-deleted node: live dead=%v, recovered dead=%v; want both true",
			liveDead, r.Graph().IsTombstoned(want[keys[1]]))
	}
}

// rewriteCommitAnnex rewrites the annex of the n-th (1-based) commit marker in
// dir's WAL with annex, re-encoding the frame so its CRC is valid.
func rewriteCommitAnnex(t *testing.T, dir string, n int, annex []byte) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	seen, done := 0, false
	err := waltest.RewriteFrames(walPath, func(_ int, f *wal.Frame) bool {
		if op, oerr := recovery.Decode(f.Payload); oerr == nil && op.Kind == txn.OpCommit {
			seen++
			if seen == n {
				// payload: 0xFD, kind, txnSeq(8), commitTS(8), annex…
				p := append([]byte(nil), f.Payload[:18]...)
				p = append(p, annex...)
				f.Payload = p
				done = true
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatalf("no commit marker %d in %s", n, walPath)
	}
}

// TestAnnex_TamperIsRefused pins the fail-stop sentinels: a marker whose annex
// names an id another key holds is ErrNodeIDMismatch, and one whose annex omits a
// node its op creates is ErrUnboundNodeKey. Both refuse store.Open.
func TestAnnex_TamperIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want error
		// annex builds the second marker's replacement annex from K1's id.
		annex func(id1 graph.NodeID) []byte
	}{
		{"id_mismatch", graph.ErrNodeIDMismatch, func(id1 graph.NodeID) []byte {
			b := binary.AppendUvarint(nil, 1)
			b = binary.AppendUvarint(b, 1) // op 0, src
			return binary.AppendUvarint(b, uint64(id1))
		}},
		{"unbound_key", recovery.ErrUnboundNodeKey, func(graph.NodeID) []byte {
			return binary.AppendUvarint(nil, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys := sameShardKeys(t, 2)
			dir := t.TempDir()
			o, err := store.Open[string, float64](dir, openOptions())
			if err != nil {
				t.Fatal(err)
			}
			commitNodes(t, o.Store(), keys[0], keys[1])
			id1 := lookupID(t, o.Graph(), keys[0])
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			rewriteCommitAnnex(t, dir, 2, tc.annex(id1))
			_, err = recovery.Open[string, float64](dir, recovery.Options[string, float64]{
				Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("recovery.Open = %v, want %v", err, tc.want)
			}
			if _, err := store.Open[string, float64](dir, openOptions()); !errors.Is(err, store.ErrUncleanRecovery) || !errors.Is(err, tc.want) {
				t.Fatalf("store.Open = %v, want ErrUncleanRecovery wrapping %v", err, tc.want)
			}
		})
	}
}

// TestAnnex_LegacyMarkerReplaysAsBefore pins compatibility: a marker without an
// annex (written before step 3) replays with the old auto-interning rule.
func TestAnnex_LegacyMarkerReplaysAsBefore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "legacy")
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	// Strip the annex: the body ends after the commit timestamp, as it did.
	rewriteCommitAnnex(t, dir, 1, nil)
	r, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open on a legacy marker: %v", err)
	}
	defer func() { _ = r.Close() }()
	if !has(r.Graph().AdjList().Mapper(), "legacy") {
		t.Error("the legacy transaction did not replay")
	}
}

// TestAnnex_UndoneCreationInCommittedTxn: a creation withdrawn inside a transaction
// that then commits — what a statement-level undo inside a committed transaction
// leaves — is named by the annex but by no op. After the reopen it holds the same
// id and is dead, as in the live graph; the transaction's other creation keeps its
// id.
func TestAnnex_UndoneCreationInCommittedTxn(t *testing.T) {
	t.Parallel()
	keys := sameShardKeys(t, 2)
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	e := beginEager(o)
	if err := e.g.Writer(e.wtx).AddNode(keys[0]); err != nil {
		t.Fatal(err)
	}
	e.wtx.EnterUndo()
	if _, err := e.g.Writer(e.wtx).RemoveNode(keys[0]); err != nil {
		t.Fatal(err)
	}
	e.wtx.ExitUndo()
	e.addNode(t, keys[1])
	e.commit(t)
	idU, idB := lookupID(t, o.Graph(), keys[0]), lookupID(t, o.Graph(), keys[1])
	if !o.Graph().IsTombstoned(idU) {
		t.Fatal("premise: the undone creation is alive in the live graph")
	}
	r := reopen(t, o, dir)
	defer func() { _ = r.Close() }()
	if got := lookupID(t, r.Graph(), keys[0]); got != idU || !r.Graph().IsTombstoned(got) {
		t.Errorf("undone creation after reopen: id %d dead=%v, want id %d dead", got, r.Graph().IsTombstoned(got), idU)
	}
	if got := lookupID(t, r.Graph(), keys[1]); got != idB {
		t.Errorf("committed creation after reopen: id %d, want %d", got, idB)
	}
}
