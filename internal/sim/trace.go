package sim

import (
	"context"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TracedOp is one entry in a recorded [Trace]: the tick it ran at, the operation
// issued, and an optional injected fault marker. A trace is the ordered list of
// these, captured during a deterministic run, and is the unit a scripted replay
// executes and the shrinker reduces.
type TracedOp struct {
	// Tick is the simulated tick the op ran at during recording. On replay the
	// scripted executor re-derives ticks positionally, so this is informational
	// (it lets a report point at the original tick).
	Tick int64
	// Op is the Cypher operation that was issued.
	Op Op
	// Fault, when non-empty, is a marker injected for replay/shrinking: the
	// scripted executor applies the named fault deterministically when it reaches
	// this op. The empty string means no fault (a normal op). See [TraceFault].
	Fault TraceFault
}

// TraceFault names a deterministic fault a scripted replay injects at a specific
// op, so a trace can carry a reproducible failure for replay verification and
// shrinking. Faults are a test-only mechanism of the scripted executor; a
// recording from a real run carries [FaultNone] on every op unless a fault is
// explicitly injected.
type TraceFault string

// Trace faults.
const (
	// FaultNone is the absence of an injected fault (a normal op).
	FaultNone TraceFault = ""
	// FaultDropEngineWrite makes the scripted executor APPLY a write to the oracle
	// but SKIP it on the engine, creating a deterministic oracle-vs-engine
	// divergence the per-op check detects. It models a lost-write bug and is the
	// canonical injected violation the shrinking demo reduces.
	FaultDropEngineWrite TraceFault = "drop-engine-write"
)

// Trace is the full recorded operation stream of a deterministic run, plus a
// note of which crash ticks fired. Only the deterministic engine-API mode
// produces a Trace: it is bit-reproducible, so replaying the same Trace against
// a fresh engine/oracle/checker reaches the identical end-state. Concurrent and
// liveness modes are not bit-replayable and are not recorded.
//
// # Concurrency contract
//
// A Trace is a plain value; callers own their copies and do not share them
// across goroutines mid-mutation.
type Trace struct {
	// Seed is the seed the recording was driven by (informational; a scripted
	// replay does NOT draw from it — it executes Ops directly).
	Seed uint64
	// Ops is the ordered operation stream.
	Ops []TracedOp
	// CrashTicks lists the ticks at which a crash+recovery cycle fired during
	// recording, in order. Scripted replay runs against a plain in-memory engine
	// and does not re-inject crashes (the violations the shrinker targets are
	// oracle/engine divergences reproducible without a crash); the list is
	// retained for the report and for completeness.
	CrashTicks []int64
}

// Len returns the number of operations in the trace.
func (t Trace) Len() int { return len(t.Ops) }

// withOps returns a copy of the trace carrying a different op slice, preserving
// the seed and crash-tick metadata. The shrinker uses it to build candidate
// sub-traces without mutating the original.
func (t Trace) withOps(ops []TracedOp) Trace {
	return Trace{Seed: t.Seed, Ops: ops, CrashTicks: t.CrashTicks}
}

// RecordTrace runs a deterministic engine-API simulation under cfg and captures
// the full ordered op stream (plus any crash ticks) into a [Trace], alongside
// the run's report (nil when the run passed). Recording adds no nondeterminism:
// it only observes the op stream the simulator already produces from the seed,
// so the recorded run behaves identically to an unrecorded one and the returned
// Trace replays (via [ReplayTrace]) to the same end-state.
//
// cfg.OnOp and cfg.OnCrash are overridden by the recorder; any caller-supplied
// hooks are chained AFTER the recording hook so verbose tracing still works.
func RecordTrace(ctx context.Context, cfg Config) (Trace, *SimReport, error) {
	tr := Trace{Seed: cfg.Seed}
	userOnOp := cfg.OnOp
	userOnCrash := cfg.OnCrash

	cfg.OnOp = func(tick int64, op Op) {
		tr.Ops = append(tr.Ops, TracedOp{Tick: tick, Op: op})
		if userOnOp != nil {
			userOnOp(tick, op)
		}
	}
	cfg.OnCrash = func(tick int64, replayedWALOps int) {
		tr.CrashTicks = append(tr.CrashTicks, tick)
		if userOnCrash != nil {
			userOnCrash(tick, replayedWALOps)
		}
	}

	sm, err := New(cfg)
	if err != nil {
		return tr, nil, fmt.Errorf("sim: RecordTrace new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	report, err := sm.Run(ctx)
	if err != nil {
		return tr, nil, fmt.Errorf("sim: RecordTrace run: %w", err)
	}
	return tr, report, nil
}

// ScriptedResult is the outcome of a scripted replay: the report (nil when the
// replay found no violation) and the engine/oracle end-state counts, so a caller
// can assert two runs reach the identical end-state.
type ScriptedResult struct {
	Report    *SimReport
	NodeCount int64
	EdgeCount int64
	OracleN   int
	OracleE   int
}

// Violated reports whether the scripted replay detected a violation.
func (r ScriptedResult) Violated() bool { return r.Report != nil }

// ReplayTrace executes a recorded [Trace] against a FRESH plain in-memory
// engine, oracle, and checker — WITHOUT drawing from any seed — applying each op
// in order and checking invariants after every op exactly as the deterministic
// safety loop does. It is the foundation for both exact failure replay and
// shrinking: because the deterministic engine-API mode is a pure function of its
// op stream, replaying the stream reproduces the same end-state and re-triggers
// the same violation.
//
// An op carrying an injected [TraceFault] is applied with that fault (e.g.
// [FaultDropEngineWrite] applies the write to the oracle but skips the engine,
// producing a deterministic divergence). A trace with no faults replays cleanly
// iff the original run did.
//
// ReplayTrace spawns no goroutines and is a pure function of trace.
func ReplayTrace(ctx context.Context, trace Trace) (ScriptedResult, error) {
	x := newScriptedExecutor()
	defer x.close()

	for i, top := range trace.Ops {
		if err := ctx.Err(); err != nil {
			return ScriptedResult{}, err
		}
		tick := int64(i + 1)
		if report := x.step(ctx, tick, top); report != nil {
			return ScriptedResult{
				Report:    report,
				NodeCount: x.engineNodeCount(),
				EdgeCount: x.engineEdgeCount(),
				OracleN:   x.oracle.NodeCount(),
				OracleE:   x.oracle.EdgeCount(),
			}, nil
		}
	}
	return ScriptedResult{
		NodeCount: x.engineNodeCount(),
		EdgeCount: x.engineEdgeCount(),
		OracleN:   x.oracle.NodeCount(),
		OracleE:   x.oracle.EdgeCount(),
	}, nil
}

// scriptedExecutor holds the fresh engine/oracle/checker a scripted replay drives.
// It mirrors the deterministic Simulator's execute+apply+check pipeline but feeds
// ops from a trace instead of generating them from a seed.
type scriptedExecutor struct {
	engine  *EngineAdapter
	oracle  *GraphOracle
	checker *InvariantChecker
}

// newScriptedExecutor builds a scripted executor over a fresh in-memory engine,
// matching the deterministic Simulator's non-crash engine shape (directed
// simple graph) so its end-state is directly comparable to a recorded run.
func newScriptedExecutor() *scriptedExecutor {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return &scriptedExecutor{
		engine: NewEngineAdapter(cypher.NewEngine(g)),
		oracle: NewGraphOracle(),
		// The checker draws only for sampling; a fixed seed keeps replay
		// deterministic and independent of the recorded run's seed.
		checker: NewInvariantChecker(NewSeed(0)),
	}
}

// step runs one traced op against the engine, applies it to the oracle (honouring
// any injected fault), then checks invariants. It returns a report on the first
// violation, else nil.
func (x *scriptedExecutor) step(ctx context.Context, tick int64, top TracedOp) *SimReport {
	op := top.Op
	committed := x.execute(ctx, op, top.Fault)
	x.applyToOracle(op, committed, top.Fault)

	if violations := x.checker.Check(tick, x.oracle, x.engine); len(violations) > 0 {
		return &SimReport{
			Seed:       0,
			FailedTick: tick,
			FailedOp:   op,
			Violations: violations,
			OracleState: OracleSnapshot{
				NodeCount: x.oracle.NodeCount(),
				EdgeCount: x.oracle.EdgeCount(),
				OpCount:   len(x.oracle.Ops()),
			},
		}
	}
	return nil
}

// execute runs op against the engine unless an injected fault suppresses it.
// FaultDropEngineWrite skips the engine write entirely (modelling a lost write).
func (x *scriptedExecutor) execute(ctx context.Context, op Op, fault TraceFault) bool {
	if fault == FaultDropEngineWrite {
		// Suppress the engine write: the oracle will still model it, producing a
		// deterministic divergence the checker catches.
		return true
	}
	var (
		res Result
		err error
	)
	if op.Kind.IsWrite() {
		res, err = x.engine.RunWrite(ctx, op.Cypher, op.Params)
	} else {
		res, err = x.engine.Run(ctx, op.Cypher, op.Params)
	}
	if err != nil {
		return false
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil
}

// applyToOracle advances the oracle for op exactly as the deterministic loop
// does. An injected FaultDropEngineWrite still applies the op to the oracle, so
// the oracle diverges from the (un-written) engine.
func (x *scriptedExecutor) applyToOracle(op Op, committed bool, _ TraceFault) {
	switch op.Kind {
	case OpCreate:
		if committed {
			x.oracle.ApplyCreate(op.Cypher, op.Params)
		}
	case OpMerge:
		if committed {
			x.oracle.ApplyMerge(op.Cypher, op.Params)
		}
	case OpDelete:
		if committed {
			x.oracle.ApplyDelete(op.Cypher, op.Params)
		}
	case OpUpdate:
		if committed {
			x.oracle.ApplyMatch(op.Cypher, op.Params)
		}
	case OpMatch:
		x.oracle.ApplyMatch(op.Cypher, op.Params)
	case OpMalformed:
		x.oracle.ApplyMalformed(op.Cypher, op.Params)
	}
}

// engineNodeCount returns the engine's live node count (0 on a probe error,
// which a scripted replay never expects on a healthy engine).
func (x *scriptedExecutor) engineNodeCount() int64 {
	n, _ := x.engine.NodeCount()
	return n
}

// engineEdgeCount returns the engine's live edge count.
func (x *scriptedExecutor) engineEdgeCount() int64 {
	n, _ := x.engine.EdgeCount()
	return n
}

// close releases the scripted executor's resources. The plain in-memory engine
// owns no durable handle, so this is currently a no-op kept for symmetry and
// future-proofing.
func (x *scriptedExecutor) close() {}

// ReplayInstructions renders a human-readable, copy-pasteable description of a
// (possibly shrunk) trace: the seed it came from and the ordered op list, so a
// failure can be reproduced and inspected. It is included in the SimReport for a
// shrunk reproducer.
func ReplayInstructions(trace Trace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trace (seed=%d, ops=%d", trace.Seed, len(trace.Ops))
	if len(trace.CrashTicks) > 0 {
		fmt.Fprintf(&b, ", crashTicks=%v", trace.CrashTicks)
	}
	b.WriteString("):\n")
	for i, top := range trace.Ops {
		fault := ""
		if top.Fault != FaultNone {
			fault = fmt.Sprintf(" [FAULT:%s]", top.Fault)
		}
		fmt.Fprintf(&b, "  %3d. %s %q %v%s\n", i, top.Op.Kind, top.Op.Cypher, top.Op.Params, fault)
	}
	return b.String()
}
