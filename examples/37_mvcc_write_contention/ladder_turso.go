package main

// ladder_turso.go — three ladder arms re-implemented from Turso's MVCC tests
// (tursodatabase/turso, read at d7f4945; MIT, "Copyright 2024 the Turso
// authors"); rows IX11, L21 and L22 of docs/mvcc-scenario-catalogue.md (rmp #3017,
// #3018).
//
//   - IX11 (load arm): one writer re-values an indexed property on every :L node
//     in one commit, again and again, while readers begin read transactions at
//     every instant, inside the commit's index delivery included, and seek. Turso
//     core/mvcc/database/tests.rs:4940, :5550.
//   - L21: a reader that starts while a goroutine runs ReclaimNow continuously
//     sees the node exactly once, at a value committed at or before its start.
//     Turso testing/stress/tests/shuttle_mvcc.rs:749 (the begin-publish window GC
//     hazard), core/mvcc/database/tests.rs:792.
//   - L22: bank transfers keep one total and one account count in every pinned
//     snapshot while checkpoints run back to back, and across a crash. Turso
//     core/mvcc/database/tests.rs:5403, :18953.
//
// Every arm runs on the persisted store (newLadderEngine: store.Open, WAL,
// durable commit) and is sized to the smallest workload that still fails against
// its seeded mutant (README.md, "Turso arms").

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ---------------------------------------------------------------------------
// IX11 — large indexed commit against concurrent seeks.

// ix11LoadNodes is the number of :L nodes every commit re-values: above the two
// changes the engine's index buffer holds inline, so every delivery is a batch.
// Each node's value carries its id plus 100, so up to 900 nodes no value is a
// prefix of another's (the btree seek is a prefix seek: the planner serves a
// parameterised btree equality by a scan). 64 is the planner's floor here, not
// the delivery's: at 32 and 48 nodes the btree prefix seek is planned as a label
// scan (measured), and seeks_planned_as_index would fail.
const ix11LoadNodes = 64

const (
	ix11LoadHash  = "CREATE INDEX ix11_s FOR (n:L) ON (n.s)"
	ix11LoadBtree = "CREATE INDEX ix11_b FOR (n:L) ON (n.b) OPTIONS {indexType: 'btree'}"
	ix11Values    = "MATCH (n:L) RETURN left(n.s, 7) AS g, count(*) AS c"
	ix11SeekS     = "MATCH (n:L {s:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE m.s + '' = $v RETURN seek, count(m) AS scan"
	ix11SeekB     = "MATCH (n:L) WHERE n.b STARTS WITH $v WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE left(m.b, size($v)) = $v RETURN seek, count(m) AS scan"
)

func ix11Gen(g int64) string { return fmt.Sprintf("g%06d", g) }

// ix11Key is the value node id holds at generation g.
func ix11Key(g int64, id int) string { return fmt.Sprintf("%s-%d", ix11Gen(g), id+100) }

func rowIX11Load(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	m, err := newLadderEngine("IX11", level, "store", lc.syncLatency)
	if err != nil {
		return err
	}
	defer m.close()
	for _, q := range []string{
		"UNWIND range(0, $n - 1) AS i CREATE (:L {id:i, s:$v + '-' + toString(i + 100), b:$v + '-' + toString(i + 100)})",
		ix11LoadHash, ix11LoadBtree,
	} {
		if err := mustRun(ctx, m.eng, q, P("n", ix11LoadNodes, "v", ix11Gen(0))); err != nil {
			return err
		}
	}
	// The arm compares what it claims to compare only if both seeks are planned
	// as index accesses.
	planned, unplanned := true, ""
	for _, q := range []string{ix11SeekS, ix11SeekB} {
		plan, err := m.eng.Explain(q, P("v", ix11Key(0, 0)))
		if err != nil {
			return err
		}
		if !strings.Contains(plan, "NodeByIndex") {
			planned, unplanned = false, q+":\n"+plan
		}
	}
	var (
		commits, reads, mixed, mismatches atomic.Int64
		writerErr, readerErr              atomic.Pointer[error]
		firstBad                          atomic.Pointer[string]
	)
	bad := func(format string, args ...any) {
		mismatches.Add(1)
		s := fmt.Sprintf(format, args...)
		firstBad.CompareAndSwap(nil, &s)
	}
	readersDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := int64(1); ; gen++ {
			select {
			case <-readersDone:
				return
			default:
			}
			if err := mustRun(ctx, m.eng, "MATCH (n:L) SET n.s = $v + '-' + toString(n.id + 100), n.b = $v + '-' + toString(n.id + 100)", P("v", ix11Gen(gen))); err != nil {
				writerErr.CompareAndSwap(nil, &err)
				return
			}
			commits.Add(1)
		}
	}()
	smp := startSampler(m.g, true)
	ops := lc.opsPerWorker(level)
	err = fanOut(ctx, level, func(ctx context.Context, _ int) error {
		for range ops {
			tx, err := m.eng.BeginReadTx(ctx)
			if err != nil {
				return err
			}
			rerr := func() error {
				rows, err := drain(tx.Exec(ix11Values, nil))
				if err != nil {
					return err
				}
				reads.Add(1)
				if len(rows) != 1 || intAt(rows, 0, 1) != ix11LoadNodes {
					mixed.Add(1)
					return nil
				}
				val := sval(rows[0][0])
				var gen int64
				if _, serr := fmt.Sscanf(val, "g%06d", &gen); serr != nil {
					return fmt.Errorf("IX11: unreadable value %q", val)
				}
				for _, c := range []struct {
					q    string
					v    string
					want int64
				}{
					{ix11SeekS, ix11Key(gen, 0), 1},
					{ix11SeekS, ix11Key(gen+1, 0), 0},
					{ix11SeekB, ix11Key(gen, ix11LoadNodes-1), 1},
					{ix11SeekB, ix11Key(gen+1, ix11LoadNodes-1), 0},
				} {
					rows, err := drain(tx.Exec(c.q, P("v", c.v)))
					if err != nil {
						return err
					}
					seek, scan := intAt(rows, 0, 0), intAt(rows, 0, 1)
					if seek != scan || scan != c.want {
						bad("snapshot at %s: %s [%s] seek=%d scan=%d want %d", val, c.q[:24], c.v, seek, scan, c.want)
					}
				}
				return nil
			}()
			if cerr := tx.Commit(); rerr == nil {
				rerr = cerr
			}
			if rerr != nil {
				readerErr.CompareAndSwap(nil, &rerr)
				return rerr
			}
		}
		return nil
	})
	close(readersDone)
	wg.Wait()
	smp.finish()
	if err != nil {
		return err
	}
	if p := writerErr.Load(); p != nil {
		return fmt.Errorf("IX11 writer: %w", *p)
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	first := ""
	if p := firstBad.Load(); p != nil {
		first = *p
	}
	out.tele("IX11", level, "nodes_per_commit", ix11LoadNodes, "large_commits", commits.Load(), "read_txns", reads.Load(),
		"mixed_snapshots", mixed.Load(), "seek_scan_mismatches", mismatches.Load())
	out.check("IX11", level, "seeks_planned_as_index", planned, "a seek of the arm is not planned as an index access: %s", unplanned)
	out.check("IX11", level, "commits_overlapped_reads", commits.Load() > 1 && reads.Load() > 0,
		"%d large commits, %d read transactions", commits.Load(), reads.Load())
	out.check("IX11", level, "never_mixed", mixed.Load() == 0, "%d snapshots saw old and new values together", mixed.Load())
	out.check("IX11", level, "seek_equals_scan", mismatches.Load() == 0, "%d mismatches; first: %s", mismatches.Load(), first)
	smp.report(out, "IX11", level, level)
	reportQuiesce(out, "IX11", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L21 — a reader starting against continuous reclamation.

func rowReclaimReader(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	m, err := newLadderEngine("L21", level, "store", lc.syncLatency)
	if err != nil {
		return err
	}
	defer m.close()
	if err := mustRun(ctx, m.eng, "CREATE (:N {k:0, v:0})", nil); err != nil {
		return err
	}
	// attempted is the value of the newest commit the writer has STARTED, acked
	// the value of the newest it has finished. A reader's snapshot must hold a
	// value in [acked before its BEGIN, attempted after its BEGIN].
	var attempted, acked atomic.Int64
	var commits, reclaimPasses, reclaimed atomic.Int64
	var writerErr atomic.Pointer[error]
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			attempted.Store(i)
			if err := mustRun(ctx, m.eng, "MATCH (n:N {k:0}) SET n.v = $i", P("i", i)); err != nil {
				writerErr.CompareAndSwap(nil, &err)
				return
			}
			acked.Store(i)
			commits.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reclaimed.Add(int64(m.g.ReclaimNow()))
			reclaimPasses.Add(1)
			runtime.Gosched()
		}
	}()
	const readsPerTx = 3
	// l21Hold is the pause between a reader's reads: about one commit of the
	// writer under the injected fsync latency.
	const l21Hold = 2 * time.Millisecond
	var reads, notOne, outOfRange, unrepeatable atomic.Int64
	var firstBad atomic.Pointer[string]
	note := func(c *atomic.Int64, format string, args ...any) {
		c.Add(1)
		s := fmt.Sprintf(format, args...)
		firstBad.CompareAndSwap(nil, &s)
	}
	smp := startSampler(m.g, true)
	ops := lc.opsPerWorker(level)
	err = fanOut(ctx, level, func(ctx context.Context, _ int) error {
		for range ops {
			lo := acked.Load()
			tx, err := m.eng.BeginReadTx(ctx)
			if err != nil {
				return err
			}
			hi := attempted.Load()
			first := int64(-1)
			for r := range readsPerTx {
				rows, rerr := drain(tx.Exec("MATCH (n:N {k:0}) RETURN n.v AS v", nil))
				if rerr != nil {
					_ = tx.Rollback()
					return rerr
				}
				reads.Add(1)
				if len(rows) != 1 {
					note(&notOne, "read %d returned %d rows", r, len(rows))
					continue
				}
				v := intAt(rows, 0, 0)
				if v < lo || v > hi {
					note(&outOfRange, "read %d saw v=%d outside [%d, %d]", r, v, lo, hi)
				}
				if first < 0 {
					first = v
				} else if v != first {
					note(&unrepeatable, "read %d saw v=%d after v=%d", r, v, first)
				}
				// Hold the snapshot across a commit, so the reclaimer runs while a
				// version this reader needs has been superseded.
				time.Sleep(l21Hold)
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		return nil
	})
	close(stop)
	wg.Wait()
	smp.finish()
	if err != nil {
		return err
	}
	if p := writerErr.Load(); p != nil {
		return fmt.Errorf("L21 writer: %w", *p)
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	first := ""
	if p := firstBad.Load(); p != nil {
		first = *p
	}
	out.tele("L21", level, "commits", commits.Load(), "reads", reads.Load(), "reclaim_passes", reclaimPasses.Load(),
		"versions_reclaimed", reclaimed.Load())
	out.check("L21", level, "reclaimed_during_reads", commits.Load() > 0 && reclaimed.Load() > 0 && reads.Load() > 0,
		"%d commits, %d versions reclaimed, %d reads", commits.Load(), reclaimed.Load(), reads.Load())
	out.check("L21", level, "node_seen_exactly_once", notOne.Load() == 0, "%d reads; first anomaly of any kind: %s", notOne.Load(), first)
	out.check("L21", level, "value_committed_by_start", outOfRange.Load() == 0, "%d reads; first anomaly of any kind: %s", outOfRange.Load(), first)
	out.check("L21", level, "repeatable", unrepeatable.Load() == 0, "%d reads; first anomaly of any kind: %s", unrepeatable.Load(), first)
	smp.report(out, "L21", level, level)
	reportQuiesce(out, "L21", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L22 — snapshot invariants while checkpoints run, and across a crash.

const (
	l22MinAccounts = 8
	l22Balance     = 100
	l22Sum         = "MATCH (a:Acct) RETURN sum(a.bal) AS s, count(a) AS c"
)

func rowBankCheckpoint(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	// Two accounts per writer, at least eight: enough contention that transfers
	// are refused and retried, not so much that the retry budget is approached.
	accounts := max(l22MinAccounts, 2*level)
	m, err := newLadderEngine("L22", level, "store", lc.syncLatency)
	if err != nil {
		return err
	}
	defer m.close()
	img := m.dir + "-img"
	defer func() { _ = os.RemoveAll(img) }()
	if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:Acct {id:i, bal:$b})",
		P("n", accounts, "b", l22Balance)); err != nil {
		return err
	}
	total := int64(accounts) * l22Balance
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: m.dir}, m.g, m.o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](m.o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](m.eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](m.eng.IndexSpecsForSnapshot))
	// cpMu keeps the crash image out of a checkpoint's lock-free phases: an image
	// is taken between checkpoints, as a crash between them would leave the
	// directory.
	var cpMu sync.Mutex
	var cpOK, cpErrs atomic.Int64
	var cpFirstErr atomic.Pointer[error]
	writersDone := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-writersDone:
				return
			default:
			}
			cpMu.Lock()
			err := cp.RunCheckpoint()
			cpMu.Unlock()
			if err != nil {
				cpErrs.Add(1)
				cpFirstErr.CompareAndSwap(nil, &err)
			} else {
				cpOK.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// The acknowledged transfers, in acknowledgement order.
	var ackMu sync.Mutex
	var acks []int64
	// Readers: pinned read transactions that repeat sum and count.
	var snaps, rereads, broken atomic.Int64
	var firstBroken atomic.Pointer[string]
	readers := max(1, min(level, 8))
	bg.Add(readers)
	for range readers {
		go func() {
			defer bg.Done()
			for {
				select {
				case <-writersDone:
					return
				default:
				}
				tx, err := m.eng.BeginReadTx(ctx)
				if err != nil {
					return
				}
				snaps.Add(1)
				var s0, c0 int64 = -1, -1
				for r := range 3 {
					rows, rerr := drain(tx.Exec(l22Sum, nil))
					if rerr != nil {
						break
					}
					rereads.Add(1)
					s, c := intAt(rows, 0, 0), intAt(rows, 0, 1)
					if s != total || c != int64(accounts) || (r > 0 && (s != s0 || c != c0)) {
						broken.Add(1)
						d := fmt.Sprintf("read %d sum=%d count=%d (first %d/%d)", r, s, c, s0, c0)
						firstBroken.CompareAndSwap(nil, &d)
					}
					s0, c0 = s, c
					time.Sleep(time.Millisecond) // let a checkpoint land between the reads
				}
				_ = tx.Rollback()
			}
		}()
	}
	var st txStats
	var imgBefore int
	var imgErr error
	var imgTaken, imgAfterCheckpoint atomic.Bool
	ops := lc.opsPerWorker(level)
	half := ops * level / 2
	t0 := time.Now()
	err = fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x2200+uint64(gid)) // #nosec G115 -- small id
		for k := range ops {
			id := int64(gid)*1_000_000 + int64(k)
			i := rng.IntN(accounts)
			j := (i + 1 + rng.IntN(accounts-1)) % accounts
			amt := 1 + rng.IntN(5)
			rerr := st.retry(ctx, func() error {
				return inTx(ctx, m.eng, true, func(tx *cypher.ExplicitTx) error {
					_, e := drain(tx.Exec("MATCH (a:Acct {id:$i}), (b:Acct {id:$j}) "+
						"SET a.bal = a.bal - $x, b.bal = b.bal + $x CREATE (:Tr {id:$t})",
						P("i", i, "j", j, "x", amt, "t", id)))
					return e
				})
			})
			if werr := workerErr(rerr); werr != nil {
				return werr
			}
			if rerr != nil {
				// Refused past the retry budget: not acknowledged, so not owed.
				continue
			}
			ackMu.Lock()
			acks = append(acks, id)
			n := len(acks)
			ackMu.Unlock()
			// The crash image: once, half-way, after a checkpoint has completed,
			// between checkpoints, and under the commit lock so the acknowledged
			// count and the durable offset describe one instant.
			if n >= half && cpOK.Load() > 0 && imgTaken.CompareAndSwap(false, true) {
				cpMu.Lock()
				imgAfterCheckpoint.Store(cpOK.Load() > 0)
				imgErr = m.o.Store().RunUnderCommitLock(func() error {
					ackMu.Lock()
					imgBefore = len(acks)
					ackMu.Unlock()
					_, cerr := copyTree(m.dir, img, m.o.WAL().DurableOffset())
					return cerr
				})
				cpMu.Unlock()
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	close(writersDone)
	bg.Wait()
	if err != nil {
		return err
	}
	if imgErr != nil {
		return fmt.Errorf("L22 image: %w", imgErr)
	}
	reportTx(out, "L22", level, "sessionless", &st, elapsed)
	cpErrText := ""
	if p := cpFirstErr.Load(); p != nil {
		cpErrText = (*p).Error()
	}
	first := ""
	if p := firstBroken.Load(); p != nil {
		first = *p
	}
	out.tele("L22", level, "checkpoints", cpOK.Load(), "checkpoint_errors", cpErrs.Load(),
		"pinned_snapshots", snaps.Load(), "rereads", rereads.Load(), "acked_at_image", imgBefore)
	out.check("L22", level, "checkpoints_ran", cpOK.Load() > 0 && cpErrs.Load() == 0,
		"%d checkpoints, %d errors; first: %s", cpOK.Load(), cpErrs.Load(), cpErrText)
	out.check("L22", level, "snapshots_pinned_across_reads", rereads.Load() > snaps.Load() && snaps.Load() > 0,
		"%d snapshots, %d reads", snaps.Load(), rereads.Load())
	out.check("L22", level, "snapshot_total_and_count_constant", broken.Load() == 0,
		"%d reads broke the invariant; first: %s", broken.Load(), first)
	// Transfers that exhaust the retry budget are telemetry (reportTx prints
	// unrecovered), as in every ladder arm: a liveness figure, not an invariant.
	out.check("L22", level, "no_unexpected_errors", st.otherErrs.Load() == 0,
		"other errors %d; first: %s", st.otherErrs.Load(), st.errText())
	out.check("L22", level, "crash_image_after_checkpoint", imgTaken.Load() && imgAfterCheckpoint.Load(),
		"image taken %v, after a checkpoint %v", imgTaken.Load(), imgAfterCheckpoint.Load())
	if !imgTaken.Load() {
		return nil
	}
	ackMu.Lock()
	owed := append([]int64(nil), acks[:imgBefore]...)
	ackMu.Unlock()
	return verifyBankImage(ctx, out, level, img, owed, total, int64(accounts))
}

// verifyBankImage recovers img read-only and holds it to the L22 crash gates:
// the total and the account count unchanged, and every transfer acknowledged
// before the image present.
func verifyBankImage(ctx context.Context, out *ladderOut, level int, img string, owed []int64, total, accounts int64) error {
	res, err := recovery.OpenCtx[string, float64](ctx, img, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if res.Graph == nil {
		out.check("L22.crash", level, "recovered", false, "recovery: %v", err)
		return nil
	}
	eng := cypher.NewEngine(res.Graph)
	defer func() { _ = eng.Close() }()
	rows, qerr := drain(eng.Run(ctx, l22Sum, nil))
	if qerr != nil {
		return qerr
	}
	sum, cnt := intAt(rows, 0, 0), intAt(rows, 0, 1)
	trs, qerr := drain(eng.Run(ctx, "MATCH (t:Tr) RETURN t.id AS id", nil))
	if qerr != nil {
		return qerr
	}
	present := make(map[int64]bool, len(trs))
	for r := range trs {
		present[intAt(trs, r, 0)] = true
	}
	missing := 0
	firstMissing := int64(-1)
	for _, id := range owed {
		if !present[id] {
			missing++
			if firstMissing < 0 {
				firstMissing = id
			}
		}
	}
	out.tele("L22.crash", level, "snapshot_hit", res.SnapshotHit, "acked_owed", len(owed), "transfers_recovered", len(present),
		"recovery_clean", res.IsClean())
	out.check("L22.crash", level, "recovered", err == nil && res.IsClean(), "recovery error %v, clean %v", err, res.IsClean())
	out.check("L22.crash", level, "snapshot_used", res.SnapshotHit, "recovery did not load the checkpoint's snapshot")
	out.check("L22.crash", level, "total_and_count_unchanged", sum == total && cnt == accounts,
		"recovered sum=%d count=%d, want %d/%d", sum, cnt, total, accounts)
	out.check("L22.crash", level, "acked_present", missing == 0,
		"%d of %d acknowledged transfers missing; first %d", missing, len(owed), firstMissing)
	return nil
}
