package cypher_test

// ddl_withdrawn_schema_gate_test.go — rmp #2983: once a DDL has given up on the
// schema gate, the gate admits new writes as if the DDL had never asked.
//
// # The defect
//
// A DDL behind an in-flight autocommit write returned context.DeadlineExceeded on
// time (rmp #2982), but mvcc.Gate.StrongLockCtx only abandoned the WAIT: a helper
// goroutine kept the acquisition queued with the gate's strong flag raised until
// the in-flight write left. Every new autocommit write in that interval found the
// flag and blocked behind a DDL whose caller had already returned its error.
//
// # What is asserted
//
// On the in-memory and the WAL-backed engine, and through each autocommit write
// entry point in turn: with a write parked inside a procedure and a CREATE INDEX
// refused on its 50 ms deadline, a second write — carrying NO deadline — commits
// while the first is still parked. The schema is unchanged throughout.
//
// The only ordering asserted is "the second write committed before the first was
// released". The backstop only keeps a regression from hanging the package;
// reaching it IS the failure.

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// withdrawnGateWriters is every autocommit write entry point the second write can
// arrive through.
var withdrawnGateWriters = []ddlGateEntry{
	{"RunInTx", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.RunInTx(ctx, q, nil)
	}},
	{"RunAny", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.RunAny(ctx, q, nil)
	}},
	{"RunInTxAny", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.RunInTxAny(ctx, q, nil)
	}},
	{"Session.RunAny", func(ctx context.Context, eng *cypher.Engine, q string) (*cypher.Result, error) {
		return eng.NewSession().RunAny(ctx, q, nil)
	}},
}

func TestDDL_WithdrawnSchemaGateRequestAdmitsNewWrites(t *testing.T) {
	for _, wiring := range []string{"memory", "wal"} {
		for _, writer := range withdrawnGateWriters {
			t.Run(wiring+"/"+writer.name, func(t *testing.T) {
				t.Parallel()
				runWithdrawnGateCase(t, wiring, writer)
			})
		}
	}
}

func runWithdrawnGateCase(t *testing.T, wiring string, writer ddlGateEntry) {
	g := lpg.New[string, float64](adjlist.Config{})
	var eng *cypher.Engine
	if wiring == "memory" {
		eng = cypher.NewEngine(g)
	} else {
		w, err := wal.OpenWithSyncLatency(filepath.Join(t.TempDir(), "wal"), synclatency.ForTest(t))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() })
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		}))
	}
	t.Cleanup(func() { _ = eng.Close() })
	bg := context.Background()
	setup := func(ctx context.Context, q string) (*cypher.Result, error) { return eng.RunInTx(ctx, q, nil) }
	if err := ddlGateExec(bg, setup, "CREATE (:L {s:'x'})"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := ddlGateSchema(eng)

	// Park the first write inside the procedure: it holds the schema gate shared.
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
		t.Fatalf("the first write ended before it reached gate.hold: %v", err)
	}

	// The DDL gives up on its deadline (rmp #2982).
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	ddlErr := ddlGateExec(ctx, setup, "CREATE INDEX l_s FOR (n:L) ON (n.s)")
	cancel()
	if !errors.Is(ddlErr, context.DeadlineExceeded) {
		t.Fatalf("CREATE INDEX behind an in-flight write: err = %v, want context.DeadlineExceeded", ddlErr)
	}

	// A second write, with no deadline, while the first is still parked.
	run := func(ctx context.Context, q string) (*cypher.Result, error) { return writer.run(ctx, eng, q) }
	second := make(chan error, 1)
	go func() { second <- ddlGateExec(bg, run, "CREATE (:U {k:1})") }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("%s: second write: %v", writer.name, err)
		}
	case <-time.After(ddlGateBackstop):
		releaseHold()
		err := <-second
		t.Fatalf("%s: the second write did not commit while the first was parked; it returned "+
			"%v only after the release. The DDL that gave up still holds the gate's strong "+
			"request", writer.name, err)
	}
	if got := ddlGateSchema(eng); !slices.Equal(got, before) {
		t.Fatalf("schema changed by the refused CREATE INDEX:\n got %v\nwant %v", got, before)
	}

	releaseHold()
	werr := <-h.done
	h.done <- werr // for the cleanup
	if werr != nil {
		t.Fatalf("the parked write after release: %v", werr)
	}
}
