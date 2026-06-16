package sim

import (
	"context"
	"testing"
)

// TestRecordTrace_ReplayReachesIdenticalEndState records a clean deterministic
// run, replays the recorded trace via the scripted executor, and asserts the
// replay reaches the identical engine/oracle end-state and finds no violation.
func TestRecordTrace_ReplayReachesIdenticalEndState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Seed: 12345, MaxTicks: 400, Workload: WriteHeavyWorkload(NewSeed(12345))}

	trace, report, err := RecordTrace(ctx, cfg)
	if err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if report != nil {
		t.Fatalf("recording run unexpectedly failed:\n%s", report)
	}
	if trace.Len() != cfg.MaxTicks {
		t.Fatalf("recorded %d ops, want %d", trace.Len(), cfg.MaxTicks)
	}

	res, err := ReplayTrace(ctx, trace)
	if err != nil {
		t.Fatalf("ReplayTrace: %v", err)
	}
	if res.Violated() {
		t.Fatalf("clean trace replayed to a violation:\n%s", res.Report)
	}
	// The replay's engine end-state must equal its own oracle end-state (the
	// scripted executor keeps them in lock-step), proving replay is faithful.
	if res.NodeCount != int64(res.OracleN) || res.EdgeCount != int64(res.OracleE) {
		t.Fatalf("replay end-state mismatch: engine n=%d e=%d oracle n=%d e=%d",
			res.NodeCount, res.EdgeCount, res.OracleN, res.OracleE)
	}
}

// TestRecordTrace_ReplayIsDeterministic replays the same trace twice and asserts
// both runs reach byte-identical end-state counts — recording + replay add no
// nondeterminism.
func TestRecordTrace_ReplayIsDeterministic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Seed: 999, MaxTicks: 300, Workload: DefaultWorkload(NewSeed(999))}

	trace, _, err := RecordTrace(ctx, cfg)
	if err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	a, err := ReplayTrace(ctx, trace)
	if err != nil {
		t.Fatalf("ReplayTrace a: %v", err)
	}
	b, err := ReplayTrace(ctx, trace)
	if err != nil {
		t.Fatalf("ReplayTrace b: %v", err)
	}
	if a.NodeCount != b.NodeCount || a.EdgeCount != b.EdgeCount {
		t.Fatalf("replay not deterministic: a(n=%d e=%d) b(n=%d e=%d)", a.NodeCount, a.EdgeCount, b.NodeCount, b.EdgeCount)
	}
}

// TestReplayTrace_RetriggersInjectedViolation injects a FaultDropEngineWrite on
// a CREATE op in an otherwise-clean trace and asserts the scripted replay
// re-triggers the divergence as a violation.
func TestReplayTrace_RetriggersInjectedViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A minimal hand-built trace: create two people, then a create whose engine
	// write is dropped (lost-write fault) — the oracle counts 3, the engine 2.
	trace := Trace{
		Seed: 1,
		Ops: []TracedOp{
			{Op: Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "Ada", "age": int64(1)}}},
			{Op: Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "Alan", "age": int64(2)}}},
			{
				Op:    Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "Grace", "age": int64(3)}},
				Fault: FaultDropEngineWrite,
			},
		},
	}

	res, err := ReplayTrace(ctx, trace)
	if err != nil {
		t.Fatalf("ReplayTrace: %v", err)
	}
	if !res.Violated() {
		t.Fatal("expected the injected lost-write fault to trigger a violation")
	}
	if res.NodeCount != 2 || res.OracleN != 3 {
		t.Fatalf("expected engine=2 oracle=3 after dropped write, got engine=%d oracle=%d", res.NodeCount, res.OracleN)
	}
}

// TestReplayInstructions_RendersOps verifies the replay-instructions rendering
// includes the seed, op count, and a fault marker.
func TestReplayInstructions_RendersOps(t *testing.T) {
	t.Parallel()
	trace := Trace{
		Seed: 42,
		Ops: []TracedOp{
			{Op: Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "Ada"}}, Fault: FaultDropEngineWrite},
		},
	}
	s := ReplayInstructions(trace)
	if !contains(s, "seed=42") || !contains(s, "FAULT:drop-engine-write") {
		t.Fatalf("instructions missing expected content:\n%s", s)
	}
}

// contains is a tiny substring helper local to the trace test.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
