package main

// phases.go — the four measurement phases. See README.md for what each one has to
// answer and why.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// scalingLevels are the writer counts phase 1 sweeps. One writer is the baseline
// every ratio is stated against.
var scalingLevels = []int{1, 2, 4, 8, 16}

// phaseWriterScaling answers question 1: does write throughput scale with writer
// count?
//
// Each level commits the SAME total number of orders, so the levels are comparable:
// a sweep that gave more writers more work would report a throughput rise that is
// just more work done. The hot fraction is held at the configured value across
// levels, so what varies is only the writer count.
func phaseWriterScaling(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintln(w, "## phase 1 — writer scaling")
	total := cfg.producers * cfg.opsPerProd

	var baseline float64
	for _, writers := range scalingLevels {
		if err := ctx.Err(); err != nil {
			return err
		}
		g := newGraph()
		if err := seedGraph(g, cfg); err != nil {
			_ = g.Close()
			return err
		}
		per := total / writers
		if per < 1 {
			per = 1
		}

		var wg sync.WaitGroup
		var committed, conflicts atomic.Int64
		start := time.Now()
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				sess := g.NewSession()
				// #nosec G115 -- writer count and goroutine id are small validated
				// loop bounds, not attacker input.
				r := newRand(cfg.seed, uint64(writers)*1000+uint64(id))
				for n := 0; n < per; n++ {
					cust := customerKey(r.IntN(cfg.customers))
					inv := ""
					if cfg.hotPct > 0 && r.IntN(100) < cfg.hotPct {
						inv = inventoryKey(r.IntN(cfg.inventory))
					}
					if _, err := commitOrder(g, sess, cust, inv, n); err != nil {
						conflicts.Add(1)
						continue
					}
					committed.Add(1)
				}
			}(i)
		}
		wg.Wait()
		elapsed := time.Since(start)
		_ = g.Close()

		rate := float64(committed.Load()) / elapsed.Seconds()
		if writers == 1 {
			baseline = rate
		}
		ratio := 0.0
		if baseline > 0 {
			ratio = rate / baseline
		}
		fmt.Fprintf(w, "# scaling writers=%-2d commits=%-6d elapsed=%-12s commits_per_sec=%-10.0f ratio_vs_1=%.2fx unrecovered_conflicts=%d\n",
			writers, committed.Load(), elapsed.Round(time.Millisecond), rate, ratio, conflicts.Load())
	}
	// DETERMINISTIC: every level must complete its work. The RATIO is volatile and
	// deliberately not asserted here — it is what the operator reads, and pinning it
	// would make this example fail on a busy machine for a reason unrelated to MVCC.
	fmt.Fprintf(w, "scaling.levels=%d\n", len(scalingLevels))
	return nil
}

// phaseContention answers questions 2, 4 and 5: what contention costs, what writers
// cost readers, and what versioning retains.
//
// Readers run BESIDE the writers, not after them, because a latency measured on a
// quiet graph is not the number anyone wants. The version-chain sampling likewise
// happens DURING the workload: sampled afterwards it would report what the vacuum
// has already reclaimed rather than what a read arriving mid-workload must walk.
func phaseContention(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintln(w, "## phase 2 — contention, reader latency, version retention")
	g := newGraph()
	defer func() { _ = g.Close() }()
	if err := seedGraph(g, cfg); err != nil {
		return err
	}

	var (
		wg          sync.WaitGroup
		committed   atomic.Int64
		retried     atomic.Int64
		retrySucc   atomic.Int64
		unrecovered atomic.Int64
		hotOrders   atomic.Int64
	)
	stop := make(chan struct{})

	// SAMPLER: version retention while the workload runs.
	var maxChainDepth, maxVersions, maxWriters atomic.Int64
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				st := g.MVCCStats()
				atomicMax(&maxVersions, st.Total)
				atomicMax(&maxWriters, int64(st.Write.Writers))
				// #nosec G115 -- a chain depth the substrate reports, bounded by the
				// reclamation ceiling.
				atomicMax(&maxChainDepth, int64(st.ChainDepth.Deepest))
			}
		}
	}()

	// READERS, beside the writers.
	readerLat := make([][]time.Duration, cfg.readers)
	for i := 0; i < cfg.readers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lat := make([]time.Duration, 0, 4096)
			// #nosec G115 -- reader index, a small validated loop bound.
			r := newRand(cfg.seed, 90000+uint64(id))
			for {
				select {
				case <-stop:
					readerLat[id] = lat
					return
				default:
				}
				k := customerKey(r.IntN(cfg.customers))
				t0 := time.Now()
				snap := g.BeginRead()
				_, _ = g.GetNodePropertyAsOf(k, "orders", snap)
				g.EndRead(snap)
				lat = append(lat, time.Since(t0))
			}
		}(i)
	}

	// WRITERS. They are BOUNDED, and they get their own WaitGroup: the readers and
	// the sampler loop until told to stop, so waiting on one group for both would
	// deadlock. Counting completions in a polling loop instead was tried and hung —
	// any path that neither commits nor fails (a self-transfer, a skipped order)
	// leaves the count short forever. A WaitGroup cannot miscount.
	var writers sync.WaitGroup
	start := time.Now()
	for i := 0; i < cfg.producers; i++ {
		writers.Add(1)
		go func(id int) {
			defer writers.Done()
			sess := g.NewSession()
			// #nosec G115 -- producer index, a small validated loop bound.
			r := newRand(cfg.seed, uint64(id))
			for n := 0; n < cfg.opsPerProd; n++ {
				if err := ctx.Err(); err != nil {
					return
				}
				cust := customerKey(r.IntN(cfg.customers))
				inv := ""
				if cfg.hotPct > 0 && r.IntN(100) < cfg.hotPct {
					inv = inventoryKey(r.IntN(cfg.inventory))
					hotOrders.Add(1)
				}
				n := n
				tries, err := commitOrder(g, sess, cust, inv, n)
				if tries > 0 {
					retried.Add(1)
				}
				switch {
				case err != nil:
					unrecovered.Add(1)
				default:
					committed.Add(1)
					if tries > 0 {
						retrySucc.Add(1)
					}
				}
			}
		}(i)
	}
	writers.Wait()
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	sampleWG.Wait()

	// Conflict rate BY STORE, from the substrate's own counters.
	st := g.MVCCStats()
	fmt.Fprintf(w, "# contention commits=%d elapsed=%s commits_per_sec=%.0f hot_orders=%d hot_pct_actual=%.1f\n",
		committed.Load(), elapsed.Round(time.Millisecond),
		float64(committed.Load())/elapsed.Seconds(), hotOrders.Load(),
		100*float64(hotOrders.Load())/float64(max64(1, committed.Load()+unrecovered.Load())))
	fmt.Fprintf(w, "# contention retried_orders=%d retry_succeeded=%d unrecovered=%d\n",
		retried.Load(), retrySucc.Load(), unrecovered.Load())
	fmt.Fprintf(w, "# conflicts.total=%d aborts=%d commits=%d\n", st.Write.Conflicts, st.Write.Aborts, st.Write.Commits)
	for i := 0; i < mvcc.ConflictStoreCount; i++ {
		if n := st.Write.ByStore[i]; n > 0 {
			fmt.Fprintf(w, "# conflicts.by_store.%s=%d\n", mvcc.ConflictStoreName(i), n)
		}
	}
	fmt.Fprintf(w, "# versions.max_retained=%d versions.bound=%d versions.ceiling=%d max_chain_depth=%d max_concurrent_writers=%d\n",
		maxVersions.Load(), st.Bound, st.Ceiling, maxChainDepth.Load(), maxWriters.Load())

	var all []time.Duration
	for _, l := range readerLat {
		all = append(all, l...)
	}
	p50, p95, p99 := percentiles(all)
	fmt.Fprintf(w, "# reader.samples=%d reader.p50=%s reader.p95=%s reader.p99=%s\n",
		len(all), p50, p95, p99)

	// DETERMINISTIC. Every order either commits or is reported unrecovered; none may
	// vanish. And a retry that never succeeds would mean the retry loop is decoration.
	fmt.Fprintf(w, "contention.accounted=%v\n",
		committed.Load()+unrecovered.Load() == int64(cfg.producers*cfg.opsPerProd))
	fmt.Fprintf(w, "contention.readers_sampled=%v\n", len(all) > 0)
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// transferAccounts is the conservation fixture's size. Small enough that transfers
// collide often, which is the point: the invariant has to hold UNDER contention.
const transferAccounts = 16

// startingBalance is each account's opening balance, in cents.
const startingBalance = 1_000_000

// seedAccounts creates the conservation fixture: every account at the same opening
// balance, so the invariant total is exactly accounts x startingBalance.
func seedAccounts(g *lpg.Graph[string, float64]) error {
	for i := 0; i < transferAccounts; i++ {
		k := acct(i)
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			wv := g.Writer(tx)
			if aerr := wv.AddNode(k); aerr != nil {
				return aerr
			}
			return wv.SetNodeProperty(k, "balance", lpg.Int64Value(startingBalance))
		}); err != nil {
			return fmt.Errorf("seed account %d: %w", i, err)
		}
	}
	return nil
}

// acct is the conservation fixture's account key.
func acct(i int) string { return fmt.Sprintf("acct-%02d", i) }

// observeTotal sums every account balance AT ONE INSTANT and reports the total.
//
// One snapshot for the whole sum is the entire point: summing with per-account
// present-time reads would straddle a transfer and report a total that no instant
// ever held, which is a defect in the observer rather than in the engine.
//
// It is a named function rather than an inline loop so the instrument itself can be
// tested against a deliberately imbalanced graph — see TestConservationCheckCanFail.
func observeTotal(g *lpg.Graph[string, float64]) int64 {
	snap := g.BeginRead()
	defer g.EndRead(snap)
	var sum int64
	for a := 0; a < transferAccounts; a++ {
		v, _ := g.GetNodePropertyAsOf(acct(a), "balance", snap)
		n, _ := v.Int64()
		sum += n
	}
	return sum
}

// phaseConservation answers question 3 with the self-contradictory check.
//
// A transfer debits one account and credits another in ONE transaction, so the TOTAL
// cannot change — whatever instant observes it, and whichever transactions were
// in flight. No external oracle is needed and none is consulted: the check is that
// the two halves of the same statement agree with each other.
//
// That property is what makes it able to detect a defect a bracketed observation
// cannot. A bracket confirms an observation sits between two acknowledged commits,
// which a STALE-BUT-VALID snapshot satisfies; a broken total cannot be explained by
// any legal instant.
//
// COST: one read of every account per observation, so O(accounts) per check, taken
// once per reader iteration over a 16-account fixture. That is why the fixture is
// small — the check has to be affordable in the gate that runs it, and a check
// nobody can afford to run is not a check.
func phaseConservation(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintln(w, "## phase 3 — conservation under concurrent transfers")
	g := newGraph()
	defer func() { _ = g.Close() }()
	if err := seedAccounts(g); err != nil {
		return err
	}
	wantTotal := int64(transferAccounts) * startingBalance

	var (
		wg        sync.WaitGroup
		transfers atomic.Int64
		refused   atomic.Int64
		torn      atomic.Int64
		observed  atomic.Int64
		firstTorn atomic.Int64
	)
	stop := make(chan struct{})

	// OBSERVERS: read every balance at ONE instant and sum them.
	for i := 0; i < maxInt(1, cfg.readers); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sum := observeTotal(g)
				observed.Add(1)
				if sum != wantTotal {
					torn.Add(1)
					firstTorn.CompareAndSwap(0, sum)
				}
			}
		}()
	}

	// TRANSFERRERS, bounded, on their own WaitGroup for the same reason as phase 2 —
	// and here the polling loop was actually WRONG rather than merely fragile: a
	// self-transfer is skipped without committing or being refused, so a count of
	// commits plus refusals never reaches the number of attempts.
	var movers sync.WaitGroup
	for i := 0; i < cfg.producers; i++ {
		movers.Add(1)
		go func(id int) {
			defer movers.Done()
			sess := g.NewSession()
			// #nosec G115 -- producer index, a small validated loop bound.
			r := newRand(cfg.seed, 500000+uint64(id))
			for n := 0; n < cfg.opsPerProd; n++ {
				if err := ctx.Err(); err != nil {
					return
				}
				from := r.IntN(transferAccounts)
				to := r.IntN(transferAccounts)
				if from == to {
					continue
				}
				amt := int64(1 + r.IntN(1000))
				deadline := time.Now().Add(retryBudget)
				var err error
				for {
					err = sess.ApplyVersioned(func(tx lpg.WriteTx) error {
						wv := g.Writer(tx)
						// Read through the TRANSACTION'S OWN VIEW, not the present.
						//
						// This is the read-modify-write that every conservation
						// invariant is built on, and reading it with the present-time
						// g.GetNodeProperty is a textbook LOST UPDATE: two transfers
						// read the same balance, both write, and the second silently
						// overwrites the first. The example was written that way first
						// and reported 97 torn observations and a final total of
						// 15 999 019 against 16 000 000 — money gone. The invariant
						// caught the example's own bug before it could be mistaken for
						// the engine's, which is the whole point of a check that needs
						// no external oracle.
						rv := g.WriterViewOf(tx)
						fv, _ := rv.GetNodeProperty(acct(from), "balance")
						tv, _ := rv.GetNodeProperty(acct(to), "balance")
						fb, _ := fv.Int64()
						tb, _ := tv.Int64()
						if fb < amt {
							return nil // insufficient funds: a no-op transfer still conserves
						}
						if serr := wv.SetNodeProperty(acct(from), "balance", lpg.Int64Value(fb-amt)); serr != nil {
							return serr
						}
						return wv.SetNodeProperty(acct(to), "balance", lpg.Int64Value(tb+amt))
					})
					if err == nil || !errors.Is(err, mvcc.ErrSerializationConflict) || time.Now().After(deadline) {
						break
					}
				}
				if err != nil {
					refused.Add(1)
					continue
				}
				transfers.Add(1)
			}
		}(i)
	}

	// Wait for the transferrers, then stop the observers.
	movers.Wait()
	close(stop)
	wg.Wait()

	// The final total, read with nothing in flight.
	final := observeTotal(g)

	fmt.Fprintf(w, "# conservation transfers=%d refused=%d observations=%d\n",
		transfers.Load(), refused.Load(), observed.Load())
	// DETERMINISTIC, and the headline: no observation may see a total that differs.
	fmt.Fprintf(w, "conservation.torn_observations=%d\n", torn.Load())
	fmt.Fprintf(w, "conservation.final_total_correct=%v\n", final == wantTotal)
	// NON-DEGENERACY: an observer that never ran, or a workload with no committed
	// transfer, would report zero torn observations while proving nothing.
	fmt.Fprintf(w, "conservation.observed_any=%v\n", observed.Load() > 0)
	fmt.Fprintf(w, "conservation.committed_any=%v\n", transfers.Load() > 0)
	if torn.Load() > 0 {
		fmt.Fprintf(w, "# conservation.first_torn_total=%d want=%d\n", firstTorn.Load(), wantTotal)
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// phaseRestart answers question 6: the data AND the MVCC commit clock survive.
//
// The clock matters as much as the data. A restored clock BELOW what a previous
// process published would let a post-restart transaction re-mint an instant that is
// already durable, so two different transactions would share one timestamp and a
// reader could not order them. rmp #2309 made recovery DERIVE the clock rather than
// persist a counter; this is the end-to-end check of that.
func phaseRestart(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintln(w, "## phase 4 — restart: the data and the MVCC clock")
	dir := cfg.storeDir
	if dir == "" {
		d, err := os.MkdirTemp("", "ex37-store-*")
		if err != nil {
			return fmt.Errorf("temp store dir: %w", err)
		}
		dir = d
		defer func() { _ = os.RemoveAll(dir) }()
	}

	const nodes = 200
	var beforeTS uint64
	if err := func() error {
		wlog, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			return fmt.Errorf("open wal: %w", err)
		}
		g := newGraph()
		st := txn.NewStoreWithOptions[string, float64](g, wlog, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		db := store.New(wlog, store.WithQuiesce(st.RunUnderCommitLock))
		defer func() { _ = db.Close() }()

		for i := 0; i < nodes; i++ {
			k := fmt.Sprintf("durable-%04d", i)
			tx := st.Begin()
			if aerr := tx.AddNode(k); aerr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("durable AddNode %d: %w", i, aerr)
			}
			if perr := tx.SetNodeProperty(k, "i", lpg.Int64Value(int64(i))); perr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("durable SetNodeProperty %d: %w", i, perr)
			}
			if cerr := tx.Commit(); cerr != nil {
				return fmt.Errorf("durable commit %d: %w", i, cerr)
			}
		}
		beforeTS = g.MVCCStats().Now
		return nil
	}(); err != nil {
		return err
	}

	sizeBefore, err := dirSize(dir)
	if err != nil {
		return err
	}

	// REOPEN — a fresh process would do exactly this.
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return fmt.Errorf("recover store: %w", err)
	}
	g2 := res.Graph

	var missing int
	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("durable-%04d", i)
		v, ok := g2.GetNodeProperty(k, "i")
		if n, _ := v.Int64(); !ok || n != int64(i) {
			missing++
		}
	}
	afterTS := g2.MVCCStats().Now

	// A post-restart commit must take an instant ABOVE everything the previous
	// process published — that is the property, not merely that the clock is
	// non-zero.
	if err := g2.ApplyVersioned(func(tx lpg.WriteTx) error {
		return g2.Writer(tx).AddNode("after-restart")
	}); err != nil {
		return fmt.Errorf("post-restart write: %w", err)
	}
	postTS := g2.MVCCStats().Now

	fmt.Fprintf(w, "# restart nodes=%d store_bytes=%d clock_before=%d clock_after_reopen=%d clock_after_write=%d\n",
		nodes, sizeBefore, beforeTS, afterTS, postTS)
	// DETERMINISTIC.
	fmt.Fprintf(w, "restart.nodes_recovered=%d\n", nodes-missing)
	fmt.Fprintf(w, "restart.all_nodes_recovered=%v\n", missing == 0)
	fmt.Fprintf(w, "restart.clock_not_rewound=%v\n", afterTS >= beforeTS)
	fmt.Fprintf(w, "restart.post_restart_instant_is_new=%v\n", postTS > beforeTS)
	_ = ctx
	return nil
}

// dirSize sums the size of every regular file under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure store size: %w", err)
	}
	return total, nil
}
