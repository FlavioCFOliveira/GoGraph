package main

// catalogue_ddhz.go — the DDL-vs-DML (§1.5), abort (§1.6) and horizon,
// read-only-transaction and session (§1.7) rows of the deterministic MVCC
// scenario catalogue (rmp #2933, stage C1).
//
// # Drivers
//
// Every row runs through the Cypher engine. The lpg driver runs the rows whose
// shape exists in the lpg API: a refused write followed by a read and a commit
// (AB03), create-delete-recreate in one transaction (AB06, commit arm), an abort
// followed by a drain (HZ03), a pinned read snapshot (HZ01, HZ02, RO01, RO02). The
// lpg API has no DDL, no constraint registry, no statement that can fail midway
// and no rollback of eager writes, so the DD rows and AB01, AB02, AB04 and AB05
// are Cypher only.
//
// # Go steps that drive the engine directly
//
// A few shapes need the engine outside the session the harness gives a step: a
// sessionless read (SE01), a statement cancelled while it is executing (AB02),
// a client-side retry loop (AB04). They reach it through world.eng, the engine of
// the permutation in force.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// ---------------------------------------------------------------------------
// A statement parked mid-flight.

// heldStatement is a Cypher statement running on its own goroutine and parked
// inside the cat.hold procedure, so a step can act on it while it executes.
type heldStatement struct {
	entered chan struct{}
	release chan struct{}
	done    chan error
	cancel  context.CancelFunc
}

// registerHold registers cat.hold on eng: a procedure that parks until the held
// statement is released or its context ends. It exists so a step can cancel a
// statement at a fixed point of its execution — after every write that precedes
// the CALL — rather than at whatever point a timer fires.
func (w *world) registerHold(eng *cypher.Engine) error {
	return eng.Procs().Register(procs.Signature{
		Namespace: []string{"cat"},
		Name:      "hold",
		Outputs:   []procs.NamedType{{Name: "x", Kind: expr.KindInteger}},
	}, func(ctx context.Context, _ []expr.Value) ([][]expr.Value, error) {
		h := w.held
		if h == nil {
			return nil, errors.New("cat.hold called with no held statement")
		}
		close(h.entered)
		select {
		case <-h.release:
			return [][]expr.Value{{expr.IntegerValue(1)}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

// cancelMidStatement runs query, which must call cat.hold once, as an autocommit
// statement on its own goroutine; once it is parked in cat.hold, cancels its
// context and reports what the statement returned.
func (w *world) cancelMidStatement(name, query string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<cancel mid-statement> " + query, Hook: func(ctx context.Context) error {
		sctx, cancel := context.WithCancel(ctx)
		h := &heldStatement{entered: make(chan struct{}), release: make(chan struct{}),
			done: make(chan error, 1), cancel: cancel}
		w.held = h
		eng := w.eng
		go func() {
			res, err := eng.RunInTx(sctx, query, nil)
			if err == nil {
				for res.Next() {
				}
				err = errors.Join(res.Err(), res.Close())
			}
			h.done <- err
		}()
		select {
		case <-h.entered:
		case err := <-h.done:
			w.held = nil
			cancel()
			return fmt.Errorf("the statement ended before it reached cat.hold: %w", err)
		case <-ctx.Done():
			w.releaseHeld()
			return ctx.Err()
		}
		cancel()
		err := <-h.done
		w.held = nil
		return err
	}}
}

// releaseHeld ends a held statement that a permutation left in flight.
func (w *world) releaseHeld() {
	if h := w.held; h != nil {
		h.cancel()
		<-h.done
		w.held = nil
	}
}

// ---------------------------------------------------------------------------
// Go probes.

// schema lists the engine's indexes and constraints, sorted, so a DDL row pins
// the catalog it leaves behind.
func (w *world) schema(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<schema>", Probe: func(context.Context) ([]string, [][]string, error) {
		var rows [][]string
		idx := w.eng.ListIndexes()
		slices.Sort(idx)
		for _, n := range idx {
			rows = append(rows, []string{"index", n, ""})
		}
		cons := w.eng.Constraints()
		slices.SortFunc(cons, func(a, b cypher.ConstraintDef) int {
			if a.Name != b.Name {
				if a.Name < b.Name {
					return -1
				}
				return 1
			}
			return 0
		})
		for _, c := range cons {
			kind := "NOT NULL"
			if c.Unique {
				kind = "UNIQUE"
			}
			rows = append(rows, []string{"constraint", c.Name, fmt.Sprintf("%s (:%s).%s", kind, c.Label, c.Property)})
		}
		if len(rows) == 0 {
			rows = [][]string{{"none", "", ""}}
		}
		return []string{"kind", "name", "definition"}, rows, nil
	}}
}

// stats reports MVCCStats fields after draining the vacuum, so the reading is the
// settled state at this point of the interleaving and not the vacuum's timing.
func (w *world) stats(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<vacuum drain; MVCCStats>",
		Probe: func(context.Context) ([]string, [][]string, error) {
			w.g.ReclaimNow()
			st := w.g.MVCCStats()
			return []string{"versions", "active_snapshots", "index_removal_backlog", "in_flight_commits"},
				[][]string{{strconv.FormatInt(st.Total, 10), strconv.Itoa(st.ActiveSnapshots),
					strconv.FormatInt(st.IndexRemovalBacklog, 10), strconv.FormatUint(st.InFlightCommits, 10)}}, nil
		}}
}

// snapshots reports how many snapshots are registered with the horizon. No
// drain: the registration count does not depend on the vacuum.
func (w *world) snapshots(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<MVCCStats.ActiveSnapshots>",
		Probe: func(context.Context) ([]string, [][]string, error) {
			return []string{"active_snapshots"}, [][]string{{strconv.Itoa(w.g.MVCCStats().ActiveSnapshots)}}, nil
		}}
}

// sessionless runs query as an autocommit statement through Engine.RunInTx, with
// no cypher.Session: it gets snapshot isolation only (F11).
func (w *world) sessionless(name, query string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<sessionless> " + query,
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			res, err := w.eng.RunInTx(ctx, query, nil)
			if err != nil {
				return nil, nil, err
			}
			cols := res.Columns()
			var rows [][]string
			for res.Next() {
				row := make([]string, len(cols))
				for i := range cols {
					if v := res.ValueAt(i); v != nil {
						row[i] = v.String()
					} else {
						row[i] = "null"
					}
				}
				rows = append(rows, row)
			}
			return cols, rows, errors.Join(res.Err(), res.Close())
		}}
}

// tenWritesLPG is HZ01's lpg-driver writer: ten committed lpg transactions, each
// setting key.v.
func (w *world) tenWritesLPG(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<10 lpg transactions setting " + key + ".v>", Hook: func(ctx context.Context) error {
		for i := int64(1); i <= 10; i++ {
			if err := w.g.ApplyVersionedCtx(ctx, func(tx lpg.WriteTx) error {
				return w.g.Writer(tx).SetNodeProperty(key, "v", lpg.Int64Value(i))
			}); err != nil {
				return err
			}
		}
		return nil
	}}
}

// tenWrites is HZ01's writer: ten committed autocommit SETs of key's v.
func (w *world) tenWrites(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<10 autocommit SETs of " + key + ".v>", Hook: func(ctx context.Context) error {
		for i := 1; i <= 10; i++ {
			res, err := w.eng.RunInTx(ctx, "MATCH (n {name:'"+key+"'}) SET n.v = "+strconv.Itoa(i), nil)
			if err != nil {
				return err
			}
			if err := errors.Join(res.Err(), res.Close()); err != nil {
				return err
			}
		}
		return nil
	}}
}

// ---------------------------------------------------------------------------
// lpg driver additions.

// abort ends the open write transaction as aborted: Abandon marks it, and
// Session.EndVersionedTx then aborts its commit record instead of publishing it.
func (s *lpgSession) abort(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: WriteTx.Abandon + Session.EndVersionedTx", Hook: func(context.Context) error {
		if !s.tx.Valid() {
			return errors.New("lpg session has no open transaction")
		}
		tx := s.tx
		s.tx = lpg.WriteTx{}
		tx.Abandon()
		s.sess.EndVersionedTx(tx)
		return nil
	}}
}

// beginRead pins a read snapshot for the session's later reads.
func (s *lpgSession) beginRead(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: Graph.BeginRead", Hook: func(context.Context) error {
		if s.snap != nil {
			return errors.New("lpg session already holds a read snapshot")
		}
		s.snap = s.w.g.BeginRead()
		return nil
	}}
}

// endRead releases the session's read snapshot.
func (s *lpgSession) endRead(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: Graph.EndRead", Hook: func(context.Context) error {
		if s.snap == nil {
			return errors.New("lpg session holds no read snapshot")
		}
		s.w.g.EndRead(s.snap)
		s.snap = nil
		return nil
	}}
}

// createNode adds node key with v = val inside the open transaction.
func (s *lpgSession) createNode(name, key string, val int64) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: fmt.Sprintf("lpg: AddNode(%s); SetNodeProperty(%s, v, %d)", key, key, val),
		Hook: func(ctx context.Context) error {
			return s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
				if err := wv.AddNode(key); err != nil {
					return err
				}
				return wv.SetNodeProperty(key, "v", lpg.Int64Value(val))
			})
		}}
}

// removeNode removes node key inside the open transaction.
func (s *lpgSession) removeNode(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: RemoveNode(" + key + ")", Hook: func(ctx context.Context) error {
		return s.write(ctx, func(wv lpg.WriteView[string, float64]) error {
			_, err := wv.RemoveNode(key)
			return err
		})
	}}
}

// exists reports whether node key exists at the session's instant, and its v.
func (s *lpgSession) exists(name, key string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "lpg: exists " + key + ", " + key + ".v",
		Probe: func(context.Context) ([]string, [][]string, error) {
			g := s.w.g
			var view *lpg.ReadView[string, float64]
			var snap *lpg.Snapshot
			switch {
			case s.snap != nil:
				view = g.ReadAt(s.snap)
			case s.tx.Valid():
				view = g.WriterViewOf(s.tx)
			default:
				snap = g.BeginRead()
				defer g.EndRead(snap)
				view = g.ReadAt(snap)
			}
			id, interned := g.AdjList().Mapper().Lookup(key)
			alive := interned && !view.IsTombstoned(id)
			v := "null"
			if alive {
				pv, ok := view.GetNodeProperty(key, "v")
				v = renderProperty(pv, ok)
			}
			return []string{"exists", "v"}, [][]string{{strconv.FormatBool(alive), v}}, nil
		}}
}

// ---------------------------------------------------------------------------
// §1.5 DDL vs DML.

// dd01 is the G9 row: a CREATE INDEX whose backfill runs while a transaction
// holds uncommitted writes to the indexed property. end is the transaction's
// COMMIT or ROLLBACK.
func dd01(name, arm, endName string, endCtl isolationtest.Control) func(*world) *isolationtest.Spec {
	end := isolationtest.Step{Name: endName, Ctl: endCtl}
	return func(*world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name: name,
			Doc: "DD01 (G9), " + arm + ". 1 100 :L nodes and no index. s1 opens, moves n7 from\n" +
				"s = 'v7' to s = 'x' and creates c1 with s = 'y'; s2 creates a hash index on (:L).s in\n" +
				"an autocommit statement; s1 then ends. The backfill must read a committed state,\n" +
				"never s1's uncommitted writes, and the commit must reach the index whether it\n" +
				"lands before, during or after the build: seek = scan for 'x', 'y' and 'v7' in every\n" +
				"step and after the run.",
			Setup: steps(q("mk", ixSeed)),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'x' RETURN n.s AS s"),
					q("s1cr", "CREATE (n:L {name:'c1', s:'y'}) RETURN n.s AS s"),
					end)},
				{Name: "s2", Steps: steps(q("s2ix", ixHash), q("s2x", eqS("x")), q("s2v7", eqS("v7")))},
			},
			Final: steps(q("fx", eqS("x")), q("fy", eqS("y")), q("fv7", eqS("v7"))),
		}
	}
}

var (
	dd01Commit   = dd01("dd01-create-index-under-open-writer-commit", "commit arm", "s1c", isolationtest.Commit)
	dd01Rollback = dd01("dd01-create-index-under-open-writer-rollback", "rollback arm", "s1rb", isolationtest.Rollback)
)

func dd02(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd02-two-indexes-writer-between",
		Doc: "DD02. 1 100 :L nodes, no index. s1 creates a hash index on (:L).s and s2 one on\n" +
			"(:L).k, each in an autocommit statement; s3 moves n7 to s = 'zzz' and k = 16 (n8's\n" +
			"key) in an explicit transaction. Both indexes must answer seek = scan after the run,\n" +
			"whichever order the two builds and the commit take.",
		Setup: steps(q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(q("s1ix", ixHash))},
			{Name: "s2", Steps: steps(q("s2ix", "CREATE INDEX l_k FOR (n:L) ON (n.k)"))},
			{Name: "s3", Setup: steps(begin("s3b")), Steps: steps(
				q("s3w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz', n.k = 16 RETURN n.s AS s, n.k AS k"),
				commit("s3c"))},
		},
		Final: steps(q("fzzz", eqS("zzz")), q("fv7", eqS("v7")),
			q("fk16", seekScan("MATCH (n:L {k:16})", "MATCH (m:L) WHERE m.k + 0 = 16")),
			q("fk14", seekScan("MATCH (n:L {k:14})", "MATCH (m:L) WHERE m.k + 0 = 14"))),
	}
}

func dd03(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd03-drop-index-between-seeks",
		Doc: "DD03. Hash index on (:L).s over 1 100 nodes. s1 reads 'v7' through the seek, moves\n" +
			"n7 to 'zzz', and reads 'zzz' and 'v7' again, inside one transaction; s2 drops the\n" +
			"index in an autocommit statement. A read after the drop falls back to a scan with\n" +
			"the same answer, and no read fails: seek = scan in every step.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", eqS("v7")),
				q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz' RETURN n.s AS s"),
				q("s1r2", eqS("zzz")),
				q("s1r3", eqS("v7")),
				commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2drop", "DROP INDEX l_s"))},
		},
		Final: steps(q("fzzz", eqS("zzz")), q("fv7", eqS("v7"))),
	}
}

func dd04(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd04-drop-recreate-index-with-writers",
		Doc: "DD04 (REINDEX analogue). Hash index on (:L).s over 1 100 nodes. s1 drops the index\n" +
			"and creates it again; s2 moves n7 to 'zzz' in an explicit transaction; s3 moves n8\n" +
			"to 'yyy' in an autocommit statement. The rebuilt index must hold both writes: seek\n" +
			"= scan for 'zzz', 'v7', 'yyy' and 'v8' after the run.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(q("s1drop", "DROP INDEX l_s"), q("s1ix", ixHash))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz' RETURN n.s AS s"), commit("s2c"))},
			{Name: "s3", Steps: steps(q("s3w", "MATCH (n:L {name:'n8'}) SET n.s = 'yyy' RETURN n.s AS s"))},
		},
		Final: steps(q("fzzz", eqS("zzz")), q("fv7", eqS("v7")), q("fyyy", eqS("yyy")), q("fv8", eqS("v8")),
			w.schema("fschema")),
	}
}

// kSeed is DD05's population: 1 100 :K nodes, enough for the constraint's backing
// index to be planned as a seek, and one node holding k = 'a'.
const kSeed = "UNWIND range(0, 1099) AS i CREATE (:K {k:'k' + toString(i)}) WITH count(*) AS c CREATE (:K {k:'a', v:'orig'})"

// kSeekScan compares the backing-index seek on k = 'a' with a scan.
var kSeekScan = seekScan("MATCH (n:K {k:'a'})", "MATCH (m:K) WHERE m.k + '' = 'a'")

func dd05(name, arm, endName string, endCtl isolationtest.Control) func(*world) *isolationtest.Spec {
	end := isolationtest.Step{Name: endName, Ctl: endCtl}
	return func(w *world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name: name,
			Doc: "DD05 (G9), " + arm + ". 1 101 :K nodes, one with k = 'a'. s1 opens and creates a\n" +
				"second k = 'a'; s2 creates a UNIQUE constraint on (:K).k in an autocommit statement.\n" +
				"No committed state may violate the constraint: either the DDL is refused (the\n" +
				"duplicate committed first) or s1 is (its statement after the DDL, or its COMMIT\n" +
				"straddling it). The backfill and the value-set must read a committed state, so\n" +
				"s1's uncommitted duplicate never refuses the DDL.",
			Setup: steps(q("mk", kSeed)),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1w", "CREATE (n:K {k:'a', v:'dup'}) RETURN n.k AS k"), end)},
				{Name: "s2", Steps: steps(q("s2ddl", uniqueK))},
			},
			Final: steps(q("fa", "MATCH (n:K {k:'a'}) RETURN n.v AS v ORDER BY v"), q("fseek", kSeekScan),
				w.schema("fschema")),
		}
	}
}

var (
	dd05Commit   = dd05("dd05-unique-under-open-duplicate-commit", "commit arm", "s1c", isolationtest.Commit)
	dd05Rollback = dd05("dd05-unique-under-open-duplicate-rollback", "rollback arm", "s1rb", isolationtest.Rollback)
)

func dd06(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd06-dml-not-blocked-by-idle-tx",
		Doc: "DD06, DML half. s1 opens and writes a; it then sits idle with the transaction open.\n" +
			"s2 writes b in an autocommit statement and s3 writes c in an explicit transaction.\n" +
			"Neither waits on the idle transaction (F7): DML takes no lock between statements.\n" +
			"The DDL half is dd06-ddl-bounded-by-context.",
		Setup: steps(q("mk", "CREATE (:Item {name:'a', v:0}), (:Item {name:'b', v:0}), (:Item {name:'c', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'a'}) SET n.v = 1 RETURN n.v AS v"), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2w", "MATCH (n:Item {name:'b'}) SET n.v = 2 RETURN n.v AS v"))},
			{Name: "s3", Setup: steps(begin("s3b")), Steps: steps(
				q("s3w", "MATCH (n:Item {name:'c'}) SET n.v = 3 RETURN n.v AS v"), commit("s3c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

// dd06DDLs is every DDL kind of DD06's DDL half: both index kinds created and
// dropped, both constraint kinds created and dropped. The DROP statements remove
// the objects dd06DDL's setup creates.
var dd06DDLs = []string{
	"CREATE INDEX l_s2 FOR (n:L) ON (n.s)",
	"CREATE INDEX l_s3 FOR (n:L) ON (n.s) OPTIONS {indexType:'btree'}",
	"DROP INDEX pre_hash",
	"DROP INDEX pre_btree",
	"CREATE CONSTRAINT c_u FOR (n:L) REQUIRE n.s IS UNIQUE",
	"CREATE CONSTRAINT c_nn FOR (n:L) REQUIRE n.s IS NOT NULL",
	"DROP CONSTRAINT pre_u",
	"DROP CONSTRAINT pre_nn",
}

// dd06Deadline is the context deadline each DDL of DD06's DDL half carries. It is
// the input under test, not a latency bound: the step asserts only that the DDL
// returned its context error while the write was still parked.
const dd06Deadline = 50 * time.Millisecond

// holdStatement runs query, which must call cat.hold once, as an autocommit
// statement on its own goroutine, and returns once it is parked in cat.hold. The
// statement stays in flight until a releaseStatement step releases it.
func (w *world) holdStatement(name, query string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<park in cat.hold> " + query, Hook: func(ctx context.Context) error {
		sctx, cancel := context.WithCancel(ctx)
		h := &heldStatement{entered: make(chan struct{}), release: make(chan struct{}),
			done: make(chan error, 1), cancel: cancel}
		w.held = h
		eng := w.eng
		go func() {
			res, err := eng.RunInTx(sctx, query, nil)
			if err == nil {
				for res.Next() {
				}
				err = errors.Join(res.Err(), res.Close())
			}
			h.done <- err
		}()
		select {
		case <-h.entered:
			return nil
		case err := <-h.done:
			w.held = nil
			cancel()
			return fmt.Errorf("the statement ended before it reached cat.hold: %w", err)
		case <-ctx.Done():
			w.releaseHeld()
			return ctx.Err()
		}
	}}
}

// releaseStatement lets the statement a holdStatement step parked return from
// cat.hold, and reports what the statement returned.
func (w *world) releaseStatement(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<release cat.hold>", Hook: func(context.Context) error {
		h := w.held
		if h == nil {
			return errors.New("no statement is parked in cat.hold")
		}
		close(h.release)
		err := <-h.done
		h.cancel()
		w.held = nil
		return err
	}}
}

// ddlUnderDeadline runs every DDL of dd06DDLs with a dd06Deadline context while a
// holdStatement step's write is parked, one row per DDL: the outcome, and whether
// the write was still parked when the DDL returned.
func (w *world) ddlUnderDeadline(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<each DDL with a 50 ms deadline>", Probe: func(ctx context.Context) ([]string, [][]string, error) {
		h := w.held
		if h == nil {
			return nil, nil, errors.New("no statement is parked in cat.hold")
		}
		rows := make([][]string, 0, len(dd06DDLs))
		for _, ddl := range dd06DDLs {
			dctx, cancel := context.WithTimeout(ctx, dd06Deadline)
			res, err := w.eng.RunInTx(dctx, ddl, nil)
			if err == nil {
				for res.Next() {
				}
				err = errors.Join(res.Err(), res.Close())
			}
			cancel()
			outcome := "applied"
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				outcome = "context deadline exceeded"
			case err != nil:
				outcome = err.Error()
			}
			writer := "parked"
			if len(h.done) != 0 {
				writer = "ended"
			}
			rows = append(rows, []string{ddl, outcome, writer})
		}
		return []string{"ddl", "outcome", "writer"}, rows, nil
	}}
}

func dd06DDL(w *world) *isolationtest.Spec {
	retry := make([]isolationtest.Step, 0, len(dd06DDLs))
	for i, ddl := range dd06DDLs {
		retry = append(retry, q("s1retry"+strconv.Itoa(i+1), ddl))
	}
	return &isolationtest.Spec{
		Name: "dd06-ddl-bounded-by-context",
		Doc: "DD06, DDL half (rmp #2982). An autocommit write is parked inside cat.hold, holding\n" +
			"the schema gate shared. Each DDL kind — CREATE and DROP INDEX for the hash and btree\n" +
			"kinds, CREATE and DROP CONSTRAINT for UNIQUE and NOT NULL — runs with a 50 ms\n" +
			"deadline and returns its context error while the write is still parked. The schema\n" +
			"is unchanged before and after the write is released, the write commits, and a\n" +
			"retry of every DDL succeeds.",
		Setup: steps(
			q("mk", "CREATE (:L {s:'x'}), (:P {a:1, b:2}), (:Q {u:'u1', n:'n1'})"),
			q("mkh", "CREATE INDEX pre_hash FOR (n:P) ON (n.a)"),
			q("mkb", "CREATE INDEX pre_btree FOR (n:P) ON (n.b) OPTIONS {indexType:'btree'}"),
			q("mku", "CREATE CONSTRAINT pre_u FOR (n:Q) REQUIRE n.u IS UNIQUE"),
			q("mknn", "CREATE CONSTRAINT pre_nn FOR (n:Q) REQUIRE n.n IS NOT NULL")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(append([]isolationtest.Step{
				w.holdStatement("s1hold", "CREATE (n:T {name:'t'}) WITH n CALL cat.hold() YIELD x RETURN x"),
				w.ddlUnderDeadline("s1ddl"),
				w.schema("s1parked"),
				w.releaseStatement("s1release"),
				w.schema("s1after"),
			}, retry...)...)},
		},
		Final: steps(q("final", "MATCH (n:T) RETURN n.name AS name"), w.schema("fschema")),
	}
}

func dd07InWriteTx(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd07-ddl-in-write-tx",
		Doc: "DD07, write transactions. s1 creates t1, then issues CREATE INDEX inside its\n" +
			"explicit transaction, then COMMITs; s2 creates t2, issues CREATE CONSTRAINT, then\n" +
			"rolls back. DDL inside an explicit transaction is rejected (F9) and creates nothing.\n" +
			"This transcript pins whether the rejection poisons the transaction. MySQL commits\n" +
			"the transaction implicitly instead.",
		Setup: steps(q("mk", "CREATE (:L {name:'a', s:'x'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "CREATE (n:L {name:'t1'}) RETURN n.name AS name"), q("s1ddl", ixHash), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "CREATE (n:L {name:'t2'}) RETURN n.name AS name"), q("s2ddl", uniqueK), rollback("s2rb"))},
		},
		Final: steps(q("final", "MATCH (n:L) RETURN n.name AS name ORDER BY name"), w.schema("fschema")),
	}
}

func dd07InReadTx(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd07-ddl-in-read-tx",
		Doc: "DD07, read-only transaction. s1 issues CREATE INDEX inside BEGIN READ, then reads\n" +
			"and commits; s2 creates another index in an autocommit statement. The DDL in the\n" +
			"read-only transaction is ErrWriteInReadOnlyTx (F9) and creates nothing; the read\n" +
			"that follows still answers.",
		Setup: steps(q("mk", "CREATE (:L {name:'a', s:'x'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(
				q("s1ddl", ixHash), q("s1r", "MATCH (n:L) RETURN count(n) AS c"), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2ix", "CREATE INDEX l_name FOR (n:L) ON (n.name)"))},
		},
		Final: steps(w.schema("fschema")),
	}
}

func dd08(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd08-ddl-on-one-object",
		Doc: "DD08. Four autocommit DDL statements on two objects: s1 creates index l_s, s2 drops\n" +
			"it, s3 creates UNIQUE constraint c1 on (:K).k, s4 creates a constraint with the same\n" +
			"name on (:K).v. Every order leaves a consistent catalog, and a DDL that cannot apply\n" +
			"fails with an error that names why. The harness runs one step at a time, so the\n" +
			"statements are serialised by the script; their overlap is #2934's.",
		Setup: steps(q("mk", "CREATE (:L {name:'a', s:'x'}), (:K {k:'k1', v:'v1'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(q("s1ix", ixHash))},
			{Name: "s2", Steps: steps(q("s2drop", "DROP INDEX l_s"))},
			{Name: "s3", Steps: steps(q("s3c", "CREATE CONSTRAINT c1 FOR (n:K) REQUIRE n.k IS UNIQUE"))},
			{Name: "s4", Steps: steps(q("s4c", "CREATE CONSTRAINT c1 FOR (n:K) REQUIRE n.v IS UNIQUE"))},
		},
		Final: steps(w.schema("fschema")),
	}
}

func dd09(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "dd09-create-index-vs-vacuum",
		Doc: "DD09. 1 100 :L nodes, no index. s1 moves n7 to s = 'zzz' and creates c1 with s = 'zz'\n" +
			"in an explicit transaction, then rolls back; s2 creates a hash index on (:L).s; s3\n" +
			"drains the vacuum, which withdraws s1's aborted versions. In every order the index\n" +
			"loses no node and holds no aborted one: seek = scan after the run. The drain runs\n" +
			"between steps, not inside the backfill: no seam reaches a point inside it.",
		Setup: steps(q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz' WITH n CREATE (c:L {name:'c1', s:'zz'}) RETURN c.s AS s"),
				rollback("s1rb"))},
			{Name: "s2", Steps: steps(q("s2ix", ixHash))},
			{Name: "s3", Steps: steps(w.drain("s3dr"))},
		},
		Final: steps(q("fzzz", eqS("zzz")), q("fzz", eqS("zz")), q("fv7", eqS("v7")), q("fcount", countL)),
	}
}

// ---------------------------------------------------------------------------
// §1.6 Abort and rollback.

func ab01(name, arm, endName string, endCtl isolationtest.Control) func(*world) *isolationtest.Spec {
	end := isolationtest.Step{Name: endName, Ctl: endCtl}
	return func(w *world) *isolationtest.Spec {
		return adjacentOnly(&isolationtest.Spec{
			Name: name,
			Doc: "AB01, " + arm + ". s1 sets x.v = 1, then a statement fails (integer division by\n" +
				"zero), then s1 ends; the vacuum is drained (H1); s1 then retries in a new\n" +
				"transaction (x.v = x.v + 10) and commits. A COMMIT of the failed transaction is\n" +
				"ErrTxPoisoned (F8) and applies nothing; the peer s2 never sees v = 1; the retry\n" +
				"commits, so the final value is 10.",
			Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 1 RETURN n.v AS v"),
					q("s1e", "MATCH (n:Item {name:'x'}) RETURN n.v / 0 AS boom"),
					end, w.drain("s1dr"),
					begin("s1b2"),
					q("s1w2", "MATCH (n:Item {name:'x'}) SET n.v = n.v + 10 RETURN n.v AS v"),
					commit("s1c2"))},
				{Name: "s2", Steps: steps(q("s2r", "MATCH (n:Item {name:'x'}) RETURN n.v AS v"))},
			},
			Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
		}, [2]string{end.Name, "s1dr"})
	}
}

var (
	ab01Commit   = ab01("ab01-failed-statement-commit-then-retry", "COMMIT arm", "s1c", isolationtest.Commit)
	ab01Rollback = ab01("ab01-failed-statement-rollback-then-retry", "ROLLBACK arm", "s1rb", isolationtest.Rollback)
)

// abSeed is AB02's population: 1 100 :L nodes with an integer i and s = 'v<i>'.
const abSeed = "UNWIND range(0, 1099) AS i CREATE (:L {name:'n' + toString(i), i:i, s:'v' + toString(i)})"

func ab02(w *world) *isolationtest.Spec {
	vCount := "MATCH (n:L) WHERE left(n.s, 1) = 'v' RETURN count(n) AS untouched"
	return &isolationtest.Spec{
		Name: "ab02-autocommit-statement-fails-midway",
		Doc: "AB02. Hash index on (:L).s over 1 100 nodes. s1 runs two autocommit statements\n" +
			"that write every node and then fail: SET n.s = toString(10 / (n.i - 50)), which\n" +
			"divides by zero at i = 50, and SET n.s = 'gone' followed by a CALL that is\n" +
			"cancelled while it executes. Neither applies anything: every node keeps s = 'v<i>',\n" +
			"and the index holds no value either statement wrote. s2 reads in autocommit\n" +
			"statements between them.",
		Setup: steps(q("ix", ixHash), q("mk", abSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(
				q("s1div", "MATCH (n:L) SET n.s = toString(10 / (n.i - 50)) RETURN count(n) AS c"),
				w.cancelMidStatement("s1cancel", "MATCH (n:L) SET n.s = 'gone' WITH count(n) AS c CALL cat.hold() YIELD x RETURN c, x"))},
			{Name: "s2", Steps: steps(q("s2r", eqS("v7")), q("s2r2", vCount))},
		},
		Final: steps(q("fv7", eqS("v7")), q("fzero", eqS("0")), q("fgone", eqS("gone")), q("funtouched", vCount),
			q("fcount", countL)),
	}
}

func ab03Cypher(*world) *isolationtest.Spec {
	read := "MATCH (n:Item {name:'x'}) RETURN n.v AS v"
	return &isolationtest.Spec{
		Name: "ab03-refused-statement-then-continue",
		Doc: "AB03 (G10). s1 reads x, writes x.v = 3, reads x again and commits; s2 writes x.v = 2\n" +
			"in an autocommit statement. When s2 commits first, s1's write is refused; s1's next\n" +
			"read must still be at its snapshot (v = 0), and its COMMIT applies nothing. This\n" +
			"transcript pins which error the COMMIT returns.",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1r", read), q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 3 RETURN n.v AS v"),
				q("s1r2", read), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2w", "MATCH (n:Item {name:'x'}) SET n.v = 2 RETURN n.v AS v"))},
		},
		Final: steps(q("final", read)),
	}
}

func ab03LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ab03-refused-statement-then-continue-lpg",
		Doc: "AB03 through the lpg API. s1 reads x.v, writes x.v = 3, reads x.v again and ends\n" +
			"its transaction; s2 writes x.v = 2 in its own transaction. A refused write dooms s1:\n" +
			"its read stays at its snapshot and its commit returns the conflict.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.read("s1r", "x.v"), s1.set("s1w", "x", "v", 3), s1.read("s1r2", "x.v"), s1.commit("s1c"))},
			{Name: "s2", Steps: steps(s2.set("s2w", "x", "v", 2))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}
}

// retrier is AB04's client: a transaction that increments x.v, retried whole on a
// retriable refusal.
type retrier struct {
	w        *world
	tx       *cypher.ExplicitTx
	failed   error
	attempts int
}

// retryLimit bounds AB04's retries. A peer still holding x open refuses every
// retry, so an unbounded loop inside one step would never return.
const retryLimit = 3

func (w *world) retrier() *retrier {
	r := &retrier{w: w}
	w.resets = append(w.resets, func() { r.tx, r.failed, r.attempts = nil, nil, 0 })
	w.closers = append(w.closers, func() {
		if r.tx != nil {
			_ = r.tx.Rollback() // the engine is discarded right after; nothing to report
			r.tx = nil
		}
	})
	return r
}

const ab04Inc = "MATCH (n:Item {name:'x'}) SET n.v = n.v + 1"

// attempt begins a transaction and runs the increment, leaving it open.
func (r *retrier) attempt(ctx context.Context) {
	r.attempts++
	tx, err := r.w.eng.BeginTx(ctx)
	if err != nil {
		r.failed = err
		return
	}
	r.tx = tx
	res, err := tx.Exec(ab04Inc, nil)
	if err == nil {
		err = errors.Join(res.Err(), res.Close())
	}
	r.failed = err
}

// begin is the step that opens the first attempt and runs its increment.
func (r *retrier) begin(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<retrier: BEGIN; SET x.v = x.v + 1>", Hook: func(ctx context.Context) error {
		r.attempt(ctx)
		return nil
	}}
}

// finish commits the open attempt and, on a retriable refusal, rolls it back,
// drains the vacuum (H1) and runs the whole transaction again, up to retryLimit
// attempts. It reports the attempts made and whether the increment was
// acknowledged.
func (r *retrier) finish(name string) isolationtest.Step {
	return isolationtest.Step{Name: name, Label: "<retrier: COMMIT, retry on a retriable refusal>",
		Probe: func(ctx context.Context) ([]string, [][]string, error) {
			for {
				err := r.failed
				if err == nil {
					err = r.tx.Commit()
				}
				if err == nil {
					r.tx = nil
					break
				}
				if r.tx != nil {
					_ = r.tx.Rollback() // ErrTxFinished when Commit already finished it; nothing else to undo
					r.tx = nil
				}
				retriable := errors.Is(err, mvcc.ErrSerializationConflict) || errors.Is(err, cypher.ErrTxPoisoned)
				if !retriable {
					return nil, nil, err
				}
				if r.attempts >= retryLimit {
					return []string{"attempts", "acknowledged"},
						[][]string{{strconv.Itoa(r.attempts), "false"}}, nil
				}
				r.w.g.ReclaimNow()
				r.attempt(ctx)
			}
			return []string{"attempts", "acknowledged"}, [][]string{{strconv.Itoa(r.attempts), "true"}}, nil
		}}
}

func ab04(w *world) *isolationtest.Spec {
	r1, r2 := w.retrier(), w.retrier()
	return &isolationtest.Spec{
		Name: "ab04-retry-loop-converges",
		Doc: "AB04. WW03's two increments of x.v, each run by a client that retries the whole\n" +
			"transaction on a retriable refusal (a serialization conflict, or the poisoning it\n" +
			"leaves), up to three attempts. The final x.v must equal the number of increments\n" +
			"acknowledged, in every interleaving: a retry never double-applies and an\n" +
			"acknowledged increment is never lost.",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(r1.begin("s1go"), r1.finish("s1fin"))},
			{Name: "s2", Steps: steps(r2.begin("s2go"), r2.finish("s2fin"))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}
}

// acknowledgedEqualsFinal asserts, per permutation, that AB04's final x.v is the
// number of increments the two clients were told committed.
func acknowledgedEqualsFinal() isolationtest.Observer {
	acked := map[string]int{}
	return func(o isolationtest.Observation) error {
		if len(o.Rows) != 1 {
			return nil
		}
		switch o.Step {
		case "s1fin", "s2fin":
			if o.Rows[0][1] == "true" {
				acked[o.Permutation]++
			}
		case "final":
			if want := strconv.Itoa(acked[o.Permutation]); o.Rows[0][0] != want {
				return fmt.Errorf("final x.v = %s, but %s increments were acknowledged", o.Rows[0][0], want)
			}
		}
		return nil
	}
}

func ab05(w *world) *isolationtest.Spec {
	edges := "MATCH ()-[r:E]->() WITH count(r) AS seek OPTIONAL MATCH ()-[q]->() WHERE type(q) = 'E' RETURN seek, count(q) AS scan"
	return adjacentOnly(&isolationtest.Spec{
		Name: "ab05-rollback-of-created-subgraph",
		Doc: "AB05. UNIQUE on (:K).k. s1 creates 100 (:L)-[:E]->(:M) pairs and k = 'r1' in one\n" +
			"transaction, then rolls back; the vacuum is drained (H1). s2 counts in autocommit\n" +
			"statements and then creates k = 'r1'. After the rollback nothing s1 created is\n" +
			"visible: label counts, the relationship count and the UNIQUE value-set are all\n" +
			"restored, and the fast count equals the scan.",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'k0'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1cr", "UNWIND range(1, 100) AS i CREATE (:L {name:'r' + toString(i)})-[:E]->(:M {name:'m' + toString(i)}) "+
					"WITH count(*) AS c CREATE (k:K {k:'r1'}) RETURN c"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(q("s2r", countL), q("s2u", "CREATE (k:K {k:'r1'}) RETURN k.k AS k"))},
		},
		Final: steps(q("fcount", countL), q("fedges", edges),
			q("fm", "MATCH (n:M) RETURN count(n) AS m"), q("fk", finalK)),
	}, [2]string{"s1rb", "s1dr"})
}

func ab06(name, arm, endName string, endCtl isolationtest.Control) func(*world) *isolationtest.Spec {
	end := isolationtest.Step{Name: endName, Ctl: endCtl}
	return func(*world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name: name,
			Doc: "AB06, " + arm + ". In one transaction s1 creates z (v = 1), deletes it, creates it\n" +
				"again (v = 2) and reads it, then ends; s2 counts z in an autocommit statement. On\n" +
				"COMMIT z is visible once with v = 2; on ROLLBACK it was never visible.",
			Setup: steps(q("mk", "CREATE (:Anchor {name:'a'})")),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1c1", "CREATE (n:L {name:'z', v:1}) RETURN n.v AS v"),
					q("s1d", "MATCH (n:L {name:'z'}) DETACH DELETE n RETURN count(*) AS deleted"),
					q("s1c2", "CREATE (n:L {name:'z', v:2}) RETURN n.v AS v"),
					q("s1r", "MATCH (n:L {name:'z'}) RETURN n.v AS v"),
					end)},
				{Name: "s2", Steps: steps(q("s2r", "MATCH (n:L {name:'z'}) RETURN count(n) AS c"))},
			},
			Final: steps(q("final", "MATCH (n:L {name:'z'}) RETURN n.v AS v")),
		}
	}
}

var (
	ab06Commit   = ab06("ab06-create-delete-recreate-commit", "COMMIT arm", "s1c", isolationtest.Commit)
	ab06Rollback = ab06("ab06-create-delete-recreate-rollback", "ROLLBACK arm", "s1rb", isolationtest.Rollback)
)

func ab06LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ab06-create-delete-recreate-commit-lpg",
		Doc: "AB06 through the lpg API, commit arm (the lpg API has no rollback of eager\n" +
			"writes). s1 adds z (v = 1), removes it, adds it again (v = 2), reads it and ends its\n" +
			"transaction; s2 reads z at a fresh snapshot. z is visible once with v = 2 after the\n" +
			"commit and absent before it.",
		Setup: steps(w.lpgFixture(lpgNode{key: "a", labels: []string{"Anchor"}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.createNode("s1c1", "z", 1), s1.removeNode("s1d", "z"), s1.createNode("s1c2", "z", 2),
				s1.exists("s1r", "z"), s1.commit("s1c"))},
			{Name: "s2", Steps: steps(s2.exists("s2r", "z"))},
		},
		Final: steps(s2.exists("final", "z")),
	}
}

// ---------------------------------------------------------------------------
// §1.7 Horizon, read-only transactions, sessions and frontier.

func hz01Cypher(w *world) *isolationtest.Spec {
	read := "MATCH (n:Item {name:'x'}) RETURN n.v AS v"
	return &isolationtest.Spec{
		Name: "hz01-long-reader-retains-versions",
		Doc: "HZ01. s1 is a read-only transaction pinned at BEGIN; s2 commits ten SETs of x.v.\n" +
			"Each MVCCStats reading drains the vacuum first, so it is the settled state at that\n" +
			"point. The versions retained rise while s1 holds its snapshot, s1 still reads\n" +
			"v = 0, and after s1 ends a drain reclaims them (F13).",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(beginRead("s1b"), q("s1r", read), q("s1r2", read), commit("s1c"))},
			{Name: "s2", Steps: steps(w.stats("s2st0"), w.tenWrites("s2w", "x"), w.stats("s2st1"), w.stats("s2st2"))},
		},
		Permutations: perms("s2st0 s1b s1r s2w s2st1 s1r2 s1c s2st2"),
		Final:        steps(q("final", read)),
	}
}

func hz01LPG(w *world) *isolationtest.Spec {
	s1 := w.lpgSession()
	return &isolationtest.Spec{
		Name: "hz01-long-reader-retains-versions-lpg",
		Doc: "HZ01 through the lpg API: s1 pins Graph.BeginRead; s2 commits ten SETs of x.v; s1\n" +
			"reads x.v before and after them and releases its snapshot. The drained version\n" +
			"count rises while the snapshot is held and falls after it is released.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(s1.beginRead("s1b"), s1.read("s1r", "x.v"), s1.read("s1r2", "x.v"), s1.endRead("s1c"))},
			{Name: "s2", Steps: steps(w.stats("s2st0"), w.tenWritesLPG("s2w", "x"), w.stats("s2st1"), w.stats("s2st2"))},
		},
		Permutations: perms("s2st0 s1b s1r s2w s2st1 s1r2 s1c s2st2"),
		Final:        steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}
}

// retainedThenReclaimed asserts HZ01's property per permutation: the drained
// version count while the reader holds its snapshot (s2st1) is above the count
// before (s2st0), and the count after it ends (s2st2) is below it.
func retainedThenReclaimed() isolationtest.Observer {
	seen := map[string]map[string]int{}
	return func(o isolationtest.Observation) error {
		if len(o.Rows) != 1 || len(o.Cols) == 0 || o.Cols[0] != "versions" {
			return nil
		}
		n, err := strconv.Atoi(o.Rows[0][0])
		if err != nil {
			return err
		}
		m := seen[o.Permutation]
		if m == nil {
			m = map[string]int{}
			seen[o.Permutation] = m
		}
		m[o.Step] = n
		switch o.Step {
		case "s2st1":
			if n <= m["s2st0"] {
				return fmt.Errorf("versions %d while the reader is held, not above %d before it", n, m["s2st0"])
			}
		case "s2st2":
			if n >= m["s2st1"] {
				return fmt.Errorf("versions %d after the reader ended, not below %d while it was held", n, m["s2st1"])
			}
		}
		return nil
	}
}

func hz02Cypher(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "hz02-autocommit-read-releases-slot",
		Doc: "HZ02. s1 opens a read-only transaction and reads the number of registered\n" +
			"snapshots, then commits and reads it again; s2 runs an autocommit read and then\n" +
			"reads the number. An autocommit read holds no slot after it returns; the open read\n" +
			"transaction holds exactly one.",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(beginRead("s1b"), w.snapshots("s1p"), commit("s1c"), w.snapshots("s1p2"))},
			{Name: "s2", Steps: steps(q("s2r", "MATCH (n:Item) RETURN count(n) AS c"), w.snapshots("s2p"))},
		},
	}
}

func hz02LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "hz02-autocommit-read-releases-slot-lpg",
		Doc: "HZ02 through the lpg API: s1 pins Graph.BeginRead and releases it; s2 reads x.v at\n" +
			"a snapshot taken and released for that one read. Only a held snapshot occupies a\n" +
			"slot.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(s1.beginRead("s1b"), w.snapshots("s1p"), s1.endRead("s1c"), w.snapshots("s1p2"))},
			{Name: "s2", Steps: steps(s2.read("s2r", "x.v"), w.snapshots("s2p"))},
		},
	}
}

func hz03Cypher(w *world) *isolationtest.Spec {
	read := "MATCH (n:Item {name:'x'}) RETURN n.v AS v"
	return adjacentOnly(&isolationtest.Spec{
		Name: "hz03-aborted-versions-withdrawn",
		Doc: "HZ03. s1 sets x.v = 1 and rolls back; the vacuum is drained (H1). s2 reads x and\n" +
			"then writes x.v = 2 in autocommit statements. No reader ever sees v = 1; s2's write\n" +
			"is refused only while s1's version is in flight, and succeeds once it is withdrawn.",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 1 RETURN n.v AS v"), rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(q("s2r", read), q("s2w", "MATCH (n:Item {name:'x'}) SET n.v = 2 RETURN n.v AS v"))},
		},
		Final: steps(q("final", read)),
	}, [2]string{"s1rb", "s1dr"})
}

func hz03LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return adjacentOnly(&isolationtest.Spec{
		Name: "hz03-aborted-versions-withdrawn-lpg",
		Doc: "HZ03 through the lpg API: s1 sets x.v = 1 and aborts (WriteTx.Abandon, then\n" +
			"EndVersionedTx); the vacuum is drained. s2 reads x.v and then sets it to 2, each in\n" +
			"its own transaction.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(
				s1.set("s1w", "x", "v", 1), s1.abort("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(s2.read("s2r", "x.v"), s2.set("s2w", "x", "v", 2))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}, [2]string{"s1rb", "s1dr"})
}

func hz04(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "hz04-vacuum-during-drop-index",
		Doc: "HZ04. Hash index on (:L).s over 1 100 nodes. s0 is a read-only transaction pinned\n" +
			"at BEGIN; s1 removes :L from the ten nodes with k < 20, which leaves label-index\n" +
			"removals waiting for the watermark; s2 drops the index; s3 drains the vacuum and\n" +
			"reads the backlog. Nothing fails, and once s0 has ended the backlog drains to 0.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(beginRead("s0b")), Steps: steps(q("s0r", countL), commit("s0c"))},
			{Name: "s1", Steps: steps(
				q("s1rm", "MATCH (n:L) WHERE n.k < 20 REMOVE n:L RETURN count(n) AS removed"), w.stats("s1st"))},
			{Name: "s2", Steps: steps(q("s2drop", "DROP INDEX l_s"))},
			{Name: "s3", Steps: steps(w.stats("s3st"))},
		},
		Permutations: perms(
			"s1rm s1st s2drop s0r s0c s3st",
			"s2drop s1rm s1st s0r s0c s3st",
			"s1rm s1st s0r s0c s2drop s3st"),
		Final: steps(q("fcount", countL), w.schema("fschema")),
	}
}

func ro01Cypher(*world) *isolationtest.Spec {
	read := "MATCH (n:Item {name:'x'}) RETURN n.v AS v"
	return &isolationtest.Spec{
		Name: "ro01-read-only-tx-stable-and-rejects-writes",
		Doc: "RO01. s1 is a read-only transaction: it reads x, attempts a SET and a CREATE INDEX,\n" +
			"reads x again and commits; s2 sets x.v = 2 in an autocommit statement. Both reads of\n" +
			"s1 return v = 0 (F6); the SET and the DDL are ErrWriteInReadOnlyTx and apply nothing\n" +
			"(F9).",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(
				q("s1r", read), q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 9 RETURN n.v AS v"),
				q("s1ddl", "CREATE INDEX item_v FOR (n:Item) ON (n.v)"), q("s1r2", read), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2w", "MATCH (n:Item {name:'x'}) SET n.v = 2 RETURN n.v AS v"))},
		},
		Final: steps(q("final", read)),
	}
}

func ro01LPG(w *world) *isolationtest.Spec {
	s1, s2 := w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ro01-read-only-snapshot-stable-lpg",
		Doc: "RO01 through the lpg API, the stable-snapshot half (an lpg read snapshot has no\n" +
			"write surface to reject): s1 pins Graph.BeginRead and reads x.v twice; s2 sets\n" +
			"x.v = 2 in its own transaction. Both reads return 0.",
		Setup: steps(w.lpgFixture(lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.beginRead("s1b")), Steps: steps(
				s1.read("s1r", "x.v"), s1.read("s1r2", "x.v"), s1.endRead("s1c"))},
			{Name: "s2", Steps: steps(s2.set("s2w", "x", "v", 2))},
		},
		Final: steps(q("final", "MATCH (n:Item {name:'x'}) RETURN n.v AS v")),
	}
}

// stableRead asserts, per permutation, that s1's two reads agree.
func stableRead() isolationtest.Observer {
	first := map[string]string{}
	return func(o isolationtest.Observation) error {
		if len(o.Rows) != 1 {
			return nil
		}
		switch o.Step {
		case "s1r":
			first[o.Permutation] = o.Rows[0][0]
		case "s1r2":
			if want := first[o.Permutation]; o.Rows[0][0] != want {
				return fmt.Errorf("second read %s differs from first %s", o.Rows[0][0], want)
			}
		}
		return nil
	}
}

func ro02Cypher(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ro02-read-view-before-two-writers",
		Doc: "RO02. s1 and s2 BEGIN; s3 then opens a read-only transaction. s1 writes x and s2\n" +
			"writes y, each committing; s3 reads both. s3 sees neither write in any\n" +
			"interleaving: both commit after its snapshot was taken. The final read, at a new\n" +
			"snapshot, sees both.",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0}), (:Item {name:'y', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 1 RETURN n.v AS v"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:Item {name:'y'}) SET n.v = 2 RETURN n.v AS v"), commit("s2c"))},
			{Name: "s3", Setup: steps(beginRead("s3b")), Steps: steps(
				q("s3r", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name"), commit("s3c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

func ro02LPG(w *world) *isolationtest.Spec {
	s1, s2, s3 := w.lpgSession(), w.lpgSession(), w.lpgSession()
	return &isolationtest.Spec{
		Name: "ro02-read-view-before-two-writers-lpg",
		Doc: "RO02 through the lpg API: s1 and s2 open write transactions, then s3 pins\n" +
			"Graph.BeginRead; s1 sets x.v = 1 and s2 sets y.v = 2, each ending its transaction;\n" +
			"s3 reads both. s3 sees neither.",
		Setup: steps(w.lpgFixture(
			lpgNode{key: "x", labels: []string{"Item"}, props: map[string]int64{"v": 0}},
			lpgNode{key: "y", labels: []string{"Item"}, props: map[string]int64{"v": 0}})),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(s1.begin("s1b")), Steps: steps(s1.set("s1w", "x", "v", 1), s1.commit("s1c"))},
			{Name: "s2", Setup: steps(s2.begin("s2b")), Steps: steps(s2.set("s2w", "y", "v", 2), s2.commit("s2c"))},
			{Name: "s3", Setup: steps(s3.beginRead("s3b")), Steps: steps(s3.read("s3r", "x.v", "y.v"), s3.endRead("s3c"))},
		},
		Final: steps(q("final", "MATCH (n:Item) RETURN n.name AS name, n.v AS v ORDER BY name")),
	}
}

// ro03Nodes is RO03's population, above the parallel-count threshold RO03 sets.
const (
	ro03Nodes     = 3000
	ro03Threshold = 2048
)

func ro03(w *world) *isolationtest.Spec {
	w.parallelScanThreshold = ro03Threshold
	fast := "MATCH (n) RETURN count(n) AS c"
	scan := "MATCH (n) WHERE n.name IS NOT NULL RETURN count(n) AS c"
	return &isolationtest.Spec{
		Name: "ro03-read-only-tx-parallel-count",
		Doc: "RO03. 3 000 nodes, and the engine's parallel-count threshold lowered to 2 048 so a\n" +
			"bare count(n) takes the morsel-parallel path (#1672). s1 is a read-only transaction\n" +
			"that counts through that path and through a filtered serial scan, twice; s2 creates\n" +
			"ten nodes and deletes five in one explicit transaction. Each parallel count equals\n" +
			"the serial one, at s1's snapshot, in every interleaving.",
		Setup: steps(q("mk", "UNWIND range(1, 3000) AS i CREATE (:N {name:'n' + toString(i)})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(
				q("s1fa", fast), q("s1sa", scan), q("s1fl", fast), q("s1sl", scan), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "UNWIND range(1, 10) AS i CREATE (:N {name:'new' + toString(i)}) WITH count(*) AS c "+
					"MATCH (d:N) WHERE d.name IN ['n1', 'n2', 'n3', 'n4', 'n5'] DETACH DELETE d RETURN count(*) AS deleted"),
				commit("s2c"))},
		},
		Final: steps(q("ffast", fast), q("fscan", scan)),
	}
}

func se01(w *world) *isolationtest.Spec {
	read := "MATCH (n:Item {name:'x'}) RETURN n.v AS v"
	return &isolationtest.Spec{
		Name: "se01-session-reads-own-commit",
		Doc: "SE01. s1 commits x.v = 1 in an autocommit statement and then x.v = 2 in an explicit\n" +
			"transaction, reading x through its own cypher.Session after each; s2 reads x\n" +
			"sessionless (Engine.RunInTx). s1 sees each of its commits immediately (F11,\n" +
			"docs/isolation-design.md:189).",
		Setup: steps(q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(
				q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 1 RETURN n.v AS v"), q("s1r", read),
				begin("s1b"), q("s1w2", "MATCH (n:Item {name:'x'}) SET n.v = 2 RETURN n.v AS v"), commit("s1c"),
				q("s1r2", read))},
			{Name: "s2", Steps: steps(w.sessionless("s2r", read))},
		},
		Final: steps(q("final", read)),
	}
}

func se03(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "se03-abandoned-commit-does-not-stall-frontier",
		Doc: "SE03, NOT NULL arm. NOT NULL on (:R).p. s0 creates an :R node without p and\n" +
			"commits: the COMMIT is refused by the constraint. s1 commits x.v = 1 in an explicit\n" +
			"transaction. A sessionless read sees s1's commit, and no commit is left in flight\n" +
			"(InFlightCommits = 0) after either: the refused commit does not hold the frontier.\n" +
			"The fsync arm is se03-abandoned-fsync-does-not-stall-frontier, on the commit hold.",
		Setup: steps(q("ddl", notNullP), q("mk", "CREATE (:Item {name:'x', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s0", Setup: steps(begin("s0b")), Steps: steps(
				q("s0w", "CREATE (n:R {name:'bad'}) RETURN n.name AS name"), commit("s0c"), w.stats("s0st"))},
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:Item {name:'x'}) SET n.v = 1 RETURN n.v AS v"), commit("s1c"),
				w.sessionless("s1r", "MATCH (n:Item {name:'x'}) RETURN n.v AS v"))},
		},
		Final: steps(w.stats("fst"), q("fr", "MATCH (n:R) RETURN count(n) AS r")),
	}
}
