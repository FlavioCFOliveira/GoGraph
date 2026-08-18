package sim

// counters_oracle_test.go — unit gate for the per-op QueryCounters oracle
// (rmp #2448).
//
// The happy-path test drives a scripted op sequence against the REAL engine and
// asserts the check accepts the engine's own counters at every step — which is
// also the empirical validation that expectedOpCounters models the engine's
// pinned counter semantics (cypher/query_counters_test.go) for every workload
// template. The sensitivity tests prove the check can fire: a perturbed
// counters value, a nil report on a committed write, and a non-nil report on a
// pure read each produce a violation, so a clean scenario run is evidence, not
// a tautology.
//
// Layer: short.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// newCountersSim builds an in-memory simulator whose engine and oracle the
// tests drive directly (the tick loop is not used, so MaxTicks is irrelevant).
func newCountersSim(t *testing.T) *Simulator {
	t.Helper()
	sm, err := New(Config{Seed: 0xC0DE, MaxTicks: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	return sm
}

// runStep executes op against the simulator's engine, asserts it committed,
// runs the counters check on the pre-apply oracle, and then advances the
// oracle — exactly the tick loop's ordering.
func runStep(t *testing.T, sm *Simulator, step string, op Op) {
	t.Helper()
	committed, counters := sm.executeCounted(context.Background(), op)
	if !committed {
		t.Fatalf("%s: op %q did not commit", step, op.Cypher)
	}
	if vs := CheckOpCounters(1, op, committed, counters, sm.oracle); len(vs) > 0 {
		t.Fatalf("%s: counters check rejected the engine's own report: %v", step, vs)
	}
	sm.applyToOracle(op, committed)
}

// TestCheckOpCounters_HappyPathScriptedSequence walks the scripted sequence the
// task mandates — CREATE node, CREATE edge (plus its duplicate no-op), SET
// property, MERGE create + MERGE match, SET/REMOVE label, REMOVE absent
// property, SET += map, SET = map, MERGE-relationship create + match, and
// DETACH DELETE (hit and miss) — asserting the oracle accepts the engine's
// correct counters at every step.
func TestCheckOpCounters_HappyPathScriptedSequence(t *testing.T) {
	t.Parallel()
	sm := newCountersSim(t)

	steps := []struct {
		name string
		op   Op
	}{
		{"create Alice", Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": "Alice-1", "age": int64(30)}}},
		{"create Bob", Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": "Bob-1", "age": int64(40)}}},
		{"create edge Alice->Bob", Op{Kind: OpCreate, Cypher: tmplCreateKnows,
			Params: map[string]any{"a": "Alice-1", "b": "Bob-1"}}},
		{"edge CREATE with a missing endpoint applies nothing", Op{Kind: OpCreate, Cypher: tmplCreateKnows,
			Params: map[string]any{"a": "Alice-1", "b": "Ghost-0"}}},
		{"set property", Op{Kind: OpUpdate, Cypher: tmplSetAge,
			Params: map[string]any{"name": "Alice-1", "age": int64(31)}}},
		{"MERGE that creates", Op{Kind: OpMerge, Cypher: tmplMergePerson,
			Params: map[string]any{"name": "Zed"}}},
		{"MERGE that matches applies nothing", Op{Kind: OpMerge, Cypher: tmplMergePerson,
			Params: map[string]any{"name": "Zed"}}},
		{"add label", Op{Kind: OpUpdate, Cypher: tmplAddVip,
			Params: map[string]any{"name": "Alice-1"}}},
		{"re-add carried label counts nothing", Op{Kind: OpUpdate, Cypher: tmplAddVip,
			Params: map[string]any{"name": "Alice-1"}}},
		{"remove label", Op{Kind: OpUpdate, Cypher: tmplRemoveVip,
			Params: map[string]any{"name": "Alice-1"}}},
		{"remove absent property counts nothing", Op{Kind: OpUpdate, Cypher: tmplRemoveTag,
			Params: map[string]any{"name": "Alice-1"}}},
		{"set tag", Op{Kind: OpUpdate, Cypher: tmplSetTag,
			Params: map[string]any{"name": "Alice-1", "tag": "t7"}}},
		{"SET += map", Op{Kind: OpUpdate, Cypher: tmplMergeProps,
			Params: map[string]any{"name": "Alice-1", "props": map[string]any{"score": int64(9)}}}},
		// Alice now carries {name, age, tag, score}: the replace must report
		// -properties 4 and +properties 2.
		{"SET = map replaces the property set", Op{Kind: OpUpdate, Cypher: tmplReplaceProps,
			Params: map[string]any{"name": "Alice-1", "props": map[string]any{"name": "Alice-1", "age": int64(31)}}}},
		{"MERGE relationship that creates", Op{Kind: OpMerge, Cypher: tmplMergeKnowsN,
			Params: map[string]any{"a": "Bob-1", "b": "Zed"}}},
		{"MERGE relationship that matches increments", Op{Kind: OpMerge, Cypher: tmplMergeKnowsN,
			Params: map[string]any{"a": "Bob-1", "b": "Zed"}}},
		// Alice has one incident edge (Alice->Bob): -nodes 1, -relationships 1.
		{"DETACH DELETE with an incident edge", Op{Kind: OpDelete, Cypher: tmplDetachDelete,
			Params: map[string]any{"name": "Alice-1"}}},
		{"DETACH DELETE miss applies nothing", Op{Kind: OpDelete, Cypher: tmplDetachDelete,
			Params: map[string]any{"name": "Alice-1"}}},
		{"pure read reports nil counters", Op{Kind: OpMatch,
			Cypher: "MATCH (n:Person) RETURN n.name, n.age LIMIT 10"}},
	}
	for _, s := range steps {
		runStep(t, sm, s.name, s.op)

		if s.name == "create edge Alice->Bob" {
			// A duplicate CREATE of the same simple-graph edge is REJECTED by the
			// engine (openCypher parallel-edge semantics need a multigraph), so it
			// reaches the check as an uncommitted op and is skipped — the oracle
			// stays frozen and no violation fires.
			dup := Op{Kind: OpCreate, Cypher: tmplCreateKnows,
				Params: map[string]any{"a": "Alice-1", "b": "Bob-1"}}
			committed, counters := sm.executeCounted(context.Background(), dup)
			if committed {
				t.Fatal("duplicate simple-graph edge CREATE unexpectedly committed")
			}
			if vs := CheckOpCounters(1, dup, committed, counters, sm.oracle); len(vs) > 0 {
				t.Fatalf("rejected duplicate edge CREATE fired the counters check: %v", vs)
			}
		}
	}
}

// TestCheckOpCounters_PerturbedCountersFire is the mandated sensitivity proof:
// the same committed op whose real counters the check accepts must be rejected
// once a single field of the report is perturbed, with a violation naming the
// field and both values.
func TestCheckOpCounters_PerturbedCountersFire(t *testing.T) {
	t.Parallel()
	sm := newCountersSim(t)
	op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": "Ada-9", "age": int64(50)}}

	committed, counters := sm.executeCounted(context.Background(), op)
	if !committed || counters == nil {
		t.Fatalf("seed op did not commit with counters (committed=%v counters=%v)", committed, counters)
	}
	if vs := CheckOpCounters(1, op, true, counters, sm.oracle); len(vs) > 0 {
		t.Fatalf("check rejected the engine's genuine counters: %v", vs)
	}

	perturbed := *counters
	perturbed.NodesCreated++ // a wrong effect report the check must catch
	vs := CheckOpCounters(1, op, true, &perturbed, sm.oracle)
	if len(vs) != 1 {
		t.Fatalf("perturbed counters produced %d violations, want 1: %v", len(vs), vs)
	}
	if vs[0].Kind != ViolationOracleDeviation {
		t.Errorf("violation kind = %s, want %s", vs[0].Kind, ViolationOracleDeviation)
	}
	msg := vs[0].Message
	if !strings.Contains(msg, "NodesCreated") || !strings.Contains(msg, "want=1") || !strings.Contains(msg, "got=2") {
		t.Errorf("violation message does not name the field with expected vs got: %q", msg)
	}
}

// TestCheckOpCounters_NilReportOnCommittedWriteFires proves the check rejects a
// committed write that reports no counters at all — a write statement must
// carry its (possibly all-zero) effect set.
func TestCheckOpCounters_NilReportOnCommittedWriteFires(t *testing.T) {
	t.Parallel()
	oracle := NewGraphOracle()
	op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": "Ada-9", "age": int64(50)}}
	vs := CheckOpCounters(1, op, true, nil, oracle)
	if len(vs) != 1 || vs[0].Kind != ViolationOracleDeviation {
		t.Fatalf("nil counters on a committed write: got %v, want one ORACLE_DEVIATION", vs)
	}
	if !strings.Contains(vs[0].Message, "nil counters") {
		t.Errorf("violation message does not name the nil report: %q", vs[0].Message)
	}
}

// TestCheckOpCounters_ReadReportingCountersFires proves the check enforces the
// engine's read contract: a pure read must report NIL counters, and even an
// all-zero non-nil report is a deviation (it claims a write surface a MATCH
// does not have).
func TestCheckOpCounters_ReadReportingCountersFires(t *testing.T) {
	t.Parallel()
	oracle := NewGraphOracle()
	op := Op{Kind: OpMatch, Cypher: "MATCH (n:Person) RETURN n.name, n.age LIMIT 10"}
	vs := CheckOpCounters(1, op, true, &exec.QueryCounters{}, oracle)
	if len(vs) != 1 || vs[0].Kind != ViolationOracleDeviation {
		t.Fatalf("read with non-nil counters: got %v, want one ORACLE_DEVIATION", vs)
	}
	if vs := CheckOpCounters(1, op, true, nil, oracle); len(vs) != 0 {
		t.Fatalf("read with nil counters must pass, got %v", vs)
	}
}

// TestCheckOpCounters_SkipsUncommittedAndUnmodelled pins the check's two
// documented skips: an op the engine did not commit (the failed statement's own
// counters are not part of the engine contract, and the oracle stays frozen),
// and a committed write of a template the oracle does not model exactly.
func TestCheckOpCounters_SkipsUncommittedAndUnmodelled(t *testing.T) {
	t.Parallel()
	oracle := NewGraphOracle()

	// Uncommitted: even a wildly wrong report must not fire — nothing about the
	// rolled-back statement's own counters is pinned.
	failed := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": "Ada-9", "age": int64(50)}}
	if vs := CheckOpCounters(1, failed, false, &exec.QueryCounters{NodesCreated: 99}, oracle); len(vs) != 0 {
		t.Fatalf("uncommitted op fired the counters check: %v", vs)
	}

	// Unmodelled template: skipped rather than asserted vacuously.
	unmodelled := Op{Kind: OpCreate, Cypher: "CREATE (n:Widget {id:$id})",
		Params: map[string]any{"id": int64(1)}}
	if vs := CheckOpCounters(1, unmodelled, true, &exec.QueryCounters{NodesCreated: 99}, oracle); len(vs) != 0 {
		t.Fatalf("unmodelled template fired the counters check: %v", vs)
	}
}

// TestCheckOpCounters_WrongExpectationFires drives the "deliberately wrong
// expectation" direction of the sensitivity proof: the counters of a MERGE that
// CREATED are checked against an oracle that already models the name, so the
// derived expectation is the all-zero matched effect and the genuine created
// report must be rejected — created-vs-matched adjudication is live, not a
// tautology.
func TestCheckOpCounters_WrongExpectationFires(t *testing.T) {
	t.Parallel()
	sm := newCountersSim(t)
	op := Op{Kind: OpMerge, Cypher: tmplMergePerson, Params: map[string]any{"name": "Grace"}}

	committed, counters := sm.executeCounted(context.Background(), op)
	if !committed || counters == nil {
		t.Fatalf("MERGE did not commit with counters (committed=%v counters=%v)", committed, counters)
	}
	// Wrong world: pretend the oracle had already seen the name, so the check
	// expects a zero-effect match while the engine truthfully reports a create.
	sm.oracle.ApplyMerge(op.Cypher, op.Params)
	vs := CheckOpCounters(1, op, true, counters, sm.oracle)
	if len(vs) != 1 || vs[0].Kind != ViolationOracleDeviation {
		t.Fatalf("wrong expectation did not fire exactly once: %v", vs)
	}
	if !strings.Contains(vs[0].Message, "NodesCreated") {
		t.Errorf("violation message does not name the disagreeing field: %q", vs[0].Message)
	}
}
