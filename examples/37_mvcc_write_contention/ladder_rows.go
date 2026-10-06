package main

// ladder_rows.go — the arms of the concurrency ladder (rmp #2934): the §2 rows of
// docs/mvcc-scenario-catalogue.md, L01-L20, and MG11. See ladder.go for the
// driver, the output contract and the sampler.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/anomaly"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// workerErr returns err when it is a failure of the arm rather than an outcome
// the engine documents for concurrent writers (a refusal, a constraint violation,
// a deliberate rollback). A worker returns only the former, which aborts the run.
func workerErr(err error) error {
	if err == nil || errors.Is(err, errRolledBack) || isTypedRefusal(err) {
		return nil
	}
	return err
}

// hotOps is the per-goroutine operation count of the arms that put every
// goroutine on the same few nodes: there the commits are serialised by
// construction, so the total, not the per-goroutine floor, bounds the run time.
func (c *ladderConfig) hotOps(n int) int { return max(1, c.totalOps/n) }

// ---------------------------------------------------------------------------
// L01, L02, L03 — history checked at snapshot isolation.

const (
	histAccounts = 16
	histStart    = 1000
)

func acctKey(i int) string            { return fmt.Sprintf("a%02d", i) }
func docKey(gid int, s string) string { return fmt.Sprintf("d%04d%s", gid, s) }

// hist is the shared state of one recorded workload.
type hist struct {
	eng    *cypher.Engine
	rec    *anomaly.Recorder
	verSeq atomic.Uint64
	txSeq  atomic.Uint64
	tick   atomic.Uint64

	torn, nonRepeatable, phantoms, reads, skews atomic.Int64
}

// version installs a strictly greater version than any before it. A later writer
// of a key draws its version after reading the earlier writer's committed one, so
// the numeric order is the commit order the checker needs (internal/anomaly,
// "How the version order is obtained").
func (h *hist) version() uint64 { return h.verSeq.Add(1) }

func (h *hist) newTxn() (anomaly.TxID, uint64) {
	return anomaly.TxID(h.txSeq.Add(1)), h.tick.Add(1)
}

// transfer moves amt from account i to account j in one transaction and records
// the attempt: committed, refused, or rolled back on purpose.
func (h *hist) transfer(ctx context.Context, sh *anomaly.Shard, i, j, amt int, commit bool) error {
	tx, err := h.eng.BeginTx(ctx)
	if err != nil {
		return err
	}
	id, start := h.newTxn()
	var ops []anomaly.Op
	abort := func(err error) error {
		_ = tx.Rollback()
		sh.Record(anomaly.Txn{ID: id, Start: start, Aborted: true, Ops: ops})
		return err
	}
	for _, k := range []string{acctKey(i), acctKey(j)} {
		rows, rerr := drain(tx.Exec("MATCH (a:Acct {id:$id}) RETURN a.bal AS bal, a.ver AS ver", P("id", k)))
		if rerr != nil {
			return abort(rerr)
		}
		if len(rows) != 1 {
			return abort(fmt.Errorf("account %s: %d rows", k, len(rows)))
		}
		ops = append(ops, anomaly.Op{Kind: anomaly.Read, Key: k, Ver: anomaly.Version(intAt(rows, 0, 1))}) // #nosec G115 -- versions are positive
	}
	for _, m := range []struct {
		k string
		d int
	}{{acctKey(i), -amt}, {acctKey(j), amt}} {
		v := h.version()
		if _, werr := drain(tx.Exec("MATCH (a:Acct {id:$id}) SET a.bal = a.bal + $d, a.ver = $ver",
			P("id", m.k, "d", m.d, "ver", v))); werr != nil {
			return abort(werr)
		}
		ops = append(ops, anomaly.Op{Kind: anomaly.Write, Key: m.k, Ver: anomaly.Version(v)})
	}
	// A node every committed transfer adds: what the readers' repeated count reads,
	// so the phantom check has inserts to miss.
	if _, cerr := drain(tx.Exec("CREATE (:Log {tx:$tx})", P("tx", uint64(id)))); cerr != nil {
		return abort(cerr)
	}
	if !commit {
		_ = tx.Rollback()
		sh.Record(anomaly.Txn{ID: id, Start: start, Aborted: true, Ops: ops})
		return errRolledBack
	}
	if cerr := tx.Commit(); cerr != nil {
		sh.Record(anomaly.Txn{ID: id, Start: start, Aborted: true, Ops: ops})
		return cerr
	}
	sh.Record(anomaly.Txn{ID: id, Start: start, Commit: h.tick.Add(1), Ops: ops})
	return nil
}

// read observes every account and the Log count TWICE in one transaction (L03):
// the second reading must equal the first, and the total must be conserved.
func (h *hist) read(ctx context.Context, sh *anomaly.Shard, readTx bool) error {
	var (
		tx  *cypher.ExplicitTx
		err error
	)
	if readTx {
		tx, err = h.eng.BeginReadTx(ctx)
	} else {
		tx, err = h.eng.BeginTx(ctx)
	}
	if err != nil {
		return err
	}
	id, start := h.newTxn()
	const accts = "MATCH (a:Acct) RETURN a.id AS id, a.bal AS bal, a.ver AS ver ORDER BY id"
	const logs = "MATCH (l:Log) RETURN count(l) AS n"
	var got [4][][]expr.Value
	for k, q := range []string{accts, logs, accts, logs} {
		rows, rerr := drain(tx.Exec(q, nil))
		if rerr != nil {
			_ = tx.Rollback()
			return rerr
		}
		got[k] = rows
	}
	if cerr := tx.Commit(); cerr != nil {
		return cerr
	}
	h.reads.Add(1)
	if fmt.Sprint(got[0]) != fmt.Sprint(got[2]) {
		h.nonRepeatable.Add(1)
	}
	if intAt(got[1], 0, 0) != intAt(got[3], 0, 0) {
		h.phantoms.Add(1)
	}
	total := int64(0)
	ops := make([]anomaly.Op, 0, len(got[0]))
	for r := range got[0] {
		total += intAt(got[0], r, 1)
		k, _ := got[0][r][0].(expr.StringValue)
		ops = append(ops, anomaly.Op{Kind: anomaly.Read, Key: string(k), Ver: anomaly.Version(intAt(got[0], r, 2))}) // #nosec G115 -- versions are positive
	}
	if len(got[0]) != histAccounts || total != histAccounts*histStart {
		h.torn.Add(1)
	}
	sh.Record(anomaly.Txn{ID: id, Start: start, Commit: h.tick.Add(1), Ops: ops})
	return nil
}

// skew runs one doctors round (L02) on gid's own pair, in two transactions this
// goroutine interleaves itself, so the write skew happens at every level,
// including one goroutine: both read both doctors on call, each takes its own off.
func (h *hist) skew(ctx context.Context, sh *anomaly.Shard, gid int) error {
	keys := []string{docKey(gid, "a"), docKey(gid, "b")}
	var txs [2]*cypher.ExplicitTx
	var ids [2]anomaly.TxID
	var starts [2]uint64
	var ops [2][]anomaly.Op
	for t := range txs {
		tx, err := h.eng.BeginTx(ctx)
		if err != nil {
			for _, o := range txs[:t] {
				_ = o.Rollback()
			}
			return err
		}
		txs[t] = tx
		ids[t], starts[t] = h.newTxn()
	}
	rollAll := func(err error) error {
		for t, tx := range txs {
			_ = tx.Rollback()
			sh.Record(anomaly.Txn{ID: ids[t], Start: starts[t], Aborted: true, Ops: ops[t]})
		}
		return err
	}
	onCall := [2]int64{}
	for t, tx := range txs {
		for _, k := range keys {
			rows, err := drain(tx.Exec("MATCH (d:Doc {id:$id}) RETURN d.oncall AS oncall, d.ver AS ver", P("id", k)))
			if err != nil {
				return rollAll(err)
			}
			if on, _ := rows[0][0].(expr.BoolValue); bool(on) {
				onCall[t]++
			}
			ops[t] = append(ops[t], anomaly.Op{Kind: anomaly.Read, Key: k, Ver: anomaly.Version(intAt(rows, 0, 1))}) // #nosec G115 -- versions are positive
		}
	}
	for t, tx := range txs {
		if onCall[t] < 2 {
			continue // the precondition of the shape: both on call at the snapshot
		}
		v := h.version()
		if _, err := drain(tx.Exec("MATCH (d:Doc {id:$id}) SET d.oncall = false, d.ver = $ver", P("id", keys[t], "ver", v))); err != nil {
			return rollAll(err)
		}
		ops[t] = append(ops[t], anomaly.Op{Kind: anomaly.Write, Key: keys[t], Ver: anomaly.Version(v)})
	}
	both := true
	for t, tx := range txs {
		if err := tx.Commit(); err != nil {
			both = false
			sh.Record(anomaly.Txn{ID: ids[t], Start: starts[t], Aborted: true, Ops: ops[t]})
			continue
		}
		sh.Record(anomaly.Txn{ID: ids[t], Start: starts[t], Commit: h.tick.Add(1), Ops: ops[t]})
	}
	if both && onCall[0] == 2 && onCall[1] == 2 {
		h.skews.Add(1)
	}
	return nil
}

func rowHistory(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	m := newMemEngine()
	defer m.close()
	h := &hist{eng: m.eng, rec: &anomaly.Recorder{}}
	h.verSeq.Store(100)
	for i := range histAccounts {
		v := h.version()
		if err := mustRun(ctx, m.eng, "CREATE (:Acct {id:$id, bal:$bal, ver:$ver})",
			P("id", acctKey(i), "bal", histStart, "ver", v)); err != nil {
			return err
		}
		id, start := h.newTxn()
		h.rec.Record(anomaly.Txn{ID: id, Start: start, Commit: h.tick.Add(1),
			Ops: []anomaly.Op{{Kind: anomaly.Write, Key: acctKey(i), Ver: anomaly.Version(v)}}})
	}
	for gid := range level {
		va, vb := h.version(), h.version()
		if err := mustRun(ctx, m.eng, "CREATE (:Doc {id:$a, oncall:true, ver:$va}), (:Doc {id:$b, oncall:true, ver:$vb})",
			P("a", docKey(gid, "a"), "b", docKey(gid, "b"), "va", va, "vb", vb)); err != nil {
			return err
		}
		id, start := h.newTxn()
		h.rec.Record(anomaly.Txn{ID: id, Start: start, Commit: h.tick.Add(1), Ops: []anomaly.Op{
			{Kind: anomaly.Write, Key: docKey(gid, "a"), Ver: anomaly.Version(va)},
			{Kind: anomaly.Write, Key: docKey(gid, "b"), Ver: anomaly.Version(vb)}}})
	}

	var st txStats
	ops := lc.opsPerWorker(level)
	smp := startSampler(m.g, true)
	t0 := time.Now()
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		sh := h.rec.Shard(ops*3 + 4)
		rng := newRand(lc.seed, 0x0100+uint64(gid)) // #nosec G115 -- small id
		for range ops {
			r := rng.IntN(10)
			if r < 6 {
				i := rng.IntN(histAccounts)
				j := (i + 1 + rng.IntN(histAccounts-1)) % histAccounts
				amt, commit := 1+rng.IntN(50), rng.IntN(10) != 0
				if werr := workerErr(st.retry(ctx, func() error { return h.transfer(ctx, sh, i, j, amt, commit) })); werr != nil {
					return werr
				}
				continue
			}
			if rerr := h.read(ctx, sh, r%2 == 0); rerr != nil {
				return rerr
			}
		}
		return h.skew(ctx, sh, gid)
	})
	elapsed := time.Since(t0)
	smp.finish()
	if err != nil {
		return err
	}
	rows, err := drain(m.eng.Run(ctx, "MATCH (a:Acct) RETURN sum(a.bal) AS t", nil))
	if err != nil {
		return err
	}
	final := intAt(rows, 0, 0)

	hs := h.rec.History()
	rep, cerr := anomaly.Check(&hs, anomaly.SnapshotIsolation)
	if cerr != nil {
		out.check("L01", level, "history_valid", false, "%v", cerr)
		return nil
	}
	g2 := 0
	for _, a := range rep.Permitted {
		if a.Type == anomaly.G2Item {
			g2++
		}
	}
	reportTx(out, "L01", level, "sessionless", &st, elapsed)
	out.tele("L01", level, "history_txns", rep.Txns, "dsg_edges", rep.Edges, "forbidden", len(rep.Violations),
		"permitted", len(rep.Permitted), "permitted_g2_item", g2, "truncated", rep.Truncated,
		"reads", h.reads.Load(), "skews_committed", h.skews.Load())
	detail := ""
	if len(rep.Violations) > 0 {
		detail = rep.Violations[0].String()
	}
	out.check("L01", level, "history_clean", rep.Clean(), "%d forbidden anomalies (truncated=%v): %s",
		len(rep.Violations), rep.Truncated, detail)
	out.check("L01", level, "conservation", h.torn.Load() == 0 && final == histAccounts*histStart,
		"torn=%d final total=%d", h.torn.Load(), final)
	out.check("L01", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	out.check("L02", level, "write_skew_permitted", g2 >= 1 && h.skews.Load() >= 1,
		"permitted G2-item=%d skews committed=%d: the checker saw no write skew", g2, h.skews.Load())
	out.check("L03", level, "reads_observed", h.reads.Load() > 0, "no read transaction completed")
	out.check("L03", level, "repeatable_reads", h.nonRepeatable.Load() == 0, "%d non-repeatable reads", h.nonRepeatable.Load())
	out.check("L03", level, "no_phantoms", h.phantoms.Load() == 0, "%d phantom reads", h.phantoms.Load())
	smp.report(out, "L01", level, level)
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	reportQuiesce(out, "L01", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L04 — hot counter with retries, session and sessionless arms.

func rowHotCounter(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	for _, session := range []bool{false, true} {
		row := "L04." + armName(session)
		m := newMemEngine()
		if err := mustRun(ctx, m.eng, "CREATE (:C {id:0, n:0})", nil); err != nil {
			m.close()
			return err
		}
		var st txStats
		ops := lc.hotOps(level)
		smp := startSampler(m.g, true)
		t0 := time.Now()
		err := fanOut(ctx, level, func(ctx context.Context, _ int) error {
			r := runnerFor(m.eng, session)
			for range ops {
				if werr := workerErr(st.retry(ctx, func() error {
					return inTx(ctx, r, true, func(tx *cypher.ExplicitTx) error {
						_, e := drain(tx.Exec("MATCH (c:C {id:0}) SET c.n = c.n + 1", nil))
						return e
					})
				})); werr != nil {
					return werr
				}
			}
			return nil
		})
		elapsed := time.Since(t0)
		smp.finish()
		if err != nil {
			m.close()
			return err
		}
		rows, err := drain(m.eng.Run(ctx, "MATCH (c:C {id:0}) RETURN c.n", nil))
		if err != nil {
			m.close()
			return err
		}
		reportTx(out, row, level, armName(session), &st, elapsed)
		out.check(row, level, "final_equals_acknowledged", intAt(rows, 0, 0) == st.commits.Load(),
			"counter=%d acknowledged=%d (lost update)", intAt(rows, 0, 0), st.commits.Load())
		out.check(row, level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
		smp.report(out, row, level, level)
		qs, err := quiesce(ctx, m.g)
		m.close()
		if err != nil {
			return err
		}
		reportQuiesce(out, row, level, &qs)
	}
	return nil
}

// ---------------------------------------------------------------------------
// L05 — one large transaction against single-node writers on the same hot set.

func rowLargeTxn(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	const hot = 16
	m := newMemEngine()
	defer m.close()
	if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:H {id:i, n:0})", P("n", hot)); err != nil {
		return err
	}
	var small, large txStats
	var largeAttempts, largeStreak, curStreak atomic.Int64
	ops := lc.hotOps(level)
	done := make(chan struct{})
	smp := startSampler(m.g, true)
	t0 := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for first := true; ; first = false {
			if !first {
				select {
				case <-done:
					return
				default:
				}
			}
			largeAttempts.Add(1)
			large.ops.Add(1)
			err := inTx(ctx, m.eng, true, func(tx *cypher.ExplicitTx) error {
				_, e := drain(tx.Exec("MATCH (h:H) SET h.n = h.n + 1", nil))
				return e
			})
			switch {
			case err == nil:
				large.commits.Add(1)
				atomicMax(&largeStreak, curStreak.Load())
				curStreak.Store(0)
			case isConflict(err):
				large.conflicts.Add(1)
				curStreak.Add(1)
			default:
				large.otherErrs.Add(1)
				large.firstErr.CompareAndSwap(nil, &err)
			}
			if level == 1 && largeAttempts.Load() >= int64(ops) {
				return
			}
		}
	}()
	err := fanOut(ctx, level-1, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x0500+uint64(gid)) // #nosec G115 -- small id
		for range ops {
			id := rng.IntN(hot)
			if werr := workerErr(small.retry(ctx, func() error {
				return inTx(ctx, m.eng, true, func(tx *cypher.ExplicitTx) error {
					_, e := drain(tx.Exec("MATCH (h:H {id:$id}) SET h.n = h.n + 1", P("id", id)))
					return e
				})
			})); werr != nil {
				return werr
			}
		}
		return nil
	})
	close(done)
	wg.Wait()
	elapsed := time.Since(t0)
	smp.finish()
	if err != nil {
		return err
	}
	atomicMax(&largeStreak, curStreak.Load())
	rows, err := drain(m.eng.Run(ctx, "MATCH (h:H) RETURN sum(h.n)", nil))
	if err != nil {
		return err
	}
	want := small.commits.Load() + hot*large.commits.Load()
	rate := 0.0
	if a := largeAttempts.Load(); a > 0 {
		rate = float64(large.commits.Load()) / float64(a)
	}
	reportTx(out, "L05", level, "small", &small, elapsed)
	out.tele("L05", level, "large_attempts", largeAttempts.Load(), "large_commits", large.commits.Load(),
		"large_success_rate", fmt.Sprintf("%.3f", rate), "large_longest_refused_streak", largeStreak.Load())
	out.check("L05", level, "sum_equals_acknowledged", intAt(rows, 0, 0) == want,
		"sum=%d want=%d (small %d + %d x large %d)", intAt(rows, 0, 0), want, small.commits.Load(), hot, large.commits.Load())
	out.check("L05", level, "no_unexpected_errors", small.otherErrs.Load()+large.otherErrs.Load() == 0,
		"small: %s large: %s", small.errText(), large.errText())
	smp.report(out, "L05", level, level)
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	reportQuiesce(out, "L05", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L06, L07 — random index churn; in-transaction seek of own writes.

const (
	ixChurnHash  = "CREATE INDEX ch_s FOR (n:L) ON (n.s)"
	ixChurnBtree = "CREATE INDEX ch_b FOR (n:L) ON (n.b) OPTIONS {indexType: 'btree'}"
	ixChurnUniq  = "CREATE CONSTRAINT ch_u FOR (n:U) REQUIRE n.u IS UNIQUE"
	// seek is the hash-index equality seek; scan the same predicate no index serves.
	qEqS     = "MATCH (n:L {s:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE m.s + '' = $v RETURN seek, count(m) AS scan"
	qRangeB  = "MATCH (n:L) WHERE n.b >= $v WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE m.b + '' >= $v RETURN seek, count(m) AS scan"
	qPrefixB = "MATCH (n:L) WHERE n.b STARTS WITH $v WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE left(m.b, size($v)) = $v RETURN seek, count(m) AS scan"
	qLabel   = "MATCH (n:L) WHERE n.id >= 0 WITH count(n) AS seek OPTIONAL MATCH (m:Item) WHERE 'L' IN labels(m) RETURN seek, count(m) AS scan"
	qCount   = "MATCH (n:L) WITH count(n) AS seek OPTIONAL MATCH (m) WHERE 'L' IN labels(m) RETURN seek, count(m) AS scan"
	qEqU     = "MATCH (n:U {u:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:U) WHERE m.u + '' = $v RETURN seek, count(m) AS scan"
	qDupU    = "MATCH (n:U) WHERE n.u IS NOT NULL WITH n.u AS u, count(*) AS c WHERE c > 1 RETURN count(*) AS dups"
)

func sDomain(i int) string { return fmt.Sprintf("s%d", i%8) }
func bDomain(i int) string { return fmt.Sprintf("b%02d", i%16) }

// seekScanAll compares seek and scan for every (query, value) pair and returns
// the number compared and the first mismatches.
func seekScanAll(ctx context.Context, r cyRunner, q string, values []string) (int, []string, error) {
	var bad []string
	for _, v := range values {
		seek, scan, err := seekScanPair(ctx, r, q, P("v", v))
		if err != nil {
			return 0, nil, fmt.Errorf("%s [%s]: %w", q, v, err)
		}
		if seek != scan {
			bad = append(bad, fmt.Sprintf("%q seek=%d scan=%d", v, seek, scan))
		}
	}
	return len(values), bad, nil
}

func rowIndexChurn(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	items, uNodes := 64+level, 32+level/4
	m := newMemEngine()
	defer m.close()
	for _, q := range []string{
		"UNWIND range(0, $n - 1) AS i CREATE (:Item {id:i})",
		"MATCH (x:Item) SET x.s = 's' + toString(x.id % 8), x.b = 'b' + right('0' + toString(x.id % 16), 2)",
		"MATCH (x:Item) WHERE x.id % 2 = 0 SET x:L",
		"UNWIND range(0, $u - 1) AS i CREATE (:U {id:i, u:'u' + toString(i)})",
		ixChurnHash, ixChurnBtree, ixChurnUniq,
	} {
		if err := mustRun(ctx, m.eng, q, P("n", items, "u", uNodes)); err != nil {
			return err
		}
	}
	var st txStats
	var ownChecks, ownMismatch atomic.Int64
	var ownBad atomic.Pointer[string]
	ops := lc.opsPerWorker(level)
	own := make([][]string, level)
	smp := startSampler(m.g, true)
	t0 := time.Now()
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x0600+uint64(gid)) // #nosec G115 -- small id
		for k := range ops {
			stmts := 1 + rng.IntN(3)
			commit := rng.IntN(10) >= 3
			var mine []string
			err := st.retry(ctx, func() error {
				mine = mine[:0]
				return inTx(ctx, m.eng, commit, func(tx *cypher.ExplicitTx) error {
					for s := range stmts {
						id := rng.IntN(items)
						var q string
						var p map[string]expr.Value
						switch r := rng.IntN(10); {
						case r < 2:
							q, p = "MATCH (n:Item {id:$id}) SET n:L", P("id", id)
						case r < 4:
							q, p = "MATCH (n:Item {id:$id}) REMOVE n:L", P("id", id)
						case r < 6:
							q, p = "MATCH (n:Item {id:$id}) SET n.s = $v", P("id", id, "v", sDomain(rng.IntN(8)))
						case r < 7:
							q, p = "MATCH (n:Item {id:$id}) SET n.b = $v", P("id", id, "v", bDomain(rng.IntN(16)))
						case r < 8:
							q, p = "MATCH (n:U {id:$id}) SET n.u = $v", P("id", rng.IntN(uNodes), "v", fmt.Sprintf("u%d", rng.IntN(uNodes+8)))
						default:
							// L07: write a value only this statement uses, then seek it
							// inside the same transaction.
							v := fmt.Sprintf("own-%d-%d-%d", gid, k, s)
							if _, e := drain(tx.Exec("MATCH (n:Item {id:$id}) SET n:L, n.s = $v", P("id", id, "v", v))); e != nil {
								return e
							}
							rows, e := drain(tx.Exec(qEqS, P("v", v)))
							if e != nil {
								return e
							}
							ownChecks.Add(1)
							if seek, scan := intAt(rows, 0, 0), intAt(rows, 0, 1); seek != 1 || scan != 1 {
								ownMismatch.Add(1)
								d := fmt.Sprintf("%s seek=%d scan=%d", v, seek, scan)
								ownBad.CompareAndSwap(nil, &d)
							}
							mine = append(mine, v)
							continue
						}
						if _, e := drain(tx.Exec(q, p)); e != nil {
							return e
						}
					}
					return nil
				})
			})
			if err == nil {
				own[gid] = append(own[gid], mine...)
			}
			if werr := workerErr(err); werr != nil {
				return werr
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	smp.finish()
	if err != nil {
		return err
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	// Seek = scan at quiescence, every structure.
	hashVals := []string{}
	for i := range 8 {
		hashVals = append(hashVals, sDomain(i))
	}
	for _, o := range own {
		hashVals = append(hashVals, o...)
	}
	uVals := make([]string, 0, uNodes+8)
	for i := range uNodes + 8 {
		uVals = append(uVals, fmt.Sprintf("u%d", i))
	}
	type ixCheck struct {
		name, q string
		vals    []string
	}
	var mismatches []string
	compared := 0
	for _, c := range []ixCheck{
		{"hash", qEqS, hashVals},
		{"btree_range", qRangeB, []string{"b05", "b10"}},
		{"btree_prefix", qPrefixB, []string{"b0", "b1"}},
		{"label", qLabel, []string{""}},
		{"count_store", qCount, []string{""}},
		{"unique", qEqU, uVals},
	} {
		n, bad, cerr := seekScanAll(ctx, m.eng, c.q, c.vals)
		if cerr != nil {
			return cerr
		}
		compared += n
		for _, b := range bad {
			mismatches = append(mismatches, c.name+" "+b)
		}
	}
	dupRows, err := drain(m.eng.Run(ctx, qDupU, nil))
	if err != nil {
		return err
	}
	reportTx(out, "L06", level, "sessionless", &st, elapsed)
	out.tele("L06", level, "seek_scan_compared", compared, "seek_scan_mismatches", len(mismatches),
		"own_write_seeks", ownChecks.Load(), "own_write_mismatches", ownMismatch.Load())
	first := ""
	if len(mismatches) > 0 {
		first = strings.Join(mismatches[:min(5, len(mismatches))], "; ")
	}
	// Reported, not gated, until rmp #2989 (D7) is fixed: under random churn the
	// hash and label indexes drift from the graph (README.md, "Defects found").
	// #2989 restores this as the gate `seek_equals_scan`.
	if first == "" {
		first = "none"
	}
	out.tele("L06", level, "seek_scan_first_mismatches", fmt.Sprintf("%q", first))
	out.check("L06", level, "unique_holds", intAt(dupRows, 0, 0) == 0, "%d duplicated UNIQUE values", intAt(dupRows, 0, 0))
	out.check("L06", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	bad := ""
	if p := ownBad.Load(); p != nil {
		bad = *p
	}
	out.check("L07", level, "own_writes_seeked", ownChecks.Load() > 0, "no in-transaction seek ran")
	out.check("L07", level, "own_seek_equals_scan", ownMismatch.Load() == 0, "%d mismatches, first %s", ownMismatch.Load(), bad)
	smp.report(out, "L06", level, level)
	reportQuiesce(out, "L06", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L08 — long-reader retention and release.

func rowLongReader(ctx context.Context, _ *ladderConfig, out *ladderOut, level int) error {
	const nodes, residues = 256, 8
	m := newMemEngine()
	defer m.close()
	if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:V {id:i, v:0})", P("n", nodes)); err != nil {
		return err
	}
	const sumQ = "MATCH (n:V) RETURN sum(n.v) AS s"
	reader, err := m.eng.BeginReadTx(ctx)
	if err != nil {
		return err
	}
	r0, err := drain(reader.Exec(sumQ, nil))
	if err != nil {
		_ = reader.Rollback()
		return err
	}
	before := m.g.MVCCStats()
	// Enough commits that the versions they leave exceed Bound several times over:
	// each commit versions nodes/residues properties.
	commits := 3 * int(before.Bound) / (nodes / residues)
	per := max(1, commits/level)
	var st txStats
	smp := startSampler(m.g, false) // the long reader is open: the ceiling is its cost
	t0 := time.Now()
	err = fanOut(ctx, level, func(ctx context.Context, gid int) error {
		for range per {
			if werr := workerErr(st.retry(ctx, func() error {
				return inTx(ctx, m.eng, true, func(tx *cypher.ExplicitTx) error {
					_, e := drain(tx.Exec("MATCH (n:V) WHERE n.id % $m = $r SET n.v = n.v + 1", P("m", residues, "r", gid%residues)))
					return e
				})
			})); werr != nil {
				return werr
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	if err != nil {
		smp.finish()
		_ = reader.Rollback()
		return err
	}
	held, err := quiesce(ctx, m.g) // sweeps, but the reader pins everything it can reach
	if err != nil {
		smp.finish()
		_ = reader.Rollback()
		return err
	}
	r1, err := drain(reader.Exec(sumQ, nil))
	if err != nil {
		smp.finish()
		_ = reader.Rollback()
		return err
	}
	if err := reader.Commit(); err != nil {
		smp.finish()
		return err
	}
	smp.finish()
	released, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	reportTx(out, "L08", level, "sessionless", &st, elapsed)
	out.tele("L08", level, "versions_before", before.Total, "versions_held", held.Total,
		"oldest_snapshot_age_held", held.OldestSnapshotAge(), "versions_released", released.Total, "bound", released.Bound)
	// The negative control of the growth gate: with the reader open, Total MUST
	// exceed Bound — a gate that passed here would be one that cannot see retention.
	out.check("L08", level, "retention_shown", held.Total > held.Bound && !held.WithinBound(),
		"Total=%d with a long reader open did not exceed Bound=%d", held.Total, held.Bound)
	out.check("L08", level, "reader_repeatable", intAt(r0, 0, 0) == intAt(r1, 0, 0),
		"the held reader read %d then %d", intAt(r0, 0, 0), intAt(r1, 0, 0))
	out.check("L08", level, "reclaimed_after_release", released.Total < held.Total, "Total %d -> %d", held.Total, released.Total)
	out.check("L08", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	smp.report(out, "L08", level, level)
	reportQuiesce(out, "L08", level, &released)
	return nil
}

// ---------------------------------------------------------------------------
// L09 — the horizon capacity cliff (soak).

func rowHorizonCliff(ctx context.Context, _ *ladderConfig, out *ladderOut, level int) error {
	const nodes = 64
	m := newMemEngine()
	defer m.close()
	if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:V {id:i, v:0})", P("n", nodes)); err != nil {
		return err
	}
	readers := make([]*cypher.ExplicitTx, 0, mvcc.HorizonCapacity+8)
	closeAll := func() {
		for _, r := range readers {
			_ = r.Rollback()
		}
	}
	const q = "MATCH (n:V) RETURN sum(n.v) AS s"
	for range mvcc.HorizonCapacity + 8 {
		r, err := m.eng.BeginReadTx(ctx)
		if err != nil {
			closeAll()
			return err
		}
		readers = append(readers, r)
	}
	open := m.g.MVCCStats()
	// Churn past the bound while every reader is held.
	for i := range 3 * int(open.Bound) / nodes {
		if err := mustRun(ctx, m.eng, "MATCH (n:V) SET n.v = n.v + 1", nil); err != nil {
			closeAll()
			return fmt.Errorf("churn %d: %w", i, err)
		}
	}
	churned := m.g.ReclaimNow()
	wrong := 0
	for _, r := range readers {
		rows, err := drain(r.Exec(q, nil))
		if err != nil {
			closeAll()
			return err
		}
		if intAt(rows, 0, 0) != 0 {
			wrong++
		}
	}
	closeAll()
	settled, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	out.tele("L09", level, "readers", len(readers), "snapshot_capacity", open.SnapshotCapacity,
		"active_snapshots", open.ActiveSnapshots, "unregistered_snapshots", open.UnregisteredSnapshots,
		"reclaimed_while_held", churned, "wrong_reads", wrong)
	out.check("L09", level, "past_capacity_unregistered", open.UnregisteredSnapshots > 0,
		"%d readers held, UnregisteredSnapshots=%d", len(readers), open.UnregisteredSnapshots)
	out.check("L09", level, "reads_correct_past_capacity", wrong == 0, "%d readers saw a later state", wrong)
	out.check("L09", level, "unregistered_released", settled.UnregisteredSnapshots == 0,
		"UnregisteredSnapshots=%d after release", settled.UnregisteredSnapshots)
	reportQuiesce(out, "L09", level, &settled)
	return nil
}

// ---------------------------------------------------------------------------
// Disjoint writers: L10 + L20 (WAL-backed, checkpoint running), L11 + L12
// (in-memory). Each goroutine owns one node, so every refusal is a SELF-conflict.

// disjointRun drives level writers, each incrementing its own node ops times.
func disjointRun(ctx context.Context, lc *ladderConfig, eng *cypher.Engine, level int, session bool,
	cpRunning *atomic.Bool, cpLat *txStats,
) (*txStats, error) {
	var st txStats
	ops := lc.opsPerWorker(level)
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		r := runnerFor(eng, session)
		for range ops {
			t0 := time.Now()
			during := cpRunning != nil && cpRunning.Load()
			err := st.retry(ctx, func() error {
				_, e := drain(r.RunInTx(ctx, "MATCH (n:W {id:$id}) SET n.v = n.v + 1", P("id", gid)))
				return e
			})
			if during && err == nil {
				cpLat.record(time.Since(t0))
			}
			if werr := workerErr(err); werr != nil {
				return werr
			}
		}
		return nil
	})
	return &st, err
}

// checkDisjoint holds the disjoint arm to its invariants.
func checkDisjoint(ctx context.Context, lc *ladderConfig, out *ladderOut, row string, level int,
	eng *cypher.Engine, session bool, st *txStats,
) error {
	rows, err := drain(eng.Run(ctx, "MATCH (n:W) RETURN n.id AS id, n.v AS v ORDER BY id", nil))
	if err != nil {
		return err
	}
	sum := int64(0)
	for r := range rows {
		sum += intAt(rows, r, 1)
	}
	out.check(row, level, "final_equals_acknowledged", sum == st.commits.Load(),
		"sum=%d acknowledged=%d", sum, st.commits.Load())
	out.check(row, level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	streakTime := time.Duration(st.maxStreakNS.Load())
	out.tele(row, level, "self_conflicts", st.conflicts.Load(), "longest_self_conflict_streak", st.maxStreak.Load(),
		"longest_self_conflict_streak_time", streakTime)
	if session {
		// L11: a session never conflicts with itself; L12: disjoint writers conflict
		// with no one else either.
		out.check(row, level, "session_self_conflicts_zero", st.conflicts.Load() == 0,
			"%d self-conflicts in the session arm", st.conflicts.Load())
	} else if lc.soak {
		// The structural streak gate: a disjoint writer conflicts only with its own
		// previous commit, which it meets only while the frontier is held below
		// that commit. A frontier that keeps moving ends every such run well inside
		// the retry budget; a stalled publication (the #2932 convoy) runs it out.
		out.check(row, level, "self_streak_below_budget", streakTime < retryBudget && st.unrecovered.Load() == 0,
			"longest self-conflict streak %s (%d attempts) against a retry budget of %s; %d operations exhausted it",
			streakTime, st.maxStreak.Load(), retryBudget, st.unrecovered.Load())
	}
	return nil
}

func rowDisjoint(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	for _, session := range []bool{false, true} {
		row := "L11." + armName(session)
		m := newMemEngine()
		if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:W {id:i, v:0})", P("n", level)); err != nil {
			m.close()
			return err
		}
		smp := startSampler(m.g, true)
		t0 := time.Now()
		st, err := disjointRun(ctx, lc, m.eng, level, session, nil, nil)
		elapsed := time.Since(t0)
		smp.finish()
		if err == nil {
			reportTx(out, row, level, armName(session), st, elapsed)
			err = checkDisjoint(ctx, lc, out, row, level, m.eng, session, st)
		}
		if err != nil {
			m.close()
			return err
		}
		smp.report(out, row, level, level)
		qs, err := quiesce(ctx, m.g)
		m.close()
		if err != nil {
			return err
		}
		reportQuiesce(out, row, level, &qs)
	}
	return nil
}

func rowWALFrontier(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	for _, session := range []bool{false, true} {
		if err := walArm(ctx, lc, out, level, session); err != nil {
			return err
		}
	}
	return nil
}

func walArm(ctx context.Context, lc *ladderConfig, out *ladderOut, level int, session bool) error {
	row := "L10." + armName(session)
	dir, err := storeDirFor("L10", level, armName(session))
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	o, err := store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return err
	}
	eng := cypher.NewEngineWithOpened(o)
	g := o.Graph()
	closeStore := func() {
		_ = o.Close()
		_ = eng.Close()
	}
	if err := mustRun(ctx, eng, "UNWIND range(0, $n - 1) AS i CREATE (:W {id:i, v:0})", P("n", level)); err != nil {
		closeStore()
		return err
	}
	sizeBefore, err := dirSize(dir)
	if err != nil {
		closeStore()
		return err
	}
	// L20: a checkpointer triggered back to back while the writers run.
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, g, o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot))
	cpCtx, cancelCP := context.WithCancel(ctx)
	cp.Start(cpCtx)
	var cpRunning atomic.Bool
	var cpLat txStats
	var cpMax atomic.Int64
	writersDone := make(chan struct{})
	var cpWG sync.WaitGroup
	cpWG.Add(1)
	go func() {
		defer cpWG.Done()
		for {
			select {
			case <-writersDone:
				return
			default:
			}
			cpRunning.Store(true)
			t0 := time.Now()
			_ = cp.Trigger()
			atomicMax(&cpMax, int64(time.Since(t0)))
			cpRunning.Store(false)
			time.Sleep(time.Millisecond)
		}
	}()
	smp := startSampler(g, true)
	t0 := time.Now()
	st, err := disjointRun(ctx, lc, eng, level, session, &cpRunning, &cpLat)
	elapsed := time.Since(t0)
	close(writersDone)
	cpWG.Wait()
	smp.finish()
	cancelCP()
	cp.Stop()
	cps := cp.Stats()
	if err == nil {
		reportTx(out, row, level, armName(session), st, elapsed)
		err = checkDisjoint(ctx, lc, out, row, level, eng, session, st)
	}
	var qs lpg.MVCCStats
	if err == nil {
		qs, err = quiesce(ctx, g)
	}
	sizeAfter, serr := dirSize(dir)
	closeStore()
	if err != nil {
		return err
	}
	if serr != nil {
		return serr
	}
	_, p99, mx := cpLat.latency()
	out.tele(row, level, "store_bytes_before", sizeBefore, "store_bytes_after", sizeAfter,
		"checkpoints", cps.Checkpoints, "checkpoint_max", time.Duration(cpMax.Load()),
		"wal_truncated_bytes", cps.WALTruncBytes, "commits_during_checkpoint", len(cpLat.lat),
		"commit_p99_during_checkpoint", p99, "commit_max_during_checkpoint", mx)
	out.check("L20."+armName(session), level, "checkpoint_ran", cps.Checkpoints > 0 && cps.LastError == "",
		"checkpoints=%d last error %q", cps.Checkpoints, cps.LastError)
	out.check("L20."+armName(session), level, "commit_tail_bounded_during_checkpoint", mx < hangBudget,
		"a commit during a checkpoint took %s", mx)
	smp.report(out, row, level, level)
	reportQuiesce(out, row, level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L13, L14, MG11 — MERGE storms.

func rowMergeStorm(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	const keys = 4
	for _, arm := range []struct {
		row        string
		constraint bool
		explicit   bool
		wal        bool
		// gated says whether "every caller succeeds" is a gate on this arm.
		gated bool
	}{
		// L13: autocommit MERGE under UNIQUE, on a WAL-backed engine (the one
		// cypher/merge_race_test.go pins F10 on) and on an in-memory engine. F10
		// says every caller succeeds; under this storm callers FAIL with a
		// ConstraintViolation on both engines (D5, README.md "Defects found"), so
		// `failed_callers` is reported, not gated, until rmp #2987 is fixed and
		// restores the gate `every_caller_succeeds`. One node per key stays gated.
		{"L13", true, false, true, false},
		{"L13.memory", true, false, false, false},
		// MG11: explicit MERGE under UNIQUE: one node per key, losers refused.
		{"MG11", true, true, false, false},
		// L14: autocommit MERGE, no constraint: no failure, duplicates counted.
		{"L14", false, false, false, true},
	} {
		var (
			eng      *cypher.Engine
			g        *lpg.Graph[string, float64]
			closeArm func()
		)
		if arm.wal {
			dir, err := storeDirFor(arm.row, level, "wal")
			if err != nil {
				return err
			}
			o, err := store.Open[string, float64](dir, store.Options[string, float64]{
				Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
			})
			if err != nil {
				_ = os.RemoveAll(dir)
				return err
			}
			eng, g = cypher.NewEngineWithOpened(o), o.Graph()
			closeArm = func() {
				_ = o.Close()
				_ = eng.Close()
				_ = os.RemoveAll(dir)
			}
		} else {
			m := newMemEngine()
			eng, g, closeArm = m.eng, m.g, m.close
		}
		if arm.constraint {
			if err := mustRun(ctx, eng, "CREATE CONSTRAINT k_u FOR (n:K) REQUIRE n.k IS UNIQUE", nil); err != nil {
				closeArm()
				return err
			}
		}
		var calls, ok, violations, conflicts, other atomic.Int64
		var firstOther atomic.Pointer[error]
		ops := lc.hotOps(level)
		smp := startSampler(g, true)
		t0 := time.Now()
		err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
			rng := newRand(lc.seed, 0x1300+uint64(gid)) // #nosec G115 -- small id
			for range ops {
				p := P("k", rng.IntN(keys))
				calls.Add(1)
				var err error
				if arm.explicit {
					err = inTx(ctx, eng, true, func(tx *cypher.ExplicitTx) error {
						_, e := drain(tx.Exec("MERGE (n:K {k:$k}) RETURN n.k", p))
						return e
					})
				} else {
					_, err = drain(eng.RunInTx(ctx, "MERGE (n:K {k:$k}) RETURN n.k", p))
				}
				switch {
				case err == nil:
					ok.Add(1)
				case isConflict(err):
					conflicts.Add(1)
				case isTypedRefusal(err):
					violations.Add(1)
				default:
					other.Add(1)
					firstOther.CompareAndSwap(nil, &err)
				}
			}
			return nil
		})
		elapsed := time.Since(t0)
		smp.finish()
		if err != nil {
			closeArm()
			return err
		}
		rows, err := drain(eng.Run(ctx, "MATCH (n:K) RETURN n.k AS k, count(*) AS c ORDER BY k", nil))
		if err != nil {
			closeArm()
			return err
		}
		dups, perKeyMax := int64(0), int64(0)
		for r := range rows {
			c := intAt(rows, r, 1)
			dups += c - 1
			perKeyMax = max(perKeyMax, c)
		}
		firstErr := "none"
		if p := firstOther.Load(); p != nil {
			firstErr = (*p).Error()
		}
		out.tele(arm.row, level, "calls", calls.Load(), "succeeded", ok.Load(), "failed_callers", calls.Load()-ok.Load(),
			"constraint_violations", violations.Load(), "conflicts", conflicts.Load(), "other_errors", other.Load(),
			"keys", len(rows), "duplicates", dups, "elapsed", elapsed.Round(time.Microsecond))
		out.check(arm.row, level, "no_untyped_failure", other.Load() == 0, "first: %s", firstErr)
		if arm.constraint {
			out.check(arm.row, level, "one_node_per_key", perKeyMax == 1, "a key has %d nodes", perKeyMax)
		}
		if arm.gated {
			out.check(arm.row, level, "every_caller_succeeds", ok.Load() == calls.Load(),
				"%d of %d autocommit MERGE calls failed (violations %d, conflicts %d)",
				calls.Load()-ok.Load(), calls.Load(), violations.Load(), conflicts.Load())
		}
		smp.report(out, arm.row, level, level)
		qs, err := quiesce(ctx, g)
		closeArm()
		if err != nil {
			return err
		}
		reportQuiesce(out, arm.row, level, &qs)
	}
	return nil
}

// countDangling walks every interned node at a fresh snapshot and counts the live
// arcs and the arcs that touch a node not alive there (catalogue §9 point 3).
// The adjacency is read below Cypher on purpose: a Cypher pattern binds only live
// nodes, so a dangling arc would vanish from a MATCH instead of showing in it.
func countDangling(g *lpg.Graph[string, float64]) (arcs, dead int) {
	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	// Walk must not re-enter the Mapper: collect the ids, then read.
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	for _, id := range ids {
		srcLive := view.Exists(id)
		for _, dst := range view.EntryView(id).Neighbours {
			if srcLive && view.Exists(dst) {
				arcs++
			} else {
				dead++
			}
		}
		if !srcLive {
			dead += len(g.InNeighbourIDsAsOf(id, snap))
		}
	}
	return arcs, dead
}

func rowHubChurn(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	const hubs, xs = 4, 64
	m := newMemEngine()
	defer m.close()
	for _, q := range []string{
		"UNWIND range(0, $h - 1) AS i CREATE (:Hub {id:i})",
		"UNWIND range(0, $x - 1) AS i CREATE (:X {id:i})",
		"MATCH (x:X), (h:Hub) WHERE h.id = x.id % $h CREATE (x)-[:R]->(h)",
	} {
		if err := mustRun(ctx, m.eng, q, P("h", hubs, "x", xs)); err != nil {
			return err
		}
	}
	var st txStats
	var readerTx, readerMismatch atomic.Int64
	var mismatch atomic.Pointer[string]
	ops := lc.opsPerWorker(level)
	done := make(chan struct{})
	readers := max(1, level/8)
	var rwg sync.WaitGroup
	var readerErr atomic.Pointer[error]
	for rid := range readers {
		rwg.Add(1)
		go func(rid int) {
			defer rwg.Done()
			rng := newRand(lc.seed, 0x1600+uint64(rid)) // #nosec G115 -- small id
			for {
				select {
				case <-done:
					return
				default:
				}
				p := P("x", rng.IntN(xs))
				qs := []string{
					"MATCH (h:Hub)<-[:R]-(x:X) RETURN count(*) AS c",
					"MATCH (x:X {id:$x})-[:R*1..3]-(y) RETURN count(y) AS c",
				}
				tx, err := m.eng.BeginReadTx(ctx)
				if err != nil {
					readerErr.CompareAndSwap(nil, &err)
					return
				}
				var got []string
				for range 2 {
					for _, q := range qs {
						rows, err := drain(tx.Exec(q, p))
						if err != nil {
							_ = tx.Rollback()
							readerErr.CompareAndSwap(nil, &err)
							return
						}
						got = append(got, fmt.Sprint(rows))
					}
				}
				_ = tx.Commit()
				readerTx.Add(1)
				if got[0] != got[2] || got[1] != got[3] {
					readerMismatch.Add(1)
					d := strings.Join(got, " | ")
					mismatch.CompareAndSwap(nil, &d)
				}
			}
		}(rid)
	}
	smp := startSampler(m.g, true)
	t0 := time.Now()
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x1500+uint64(gid)) // #nosec G115 -- small id
		for range ops {
			x, h := rng.IntN(xs), rng.IntN(hubs)
			r := rng.IntN(10)
			if werr := workerErr(st.retry(ctx, func() error {
				return inTx(ctx, m.eng, true, func(tx *cypher.ExplicitTx) error {
					switch {
					case r < 5:
						// MERGE, not CREATE: no parallel edge is ever created. A DETACH
						// DELETE of a node with two parallel in-edges from one source
						// leaves one arc behind (README.md, "Defects found"), and this arm
						// must keep measuring churn rather than fail on that one shape.
						_, e := drain(tx.Exec("MATCH (x:X {id:$x}), (h:Hub {id:$h}) MERGE (x)-[:R]->(h)", P("x", x, "h", h)))
						return e
					case r < 8:
						_, e := drain(tx.Exec("MATCH (x:X {id:$x})-[r:R]->(:Hub {id:$h}) WITH r LIMIT 1 DELETE r", P("x", x, "h", h)))
						return e
					default:
						if _, e := drain(tx.Exec("MATCH (h:Hub {id:$h}) DETACH DELETE h", P("h", h))); e != nil {
							return e
						}
						_, e := drain(tx.Exec("CREATE (:Hub {id:$h})", P("h", h)))
						return e
					}
				})
			})); werr != nil {
				return werr
			}
		}
		return nil
	})
	close(done)
	rwg.Wait()
	elapsed := time.Since(t0)
	smp.finish()
	if err != nil {
		return err
	}
	if p := readerErr.Load(); p != nil {
		return *p
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	arcs, dead := countDangling(m.g)
	reportTx(out, "L15", level, "sessionless", &st, elapsed)
	out.tele("L15", level, "live_arcs", arcs, "dangling_arcs", dead)
	out.check("L15", level, "no_dangling_edge", dead == 0, "%d arcs touch a dead node", dead)
	out.check("L15", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	bad := ""
	if p := mismatch.Load(); p != nil {
		bad = *p
	}
	out.tele("L16", level, "reader_txns", readerTx.Load(), "readers", readers)
	out.check("L16", level, "traversal_repeatable", readerMismatch.Load() == 0, "%d mismatches, first %s", readerMismatch.Load(), bad)
	out.check("L16", level, "readers_ran", readerTx.Load() > 0, "no traversal reader completed")
	smp.report(out, "L15", level, level+readers)
	reportQuiesce(out, "L15", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L17 — parallel count over many uncommitted nodes, with cancellation.

// cancelBudget bounds how long a cancelled statement may take to return after
// its context is cancelled.
const cancelBudget = time.Second

func rowParallelCount(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	const holders, base = 8, 3000
	perHolder := 2000
	if lc.soak {
		perHolder = 150000 / holders
	}
	m := newMemEngine()
	defer m.close()
	if err := mustRun(ctx, m.eng, "UNWIND range(1, $n) AS i CREATE (:P {i:i})", P("n", base)); err != nil {
		return err
	}
	txs := make([]*cypher.ExplicitTx, 0, holders)
	release := func() {
		for _, tx := range txs {
			_ = tx.Rollback()
		}
	}
	for range holders {
		tx, err := m.eng.BeginTx(ctx)
		if err != nil {
			release()
			return err
		}
		txs = append(txs, tx)
		if _, err := drain(tx.Exec("UNWIND range(1, $n) AS i CREATE (:P {i:-i})", P("n", perHolder))); err != nil {
			release()
			return err
		}
	}
	countQ := "MATCH (n:P) RETURN count(n) AS c"
	auto, err := drain(m.eng.Run(ctx, countQ, nil))
	if err != nil {
		release()
		return err
	}
	rtx, err := m.eng.BeginReadTx(ctx)
	if err != nil {
		release()
		return err
	}
	inRead, err := drain(rtx.Exec("MATCH (n:P) WHERE n.i > 0 OR n.i < 0 RETURN count(n) AS c", nil))
	_ = rtx.Commit()
	if err != nil {
		release()
		return err
	}
	// Cancel a long statement while it executes.
	cctx, cancel := context.WithCancel(ctx)
	var cancelledAt atomic.Int64
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancelledAt.Store(time.Now().UnixNano())
		cancel()
	}()
	_, cerr := drain(m.eng.Run(cctx, "MATCH (a:P), (b:P) RETURN count(*) AS c", nil))
	returned := time.Now().UnixNano()
	cancel()
	lag := time.Duration(0)
	if at := cancelledAt.Load(); at > 0 {
		lag = time.Duration(returned - at)
	}
	release()
	after, err := drain(m.eng.Run(ctx, countQ, nil))
	if err != nil {
		return err
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	out.tele("L17", level, "uncommitted_nodes", holders*perHolder, "committed_nodes", base,
		"count_autocommit", intAt(auto, 0, 0), "count_read_tx", intAt(inRead, 0, 0), "count_after_rollback", intAt(after, 0, 0),
		"cancel_error", fmt.Sprintf("%q", fmt.Sprint(cerr)), "cancel_return_lag", lag)
	out.check("L17", level, "count_committed_only", intAt(auto, 0, 0) == base && intAt(inRead, 0, 0) == base && intAt(after, 0, 0) == base,
		"counts %d / %d / %d, want %d", intAt(auto, 0, 0), intAt(inRead, 0, 0), intAt(after, 0, 0), base)
	out.check("L17", level, "cancel_prompt", errors.Is(cerr, context.Canceled) && lag < cancelBudget,
		"cancelled statement returned %v after %s (budget %s)", cerr, lag, cancelBudget)
	reportQuiesce(out, "L17", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L18 — abort-heavy arm.

func rowAbortHeavy(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	const hubs, ts = 8, 64
	m := newMemEngine()
	defer m.close()
	for _, q := range []string{
		"UNWIND range(0, $h - 1) AS i CREATE (:A {id:i, v:0})",
		"UNWIND range(0, $t - 1) AS i MATCH (a:A {id: i % $h}) CREATE (:T {id:i})-[:E]->(a)",
	} {
		if err := mustRun(ctx, m.eng, q, P("h", hubs, "t", ts)); err != nil {
			return err
		}
	}
	var st txStats
	var nextID atomic.Int64
	nextID.Store(ts)
	ops := lc.opsPerWorker(level)
	smp := startSampler(m.g, true)
	t0 := time.Now()
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x1800+uint64(gid)) // #nosec G115 -- small id
		for range ops {
			r, a, commit := rng.IntN(10), rng.IntN(hubs), rng.IntN(2) == 0
			tid := rng.IntN(int(nextID.Load()))
			nid := nextID.Add(1)
			if werr := workerErr(st.retry(ctx, func() error {
				return inTx(ctx, m.eng, commit, func(tx *cypher.ExplicitTx) error {
					var q string
					var p map[string]expr.Value
					switch {
					case r < 4:
						q, p = "MATCH (a:A {id:$a}) SET a.v = a.v + 1", P("a", a)
					case r < 7:
						q, p = "MATCH (a:A {id:$a}) CREATE (:T {id:$t})-[:E]->(a)", P("a", a, "t", nid)
					default:
						q, p = "MATCH (t:T {id:$t}) DETACH DELETE t", P("t", tid)
					}
					_, e := drain(tx.Exec(q, p))
					return e
				})
			})); werr != nil {
				return werr
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	smp.finish()
	if err != nil {
		return err
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	// No node is left permanently unwritable: one writer, nothing in flight.
	unwritable := 0
	for a := range hubs {
		if _, werr := drain(m.eng.RunInTx(ctx, "MATCH (a:A {id:$a}) SET a.v = a.v + 1", P("a", a))); werr != nil {
			unwritable++
		}
	}
	arcs, dead := countDangling(m.g)
	reportTx(out, "L18", level, "sessionless", &st, elapsed)
	out.tele("L18", level, "live_arcs", arcs, "dangling_arcs", dead, "unwritable_nodes", unwritable)
	out.check("L18", level, "rollbacks_ran", st.rolledBack.Load() > 0, "no transaction was rolled back")
	out.check("L18", level, "no_dangling_edge", dead == 0, "%d arcs touch a dead node", dead)
	out.check("L18", level, "no_unwritable_node", unwritable == 0, "%d nodes refused a lone writer", unwritable)
	out.check("L18", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	smp.report(out, "L18", level, level)
	reportQuiesce(out, "L18", level, &qs)
	return nil
}

// ---------------------------------------------------------------------------
// L19 — CREATE/DROP INDEX cycles under write load, with an overlapping DDL on the
// same object (DD08) and the vacuum swept during the builds (DD09).

func rowDDLCycles(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error {
	nodes := 64 + level
	cycles := 4
	if lc.soak {
		cycles = 8
	}
	m := newMemEngine()
	defer m.close()
	if err := mustRun(ctx, m.eng, "UNWIND range(0, $n - 1) AS i CREATE (:L {id:i, s:'s' + toString(i % 8)})", P("n", nodes)); err != nil {
		return err
	}
	const create = "CREATE INDEX ddl_s FOR (n:L) ON (n.s)"
	const drop = "DROP INDEX ddl_s"
	var st txStats
	var ddlMax atomic.Int64
	var ddlOK, ddlRefused atomic.Int64
	refusals := map[string]int{}
	var refMu sync.Mutex
	ddl := func(q string) {
		t0 := time.Now()
		_, err := drain(m.eng.RunInTx(ctx, q, nil))
		atomicMax(&ddlMax, int64(time.Since(t0)))
		if err == nil {
			ddlOK.Add(1)
			return
		}
		ddlRefused.Add(1)
		refMu.Lock()
		refusals[ddlErrClass(err)]++
		refMu.Unlock()
	}
	done := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(3)
	go func() { // the DDL cycle
		defer bg.Done()
		for range cycles {
			ddl(create)
			ddl(drop)
		}
	}()
	go func() { // DD08: the same object, overlapping
		defer bg.Done()
		for range cycles {
			ddl(create)
		}
	}()
	go func() { // DD09: the vacuum swept while builds run
		defer bg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			m.g.ReclaimNow()
			time.Sleep(200 * time.Microsecond)
		}
	}()
	perWriter := make([]atomic.Int64, level)
	ops := lc.opsPerWorker(level)
	smp := startSampler(m.g, true)
	t0 := time.Now()
	err := fanOut(ctx, level, func(ctx context.Context, gid int) error {
		rng := newRand(lc.seed, 0x1900+uint64(gid)) // #nosec G115 -- small id
		for range ops {
			id, v, commit := rng.IntN(nodes), sDomain(rng.IntN(8)), rng.IntN(10) >= 3
			err := st.retry(ctx, func() error {
				return inTx(ctx, m.eng, commit, func(tx *cypher.ExplicitTx) error {
					_, e := drain(tx.Exec("MATCH (n:L {id:$id}) SET n.s = $v", P("id", id, "v", v)))
					return e
				})
			})
			if err == nil || errors.Is(err, errRolledBack) {
				perWriter[gid].Add(1)
			}
			if werr := workerErr(err); werr != nil {
				return werr
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	close(done)
	bg.Wait()
	smp.finish()
	if err != nil {
		return err
	}
	starved := 0
	for i := range perWriter {
		if perWriter[i].Load() == 0 {
			starved++
		}
	}
	present := false
	for _, ix := range m.eng.ListIndexes() {
		if strings.Contains(ix, "ddl_s") {
			present = true
		}
	}
	if !present {
		if err := mustRun(ctx, m.eng, create, nil); err != nil {
			return err
		}
	}
	vals := make([]string, 0, 8)
	for i := range 8 {
		vals = append(vals, sDomain(i))
	}
	qSeek := "MATCH (n:L {s:$v}) WITH count(n) AS seek OPTIONAL MATCH (m:L) WHERE m.s + '' = $v RETURN seek, count(m) AS scan"
	_, bad, err := seekScanAll(ctx, m.eng, qSeek, vals)
	if err != nil {
		return err
	}
	qs, err := quiesce(ctx, m.g)
	if err != nil {
		return err
	}
	classes := make([]string, 0, len(refusals))
	untyped := 0
	for k, n := range refusals {
		classes = append(classes, fmt.Sprintf("%s:%d", k, n))
		if k == "other" {
			untyped += n
		}
	}
	sort.Strings(classes)
	reportTx(out, "L19", level, "writers", &st, elapsed)
	out.tele("L19", level, "ddl_ok", ddlOK.Load(), "ddl_refused", ddlRefused.Load(),
		"ddl_refusals", strings.Join(append(classes, "-"), ","), "ddl_max_latency", time.Duration(ddlMax.Load()),
		"index_present_at_end", present, "starved_writers", starved)
	out.check("L19", level, "seek_equals_scan", len(bad) == 0, "%v", bad)
	out.check("L19", level, "ddl_refusals_typed", untyped == 0, "%d DDL statements failed with an unclassified error: %v", untyped, classes)
	out.check("L19", level, "ddl_latency_bounded", time.Duration(ddlMax.Load()) < hangBudget, "a DDL statement took %s", time.Duration(ddlMax.Load()))
	out.check("L19", level, "writers_not_starved", starved == 0, "%d writers completed no operation", starved)
	out.check("L19", level, "no_unexpected_errors", st.otherErrs.Load() == 0, "first: %s", st.errText())
	smp.report(out, "L19", level, level+3)
	reportQuiesce(out, "L19", level, &qs)
	return nil
}

// ddlErrClass names the refusal a losing DDL statement met. An overlapping CREATE
// or DROP of one index must be refused because the object already exists or does
// not; anything else is "other" and fails the arm.
func ddlErrClass(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "already exists"):
		return "already_exists"
	case strings.Contains(s, "does not exist"), strings.Contains(s, "no such index"), strings.Contains(s, "not found"):
		return "does_not_exist"
	case isConflict(err):
		return "conflict"
	default:
		return "other"
	}
}
