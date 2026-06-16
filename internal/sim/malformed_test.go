package sim

import (
	"context"
	"testing"
)

// forceMalformedFamily constructs each malformed family directly so the test
// covers every one regardless of the seed draw. It returns one Op per family
// index in [0, malformedKindCount).
func forceMalformedFamily(m MalformedSender, seed *Seed, family int) Op {
	switch family {
	case 0:
		return m.opSyntaxError(seed)
	case 1:
		return m.opMissingParam()
	case 2:
		return m.opWrongParamType(seed)
	case 3:
		return m.opTypeMismatchedPredicate()
	case 4:
		return m.opOversizedInput(seed)
	default:
		return m.opUnknownFunction()
	}
}

// TestMalformedSender_EveryFamilyRejectedAndNoOp verifies that every malformed
// family is rejected by the engine with a typed error (never a panic, never a
// commit) and leaves node and edge counts unchanged — including atomicity on
// error for the families whose rejecting statement begins with a CREATE: the
// leading write must not survive the rejected clause.
func TestMalformedSender_EveryFamilyRejectedAndNoOp(t *testing.T) {
	m := MalformedSender{}
	seed := NewSeed(0x5EED)
	ctx := context.Background()

	for family := 0; family < malformedKindCount; family++ {
		family := family
		t.Run("family", func(t *testing.T) {
			op := forceMalformedFamily(m, seed, family)
			if op.Kind != OpMalformed {
				t.Fatalf("family %d: kind = %s, want OpMalformed", family, op.Kind)
			}

			store, err := OpenSimStore(NewSimDisk(NewSeed(1), 0), defaultSimStoreConfig())
			if err != nil {
				t.Fatalf("OpenSimStore: %v", err)
			}
			defer func() { _ = store.Close() }()
			a := NewEngineAdapter(store.Engine())

			beforeN, _ := a.NodeCount()
			beforeE, _ := a.EdgeCount()

			// A malformed op must never panic; RunWrite returning an error is the
			// expected, well-behaved outcome.
			res, runErr := a.RunWrite(ctx, op.Cypher, op.Params)
			if runErr == nil {
				// If it parsed and ran, draining must not panic either, and the op
				// must still have left counts unchanged (asserted below).
				for res.Next() {
				}
				_ = res.Err()
				_ = res.Close()
			}

			afterN, _ := a.NodeCount()
			afterE, _ := a.EdgeCount()
			if afterN != beforeN || afterE != beforeE {
				t.Fatalf("family %d (%q) mutated state: nodes %d->%d edges %d->%d",
					family, op.Cypher, beforeN, afterN, beforeE, afterE)
			}
			if runErr == nil {
				t.Fatalf("family %d (%q) was accepted; every malformed family must be rejected with a typed error",
					family, op.Cypher)
			}
		})
	}
}

// TestMalformedSender_OracleNoOp verifies that ApplyMalformed never changes the
// modelled oracle state, so a malformed op cannot drag the oracle out of sync
// with the (rejecting) engine.
func TestMalformedSender_OracleNoOp(t *testing.T) {
	o := NewGraphOracle()
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "Ada", "age": int64(36)})
	nBefore, eBefore, opsBefore := o.NodeCount(), o.EdgeCount(), len(o.Ops())

	o.ApplyMalformed("MATCH (n:Person RETURN n", nil)

	if o.NodeCount() != nBefore || o.EdgeCount() != eBefore {
		t.Fatalf("ApplyMalformed changed modelled state: nodes %d->%d edges %d->%d",
			nBefore, o.NodeCount(), eBefore, o.EdgeCount())
	}
	if len(o.Ops()) != opsBefore+1 {
		t.Fatalf("ApplyMalformed must record one history entry: ops %d->%d", opsBefore, len(o.Ops()))
	}
	last := o.Ops()[len(o.Ops())-1]
	if last.Expected.Committed || last.Expected.ErrorMsg == "" {
		t.Fatalf("ApplyMalformed must record an expected-error no-op, got %+v", last.Expected)
	}
}

// TestMalformedSender_IsWriteRoutesThroughWritePath documents that OpMalformed
// is routed through the write (atomicity-bearing) path.
func TestMalformedSender_IsWriteRoutesThroughWritePath(t *testing.T) {
	if !OpMalformed.IsWrite() {
		t.Fatal("OpMalformed must route through the write path so atomicity on error is exercised")
	}
}

// TestBadActorWorkload_RunsClean runs the bad-actor mix and asserts that the
// continuous stream of malformed ops never trips an invariant: every malformed
// op is rejected and the engine stays in lock-step with the oracle. Honest
// writers keep the graph populated so the malformed traffic runs against
// non-trivial state.
func TestBadActorWorkload_RunsClean(t *testing.T) {
	// Kept modest so the short layer stays within the per-package budget under
	// -race; the malformed stream is dense (20% of ops) so even this window
	// exercises every rejection path many times.
	runToCompletion(t, Config{
		Seed:       0xBADAC7,
		MaxTicks:   1200,
		CheckEvery: 16,
		Workload:   BadActorWorkload(NewSeed(0xBADAC7)),
	})
}

// TestBadActorWorkload_Reproducible verifies the bad-actor workload is, like the
// honest mixes, a pure function of the seed: two runs reach identical modelled
// state with no violations.
func TestBadActorWorkload_Reproducible(t *testing.T) {
	mk := func() Config {
		return Config{Seed: 99, MaxTicks: 1000, CheckEvery: 32, Workload: BadActorWorkload(NewSeed(99))}
	}
	a := runToCompletion(t, mk())
	b := runToCompletion(t, mk())
	if a.Oracle().NodeCount() != b.Oracle().NodeCount() || a.Oracle().EdgeCount() != b.Oracle().EdgeCount() {
		t.Fatalf("bad-actor workload not reproducible: a(n=%d,e=%d) b(n=%d,e=%d)",
			a.Oracle().NodeCount(), a.Oracle().EdgeCount(), b.Oracle().NodeCount(), b.Oracle().EdgeCount())
	}
}
