package sim

import (
	"context"
	"errors"
	"testing"
)

// ============================================================================
// Round-6 harness-fidelity audit (READ-ONLY; these are NEW tests only).
//
// Goal: prove the DST oracle/checker WOULD catch an injected ACID violation,
// i.e. that the checker is not a no-op / not comparing the engine to itself in a
// vacuous way. The technique is mutation testing of the harness: feed the
// checker a deliberately WRONG engine and confirm the expected typed Violation
// fires. Also probe the cross-layer write+recover path through the real engine.
// ============================================================================

// fakeResult is a canned single-scalar Result for driving the checker without a
// real engine. It returns one row carrying `scalar`, or zero rows when empty.
type fakeResult struct {
	scalar   int64
	emitted  bool
	rows     int
	empty    bool
	closeErr error
}

func (r *fakeResult) Next() bool {
	if r.empty || r.emitted {
		return false
	}
	r.emitted = true
	r.rows++
	return true
}
func (r *fakeResult) ScalarInt() (int64, bool)    { return r.scalar, !r.empty }
func (r *fakeResult) IntAt(int) (int64, bool)     { return r.scalar, !r.empty }
func (r *fakeResult) StringAt(int) (string, bool) { return "", false }
func (r *fakeResult) RowCount() int               { return r.rows }
func (r *fakeResult) Err() error                  { return nil }
func (r *fakeResult) Close() error                { return r.closeErr }

// wrongEngine is a deliberately-buggy Engine: it reports a fixed node/edge count
// and answers EVERY existence/count probe with `probeAnswer`. By setting
// probeAnswer=0 we model an engine that LOST a committed datum (durability bug);
// by setting nodeCount/edgeCount off the oracle we model a count divergence.
type wrongEngine struct {
	nodeCount, edgeCount int64
	probeAnswer          int64
	runErr               error
}

func (e *wrongEngine) Run(_ context.Context, _ string, _ map[string]any) (Result, error) {
	if e.runErr != nil {
		return nil, e.runErr
	}
	return &fakeResult{scalar: e.probeAnswer, empty: false}, nil
}
func (e *wrongEngine) NodeCount() (int64, error) { return e.nodeCount, nil }
func (e *wrongEngine) EdgeCount() (int64, error) { return e.edgeCount, nil }

// buildOracleWithData seeds an oracle with n Persons and one KNOWS edge so the
// checker has both nodes and an edge to probe.
func buildOracleWithData(t *testing.T) *GraphOracle {
	t.Helper()
	o := NewGraphOracle()
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "alice", "age": int64(30)})
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(40)})
	o.ApplyCreate(tmplCreateKnows, map[string]any{"a": "alice", "b": "bob"})
	if o.NodeCount() != 2 || o.EdgeCount() != 1 {
		t.Fatalf("oracle seed: nodes=%d edges=%d, want 2,1", o.NodeCount(), o.EdgeCount())
	}
	return o
}

func r6HasKind(vs []Violation, k ViolationKind) bool {
	for _, v := range vs {
		if v.Kind == k {
			return true
		}
	}
	return false
}

// TestFidelity_DurabilityChecker_CatchesLostCommittedNode is the headline
// mutation test: the oracle models 2 committed nodes but the engine reports 0
// and answers every existence probe with 0 (it "lost" everything across
// recovery). CheckDurability MUST flag a Durability violation. If it returned
// nil here, the durability checker would be vacuous.
func TestFidelity_DurabilityChecker_CatchesLostCommittedNode(t *testing.T) {
	o := buildOracleWithData(t)
	chk := NewInvariantChecker(NewSeed(1))

	// Engine that lost all data: counts 0, probes return 0.
	eng := &wrongEngine{nodeCount: 0, edgeCount: 0, probeAnswer: 0}
	vs := chk.CheckDurability(99, o, eng)
	if len(vs) == 0 {
		t.Fatal("FIDELITY GAP: CheckDurability returned NO violation for an engine that lost every committed node/edge")
	}
	if !r6HasKind(vs, ViolationACIDDurability) {
		t.Fatalf("expected a ViolationACIDDurability, got %v", vs)
	}
	t.Logf("VERIFIED: durability checker caught lost-commit; violations=%d first=%q", len(vs), vs[0].String())
}

// TestFidelity_DurabilityChecker_CatchesLeakedUncommitted models the atomicity
// side of the crash boundary: the oracle models 2 committed nodes but the engine
// recovered 3 (uncommitted state leaked in). CheckDurability must flag this as an
// Atomicity violation (a surplus over the oracle).
func TestFidelity_DurabilityChecker_CatchesLeakedUncommitted(t *testing.T) {
	o := buildOracleWithData(t)
	chk := NewInvariantChecker(NewSeed(2))
	// Counts above oracle (3 nodes vs 2), probes succeed so only the count check fires.
	eng := &wrongEngine{nodeCount: 3, edgeCount: 1, probeAnswer: 1}
	vs := chk.CheckDurability(7, o, eng)
	if !r6HasKind(vs, ViolationACIDAtomicity) {
		t.Fatalf("FIDELITY GAP: expected ViolationACIDAtomicity for leaked uncommitted state, got %v", vs)
	}
	t.Logf("VERIFIED: durability checker caught leaked-uncommitted (atomicity); %q", vs[0].String())
}

// TestFidelity_Checker_CatchesGhostEdge: oracle has the right counts but the
// engine returns 0 for the edge-existence probe (a committed edge is missing
// even though counts happen to match — e.g. a ghost edge of a different pair
// inflated the count). The sampled-edge check must flag GRAPH_INTEGRITY.
func TestFidelity_Checker_CatchesGhostEdge(t *testing.T) {
	o := buildOracleWithData(t)
	chk := NewInvariantChecker(NewSeed(3))
	// Counts agree (2 nodes, 1 edge) but EVERY probe answers 0 -> nodes & edge "absent".
	eng := &wrongEngine{nodeCount: 2, edgeCount: 1, probeAnswer: 0}
	vs := chk.Check(5, o, eng)
	if !r6HasKind(vs, ViolationGraphIntegrity) {
		t.Fatalf("FIDELITY GAP: expected GRAPH_INTEGRITY when committed node/edge absent despite matching counts, got %v", vs)
	}
	t.Logf("VERIFIED: sampled existence check caught absent committed datum under matching counts; %q", vs[0].String())
}

// TestFidelity_Checker_CatchesProbeError verifies a probe error is surfaced (not
// swallowed) as ORACLE_DEVIATION rather than silently passing.
func TestFidelity_Checker_CatchesProbeError(t *testing.T) {
	o := buildOracleWithData(t)
	chk := NewInvariantChecker(NewSeed(4))
	eng := &wrongEngine{nodeCount: 2, edgeCount: 1, runErr: errors.New("boom")}
	vs := chk.Check(1, o, eng)
	if !r6HasKind(vs, ViolationOracleDeviation) {
		t.Fatalf("FIDELITY GAP: expected ORACLE_DEVIATION when a probe errors, got %v", vs)
	}
	t.Logf("VERIFIED: probe error surfaced as ORACLE_DEVIATION; %q", vs[0].String())
}

// TestFidelity_Checker_CleanPasses is the negative control: a correct engine
// (counts match, every probe returns 1) must produce ZERO violations. If this
// failed, the checker would be reporting false positives.
func TestFidelity_Checker_CleanPasses(t *testing.T) {
	o := buildOracleWithData(t)
	chk := NewInvariantChecker(NewSeed(5))
	eng := &wrongEngine{nodeCount: 2, edgeCount: 1, probeAnswer: 1}
	if vs := chk.Check(1, o, eng); len(vs) != 0 {
		t.Fatalf("false positive: correct engine produced violations %v", vs)
	}
	if vs := chk.CheckDurability(1, o, eng); len(vs) != 0 {
		t.Fatalf("false positive: correct engine produced durability violations %v", vs)
	}
	t.Log("VERIFIED: negative control — correct engine yields zero violations")
}
