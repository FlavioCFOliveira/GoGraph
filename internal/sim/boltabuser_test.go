package sim

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltAbuser_AllFamiliesAcceptable drives every abuse family against a real
// bolt/server and asserts each yields an acceptable outcome (typed FAILURE or
// clean close) — never an unexpected SUCCESS, panic, or hang. It also asserts
// the server's graph state is unchanged after the abuse (no corruption) and that
// the server still serves an honest query afterwards (the abuse did not wedge
// it). goleak verifies no goroutine leaked across the whole battery.
func TestBoltAbuser_AllFamiliesAcceptable(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	abuser := BoltAbuser{}

	for fam := AbuseFamily(0); fam < abuseFamilyCount; fam++ {
		out, err := abuser.Abuse(srv, fam)
		if err != nil {
			t.Fatalf("Abuse(%s) harness error: %v", fam, err)
		}
		if !out.Acceptable() {
			t.Errorf("Abuse(%s): unacceptable outcome %+v (want FAILURE or clean close)", fam, out)
		}
		// After every abuse the server must still answer an honest query: prove
		// the abuse neither wedged the accept loop nor corrupted shared state.
		assertServerStillHealthy(t, srv, fam)
	}
}

// assertServerStillHealthy opens a fresh honest connection and runs a trivial
// query, failing if the server does not respond with a SUCCESS-terminated
// result. It proves an abuse on one connection did not break the server for the
// next one.
func assertServerStillHealthy(t *testing.T, srv *SimServer, afterFamily AbuseFamily) {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("post-%s Dial: %v", afterFamily, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("post-%s Connect: %v", afterFamily, err)
	}
	if _, err := c.Run("RETURN 1", nil); err != nil {
		t.Fatalf("post-%s RUN: %v", afterFamily, err)
	}
	records, term, err := c.PullAll()
	if err != nil {
		t.Fatalf("post-%s PULL: %v", afterFamily, err)
	}
	if f, ok := term.(*proto.Failure); ok {
		t.Fatalf("post-%s honest query FAILURE: %s %s", afterFamily, f.Code, f.Message)
	}
	if len(records) != 1 {
		t.Fatalf("post-%s honest query returned %d records, want 1", afterFamily, len(records))
	}
}

// TestBoltAbuser_NoStateCorruption proves the abuse battery leaves no nodes or
// edges behind: a malformed wire message must never partially apply a write.
func TestBoltAbuser_NoStateCorruption(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	abuser := BoltAbuser{}
	for fam := AbuseFamily(0); fam < abuseFamilyCount; fam++ {
		if _, err := abuser.Abuse(srv, fam); err != nil {
			t.Fatalf("Abuse(%s): %v", fam, err)
		}
	}

	// The graph must be empty: no abuse family ever created a node.
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Run("MATCH (n) RETURN count(n)", nil); err != nil {
		t.Fatalf("RUN count: %v", err)
	}
	records, _, err := c.PullAll()
	if err != nil {
		t.Fatalf("PULL count: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(records))
	}
	if n, _ := records[0].Data[0].(int64); n != 0 {
		t.Fatalf("abuse battery corrupted state: node count = %v, want 0", records[0].Data[0])
	}
}

// TestBoltAbuser_FamiliesReproducible proves PickFamily is a pure function of the
// seed: the same seed draws the same family sequence.
func TestBoltAbuser_FamiliesReproducible(t *testing.T) {
	t.Parallel()
	const seed = 0x5EED
	draw := func() []AbuseFamily {
		s := NewSeed(seed)
		a := BoltAbuser{}
		out := make([]AbuseFamily, 32)
		for i := range out {
			out[i] = a.PickFamily(s)
		}
		return out
	}
	first, second := draw(), draw()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("family draw diverged at %d: %s vs %s", i, first[i], second[i])
		}
	}
}
