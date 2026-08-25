package sim

// bolt_stream_semantics.go — the Bolt streaming surface under simulation
// (rmp #2484).
//
// # The gap this closes
//
// Every result stream the harness had ever opened was drained with a single
// PULL {n:-1, qid:-1}. So the whole of Bolt's paging machinery was driven by no
// scenario at all: no PULL carried a finite n, `has_more` was therefore always
// false and never once observed true, DISCARD did not appear anywhere in the
// package, and no arm ever addressed a stream by an explicit qid. The three
// server code paths that implement paging — handlePull's n limit and its
// look-ahead peek, handleDiscard's own n accounting, and the qid validation both
// share — were reachable only from `bolt/server`'s unit tests.
//
// # The task's premise was half wrong, and the refutation is the point
//
// The task asked for "QID multiplexing" and "QID routing": several open result
// streams on one session, addressed by qid. This server has neither, and cannot,
// and that was VERIFIED in the code rather than inferred from a passing test:
//
//   - handlePull refuses any qid >= 0 outright — `if m.QID >= 0` returns
//     Neo.ClientError.Request.Invalid with "no such query: qid %d"
//     (bolt/server/session.go:1240-1243). handleDiscard carries the identical
//     guard (:1421-1424).
//   - RUN's SUCCESS always reports `"qid": int64(-1)` (:1223), so no positive qid
//     is ever minted for a client to send back.
//   - A second RUN while a stream is open is refused by the state machine:
//     handleRun requires READY or TX_READY (:1075), and a live stream leaves the
//     session in STREAMING or TX_STREAMING (bolt/server/state.go:181-228 and
//     :230-277).
//
// There is therefore exactly ONE open stream per session at any instant, and
// "routing" is a property to REFUTE, not to test. What does exist — and is the
// honest reading of the objective — is that cursors ACCUMULATE across SEQUENTIAL
// RUNs inside one explicit transaction: each RUN appends to `tx.results`
// (bolt/server/tx.go:135 and :140), the slice is cleared only by
// Tx.closeCursors on COMMIT/ROLLBACK, and Options.MaxInFlightPerConnection is the
// bound (session.go:518-526 counts it, :1086 refuses past it). This scenario
// therefore pins the contract that is real:
//
//   - a positive qid is REFUSED on both PULL and DISCARD, with the exact code
//     AND message text, and the refused message delivers no row;
//   - a second RUN while streaming is refused, and the refusal is attributed to
//     the STATE gate by the ORIGIN STATE failTransition names (:1884-1891) —
//     necessary because the authentication gate one line above (:1072) returns
//     the SAME code, so a code match alone cannot tell the two apart (the
//     gate-attribution discipline of rmp #2481);
//   - cursors accumulate across sequential RUN+PULL cycles in one transaction and
//     the cap refuses the RUN that would exceed it with a typed FAILURE, not a
//     stall.
//
// # The load-bearing oracle: an independent reference drain
//
// The paging oracle is not the server agreeing with itself. The same query is
// drained TWICE on two connections: once with a single PULL {n:-1}, which is the
// reference record set, and once with a seed-drawn sequence of PULL {n} pages.
// The concatenation of the pages must equal the reference ELEMENT BY ELEMENT and
// IN ORDER, comparing decoded PackStream values through the same value+type
// discipline [compareWireRow] applies — the dynamic Go type IS the wire encoding,
// so an Integer arriving as a String, or a Float where an Integer belongs, is a
// divergence even when a String() rendering of the two would match.
//
// The partial-DISCARD arm sharpens that into an exact statement about which rows
// were skipped: it pages a seed-chosen prefix, DISCARDs a seed-chosen window, then
// PULLs the remainder, and requires prefix++remainder to equal the reference with
// exactly that window cut out of it. A DISCARD that dropped one row too many, or
// one too few, moves the suffix and fails; "the session still works afterwards"
// could not see either.
//
// # What DISCARD abandons, and what it does not
//
// DISCARD abandons DELIVERY, not the statement, and the arm confirms it where it
// has an effect to confirm. An autocommit write commits during the drain rather
// than at RUN (session.go's runCtx comment at :1143-1148), and DISCARD drains the
// cursor exactly as PULL does. Measured: an autocommit CREATE whose delivery is
// DISCARDed delivers ZERO records, its terminal SUCCESS still reports
// `stats: {nodes-created: 1, labels-added: 1, properties-set: 1,
// contains-updates: true}` and a bookmark, and the node is present both in the
// live engine and in a graph reopened through real WAL recovery after a crash.
//
// # Seed-purity: counts, not byte totals
//
// The in-flight arm brackets each transaction between two readings of
// wal.Writer.Stats. Frame COUNTS are a pure function of the seed; byte totals are
// not, because a created node's hidden internal key is minted by cypher/exec as
// "__cx_"+hex(n) from a PROCESS-GLOBAL counter, so the same seed yields frames of
// different widths depending on how many nodes every other test in the process
// created first (the same limitation documented in bolt_auth_surface.go and
// schema_mutation.go). The refused half therefore asserts bytes+0 — zero is zero
// at any width — while the committed half asserts only that frames MOVED, and no
// byte total is rendered for it.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// The Bolt failure codes this surface must produce, spelled out so the oracle
// pins the EXACT code a driver sees. A qid refusal answered with a
// Security or a Transaction code, or a cap breach answered with the generic
// Request.Invalid, would change what a client is told and is a defect a
// "some failure" assertion would miss.
const (
	// streamCodeRequestInvalid is what both the qid guard and the state machine's
	// failTransition return.
	streamCodeRequestInvalid = "Neo.ClientError.Request.Invalid"
	// streamCodeLimitExceeded is what the per-connection in-flight cursor cap
	// returns (bolt/server/session.go:1092).
	streamCodeLimitExceeded = "Neo.ClientError.General.LimitExceeded"
)

// The shape of the paged workload.
const (
	// boltStreamRows is the reference result size. It is PRIME so a drawn page
	// size almost never divides it, which keeps the final page short and the
	// has_more transition on a row boundary the harness did not choose.
	boltStreamRows = 97
	// boltStreamMaxPage bounds a drawn page size. Small enough that the drain takes
	// many pages (so has_more is observed true repeatedly), large enough that pages
	// differ from one another.
	boltStreamMaxPage = 16
	// boltStreamInFlightCap is the Options.MaxInFlightPerConnection the scenario's
	// server runs with. The server's own default is 1024, which would need 1024
	// RUN+PULL round trips inside one transaction to reach — neither a short-layer
	// budget nor a legible report.
	boltStreamInFlightCap = 3
)

// boltStreamColumns are the reference query's columns, in order. They span five
// distinct PackStream encodings — Integer, String, Boolean, List and Float — so
// the element-by-element comparison is a statement about the wire encoding and
// not only about a scalar count.
var boltStreamColumns = []string{"n", "s", "third", "pair", "half"}

// boltStreamQuery is the reference query. It touches no node, so every value it
// yields is a pure function of the query text: there is no created-node internal
// key in the rows and therefore nothing in the reference that varies with process
// history.
func boltStreamQuery() string {
	return fmt.Sprintf(
		"UNWIND range(1, %d) AS x RETURN x AS n, toString(x) AS s, x %% 3 = 0 AS third, [x, x+1] AS pair, x / 2.0 AS half",
		boltStreamRows)
}

// The labels the write arms use. Each is distinct so a census can attribute a
// node to the arm that created it by label alone.
const (
	// boltStreamDiscardLabel is written by the statement whose delivery is
	// DISCARDed. It must SURVIVE: DISCARD drops rows, not writes.
	boltStreamDiscardLabel = "StreamDiscarded"
	// boltStreamCommitLabel is written by the transaction that stays under the cap
	// and COMMITs. It must survive too, and it is what proves the WAL bracket is a
	// live instrument rather than a constant zero.
	boltStreamCommitLabel = "StreamCommitted"
	// boltStreamDoomedLabel is written by the transaction whose next RUN breaches
	// the cap. It must NOT survive: the cap breach moves the session to FAILED,
	// which rolls the transaction back (session.go enterFailed).
	boltStreamDoomedLabel = "StreamDoomed"
)

// boltStreamSeedMix decorrelates the SimDisk sub-seed from the scenario seed so
// the disk's draw stream is independent of the page-size draws.
const boltStreamSeedMix = 0x2484_5EED

// boltStreamReplyBound is the read deadline armed on the client connection around
// the decisive message of the cap arm. It is the anti-stall oracle: the mandate is
// that saturation is answered with a typed error rather than by blocking, so the
// harness refuses to wait indefinitely for the answer. A reply that does not
// arrive inside the bound is a harness error naming the stall, never a pass.
const boltStreamReplyBound = 30 * time.Second

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// BoltStreamPage is one stream-consuming exchange: a PULL {n} or a DISCARD {n}
// and the terminal reply it drew.
type BoltStreamPage struct {
	// Terminal is the terminal reply's kind: "SUCCESS", "FAILURE", "IGNORED", or
	// "" when the exchange produced no terminal at all. It is recorded as a kind
	// rather than as a bool because a RUN SUCCESS is not an acknowledgement and an
	// IGNORED is not one either; only the terminal reply of the drain is.
	Terminal string
	// Code is the failure code when Terminal is "FAILURE", else "".
	Code string
	// Requested is the n the client asked for.
	Requested int64
	// Delivered is how many RECORDs actually arrived before the terminal reply.
	Delivered int
	// HasMore is the has_more the terminal SUCCESS reported.
	HasMore bool
	// Bookmarked reports whether the terminal SUCCESS carried a "bookmark" key.
	// The bookmark rides on the TERMINAL reply only, so it doubles as an
	// independent witness of which page the server considered final.
	Bookmarked bool
	// Discard records that this exchange was a DISCARD rather than a PULL.
	Discard bool
}

// BoltStreamRefusal is one probe whose decisive message must be refused, together
// with everything needed to attribute the refusal to the gate under test.
type BoltStreamRefusal struct {
	// Name identifies the probe in a violation message.
	Name string
	// WantCode / GotCode are the failure code the probe must be told and the one it
	// received ("" when the probe was not refused at all).
	WantCode, GotCode string
	// WantMessage is the exact FAILURE text the probe must be told, or "" to waive
	// the exact-text check in favour of WantStateInMessage.
	WantMessage string
	// GotMessage is the FAILURE text actually received.
	GotMessage string
	// WantStateInMessage is the session state failTransition must name, or "" when
	// the refusal is not a failTransition.
	//
	// It exists because handleRun's authentication gate (session.go:1072) and its
	// state gate (:1075) both return failTransition's
	// Neo.ClientError.Request.Invalid, so a code match cannot say which refused.
	// failTransition reports the ORIGIN state (:1885), which is the discriminator:
	// a refusal by the state gate on a live stream names STREAMING or TX_STREAMING,
	// whereas the auth gate would name whatever state the de-authorised session sat
	// in.
	WantStateInMessage string
	// NextTerminal is the terminal kind the NEXT request on the same connection
	// drew. Measured "IGNORED": the refusal routes through enterFailed, and a
	// FAILED session soft-ignores request-phase messages until RESET.
	NextTerminal string
	// Delivered is how many RECORDs the refused message delivered. It must be 0.
	Delivered int
	// Accepted records what actually happened: the decisive message drew a SUCCESS.
	Accepted bool
	// PriorAck records that an earlier statement on the SAME connection was
	// acknowledged through its terminal reply. Without it, "the message was
	// refused" is equally explained by a connection that never worked.
	PriorAck bool
	// RecoveredAfterReset records that a RUN+PULL after RESET was acknowledged on
	// the same connection, which is what makes the refusal SCOPED rather than
	// terminal.
	RecoveredAfterReset bool
}

// BoltStreamTxArm is one explicit transaction driven either to COMMIT under the
// in-flight cursor cap or to the cap's refusal, bracketed by the WAL counters.
type BoltStreamTxArm struct {
	// Name identifies the arm.
	Name string
	// RefusalCode / RefusalMessage are the typed FAILURE the over-cap RUN drew.
	RefusalCode, RefusalMessage string
	// FramesBefore / FramesAfter bracket the arm with wal.Stats.Frames.
	FramesBefore, FramesAfter uint64
	// BytesBefore / BytesAfter bracket it with wal.Stats.Bytes. Only a ZERO delta
	// is adjudicated or rendered; see the file comment on seed-purity.
	BytesBefore, BytesAfter uint64
	// Cycles is how many RUN+PULL cycles the server accepted inside the
	// transaction. Each accepted RUN appends one cursor to tx.results.
	Cycles int
	// CursorsAtRefusal is the "open=N" figure the cap's own diagnostic reported,
	// parsed back out of the message. It is the server's own count of accumulated
	// cursors, so agreeing with Cycles is a cross-check between two independent
	// accountings.
	CursorsAtRefusal int
	// RefusalObserved records that a typed FAILURE arrived within
	// boltStreamReplyBound rather than the round trip stalling.
	RefusalObserved bool
	// Committed records that the arm's COMMIT was acknowledged.
	Committed bool
}

// framesAppended reports how many WAL frames the arm appended. The receiver is a
// pointer because a BoltStreamTxArm is well past the 80-byte threshold gocritic
// flags for a value receiver, and these are called per arm in both the
// adjudicator and the renderer.
func (a *BoltStreamTxArm) framesAppended() uint64 { return a.FramesAfter - a.FramesBefore }

// bytesAppended reports how many WAL bytes the arm appended.
func (a *BoltStreamTxArm) bytesAppended() uint64 { return a.BytesAfter - a.BytesBefore }

// BoltStreamEvidence is everything one run observed. It holds OBSERVATIONS only:
// every expectation is derived by the checkers from the reference drain, which is
// what lets a test perturb one field and prove the matching clause fires.
type BoltStreamEvidence struct {
	// Seed is the seed the run was built from.
	Seed uint64

	// ── the reference drain (a single PULL -1 on its own connection) ──────────

	// Reference is the record set the reference drain produced, decoded.
	Reference [][]packstream.Value
	// ReferenceTerminal / ReferenceHasMore are its terminal reply's kind and
	// has_more. A single PULL -1 always exhausts the stream, so has_more must be
	// false here; recording it makes the paged arm's true readings comparable
	// against a measured false rather than against an assumption.
	ReferenceTerminal string
	ReferenceHasMore  bool

	// ── the paging arm ───────────────────────────────────────────────────────

	// PageSizes are the seed-drawn n values, in the order they were sent.
	PageSizes []int64
	// Pages is one entry per PULL {n} of the paged drain.
	Pages []BoltStreamPage
	// Paged is the concatenation of every RECORD the paged drain delivered.
	Paged [][]packstream.Value
	// PagingReusable records that a fresh RUN+PULL on the SAME connection was
	// acknowledged after the paged drain completed, i.e. the session returned to
	// READY.
	PagingReusable bool

	// ── the discard-window arm ───────────────────────────────────────────────

	// WindowPageSizes are the drawn n values of the prefix pages, in order.
	WindowPageSizes []int64
	// WindowPrefixPages is how many drawn pages were pulled before the DISCARD.
	WindowPrefixPages int
	// WindowPrefixPagesSeen is one entry per prefix page.
	WindowPrefixPagesSeen []BoltStreamPage
	// WindowPrefix is what those pages delivered, concatenated.
	WindowPrefix [][]packstream.Value
	// WindowDiscardN is the seed-drawn n the DISCARD asked to drop.
	WindowDiscardN int64
	// WindowDiscard is the DISCARD exchange itself.
	WindowDiscard BoltStreamPage
	// WindowSuffix is what the closing PULL -1 delivered.
	WindowSuffix [][]packstream.Value
	// WindowSuffixPage is that closing exchange.
	WindowSuffixPage BoltStreamPage
	// WindowReusable records that a fresh RUN+PULL on the same connection was
	// acknowledged afterwards.
	WindowReusable bool

	// ── the discard-effect arm ───────────────────────────────────────────────

	// EffectDiscard is the DISCARD exchange that abandoned the write's delivery.
	EffectDiscard BoltStreamPage
	// EffectStats is the "stats" map the DISCARD's terminal SUCCESS reported,
	// flattened to int64 counters (a bool counter is recorded as 1).
	EffectStats map[string]int64
	// EffectLive / EffectRecovered are how many nodes carrying
	// boltStreamDiscardLabel exist in the live engine and in a graph reopened
	// through real recovery after a crash.
	EffectLive, EffectRecovered int
	// EffectReusable records that a fresh RUN+PULL on the same connection was
	// acknowledged after the mid-stream DISCARD, which is the AC's "a post-DISCARD
	// RUN succeeds" stated on the connection that did the discarding.
	EffectReusable bool

	// ── the qid and second-RUN arms ──────────────────────────────────────────

	// Refusals are the refusal probes in the order they ran.
	Refusals []BoltStreamRefusal
	// ProbedQIDs are the qid values the qid probes actually sent. Recorded so the
	// non-vacuity gate can require them to be POSITIVE: a probe that sent -1 would
	// be asserting the refusal of the current-stream qid.
	ProbedQIDs []int64
	// RunQIDs are the qid values every RUN SUCCESS in the run reported. Every one
	// must be -1, which is what makes "no positive qid is ever minted" an assertion
	// rather than an argument.
	RunQIDs []int64
	// QIDControlRows is how many RECORDs the qid=-1 control drain delivered on a
	// stream identical to the refused probes'. Zero would mean "every PULL is
	// refused", under which the refusal clauses say nothing about the qid.
	QIDControlRows int

	// ── the in-flight cursor arms ────────────────────────────────────────────

	// TxArms are the committed and the doomed transaction, in that order.
	TxArms []BoltStreamTxArm
	// DoomedLive / DoomedRecovered are how many nodes carrying
	// boltStreamDoomedLabel exist live and after recovery. Both must be 0.
	DoomedLive, DoomedRecovered int
	// CommittedLive / CommittedRecovered are the same census for
	// boltStreamCommitLabel. Both must equal boltStreamInFlightCap: the cap allows
	// exactly that many cursors, and the arm writes one node per cursor.
	CommittedLive, CommittedRecovered int

	// ── the concurrent stall arm ─────────────────────────────────────────────

	// StallArm marks this evidence as the concurrent slow-consumer arm rather than
	// the deterministic battery, so the checkers adjudicate the stall clauses and
	// skip the deterministic ones.
	StallArm bool
	// StallRecordsPulled is how many RECORDs the slow consumer drained before it
	// stopped, StallParked whether the stream was still open when the consumer
	// stalled, and StallClosedCleanly whether the connection tore down without an
	// unexpected transport fault.
	StallRecordsPulled int
	StallParked        bool
	StallClosedCleanly bool
	// StallBufferedPeak is the MAXIMUM number of bytes observed queued toward the
	// stalled consumer, sampled by [boltStreamPollParked].
	//
	// What it can and cannot show was verified in the harness's own code rather
	// than assumed. [halfPipe.write] fills to exactly [simConnBufferSize] and then
	// parks (internal/sim/simconn.go:96-124: it chunks each write to the space
	// remaining, so the buffer NEVER exceeds the bound). So "the server did not
	// buffer past the bound" is an invariant of the pipe, not a property of the
	// server, and a clause asserting it cannot fail against a real server. It is
	// kept as a guard on the harness itself and is labelled as such.
	//
	// What the peak DOES establish is that the writer really was blocked when the
	// consumer tore the connection down: a peak at the bound means the server had
	// rows it could not hand over. The server-side HEAP bound — that the page was
	// not materialised into a second in-memory copy ahead of the wire — is measured
	// where it can be measured, by the live-heap gate in
	// bolt/server/streaming_backpressure_test.go, and is not restated here.
	StallBufferedPeak int
	// StallSurfaceValues are the integers a FRESH connection's paged drain
	// delivered after the stalled connection was torn down mid-stream, and
	// StallSurfacePages the pages that drain took.
	//
	// The values are stored rather than a comparison verdict, because the
	// reference here is ARITHMETIC — the drain runs UNWIND range(1, N), so the
	// expected sequence is 1..N and the checker derives it without asking the
	// server anything. That keeps the stall arm's oracle independent without
	// paying for a second full reference drain.
	StallSurfaceValues []int64
	StallSurfacePages  []BoltStreamPage
}

// -----------------------------------------------------------------------------
// The runner
// -----------------------------------------------------------------------------

// RunBoltStreamSemantics drives the whole deterministic streaming surface once
// against a WAL-backed server whose in-flight cursor cap is
// boltStreamInFlightCap, and returns the evidence.
//
// It is bit-reproducible from seed: every arm is a fixed lock-step script on its
// own connection, the page sizes are drawn from one seeded stream in a fixed
// order, and the only other seeded component is the SimDisk the WAL lives on. No
// field it records is a byte total of a committed write; see the file comment.
//
// The returned error is reserved for harness failures (the store would not open,
// a dial was refused, a reply did not arrive inside boltStreamReplyBound). A
// refused message or an unexpected reply shape is EVIDENCE, not an error.
func RunBoltStreamSemantics(ctx context.Context, seed uint64) (*BoltStreamEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^boltStreamSeedMix), 0) // faultRate 0: this scenario faults nothing
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream open store: %w", err)
	}
	srv, err := NewSimServerInFlight(st.Engine(), clock.Real(), boltStreamInFlightCap)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("sim: bolt-stream server: %w", err)
	}

	ev := &BoltStreamEvidence{Seed: seed}
	r := &boltStreamRunner{srv: srv, st: st, ev: ev, rng: NewSeed(seed)}
	if err := r.driveAll(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}

	// Census the live engine before tearing anything down.
	if err := r.censusLive(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}

	// Then crash (drop the engine, keep the SimDisk image) and reopen through real
	// recovery. A write that reached the WAL without becoming visible in the live
	// engine — or a rolled-back transaction that left a frame behind — is invisible
	// to the live census and surfaces only here.
	_ = srv.Close()
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	if err := r.censusRecovered(ctx, st2); err != nil {
		return nil, err
	}
	return ev, nil
}

// boltStreamRunner threads the server, the store, the seeded draw stream and the
// accumulating evidence through the arms. It exists so each arm is a short method
// that cannot forget to record what it observed.
type boltStreamRunner struct {
	srv *SimServer
	st  *SimStore
	ev  *BoltStreamEvidence
	rng *Seed
}

// driveAll runs every arm in a fixed order.
//
// The reference drain comes first because every other arm's oracle is stated
// against it. The refusal arms come before the write arms so any row a refused
// message managed to deliver is attributed to the refusal rather than masked by a
// later honest drain.
func (r *boltStreamRunner) driveAll(ctx context.Context) error {
	steps := []func(context.Context) error{
		r.armReference,
		r.armPaging,
		r.armDiscardWindow,
		r.armQIDRefusals,
		r.armSecondRun,
		r.armDiscardEffect,
		r.armInFlight,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// connect dials the server and completes the handshake plus HELLO/LOGON. The
// caller closes the returned client.
func (r *boltStreamRunner) connect(ctx context.Context) (*WireClient, error) {
	c, err := r.srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream dial: %w", err)
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-stream connect: %w", err)
	}
	return c, nil
}

// run sends a RUN, records the qid its SUCCESS reported, and returns the reply.
// Recording the qid HERE rather than in one arm is what makes
// [BoltStreamEvidence.RunQIDs] a census of the whole run: every RUN the scenario
// issues passes through this one path.
func (r *boltStreamRunner) run(c *WireClient, query string) (any, error) {
	resp, err := c.Run(query, nil)
	if err != nil {
		return nil, err
	}
	if s, ok := resp.(*proto.Success); ok {
		if q, ok := s.Metadata["qid"].(int64); ok {
			r.ev.RunQIDs = append(r.ev.RunQIDs, q)
		}
	}
	return resp, nil
}

// drawPageSizes draws the page-size sequence for one paged drain: values in
// [1, boltStreamMaxPage], accumulated until the total reaches boltStreamRows.
//
// The loop stops on the FIRST draw that reaches the total, so no strict prefix of
// the sequence can drain the stream. The paged drain therefore uses exactly
// len(sizes) pages, which the contract asserts — a drain that finished early, or
// needed one more page, is a divergence rather than a shrug.
func (r *boltStreamRunner) drawPageSizes() []int64 {
	var sizes []int64
	total := int64(0)
	for total < boltStreamRows {
		n := int64(1 + r.rng.IntN(boltStreamMaxPage))
		sizes = append(sizes, n)
		total += n
	}
	return sizes
}

// armReference drains the query once with a single PULL {n:-1} on its own
// connection. This is the independent reference every paging oracle is stated
// against: it is produced by a different message sequence from the paged drain,
// so agreement between the two is a property of the server and not of one code
// path agreeing with itself.
func (r *boltStreamRunner) armReference(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if _, err := r.run(c, boltStreamQuery()); err != nil {
		return fmt.Errorf("sim: bolt-stream reference RUN: %w", err)
	}
	recs, terminal, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("sim: bolt-stream reference PULL: %w", err)
	}
	page := boltStreamMakePage(-1, false, recs, terminal)
	r.ev.Reference = boltStreamDecodeRows(recs)
	r.ev.ReferenceTerminal, r.ev.ReferenceHasMore = page.Terminal, page.HasMore
	return nil
}

// armPaging drains the same query with the drawn PULL {n} sequence and records
// every page, then checks the session is reusable by running one more statement to
// its terminal reply on the same connection.
func (r *boltStreamRunner) armPaging(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	r.ev.PageSizes = r.drawPageSizes()
	if _, err := r.run(c, boltStreamQuery()); err != nil {
		return fmt.Errorf("sim: bolt-stream paged RUN: %w", err)
	}
	for _, n := range r.ev.PageSizes {
		recs, terminal, err := c.Pull(n)
		if err != nil {
			return fmt.Errorf("sim: bolt-stream PULL %d: %w", n, err)
		}
		page := boltStreamMakePage(n, false, recs, terminal)
		r.ev.Pages = append(r.ev.Pages, page)
		r.ev.Paged = append(r.ev.Paged, boltStreamDecodeRows(recs)...)
		if page.Terminal != "SUCCESS" || !page.HasMore {
			break
		}
	}
	r.ev.PagingReusable, err = r.statementAcked(c)
	if err != nil {
		return err
	}
	return nil
}

// armDiscardWindow pages a drawn prefix, DISCARDs a drawn window mid-stream, then
// PULLs the remainder. The window is always strictly interior — at most four pages
// of at most boltStreamMaxPage rows leaves at least 33 of the 97 rows behind, and
// the drawn window is at most boltStreamMaxPage — so there is always at least one
// row before the window and at least one after it. A window at either end would
// make the removal oracle degenerate into the paging oracle.
func (r *boltStreamRunner) armDiscardWindow(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	prefixPages := 1 + r.rng.IntN(4)
	sizes := make([]int64, prefixPages)
	for i := range sizes {
		sizes[i] = int64(1 + r.rng.IntN(boltStreamMaxPage))
	}
	r.ev.WindowPageSizes = sizes
	r.ev.WindowPrefixPages = prefixPages
	r.ev.WindowDiscardN = int64(1 + r.rng.IntN(boltStreamMaxPage))

	if _, err := r.run(c, boltStreamQuery()); err != nil {
		return fmt.Errorf("sim: bolt-stream window RUN: %w", err)
	}
	for _, n := range sizes {
		recs, terminal, err := c.Pull(n)
		if err != nil {
			return fmt.Errorf("sim: bolt-stream window PULL %d: %w", n, err)
		}
		page := boltStreamMakePage(n, false, recs, terminal)
		r.ev.WindowPrefixPagesSeen = append(r.ev.WindowPrefixPagesSeen, page)
		r.ev.WindowPrefix = append(r.ev.WindowPrefix, boltStreamDecodeRows(recs)...)
	}

	dRecs, dTerm, err := c.Discard(r.ev.WindowDiscardN)
	if err != nil {
		return fmt.Errorf("sim: bolt-stream window DISCARD: %w", err)
	}
	r.ev.WindowDiscard = boltStreamMakePage(r.ev.WindowDiscardN, true, dRecs, dTerm)

	sRecs, sTerm, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("sim: bolt-stream window closing PULL: %w", err)
	}
	r.ev.WindowSuffixPage = boltStreamMakePage(-1, false, sRecs, sTerm)
	r.ev.WindowSuffix = boltStreamDecodeRows(sRecs)

	r.ev.WindowReusable, err = r.statementAcked(c)
	if err != nil {
		return err
	}
	return nil
}

// armDiscardEffect DISCARDs the delivery of an autocommit WRITE and records both
// halves of the contract: no row was delivered, and the statement's effect stands.
//
// An autocommit write commits during the DRAIN rather than at RUN
// (bolt/server/session.go:1144-1148 explains why the statement's deadline is held
// across the drain for exactly that reason), and DISCARD drains the cursor with
// the same s.result.Next() loop PULL uses (:1453-1458). So the interesting
// question is not whether DISCARD is safe but whether it silently DROPS the write
// along with the rows. Measured: it does not — the terminal SUCCESS reports the
// write stats and the node survives real recovery.
func (r *boltStreamRunner) armDiscardEffect(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	stmt := fmt.Sprintf("CREATE (:%s {k: 1}) RETURN 1 AS one", boltStreamDiscardLabel)
	if _, err := r.run(c, stmt); err != nil {
		return fmt.Errorf("sim: bolt-stream effect RUN: %w", err)
	}
	recs, terminal, err := c.Discard(-1)
	if err != nil {
		return fmt.Errorf("sim: bolt-stream effect DISCARD: %w", err)
	}
	r.ev.EffectDiscard = boltStreamMakePage(-1, true, recs, terminal)
	r.ev.EffectStats = boltStreamStats(terminal)

	r.ev.EffectReusable, err = r.statementAcked(c)
	if err != nil {
		return err
	}
	return nil
}

// armQIDRefusals drives an explicit positive qid on PULL and on DISCARD, and the
// qid=-1 control on the identical message shape.
//
// The control is what stops the refusal clauses from being satisfied by a server
// that refuses every PULL: it sends the SAME message type through the SAME helper
// with ONE field different, and must be served the whole reference record set.
func (r *boltStreamRunner) armQIDRefusals(ctx context.Context) error {
	qidPull := int64(1 + r.rng.IntN(1000))
	qidDiscard := int64(1 + r.rng.IntN(1000))
	r.ev.ProbedQIDs = append(r.ev.ProbedQIDs, qidPull, qidDiscard)

	probes := []struct {
		name string
		qid  int64
		send func(*WireClient, int64) ([]*proto.Record, any, error)
	}{
		{"pull-positive-qid", qidPull, func(c *WireClient, q int64) ([]*proto.Record, any, error) {
			return c.PullQID(-1, q)
		}},
		{"discard-positive-qid", qidDiscard, func(c *WireClient, q int64) ([]*proto.Record, any, error) {
			return c.DiscardQID(-1, q)
		}},
	}
	for _, p := range probes {
		ref := BoltStreamRefusal{
			Name:        p.name,
			WantCode:    streamCodeRequestInvalid,
			WantMessage: fmt.Sprintf("no such query: qid %d", p.qid),
		}
		if err := r.driveRefusal(ctx, &ref, func(c *WireClient) (int, any, error) {
			if _, err := r.run(c, boltStreamQuery()); err != nil {
				return 0, nil, err
			}
			recs, terminal, err := p.send(c, p.qid)
			return len(recs), terminal, err
		}); err != nil {
			return err
		}
		r.ev.Refusals = append(r.ev.Refusals, ref)
	}

	// The control, on its own connection.
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err := r.run(c, boltStreamQuery()); err != nil {
		return fmt.Errorf("sim: bolt-stream qid control RUN: %w", err)
	}
	recs, terminal, err := c.PullQID(-1, -1)
	if err != nil {
		return fmt.Errorf("sim: bolt-stream qid control PULL: %w", err)
	}
	if !isSuccess(terminal) {
		// Not an error: the control being refused is exactly what the non-vacuity
		// gate must be able to report, so it is left as evidence (zero rows).
		return nil
	}
	r.ev.QIDControlRows = len(recs)
	return nil
}

// armSecondRun drives a second RUN while a stream is open, once from STREAMING and
// once from TX_STREAMING, and requires the refusal to NAME the origin state.
//
// The needle is the whole "in state X" phrase, not the state name alone, because
// "TX_STREAMING" contains "STREAMING" as a substring: a bare containment check
// would let a TX_STREAMING refusal satisfy the STREAMING clause, which is exactly
// the gate confusion the attribution exists to prevent.
func (r *boltStreamRunner) armSecondRun(ctx context.Context) error {
	probes := []struct {
		name    string
		state   string
		message string
		openTx  bool
	}{
		{
			name:    "second-run-in-streaming",
			state:   "STREAMING",
			message: "illegal message *proto.Run in state STREAMING",
		},
		{
			name:    "second-run-in-tx-streaming",
			state:   "TX_STREAMING",
			message: "illegal message *proto.Run in state TX_STREAMING",
			openTx:  true,
		},
	}
	for _, p := range probes {
		ref := BoltStreamRefusal{
			Name:               p.name,
			WantCode:           streamCodeRequestInvalid,
			WantMessage:        p.message,
			WantStateInMessage: p.state,
		}
		openTx := p.openTx
		if err := r.driveRefusal(ctx, &ref, func(c *WireClient) (int, any, error) {
			if openTx {
				resp, err := c.Begin()
				if err != nil {
					return 0, nil, err
				}
				if !isSuccess(resp) {
					return 0, nil, fmt.Errorf("sim: bolt-stream BEGIN refused: %v", resp)
				}
			}
			// Open the stream, then send a second RUN without draining the first.
			if _, err := r.run(c, boltStreamQuery()); err != nil {
				return 0, nil, err
			}
			resp, err := r.run(c, "RETURN 1 AS one")
			if err != nil {
				return 0, nil, err
			}
			return 0, resp, nil
		}); err != nil {
			return err
		}
		r.ev.Refusals = append(r.ev.Refusals, ref)
	}
	return nil
}

// driveRefusal runs one refusal probe on its own connection: it first drives a
// statement to its acknowledged terminal reply (so the connection is proven to
// work), then calls decisive, then records what the NEXT request drew and whether
// a RESET restores the session.
//
// PriorAck is not decoration. Without it, every clause of the form "the decisive
// message was refused" is equally explained by a connection that never carried a
// working statement at all.
func (r *boltStreamRunner) driveRefusal(
	ctx context.Context,
	ref *BoltStreamRefusal,
	decisive func(*WireClient) (delivered int, terminal any, err error),
) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if ref.PriorAck, err = r.statementAcked(c); err != nil {
		return err
	}

	delivered, terminal, err := decisive(c)
	if err != nil {
		return fmt.Errorf("sim: bolt-stream refusal %q: %w", ref.Name, err)
	}
	ref.Delivered = delivered
	ref.Accepted = isSuccess(terminal)
	ref.GotCode = failureCode(terminal)
	ref.GotMessage = failureMessage(terminal)

	// What does the next request get? A refusal that routed through enterFailed
	// leaves the session FAILED, where a request-phase message is soft-IGNORED.
	next, err := r.run(c, "RETURN 1 AS one")
	if err != nil {
		// A transport fault here is an acceptable refusal shape for a terminated
		// session; record it as such rather than failing the harness.
		ref.NextTerminal = "TRANSPORT"
		return nil
	}
	ref.NextTerminal = boltStreamTerminalKind(next)

	if resp, err := c.Reset(); err != nil {
		return fmt.Errorf("sim: bolt-stream refusal %q RESET: %w", ref.Name, err)
	} else if !isSuccess(resp) {
		return nil
	}
	if ref.RecoveredAfterReset, err = r.statementAcked(c); err != nil {
		return err
	}
	return nil
}

// statementAcked drives one trivial statement to its TERMINAL reply on c and
// reports whether that terminal reply was a SUCCESS.
//
// The terminal reply is the acknowledgement and nothing else: handleRun never
// consults Result.Err() (bolt/server/session.go:1214-1220 stores the cursor and
// returns the RUN SUCCESS before any row is produced) and an IGNORED is a refusal, so a RUN SUCCESS alone
// would not do.
func (r *boltStreamRunner) statementAcked(c *WireClient) (bool, error) {
	resp, err := r.run(c, "RETURN 1 AS one")
	if err != nil {
		return false, fmt.Errorf("sim: bolt-stream probe RUN: %w", err)
	}
	if !isSuccess(resp) {
		return false, nil
	}
	recs, terminal, err := c.PullAll()
	if err != nil {
		return false, fmt.Errorf("sim: bolt-stream probe PULL: %w", err)
	}
	return isSuccess(terminal) && len(recs) == 1, nil
}

// armInFlight drives the two halves of the cursor-accumulation contract, each on
// its own connection and each bracketed by the WAL counters.
//
// The COMMITTED half runs exactly boltStreamInFlightCap RUN+PULL cycles inside one
// explicit transaction and commits. It is the non-vacuity witness in two senses:
// it proves the cap admits accumulation up to its bound (so the refusal below is
// about the bound and not about transactions in general), and it is the run in
// which the WAL frame counter is observed MOVING, without which "the doomed
// transaction appended no frame" would be a statement about a dead instrument.
//
// The DOOMED half runs the same cap cycles and then one more RUN. That RUN must
// draw the typed LimitExceeded FAILURE, and it must draw it PROMPTLY: the read
// deadline armed around it turns a stall into a harness error instead of a silent
// pass, which is the "backpressure or a typed error, never a block" mandate stated
// as an observation.
func (r *boltStreamRunner) armInFlight(ctx context.Context) error {
	if err := r.armInFlightCommitted(ctx); err != nil {
		return err
	}
	return r.armInFlightDoomed(ctx)
}

// armInFlightCommitted accumulates exactly the cap's worth of cursors and commits.
func (r *boltStreamRunner) armInFlightCommitted(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	arm := BoltStreamTxArm{Name: "cursors-under-cap"}
	arm.FramesBefore, arm.BytesBefore = r.walCounters()

	if resp, err := c.Begin(); err != nil {
		return fmt.Errorf("sim: bolt-stream committed BEGIN: %w", err)
	} else if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-stream committed BEGIN refused: %v", resp)
	}
	for i := 0; i < boltStreamInFlightCap; i++ {
		ok, err := r.txCycle(c, boltStreamCommitLabel, i)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		arm.Cycles++
	}
	commit, err := c.Commit()
	if err != nil {
		return fmt.Errorf("sim: bolt-stream committed COMMIT: %w", err)
	}
	arm.Committed = isSuccess(commit)
	arm.FramesAfter, arm.BytesAfter = r.walCounters()
	arm.CursorsAtRefusal = -1 // no refusal in this half
	r.ev.TxArms = append(r.ev.TxArms, arm)
	return nil
}

// armInFlightDoomed accumulates the cap's worth of cursors and then breaches it.
func (r *boltStreamRunner) armInFlightDoomed(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	arm := BoltStreamTxArm{Name: "cursors-over-cap", CursorsAtRefusal: -1}
	arm.FramesBefore, arm.BytesBefore = r.walCounters()

	if resp, err := c.Begin(); err != nil {
		return fmt.Errorf("sim: bolt-stream doomed BEGIN: %w", err)
	} else if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-stream doomed BEGIN refused: %v", resp)
	}
	for i := 0; i < boltStreamInFlightCap; i++ {
		ok, err := r.txCycle(c, boltStreamDoomedLabel, i)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		arm.Cycles++
	}

	// The decisive RUN, under a read deadline so a stall cannot read as a pass.
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltStreamReplyBound)); err != nil {
		return fmt.Errorf("sim: bolt-stream doomed arm read deadline: %w", err)
	}
	resp, err := r.run(c, fmt.Sprintf("CREATE (:%s {i: %d}) RETURN %d AS i",
		boltStreamDoomedLabel, boltStreamInFlightCap, boltStreamInFlightCap))
	if dlErr := c.Conn().SetReadDeadline(time.Time{}); dlErr != nil {
		return fmt.Errorf("sim: bolt-stream doomed arm disarm deadline: %w", dlErr)
	}
	if err != nil {
		return fmt.Errorf("sim: bolt-stream over-cap RUN drew no reply within %s (a saturated "+
			"connection must be answered with a typed error, never by blocking): %w", boltStreamReplyBound, err)
	}
	arm.RefusalObserved = failureCode(resp) != ""
	arm.RefusalCode = failureCode(resp)
	arm.RefusalMessage = failureMessage(resp)
	arm.CursorsAtRefusal = boltStreamParseOpen(arm.RefusalMessage)

	arm.FramesAfter, arm.BytesAfter = r.walCounters()
	r.ev.TxArms = append(r.ev.TxArms, arm)
	return nil
}

// txCycle runs one RUN+PULL cycle inside the open transaction, writing one node
// under label. It reports false when the RUN was refused, so the caller stops
// counting accepted cycles rather than mistaking a refusal for a cycle.
func (r *boltStreamRunner) txCycle(c *WireClient, label string, i int) (bool, error) {
	resp, err := r.run(c, fmt.Sprintf("CREATE (:%s {i: %d}) RETURN %d AS i", label, i, i))
	if err != nil {
		return false, fmt.Errorf("sim: bolt-stream tx cycle %d RUN: %w", i, err)
	}
	if !isSuccess(resp) {
		return false, nil
	}
	if _, terminal, err := c.PullAll(); err != nil {
		return false, fmt.Errorf("sim: bolt-stream tx cycle %d PULL: %w", i, err)
	} else if !isSuccess(terminal) {
		return false, nil
	}
	return true, nil
}

// walCounters reads the live WAL frame/byte counters.
func (r *boltStreamRunner) walCounters() (frames, bytes uint64) {
	s := r.st.WAL().Stats()
	return s.Frames, s.Bytes
}

// censusLive counts each arm's label in the live engine, over the genuine wire.
func (r *boltStreamRunner) censusLive(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	counts := map[string]*int{
		boltStreamDiscardLabel: &r.ev.EffectLive,
		boltStreamCommitLabel:  &r.ev.CommittedLive,
		boltStreamDoomedLabel:  &r.ev.DoomedLive,
	}
	for label, dst := range counts {
		n, err := boltStreamCountLabel(c, label)
		if err != nil {
			return err
		}
		*dst = n
	}
	return nil
}

// censusRecovered counts each arm's label on a store reopened through real
// recovery, reading the engine directly.
func (r *boltStreamRunner) censusRecovered(ctx context.Context, st *SimStore) error {
	counts := map[string]*int{
		boltStreamDiscardLabel: &r.ev.EffectRecovered,
		boltStreamCommitLabel:  &r.ev.CommittedRecovered,
		boltStreamDoomedLabel:  &r.ev.DoomedRecovered,
	}
	for label, dst := range counts {
		n, err := scalarCountViaEngine(ctx, st.Engine(), fmt.Sprintf("MATCH (n:%s) RETURN count(n)", label))
		if err != nil {
			return fmt.Errorf("sim: bolt-stream recovered count %s: %w", label, err)
		}
		*dst = int(n)
	}
	return nil
}

// boltStreamCountLabel returns the number of nodes carrying label, read over the
// wire.
func boltStreamCountLabel(c *WireClient, label string) (int, error) {
	recs, err := wireQuery(c, fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", label), nil)
	if err != nil {
		return 0, fmt.Errorf("sim: bolt-stream count %s: %w", label, err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("sim: bolt-stream count %s: got %d record(s), want 1 with 1 field", label, len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("sim: bolt-stream count %s: got %T, want int64", label, recs[0].Data[0])
	}
	return int(n), nil
}

// -----------------------------------------------------------------------------
// Observation helpers
// -----------------------------------------------------------------------------

// boltStreamTerminalKind classifies a terminal reply. It composes the package's
// existing classifiers ([isSuccess], [isIgnored], [failureCode]) rather than
// re-deciding what a reply is.
func boltStreamTerminalKind(resp any) string {
	switch {
	case resp == nil:
		return ""
	case isSuccess(resp):
		return "SUCCESS"
	case isIgnored(resp):
		return "IGNORED"
	case failureCode(resp) != "":
		return "FAILURE"
	default:
		return fmt.Sprintf("%T", resp)
	}
}

// boltStreamMakePage records one stream-consuming exchange.
func boltStreamMakePage(requested int64, discard bool, recs []*proto.Record, terminal any) BoltStreamPage {
	p := BoltStreamPage{
		Requested: requested,
		Delivered: len(recs),
		Discard:   discard,
		Terminal:  boltStreamTerminalKind(terminal),
		Code:      failureCode(terminal),
	}
	if s, ok := terminal.(*proto.Success); ok {
		p.HasMore, _ = s.Metadata["has_more"].(bool)
		_, p.Bookmarked = s.Metadata["bookmark"]
	}
	return p
}

// boltStreamDecodeRows copies each RECORD's decoded fields out of the message.
//
// The outer slice is CLONED rather than retained: the row a RECORD carries is the
// harness's only copy of the wire value, and keeping a reference to a decoder-owned
// slice would make the reference record set depend on when the decoder next reused
// its buffer.
func boltStreamDecodeRows(recs []*proto.Record) [][]packstream.Value {
	if len(recs) == 0 {
		return nil
	}
	out := make([][]packstream.Value, 0, len(recs))
	for _, rec := range recs {
		row := make([]packstream.Value, len(rec.Data))
		copy(row, rec.Data)
		out = append(out, row)
	}
	return out
}

// boltStreamStats flattens the "stats" map of a terminal SUCCESS into int64
// counters, recording a boolean counter (Neo4j's "contains-updates") as 1. A
// terminal that carries no stats yields nil, which is itself the observation the
// non-vacuity gate reads.
func boltStreamStats(terminal any) map[string]int64 {
	s, ok := terminal.(*proto.Success)
	if !ok {
		return nil
	}
	raw, ok := s.Metadata["stats"].(map[string]packstream.Value)
	if !ok {
		return nil
	}
	out := make(map[string]int64, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case int64:
			out[k] = t
		case bool:
			if t {
				out[k] = 1
			}
		}
	}
	return out
}

// boltStreamParseOpen extracts the "open=N" figure from the cap's diagnostic,
// returning -1 when the message does not carry one.
//
// It is a cross-check, not decoration: the figure is the SERVER's own count of the
// cursors it has accumulated (bolt/server/session.go:1086 passes
// inFlightCount()'s return into the message), so requiring it to agree with the
// cycles the harness counted compares two independent accountings of the same
// quantity.
func boltStreamParseOpen(msg string) int {
	const key = "open="
	i := strings.Index(msg, key)
	if i < 0 {
		return -1
	}
	rest := msg[i+len(key):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return -1
	}
	return n
}

// -----------------------------------------------------------------------------
// The concurrent stall arm
// -----------------------------------------------------------------------------

// boltStreamStallSurfaceRows / boltStreamStallSurfacePage size the fresh paged
// drain the stall arm runs after tearing the stalled connection down mid-stream.
// The page size is fixed rather than drawn because this arm is not
// bit-reproducible anyway (the stall races the server's writer), so a drawn page
// size would buy variability the arm cannot replay.
const (
	boltStreamStallSurfaceRows = 40
	boltStreamStallSurfacePage = 7
)

// RunBoltStreamStall drives the slow-consumer fault arm: a consumer opens a large
// stream, drains a tiny prefix, then stalls and finally tears the connection down
// mid-stream — and the streaming surface must be intact for everyone else
// afterwards.
//
// It reuses the harness's existing [SlowConsumer] actor rather than a second
// implementation of stalling, so the backpressure this arm exercises is the same
// one bounded by [simConnBufferSize] that the actor was built for.
//
// It is NOT bit-reproducible: how many records reach the bounded buffer before the
// consumer stops depends on the scheduler. The seed drives the stall DURATION; the
// oracle is what every interleaving must share — the writer parked, the buffer
// stayed bounded, the teardown leaked nothing, and a fresh connection's paged
// drain still matches plain range arithmetic.
func RunBoltStreamStall(ctx context.Context, seed uint64) (*BoltStreamEvidence, error) {
	rng := NewSeed(seed)
	// The server log is discarded: tearing a connection down mid-stream makes the
	// server's parked writer fail its next chunk write, which it reports at WARN by
	// design. This arm provokes that on purpose, so the noise is expected output
	// rather than a signal (the same reasoning [NewSimServerAuth] documents for a
	// scenario that provokes rejected credentials).
	srv, err := newSimServerWithLogger(SimEngineForServer(), clock.Real(), quietSimLogger())
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream stall server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	ev := &BoltStreamEvidence{Seed: seed, StallArm: true}
	stallFor := time.Duration(5+rng.IntN(20)) * time.Millisecond

	sc := NewSlowConsumer(clock.Real())
	res, err := sc.Stall(ctx, srv, stallFor, func(c *WireClient) {
		ev.StallBufferedPeak = boltStreamPollParked(ctx, c)
	})
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream stall: %w", err)
	}
	ev.StallRecordsPulled = res.RecordsPulled
	ev.StallParked = res.ServerParked
	ev.StallClosedCleanly = res.ClosedCleanly

	// The stalled connection is gone. Page a fresh stream on a NEW connection: the
	// server must still honour n, still report has_more on the non-final pages, and
	// still deliver 1..N in order.
	c, err := srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream stall surface dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("sim: bolt-stream stall surface connect: %w", err)
	}
	resp, err := c.Run(fmt.Sprintf("UNWIND range(1, %d) AS x RETURN x", boltStreamStallSurfaceRows), nil)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-stream stall surface RUN: %w", err)
	}
	if s, ok := resp.(*proto.Success); ok {
		if q, ok := s.Metadata["qid"].(int64); ok {
			ev.RunQIDs = append(ev.RunQIDs, q)
		}
	}
	for {
		recs, terminal, err := c.Pull(boltStreamStallSurfacePage)
		if err != nil {
			return nil, fmt.Errorf("sim: bolt-stream stall surface PULL: %w", err)
		}
		page := boltStreamMakePage(boltStreamStallSurfacePage, false, recs, terminal)
		ev.StallSurfacePages = append(ev.StallSurfacePages, page)
		for _, rec := range recs {
			if len(rec.Data) == 1 {
				if n, ok := rec.Data[0].(int64); ok {
					ev.StallSurfaceValues = append(ev.StallSurfaceValues, n)
					continue
				}
			}
			// A row of the wrong shape is recorded as a sentinel so the checker's
			// arithmetic comparison fails on it rather than silently skipping it.
			ev.StallSurfaceValues = append(ev.StallSurfaceValues, -1)
		}
		if page.Terminal != "SUCCESS" || !page.HasMore {
			break
		}
	}
	return ev, nil
}

// -----------------------------------------------------------------------------
// The contract
// -----------------------------------------------------------------------------

// boltStreamOp renders the Op field of a violation for one clause, so a report
// names the clause that fired.
func boltStreamOp(clause string) string { return "<bolt-stream:" + clause + ">" }

// boltStreamViolation builds one violation for a clause.
func boltStreamViolation(kind ViolationKind, clause, msg string) Violation {
	return Violation{Kind: kind, Op: boltStreamOp(clause), Message: msg}
}

// boltStreamMaxDivergences bounds how many row divergences a single comparison
// reports, so one shifted stream does not produce a hundred-line report. The
// count of remaining divergences is reported alongside.
const boltStreamMaxDivergences = 3

// boltStreamCompareRows compares two decoded record sets element by element, in
// order, and reports divergences.
//
// It defers the per-row value comparison to [compareWireRow], which is the
// package's existing statement of the discipline the AC demands: both the VALUE
// and its concrete Go type are compared, because the dynamic type IS the wire
// encoding. Comparing String() renderings would let an Integer arriving as a
// String, or a Float where an Integer belongs, pass unnoticed.
func boltStreamCompareRows(clause, label string, got, want [][]packstream.Value) []Violation {
	if len(got) != len(want) {
		return []Violation{boltStreamViolation(ViolationOracleDeviation, clause,
			fmt.Sprintf("%s delivered %d row(s), want %d", label, len(got), len(want)))}
	}
	var v []Violation
	diverged := 0
	for i := range want {
		var fails []string
		switch {
		case len(got[i]) != len(want[i]):
			fails = []string{fmt.Sprintf("row %d: got %d column(s), want %d", i, len(got[i]), len(want[i]))}
		case len(want[i]) != len(boltStreamColumns):
			fails = []string{fmt.Sprintf("row %d: %d column(s) but the reference query declares %d",
				i, len(want[i]), len(boltStreamColumns))}
		default:
			fails = compareWireRow(fmt.Sprintf("row %d", i), got[i], want[i], boltStreamColumns)
		}
		if len(fails) == 0 {
			continue
		}
		diverged++
		if diverged <= boltStreamMaxDivergences {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, clause,
				fmt.Sprintf("%s: %s", label, strings.Join(fails, "; "))))
		}
	}
	if diverged > boltStreamMaxDivergences {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, clause,
			fmt.Sprintf("%s: %d further row(s) diverged (only the first %d are listed)",
				label, diverged-boltStreamMaxDivergences, boltStreamMaxDivergences)))
	}
	return v
}

// checkBoltStreamSemantics adjudicates the evidence against the streaming
// contract. The stall arm and the deterministic battery are disjoint runs, so the
// arm flag selects which clauses apply rather than gating each one.
func checkBoltStreamSemantics(e *BoltStreamEvidence) []Violation {
	if e.StallArm {
		return checkBoltStreamStall(e)
	}
	return slices.Concat(
		checkBoltStreamPaging(e),
		checkBoltStreamWindow(e),
		checkBoltStreamEffect(e),
		checkBoltStreamRefusals(e),
		checkBoltStreamQIDIdentity(e),
		checkBoltStreamInFlight(e),
	)
}

// checkBoltStreamPaging is the load-bearing oracle: the concatenation of the paged
// drain must equal the reference drain, and has_more must be true on exactly the
// non-final pages.
func checkBoltStreamPaging(e *BoltStreamEvidence) []Violation {
	var v []Violation
	if e.ReferenceTerminal != "SUCCESS" || e.ReferenceHasMore {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "reference-drain",
			fmt.Sprintf("the reference PULL -1 ended %s with has_more=%v; a pull-all must exhaust the stream",
				e.ReferenceTerminal, e.ReferenceHasMore)))
	}
	if len(e.Pages) != len(e.PageSizes) {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-page-count",
			fmt.Sprintf("the drain took %d page(s) for a %d-page plan: no strict prefix of the drawn "+
				"sizes can exhaust %d rows, so the counts must match",
				len(e.Pages), len(e.PageSizes), boltStreamRows)))
	}
	v = append(v, boltStreamCompareRows("paging-equivalence", "the paged drain", e.Paged, e.Reference)...)
	v = append(v, checkBoltStreamPageShape(e)...)
	if !e.PagingReusable {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-reusable",
			"no statement was acknowledged on the paging connection after the drain: a fully drained "+
				"stream must leave the session in READY"))
	}
	return v
}

// checkBoltStreamPageShape adjudicates each page: its terminal reply, the number
// of rows it delivered against the n it asked for, its has_more, and whether it
// carried the bookmark.
func checkBoltStreamPageShape(e *BoltStreamEvidence) []Violation {
	var v []Violation
	remaining := len(e.Reference)
	for i := range e.Pages {
		p := &e.Pages[i]
		final := i == len(e.Pages)-1
		if p.Terminal != "SUCCESS" {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-terminal",
				fmt.Sprintf("page %d ended %s (code %q), want SUCCESS", i, p.Terminal, p.Code)))
			continue
		}
		want := int(min(p.Requested, int64(remaining)))
		if p.Delivered != want {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-page-size",
				fmt.Sprintf("page %d asked for n=%d with %d row(s) remaining and got %d, want %d",
					i, p.Requested, remaining, p.Delivered, want)))
		}
		remaining -= p.Delivered
		if p.HasMore == final {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-has_more",
				fmt.Sprintf("page %d of %d reported has_more=%v: has_more must be true on exactly "+
					"the non-final pages", i, len(e.Pages), p.HasMore)))
		}
		if p.Bookmarked != final {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "paging-bookmark",
				fmt.Sprintf("page %d of %d %s a bookmark: the bookmark rides on the TERMINAL reply only",
					i, len(e.Pages), boltStreamCarried(p.Bookmarked))))
		}
	}
	return v
}

// boltStreamCarried renders a bookmark presence for a violation message.
func boltStreamCarried(b bool) string {
	if b {
		return "carried"
	}
	return "did not carry"
}

// checkBoltStreamWindow adjudicates the partial-DISCARD arm.
//
// The oracle is exact about WHICH rows were skipped: the prefix the client paged,
// concatenated with the suffix it pulled afterwards, must equal the reference with
// exactly the discarded window cut out of it. A DISCARD that dropped one row too
// many, or one too few, shifts the suffix and fails here — which "the session
// still works afterwards" could not see.
func checkBoltStreamWindow(e *BoltStreamEvidence) []Violation {
	var v []Violation
	k := len(e.WindowPrefix)
	d := int(e.WindowDiscardN)
	if k > len(e.Reference) || k+d > len(e.Reference) {
		return append(v, boltStreamViolation(ViolationOracleDeviation, "window-bounds",
			fmt.Sprintf("the window (prefix %d + discard %d) exceeds the %d-row reference",
				k, d, len(e.Reference))))
	}
	// The expectation is DERIVED here, from the reference drain and the two indices.
	// The evidence records neither, so a runner that sliced wrongly cannot make its
	// own mistake agree with itself.
	want := make([][]packstream.Value, 0, len(e.Reference)-d)
	want = append(want, e.Reference[:k]...)
	want = append(want, e.Reference[k+d:]...)
	got := make([][]packstream.Value, 0, len(want))
	got = append(got, e.WindowPrefix...)
	got = append(got, e.WindowSuffix...)
	v = append(v, boltStreamCompareRows("window-equivalence",
		fmt.Sprintf("prefix(%d rows)++suffix after DISCARD n=%d", k, d), got, want)...)

	dp := &e.WindowDiscard
	if dp.Delivered != 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-no-delivery",
			fmt.Sprintf("the mid-stream DISCARD delivered %d RECORD(s): DISCARD drops rows, it does not "+
				"deliver them", dp.Delivered)))
	}
	if dp.Terminal != "SUCCESS" {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-terminal",
			fmt.Sprintf("the mid-stream DISCARD ended %s (code %q), want SUCCESS", dp.Terminal, dp.Code)))
	}
	wantMore := k+d < len(e.Reference)
	if dp.HasMore != wantMore {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-has_more",
			fmt.Sprintf("DISCARD n=%d after %d of %d rows reported has_more=%v, want %v",
				d, k, len(e.Reference), dp.HasMore, wantMore)))
	}
	if dp.HasMore && dp.Bookmarked {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-bookmark",
			"a DISCARD that left rows behind carried a bookmark: the bookmark rides on the terminal "+
				"reply only, and this one is not terminal"))
	}
	v = append(v, checkBoltStreamWindowSuffix(e)...)
	if !e.WindowReusable {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-reusable",
			"no statement was acknowledged after the mid-stream DISCARD: a post-DISCARD RUN must succeed "+
				"on the same connection"))
	}
	return v
}

// checkBoltStreamWindowSuffix adjudicates the closing PULL -1 of the window arm.
func checkBoltStreamWindowSuffix(e *BoltStreamEvidence) []Violation {
	var v []Violation
	sp := &e.WindowSuffixPage
	if sp.Terminal != "SUCCESS" {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-suffix-terminal",
			fmt.Sprintf("the closing PULL -1 ended %s (code %q), want SUCCESS", sp.Terminal, sp.Code)))
		return v
	}
	if sp.HasMore {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-suffix-has_more",
			"the closing PULL -1 reported has_more=true: a pull-all exhausts the stream"))
	}
	if !sp.Bookmarked {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "window-suffix-bookmark",
			"the closing PULL -1 carried no bookmark, so the exchange that ended the stream was not the "+
				"one the driver would build its summary from"))
	}
	return v
}

// checkBoltStreamEffect adjudicates the discard-effect arm: DISCARD abandons
// delivery, not the statement.
func checkBoltStreamEffect(e *BoltStreamEvidence) []Violation {
	var v []Violation
	p := &e.EffectDiscard
	if p.Delivered != 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "effect-no-delivery",
			fmt.Sprintf("the DISCARD of a write's delivery handed back %d RECORD(s)", p.Delivered)))
	}
	if p.Terminal != "SUCCESS" || p.HasMore || !p.Bookmarked {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "effect-terminal",
			fmt.Sprintf("the DISCARD-all of a write ended %s (code %q) has_more=%v bookmark=%v; want a "+
				"terminal SUCCESS with has_more=false carrying the bookmark",
				p.Terminal, p.Code, p.HasMore, p.Bookmarked)))
	}
	if got := e.EffectStats["nodes-created"]; got != 1 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "effect-stats",
			fmt.Sprintf("the DISCARD's terminal SUCCESS reported nodes-created=%d, want 1: the write "+
				"counters are the only place the effect can reach a client that took no rows", got)))
	}
	if e.EffectLive != 1 {
		v = append(v, boltStreamViolation(ViolationACIDConsistency, "effect-live",
			fmt.Sprintf("%d node(s) labelled %s exist in the live engine, want 1: DISCARD abandons "+
				"delivery, not the statement", e.EffectLive, boltStreamDiscardLabel)))
	}
	if e.EffectRecovered != e.EffectLive {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "effect-recovered",
			fmt.Sprintf("WAL replay recovered %d node(s) labelled %s but %d were live: a write whose "+
				"delivery was discarded must still be durable",
				e.EffectRecovered, boltStreamDiscardLabel, e.EffectLive)))
	}
	if !e.EffectReusable {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "effect-reusable",
			"no statement was acknowledged after DISCARDing a write's delivery"))
	}
	return v
}

// boltStreamExpectedRefusals is the refusal roster the run must produce. It is
// pinned so a dropped probe is a violation rather than a silent reduction in
// coverage.
var boltStreamExpectedRefusals = []string{
	"pull-positive-qid",
	"discard-positive-qid",
	"second-run-in-streaming",
	"second-run-in-tx-streaming",
}

// checkBoltStreamRefusals adjudicates every refusal probe: the verdict, the exact
// code, the exact message, the gate the refusal came from, and what the connection
// did afterwards.
func checkBoltStreamRefusals(e *BoltStreamEvidence) []Violation {
	// One violation per probe is the shape a broken gate produces (a wrong code, a
	// wrong origin state), so the roster length is the right capacity hint. A clean
	// run returns this slice empty rather than nil; every caller tests len().
	v := make([]Violation, 0, len(e.Refusals))
	for i := range e.Refusals {
		v = append(v, checkBoltStreamRefusal(&e.Refusals[i])...)
	}
	return v
}

// checkBoltStreamRefusal adjudicates one refusal probe. The receiver is a pointer
// because a BoltStreamRefusal is well past the value-copy threshold and this is
// called per probe.
func checkBoltStreamRefusal(f *BoltStreamRefusal) []Violation {
	var v []Violation
	name := f.Name
	if f.Accepted {
		return append(v, boltStreamViolation(ViolationACIDConsistency, "refusal-admitted",
			fmt.Sprintf("probe %q was ADMITTED but must be refused", name)))
	}
	if f.GotCode != f.WantCode {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-code",
			fmt.Sprintf("probe %q was refused with code %q, want %q", name, f.GotCode, f.WantCode)))
	}
	if f.WantMessage != "" && f.GotMessage != f.WantMessage {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-message",
			fmt.Sprintf("probe %q was told %q, want exactly %q", name, f.GotMessage, f.WantMessage)))
	}
	// Gate attribution. The needle is the whole "in state X" phrase and not the bare
	// state name, because "TX_STREAMING" contains "STREAMING": a bare containment
	// check would let a TX_STREAMING refusal satisfy the STREAMING clause, which is
	// precisely the confusion this attribution exists to prevent.
	if f.WantStateInMessage != "" {
		needle := "in state " + f.WantStateInMessage
		if !strings.Contains(f.GotMessage, needle) {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-origin-state",
				fmt.Sprintf("probe %q was refused from the wrong state: message %q does not contain %q, "+
					"so the refusal came from a different gate than the state machine's",
					name, f.GotMessage, needle)))
		}
	}
	if f.Delivered != 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-delivery",
			fmt.Sprintf("probe %q was refused but still delivered %d RECORD(s)", name, f.Delivered)))
	}
	if f.NextTerminal != "IGNORED" {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-poisons-session",
			fmt.Sprintf("after probe %q the next request drew %s, want IGNORED: the refusal routes "+
				"through enterFailed, and a FAILED session soft-ignores request-phase messages until RESET",
				name, f.NextTerminal)))
	}
	if !f.RecoveredAfterReset {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "refusal-reset-recovers",
			fmt.Sprintf("no statement was acknowledged after RESET on probe %q's connection: the refusal "+
				"must be scoped to the session's FAILED phase, not terminal for the connection", name)))
	}
	return v
}

// checkBoltStreamQIDIdentity adjudicates the two clauses that make the QID
// refutation an assertion rather than an argument: the server never mints a
// positive qid, and the current-stream qid is served.
func checkBoltStreamQIDIdentity(e *BoltStreamEvidence) []Violation {
	var v []Violation
	for i, q := range e.RunQIDs {
		if q != -1 {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "run-qid-current",
				fmt.Sprintf("RUN reply %d reported qid=%d, want -1: this server keeps exactly one open "+
					"stream per session and never names it, so a positive qid could only come from a "+
					"multiplexing implementation that does not exist here", i, q)))
			break
		}
	}
	if e.QIDControlRows != len(e.Reference) {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "qid-control",
			fmt.Sprintf("the qid=-1 control drain delivered %d row(s), want %d: with the control refused "+
				"too, \"a positive qid is refused\" would say nothing about the qid",
				e.QIDControlRows, len(e.Reference))))
	}
	return v
}

// boltStreamTxArm looks an in-flight arm up by name, returning nil when absent.
// Arms are addressed by name rather than by index so a reordering cannot silently
// aim a clause at the wrong transaction.
func boltStreamTxArm(e *BoltStreamEvidence, name string) *BoltStreamTxArm {
	for i := range e.TxArms {
		if e.TxArms[i].Name == name {
			return &e.TxArms[i]
		}
	}
	return nil
}

// checkBoltStreamInFlight adjudicates cursor accumulation and the cap.
func checkBoltStreamInFlight(e *BoltStreamEvidence) []Violation {
	var v []Violation
	under := boltStreamTxArm(e, "cursors-under-cap")
	over := boltStreamTxArm(e, "cursors-over-cap")
	if under == nil || over == nil {
		return append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-roster",
			fmt.Sprintf("the run recorded %d in-flight arm(s); both cursors-under-cap and "+
				"cursors-over-cap must have run", len(e.TxArms))))
	}
	v = append(v, checkBoltStreamInFlightUnder(e, under)...)
	v = append(v, checkBoltStreamInFlightOver(e, over)...)
	return v
}

// checkBoltStreamInFlightUnder adjudicates the transaction that stayed under the
// cap: cursors accumulate across sequential RUNs, the COMMIT is acknowledged, and
// every write is durable. It is also where the WAL counter is shown MOVING.
func checkBoltStreamInFlightUnder(e *BoltStreamEvidence, a *BoltStreamTxArm) []Violation {
	var v []Violation
	if a.Cycles != boltStreamInFlightCap {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-accumulates",
			fmt.Sprintf("the under-cap transaction completed %d RUN+PULL cycle(s), want %d: a cursor "+
				"per RUN must accumulate up to the cap without being refused",
				a.Cycles, boltStreamInFlightCap)))
	}
	if !a.Committed {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-commit",
			"the under-cap transaction's COMMIT was not acknowledged"))
	}
	if a.framesAppended() == 0 {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "inflight-admit-frames",
			fmt.Sprintf("the committed transaction appended NO WAL frame (frames stuck at %d): the "+
				"commit was acknowledged without reaching the log, and the counter the over-cap arm "+
				"reads is not a live instrument", a.FramesBefore)))
	}
	if e.CommittedLive != boltStreamInFlightCap {
		v = append(v, boltStreamViolation(ViolationACIDAtomicity, "inflight-committed-live",
			fmt.Sprintf("%d node(s) labelled %s exist in the live engine, want %d (one per accumulated "+
				"cursor): a committed transaction is all-or-nothing",
				e.CommittedLive, boltStreamCommitLabel, boltStreamInFlightCap)))
	}
	if e.CommittedRecovered != e.CommittedLive {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "inflight-committed-recovered",
			fmt.Sprintf("WAL replay recovered %d node(s) labelled %s but %d were live",
				e.CommittedRecovered, boltStreamCommitLabel, e.CommittedLive)))
	}
	return v
}

// checkBoltStreamInFlightOver adjudicates the transaction that breached the cap:
// the refusal is typed and prompt, it names the cap, the server's own cursor count
// agrees with the harness's, and the doomed transaction left nothing behind.
func checkBoltStreamInFlightOver(e *BoltStreamEvidence, a *BoltStreamTxArm) []Violation {
	var v []Violation
	if a.Cycles != boltStreamInFlightCap {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-over-cycles",
			fmt.Sprintf("the over-cap transaction completed %d cycle(s) before its refusal, want %d: "+
				"the cap must be reached by ACCUMULATION, not by refusing the first RUN",
				a.Cycles, boltStreamInFlightCap)))
	}
	if !a.RefusalObserved {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-typed",
			"the over-cap RUN drew no typed FAILURE: saturation must be answered with a typed error"))
	}
	if a.RefusalCode != streamCodeLimitExceeded {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-cap-code",
			fmt.Sprintf("the over-cap RUN was refused with code %q, want %q",
				a.RefusalCode, streamCodeLimitExceeded)))
	}
	if want := fmt.Sprintf("cap=%d", boltStreamInFlightCap); !strings.Contains(a.RefusalMessage, want) {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-cap-named",
			fmt.Sprintf("the refusal message %q does not name %q: an operator cannot raise a bound the "+
				"diagnostic does not state", a.RefusalMessage, want)))
	}
	if a.CursorsAtRefusal != a.Cycles {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "inflight-open-count",
			fmt.Sprintf("the refusal reported open=%d cursors but the harness counted %d accepted "+
				"cycle(s): two independent accountings of the same quantity disagree",
				a.CursorsAtRefusal, a.Cycles)))
	}
	if n := a.framesAppended(); n != 0 {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "inflight-doomed-frames",
			fmt.Sprintf("the doomed transaction appended %d WAL frame(s) (%d -> %d): a transaction "+
				"reclaimed by the cap breach must reach the log with nothing",
				n, a.FramesBefore, a.FramesAfter)))
	}
	if n := a.bytesAppended(); n != 0 {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "inflight-doomed-bytes",
			fmt.Sprintf("the doomed transaction appended %d WAL byte(s)", n)))
	}
	if e.DoomedLive != 0 {
		v = append(v, boltStreamViolation(ViolationACIDAtomicity, "inflight-doomed-live",
			fmt.Sprintf("%d node(s) labelled %s exist in the live engine: the cap breach moves the "+
				"session to FAILED, which must roll the transaction back",
				e.DoomedLive, boltStreamDoomedLabel)))
	}
	if e.DoomedRecovered != 0 {
		v = append(v, boltStreamViolation(ViolationACIDDurability, "inflight-doomed-recovered",
			fmt.Sprintf("%d node(s) labelled %s survived WAL replay: a rolled-back transaction reached "+
				"the durable log", e.DoomedRecovered, boltStreamDoomedLabel)))
	}
	return v
}

// checkBoltStreamStall adjudicates the concurrent slow-consumer arm.
//
// It asserts only what every interleaving must share. How many records reach the
// bounded buffer before the consumer stops is a scheduling outcome, so the clause
// is "a non-empty proper prefix", never an exact count.
func checkBoltStreamStall(e *BoltStreamEvidence) []Violation {
	var v []Violation
	if !e.StallParked {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-parked",
			"the stream terminated before the consumer could stall: the result was not large enough to "+
				"park the server's writer, so nothing about backpressure was exercised"))
	}
	// A guard on the HARNESS, not on the server: halfPipe.write chunks to the space
	// remaining, so a reading above the bound would mean the pipe itself broke its
	// own invariant. See [BoltStreamEvidence.StallBufferedPeak].
	if e.StallBufferedPeak > simConnBufferSize {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-bounded",
			fmt.Sprintf("%d byte(s) were queued toward the stalled consumer, above the %d-byte pipe "+
				"bound: the connection buffer broke its own invariant, so no reading taken through it "+
				"can be trusted", e.StallBufferedPeak, simConnBufferSize)))
	}
	if e.StallRecordsPulled <= 0 || e.StallRecordsPulled >= slowConsumerResultRows {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-prefix",
			fmt.Sprintf("the consumer drained %d of %d row(s): a stall must leave a non-empty PROPER "+
				"prefix, or it either never opened the stream or never stalled",
				e.StallRecordsPulled, slowConsumerResultRows)))
	}
	if !e.StallClosedCleanly {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-clean-close",
			"the stalled connection reported an unexpected transport fault on teardown"))
	}
	v = append(v, checkBoltStreamStallSurface(e)...)
	return v
}

// checkBoltStreamStallSurface adjudicates the fresh paged drain that follows the
// mid-stream teardown, against plain range arithmetic.
func checkBoltStreamStallSurface(e *BoltStreamEvidence) []Violation {
	var v []Violation
	if len(e.StallSurfaceValues) != boltStreamStallSurfaceRows {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-surface-rows",
			fmt.Sprintf("the fresh paged drain delivered %d row(s), want %d: a connection torn down "+
				"mid-stream must not disturb the streaming surface for anyone else",
				len(e.StallSurfaceValues), boltStreamStallSurfaceRows)))
		return v
	}
	for i, got := range e.StallSurfaceValues {
		if want := int64(i + 1); got != want {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-surface-order",
				fmt.Sprintf("the fresh paged drain delivered %d at position %d, want %d", got, i, want)))
			break
		}
	}
	for i := range e.StallSurfacePages {
		p := &e.StallSurfacePages[i]
		final := i == len(e.StallSurfacePages)-1
		if p.Terminal != "SUCCESS" {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-surface-terminal",
				fmt.Sprintf("fresh page %d ended %s (code %q), want SUCCESS", i, p.Terminal, p.Code)))
			continue
		}
		if p.HasMore == final {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "stall-surface-has_more",
				fmt.Sprintf("fresh page %d of %d reported has_more=%v", i, len(e.StallSurfacePages), p.HasMore)))
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// The non-vacuity gate
// -----------------------------------------------------------------------------

// boltStreamMinRunQIDs is the floor on how many RUN replies the qid census must
// have inspected. The deterministic battery issues well over twenty; the floor
// exists so a run that inspected none cannot report "every qid was -1" as a
// finding about the server.
const boltStreamMinRunQIDs = 12

// checkBoltStreamSemanticsNonVacuity proves the run actually exercised the
// surface, so a green verdict cannot come from a battery that quietly did nothing.
//
// It returns []Violation rather than the []string that rmp #2554 demoted the MERGE
// and FOREACH coverage gates to, and the distinction is the reason: those gates
// reported a shortfall when a SEEDED WORKLOAD happened not to drive a branch, which
// is an uninformative run and not a defect. Every precondition here is
// CONSTRUCTED — each arm runs by rule on its own connection, and the draws are
// bounded so that a window is always strictly interior and a paged drain always
// takes at least seven pages — so a shortfall means the harness itself stopped
// exercising the surface, which must fail loudly. This matches the constructed
// battery of rmp #2483 in the same sprint.
func checkBoltStreamSemanticsNonVacuity(e *BoltStreamEvidence) []Violation {
	if e.StallArm {
		return checkBoltStreamStallNonVacuity(e)
	}
	var v []Violation
	v = append(v, checkBoltStreamPagingNonVacuity(e)...)
	v = append(v, checkBoltStreamRefusalNonVacuity(e)...)
	if got := e.EffectStats["nodes-created"]; got == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-effect",
			"the discarded statement reported no write counter, so it had no EFFECT to confirm and "+
				"\"DISCARD abandons delivery, not the statement\" is a claim about a read"))
	}
	if under := boltStreamTxArm(e, "cursors-under-cap"); under == nil || under.framesAppended() == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-wal-instrument",
			"no transaction was observed appending a WAL frame: the frame counter is not a live "+
				"instrument, so \"the doomed transaction appended none\" proves nothing"))
	}
	return v
}

// checkBoltStreamPagingNonVacuity proves the paged drain actually paged.
func checkBoltStreamPagingNonVacuity(e *BoltStreamEvidence) []Violation {
	var v []Violation
	if len(e.Reference) != boltStreamRows {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-reference",
			fmt.Sprintf("the reference drain produced %d row(s), want %d: with an empty reference every "+
				"equivalence clause is a statement about two empty slices",
				len(e.Reference), boltStreamRows)))
	}
	if len(e.Pages) < 3 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-pages",
			fmt.Sprintf("the drain took %d page(s): a single page is a pull-all by another name and "+
				"says nothing about paging", len(e.Pages))))
	}
	var sawMore, sawDone, sawBounded bool
	for i := range e.Pages {
		p := &e.Pages[i]
		if p.HasMore {
			sawMore = true
		} else {
			sawDone = true
		}
		// A page that delivered exactly the n it asked for, with rows still to come,
		// is the proof n was a BOUND rather than "everything that was left".
		if p.HasMore && p.Requested > 0 && int64(p.Delivered) == p.Requested {
			sawBounded = true
		}
	}
	if !sawMore || !sawDone {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-has_more",
			fmt.Sprintf("has_more was observed true=%v false=%v across the drain; both readings are "+
				"needed or \"has_more is true on exactly the non-final pages\" is unfalsifiable",
				sawMore, sawDone)))
	}
	if !sawBounded {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-bounded-page",
			"no page delivered exactly its requested n with rows still remaining, so the n field was "+
				"never shown to BOUND a page"))
	}
	k, d := len(e.WindowPrefix), int(e.WindowDiscardN)
	if k == 0 || d <= 0 || k+d >= len(e.Reference) {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-window",
			fmt.Sprintf("the discard window (prefix %d, n=%d, reference %d) is not strictly interior: "+
				"with nothing before or nothing after it, the removal oracle degenerates into the "+
				"paging oracle", k, d, len(e.Reference))))
	}
	return v
}

// checkBoltStreamRefusalNonVacuity proves the refusal probes were probes of
// something: the roster ran, the qids were positive, the control was served, and
// each probe's connection was demonstrably working beforehand.
func checkBoltStreamRefusalNonVacuity(e *BoltStreamEvidence) []Violation {
	var v []Violation
	seen := make(map[string]bool, len(e.Refusals))
	for i := range e.Refusals {
		seen[e.Refusals[i].Name] = true
	}
	for _, want := range boltStreamExpectedRefusals {
		if !seen[want] {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-refusal-roster",
				fmt.Sprintf("probe %q did not run: the surface was not fully driven", want)))
		}
	}
	for i := range e.Refusals {
		if !e.Refusals[i].PriorAck {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-prior-ack",
				fmt.Sprintf("probe %q never acknowledged a statement before its decisive message, so "+
					"\"the message was refused\" is equally explained by a connection that never worked",
					e.Refusals[i].Name)))
		}
	}
	for _, q := range e.ProbedQIDs {
		if q <= 0 {
			v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-qid-positive",
				fmt.Sprintf("a qid probe sent qid=%d: only a NON-NEGATIVE qid names a stream that does "+
					"not exist, so a probe at -1 asserts the refusal of the current stream", q)))
		}
	}
	if len(e.ProbedQIDs) == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-qid-positive",
			"no explicit qid was ever sent"))
	}
	if e.QIDControlRows == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-qid-control",
			"the qid=-1 control delivered no row, so the qid refusals are indistinguishable from a "+
				"server that refuses every PULL"))
	}
	if len(e.RunQIDs) < boltStreamMinRunQIDs {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-run-qids",
			fmt.Sprintf("the qid census inspected %d RUN reply/replies, want at least %d",
				len(e.RunQIDs), boltStreamMinRunQIDs)))
	}
	var recovered int
	for i := range e.Refusals {
		if e.Refusals[i].RecoveredAfterReset {
			recovered++
		}
	}
	if recovered == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-reset-recovery",
			"no probe recovered after RESET, so the refusal clauses cannot distinguish a scoped refusal "+
				"from a connection that was dead from the start"))
	}
	return v
}

// checkBoltStreamStallNonVacuity proves the stall arm actually stalled a stream and
// then actually drove a fresh one.
func checkBoltStreamStallNonVacuity(e *BoltStreamEvidence) []Violation {
	var v []Violation
	if e.StallRecordsPulled == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-stall-open",
			"the slow consumer drained no record at all, so the stream was never opened and the "+
				"backpressure clauses adjudicate nothing"))
	}
	if e.StallBufferedPeak < simConnBufferSize {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-stall-parked",
			fmt.Sprintf("the queue toward the stalled consumer peaked at %d of %d byte(s): the writer "+
				"was never observed blocked against a FULL buffer, so the teardown did not interrupt a "+
				"parked writer and the no-leak clause is a statement about an idle connection",
				e.StallBufferedPeak, simConnBufferSize)))
	}
	if len(e.StallSurfacePages) < 2 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-stall-pages",
			fmt.Sprintf("the post-stall drain took %d page(s): fewer than two cannot show has_more "+
				"changing, so \"the surface is intact\" is read off a pull-all",
				len(e.StallSurfacePages))))
	}
	if len(e.RunQIDs) == 0 {
		v = append(v, boltStreamViolation(ViolationOracleDeviation, "nonvacuity-stall-run-qid",
			"no RUN reply was inspected on the stall arm"))
	}
	return v
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// String renders the evidence for a report.
//
// It carries no session id, no connection id, no address and no timing, and it
// renders no WAL BYTE total that could be non-zero: byte totals embed a created
// node's "__cx_"+hex(n) internal key, whose width depends on a process-global
// counter, so a rendering containing one is not byte-identical across two runs of
// the same seed. Frame COUNTS are seed-pure and are rendered. Every map is walked
// in sorted key order for the same reason.
//
// The receiver is a pointer because a BoltStreamEvidence is far past the size at
// which gocritic flags a value copy, and the renderer is called on every failing
// run.
func (e *BoltStreamEvidence) String() string {
	if e.StallArm {
		return e.renderStall()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-stream evidence (seed %d):", e.Seed)
	fmt.Fprintf(&b, "\n  reference: %d row(s), terminal %s has_more=%v",
		len(e.Reference), e.ReferenceTerminal, e.ReferenceHasMore)
	fmt.Fprintf(&b, "\n  paging: plan %v -> %d page(s), %d row(s) delivered, reusable=%v",
		e.PageSizes, len(e.Pages), len(e.Paged), e.PagingReusable)
	for i := range e.Pages {
		fmt.Fprintf(&b, "\n    page %2d: %s", i, e.Pages[i].render())
	}
	fmt.Fprintf(&b, "\n  window: prefix plan %v -> %d row(s); %s; suffix %d row(s) via %s; reusable=%v",
		e.WindowPageSizes, len(e.WindowPrefix), e.WindowDiscard.render(),
		len(e.WindowSuffix), e.WindowSuffixPage.render(), e.WindowReusable)
	fmt.Fprintf(&b, "\n  effect: %s stats{%s} live=%d recovered=%d reusable=%v",
		e.EffectDiscard.render(), boltStreamRenderStats(e.EffectStats),
		e.EffectLive, e.EffectRecovered, e.EffectReusable)
	fmt.Fprintf(&b, "\n  qid: probed %v; run-qid readings=%d distinct=%v; control rows=%d",
		e.ProbedQIDs, len(e.RunQIDs), boltStreamDistinct(e.RunQIDs), e.QIDControlRows)
	b.WriteString("\n  refusals:")
	for i := range e.Refusals {
		f := &e.Refusals[i]
		fmt.Fprintf(&b, "\n    %-28s accepted=%-5v code=%-40q delivered=%d next=%s prior-ack=%v reset-recovered=%v",
			f.Name, f.Accepted, f.GotCode, f.Delivered, f.NextTerminal, f.PriorAck, f.RecoveredAfterReset)
		if f.GotMessage != "" {
			fmt.Fprintf(&b, "\n      message: %q", f.GotMessage)
		}
	}
	for i := range e.TxArms {
		a := &e.TxArms[i]
		fmt.Fprintf(&b, "\n  inflight %-18s cycles=%d open=%s committed=%v frames+%d",
			a.Name, a.Cycles, boltStreamRenderOpen(a.CursorsAtRefusal), a.Committed, a.framesAppended())
		if a.RefusalCode != "" {
			fmt.Fprintf(&b, " refused=%q bytes+%d", a.RefusalCode, a.bytesAppended())
		}
	}
	fmt.Fprintf(&b, "\n  census: committed live=%d recovered=%d | doomed live=%d recovered=%d | discarded live=%d recovered=%d",
		e.CommittedLive, e.CommittedRecovered, e.DoomedLive, e.DoomedRecovered, e.EffectLive, e.EffectRecovered)
	return b.String()
}

// renderStall renders the concurrent arm. Unlike the deterministic battery this arm
// is not bit-reproducible by construction, so it does render the scheduling-
// dependent figures (records drained, bytes buffered) — they are what the arm is
// about, and the report says so.
func (e *BoltStreamEvidence) renderStall() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-stream stall evidence (seed %d, scheduling-dependent):", e.Seed)
	fmt.Fprintf(&b, "\n  stall: open=%v drained=%d/%d queue-peak=%d/%d clean=%v",
		e.StallParked, e.StallRecordsPulled, slowConsumerResultRows,
		e.StallBufferedPeak, simConnBufferSize, e.StallClosedCleanly)
	fmt.Fprintf(&b, "\n  post-stall drain: %d row(s) over %d page(s) (reference: range(1, %d))",
		len(e.StallSurfaceValues), len(e.StallSurfacePages), boltStreamStallSurfaceRows)
	for i := range e.StallSurfacePages {
		fmt.Fprintf(&b, "\n    page %2d: %s", i, e.StallSurfacePages[i].render())
	}
	return b.String()
}

// render renders one exchange for a report.
func (p *BoltStreamPage) render() string {
	kind := "PULL"
	if p.Discard {
		kind = "DISCARD"
	}
	return fmt.Sprintf("%s n=%-3d delivered=%-3d has_more=%-5v bookmark=%-5v terminal=%s%s",
		kind, p.Requested, p.Delivered, p.HasMore, p.Bookmarked, p.Terminal, boltStreamRenderCode(p.Code))
}

// boltStreamRenderCode appends a failure code when there is one.
func boltStreamRenderCode(code string) string {
	if code == "" {
		return ""
	}
	return " code=" + code
}

// boltStreamRenderOpen renders the cap diagnostic's cursor count, spelling the
// "no refusal happened here" sentinel rather than printing a bare -1.
func boltStreamRenderOpen(n int) string {
	if n < 0 {
		return "n/a"
	}
	return strconv.Itoa(n)
}

// boltStreamRenderStats renders a counter map in sorted key order, so the same seed
// renders the same bytes.
func boltStreamRenderStats(stats map[string]int64) string {
	if len(stats) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(stats))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, stats[k]))
	}
	return strings.Join(parts, " ")
}

// boltStreamDistinct returns the distinct values of xs in ascending order, so a
// census of many identical readings renders as one short list.
func boltStreamDistinct(xs []int64) []int64 {
	if len(xs) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(xs))
	for _, x := range xs {
		seen[x] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}

// -----------------------------------------------------------------------------
// The scenarios
// -----------------------------------------------------------------------------

// The catalogue defaults for the two streaming scenarios.
const (
	boltStreamDefaultSeed      = 0x2484_5701
	boltStreamStallDefaultSeed = 0x2484_57A1
)

// boltStreamSemanticsScenario drives the deterministic streaming battery: the
// paging-equivalence oracle against an independent reference drain, the exact
// window a partial DISCARD removes, DISCARD's effect on a write, the qid and
// second-RUN refusals with their gates attributed, and cursor accumulation up to
// and past the per-connection in-flight cap.
func boltStreamSemanticsScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltStreaming,
		Description: "Bolt streaming semantics: a seed-drawn PULL n paging sequence must concatenate to the " +
			"PULL -1 record set element by element, has_more must be true on exactly the non-final pages, " +
			"a mid-stream DISCARD must remove exactly its window and abandon delivery without abandoning " +
			"the statement, a positive qid must be refused on PULL and DISCARD, a second RUN while " +
			"streaming must be refused BY THE STATE GATE, and cursors must accumulate across sequential " +
			"RUNs in one transaction until the in-flight cap refuses one with a typed error",
		Mode:        ModeDeterministic,
		DefaultSeed: boltStreamDefaultSeed,
		run:         runBoltStreamSemanticsScenario,
	}
}

// boltStreamStallScenario drives the concurrent fault arm: the existing
// [SlowConsumer] stalls mid-stream against the bounded SimConn buffer and then tears
// its connection down, and the streaming surface must be intact for a fresh
// connection afterwards.
//
// It is registered separately from [boltStreamSemanticsScenario] because it is NOT
// bit-reproducible — how many records reach the bounded buffer before the consumer
// stops depends on the scheduler — and folding it into a deterministic scenario
// would make that scenario's report vary run to run while still claiming
// determinism.
func boltStreamStallScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltStreamingStall,
		Description: "Bolt streaming under a slow consumer: a stalled consumer must be handed a non-empty " +
			"PROPER prefix with the server's writer left blocked against the full connection buffer, and " +
			"a connection torn down on that blocked writer must leak no goroutine and leave paging, " +
			"has_more and record order intact for a fresh connection",
		Mode:        ModeConcurrent,
		DefaultSeed: boltStreamStallDefaultSeed,
		Connections: 2,
		OpsPerConn:  1,
		Mix:         &ConcurrentMix{ReaderWeight: 1.0},
		run:         runBoltStreamStallScenario,
	}
}

// runBoltStreamSemanticsScenario collects the deterministic evidence and adjudicates
// it against both the contract and the non-vacuity gate.
func runBoltStreamSemanticsScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltStreamSemantics(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltStreamSemantics(ev), checkBoltStreamSemanticsNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return boltStreamReport(ScenarioBoltStreaming, ModeDeterministic, seed, v), nil
}

// runBoltStreamStallScenario drives the concurrent fault arm.
func runBoltStreamStallScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltStreamStall(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltStreamSemantics(ev), checkBoltStreamSemanticsNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return boltStreamReport(ScenarioBoltStreamingStall, ModeConcurrent, seed, v), nil
}

// boltStreamReport wraps violations in a scenario report.
func boltStreamReport(name string, mode ExecMode, seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   name,
		Mode:       mode,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt streaming semantics>"},
		Violations: v,
	}
}

// boltStreamStallPollBudget / boltStreamStallPollStep bound the search for the
// parked state. The budget is generous relative to the work involved — the server
// only has to encode 64 KiB of integers — so expiring it means the writer never got
// scheduled at all, which the non-vacuity gate reports as a shortfall rather than as
// a defect of the server.
const (
	boltStreamStallPollBudget = 5 * time.Second
	boltStreamStallPollStep   = time.Millisecond
)

// boltStreamPollParked samples the bytes queued toward a stalled consumer until the
// connection buffer is FULL — the state in which the server's writer is provably
// blocked — and returns the largest reading it saw.
//
// The reachability is CONSTRUCTED rather than left to the scheduler: the consumer
// asked for slowConsumerResultRows rows, which encode to far more than
// [simConnBufferSize], and it has stopped reading, so the writer must fill the
// buffer and park. Sampling immediately instead — as a single ReadBuffered call
// does — reads whatever happens to be queued at that instant, which was MEASURED at
// 0 bytes on 2 of 3 seeds. A bound asserted against 0 is a bound asserted against
// nothing.
func boltStreamPollParked(ctx context.Context, c *WireClient) int {
	peak := 0
	deadline := time.Now().Add(boltStreamStallPollBudget)
	for {
		if n := c.Conn().ReadBuffered(); n > peak {
			peak = n
		}
		if peak >= simConnBufferSize || time.Now().After(deadline) || ctx.Err() != nil {
			return peak
		}
		time.Sleep(boltStreamStallPollStep)
	}
}
