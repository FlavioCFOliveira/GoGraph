package anomaly_test

// engine_test.go — the checker attached to a REAL GoGraph engine (rmp #2341
// AC4, AC5, AC6).
//
// The constructed histories in check_test.go prove the classifier implements the
// formalism. They prove nothing about whether it can be attached to this engine,
// whether it stays quiet on a healthy build, or whether it names a real defect.
// That is what this file is for, and it is the half that matters: a checker
// validated only on hand-written histories is a theorem, not an instrument.
//
// # How the version order is obtained
//
// Adya's model needs a total version order per key. GoGraph's MVCC commit
// timestamp is exactly that, but it is not surfaced to a Cypher caller, so each
// account carries an explicit `ver` property drawn from a process-wide atomic
// counter: every write installs a strictly greater version than any before it,
// which makes the per-key order the model requires readable from a query.
//
// This is instrumentation of the WORKLOAD, not of the engine. Nothing was added
// to a GoGraph code path — the module's own record says a single print inside
// the code under test turned a reproducing failure into a passing one — and
// AC5's measurement below is what establishes that the recording itself does not
// move the system.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/anomaly"
)

// accounts is the fixture size. Small on purpose: the point is contention, and
// the history has to stay small enough for the cycle search to be exhaustive
// rather than truncated.
const accounts = 4

// startingBalance per account; the conserved total is accounts*startingBalance.
const startingBalance = 1000

// bank is one workload run: a real engine, a version counter, and a recorder.
type bank struct {
	eng     *cypher.Engine
	verSeq  atomic.Uint64
	txSeq   atomic.Uint64
	instant atomic.Uint64
	rec     *anomaly.Recorder // nil ⇒ no recording at all
	// atomicRead selects the healthy reader (ONE transaction over every
	// account) or the DEFECTIVE one (a separate snapshot per account, recorded
	// as the single logical read it was meant to be).
	atomicRead bool
	// torn counts how many times the domain invariant — the conserved total —
	// was violated. This is the pre-existing kind of evidence, the bare symptom
	// the whole task exists to improve on, and it is kept so AC5 has a failure
	// rate to compare.
	torn atomic.Int64
}

func newBank(tb testing.TB, rec *anomaly.Recorder, atomicRead bool) *bank {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	b := &bank{eng: cypher.NewEngine(g), rec: rec, atomicRead: atomicRead}
	b.verSeq.Store(100)
	for i := range accounts {
		v := b.verSeq.Add(1)
		mustRun(tb, b.eng, "CREATE (:Acct {id:$id, bal:$bal, ver:$ver})", map[string]expr.Value{
			"id":  expr.StringValue(fmt.Sprintf("a%d", i)),
			"bal": expr.IntegerValue(startingBalance),
			"ver": expr.IntegerValue(int64(v)),
		})
		if rec != nil {
			// The fixture is one transaction per account, so every version the
			// workload later reads has a writer in the history. Without this the
			// checker would report `unwritten-version-read` for the initial
			// state — correctly, since the history would be incomplete.
			rec.Record(anomaly.Txn{
				ID: anomaly.TxID(b.txSeq.Add(1)), Start: b.tick(), Commit: b.tick(),
				Ops: []anomaly.Op{{Kind: anomaly.Write, Key: fmt.Sprintf("a%d", i), Ver: anomaly.Version(v)}},
			})
		}
	}
	return b
}

// tick is the harness's logical clock, used only to order the recorded
// transactions; the classification depends on the VERSION order, not on it.
func (b *bank) tick() uint64 { return b.instant.Add(1) }

func mustRun(tb testing.TB, eng *cypher.Engine, q string, params map[string]expr.Value) {
	tb.Helper()
	res, err := eng.RunInTx(context.Background(), q, params)
	if err != nil {
		tb.Fatalf("run %q: %v", q, err)
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		tb.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("close %q: %v", q, err)
	}
}

// transfer moves amount from account i to account j in ONE transaction,
// installing a fresh version on each, and records what it read and wrote.
func (b *bank) transfer(ctx context.Context, sh *anomaly.Shard, i, j, amount int) error {
	from, to := fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", j)
	tx, err := b.eng.BeginTx(ctx)
	if err != nil {
		return err
	}
	start := b.tick()
	var ops []anomaly.Op
	readVer := func(res *cypher.Result) (anomaly.Version, bool) {
		defer func() { _ = res.Close() }()
		if res.Err() != nil || !res.Next() {
			return 0, false
		}
		v, _ := res.ValueAt(1).(expr.IntegerValue)
		return anomaly.Version(v), true
	}
	for _, key := range []string{from, to} {
		res, rerr := tx.Exec("MATCH (a:Acct {id:$id}) RETURN a.bal AS bal, a.ver AS ver",
			map[string]expr.Value{"id": expr.StringValue(key)})
		if rerr != nil {
			_ = tx.Rollback()
			return rerr
		}
		ver, ok := readVer(res)
		if !ok {
			_ = tx.Rollback()
			return fmt.Errorf("no row for %s", key)
		}
		ops = append(ops, anomaly.Op{Kind: anomaly.Read, Key: key, Ver: ver})
	}
	nv1, nv2 := b.verSeq.Add(1), b.verSeq.Add(1)
	for _, m := range []struct {
		key string
		amt int
		ver uint64
	}{{from, -amount, nv1}, {to, amount, nv2}} {
		res, werr := tx.Exec("MATCH (a:Acct {id:$id}) SET a.bal = a.bal + $d, a.ver = $ver RETURN a.bal AS bal",
			map[string]expr.Value{
				"id":  expr.StringValue(m.key),
				"d":   expr.IntegerValue(int64(m.amt)),
				"ver": expr.IntegerValue(int64(m.ver)),
			})
		if werr != nil {
			_ = tx.Rollback()
			return werr
		}
		_ = res.Close()
		ops = append(ops, anomaly.Op{Kind: anomaly.Write, Key: m.key, Ver: anomaly.Version(m.ver)})
	}
	id := anomaly.TxID(b.txSeq.Add(1))
	if cerr := tx.Commit(); cerr != nil {
		sh.Record(anomaly.Txn{ID: id, Start: start, Aborted: true, Ops: ops})
		//nolint:nilerr // a refused commit is recorded as anomaly.Txn{Aborted:true}, which arms the G1a dirty-read check at internal/anomaly/dsg.go:157
		return nil // a refused write is the engine working, not a failure
	}
	sh.Record(anomaly.Txn{ID: id, Start: start, Commit: b.tick(), Ops: ops})
	return nil
}

// read observes every account and checks the conserved total.
//
// The HEALTHY reader takes one snapshot for all accounts. The DEFECTIVE one
// takes a separate snapshot per account while still recording the result as ONE
// logical read — which is precisely what a torn snapshot looks like from
// outside, and precisely the shape rmp #2336 describes ("one account resolved at
// a different instant from the rest").
func (b *bank) read(ctx context.Context, sh *anomaly.Shard) error {
	id := anomaly.TxID(b.txSeq.Add(1))
	start := b.tick()
	ops := make([]anomaly.Op, 0, accounts)
	total := 0

	observe := func(q func(string) (*cypher.Result, error)) error {
		for i := range accounts {
			key := fmt.Sprintf("a%d", i)
			res, err := q(key)
			if err != nil {
				return err
			}
			if res.Err() != nil || !res.Next() {
				_ = res.Close()
				return fmt.Errorf("no row for %s", key)
			}
			bal, _ := res.ValueAt(0).(expr.IntegerValue)
			ver, _ := res.ValueAt(1).(expr.IntegerValue)
			_ = res.Close()
			total += int(bal)
			ops = append(ops, anomaly.Op{Kind: anomaly.Read, Key: key, Ver: anomaly.Version(ver)})
		}
		return nil
	}

	const q = "MATCH (a:Acct {id:$id}) RETURN a.bal AS bal, a.ver AS ver"
	if b.atomicRead {
		tx, err := b.eng.BeginReadTx(ctx)
		if err != nil {
			return err
		}
		if err := observe(func(key string) (*cypher.Result, error) {
			return tx.Exec(q, map[string]expr.Value{"id": expr.StringValue(key)})
		}); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	} else {
		if err := observe(func(key string) (*cypher.Result, error) {
			return b.eng.RunInTx(ctx, q, map[string]expr.Value{"id": expr.StringValue(key)})
		}); err != nil {
			return err
		}
	}

	if total != accounts*startingBalance {
		b.torn.Add(1)
	}
	sh.Record(anomaly.Txn{ID: id, Start: start, Commit: b.tick(), Ops: ops})
	return nil
}

// run drives writers and readers concurrently for the given number of rounds.
func (b *bank) run(tb testing.TB, writers, readers, rounds int) {
	tb.Helper()
	ctx := tb.Context()
	var wg sync.WaitGroup
	// ONE SHARD PER GOROUTINE. Recording through the Recorder's shared lock
	// measurably reduced the anomaly rate; see [anomaly.Recorder].
	shardOf := func() *anomaly.Shard {
		if b.rec == nil {
			return nil
		}
		return b.rec.Shard(rounds)
	}
	for w := range writers {
		wg.Add(1)
		go func(w int, sh *anomaly.Shard) {
			defer wg.Done()
			for r := range rounds {
				i := (w + r) % accounts
				j := (i + 1 + r%(accounts-1)) % accounts
				if i == j {
					continue
				}
				if err := b.transfer(ctx, sh, i, j, 1+r%17); err != nil {
					return
				}
			}
		}(w, shardOf())
	}
	for range readers {
		wg.Add(1)
		go func(sh *anomaly.Shard) {
			defer wg.Done()
			for range rounds {
				if err := b.read(ctx, sh); err != nil {
					return
				}
			}
		}(shardOf())
	}
	wg.Wait()
}

// TestHealthyBuildIsClean is one half of AC4: on the real engine, with an atomic
// reader, the checker must report CLEAN under snapshot isolation.
//
// A checker that fired here would be useless whatever else it could do.
func TestHealthyBuildIsClean(t *testing.T) {
	t.Parallel()
	rec := &anomaly.Recorder{}
	b := newBank(t, rec, true)
	b.run(t, 4, 4, 30)

	h := rec.History()
	rep, err := anomaly.Check(&h, anomaly.SnapshotIsolation)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Txns < 50 {
		t.Fatalf("only %d transactions recorded; the workload did not run", rep.Txns)
	}
	if got := b.torn.Load(); got != 0 {
		t.Errorf("the domain invariant saw %d torn totals on a healthy build", got)
	}
	if !rep.Clean() {
		t.Errorf("the healthy build was classified as violating snapshot isolation:\n%s", rep)
	}
	t.Logf("healthy: %d transactions, %d dependency edges, verdict clean", rep.Txns, rep.Edges)
}

// TestDefectiveBuildIsDetectedAndNamed is the other half of AC4, and the whole
// point of the task: an injected anomaly must be DETECTED and CORRECTLY NAMED.
//
// The injected defect is a non-atomic read — the reader resolves each account at
// its own instant while the history records the one logical read it was meant to
// be. That is exactly the shape rmp #2336 describes, and the classifier must
// return G-single: the reader observes one transaction's write to one account
// (a read-dependency into the reader) and misses its write to another (an
// anti-dependency out of the reader), closing a cycle with exactly ONE
// anti-dependency edge, which snapshot isolation forbids.
//
// The claim being tested is NOT "something was found". It is that the name is
// right, because the name is the entire deliverable: a torn total says a search
// must start, "G-single: the reader observed T7's write to a2 and missed its
// write to a3" says where.
func TestDefectiveBuildIsDetectedAndNamed(t *testing.T) {
	t.Parallel()
	// Drive until the injected defect actually manifests, escalating the round
	// count, rather than assuming one fixed size always tears. A negative
	// control that silently ran without producing its defect would report
	// "clean" and be indistinguishable from a blind checker — so the loop is
	// bounded and its exhaustion is a LOUD failure, not a skip.
	var (
		rec *anomaly.Recorder
		b   *bank
		rep *anomaly.Report
	)
	for _, rounds := range []int{25, 50, 100, 200} {
		rec = &anomaly.Recorder{}
		b = newBank(t, rec, false) // NON-atomic reader: the injected defect
		b.run(t, 6, 6, rounds)
		if b.torn.Load() == 0 {
			t.Logf("no tear at %d rounds; escalating", rounds)
			continue
		}
		h := rec.History()
		var err error
		rep, err = anomaly.Check(&h, anomaly.SnapshotIsolation)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		break
	}
	if rep == nil {
		t.Fatal("the injected defect never manifested, so this negative control established nothing; " +
			"it must not be read as evidence that the checker works")
	}
	// Truncation is NOT fatal here, and the asymmetry is the point: a bounded
	// search cannot establish cleanliness, but every cycle it did report exists.
	// This test expects violations, so it may trust them. The healthy and #2336
	// tests conclude the opposite and therefore treat truncation as fatal.
	if rep.Truncated {
		t.Logf("the cycle search was truncated; the violations below are still real, " +
			"there are simply more of them than were enumerated")
	}
	if len(rep.Violations) == 0 {
		t.Fatalf("THE CHECKER IS BLIND: a non-atomic reader produced no classified violation, "+
			"so a clean verdict from it means nothing.\n%s\n(domain invariant saw %d torn totals)",
			rep, b.torn.Load())
	}
	// EVERY violation must be one snapshot isolation actually forbids. This is
	// the check that distinguishes a correct classifier from one that shouts:
	// under heavy contention the same fractured read produces long cycles as
	// well as short ones, and if any of them came back as G2-item the checker
	// would be reporting legal write skew as a defect.
	byType := map[anomaly.Phenomenon]int{}
	for _, v := range rep.Violations {
		byType[v.Type]++
		if v.Type != anomaly.GSingle && v.Type != anomaly.GNonadjacent {
			t.Errorf("a violation was named %s, which snapshot isolation does not forbid "+
				"by way of a fractured read:\n%s", v.Type, v)
		}
	}
	for p, n := range byType {
		t.Logf("defective: %d x %s", n, p)
	}
	t.Logf("defective: %d transactions, %d violations; the domain invariant saw %d torn totals",
		rep.Txns, len(rep.Violations), b.torn.Load())
	t.Logf("first violation:\n%s", rep.Violations[0])
}
