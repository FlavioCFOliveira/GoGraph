package txn_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// cappedStore builds a typed string+int64 store under dir with an explicit
// per-transaction op cap, returning the store and its WAL writer (the caller
// closes the writer). The cap follows the standard convention (0 → default,
// MaxTxnOpsUnlimited → off, n → n). dir is created if absent so subtests may
// pass a fresh subdirectory.
func cappedStore(t *testing.T, dir string, maxTxnOps int) (*txn.Store[string, int64], *wal.Writer) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptionsCapped[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}, maxTxnOps)
	return s, w
}

// TestTx_Commit_RejectsOverCapTransaction is the producer-side headline for
// task #1296: a transaction buffering more than the store's op cap must be
// rejected by Commit with the typed [txn.ErrTransactionTooLarge], and NOTHING
// may be made durable — the check runs before any WAL frame is written, so a
// fresh recovery of the directory observes none of the rejected ops.
//
// This is the producer half of the cap that AGREES with recovery: a producer
// must never durably commit a transaction recovery would later reject (that
// would make a committed transaction unrecoverable — an ACID Durability
// violation). The boundary is pinned by the sibling exactly-cap success below.
func TestTx_Commit_RejectsOverCapTransaction(t *testing.T) {
	t.Parallel()
	const opCap = 16
	dir := t.TempDir()
	s, w := cappedStore(t, dir, opCap)

	tx := s.Begin()
	for i := 0; i < opCap+1; i++ { // one past the cap
		if err := tx.AddNode("n" + strconv.Itoa(i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	err := tx.Commit()
	if err == nil {
		t.Fatal("Commit returned nil for an over-cap transaction, want ErrTransactionTooLarge")
	}
	if !errors.Is(err, txn.ErrTransactionTooLarge) {
		t.Fatalf("Commit error = %v, want errors.Is(err, txn.ErrTransactionTooLarge)", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Nothing was made durable: the WAL holds no frames from the rejected
	// transaction. Assert via the raw on-disk size (no frames written) and via
	// a fresh recovery observing none of the nodes.
	raw, rerr := os.ReadFile(filepath.Join(dir, "wal")) //nolint:gosec // path under t.TempDir
	if rerr != nil {
		t.Fatalf("ReadFile(wal): %v", rerr)
	}
	if len(raw) != 0 {
		t.Fatalf("WAL is %d bytes after a rejected commit, want 0 (nothing durable)", len(raw))
	}

	res, oerr := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if oerr != nil {
		t.Fatalf("recovery.Open after rejected commit: %v", oerr)
	}
	if res.WALOps != 0 {
		t.Fatalf("recovered WALOps = %d, want 0 (rejected transaction is not durable)", res.WALOps)
	}
	if _, ok := res.Graph.AdjList().Mapper().Lookup("n0"); ok {
		t.Error("node n0 from the rejected transaction must not be recoverable")
	}
}

// TestTx_CommitWALOnly_RejectsOverCapTransaction covers the second durable
// commit entry point (the Cypher write path uses CommitWALOnly, not Commit):
// it must reject an over-cap transaction with the same typed error before
// writing any frame.
func TestTx_CommitWALOnly_RejectsOverCapTransaction(t *testing.T) {
	t.Parallel()
	const opCap = 8
	dir := t.TempDir()
	s, w := cappedStore(t, dir, opCap)
	defer func() { _ = w.Close() }()

	tx := s.Begin()
	for i := 0; i < opCap+1; i++ {
		if err := tx.AddNode("n" + strconv.Itoa(i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	err := tx.CommitWALOnly()
	if err == nil {
		t.Fatal("CommitWALOnly returned nil for an over-cap transaction, want ErrTransactionTooLarge")
	}
	if !errors.Is(err, txn.ErrTransactionTooLarge) {
		t.Fatalf("CommitWALOnly error = %v, want errors.Is(err, txn.ErrTransactionTooLarge)", err)
	}
}

// TestTx_Commit_ExactlyCapSucceeds_RoundTripsThroughRecovery pins the
// producer/recovery agreement at the boundary: a transaction of EXACTLY the
// cap commits durably AND round-trips through recovery with the SAME cap. This
// is the load-bearing invariant — anything the producer durably commits must
// replay — and the dual of the over-cap rejection above.
func TestTx_Commit_ExactlyCapSucceeds_RoundTripsThroughRecovery(t *testing.T) {
	t.Parallel()
	const opCap = 16
	dir := t.TempDir()
	s, w := cappedStore(t, dir, opCap)

	tx := s.Begin()
	for i := 0; i < opCap; i++ { // exactly the cap
		if err := tx.AddNode("n" + strconv.Itoa(i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit at exactly the cap: %v, want success", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recovery with the SAME cap replays the whole committed transaction.
	res, oerr := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
		MaxTxnOps:   opCap,
	})
	if oerr != nil {
		t.Fatalf("recovery.Open at exactly the cap: %v", oerr)
	}
	if !res.IsClean() {
		t.Fatalf("Result.IsClean() = false, want true (TailErr=%v)", res.TailErr)
	}
	if res.WALOps != opCap {
		t.Fatalf("recovered WALOps = %d, want %d", res.WALOps, opCap)
	}
	for i := 0; i < opCap; i++ {
		if _, ok := res.Graph.AdjList().Mapper().Lookup("n" + strconv.Itoa(i)); !ok {
			t.Errorf("node n%d missing after recovery of a cap-sized transaction", i)
		}
	}
}

// TestTx_MaxTxnOps_ResolutionAndDefault asserts the cap-resolution convention
// at the constructor: the uncapped constructor inherits the finite default,
// an explicit value is taken verbatim, and the unlimited sentinel disables the
// cap (the white-box constructor-default a black-box test cannot observe).
func TestTx_MaxTxnOps_ResolutionAndDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("uncapped_constructor_uses_default", func(t *testing.T) {
		t.Parallel()
		s, w := cappedStoreUncapped(t, filepath.Join(dir, "a"))
		defer func() { _ = w.Close() }()
		if got := s.MaxTxnOps(); got != txn.DefaultMaxTxnOps {
			t.Fatalf("MaxTxnOps() = %d, want DefaultMaxTxnOps (%d)", got, txn.DefaultMaxTxnOps)
		}
	})

	t.Run("explicit_value_verbatim", func(t *testing.T) {
		t.Parallel()
		s, w := cappedStore(t, filepath.Join(dir, "b"), 1234)
		defer func() { _ = w.Close() }()
		if got := s.MaxTxnOps(); got != 1234 {
			t.Fatalf("MaxTxnOps() = %d, want 1234", got)
		}
	})

	t.Run("unlimited_disables_cap", func(t *testing.T) {
		t.Parallel()
		s, w := cappedStore(t, filepath.Join(dir, "c"), txn.MaxTxnOpsUnlimited)
		defer func() { _ = w.Close() }()
		if got := s.MaxTxnOps(); got != 0 {
			t.Fatalf("MaxTxnOps() = %d, want 0 (unlimited disables the cap)", got)
		}
	})
}

// cappedStoreUncapped builds a store via the uncapped NewStoreWithOptions
// constructor (which resolves to txn.DefaultMaxTxnOps), mkdir-ing dir first so
// each subtest gets its own WAL directory.
func cappedStoreUncapped(t *testing.T, dir string) (*txn.Store[string, int64], *wal.Writer) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	return s, w
}
