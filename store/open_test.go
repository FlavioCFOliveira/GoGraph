package store_test

// open_test.go — rmp #2523: the composed open, store.Open.
//
// Layer: short.
//
// Three properties are pinned here, each adjudicated against the DURABLE record
// rather than an in-memory counter:
//
//   - The recovered transaction sequence survives a reopen through store.Open
//     (the rmp #2522 class): every sequence a reopened store mints is strictly
//     above every sequence the WAL already holds.
//   - A recovery that is not clean is refused with a typed error, leaves the
//     directory untouched and the WAL lock free; that holds for the
//     nil-error not-clean outcome and for fail-stop corruption alike.
//   - A SIGKILL, reopen and append cycle through store.Open loses no
//     acknowledged commit.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// openOptions is the codec pair every test here opens and writes with.
func openOptions() store.Options[string, float64] {
	return store.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// commitNodes commits one AddNode transaction per key through st.
func commitNodes(t testing.TB, st *txn.Store[string, float64], keys ...string) {
	t.Helper()
	for _, k := range keys {
		tx := st.Begin()
		if err := tx.AddNode(k); err != nil {
			_ = tx.Rollback()
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%q): %v", k, err)
		}
	}
}

// walCommitSeqs returns the TxnSeq of every v3 commit marker in dir's WAL, in
// file order, read off disk.
func walCommitSeqs(t testing.TB, dir string) []uint64 {
	t.Helper()
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("reader Close: %v", cerr)
		}
	}()
	var seqs []uint64
	for f := range r.Frames() {
		p := f.Payload
		if len(p) < 10 || p[0] != txn.OpRecordV3 || p[1] != byte(txn.OpCommit) {
			continue
		}
		seqs = append(seqs, binary.LittleEndian.Uint64(p[2:10]))
	}
	return seqs
}

// has reports whether key was ever interned by m. No test here removes a
// node, so interned means present.
func has(m *graph.Mapper[string], key string) bool {
	_, ok := m.Lookup(key)
	return ok
}

// requireStrictlyIncreasing fails unless seqs is non-empty and strictly
// increasing, which is the property "no sequence is spent twice in one WAL".
func requireStrictlyIncreasing(t testing.TB, seqs []uint64) {
	t.Helper()
	if len(seqs) == 0 {
		t.Fatal("the WAL holds no commit markers: the durable record is not being read")
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("the durable transaction sequence is not strictly increasing at marker %d: %v. "+
				"One WAL holds two transactions under one sequence number, which is what "+
				"recovery's TxnSeq-suffix atomicity filter tells transactions apart by", i, seqs)
		}
	}
}

// TestOpen_ResumesTheRecoveredTxnSeq is the rmp #2522-class regression for the
// composed open: across repeated store.Open epochs on one directory, every
// sequence minted after a reopen exceeds every sequence the WAL already holds.
//
// A hand-written reopen that builds the store with txn.NewStoreWithOptions
// over the recovered graph restarts the sequence at 0 and fails this test at
// the second epoch.
func TestOpen_ResumesTheRecoveredTxnSeq(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shape := []int{3, 2, 5}
	var prior []uint64
	key := 0
	for epoch, n := range shape {
		o, err := store.Open(dir, openOptions())
		if err != nil {
			t.Fatalf("epoch %d: store.Open: %v", epoch, err)
		}
		keys := make([]string, n)
		for i := range keys {
			keys[i] = "k" + strconv.Itoa(key)
			key++
		}
		commitNodes(t, o.Store(), keys...)
		if err := o.Close(); err != nil {
			t.Fatalf("epoch %d: Close: %v", epoch, err)
		}

		got := walCommitSeqs(t, dir)
		if len(got) != len(prior)+n {
			t.Fatalf("epoch %d committed %d transactions; the WAL went from %d to %d commit markers",
				epoch, n, len(prior), len(got))
		}
		for i, want := range prior {
			if got[i] != want {
				t.Fatalf("epoch %d rewrote durable marker %d: %d became %d", epoch, i, want, got[i])
			}
		}
		requireStrictlyIncreasing(t, got)
		prior = got
	}

	// Every acknowledged key is present after a final reopen.
	o, err := store.Open(dir, openOptions())
	if err != nil {
		t.Fatalf("final store.Open: %v", err)
	}
	defer func() {
		if cerr := o.Close(); cerr != nil {
			t.Errorf("final Close: %v", cerr)
		}
	}()
	for i := 0; i < key; i++ {
		if !has(o.Graph().AdjList().Mapper(), "k"+strconv.Itoa(i)) {
			t.Errorf("acknowledged node k%d is missing after reopen", i)
		}
	}
	if got := o.Recovery().MaxTxnSeq; got != prior[len(prior)-1] {
		t.Errorf("Recovery().MaxTxnSeq = %d, want the durable maximum %d", got, prior[len(prior)-1])
	}
}

// buildCommittedWAL writes one transaction per key into dir/wal through a
// plain store and closes the WAL, so the directory holds a WAL-only image.
func buildCommittedWAL(t *testing.T, dir string, keys ...string) {
	t.Helper()
	o, err := store.Open(dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open (build): %v", err)
	}
	commitNodes(t, o.Store(), keys...)
	if err := o.Close(); err != nil {
		t.Fatalf("Close (build): %v", err)
	}
}

// injectUndecodableBodyInCommittedTxn rewrites dir/wal so that the first data
// frame of transaction txnSeq keeps its v3 header but loses its codec body, and
// re-encodes every frame so all CRCs match. The result is a CRC-valid,
// committed transaction whose op cannot be decoded, which recovery reports as
// [recovery.ErrCommittedTxnCorruptOp]: not clean, and not fail-stop.
func injectUndecodableBodyInCommittedTxn(t *testing.T, dir string, txnSeq uint64) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	raw, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var frames []wal.Frame
	injected := false
	r := bytes.NewReader(raw)
	for {
		f, derr := wal.Decode(r)
		if derr != nil {
			break
		}
		op, oerr := recovery.Decode(f.Payload)
		if oerr != nil {
			t.Fatalf("recovery.Decode: %v", oerr)
		}
		if !injected && op.Version == txn.OpRecordV3 && op.TxnSeq == txnSeq && op.Kind != txn.OpCommit {
			f.Payload = append([]byte(nil), f.Payload[:10]...)
			injected = true
		}
		frames = append(frames, f)
	}
	if !injected {
		t.Fatalf("no data frame of transaction %d in %s", txnSeq, walPath)
	}
	var out bytes.Buffer
	for i := range frames {
		if _, err := wal.Encode(&out, frames[i]); err != nil {
			t.Fatalf("wal.Encode(frame %d): %v", i, err)
		}
	}
	if err := os.WriteFile(walPath, out.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// readWAL returns dir/wal's bytes.
func readWAL(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "wal")) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// requireWALLockFree proves a refused open released (or never took) the WAL
// lock: a fresh writer can open the WAL and close it again.
func requireWALLockFree(t *testing.T, dir string) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("the WAL is not openable after a refused store.Open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}
}

// TestOpen_UncleanRecovery_Refused pins the clean gate on the not-clean,
// not-fail-stop outcome ([recovery.ErrCommittedTxnCorruptOp]): recovery
// returns a nil error for it, so a gate on the error alone would let it
// through, and every commit appended afterwards would be discarded by the next
// recovery.
func TestOpen_UncleanRecovery_Refused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	buildCommittedWAL(t, dir, "keep", "lost", "after")
	injectUndecodableBodyInCommittedTxn(t, dir, 2)

	// Precondition: this really is the nil-error, not-clean outcome.
	res, rerr := recovery.Open(dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if rerr != nil || res.IsClean() || !errors.Is(res.TailErr, recovery.ErrCommittedTxnCorruptOp) {
		t.Fatalf("precondition: want nil error, IsClean false, TailErr ErrCommittedTxnCorruptOp; "+
			"got err=%v IsClean=%t TailErr=%v", rerr, res.IsClean(), res.TailErr)
	}
	before := readWAL(t, dir)

	o, err := store.Open(dir, openOptions())
	if err == nil {
		_ = o.Close()
		t.Fatal("store.Open accepted a recovery that is not clean")
	}
	if o != nil {
		t.Fatalf("store.Open returned a non-nil store with error %v", err)
	}
	if !errors.Is(err, store.ErrUncleanRecovery) {
		t.Fatalf("error %v does not match store.ErrUncleanRecovery", err)
	}
	if !errors.Is(err, recovery.ErrCommittedTxnCorruptOp) {
		t.Fatalf("error %v does not carry the recovery's TailErr", err)
	}
	var ue *store.UncleanRecoveryError[string, float64]
	if !errors.As(err, &ue) {
		t.Fatalf("error %T is not *store.UncleanRecoveryError", err)
	}
	if ue.Dir != dir || ue.Result.Graph == nil || !has(ue.Result.Graph.AdjList().Mapper(), "keep") {
		t.Fatalf("UncleanRecoveryError does not carry the refused recovery: dir=%q graph=%v", ue.Dir, ue.Result.Graph)
	}
	if m := ue.Result.Graph.AdjList().Mapper(); has(m, "lost") || has(m, "after") {
		t.Fatal("the diagnostic graph holds a transaction at or after the corrupt op")
	}
	if !bytes.Equal(readWAL(t, dir), before) {
		t.Fatal("a refused store.Open modified the WAL")
	}
	requireWALLockFree(t, dir)
}

// TestOpen_FailStopCorruption_Refused pins the refusal on a fail-stop
// corruption (a CRC mismatch), which recovery returns as its error.
func TestOpen_FailStopCorruption_Refused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	buildCommittedWAL(t, dir, "a", "b", "c")
	raw := readWAL(t, dir)
	// Flip one byte inside the first frame's payload: its CRC no longer
	// matches and every later frame is unreachable.
	raw[20] ^= 0xFF
	if err := os.WriteFile(filepath.Join(dir, "wal"), raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	o, err := store.Open(dir, openOptions())
	if err == nil {
		_ = o.Close()
		t.Fatal("store.Open accepted a fail-stop corruption")
	}
	if !errors.Is(err, store.ErrUncleanRecovery) || !errors.Is(err, wal.ErrCRCMismatch) {
		t.Fatalf("error %v: want both store.ErrUncleanRecovery and wal.ErrCRCMismatch", err)
	}
	if !bytes.Equal(readWAL(t, dir), raw) {
		t.Fatal("a refused store.Open modified the WAL")
	}
	requireWALLockFree(t, dir)
}

// TestOpen_RejectsInvalidArguments covers the argument checks.
func TestOpen_RejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	if _, err := store.Open("", openOptions()); err == nil {
		t.Error("store.Open accepted an empty directory")
	}
	if _, err := store.Open(t.TempDir(), store.Options[string, float64]{}); err == nil {
		t.Error("store.Open accepted a nil codec")
	}
}

// TestOpen_WALLockedFailsAndLeaksNothing proves that a second open of a
// directory held by a live store fails with wal.ErrWALLocked, and that the
// live store is unaffected.
func TestOpen_WALLockedFailsAndLeaksNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open(dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := store.Open(dir, openOptions()); !errors.Is(err, wal.ErrWALLocked) {
		t.Fatalf("second store.Open: got %v, want wal.ErrWALLocked", err)
	}
	commitNodes(t, o.Store(), "still-writable")
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ─── SIGKILL, reopen, append ────────────────────────────────────────────────

// childAppendMode is the subproc mode of the crash-cycle child.
const childAppendMode = "store-open-append-loop"

func init() {
	// The child opens args[0] with store.Open, then commits one AddNode
	// transaction per key "<args[1]>-<i>" for i = 0, 1, ... until it is
	// killed, printing "ack <key>" to stdout after each Commit returns nil.
	// A line on stdout is therefore an acknowledged commit.
	subproc.Register(childAppendMode, func(args []string) int {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "need <dir> <prefix>")
			return 2
		}
		o, err := store.Open(args[0], openOptions())
		if err != nil {
			fmt.Fprintf(os.Stderr, "store.Open: %v\n", err)
			return 1
		}
		st := o.Store()
		for i := 0; ; i++ {
			k := args[1] + "-" + strconv.Itoa(i)
			tx := st.Begin()
			if err := tx.AddNode(k); err != nil {
				fmt.Fprintf(os.Stderr, "AddNode: %v\n", err)
				return 1
			}
			if err := tx.Commit(); err != nil {
				fmt.Fprintf(os.Stderr, "Commit: %v\n", err)
				return 1
			}
			fmt.Printf("ack %s\n", k)
		}
	})
}

// runKilledAppender starts the crash-cycle child on dir, waits for want
// acknowledged commits, SIGKILLs it, and returns every key it acknowledged
// (including any read after the kill was sent).
func runKilledAppender(t *testing.T, dir, prefix string, want int) []string {
	t.Helper()
	cmd := exec.Command(os.Args[0], dir, prefix) //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), subproc.EnvMode+"="+childAppendMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var acked []string
	sc := bufio.NewScanner(stdout)
	killed := false
	deadline := time.Now().Add(60 * time.Second)
	for sc.Scan() {
		line := sc.Text()
		if k, ok := strings.CutPrefix(line, "ack "); ok {
			acked = append(acked, k)
		}
		if !killed && (len(acked) >= want || time.Now().After(deadline)) {
			if kerr := cmd.Process.Kill(); kerr != nil {
				t.Fatalf("Kill: %v", kerr)
			}
			killed = true
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	if !killed {
		t.Fatalf("child %s exited before it was killed (%v); stderr: %s", prefix, werr, stderr.String())
	}
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		t.Fatalf("child %s: Wait = %v, want an exit by signal; stderr: %s", prefix, werr, stderr.String())
	}
	if len(acked) < want {
		t.Fatalf("child %s acknowledged %d commits before the deadline, want %d; stderr: %s",
			prefix, len(acked), want, stderr.String())
	}
	return acked
}

// TestOpen_SIGKILLReopenAppend_LosesNothingAcknowledged runs several cycles of
// "store.Open, append, SIGKILL" on one directory. Every cycle after the first
// reopens a WAL whose tail a SIGKILL may have torn. At the end, a final
// store.Open must recover cleanly, hold every key any child acknowledged, and
// show a strictly increasing durable sequence across every cycle.
func TestOpen_SIGKILLReopenAppend_LosesNothingAcknowledged(t *testing.T) {
	dir := t.TempDir()
	const cycles = 4
	const perCycle = 50
	var acked []string
	for c := 0; c < cycles; c++ {
		acked = append(acked, runKilledAppender(t, dir, "c"+strconv.Itoa(c), perCycle)...)
	}

	o, err := store.Open(dir, openOptions())
	if err != nil {
		t.Fatalf("final store.Open: %v", err)
	}
	defer func() {
		if cerr := o.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if !o.Recovery().IsClean() {
		t.Fatalf("final recovery is not clean: %v", o.Recovery().TailErr)
	}
	m := o.Graph().AdjList().Mapper()
	lost := 0
	for _, k := range acked {
		if !has(m, k) {
			lost++
			if lost <= 5 {
				t.Errorf("acknowledged commit %q is missing after SIGKILL and reopen", k)
			}
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d acknowledged commits lost", lost, len(acked))
	}
	requireStrictlyIncreasing(t, walCommitSeqs(t, dir))
	t.Logf("%d cycles, %d acknowledged commits, all recovered; WALTailOffset=%d",
		cycles, len(acked), o.Recovery().WALTailOffset)
}
