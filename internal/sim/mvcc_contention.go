package sim

// mvcc_contention.go — the deterministic lost-update / write-conflict
// adjudication scenario (rmp #2437).
//
// Under MVCC the ONLY acceptable outcomes of a contended read-modify-write
// are: the commit applies exactly once, or the transaction fails with the
// typed retriable serialization conflict and applies NOTHING. Sessions here
// collide deliberately on a small shared counter key space — each transaction
// reads a counter through its own snapshot, writes back read+1 (the classic
// lost-update shape: a stale read plus a blind write), and also writes a
// DISJOINT per-session control key that must never conflict on its own. The
// adjudication is exact, at transaction granularity:
//
//   - every counter's final value equals the increments the engine ACKED for
//     it (a shortfall is a LOST UPDATE — a conflicting write silently
//     dropped; an excess is a PHANTOM APPLY — a refused transaction that left
//     a trace);
//   - every conflicted transaction left no trace on its CONTROL key either
//     (atomicity of the refusal, observable on the uncontended write);
//   - every refusal matches [cypher.ErrSerializationConflict] with
//     [errors.Is] — anything else is a hard fault, because a client can only
//     retry what is typed as retriable.
//
// Like the multi-session mode it runs over the WAL-backed SimDisk store, on
// one goroutine, interleaved at statement granularity by the seeded
// scheduler; the whole run is a pure function of the seed.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// Contention workload templates. Counters and controls are Person nodes so
// the existing store configuration and read paths apply unchanged; the
// contended property is `count`.
const (
	tmplCounterRead = "MATCH (n:Person {name:$name}) RETURN n.val"
	tmplCounterSet  = "MATCH (n:Person {name:$name}) SET n.val=$v"
)

// MVCCContentionConfig parameterises a deterministic contended-counter run.
type MVCCContentionConfig struct {
	// Seed is the master seed; scheduling and per-session choices derive from it.
	Seed uint64
	// Ticks is the number of scheduler steps (one session step per tick).
	Ticks int
	// Sessions is the number of concurrently colliding sessions (< 2
	// normalises to 2 — below that nothing contends).
	Sessions int
	// Counters is the size of the SHARED counter key space (< 1 normalises to
	// 2). Fewer counters means more collisions.
	Counters int
	// OnStep is the observation hook, as in [MVCCSessionsConfig.OnStep].
	OnStep func(tick, session int, what string)
	// OnQuiesce, when set, is called once at the DRAIN POINT: every open
	// transaction has been rolled back and no write is in flight, but the store
	// is still open and the adjudication has not yet run.
	//
	// It exists for the MVCC-substrate telemetry oracle (rmp #2470), which must
	// read [lpg.MVCCStats] at a point where in-flight commits returning to zero
	// is a fair question. It is deliberately NOT allowed to influence the run:
	// it is called after the last transaction and its observations go to the
	// caller, never into [MVCCContentionResult], because substrate telemetry is
	// scheduling-dependent and the result is compared byte for byte by the
	// determinism gate.
	OnQuiesce func(st *SimStore)
}

func (c *MVCCContentionConfig) normalise() {
	if c.Sessions < 2 {
		c.Sessions = 2
	}
	if c.Counters < 1 {
		c.Counters = 2
	}
}

// MVCCContentionResult summarises a contended-counter run; a pure function of
// the seed (the determinism gate compares results byte for byte).
type MVCCContentionResult struct {
	// Violations holds the adjudication findings; empty on a clean run.
	Violations []Violation
	// AckedIncrements is the number of COMMIT-acknowledged increments per
	// counter — the value each counter must hold at the end.
	AckedIncrements []int
	Seed            uint64
	Sessions        int
	Counters        int
	// TxCommitted / TxConflicted count finished write transactions by outcome.
	TxCommitted  int
	TxConflicted int
	// TypedConflicts counts refusals that matched cypher.ErrSerializationConflict
	// via errors.Is. Every refusal must be typed, so on a clean run
	// TypedConflicts == TxConflicted; an untyped refusal is a hard error, not a
	// counter.
	TypedConflicts int
}

// Clean reports whether the run finished with no violations.
func (r *MVCCContentionResult) Clean() bool { return len(r.Violations) == 0 }

// contSession is one colliding session's state machine.
type contSession struct {
	sess *cypher.Session
	tx   *cypher.ExplicitTx
	rng  *Seed
	id   int
	// step is the phase inside the open transaction: 0 = read the counter,
	// 1 = write counter+1, 2 = write the control key, 3 = commit.
	step int
	// counter is the shared counter this transaction picked; readValue is what
	// it read through its own snapshot.
	counter   int
	readValue int64
	// controlValue is the value this transaction wrote to its own control key.
	controlValue int64
	// writes counts this session's control writes, for unique control values.
	writes int
}

// contHarness bundles a run's live pieces.
type contHarness struct {
	store *SimStore
	res   *MVCCContentionResult
	// expectedControl is, per session, the control value of the LAST
	// acknowledged transaction — what the control key must hold at the end.
	expectedControl []int64
	sessions        []*contSession
	step            string
}

// RunMVCCContention executes the deterministic contended-counter scenario and
// returns its result. Determinism contract as [RunMVCCSessions]: same config,
// identical result.
//
// # Concurrency contract
//
// Runs entirely on the calling goroutine; "concurrent" transactions are
// interleaved deterministically, never parallel.
func RunMVCCContention(ctx context.Context, cfg MVCCContentionConfig) (*MVCCContentionResult, error) {
	cfg.normalise()
	if cfg.Ticks < 0 {
		return nil, fmt.Errorf("sim: Ticks must be non-negative, got %d", cfg.Ticks)
	}

	master := NewSeed(cfg.Seed)
	disk := NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), 0)
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		return nil, fmt.Errorf("sim: open SimDisk-backed store: %w", err)
	}
	defer func() { _ = store.Close() }()

	h := &contHarness{
		store: store,
		res: &MVCCContentionResult{
			Seed: cfg.Seed, Sessions: cfg.Sessions, Counters: cfg.Counters,
			AckedIncrements: make([]int, cfg.Counters),
		},
		expectedControl: make([]int64, cfg.Sessions),
	}

	// Seed the shared counters and the per-session control keys, committed
	// before any contention begins.
	setup := store.Engine().NewSession()
	for i := 0; i < cfg.Counters; i++ {
		if err := h.exec(ctx, setup,
			"CREATE (n:Person {name:$name, val:$v})",
			map[string]any{"name": counterName(i), "v": int64(0)}); err != nil {
			return nil, fmt.Errorf("sim: seed counter %d: %w", i, err)
		}
	}
	h.sessions = make([]*contSession, cfg.Sessions)
	for i := range h.sessions {
		if err := h.exec(ctx, setup,
			"CREATE (n:Person {name:$name, val:$v})",
			map[string]any{"name": controlName(i), "v": int64(0)}); err != nil {
			return nil, fmt.Errorf("sim: seed control %d: %w", i, err)
		}
		h.sessions[i] = &contSession{
			id:   i,
			sess: store.Engine().NewSession(),
			rng:  NewSeed(master.Uint64N(^uint64(0)) ^ mvccSessSeedMix),
		}
	}

	for tick := 1; tick <= cfg.Ticks; tick++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := h.sessions[master.IntN(cfg.Sessions)]
		if err := h.advance(ctx, s); err != nil {
			return nil, err
		}
		if cfg.OnStep != nil {
			cfg.OnStep(tick, s.id, h.step)
		}
	}
	// Drain open transactions: a client that goes away rolls back.
	for _, s := range h.sessions {
		if s.tx == nil {
			continue
		}
		if err := s.tx.Rollback(); err != nil {
			return nil, fmt.Errorf("sim: drain rollback (session %d): %w", s.id, err)
		}
		s.tx = nil
	}

	// The drain point: nothing is in flight, and the store is still open.
	if cfg.OnQuiesce != nil {
		cfg.OnQuiesce(store)
	}

	h.res.Violations = append(h.res.Violations, h.adjudicate(ctx)...)
	return h.res, nil
}

func counterName(i int) string { return fmt.Sprintf("mv-counter-%d", i) }
func controlName(i int) string { return fmt.Sprintf("mv-control-%d", i) }

// exec runs one autocommit-style statement in its own committed transaction.
func (h *contHarness) exec(ctx context.Context, sess *cypher.Session, q string, params map[string]any) error {
	tx, err := sess.BeginTx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecAny(q, params); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// advance moves one session a single step through the read-modify-write
// transaction: BEGIN+read, write counter, write control, commit.
func (h *contHarness) advance(ctx context.Context, s *contSession) error {
	switch s.step {
	case 0:
		s.counter = s.rng.IntN(h.res.Counters)
		tx, err := s.sess.BeginTx(ctx)
		if err != nil {
			return fmt.Errorf("sim: session %d BEGIN: %w", s.id, err)
		}
		s.tx = tx
		v, conceded, err := h.readCounter(s)
		if err != nil || conceded {
			return err
		}
		s.readValue = v
		h.step = fmt.Sprintf("BEGIN+READ counter=%d v=%d", s.counter, v)
	case 1:
		// The lost-update shape: write back the SNAPSHOT read plus one. If a
		// concurrent increment committed after this transaction began, this
		// write MUST be refused — admitting it would overwrite that increment.
		if err := h.contendedWrite(s, tmplCounterSet,
			map[string]any{"name": counterName(s.counter), "v": s.readValue + 1},
			fmt.Sprintf("SET counter=%d v=%d", s.counter, s.readValue+1)); err != nil {
			return err
		}
	case 2:
		s.writes++
		s.controlValue = int64(s.id+1)*1_000_000 + int64(s.writes)
		if err := h.contendedWrite(s, tmplCounterSet,
			map[string]any{"name": controlName(s.id), "v": s.controlValue},
			fmt.Sprintf("SET control=%d v=%d", s.id, s.controlValue)); err != nil {
			return err
		}
	case 3:
		err := s.tx.Commit()
		switch {
		case err == nil:
			h.res.TxCommitted++
			h.res.AckedIncrements[s.counter]++
			h.expectedControl[s.id] = s.controlValue
			h.step = "COMMIT ok"
		case errors.Is(err, cypher.ErrSerializationConflict):
			h.res.TxConflicted++
			h.res.TypedConflicts++
			h.step = "COMMIT -> CONFLICT"
		default:
			return fmt.Errorf("sim: session %d COMMIT failed with an UNTYPED error (want cypher.ErrSerializationConflict): %w", s.id, err)
		}
		s.tx = nil
		s.step = 0
		return nil
	}
	// A conceded phase cleared the transaction and reset the machine; only a
	// surviving transaction advances to the next phase.
	if s.tx != nil {
		s.step++
	}
	return nil
}

// readCounter reads the session's chosen counter through its own transaction.
func (h *contHarness) readCounter(s *contSession) (int64, bool, error) {
	res, err := s.tx.ExecAny(tmplCounterRead, map[string]any{"name": counterName(s.counter)})
	var v int64
	if err == nil {
		if res.Next() {
			if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
				v = int64(iv)
			}
		}
		drainErr := res.Err()
		_ = res.Close()
		err = drainErr
	}
	if err != nil {
		if errors.Is(err, cypher.ErrSerializationConflict) {
			return 0, true, h.concede(s)
		}
		return 0, false, fmt.Errorf("sim: session %d counter read: %w", s.id, err)
	}
	return v, false, nil
}

// contendedWrite runs one write statement inside the open transaction; a
// typed refusal concedes the transaction, an untyped one is a hard fault (the
// retriability contract this scenario exists to assert).
func (h *contHarness) contendedWrite(s *contSession, q string, params map[string]any, what string) error {
	res, err := s.tx.ExecAny(q, params)
	if err == nil {
		for res.Next() {
		}
		drainErr := res.Err()
		_ = res.Close()
		err = drainErr
	}
	if err != nil {
		if errors.Is(err, cypher.ErrSerializationConflict) {
			h.step = what + " -> CONFLICT"
			return h.concede(s)
		}
		return fmt.Errorf("sim: session %d %s failed with an UNTYPED error (want cypher.ErrSerializationConflict): %w", s.id, what, err)
	}
	h.step = what
	return nil
}

// concede rolls the refused transaction back; it must leave no trace, which
// the terminal adjudication asserts on both the counters and the control keys.
func (h *contHarness) concede(s *contSession) error {
	if err := s.tx.Rollback(); err != nil {
		return fmt.Errorf("sim: session %d rollback after conflict: %w", s.id, err)
	}
	s.tx = nil
	s.step = 0
	h.res.TxConflicted++
	h.res.TypedConflicts++
	return nil
}

// adjudicate compares the terminal engine state against the acknowledged
// bookkeeping: each counter equals its acked increments (a shortfall is a
// lost update, an excess a phantom apply), and each control key holds the
// last acknowledged control value (a conflicted transaction left no trace).
func (h *contHarness) adjudicate(ctx context.Context) []Violation {
	var out []Violation
	read := func(name string) (int64, bool) {
		res, err := h.store.Engine().Run(ctx, tmplCounterRead, map[string]expr.Value{"name": expr.StringValue(name)})
		if err != nil {
			out = append(out, Violation{Kind: ViolationOracleDeviation, Op: "contention adjudication",
				Message: fmt.Sprintf("terminal read of %q failed: %v", name, err)})
			return 0, false
		}
		defer func() { _ = res.Close() }()
		var v int64
		if res.Next() {
			if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
				v = int64(iv)
			}
		}
		return v, true
	}
	for i, acked := range h.res.AckedIncrements {
		got, ok := read(counterName(i))
		if !ok {
			continue
		}
		if got != int64(acked) {
			kind := "PHANTOM APPLY: a refused transaction left a trace"
			if got < int64(acked) {
				kind = "LOST UPDATE: an acknowledged increment was silently dropped"
			}
			out = append(out, Violation{Kind: ViolationACIDIsolation, Op: "lost update",
				Message: fmt.Sprintf("counter %d: final value %d, acked increments %d — %s", i, got, acked, kind)})
		}
	}
	for sid, want := range h.expectedControl {
		got, ok := read(controlName(sid))
		if !ok {
			continue
		}
		if got != want {
			out = append(out, Violation{Kind: ViolationACIDAtomicity, Op: "conflict trace",
				Message: fmt.Sprintf("control %d: final value %d, want %d (the last acknowledged write) — a conflicted transaction left a trace or an acked write vanished", sid, got, want)})
		}
	}
	return out
}
