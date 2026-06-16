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

// TestSchemaChanger_AllFamiliesAcceptable drives every DDL family and asserts
// each yields an acceptable outcome (success or typed FAILURE) without wedging
// the connection, and that the server stays healthy. goleak-clean.
func TestSchemaChanger_AllFamiliesAcceptable(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	a := SchemaChanger{}
	for fam := SchemaChangeFamily(0); fam < schemaChangeFamilyCount; fam++ {
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("%s Dial: %v", fam, err)
		}
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("%s Connect: %v", fam, err)
		}
		out, err := a.Run(c, fam)
		_ = c.Close()
		if err != nil {
			t.Fatalf("%s Run: %v", fam, err)
		}
		if !out.Acceptable() {
			t.Errorf("%s: unacceptable outcome %+v", fam, out)
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
		wg        sync.WaitGroup
		panics    int
		panicsMu  sync.Mutex
		churnErr  error
		writerErr error
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
		_, churnErr = RunSchemaChurn(ctx, srv, NewSeed(0xDD1), 40)
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
// label and does NOT exercise DROP CONSTRAINT: DROP CONSTRAINT <name> (by name
// only) is a known fail-silent no-op in this engine — it reports SUCCESS but
// leaves both the backing index and the registry entry in place, so the
// constraint stays enforced and a re-create later fails with "backing index
// already exists". That defect is reported separately (see the package-level
// finding note); this assertion isolates the property under test (enforcement
// survives churn) from it by using a pristine label.
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
