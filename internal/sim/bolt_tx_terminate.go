package sim

// bolt_tx_terminate.go — arm 2 of the Bolt transaction-registry surface
// (rmp #2482): what server.Server.TerminateTransaction ends, what it REFUSES,
// and what an operator termination leaves behind.
//
// It shares the abandoned-registry arm's machinery — [txClockProbe],
// [waitForTxRegistry], [settleTxRegistry], [BoltTxWriteWindow] and the pinned
// reaper failure text — and adds nothing of its own that arm 1 already provides.
//
// # Why it runs on its OWN server, and why it never advances the clock
//
// The whole arm rests on ONE claim: every departure from the registry is the
// operator's doing. Two things could make that false, and both are removed by
// construction rather than argued away:
//
//   - a reaper firing. effectiveTxDeadline (bolt/server/serve.go:1155) reaps at
//     the EARLIER of the total and the idle bound, so BOTH are installed at
//     [txTerminateBound] — ten minutes of FAKE time — and the arm makes ZERO
//     advances. clock.Fake.NewTimer only registers a waiter at now+d
//     (internal/clock/fake.go:66-72); nothing is ever delivered until Advance is
//     called. With no advance at all, no timer this server armed can fire, and
//     the bound's size is belt and braces rather than the argument.
//   - arm 1's timers. A shared server would mix the two arms' timer counts, and
//     the counted-timer barrier is how each arm knows a transaction's reaper is
//     armed. Arm 2 therefore builds its own SimStore, its own SimServer and its
//     own probe.
//
// # Why this arm compares the listing as a SET and arm 1 compares it as a LIST
//
// MEASURED, not assumed: with zero advances every transaction registers at the
// SAME fake instant, and txRegistry.list sorts by StartedAt with an insertion
// sort that swaps only on a strict Before (bolt/server/txregistry.go:190-194).
// Equal keys are therefore left in Go map-iteration order, which is randomised
// per range. Asserting an ORDER here would be asserting the map. Arm 1 staggers
// its opens one advance apart precisely so that the oldest-first order IS
// deterministic, and that is where the order clause lives; here the listing is
// adjudicated as a set and rendered sorted so a report stays byte-identical.
//
// # What "successor immunity" rests on, and which half this arm exercises
//
// txRegistry documents that a stale id can never terminate whatever transaction
// the same connection opened next. Two mechanisms implement it, both read off
// the code:
//
//  1. txRegistry.nextID mints "<sessionID>-<seq>" from a server-wide counter that
//     only ever increases (bolt/server/txregistry.go:120-125), so the successor's
//     id differs from its predecessor's even on one connection.
//  2. Session.unregisterTx drains any terminate request that arrived for the
//     transaction just ended (bolt/server/session.go:744-751), so a signal queued
//     against a transaction that finished by another route cannot be observed
//     against the next one.
//
// This arm exercises (1) directly: the stale id is refused by the registry
// lookup, which sends no signal at all. It cannot CONSTRUCT the interleaving (2)
// exists for — the signal would have to be queued while the session is inside
// HandleMessage, which is a scheduler outcome and not something the harness can
// force — so what it does instead is assert the OBSERVABLE property across a
// settle window: the successor must still be listed after the stale terminate.
// A stale signal that had survived would roll the successor back, and this arm
// would see it go.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// boltTxArmTerminate names the arm in every violation message.
const boltTxArmTerminate = "operator-terminate"

// The virtual geometry. Both bounds are the same because the arm needs neither:
// it makes no advance, so neither can fire. See the file comment.
const (
	// txTerminateBound is installed as BOTH server.Options.MaxTxIdleTime and
	// server.Options.DefaultTxTimeout.
	txTerminateBound = 10 * time.Minute
	// txTerminateBoundFloor is the minimum either bound may be for the arm's
	// attribution clause to be legible. It is a documentation floor, not the
	// mechanism: zero advances is the mechanism.
	txTerminateBoundFloor = 1 * time.Minute
)

// The labels the arm writes under. Three, so the three censuses cannot be
// confused with one another.
const (
	// txTerminateGhostLabel is carried by the node the VICTIM transaction stages
	// and never commits. It must be absent from the live engine and from a graph
	// reopened through real recovery: a termination is a rollback.
	txTerminateGhostLabel = "TxTerminateGhost"
	// txTerminateHonestLabel is carried by the autocommit writes that bracket the
	// termination. They are what proves the WAL frame counter moves at all, so
	// "the termination appended nothing" is a statement about the server.
	txTerminateHonestLabel = "TxTerminateHonest"
	// txTerminateCommittedLabel is carried by the write the COMMITTED transaction
	// makes inside an explicit transaction and then commits. It is the other
	// non-vacuity witness: it proves an explicit transaction on this server CAN
	// reach the store, so "no ghost survived" is not satisfied by a path that
	// could never have written.
	txTerminateCommittedLabel = "TxTerminateCommitted"
)

// The three roles in the id ledger, in the order they are opened.
const (
	// txTermRoleVictim is terminated while live, and its connection is then
	// reused for the successor.
	txTermRoleVictim = "victim"
	// txTermRoleBystander must be untouched by every terminate call in the arm.
	txTermRoleBystander = "bystander"
	// txTermRoleCommitted ends by a client COMMIT, which is what makes its id a
	// stale one the harness WATCHED go live and then finish — strictly stronger
	// than the never-seen id bolt/server/tx_introspection_test.go already covers.
	txTermRoleCommitted = "committed"
)

// txTerminateRoles is the ledger roster in the order it must be opened. The
// order fixes each row's predicted id suffix, because txRegistry.nextID's
// sequence is per SERVER and only an explicit BEGIN consumes one (registerTx is
// reached from handleBegin alone; every honest write in this arm is autocommit).
var txTerminateRoles = []string{txTermRoleVictim, txTermRoleBystander, txTermRoleCommitted}

// txTerminateSeedMix decorrelates the SimDisk sub-seed from the arm seed.
const txTerminateSeedMix = 0x2482_7E12

// txTerminateOutcome classifies what server.Server.TerminateTransaction
// returned. It is a CLASSIFICATION rather than the error text on purpose: the
// error wraps the id (bolt/server/txregistry.go:210), whose prefix is a
// crypto/rand session id, so recording the text would leak a non-reproducible
// value into every report.
type txTerminateOutcome string

const (
	// txTermOutcomeOK is a nil return: the request was delivered.
	txTermOutcomeOK txTerminateOutcome = "nil"
	// txTermOutcomeNoSuchTx is an error wrapping server.ErrNoSuchTransaction.
	txTermOutcomeNoSuchTx txTerminateOutcome = "ErrNoSuchTransaction"
	// txTermOutcomeOther is any other error, which the contract has no name for.
	txTermOutcomeOther txTerminateOutcome = "other-error"
)

// classifyTerminate reduces a TerminateTransaction return to a reproducible
// outcome.
func classifyTerminate(err error) txTerminateOutcome {
	switch {
	case err == nil:
		return txTermOutcomeOK
	case errors.Is(err, server.ErrNoSuchTransaction):
		return txTermOutcomeNoSuchTx
	default:
		return txTermOutcomeOther
	}
}

// The names of the terminate calls the arm makes, in order.
const (
	// txTermCallLive terminates a transaction the registry is listing.
	txTermCallLive = "live"
	// txTermCallStaleTerminated re-terminates the id just terminated.
	txTermCallStaleTerminated = "stale-terminated"
	// txTermCallStaleCommitted terminates an id the harness watched go live and
	// then finish by a client COMMIT.
	txTermCallStaleCommitted = "stale-committed"
	// txTermCallStaleVsSuccessor terminates the victim's ORIGINAL id once the same
	// connection holds a NEW transaction. It is the successor-immunity call.
	txTermCallStaleVsSuccessor = "stale-vs-successor"
)

// txTerminateCalls is the call roster in the order it must be driven.
var txTerminateCalls = []string{
	txTermCallLive, txTermCallStaleTerminated, txTermCallStaleCommitted, txTermCallStaleVsSuccessor,
}

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// BoltTxTerminateRow is one transaction in the harness's own id ledger — the
// independent reference every listing comparison is made against. It carries
// what the harness SENT and what it PREDICTED, never a verdict.
type BoltTxTerminateRow struct {
	// Role is the row's part in the arm: one of [txTerminateRoles].
	Role string
	// Principal is the identity the connection authenticated as, and the only
	// field that can attribute a listing entry to a connection (Remote is the same
	// constant on every SimConn).
	Principal string
	// Mode is the BEGIN access mode, "r" or "w", exactly as sent.
	Mode string
	// Query is the single statement run inside the transaction.
	Query string
	// IDSuffix is the per-server sequence suffix taken from the OBSERVED id;
	// WantSuffix is the suffix the harness predicted from the BEGIN's ordinal on
	// this server. The id's prefix is crypto/rand and is never recorded.
	IDSuffix   string
	WantSuffix string
	// Conn is the connection's index in the ledger, 0-based.
	Conn int
}

// BoltTxTerminateCall is one server.Server.TerminateTransaction call and what it
// returned, classified so the record is reproducible.
type BoltTxTerminateCall struct {
	// Name is the call's place in the roster: one of [txTerminateCalls].
	Name string
	// Target is the id SUFFIX the call aimed at.
	Target string
	// Got is what TerminateTransaction returned; Want is what the contract
	// requires for this call.
	Got  txTerminateOutcome
	Want txTerminateOutcome
}

// BoltTxTerminateEvidence is everything one run of the operator-termination arm
// observed, and NO verdict. The checkers are pure functions of it.
//
// Every field is seed-pure except Nows, which counts how many times the HARNESS
// polled the registry (txRegistry.list reads the clock once per call,
// bolt/server/txregistry.go:174) and is therefore scheduling-dependent, and the
// honest windows' byte deltas, whose frame payload embeds a node key drawn from
// a process-global counter in cypher/exec. Neither is rendered as a number by
// [BoltTxTerminateEvidence.String].
type BoltTxTerminateEvidence struct {
	// Arm names the arm; Seed is the seed it was built from.
	Arm  string
	Seed uint64

	// IdleBound and TotalBound are the two bounds installed on the SERVER, and
	// Advances is how many times the arm advanced the fake clock. The arm's whole
	// attribution rests on Advances being zero.
	IdleBound  time.Duration
	TotalBound time.Duration
	Advances   int

	// Begins counts the explicit BEGINs the arm made; TimersArmed, Untils and
	// Tickers are the counted registrations on the injected clock, and Nows the
	// read count (not seed-pure; see the type comment).
	Begins      int
	TimersArmed int64
	Untils      int64
	Tickers     int64
	Nows        int64

	// Plan is the harness's id ledger, in the order it opened the transactions.
	Plan []BoltTxTerminateRow

	// ListedAtCap is every id suffix the registry held with the whole ledger open;
	// ListedAfterTerminate is what it held once the victim had gone. Both are
	// SORTED, not in listing order — see the file comment on why the order is not
	// deterministic in an arm that never advances the clock.
	ListedAtCap          []string
	ListedAfterTerminate []string
	// RegistryPeak is the largest listing the run observed.
	RegistryPeak int

	// Calls are the terminate calls in the order they were made.
	Calls []BoltTxTerminateCall

	// SuccessorSuffix is the id suffix of the SECOND transaction the victim's
	// connection opened; WantSuccessorSuffix is what the harness predicted from
	// its BEGIN ordinal. SuccessorListed and BystanderListed record whether each
	// was still listed after the stale-vs-successor terminate had settled.
	SuccessorSuffix     string
	WantSuccessorSuffix string
	SuccessorListed     bool
	BystanderListed     bool

	// VictimCode and VictimMessage are what the terminated connection was told on
	// the one message sent to it after the termination. They are what attributes
	// its departure to the operator call rather than to a rollback or a dropped
	// connection — and they are adjudicated against [txReapFailureCode] and
	// [txReapFailureMessage], BOTH of whose halves are false for an operator
	// termination (rmp #2560).
	VictimCode    string
	VictimMessage string

	// TermFrames and TermBytes bracket the termination itself with [wal.Stats].
	// A rollback must append neither.
	TermFrames uint64
	TermBytes  uint64

	// Windows are the honest autocommit writes that bracket the termination.
	Windows []BoltTxWriteWindow

	// GhostsLive/GhostsRecovered count [txTerminateGhostLabel] in the live engine
	// and after a crash and a real WAL replay; Honest* and Committed* do the same
	// for the two witness labels.
	GhostsLive         int
	GhostsRecovered    int
	HonestLive         int
	HonestRecovered    int
	CommittedLive      int
	CommittedRecovered int
}

// -----------------------------------------------------------------------------
// The arm
// -----------------------------------------------------------------------------

// RunBoltTxTerminate drives the operator-termination arm once and returns the
// evidence. It is bit-reproducible from seed: the whole arm runs on ONE
// goroutine and makes no seeded choice at all — the ledger's roles, modes and
// statements are fixed — so the seed reaches only the SimDisk's (unused) fault
// stream.
//
// The returned error is reserved for harness failures. A terminate that ended
// the wrong transaction, a stale id that was accepted, or a ghost that survived
// recovery is EVIDENCE, not an error.
func RunBoltTxTerminate(ctx context.Context, seed uint64) (*BoltTxTerminateEvidence, error) {
	opts := defaultBoltTxTerminateOptions()
	return runBoltTxTerminate(ctx, seed, &opts)
}

// boltTxTerminateOptions parameterises the arm. The zero value is not usable;
// [defaultBoltTxTerminateOptions] builds the nominal geometry and a live control
// varies exactly one field of it.
type boltTxTerminateOptions struct {
	// TargetRole is the ledger role the FIRST terminate call aims at, and the one
	// the arm then waits to see leave the registry.
	//
	// The arm proper aims at [txTermRoleVictim]. A live control aims at
	// [txTermRoleBystander] instead, changing nothing else: the wrong transaction
	// then ends, the victim survives, and every clause that claims to measure
	// WHICH transaction a termination ended has to fire. Without that control,
	// "the victim left and the others stayed" would be equally true of a server
	// that emptied its registry on any terminate call at all.
	TargetRole string
}

// defaultBoltTxTerminateOptions returns the arm's nominal configuration.
func defaultBoltTxTerminateOptions() boltTxTerminateOptions {
	return boltTxTerminateOptions{TargetRole: txTermRoleVictim}
}

// runBoltTxTerminate is [RunBoltTxTerminate] with the target made explicit, so a
// live control can vary it and drive the same real server through the same real
// wire.
func runBoltTxTerminate(ctx context.Context, seed uint64, opts *boltTxTerminateOptions) (*BoltTxTerminateEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^txTerminateSeedMix), 0) // faultRate 0: this arm faults nothing
	cfg := durableStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-terminate open store: %w", err)
	}

	probe := newTxClockProbe()
	// The listener keeps REAL time; see [NewSimServerTxRegistry] for why the two
	// clocks must be different objects.
	srv, err := NewSimServerTxRegistry(st.Engine(), clock.Real(), probe,
		txTerminateBound, txTerminateBound, 0)
	if err != nil {
		st.Crash()
		return nil, fmt.Errorf("sim: bolt-tx-terminate server: %w", err)
	}

	r := &boltTxTerminateRunner{
		srv: srv, st: st, probe: probe, opts: opts,
		ids: make(map[string]string, len(txTerminateRoles)),
		ev: &BoltTxTerminateEvidence{
			Arm: boltTxArmTerminate, Seed: seed,
			IdleBound: txTerminateBound, TotalBound: txTerminateBound,
		},
	}
	if derr := r.drive(ctx); derr != nil {
		r.closeConns()
		_ = srv.Close()
		st.Crash()
		return nil, derr
	}
	r.closeConns()
	_ = srv.Close()

	// Crash (drop the engine, keep the SimDisk image) and reopen through real
	// recovery: a ghost frame that reached the durable log without ever being
	// visible live is invisible to the live census and surfaces only here.
	st.Crash()
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-terminate reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	if cerr := r.censusRecovered(ctx, st2); cerr != nil {
		return nil, cerr
	}
	return r.ev, nil
}

// boltTxTerminateRunner threads the server, the store, the clock probe and the
// accumulating evidence through the arm's phases. Exactly one goroutine drives
// it.
type boltTxTerminateRunner struct {
	srv    *SimServer
	st     *SimStore
	probe  *txClockProbe
	opts   *boltTxTerminateOptions
	ev     *BoltTxTerminateEvidence
	honest *WireClient
	conns  []*WireClient
	// ids maps a ledger role to the RAW registry id, which TerminateTransaction
	// needs and no report may ever contain.
	ids map[string]string
}

// closeConns closes every client the run opened. Idempotent.
func (r *boltTxTerminateRunner) closeConns() {
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.conns = nil
	if r.honest != nil {
		_ = r.honest.Close()
		r.honest = nil
	}
}

// drive runs the arm's phases in order.
func (r *boltTxTerminateRunner) drive(ctx context.Context) error {
	hc, err := r.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate dial honest: %w", err)
	}
	r.honest = hc
	if cerr := hc.Connect(ctx); cerr != nil {
		return fmt.Errorf("sim: bolt-tx-terminate connect honest: %w", cerr)
	}
	if werr := r.honestWindow(txWindowBefore); werr != nil {
		return werr
	}
	if oerr := r.openLedger(ctx); oerr != nil {
		return oerr
	}
	r.ev.ListedAtCap = txSortedSuffixes(r.srv.Server().Transactions())

	if terr := r.terminateLive(); terr != nil {
		return terr
	}
	if perr := r.probeVictim(); perr != nil {
		return perr
	}
	r.call(txTermCallStaleTerminated, txTermRoleVictim, txTermOutcomeNoSuchTx)

	if cerr := r.commitThenTerminate(); cerr != nil {
		return cerr
	}
	if serr := r.successor(); serr != nil {
		return serr
	}
	if werr := r.honestWindow(txWindowAfter); werr != nil {
		return werr
	}

	r.ev.Begins = len(r.ev.Plan) + 1 // the ledger plus the successor
	r.ev.TimersArmed = r.probe.Timers()
	r.ev.Untils = r.probe.Untils()
	r.ev.Tickers = r.probe.Tickers()
	if cerr := r.censusLive(); cerr != nil {
		return cerr
	}
	// Read last, so the count covers every registry listing the run took.
	r.ev.Nows = r.probe.Nows()
	return nil
}

// terminateRole is the ledger row a role's statement and mode come from.
type terminateRole struct {
	mode  string
	query string
}

// terminateRoleSpec returns the mode and statement for a ledger role.
//
// The victim WRITES and never commits, which is the ghost the atomicity clause
// hunts for. The bystander is READ-ONLY, so the arm's "no other id left" clause
// is asserted for a kind of transaction that holds no write state at all. The
// committed one writes and COMMITS, which is the witness that an explicit
// transaction on this server can reach the store.
func terminateRoleSpec(role string) terminateRole {
	switch role {
	case txTermRoleBystander:
		return terminateRole{
			mode:  "r",
			query: fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", txTerminateHonestLabel),
		}
	case txTermRoleCommitted:
		return terminateRole{
			mode:  "w",
			query: fmt.Sprintf("CREATE (:%s {name: %q})", txTerminateCommittedLabel, "committed-write"),
		}
	default:
		return terminateRole{
			mode:  "w",
			query: fmt.Sprintf("CREATE (:%s {name: %q})", txTerminateGhostLabel, "victim-ghost"),
		}
	}
}

// openLedger opens one transaction per role, each on its own connection under
// its own principal, and records the harness's independent ledger.
func (r *boltTxTerminateRunner) openLedger(ctx context.Context) error {
	r.ev.Plan = make([]BoltTxTerminateRow, 0, len(txTerminateRoles))
	for k, role := range txTerminateRoles {
		spec := terminateRoleSpec(role)
		principal := "tx-term-" + role
		c, err := r.srv.Dial()
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-terminate dial %s: %w", role, err)
		}
		r.conns = append(r.conns, c)
		authResp, err := c.ConnectAs(ctx, principal, "")
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-terminate auth %q: %w", principal, err)
		}
		if !isSuccess(authResp) {
			return fmt.Errorf("sim: bolt-tx-terminate auth %q refused: %s %s",
				principal, failureCode(authResp), failureMessage(authResp))
		}
		if berr := r.beginAndRun(c, spec.mode, spec.query, role); berr != nil {
			return berr
		}
		// THE barrier, in the message loop's own order: the entry is listed first,
		// the timer armed strictly later (see the bolt_tx_registry.go file comment).
		// This arm never advances, so the arming is not a reap barrier here — it is
		// the evidence that the reaper WAS armed, which is what makes "no advance,
		// so no reaper could fire" a statement about a live reaper.
		want := k + 1
		if werr := waitForTxRegistry(func() bool { return len(r.srv.Server().Transactions()) == want },
			fmt.Sprintf("the registry never listed %d open transaction(s) after the %s BEGIN", want, role)); werr != nil {
			return werr
		}
		if werr := waitForTxRegistry(func() bool { return r.probe.Timers() >= int64(want) },
			fmt.Sprintf("the server never armed timer %d on the injected clock", want)); werr != nil {
			return werr
		}

		txs := r.srv.Server().Transactions()
		r.notePeak(len(txs))
		idx := slices.IndexFunc(txs, func(t server.TransactionInfo) bool { return t.Principal == principal })
		if idx < 0 {
			return fmt.Errorf("sim: bolt-tx-terminate: the registry lists %d transaction(s) but none for %q",
				len(txs), principal)
		}
		r.ids[role] = txs[idx].ID
		r.ev.Plan = append(r.ev.Plan, BoltTxTerminateRow{
			Role: role, Principal: principal, Mode: spec.mode, Query: spec.query,
			IDSuffix: txIDSuffix(txs[idx].ID), WantSuffix: fmt.Sprintf("-%d", want), Conn: k,
		})
	}
	return nil
}

// beginAndRun sends BEGIN in the given mode and runs one statement inside it.
func (r *boltTxTerminateRunner) beginAndRun(c *WireClient, mode, query, role string) error {
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	beginResp, err := c.BeginMode(mode)
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate BEGIN %s: %w", role, err)
	}
	if !isSuccess(beginResp) {
		return fmt.Errorf("sim: bolt-tx-terminate BEGIN %s refused: %s %s",
			role, failureCode(beginResp), failureMessage(beginResp))
	}
	if rerr := runInOpenTx(c, query); rerr != nil {
		return fmt.Errorf("sim: bolt-tx-terminate statement for %s: %w", role, rerr)
	}
	return clearTxReadDeadline(c)
}

// terminateLive terminates the victim while the registry is listing it, brackets
// the call with the WAL counters, and records which ids survived it.
func (r *boltTxTerminateRunner) terminateLive() error {
	framesBefore, bytesBefore := r.walCounters()
	r.call(txTermCallLive, r.opts.TargetRole, txTermOutcomeOK)

	target := txIDSuffix(r.ids[r.opts.TargetRole])
	if werr := waitForTxRegistry(func() bool {
		return !slices.Contains(txSuffixes(r.srv.Server().Transactions()), target)
	}, "the terminated transaction never left the registry"); werr != nil {
		return werr
	}
	txs := settleTxRegistry(r.srv)
	r.notePeak(len(txs))
	r.ev.ListedAfterTerminate = txSortedSuffixes(txs)

	framesAfter, bytesAfter := r.walCounters()
	r.ev.TermFrames = framesAfter - framesBefore
	r.ev.TermBytes = bytesAfter - bytesBefore
	return nil
}

// call makes one TerminateTransaction against a ledger role's id and records the
// classified outcome against what the contract requires.
func (r *boltTxTerminateRunner) call(name, role string, want txTerminateOutcome) {
	id := r.ids[role]
	r.ev.Calls = append(r.ev.Calls, BoltTxTerminateCall{
		Name:   name,
		Target: txIDSuffix(id),
		Got:    classifyTerminate(r.srv.Server().TerminateTransaction(id)),
		Want:   want,
	})
}

// probeVictim sends ONE statement down the terminated connection and records
// what the server answered. An absent registry entry is an ambiguous post-state
// on its own — a rollback, a dropped connection or a harness mistake all produce
// it — and only Session.reapTimedOutTx arms the typed FAILURE recorded here.
func (r *boltTxTerminateRunner) probeVictim() error {
	c := r.conns[0]
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	resp, err := c.Run("RETURN 1", nil)
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate victim probe: %w", err)
	}
	r.ev.VictimCode, r.ev.VictimMessage = failureCode(resp), failureMessage(resp)
	return clearTxReadDeadline(c)
}

// commitThenTerminate lets the committed role finish by a client COMMIT, waits
// for its entry to go, and then terminates its id — the stale case that is
// strictly stronger than a never-seen id, because the harness WATCHED this one
// be live.
func (r *boltTxTerminateRunner) commitThenTerminate() error {
	c := r.conns[2]
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	resp, err := c.Commit()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate COMMIT: %w", err)
	}
	if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-tx-terminate COMMIT refused: %s %s", failureCode(resp), failureMessage(resp))
	}
	if derr := clearTxReadDeadline(c); derr != nil {
		return derr
	}
	committed := txIDSuffix(r.ids[txTermRoleCommitted])
	if werr := waitForTxRegistry(func() bool {
		return !slices.Contains(txSuffixes(r.srv.Server().Transactions()), committed)
	}, "the committed transaction never left the registry"); werr != nil {
		return werr
	}
	r.call(txTermCallStaleCommitted, txTermRoleCommitted, txTermOutcomeNoSuchTx)
	return nil
}

// successor reuses the victim's connection for a SECOND transaction and then
// aims the victim's ORIGINAL id at it.
//
// The RESET is required and is not incidental: reapTimedOutTx left the session
// in FAILED with a pending termination failure, and only RESET clears both
// (bolt/server/session.go:1017 and handleReset's transition). RESET does not
// de-authorise — only LOGOFF does — so the BEGIN that follows is still
// authenticated.
func (r *boltTxTerminateRunner) successor() error {
	c := r.conns[0]
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	resetResp, err := c.Reset()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate RESET: %w", err)
	}
	if !isSuccess(resetResp) {
		return fmt.Errorf("sim: bolt-tx-terminate RESET refused: %s %s",
			failureCode(resetResp), failureMessage(resetResp))
	}
	beginResp, err := c.BeginMode("w")
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-terminate successor BEGIN: %w", err)
	}
	if !isSuccess(beginResp) {
		return fmt.Errorf("sim: bolt-tx-terminate successor BEGIN refused: %s %s",
			failureCode(beginResp), failureMessage(beginResp))
	}
	if derr := clearTxReadDeadline(c); derr != nil {
		return derr
	}

	principal := r.ev.Plan[0].Principal
	if werr := waitForTxRegistry(func() bool {
		return slices.ContainsFunc(r.srv.Server().Transactions(),
			func(t server.TransactionInfo) bool { return t.Principal == principal })
	}, "the successor transaction never reached the registry"); werr != nil {
		return werr
	}
	txs := r.srv.Server().Transactions()
	r.notePeak(len(txs))
	idx := slices.IndexFunc(txs, func(t server.TransactionInfo) bool { return t.Principal == principal })
	r.ev.SuccessorSuffix = txIDSuffix(txs[idx].ID)
	// The ledger's three BEGINs plus this one: txRegistry.nextID's sequence is per
	// SERVER, and only handleBegin consumes one.
	r.ev.WantSuccessorSuffix = fmt.Sprintf("-%d", len(r.ev.Plan)+1)

	r.call(txTermCallStaleVsSuccessor, txTermRoleVictim, txTermOutcomeNoSuchTx)

	// Settled, not sampled: a stale terminate signal that had survived the
	// predecessor would roll the successor back a moment later, and the whole
	// point of this clause is to give that a window in which to happen.
	after := txSuffixes(settleTxRegistry(r.srv))
	r.ev.SuccessorListed = slices.Contains(after, r.ev.SuccessorSuffix)
	r.ev.BystanderListed = slices.Contains(after, txIDSuffix(r.ids[txTermRoleBystander]))
	return nil
}

// honestWindow drives one autocommit write on the honest connection, bracketed
// by the WAL counters. It is AUTOCOMMIT so it registers no transaction and arms
// no timer of its own, leaving the arm's counted registrations attributable to
// the ledger alone.
func (r *boltTxTerminateRunner) honestWindow(name string) error {
	open := len(r.srv.Server().Transactions())
	r.notePeak(open)
	w := BoltTxWriteWindow{Name: name, Node: "honest-" + name, OpenTx: open}
	w.FramesBefore, w.BytesBefore = r.walCounters()
	err := r.runAutocommit(fmt.Sprintf("CREATE (:%s {name: %q})", txTerminateHonestLabel, w.Node))
	w.FramesAfter, w.BytesAfter = r.walCounters()
	if err != nil {
		if !isWireRefusal(err) {
			return fmt.Errorf("sim: bolt-tx-terminate honest window %q: %w", name, err)
		}
	} else {
		w.Committed = true
	}
	r.ev.Windows = append(r.ev.Windows, w)
	return nil
}

// runAutocommit runs one statement outside any explicit transaction on the
// honest connection and drains it.
func (r *boltTxTerminateRunner) runAutocommit(query string) error {
	if err := armTxReadDeadline(r.honest); err != nil {
		return err
	}
	if _, err := wireQuery(r.honest, query, nil); err != nil {
		return err
	}
	return clearTxReadDeadline(r.honest)
}

// walCounters reads the live WAL frame/byte counters.
func (r *boltTxTerminateRunner) walCounters() (frames, bytes uint64) {
	s := r.st.WAL().Stats()
	return s.Frames, s.Bytes
}

// notePeak folds one listing size into the running maximum.
func (r *boltTxTerminateRunner) notePeak(n int) {
	if n > r.ev.RegistryPeak {
		r.ev.RegistryPeak = n
	}
}

// censusLive counts all three labels in the LIVE engine, over the wire on the
// honest connection.
func (r *boltTxTerminateRunner) censusLive() error {
	var err error
	if r.ev.GhostsLive, err = countLabelOverWire(r.honest, txTerminateGhostLabel); err != nil {
		return err
	}
	if r.ev.HonestLive, err = countLabelOverWire(r.honest, txTerminateHonestLabel); err != nil {
		return err
	}
	r.ev.CommittedLive, err = countLabelOverWire(r.honest, txTerminateCommittedLabel)
	return err
}

// censusRecovered counts all three labels on a reopened store, after recovery
// has replayed the WAL.
func (r *boltTxTerminateRunner) censusRecovered(ctx context.Context, st *SimStore) error {
	var err error
	if r.ev.GhostsRecovered, err = countLabelViaEngine(ctx, st, txTerminateGhostLabel); err != nil {
		return err
	}
	if r.ev.HonestRecovered, err = countLabelViaEngine(ctx, st, txTerminateHonestLabel); err != nil {
		return err
	}
	r.ev.CommittedRecovered, err = countLabelViaEngine(ctx, st, txTerminateCommittedLabel)
	return err
}

// -----------------------------------------------------------------------------
// The oracles
// -----------------------------------------------------------------------------

// checkBoltTxTerminate adjudicates the evidence against the CONTRACT: what a
// termination ends, what it must refuse, and what it leaves behind. It is split
// from [checkBoltTxTerminateNonVacuity] so an uninformative run cannot read as a
// faulty one (rmp #2470).
//
// The receiver is a pointer because the value is far over the copy threshold; it
// mutates nothing.
func checkBoltTxTerminate(e *BoltTxTerminateEvidence) []Violation {
	return slices.Concat(
		checkBoltTxTerminateLedger(e),
		checkBoltTxTerminateCalls(e),
		checkBoltTxTerminateResidue(e),
	)
}

// checkBoltTxTerminateLedger adjudicates the registry's view: the ids the server
// minted, what it listed with the ledger open, and what survived the
// termination.
func checkBoltTxTerminateLedger(e *BoltTxTerminateEvidence) []Violation {
	var v []Violation
	for i := range e.Plan {
		p := &e.Plan[i]
		if p.IDSuffix != p.WantSuffix {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "ledger-id"),
				Message: fmt.Sprintf("the %s transaction was minted id suffix %s, want %s: txRegistry.nextID's sequence "+
					"is per SERVER and only a BEGIN consumes one, so this row is the %s BEGIN on it",
					p.Role, p.IDSuffix, p.WantSuffix, p.WantSuffix),
			})
		}
	}
	want := make([]string, 0, len(e.Plan))
	for i := range e.Plan {
		want = append(want, e.Plan[i].WantSuffix)
	}
	slices.Sort(want)
	if !slices.Equal(e.ListedAtCap, want) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-at-cap"),
			Message: fmt.Sprintf("with the whole ledger open the registry listed %v, want exactly %v "+
				"(compared as a SET: every entry shares one instant on a clock this arm never advances)",
				e.ListedAtCap, want),
		})
	}
	// Everything but the victim must survive, and this is the clause that
	// distinguishes "TerminateTransaction ended the transaction it was given" from
	// "TerminateTransaction emptied the registry".
	survivors := make([]string, 0, len(e.Plan))
	for i := range e.Plan {
		if e.Plan[i].Role != txTermRoleVictim {
			survivors = append(survivors, e.Plan[i].WantSuffix)
		}
	}
	slices.Sort(survivors)
	if !slices.Equal(e.ListedAfterTerminate, survivors) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-after-terminate"),
			Message: fmt.Sprintf("after terminating the victim the registry listed %v, want exactly %v: a termination "+
				"must end the transaction it was given and NO other", e.ListedAfterTerminate, survivors),
		})
	}
	if e.SuccessorSuffix != e.WantSuccessorSuffix {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "successor-id"),
			Message: fmt.Sprintf("the successor transaction on the victim's own connection was minted id suffix %s, "+
				"want %s: nextID's sequence only ever increases, which is half of why a stale id cannot reach it",
				e.SuccessorSuffix, e.WantSuccessorSuffix),
		})
	}
	if !e.SuccessorListed {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "successor-immunity"),
			Message: fmt.Sprintf("the successor transaction %s left the registry after its PREDECESSOR's id was "+
				"terminated: a stale id reached the transaction the same connection opened next",
				e.SuccessorSuffix),
		})
	}
	if !e.BystanderListed {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "bystander"),
			Message: "the read-only bystander left the registry although no terminate call ever named it, and this arm " +
				"makes no advance, so neither reaper can have taken it",
		})
	}
	return v
}

// checkBoltTxTerminateCalls adjudicates every TerminateTransaction return
// against the contract, and the client's reply against the pinned text.
func checkBoltTxTerminateCalls(e *BoltTxTerminateEvidence) []Violation {
	var v []Violation
	for i := range e.Calls {
		c := &e.Calls[i]
		if c.Got != c.Want {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "terminate:"+c.Name),
				Message: fmt.Sprintf("TerminateTransaction on the %s id (%s) returned %s, want %s",
					c.Name, c.Target, c.Got, c.Want),
			})
		}
	}
	if e.VictimCode != txReapFailureCode {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "terminate-attribution"),
			Message: fmt.Sprintf("the terminated connection was answered %q on its next message, want %q: only "+
				"Session.reapTimedOutTx arms that failure, so an absent entry alone does not prove a termination",
				e.VictimCode, txReapFailureCode),
		})
	} else if e.VictimMessage != txReapFailureMessage {
		// Pinned VERBATIM although BOTH halves are false for an operator
		// termination — it exceeded no timeout, and no writer lock has been held for
		// a transaction's lifetime since rmp #2305 (rmp #2560). Pinning it means the
		// eventual correction fails this arm on purpose instead of slipping through;
		// when that happens, update the arm with the ticket.
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "terminate-attribution"),
			Message: fmt.Sprintf("the terminated connection was told %q, want the text pinned in txReapFailureMessage "+
				"(rmp #2560: an OPERATOR termination is told it exceeded a timeout and released a writer lock, and "+
				"neither is true); if that text has been corrected, update this arm with the ticket", e.VictimMessage),
		})
	}
	return v
}

// checkBoltTxTerminateResidue adjudicates what a termination leaves behind: no
// ghost anywhere, no WAL frame charged to it, and every honest and committed
// write durable.
func checkBoltTxTerminateResidue(e *BoltTxTerminateEvidence) []Violation {
	var v []Violation
	if e.GhostsLive != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: txOp(e.Arm, "ghost-live"),
			Message: fmt.Sprintf("%d node(s) labelled %s exist in the live engine: a terminated transaction's "+
				"uncommitted write survived the rollback", e.GhostsLive, txTerminateGhostLabel),
		})
	}
	if e.GhostsRecovered != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "ghost-recovered"),
			Message: fmt.Sprintf("%d node(s) labelled %s survived WAL replay: a terminated transaction's uncommitted "+
				"write reached the durable log", e.GhostsRecovered, txTerminateGhostLabel),
		})
	}
	if e.TermFrames != 0 || e.TermBytes != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "terminate-wal"),
			Message: fmt.Sprintf("the termination appended %d WAL frame(s) and %d byte(s); a rollback must append neither",
				e.TermFrames, e.TermBytes),
		})
	}
	for i := range e.Windows {
		v = append(v, checkBoltTxWindow(e.Arm, &e.Windows[i])...)
	}
	if e.HonestLive != len(e.Windows) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "honest-census"),
			Message: fmt.Sprintf("the live engine holds %d node(s) labelled %s, want %d (one per honest window)",
				e.HonestLive, txTerminateHonestLabel, len(e.Windows)),
		})
	}
	if e.HonestRecovered != e.HonestLive {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "honest-recovered"),
			Message: fmt.Sprintf("WAL replay recovered %d node(s) labelled %s, but %d were acknowledged live",
				e.HonestRecovered, txTerminateHonestLabel, e.HonestLive),
		})
	}
	if e.CommittedLive != 1 {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: txOp(e.Arm, "committed-live"),
			Message: fmt.Sprintf("the live engine holds %d node(s) labelled %s, want 1: the transaction that COMMITTED "+
				"beside the terminated one must keep its write", e.CommittedLive, txTerminateCommittedLabel),
		})
	}
	if e.CommittedRecovered != e.CommittedLive {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "committed-recovered"),
			Message: fmt.Sprintf("WAL replay recovered %d node(s) labelled %s, but %d were acknowledged live",
				e.CommittedRecovered, txTerminateCommittedLabel, e.CommittedLive),
		})
	}
	if e.Tickers != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-ticker"),
			Message: fmt.Sprintf("the server registered %d ticker(s) on the injected clock, want 0", e.Tickers),
		})
	}
	if e.Untils != e.TimersArmed {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-until"),
			Message: fmt.Sprintf("Until was called %d time(s) against %d timer registration(s); syncTxTimer calls "+
				"clk.Until exactly once per arming (bolt/server/serve.go:1178), so these must match",
				e.Untils, e.TimersArmed),
		})
	}
	if e.TimersArmed != int64(e.Begins) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming"),
			Message: fmt.Sprintf("%d timer(s) were armed on the injected clock for %d BEGIN(s), want one each: a "+
				"re-armed or missing timer would mean the reaper this arm claims could not fire is not the one that ran",
				e.TimersArmed, e.Begins),
		})
	}
	return v
}

// checkBoltTxTerminateNonVacuity proves the run actually exercised the surface,
// and above all that the arm's ATTRIBUTION holds: with no advance on a fake
// clock no timer can fire, so every departure from the registry is the operator
// call's doing. A shortfall here says the RUN was uninformative, never that the
// server is faulty (rmp #2470).
func checkBoltTxTerminateNonVacuity(e *BoltTxTerminateEvidence) []Violation {
	var v []Violation
	shortfall := func(clause, msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: txOp(e.Arm, clause), Message: msg})
	}
	// THE attribution clause. clock.Fake delivers to a waiter only from Advance
	// (internal/clock/fake.go), so zero advances is a PROOF that neither reaper
	// fired, not an estimate.
	if e.Advances != 0 {
		shortfall("nonvacuity-attribution", fmt.Sprintf("the arm advanced the fake clock %d time(s); with any advance "+
			"at all a departure from the registry could be the idle or the total reaper's work and every clause that "+
			"attributes one to TerminateTransaction collapses", e.Advances))
	}
	if e.IdleBound < txTerminateBoundFloor || e.TotalBound < txTerminateBoundFloor {
		shortfall("nonvacuity-bound", fmt.Sprintf("the bounds are idle %s and total %s, below the %s floor: "+
			"effectiveTxDeadline reaps at the EARLIER of the two, so both must be far clear of the arm's own reach",
			e.IdleBound, e.TotalBound, txTerminateBoundFloor))
	}
	if e.TimersArmed == 0 || e.Nows == 0 {
		shortfall("nonvacuity-seam", fmt.Sprintf("the server registered %d timer(s) and made %d Now call(s) on the "+
			"injected clock: the SetClock seam never reached the session, so \"no advance, so no reaper could fire\" "+
			"is a statement about a reaper that was never armed", e.TimersArmed, e.Nows))
	}
	if len(e.Plan) != len(txTerminateRoles) {
		shortfall("nonvacuity-ledger", fmt.Sprintf("the run opened %d transaction(s), want %d (one per role): a "+
			"shorter ledger has no bystander for \"no other id left\" to be about", len(e.Plan), len(txTerminateRoles)))
	}
	if e.RegistryPeak < len(e.Plan) {
		shortfall("nonvacuity-peak", fmt.Sprintf("the registry never held more than %d transaction(s) although the "+
			"ledger opened %d: they were not all open together, so the termination had nothing to spare",
			e.RegistryPeak, len(e.Plan)))
	}
	if len(e.Calls) != len(txTerminateCalls) {
		shortfall("nonvacuity-calls", fmt.Sprintf("%d terminate call(s) were made, want %d (%v)",
			len(e.Calls), len(txTerminateCalls), txTerminateCalls))
	}
	var live, stale int
	for i := range e.Calls {
		if e.Calls[i].Want == txTermOutcomeOK {
			live++
			continue
		}
		stale++
	}
	if live == 0 || stale == 0 {
		shortfall("nonvacuity-calls", fmt.Sprintf("the roster made %d call(s) on a LIVE id and %d on a stale one: "+
			"without both, \"a live id is ended and a stale one is refused\" is half a claim", live, stale))
	}
	if e.VictimCode == "" {
		shortfall("nonvacuity-attribution", "the terminated connection was never probed, so the run cannot say WHY its "+
			"transaction left the registry")
	}
	var appended uint64
	for i := range e.Windows {
		appended += e.Windows[i].framesAppended()
	}
	if appended == 0 {
		shortfall("nonvacuity-wal", "no honest window appended a WAL frame: the frame counter is not a live instrument "+
			"here, so \"the termination appended none\" proves nothing")
	}
	if e.CommittedLive == 0 {
		shortfall("nonvacuity-write", fmt.Sprintf("no node labelled %s reached the engine: an explicit transaction on "+
			"this server never wrote anything, so \"no %s survived\" is satisfied by a path that could not have written",
			txTerminateCommittedLabel, txTerminateGhostLabel))
	}
	var readers, writers int
	for i := range e.Plan {
		if e.Plan[i].Mode == "r" {
			readers++
			continue
		}
		writers++
	}
	if readers == 0 || writers == 0 {
		shortfall("nonvacuity-mode", fmt.Sprintf("the ledger holds %d read-only and %d writing transaction(s): with "+
			"only one kind the arm never shows that termination and immunity apply to both", readers, writers))
	}
	return v
}

// -----------------------------------------------------------------------------
// The renderer
// -----------------------------------------------------------------------------

// String renders the evidence for a report. It renders no raw transaction id —
// only the per-server sequence suffix — and no quantity that is a function of
// the process rather than of the seed, so two runs of one seed render
// byte-identically.
func (e *BoltTxTerminateEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-tx-terminate evidence (arm=%s seed=%d):", e.Arm, e.Seed)
	fmt.Fprintf(&b, "\n  geometry: idle=%s total=%s advances=%d begins=%d", e.IdleBound, e.TotalBound, e.Advances, e.Begins)
	fmt.Fprintf(&b, "\n  clock: timers=%d untils=%d tickers=%d nows-observed=%t",
		e.TimersArmed, e.Untils, e.Tickers, e.Nows > 0)
	b.WriteString("\n  ledger:")
	for i := range e.Plan {
		p := &e.Plan[i]
		fmt.Fprintf(&b, "\n    conn=%d role=%-10s id=%-4s want=%-4s principal=%-20s mode=%s query=%q",
			p.Conn, p.Role, p.IDSuffix, p.WantSuffix, p.Principal, p.Mode, p.Query)
	}
	fmt.Fprintf(&b, "\n  listing: at-cap=%v after-terminate=%v peak=%d", e.ListedAtCap, e.ListedAfterTerminate, e.RegistryPeak)
	b.WriteString("\n  terminate calls:")
	for i := range e.Calls {
		c := &e.Calls[i]
		fmt.Fprintf(&b, "\n    %-20s target=%-4s got=%-20s want=%s", c.Name, c.Target, c.Got, c.Want)
	}
	fmt.Fprintf(&b, "\n  successor: id=%s want=%s listed=%t bystander-listed=%t",
		e.SuccessorSuffix, e.WantSuccessorSuffix, e.SuccessorListed, e.BystanderListed)
	fmt.Fprintf(&b, "\n  attribution: code=%s message-as-pinned=%t",
		e.VictimCode, e.VictimMessage == txReapFailureMessage)
	fmt.Fprintf(&b, "\n  terminate bracket: frames+%d bytes+%d", e.TermFrames, e.TermBytes)
	b.WriteString("\n  honest windows:")
	for i := range e.Windows {
		w := &e.Windows[i]
		fmt.Fprintf(&b, "\n    %-12s openTx=%d committed=%t frames+%d bytes-moved=%t",
			w.Name, w.OpenTx, w.Committed, w.framesAppended(), w.bytesAppended() > 0)
	}
	fmt.Fprintf(&b, "\n  census: ghosts=%d honest=%d committed=%d | after recovery: ghosts=%d honest=%d committed=%d",
		e.GhostsLive, e.HonestLive, e.CommittedLive,
		e.GhostsRecovered, e.HonestRecovered, e.CommittedRecovered)
	return b.String()
}
