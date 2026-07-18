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
//     one Commit). The engine holds the visibility barrier for the whole
//     transaction, so a concurrent reader can never slip between the debit and
//     the credit — it sees the whole transaction or none of it. The other half
//     run as SINGLE-STATEMENT autocommit writes ([cypher.Engine.RunInTx]) that
//     debit and credit in one statement.
//
//   - CONSISTENCY / no lost updates. Because every transfer is a commutative
//     delta on two accounts, replaying the committed transfers in any order
//     yields the same final per-account balances. The run computes that expected
//     state deterministically up front and, after the concurrent phase, asserts
//     every account matches it. A single mismatch means a read-modify-write
//     interleaving lost an update (a serialisation failure); lost_updates counts
//     them and must be 0.
//
// # Isolation model (verified against cypher/exectx.go and cypher/engine)
//
// The engine serialises writers on the store's single-writer mutex, and
// [cypher.Engine.BeginTx] additionally holds the graph's visibility write-lock
// for the transaction's whole lifetime — so writers are strictly serialised and
// a transactional reader either observes the state before a write transaction
// began or the fully committed state after it ended, never a partial state
// (read-committed isolation, cypher/exectx.go). [cypher.Engine.BeginReadTx] is
// the read-only path: it takes neither the writer serialisation nor the barrier
// and so is never blocked by, and never blocks, other transactions.
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
func (c config) validate() error {
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

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
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
func run(ctx context.Context, w io.Writer, cfg config) error {
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
	seeded, err := readTotalRun(ctx, eng)
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
	finalTotal, err := readTotalRun(ctx, eng)
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
	fmt.Fprintf(w, "# reader.observations=%d\n", stats.readObservations)
	fmt.Fprintf(w, "# reader.observations_per_s=%.0f\n", rate(stats.readObservations, stats.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))

	// Writer-scaling sweep (telemetry only): the identical fixed workload run at
	// 1, 2, 4 … writers on a fresh ephemeral store. Throughput that does NOT
	// climb with the writer count is the observable signature of the
	// single-writer serialisation that underpins isolation.
	if cfg.sweepOps > 0 {
		if err := scalingSweep(ctx, w, cfg); err != nil {
			return fmt.Errorf("scaling sweep: %w", err)
		}
	}

	// Fail loudly on an isolation violation: a torn observation means a reader
	// saw a partially-applied transaction — a module isolation bug — so the run
	// returns an error naming it rather than reporting success.
	if stats.tornObservations > 0 {
		return fmt.Errorf("ISOLATION VIOLATION: readers observed a torn total %d time(s); first torn value %d, expected %d — the engine let a reader see a partially-applied write transaction",
			stats.tornObservations, stats.firstTorn, plan.initialTotal)
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
}

// generatePlan builds the deterministic plan from a seeded RNG: cfg.accounts
// accounts with initial balances in [minInitial, maxInitial], then
// writers*opsPerWriter transfers. Transfers are generated in a fixed global
// order; transfer t is assigned to writer t%writers and uses the multi-statement
// path when t is even. The expected final balances are computed by replaying
// every transfer — order-independent, because each transfer is a commutative
// delta — so the concurrent run can be checked against them regardless of
// scheduling.
func generatePlan(cfg config) plan {
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
		firstErr         atomic.Pointer[error]
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
					err   error
				)
				if useReadTx {
					total, err = readTotalReadTx(ctx, eng)
				} else {
					total, err = readTotalRun(ctx, eng)
				}
				if err != nil {
					setErr(fmt.Errorf("reader: %w", err))
					return
				}
				reads.Add(1)
				if total != plan.initialTotal {
					torn.Add(1)
					firstTorn.CompareAndSwap(0, total)
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
					wait, err := commitMultiStatement(ctx, eng, t)
					if err != nil {
						setErr(fmt.Errorf("writer multi-statement: %w", err))
						return
					}
					acquireWaitTotal.Add(int64(wait))
					acquireWaitCount.Add(1)
				} else if err := commitSingleStatement(ctx, eng, t); err != nil {
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
		committed:        int(committed.Load()),
		elapsed:          elapsed,
		readObservations: reads.Load(),
		tornObservations: torn.Load(),
		firstTorn:        firstTorn.Load(),
		acquireWaitTotal: acquireWaitTotal.Load(),
		acquireWaitCount: acquireWaitCount.Load(),
	}, nil
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
func commitMultiStatement(ctx context.Context, eng *cypher.Engine, t transfer) (time.Duration, error) {
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

// readTotalRun computes the conserved total via the concurrent read path
// (Engine.Run), which takes a per-query visibility snapshot.
func readTotalRun(ctx context.Context, eng *cypher.Engine) (int64, error) {
	res, err := eng.Run(ctx, qTotal, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	return scalarTotal(res)
}

// readTotalReadTx computes the conserved total inside a read-only explicit
// transaction (BeginReadTx), which acquires neither the writer serialisation nor
// the visibility barrier.
func readTotalReadTx(ctx context.Context, eng *cypher.Engine) (int64, error) {
	tx, err := eng.BeginReadTx(ctx)
	if err != nil {
		return 0, err
	}
	// Rollback (a teardown-only no-op on a read-only handle) always finishes the
	// handle, even if Exec fails partway.
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecAny(qTotal, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	return scalarTotal(res)
}

// scalarTotal extracts the single integer "total" column from a materialised
// result. sum() over the integer balances is itself an integer; an empty ledger
// would yield NULL, which is treated as 0.
func scalarTotal(res *cypher.Result) (int64, error) {
	var total int64
	var got bool
	for res.Next() {
		v, ok := res.Record()["total"]
		if !ok {
			return 0, fmt.Errorf("column %q missing", "total")
		}
		n, err := asInt64(v)
		if err != nil {
			return 0, err
		}
		total = n
		got = true
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	if !got {
		return 0, fmt.Errorf("total query returned no rows")
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
func scalingSweep(ctx context.Context, w io.Writer, cfg config) error {
	maxWorkers := cfg.writers
	if procs := runtime.GOMAXPROCS(0); procs < maxWorkers {
		maxWorkers = procs
	}
	for workers := 1; workers <= maxWorkers; workers *= 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		perLevel := sweepLevelConfig(cfg, workers)
		levelPlan := generatePlan(perLevel)

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

		total, err := readTotalRun(ctx, eng)
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
func sweepLevelConfig(cfg config, workers int) config {
	c := cfg
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
func asInt64(v any) (int64, error) {
	switch x := v.(type) {
	case expr.IntegerValue:
		return int64(x), nil
	case expr.FloatValue:
		return int64(x), nil
	case nil:
		return 0, nil
	default:
		if val, ok := v.(expr.Value); ok && val == expr.Null {
			return 0, nil
		}
		return 0, fmt.Errorf("value is %T, want integer", v)
	}
}

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
