package cypher_test

// ddl_schema_gate_ctx_test.go — rmp #2982: a DDL statement honours its context
// while an autocommit write is in flight.
//
// # The defect
//
// An autocommit write holds Engine.schemaGate shared for its whole statement, and
// every DDL path took the gate exclusively with the context-free StrongLock. A
// CREATE INDEX carrying a 50 ms deadline behind a write parked inside a procedure
// stayed blocked for as long as the write ran, and reported its deadline only once
// the write ended. Found by example 37, catalogue row DD06.
//
// # What is asserted
//
// For every DDL kind, on the in-memory and the WAL-backed engine, and through each
// public entry point in turn:
//
//   - the DDL returns an error matching context.DeadlineExceeded BEFORE the parked
//     write is released;
//   - the schema is unchanged while the write is parked and after it ends, and the
//     write itself commits;
//   - on the WAL-backed engine, recovery of the WAL yields the original schema, so
//     the refused DDL appended nothing;
//   - a retry of the same DDL, with no deadline, succeeds and changes the schema.
//
// The 50 ms deadline is the input under test, not a latency bound: the only
// ordering asserted is "the DDL returned before the blocker was released". The
// backstop below only keeps a regression from hanging the package; reaching it IS
// the failure.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ddlGateSetup is the schema every subtest starts from: one object of each kind
// for the DROP statements to remove, and a node for the CREATE statements to
// index or constrain.
var ddlGateSetup = []string{
	"CREATE (:L {s:'x'}), (:P {a:1, b:2}), (:Q {u:'u1', n:'n1'})",
	"CREATE INDEX pre_hash FOR (n:P) ON (n.a)",
	"CREATE INDEX pre_btree FOR (n:P) ON (n.b) OPTIONS {indexType:'btree'}",
	"CREATE CONSTRAINT pre_u FOR (n:Q) REQUIRE n.u IS UNIQUE",
	"CREATE CONSTRAINT pre_nn FOR (n:Q) REQUIRE n.n IS NOT NULL",
}

// ddlGateKinds is every DDL kind that takes the schema gate: CREATE INDEX for both
// index kinds, DROP INDEX of both kinds, CREATE CONSTRAINT for both constraint
// kinds, DROP CONSTRAINT of both kinds.
var ddlGateKinds = []struct{ name, ddl string }{
	{"create-hash-index", "CREATE INDEX l_s2 FOR (n:L) ON (n.s)"},
	{"create-btree-index", "CREATE INDEX l_s3 FOR (n:L) ON (n.s) OPTIONS {indexType:'btree'}"},
	{"drop-hash-index", "DROP INDEX pre_hash"},
	{"drop-btree-index", "DROP INDEX pre_btree"},
	{"create-unique-constraint", "CREATE CONSTRAINT c_u FOR (n:L) REQUIRE n.s IS UNIQUE"},
	{"create-not-null-constraint", "CREATE CONSTRAINT c_nn FOR (n:L) REQUIRE n.s IS NOT NULL"},
	{"drop-unique-constraint", "DROP CONSTRAINT pre_u"},
	{"drop-not-null-constraint", "DROP CONSTRAINT pre_nn"},
}

// ddlGateEntry is a public entry point a DDL can arrive through. Each one routes
// to the engine's DDL dispatcher with the caller's context.
type ddlGateEntry struct {
	name string
	run  func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error)
}

var ddlGateEntries = []ddlGateEntry{
	{"Run", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.Run(ctx, q, nil)
	}},
	{"RunInTx", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.RunInTx(ctx, q, nil)
	}},
	{"RunAny", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.RunAny(ctx, q, nil)
	}},
	{"Session.RunAny", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.NewSession().RunAny(ctx, q, nil)
	}},
}

// ddlGateHold is the write statement parked inside the gate.hold procedure.
type ddlGateHold struct {
	entered chan struct{}
	release chan struct{}
	done    chan error
}

// registerDDLGateHold registers gate.hold on eng: a procedure that parks until
// h.release is closed or its context ends.
func registerDDLGateHold(t *testing.T, eng *cypher.Engine, h *ddlGateHold) {
	t.Helper()
	err := eng.Procs().Register(procs.Signature{
		Namespace: []string{"gate"},
		Name:      "hold",
		Outputs:   []procs.NamedType{{Name: "x", Kind: expr.KindInteger}},
	}, func(ctx context.Context, _ []expr.Value) ([][]expr.Value, error) {
		close(h.entered)
		select {
		case <-h.release:
			return [][]expr.Value{{expr.IntegerValue(1)}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("register gate.hold: %v", err)
	}
}

// ddlGateExec runs q and drains its result.
func ddlGateExec(ctx context.Context, run func(context.Context, string) (*cypher.Result, error), q string) error {
	res, err := run(ctx, q)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	return errors.Join(res.Err(), res.Close())
}

// ddlGateSchema renders the engine's indexes and constraints, sorted.
func ddlGateSchema(eng *cypher.Engine) []string {
	idx, cons := eng.ListIndexes(), eng.Constraints()
	out := make([]string, 0, len(idx)+len(cons))
	for _, n := range idx {
		out = append(out, "index "+n)
	}
	for _, c := range cons {
		out = append(out, fmt.Sprintf("constraint %s unique=%v (:%s).%s", c.Name, c.Unique, c.Label, c.Property))
	}
	slices.Sort(out)
	return out
}

// ddlGateDurableSchema renders the index and constraint definitions recovered
// from dir, sorted, so the WAL content is compared with the live schema's names.
func ddlGateDurableSchema(t *testing.T, dir string) []string {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	out := make([]string, 0, len(res.Indexes)+len(res.Constraints))
	for _, r := range res.Indexes {
		out = append(out, fmt.Sprintf("index %s kind=%d (:%s).%s", r.Name, r.Kind, r.Label, r.Property))
	}
	for _, c := range res.Constraints {
		out = append(out, fmt.Sprintf("constraint %s kind=%d (:%s).%s", c.Name, c.Kind, c.Label, c.Property))
	}
	slices.Sort(out)
	return out
}

// ddlGateBackstop bounds the wait for a DDL that does not honour its context, so a
// regression fails the test rather than hanging it. It is not a latency bound.
const ddlGateBackstop = 10 * time.Second

func TestDDL_HonoursDeadlineBehindInFlightWrite(t *testing.T) {
	for _, wiring := range []string{"memory", "wal"} {
		for i, k := range ddlGateKinds {
			entry := ddlGateEntries[i%len(ddlGateEntries)]
			t.Run(wiring+"/"+k.name+"/"+entry.name, func(t *testing.T) {
				t.Parallel()
				runDDLGateCase(t, wiring, k.ddl, entry)
			})
		}
	}
}

func runDDLGateCase(t *testing.T, wiring, ddl string, entry ddlGateEntry) {
	var (
		eng *cypher.Engine
		dir string
		w   *wal.Writer
	)
	if wiring == "memory" {
		eng = cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	} else {
		dir = t.TempDir()
		var err error
		w, err = wal.OpenWithSyncLatency(filepath.Join(dir, "wal"), synclatency.ForTest(t))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() })
		g := lpg.New[string, float64](adjlist.Config{})
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		}))
	}
	t.Cleanup(func() { _ = eng.Close() })
	run := func(ctx context.Context, q string) (*cypher.Result, error) { return entry.run(ctx, eng, q) }
	bg := context.Background()

	setup := func(ctx context.Context, q string) (*cypher.Result, error) { return eng.RunInTx(ctx, q, nil) }
	for _, q := range ddlGateSetup {
		if err := ddlGateExec(bg, setup, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	before := ddlGateSchema(eng)
	var durableBefore []string
	if w != nil {
		if err := w.Sync(); err != nil {
			t.Fatalf("wal.Sync: %v", err)
		}
		durableBefore = ddlGateDurableSchema(t, dir)
		// Non-vacuity: the WAL comparison below means something only if the
		// setup's four schema objects are durable.
		if len(durableBefore) != 4 {
			t.Fatalf("durable schema after setup = %v, want the 4 setup objects", durableBefore)
		}
	}

	// Park an autocommit write inside the procedure: it holds the schema gate
	// shared until it is released.
	h := &ddlGateHold{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
	registerDDLGateHold(t, eng, h)
	released := false
	releaseHold := func() {
		if !released {
			released = true
			close(h.release)
		}
	}
	t.Cleanup(func() {
		releaseHold()
		<-h.done
	})
	go func() {
		h.done <- ddlGateExec(bg, setup, "CREATE (n:T {name:'t'}) WITH n CALL gate.hold() YIELD x RETURN x")
	}()
	select {
	case <-h.entered:
	case err := <-h.done:
		h.done <- err // for the cleanup
		t.Fatalf("the write ended before it reached gate.hold: %v", err)
	}

	// The DDL, bounded by a 50 ms deadline, while the write is parked.
	ddlDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
		defer cancel()
		ddlDone <- ddlGateExec(ctx, run, ddl)
	}()
	var ddlErr error
	select {
	case ddlErr = <-ddlDone:
	case <-time.After(ddlGateBackstop):
		releaseHold()
		ddlErr = <-ddlDone
		t.Fatalf("%q did not return before the in-flight write was released; it returned %v only afterwards", ddl, ddlErr)
	}
	if !errors.Is(ddlErr, context.DeadlineExceeded) {
		t.Fatalf("%q behind an in-flight write: err = %v, want context.DeadlineExceeded", ddl, ddlErr)
	}
	if got := ddlGateSchema(eng); !slices.Equal(got, before) {
		t.Fatalf("schema changed by the refused %q while the write is parked:\n got %v\nwant %v", ddl, got, before)
	}

	// Release the write: it commits, and the schema is still the original one.
	releaseHold()
	werr := <-h.done
	h.done <- werr // for the cleanup
	if werr != nil {
		t.Fatalf("the parked write after release: %v", werr)
	}
	countT := 0
	res, err := eng.Run(bg, "MATCH (n:T) RETURN n.name AS name", nil)
	if err != nil {
		t.Fatalf("read :T: %v", err)
	}
	for res.Next() {
		countT++
	}
	if err := errors.Join(res.Err(), res.Close()); err != nil {
		t.Fatalf("read :T: %v", err)
	}
	if countT != 1 {
		t.Fatalf(":T nodes after the parked write committed = %d, want 1", countT)
	}
	if got := ddlGateSchema(eng); !slices.Equal(got, before) {
		t.Fatalf("schema changed by the refused %q after the write ended:\n got %v\nwant %v", ddl, got, before)
	}
	if w != nil {
		if err := w.Sync(); err != nil {
			t.Fatalf("wal.Sync: %v", err)
		}
		if got := ddlGateDurableSchema(t, dir); !slices.Equal(got, durableBefore) {
			t.Fatalf("the refused %q reached the WAL:\n got %v\nwant %v", ddl, got, durableBefore)
		}
	}

	// A retry with no deadline succeeds and does change the schema.
	if err := ddlGateExec(bg, run, ddl); err != nil {
		t.Fatalf("retry %q: %v", ddl, err)
	}
	if got := ddlGateSchema(eng); slices.Equal(got, before) {
		t.Fatalf("retry %q left the schema unchanged: %v", ddl, got)
	}
}
