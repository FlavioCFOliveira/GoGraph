package sim

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// mvcc_sessions.go — the deterministic multi-session transaction mode: K
// logical sessions run explicit multi-statement transactions over the
// WAL-backed SimDisk store, interleaved at statement granularity by the seeded
// scheduler on a single goroutine. It is the mode that exercises GoGraph's
// MVCC machinery — overlapping ExplicitTx handles, per-object conflict
// detection, session read-your-own-writes — deterministically, which the
// one-op-per-tick autocommit loop structurally cannot (rmp #2435).
//
// Overlapping explicit transactions on ONE goroutine cannot deadlock: since
// rmp #2305/#2306 BEGIN acquires nothing and nothing is held between
// statements, so every statement and every COMMIT runs to completion
// synchronously. A genuine write-write collision surfaces as the typed
// [mvcc.ErrSerializationConflict], which the harness answers with a rollback —
// exactly as a production client would.

// mvccSessSeedMix derives the per-session sub-seed space from the master seed,
// so a session's op stream is independent of the scheduler's pick sequence.
const mvccSessSeedMix uint64 = 0x94d049bb133111eb

// MVCCSessionsConfig parameterises a deterministic multi-session run. Every
// field is bounded and the whole run is a pure function of Seed.
type MVCCSessionsConfig struct {
	// Seed is the master seed; scheduling, per-session op streams, transaction
	// shapes, and terminal choices are all derived from it.
	Seed uint64
	// Ticks is the number of scheduler steps. Each tick advances exactly one
	// session by one step (BEGIN, one statement, or COMMIT/ROLLBACK).
	Ticks int
	// Sessions is the number of logical sessions (each a [cypher.Session] with
	// its own transaction lifecycle). Values < 2 are normalised to 2 — below
	// that no transactions can overlap and the mode loses its purpose.
	Sessions int
	// MinTxOps and MaxTxOps bound the statements per transaction, drawn per
	// transaction from the session's sub-seed. Non-positive values are
	// normalised to 1..4.
	MinTxOps, MaxTxOps int
	// ReadTxWeight is the probability (clamped to [0,1]) that a new transaction
	// is read-only ([cypher.Session.BeginReadTx]).
	ReadTxWeight float64
	// RollbackWeight is the probability (clamped to [0,1]) that a finished
	// write transaction is rolled back instead of committed, so the abort path
	// is exercised alongside the commit path.
	RollbackWeight float64
	// CheckEvery is the oracle/engine parity-check cadence in ticks; values
	// <= 0 are normalised to 1. Parity holds at every tick boundary because
	// the oracle folds a workspace exactly when the engine acknowledges the
	// matching COMMIT.
	CheckEvery int
	// OnStep, when non-nil, is called synchronously after every scheduler step
	// with the tick, the session advanced, and a one-line description of what
	// happened. Like [Config.OnOp] it is an observation hook: it must not
	// mutate state or draw randomness, or reproducibility breaks.
	OnStep func(tick, session int, what string)
}

// normalise applies the documented defaults in place.
func (c *MVCCSessionsConfig) normalise() {
	if c.Sessions < 2 {
		c.Sessions = 2
	}
	if c.MinTxOps <= 0 {
		c.MinTxOps = 1
	}
	if c.MaxTxOps < c.MinTxOps {
		c.MaxTxOps = c.MinTxOps + 3
	}
	if c.CheckEvery <= 0 {
		c.CheckEvery = 1
	}
	c.ReadTxWeight = clamp01(c.ReadTxWeight)
	c.RollbackWeight = clamp01(c.RollbackWeight)
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// MVCCSessionsResult summarises a deterministic multi-session run. All fields
// are pure functions of the config seed, so two runs with the same config are
// directly comparable (the determinism gate compares them byte for byte).
type MVCCSessionsResult struct {
	// Violations holds the first invariant violations detected, if any; empty
	// on a clean run.
	Violations []Violation
	// FoldErrors records every oracle fold refusal: the engine acknowledged a
	// COMMIT whose decided effects no longer fold over the committed model.
	// Any entry is an isolation finding (or a harness bug) — never noise.
	FoldErrors []string
	Seed       uint64
	Sessions   int
	Ticks      int
	// Statements counts the statements executed inside transactions (BEGIN and
	// COMMIT/ROLLBACK excluded).
	Statements int
	// TxCommitted / TxRolledBack / TxConflicted / TxReadOnly count finished
	// transactions by outcome. A conflicted transaction is one the engine
	// refused with [mvcc.ErrSerializationConflict] — at a statement or at
	// COMMIT — and whose workspace was discarded.
	TxCommitted  int
	TxRolledBack int
	TxConflicted int
	TxReadOnly   int
	// OverlapTicks counts ticks at which >= 2 transactions were open at once;
	// WriteOverlapTicks counts ticks with >= 2 WRITE transactions open. A run
	// that never overlaps proves nothing about MVCC — the gates assert these
	// are nonzero.
	OverlapTicks      int
	WriteOverlapTicks int
	// MaxOpenTx is the high-water mark of simultaneously open transactions.
	MaxOpenTx int
}

// Clean reports whether the run finished with no violations and no fold
// refusals.
func (r *MVCCSessionsResult) Clean() bool {
	return len(r.Violations) == 0 && len(r.FoldErrors) == 0
}

// mvccSessionState is one logical session's transaction state machine.
type mvccSessionState struct {
	sess *cypher.Session
	// tx is the open explicit transaction, nil when idle.
	tx *cypher.ExplicitTx
	// otx is the oracle workspace paired with a WRITE tx; nil when idle or
	// read-only.
	otx *OracleTx
	// rng is the session's private sub-seed stream.
	rng *Seed
	// remaining is the number of statements left before the terminal step.
	remaining int
	// created counts the names this session has created, for unique naming.
	created int
	// id namespaces the session's created names.
	id       int
	readOnly bool
}

// open reports whether the session has a transaction in flight.
func (s *mvccSessionState) open() bool { return s.tx != nil }

// mvccHarness bundles the run's live pieces.
type mvccHarness struct {
	store    *SimStore
	oracle   *GraphOracle
	checker  *InvariantChecker
	adapter  *EngineAdapter
	sessions []*mvccSessionState
	res      *MVCCSessionsResult
	cfg      MVCCSessionsConfig
	// step is the one-line description of the last scheduler step, for the
	// OnStep observation hook. Single-goroutine, reset every tick.
	step string
}

// RunMVCCSessions executes a deterministic multi-session transaction run over
// a WAL-backed SimDisk store and returns its result. The whole run — schedule,
// statements, outcomes — is a pure function of cfg.Seed: two calls with the
// same config produce identical results (the determinism gate relies on it).
//
// Durability: the engine is the real persistence stack ([OpenSimStore], WAL
// append+sync on the SimDisk), never the bare in-memory engine, so every
// acknowledged COMMIT in this mode is a durable commit (rmp #2435).
//
// # Concurrency contract
//
// RunMVCCSessions runs entirely on the calling goroutine and spawns none;
// "concurrent" transactions are interleaved deterministically, never parallel.
func RunMVCCSessions(ctx context.Context, cfg MVCCSessionsConfig) (*MVCCSessionsResult, error) {
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

	h := &mvccHarness{
		cfg:     cfg,
		store:   store,
		oracle:  NewGraphOracle(),
		checker: NewInvariantChecker(NewSeed(cfg.Seed ^ checkerSeedMix)),
		adapter: NewEngineAdapter(store.Engine()),
		res:     &MVCCSessionsResult{Seed: cfg.Seed, Sessions: cfg.Sessions, Ticks: cfg.Ticks},
	}
	// Per-session sub-seeds are drawn up front on this goroutine so each
	// session's op stream is a function of (master seed, session index) alone,
	// independent of the scheduler's later picks.
	h.sessions = make([]*mvccSessionState, cfg.Sessions)
	for i := range h.sessions {
		h.sessions[i] = &mvccSessionState{
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
		h.observeOverlap()
		if len(h.res.FoldErrors) > 0 {
			// A fold refusal is a finding; stop at first occurrence so the seed
			// reproduces it at the failing tick.
			return h.res, nil
		}
		if tick%cfg.CheckEvery == 0 {
			if v := h.checker.Check(int64(tick), h.oracle, h.adapter); len(v) > 0 {
				v = append(v, h.nameDiff(int64(tick))...)
				h.res.Violations = v
				return h.res, nil
			}
		}
	}

	// Drain: roll back every open transaction (a client that goes away), then
	// run one terminal parity check.
	for _, s := range h.sessions {
		if !s.open() {
			continue
		}
		if err := s.tx.Rollback(); err != nil {
			return nil, fmt.Errorf("sim: drain rollback (session %d): %w", s.id, err)
		}
		if s.otx != nil {
			s.otx.Abort()
			s.otx = nil
		}
		s.tx = nil
		h.res.TxRolledBack++
	}
	if v := h.checker.Check(int64(cfg.Ticks), h.oracle, h.adapter); len(v) > 0 {
		h.res.Violations = v
	}
	return h.res, nil
}

// nameDiff renders the Person-name symmetric difference between the oracle
// and the engine as extra violations, so a count mismatch names the exact
// node(s) that diverged — a count alone cannot be acted on.
func (h *mvccHarness) nameDiff(tick int64) []Violation {
	res, err := h.adapter.Run(context.Background(), "MATCH (n:Person) RETURN n.name", nil)
	if err != nil {
		return []Violation{{Tick: tick, Op: "name diff", Kind: ViolationOracleDeviation, Message: err.Error()}}
	}
	engineNames := make(map[string]int)
	for res.Next() {
		if name, ok := res.StringAt(0); ok {
			engineNames[name]++
		}
	}
	drainErr := res.Err()
	_ = res.Close()
	if drainErr != nil {
		// A partial enumeration would produce a MISLEADING diff (names missing
		// because the read died, not because the engine lost them), so the
		// drain failure replaces the diff outright.
		return []Violation{{Tick: tick, Op: "name diff", Kind: ViolationOracleDeviation,
			Message: fmt.Sprintf("name enumeration drain failed: %v", drainErr)}}
	}

	var out []Violation
	oracleNames := make(map[string]bool, len(h.oracle.NodeNames()))
	for _, n := range h.oracle.NodeNames() {
		oracleNames[n] = true
		if engineNames[n] == 0 {
			out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
				Message: fmt.Sprintf("oracle has %q, engine does not (%s)", n, h.lpgState(n))})
		}
	}
	for _, n := range slices.Sorted(maps.Keys(engineNames)) {
		if !oracleNames[n] {
			out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
				Message: fmt.Sprintf("engine has %q, oracle does not", n)})
		}
		if engineNames[n] > 1 {
			out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
				Message: fmt.Sprintf("engine holds %d nodes named %q (workload names are unique)", engineNames[n], n)})
		}
	}

	// The transactional workload creates ONLY Person nodes, so any node
	// without the label is itself a finding (e.g. a half-deleted or
	// label-stripped remnant) — and it explains a whole-graph count mismatch
	// that the name diff cannot see. The id makes the remnant traceable to its
	// lpg-level records.
	// Best-effort supplementary diagnostics: a failure to enumerate the
	// non-Person remnants only loses DETAIL on a violation the caller is
	// already returning, so the error is deliberately not surfaced as its own
	// violation (unlike the primary enumeration above, whose failure would
	// forge the diff itself).
	if res, err := h.adapter.Run(context.Background(),
		"MATCH (n) WHERE NOT n:Person RETURN id(n), coalesce(n.name, '<unnamed>')", nil); err == nil {
		for res.Next() {
			nodeID, _ := res.IntAt(0)
			name, _ := res.StringAt(1)
			g := h.store.Graph()
			id := graph.NodeID(nodeID)
			snap := g.BeginRead()
			state := fmt.Sprintf("tombstoned=%v existsPresent=%v existsSnap=%v",
				g.IsTombstoned(id), g.NodeExistsAsOf(id, nil), g.NodeExistsAsOf(id, snap))
			g.EndRead(snap)
			out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
				Message: fmt.Sprintf("engine holds a non-Person node (id=%d name=%q, %s) the workload never creates", nodeID, name, state)})
		}
		_ = res.Close()
	}
	return out
}

// lpgState renders the lpg-level existence facts for the named node, so a
// name-diff violation carries the substrate's own answer alongside the
// query-level symptom.
func (h *mvccHarness) lpgState(name string) string {
	g := h.store.Graph()
	id, ok := g.AdjList().Mapper().Lookup(name)
	if !ok {
		return "mapper: name not interned"
	}
	snap := g.BeginRead()
	existsSnap := g.NodeExistsAsOf(id, snap)
	g.EndRead(snap)
	return fmt.Sprintf("id=%d tombstoned=%v existsPresent=%v existsSnap=%v",
		id, g.IsTombstoned(id), g.NodeExistsAsOf(id, nil), existsSnap)
}

// observeOverlap updates the overlap counters for the current tick.
func (h *mvccHarness) observeOverlap() {
	openTx, openWrite := 0, 0
	for _, s := range h.sessions {
		if !s.open() {
			continue
		}
		openTx++
		if !s.readOnly {
			openWrite++
		}
	}
	if openTx >= 2 {
		h.res.OverlapTicks++
	}
	if openWrite >= 2 {
		h.res.WriteOverlapTicks++
	}
	if openTx > h.res.MaxOpenTx {
		h.res.MaxOpenTx = openTx
	}
}

// advance moves one session's state machine a single step: BEGIN when idle,
// one statement while the budget lasts, COMMIT/ROLLBACK at the end.
func (h *mvccHarness) advance(ctx context.Context, s *mvccSessionState) error {
	switch {
	case !s.open():
		return h.begin(ctx, s)
	case s.remaining > 0:
		return h.statement(s)
	default:
		return h.finish(s)
	}
}

// begin opens a new transaction on the session: read-only with probability
// ReadTxWeight, write otherwise (paired with a fresh oracle workspace).
func (h *mvccHarness) begin(ctx context.Context, s *mvccSessionState) error {
	s.readOnly = s.rng.Float64() < h.cfg.ReadTxWeight
	s.remaining = h.cfg.MinTxOps
	if spread := h.cfg.MaxTxOps - h.cfg.MinTxOps; spread > 0 {
		s.remaining += s.rng.IntN(spread + 1)
	}
	var err error
	if s.readOnly {
		s.tx, err = s.sess.BeginReadTx(ctx)
	} else {
		s.tx, err = s.sess.BeginTx(ctx)
	}
	if err != nil {
		return fmt.Errorf("sim: session %d BEGIN(readOnly=%v): %w", s.id, s.readOnly, err)
	}
	if !s.readOnly {
		s.otx = h.oracle.BeginTx()
	}
	h.step = fmt.Sprintf("BEGIN readOnly=%v ops=%d", s.readOnly, s.remaining)
	return nil
}

// mvccReadTemplates are the bounded reads a transaction issues (both kinds of
// transaction read; only write transactions mutate).
var mvccReadTemplates = []string{
	"MATCH (n:Person) RETURN count(n)",
	"MATCH (:Person)-[r:KNOWS]->(:Person) RETURN count(r)",
}

// statement runs one seed-drawn statement inside the open transaction and
// mirrors a successful write into the oracle workspace. A typed serialization
// conflict finishes the transaction as conflicted (rolled back, workspace
// discarded); any other engine error is a hard fault.
func (h *mvccHarness) statement(s *mvccSessionState) error {
	s.remaining--
	h.res.Statements++

	cypherText, params, kind := h.drawStatement(s)
	res, err := s.tx.ExecAny(cypherText, params)
	if err == nil {
		for res.Next() {
		}
		drainErr := res.Err()
		_ = res.Close()
		err = drainErr
	}
	if err != nil {
		if errors.Is(err, mvcc.ErrSerializationConflict) {
			h.step = fmt.Sprintf("STMT %s %v -> CONFLICT: %v", cypherText, params, err)
			return h.concede(s)
		}
		return fmt.Errorf("sim: session %d statement %q: %w", s.id, cypherText, err)
	}
	if s.otx != nil {
		h.mirror(s.otx, kind, cypherText, params)
	}
	h.step = fmt.Sprintf("STMT %s %v", cypherText, params)
	return nil
}

// drawStatement picks the next statement for the session from its sub-seed.
// Read-only transactions draw only reads. Write transactions draw a mix in
// which CREATE/MERGE names are namespaced per session (cross-session name
// collisions are the contended scenario's business, rmp #2437) while SET and
// DETACH DELETE target the shared visible node set — which is what makes
// genuine cross-session write-write conflicts reachable.
func (h *mvccHarness) drawStatement(s *mvccSessionState) (string, map[string]any, OpKind) {
	if s.readOnly {
		q := mvccReadTemplates[s.rng.IntN(len(mvccReadTemplates))]
		return q, nil, OpMatch
	}
	names := s.otx.NodeNames()
	roll := s.rng.Float64()
	switch {
	case roll < 0.40 || len(names) == 0:
		name := fmt.Sprintf("mv-s%d-n%d", s.id, s.created)
		s.created++
		return tmplCreatePerson, map[string]any{"name": name, "age": int64(s.rng.IntN(100))}, OpCreate
	case roll < 0.65:
		return tmplSetAge, map[string]any{"name": names[s.rng.IntN(len(names))], "age": int64(s.rng.IntN(100))}, OpUpdate
	case roll < 0.80:
		name := fmt.Sprintf("mv-s%d-m%d", s.id, s.created)
		s.created++
		return tmplMergePerson, map[string]any{"name": name}, OpMerge
	case roll < 0.90 && len(names) >= 2:
		a := names[s.rng.IntN(len(names))]
		b := names[s.rng.IntN(len(names))]
		// Never re-create an edge that already exists in the PRESENT committed
		// state or in this transaction's own pending set: the engine's
		// parallel-edge guard checks the present adjacency, so on the
		// non-multigraph sim store a duplicate CREATE is a typed refusal, not a
		// no-op. The draw still consumed the same randomness, so the op stream
		// stays a pure function of the seed.
		if h.oracle.HasKnowsByName(a, b) || s.otx.PendingKnows(a, b) {
			q := mvccReadTemplates[s.rng.IntN(len(mvccReadTemplates))]
			return q, nil, OpMatch
		}
		return tmplCreateKnows, map[string]any{"a": a, "b": b}, OpCreate
	case roll < 0.95:
		return tmplDetachDelete, map[string]any{"name": names[s.rng.IntN(len(names))]}, OpDelete
	default:
		q := mvccReadTemplates[s.rng.IntN(len(mvccReadTemplates))]
		return q, nil, OpMatch
	}
}

// mirror applies a successfully executed write statement to the transaction's
// oracle workspace, keeping the decided-effect model in lock-step with the
// engine's own uncommitted state.
func (h *mvccHarness) mirror(otx *OracleTx, kind OpKind, cypherText string, params map[string]any) {
	switch kind {
	case OpCreate:
		if cypherText == tmplCreateKnows {
			otx.ApplyCreateKnows(params)
			return
		}
		otx.ApplyCreate(cypherText, params)
	case OpMerge:
		otx.ApplyMerge(cypherText, params)
	case OpDelete:
		otx.ApplyDelete(cypherText, params)
	case OpUpdate, OpMatch:
		otx.ApplyMatch(cypherText, params)
	}
}

// concede finishes a transaction the engine refused with the typed
// serialization conflict: the client rolls back and the workspace vanishes.
func (h *mvccHarness) concede(s *mvccSessionState) error {
	if err := s.tx.Rollback(); err != nil {
		return fmt.Errorf("sim: session %d rollback after conflict: %w", s.id, err)
	}
	if s.otx != nil {
		s.otx.Abort()
		s.otx = nil
	}
	s.tx = nil
	h.res.TxConflicted++
	return nil
}

// finish terminates a transaction whose statement budget is exhausted:
// read-only handles tear down via Rollback; a write transaction rolls back
// with probability RollbackWeight and otherwise COMMITs — folding its oracle
// workspace only when the engine acknowledged, and recording a fold refusal
// as a finding.
func (h *mvccHarness) finish(s *mvccSessionState) error {
	if s.readOnly {
		if err := s.tx.Rollback(); err != nil {
			return fmt.Errorf("sim: session %d read-tx teardown: %w", s.id, err)
		}
		s.tx = nil
		h.res.TxReadOnly++
		h.step = "END read-tx"
		return nil
	}
	if s.rng.Float64() < h.cfg.RollbackWeight {
		if err := s.tx.Rollback(); err != nil {
			return fmt.Errorf("sim: session %d rollback: %w", s.id, err)
		}
		s.otx.Abort()
		s.otx = nil
		s.tx = nil
		h.res.TxRolledBack++
		h.step = "ROLLBACK"
		return nil
	}
	err := s.tx.Commit()
	switch {
	case err == nil:
		if foldErr := s.otx.Commit(); foldErr != nil {
			// The engine acknowledged a commit the model cannot fold: either an
			// isolation defect (a collision the engine should have refused) or a
			// harness modelling gap. Recorded as a finding, never swallowed. The
			// workspace is discarded so the run's terminal state stays defined.
			h.res.FoldErrors = append(h.res.FoldErrors,
				fmt.Sprintf("session %d: engine committed but oracle refused: %v", s.id, foldErr))
			s.otx.Abort()
		}
	case errors.Is(err, mvcc.ErrSerializationConflict):
		s.otx.Abort()
		s.otx = nil
		s.tx = nil
		h.res.TxConflicted++
		h.step = "COMMIT -> CONFLICT"
		return nil
	default:
		return fmt.Errorf("sim: session %d COMMIT: %w", s.id, err)
	}
	s.otx = nil
	s.tx = nil
	h.res.TxCommitted++
	h.step = "COMMIT ok"
	return nil
}
