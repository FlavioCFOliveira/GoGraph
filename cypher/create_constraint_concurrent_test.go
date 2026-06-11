package cypher_test

// create_constraint_concurrent_test.go — task #1339
//
// Liveness gate: CREATE CONSTRAINT must complete while a concurrent writer
// sustains CREATE statements, and the engine must remain responsive after.
//
// Before the #1339 fix, the validation scan (Engine.scanLabelProperty) called
// HasNodeLabel/GetNodeProperty — both of which re-enter Mapper.Lookup — from
// inside Mapper.Walk, while Walk held the shard's read lock. A concurrent
// writer's Mapper.Intern (internSlow) queued a write lock between the outer
// and the nested read lock; sync.RWMutex stops admitting new readers once a
// writer waits, so the nested Lookup blocked forever, the writer blocked
// forever, and every future operation on that shard froze the whole engine.
// The fix snapshots the (id, key) pairs under Walk and resolves graph state
// only after every shard lock has been released.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// drainCloseResult iterates res to exhaustion and closes it, returning the
// first error encountered (iteration error wins over close error).
func drainCloseResult(res *cypher.Result) error {
	for res.Next() {
	}
	err := res.Err()
	if cerr := res.Close(); err == nil {
		err = cerr
	}
	return err
}

// TestCreateConstraint_ConcurrentWriter_NoDeadlock is the regression gate for
// task #1339. A background goroutine sustains CREATE (:P {email: …}) write
// statements while the main goroutine issues a sequence of CREATE CONSTRAINT
// statements, each of which runs the pre-existing-data validation scan over
// the whole node mapper. Every constraint creation must complete within the
// deadline; before the fix the scan deadlocked against the writer on a mapper
// shard lock, wedging the engine permanently, which this test detects via the
// deadline instead of hanging forever.
func TestCreateConstraint_ConcurrentWriter_NoDeadlock(t *testing.T) {
	// Deliberately not parallel: this is a timing-sensitive liveness gate that
	// needs the writer goroutine running unhindered while the scans execute.
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed enough :P nodes that the validation scan visits every mapper shard
	// (256) and performs many nested per-key lookups inside each one, keeping
	// each shard's read lock held long enough for a concurrent intern to queue
	// behind it (the pre-fix deadlock window).
	const seedNodes = 20000
	seedQ := fmt.Sprintf(
		`UNWIND range(0, %d) AS i CREATE (:P {email: 'seed-' + toString(i)})`,
		seedNodes-1)
	res, err := eng.RunInTx(ctx, seedQ, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := drainCloseResult(res); err != nil {
		t.Fatalf("seed drain: %v", err)
	}

	// Background writer: batches of CREATE (:P {email: …}) with globally
	// unique emails, each batch interning fresh node keys into pseudo-random
	// mapper shards for the whole duration of the constraint scans.
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		const batch = 64
		for n := 0; ; n += batch {
			select {
			case <-stop:
				return
			default:
			}
			q := fmt.Sprintf(
				`UNWIND range(%d, %d) AS i CREATE (:P {email: 'w-' + toString(i)})`,
				n, n+batch-1)
			wres, werr := eng.RunInTx(ctx, q, nil)
			if werr != nil {
				continue // keep sustaining write pressure
			}
			_ = drainCloseResult(wres)
		}
	}()

	// Give the writer a moment to be mid-flight before the first scan.
	time.Sleep(20 * time.Millisecond)

	// Run a sequence of CREATE CONSTRAINT statements, each triggering a full
	// validation scan, then a final UNIQUE constraint on the seeded property.
	// The whole sequence must finish within the deadline; a single pre-fix
	// deadlock freezes the sequence and trips it.
	const (
		attempts = 10
		deadline = 15 * time.Second
	)
	constraintErr := make(chan error, 1)
	go func() {
		for k := 0; k < attempts; k++ {
			q := fmt.Sprintf(
				`CREATE CONSTRAINT c_gate_%d ON (n:P) ASSERT n.gate_%d IS UNIQUE`, k, k)
			cres, cerr := eng.Run(ctx, q, nil)
			if cerr == nil {
				cerr = drainCloseResult(cres)
			}
			if cerr != nil {
				constraintErr <- fmt.Errorf("attempt %d: %w", k, cerr)
				return
			}
		}
		cres, cerr := eng.Run(ctx,
			`CREATE CONSTRAINT email_uniq ON (n:P) ASSERT n.email IS UNIQUE`, nil)
		if cerr == nil {
			cerr = drainCloseResult(cres)
		}
		constraintErr <- cerr
	}()

	select {
	case err := <-constraintErr:
		if err != nil {
			t.Fatalf("CREATE CONSTRAINT under concurrent writer: %v", err)
		}
	case <-time.After(deadline):
		close(stop) // best effort; the writer is most likely wedged too
		t.Fatalf("deadlock: CREATE CONSTRAINT did not complete within %v while a concurrent writer was active (task #1339 liveness gate)", deadline)
	}

	// The writer must drain promptly — a wedged writer means the engine is no
	// longer responsive even though the constraint goroutine returned.
	close(stop)
	select {
	case <-writerDone:
	case <-time.After(deadline):
		t.Fatalf("writer goroutine did not stop within %v after CREATE CONSTRAINT completed (engine wedged)", deadline)
	}

	// Liveness probes: one write and one read must both complete promptly.
	probe := func(name string, run func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- run() }()
		select {
		case perr := <-done:
			if perr != nil {
				t.Fatalf("%s probe: %v", name, perr)
			}
		case <-time.After(deadline):
			t.Fatalf("%s probe did not complete within %v (engine wedged)", name, deadline)
		}
	}
	probe("write", func() error {
		pres, perr := eng.RunInTx(ctx, `CREATE (:P {email: 'probe@example.com'})`, nil)
		if perr != nil {
			return perr
		}
		return drainCloseResult(pres)
	})
	probe("read", func() error {
		pres, perr := eng.Run(ctx, `MATCH (p:P) RETURN count(p) AS c`, nil)
		if perr != nil {
			return perr
		}
		return drainCloseResult(pres)
	})
}
