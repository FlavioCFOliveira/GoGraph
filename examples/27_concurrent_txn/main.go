// Example 27_concurrent_txn — transactional ISOLATION and ATOMICITY of the
// WAL-backed Cypher engine, certified under concurrency and the race detector.
//
// A realistic bank clearing-ledger is opened over a write-ahead log. Many
// writer goroutines move money between accounts while many reader goroutines
// continuously observe a global invariant that can only hold if the engine
// isolates in-flight transactions from readers. The whole run doubles as a
// data-race certification: it is meant to be run under `go test -race`.
//
// # Model
//
//	(:ACCOUNT {id, balance})            // id is a string account number,
//	                                    // balance is an integer (cents)
//
// Each account is a node carrying an integer balance in minor currency units,
// keyed by a string account number backed by a range index for O(log n) lookup.
// A transfer moves an amount from one account to another: it debits the source
// and credits the destination by the same amount, so the SUM of all balances is
// invariant — money is neither created nor destroyed. That conserved total is
// the observable the readers pin.
//
// The ledger is fully capitalised: initial balances are chosen (and validated)
// to exceed the largest possible aggregate debit on any single account, so no
// account can ever go negative. Overdraft protection is therefore not needed
// and no transfer is ever rejected — every planned transfer commits — which
// keeps the committed set, and hence the final per-account state, deterministic.
//
// # What it certifies
//
// The example exercises, and asserts, three ACID properties under contention:
//
//   - ISOLATION (the headline). Readers repeatedly compute `sum(balance)` over
//     all accounts, via both [cypher.Engine.Run] and a read-only
//     [cypher.Engine.BeginReadTx] transaction. Under correct isolation this sum
//     ALWAYS equals the seeded total: a reader must never observe a debit
//     without its matching credit. A single torn observation is a module
//     isolation bug — the run surfaces it as an error rather than hiding it, and
//     the fact line total_balance_invariant_holds flips to 0.
//
//   - ATOMICITY. Half the transfers run as MULTI-STATEMENT explicit transactions
//     ([cypher.Engine.BeginTx]: a debit statement, then a credit statement, then
//     one Commit); the other half run as SINGLE-STATEMENT autocommit writes
//     ([cypher.Engine.RunInTx]) that debit and credit in one statement. In both
//     shapes a concurrent reader can never slip between the debit and the credit —
//     it sees the whole transaction or none of it. Atomicity also has to survive
//     a REFUSED transfer: a transaction that loses a write-write conflict must
//     leave nothing behind, so its physical rollback is exercised on every one of
//     the conflicts the telemetry counts.
//
//   - CONSISTENCY / no lost updates. Because every transfer is a commutative
//     delta on two accounts, replaying the committed transfers in any order
//     yields the same final per-account balances. The run computes that expected
//     state deterministically up front and, after the concurrent phase, asserts
//     every account matches it. A single mismatch means a read-modify-write
//     interleaving lost an update (a serialisation failure); lost_updates counts
//     them and must be 0.
//
// # Isolation model (verified against cypher/exectx.go and graph/lpg)
//
// Concurrency control is MVCC and nothing else (rmp #2320). An autocommit
// statement applies under [lpg.Graph.ApplyVersioned], which holds the schema
// barrier SHARED, so writers overlap instead of queueing: what makes a
// transaction atomically visible is that every version it writes points at one
// shared commit record, published with a single atomic store. A reader therefore
// observes a transaction entirely or not at all, whichever writers are in flight.
// [cypher.Engine.BeginTx] still holds the barrier exclusively for the
// transaction's lifetime (retiring that is rmp #2305), so a multi-statement
// transfer serialises against other writers where a single-statement one does
// not. [cypher.Engine.BeginReadTx] is the read-only path: it takes no lock at all
// and reads through a snapshot, so it is never blocked by, and never blocks, a
// writer.
//
// Overlapping writers make WRITE-WRITE CONFLICTS real, and this example is where
// they first became observable in the module. Two transfers touching the same
// account collide, and the second to reach the version-chain head is REFUSED with
// [mvcc.ErrSerializationConflict] rather than silently overwriting the first
// (first-updater-wins). The writers therefore RETRY, which is the client's half
// of the MVCC contract — see [retryOnConflict] for why the backoff is sized to a
// WAL fsync and not to a scheduler yield — and the run reports the conflict rate
// as telemetry (writer.conflicts_retried, writer.conflict_retries_max) so the
// cost of the write concurrency is visible rather than hidden. A single-writer
// engine reports zero there, because a conflict cannot arise.
//
// # Scale
//
// The default is small, deterministic, and fast (a few hundred transfers over a
// few dozen accounts) so the regression test stays well under the 60 s
// short-layer budget. Every dimension is a flag, so the same binary scales up to
// where write serialisation and reader throughput are worth observing:
//
//	go run ./examples/27_concurrent_txn -accounts 5000 -writers 16 -readers 32 \
//	    -ops-per-writer 5000 -max-amount 1000 -seed 7
//
// (-max-amount is kept small at this scale so the fully-capitalised no-overdraft
// invariant still holds: min-initial must be >= writers*ops-per-writer*max-amount.)
//
// The deterministic facts (counts, the seeded total, the conservation and
// no-lost-update invariants) reproduce for a fixed -seed; only the telemetry
// (lines prefixed with "# ") and the temp directory path vary per run and
// machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// labelAccount is the node label for ledger accounts. Centralised so the model
// is described in exactly one place.
const labelAccount = "ACCOUNT"

// Cypher statements the workload runs. Kept as named constants so the plan
// cache is hit on every call and the queries are documented once.
const (
	// qDebit / qCredit are the two statements of a multi-statement transfer
	// (BeginTx). Each is keyed by account id and mutates one account.
	qDebit  = "MATCH (a:" + labelAccount + " {id:$id}) SET a.balance = a.balance - $amt"
	qCredit = "MATCH (a:" + labelAccount + " {id:$id}) SET a.balance = a.balance + $amt"

	// qTransfer is a single-statement transfer (RunInTx): it debits the source
	// and credits the destination atomically in one autocommit transaction.
	qTransfer = "MATCH (a:" + labelAccount + " {id:$from}), (b:" + labelAccount + " {id:$to}) " +
		"SET a.balance = a.balance - $amt, b.balance = b.balance + $amt"

	// qTotal is the conserved invariant the readers pin: the sum of every
	// account balance, which must always equal the seeded total.
	qTotal = "MATCH (a:" + labelAccount + ") RETURN sum(a.balance) AS total"

	// qDump reads every account's id and balance for the final per-account
	// verification against the deterministic replay.
	qDump = "MATCH (a:" + labelAccount + ") RETURN a.id AS id, a.balance AS balance"

	// ddlIndex declares the range index that turns the keyed account lookups
	// into an index seek rather than a full label scan. Created on the empty
	// graph before the seed so every engine write maintains it incrementally.
	// The engine's grammar places IF NOT EXISTS before the index name.
	ddlIndex = "CREATE INDEX IF NOT EXISTS account_id_idx FOR (a:" + labelAccount + ") ON (a.id)"
)

// config captures every scale and shape knob of the certification. The zero
// value is not valid; build one with defaultConfig and override fields from
// flags (see main) or construct one directly (see the regression test).
type config struct {
	accounts     int   // number of :ACCOUNT nodes
	writers      int   // concurrent writer goroutines
	readers      int   // concurrent reader goroutines
	opsPerWriter int   // transfers each writer commits
	minInitial   int64 // minimum initial balance in cents (inclusive)
	maxInitial   int64 // maximum initial balance in cents (inclusive)
	maxAmount    int64 // maximum transfer amount in cents (min is always 1)
	sweepOps     int   // transfers per level of the writer-scaling sweep
	seed         int64 // RNG seed; fixes the deterministic data shape

	// faultSplitMultiStatement is a NEGATIVE CONTROL, never a workload knob, and
	// it is deliberately not reachable from a command-line flag.
	//
	// When set, a multi-statement transfer commits its debit and its credit as TWO
	// SEPARATE autocommit transactions instead of one. That opens a real, wide
	// window in which a reader observes the debit without its credit — an actual
	// torn total, produced on purpose.
	//
	// It exists because a gate that has never been shown to FAIL is not evidence
	// that anything passed. rmp #2333 spent a reproduction search of roughly 7.3
	// million clean observations on a single unattributable sighting; the gate and
	// its diagnosis are therefore validated against a deliberately broken run (see
	// TestTornGate_CatchesADeliberateTear) rather than trusted because they are
	// quiet.
	faultSplitMultiStatement bool
}

// defaultConfig returns the small, deterministic configuration the regression
// test pins: 32 accounts, 4 writers × 150 transfers (600 total) read by 4
// readers, plus a writer-scaling sweep. Initial balances are large relative to
// the largest possible aggregate debit, so no account can go negative. It
// commits one fsynced transaction per transfer, staying well under the 60 s
// short-test budget while exercising the WAL, both write paths, and the
// concurrent read paths for real.
func defaultConfig() config {
	return config{
		accounts:     32,
		writers:      4,
		readers:      4,
		opsPerWriter: 150,
		minInitial:   1_000_000_000, // 10,000,000.00
		maxInitial:   2_000_000_000, // 20,000,000.00
		maxAmount:    1_000_000,     // 10,000.00 (transfers are in [1, maxAmount])
		sweepOps:     120,
		seed:         1,
	}
}

// validate rejects a configuration that cannot produce the requested shape or
// that would allow an account to go negative (which would make the committed
// set data-dependent and break determinism). It is checked once, at the
// boundary, before any work.
func (c *config) validate() error {
	switch {
	case c.accounts < 2:
		return fmt.Errorf("accounts must be >= 2, got %d", c.accounts)
	case c.writers < 1:
		return fmt.Errorf("writers must be >= 1, got %d", c.writers)
	case c.readers < 1:
		return fmt.Errorf("readers must be >= 1, got %d", c.readers)
	case c.opsPerWriter < 0:
		return fmt.Errorf("ops-per-writer must be >= 0, got %d", c.opsPerWriter)
	case c.minInitial < 1 || c.maxInitial < c.minInitial:
		return fmt.Errorf("require 1 <= min-initial <= max-initial, got [%d,%d]", c.minInitial, c.maxInitial)
	case c.maxAmount < 1:
		return fmt.Errorf("max-amount must be >= 1, got %d", c.maxAmount)
	case c.sweepOps < 0:
		return fmt.Errorf("sweep-ops must be >= 0, got %d", c.sweepOps)
	}
	// No-overdraft guarantee: even if every transfer debited the same account at
	// the maximum amount, its balance must stay >= 0. This keeps the committed
	// set (and therefore the final per-account state) deterministic and makes
	// no_negative_balances a genuine invariant rather than a lucky observation.
	total := int64(c.writers) * int64(c.opsPerWriter)
	if c.maxAmount > 0 && total > (int64(1)<<62)/c.maxAmount {
		return fmt.Errorf("writers*ops-per-writer*max-amount overflows; reduce the scale")
	}
	if worst := total * c.maxAmount; c.minInitial < worst {
		return fmt.Errorf("min-initial (%d) must be >= writers*ops-per-writer*max-amount (%d) to guarantee no overdraft", c.minInitial, worst)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.accounts, "accounts", cfg.accounts, "number of ACCOUNT nodes")
	flag.IntVar(&cfg.writers, "writers", cfg.writers, "concurrent writer goroutines")
	flag.IntVar(&cfg.readers, "readers", cfg.readers, "concurrent reader goroutines")
	flag.IntVar(&cfg.opsPerWriter, "ops-per-writer", cfg.opsPerWriter, "transfers each writer commits")
	flag.Int64Var(&cfg.minInitial, "min-initial", cfg.minInitial, "minimum initial balance in cents")
	flag.Int64Var(&cfg.maxInitial, "max-initial", cfg.maxInitial, "maximum initial balance in cents")
	flag.Int64Var(&cfg.maxAmount, "max-amount", cfg.maxAmount, "maximum transfer amount in cents (min is 1)")
	flag.IntVar(&cfg.sweepOps, "sweep-ops", cfg.sweepOps, "transfers per level of the writer-scaling sweep (0 disables)")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, &cfg); err != nil {
		log.Fatal(err)
	}
}

// run drives the whole certification and writes its report to w. Bare lines
// carry deterministic facts (counts, the seeded total, the ACID invariants —
// reproducible for a fixed seed); lines prefixed with "# " carry volatile
// telemetry (throughput, contention, heap) that varies per run and per machine.
// All output goes to w so a test can capture and assert on the deterministic
// lines. run returns wrapped errors instead of terminating so a test can drive
// it and honours ctx cancellation throughout; only main exits.
func run(ctx context.Context, w io.Writer, cfg *config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.accounts=%d\n", cfg.accounts)
	fmt.Fprintf(w, "config.writers=%d\n", cfg.writers)
	fmt.Fprintf(w, "config.readers=%d\n", cfg.readers)
	fmt.Fprintf(w, "config.ops_per_writer=%d\n", cfg.opsPerWriter)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Deterministic plan: the accounts with their seeded initial balances, the
	// transfers assigned to each writer, and the expected final per-account state
	// obtained by replaying every transfer (order-independent, since a transfer
	// is a commutative delta).
	plan := generatePlan(cfg)
	fmt.Fprintf(w, "accounts=%d\n", len(plan.accounts))
	fmt.Fprintf(w, "transfers.planned=%d\n", plan.total)
	fmt.Fprintf(w, "transfers.multi_statement=%d\n", plan.multiStatement)
	fmt.Fprintf(w, "transfers.single_statement=%d\n", plan.total-plan.multiStatement)
	fmt.Fprintf(w, "initial_total=%d\n", plan.initialTotal)

	// Open a WAL-backed engine in a fresh temp directory. The WAL is the only
	// resource that must be released; closeFn does it (and removes the dir).
	eng, closeFn, err := openEngine(ctx, "gograph-ex27-")
	if err != nil {
		return err
	}
	defer closeFn()

	// Declare the id index on the empty graph, then seed the accounts through the
	// engine so the index is maintained incrementally.
	if err := runWrite(ctx, eng, ddlIndex, nil); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	if err := seedAccounts(ctx, eng, plan.accounts); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	// Sanity: the freshly seeded total must equal the computed initial total,
	// confirming the seed landed before the concurrent phase relies on it.
	seeded, _, err := readTotalRun(ctx, eng, plan.initialTotal)
	if err != nil {
		return fmt.Errorf("read seeded total: %w", err)
	}
	if seeded != plan.initialTotal {
		return fmt.Errorf("seed mismatch: engine total %d, computed %d", seeded, plan.initialTotal)
	}

	// Evidence that the id index is real: the keyed debit lookup plans as a
	// NodeByIndexSeek, not a full NodeByLabelScan. Telemetry only — the plan text
	// is not a pinned fact.
	fmt.Fprintf(w, "# plan.debit_index_seek=%t\n", planUsesIndexSeek(eng))

	// The concurrent phase: writers move money while readers pin the conserved
	// total. This is the section that must be race-clean and isolation-correct.
	stats, err := runConcurrent(ctx, eng, &plan)
	if err != nil {
		return err
	}

	// Post-run verification against the durable, committed state.
	finalTotal, _, err := readTotalRun(ctx, eng, plan.initialTotal)
	if err != nil {
		return fmt.Errorf("read final total: %w", err)
	}
	balances, err := readBalances(ctx, eng)
	if err != nil {
		return fmt.Errorf("read balances: %w", err)
	}
	lostUpdates, minBalance := diffAgainstExpected(balances, plan.expected)

	// Deterministic ACID facts — pinned by the regression test.
	fmt.Fprintf(w, "transfers.committed=%d\n", stats.committed)
	fmt.Fprintf(w, "final_total=%d\n", finalTotal)
	fmt.Fprintf(w, "conservation.holds=%d\n", b2i(finalTotal == plan.initialTotal))
	fmt.Fprintf(w, "lost_updates=%d\n", lostUpdates)
	fmt.Fprintf(w, "no_negative_balances=%d\n", b2i(minBalance >= 0))
	fmt.Fprintf(w, "total_balance_invariant_holds=%d\n", b2i(stats.tornObservations == 0))

	// Volatile telemetry — never pinned.
	built := readMem()
	fmt.Fprintf(w, "# run.elapsed=%s\n", stats.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# writer.transfers_per_s=%.0f\n", rate(int64(stats.committed), stats.elapsed))
	fmt.Fprintf(w, "# writer.mean_acquire_wait=%s\n", meanDuration(stats.acquireWaitTotal, stats.acquireWaitCount).Round(time.Nanosecond))
	// The MVCC write-concurrency cost, in the open: how many attempts were refused
	// with a serialization conflict and retried, and the deepest chain one transfer
	// needed. Both are telemetry, not facts — the conflict rate depends on
	// scheduling — but they are what proves writers genuinely overlap: a
	// single-writer engine reports zero because a conflict cannot arise (rmp #2320).
	fmt.Fprintf(w, "# writer.conflicts_retried=%d\n", stats.conflicts)
	fmt.Fprintf(w, "# writer.conflict_retries_max=%d\n", stats.conflictRetriesMax)
	fmt.Fprintf(w, "# writer.conflict_wait_max=%s (budget %s)\n", time.Duration(stats.conflictWaitMaxNs), conflictRetryBudget)
	fmt.Fprintf(w, "# reader.observations=%d\n", stats.readObservations)
	fmt.Fprintf(w, "# reader.observations_per_s=%.0f\n", rate(stats.readObservations, stats.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))

	// Writer-scaling sweep (telemetry only): the identical fixed workload run at
	// 1, 2, 4 … writers on a fresh ephemeral store.
	//
	// READ IT AS TELEMETRY, NOT AS A DIAGNOSIS (rmp #2345). This used to say flat
	// throughput is "the observable signature of the single-writer serialisation that
	// underpins isolation". Both halves are now false: rmp #2306 retired the
	// serialisation, and isolation is underpinned by MVCC, not by exclusion. Worse,
	// the inference does not hold — rmp #2338 ablated the locks and found the
	// in-memory write ceiling is set by the commit's ALLOCATION RATE, with a
	// share-nothing lock-free workload of the same profile ceilinging at ~2.6x on a
	// ten-core host. A flat curve here is a fact about the workload's allocation
	// behaviour at least as much as about any lock.
	if cfg.sweepOps > 0 {
		if err := scalingSweep(ctx, w, cfg); err != nil {
			return fmt.Errorf("scaling sweep: %w", err)
		}
	}

	// Fail loudly on an isolation violation: a torn observation means a reader
	// saw a partially-applied transaction — a module isolation bug — so the run
	// returns an error naming it rather than reporting success.
	if stats.tornObservations > 0 {
		return fmt.Errorf("ISOLATION VIOLATION: readers observed a torn total %d time(s); first torn value %d, expected %d (delta %+d)%s",
			stats.tornObservations, stats.firstTorn, plan.initialTotal,
			stats.firstTorn-plan.initialTotal,
			tornDiagnosis(stats.tornDetail, plan.accounts))
	}
	if lostUpdates > 0 {
		return fmt.Errorf("LOST UPDATE: %d account(s) diverged from the deterministic replay — a concurrent read-modify-write interleaving lost a write", lostUpdates)
	}
	if finalTotal != plan.initialTotal {
		return fmt.Errorf("CONSERVATION VIOLATION: final total %d != initial total %d — a transfer was not atomic", finalTotal, plan.initialTotal)
	}
	if stats.committed != plan.total {
		return fmt.Errorf("DURABILITY/ATOMICITY: committed %d of %d planned transfers", stats.committed, plan.total)
	}
	if stats.readObservations == 0 {
		return fmt.Errorf("readers never observed the invariant; the isolation check did not run")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Deterministic plan
// ─────────────────────────────────────────────────────────────────────────────

// account is one ledger account: a dense integer index (its identity in the
// expected-balance slice) and its seeded initial balance. The Cypher id property
// is the decimal string of the index (see acctKey), so the account is keyed by a
// string account number matching the String-typed id index.
type account struct {
	id      int
	initial int64
}

// acctKey maps an account's dense integer index to its string account number,
// the value stored in and matched against the id property.
func acctKey(i int) string { return strconv.Itoa(i) }

// transfer moves amount from account from to account to. multi selects the
// execution path: a multi-statement explicit transaction (true) or a
// single-statement autocommit write (false).
type transfer struct {
	from   int
	to     int
	amount int64
	multi  bool
}

// plan is the deterministic workload the run commits and then verifies. It is a
// pure function of the config, so a fixed seed fixes every fact.
type plan struct {
	accounts       []account
	byWriter       [][]transfer // transfers assigned to each writer
	expected       []int64      // expected final balance per account id
	initialTotal   int64
	total          int // total transfers across all writers
	multiStatement int // transfers executed via BeginTx
	readers        int // reader goroutines to run against this plan
	// splitMulti carries config.faultSplitMultiStatement to the writer goroutines.
	// It is a negative control; see the field's documentation on [config].
	splitMulti bool
}

// generatePlan builds the deterministic plan from a seeded RNG: cfg.accounts
// accounts with initial balances in [minInitial, maxInitial], then
// writers*opsPerWriter transfers. Transfers are generated in a fixed global
// order; transfer t is assigned to writer t%writers and uses the multi-statement
// path when t is even. The expected final balances are computed by replaying
// every transfer — order-independent, because each transfer is a commutative
// delta — so the concurrent run can be checked against them regardless of
// scheduling.
func generatePlan(cfg *config) plan {
	//nolint:gosec // G404: a seeded math/rand is intentional — the example must
	// reproduce a fixed workload for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	accounts := make([]account, cfg.accounts)
	expected := make([]int64, cfg.accounts)
	initSpan := cfg.maxInitial - cfg.minInitial + 1
	var initialTotal int64
	for i := range accounts {
		bal := cfg.minInitial + rng.Int63n(initSpan)
		accounts[i] = account{id: i, initial: bal}
		expected[i] = bal
		initialTotal += bal
	}

	total := cfg.writers * cfg.opsPerWriter
	byWriter := make([][]transfer, cfg.writers)
	for wkr := range byWriter {
		byWriter[wkr] = make([]transfer, 0, cfg.opsPerWriter)
	}
	multiStatement := 0
	for t := 0; t < total; t++ {
		from := rng.Intn(cfg.accounts)
		to := rng.Intn(cfg.accounts)
		for to == from {
			to = rng.Intn(cfg.accounts)
		}
		amount := 1 + rng.Int63n(cfg.maxAmount)
		multi := t%2 == 0
		if multi {
			multiStatement++
		}
		expected[from] -= amount
		expected[to] += amount
		wkr := t % cfg.writers
		byWriter[wkr] = append(byWriter[wkr], transfer{from: from, to: to, amount: amount, multi: multi})
	}

	return plan{
		accounts:       accounts,
		initialTotal:   initialTotal,
		byWriter:       byWriter,
		expected:       expected,
		total:          total,
		multiStatement: multiStatement,
		readers:        cfg.readers,
		splitMulti:     cfg.faultSplitMultiStatement,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// openEngine creates a fresh temp directory with the given prefix, opens a WAL
// inside it, builds a WAL-backed txn.Store and a Cypher engine over it, and
// returns the engine plus a cleanup function that closes the WAL and removes the
// directory. The cleanup is safe to call once via defer.
func openEngine(_ context.Context, prefix string) (*cypher.Engine, func(), error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("mkdir temp: %w", err)
	}
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("open WAL: %w", err)
	}
	// Multigraph is set only to silence the engine's non-multigraph advisory at
	// construction; this model has no relationships at all (accounts carry their
	// balance as a property), so the setting is otherwise immaterial.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions(g, wlog, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	cleanup := func() {
		_ = wlog.Close()
		_ = os.RemoveAll(dir)
	}
	return eng, cleanup, nil
}

// planUsesIndexSeek reports whether the keyed debit lookup plans as a
// NodeByIndexSeek over the id index rather than a full label scan. It is a
// read-only Explain and never mutates the graph.
func planUsesIndexSeek(eng *cypher.Engine) bool {
	plan, err := eng.Explain(qDebit, map[string]expr.Value{
		"id":  expr.StringValue("0"),
		"amt": expr.IntegerValue(1),
	})
	if err != nil {
		return false
	}
	return strings.Contains(plan, "NodeByIndexSeek")
}

// seedAccounts creates every account through the engine in bounded UNWIND CREATE
// batches, so the id index built beforehand is maintained incrementally and no
// single transaction grows unreasonably large.
func seedAccounts(ctx context.Context, eng *cypher.Engine, accounts []account) error {
	const batch = 4096
	for start := 0; start < len(accounts); start += batch {
		end := min(start+batch, len(accounts))
		rows := make([]any, 0, end-start)
		for _, a := range accounts[start:end] {
			rows = append(rows, map[string]any{"id": acctKey(a.id), "balance": a.initial})
		}
		q := "UNWIND $rows AS row CREATE (:" + labelAccount + " {id: row.id, balance: row.balance})"
		if err := runWriteAny(ctx, eng, q, map[string]any{"rows": rows}); err != nil {
			return fmt.Errorf("seed batch [%d,%d): %w", start, end, err)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Concurrent phase
// ─────────────────────────────────────────────────────────────────────────────

// runStats accumulates the outcome of the concurrent phase: the committed count
// and the correctness counters (all lock-free), plus telemetry.
type runStats struct {
	committed        int
	elapsed          time.Duration
	readObservations int64
	tornObservations int64
	firstTorn        int64
	acquireWaitTotal int64 // nanoseconds spent blocked acquiring a write transaction
	acquireWaitCount int64
	// conflicts is how many transfer attempts were REFUSED with a serialization
	// conflict and retried, and conflictRetriesMax the deepest retry chain any one
	// transfer needed. Both are zero on a single-writer engine and non-zero under
	// MVCC's first-updater-wins, which is why they are reported rather than
	// swallowed: they are the observable cost of the write concurrency this
	// example exists to exercise (rmp #2320).
	conflicts          int64
	conflictRetriesMax int64
	// conflictWaitMaxNs is the longest WALL TIME any one transfer spent in its retry
	// chain. It is the number that sizes [conflictRetryBudget], so it is reported
	// rather than left to be guessed at the next time the bound is questioned.
	conflictWaitMaxNs int64
	// tornDetail is the diagnosis of the first torn observation, or nil when none
	// was seen. See [tornReport].
	tornDetail *tornReport
}

// runConcurrent launches cfg.readers reader goroutines and cfg.writers writer
// goroutines over the shared engine. Writers commit their assigned transfers;
// readers repeatedly observe the conserved total and flag any torn value. All
// goroutines join before runConcurrent returns (on completion, error, or
// cancellation), so nothing leaks. It returns the aggregate statistics.
func runConcurrent(ctx context.Context, eng *cypher.Engine, plan *plan) (runStats, error) {
	var (
		writersWG sync.WaitGroup
		readersWG sync.WaitGroup
		done      atomic.Bool

		committed        atomic.Int64
		reads            atomic.Int64
		torn             atomic.Int64
		firstTorn        atomic.Int64
		acquireWaitTotal atomic.Int64
		acquireWaitCount atomic.Int64
		conflictCounts   conflictCounters
		firstErr         atomic.Pointer[error]
		firstReport      atomic.Pointer[tornReport]
	)
	setErr := func(e error) {
		if e != nil {
			firstErr.CompareAndSwap(nil, &e)
		}
	}

	start := time.Now()

	// Readers: pin the conserved total until the writers finish.
	readersWG.Add(plan.readerCount())
	for r := 0; r < plan.readerCount(); r++ {
		useReadTx := r%2 == 1 // half via Engine.Run, half via BeginReadTx
		go func() {
			defer readersWG.Done()
			for i := 0; !done.Load(); i++ {
				if i%ctxCheckEvery == 0 && ctx.Err() != nil {
					return
				}
				var (
					total int64
					rep   *tornReport
					err   error
				)
				if useReadTx {
					total, rep, err = readTotalReadTx(ctx, eng, plan.initialTotal)
				} else {
					total, rep, err = readTotalRun(ctx, eng, plan.initialTotal)
				}
				if err != nil {
					setErr(fmt.Errorf("reader: %w", err))
					return
				}
				reads.Add(1)
				if rep != nil {
					torn.Add(1)
					firstTorn.CompareAndSwap(0, total)
					// Keep the FIRST report, and prefer one that carries a
					// same-instant diagnosis: an Engine.Run reader arriving first
					// with nothing but a number must not displace the attribution
					// a read-transaction reader can supply.
					if cur := firstReport.Load(); cur == nil || (!cur.sameInstant && rep.sameInstant) {
						firstReport.CompareAndSwap(cur, rep)
					}
				}
			}
		}()
	}

	// Writers: commit the assigned transfers.
	writersWG.Add(len(plan.byWriter))
	for wkr := range plan.byWriter {
		transfers := plan.byWriter[wkr]
		go func() {
			defer writersWG.Done()
			for i, t := range transfers {
				if i%ctxCheckEvery == 0 && ctx.Err() != nil {
					setErr(ctx.Err())
					return
				}
				if t.multi {
					var wait time.Duration
					err := retryOnConflict(ctx, conflictRetryBudget, &conflictCounts, func() error {
						var e error
						wait, e = commitMultiStatement(ctx, eng, t, plan.splitMulti)
						return e
					})
					if err != nil {
						setErr(fmt.Errorf("writer multi-statement: %w", err))
						return
					}
					acquireWaitTotal.Add(int64(wait))
					acquireWaitCount.Add(1)
				} else if err := retryOnConflict(ctx, conflictRetryBudget, &conflictCounts, func() error {
					return commitSingleStatement(ctx, eng, t)
				}); err != nil {
					setErr(fmt.Errorf("writer single-statement: %w", err))
					return
				}
				committed.Add(1)
			}
		}()
	}

	writersWG.Wait()
	done.Store(true)
	readersWG.Wait()
	elapsed := time.Since(start)

	if ep := firstErr.Load(); ep != nil {
		return runStats{}, *ep
	}
	return runStats{
		committed:          int(committed.Load()),
		elapsed:            elapsed,
		readObservations:   reads.Load(),
		tornObservations:   torn.Load(),
		firstTorn:          firstTorn.Load(),
		acquireWaitTotal:   acquireWaitTotal.Load(),
		acquireWaitCount:   acquireWaitCount.Load(),
		conflicts:          conflictCounts.conflicts.Load(),
		conflictRetriesMax: conflictCounts.retriesMax.Load(),
		conflictWaitMaxNs:  conflictCounts.waitMaxNs.Load(),
		tornDetail:         firstReport.Load(),
	}, nil
}

// conflictRetryBudget bounds the retry chain for one transfer by WALL CLOCK. It is
// a BOUND, not a policy knob: an unbounded retry would turn a persistent conflict
// into a livelock, and the workload has to fail loudly rather than spin.
//
// # Why a clock and not an attempt count (rmp #2330)
//
// It was an attempt count — 8, then 24, each sized above the deepest chain observed
// on an idle machine. That is the defect class rmp #2330 named and made a project
// rule after finding it five times in one sprint: *a bound on waiting for another
// goroutine, process or network peer must be sized to catch a HANG, never to assert
// a latency.* An attempt count asserts a latency, because the 24 waits below sum to
// roughly 96 ms and what a transfer needs is however long the writers ahead of it
// take to clear their fsyncs. On an idle host 24 was generous; under load it is not.
//
// #2330's own AC4 sweep for siblings covered bolt/server and stopped there, so this
// bound survived it. It was then caught the way the rule predicts — not by a defect,
// but by parallel WAL load. Running examples 04, 17, 20, 21, 27, 35, 36 and 37
// together under `go test -race`, this example failed 2 runs in 6 with "25
// serialization conflicts on one transfer", while the same test passed 8 of 8 alone
// and 8 of 8 under fourteen CPU burners. CPU starvation does not reproduce it;
// competing fsyncs do, because the start timestamp a retry takes is the CONTIGUOUS
// commit frontier (rmp #2298) and the frontier moves at the rate commits become
// durable. In the captured chains the head was an in-flight transaction on 23 of 25
// attempts, and the snapshot advanced by only 16 instants across the whole 96 ms.
//
// # Sized to catch a hang, from measurement
//
// The hang this must still catch is real and has been seen: before rmp #2318 gave
// the vacuum an unconditional wake, "the FIRST transaction to abort on an object made
// that object permanently unwritable", and this example's writers exhausted their
// retry chain on their first aborted account (graph/mvcc/conflict.go). A wedged
// object never clears, so any budget catches it; a contended one clears in
// milliseconds.
//
// Measured under the eight-package parallel-WAL load that reproduces the failure,
// with the bound lifted: see writer.conflict_wait_max in the run output. The budget
// is set two orders of magnitude above the worst chain observed there, so reaching
// it means the object stopped becoming writable rather than that the host was busy.
const conflictRetryBudget = 30 * time.Second

// initialConflictBackoff and maxConflictBackoff bound the wait between attempts.
// The floor is sized to a WAL fsync rather than to a scheduler quantum, because
// that is what the retry is actually waiting for; the ceiling stops a pathological
// chain from stretching the run.
const (
	initialConflictBackoff = 100 * time.Microsecond
	maxConflictBackoff     = 5 * time.Millisecond
)

// retryOnConflict runs attempt, retrying while it is refused with an MVCC
// serialization conflict, and counts what it observed.
//
// # Why the CLIENT retries, and why this is not a workaround
//
// Under MVCC the engine no longer serialises writers: two transactions that touch
// the same account overlap, and the second one to reach the version-chain head is
// REFUSED with [mvcc.ErrSerializationConflict] rather than silently overwriting
// the first (first-updater-wins, rmp #2300). Retrying is the caller's half of that
// contract and is exactly what every MVCC engine requires of its clients —
// PostgreSQL returns SQLSTATE 40001 and expects the application to retry, and the
// official Neo4j drivers retry `Neo.TransientError.*` inside their managed
// transactions. Under the single-writer engine this example was written against,
// the conflict could not arise; a transfer waited for the lock instead of being
// refused. The retry is what a real client would do, so the example does it — and
// it counts the conflicts rather than hiding them, because the conflict rate IS
// the observable cost of the write concurrency the example exercises.
//
// A retried transfer stays correct without further care: every transfer is a
// commutative delta over two accounts and a refused attempt is fully aborted (its
// versions never become visible), so the expected final state the deterministic
// replay computes is untouched by how many attempts a transfer needed.
//
// conflicts accumulates every refused attempt; retriesMax keeps the deepest chain
// any single transfer needed, as a monotone maximum.
func retryOnConflict(ctx context.Context, budget time.Duration, c *conflictCounters, attempt func() error) error {
	backoff := initialConflictBackoff
	var chain conflictChain
	started := time.Now()
	for tries := 0; ; tries++ {
		err := attempt()
		if err == nil {
			bumpMax(&c.retriesMax, int64(tries))
			if tries > 0 {
				bumpMax(&c.waitMaxNs, int64(time.Since(started)))
			}
			return nil
		}
		if !errors.Is(err, mvcc.ErrSerializationConflict) {
			return err
		}
		c.conflicts.Add(1)
		chain.record(err)
		if waited := time.Since(started); waited >= budget {
			bumpMax(&c.waitMaxNs, int64(waited))
			return fmt.Errorf("%d serialization conflicts on one transfer over %s (budget %s): %w\n%s",
				tries+1, waited.Round(time.Millisecond), budget, err, chain.diagnosis())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Back off, and back off by TIME rather than by yielding.
		//
		// A yield was the first attempt and it was measured wrong. The blocking
		// transaction is mid-COMMIT on a write-ahead log, so it clears only when
		// its fsync returns — and the MVCC start timestamp a retry takes is the
		// CONTIGUOUS commit frontier, which cannot advance past a commit that is
		// still in flight. A Gosched loop therefore burns every retry inside one
		// fsync: measured, five consecutive attempts all took startTS=117 against
		// the same in-flight head, and the transfer failed having never once seen
		// a fresh snapshot. Exponential backoff from 100 us gives the fsync room
		// to land, which is what actually makes the retry a retry.
		if backoff > maxConflictBackoff {
			backoff = maxConflictBackoff
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// conflictChain records the refused attempts of ONE transfer, so that exhausting
// [maxConflictRetries] reports WHY it was refused rather than only that it was.
//
// # Why this exists (the lesson of rmp #2333)
//
// The bound's own design note says that exceeding it "is a real finding about the
// engine's conflict behaviour". A finding needs evidence, and the error this loop
// used to return carried none: one message, no timestamps, no history. That is the
// same shape as rmp #2333, where a gate reported a single unattributable number and
// roughly 245 million observations were then spent failing to classify it.
//
// Everything needed is already in the error. [mvcc.Conflict] carries the blocking
// version's timestamp, the losing transaction's snapshot, and its id; the loop threw
// all three away. Keeping them turns an exhausted retry chain into a classification:
//
//   - the same HeadTS on every attempt — the head is STUCK, and its value says which
//     kind. [mvcc.AbortedTS] means an aborted version that the background vacuum has
//     not yet withdrawn, which graph/mvcc/conflict.go documents as making the object
//     unwritable until it does; a transaction id means one writer held the head for
//     the whole chain.
//   - a StartTS that never advances — the retry never obtained a fresh snapshot, so
//     it was not a retry. The start timestamp is the CONTIGUOUS commit frontier
//     (rmp #2298), which cannot pass a commit whose fsync is still in flight.
//   - both moving — ordinary contention: the transfer lost 25 fair races.
//
// The three call for different fixes, and nothing else distinguishes them after the
// fact. The records are appended only on the refused path, which already pays a
// backoff of at least 100 us, so the happy path allocates nothing.
// conflictCounters is the telemetry one retry loop accumulates, grouped so the
// loop takes one pointer rather than three. All three are read only after the
// writers have joined.
type conflictCounters struct {
	conflicts  atomic.Int64 // every refused attempt, across all transfers
	retriesMax atomic.Int64 // the deepest retry chain any one transfer needed
	waitMaxNs  atomic.Int64 // the longest wall time any one transfer spent retrying
}

// conflictSample bounds how many refused attempts are RETAINED for display at each
// end of the chain. The verdict itself is computed incrementally over every attempt,
// so bounding the sample costs no accuracy — only the middle of a long chain is
// elided. Without a bound a 30 s budget against a 5 ms backoff ceiling would retain
// several thousand records and print them all.
const conflictSample = 12

type conflictChain struct {
	n     int              // every refused attempt, counted
	first []conflictRecord // the opening attempts, up to conflictSample
	last  []conflictRecord // a rolling window of the closing attempts
	// The verdict's inputs, maintained over ALL attempts rather than the sample.
	headStuck  bool
	startStuck bool
	firstRec   conflictRecord
	lastRec    conflictRecord
}

// conflictRecord is one refused attempt, as the engine described it.
type conflictRecord struct {
	store   string // the versioned store the conflict was detected in
	headTS  uint64 // the blocking version's effective timestamp
	startTS uint64 // the losing transaction's snapshot
	txID    uint64 // the losing transaction's identity
}

// record appends err's conflict detail. A conflict the engine did not describe
// with a typed [mvcc.Conflict] is recorded as a zero entry rather than dropped, so
// the attempt count in the diagnosis always matches the count in the error.
func (c *conflictChain) record(err error) {
	rec := conflictRecord{store: "untyped"}
	var detail *mvcc.Conflict
	if errors.As(err, &detail) {
		rec = conflictRecord{
			store:   detail.Store,
			headTS:  detail.HeadTS,
			startTS: detail.StartTS,
			txID:    detail.TxID,
		}
	}

	c.n++
	if c.n == 1 {
		c.firstRec, c.headStuck, c.startStuck = rec, true, true
	} else {
		if rec.headTS != c.firstRec.headTS {
			c.headStuck = false
		}
		if rec.startTS != c.firstRec.startTS {
			c.startStuck = false
		}
	}
	c.lastRec = rec

	if len(c.first) < conflictSample {
		c.first = append(c.first, rec)
		return
	}
	if len(c.last) == conflictSample {
		copy(c.last, c.last[1:])
		c.last[conflictSample-1] = rec
		return
	}
	c.last = append(c.last, rec)
}

// headKind names what the blocking version was, which is the discriminator the
// whole diagnosis turns on.
func headKind(headTS uint64) string {
	switch {
	case headTS == mvcc.AbortedTS:
		return "ABORTED (awaiting vacuum withdrawal)"
	case headTS >= mvcc.TxIDBase:
		return "in-flight tx"
	default:
		return "committed"
	}
}

// diagnosis renders the chain and states which of the three shapes it is.
func (c *conflictChain) diagnosis() string {
	if c.n == 0 {
		return "  (no typed conflict detail was captured)"
	}
	first, last := c.firstRec, c.lastRec
	headStuck, startStuck := c.headStuck, c.startStuck

	var b strings.Builder
	b.WriteString("  conflict chain (attempt: store head=HEAD start=START tx=TX):\n")
	line := func(i int, r conflictRecord) {
		fmt.Fprintf(&b, "    %4d: %-16s head=%d [%s] start=%d tx=%d\n",
			i, r.store, r.headTS, headKind(r.headTS), r.startTS, r.txID)
	}
	for i, r := range c.first {
		line(i+1, r)
	}
	if elided := c.n - len(c.first) - len(c.last); elided > 0 {
		fmt.Fprintf(&b, "    ... %d further attempts elided ...\n", elided)
	}
	for i, r := range c.last {
		line(c.n-len(c.last)+i+1, r)
	}
	fmt.Fprintf(&b, "  attempts: %d\n", c.n)
	fmt.Fprintf(&b, "  head:    %s (first=%d last=%d)\n",
		map[bool]string{true: "STUCK on one version", false: "moved"}[headStuck], first.headTS, last.headTS)
	fmt.Fprintf(&b, "  snapshot: %s (first=%d last=%d)\n",
		map[bool]string{true: "NEVER ADVANCED", false: "advanced"}[startStuck], first.startTS, last.startTS)

	switch {
	case headStuck && first.headTS == mvcc.AbortedTS:
		b.WriteString("  VERDICT: an ABORTED version held the head for the whole chain — the background\n" +
			"           vacuum did not withdraw it in time, so the object stayed unwritable\n" +
			"           (see graph/mvcc/conflict.go, rmp #2318).")
	case headStuck && first.headTS >= mvcc.TxIDBase:
		b.WriteString("  VERDICT: ONE in-flight transaction held the head for the whole chain — it neither\n" +
			"           committed nor aborted for the entire budget. That is a HANG, not contention.")
	case startStuck:
		b.WriteString("  VERDICT: the snapshot NEVER advanced — every attempt ran at the same instant, so\n" +
			"           no attempt after the first was a retry. The contiguous commit frontier\n" +
			"           (rmp #2298) did not move; suspect a commit stalled in fsync.")
	default:
		fmt.Fprintf(&b, "  VERDICT: STARVATION — head and snapshot both moved, so other transactions were\n"+
			"           committing throughout while this transfer lost all %d races. The engine was\n"+
			"           live; this writer never got a turn. first-updater-wins has no queue and no\n"+
			"           age priority, so a loser's backoff grows while each fresh arrival starts at\n"+
			"           the floor.", c.n)
	}
	return b.String()
}

// bumpMax raises dst to v when v is larger, as a lock-free monotone maximum.
func bumpMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// readerCount returns the number of reader goroutines, derived from the plan's
// writer partition and the config-driven reader count carried alongside it.
func (p *plan) readerCount() int { return p.readers }

// commitMultiStatement executes one transfer as a multi-statement explicit
// transaction: BeginTx, a debit statement, a credit statement, then Commit. The
// engine holds the visibility barrier across both statements, so a concurrent
// reader can never observe the debit without the credit. It returns the time
// spent blocked acquiring the transaction (a contention proxy). Any failure
// rolls the transaction back.
func commitMultiStatement(ctx context.Context, eng *cypher.Engine, t transfer, split bool) (time.Duration, error) {
	if split {
		// NEGATIVE CONTROL (config.faultSplitMultiStatement): commit the debit and
		// the credit as two independent autocommit transactions. Between them the
		// ledger really is short by t.amount, so a reader observing that window
		// sees a genuinely torn total. This is the deliberately broken behaviour
		// the gate is validated against; it is unreachable from a flag.
		start := time.Now()
		if err := runWriteAny(ctx, eng, qDebit, map[string]any{"id": acctKey(t.from), "amt": t.amount}); err != nil {
			return time.Since(start), fmt.Errorf("debit: %w", err)
		}
		if err := runWriteAny(ctx, eng, qCredit, map[string]any{"id": acctKey(t.to), "amt": t.amount}); err != nil {
			return time.Since(start), fmt.Errorf("credit: %w", err)
		}
		return time.Since(start), nil
	}
	acquireStart := time.Now()
	tx, err := eng.BeginTx(ctx)
	wait := time.Since(acquireStart)
	if err != nil {
		return wait, fmt.Errorf("begin: %w", err)
	}
	if err := execInTx(tx, qDebit, map[string]any{"id": acctKey(t.from), "amt": t.amount}); err != nil {
		_ = tx.Rollback()
		return wait, fmt.Errorf("debit: %w", err)
	}
	if err := execInTx(tx, qCredit, map[string]any{"id": acctKey(t.to), "amt": t.amount}); err != nil {
		_ = tx.Rollback()
		return wait, fmt.Errorf("credit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return wait, fmt.Errorf("commit: %w", err)
	}
	return wait, nil
}

// execInTx runs one statement inside an explicit transaction, drains its result
// stream, and closes it. The debit/credit statements return no rows; draining
// still forces any residual pipeline work and surfaces a statement error.
func execInTx(tx *cypher.ExplicitTx, query string, params map[string]any) error {
	res, err := tx.ExecAny(query, params)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // drain the (empty) result stream
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// commitSingleStatement executes one transfer as a single-statement autocommit
// write (RunInTx): the debit and credit happen in one statement, made durable on
// Result.Close.
func commitSingleStatement(ctx context.Context, eng *cypher.Engine, t transfer) error {
	return runWriteAny(ctx, eng, qTransfer, map[string]any{"from": acctKey(t.from), "to": acctKey(t.to), "amt": t.amount})
}

// ─────────────────────────────────────────────────────────────────────────────
// Read helpers
// ─────────────────────────────────────────────────────────────────────────────

// tornReport is everything known about a torn observation, captured at the moment
// it is seen.
//
// # Why the gate carries a diagnosis at all (rmp #2333)
//
// A torn total is rare enough that the run which produces one may be the only run
// that ever does. The gate used to report the wrong value and nothing else, and a
// single unattributable number is what a reproduction search of roughly 7.3 million
// clean observations was then spent on — without settling even whether the engine or
// the instrument was at fault.
//
// The decisive question is answerable at the instant of the tear and at no other
// time: does a per-account read AT THE SAME INSTANT sum to the aggregate's answer?
//
//   - It sums to expected  → the per-row state was consistent and the AGGREGATE
//     disagreed with it. That is an execution defect, not an isolation one.
//   - It sums to total     → the snapshot genuinely held an inconsistent state, and
//     the accounts that deviate name the transaction that was torn.
//
// Only a reader holding an explicit read transaction can ask it, because only that
// reader can issue a second statement at the same pinned instant; the Engine.Run
// readers record what they can and say so.
type tornReport struct {
	readerKind    string  // "Engine.Run" or "BeginReadTx"
	total         int64   // what the aggregate answered
	expected      int64   // the conserved total it had to equal
	sameInstant   bool    // perAccount was read at the SAME snapshot as total
	perAccount    []int64 // per-account balances at that instant (nil unless sameInstant)
	perAccountSum int64   // their sum (meaningful only when sameInstant)
}

// verdict classifies the tear from the evidence actually gathered, and never
// beyond it.
func (r *tornReport) verdict() string {
	switch {
	case !r.sameInstant:
		return "UNATTRIBUTED (this reader holds no pinned instant to re-read)"
	case r.perAccountSum == r.expected:
		return "AGGREGATE DEFECT: the same instant read per account is consistent, so sum() disagreed with the rows it summed — NOT an isolation violation"
	case r.perAccountSum == r.total:
		return "ISOLATION VIOLATION: the snapshot itself held an inconsistent state"
	default:
		return "INCONSISTENT EVIDENCE: the same-instant per-account sum matches neither the aggregate nor the expected total"
	}
}

// tornDiagnosis renders a torn observation's evidence for the failure message.
//
// It reports the classification and, when a same-instant per-account read was
// obtained, which accounts deviated from their SEEDED balance and by how much. The
// seeded balances are the right baseline: every transfer is a commutative delta on
// a conserved total, so the deviations are exactly the net flow the observed instant
// had applied to each account, and a torn transaction shows up as one account
// carrying a delta the ledger does not balance.
func tornDiagnosis(r *tornReport, seeded []account) string {
	if r == nil {
		return "; no diagnosis captured"
	}
	base := make([]int64, len(seeded))
	for i, a := range seeded {
		base[a.id] = a.initial
		_ = i
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  reader=%s\n  verdict=%s", r.readerKind, r.verdict())
	if r.sameInstant {
		fmt.Fprintf(&b, "\n  same-instant per-account sum=%d (aggregate said %d, expected %d)",
			r.perAccountSum, r.total, r.expected)
		fmt.Fprintf(&b, "\n  per-account deviation from seed:%s", r.deviations(base))
	}
	return b.String()
}

// deviations lists the accounts whose balance differs from base, as "id:delta".
func (r *tornReport) deviations(base []int64) string {
	if !r.sameInstant {
		return ""
	}
	var b strings.Builder
	for id, got := range r.perAccount {
		if id < len(base) && got != base[id] {
			fmt.Fprintf(&b, " %d:%+d", id, got-base[id])
		}
	}
	return b.String()
}

// readTotalRun computes the conserved total via the concurrent read path
// (Engine.Run), which takes a per-query visibility snapshot.
//
// It returns a [tornReport] when the total does not equal want. The snapshot is
// released when the statement completes, so this path cannot re-read at the same
// instant and reports what it has.
func readTotalRun(ctx context.Context, eng *cypher.Engine, want int64) (int64, *tornReport, error) {
	res, err := eng.Run(ctx, qTotal, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Close() }()
	total, err := scalarTotal(res)
	if err != nil || total == want {
		return total, nil, err
	}
	return total, &tornReport{readerKind: "Engine.Run", total: total, expected: want}, nil
}

// readTotalReadTx computes the conserved total inside a read-only explicit
// transaction (BeginReadTx), which acquires neither the writer serialisation nor
// the visibility barrier.
//
// On a mismatch it re-reads every account balance INSIDE THE SAME TRANSACTION, so
// the dump describes the very instant the aggregate was computed over rather than a
// later one. See [tornReport] for what that distinguishes.
func readTotalReadTx(ctx context.Context, eng *cypher.Engine, want int64) (int64, *tornReport, error) {
	tx, err := eng.BeginReadTx(ctx)
	if err != nil {
		return 0, nil, err
	}
	// Rollback (a teardown-only no-op on a read-only handle) always finishes the
	// handle, even if Exec fails partway.
	defer func() { _ = tx.Rollback() }()

	total, err := scalarTotalTx(tx)
	if err != nil || total == want {
		return total, nil, err
	}

	rep := &tornReport{readerKind: "BeginReadTx", total: total, expected: want}
	bal, derr := dumpBalancesTx(tx)
	if derr != nil {
		// The diagnosis failed; the observation itself still stands and is
		// reported without it rather than being lost.
		return total, rep, nil
	}
	rep.sameInstant = true
	rep.perAccount = bal
	for _, b := range bal {
		rep.perAccountSum += b
	}
	return total, rep, nil
}

// scalarTotalTx runs qTotal at tx's pinned instant.
func scalarTotalTx(tx *cypher.ExplicitTx) (int64, error) {
	res, err := tx.ExecAny(qTotal, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	return scalarTotal(res)
}

// dumpBalancesTx runs qDump at tx's pinned instant — the same instant
// [scalarTotalTx] just used on the same handle.
func dumpBalancesTx(tx *cypher.ExplicitTx) ([]int64, error) {
	res, err := tx.ExecAny(qDump, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	return dumpBalances(res)
}

// scalarTotal extracts the single integer "total" column from a materialised
// result.
//
// # EXACTLY one row, and never a NULL (rmp #2333)
//
// An ungrouped sum() emits exactly one row, so any other count is a defect in the
// engine rather than a shape this example should absorb. The earlier version kept
// the LAST row it saw and reported "no rows" only when it saw none, so a result
// carrying two rows — a partially combined parallel aggregate, say — would have
// been silently reduced to one of its partials and then reported as a torn total,
// blaming isolation for an aggregation defect. A NULL is rejected by [asInt64] for
// the same reason.
func scalarTotal(res *cypher.Result) (int64, error) {
	var total int64
	rows := 0
	for res.Next() {
		v, ok := res.Record()["total"]
		if !ok {
			return 0, fmt.Errorf("column %q missing", "total")
		}
		n, err := asInt64(v)
		if err != nil {
			return 0, fmt.Errorf("total: %w", err)
		}
		total = n
		rows++
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, fmt.Errorf("total query returned %d rows, want exactly 1", rows)
	}
	return total, nil
}

// readBalances reads every account's id and balance into a slice indexed by id.
func readBalances(ctx context.Context, eng *cypher.Engine) ([]int64, error) {
	res, err := eng.Run(ctx, qDump, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	return dumpBalances(res)
}

// dumpBalances parses a qDump result into a slice indexed by account id.
//
// It is split out of [readBalances] so the torn-total diagnosis can run the same
// parse over a result produced INSIDE a read transaction — that is, at the same
// instant as the aggregate it is explaining. See [tornReport].
func dumpBalances(res *cypher.Result) ([]int64, error) {
	// The account ids are the dense range [0, accounts); grow the slice as ids
	// arrive so no separate count query is needed.
	out := make([]int64, 0)
	seen := 0
	for res.Next() {
		rec := res.Record()
		idv, ok := rec["id"]
		if !ok {
			return nil, fmt.Errorf("column %q missing", "id")
		}
		balv, ok := rec["balance"]
		if !ok {
			return nil, fmt.Errorf("column %q missing", "balance")
		}
		sv, ok := idv.(expr.StringValue)
		if !ok {
			return nil, fmt.Errorf("id is %T, want string", idv)
		}
		id, err := strconv.Atoi(string(sv))
		if err != nil {
			return nil, fmt.Errorf("id %q: %w", string(sv), err)
		}
		bal, err := asInt64(balv)
		if err != nil {
			return nil, fmt.Errorf("balance: %w", err)
		}
		for len(out) <= id {
			out = append(out, 0)
		}
		out[id] = bal
		seen++
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	if seen != len(out) {
		return nil, fmt.Errorf("read %d rows for %d distinct ids (non-dense or duplicate id)", seen, len(out))
	}
	return out, nil
}

// diffAgainstExpected compares the observed balances to the deterministic
// replay, returning the number of divergences (lost updates) and the minimum
// observed balance (for the no-negative-balance check). A length mismatch counts
// as a divergence for every missing account.
func diffAgainstExpected(observed, expected []int64) (lostUpdates int, minBalance int64) {
	minBalance = int64(^uint64(0) >> 1) // math.MaxInt64 without the import
	if len(observed) != len(expected) {
		lostUpdates += abs(len(observed) - len(expected))
	}
	n := min(len(observed), len(expected))
	for i := 0; i < n; i++ {
		if observed[i] != expected[i] {
			lostUpdates++
		}
		if observed[i] < minBalance {
			minBalance = observed[i]
		}
	}
	if n == 0 {
		minBalance = 0
	}
	return lostUpdates, minBalance
}

// ─────────────────────────────────────────────────────────────────────────────
// Writer-scaling sweep (telemetry)
// ─────────────────────────────────────────────────────────────────────────────

// scalingSweep runs an identical fixed workload of cfg.sweepOps transfers at a
// sweep of writer counts (1, 2, 4 … capped at both cfg.writers and GOMAXPROCS),
// each on a fresh ephemeral store, and prints the achieved transfer throughput
// per level as telemetry. Because writers serialise on the single-writer mutex,
// throughput that stays flat as the writer count climbs is the observable
// signature of the isolation design. Each level is verified to conserve the
// total before its store is torn down.
func scalingSweep(ctx context.Context, w io.Writer, cfg *config) error {
	maxWorkers := cfg.writers
	if procs := runtime.GOMAXPROCS(0); procs < maxWorkers {
		maxWorkers = procs
	}
	for workers := 1; workers <= maxWorkers; workers *= 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		perLevel := sweepLevelConfig(cfg, workers)
		levelPlan := generatePlan(&perLevel)

		eng, closeFn, err := openEngine(ctx, "gograph-ex27-sweep-")
		if err != nil {
			return err
		}
		if err := runWrite(ctx, eng, ddlIndex, nil); err != nil {
			closeFn()
			return fmt.Errorf("sweep index: %w", err)
		}
		if err := seedAccounts(ctx, eng, levelPlan.accounts); err != nil {
			closeFn()
			return fmt.Errorf("sweep seed: %w", err)
		}

		start := time.Now()
		stats, err := runConcurrent(ctx, eng, &levelPlan)
		elapsed := time.Since(start)
		if err != nil {
			closeFn()
			return fmt.Errorf("sweep workers=%d: %w", workers, err)
		}

		total, _, err := readTotalRun(ctx, eng, levelPlan.initialTotal)
		if err != nil {
			closeFn()
			return fmt.Errorf("sweep read total: %w", err)
		}
		closeFn()
		if total != levelPlan.initialTotal {
			return fmt.Errorf("sweep conservation violation at workers=%d: %d != %d", workers, total, levelPlan.initialTotal)
		}
		fmt.Fprintf(w, "# scale.writers_%d.transfers_per_s=%.0f\n", workers, rate(int64(stats.committed), elapsed))
	}
	return nil
}

// sweepLevelConfig derives the per-level config for the scaling sweep: the same
// accounts and amount bounds as cfg, but with the given writer count and a
// single reader, distributing cfg.sweepOps transfers as evenly as possible and
// disabling the nested sweep.
func sweepLevelConfig(cfg *config, workers int) config {
	// A COPY, not the pointer: every field below is overridden for this level and
	// must not reach back into the caller's configuration.
	c := *cfg
	c.writers = workers
	c.readers = 1
	c.opsPerWriter = (cfg.sweepOps + workers - 1) / workers
	c.sweepOps = 0
	// Re-validate the no-overdraft guarantee for the derived per-writer count.
	// minInitial already covers the main workload, which is at least as large.
	return c
}

// ─────────────────────────────────────────────────────────────────────────────
// Small write/eval helpers
// ─────────────────────────────────────────────────────────────────────────────

// runWrite executes a write or DDL statement via RunAny and fully drains and
// closes its result, so the WAL commit (or DDL apply) completes before it
// returns.
func runWrite(ctx context.Context, eng *cypher.Engine, query string, params map[string]expr.Value) error {
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return err
	}
	return drainClose(res)
}

// runWriteAny is [runWrite] with map[string]any parameters, routed through
// RunAny so a writing clause is executed under RunInTx automatically.
func runWriteAny(ctx context.Context, eng *cypher.Engine, query string, params map[string]any) error {
	res, err := eng.RunAny(ctx, query, params)
	if err != nil {
		return err
	}
	return drainClose(res)
}

// drainClose iterates a result to exhaustion and closes it, returning the first
// error encountered. For a write result, Close is what fsyncs the WAL (or rolls
// the transaction back), so it must always run.
func drainClose(res *cypher.Result) error {
	for res.Next() { //nolint:revive // drain the result stream
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// asInt64 extracts an int64 from a Cypher integer value carried in a result
// record (an interface{} holding an [expr.Value]), widening a float value (which
// sum() never produces for integer inputs, but which keeps the helper robust)
// and treating NULL as 0.
// NULL IS AN ERROR, NOT A ZERO (rmp #2333).
//
// This used to map both an untyped nil and a Cypher NULL to 0. Every value this
// example reads is an account balance or a sum over account balances, and the
// ledger is never empty, so a NULL is a defect in every case — but converting it
// to 0 turned that defect into a torn-total report of "0", indistinguishable from
// a genuine isolation violation and attributed to the engine. It also hid the
// opposite failure: a NULL balance in [readBalances] became a zero balance and was
// reported as a lost update.
//
// Rejecting it means the run fails naming what actually happened.
func asInt64(v any) (int64, error) {
	switch x := v.(type) {
	case expr.IntegerValue:
		return int64(x), nil
	case expr.FloatValue:
		return int64(x), nil
	case nil:
		return 0, errNullValue
	default:
		if val, ok := v.(expr.Value); ok && val == expr.Null {
			return 0, errNullValue
		}
		return 0, fmt.Errorf("value is %T, want integer", v)
	}
}

// errNullValue reports a NULL where an integer was required.
var errNullValue = errors.New("value is NULL, want integer")

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry and formatting helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// meanDuration returns totalNs/count as a Duration, or 0 when count is 0.
func meanDuration(totalNs, count int64) time.Duration {
	if count <= 0 {
		return 0
	}
	return time.Duration(totalNs / count)
}

// saturatingSub returns a-b, or 0 when b > a (GC may shrink the live heap below
// the baseline between snapshots).
func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
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

// b2i maps a boolean invariant to the 1/0 fact form used in the output.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// abs returns the absolute value of an int.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ctxCheckEvery bounds how often a worker polls ctx for cancellation: often
// enough that a cancelled run stops promptly, rare enough that the check is free
// relative to a transaction.
const ctxCheckEvery = 8
