package main

// durability_turso.go — phase-7 arms D17, D18 and D19 (rmp #3019;
// docs/mvcc-scenario-catalogue.md §3), re-implemented from Turso's MVCC tests
// (tursodatabase/turso, read at d7f4945; MIT, "Copyright 2024 the Turso authors"):
//
//   - D17, torn tail, recover, append, recover again: core/mvcc/database/tests.rs
//     test_recovery_overwrites_torn_tail_on_next_append (2751), :2800;
//     core/mvcc/persistent_storage/logical_log.rs:4741.
//   - D18, a checkpoint against a commit that has allocated its instant and not
//     published it: core/mvcc/database/tests.rs
//     test_checkpoint_snapshot_ts_clamps_below_inflight_preparing (3302).
//   - D19, a commit cancelled at each phase, and no ghost commit:
//     core/mvcc/database/group_commit_tests.rs:378; tests.rs:14581;
//     testing/stress/tests/shuttle_mvcc.rs:349.
//
// # D18 and the commit-hold seam (H2)
//
// The catalogue asked whether H2 (lpg.Graph.AllocateCommitTS) can hold a commit
// INSIDE store/txn between its fsync and its publish. It cannot: Tx.CommitCtx runs
// lpg.Graph.ApplyDurable, whose durable step appends and fsyncs with commit
// timestamp 0 and publishes when the apply bracket ends, with no caller-visible
// point between the two (store/txn/txn.go, commitDurable). The Cypher engine's
// commit (txn.Tx.CommitWALOnly plus an lpg bracket) does allocate through
// AllocateCommitTS between its fsync and its publish, but offers no hook to hold
// it there either. D18 therefore holds the window the checkpoint must wait out
// with H2 directly on the store's graph: an lpg transaction that has allocated a
// commit instant and not published it is exactly the state
// lpg.Graph.AwaitCommitQuiescence exists to wait for, whatever path allocated it.
// It writes nothing, so nothing outside the WAL can reach the snapshot.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// tursoTxnsPerWriter is the commits each writer makes per epoch or phase in the
// Turso arms: the smallest count that gives every writer more than one commit.
const tursoTxnsPerWriter = 2

// openTursoStore opens a durable store with lat as its fsync latency.
func openTursoStore(dir string, lat *wal.SyncLatency) (*store.Opened[string, float64], error) {
	return store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		SyncLatency: lat,
	})
}

// recoverEpochs runs recovery over dir read-only and returns every :E node's id
// and epoch.
func recoverEpochs(ctx context.Context, dir string) (map[int64]int64, recovery.Result[string, float64], error) {
	res, err := recovery.OpenCtx[string, float64](ctx, dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if res.Graph == nil {
		return nil, res, err
	}
	eng := cypher.NewEngine(res.Graph)
	defer func() { _ = eng.Close() }()
	rows, qerr := drain(eng.Run(ctx, "MATCH (n:E) RETURN n.id AS id, n.ep AS ep", nil))
	if qerr != nil {
		return nil, res, qerr
	}
	got := make(map[int64]int64, len(rows))
	for r := range rows {
		got[intAt(rows, r, 0)] = intAt(rows, r, 1)
	}
	return got, res, err
}

// runEpoch has n writers each commit tursoTxnsPerWriter autocommit CREATEs of
// epoch ep, and returns the acknowledged ids.
func runEpoch(ctx context.Context, eng *cypher.Engine, n int, ep int64) ([]int64, error) {
	var mu sync.Mutex
	var acked []int64
	err := fanOut(ctx, n, func(ctx context.Context, gid int) error {
		for k := range tursoTxnsPerWriter {
			id := ep*1_000_000 + int64(gid)*1_000 + int64(k)
			if err := mustRun(ctx, eng, "CREATE (:E {ep:$e, id:$id})", P("e", ep, "id", id)); err != nil {
				return err
			}
			mu.Lock()
			acked = append(acked, id)
			mu.Unlock()
		}
		return nil
	})
	return acked, err
}

// ---------------------------------------------------------------------------
// D17 — torn tail, recover, append, recover again.

func armTornAppend(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	dir, err := storeDirFor("D17", level, "live")
	if err != nil {
		return err
	}
	torn, second := dir+"-torn", dir+"-epoch2"
	defer func() {
		for _, d := range []string{dir, torn, second} {
			_ = os.RemoveAll(d)
		}
	}()
	o, err := openTursoStore(dir, dc.syncLatency)
	if err != nil {
		return err
	}
	eng := cypher.NewEngineWithOpened(o)
	acked1, err := runEpoch(ctx, eng, level, 1)
	if err == nil {
		// Every acknowledgement is durable: the image is the directory a crash
		// right now leaves.
		_, err = copyTree(dir, torn, o.WAL().DurableOffset())
	}
	_ = o.Close()
	_ = eng.Close()
	if err != nil {
		return err
	}
	// The crash tore the last frame: its final bytes never reached the device.
	frames, err := walFrames(filepath.Join(torn, walFile))
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		out.check("D17", level, "wal_has_frames", false, "the epoch-1 image has no frame")
		return nil
	}
	last := frames[len(frames)-1]
	rel, err := filepath.Rel(torn, last.path)
	if err != nil {
		return err
	}
	if err := truncateFile(filepath.Join(torn, rel), last.fileOff+last.size-3); err != nil {
		return err
	}
	r1, _, err := recoverEpochs(ctx, torn)
	if err != nil || r1 == nil {
		return fmt.Errorf("D17: recover the torn image: %w", err)
	}
	lost1 := 0
	for _, id := range acked1 {
		if _, ok := r1[id]; !ok {
			lost1++
		}
	}
	// Epoch 2: the torn image opened for writing (recovery, then wal.Open
	// discards the torn tail), more commits, and a second crash.
	o2, err := openTursoStore(torn, dc.syncLatency)
	if err != nil {
		out.check("D17", level, "reopens_over_torn_tail", false, "store.Open: %v", err)
		return nil
	}
	eng2 := cypher.NewEngineWithOpened(o2)
	acked2, err := runEpoch(ctx, eng2, level, 2)
	if err == nil {
		_, err = copyTree(torn, second, o2.WAL().DurableOffset())
	}
	_ = o2.Close()
	_ = eng2.Close()
	if err != nil {
		return err
	}
	r2, res2, err := recoverEpochs(ctx, second)
	if r2 == nil {
		out.check("D17", level, "second_recovery_opens", false, "recovery: %v", err)
		return nil
	}
	survivorsMissing, gained, ep2Missing, ep1In2 := 0, 0, 0, 0
	for id := range r1 {
		if _, ok := r2[id]; !ok {
			survivorsMissing++
		}
	}
	for id, ep := range r2 {
		if ep == 1 {
			ep1In2++
			if _, ok := r1[id]; !ok {
				gained++
			}
		}
	}
	for _, id := range acked2 {
		if _, ok := r2[id]; !ok {
			ep2Missing++
		}
	}
	out.tele("D17", level, "epoch1_acked", len(acked1), "epoch1_recovered", len(r1), "epoch1_lost_to_tear", lost1,
		"epoch2_acked", len(acked2), "epoch1_after_second_recovery", ep1In2, "epoch2_missing", ep2Missing,
		"second_recovery_clean", res2.IsClean())
	out.check("D17", level, "torn_record_discarded_alone", lost1 <= 1,
		"the tear lost %d acknowledged epoch-1 commits; one frame holds at most one transaction's marker", lost1)
	out.check("D17", level, "epoch1_survives_second_recovery", survivorsMissing == 0 && gained == 0,
		"%d epoch-1 commits missing, %d gained after the second recovery", survivorsMissing, gained)
	out.check("D17", level, "epoch2_present", ep2Missing == 0 && err == nil && res2.IsClean(),
		"%d of %d acknowledged epoch-2 commits missing (recovery error %v, clean %v): the second recovery stopped at the first epoch's torn bytes",
		ep2Missing, len(acked2), err, res2.IsClean())
	return nil
}

// ---------------------------------------------------------------------------
// D18 — a checkpoint against a held, unpublished commit instant.

// d18Hold is how long the commit instant is held with the checkpoint started:
// long enough for an unguarded checkpoint to finish, which takes well under a
// millisecond on this graph.
const d18Hold = 50 * time.Millisecond

func armHeldCheckpoint(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	dir, err := storeDirFor("D18", level, "live")
	if err != nil {
		return err
	}
	img := dir + "-img"
	defer func() {
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(img)
	}()
	o, err := openTursoStore(dir, dc.syncLatency)
	if err != nil {
		return err
	}
	eng := cypher.NewEngineWithOpened(o)
	closeAll := func() {
		_ = o.Close()
		_ = eng.Close()
	}
	acked, err := runEpoch(ctx, eng, level, 1)
	if err != nil {
		closeAll()
		return err
	}
	g := o.Graph()
	// T0: an instant allocated and not published (H2).
	sess := g.NewSession()
	t0, err := sess.BeginVersionedTx()
	if err != nil {
		closeAll()
		return err
	}
	released := false
	release := func() {
		if !released {
			released = true
			sess.EndVersionedTx(t0)
		}
	}
	defer release()
	if g.AllocateCommitTS(t0) == 0 {
		release()
		closeAll()
		return errors.New("D18: AllocateCommitTS reserved no instant")
	}
	// T1 commits above T0's instant: its acknowledgement does not wait on T0.
	t1 := int64(9_000_000)
	t1Err := mustRun(ctx, eng, "CREATE (:E {ep:1, id:$id})", P("id", t1))
	if t1Err == nil {
		acked = append(acked, t1)
	}
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, g, o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot))
	cpDone := make(chan error, 1)
	go func() { cpDone <- cp.RunCheckpoint() }()
	var finishedWhileHeld bool
	var heldErr error
	select {
	case heldErr = <-cpDone:
		finishedWhileHeld = true
	case <-time.After(d18Hold):
	}
	heldStats := cp.Stats()
	release()
	cpErr := heldErr
	if !finishedWhileHeld {
		select {
		case cpErr = <-cpDone:
		case <-time.After(hangBudget):
			closeAll()
			return fmt.Errorf("D18: the checkpoint did not finish within %s of the release", hangBudget)
		}
	}
	// The crash after the checkpoint: the directory as it stands, under the
	// commit lock so no commit is mid-flight.
	ierr := o.Store().RunUnderCommitLock(func() error {
		_, e := copyTree(dir, img, o.WAL().DurableOffset())
		return e
	})
	closeAll()
	if ierr != nil {
		return ierr
	}
	got, res, rerr := recoverEpochs(ctx, img)
	missing := -1
	if got != nil {
		missing = 0
		for _, id := range acked {
			if _, ok := got[id]; !ok {
				missing++
			}
		}
	}
	out.tele("D18", level, "held_for", d18Hold, "finished_while_held", finishedWhileHeld,
		"checkpoints_while_held", heldStats.Checkpoints, "wal_truncated_bytes_while_held", heldStats.WALTruncBytes,
		"checkpoint_error", fmt.Sprintf("%q", errText(cpErr)), "snapshot_hit", res.SnapshotHit, "acked", len(acked))
	out.check("D18", level, "t1_acked_while_t0_held", t1Err == nil, "T1's commit: %v", t1Err)
	out.check("D18", level, "checkpoint_waited", !finishedWhileHeld && heldStats.WALTruncBytes == 0,
		"the checkpoint finished (err %v) while an allocated instant was unpublished, truncated %d bytes",
		heldErr, heldStats.WALTruncBytes)
	out.check("D18", level, "checkpoint_ran_after_release", cpErr == nil, "checkpoint: %v", cpErr)
	out.check("D18", level, "acked_present_after_crash", missing == 0 && rerr == nil && res.SnapshotHit,
		"%d of %d acknowledged commits missing (recovery error %v, snapshot hit %v)", missing, len(acked), rerr, res.SnapshotHit)
	return nil
}

// ---------------------------------------------------------------------------
// D19 — CommitCtx cancelled at each phase; no ghost commit.

func armCancelPhases(ctx context.Context, dc *durabilityConfig, out *ladderOut, level int) error {
	dir, err := storeDirFor("D19", level, "live")
	if err != nil {
		return err
	}
	img := dir + "-img"
	defer func() {
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(img)
	}()
	o, err := openTursoStore(dir, dc.syncLatency)
	if err != nil {
		return err
	}
	eng := cypher.NewEngineWithOpened(o)
	closeAll := func() {
		_ = o.Close()
		_ = eng.Close()
	}
	st := o.Store()
	// The cancellation delay of phase (b) is drawn across the commit's fsync.
	hi := 2 * time.Millisecond
	if dc.syncLatency != nil {
		_, hi = dc.syncLatency.Bounds()
		hi *= 2
	}
	var okMu sync.Mutex
	ok := map[int64]bool{}
	var aRefused, aNotRefused, aRetryFailed, bOK, bCancelled, cFailed, otherErrs atomic.Int64
	var firstErr atomic.Pointer[error]
	other := func(err error) {
		otherErrs.Add(1)
		firstErr.CompareAndSwap(nil, &err)
	}
	commit := func(c context.Context, id int64) error {
		key := fmt.Sprintf("g%08d", id)
		tx := st.Begin()
		if err := tx.AddNode(key); err != nil {
			return err
		}
		if err := tx.SetNodeProperty(key, "gid", lpg.Int64Value(id)); err != nil {
			return err
		}
		return tx.CommitCtx(c)
	}
	acked := func(id int64) {
		okMu.Lock()
		ok[id] = true
		okMu.Unlock()
	}
	err = fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(dc.seed, 0x1900+uint64(gid)) // #nosec G115 -- small id
		for k := range 3 * tursoTxnsPerWriter {
			id := int64(gid)*1_000 + int64(k)*2
			switch k % 3 {
			case 0: // (a) cancelled before the apply takes its claims
				c, cancel := context.WithCancel(ctx)
				cancel()
				switch err := commit(c, id); {
				case errors.Is(err, context.Canceled):
					aRefused.Add(1)
				case err == nil:
					aNotRefused.Add(1)
					acked(id)
				default:
					other(err)
				}
				// A retry, as a new transaction, commits.
				if err := commit(ctx, id+1); err != nil {
					aRetryFailed.Add(1)
					other(err)
				} else {
					acked(id + 1)
				}
			case 1: // (b) cancelled at a random point across the commit
				c, cancel := context.WithCancel(ctx)
				delay := time.Duration(rng.Int64N(int64(hi) + 1))
				timer := time.AfterFunc(delay, cancel)
				err := commit(c, id)
				timer.Stop()
				cancel()
				switch {
				case err == nil:
					bOK.Add(1)
					acked(id)
				case errors.Is(err, context.Canceled):
					bCancelled.Add(1)
				default:
					other(err)
				}
			default: // (c) cancelled after the commit returned
				c, cancel := context.WithCancel(ctx)
				err := commit(c, id)
				cancel()
				if err != nil {
					cFailed.Add(1)
					other(err)
				} else {
					acked(id)
				}
			}
		}
		return nil
	})
	if err != nil {
		closeAll()
		return err
	}
	// Later commits are never blocked: one more, with a deadline.
	lctx, lcancel := context.WithTimeout(ctx, hangBudget)
	laterErr := commit(lctx, 99_999_998)
	lcancel()
	if laterErr == nil {
		acked(99_999_998)
	}
	compare := func(present map[int64]bool) (ghost, lost int) {
		for id := range present {
			if !ok[id] {
				ghost++
			}
		}
		for id := range ok {
			if !present[id] {
				lost++
			}
		}
		return ghost, lost
	}
	readGids := func(e *cypher.Engine) (map[int64]bool, error) {
		rows, err := drain(e.Run(ctx, "MATCH (n) WHERE n.gid IS NOT NULL RETURN n.gid AS g", nil))
		if err != nil {
			return nil, err
		}
		m := make(map[int64]bool, len(rows))
		for r := range rows {
			m[intAt(rows, r, 0)] = true
		}
		return m, nil
	}
	mem, err := readGids(eng)
	if err == nil {
		_, err = copyTree(dir, img, o.WAL().DurableOffset())
	}
	closeAll()
	if err != nil {
		return err
	}
	memGhost, memLost := compare(mem)
	res, rerr := recovery.OpenCtx[string, float64](ctx, img, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	crashGhost, crashLost := -1, -1
	if res.Graph != nil {
		re := cypher.NewEngine(res.Graph)
		rec, qerr := readGids(re)
		_ = re.Close()
		if qerr != nil {
			return qerr
		}
		crashGhost, crashLost = compare(rec)
	}
	first := ""
	if p := firstErr.Load(); p != nil {
		first = (*p).Error()
	}
	out.tele("D19", level, "ok", len(ok), "committed_in_memory", len(mem), "a_refused", aRefused.Load(),
		"b_ok", bOK.Load(), "b_cancelled", bCancelled.Load(), "b_cancel_window", hi,
		"ghost_in_memory", memGhost, "lost_in_memory", memLost, "ghost_after_crash", crashGhost, "lost_after_crash", crashLost)
	out.check("D19", level, "a_refused_with_canceled", aNotRefused.Load() == 0 && aRefused.Load() > 0,
		"%d commits with a cancelled context succeeded, %d refused", aNotRefused.Load(), aRefused.Load())
	out.check("D19", level, "a_retry_commits", aRetryFailed.Load() == 0, "%d retries failed", aRetryFailed.Load())
	out.check("D19", level, "c_completes", cFailed.Load() == 0, "%d commits failed", cFailed.Load())
	out.check("D19", level, "no_unexpected_errors", otherErrs.Load() == 0, "%d; first: %s", otherErrs.Load(), first)
	out.check("D19", level, "committed_equals_ok_in_memory", memGhost == 0 && memLost == 0,
		"%d ghost commits (applied, reported failed), %d lost (reported OK, absent)", memGhost, memLost)
	out.check("D19", level, "committed_equals_ok_after_crash", crashGhost == 0 && crashLost == 0 && rerr == nil,
		"%d ghost, %d lost after recovery (error %v)", crashGhost, crashLost, rerr)
	out.check("D19", level, "later_commit_not_blocked", laterErr == nil, "commit after the run: %v", laterErr)
	return nil
}
