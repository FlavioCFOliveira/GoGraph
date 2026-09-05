package main

// crash.go implements the real cross-process kill -9 crash + recovery
// demonstration. Unlike the in-process crash in run (which merely abandons the
// in-memory graph and reopens it), this path re-execs the example binary as a
// child, commits a fixed number of fsynced transfers, then hard-kills the child
// with SIGKILL WITH torn work still in flight — so recovery is exercised against
// a real, OS-level torn WAL tail. The parent proves the recovered ledger
// balances to exactly the durably-committed prefix: no committed transfer lost,
// no uncommitted transfer resurrected, no torn frame accepted as data.
//
// The two OS-specific primitives — hardKillSelf (raise SIGKILL) and
// interpretChildExit (decode the child's wait status) — live in the build-tagged
// selfkill_unix.go / selfkill_other.go so this file stays portable.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// errCrashUnsupported is returned by the real-crash entry points on platforms
// that cannot deliver SIGKILL to themselves (see selfkill_other.go), so callers
// degrade gracefully instead of pretending to have crashed.
var errCrashUnsupported = errors.New("real cross-process crash demo requires SIGKILL; unsupported on this platform")

// tornFramePayloadLen is the payload length the torn partial frame declares
// while writing none of it, so a WAL reader stops at a benign torn tail. It
// mirrors the crash-injection battery's wal.mid-frame scenario.
const tornFramePayloadLen = 100

// runRealCrashDemo performs the real cross-process crash + recovery
// demonstration and writes its report to w. It re-execs selfBin (this example's
// own binary) as a crash-child that commits cfg.crashCommitted fsynced transfers
// then SIGKILLs itself mid-stream with a torn frame in flight; the parent then
// reopens the data directory with recovery.OpenCtx and proves the ledger
// balances to exactly the durably-committed prefix.
//
// Bare lines carry deterministic facts (the committed/replayed counts and the
// conservation, torn-tail, and clean-recovery flags — reproducible for a fixed
// seed); lines prefixed "# " carry volatile telemetry (recovery wall-clock, WAL
// bytes replayed). Any lost committed transfer, resurrected uncommitted
// transfer, or accepted torn frame is a module durability defect and is
// returned as an error whose message is a deterministic repro recipe.
func runRealCrashDemo(ctx context.Context, w io.Writer, cfg config, crashCommitted int, selfBin string) error {
	if !crashDemoSupported {
		return errCrashUnsupported
	}
	if crashCommitted < 1 {
		return fmt.Errorf("crash-committed must be >= 1, got %d", crashCommitted)
	}

	// Size the plan to one more than the committed count: the extra transfer is
	// the deterministic "in-flight" one whose commit the crash interrupts. Both
	// the parent (here) and the child derive the identical plan from the seed.
	planCfg := cfg
	planCfg.transfers = crashCommitted + 1
	if err := planCfg.validate(); err != nil {
		return fmt.Errorf("crash config: %w", err)
	}
	plan, err := generateLedger(ctx, planCfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	dir, err := os.MkdirTemp("", "gograph-ex17-crash-")
	if err != nil {
		return fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	fmt.Fprintf(w, "config.accounts=%d\n", cfg.accounts)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	// Spawn and reap the crash-child; it must die by SIGKILL.
	if err := spawnCrashChild(ctx, selfBin, cfg, crashCommitted, dir, w); err != nil {
		return err
	}

	// Parent recovery: rebuild the ledger from disk alone (WAL only — the child
	// runs no checkpointer, so every committed transfer is replayed from the WAL
	// and none comes from a snapshot).
	start := time.Now()
	res, err := recovery.OpenCtx[string, int64](ctx, dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	recElapsed := time.Since(start)
	if err != nil {
		// A benign torn tail (the crash artefact) must NOT fail-stop recovery.
		// If it does, recovery mis-classified the partial tail frame as genuine
		// corruption — a durability regression.
		return fmt.Errorf("DURABILITY: recovery fail-stopped on a benign torn tail after kill -9 (repro: -real-crash -seed %d -accounts %d -crash-committed %d): %w",
			cfg.seed, cfg.accounts, crashCommitted, err)
	}
	if !res.IsClean() {
		return fmt.Errorf("DURABILITY: recovery reports unclean — the torn tail was treated as corruption (repro: -real-crash -seed %d -accounts %d -crash-committed %d): %w",
			cfg.seed, cfg.accounts, crashCommitted, res.TailErr)
	}

	rec, err := verifyRecoveredLedger(res.Graph, plan, cfg, crashCommitted)
	if err != nil {
		return err
	}

	plannedSum := sumAmounts(plan.transfers[:crashCommitted])
	conserved := rec.accountsReconciled && rec.amountSum == plannedSum
	// TailErr is non-nil exactly when recovery observed the torn frame the child
	// left behind; IsClean already guaranteed it was the benign torn-tail kind.
	tornDetected := res.TailErr != nil

	// Deterministic facts.
	fmt.Fprintf(w, "crash.committed_before_kill=%d\n", crashCommitted)
	fmt.Fprintf(w, "recovery.transfers_replayed=%d\n", rec.transfers)
	fmt.Fprintf(w, "recovery.torn_tail_detected=%d\n", boolToInt(tornDetected))
	fmt.Fprintf(w, "recovery.balance_conserved=%d\n", boolToInt(conserved))
	fmt.Fprintf(w, "recovery.is_clean=%d\n", boolToInt(res.IsClean()))

	// Volatile telemetry.
	fmt.Fprintf(w, "# recovery.elapsed=%s\n", recElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# recovery.wal_bytes_replayed=%s\n", humanBytes(walOffsetBytes(res.WALTailOffset)))
	fmt.Fprintf(w, "# recovery.wal_ops=%d\n", res.WALOps)
	fmt.Fprintf(w, "# recovery.snapshot_hit=%t\n", res.SnapshotHit)

	if !conserved {
		return fmt.Errorf("DURABILITY: ledger not conserved after crash recovery: debit=%d credit=%d recovered_sum=%d planned_sum=%d (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
			rec.debitSum, rec.creditSum, rec.amountSum, plannedSum, cfg.seed, cfg.accounts, crashCommitted)
	}
	return nil
}

// spawnCrashChild re-execs selfBin in crash-child mode against dir, waits for it
// to terminate, and records how it died as telemetry. It returns an error
// unless the child was terminated by SIGKILL (the expected crash), so a child
// that exited on its own — for example because a commit failed before the kill
// — surfaces as a harness failure carrying the child's stderr.
func spawnCrashChild(ctx context.Context, selfBin string, cfg config, crashCommitted int, dir string, w io.Writer) error {
	args := []string{
		"-crash-child-dir", dir,
		"-crash-committed", strconv.Itoa(crashCommitted),
		"-seed", strconv.FormatInt(cfg.seed, 10),
		"-accounts", strconv.Itoa(cfg.accounts),
		"-min-amount", strconv.FormatInt(cfg.minAmount, 10),
		"-max-amount", strconv.FormatInt(cfg.maxAmount, 10),
	}
	// selfBin is this example's own binary (os.Args[0] under `go run`, or a
	// test-built binary); args are fixed flags. Neither is user-tainted.
	cmd := exec.CommandContext(ctx, selfBin, args...) //nolint:gosec // G204: selfBin is the example's own binary, args are fixed flags
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		// The context was cancelled/timed out: exec.CommandContext kills the
		// child itself, which is indistinguishable at the OS level from the
		// child's own SIGKILL. Report it as a harness abort, not a crash.
		return fmt.Errorf("crash child run cancelled: %w", ctx.Err())
	}

	killed, desc := interpretChildExit(runErr)
	fmt.Fprintf(w, "# crash.child_termination=%s\n", desc)
	if !killed {
		return fmt.Errorf("crash child did not die by SIGKILL as expected (termination=%s); child stderr:\n%s", desc, stderr.String())
	}
	return nil
}

// runCrashChild is the INTERNAL crash-child entry point: it opens the store in
// cfg.crashChildDir, commits exactly cfg.crashCommitted transfers (each its own
// fsynced transaction), appends a torn partial WAL frame modelling the
// interrupted next commit, then hard-kills this process with SIGKILL. The
// store's WAL writer is deliberately left open, so the process is killed exactly
// as an abrupt crash mid-write would leave it. On a supported platform this
// function does not return.
func runCrashChild(ctx context.Context, cfg config, crashChildDir string, crashCommitted int) error {
	if !crashDemoSupported {
		return errCrashUnsupported
	}
	if crashCommitted < 1 {
		return fmt.Errorf("crash-committed must be >= 1, got %d", crashCommitted)
	}

	planCfg := cfg
	planCfg.transfers = crashCommitted + 1
	if err := planCfg.validate(); err != nil {
		return fmt.Errorf("crash config: %w", err)
	}
	plan, err := generateLedger(ctx, planCfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	walPath := filepath.Join(crashChildDir, "wal")
	wlog, err := wal.Open(walPath)
	if err != nil {
		return fmt.Errorf("open WAL: %w", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions(g, wlog, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Commit exactly crashCommitted transfers. Each Commit fsyncs its WAL frames
	// before returning (Tx.Commit's group-commit SyncGroup), so on return these
	// transfers are durable.
	for i := 0; i < crashCommitted; i++ {
		t := plan.transfers[i]
		if err := commitTransfer(store, plan.accountIDs[t.src], plan.accountIDs[t.dst], t.amount); err != nil {
			return fmt.Errorf("commit transfer %d: %w", i, err)
		}
	}

	// Model the interrupted next commit: a torn partial WAL frame appended after
	// the last durable transaction. It is the "uncommitted/torn work in flight"
	// a kill -9 mid-write would leave; recovery must stop at it, not apply it.
	if err := appendTornFrame(walPath); err != nil {
		return err
	}

	// Crash: SIGKILL this process. Does not return on a supported platform.
	return hardKillSelf()
}

// verifyRecoveredLedger checks that the recovered graph contains exactly the
// durably-committed prefix of the plan: every one of the first crashCommitted
// transfers present with its bit-exact amount, the interrupted transfer absent,
// and no spurious edge conjured from the torn frame. Any violation is a module
// durability defect and is returned with a deterministic repro recipe.
func verifyRecoveredLedger(g *lpg.Graph[string, int64], plan ledgerPlan, cfg config, crashCommitted int) (recoveryStats, error) {
	var rec recoveryStats
	for i := 0; i < crashCommitted; i++ {
		t := plan.transfers[i]
		src, dst := plan.accountIDs[t.src], plan.accountIDs[t.dst]
		if !g.AdjList().HasEdge(src, dst) {
			return recoveryStats{}, fmt.Errorf("DURABILITY: recovery lost committed transfer %d (%s->%s) after kill -9 (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
				i, src, dst, cfg.seed, cfg.accounts, crashCommitted)
		}
		got, ok := g.EdgeWeight(src, dst)
		if !ok {
			return recoveryStats{}, fmt.Errorf("DURABILITY: recovery lost the weight of committed transfer %d (%s->%s) (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
				i, src, dst, cfg.seed, cfg.accounts, crashCommitted)
		}
		if got != t.amount {
			return recoveryStats{}, fmt.Errorf("DURABILITY: recovery corrupted transfer %d amount (%s->%s): got %d want %d (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
				i, src, dst, got, t.amount, cfg.seed, cfg.accounts, crashCommitted)
		}
		rec.transfers++
		rec.amountSum += got
	}

	// The interrupted (never-committed) transfer must NOT have survived — not as
	// its intended edge, nor as any spurious edge conjured from the torn frame.
	inflight := plan.transfers[crashCommitted]
	ifSrc, ifDst := plan.accountIDs[inflight.src], plan.accountIDs[inflight.dst]
	if g.AdjList().HasEdge(ifSrc, ifDst) {
		return recoveryStats{}, fmt.Errorf("DURABILITY: recovery resurrected the uncommitted in-flight transfer (%s->%s) after kill -9 (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
			ifSrc, ifDst, cfg.seed, cfg.accounts, crashCommitted)
	}
	wantEdges := uint64(crashCommitted) //nolint:gosec // G115: crashCommitted is a small positive count
	if size := g.AdjList().Size(); size != wantEdges {
		return recoveryStats{}, fmt.Errorf("DURABILITY: recovered edge count = %d, want %d — a torn-tail frame was accepted as data (repro: -real-crash -seed %d -accounts %d -crash-committed %d)",
			size, crashCommitted, cfg.seed, cfg.accounts, crashCommitted)
	}

	// Double-entry reconciliation over exactly the transfers committed before
	// the kill, read from the recovered graph itself (see reconcileNetPositions).
	rec.debitSum, rec.creditSum, rec.accountsReconciled = reconcileNetPositions(g, plan.accountIDs, plan.transfers[:crashCommitted])
	return rec, nil
}

// appendTornFrame appends a deliberately incomplete WAL frame to walPath: the
// magic, version, and a payload-length field declaring tornFramePayloadLen
// bytes, but with the CRC and the payload never written. A wal.Reader stops at
// this torn tail with wal.ErrTornFrame — the benign crash-after-last-fsync case
// recovery treats as a clean cut. It opens a fresh fd (the WAL's exclusive lock
// is on a separate LOCK file, not the data file), so it is safe alongside the
// still-open store writer.
func appendTornFrame(walPath string) error {
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // G304: walPath is the example's own temp WAL file, not user input
	if err != nil {
		return fmt.Errorf("open WAL for torn append: %w", err)
	}
	// magic(4) + version(2) + length(4) = 10 bytes; the CRC(4) and the payload
	// never follow, so the reader is left short of a full header/frame.
	partial := make([]byte, 10)
	copy(partial[0:4], wal.Magic[:])
	binary.LittleEndian.PutUint16(partial[4:6], wal.CurrentVersion)
	binary.LittleEndian.PutUint32(partial[6:10], tornFramePayloadLen)
	if _, err := f.Write(partial); err != nil {
		_ = f.Close()
		return fmt.Errorf("write torn header: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync torn header: %w", err)
	}
	return f.Close()
}

// sumAmounts returns the total of every transfer amount in ts.
func sumAmounts(ts []transfer) int64 {
	var s int64
	for _, t := range ts {
		s += t.amount
	}
	return s
}

// boolToInt maps a boolean invariant to the 1/0 form used for deterministic
// fact lines (recovery.balance_conserved, recovery.is_clean, …).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// walOffsetBytes converts a WAL byte offset (which is never negative) to the
// uint64 humanBytes expects.
func walOffsetBytes(off int64) uint64 {
	if off < 0 {
		return 0
	}
	return uint64(off) // G115: off is a guarded non-negative file offset
}
