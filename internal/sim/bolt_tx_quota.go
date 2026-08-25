package sim

// bolt_tx_quota.go — arm 3 of the Bolt transaction-registry surface (rmp #2482):
// what server.Options.MaxOpenTxPerPrincipal refuses, WHO it refuses, at what
// number, and what has to happen for the refusal to stop.
//
// It shares the abandoned-registry arm's machinery — [txClockProbe],
// [idleReapModel], [waitForTxRegistry], [settleTxRegistry] and
// [BoltTxWriteWindow] — and adds only what a cap needs.
//
// # Why the cap is TWO and not one
//
// A cap of one cannot be distinguished from a broken BEGIN. Under it the very
// first BEGIN a principal sends succeeds and the second is refused, so "the
// server refuses BEGIN" and "the server refuses BEGIN once the principal holds
// its maximum" produce the same wire trace. At two the arm shows the cap
// COUNTING: one accepted, two accepted, three refused. [txQuotaLimit] is
// asserted to be at least two by the non-vacuity gate, so the geometry cannot be
// lowered without the run saying so.
//
// # Why the refusal MESSAGE is recomputed and not merely code-matched
//
// The refusal reaches the client as Neo.ClientError.General.LimitExceeded with
// the quota error's own text, VERBATIM: bolt/server/session.go:1602-1605 returns
// qerr.Error() and does NOT route it through Session.sanitiseErr, unlike every
// neighbouring failure in that handler. The text names the principal and the
// limit (bolt/server/txquota.go:59-62), so recomputing it from the principal and
// the cap the harness CONFIGURED pins who was refused and at what number. A
// code-only assertion cannot: it is equally satisfied by a server that refused
// the wrong principal, or refused the right one at the wrong count, or refused
// for an entirely different limit that happens to share the code.
//
// # rmp #2561: a quota-refused BEGIN leaves the session READY
//
// handleBegin's quota branch returns BEFORE Transition and never calls
// enterFailed (bolt/server/session.go:1597-1606), unlike the adjacent newTx
// failure path immediately above it, which does call enterFailed (:1583). The
// session therefore stays in whatever state it was — READY — and its next
// message is served normally rather than IGNORED. That is observable from the
// client, and this arm PINS it: an autocommit statement sent down the refused
// connection must SUCCEED. If #2561 is closed by making the refusal enter FAILED,
// this clause fails on purpose and is updated with the ticket.
//
// # The three ways a slot comes back
//
// Nothing tested that a refused principal ever becomes able to BEGIN again by
// any route other than a client ROLLBACK (bolt/server/abandoned_tx_test.go does
// that one). This arm drives the other three, each ending in a BEGIN that must
// now succeed:
//
//   - the IDLE REAPER reclaims one, on virtual time, at the advance ordinal an
//     independent [idleReapModel] predicts;
//   - server.Server.TerminateTransaction reclaims one, by operator action;
//   - a DE-AUTHORISED session's refused COMMIT reclaims one. That last is the
//     clause rmp #2482 carries over from the #2481 security review: a COMMIT
//     refused after LOGOFF satisfies the auth scenario's WAL-frame and
//     ghost-node oracles even if the transaction were left OPEN with its quota
//     slot held and its registry entry live, because a refusal that skipped
//     enterFailed would write nothing either. Only the registry and the quota can
//     tell the two apart.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// boltTxArmQuota names the arm in every violation message.
const boltTxArmQuota = "principal-quota"

// The cap and the two identities it is measured with.
const (
	// txQuotaLimit is server.Options.MaxOpenTxPerPrincipal for the arm. It must be
	// at least two; see the file comment and the non-vacuity gate.
	txQuotaLimit = 2
	// txQuotaPrincipalA is the principal driven to its cap. Under
	// server.NoAuthHandler the identity is whatever the client sent
	// (bolt/server/auth.go:52-54), and txQuota counts by
	// Session.identity.Principal captured at BEGIN, so this string IS the quota
	// key. TransactionInfo.Remote cannot discriminate connections — it is the same
	// constant on every SimConn — so the principal is also the only attribution
	// the listing offers.
	txQuotaPrincipalA = "quota-alpha"
	// txQuotaPrincipalB is the control: a second principal, unaffected by the
	// first one's slots.
	txQuotaPrincipalB = "quota-beta"
)

// The virtual geometry. All FAKE; the server clock is the counting probe.
const (
	// txQuotaIdleBound is server.Options.MaxTxIdleTime — the bound the reap
	// composition fires against.
	txQuotaIdleBound = 1 * time.Second
	// txQuotaStep is one advance, half the idle bound, so a transaction TOUCHED
	// after the first advance outlives one that was not. That is what lets the
	// reaper free exactly ONE of the principal's slots.
	txQuotaStep = 500 * time.Millisecond
	// txQuotaTotalBound is server.Options.DefaultTxTimeout, far above the idle
	// bound: effectiveTxDeadline reaps at the EARLIER of the two
	// (bolt/server/serve.go:1155), so a total bound near the idle bound would make
	// the arm measure the wrong reaper.
	txQuotaTotalBound = 10 * time.Minute
	// txQuotaAdvances is how many advances the reap composition makes. Two: the
	// first moves the clock so a touch can genuinely re-arm, the second reaches
	// the untouched transaction's deadline.
	txQuotaAdvances = 2
)

// txQuotaRefusalCeiling bounds, in REAL time, how long the refused BEGIN may
// take. It is deliberately generous: the clause is "the cap answers with a typed
// error rather than parking the client", so it must separate an answer from a
// stall, not measure latency.
const txQuotaRefusalCeiling = 5 * time.Second

// The labels the arm writes under.
const (
	// txQuotaGhostLabel is carried by the node staged inside the transaction whose
	// de-authorised COMMIT is refused. It must be absent live and after recovery.
	txQuotaGhostLabel = "TxQuotaGhost"
	// txQuotaHonestLabel is carried by the autocommit writes bracketing the run,
	// which prove the WAL frame counter and the engine are live instruments.
	txQuotaHonestLabel = "TxQuotaHonest"
)

// The wire contract the arm pins.
const (
	// txQuotaRefusalCode is what a BEGIN over the cap is answered with
	// (bolt/server/session.go:1603).
	txQuotaRefusalCode = "Neo.ClientError.General.LimitExceeded"
	// txQuotaLogoffRefusalCode is what a COMMIT from a de-authorised session is
	// answered with: handleCommit's !s.authenticated gate routes to failTransition
	// (bolt/server/session.go:1656-1657), which returns
	// Neo.ClientError.Request.Invalid naming the ORIGIN state.
	txQuotaLogoffRefusalCode = "Neo.ClientError.Request.Invalid"
	// txQuotaLogoffOriginState is the state failTransition must name: the session
	// is in TX_READY when the COMMIT arrives, because LOGOFF from TX_READY leaves
	// it there. Pinning the origin state is what distinguishes a refusal by the
	// AUTHENTICATION gate from one by the state machine, which share a code.
	txQuotaLogoffOriginState = "TX_READY"
)

// The BEGIN roster, in the order the arm drives it. Each name is a phase, and
// the checkers grade by name rather than by position.
const (
	// txQuotaPhaseCap0 and txQuotaPhaseCap1 fill the principal's cap. They use
	// DIFFERENT access modes, because the cap counts both.
	txQuotaPhaseCap0 = "cap-0"
	txQuotaPhaseCap1 = "cap-1"
	// txQuotaPhaseOverCap is the refusal the arm exists for.
	txQuotaPhaseOverCap = "over-cap"
	// txQuotaPhaseOther is the per-principal control: a second identity must still
	// be served while the first is at its cap.
	txQuotaPhaseOther = "other-principal"
	// txQuotaPhaseAfterReap, txQuotaPhaseAfterTerminate and txQuotaPhaseAfterLogoff
	// are the three compositions: each follows a slot being freed by a DIFFERENT
	// mechanism and must succeed.
	txQuotaPhaseAfterReap = "after-reap"
	// txQuotaPhaseOverCapAgain refuses a second time, once the reaper has refilled
	// the principal to its cap. Without it the run holds a single refusal, and a
	// single event cannot show a counter distinguishing anything.
	txQuotaPhaseOverCapAgain   = "over-cap-again"
	txQuotaPhaseAfterTerminate = "after-terminate"
	txQuotaPhaseAfterLogoff    = "after-logoff"
)

// txQuotaAccepted is the roster of BEGINs that must be ACCEPTED, in order;
// txQuotaRefused is the roster that must be REFUSED. Together they are the full
// BEGIN plan, and the non-vacuity gate holds the run to both.
var (
	txQuotaAccepted = []string{
		txQuotaPhaseCap0, txQuotaPhaseCap1, txQuotaPhaseOther,
		txQuotaPhaseAfterReap, txQuotaPhaseAfterTerminate, txQuotaPhaseAfterLogoff,
	}
	txQuotaRefused = []string{txQuotaPhaseOverCap, txQuotaPhaseOverCapAgain}
)

// txQuotaSeedMix decorrelates the SimDisk sub-seed from the arm seed.
//
// It must DIFFER from boltTxQuotaDefaultSeed. It did not until rmp #2487: both
// were 0x2482_9074, so `NewSeed(seed ^ txQuotaSeedMix)` at the catalogue default
// was `NewSeed(0)` and the mix decorrelated nothing on the one run every report
// starts from. The guard that catches it is
// TestBoltScenarios_SeedMixDoesNotCancelTheDefaultSeed, which is table-driven
// over every Bolt scenario precisely because the per-surface copies it replaced
// left this one unchecked.
const txQuotaSeedMix = 0x2482_5EED

// txQuotaRefusalMessage RECOMPUTES the text the server sends for a BEGIN over
// the cap, from the principal and the limit the harness configured.
//
// It is a restatement of errTxQuotaExceeded.Error() (bolt/server/txquota.go:59-62)
// and is deliberately not shared with it: the point is that two independent
// statements of the same format agree. It reaches the client verbatim because
// handleBegin returns qerr.Error() without passing it through sanitiseErr
// (bolt/server/session.go:1604) — VERIFIED by reading that handler, not assumed
// from the neighbouring failure paths, which all DO sanitise.
func txQuotaRefusalMessage(principal string, limit int) string {
	return fmt.Sprintf("principal %q already holds the maximum of %d concurrently open transactions",
		principal, limit)
}

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// BoltTxQuotaBegin is one BEGIN the arm sent and what the server answered. It is
// the arm's ledger: every clause about the cap is graded against a named row of
// it rather than against a position.
type BoltTxQuotaBegin struct {
	// Phase names the BEGIN's place in the roster.
	Phase string
	// Principal is the identity the connection authenticated as, which under
	// server.NoAuthHandler is exactly the quota key.
	Principal string
	// Mode is the access mode sent, "r" or "w".
	Mode string
	// Accepted records that the server answered SUCCESS. Code and Message are the
	// FAILURE's, verbatim, when it did not.
	Accepted bool
	Code     string
	Message  string
	// IDSuffix is the per-server sequence suffix of the id the registry minted,
	// empty for a refused BEGIN; WantSuffix is what the harness predicted from the
	// BEGIN's ordinal among the ACCEPTED ones, because only an accepted BEGIN
	// reaches txRegistry.nextID.
	IDSuffix   string
	WantSuffix string
	// RegistryBefore/RegistryAfter bracket the exchange with the WHOLE server's
	// listing size, and TimersBefore/TimersAfter with the counted timer
	// registrations. A refused BEGIN must move neither: it must leave the registry
	// exactly as it found it and arm nothing.
	//
	// The whole-server size is deliberately not the same quantity as the cap —
	// the control principal holds transactions of its own — so PrincipalOpenAfter
	// carries how many entries the BEGIN's OWN principal held once the answer was
	// in hand, which is the number the cap is actually about. The harness counts
	// it from the listing rather than from txQuota, which is unexported: they are
	// the same set, because Session.txClosed releases the slot and unregisters the
	// entry together (bolt/server/session.go:813-814).
	RegistryBefore     int
	RegistryAfter      int
	TimersBefore       int64
	TimersAfter        int64
	PrincipalOpenAfter int
	// WithinCeiling records that the exchange completed inside
	// [txQuotaRefusalCeiling] of REAL time — the clause that separates a typed
	// refusal from a stall. It is a boolean rather than the duration because the
	// duration is wall time and not seed-pure.
	WithinCeiling bool
}

// BoltTxQuotaEvidence is everything one run of the quota arm observed, and NO
// verdict. The checkers are pure functions of it.
//
// Every field is seed-pure except Nows (it counts how often the HARNESS polled
// the registry) and the honest windows' byte deltas (the frame payload embeds a
// node key from a process-global counter). Neither is rendered as a number by
// [BoltTxQuotaEvidence.String].
type BoltTxQuotaEvidence struct {
	// Arm names the arm; Seed is the seed it was built from.
	Arm  string
	Seed uint64

	// Limit is server.Options.MaxOpenTxPerPrincipal as installed; PrincipalA is
	// the identity driven to it and PrincipalB the control.
	Limit      int
	PrincipalA string
	PrincipalB string

	// IdleBound, TotalBound and Step are the virtual geometry; Advances is how
	// many advances the reap composition made.
	IdleBound  time.Duration
	TotalBound time.Duration
	Step       time.Duration
	Advances   int

	// TimersTotal, Untils and Tickers are the counted registrations on the
	// injected clock, WantTimers the harness's own prediction of the total, and
	// Nows the read counter (not seed-pure; see the type comment). Touches is how
	// many open transactions the arm deliberately re-armed by sending them a
	// message after the clock had moved.
	TimersTotal int64
	WantTimers  int64
	Untils      int64
	Tickers     int64
	Nows        int64
	Touches     int

	// Begins is the BEGIN ledger, in the order the arm sent them.
	Begins []BoltTxQuotaBegin

	// CapModes are the access modes the cap-filling transactions used. Both kinds
	// must appear: the cap counts a read transaction exactly as it counts a write.
	CapModes []string
	// RegistryAtCap is the listing size once the principal held its cap.
	RegistryAtCap int

	// WantRefusalMessage is [txQuotaRefusalMessage] recomputed from PrincipalA and
	// Limit; the refusal's own text is on its ledger row.
	WantRefusalMessage string

	// PostRefusalAccepted records that an autocommit statement sent down the
	// REFUSED connection was served. It pins rmp #2561: the quota branch returns
	// before Transition and never enters FAILED, so the session is still READY.
	// PostRefusalCode and PostRefusalMessage carry what it was answered instead.
	PostRefusalAccepted bool
	PostRefusalCode     string
	PostRefusalMessage  string

	// PredictedReapOrdinals is [idleReapModel.reapOrdinalsWithin] computed BEFORE
	// the advances ran, over the three transactions open at that point;
	// ObservedReapOrdinals is the ordinal at which each was first absent, in the
	// same order. ReapProbeFires are the ordinals at which the arm's own uncounted
	// timer received the advance.
	PredictedReapOrdinals []int
	ObservedReapOrdinals  []int
	ReapProbeFires        []int
	// ReapCode and ReapMessage are what the reaped connection was told on its next
	// message — the attribution that turns "it is no longer listed" into "the idle
	// reaper ended it".
	ReapCode    string
	ReapMessage string

	// TerminateOutcome is what server.Server.TerminateTransaction returned for the
	// slot the operator reclaimed, and TerminatedGone that the entry left the
	// registry.
	TerminateOutcome txTerminateOutcome
	TerminatedGone   bool

	// The de-authorised-refusal composition (the rmp #2482 carry-over clause).
	// LogoffCommitCode and LogoffCommitMessage are what the refused COMMIT was
	// answered with, and LogoffEntryGone that the registry entry was reclaimed
	// rather than merely left behind by a refusal that wrote nothing.
	LogoffCommitCode    string
	LogoffCommitMessage string
	LogoffEntryGone     bool

	// Windows are the honest autocommit writes bracketing the run.
	Windows []BoltTxWriteWindow

	// GhostsLive/GhostsRecovered count [txQuotaGhostLabel] in the live engine and
	// after a crash and a real WAL replay; Honest* do the same for the witness.
	GhostsLive      int
	GhostsRecovered int
	HonestLive      int
	HonestRecovered int
}

// acceptedBegins reports how many BEGINs the server accepted.
func (e *BoltTxQuotaEvidence) acceptedBegins() int {
	n := 0
	for i := range e.Begins {
		if e.Begins[i].Accepted {
			n++
		}
	}
	return n
}

// beginFor returns the ledger row for a phase, or nil when the run never
// reached it.
func (e *BoltTxQuotaEvidence) beginFor(phase string) *BoltTxQuotaBegin {
	for i := range e.Begins {
		if e.Begins[i].Phase == phase {
			return &e.Begins[i]
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// The arm
// -----------------------------------------------------------------------------

// RunBoltTxQuota drives the per-principal quota arm once and returns the
// evidence. It is bit-reproducible from seed: the whole arm runs on ONE
// goroutine, every advance is made by it, and the arm makes no seeded choice —
// the roster, the modes and the statements are fixed — so the seed reaches only
// the SimDisk's (unused) fault stream.
//
// The returned error is reserved for harness failures. A cap that refused the
// wrong principal, a slot that was never returned, or a ghost that survived
// recovery is EVIDENCE, not an error.
func RunBoltTxQuota(ctx context.Context, seed uint64) (*BoltTxQuotaEvidence, error) {
	opts := defaultBoltTxQuotaOptions()
	return runBoltTxQuota(ctx, seed, &opts)
}

// boltTxQuotaOptions parameterises the arm. The zero value is not usable;
// [defaultBoltTxQuotaOptions] builds the nominal geometry and a live control
// varies exactly one field of it.
type boltTxQuotaOptions struct {
	// ServerLimit is installed as server.Options.MaxOpenTxPerPrincipal. The arm
	// proper uses [txQuotaLimit]; a live control installs a NEGATIVE value, which
	// that option documents as DISABLING enforcement (bolt/server/serve.go:339,
	// newTxQuota's limit <= 0 branch).
	//
	// It is a real alternative CONFIGURATION rather than a doctored evidence field,
	// which is what makes it a control: with the cap switched off the identical
	// over-cap BEGIN must be ADMITTED, so every refusal the arm records is pinned
	// on server.Options.MaxOpenTxPerPrincipal and not on the state machine, the
	// wire, or a mistake in the harness.
	ServerLimit int
	// ModelLimit is the cap the harness's own expectations are built from. It stays
	// at [txQuotaLimit] in every configuration, so a control that lowers only the
	// SERVER's cap is graded against the cap it thought it installed.
	ModelLimit int
}

// defaultBoltTxQuotaOptions returns the arm's nominal configuration.
func defaultBoltTxQuotaOptions() boltTxQuotaOptions {
	return boltTxQuotaOptions{ServerLimit: txQuotaLimit, ModelLimit: txQuotaLimit}
}

// runBoltTxQuota is [RunBoltTxQuota] with the cap made explicit, so a live
// control can vary it and drive the same real server through the same real wire.
func runBoltTxQuota(ctx context.Context, seed uint64, opts *boltTxQuotaOptions) (*BoltTxQuotaEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^txQuotaSeedMix), 0) // faultRate 0: this arm faults nothing
	cfg := durableStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-quota open store: %w", err)
	}

	probe := newTxClockProbe()
	srv, err := NewSimServerTxRegistry(st.Engine(), clock.Real(), probe,
		txQuotaIdleBound, txQuotaTotalBound, opts.ServerLimit)
	if err != nil {
		st.Crash()
		return nil, fmt.Errorf("sim: bolt-tx-quota server: %w", err)
	}

	r := &boltTxQuotaRunner{
		srv: srv, st: st, probe: probe,
		ids: make(map[string]string, len(txQuotaAccepted)),
		ev: &BoltTxQuotaEvidence{
			Arm: boltTxArmQuota, Seed: seed,
			Limit: opts.ModelLimit, PrincipalA: txQuotaPrincipalA, PrincipalB: txQuotaPrincipalB,
			IdleBound: txQuotaIdleBound, TotalBound: txQuotaTotalBound, Step: txQuotaStep,
			WantRefusalMessage: txQuotaRefusalMessage(txQuotaPrincipalA, opts.ModelLimit),
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

	st.Crash()
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-quota reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	if r.ev.GhostsRecovered, err = countLabelViaEngine(ctx, st2, txQuotaGhostLabel); err != nil {
		return nil, err
	}
	if r.ev.HonestRecovered, err = countLabelViaEngine(ctx, st2, txQuotaHonestLabel); err != nil {
		return nil, err
	}
	return r.ev, nil
}

// boltTxQuotaRunner threads the server, the store, the clock probe and the
// accumulating evidence through the arm's phases. Exactly one goroutine drives
// it.
type boltTxQuotaRunner struct {
	srv    *SimServer
	st     *SimStore
	probe  *txClockProbe
	ev     *BoltTxQuotaEvidence
	honest *WireClient
	conns  []*WireClient
	// ids maps a BEGIN phase to the RAW registry id, which TerminateTransaction
	// needs and no report may ever contain.
	ids map[string]string
	// byPhase maps a BEGIN phase to the connection that opened it.
	byPhase map[string]*WireClient
	// now is the fake instant the harness believes it has advanced to.
	now time.Duration
}

// closeConns closes every client the run opened. Idempotent.
func (r *boltTxQuotaRunner) closeConns() {
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
func (r *boltTxQuotaRunner) drive(ctx context.Context) error {
	r.byPhase = make(map[string]*WireClient, len(txQuotaAccepted))
	hc, err := r.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota dial honest: %w", err)
	}
	r.honest = hc
	if cerr := hc.Connect(ctx); cerr != nil {
		return fmt.Errorf("sim: bolt-tx-quota connect honest: %w", cerr)
	}
	if werr := r.honestWindow(txWindowBefore); werr != nil {
		return werr
	}

	if cerr := r.fillCap(ctx); cerr != nil {
		return cerr
	}
	if rerr := r.refuse(ctx, txQuotaPhaseOverCap); rerr != nil {
		return rerr
	}
	if cerr := r.otherPrincipal(ctx); cerr != nil {
		return cerr
	}
	if rerr := r.reapOneSlot(ctx); rerr != nil {
		return rerr
	}
	if rerr := r.refuse(ctx, txQuotaPhaseOverCapAgain); rerr != nil {
		return rerr
	}
	if terr := r.terminateOneSlot(ctx); terr != nil {
		return terr
	}
	if lerr := r.logoffOneSlot(ctx); lerr != nil {
		return lerr
	}
	if werr := r.honestWindow(txWindowAfter); werr != nil {
		return werr
	}

	r.ev.TimersTotal = r.probe.Timers()
	r.ev.Untils = r.probe.Untils()
	r.ev.Tickers = r.probe.Tickers()
	// The harness's own prediction of the total: one timer per ACCEPTED BEGIN
	// (handleBegin arms exactly one, and a refused BEGIN never reaches the arming
	// because it returns before txActive is set), plus one per deliberate touch,
	// each of which re-arms because the clock has moved since its BEGIN.
	r.ev.WantTimers = int64(r.ev.acceptedBegins() + r.ev.Touches)

	if r.ev.GhostsLive, err = countLabelOverWire(r.honest, txQuotaGhostLabel); err != nil {
		return err
	}
	if r.ev.HonestLive, err = countLabelOverWire(r.honest, txQuotaHonestLabel); err != nil {
		return err
	}
	// Read last, so the count covers every registry listing the run took.
	r.ev.Nows = r.probe.Nows()
	return nil
}

// dialAs opens a connection and authenticates it as principal. Under
// server.NoAuthHandler the principal is admitted as sent, which is what makes it
// the quota key.
func (r *boltTxQuotaRunner) dialAs(ctx context.Context, principal string) (*WireClient, error) {
	c, err := r.srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-quota dial %q: %w", principal, err)
	}
	r.conns = append(r.conns, c)
	authResp, err := c.ConnectAs(ctx, principal, "")
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-quota auth %q: %w", principal, err)
	}
	if !isSuccess(authResp) {
		return nil, fmt.Errorf("sim: bolt-tx-quota auth %q refused: %s %s",
			principal, failureCode(authResp), failureMessage(authResp))
	}
	return c, nil
}

// begin sends one BEGIN on its own fresh connection and records the ledger row,
// bracketing the exchange in REAL time so a refusal that STALLED is
// distinguishable from one that answered.
//
// An accepted BEGIN is waited on twice, in the message loop's own order: the
// entry is listed first, the timer armed strictly later. Both barriers matter —
// the listing so the id can be read back from the registry rather than invented,
// the arming so a later advance is a deterministic reap rather than a race
// against syncTxTimer.
func (r *boltTxQuotaRunner) begin(ctx context.Context, phase, principal, mode string) error {
	c, err := r.dialAs(ctx, principal)
	if err != nil {
		return err
	}
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	before := r.srv.Server().Transactions()
	timersBefore := r.probe.Timers()
	start := time.Now()
	resp, err := c.BeginMode(mode)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota BEGIN %s: %w", phase, err)
	}
	if derr := clearTxReadDeadline(c); derr != nil {
		return derr
	}

	row := BoltTxQuotaBegin{
		Phase: phase, Principal: principal, Mode: mode,
		Accepted: isSuccess(resp), WithinCeiling: elapsed <= txQuotaRefusalCeiling,
		RegistryBefore: len(before), TimersBefore: timersBefore,
	}
	if !row.Accepted {
		row.Code, row.Message = failureCode(resp), failureMessage(resp)
		// A refusal must move neither instrument, so both are sampled AFTER a settle
		// window rather than immediately: a registry entry or a timer that appeared a
		// moment later would otherwise be missed.
		settled := settleTxRegistry(r.srv)
		row.RegistryAfter = len(settled)
		row.TimersAfter = r.probe.Timers()
		row.PrincipalOpenAfter = countPrincipal(settled, principal)
		r.ev.Begins = append(r.ev.Begins, row)
		return nil
	}

	r.ev.Begins = append(r.ev.Begins, row)
	accepted := r.ev.acceptedBegins()
	if werr := waitForTxRegistry(func() bool {
		return slices.ContainsFunc(r.srv.Server().Transactions(),
			func(t server.TransactionInfo) bool { return t.ID == r.freshIDFor(principal) })
	}, fmt.Sprintf("the registry never listed the transaction opened in phase %s", phase)); werr != nil {
		return werr
	}
	if werr := waitForTxRegistry(func() bool { return r.probe.Timers() >= int64(accepted+r.ev.Touches) },
		fmt.Sprintf("the server never armed the timer for the BEGIN in phase %s", phase)); werr != nil {
		return werr
	}

	txs := r.srv.Server().Transactions()
	id := r.newestIDFor(txs, principal)
	if id == "" {
		return fmt.Errorf("sim: bolt-tx-quota: the registry lists %d transaction(s) but none for %q after phase %s",
			len(txs), principal, phase)
	}
	r.ids[phase] = id
	r.byPhase[phase] = c
	last := &r.ev.Begins[len(r.ev.Begins)-1]
	last.IDSuffix = txIDSuffix(id)
	last.WantSuffix = fmt.Sprintf("-%d", accepted)
	last.RegistryAfter = len(txs)
	last.TimersAfter = r.probe.Timers()
	last.PrincipalOpenAfter = countPrincipal(txs, principal)
	return nil
}

// countPrincipal reports how many of a listing's entries belong to principal.
// Under server.NoAuthHandler the principal is admitted as sent, so it is exactly
// the quota key; TransactionInfo.Remote could not stand in for it, being the same
// constant on every SimConn.
func countPrincipal(txs []server.TransactionInfo, principal string) int {
	n := 0
	for i := range txs {
		if txs[i].Principal == principal {
			n++
		}
	}
	return n
}

// freshIDFor reports the id the harness has NOT yet recorded for principal, if
// the registry is already showing one. It is the wait predicate's helper: a
// principal may hold several transactions at once, so "an entry for this
// principal exists" is not enough to know the NEW one has landed.
func (r *boltTxQuotaRunner) freshIDFor(principal string) string {
	return r.newestIDFor(r.srv.Server().Transactions(), principal)
}

// newestIDFor returns the id of the principal's entry that the harness has not
// yet claimed, or "" when every one it can see is already in the ledger. The
// registry's ids carry a monotone per-server sequence, so "not yet claimed" is
// unambiguous.
func (r *boltTxQuotaRunner) newestIDFor(txs []server.TransactionInfo, principal string) string {
	known := make(map[string]bool, len(r.ids))
	for _, id := range r.ids {
		known[id] = true
	}
	for i := range txs {
		if txs[i].Principal == principal && !known[txs[i].ID] {
			return txs[i].ID
		}
	}
	return ""
}

// fillCap opens exactly Limit transactions for the capped principal, one per
// connection, using BOTH access modes — the cap counts a read transaction
// exactly as it counts a write, which nothing asserted before.
func (r *boltTxQuotaRunner) fillCap(ctx context.Context) error {
	phases := []string{txQuotaPhaseCap0, txQuotaPhaseCap1}
	modes := []string{"r", "w"}
	for i := range r.ev.Limit {
		// A limit above the two named phases would reuse the last one's shape; the
		// non-vacuity gate holds the roster to what is declared, so this never
		// silently under-fills.
		phase, mode := phases[i%len(phases)], modes[i%len(modes)]
		if berr := r.begin(ctx, phase, txQuotaPrincipalA, mode); berr != nil {
			return berr
		}
		r.ev.CapModes = append(r.ev.CapModes, mode)
	}
	r.ev.RegistryAtCap = len(r.srv.Server().Transactions())
	return nil
}

// refuse drives one BEGIN that must be refused by the cap, and — for the FIRST
// refusal — then sends an autocommit statement down the same connection to pin
// rmp #2561: the quota branch never enters FAILED, so the session is still READY
// and the next message is served.
func (r *boltTxQuotaRunner) refuse(ctx context.Context, phase string) error {
	if berr := r.begin(ctx, phase, txQuotaPrincipalA, "r"); berr != nil {
		return berr
	}
	if phase != txQuotaPhaseOverCap {
		return nil
	}
	c := r.conns[len(r.conns)-1]
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	resp, err := c.Run("RETURN 1", nil)
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota post-refusal RUN: %w", err)
	}
	if !isSuccess(resp) {
		r.ev.PostRefusalCode, r.ev.PostRefusalMessage = failureCode(resp), failureMessage(resp)
		return clearTxReadDeadline(c)
	}
	_, terminal, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota post-refusal PULL: %w", err)
	}
	if isSuccess(terminal) {
		r.ev.PostRefusalAccepted = true
	} else {
		r.ev.PostRefusalCode, r.ev.PostRefusalMessage = failureCode(terminal), failureMessage(terminal)
	}
	return clearTxReadDeadline(c)
}

// otherPrincipal is the per-principal control: a SECOND identity must still be
// served while the first is at its cap. Without it, "the cap refused" would be
// equally true of a server-wide limit.
func (r *boltTxQuotaRunner) otherPrincipal(ctx context.Context) error {
	return r.begin(ctx, txQuotaPhaseOther, txQuotaPrincipalB, "r")
}

// reapOneSlot frees exactly one of the capped principal's slots with the IDLE
// REAPER, on virtual time, and then shows the principal can BEGIN again.
//
// The stagger is what makes it exactly one. Three transactions are open, all
// registered at the same fake instant; the first advance moves the clock without
// reaching any deadline, the arm then TOUCHES two of them (which re-arms their
// timers for a fresh instant, because syncTxTimer's at.Equal early-return no
// longer holds once the clock has moved), and the second advance reaches only the
// untouched one. An independent [idleReapModel] predicts that from the plan's
// arithmetic alone.
func (r *boltTxQuotaRunner) reapOneSlot(ctx context.Context) error {
	victim := txQuotaPhaseCap0
	survivors := []string{txQuotaPhaseCap1, txQuotaPhaseOther}

	// Advance 1: nothing is due, and the clock has now MOVED, which is what lets a
	// touch re-arm rather than early-return.
	if aerr := r.advance(1); aerr != nil {
		return aerr
	}
	for _, phase := range survivors {
		if terr := r.touch(phase); terr != nil {
			return terr
		}
	}

	// The prediction, built from the harness's own arithmetic before the deciding
	// advance runs. Offsets are each transaction's LAST message: zero for the
	// untouched victim, one step for the two that were touched.
	model := &idleReapModel{
		MaxTxIdle: txQuotaIdleBound,
		Step:      txQuotaStep,
		Offsets:   []time.Duration{0, txQuotaStep, txQuotaStep},
	}
	r.ev.PredictedReapOrdinals = model.reapOrdinalsWithin(txQuotaAdvances)

	if aerr := r.advance(2); aerr != nil {
		return aerr
	}
	victimSuffix := txIDSuffix(r.ids[victim])
	if werr := waitForTxRegistry(func() bool {
		return !slices.Contains(txSuffixes(r.srv.Server().Transactions()), victimSuffix)
	}, "the idle reaper never reclaimed the untouched transaction"); werr != nil {
		return werr
	}
	present := txSuffixes(settleTxRegistry(r.srv))
	r.ev.ObservedReapOrdinals = []int{txQuotaAdvances, reapNever, reapNever}
	if slices.Contains(present, victimSuffix) {
		r.ev.ObservedReapOrdinals[0] = reapNever
	}
	for i, phase := range survivors {
		if !slices.Contains(present, txIDSuffix(r.ids[phase])) {
			// A survivor that went early is recorded at the advance it could only have
			// gone at, which is what makes the comparison against the prediction fail
			// with the ordinal rather than merely with a count.
			r.ev.ObservedReapOrdinals[i+1] = txQuotaAdvances
		}
	}

	// Attribution: only Session.reapTimedOutTx arms this typed FAILURE, so it is
	// what turns "no longer listed" into "the idle reaper ended it".
	if perr := r.probeReaped(victim); perr != nil {
		return perr
	}
	return r.begin(ctx, txQuotaPhaseAfterReap, txQuotaPrincipalA, "w")
}

// advance moves the fake clock one step and records that the advance reached a
// waiter at all. The witness timer is UNCOUNTED, so it leaves the arm's
// attribution of one counted timer per BEGIN untouched.
func (r *boltTxQuotaRunner) advance(ordinal int) error {
	witness := r.probe.probeTimer(r.ev.Step)
	r.probe.Advance(r.ev.Step)
	r.now += r.ev.Step
	r.ev.Advances = ordinal
	select {
	case <-witness.C():
		r.ev.ReapProbeFires = append(r.ev.ReapProbeFires, ordinal)
	default:
	}
	witness.Stop()
	return nil
}

// touch sends one statement inside an open transaction so its idle deadline moves
// forward, and WAITS for the re-arming rather than for the reply.
//
// The wait is the whole point. syncTxTimer runs AFTER the response is flushed
// (bolt/server/serve.go:1350 against the flush at :1501-1505), so a reply in hand
// does not mean the deadline has moved yet; advancing on the reply would race the
// arming and the reap ordinal would be a scheduler outcome.
func (r *boltTxQuotaRunner) touch(phase string) error {
	c := r.byPhase[phase]
	if c == nil {
		return fmt.Errorf("sim: bolt-tx-quota: no connection recorded for phase %s", phase)
	}
	before := r.probe.Timers()
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	if rerr := runInOpenTx(c, "RETURN 1"); rerr != nil {
		return fmt.Errorf("sim: bolt-tx-quota touch %s: %w", phase, rerr)
	}
	if derr := clearTxReadDeadline(c); derr != nil {
		return derr
	}
	if werr := waitForTxRegistry(func() bool { return r.probe.Timers() > before },
		fmt.Sprintf("the server never re-armed the timer for phase %s after the clock had moved", phase)); werr != nil {
		return werr
	}
	r.ev.Touches++
	return nil
}

// probeReaped sends ONE statement down the reaped connection and records what the
// server answered.
func (r *boltTxQuotaRunner) probeReaped(phase string) error {
	c := r.byPhase[phase]
	if c == nil {
		return fmt.Errorf("sim: bolt-tx-quota: no connection recorded for phase %s", phase)
	}
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	resp, err := c.Run("RETURN 1", nil)
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota reap probe: %w", err)
	}
	r.ev.ReapCode, r.ev.ReapMessage = failureCode(resp), failureMessage(resp)
	return clearTxReadDeadline(c)
}

// terminateOneSlot frees a slot by OPERATOR action and shows the principal can
// BEGIN again.
func (r *boltTxQuotaRunner) terminateOneSlot(ctx context.Context) error {
	id := r.ids[txQuotaPhaseCap1]
	r.ev.TerminateOutcome = classifyTerminate(r.srv.Server().TerminateTransaction(id))
	suffix := txIDSuffix(id)
	if werr := waitForTxRegistry(func() bool {
		return !slices.Contains(txSuffixes(r.srv.Server().Transactions()), suffix)
	}, "the terminated transaction never left the registry"); werr != nil {
		return werr
	}
	r.ev.TerminatedGone = !slices.Contains(txSuffixes(settleTxRegistry(r.srv)), suffix)
	return r.begin(ctx, txQuotaPhaseAfterTerminate, txQuotaPrincipalA, "w")
}

// logoffOneSlot frees a slot through the DE-AUTHORISED refusal path and shows the
// principal can BEGIN again.
//
// This is the clause rmp #2482 carries over from the #2481 security review. The
// sequence is: stage a write inside the open transaction, LOGOFF (legal from
// TX_READY, and it clears s.authenticated), then COMMIT — which handleCommit
// refuses at its !s.authenticated gate via failTransition, and failTransition
// enters FAILED, which reclaims the transaction. What has to be measured is the
// RECLAMATION, because the WAL-frame and ghost-node oracles the auth scenario
// uses are satisfied either way: a refusal that left the transaction open with
// its slot held would also have written nothing.
func (r *boltTxQuotaRunner) logoffOneSlot(ctx context.Context) error {
	phase := txQuotaPhaseAfterTerminate
	c := r.byPhase[phase]
	if c == nil {
		return fmt.Errorf("sim: bolt-tx-quota: no connection recorded for phase %s", phase)
	}
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	if rerr := runInOpenTx(c, fmt.Sprintf("CREATE (:%s {name: %q})", txQuotaGhostLabel, "logoff-ghost")); rerr != nil {
		return fmt.Errorf("sim: bolt-tx-quota staged write: %w", rerr)
	}
	logoffResp, err := c.Logoff()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota LOGOFF: %w", err)
	}
	if !isSuccess(logoffResp) {
		return fmt.Errorf("sim: bolt-tx-quota LOGOFF refused: %s %s",
			failureCode(logoffResp), failureMessage(logoffResp))
	}
	commitResp, err := c.Commit()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-quota COMMIT after LOGOFF: %w", err)
	}
	if derr := clearTxReadDeadline(c); derr != nil {
		return derr
	}
	r.ev.LogoffCommitCode, r.ev.LogoffCommitMessage = failureCode(commitResp), failureMessage(commitResp)

	suffix := txIDSuffix(r.ids[phase])
	if werr := waitForTxRegistry(func() bool {
		return !slices.Contains(txSuffixes(r.srv.Server().Transactions()), suffix)
	}, "the de-authorised session's transaction never left the registry"); werr != nil {
		// Not a harness failure: it IS the defect the clause hunts for, so it is
		// recorded as evidence and adjudicated below.
		r.ev.LogoffEntryGone = false
		return r.begin(ctx, txQuotaPhaseAfterLogoff, txQuotaPrincipalA, "r")
	}
	r.ev.LogoffEntryGone = !slices.Contains(txSuffixes(settleTxRegistry(r.srv)), suffix)
	return r.begin(ctx, txQuotaPhaseAfterLogoff, txQuotaPrincipalA, "r")
}

// honestWindow drives one autocommit write on the honest connection, bracketed by
// the WAL counters. It is AUTOCOMMIT so it neither registers a transaction, nor
// arms a timer, nor consumes one of the honest connection's own quota slots.
func (r *boltTxQuotaRunner) honestWindow(name string) error {
	open := len(r.srv.Server().Transactions())
	w := BoltTxWriteWindow{Name: name, Node: "honest-" + name, OpenTx: open}
	w.FramesBefore, w.BytesBefore = r.walCounters()
	err := r.runAutocommit(fmt.Sprintf("CREATE (:%s {name: %q})", txQuotaHonestLabel, w.Node))
	w.FramesAfter, w.BytesAfter = r.walCounters()
	if err != nil {
		if !isWireRefusal(err) {
			return fmt.Errorf("sim: bolt-tx-quota honest window %q: %w", name, err)
		}
	} else {
		w.Committed = true
	}
	r.ev.Windows = append(r.ev.Windows, w)
	return nil
}

// runAutocommit runs one statement outside any explicit transaction on the honest
// connection and drains it.
func (r *boltTxQuotaRunner) runAutocommit(query string) error {
	if err := armTxReadDeadline(r.honest); err != nil {
		return err
	}
	if _, err := wireQuery(r.honest, query, nil); err != nil {
		return err
	}
	return clearTxReadDeadline(r.honest)
}

// walCounters reads the live WAL frame/byte counters.
func (r *boltTxQuotaRunner) walCounters() (frames, bytes uint64) {
	s := r.st.WAL().Stats()
	return s.Frames, s.Bytes
}

// -----------------------------------------------------------------------------
// The oracles
// -----------------------------------------------------------------------------

// checkBoltTxQuota adjudicates the evidence against the CONTRACT: who the cap
// refuses and at what number, that it is per-principal, that a refusal costs
// nothing, and that each of the three reclamation routes genuinely returns a
// slot. It is split from [checkBoltTxQuotaNonVacuity] so an uninformative run
// cannot read as a faulty one (rmp #2470).
//
// The receiver is a pointer because the value is far over the copy threshold; it
// mutates nothing.
func checkBoltTxQuota(e *BoltTxQuotaEvidence) []Violation {
	return slices.Concat(
		checkBoltTxQuotaRoster(e),
		checkBoltTxQuotaRefusal(e),
		checkBoltTxQuotaReclamation(e),
		checkBoltTxQuotaResidue(e),
	)
}

// checkBoltTxQuotaRoster adjudicates every BEGIN against the answer the roster
// requires of it, and every accepted one against the id the harness predicted.
func checkBoltTxQuotaRoster(e *BoltTxQuotaEvidence) []Violation {
	var v []Violation
	for i := range e.Begins {
		b := &e.Begins[i]
		wantAccepted := slices.Contains(txQuotaAccepted, b.Phase)
		if b.Accepted != wantAccepted {
			verb := "was REFUSED"
			detail := fmt.Sprintf(" (%s: %s)", b.Code, b.Message)
			if b.Accepted {
				verb, detail = "was ACCEPTED", ""
			}
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin:"+b.Phase),
				Message: fmt.Sprintf("the BEGIN in phase %q by %q %s%s; the cap of %d requires the opposite",
					b.Phase, b.Principal, verb, detail, e.Limit),
			})
			continue
		}
		if !b.Accepted {
			continue
		}
		if b.IDSuffix != b.WantSuffix {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin-id:"+b.Phase),
				Message: fmt.Sprintf("the BEGIN in phase %q was minted id suffix %s, want %s: txRegistry.nextID's "+
					"sequence is per SERVER and only an ACCEPTED BEGIN consumes one",
					b.Phase, b.IDSuffix, b.WantSuffix),
			})
		}
		if !b.WithinCeiling {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin-latency:"+b.Phase),
				Message: fmt.Sprintf("the BEGIN in phase %q took longer than %s of REAL time",
					b.Phase, txQuotaRefusalCeiling),
			})
		}
		if b.RegistryAfter != b.RegistryBefore+1 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin-registry:"+b.Phase),
				Message: fmt.Sprintf("the accepted BEGIN in phase %q took the registry from %d to %d transaction(s), "+
					"want exactly one more", b.Phase, b.RegistryBefore, b.RegistryAfter),
			})
		}
		if b.TimersAfter != b.TimersBefore+1 {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin-timers:"+b.Phase),
				Message: fmt.Sprintf("the accepted BEGIN in phase %q registered %d timer(s) on the injected clock "+
					"(%d -> %d), want exactly one: the reap ordinal assumes one waiter per open transaction",
					b.Phase, b.TimersAfter-b.TimersBefore, b.TimersBefore, b.TimersAfter),
			})
		}
	}
	if e.RegistryAtCap != e.Limit {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "cap-size"),
			Message: fmt.Sprintf("the registry listed %d transaction(s) once %q held its cap, want exactly %d",
				e.RegistryAtCap, e.PrincipalA, e.Limit),
		})
	}
	return v
}

// checkBoltTxQuotaRefusal adjudicates the refusal itself: the code, the
// recomputed message, that it cost nothing, that it was an answer rather than a
// stall, and the session state it left behind (rmp #2561).
func checkBoltTxQuotaRefusal(e *BoltTxQuotaEvidence) []Violation {
	var v []Violation
	over := e.beginFor(txQuotaPhaseOverCap)
	if over == nil {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: txOp(e.Arm, "refusal"),
			Message: fmt.Sprintf("the run never drove the %q phase, so the cap was never asked to refuse",
				txQuotaPhaseOverCap),
		})
		return v
	}
	if over.Code != txQuotaRefusalCode {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-code"),
			Message: fmt.Sprintf("the BEGIN over the cap was refused with code %q, want %q",
				over.Code, txQuotaRefusalCode),
		})
	}
	// The load-bearing half: the text names WHO was refused and at WHAT number,
	// recomputed from the principal and the limit the harness configured. A
	// code-only assertion is equally satisfied by refusing the wrong principal.
	if over.Message != e.WantRefusalMessage {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-message"),
			Message: fmt.Sprintf("the refusal read %q, want %q recomputed from the principal and the cap this run "+
				"installed; handleBegin returns the quota error VERBATIM (bolt/server/session.go:1604), so this text "+
				"is the contract and not a paraphrase of it", over.Message, e.WantRefusalMessage),
		})
	}
	if !over.WithinCeiling {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-latency"),
			Message: fmt.Sprintf("the refused BEGIN took longer than %s of REAL time: saturation must be answered "+
				"with a typed error, never by parking the client", txQuotaRefusalCeiling),
		})
	}
	for _, phase := range txQuotaRefused {
		b := e.beginFor(phase)
		if b == nil {
			continue
		}
		// The number the CAP is about: the refusing principal must still hold exactly
		// its limit. The whole-server size is a different quantity — the control
		// principal holds transactions of its own — and is graded separately below.
		if b.PrincipalOpenAfter != e.Limit {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-quota"),
				Message: fmt.Sprintf("after the refused BEGIN in phase %q the principal %q held %d open "+
					"transaction(s), want exactly the cap of %d: a refused BEGIN must neither add a slot nor lose one",
					phase, b.Principal, b.PrincipalOpenAfter, e.Limit),
			})
		}
		if b.RegistryAfter != b.RegistryBefore {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-registry"),
				Message: fmt.Sprintf("the refused BEGIN in phase %q left the registry holding %d transaction(s) "+
					"against %d before it: a refusal must leave the registry exactly as it found it",
					phase, b.RegistryAfter, b.RegistryBefore),
			})
		}
		if b.TimersAfter != b.TimersBefore {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-timers"),
				Message: fmt.Sprintf("the refused BEGIN in phase %q registered %d timer(s) on the injected clock "+
					"(%d -> %d), want ZERO: handleBegin's quota branch returns before txActive is set, so the serve "+
					"loop's syncTxTimer has nothing to arm (bolt/server/serve.go:1171)",
					phase, b.TimersAfter-b.TimersBefore, b.TimersBefore, b.TimersAfter),
			})
		}
	}
	if !e.PostRefusalAccepted {
		// rmp #2561 pinned as OBSERVED: handleBegin's quota branch returns before
		// Transition and never calls enterFailed (bolt/server/session.go:1597-1606),
		// unlike the newTx failure path directly above it, so the session is left
		// READY and its next message is served. If that is corrected, this clause
		// fires on purpose and the arm is updated with the ticket.
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "refusal-session-state"),
			Message: fmt.Sprintf("the connection whose BEGIN the cap refused was answered %q (%q) to the next "+
				"statement instead of serving it: rmp #2561 pins the OBSERVED behaviour, which is that the quota "+
				"branch returns before Transition and leaves the session READY", e.PostRefusalCode, e.PostRefusalMessage),
		})
	}
	return v
}

// checkBoltTxQuotaReclamation adjudicates the three routes by which a slot comes
// back, each of which must end in a BEGIN the cap now allows.
func checkBoltTxQuotaReclamation(e *BoltTxQuotaEvidence) []Violation {
	var v []Violation
	if !slices.Equal(e.ObservedReapOrdinals, e.PredictedReapOrdinals) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-ordinal"),
			Message: fmt.Sprintf("transactions left the registry at advance ordinals %v, but the idle rule predicts %v "+
				"over %d advance(s) (idle bound %s, step %s; ordinal %d means still open): the reaper must reclaim the "+
				"UNTOUCHED slot and leave the two that were touched",
				e.ObservedReapOrdinals, e.PredictedReapOrdinals, e.Advances, e.IdleBound, e.Step, reapNever),
		})
	}
	for n := 1; n <= e.Advances; n++ {
		if !slices.Contains(e.ReapProbeFires, n) {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "advance-delivery"),
				Message: fmt.Sprintf("advance %d of %d reached no waiter on the fake clock: \"nothing was reaped at "+
					"ordinal %d\" would be a statement about a tick that never arrived", n, e.Advances, n),
			})
		}
	}
	if e.ReapCode != txReapFailureCode {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-attribution"),
			Message: fmt.Sprintf("the reaped connection was answered %q on its next message, want %q: only the reaper "+
				"arms that failure, so the slot's return could equally have been a rollback", e.ReapCode, txReapFailureCode),
		})
	} else if e.ReapMessage != txReapFailureMessage {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-attribution"),
			Message: fmt.Sprintf("the reaped connection was told %q, want the text pinned in txReapFailureMessage "+
				"(rmp #2560); if that text has been corrected, update this arm with the ticket", e.ReapMessage),
		})
	}
	if e.TerminateOutcome != txTermOutcomeOK {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "terminate"),
			Message: fmt.Sprintf("TerminateTransaction on a live id returned %s, want %s",
				e.TerminateOutcome, txTermOutcomeOK),
		})
	}
	if !e.TerminatedGone {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "terminate-registry"),
			Message: "the terminated transaction was still listed after the termination settled",
		})
	}
	if e.LogoffCommitCode != txQuotaLogoffRefusalCode {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "logoff-commit-code"),
			Message: fmt.Sprintf("the COMMIT from a de-authorised session was answered %q (%q), want %q",
				e.LogoffCommitCode, e.LogoffCommitMessage, txQuotaLogoffRefusalCode),
		})
	} else if !strings.Contains(e.LogoffCommitMessage, txQuotaLogoffOriginState) {
		// The AUTHENTICATION gate and the STATE-MACHINE gate share one code, so the
		// code alone cannot say which refused. failTransition names the ORIGIN state,
		// and only the auth gate can be reached from TX_READY: a message illegal in
		// TX_READY by the state machine would name FAILED instead.
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "logoff-commit-origin"),
			Message: fmt.Sprintf("the refused COMMIT read %q, which does not name the origin state %s: the "+
				"authentication gate and the state machine share a code, so without the origin state the refusal "+
				"cannot be attributed to the de-authorisation", e.LogoffCommitMessage, txQuotaLogoffOriginState),
		})
	}
	if !e.LogoffEntryGone {
		// THE clause rmp #2482 carries over from the #2481 security review.
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "logoff-reclaim"),
			Message: "the de-authorised session's transaction was still listed after its COMMIT was refused: the " +
				"refusal did not RECLAIM the transaction, only declined the message. The WAL-frame and ghost-node " +
				"oracles cannot see this — a refusal that skipped enterFailed writes nothing either — so the registry " +
				"and the quota are the only instruments that can",
		})
	}
	return v
}

// checkBoltTxQuotaResidue adjudicates what the run left behind: no ghost from the
// refused COMMIT, every honest write durable, and a clock seam used exactly as
// the arm's arithmetic assumes.
func checkBoltTxQuotaResidue(e *BoltTxQuotaEvidence) []Violation {
	var v []Violation
	if e.GhostsLive != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: txOp(e.Arm, "ghost-live"),
			Message: fmt.Sprintf("%d node(s) labelled %s exist in the live engine: the write staged by the "+
				"de-authorised session survived the refused COMMIT", e.GhostsLive, txQuotaGhostLabel),
		})
	}
	if e.GhostsRecovered != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "ghost-recovered"),
			Message: fmt.Sprintf("%d node(s) labelled %s survived WAL replay: a write a refused COMMIT should have "+
				"discarded reached the durable log", e.GhostsRecovered, txQuotaGhostLabel),
		})
	}
	for i := range e.Windows {
		v = append(v, checkBoltTxWindow(e.Arm, &e.Windows[i])...)
	}
	if e.HonestLive != len(e.Windows) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "honest-census"),
			Message: fmt.Sprintf("the live engine holds %d node(s) labelled %s, want %d (one per honest window)",
				e.HonestLive, txQuotaHonestLabel, len(e.Windows)),
		})
	}
	if e.HonestRecovered != e.HonestLive {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "honest-recovered"),
			Message: fmt.Sprintf("WAL replay recovered %d node(s) labelled %s, but %d were acknowledged live",
				e.HonestRecovered, txQuotaHonestLabel, e.HonestLive),
		})
	}
	if e.TimersTotal != e.WantTimers {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming"),
			Message: fmt.Sprintf("%d timer(s) were armed on the injected clock, want %d (one per ACCEPTED BEGIN plus "+
				"one per deliberate touch): a refused BEGIN returns before txActive is set, so it must arm none, and a "+
				"count that drifted would move the reap ordinal", e.TimersTotal, e.WantTimers),
		})
	}
	if e.Untils != e.TimersTotal {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-until"),
			Message: fmt.Sprintf("Until was called %d time(s) against %d timer registration(s); syncTxTimer calls "+
				"clk.Until exactly once per arming (bolt/server/serve.go:1178), so these must match",
				e.Untils, e.TimersTotal),
		})
	}
	if e.Tickers != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-ticker"),
			Message: fmt.Sprintf("the server registered %d ticker(s) on the injected clock, want 0: a ticker consumes "+
				"advances the reap ordinal depends on", e.Tickers),
		})
	}
	return v
}

// checkBoltTxQuotaNonVacuity proves the run actually exercised the cap, so a
// green contract verdict cannot come from an arm that quietly did nothing. A
// shortfall here says the RUN was uninformative, never that the server is faulty
// (rmp #2470).
func checkBoltTxQuotaNonVacuity(e *BoltTxQuotaEvidence) []Violation {
	var v []Violation
	shortfall := func(clause, msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: txOp(e.Arm, clause), Message: msg})
	}
	// THE geometry clause. At a cap of one, "BEGIN never works" and "the cap
	// fired" produce the same wire trace.
	if e.Limit < 2 {
		shortfall("nonvacuity-limit", fmt.Sprintf("the cap installed was %d: at a limit below 2 the FIRST BEGIN a "+
			"principal sends is its last, so a refusal cannot be told apart from a BEGIN that never works at all",
			e.Limit))
	}
	if len(e.CapModes) != e.Limit {
		shortfall("nonvacuity-cap", fmt.Sprintf("the principal filled %d of its %d slots before the refusal was "+
			"attempted: the refusal then grades something other than the cap", len(e.CapModes), e.Limit))
	}
	if !slices.Contains(e.CapModes, "r") || !slices.Contains(e.CapModes, "w") {
		shortfall("nonvacuity-mode", fmt.Sprintf("the cap was filled with modes %v: without both kinds the run never "+
			"shows that the cap counts a READ transaction exactly as it counts a write", e.CapModes))
	}
	for _, phase := range txQuotaAccepted {
		if e.beginFor(phase) == nil {
			shortfall("nonvacuity-roster", fmt.Sprintf("the run never drove the %q BEGIN, so one of the routes by "+
				"which a slot comes back was not exercised", phase))
		}
	}
	for _, phase := range txQuotaRefused {
		if e.beginFor(phase) == nil {
			shortfall("nonvacuity-roster", fmt.Sprintf("the run never drove the %q BEGIN", phase))
		}
	}
	var refusals int
	for i := range e.Begins {
		if !e.Begins[i].Accepted {
			refusals++
		}
	}
	if refusals < 2 {
		shortfall("nonvacuity-refusal", fmt.Sprintf("the run observed %d refusal(s): one refusal cannot show a cap "+
			"COUNTING, because a server that refused every BEGIN would produce it too", refusals))
	}
	if e.acceptedBegins() <= e.Limit {
		shortfall("nonvacuity-reclaim", fmt.Sprintf("the server accepted %d BEGIN(s) against a cap of %d: no slot was "+
			"ever returned, so every reclamation clause is about something that did not happen",
			e.acceptedBegins(), e.Limit))
	}
	if b := e.beginFor(txQuotaPhaseOther); b == nil || b.Principal == e.PrincipalA {
		shortfall("nonvacuity-control", fmt.Sprintf("the per-principal control never ran under a SECOND identity: "+
			"without it, \"the cap refused\" is equally true of a server-wide limit (principals %q and %q)",
			e.PrincipalA, e.PrincipalB))
	}
	if e.Advances != txQuotaAdvances {
		shortfall("nonvacuity-advance", fmt.Sprintf("the reap composition made %d advance(s), want %d: the first "+
			"moves the clock so a touch can genuinely re-arm, and the second reaches the untouched deadline",
			e.Advances, txQuotaAdvances))
	}
	if e.Touches == 0 {
		shortfall("nonvacuity-stagger", "no open transaction was touched between the advances: without the stagger "+
			"one advance reaps every slot at once and the reaper cannot be shown to free exactly ONE")
	}
	if !slices.Contains(e.PredictedReapOrdinals, reapNever) {
		shortfall("nonvacuity-stagger", fmt.Sprintf("the model predicts %v, with nothing left open: a plan in which "+
			"every transaction falls due together cannot tell a correct reaper from one that empties the registry",
			e.PredictedReapOrdinals))
	}
	if e.TimersTotal == 0 || e.Nows == 0 {
		shortfall("nonvacuity-seam", fmt.Sprintf("the server registered %d timer(s) and made %d Now call(s) on the "+
			"injected clock: the SetClock seam never reached the session, so every virtual-time clause is inert",
			e.TimersTotal, e.Nows))
	}
	if e.TotalBound <= e.IdleBound {
		shortfall("nonvacuity-bound", fmt.Sprintf("the total-lifetime bound is %s against an idle bound of %s: "+
			"effectiveTxDeadline reaps at the EARLIER of the two, so this run measured the total-lifetime reaper",
			e.TotalBound, e.IdleBound))
	}
	var appended uint64
	for i := range e.Windows {
		appended += e.Windows[i].framesAppended()
	}
	if appended == 0 {
		shortfall("nonvacuity-wal", "no honest window appended a WAL frame: the frame counter is not a live instrument "+
			"here, so \"the staged write never reached the log\" proves nothing")
	}
	if e.HonestLive == 0 {
		shortfall("nonvacuity-census", fmt.Sprintf("no node labelled %s reached the engine, so the census cannot "+
			"witness that %s is absent by anything other than a query that finds nothing at all",
			txQuotaHonestLabel, txQuotaGhostLabel))
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
func (e *BoltTxQuotaEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-tx-quota evidence (arm=%s seed=%d):", e.Arm, e.Seed)
	fmt.Fprintf(&b, "\n  cap: limit=%d principals=%s/%s modes=%v registry-at-cap=%d",
		e.Limit, e.PrincipalA, e.PrincipalB, e.CapModes, e.RegistryAtCap)
	fmt.Fprintf(&b, "\n  geometry: idle=%s total=%s step=%s advances=%d touches=%d",
		e.IdleBound, e.TotalBound, e.Step, e.Advances, e.Touches)
	fmt.Fprintf(&b, "\n  clock: timers=%d (want %d) untils=%d tickers=%d nows-observed=%t",
		e.TimersTotal, e.WantTimers, e.Untils, e.Tickers, e.Nows > 0)
	b.WriteString("\n  begins:")
	for i := range e.Begins {
		bg := &e.Begins[i]
		fmt.Fprintf(&b, "\n    %-16s principal=%-12s mode=%s accepted=%-5t id=%-4s want=%-4s registry=%d->%d timers=%d->%d principal-open=%d in-ceiling=%t",
			bg.Phase, bg.Principal, bg.Mode, bg.Accepted, bg.IDSuffix, bg.WantSuffix,
			bg.RegistryBefore, bg.RegistryAfter, bg.TimersBefore, bg.TimersAfter,
			bg.PrincipalOpenAfter, bg.WithinCeiling)
		if !bg.Accepted {
			fmt.Fprintf(&b, "\n      refused: %s | %s", bg.Code, bg.Message)
		}
	}
	fmt.Fprintf(&b, "\n  refusal message as recomputed: %t (want %q)",
		e.beginFor(txQuotaPhaseOverCap) != nil && e.beginFor(txQuotaPhaseOverCap).Message == e.WantRefusalMessage,
		e.WantRefusalMessage)
	fmt.Fprintf(&b, "\n  post-refusal session: served=%t code=%q", e.PostRefusalAccepted, e.PostRefusalCode)
	fmt.Fprintf(&b, "\n  reap: predicted=%v observed=%v probe-fires=%v code=%s message-as-pinned=%t",
		e.PredictedReapOrdinals, e.ObservedReapOrdinals, e.ReapProbeFires,
		e.ReapCode, e.ReapMessage == txReapFailureMessage)
	fmt.Fprintf(&b, "\n  terminate: outcome=%s gone=%t", e.TerminateOutcome, e.TerminatedGone)
	fmt.Fprintf(&b, "\n  logoff-refused commit: code=%s entry-reclaimed=%t names-origin=%t",
		e.LogoffCommitCode, e.LogoffEntryGone, strings.Contains(e.LogoffCommitMessage, txQuotaLogoffOriginState))
	b.WriteString("\n  honest windows:")
	for i := range e.Windows {
		w := &e.Windows[i]
		fmt.Fprintf(&b, "\n    %-12s openTx=%d committed=%t frames+%d bytes-moved=%t",
			w.Name, w.OpenTx, w.Committed, w.framesAppended(), w.bytesAppended() > 0)
	}
	fmt.Fprintf(&b, "\n  census: ghosts=%d honest=%d | after recovery: ghosts=%d honest=%d",
		e.GhostsLive, e.HonestLive, e.GhostsRecovered, e.HonestRecovered)
	return b.String()
}
