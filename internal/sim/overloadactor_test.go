package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestOverloadActor_AllFamiliesBounded drives every overload family and asserts
// each yields an acceptable outcome (a bounded success or a typed bound error)
// within a generous deadline (no deadlock), and that a result-bearing family
// never streams more rows than the engine's declared cap (no unbounded
// materialisation). goleak verifies no goroutine leaked.
func TestOverloadActor_AllFamiliesBounded(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// Seed a small graph so the VLE family has something to expand over.
	seedGraph(t, srv, 30)

	actor := OverloadActor{}
	for fam := OverloadFamily(0); fam < overloadFamilyCount; fam++ {
		out := runOverloadWithDeadline(t, srv, actor, fam, 60*time.Second)
		if !out.Acceptable() {
			t.Errorf("%s: unacceptable outcome %+v (want bounded success or typed bound error)", fam, out)
		}
		// A successful result-bearing family must not exceed the engine's cap: the
		// streamed row count is the proof the result set was bounded, not
		// materialised whole in memory.
		if out.Succeeded && out.Rows > defaultSimResultRowCap {
			t.Errorf("%s: streamed %d rows, exceeds the cap %d — result set was not bounded",
				fam, out.Rows, defaultSimResultRowCap)
		}
	}
}

// runOverloadWithDeadline runs one overload family on a fresh connection,
// failing the test if it does not return within d (a deadlock/livelock guard).
func runOverloadWithDeadline(t *testing.T, srv *SimServer, actor OverloadActor, fam OverloadFamily, d time.Duration) OverloadOutcome {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("%s Dial: %v", fam, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("%s Connect: %v", fam, err)
	}

	type result struct {
		out OverloadOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := actor.Run(c, fam)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%s Run: %v", fam, r.err)
		}
		return r.out
	case <-time.After(d):
		t.Fatalf("%s did not complete within %s — possible deadlock/livelock", fam, d)
		return OverloadOutcome{}
	}
}

// TestOverloadActor_LargeTxDurable proves a large legitimate transaction
// commits durably: after a single statement creates overloadCreateBatch nodes,
// a fresh read sees exactly that many. Acknowledged work is never dropped.
func TestOverloadActor_LargeTxDurable(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	out := runOverloadWithDeadline(t, srv, OverloadActor{}, OverloadLargeCreateTx, 60*time.Second)
	if !out.Succeeded {
		t.Fatalf("large create tx did not succeed: %+v", out)
	}

	// Re-read the bulk node count over a fresh connection.
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Run("MATCH (n:Bulk) RETURN count(n)", nil); err != nil {
		t.Fatalf("RUN count: %v", err)
	}
	records, _, err := c.PullAll()
	if err != nil {
		t.Fatalf("PULL count: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(records))
	}
	if n, _ := records[0].Data[0].(int64); n != overloadCreateBatch {
		t.Fatalf("acknowledged bulk write lost: count=%v, want %d", records[0].Data[0], overloadCreateBatch)
	}
}

// TestOverloadActor_FamiliesReproducible proves PickFamily is seed-pure.
func TestOverloadActor_FamiliesReproducible(t *testing.T) {
	t.Parallel()
	const seed = 0x0AD
	draw := func() []OverloadFamily {
		s := NewSeed(seed)
		a := OverloadActor{}
		out := make([]OverloadFamily, 24)
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

// seedGraph creates n linked Person nodes over the wire so traversal-heavy
// overload families have a graph to expand over.
func seedGraph(t *testing.T, srv *SimServer, n int) {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("seed Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("seed Connect: %v", err)
	}
	q := "UNWIND range(1, $n) AS i CREATE (:Person {name: 'p' + toString(i)})"
	if _, err := c.Run(q, map[string]any{"n": int64(n)}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if _, term, err := c.PullAll(); err != nil {
		t.Fatalf("seed pull: %v", err)
	} else if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("seed create FAILURE: %s %s", f.Code, f.Message)
	}
}
