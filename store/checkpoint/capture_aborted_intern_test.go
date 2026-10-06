package checkpoint

// capture_aborted_intern_test.go — rmp #2991: a checkpoint capture refused
// itself with snapshot.ErrCaptureNotQuiesced while no transaction was open at
// its instant.
//
// Layer: short.
//
// # The interleaving
//
// Phase 1 opens the MVCC instant inside the commit serialiser's drain; phase 1b
// walks the mapper with the lock RELEASED, while writers intern, commit and
// abort. Two writers acting in that window, on keys of ONE mapper shard:
//
//   - L interns the lower slot and commits after the instant: its birth record
//     is not visible at the instant, so the walk drops L;
//   - H interns the next slot and aborts: the abort withdraws H's first-birth
//     record, and an id with no record read as "interned in every reader's
//     past", so the walk kept H above the dropped L.
//
// A kept id above a dropped one is the intra-index hole the capture refuses.
// The interleaving is constructed in a wrapping snapshot backend, immediately
// before the real capture, so it happens on every run.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// interleavingBackend is the production snapshot backend with one hook run
// before the capture, in phase 1b: the instant is open and the commit lock is
// released.
type interleavingBackend struct {
	osSnapshotBackend[string, int64]
	beforeCapture func()
}

func (b interleavingBackend) CaptureGraph(cs *csr.CSR[int64], g *lpg.Graph[string, int64],
	codec txn.Codec[string], wcodec txn.WeightCodec[int64], at *lpg.Snapshot) (*snapshot.Capture[int64], error) {
	b.beforeCapture()
	return b.osSnapshotBackend.CaptureGraph(cs, g, codec, wcodec, at)
}

// sameShardPair returns two keys that intern into the same mapper shard, found
// by probing a throwaway mapper: placement is a pure function of the key.
func sameShardPair(t *testing.T) (string, string) {
	t.Helper()
	m := graph.NewMapper[string]()
	seen := make(map[uint64]string)
	for i := range 100000 {
		k := fmt.Sprintf("abort-intern-%06d", i)
		s := graph.MapperShardOf(m.Intern(k))
		if prev, ok := seen[s]; ok {
			return prev, k
		}
		seen[s] = k
	}
	t.Fatal("no two probe keys landed in the same mapper shard")
	return "", ""
}

// TestCheckpoint_CaptureIgnoresAbortedInternAfterInstant drives the interleaving
// above and asserts the checkpoint succeeds, its snapshot loads, and recovery
// holds exactly the durable state: the seed commit, L (committed after the
// instant, so replayed from the WAL suffix), and never H.
func TestCheckpoint_CaptureIgnoresAbortedInternAfterInstant(t *testing.T) {
	t.Parallel()
	keyL, keyH := sameShardPair(t)
	dir, g, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	// A committed node before the instant, so the image is not empty.
	seed := st.Begin()
	if err := seed.AddNode("seed"); err != nil {
		t.Fatalf("seed AddNode: %v", err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	var hookRan bool
	cp.snap = interleavingBackend{beforeCapture: func() {
		hookRan = true
		// L: interned first, committed after the instant, durably.
		tx := st.Begin()
		if err := tx.AddNode(keyL); err != nil {
			t.Errorf("L AddNode: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("L Commit: %v", err)
		}
		// H: interned next in the same shard, then rolled back: the record is
		// aborted and its first-birth record withdrawn.
		htx := g.BeginVersionedTx()
		if err := g.Writer(htx).AddNode(keyH); err != nil {
			t.Errorf("H AddNode: %v", err)
		}
		htx.Abandon()
		g.EndVersionedTx(htx)
		idL, okL := g.AdjList().Mapper().Lookup(keyL)
		idH, okH := g.AdjList().Mapper().Lookup(keyH)
		if !okL || !okH || graph.MapperShardOf(idL) != graph.MapperShardOf(idH) || idL >= idH {
			t.Errorf("premise lost: L=%d(%v) H=%d(%v) must share a shard with L below H",
				uint64(idL), okL, uint64(idH), okH)
		}
	}}

	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("RunCheckpoint: %v (snapshot.ErrCaptureNotQuiesced: %v)",
			err, errors.Is(err, snapshot.ErrCaptureNotQuiesced))
	}
	if !hookRan {
		t.Fatal("the interleaving hook never ran: the capture was not reached")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil || !res.IsClean() || !res.SnapshotHit {
		t.Fatalf("recovery: err=%v clean=%v snapshotHit=%v", err, res.IsClean(), res.SnapshotHit)
	}
	rg := res.Graph
	alive := func(k string) bool {
		id, ok := rg.AdjList().Mapper().Lookup(k)
		return ok && rg.NodeExistsAsOf(id, nil)
	}
	for _, k := range []string{"seed", keyL} {
		if !alive(k) {
			t.Errorf("committed node %q missing after recovery", k)
		}
	}
	if alive(keyH) {
		t.Errorf("aborted node %q is alive after recovery", keyH)
	}
}

// TestCheckpoint_CaptureTombstonesKeyOfTxnOpenAtInstant pins the case the commit
// serialiser's drain does NOT exclude (storage audit F4): an lpg write transaction
// is not a registered store writer, so a key it interned can be open, uncommitted,
// at the capture instant.
//
// X is interned by a BeginVersionedTx left open across the instant; Y is interned
// above X in the same shard and committed before the instant. The capture read's
// mapper watermark covers both: Y is captured alive and X as a tombstone with its
// key. After the instant the open transaction is abandoned and X is created through
// txn.Store with a property, so its frames follow the checkpoint's WAL watermark.
// Recovery must be clean and hold the seed, Y, and X with its property: replaying
// X's creation revives the tombstoned id.
//
// A plain BeginRead snapshot drops X (its birth is not visible) and keeps Y above
// it, which is the hole snapshot.ErrCaptureNotQuiesced refuses.
func TestCheckpoint_CaptureTombstonesKeyOfTxnOpenAtInstant(t *testing.T) {
	t.Parallel()
	keyX, keyY := sameShardPair(t)
	dir, g, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	commit := func(key string, fn func(tx *txn.Tx[string, int64]) error) {
		t.Helper()
		tx := st.Begin()
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s: %v", key, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("%s Commit: %v", key, err)
		}
	}
	commit("seed", func(tx *txn.Tx[string, int64]) error { return tx.AddNode("seed") })

	// X: interned by an lpg write transaction that stays OPEN across the instant.
	htx := g.BeginVersionedTx()
	ended := false
	defer func() {
		if !ended {
			htx.Abandon()
			g.EndVersionedTx(htx)
		}
	}()
	if err := g.Writer(htx).AddNode(keyX); err != nil {
		t.Fatalf("X AddNode: %v", err)
	}
	// Y: interned above X in the same shard, committed before the instant.
	commit(keyY, func(tx *txn.Tx[string, int64]) error { return tx.AddNode(keyY) })
	idX, okX := g.AdjList().Mapper().Lookup(keyX)
	idY, okY := g.AdjList().Mapper().Lookup(keyY)
	if !okX || !okY || graph.MapperShardOf(idX) != graph.MapperShardOf(idY) || idX >= idY {
		t.Fatalf("premise lost: X=%d(%v) Y=%d(%v) must share a shard with X below Y",
			uint64(idX), okX, uint64(idY), okY)
	}

	var hookRan bool
	cp.snap = interleavingBackend{beforeCapture: func() {
		hookRan = true
		htx.Abandon()
		g.EndVersionedTx(htx)
		ended = true
		commit(keyX, func(tx *txn.Tx[string, int64]) error {
			if err := tx.AddNode(keyX); err != nil {
				return err
			}
			return tx.SetNodeProperty(keyX, "p", lpg.StringValue("after-instant"))
		})
	}}

	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("RunCheckpoint: %v (snapshot.ErrCaptureNotQuiesced: %v)",
			err, errors.Is(err, snapshot.ErrCaptureNotQuiesced))
	}
	if !hookRan {
		t.Fatal("the interleaving hook never ran: the capture was not reached")
	}
	if cp.Stats().WALTruncBytes == 0 {
		t.Fatal("the checkpoint truncated no WAL bytes: Y and the seed were not folded " +
			"into the snapshot, so the test cannot show the snapshot carries them")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil || !res.IsClean() || !res.SnapshotHit {
		t.Fatalf("recovery: err=%v clean=%v snapshotHit=%v", err, res.IsClean(), res.SnapshotHit)
	}
	rg := res.Graph
	for _, k := range []string{"seed", keyY, keyX} {
		id, ok := rg.AdjList().Mapper().Lookup(k)
		if !ok || !rg.NodeExistsAsOf(id, nil) {
			t.Errorf("node %q not alive after recovery (interned %v)", k, ok)
		}
	}
	v, ok := rg.GetNodeProperty(keyX, "p")
	if s, _ := v.String(); !ok || s != "after-instant" {
		t.Errorf("X property p = %v (present %v), want \"after-instant\"", v, ok)
	}
}
