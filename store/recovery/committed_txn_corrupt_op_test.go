package recovery

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// countingBackend records every counter increment by name so a test can assert
// that a specific metric fired. It is safe for concurrent use because
// [metrics.SetBackend] installs it process-wide: any other test running in
// parallel in this package emits into it too, so every access is mutex-guarded
// and assertions only ever look at one name.
type countingBackend struct {
	mu       sync.Mutex
	counters map[string]uint64
}

func newCountingBackend() *countingBackend {
	return &countingBackend{counters: make(map[string]uint64)}
}

func (b *countingBackend) IncCounter(name string, delta uint64) {
	b.mu.Lock()
	b.counters[name] += delta
	b.mu.Unlock()
}

func (b *countingBackend) ObserveLatency(string, time.Duration) {}
func (b *countingBackend) SetGauge(string, float64)             {}

func (b *countingBackend) count(name string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters[name]
}

// TestRecovery_CorruptOpInsideCommittedTxn_NotCleanAndDiagnosable_2794 is the
// gate for rmp #2794.
//
// # The defect
//
// An op that cannot be decoded and applied INSIDE an already-durable,
// already-committed v3 transaction stops replay for the whole remainder of the
// file — the transaction carrying it and every transaction after it are
// discarded, even though each one's commit was acknowledged. That much is
// deliberate: the ops are irrecoverable, so replay cannot honestly continue.
//
// What was wrong is that the state was reported CLEAN. The stop reason was a
// plain errors.New, so [tailErrIsCorruption] fell to its default arm and called
// it benign: [Open] returned nil, [Result.IsClean] returned true, no counter
// moved, and nothing was logged. A whole WAL suffix of acknowledged
// transactions vanished in total silence — the fail-silent shape CLAUDE.md's
// "fail-stop, never fail-silent" mandate exists to forbid.
//
// # The decision this test pins (recorded on rmp #2794)
//
// The store still OPENS: the lost bytes are irrecoverable, so refusing to open
// would trade silent data loss for total loss of service with no repair path.
// What changes is that the loss is now DIAGNOSABLE and the state is no longer
// called clean:
//
//	Open        -> nil error (the store opens; the committed prefix is usable)
//	TailErr     -> wraps ErrCommittedTxnCorruptOp
//	IsClean()   -> FALSE
//	metric      -> store.recovery.openCodec.committedTxnCorruptOp incremented
//	log         -> a structured slog warning naming the frame and the transaction
//
// See the departure note on [tailErrIsCorruption] for why the open is allowed
// to succeed while the result is reported not-clean.
//
// # What fails without the fix
//
// IsClean() reports true, the counter stays at 0, and the log stays empty. The
// suffix-loss assertions ("lost" and "after" absent) pass on the unfixed code
// too — they are here to prove the loss is real, not to detect the defect.
func TestRecovery_CorruptOpInsideCommittedTxn_NotCleanAndDiagnosable_2794(t *testing.T) {
	// NOT parallel: the test installs a process-wide metrics backend and a
	// process-wide slog default.
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}

	// Three transactions on ONE store, so the sequences are monotonic:
	// tx1 = seq 1 (keep), tx2 = seq 2 (lost), tx3 = seq 3 (after).
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	for _, key := range []string{"keep", "lost", "after"} {
		tx := s.Begin()
		if err := tx.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%s): %v", key, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Inject the defect DIRECTLY: truncate the body of tx2's AddNode frame to
	// the bare v3 header (version + kind + txnSeq), leaving the frame
	// well-formed and CRC-VALID but its codec body undecodable
	// (stringCodec.Decode refuses a zero-length buffer), then re-encode the
	// whole WAL so every CRC matches. This is an already-durable frame whose op
	// cannot be applied — the general case rmp #2794 is about. (The rmp #2783
	// route to the same state, a nested property list, is closed at commit by
	// ef0467e6, so it cannot be used to reproduce this any more.)
	injectUndecodableBodyInCommittedTxn(t, walPath, 2)

	// Capture the counter and the warning the fix must emit.
	backend := newCountingBackend()
	metrics.SetBackend(backend)
	defer metrics.SetBackend(nil)

	var logbuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prevLogger)

	res, err := Open[string, int64](dir, OptionsFromTxn(opts))

	// 1. The store still opens. Refusing here is what the recorded decision
	//    rejected: nothing is recoverable from the discarded suffix, so a
	//    refusal costs the whole service and buys no repair.
	if err != nil {
		t.Fatalf("recovery.Open returned %v; the recorded decision for rmp #2794 is that the store still OPENS "+
			"(the discarded suffix is irrecoverable, so refusing to open trades silent loss for total loss of service)", err)
	}
	if res.Graph == nil {
		t.Fatal("res.Graph == nil; the committed prefix must stay usable")
	}

	// 2. TailErr carries the sentinel, so a caller can classify it with errors.Is.
	if res.TailErr == nil {
		t.Fatal("res.TailErr == nil; an undecodable op inside a committed transaction must be surfaced")
	}
	if !errors.Is(res.TailErr, ErrCommittedTxnCorruptOp) {
		t.Fatalf("res.TailErr = %v; want errors.Is(..., ErrCommittedTxnCorruptOp). A plain errors.New here is "+
			"precisely why tailErrIsCorruption fell to its default arm and called the loss benign (rmp #2794)", res.TailErr)
	}

	// 3. NOT clean. This is the assertion the defect failed.
	if res.IsClean() {
		t.Fatalf("res.IsClean() = true after an undecodable op inside an already-committed transaction. "+
			"Two acknowledged transactions were discarded and the state was reported clean (rmp #2794). TailErr = %v", res.TailErr)
	}

	// 4. The corruption metric fired, so the loss is countable in production.
	const metric = "store.recovery.openCodec.committedTxnCorruptOp"
	if got := backend.count(metric); got == 0 {
		t.Fatalf("counter %q = 0; the discarded suffix left no metric trace (rmp #2794)", metric)
	}

	// 5. A structured warning was emitted, naming the transaction and the frame,
	//    so the loss is diagnosable from logs alone.
	logged := logbuf.String()
	if !strings.Contains(logged, "undecodable op inside an already-committed transaction") {
		t.Fatalf("no warning logged for the discarded suffix; slog output = %q (rmp #2794)", logged)
	}
	for _, attr := range []string{"txn_seq=2", "wal_frame=", "txn_op_index=", "wal_ops_applied="} {
		if !strings.Contains(logged, attr) {
			t.Fatalf("warning does not name %q; it must identify the frame and the transaction. slog output = %q", attr, logged)
		}
	}

	// 6. The loss itself: the committed prefix survives, and the transaction
	//    carrying the bad op AND every transaction after it are gone. These
	//    assertions hold on the unfixed code too — they document the severity,
	//    they do not detect the defect.
	mapper := res.Graph.AdjList().Mapper()
	if _, ok := mapper.Lookup("keep"); !ok {
		t.Fatal("node 'keep' (the committed prefix) missing after recovery")
	}
	if _, ok := mapper.Lookup("lost"); ok {
		t.Fatal("node 'lost' present; the transaction whose op is undecodable cannot be replayed")
	}
	if _, ok := mapper.Lookup("after"); ok {
		t.Fatal("node 'after' present; replay stops at the bad frame, so the suffix cannot be applied")
	}
	if res.WALOps != 1 {
		t.Fatalf("WALOps = %d, want 1 (only tx1 replays)", res.WALOps)
	}
}

// TestRecovery_CorruptOpInsideCommittedTxn_ReplayWALAgrees pins the same
// outcome on the WAL-only core, which is the path the title of rmp #2794 names
// and the one the deterministic simulation harness drives ([ReplayWAL], not
// [Open]). The classification lives in the shared replay core, so both entry
// points must report the identical state.
func TestRecovery_CorruptOpInsideCommittedTxn_ReplayWALAgrees(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	for _, key := range []string{"keep", "lost"} {
		tx := s.Begin()
		if err := tx.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%s): %v", key, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	injectUndecodableBodyInCommittedTxn(t, walPath, 2)

	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	rg := lpg.New[string, int64](adjlist.Config{Directed: true})
	rr, err := ReplayWAL[string, int64](t.Context(), r, rg, opts.Codec, opts.WeightCodec, 0)
	if err != nil {
		t.Fatalf("ReplayWAL returned %v; only a ctx cancellation is reported as the function error", err)
	}
	if !errors.Is(rr.TailErr, ErrCommittedTxnCorruptOp) {
		t.Fatalf("ReplayResult.TailErr = %v; want errors.Is(..., ErrCommittedTxnCorruptOp)", rr.TailErr)
	}
	if rr.IsClean() {
		t.Fatalf("ReplayResult.IsClean() = true; ReplayWAL's own godoc promises genuine corruption inside an "+
			"already-durable frame is reported not-clean (rmp #2794). TailErr = %v", rr.TailErr)
	}
	if rr.WALOps != 1 {
		t.Fatalf("ReplayResult.WALOps = %d, want 1", rr.WALOps)
	}
}

// injectUndecodableBodyInCommittedTxn rewrites the WAL at walPath so that the
// first non-marker v3 frame carrying transaction sequence txnSeq has its codec
// body removed, leaving only the 10-byte v3 header (version + kind + txnSeq).
// Every frame — the mutilated one included — is re-encoded through
// [wal.Encode], so all CRCs are valid and the file is a well-formed WAL: the
// only defect is that one op inside an already-committed transaction can no
// longer be walked through the codec.
//
// It is the direct injection rmp #2794 calls for. The frame stays a legal
// v3 record (so [Decode] accepts it and the replay loop buffers it as part of
// the transaction) and fails only at the apply, which is exactly where a
// genuinely damaged body fails.
func injectUndecodableBodyInCommittedTxn(t *testing.T, walPath string, txnSeq uint64) {
	t.Helper()

	raw, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", walPath, err)
	}

	frames := make([]wal.Frame, 0, 8)
	injected := false
	r := bytes.NewReader(raw)
	for {
		f, derr := wal.Decode(r)
		if derr != nil {
			break // clean EOF surfaces as a torn-frame error; the walk is done
		}
		op, oerr := Decode(f.Payload)
		if oerr != nil {
			t.Fatalf("Decode: %v", oerr)
		}
		if !injected && op.Version == txn.OpRecordV3 && op.TxnSeq == txnSeq && op.Kind != txn.OpCommit {
			// Keep the v3 header, drop the codec body. len(payload) >= 10 is
			// guaranteed by decodeV3 having accepted it.
			f.Payload = append([]byte(nil), f.Payload[:10]...)
			injected = true
		}
		frames = append(frames, f)
	}
	if !injected {
		t.Fatalf("no non-marker v3 frame with TxnSeq == %d found in %s", txnSeq, walPath)
	}

	var out bytes.Buffer
	for i := range frames {
		if _, err := wal.Encode(&out, frames[i]); err != nil {
			t.Fatalf("wal.Encode(frame %d): %v", i, err)
		}
	}
	if err := os.WriteFile(walPath, out.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", walPath, err)
	}
}
