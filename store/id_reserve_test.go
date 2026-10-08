package store_test

// id_reserve_test.go — WAL v2 step 4 (rmp #3021 B, docs/design-wal-v2.md §4 item
// 9): the fail-stop sentinels of the node id records.
//
// Layer: short.

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func requireIDRefusal(t *testing.T, dir string, want error) {
	t.Helper()
	_, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if !errors.Is(err, want) {
		t.Fatalf("recovery.Open = %v, want %v", err, want)
	}
	if _, err := store.Open[string, float64](dir, openOptions()); !errors.Is(err, store.ErrUncleanRecovery) || !errors.Is(err, want) {
		t.Fatalf("store.Open = %v, want ErrUncleanRecovery wrapping %v", err, want)
	}
}

// TestIDReserve_AnnexBeyondReservationIsRefused: a commit marker whose annex names
// an id far above every reservation logged for its shard is ErrIDBeyondReservation.
// Before step 4 the replay placed the key at that id and the store opened.
func TestIDReserve_AnnexBeyondReservationIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "beyond")
	shard := uint64(graph.MapperShardOf(lookupID(t, o.Graph(), "beyond")))
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	far := uint64(1)<<20<<8 | shard
	b := binary.AppendUvarint(nil, 1)
	b = binary.AppendUvarint(b, 1) // op 0, src
	rewriteCommitAnnex(t, dir, 1, binary.AppendUvarint(b, far))
	requireIDRefusal(t, dir, recovery.ErrIDBeyondReservation)
}

// TestIDReserve_CorruptRecordIsRefused: a ReserveIDs record of the wrong length
// is ErrIDRecordCorrupt.
func TestIDReserve_CorruptRecordIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "corrupt")
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	done := false
	err = waltest.RewriteFrames(filepath.Join(dir, "wal"), func(_ int, f *wal.Frame) bool {
		if !done && len(f.Payload) == wal.ReserveIDsSize && f.Payload[0] == wal.ControlRecordTag && f.Payload[1] == wal.CtlReserveIDs {
			f.Payload = f.Payload[:5]
			done = true
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("premise: the log holds no reservation record")
	}
	requireIDRefusal(t, dir, recovery.ErrIDRecordCorrupt)
}
