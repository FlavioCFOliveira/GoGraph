// Example 17_transactional_log — a durable financial ledger: a WAL-backed
// store with a background checkpointer that folds the log into a
// self-sufficient on-disk snapshot, plus recovery after a simulated crash.
//
// The flow: open a write-ahead log, build a [txn.Store] over it with a node
// codec and an int64 weight codec, commit a seeded stream of ledger transfers
// — one transaction each — while a background checkpointer folds the WAL tail
// into a self-sufficient snapshot and truncates the log, then simulate a
// crash (every in-memory reference is abandoned) and recover the ledger from
// disk alone, verifying that every committed transfer comes back with its
// exact amount.
//
// # Model
//
//	(:ACCOUNT {id})                              // id is a 24-char hex string
//	(:ACCOUNT)-[transfer]->(:ACCOUNT)            // weight = amount in cents
//
// A transfer is a directed edge whose WEIGHT carries the transfer amount in
// minor currency units (integer cents). The amount is therefore part of the
// durable record: it is committed via [txn.Tx.AddEdge] (which a weight-codec
// store emits as an [txn.OpAddEdgeH] frame carrying the int64 amount through
// the [txn.WeightCodec]) and read back after recovery with
// [lpg.Graph.EdgeWeight]. No redundant "amount" property is stored — the edge
// weight is the single source of truth, so the example exercises the WAL
// weighted-edge op and the weight-codec recovery path end to end.
//
// The ledger is a SIMPLE directed graph: at most one transfer per ordered
// (src, dst) pair and no self-loops. That keeps the per-amount verification
// unambiguous — EdgeWeight returns the weight of the first edge for a pair, so
// one edge per pair means EdgeWeight(src, dst) is exactly that transfer's
// amount.
//
// After recovery the example runs a double-entry reconciliation
// (reconcileNetPositions) that reconstructs each account's net position —
// inflows minus outflows — by walking the RECOVERED graph's own edges, and
// checks it against the net position obtained by replaying the plan. Because
// the reconstruction reads the graph's edge set and weights directly, not the
// plan's, it is a genuine invariant that would fail if a recovered edge were
// spurious, duplicated, missing, or attributed to the wrong endpoint — it is
// not the same accumulator counted twice. debit_sum (outflows per source) and
// credit_sum (inflows per destination) are reported from that same walk; for a
// balanced ledger they are equal and both equal the sum of committed amounts.
//
// # ACID coordination
//
// The checkpointer snapshots the live graph; if it read concurrently with a
// transaction's in-memory apply it could persist a partially-applied
// transaction and violate Atomicity. This example drives the [txn.Store]
// directly, so it hands the checkpointer the store's own commit serialiser via
// [checkpoint.WithCommitSerialiser] ([txn.Store.RunUnderCommitLock]): the
// checkpointer runs its snapshot-capture and WAL-truncate critical section
// under the store's private single-writer lock, so no transaction can be
// between Begin and Commit while a snapshot is taken or the WAL is truncated.
// [checkpoint.WithMapperCodec] persists the NodeID->key mapper so the
// string-keyed snapshot is self-sufficient and the WAL can be safely truncated
// after each checkpoint.
//
// # Crash modes
//
// The example ships two crash demonstrations. The default is an in-process
// crash: run drives the commit stream, abandons every in-memory reference, and
// reopens the store from disk. It is fast and deterministic, so the regression
// test pins it — but it never exercises OS-level torn-frame handling, because
// nothing was interrupted mid-write. The -real-crash flag instead re-execs this
// binary as a child process (runRealCrashDemo): the child commits
// -crash-committed transfers, each fsynced, appends a torn partial WAL frame
// modelling an interrupted next commit, and hard-kills itself with SIGKILL; the
// parent then reopens the data directory with recovery.OpenCtx and proves the
// ledger balances to exactly the durably-committed prefix — no committed
// transfer lost, no uncommitted or torn frame accepted. The real-crash path
// needs SIGKILL and degrades to a no-op on platforms that lack it.
//
// # Scale
//
// The default is a small, deterministic ledger (200 accounts, 2000 transfers)
// that builds and recovers well under a second, so the regression test stays
// fast. Every dimension is a flag, so the same binary scales up to where the
// persistence stack is worth observing:
//
//	go run ./examples/17_transactional_log -accounts 50000 -transfers 2000000 -seed 7
//
// The deterministic facts (counts, the recovered-amount sum, the conservation
// totals) are reproducible for a fixed -seed; only the telemetry (lines
// prefixed with "# ") and the temp directory path vary between runs and
// machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Node label for the ledger accounts. Centralised so the model is described
// in exactly one place.
const labelAccount = "ACCOUNT"

// config captures every scale and shape knob of the ledger benchmark. The
// zero value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression test).
type config struct {
	accounts  int   // number of :ACCOUNT nodes
	transfers int   // number of directed transfer edges (distinct src,dst pairs)
	minAmount int64 // minimum transfer amount in cents (inclusive)
	maxAmount int64 // maximum transfer amount in cents (inclusive)
	seed      int64 // RNG seed; fixes the deterministic data shape

	// checkpointEvery is the background checkpointer's age threshold: a
	// checkpoint fires once this much wall-clock time has elapsed since the
	// previous one. It controls how often the WAL is folded into the snapshot
	// during the commit stream; it does not change the recovered facts.
	checkpointEvery time.Duration
}

// The real cross-process crash demonstration (main's -real-crash path) is driven
// by plain local variables rather than config fields, so config stays a compact
// value type passed by value throughout the in-process flow.

// defaultConfig returns the small, deterministic ledger the regression test
// pins: 100 accounts, 600 transfers, amounts in [1.00, 10000.00], folded by a
// checkpointer firing roughly every 5 ms so dozens of checkpoints occur during
// the commit stream. The default commits one fsynced transaction per transfer,
// so it stays comfortably under the 60 s per-package short-test budget while
// still exercising the WAL, the background checkpointer, and recovery for real.
func defaultConfig() config {
	return config{
		accounts:        100,
		transfers:       600,
		minAmount:       100,     // 1.00 in cents
		maxAmount:       1000000, // 10000.00 in cents
		seed:            1,
		checkpointEvery: 5 * time.Millisecond,
	}
}

// validate rejects a configuration that cannot produce the requested shape —
// for instance more distinct transfers than there are ordered account pairs.
// It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.accounts <= 0:
		return fmt.Errorf("accounts must be > 0, got %d", c.accounts)
	case c.transfers < 0:
		return fmt.Errorf("transfers must be >= 0, got %d", c.transfers)
	case c.minAmount <= 0 || c.maxAmount < c.minAmount:
		return fmt.Errorf("require 0 < minAmount <= maxAmount, got [%d,%d]", c.minAmount, c.maxAmount)
	case c.checkpointEvery <= 0:
		return fmt.Errorf("checkpoint-every must be > 0, got %s", c.checkpointEvery)
	}
	// A simple directed graph with no self-loops has at most accounts*(accounts-1)
	// distinct ordered pairs; asking for more transfers than that cannot be
	// satisfied without duplicate or self edges.
	if maxPairs := int64(c.accounts) * int64(c.accounts-1); int64(c.transfers) > maxPairs {
		return fmt.Errorf("transfers (%d) exceeds accounts*(accounts-1) (%d): not enough distinct ordered pairs", c.transfers, maxPairs)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.accounts, "accounts", cfg.accounts, "number of ACCOUNT nodes")
	flag.IntVar(&cfg.transfers, "transfers", cfg.transfers, "number of distinct directed transfer edges")
	flag.Int64Var(&cfg.minAmount, "min-amount", cfg.minAmount, "minimum transfer amount in cents")
	flag.Int64Var(&cfg.maxAmount, "max-amount", cfg.maxAmount, "maximum transfer amount in cents")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.DurationVar(&cfg.checkpointEvery, "checkpoint-every", cfg.checkpointEvery,
		"background checkpointer age threshold (how often the WAL is folded into the snapshot)")

	// Real-crash knobs are plain locals (not config fields) so config stays a
	// compact by-value type. realCrash selects the demo; crashCommitted sizes
	// it; crashChildDir is the INTERNAL re-exec signal set by runRealCrashDemo.
	var (
		realCrash      bool
		crashCommitted int
		crashChildDir  string
	)
	flag.BoolVar(&realCrash, "real-crash", false,
		"run the real cross-process kill -9 crash + recovery demonstration instead of the in-process one")
	flag.IntVar(&crashCommitted, "crash-committed", 50,
		"real-crash demo: number of durably-committed transfers before the kill")
	flag.StringVar(&crashChildDir, "crash-child-dir", "",
		"INTERNAL: crash-child data dir; when set, this process commits -crash-committed transfers, appends torn work, and SIGKILLs itself")
	flag.Parse()

	ctx := context.Background()
	switch {
	case crashChildDir != "":
		// INTERNAL crash-child mode: commit, tear, and self-SIGKILL. On a
		// supported platform this call does not return; the error path covers
		// only a pre-crash failure or an unsupported platform.
		if err := runCrashChild(ctx, cfg, crashChildDir, crashCommitted); err != nil {
			log.Fatal(err)
		}
	case realCrash:
		if err := runRealCrashDemo(ctx, os.Stdout, cfg, crashCommitted, os.Args[0]); err != nil {
			log.Fatal(err)
		}
	default:
		if err := run(ctx, os.Stdout, cfg); err != nil {
			log.Fatal(err)
		}
	}
}

// run drives the whole walk-through and writes its report to w. Bare lines
// carry deterministic facts (counts, the recovered-amount sum, the
// conservation totals — reproducible for a fixed seed); lines prefixed with
// "# " carry volatile telemetry (throughput, on-disk bytes, recovery
// wall-clock, heap) that varies per run and per machine. All output goes to w
// so a test can capture and assert on the deterministic lines. run returns
// errors instead of terminating so a test can drive it; only main exits.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.accounts=%d\n", cfg.accounts)
	fmt.Fprintf(w, "config.transfers=%d\n", cfg.transfers)
	fmt.Fprintf(w, "config.amount=[%d,%d]\n", cfg.minAmount, cfg.maxAmount)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	// Generate the deterministic ledger plan up front, so the durable commit
	// stream and the post-recovery verification both read from the same fixed
	// shape. ctx cancellation is honoured during generation.
	plan, err := generateLedger(ctx, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	dir, err := os.MkdirTemp("", "gograph-ex17-")
	if err != nil {
		return fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	stats, err := commitLedger(ctx, dir, cfg, plan, w)
	if err != nil {
		return err
	}

	// Deterministic facts about what was committed.
	fmt.Fprintf(w, "nodes.accounts=%d\n", cfg.accounts)
	fmt.Fprintf(w, "edges.transfers=%d\n", len(plan.transfers))
	fmt.Fprintf(w, "ledger.amount_sum=%d\n", plan.totalAmount)

	// Volatile telemetry about the durable write phase and the checkpointer.
	fmt.Fprintf(w, "# commit.elapsed=%s\n", stats.commitElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# commit.tx_rate=%.0f tx/s\n", rate(len(plan.transfers), stats.commitElapsed))
	fmt.Fprintf(w, "# checkpoint.count=%d\n", stats.checkpoints)
	fmt.Fprintf(w, "# checkpoint.wal_bytes_folded=%s\n", humanBytes(stats.walBytesFolded))
	fmt.Fprintf(w, "# checkpoint.snapshot_bytes=%s\n", humanBytes(stats.snapshotBytes))

	// Simulate a crash: abandon every in-memory reference and rebuild the
	// ledger from disk alone (snapshot + any WAL tail). Nothing from the write
	// phase is touched after this point.
	rec, err := recoverLedger(ctx, dir, plan, w)
	if err != nil {
		return err
	}

	// Deterministic facts about the recovered ledger. Recovery must reproduce
	// the exact committed shape and the exact per-transfer amounts.
	fmt.Fprintf(w, "recovered.accounts=%d\n", rec.accounts)
	fmt.Fprintf(w, "recovered.transfers=%d\n", rec.transfers)
	fmt.Fprintf(w, "recovered.amount_sum=%d\n", rec.amountSum)
	fmt.Fprintf(w, "ledger.debit_sum=%d\n", rec.debitSum)
	fmt.Fprintf(w, "ledger.credit_sum=%d\n", rec.creditSum)
	fmt.Fprintf(w, "ledger.accounts_reconciled=%d\n", boolToInt(rec.accountsReconciled))
	fmt.Fprintf(w, "ledger.conserved=%t\n", rec.amountSum == plan.totalAmount)

	return nil
}

// transfer is one ledger entry: an amount (in cents) moved from a source
// account to a destination account. src and dst are indices into the
// account-id slice; amount is the durable edge weight.
type transfer struct {
	src, dst int
	amount   int64
}

// ledgerPlan is the deterministic ledger the run commits and then verifies
// after recovery. The slice order is the commit order; totalAmount is the sum
// of every transfer amount, which a sound recovery must reproduce exactly.
type ledgerPlan struct {
	accountIDs  []string
	transfers   []transfer
	totalAmount int64
}

// generateLedger builds the deterministic ledger described by cfg from a
// seeded RNG: cfg.accounts accounts with 24-char hex ids, then cfg.transfers
// distinct directed transfers (no self-loops, no duplicate ordered pairs),
// each carrying a random amount in [minAmount, maxAmount]. Fixing -seed fixes
// the whole shape — the same ids, pairs, amounts, and commit order — so the
// deterministic facts reproduce across machines. Generation honours ctx
// cancellation on a coarse interval.
func generateLedger(ctx context.Context, cfg config) (ledgerPlan, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed ledger for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	accountIDs := make([]string, cfg.accounts)
	seen := make(map[string]struct{}, cfg.accounts)
	for i := range accountIDs {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return ledgerPlan{}, err
			}
		}
		accountIDs[i] = uniqueHexID(rng, seen)
	}

	transfers := make([]transfer, 0, cfg.transfers)
	pairs := make(map[[2]int]struct{}, cfg.transfers)
	span := cfg.maxAmount - cfg.minAmount + 1
	var total int64
	for len(transfers) < cfg.transfers {
		if len(transfers)%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return ledgerPlan{}, err
			}
		}
		src := rng.Intn(cfg.accounts)
		dst := rng.Intn(cfg.accounts)
		if src == dst {
			continue // no self-loops
		}
		key := [2]int{src, dst}
		if _, dup := pairs[key]; dup {
			continue // at most one transfer per ordered pair
		}
		pairs[key] = struct{}{}
		amount := cfg.minAmount + rng.Int63n(span)
		transfers = append(transfers, transfer{src: src, dst: dst, amount: amount})
		total += amount
	}

	return ledgerPlan{accountIDs: accountIDs, transfers: transfers, totalAmount: total}, nil
}

// commitStats reports the volatile cost of the durable write phase plus the
// checkpointer's lifetime counters and the on-disk snapshot footprint.
type commitStats struct {
	commitElapsed  time.Duration
	checkpoints    uint64
	walBytesFolded uint64
	snapshotBytes  uint64
}

// checkEvery bounds how often the generator and commit loop poll ctx for
// cancellation: often enough that a cancelled large run stops promptly, rare
// enough that the check is free relative to the surrounding work.
const checkEvery = 1024

// commitLedger opens the WAL, builds a weight-codec store over it, starts the
// background checkpointer, and commits every transfer in the plan as its own
// transaction. The checkpointer folds the WAL into the snapshot on its timer
// throughout the stream; it is cleanly stopped before the WAL is closed so no
// goroutine leaks. It returns the write-phase telemetry.
func commitLedger(ctx context.Context, dir string, cfg config, plan ledgerPlan, w io.Writer) (commitStats, error) {
	walPath := filepath.Join(dir, "wal")
	wlog, err := wal.Open(walPath)
	if err != nil {
		return commitStats{}, fmt.Errorf("open WAL: %w", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions(g, wlog, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Background checkpointer. Because the store is driven directly here, the
	// checkpointer is wired with the store's own commit serialiser
	// (RunUnderCommitLock) rather than a shared mutex: it runs its
	// snapshot-capture + WAL-truncate critical section under the store's
	// private single-writer lock, so a snapshot can never capture a
	// half-applied transaction and the WAL is never truncated mid-commit (the
	// Atomicity + Durability contract on checkpoint.New, docs/acid-audit.md
	// F3.5). WithMapperCodec persists the NodeID->key mapper so the snapshot is
	// self-sufficient and the WAL can be truncated after each checkpoint.
	var unusedMu sync.Mutex // unused: WithCommitSerialiser supersedes storeMu.
	cp := checkpoint.New(checkpoint.Config{
		Dir:      dir,
		MaxAge:   cfg.checkpointEvery,
		Interval: cfg.checkpointEvery / 4,
	}, g, wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, int64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, int64](store.Codec()))
	cpCtx, cancelCP := context.WithCancel(ctx)
	cp.Start(cpCtx)

	// Commit the workload, one transaction per transfer. On any failure the
	// checkpointer is stopped and the WAL closed before returning, so no
	// goroutine or file handle leaks on the error path.
	start := time.Now()
	commitErr := commitTransfers(ctx, store, plan)
	commitElapsed := time.Since(start)

	// Quiesce the checkpointer before reading its final stats and closing the
	// WAL: Stop blocks until the goroutine has exited, so Stats() below is a
	// stable terminal snapshot and the WAL is closed with no checkpoint in
	// flight.
	cancelCP()
	cp.Stop()
	cpStats := cp.Stats()
	if closeErr := wlog.Close(); closeErr != nil && commitErr == nil {
		commitErr = fmt.Errorf("close WAL: %w", closeErr)
	}
	if commitErr != nil {
		return commitStats{}, commitErr
	}

	fmt.Fprintf(w, "# checkpoint.last_error=%q\n", cpStats.LastError)

	return commitStats{
		commitElapsed:  commitElapsed,
		checkpoints:    cpStats.Checkpoints,
		walBytesFolded: cpStats.WALTruncBytes,
		snapshotBytes:  snapshotBytes(dir),
	}, nil
}

// commitTransfers commits every transfer in the plan as its own transaction:
// each transaction interns both endpoints, adds the weighted transfer edge
// (the amount is the durable edge weight), and commits — so the WAL gains one
// fsynced transaction per transfer. It honours ctx cancellation on a coarse
// interval between transactions.
func commitTransfers(ctx context.Context, store *txn.Store[string, int64], plan ledgerPlan) error {
	for i, t := range plan.transfers {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := commitTransfer(store, plan.accountIDs[t.src], plan.accountIDs[t.dst], t.amount); err != nil {
			return fmt.Errorf("commit transfer %d (%s->%s): %w", i, plan.accountIDs[t.src], plan.accountIDs[t.dst], err)
		}
	}
	return nil
}

// commitTransfer commits one ledger transfer as a single transaction: it
// interns the two accounts (idempotent if already present), labels them, and
// adds the directed transfer edge whose weight is the amount in cents. The
// whole batch is made durable atomically — recovery replays all of it or none
// of it.
func commitTransfer(store *txn.Store[string, int64], src, dst string, amount int64) error {
	tx := store.Begin()
	if err := tx.AddNode(src); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add node %s: %w", src, err)
	}
	if err := tx.SetNodeLabel(src, labelAccount); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("label node %s: %w", src, err)
	}
	if err := tx.AddNode(dst); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add node %s: %w", dst, err)
	}
	if err := tx.SetNodeLabel(dst, labelAccount); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("label node %s: %w", dst, err)
	}
	if err := tx.AddEdge(src, dst, amount); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add transfer edge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// recoveryStats reports what was reconstructed from disk after the simulated
// crash, including the bit-exact amount verification.
type recoveryStats struct {
	accounts  int
	transfers int
	amountSum int64 // sum of every recovered transfer's edge weight
	debitSum  int64 // sum of amounts leaving an account (per src), from the graph walk
	creditSum int64 // sum of amounts entering an account (per dst), from the graph walk
	// accountsReconciled is true when each account's net position (inflows
	// minus outflows), reconstructed by walking the recovered graph's own
	// edges, equals the net position obtained by replaying the plan, AND the
	// recovered edge count matches the plan. It is the genuine double-entry
	// invariant (see reconcileNetPositions), independent of the per-transfer
	// probe above.
	accountsReconciled bool
}

// reconcileNetPositions performs a double-entry reconciliation of the recovered
// ledger, read directly from the graph's own adjacency rather than from the
// plan. It walks every recovered node and its out-edges, attributing each edge
// weight as a debit to its source account and a credit to its destination
// account, then compares each account's net position (credit minus debit) to
// the net position obtained by replaying want (the transfers a sound recovery
// must reproduce). It returns the per-source debit total, the per-destination
// credit total (genuinely distinct aggregations, unlike a single accumulator
// counted twice), and whether every account reconciled AND the observed edge
// count matched len(want). It therefore fails if a recovered edge is spurious,
// duplicated, missing, or attributed to the wrong endpoint — corruption the
// per-transfer point probe cannot see.
func reconcileNetPositions(g *lpg.Graph[string, int64], accountIDs []string, want []transfer) (debitSum, creditSum int64, reconciled bool) {
	idxOf := make(map[string]int, len(accountIDs))
	for i, id := range accountIDs {
		idxOf[id] = i
	}
	debit := make([]int64, len(accountIDs))
	credit := make([]int64, len(accountIDs))
	edges := 0
	anomaly := false // a recovered node/edge referencing an unknown account id
	adj := g.AdjList()
	adj.Mapper().Walk(func(_ graph.NodeID, srcKey string) bool {
		si, ok := idxOf[srcKey]
		if !ok {
			anomaly = true
			return true
		}
		for dstKey, wgt := range adj.Neighbours(srcKey) {
			di, ok := idxOf[dstKey]
			if !ok {
				anomaly = true
				continue
			}
			debit[si] += wgt
			credit[di] += wgt
			edges++
		}
		return true
	})
	for i := range accountIDs {
		debitSum += debit[i]
		creditSum += credit[i]
	}
	expectedNet := make([]int64, len(accountIDs))
	for _, t := range want {
		expectedNet[t.src] -= t.amount
		expectedNet[t.dst] += t.amount
	}
	reconciled = !anomaly && edges == len(want)
	for a := range expectedNet {
		if credit[a]-debit[a] != expectedNet[a] {
			reconciled = false
		}
	}
	return debitSum, creditSum, reconciled
}

// recoverLedger reopens the store from disk alone (snapshot + any WAL tail),
// then verifies every committed transfer survived with its exact amount. It
// checks, per transfer, that the recovered edge exists and that
// EdgeWeight(src, dst) equals the committed amount — a bit-exact verification
// of the durable weight-codec path. It then runs a separate, graph-driven
// double-entry reconciliation (reconcileNetPositions) that reconstructs each
// account's net position from the recovered graph's own edges and checks it
// against the plan replay. A corrupt WAL is fail-stop: recovery returns a
// non-nil error and IsClean reports false, and the run refuses to proceed
// onto a damaged prefix.
func recoverLedger(ctx context.Context, dir string, plan ledgerPlan, w io.Writer) (recoveryStats, error) {
	start := time.Now()
	res, err := recovery.OpenCtx[string, int64](ctx, dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	recoveryElapsed := time.Since(start)
	if err != nil {
		return recoveryStats{}, fmt.Errorf("recovery open: %w", err)
	}
	if !res.IsClean() {
		return recoveryStats{}, fmt.Errorf("recovery: refusing to use a corrupt WAL: %w", res.TailErr)
	}

	fmt.Fprintf(w, "# recovery.elapsed=%s\n", recoveryElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# recovery.snapshot_hit=%t\n", res.SnapshotHit)
	fmt.Fprintf(w, "# recovery.wal_ops=%d\n", res.WALOps)

	g := res.Graph
	var rec recoveryStats
	for i, t := range plan.transfers {
		src, dst := plan.accountIDs[t.src], plan.accountIDs[t.dst]
		if !g.AdjList().HasEdge(src, dst) {
			return recoveryStats{}, fmt.Errorf("recovery lost committed transfer %d: %s->%s", i, src, dst)
		}
		got, ok := g.EdgeWeight(src, dst)
		if !ok {
			return recoveryStats{}, fmt.Errorf("recovery lost weight of transfer %d: %s->%s", i, src, dst)
		}
		if got != t.amount {
			return recoveryStats{}, fmt.Errorf("recovery corrupted transfer %d amount: %s->%s got %d, want %d", i, src, dst, got, t.amount)
		}
		rec.transfers++
		rec.amountSum += got
	}

	// Double-entry reconciliation, computed independently of the per-transfer
	// probe above by walking the recovered graph's own edges. debitSum (per
	// source) and creditSum (per destination) are genuinely distinct sums here,
	// and accountsReconciled verifies each account's net position matches the
	// plan replay — a check that fails on a misattributed or spurious edge.
	rec.debitSum, rec.creditSum, rec.accountsReconciled = reconcileNetPositions(g, plan.accountIDs, plan.transfers)

	// Account count is reported by the live graph after recovery rather than
	// re-derived from the plan, so a lost or spurious node surfaces as a
	// mismatch the test pins. LiveOrder counts non-tombstoned interned nodes;
	// the ledger never removes a node, so it equals the account total.
	rec.accounts = int(g.LiveOrder()) //nolint:gosec // G115: account count is bounded by cfg.accounts (an int), no realistic overflow

	return rec, nil
}

// snapshotBytes returns the total on-disk size of the snapshot the
// checkpointer writes — every component file (csr.bin, mapper.bin, labels.bin,
// the manifest, …) under the "snapshot" subdirectory of dir — or 0 if no
// snapshot was written. It is telemetry only: the figure depends on whether
// and when the background checkpointer fired, so it is never asserted.
func snapshotBytes(dir string) uint64 {
	var total uint64
	snapDir := filepath.Join(dir, "snapshot")
	_ = filepath.WalkDir(snapDir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a missing snapshot dir means 0 bytes, not a failure
		}
		if info, statErr := d.Info(); statErr == nil && info.Size() > 0 {
			total += uint64(info.Size()) //nolint:gosec // G115: guarded info.Size() > 0, so the int64->uint64 conversion is safe
		}
		return nil
	})
	return total
}

// uniqueHexID returns a 24-character lowercase hex id (12 random bytes) that
// has not been handed out before, recording it in seen. Drawing from the
// seeded rng keeps the whole dataset reproducible.
func uniqueHexID(rng *rand.Rand, seen map[string]struct{}) string {
	const hexDigits = "0123456789abcdef"
	var b [24]byte
	for {
		for i := range b {
			b[i] = hexDigits[rng.Intn(16)]
		}
		id := string(b[:])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		return id
	}
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
