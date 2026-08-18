package sim

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestSchemaChanger_AllFamiliesMeetContract drives every DDL family — the
// constraint create in BOTH grammars (legacy ON ... ASSERT and modern
// FOR ... REQUIRE) — and asserts each outcome meets its family contract (the
// IF NOT EXISTS create families must SUCCEED; drops accept success or a typed
// FAILURE) without wedging the connection. goleak-clean.
func TestSchemaChanger_AllFamiliesMeetContract(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	a := SchemaChanger{}
	runs := []struct {
		fam    SchemaChangeFamily
		modern bool
	}{
		{SchemaCreateIndex, false},
		{SchemaDropIndex, false},
		{SchemaCreateConstraint, false}, // legacy ON ... ASSERT ... IF NOT EXISTS
		{SchemaDropConstraint, false},
		{SchemaCreateConstraint, true}, // modern IF NOT EXISTS FOR ... REQUIRE
		{SchemaDropConstraint, false},
	}
	for _, r := range runs {
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("%s Dial: %v", r.fam, err)
		}
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("%s Connect: %v", r.fam, err)
		}
		out, err := a.Run(c, r.fam, r.modern)
		_ = c.Close()
		if err != nil {
			t.Fatalf("%s (modern=%t) Run: %v", r.fam, r.modern, err)
		}
		if !out.MeetsContract() {
			t.Errorf("%s (modern=%t): contract-breaching outcome %+v", r.fam, r.modern, out)
		}
	}
}

// TestSchemaChanger_IfNotExistsRecreateSucceeds pins the idempotent-SUCCESS
// contract of the IF NOT EXISTS create families (rmp #2455): re-creating the
// SAME index or constraint — in either constraint grammar — must SUCCEED as a
// clean no-op, never return a tolerated "already exists" FAILURE. Before this
// task the constraint create carried no IF NOT EXISTS and the churn treated
// the re-create failure as acceptable; this test fails on that behaviour.
func TestSchemaChanger_IfNotExistsRecreateSucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	a := SchemaChanger{}
	steps := []struct {
		name   string
		fam    SchemaChangeFamily
		modern bool
	}{
		{"create index", SchemaCreateIndex, false},
		{"re-create index", SchemaCreateIndex, false},
		{"create constraint (legacy)", SchemaCreateConstraint, false},
		{"re-create constraint (legacy)", SchemaCreateConstraint, false},
		{"re-create constraint (modern)", SchemaCreateConstraint, true},
	}
	for _, s := range steps {
		out, err := a.Run(c, s.fam, s.modern)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if !out.Succeeded {
			t.Fatalf("%s: want idempotent SUCCESS, got %+v", s.name, out)
		}
	}
}

// TestSchemaChanger_DDLUnderConcurrentWritesConsistent runs DDL churn
// concurrently with honest writers, then drives the schema into a known state
// and asserts the index is consistent with its base data and the UNIQUE
// constraint is enforced — proving the engine survived the races with no torn
// index, no lost constraint, no panic, and no leak.
func TestSchemaChanger_DDLUnderConcurrentWritesConsistent(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// Seed some Person nodes so the (:Person).name index has base data.
	seedNamedPersons(t, srv, 200)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		wg            sync.WaitGroup
		panics        int
		panicsMu      sync.Mutex
		churnErr      error
		churnOutcomes []SchemaChangeOutcome
		writerErr     error
	)
	recordPanic := func() {
		if r := recover(); r != nil {
			panicsMu.Lock()
			panics++
			panicsMu.Unlock()
		}
	}

	// DDL churn goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer recordPanic()
		churnOutcomes, churnErr = RunSchemaChurn(ctx, srv, NewSeed(0xDD1), 40)
	}()

	// Concurrent honest writer goroutines, adding more Person nodes while DDL runs.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer recordPanic()
			c, err := srv.Dial()
			if err != nil {
				writerErr = err
				return
			}
			defer func() { _ = c.Close() }()
			if err := c.Connect(ctx); err != nil {
				return
			}
			for i := 0; i < 50 && ctx.Err() == nil; i++ {
				name := fmt.Sprintf("w%d-person-%d", id, i)
				if _, err := c.Run(tmplCreatePerson, map[string]any{"name": name, "age": int64(i)}); err != nil {
					return
				}
				if _, _, err := c.PullAll(); err != nil {
					return
				}
			}
		}(w)
	}

	wg.Wait()
	if churnErr != nil {
		t.Fatalf("schema churn: %v", churnErr)
	}
	// Every churn outcome must meet its family contract: the IF NOT EXISTS
	// create families succeed even when they re-create (rmp #2455), and the
	// drops complete cleanly (success or a typed FAILURE under contention).
	for i, out := range churnOutcomes {
		if !out.MeetsContract() {
			t.Errorf("churn outcome %d breaches its family contract: %+v", i, out)
		}
	}
	if writerErr != nil {
		t.Fatalf("concurrent writer: %v", writerErr)
	}
	if panics != 0 {
		t.Fatalf("recovered %d panics during DDL-under-load (want 0)", panics)
	}

	// Drive the schema into a known state and assert structural invariants.
	assertIndexConsistentWithData(t, srv)
	assertConstraintEnforced(t, srv)
}

// seedNamedPersons creates n Person nodes with distinct names over the wire.
func seedNamedPersons(t *testing.T, srv *SimServer, n int) {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("seed Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("seed Connect: %v", err)
	}
	if _, err := c.Run("UNWIND range(1, $n) AS i CREATE (:Person {name: 'seed-' + toString(i)})", map[string]any{"n": int64(n)}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if _, term, err := c.PullAll(); err != nil {
		t.Fatalf("seed pull: %v", err)
	} else if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("seed FAILURE: %s %s", f.Code, f.Message)
	}
}

// assertIndexConsistentWithData ensures an index built after the churn returns
// the same result as a scan: a torn index would disagree. It creates the index
// freshly (idempotent), then compares an indexed equality lookup against a
// scan-based one for the same name.
func assertIndexConsistentWithData(t *testing.T, srv *SimServer) {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("index-check Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("index-check Connect: %v", err)
	}

	// Ensure the index exists (idempotent) so subsequent lookups are index-backed.
	mustRunDDL(t, c, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s FOR (n:Person) ON (n.name)", schemaIndexName))

	// An indexed equality lookup must return exactly the nodes a scan would. Use a
	// known seeded name and compare counts.
	const probe = "seed-42"
	indexed := scalarCount(t, c, "MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": probe})
	scanned := scalarCount(t, c, "MATCH (n:Person) WHERE n.name = $name RETURN count(n)", map[string]any{"name": probe})
	if indexed != scanned {
		t.Errorf("index inconsistent with base data: indexed lookup=%d, scan=%d for name %q", indexed, scanned, probe)
	}
	if indexed != 1 {
		t.Errorf("seeded node %q not found via index (count=%d, want 1) — index/data torn", probe, indexed)
	}
}

// assertConstraintEnforced creates a UNIQUE constraint on a label the churn
// never touched and proves it rejects a duplicate — the constraint machinery
// survived the concurrent DDL races and still enforces uniqueness.
//
// It deliberately uses a fresh label (SimUniq) rather than the churned Account
// label and does NOT exercise DROP CONSTRAINT — not because the by-name drop
// is defective (the historical #1556 fail-silent no-op is FIXED, and
// dropconstraint_finding_test.go pins the correct behaviour: a by-name drop
// removes enforcement and its backing index atomically), but to isolate the
// property under test — enforcement survives churn — on a pristine label with
// no other DDL in play.
func assertConstraintEnforced(t *testing.T, srv *SimServer) {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("constraint-check Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("constraint-check Connect: %v", err)
	}

	mustRunDDL(t, c, "CREATE CONSTRAINT sim_uniq_check ON (u:SimUniq) ASSERT u.k IS UNIQUE")

	// First insert with a given key succeeds.
	if _, err := c.Run("CREATE (u:SimUniq {k:$k})", map[string]any{"k": "dup"}); err != nil {
		t.Fatalf("first SimUniq RUN: %v", err)
	}
	if _, term, err := c.PullAll(); err != nil {
		t.Fatalf("first SimUniq PULL: %v", err)
	} else if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("first SimUniq unexpectedly rejected: %s %s", f.Code, f.Message)
	}

	// A duplicate key must be rejected by the constraint.
	resp, err := c.Run("CREATE (u:SimUniq {k:$k})", map[string]any{"k": "dup"})
	if err != nil {
		t.Fatalf("dup SimUniq RUN: %v", err)
	}
	rejected := false
	if _, ok := resp.(*proto.Failure); ok {
		rejected = true
	} else if _, term, err := c.PullAll(); err == nil {
		if _, ok := term.(*proto.Failure); ok {
			rejected = true
		}
	}
	if !rejected {
		t.Error("UNIQUE constraint not enforced after DDL churn: a duplicate key was accepted")
	}
}

// mustRunDDL runs a DDL statement and fails the test if it errors or returns a
// FAILURE; it RESETs the session on failure so the connection stays usable.
func mustRunDDL(t *testing.T, c *WireClient, q string) {
	t.Helper()
	resp, err := c.Run(q, nil)
	if err != nil {
		t.Fatalf("DDL %q RUN: %v", q, err)
	}
	if f, ok := resp.(*proto.Failure); ok {
		t.Fatalf("DDL %q FAILURE: %s %s", q, f.Code, f.Message)
	}
	if _, term, err := c.PullAll(); err != nil {
		t.Fatalf("DDL %q PULL: %v", q, err)
	} else if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("DDL %q FAILURE: %s %s", q, f.Code, f.Message)
	}
}

// scalarCount runs a count query and returns the single int64 result.
func scalarCount(t *testing.T, c *WireClient, q string, params map[string]any) int64 {
	t.Helper()
	if _, err := c.Run(q, params); err != nil {
		t.Fatalf("count RUN %q: %v", q, err)
	}
	records, term, err := c.PullAll()
	if err != nil {
		t.Fatalf("count PULL %q: %v", q, err)
	}
	if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("count %q FAILURE: %s %s", q, f.Code, f.Message)
	}
	if len(records) != 1 || len(records[0].Data) != 1 {
		t.Fatalf("count %q unexpected shape: %d rows", q, len(records))
	}
	n, _ := records[0].Data[0].(int64)
	return n
}

// TestSchemaChanger_FamiliesReproducible proves PickFamily is seed-pure.
func TestSchemaChanger_FamiliesReproducible(t *testing.T) {
	t.Parallel()
	const seed = 0xDDDD
	draw := func() []SchemaChangeFamily {
		s := NewSeed(seed)
		a := SchemaChanger{}
		out := make([]SchemaChangeFamily, 24)
		for i := range out {
			out[i] = a.PickFamily(s)
		}
		return out
	}
	first, second := draw(), draw()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("draw diverged at %d: %s vs %s", i, first[i], second[i])
		}
	}
}
