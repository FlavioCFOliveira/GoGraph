package sim

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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
	// Crash opts into deterministic crash injection (rmp #2438): at
	// seed-scheduled ticks the SimDisk takes a HOST crash (every byte and every
	// dirent no successful fsync covered is lost — [SimDisk.CrashHost]), the
	// store reopens through real WAL recovery, every open
	// transaction dies unacknowledged, and the recovered state is adjudicated
	// at TRANSACTION granularity against the folded oracle. The zero value
	// disables crashes — the safe default, byte-identical to a pre-crash run.
	//
	// Crashes land BETWEEN scheduler steps (the mode is single-goroutine), so
	// they always land with transactions OPEN mid-flight; intra-commit crash
	// points remain the internal/crashpoint battery's domain.
	Crash CrashConfig
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
	// StabilityProbes counts adjudicated count reads inside read-only
	// transactions (snapshot stability, rmp #2436); StabilityInterleaved
	// counts the subset with at least one commit folded since the
	// transaction's BEGIN — the probes that prove stability holds even when
	// other sessions commit in between. A run with StabilityInterleaved == 0
	// never exercised the interesting case.
	StabilityProbes, StabilityInterleaved int
	// RYOWProbes counts in-transaction read-your-own-writes probes (after
	// each write statement, plus the write-view count adjudications);
	// RYOWCrossTx counts the session-level probes at BEGIN for a name the
	// session committed earlier.
	RYOWProbes, RYOWCrossTx int
	// PairsCommitted counts committed invariant-bearing pairs (two nodes
	// created by two statements of one transaction); PairProbes counts the
	// atomic-visibility probes adjudicated against them.
	PairsCommitted, PairProbes int
	// TxDoomed counts write transactions observed under the doomed-tx
	// contract (rmp #2354): an own write read back wrong — a refused void
	// write — and the transaction then ended in a serialization conflict, as
	// the contract requires. A suspect that instead COMMITs cleanly is a
	// Violation, never counted here.
	TxDoomed int
	// Crashes counts injected crash+recovery cycles (rmp #2438); TxCrashed
	// counts transactions that were OPEN when a crash landed and therefore
	// died unacknowledged — their effects must be absent after recovery.
	// ReplayedOps totals the WAL operations recovery replayed.
	Crashes     int
	TxCrashed   int
	ReplayedOps int
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

	// beginFoldSeq is the harness fold counter captured at this transaction's
	// BEGIN; it dates the transaction's snapshot against committed pairs and
	// counts the commits that folded while the transaction stayed open.
	beginFoldSeq int
	// expectNodes / expectEdges are the committed oracle counts captured at
	// the BEGIN of a READ-ONLY transaction — the whole-transaction snapshot
	// expectation every in-tx count read is adjudicated against (rmp #2436).
	// expectNames / expectEdgeNames are the matching committed name and edge
	// sets, kept so a stability violation can NAME what appeared or vanished —
	// a count alone cannot be acted on.
	expectNodes, expectEdges int64
	expectNames              []string
	expectEdgeNames          []string
	// pairEmit is the second pair-member name the session's next drawn
	// statement must CREATE; pairFirst/pairSecond track the in-flight pair
	// until both statements succeed; pairsDone holds pairs completed inside
	// the open transaction, registered as committed pairs at fold.
	pairEmit, pairFirst, pairSecond string
	pairsDone                       [][2]string
	// lastCommitted is a name this session created and committed in an
	// earlier transaction, probed at the next BEGIN for the session-level
	// cross-transaction read-your-own-writes contract.
	lastCommitted string
	// doomSuspect holds the first read-your-own-writes divergence observed in
	// the open WRITE transaction. Under the engine's documented doomed-tx
	// contract (rmp #2354) a conflict hit by a VOID primitive does not fail
	// its statement — the write is refused internally and the conflict
	// surfaces at the next write or at COMMIT — so an own write that reads
	// back wrong is not a violation YET: the transaction must now end in a
	// serialization conflict (or a client rollback). A clean COMMIT of a
	// suspect transaction is the violation (a silently lost write).
	doomSuspect string
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
	// disk is the faulting SimDisk under the store, kept so a crash can revoke
	// its unsynced state; crash is the seed-driven schedule (rmp #2438).
	disk  *SimDisk
	crash *CrashSchedule
	// step is the one-line description of the last scheduler step, for the
	// OnStep observation hook. Single-goroutine, reset every tick.
	step string
	// tick is the scheduler tick currently executing, stamped on violations
	// the isolation probes raise mid-step.
	tick int64
	// foldSeq counts successful oracle folds (acknowledged commits applied to
	// the committed model); it dates transaction snapshots and committed pairs.
	foldSeq int
	// pairs holds every committed invariant-bearing pair, in fold order, for
	// the atomic-visibility probes.
	pairs []mvccPair
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
		disk:    disk,
		crash:   NewCrashSchedule(NewSeed(cfg.Seed^crashSeedMix), cfg.Crash),
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
		h.tick = int64(tick)
		s := h.sessions[master.IntN(cfg.Sessions)]
		if err := h.advance(ctx, s); err != nil {
			return nil, err
		}
		if cfg.OnStep != nil {
			cfg.OnStep(tick, s.id, h.step)
		}
		h.observeOverlap()
		if len(h.res.FoldErrors) > 0 || len(h.res.Violations) > 0 {
			// A fold refusal, or a violation raised by an isolation probe inside
			// the step, is a finding; stop at first occurrence so the seed
			// reproduces it at the failing tick.
			return h.res, nil
		}
		if tick%cfg.CheckEvery == 0 {
			if v := h.checker.Check(int64(tick), h.oracle, h.adapter); len(v) > 0 {
				v = append(v, h.nameDiff(int64(tick))...)
				h.res.Violations = v
				return h.res, nil
			}
			if v := h.checkCommittedPairs(int64(tick)); len(v) > 0 {
				h.res.Violations = v
				return h.res, nil
			}
		}
		if err := h.maybeCrash(int64(tick)); err != nil {
			return nil, err
		}
		if len(h.res.Violations) > 0 {
			return h.res, nil
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

// maybeCrash injects a HOST crash at seed-scheduled ticks
// (rmp #2438): the SimDisk revokes its unsynced state, the store reopens
// through real WAL recovery, every OPEN transaction dies unacknowledged (its
// oracle workspace is discarded — the folded oracle is exactly the
// acknowledged-transaction set), and the recovered state is adjudicated at
// TRANSACTION granularity:
//
//   - [InvariantChecker.CheckDurability] full-scans the folded model: a
//     recovered state below it lost an acknowledged transaction's effects
//     (Durability); above it, an unacknowledged transaction leaked partial
//     effects in (Atomicity at the crash boundary). acked ⊆ recovered ⊆
//     issued collapses to exact equality with the folded model, because the
//     oracle folds whole transactions at COMMIT acknowledgement and nothing
//     else.
//   - every committed invariant-bearing pair is swept whole: a torn replay of
//     a multi-statement transaction surfaces as exactly ONE member present —
//     the "all present or all absent" clause made observable.
//
// Violations land in res.Violations (the caller stops at the tick, so the
// seed reproduces the finding); a recovery failure is a hard fault. Inert
// when crashes are disabled.
func (h *mvccHarness) maybeCrash(tick int64) error {
	if !h.crash.ShouldCrash(tick) {
		return nil
	}
	// HOST crash: no graceful close; every byte no successful fsync covered and
	// every not-yet-durable directory entry are lost, exactly as a power failure
	// loses them ([SimDisk.Crash] is [SimDisk.CrashHost]).
	h.disk.Crash()
	store, err := OpenSimStore(h.disk, h.store.Config())
	if err != nil {
		return fmt.Errorf("sim: crash recovery at tick %d: %w", tick, err)
	}
	h.store = store
	h.adapter = NewEngineAdapter(store.Engine())
	h.res.Crashes++
	h.res.ReplayedOps += store.WALOps()

	// Every open transaction died with the store, unacknowledged: discard its
	// workspace (nothing folds), drop the dead handle untouched, and rebind
	// each session to the RECOVERED engine. lastCommitted survives — it names
	// an acknowledged, folded write the recovery must serve.
	for _, s := range h.sessions {
		if s.open() {
			h.res.TxCrashed++
			if s.otx != nil {
				s.otx.Abort()
				s.otx = nil
			}
			s.tx = nil
			s.clearPairState()
			s.doomSuspect = ""
			s.remaining = 0
		}
		s.sess = store.Engine().NewSession()
	}

	if v := h.checker.CheckDurability(tick, h.oracle, h.adapter); len(v) > 0 {
		h.res.Violations = append(h.res.Violations, v...)
		h.res.Violations = append(h.res.Violations, h.nameDiff(tick)...)
		return nil
	}
	h.res.Violations = append(h.res.Violations, h.crashPairSweep(tick)...)
	return nil
}

// crashPairSweep asserts, over EVERY committed invariant-bearing pair (not
// the bounded per-statement sample), that recovery kept the pair whole: a
// count of one is a torn multi-statement transaction — half a transaction
// replayed — which is precisely the atomicity-at-transaction-granularity
// breach rmp #2438 exists to detect.
func (h *mvccHarness) crashPairSweep(tick int64) []Violation {
	var out []Violation
	for _, p := range h.pairs {
		got, err := h.checker.countQuery(h.adapter, tmplCountPair, map[string]any{"a": p.a, "b": p.b})
		if err != nil {
			out = append(out, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "crash pair sweep",
				Message: fmt.Sprintf("post-recovery pair probe (%q,%q) failed: %v", p.a, p.b, err)})
			continue
		}
		if got != 2 {
			detail := "an acknowledged multi-statement transaction did not survive recovery whole"
			if got == 1 {
				detail = "TORN TRANSACTION: exactly one member of an acknowledged pair survived replay"
			}
			out = append(out, Violation{Kind: ViolationACIDAtomicity, Tick: tick, Op: "crash pair sweep",
				Message: fmt.Sprintf("committed pair (%q,%q) counts %d members after recovery, want 2 — %s",
					p.a, p.b, got, detail)})
		}
	}
	return out
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

	// Edge diff: enumerate the engine's KNOWS edges by endpoint names and
	// compare against the oracle's committed edge set, so an edge-count
	// mismatch names the exact edge that leaked or vanished.
	if res, err := h.adapter.Run(context.Background(),
		"MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.name, b.name", nil); err == nil {
		type pair struct{ a, b string }
		engineEdges := make(map[pair]int)
		for res.Next() {
			a, okA := res.StringAt(0)
			b, okB := res.StringAt(1)
			if okA && okB {
				engineEdges[pair{a, b}]++
			}
		}
		drainErr := res.Err()
		_ = res.Close()
		if drainErr == nil {
			// Walked via edgeStates + nameOf (not KnowsEdgesByName, which serves
			// the edge-PROPERTY checker and skips property-less edges — all of
			// this workload's edges).
			oracleEdges := make(map[pair]bool)
			for _, e := range h.oracle.edgeStates() {
				p := pair{h.oracle.nameOf(e.SrcID), h.oracle.nameOf(e.DstID)}
				oracleEdges[p] = true
				if engineEdges[p] == 0 {
					out = append(out, Violation{Tick: tick, Op: "edge diff", Kind: ViolationACIDConsistency,
						Message: fmt.Sprintf("oracle has edge (%q)-[KNOWS]->(%q), engine does not", p.a, p.b)})
				}
			}
			keys := make([]pair, 0, len(engineEdges))
			for p := range engineEdges {
				keys = append(keys, p)
			}
			slices.SortFunc(keys, func(x, y pair) int {
				if x.a != y.a {
					return strings.Compare(x.a, y.a)
				}
				return strings.Compare(x.b, y.b)
			})
			for _, p := range keys {
				if !oracleEdges[p] {
					out = append(out, Violation{Tick: tick, Op: "edge diff", Kind: ViolationACIDConsistency,
						Message: fmt.Sprintf("engine has edge (%q)-[KNOWS]->(%q), oracle does not", p.a, p.b)})
				}
				if engineEdges[p] > 1 {
					out = append(out, Violation{Tick: tick, Op: "edge diff", Kind: ViolationACIDConsistency,
						Message: fmt.Sprintf("engine holds %d parallel (%q)-[KNOWS]->(%q) edges on a non-multigraph store", engineEdges[p], p.a, p.b)})
				}
			}
		}
	}

	// The engine must also agree with itself on EDGES: the unlabeled edge scan
	// (the edge-count probe's path) must enumerate exactly the edges the
	// labeled path serves. An arc on one path but not the other is a torn edge
	// remnant (rmp #2445 family).
	if res, err := h.adapter.Run(context.Background(),
		"MATCH (a)-[r]->(b) RETURN id(a), id(b), coalesce(a.name,'?'), coalesce(b.name,'?')", nil); err == nil {
		var rows []string
		for res.Next() {
			ia, _ := res.IntAt(0)
			ib, _ := res.IntAt(1)
			na, _ := res.StringAt(2)
			nb, _ := res.StringAt(3)
			rows = append(rows, fmt.Sprintf("(%d %q)->(%d %q)", ia, na, ib, nb))
		}
		drainErr := res.Err()
		_ = res.Close()
		if drainErr == nil && int64(len(rows)) != int64(h.oracle.EdgeCount()) {
			slices.Sort(rows)
			oracleSet := make(map[string]bool)
			for _, e := range h.oracle.edgeStates() {
				oracleSet[fmt.Sprintf("%q->%q", h.oracle.nameOf(e.SrcID), h.oracle.nameOf(e.DstID))] = true
			}
			for i, r := range rows {
				// Mark arcs the committed model does not hold — the leak.
				if idx := strings.Index(r, ` "`); idx >= 0 {
					key := extractArcNames(r)
					if key != "" && !oracleSet[key] {
						rows[i] = r + " <== NOT IN ORACLE"
					}
				}
			}
			out = append(out, Violation{Tick: tick, Op: "edge diff", Kind: ViolationACIDConsistency,
				Message: fmt.Sprintf("unlabeled edge scan serves %d arcs (oracle %d): %s",
					len(rows), h.oracle.EdgeCount(), strings.Join(rows, ", "))})
		}
	}

	// The engine must also agree with ITSELF: the unlabeled whole-graph scan
	// (the node-count probe's path) must enumerate exactly the names the
	// label-index path serves. A name on one path but not the other is torn
	// per-node state — the exact shape of the rmp #2443/#2444 defect family —
	// and it explains a count mismatch that the labeled diff above cannot see.
	if res, err := h.adapter.Run(context.Background(), "MATCH (n) RETURN n.name", nil); err == nil {
		unlabeled := make(map[string]int)
		for res.Next() {
			if name, ok := res.StringAt(0); ok {
				unlabeled[name]++
			}
		}
		drainErr := res.Err()
		_ = res.Close()
		if drainErr == nil {
			for _, n := range slices.Sorted(maps.Keys(engineNames)) {
				if unlabeled[n] == 0 {
					out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
						Message: fmt.Sprintf("engine self-disagreement: %q visible via :Person label scan but NOT via the unlabeled scan (%s)", n, h.lpgState(n))})
				}
			}
			for _, n := range slices.Sorted(maps.Keys(unlabeled)) {
				if engineNames[n] == 0 {
					out = append(out, Violation{Tick: tick, Op: "name diff", Kind: ViolationACIDConsistency,
						Message: fmt.Sprintf("engine self-disagreement: %q visible via the unlabeled scan but NOT via the :Person label scan (%s)", n, h.lpgState(n))})
				}
			}
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

// extractArcNames turns a rendered raw-arc row `(id "a")->(id "b")` back into
// the `"a"->"b"` key the oracle edge set is indexed by, or "" when the row
// does not parse.
func extractArcNames(row string) string {
	var names []string
	rest := row
	for range 2 {
		i := strings.Index(rest, `"`)
		if i < 0 {
			return ""
		}
		j := strings.Index(rest[i+1:], `"`)
		if j < 0 {
			return ""
		}
		names = append(names, rest[i+1:i+1+j])
		rest = rest[i+j+2:]
	}
	return fmt.Sprintf("%q->%q", names[0], names[1])
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
	// Date the transaction's snapshot for the isolation probes, and capture
	// the whole-transaction count expectations of a read-only handle: the
	// committed oracle at BEGIN is exactly what the pinned snapshot must keep
	// serving, however many commits fold while the transaction stays open.
	s.beginFoldSeq = h.foldSeq
	if s.readOnly {
		s.expectNodes = int64(h.oracle.NodeCount())
		s.expectEdges = int64(h.oracle.EdgeCount())
		s.expectNames = h.oracle.NodeNames()
		s.expectEdgeNames = h.oracleEdgeNames()
	}
	h.step = fmt.Sprintf("BEGIN readOnly=%v ops=%d", s.readOnly, s.remaining)
	// Session-level read-your-own-writes: a name this session committed
	// earlier must be visible to its new transaction (probed only while the
	// committed model still holds it).
	return h.probeCrossTxRYOW(s)
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
	// scalar captures the first row's first integer column, so a count read
	// can be adjudicated by the isolation checkers without re-running it.
	var scalar int64
	var haveScalar bool
	if err == nil {
		if res.Next() {
			if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
				scalar, haveScalar = int64(iv), true
			}
		}
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
	// Isolation probes (rmp #2436) — pure observations through the same
	// handle, after the workspace mirrored the statement.
	if kind == OpMatch && params == nil {
		h.adjudicateCountRead(s, cypherText, scalar, haveScalar)
		return h.probePairs(s)
	}
	if !s.readOnly {
		if err := h.afterWrite(s, kind, cypherText, params); err != nil {
			return err
		}
	}
	return nil
}

// afterWrite runs the post-statement bookkeeping of a successful write:
// registers a completed atomic-visibility pair and probes read-your-own-writes
// through the transaction's own handle.
func (h *mvccHarness) afterWrite(s *mvccSessionState, kind OpKind, cypherText string, params map[string]any) error {
	if name, _ := params["name"].(string); kind == OpCreate && name != "" && name == s.pairSecond {
		s.pairsDone = append(s.pairsDone, [2]string{s.pairFirst, name})
		s.pairFirst, s.pairSecond = "", ""
	}
	return h.probeRYOW(s, kind, cypherText, params)
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
	// The second member of an in-flight atomic-visibility pair is emitted
	// before any new draw, so a drawn pair always completes within the
	// transaction's statement budget (the first member is only drawn when at
	// least one more statement remains).
	if s.pairEmit != "" {
		name := s.pairEmit
		s.pairEmit = ""
		s.pairSecond = name
		return tmplCreatePerson, map[string]any{"name": name, "age": int64(s.rng.IntN(100))}, OpCreate
	}
	singleCreate := func() (string, map[string]any, OpKind) {
		name := fmt.Sprintf("mv-s%d-n%d", s.id, s.created)
		s.created++
		return tmplCreatePerson, map[string]any{"name": name, "age": int64(s.rng.IntN(100))}, OpCreate
	}
	names := s.otx.NodeNames()
	roll := s.rng.Float64()
	switch {
	case roll < 0.32 || len(names) == 0:
		return singleCreate()
	case roll < 0.40:
		// Atomic-visibility pair (rmp #2436): two Person nodes created by two
		// statements of ONE transaction. Committed pairs are invariant-bearing:
		// every reader must observe both members or neither, so pair members
		// are excluded from DETACH DELETE below. Needs one more statement in
		// the budget for the second member; otherwise fall back to a single
		// create (the roll was consumed either way, keeping the op stream a
		// pure function of the seed).
		if s.remaining < 1 {
			return singleCreate()
		}
		k := s.created
		s.created++
		first := fmt.Sprintf("mv-s%d-pa%d", s.id, k)
		s.pairFirst = first
		s.pairEmit = fmt.Sprintf("mv-s%d-pb%d", s.id, k)
		return tmplCreatePerson, map[string]any{"name": first, "age": int64(s.rng.IntN(100))}, OpCreate
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
		// DETACH DELETE targets exclude atomic-visibility pair members, whose
		// both-or-neither invariant an individual delete would legitimately
		// break. With no deletable target, fall back to a read.
		targets := names[:0:0]
		for _, n := range names {
			if !isPairName(n) {
				targets = append(targets, n)
			}
		}
		if len(targets) == 0 {
			q := mvccReadTemplates[s.rng.IntN(len(mvccReadTemplates))]
			return q, nil, OpMatch
		}
		return tmplDetachDelete, map[string]any{"name": targets[s.rng.IntN(len(targets))]}, OpDelete
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
	s.clearPairState()
	if s.doomSuspect != "" {
		// The suspect transaction surfaced its refused write as a conflict —
		// exactly what the doomed-tx contract (rmp #2354) requires.
		h.res.TxDoomed++
		s.doomSuspect = ""
	}
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
		s.clearPairState()
		// A voluntary rollback discards a doom suspect with the transaction:
		// nothing it wrote (refused or not) is observable afterwards.
		s.doomSuspect = ""
		h.res.TxRolledBack++
		h.step = "ROLLBACK"
		return nil
	}
	err := s.tx.Commit()
	switch {
	case err == nil:
		if s.doomSuspect != "" {
			// The engine refused one of this transaction's writes mid-flight
			// (the read-back diverged) and then ACKNOWLEDGED the commit: a
			// silently lost write. This is the violation the doomed-tx
			// contract (rmp #2354) forbids.
			h.violate("read-your-own-writes",
				"session %d: tx COMMITTED cleanly after an own write read back wrong (%s) — a refused write must doom the transaction",
				s.id, s.doomSuspect)
			s.doomSuspect = ""
		}
		if foldErr := s.otx.Commit(); foldErr != nil {
			// The engine acknowledged a commit the model cannot fold: either an
			// isolation defect (a collision the engine should have refused) or a
			// harness modelling gap. Recorded as a finding, never swallowed. The
			// workspace is discarded so the run's terminal state stays defined.
			h.res.FoldErrors = append(h.res.FoldErrors,
				fmt.Sprintf("session %d: engine committed but oracle refused: %v", s.id, foldErr))
			s.otx.Abort()
		} else {
			// The fold advanced the committed model: date it, register the
			// transaction's completed pairs as committed (atomic-visibility
			// probes adjudicate against the fold sequence), and remember one
			// committed created name for the session's next cross-tx
			// read-your-own-writes probe.
			h.foldSeq++
			for _, p := range s.pairsDone {
				h.pairs = append(h.pairs, mvccPair{a: p[0], b: p[1], foldSeq: h.foldSeq})
				h.res.PairsCommitted++
			}
			if created := s.otx.CreatedNames(); len(created) > 0 {
				s.lastCommitted = created[0]
			}
		}
	case errors.Is(err, mvcc.ErrSerializationConflict):
		s.otx.Abort()
		s.otx = nil
		s.tx = nil
		s.clearPairState()
		if s.doomSuspect != "" {
			h.res.TxDoomed++
			s.doomSuspect = ""
		}
		h.res.TxConflicted++
		h.step = "COMMIT -> CONFLICT"
		return nil
	default:
		return fmt.Errorf("sim: session %d COMMIT: %w", s.id, err)
	}
	s.otx = nil
	s.tx = nil
	s.clearPairState()
	h.res.TxCommitted++
	h.step = "COMMIT ok"
	return nil
}
