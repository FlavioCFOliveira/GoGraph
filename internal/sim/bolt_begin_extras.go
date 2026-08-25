package sim

// bolt_begin_extras.go — the BEGIN extras surface under simulation (rmp #2485).
//
// # The gap this closes
//
// Until this task the harness had two ways to open an explicit transaction:
// [WireClient.Begin], which sent BEGIN with an EMPTY extras map, and
// [WireClient.BeginMode], which rmp #2482 added for the one key `mode` (VERIFIED with
// git log -S "BeginMode" -- internal/sim/wireclient.go, which names commit e2e45cf1,
// "#2482", and no other). Everything else a real driver puts
// in those extras was therefore driven by no DST scenario at all: `bookmarks`,
// `tx_timeout`, `tx_metadata`, `db`, and — a separate message rather than an extra,
// but the same routing-mode surface a driver exercises before it ever sends BEGIN —
// ROUTE's payload.
//
// # The bookmark result, which is the one to read carefully
//
// A causal read across two connections DOES observe the writer's commit. It is not
// evidence of anything about bookmarks.
//
// This server does not honour an incoming bookmark. [server.ExtractBookmarks] has
// exactly two non-test call sites in the module — bolt/server/session.go:1099 (RUN)
// and :1529 (BEGIN) — and both do nothing with the result but write it to a Debug
// log. The RUN site says so outright — "single-host server ignores them for causal
// consistency but they should not be silently dropped"
// (bolt/server/session.go:1097-1098). The BEGIN site carries no such note, only "Log
// incoming bookmarks for observability" (:1528). VERIFIED by grep of the whole
// module: that sentence occurs at exactly one place under bolt/.
//
// So a reader on a different connection that presents the writer's bookmark and
// then sees the write has seen it for a reason that has nothing to do with the
// token it sent: a single-host server has ONE store, and a committed write is
// already visible to any later read. "The reader saw the write" is a TRUE assertion
// that proves nothing, and an arm that stopped there would be exactly the vacuous
// shape this sprint has hit four times.
//
// The distinction is therefore made explicit and PROVED, by driving the SAME causal
// read three ways and requiring the three to be indistinguishable:
//
//   - with the writer's REAL bookmark — the property a driver actually depends on,
//     and it holds;
//   - with a FABRICATED bookmark this server never issued, whose counter is
//     provably ahead of every bookmark it did issue;
//   - with NO bookmarks at all, the baseline.
//
// All three must be ACCEPTED, must observe the identical count, and must not wait.
// That the fabricated token is accepted is the evidence that the token is IGNORED
// rather than honoured, and it is what makes the first arm's meaning honest. Were
// bookmarks ever honoured, a far-future token could only either block until the
// server's counter reached it — caught by the real-time bound — or be refused —
// caught by the acceptance clause. Either way the pin fires deliberately and this
// comment, and docs/dst-feature-coverage.md, have to be rewritten. That is the
// point of the pin.
//
// # What a bookmark IS here, as verified rather than described
//
// [server.NextBookmark] (bolt/server/bookmark.go:20) returns "FB:k" followed by the
// value of a PROCESS-GLOBAL atomic counter as 8 zero-padded hex digits. It is
// assigned to the session in exactly ONE place — s.bookmark = NextBookmark() in
// handleCommit, bolt/server/session.go:1694 — and delivered in three:
//
//   - the COMMIT SUCCESS, whose metadata is that bookmark and nothing else (:1696);
//   - the terminal PULL SUCCESS, i.e. the one with has_more false (:1397);
//   - the terminal DISCARD SUCCESS (:1500).
//
// rmp #2484 established that the terminal reply is also the durability
// acknowledgement, so the bookmark rides on the ack.
//
// Two consequences follow from "assigned only in handleCommit", and this scenario
// pins both because they are what a driver sees:
//
//   - An AUTOCOMMIT statement's terminal PULL SUCCESS carries a bookmark that was
//     NOT minted for it. On a session that has never committed an explicit
//     transaction the field is the EMPTY STRING — measured, on a write whose own
//     stats in the same SUCCESS report nodes-created=1 and contains-updates=true.
//   - On a session that HAS committed one, a later autocommit write's terminal PULL
//     SUCCESS carries that EARLIER transaction's bookmark — measured equal to it,
//     not merely similar. A driver chaining causality from an autocommit
//     ResultSummary is therefore chaining a strictly earlier transaction's token.
//
// Neither is asserted anywhere in bolt/server: the only bookmark-key assertion in
// that package is on a COMMIT SUCCESS and checks existence and non-emptiness only
// (bolt/server/tx_test.go:82-88).
//
// # The bookmark VALUE is not reachable from the seed
//
// bookmarkCounter is process-global (bolt/server/bookmark.go:13), exactly like the
// "__cx_"+hex(n) node key that limits the auth surface's byte oracle. The literal
// text of an issued bookmark therefore depends on how many transactions every OTHER
// test in the process committed first. Every clause here is consequently written
// over a DERIVED relation — equality between two observed bookmarks, strict advance
// between two successive ones, an ordering against the fabricated counter — never
// over a literal value; and [BoltBeginExtrasEvidence.String] renders an issued
// bookmark as a positional token so the rendering stays byte-stable.
//
// # tx_timeout: what the reap can and cannot be attributed to
//
// The serve loop reaps at the EARLIER of two bounds — the transaction's total
// lifetime and its silence (effectiveTxDeadline, bolt/server/serve.go:1155-1167,
// established by rmp #2482) — so an arm that left the idle bound at its 5 s default
// would be timing the wrong reaper. The SUBJECT arm therefore lifts BOTH the idle
// bound and the server's default total bound to 10 virtual minutes and asks for a
// small `tx_timeout`, so the ONLY deadline its advance can reach is the one the
// client sent. That baseline is the subject's alone: each of the other three arms in
// beginTimeoutPlans moves exactly one knob away from it, which is what makes it a
// control rather than a repetition. See the roster for which knob each one moves.
//
// The load-bearing part is the CONTROL: an identical script with the `tx_timeout`
// key REMOVED and the identical advance delivered must NOT be reaped. Without it,
// "advance and the transaction died" is satisfiable by any timer at all; with it,
// the single difference between the two arms is the extra.
//
// # A tx_timeout abort is NOT distinguishable on the wire, and that is asserted
//
// rmp #2482 pinned that an operator termination and a timeout deliver one shared
// code and message (filed as rmp #2560). Reading bolt/server widens that: the idle
// reaper, the total-lifetime reaper and Server.TerminateTransaction ALL funnel
// through [Session.reapTimedOutTx] (bolt/server/session.go:1831), which arms a
// single pendingTermErr with one code and one message (:1839-1842). The collision is
// three-way, not two-way.
//
// It is asserted rather than described: the idle-bound arm below is a CONSTRUCTED
// control that reaches the same reap through the idle bound instead, and the checker
// requires its code AND message to be byte-identical to the client-bound arm's. It
// differs from that arm in TWO fields, not one — the idle bound is the small one and
// `tx_timeout` is not sent — and the second is FORCED by the first: leaving the
// client's bound in place would arm a total-lifetime deadline at the same instant, so
// the arm could no longer attribute its reap to the idle reaper. If a later change
// separates the two paths' code or message, that clause fires.
//
// The abort is also not PUSHED. pendingTermErr is delivered on the client's next
// request-phase message and cleared there (bolt/server/session.go:594-597); a
// second message then draws *proto.Ignored. Both halves are pinned.
//
// # What the other extras actually do, each measured
//
//   - `mode`: only the exact string "r" selects read-only (handleBegin,
//     bolt/server/session.go:1561-1566). "w", "R", "bogus" and an absent key all
//     yield a WRITE transaction, and the arms confirm it against the server's own
//     [server.TransactionInfo.Mode] and by attempting a write. The coercion FAILS
//     OPEN — a driver that misspells the mode silently gets write authority — and
//     "R" is the plausible misspelling, so it is one of the arms.
//   - `db`: selectDatabaseFrom (:322) records the name unvalidated, and
//     databaseName (:309) reports it. A name that is not this server's is ECHOED,
//     which Options.DatabaseName's own godoc states deliberately
//     (bolt/server/serve.go:308-322): GoGraph serves one graph, so the name is a
//     label, not a selector. The arms pin the echo — including for "system", which
//     in Neo4j is a real distinct database and here is echoed like any other label —
//     and pin that the reported name is never empty, which is the rmp #2172
//     driver-panic guard.
//   - `tx_metadata`: not read ANYWHERE in the module. A grep for the key over every
//     .go file matches nothing, so unlike `bookmarks` it is not even logged. The arm
//     pins that it is accepted and echoed nowhere, rather than asserting a round
//     trip that does not happen. docs/bolt.md:225-226 already claimed this; nothing
//     drove it.
//   - ROUTE: handleRoute ignores the message's Routing map, its Bookmarks and its DB
//     entirely, answering RoutingTable(s.localAddr) (:1745-1752). So a ROUTE naming
//     a database is answered by a table whose own `db` is the EMPTY string, and
//     ROUTE's bookmarks are dropped without even the Debug line RUN and BEGIN give
//     theirs. Both are pinned. rmp #2481 covered ROUTE's auth gate and deliberately
//     left the payload here.

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
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// -----------------------------------------------------------------------------
// The wire vocabulary, spelled out so a clause pins the EXACT string
// -----------------------------------------------------------------------------

// The BEGIN extras keys this scenario drives. They are the wire spellings a real
// driver uses, and they are named here so a typo cannot silently turn an arm into a
// probe of an extra the server has never heard of — which would be accepted, and
// would pass every clause about acceptance.
const (
	beginKeyBookmarks = "bookmarks"
	beginKeyTxTimeout = "tx_timeout"
	beginKeyMetadata  = "tx_metadata"
	beginKeyMode      = "mode"
	beginKeyDB        = "db"
)

// The result-metadata keys the arms read back.
const (
	metaKeyBookmark = "bookmark"
	metaKeyDB       = "db"
	metaKeyHasMore  = "has_more"
	metaKeyRT       = "rt"
)

// The bookmark shape, as [server.NextBookmark] produces it
// (bolt/server/bookmark.go:20-23).
const (
	// bookmarkPrefix is the literal prefix of every bookmark this server mints.
	bookmarkPrefix = "FB:k"
	// bookmarkHexDigits is how many hex digits follow it. The counter is formatted
	// %08x, so a value above 0xFFFFFFFF would widen the field; the arms never reach
	// that, and the shape clause reports it rather than assuming it cannot happen.
	bookmarkHexDigits = 8
)

// beginFabricatedFarFuture is a syntactically VALID bookmark whose counter this
// server will never reach in a test: 0xFFFFFFFF is the largest value that still
// renders in [bookmarkHexDigits] digits, and the process-global counter starts at
// zero and advances once per COMMIT. It is a constant rather than a seed-drawn
// offset above the issued counter deliberately — that counter is process-global, so
// a derived value would not be reproducible from the seed — and the ordering against
// what the server DID issue is asserted instead (see [checkBoltBookmarkCausality]).
const beginFabricatedFarFuture = bookmarkPrefix + "ffffffff"

// beginFabricatedUnparseable is a token that is not a bookmark this server could
// ever have minted, in a different way from the far-future one: it does not carry the
// prefix at all. [server.ExtractBookmarks] does no validation whatsoever — it filters
// only by Go type — so this reaches the same do-nothing branch, which is exactly what
// the arm asserts.
const beginFabricatedUnparseable = "not-a-gograph-bookmark"

// beginBookmarkWrongTypeValue is a bookmarks-list element of the wrong PackStream
// type. [server.ExtractBookmarks] keeps only elements that assert to string
// (bolt/server/bookmark.go:38-41) and returns nil when none do (:43-45), so the server sees
// ZERO bookmarks from this arm rather than one it cannot parse. The arm exists
// because that is a different code path from a well-typed unknown token, and because
// it is the shape a buggy driver produces.
const beginBookmarkWrongTypeValue = int64(0x2485)

// The mode values driven. Only "r" is special (handleBegin,
// bolt/server/session.go:1561-1566); the rest are here to pin that the coercion to
// "w" is silent.
const (
	beginModeRead      = "r"
	beginModeWrite     = "w"
	beginModeUpperR    = "R"
	beginModeNonsense  = "bogus"
	beginModeAbsentTag = "<absent>"
)

// The database names driven. beginDBForeign is a name this server does not serve
// and beginDBSystem is the name of a database that in Neo4j is real and distinct.
const (
	beginDBForeign     = "not-this-server"
	beginDBSystem      = "system"
	beginDBAbsentTag   = "<absent>"
	beginTxMetadataKey = "gograph.dst.arm"
)

// beginReadOnlyRefusalCode / beginReadOnlyRefusalNeedle are what a write inside a
// mode "r" transaction is refused with. The needle is a substring of the engine's
// message rather than the whole of it: the text comes from cypher, not from
// bolt/server, so pinning it verbatim here would couple this scenario to a message
// the engine owns. The CODE is pinned exactly.
const (
	beginReadOnlyRefusalCode   = "Neo.ClientError.Request.Invalid"
	beginReadOnlyRefusalNeedle = "read-only transaction"
)

// The virtual geometry of the timeout arms. These are FAKE durations delivered
// through a [txClockProbe]; no wall time passes for any of them.
const (
	// beginTxTimeoutSmall is the total-lifetime bound an arm asks for with
	// `tx_timeout`, in virtual time.
	beginTxTimeoutSmall = 100 * time.Millisecond
	// beginTxTimeoutClear is the value the idle bound and the server's DEFAULT total
	// bound are lifted to on an arm that drives the CLIENT's bound. Both are far
	// above beginTxTimeoutSmall, so the earlier-of-the-two rule
	// (effectiveTxDeadline) can only ever select the client's.
	beginTxTimeoutClear = 10 * time.Minute
	// beginTxTimeoutHostile is the out-of-range `tx_timeout` the hostile arm sends, in
	// MILLISECONDS. No overflow is ever actually computed: clientMillisToDuration
	// rejects it on the magnitude guard ms > maxClientTimeoutMillis, which is tested
	// BEFORE the multiply by time.Millisecond (bolt/server/session.go:460-465, the
	// constant at :452), and returns (0, false). handleBegin then treats the key as
	// "unset" and leaves the server default in force. That is asserted in two
	// directions rather than described.
	beginTxTimeoutHostile = int64(1) << 62
)

// beginReplyBound bounds, in REAL time, every wait on the server: how long an
// observer may wait for a server goroutine to act on what the fake clock has already
// delivered, and how long a reply may take. It is generous relative to the work —
// the measured reap-to-registry-departure was ~240 microseconds and the measured
// abort reply ~81 — so exceeding it means the server STALLED rather than aborted,
// which is precisely the failure the acceptance criterion asks about. No virtual time
// is governed by it.
const beginReplyBound = 30 * time.Second

// beginNotReapedSettle is how long an arm waits, in REAL time, before concluding
// that an advance did NOT reap a transaction. It is a settle budget, not a proof:
// what forbids the reap is that no armed deadline is within the advance, which
// [clock.Fake]'s own arithmetic guarantees. What the budget buys is a window in which
// a reap that should not have happened can still land and be recorded.
const beginNotReapedSettle = 250 * time.Millisecond

// beginPollInterval is the granularity of every real-time wait here.
const beginPollInterval = 200 * time.Microsecond

// beginSeedMix decorrelates this scenario's draw stream from anything else derived from
// the same seed: [RunBoltBeginExtras] draws from NewSeed(seed ^ beginSeedMix).
//
// It MUST NOT equal [boltBeginExtrasDefaultSeed]. XOR is self-annihilating, so a mix
// equal to a given seed makes the mix a NO-OP at exactly that seed — and if that seed is
// the catalogue default, the mix does nothing on the one run every report, sweep and
// reproduction starts from, which is the run it most needs to decorrelate. (The draw
// itself stays sound: NewSeed(0) is rand.NewPCG(0, seedMix), not a degenerate stream.
// What is lost is the decorrelation, silently.)
//
// This is not a hypothetical trap. The constant shipped as 0x2485_B0_0C against a
// default of 0x2485_B00C, and Go reads those as the SAME number: digit separators are
// cosmetic and carry no grouping meaning whatsoever.
// [TestBoltBeginExtras_SeedMixDoesNotCancelTheDefaultSeed] guards the constant so the
// collision cannot return unnoticed.
const beginSeedMix = 0x2485_5EED

// beginCausalLabel is the label the writer commits under and the causal readers
// count. beginModeLabel is what a mode arm attempts to write.
const (
	beginCausalLabel = "BeginCausal"
	beginModeLabel   = "BeginModeWrite"
)

// beginCausalNodesMin / beginCausalNodesMax bound the seed-drawn number of nodes the
// writer commits. The minimum is 2 rather than 1 because driveWriter splits the draw
// across TWO explicit transactions, so a draw of 1 would leave one of them empty. It
// is never 0 for a second reason: a causal read that observed ZERO nodes could not
// distinguish "the reader saw the write" from "the write never happened" — the
// non-vacuity gate asserts the count is positive, and this makes it so by
// construction.
const (
	beginCausalNodesMin = 2
	beginCausalNodesMax = 6
)

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// BoltBookmarkArm is one causal read: what the reader put in its BEGIN
// `bookmarks` list, whether the BEGIN was accepted, and how many of the writer's
// committed nodes it went on to observe.
//
// It carries observations and no verdicts. Whether a fabricated token being
// accepted is correct is a question for [checkBoltBookmarkCausality], not for the
// collector.
type BoltBookmarkArm struct {
	// Name identifies the arm in a violation message and in the rendering.
	Name string
	// SentToken renders the single element the arm put in the `bookmarks` list, as
	// the arm built it. It is "<issued>" for the writer's own bookmark — never the
	// literal text, which is not reachable from the seed — and the verbatim constant
	// for a fabricated one. Empty means the key was omitted entirely.
	SentToken string
	// SentKey records whether the `bookmarks` key was present at all, so an arm that
	// omitted it is distinguishable from one that sent an empty list.
	SentKey bool
	// ServerSawTokens is how many tokens [server.ExtractBookmarks] would keep from
	// what the arm sent, computed by the HARNESS from its own list rather than read
	// back from the server (the server reports nothing). It is 0 for the wrong-type
	// arm, which is the point of that arm.
	ServerSawTokens int
	// Accepted is true when BEGIN was answered SUCCESS.
	Accepted bool
	// GotCode / GotMessage are the FAILURE's code and message, empty when none came.
	GotCode, GotMessage string
	// Observed is how many nodes carrying [beginCausalLabel] the arm's causal read
	// returned. It is compared ACROSS arms rather than against a literal, because
	// what the pin is about is that the token changed nothing.
	Observed int
	// ReadFailed carries a harness-level note when the causal read could not be
	// completed, so a broken read is never silently indistinguishable from a read
	// that returned zero.
	ReadFailed string
	// BeginElapsed is how long the BEGIN took in REAL time. It is the instrument for
	// "an unknown bookmark is not WAITED on", and it is deliberately absent from the
	// rendering: it is scheduling-dependent (rmp #2483's lesson) and belongs in a
	// clause, not in a byte-identity comparison.
	BeginElapsed time.Duration
}

// BoltTimeoutArm is one probe of the transaction-timeout reaper: the bounds it was
// run under, whether the advance reaped the transaction, and what the client was
// told when it next spoke.
type BoltTimeoutArm struct {
	// Name identifies the arm.
	Name string
	// SentTimeoutMS is the `tx_timeout` value the arm put on BEGIN, in
	// milliseconds. SentKey distinguishes "sent zero" from "omitted".
	SentTimeoutMS int64
	SentKey       bool
	// IdleBound / DefaultTotalBound are the two server-side bounds installed, in
	// VIRTUAL time. They are recorded because the reaper fires at the earlier of the
	// two and the whole attribution of the arm rests on which one that is.
	IdleBound, DefaultTotalBound time.Duration
	// Advanced is how much VIRTUAL time the arm delivered, in the order it delivered
	// it. An arm may advance twice: once to show a bound is NOT yet reached, once to
	// reach it.
	Advanced []time.Duration
	// ReapedAfter is the index into Advanced of the advance that reaped the
	// transaction, or -1 when no advance did.
	ReapedAfter int
	// TimersArmed is how many timers the server registered on the injected clock
	// across the arm. It is what makes a NOT-reaped reading evidence: a reaper that
	// declined is a different thing from a reaper that was never armed, and only
	// this counter separates them. syncTxTimer's NewTimer is the only clock timer
	// registration in bolt/server (verified exhaustively for rmp #2482), so the count
	// is attributable.
	TimersArmed int64
	// GotCode / GotMessage are what the client was told on its next request-phase
	// message after the reap. SecondReplyKind is what the message AFTER that drew,
	// which must be IGNORED.
	GotCode, GotMessage string
	SecondReplyKind     string
	// Committed is true when the arm's COMMIT succeeded, which is what a NOT-reaped
	// control must report.
	Committed bool
	// ReplyElapsed is the REAL time from delivering the reaping advance to holding
	// the abort in hand. It is the "aborted rather than stalled" instrument and, being
	// scheduling-dependent, is kept out of the rendering.
	ReplyElapsed time.Duration
}

// reaped reports whether any advance reaped this arm.
// The receiver is a pointer because a BoltTimeoutArm is well over gocritic's
// hugeParam threshold and these are called per arm in the adjudicator and the
// renderer.
func (a *BoltTimeoutArm) reaped() bool { return a.ReapedAfter >= 0 }

// BoltModeArm is one probe of the `mode` extra: what was sent, what the server's own
// registry says the transaction's mode is, and whether a write inside it was allowed.
type BoltModeArm struct {
	// Name identifies the arm; SentMode is the value sent, and SentKey whether the
	// key was present at all.
	Name     string
	SentMode string
	SentKey  bool
	// RegistryMode is [server.TransactionInfo.Mode] read off the server's own
	// open-transaction registry while the transaction was open. It is the INDEPENDENT
	// observable: BEGIN's SUCCESS carries no mode, so without the registry the only
	// evidence of the server's decision would be the write attempt itself, and a
	// clause resting on one observable could not tell a mis-recorded mode from a
	// mis-enforced one.
	RegistryMode string
	// WriteAccepted is true when a write statement inside the transaction was
	// answered SUCCESS; WriteCode and WriteMessage carry the refusal when it was not.
	WriteAccepted           bool
	WriteCode, WriteMessage string
}

// BoltDBArm is one probe of the `db` extra: the name sent on BEGIN and the name
// reported back in the terminal PULL SUCCESS.
type BoltDBArm struct {
	// Name identifies the arm; SentDB is the name sent, SentKey whether it was
	// present.
	Name    string
	SentDB  string
	SentKey bool
	// ReportedDB is the `db` field of the terminal PULL SUCCESS — the one a driver
	// turns into ResultSummary.Database().Name() (rmp #2172).
	ReportedDB string
	// ReportedOnRun is the `db` field of the RUN SUCCESS, which carries it too
	// (bolt/server/session.go handleRun). Recorded so the two are compared rather
	// than one assumed to follow the other.
	ReportedOnRun string
	// CommitMetaKeys is every key the COMMIT SUCCESS carried, sorted. It is here to
	// pin what COMMIT does NOT carry: `db` is absent from it, so a driver reading the
	// database name off a commit reply gets nothing.
	CommitMetaKeys []string
}

// BoltRouteObs is the ROUTE payload, read twice: once from a ROUTE carrying a
// routing context, a bookmark and a database name, and once from the zero message.
type BoltRouteObs struct {
	// ListenerAddr is [SimServer.ListenerAddr] — the independent reference the
	// advertised addresses are compared against.
	ListenerAddr string
	// SentDB and SentBookmark are what the populated ROUTE asked for, so the clause
	// that the reply ignores them names what was ignored.
	SentDB, SentBookmark string
	// Roles are the roles the table advertised, in the order the server listed them.
	Roles []string
	// AddressesByRole maps each advertised role to its address list. A role listed
	// twice would collide here, so RoleCount records the raw entry count separately.
	AddressesByRole map[string][]string
	RoleCount       int
	// TTL is the table's `ttl`, and TTLPresent whether the key was there and of the
	// expected type at all.
	TTL        int64
	TTLPresent bool
	// TableDB is the table's own `db` field. RoutingTable hardcodes it to the empty
	// string (bolt/server/route.go:30), so it does NOT echo SentDB — which is what
	// the arm pins.
	TableDB string
	// ZeroMessageRender is the rendering of the table returned for the ZERO ROUTE
	// message, and PopulatedRender the one for the populated message. They are
	// compared for equality, which is how "the routing context and the bookmarks are
	// ignored" is asserted rather than described.
	PopulatedRender, ZeroMessageRender string
	// Accepted records that both ROUTE messages were answered SUCCESS.
	Accepted bool
	// GotCode carries a refusal, empty when none came.
	GotCode string
}

// BoltMetadataObs is the `tx_metadata` probe: what was sent, and every metadata key
// that came back anywhere.
type BoltMetadataObs struct {
	// SentKeys are the keys the arm put in the `tx_metadata` map, sorted.
	SentKeys []string
	// Accepted is true when the BEGIN carrying them was answered SUCCESS.
	Accepted bool
	// GotCode carries a refusal, empty when none came.
	GotCode string
	// BeginMetaKeys, TerminalMetaKeys and CommitMetaKeys are every key the BEGIN
	// SUCCESS, the terminal PULL SUCCESS and the COMMIT SUCCESS carried, each sorted.
	// The clause is that no SentKey appears in any of them: `tx_metadata` is read
	// nowhere in the module, so there is nothing to echo, and pinning the absence is
	// the honest assertion.
	BeginMetaKeys, TerminalMetaKeys, CommitMetaKeys []string
}

// BoltBeginExtrasEvidence is everything one [RunBoltBeginExtras] observed. It is a
// plain value carrying observations only, so every checker is a pure function of it
// — which is what lets a test perturb one field and prove the corresponding clause
// fires.
type BoltBeginExtrasEvidence struct {
	// Bookmarks are the causal-read arms, in the order they ran (a seed-drawn
	// permutation).
	Bookmarks []BoltBookmarkArm
	// Timeouts, Modes and DBs are the other arm families, likewise in run order.
	Timeouts []BoltTimeoutArm
	Modes    []BoltModeArm
	DBs      []BoltDBArm
	// Route and Metadata are single observations rather than families.
	Route    BoltRouteObs
	Metadata BoltMetadataObs
	// CommittedNodes is how many nodes the writer committed under
	// [beginCausalLabel]: the seed-drawn figure every causal read must observe. It is
	// the harness's own count, never read back from the engine, so it is the
	// independent side of the causality comparison.
	CommittedNodes int
	// IssuedBookmarks are the bookmarks the writer's successive COMMITs returned, in
	// order. Their literal text is NOT reachable from the seed (see the file header),
	// so clauses are written over relations between them, and the rendering shows
	// them positionally.
	IssuedBookmarks []string
	// AutocommitBookmarkFresh is the `bookmark` in the terminal PULL SUCCESS of an
	// autocommit WRITE on a connection that has never committed an explicit
	// transaction. Measured EMPTY.
	AutocommitBookmarkFresh string
	// AutocommitBookmarkAfterCommit is the same field on a connection that HAS, and
	// AutocommitAfterCommitExpected is the bookmark that connection's COMMIT had
	// returned. The clause is that the two are EQUAL: the autocommit statement was
	// handed the earlier transaction's token rather than one minted for itself.
	AutocommitBookmarkAfterCommit string
	AutocommitAfterCommitExpected string
	// AutocommitStatsSeen records that the autocommit write's own terminal SUCCESS
	// reported it as an update, so the empty bookmark above cannot be explained by
	// the statement having written nothing.
	AutocommitStatsSeen bool
	// Seed is the seed the run was built from.
	Seed uint64
}

// -----------------------------------------------------------------------------
// The collector
// -----------------------------------------------------------------------------

// RunBoltBeginExtras drives the whole BEGIN extras surface once and returns the
// evidence.
//
// It is bit-reproducible from seed. The seed draws three things and nothing else:
// how many nodes the writer commits, the order the causal-read arms run in, and the
// order the mode and database arms run in. Every other quantity is either a fixed
// script or a virtual duration; no arm depends on wall time for its outcome, only
// for its bound.
//
// The bookmark, mode, database, ROUTE and metadata families share ONE server,
// because the causal read is only meaningful against the store the writer committed
// to. The timeout family builds a server per arm: each needs its own bounds and its
// own fake clock, and a fake clock cannot be installed on a server that is already
// serving (see [SimServer.Server]).
func RunBoltBeginExtras(ctx context.Context, seed uint64) (*BoltBeginExtrasEvidence, error) {
	sd := NewSeed(seed ^ beginSeedMix)
	ev := &BoltBeginExtrasEvidence{Seed: seed}
	ev.CommittedNodes = beginCausalNodesMin + sd.IntN(beginCausalNodesMax-beginCausalNodesMin+1)

	// The shared server. Both bounds are lifted well clear of the arm's own runtime:
	// these families hold explicit transactions open across several round trips while
	// pinning exact failure codes, and a default 5 s idle reaper firing mid-arm would
	// answer Neo.ClientError.Transaction.TransactionTimedOut to something that was not
	// being tested — the same reason [NewSimServerAuth] lifts it (simserver.go
	// simAuthMaxTxIdle). The server clock stays REAL: nothing in these families
	// depends on virtual time, and the timeout family is where the fake belongs.
	srv, err := NewSimServerTxRegistry(
		SimEngineForServer(), clock.Real(), clock.Real(),
		beginTxTimeoutClear, beginTxTimeoutClear, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-begin-extras: build server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	r := &beginExtrasRunner{srv: srv, sd: sd, ev: ev}
	for _, step := range []func(context.Context) error{
		// The writer runs first: every causal read needs a bookmark to present and a
		// committed write to observe.
		r.driveWriter,
		r.driveAutocommitBookmarks,
		r.driveBookmarkArms,
		r.driveModeArms,
		r.driveDBArms,
		r.driveRoute,
		r.driveMetadata,
		// The timeout family last: it builds its own servers.
		r.driveTimeoutArms,
	} {
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	return ev, nil
}

// beginExtrasRunner holds the collector's state. It is passed by pointer and driven
// from one goroutine.
type beginExtrasRunner struct {
	srv *SimServer
	sd  *Seed
	ev  *BoltBeginExtrasEvidence
}

// connect opens a client and completes the handshake and authentication. The caller
// closes it.
func (r *beginExtrasRunner) connect(ctx context.Context) (*WireClient, error) {
	c, err := r.srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-begin-extras: dial: %w", err)
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-begin-extras: connect: %w", err)
	}
	return c, nil
}

// driveWriter commits the nodes every causal read must observe, in TWO explicit
// transactions on one connection, and records the bookmark each COMMIT returned.
//
// Two rather than one: a single bookmark cannot falsify a monotonicity clause (one
// event is not a sequence), so the strict-advance clause needs a second. The nodes
// are split across them so the whole committed set is what a reader must see.
func (r *beginExtrasRunner) driveWriter(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// Split the drawn total across two transactions, both non-empty: the minimum
	// draw is 2, so the halves are at least 1 each.
	first := r.ev.CommittedNodes / 2
	batches := []int{first, r.ev.CommittedNodes - first}
	for i, n := range batches {
		if resp, err := c.Begin(); err != nil {
			return fmt.Errorf("sim: bolt-begin-extras: writer BEGIN %d: %w", i, err)
		} else if !isSuccess(resp) {
			return fmt.Errorf("sim: bolt-begin-extras: writer BEGIN %d refused: %v", i, resp)
		}
		for k := 0; k < n; k++ {
			if err := r.runInTx(c, fmt.Sprintf("CREATE (:%s {batch: %d, k: %d})", beginCausalLabel, i, k)); err != nil {
				return err
			}
		}
		resp, err := c.Commit()
		if err != nil {
			return fmt.Errorf("sim: bolt-begin-extras: writer COMMIT %d: %w", i, err)
		}
		if !isSuccess(resp) {
			return fmt.Errorf("sim: bolt-begin-extras: writer COMMIT %d refused: %v", i, resp)
		}
		bm := metaString(resp, metaKeyBookmark)
		r.ev.IssuedBookmarks = append(r.ev.IssuedBookmarks, bm)
	}
	return nil
}

// runInTx sends one statement inside an already-open explicit transaction and drains
// it to its terminal reply, failing on anything other than a SUCCESS.
func (r *beginExtrasRunner) runInTx(c *WireClient, query string) error {
	resp, err := c.Run(query, nil)
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: RUN %q: %w", query, err)
	}
	if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-begin-extras: RUN %q refused: %v", query, resp)
	}
	_, term, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: PULL for %q: %w", query, err)
	}
	if !isSuccess(term) {
		return fmt.Errorf("sim: bolt-begin-extras: PULL for %q refused: %v", query, term)
	}
	return nil
}

// driveAutocommitBookmarks records what the terminal PULL SUCCESS of an AUTOCOMMIT
// write carries in its `bookmark` field, in the two states that differ: a connection
// that has never committed an explicit transaction, and one that has.
//
// Both readings are taken on connections this method owns, never on the writer's,
// so the second one's expected value is a bookmark IT observed rather than one
// carried over from another arm.
func (r *beginExtrasRunner) driveAutocommitBookmarks(ctx context.Context) error {
	// Fresh connection: no explicit COMMIT has ever run on it.
	fresh, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = fresh.Close() }()
	bm, sawStats, err := r.autocommitWrite(fresh, "fresh")
	if err != nil {
		return err
	}
	r.ev.AutocommitBookmarkFresh = bm
	r.ev.AutocommitStatsSeen = sawStats

	// A connection that has committed one explicit transaction first.
	after, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = after.Close() }()
	if resp, err := after.Begin(); err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: autocommit-after BEGIN: %w", err)
	} else if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-begin-extras: autocommit-after BEGIN refused: %v", resp)
	}
	if err := r.runInTx(after, fmt.Sprintf("CREATE (:%s {seed: 1})", beginModeLabel)); err != nil {
		return err
	}
	resp, err := after.Commit()
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: autocommit-after COMMIT: %w", err)
	}
	if !isSuccess(resp) {
		return fmt.Errorf("sim: bolt-begin-extras: autocommit-after COMMIT refused: %v", resp)
	}
	r.ev.AutocommitAfterCommitExpected = metaString(resp, metaKeyBookmark)
	bm, _, err = r.autocommitWrite(after, "after-commit")
	if err != nil {
		return err
	}
	r.ev.AutocommitBookmarkAfterCommit = bm
	return nil
}

// autocommitWrite runs one AUTOCOMMIT write and returns the `bookmark` its terminal
// PULL SUCCESS carried, together with whether that same SUCCESS reported the
// statement as an update.
//
// The stats reading exists so the empty bookmark the fresh connection reports cannot
// be explained away: the reply that omits a bookmark is the SAME reply that says a
// node was created and the statement contained updates.
func (r *beginExtrasRunner) autocommitWrite(c *WireClient, tag string) (bookmark string, sawStats bool, err error) {
	q := fmt.Sprintf("CREATE (:%s {tag: %q})", beginModeLabel, tag)
	resp, err := c.Run(q, nil)
	if err != nil {
		return "", false, fmt.Errorf("sim: bolt-begin-extras: autocommit RUN (%s): %w", tag, err)
	}
	if !isSuccess(resp) {
		return "", false, fmt.Errorf("sim: bolt-begin-extras: autocommit RUN (%s) refused: %v", tag, resp)
	}
	_, term, err := c.PullAll()
	if err != nil {
		return "", false, fmt.Errorf("sim: bolt-begin-extras: autocommit PULL (%s): %w", tag, err)
	}
	if !isSuccess(term) {
		return "", false, fmt.Errorf("sim: bolt-begin-extras: autocommit PULL (%s) refused: %v", tag, term)
	}
	return metaString(term, metaKeyBookmark), beginTerminalReportedUpdate(term), nil
}

// beginTerminalReportedUpdate reports whether a terminal SUCCESS's `stats` map says
// the statement contained updates. resultStats omits the key entirely for a
// statement that changed nothing (bolt/server/session.go handlePull), so its presence
// AND its contains-updates flag are both checked.
func beginTerminalReportedUpdate(resp any) bool {
	s, ok := resp.(*proto.Success)
	if !ok {
		return false
	}
	stats, ok := s.Metadata["stats"].(map[string]packstream.Value)
	if !ok {
		return false
	}
	upd, _ := stats["contains-updates"].(bool)
	return upd
}

// beginBookmarkArmNames are the causal-read arms, in the canonical order the
// non-vacuity gate checks the roster against. The order they RUN in is a seed-drawn
// permutation of this list.
var beginBookmarkArmNames = []string{
	"no-bookmark-baseline",
	"real-issued-bookmark",
	"fabricated-far-future",
	"fabricated-unparseable",
	"fabricated-wrong-type",
}

// driveBookmarkArms drives every causal-read arm, each on its OWN connection, in a
// seed-drawn order.
//
// A separate connection per arm is what makes the read CROSS-connection, which is the
// property a driver depends on: a reader on the writer's own connection would be
// reading its own session's writes and would prove nothing about causality even in a
// server that honoured bookmarks.
func (r *beginExtrasRunner) driveBookmarkArms(ctx context.Context) error {
	for _, name := range r.sd.Shuffle(beginBookmarkArmNames) {
		arm, err := r.driveBookmarkArm(ctx, name)
		if err != nil {
			return err
		}
		r.ev.Bookmarks = append(r.ev.Bookmarks, arm)
	}
	return nil
}

// driveBookmarkArm opens a fresh connection, BEGINs with the arm's `bookmarks` list,
// counts the writer's committed nodes, and rolls back.
func (r *beginExtrasRunner) driveBookmarkArm(ctx context.Context, name string) (BoltBookmarkArm, error) {
	arm := BoltBookmarkArm{Name: name, Observed: -1}
	var token packstream.Value
	switch name {
	case "no-bookmark-baseline":
		// The key is omitted entirely.
	case "real-issued-bookmark":
		if len(r.ev.IssuedBookmarks) == 0 {
			return arm, fmt.Errorf("sim: bolt-begin-extras: no issued bookmark to present")
		}
		token = r.ev.IssuedBookmarks[len(r.ev.IssuedBookmarks)-1]
		arm.SentKey, arm.SentToken, arm.ServerSawTokens = true, beginIssuedTag, 1
	case "fabricated-far-future":
		token = beginFabricatedFarFuture
		arm.SentKey, arm.SentToken, arm.ServerSawTokens = true, beginFabricatedFarFuture, 1
	case "fabricated-unparseable":
		token = beginFabricatedUnparseable
		arm.SentKey, arm.SentToken, arm.ServerSawTokens = true, beginFabricatedUnparseable, 1
	case "fabricated-wrong-type":
		// ExtractBookmarks keeps only string elements, so the server sees ZERO
		// bookmarks from this arm rather than one it cannot parse.
		token = beginBookmarkWrongTypeValue
		arm.SentKey, arm.SentToken, arm.ServerSawTokens = true, "<int64>", 0
	default:
		return arm, fmt.Errorf("sim: bolt-begin-extras: unknown bookmark arm %q", name)
	}

	c, err := r.connect(ctx)
	if err != nil {
		return arm, err
	}
	defer func() { _ = c.Close() }()

	extra := map[string]packstream.Value{}
	if arm.SentKey {
		extra[beginKeyBookmarks] = []packstream.Value{token}
	}
	start := time.Now()
	resp, err := c.BeginExtras(extra)
	arm.BeginElapsed = time.Since(start)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s BEGIN: %w", name, err)
	}
	arm.Accepted = isSuccess(resp)
	arm.GotCode, arm.GotMessage = failureCode(resp), failureMessage(resp)
	if !arm.Accepted {
		// A refused BEGIN has no transaction to read in; the refusal IS the
		// observation and the checker adjudicates it.
		return arm, nil
	}
	n, err := beginCountLabel(c, beginCausalLabel)
	if err != nil {
		arm.ReadFailed = err.Error()
		return arm, nil
	}
	arm.Observed = n
	if _, err := c.Rollback(); err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s ROLLBACK: %w", name, err)
	}
	return arm, nil
}

// beginIssuedTag is how an issued bookmark is RENDERED, in both the arm's SentToken
// and the evidence rendering. The literal text is not reachable from the seed (see
// the file header), so it never appears in a value whose byte-identity is asserted.
const beginIssuedTag = "<issued>"

// beginCountLabel counts nodes carrying label over the genuine wire, inside whatever
// transaction the connection currently has open.
//
// It returns an error rather than a zero count when the statement fails, so a broken
// probe can never be mistaken for "the label is absent" — which for a causality arm
// would be the difference between a pass and the defect it exists to find.
func beginCountLabel(c *WireClient, label string) (int, error) {
	q := fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", label)
	resp, err := c.Run(q, nil)
	if err != nil {
		return 0, fmt.Errorf("census RUN: %w", err)
	}
	if !isSuccess(resp) {
		return 0, fmt.Errorf("census RUN refused: %v", resp)
	}
	recs, term, err := c.PullAll()
	if err != nil {
		return 0, fmt.Errorf("census PULL: %w", err)
	}
	if !isSuccess(term) {
		return 0, fmt.Errorf("census PULL refused: %v", term)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("census returned %d row(s), want exactly 1 with 1 column", len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("census scalar is %T, want int64", recs[0].Data[0])
	}
	return int(n), nil
}

// beginModeArmNames are the `mode` arms, canonical order.
var beginModeArmNames = []string{
	beginModeRead, beginModeWrite, beginModeUpperR, beginModeNonsense, beginModeAbsentTag,
}

// driveModeArms drives every `mode` value in a seed-drawn order, reading the server's
// own registry for its decision and then attempting a write.
func (r *beginExtrasRunner) driveModeArms(ctx context.Context) error {
	for _, name := range r.sd.Shuffle(beginModeArmNames) {
		arm, err := r.driveModeArm(ctx, name)
		if err != nil {
			return err
		}
		r.ev.Modes = append(r.ev.Modes, arm)
	}
	return nil
}

// driveModeArm opens one transaction with the arm's `mode`, reads
// [server.TransactionInfo.Mode] off the registry, attempts a write, and rolls back.
func (r *beginExtrasRunner) driveModeArm(ctx context.Context, name string) (BoltModeArm, error) {
	arm := BoltModeArm{Name: "mode=" + name}
	extra := map[string]packstream.Value{}
	if name != beginModeAbsentTag {
		arm.SentKey, arm.SentMode = true, name
		extra[beginKeyMode] = name
	}

	c, err := r.connect(ctx)
	if err != nil {
		return arm, err
	}
	defer func() { _ = c.Close() }()

	// The registry must be EMPTY before the BEGIN, so the single entry read back
	// afterwards is unambiguously this arm's. Every family here is sequential, but a
	// previous arm's connection teardown is asynchronous, so this is waited for rather
	// than assumed.
	if err := r.waitRegistry(0, "before "+arm.Name); err != nil {
		return arm, err
	}
	resp, err := c.BeginExtras(extra)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s BEGIN: %w", arm.Name, err)
	}
	if !isSuccess(resp) {
		arm.WriteCode, arm.WriteMessage = failureCode(resp), failureMessage(resp)
		return arm, nil
	}
	// handleBegin registers the transaction BEFORE the response loop writes the
	// SUCCESS (rmp #2482), so holding the SUCCESS is already sufficient for the
	// listing; it is still waited for, because "already listed" is a claim about
	// ordering and a wait costs nothing when it is true.
	if err := r.waitRegistry(1, "after "+arm.Name); err != nil {
		return arm, err
	}
	arm.RegistryMode = r.srv.Server().Transactions()[0].Mode

	wr, err := c.Run(fmt.Sprintf("CREATE (:%s {mode: %q})", beginModeLabel, name), nil)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s write RUN: %w", arm.Name, err)
	}
	if isSuccess(wr) {
		_, term, err := c.PullAll()
		if err != nil {
			return arm, fmt.Errorf("sim: bolt-begin-extras: %s write PULL: %w", arm.Name, err)
		}
		arm.WriteAccepted = isSuccess(term)
		arm.WriteCode, arm.WriteMessage = failureCode(term), failureMessage(term)
	} else {
		arm.WriteCode, arm.WriteMessage = failureCode(wr), failureMessage(wr)
	}
	// A refused write leaves the session FAILED, where ROLLBACK is illegal; RESET is
	// the documented way back and it also discards the transaction.
	if _, err := c.Reset(); err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s RESET: %w", arm.Name, err)
	}
	return arm, nil
}

// waitRegistry waits, in bounded REAL time, for the server's open-transaction
// registry to hold exactly want entries.
func (r *beginExtrasRunner) waitRegistry(want int, what string) error {
	deadline := time.Now().Add(beginReplyBound)
	for {
		if len(r.srv.Server().Transactions()) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sim: bolt-begin-extras: registry never held %d transaction(s) %s (waited %s)",
				want, what, beginReplyBound)
		}
		time.Sleep(beginPollInterval)
	}
}

// beginDBArmNames are the `db` arms, canonical order. The server's OWN name is in the
// roster alongside the foreign ones so the echo clause has a case where echoing and
// falling back are indistinguishable, and the absent case where they are not.
var beginDBArmNames = []string{
	server.DefaultDatabaseName, beginDBForeign, beginDBSystem, beginDBAbsentTag,
}

// driveDBArms drives every `db` value on BEGIN in a seed-drawn order and reads the
// name back from the RUN SUCCESS and the terminal PULL SUCCESS.
//
// The name is sent on BEGIN and NOT on the subsequent RUN, which is what a real driver
// does inside an explicit transaction (rmp #2172) and what handleRun's `if !s.txActive`
// guard (bolt/server/session.go:1134-1142) makes safe: were that guard absent, the
// RUN's empty extras would CLEAR the selection BEGIN recorded, and this arm is what
// would see it.
func (r *beginExtrasRunner) driveDBArms(ctx context.Context) error {
	for _, name := range r.sd.Shuffle(beginDBArmNames) {
		arm, err := r.driveDBArm(ctx, name)
		if err != nil {
			return err
		}
		r.ev.DBs = append(r.ev.DBs, arm)
	}
	return nil
}

// driveDBArm opens one transaction naming the arm's database, runs a read, records the
// reported name from both replies, and commits.
func (r *beginExtrasRunner) driveDBArm(ctx context.Context, name string) (BoltDBArm, error) {
	arm := BoltDBArm{Name: "db=" + name}
	extra := map[string]packstream.Value{}
	if name != beginDBAbsentTag {
		arm.SentKey, arm.SentDB = true, name
		extra[beginKeyDB] = name
	}

	c, err := r.connect(ctx)
	if err != nil {
		return arm, err
	}
	defer func() { _ = c.Close() }()

	resp, err := c.BeginExtras(extra)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s BEGIN: %w", arm.Name, err)
	}
	if !isSuccess(resp) {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s BEGIN refused: %v", arm.Name, resp)
	}
	runResp, err := c.Run("RETURN 1 AS one", nil)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s RUN: %w", arm.Name, err)
	}
	if !isSuccess(runResp) {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s RUN refused: %v", arm.Name, runResp)
	}
	arm.ReportedOnRun = metaString(runResp, metaKeyDB)
	_, term, err := c.PullAll()
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s PULL: %w", arm.Name, err)
	}
	if !isSuccess(term) {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s PULL refused: %v", arm.Name, term)
	}
	arm.ReportedDB = metaString(term, metaKeyDB)
	commit, err := c.Commit()
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s COMMIT: %w", arm.Name, err)
	}
	if !isSuccess(commit) {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s COMMIT refused: %v", arm.Name, commit)
	}
	arm.CommitMetaKeys = metaKeys(commit)
	return arm, nil
}

// driveRoute sends ROUTE twice on one connection — once carrying a routing context, a
// bookmark and a database name, once as the zero message — and records the payload of
// each.
func (r *beginExtrasRunner) driveRoute(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	obs := BoltRouteObs{
		ListenerAddr: r.srv.ListenerAddr(),
		SentDB:       beginDBForeign,
		SentBookmark: beginFabricatedFarFuture,
	}
	populated, err := c.Request(&proto.Route{
		Routing:   map[string]packstream.Value{"address": "sim-routing-context:7687"},
		Bookmarks: []packstream.Value{obs.SentBookmark},
		DB:        obs.SentDB,
	})
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: populated ROUTE: %w", err)
	}
	zero, err := c.Request(&proto.Route{})
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: zero ROUTE: %w", err)
	}
	obs.Accepted = isSuccess(populated) && isSuccess(zero)
	if code := failureCode(populated); code != "" {
		obs.GotCode = code
	} else {
		obs.GotCode = failureCode(zero)
	}
	obs.PopulatedRender = beginRenderRoutingTable(populated)
	obs.ZeroMessageRender = beginRenderRoutingTable(zero)
	beginFillRouteTable(&obs, populated)
	r.ev.Route = obs
	return nil
}

// beginFillRouteTable decodes the routing table out of a ROUTE reply into the
// observation's structured fields. A reply that is not a SUCCESS, or that carries no
// well-formed `rt`, leaves them at their zero values, which the checker reports.
func beginFillRouteTable(obs *BoltRouteObs, resp any) {
	s, ok := resp.(*proto.Success)
	if !ok {
		return
	}
	rt, ok := s.Metadata[metaKeyRT].(map[string]packstream.Value)
	if !ok {
		return
	}
	obs.TableDB, _ = rt[metaKeyDB].(string)
	if ttl, ok := rt["ttl"].(int64); ok {
		obs.TTL, obs.TTLPresent = ttl, true
	}
	servers, ok := rt["servers"].([]packstream.Value)
	if !ok {
		return
	}
	obs.RoleCount = len(servers)
	obs.AddressesByRole = make(map[string][]string, len(servers))
	for _, entry := range servers {
		m, ok := entry.(map[string]packstream.Value)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		obs.Roles = append(obs.Roles, role)
		addrs, ok := m["addresses"].([]packstream.Value)
		if !ok {
			continue
		}
		list := make([]string, 0, len(addrs))
		for _, a := range addrs {
			str, _ := a.(string)
			list = append(list, str)
		}
		obs.AddressesByRole[role] = list
	}
}

// driveMetadata sends BEGIN carrying `tx_metadata` and records every metadata key that
// comes back from the BEGIN, the terminal PULL and the COMMIT.
func (r *beginExtrasRunner) driveMetadata(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	sent := map[string]packstream.Value{
		beginTxMetadataKey: "bolt-begin-extras",
		"gograph.dst.seed": int64(r.ev.Seed),
	}
	obs := BoltMetadataObs{SentKeys: slices.Sorted(maps.Keys(sent))}
	resp, err := c.BeginExtras(map[string]packstream.Value{beginKeyMetadata: sent})
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: metadata BEGIN: %w", err)
	}
	obs.Accepted, obs.GotCode = isSuccess(resp), failureCode(resp)
	obs.BeginMetaKeys = metaKeys(resp)
	if !obs.Accepted {
		r.ev.Metadata = obs
		return nil
	}
	runResp, err := c.Run("RETURN 1 AS one", nil)
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: metadata RUN: %w", err)
	}
	if !isSuccess(runResp) {
		return fmt.Errorf("sim: bolt-begin-extras: metadata RUN refused: %v", runResp)
	}
	_, term, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: metadata PULL: %w", err)
	}
	obs.TerminalMetaKeys = metaKeys(term)
	commit, err := c.Commit()
	if err != nil {
		return fmt.Errorf("sim: bolt-begin-extras: metadata COMMIT: %w", err)
	}
	obs.CommitMetaKeys = metaKeys(commit)
	r.ev.Metadata = obs
	return nil
}

// beginTimeoutPlan is one timeout arm's geometry: the bounds to install, the
// `tx_timeout` to send, and the virtual advances to deliver.
//
// It is a table rather than four hand-written arms so the difference between an arm
// and its control is visible as a differing field rather than buried in prose. The
// client-bound arm and the no-tx_timeout control differ only in sendKey. The idle
// control differs in TWO fields, idleBound and sendKey, because reaching the idle
// reaper requires the client's bound to be absent — see the three-way-collision note
// at the top of this file.
type beginTimeoutPlan struct {
	name      string
	sendKey   bool
	timeoutMS int64
	idleBound time.Duration
	defTotal  time.Duration
	advances  []time.Duration
}

// beginTimeoutPlans is the timeout roster.
//
// Every advance is VIRTUAL. The first three arms deliver exactly
// [beginTxTimeoutSmall], which is the whole attribution: on the client-bound arm that
// is the deadline the client asked for and both server bounds are 6000x further away,
// so no other timer can be what fired; on the control the SAME advance reaches nothing
// at all; on the idle arm the idle bound is what it reaches.
var beginTimeoutPlans = []beginTimeoutPlan{
	{
		// The subject: the client's own bound, with both server bounds lifted clear.
		name:      "client-tx-timeout",
		sendKey:   true,
		timeoutMS: beginTxTimeoutSmall.Milliseconds(),
		idleBound: beginTxTimeoutClear,
		defTotal:  beginTxTimeoutClear,
		advances:  []time.Duration{beginTxTimeoutSmall},
	},
	{
		// THE CONTROL. Byte-for-byte the arm above with the `tx_timeout` key removed.
		// It must survive the identical advance, which is what attributes the arm
		// above's reap to the extra rather than to the advance.
		name:      "no-tx-timeout-control",
		sendKey:   false,
		idleBound: beginTxTimeoutClear,
		defTotal:  beginTxTimeoutClear,
		advances:  []time.Duration{beginTxTimeoutSmall},
	},
	{
		// The COLLISION control: the same reap reached through the IDLE bound instead.
		// Its code and message must be byte-identical to the client-bound arm's, which
		// is how the shared-failure finding is asserted rather than described.
		name:      "idle-bound-control",
		sendKey:   false,
		idleBound: beginTxTimeoutSmall,
		defTotal:  beginTxTimeoutClear,
		advances:  []time.Duration{beginTxTimeoutSmall},
	},
	{
		// The hostile arm: an OVERFLOWING tx_timeout. clientMillisToDuration rejects
		// it, so the server default stays in force — asserted in BOTH directions by
		// two advances, the first of which must not reap and the second of which must.
		name:      "overflow-tx-timeout",
		sendKey:   true,
		timeoutMS: beginTxTimeoutHostile,
		idleBound: beginTxTimeoutClear,
		defTotal:  beginTxTimeoutSmall,
		advances:  []time.Duration{beginTxTimeoutSmall / 2, beginTxTimeoutSmall / 2},
	},
}

// driveTimeoutArms drives every timeout arm, each against its own server with its own
// fake clock. The roster order is FIXED rather than shuffled: an arm and its control
// are only comparable because they are the same script, and running them in a drawn
// order would add a variable to a comparison whose whole value is having exactly one.
func (r *beginExtrasRunner) driveTimeoutArms(ctx context.Context) error {
	for i := range beginTimeoutPlans {
		arm, err := driveBoltTimeoutArm(ctx, &beginTimeoutPlans[i])
		if err != nil {
			return err
		}
		r.ev.Timeouts = append(r.ev.Timeouts, arm)
	}
	return nil
}

// driveBoltTimeoutArm builds a server on a fake clock, opens one explicit transaction
// under the plan's bounds, delivers the plan's advances, and records what the client is
// told when it next speaks.
func driveBoltTimeoutArm(ctx context.Context, p *beginTimeoutPlan) (BoltTimeoutArm, error) {
	arm := BoltTimeoutArm{
		Name: p.name, SentKey: p.sendKey, SentTimeoutMS: p.timeoutMS,
		IdleBound: p.idleBound, DefaultTotalBound: p.defTotal,
		// CLONED, never aliased: beginTimeoutPlans is package-level, so handing its
		// backing array to the evidence would let any later mutation of one arm's
		// Advanced rewrite the plan every other arm and every other run reads from.
		Advanced: slices.Clone(p.advances), ReapedAfter: -1,
	}
	probe := newTxClockProbe()
	srv, err := NewSimServerTxRegistry(
		SimEngineForServer(), clock.Real(), probe, p.idleBound, p.defTotal, 0,
	)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s: build server: %w", p.name, err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s: dial: %w", p.name, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s: connect: %w", p.name, err)
	}

	extra := map[string]packstream.Value{}
	if p.sendKey {
		extra[beginKeyTxTimeout] = p.timeoutMS
	}
	resp, err := c.BeginExtras(extra)
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s BEGIN: %w", p.name, err)
	}
	if !isSuccess(resp) {
		// A refused BEGIN is recorded, not raised: whether an overflowing tx_timeout
		// SHOULD be refused is a question for the checker.
		arm.GotCode, arm.GotMessage = failureCode(resp), failureMessage(resp)
		return arm, nil
	}

	// THE BARRIER, in the order the message loop establishes them (rmp #2482): the
	// registry lists the transaction first, and syncTxTimer arms the timer strictly
	// later. Advancing on the listing alone would advance past a deadline no timer was
	// yet waiting for, and the reap would land an advance late.
	if err := waitForTxRegistry(func() bool { return len(srv.Server().Transactions()) == 1 },
		fmt.Sprintf("%s: the registry never listed the transaction", p.name)); err != nil {
		return arm, err
	}
	if err := waitForTxRegistry(func() bool { return probe.Timers() >= 1 },
		fmt.Sprintf("%s: the server never armed a timer on the injected clock", p.name)); err != nil {
		return arm, err
	}

	for i, adv := range p.advances {
		start := time.Now()
		probe.Advance(adv)
		if beginAwaitReap(srv) {
			arm.ReapedAfter = i
			arm.ReplyElapsed = time.Since(start)
			break
		}
	}
	arm.TimersArmed = probe.Timers()

	// The abort is not PUSHED: pendingTermErr is delivered on the next request-phase
	// message (bolt/server/session.go:594-597). So the client has to speak to learn
	// anything, and the reply is timed from the reaping advance to make "aborted rather
	// than stalled" a measured statement.
	replyStart := time.Now()
	commit, err := c.Commit()
	if err != nil {
		return arm, fmt.Errorf("sim: bolt-begin-extras: %s COMMIT: %w", p.name, err)
	}
	if arm.reaped() {
		arm.ReplyElapsed += time.Since(replyStart)
	}
	arm.Committed = isSuccess(commit)
	arm.GotCode, arm.GotMessage = failureCode(commit), failureMessage(commit)
	if !arm.Committed {
		// A second request-phase message on a FAILED session must draw IGNORED: the
		// termination is delivered ONCE and then cleared.
		second, err := c.Commit()
		if err != nil {
			return arm, fmt.Errorf("sim: bolt-begin-extras: %s second COMMIT: %w", p.name, err)
		}
		arm.SecondReplyKind = beginReplyKind(second)
	}
	return arm, nil
}

// beginAwaitReap reports whether the server's registry drops to zero open
// transactions within the settle budget: the transaction was reaped.
//
// The budget is one-sided by construction. A reap that HAS happened is observed within
// microseconds (measured ~240), so expiring the budget means no reap landed — and what
// forbids one is that no armed deadline lies within the advance, which [clock.Fake]'s
// own arithmetic guarantees rather than this budget. What the budget buys is a window
// in which a reap that should NOT have happened can still land and be recorded.
func beginAwaitReap(srv *SimServer) bool {
	deadline := time.Now().Add(beginNotReapedSettle)
	for {
		if len(srv.Server().Transactions()) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(beginPollInterval)
	}
}

// -----------------------------------------------------------------------------
// Shared readers
// -----------------------------------------------------------------------------

// beginReplyKind classifies a terminal reply for the rendering and for a clause that
// must name WHICH shape arrived. It is deliberately coarse — the discrimination that
// matters is SUCCESS vs FAILURE vs IGNORED, and rmp #2484's rule is that an
// acknowledgement is the terminal reply and nothing else.
func beginReplyKind(resp any) string {
	switch {
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

// metaString reads a string-valued metadata key off a SUCCESS, returning "" when the
// reply is not a SUCCESS, the key is absent, or its value is not a string.
//
// The three cases are deliberately conflated into "" because every clause written over
// this reads a field the server is contracted to populate; a clause that needs to tell
// "absent" from "empty" uses [metaKeys] instead, which reports presence.
func metaString(resp any, key string) string {
	s, ok := resp.(*proto.Success)
	if !ok {
		return ""
	}
	v, _ := s.Metadata[key].(string)
	return v
}

// metaKeys returns every metadata key a SUCCESS carried, sorted. A reply that is not a
// SUCCESS yields nil — and so does a SUCCESS carrying an EMPTY metadata map, because
// slices.Sorted collects into a nil slice and never returns an empty non-nil one
// (MEASURED, not assumed). The two are therefore NOT distinguishable here, and no
// clause may try: BEGIN's own SUCCESS carries exactly the empty map
// (bolt/server/session.go:1650), so BeginMetaKeys is nil on every healthy run. A
// clause that needs "this reply was a SUCCESS" establishes it separately, which is
// why the metadata non-vacuity clause gates on the terminal and COMMIT replies that
// are contracted to carry keys.
func metaKeys(resp any) []string {
	s, ok := resp.(*proto.Success)
	if !ok {
		return nil
	}
	return slices.Sorted(maps.Keys(s.Metadata))
}

// beginRenderRoutingTable renders a ROUTE reply's routing table canonically: keys
// sorted, roles in the order the server listed them, addresses verbatim.
//
// It exists so "the populated ROUTE and the zero ROUTE are answered identically" is a
// comparison of two renderings rather than a hand-written field-by-field walk that
// could omit the field a regression changed. Go's map iteration is randomised, so the
// sorting is what makes the comparison meaningful at all.
func beginRenderRoutingTable(resp any) string {
	s, ok := resp.(*proto.Success)
	if !ok {
		return "<not-a-success:" + beginReplyKind(resp) + ">"
	}
	rt, ok := s.Metadata[metaKeyRT].(map[string]packstream.Value)
	if !ok {
		return "<no-rt>"
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(rt)) {
		if k == "servers" {
			continue
		}
		fmt.Fprintf(&b, "%s=%v;", k, rt[k])
	}
	servers, _ := rt["servers"].([]packstream.Value)
	for _, entry := range servers {
		m, ok := entry.(map[string]packstream.Value)
		if !ok {
			b.WriteString("<malformed-entry>;")
			continue
		}
		role, _ := m["role"].(string)
		addrs, _ := m["addresses"].([]packstream.Value)
		fmt.Fprintf(&b, "%s%v;", role, addrs)
	}
	return b.String()
}

// beginBookmarkCounter parses the counter out of a bookmark of the shape
// [server.NextBookmark] produces, reporting whether the text had that shape at all.
//
// It is what lets the fabricated far-future token be compared against what the server
// actually issued, instead of the comparison being asserted from the constant's
// appearance. A token that does not parse yields ok false, which the shape clause
// reports rather than treating as a zero counter.
func beginBookmarkCounter(bm string) (uint64, bool) {
	hex, ok := strings.CutPrefix(bm, bookmarkPrefix)
	if !ok || len(hex) != bookmarkHexDigits {
		return 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// -----------------------------------------------------------------------------
// The contract
// -----------------------------------------------------------------------------

// boltBeginOp names the clause a violation came from.
func boltBeginOp(clause string) string { return "<bolt-begin-extras:" + clause + ">" }

// boltBeginViolation builds one violation for a clause.
func boltBeginViolation(kind ViolationKind, clause, msg string) Violation {
	return Violation{Kind: kind, Op: boltBeginOp(clause), Message: msg}
}

// checkBoltBeginExtras adjudicates the whole BEGIN extras surface. It is a pure
// function of the evidence, so a test can perturb one field of a hand-built healthy
// value and prove the corresponding clause fires.
func checkBoltBeginExtras(e *BoltBeginExtrasEvidence) []Violation {
	// slices.Concat rather than seven appends onto a nil slice: it sizes the result
	// once, and it returns nil — not an empty non-nil slice — when every clause is
	// satisfied, which is the shape a caller testing len(v) == 0 expects. Same choice,
	// for the same reason, as checkBoltTxRegistry.
	return slices.Concat(
		checkBoltBookmarkCausality(e),
		checkBoltBookmarkDelivery(e),
		checkBoltTxTimeout(e),
		checkBoltBeginMode(e),
		checkBoltBeginDB(e),
		checkBoltRoutePayload(e),
		checkBoltTxMetadata(e),
	)
}

// checkBoltBookmarkCausality adjudicates the causal-read family.
//
// Two clauses do different jobs and must not be collapsed. "The causal read observes
// the write" is the property a driver depends on and it holds. "Every arm observes the
// SAME thing, including the ones presenting a bookmark this server never issued" is
// the evidence that the token is IGNORED rather than honoured, and it is the only
// reason the first clause's meaning is honest. See the file header.
func checkBoltBookmarkCausality(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	for i := range e.Bookmarks {
		a := &e.Bookmarks[i]
		// CLAUSE bookmark-accepted. Every BEGIN must be admitted — the fabricated ones
		// included. This is the deliberate PIN: a server that honoured bookmarks could
		// not accept a token naming a transaction it never had.
		if !a.Accepted {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-accepted",
				fmt.Sprintf("arm %q: BEGIN carrying bookmark %s was REFUSED with %q / %q; this server does not honour "+
					"incoming bookmarks (ExtractBookmarks is only logged, bolt/server/session.go:1529), so every token "+
					"must be accepted — if that changed, the finding recorded in docs/dst-feature-coverage.md is now wrong",
					a.Name, a.SentToken, a.GotCode, a.GotMessage)))
			continue
		}
		// CLAUSE bookmark-read-completed. A read that could not be completed must never
		// be indistinguishable from one that returned nothing.
		if a.ReadFailed != "" {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-read-completed",
				fmt.Sprintf("arm %q: the causal read could not be completed: %s", a.Name, a.ReadFailed)))
			continue
		}
		// CLAUSE bookmark-causal-read. The reader on a DIFFERENT connection must observe
		// every node the writer committed.
		if a.Observed != e.CommittedNodes {
			v = append(v, boltBeginViolation(ViolationACIDDurability, "bookmark-causal-read",
				fmt.Sprintf("arm %q: the causal read observed %d node(s) of the %d the writer COMMITTED on another "+
					"connection; a committed write must be visible to every later reader",
					a.Name, a.Observed, e.CommittedNodes)))
		}
		// CLAUSE bookmark-does-not-wait. An unknown token must not be waited on. A
		// server that honoured bookmarks and was handed a far-future one could only
		// block, and this is what would catch it.
		if a.BeginElapsed > beginReplyBound {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-does-not-wait",
				fmt.Sprintf("arm %q: BEGIN carrying bookmark %s took %s, over the %s bound: the server WAITED on the "+
					"token instead of ignoring it", a.Name, a.SentToken, a.BeginElapsed, beginReplyBound)))
		}
	}

	// CLAUSE bookmark-token-changes-nothing. THE proof of ignoring: every arm that
	// completed its read must have observed the identical count, so presenting a real
	// bookmark, a fabricated one, or none at all is indistinguishable.
	baseline, haveBaseline := -1, false
	baselineName := ""
	for i := range e.Bookmarks {
		a := &e.Bookmarks[i]
		if !a.Accepted || a.ReadFailed != "" {
			continue
		}
		if !haveBaseline {
			baseline, baselineName, haveBaseline = a.Observed, a.Name, true
			continue
		}
		if a.Observed != baseline {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-token-changes-nothing",
				fmt.Sprintf("arm %q observed %d node(s) but arm %q observed %d: the BEGIN bookmark CHANGED what the "+
					"reader saw. This server ignores incoming bookmarks, so it cannot; if it now honours them, the "+
					"causal-read clause above has become a real test of causality and this scenario's documentation "+
					"must be rewritten", a.Name, a.Observed, baselineName, baseline)))
		}
	}

	// CLAUSE bookmark-fabricated-is-ahead. The far-future token must name a counter
	// strictly ABOVE every bookmark this server issued, or "the server never issued it"
	// is an assumption rather than an observation.
	fab, ok := beginBookmarkCounter(beginFabricatedFarFuture)
	if !ok {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-fabricated-is-ahead",
			fmt.Sprintf("the fabricated token %q does not parse as a %q + %d hex-digit bookmark, so it is not a "+
				"syntactically valid token this server could have minted and the arm proves nothing about an "+
				"UNKNOWN bookmark", beginFabricatedFarFuture, bookmarkPrefix, bookmarkHexDigits)))
	}
	for _, bm := range e.IssuedBookmarks {
		n, parsed := beginBookmarkCounter(bm)
		if !parsed || !ok {
			continue
		}
		if n >= fab {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-fabricated-is-ahead",
				fmt.Sprintf("this server issued bookmark counter %d, at or above the fabricated token's %d: the "+
					"'far future' token is one the server COULD have minted, so the arm is no longer probing an "+
					"unknown bookmark", n, fab)))
		}
	}
	return v
}

// checkBoltBookmarkDelivery adjudicates what a bookmark IS and where it arrives — the
// shape [server.NextBookmark] produces, the strict advance across successive COMMITs,
// and the two AUTOCOMMIT readings.
//
// The two autocommit clauses are deliberate PINS of a defect-shaped fact, not of
// desired behaviour: s.bookmark is assigned in exactly one place, handleCommit
// (bolt/server/session.go:1694), so a terminal PULL SUCCESS reports whatever the last
// EXPLICIT commit left there. A driver chaining causality off an autocommit
// ResultSummary therefore gets an empty token, or an earlier transaction's.
func checkBoltBookmarkDelivery(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation

	// CLAUSE bookmark-shape. Every issued bookmark must have the documented shape.
	var prev uint64
	var havePrev bool
	for i, bm := range e.IssuedBookmarks {
		n, ok := beginBookmarkCounter(bm)
		if !ok {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-shape",
				fmt.Sprintf("COMMIT %d returned bookmark %q, which is not %q followed by %d hex digits "+
					"(bolt/server/bookmark.go:20-23)", i, bm, bookmarkPrefix, bookmarkHexDigits)))
			continue
		}
		// CLAUSE bookmark-strictly-advances. Successive COMMITs on one connection must
		// mint strictly increasing counters. The comparison is between two OBSERVED
		// values, never against a literal: the counter is process-global, so its
		// absolute value is not reachable from the seed.
		if havePrev && n <= prev {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "bookmark-strictly-advances",
				fmt.Sprintf("COMMIT %d minted counter %d, not strictly above the previous COMMIT's %d: a bookmark "+
					"that does not advance cannot order two transactions", i, n, prev)))
		}
		prev, havePrev = n, true
	}

	// CLAUSE autocommit-bookmark-is-empty. On a session that never committed an
	// explicit transaction, an autocommit WRITE's terminal PULL SUCCESS carries an
	// EMPTY bookmark. Pinned as measured.
	if e.AutocommitBookmarkFresh != "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "autocommit-bookmark-is-empty",
			fmt.Sprintf("an autocommit write on a session with no prior explicit COMMIT reported bookmark %q in its "+
				"terminal PULL SUCCESS, not the empty string. s.bookmark is assigned ONLY in handleCommit "+
				"(bolt/server/session.go:1694), so this can only mean the server now mints one per autocommit "+
				"statement — which would be an IMPROVEMENT, and this pin and its documentation must be rewritten "+
				"rather than the reading dismissed", e.AutocommitBookmarkFresh)))
	}
	// CLAUSE autocommit-bookmark-is-stale. On a session that HAS committed one, the
	// autocommit terminal PULL SUCCESS carries that earlier transaction's bookmark.
	if e.AutocommitBookmarkAfterCommit != e.AutocommitAfterCommitExpected {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "autocommit-bookmark-is-stale",
			fmt.Sprintf("an autocommit write following an explicit COMMIT reported bookmark %q; the COMMIT had "+
				"returned %q. They must be EQUAL: the terminal PULL echoes s.bookmark and nothing mints a fresh one "+
				"for an autocommit statement. A difference means the behaviour changed and the pin must be rewritten",
				e.AutocommitBookmarkAfterCommit, e.AutocommitAfterCommitExpected)))
	}
	// CLAUSE autocommit-did-write. The empty bookmark above must not be explainable by
	// the statement having written nothing: the SAME reply must report it as an update.
	if !e.AutocommitStatsSeen {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "autocommit-did-write",
			"the autocommit write's terminal SUCCESS did not report contains-updates, so the empty bookmark it "+
				"carried cannot be attributed to the bookmark machinery: the statement may simply have written "+
				"nothing, and the two pins above would be measuring the wrong thing"))
	}
	return v
}

// beginTimeoutArm returns the named timeout arm, or nil.
func beginTimeoutArm(e *BoltBeginExtrasEvidence, name string) *BoltTimeoutArm {
	for i := range e.Timeouts {
		if e.Timeouts[i].Name == name {
			return &e.Timeouts[i]
		}
	}
	return nil
}

// checkBoltTxTimeout adjudicates the transaction-timeout family.
//
// The attribution rests on the CONTROL, not on the subject: "advance and the
// transaction died" is satisfiable by any timer, so what makes it a statement about
// `tx_timeout` is that the byte-identical script with the key REMOVED survives the
// identical advance.
func checkBoltTxTimeout(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	subject := beginTimeoutArm(e, "client-tx-timeout")
	control := beginTimeoutArm(e, "no-tx-timeout-control")
	idle := beginTimeoutArm(e, "idle-bound-control")
	overflow := beginTimeoutArm(e, "overflow-tx-timeout")

	// CLAUSE txtimeout-reaps. The client's own bound must reap the transaction, and it
	// must be reachable by the FIRST advance — a later ordinal would mean the deadline
	// was not the one the client asked for.
	if subject != nil {
		if subject.ReapedAfter != 0 {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-reaps",
				fmt.Sprintf("arm %q asked for tx_timeout=%dms with the idle bound at %s and the server default at %s, "+
					"and an advance of exactly that bound did NOT reap it (reaped-after ordinal %d, want 0): the "+
					"client-supplied total bound was not applied",
					subject.Name, subject.SentTimeoutMS, subject.IdleBound, subject.DefaultTotalBound, subject.ReapedAfter)))
		}
		v = append(v, beginCheckAbortShape(subject)...)
	}

	// CLAUSE txtimeout-control-survives. THE attribution. Identical script, key removed,
	// identical advance: the transaction must live and its COMMIT must succeed.
	if control != nil {
		if control.reaped() {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-control-survives",
				fmt.Sprintf("the control arm %q sent NO tx_timeout and its bounds are %s idle / %s total, yet an "+
					"advance of %v reaped it. Some other deadline is firing, so the subject arm's reap cannot be "+
					"attributed to its tx_timeout extra and the whole family proves nothing",
					control.Name, control.IdleBound, control.DefaultTotalBound, control.Advanced)))
		}
		if !control.Committed {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-control-commits",
				fmt.Sprintf("the control arm %q was not reaped but its COMMIT was answered %q / %q instead of SUCCESS",
					control.Name, control.GotCode, control.GotMessage)))
		}
	}

	// CLAUSE txtimeout-shares-one-failure. The CONSTRUCTED collision: the same reap
	// reached through the IDLE bound must be reported to the client with a
	// byte-identical code AND message. reapTimedOutTx arms one pendingTermErr for the
	// idle reaper, the total reaper and Server.TerminateTransaction alike
	// (bolt/server/session.go:1831-1842), so a client cannot tell them apart. This is
	// rmp #2560 widened from two paths to three, asserted rather than described.
	if idle != nil {
		if !idle.reaped() {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-idle-control-reaps",
				fmt.Sprintf("the collision control %q installed an idle bound of %s and advanced %v without being "+
					"reaped, so it cannot witness the shared failure shape", idle.Name, idle.IdleBound, idle.Advanced)))
		}
		v = append(v, beginCheckAbortShape(idle)...)
		if subject != nil && idle.reaped() && subject.reaped() {
			if idle.GotCode != subject.GotCode || idle.GotMessage != subject.GotMessage {
				v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-shares-one-failure",
					fmt.Sprintf("a total-bound abort reports %q / %q and an IDLE-bound abort reports %q / %q. They "+
						"are currently ONE shared pendingTermErr (bolt/server/session.go:1839-1842), which is the "+
						"finding this clause pins (rmp #2560). If they now differ, the collision has been fixed and "+
						"docs/dst-feature-coverage.md must be rewritten to say so",
						subject.GotCode, subject.GotMessage, idle.GotCode, idle.GotMessage)))
			}
		}
	}

	// CLAUSE txtimeout-overflow-keeps-default. An overflowing tx_timeout must be
	// treated as UNSET, leaving the server default in force. Asserted in both
	// directions: the first advance (half the default) must not reap, the second must.
	if overflow != nil {
		if overflow.ReapedAfter != 1 {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-overflow-keeps-default",
				fmt.Sprintf("arm %q sent tx_timeout=%d ms, which overflows to a non-positive duration and must be "+
					"treated as unset, leaving the %s server default in force. It was reaped after advance ordinal "+
					"%d, want 1: ordinal 0 means a bound SHORTER than the default is in play; no reap at all means "+
					"the deadline was left unarmed and the reaper is DISABLED, which is the security failure "+
					"bolt/server/security_bolt_txtimeout_overflow_test.go guards on a private field and this arm "+
					"guards at the client", overflow.Name, overflow.SentTimeoutMS, overflow.DefaultTotalBound,
					overflow.ReapedAfter)))
		}
		v = append(v, beginCheckAbortShape(overflow)...)
	}
	return v
}

// beginCheckAbortShape adjudicates what a REAPED arm's client was told: the exact code
// and message, that a second message is IGNORED, and that the abort arrived rather than
// stalled.
//
// It is shared by every reaped arm so the three of them cannot drift apart, and it is
// skipped entirely for an arm that was not reaped — an arm with nothing to report must
// not be able to satisfy a clause about a report.
func beginCheckAbortShape(a *BoltTimeoutArm) []Violation {
	if !a.reaped() {
		return nil
	}
	var v []Violation
	// CLAUSE txtimeout-abort-typed. The EXACT code and message, not merely "some
	// failure": these are the constants rmp #2482 pinned (bolt_tx_registry.go:517-518).
	if a.GotCode != txReapFailureCode {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-abort-typed",
			fmt.Sprintf("arm %q: after the reap the client's next request-phase message was answered code %q, "+
				"want %q", a.Name, a.GotCode, txReapFailureCode)))
	}
	if a.GotMessage != txReapFailureMessage {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-abort-typed",
			fmt.Sprintf("arm %q: abort message %q, want %q", a.Name, a.GotMessage, txReapFailureMessage)))
	}
	// CLAUSE txtimeout-abort-delivered-once. The termination is delivered ONCE and
	// cleared (bolt/server/session.go:594-597); the message after it draws IGNORED.
	if a.SecondReplyKind != "IGNORED" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-abort-delivered-once",
			fmt.Sprintf("arm %q: the SECOND request-phase message after the abort drew %s, want IGNORED. The typed "+
				"termination is delivered once and cleared; repeating it would tell a client the transaction timed "+
				"out twice", a.Name, a.SecondReplyKind)))
	}
	// CLAUSE txtimeout-aborts-not-stalls. Bracketed in REAL time from the reaping
	// advance to the abort in hand.
	if a.ReplyElapsed > beginReplyBound {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-aborts-not-stalls",
			fmt.Sprintf("arm %q: %s elapsed between the reaping advance and the abort reaching the client, over the "+
				"%s bound: the transaction STALLED rather than aborting", a.Name, a.ReplyElapsed, beginReplyBound)))
	}
	// CLAUSE txtimeout-timer-was-armed. A reap is only attributable to the reaper if the
	// reaper was armed on the injected clock.
	if a.TimersArmed < 1 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "txtimeout-timer-was-armed",
			fmt.Sprintf("arm %q was reaped but the injected clock registered %d timer(s): the reap did not come from "+
				"the serve loop's transaction timer, so nothing here is measuring tx_timeout",
				a.Name, a.TimersArmed)))
	}
	return v
}

// checkBoltBeginMode adjudicates the `mode` family.
//
// The finding it pins is that the coercion FAILS OPEN. handleBegin selects read-only
// only for the exact string "r" (bolt/server/session.go:1561-1566); every other value
// — a misspelling, the uppercase "R", a non-string, an absent key — silently yields a
// WRITE transaction. That is asserted on TWO independent observables: the server's own
// [server.TransactionInfo.Mode], and whether a write inside the transaction is allowed.
// One observable alone could not tell a mis-recorded mode from a mis-enforced one.
func checkBoltBeginMode(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	for i := range e.Modes {
		a := &e.Modes[i]
		wantRead := a.SentKey && a.SentMode == beginModeRead
		wantMode := beginModeWrite
		if wantRead {
			wantMode = beginModeRead
		}
		// CLAUSE mode-registry-agrees. The server's own registry must record the mode
		// the wire spelling selects.
		if a.RegistryMode != wantMode {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "mode-registry-agrees",
				fmt.Sprintf("arm %q sent mode %q (present=%t) and the server's open-transaction registry reports "+
					"Mode=%q, want %q. Only the exact string \"r\" selects read-only; every other value is coerced to "+
					"write (bolt/server/session.go:1561-1566)", a.Name, a.SentMode, a.SentKey, a.RegistryMode, wantMode)))
		}
		if wantRead {
			// CLAUSE mode-read-refuses-write. A read-only transaction must refuse a write
			// with the exact code, and its message must name the reason.
			if a.WriteAccepted {
				v = append(v, boltBeginViolation(ViolationACIDConsistency, "mode-read-refuses-write",
					fmt.Sprintf("arm %q opened a read-only transaction and the write inside it was ACCEPTED: "+
						"mode \"r\" must refuse a write or the access mode is decorative", a.Name)))
			}
			if a.WriteCode != beginReadOnlyRefusalCode {
				v = append(v, boltBeginViolation(ViolationOracleDeviation, "mode-read-refuses-write",
					fmt.Sprintf("arm %q: the write in a read-only transaction was refused with code %q, want %q",
						a.Name, a.WriteCode, beginReadOnlyRefusalCode)))
			}
			if !strings.Contains(strings.ToLower(a.WriteMessage), beginReadOnlyRefusalNeedle) {
				v = append(v, boltBeginViolation(ViolationOracleDeviation, "mode-read-refuses-write",
					fmt.Sprintf("arm %q: the refusal message %q does not mention %q, so the refusal cannot be "+
						"attributed to the access mode rather than to some other guard sharing the code",
						a.Name, a.WriteMessage, beginReadOnlyRefusalNeedle)))
			}
			continue
		}
		// CLAUSE mode-unknown-fails-open. Every non-"r" value must ACCEPT the write.
		// This is the deliberate pin of the fail-open coercion: a driver that sends "R"
		// gets write authority and is told nothing.
		if !a.WriteAccepted {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "mode-unknown-fails-open",
				fmt.Sprintf("arm %q sent mode %q (present=%t) and the write inside it was REFUSED with %q / %q. "+
					"Every value other than the exact string \"r\" is currently coerced to a WRITE transaction, so a "+
					"refusal means the coercion changed — which would be a HARDENING of a fail-open path, and this "+
					"pin and docs/dst-feature-coverage.md must be rewritten to record it",
					a.Name, a.SentMode, a.SentKey, a.WriteCode, a.WriteMessage)))
		}
	}
	return v
}

// checkBoltBeginDB adjudicates the `db` family: the unvalidated echo, the fallback, and
// what a COMMIT does NOT carry.
func checkBoltBeginDB(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	for i := range e.DBs {
		a := &e.DBs[i]
		want := server.DefaultDatabaseName
		if a.SentKey && a.SentDB != "" {
			want = a.SentDB
		}
		// CLAUSE db-echoed-unvalidated. A name this server does not serve is ECHOED
		// verbatim, which Options.DatabaseName's godoc states deliberately
		// (bolt/server/serve.go:308-322). Pinned as real, including for "system".
		if a.ReportedDB != want {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "db-echoed-unvalidated",
				fmt.Sprintf("arm %q sent db=%q on BEGIN (present=%t) and the terminal PULL SUCCESS reported db=%q, "+
					"want %q. GoGraph serves one graph, so the name is a label and not a selector: an unknown name is "+
					"echoed rather than refused with Neo.ClientError.Database.DatabaseNotFound. A refusal or a "+
					"different name means that decision changed", a.Name, a.SentDB, a.SentKey, a.ReportedDB, want)))
		}
		// CLAUSE db-run-and-terminal-agree. The RUN SUCCESS and the terminal PULL SUCCESS
		// both carry `db`, and they must agree: a driver reads the name off whichever it
		// happens to consume.
		if a.ReportedOnRun != a.ReportedDB {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "db-run-and-terminal-agree",
				fmt.Sprintf("arm %q: the RUN SUCCESS reported db=%q and the terminal PULL SUCCESS db=%q; a client "+
					"reading either must get the same answer", a.Name, a.ReportedOnRun, a.ReportedDB)))
		}
		// CLAUSE db-never-empty. The reported name must never be empty: the official
		// driver hands back a nil DatabaseInfo for an absent or empty `db` and
		// summary.Database().Name() then panics inside the driver (rmp #2172).
		if a.ReportedDB == "" {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "db-never-empty",
				fmt.Sprintf("arm %q: the terminal PULL SUCCESS reported an EMPTY db. The official neo4j-go-driver "+
					"returns a nil DatabaseInfo for that, so the idiomatic summary.Database().Name() panics inside "+
					"the driver (rmp #2172)", a.Name)))
		}
		// CLAUSE db-absent-from-commit. COMMIT's SUCCESS carries the bookmark and
		// NOTHING else (bolt/server/session.go:1695-1697), so a client cannot read the
		// database name off it. Pinned so a widening of that reply is noticed.
		if !slices.Equal(a.CommitMetaKeys, []string{metaKeyBookmark}) {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "db-absent-from-commit",
				fmt.Sprintf("arm %q: the COMMIT SUCCESS carried metadata keys %v, want exactly [%s]. handleCommit "+
					"answers with the bookmark alone; anything else means the reply widened and a client may now be "+
					"reading a field this scenario does not model", a.Name, a.CommitMetaKeys, metaKeyBookmark)))
		}
	}
	return v
}

// beginExpectedRoles are the three roles a single-host routing table advertises
// (bolt/server/route.go:12-25), in the order RoutingTable builds them.
var beginExpectedRoles = []string{"WRITE", "READ", "ROUTE"}

// beginRoutingTableTTL is the TTL RoutingTable hardcodes, in seconds
// (bolt/server/route.go:29).
const beginRoutingTableTTL = int64(300)

// checkBoltRoutePayload adjudicates the ROUTE reply's routing table.
//
// The addresses are compared against [SimServer.ListenerAddr] — the listener's own
// view of itself, reached by a different route than the reply's — so "the table names
// THIS server" is a comparison of two independently obtained values. Comparing against
// [server.RoutingTable]'s own output would be comparing that function with itself.
func checkBoltRoutePayload(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	o := &e.Route
	// CLAUSE route-accepted. Both ROUTE messages must be answered SUCCESS.
	if !o.Accepted {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-accepted",
			fmt.Sprintf("ROUTE was refused with code %q; an authenticated session in READY must receive a routing "+
				"table (bolt/server/session.go:1745-1752)", o.GotCode)))
		return v
	}
	// CLAUSE route-roles. Exactly the three single-host roles, in the order the server
	// builds them.
	if !slices.Equal(o.Roles, beginExpectedRoles) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-roles",
			fmt.Sprintf("the routing table advertised roles %v, want exactly %v: a single-host table points all "+
				"three roles at the one server", o.Roles, beginExpectedRoles)))
	}
	if o.RoleCount != len(beginExpectedRoles) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-roles",
			fmt.Sprintf("the routing table carried %d server entries, want %d", o.RoleCount, len(beginExpectedRoles))))
	}
	// CLAUSE route-names-this-server. Each role advertises EXACTLY the listener's own
	// address and nothing else.
	for _, role := range beginExpectedRoles {
		addrs, ok := o.AddressesByRole[role]
		if !ok {
			continue // already reported by route-roles
		}
		if !slices.Equal(addrs, []string{o.ListenerAddr}) {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-names-this-server",
				fmt.Sprintf("role %s advertises addresses %v; the listener this server is serving on reports %q. A "+
					"routing table naming anything else sends a driver to a host that is not this one",
					role, addrs, o.ListenerAddr)))
		}
	}
	// CLAUSE route-ttl. The TTL is hardcoded and a driver caches the table for it, so
	// the exact value is pinned rather than merely required to be non-negative.
	if !o.TTLPresent {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-ttl",
			"the routing table carried no int64 ttl; a driver with no TTL cannot know when to refresh the table"))
	} else if o.TTL != beginRoutingTableTTL {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-ttl",
			fmt.Sprintf("the routing table reports ttl=%d, want %d (bolt/server/route.go:29)", o.TTL, beginRoutingTableTTL)))
	}
	// CLAUSE route-db-is-dropped. THE PIN. handleRoute ignores the ROUTE message
	// entirely and answers RoutingTable(s.localAddr), whose `db` is the empty string
	// (bolt/server/route.go:30). So a driver asking for the routing table OF a named
	// database is answered by a table labelled with nothing.
	if o.TableDB != "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-db-is-dropped",
			fmt.Sprintf("the ROUTE message named database %q and the table came back labelled %q. RoutingTable "+
				"hardcodes db to the empty string and handleRoute never reads the message's DB field, so a non-empty "+
				"label means ROUTE now honours it — an improvement, and this pin and its documentation must be "+
				"rewritten", o.SentDB, o.TableDB)))
	}
	// CLAUSE route-ignores-context-and-bookmarks. A ROUTE carrying a routing context, a
	// bookmark and a database name must be answered IDENTICALLY to the zero message.
	if o.PopulatedRender != o.ZeroMessageRender {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "route-ignores-context-and-bookmarks",
			fmt.Sprintf("a ROUTE carrying routing context, bookmark %q and db %q was answered\n  %s\nand the ZERO "+
				"ROUTE message was answered\n  %s\nhandleRoute reads none of the three fields, so the two tables must "+
				"be identical", o.SentBookmark, o.SentDB, o.PopulatedRender, o.ZeroMessageRender)))
	}
	return v
}

// checkBoltTxMetadata adjudicates `tx_metadata`: accepted, and echoed nowhere.
//
// The key is read NOWHERE in the module — a grep over every .go file matches nothing —
// so unlike `bookmarks` it is not even logged. docs/bolt.md:225-226 already claimed
// "accepted and silently ignored"; this is what drives it.
func checkBoltTxMetadata(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	o := &e.Metadata
	// CLAUSE metadata-accepted. An extra the server does not implement must not break
	// the BEGIN: a driver sends tx_metadata routinely.
	if !o.Accepted {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "metadata-accepted",
			fmt.Sprintf("BEGIN carrying tx_metadata keys %v was refused with code %q; the key is read nowhere in the "+
				"module and must simply be ignored (docs/bolt.md:224-225)", o.SentKeys, o.GotCode)))
		return v
	}
	// CLAUSE metadata-echoed-nowhere. THE PIN: none of the three replies carries any key
	// that was sent. Asserting a round trip here would be asserting behaviour that does
	// not exist.
	for _, where := range []struct {
		what string
		keys []string
	}{
		{"the BEGIN SUCCESS", o.BeginMetaKeys},
		{"the terminal PULL SUCCESS", o.TerminalMetaKeys},
		{"the COMMIT SUCCESS", o.CommitMetaKeys},
	} {
		for _, sent := range o.SentKeys {
			if slices.Contains(where.keys, sent) {
				v = append(v, boltBeginViolation(ViolationOracleDeviation, "metadata-echoed-nowhere",
					fmt.Sprintf("%s carried key %q, which the client sent in tx_metadata. The server stores and "+
						"echoes no transaction metadata; if it now does, the pin and docs/bolt.md must be rewritten "+
						"together", where.what, sent)))
			}
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// The non-vacuity gate
// -----------------------------------------------------------------------------

// beginTimeoutArmNames is the timeout roster the gate checks completeness against.
var beginTimeoutArmNames = []string{
	"client-tx-timeout", "no-tx-timeout-control", "idle-bound-control", "overflow-tx-timeout",
}

// checkBoltBeginExtrasNonVacuity reports what the run FAILED TO CONSTRUCT, so a clause
// that could not have fired is a shortfall rather than a pass.
//
// It is separate from [checkBoltBeginExtras] because the two answer different questions:
// the contract asks whether the server misbehaved, this asks whether the run was in a
// position to notice. A coverage shortfall is a witness, not a verdict (rmp #2554), so
// every message here names what was not reached rather than accusing the server.
func checkBoltBeginExtrasNonVacuity(e *BoltBeginExtrasEvidence) []Violation {
	// slices.Concat for the same reason as [checkBoltBeginExtras]: one sizing, and nil
	// rather than an empty non-nil slice when the run reached everything it must.
	return slices.Concat(
		checkBoltBookmarkNonVacuity(e),
		checkBoltTimeoutNonVacuity(e),
		checkBoltModeDBNonVacuity(e),
		checkBoltRouteMetadataNonVacuity(e),
	)
}

// checkBoltBookmarkNonVacuity gates the causal-read and delivery clauses.
func checkBoltBookmarkNonVacuity(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	// CLAUSE nv-bookmark-roster. Every arm must have run, or a missing one silently
	// removes its clause.
	got := make([]string, 0, len(e.Bookmarks))
	for i := range e.Bookmarks {
		got = append(got, e.Bookmarks[i].Name)
	}
	slices.Sort(got)
	want := slices.Clone(beginBookmarkArmNames)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-roster",
			fmt.Sprintf("the causal-read roster ran %v, want %v: a missing arm removes its clause silently", got, want)))
	}
	// CLAUSE nv-bookmark-write-nonempty. A causal read of ZERO nodes could not
	// distinguish "the reader saw the write" from "the write never happened", so the
	// whole family would be satisfiable by an engine that stored nothing.
	if e.CommittedNodes <= 0 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-write-nonempty",
			fmt.Sprintf("the writer committed %d node(s): with nothing committed, every causal read trivially "+
				"observes the full committed set and the causality clause cannot fail", e.CommittedNodes)))
	}
	// CLAUSE nv-bookmark-contrast. The token-changes-nothing clause compares arms, so it
	// needs a real — that is, server-ISSUED — bookmark AND at least two fabricated ones.
	// With fewer there is nothing to contrast and the A/B/C is a single observation.
	// The counter is named for beginIssuedTag rather than "real" because `real` is a Go
	// builtin and shadowing it is a lint error (revive redefines-builtin-id).
	issued, fabricated, completed := 0, 0, 0
	for i := range e.Bookmarks {
		a := &e.Bookmarks[i]
		if a.Accepted && a.ReadFailed == "" {
			completed++
		}
		switch {
		case a.SentToken == beginIssuedTag:
			issued++
		case a.SentKey:
			fabricated++
		}
	}
	if issued < 1 || fabricated < 2 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-contrast",
			fmt.Sprintf("the family presented %d real bookmark(s) and %d fabricated one(s); it needs at least 1 and 2. "+
				"Without both, 'the token changed nothing' compares nothing and the causal read's meaning is "+
				"unestablished", issued, fabricated)))
	}
	// CLAUSE nv-bookmark-arms-completed. The comparison is only over arms that completed
	// their read; if fewer than two did, it compares a value with itself.
	if completed < 2 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-arms-completed",
			fmt.Sprintf("only %d arm(s) completed a causal read; the cross-arm comparison needs at least 2", completed)))
	}
	// CLAUSE nv-bookmark-sequence. A single bookmark cannot falsify a strict-advance
	// clause: one event is not a sequence.
	if len(e.IssuedBookmarks) < 2 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-sequence",
			fmt.Sprintf("the writer produced %d bookmark(s); the strict-advance clause needs at least 2 to compare",
				len(e.IssuedBookmarks))))
	}
	// CLAUSE nv-bookmark-stale-reference. The stale-autocommit clause is an equality
	// against an OBSERVED bookmark. Were that reference empty, the clause would collapse
	// into "both are empty" and pass on a session that never committed at all.
	if e.AutocommitAfterCommitExpected == "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-bookmark-stale-reference",
			"the reference bookmark for the stale-autocommit clause is EMPTY, so that clause degenerates into "+
				"comparing two empty strings and would pass whatever the server reported"))
	}
	return v
}

// checkBoltTimeoutNonVacuity gates the timeout clauses.
func checkBoltTimeoutNonVacuity(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	// CLAUSE nv-timeout-roster.
	got := make([]string, 0, len(e.Timeouts))
	for i := range e.Timeouts {
		got = append(got, e.Timeouts[i].Name)
	}
	if !slices.Equal(got, beginTimeoutArmNames) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-timeout-roster",
			fmt.Sprintf("the timeout roster ran %v, want %v in that order: an arm and its control are only comparable "+
				"as the same script", got, beginTimeoutArmNames)))
	}
	// CLAUSE nv-timeout-both-outcomes. The family needs a REAPED arm and a SURVIVING
	// one. With only one of the two, the instrument is one-sided: an oracle that only
	// ever sees reaps cannot tell a working bound from a reaper that fires on everything.
	reaped, survived := 0, 0
	for i := range e.Timeouts {
		if e.Timeouts[i].reaped() {
			reaped++
		} else {
			survived++
		}
	}
	if reaped < 1 || survived < 1 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-timeout-both-outcomes",
			fmt.Sprintf("the timeout family produced %d reaped and %d surviving arm(s); it needs at least one of "+
				"each. A family in which every arm is reaped cannot distinguish a bound that works from a reaper "+
				"that fires unconditionally", reaped, survived)))
	}
	// CLAUSE nv-timeout-reaper-armed. THE one that makes a NOT-reaped reading evidence.
	// A control that survived because the reaper was never armed proves nothing; only the
	// timer count separates "the reaper declined" from "there was no reaper".
	for i := range e.Timeouts {
		a := &e.Timeouts[i]
		if a.TimersArmed < 1 {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-timeout-reaper-armed",
				fmt.Sprintf("arm %q registered %d timer(s) on the injected clock. A surviving arm is only evidence "+
					"that the bound was not reached if the reaper was ARMED; with no timer, 'not reaped' means the "+
					"reaper was never installed and the control attributes nothing", a.Name, a.TimersArmed)))
		}
	}
	// CLAUSE nv-timeout-advance-nonzero. An advance of zero virtual time reaches no
	// deadline at all, so a surviving arm would survive for the wrong reason.
	for i := range e.Timeouts {
		a := &e.Timeouts[i]
		total := time.Duration(0)
		for _, d := range a.Advanced {
			total += d
		}
		if total <= 0 {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-timeout-advance-nonzero",
				fmt.Sprintf("arm %q advanced %v of virtual time in total: with no advance, no deadline is reachable "+
					"and the arm's outcome is independent of every bound it installed", a.Name, a.Advanced)))
		}
	}
	// CLAUSE nv-timeout-bounds-separated. The whole attribution of the client-bound arm
	// rests on its server bounds being FURTHER AWAY than the advance, so nothing else
	// can fire. If they are not, the arm measures the wrong reaper — the #2482 lesson.
	if a := beginTimeoutArm(e, "client-tx-timeout"); a != nil {
		reach := time.Duration(0)
		for _, d := range a.Advanced {
			reach += d
		}
		if a.IdleBound <= reach || a.DefaultTotalBound <= reach {
			v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-timeout-bounds-separated",
				fmt.Sprintf("arm %q advances %s of virtual time with the idle bound at %s and the server default "+
					"total bound at %s. Both must lie strictly BEYOND the advance, or the reap is attributable to a "+
					"server bound rather than to the client's tx_timeout and the arm measures the wrong reaper "+
					"(effectiveTxDeadline takes the earlier of the two, bolt/server/serve.go:1155-1167)",
					a.Name, reach, a.IdleBound, a.DefaultTotalBound)))
		}
	}
	return v
}

// checkBoltModeDBNonVacuity gates the mode and database clauses.
func checkBoltModeDBNonVacuity(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	// CLAUSE nv-mode-roster.
	gotModes := make([]string, 0, len(e.Modes))
	for i := range e.Modes {
		gotModes = append(gotModes, strings.TrimPrefix(e.Modes[i].Name, "mode="))
	}
	slices.Sort(gotModes)
	wantModes := slices.Clone(beginModeArmNames)
	slices.Sort(wantModes)
	if !slices.Equal(gotModes, wantModes) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-mode-roster",
			fmt.Sprintf("the mode roster ran %v, want %v", gotModes, wantModes)))
	}
	// CLAUSE nv-mode-both-sides. The family needs a transaction the server recorded as
	// READ-ONLY and one it recorded as WRITE. Without the read-only side the fail-open
	// clause is satisfiable by a server that never enforces anything; without the write
	// side, by one that refuses everything.
	readModes, writeModes := 0, 0
	for i := range e.Modes {
		switch e.Modes[i].RegistryMode {
		case beginModeRead:
			readModes++
		case beginModeWrite:
			writeModes++
		}
	}
	if readModes < 1 || writeModes < 1 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-mode-both-sides",
			fmt.Sprintf("the mode family produced %d read-only and %d write transaction(s) by the server's own "+
				"registry; it needs at least one of each, or the fail-open pin and the read-only refusal are each "+
				"one-sided", readModes, writeModes)))
	}
	// CLAUSE nv-mode-write-observed. The read-only refusal is only meaningful if the
	// same statement SUCCEEDS somewhere: a write refused everywhere would satisfy it for
	// the wrong reason.
	acceptedWrites := 0
	for i := range e.Modes {
		if e.Modes[i].WriteAccepted {
			acceptedWrites++
		}
	}
	if acceptedWrites < 1 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-mode-write-observed",
			"no mode arm's write was ACCEPTED, so the read-only refusal cannot be attributed to the access mode: "+
				"the same statement may simply be refused everywhere"))
	}
	// CLAUSE nv-db-roster.
	gotDBs := make([]string, 0, len(e.DBs))
	for i := range e.DBs {
		gotDBs = append(gotDBs, strings.TrimPrefix(e.DBs[i].Name, "db="))
	}
	slices.Sort(gotDBs)
	wantDBs := slices.Clone(beginDBArmNames)
	slices.Sort(wantDBs)
	if !slices.Equal(gotDBs, wantDBs) {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-db-roster",
			fmt.Sprintf("the database roster ran %v, want %v", gotDBs, wantDBs)))
	}
	// CLAUSE nv-db-contrast. The echo clause needs a name that is NOT this server's, and
	// the fallback clause needs an arm that sent none. Without the foreign name, "echoed"
	// and "fell back to its own name" are the same observation.
	foreign, absent := 0, 0
	for i := range e.DBs {
		a := &e.DBs[i]
		switch {
		case !a.SentKey:
			absent++
		case a.SentDB != server.DefaultDatabaseName:
			foreign++
		}
	}
	if foreign < 1 || absent < 1 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-db-contrast",
			fmt.Sprintf("the database family sent %d foreign name(s) and %d arm(s) with no name; it needs at least "+
				"one of each. With only the server's own name, an echo and a fallback are indistinguishable",
				foreign, absent)))
	}
	return v
}

// checkBoltRouteMetadataNonVacuity gates the ROUTE and tx_metadata clauses.
func checkBoltRouteMetadataNonVacuity(e *BoltBeginExtrasEvidence) []Violation {
	var v []Violation
	o := &e.Route
	// CLAUSE nv-route-reference. The address comparison is against the listener's own
	// view of itself. Were that empty, "every role advertises the listener's address"
	// would pass on a table full of empty strings.
	if o.ListenerAddr == "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-route-reference",
			"the listener reported an EMPTY address, so the routing-table address clause compares every advertised "+
				"address against \"\" and cannot fail"))
	}
	// CLAUSE nv-route-asked-for-a-db. The db-is-dropped pin only says something if the
	// ROUTE actually NAMED a database: against an empty request, "the table's db is
	// empty" is what an honest echo would also produce.
	if o.SentDB == "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-route-asked-for-a-db",
			"the ROUTE message named no database, so 'the table's db came back empty' is equally consistent with "+
				"the server ECHOING the request and with it dropping the field"))
	}
	// CLAUSE nv-route-table-decoded. A table that did not decode leaves every structured
	// field at its zero value, and several clauses would then pass on nothing.
	if o.RoleCount == 0 || len(o.AddressesByRole) == 0 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-route-table-decoded",
			fmt.Sprintf("the routing table decoded to %d entr(y/ies) and %d role(s): with nothing decoded the role, "+
				"address and ttl clauses have no values to adjudicate", o.RoleCount, len(o.AddressesByRole))))
	}
	// CLAUSE nv-route-renders-nonempty. The identical-answer clause compares two
	// renderings; two empty strings are equal and would pass on two broken replies.
	if o.PopulatedRender == "" || o.ZeroMessageRender == "" {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-route-renders-nonempty",
			fmt.Sprintf("a routing-table rendering is empty (populated=%q, zero=%q), so the identical-answer clause "+
				"compares two empty strings", o.PopulatedRender, o.ZeroMessageRender)))
	}
	// CLAUSE nv-metadata-sent. "No reply echoes any key the client sent" is vacuously
	// true when the client sent none.
	if len(e.Metadata.SentKeys) == 0 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-metadata-sent",
			"the tx_metadata arm sent NO keys, so 'no reply echoed one' is vacuously true"))
	}
	// CLAUSE nv-metadata-replies-read. The echo clause searches three key lists; if the
	// replies were never decoded, all three are empty and the search finds nothing
	// whatever the server did.
	if len(e.Metadata.TerminalMetaKeys) == 0 || len(e.Metadata.CommitMetaKeys) == 0 {
		v = append(v, boltBeginViolation(ViolationOracleDeviation, "nv-metadata-replies-read",
			fmt.Sprintf("the tx_metadata arm read %d terminal and %d commit metadata key(s); both replies carry keys "+
				"by contract (has_more/bookmark/db and bookmark), so an empty list means the reply was not decoded "+
				"and the echo search ran over nothing",
				len(e.Metadata.TerminalMetaKeys), len(e.Metadata.CommitMetaKeys))))
	}
	return v
}

// -----------------------------------------------------------------------------
// The rendering
// -----------------------------------------------------------------------------

// String renders the evidence for a report. Two runs of the same seed must produce
// BYTE-IDENTICAL output, which is asserted directly, so the whole rendering is swept
// for anything not reachable from the seed rather than each such field being handled as
// it is noticed (rmp #2483's lesson). Three classes are excluded:
//
//   - REAL-TIME durations. BoltBookmarkArm.BeginElapsed and BoltTimeoutArm.ReplyElapsed
//     are the instruments for "does not wait" and "aborts rather than stalls"; they
//     belong in a clause with a generous bound, never in a byte comparison.
//   - PROCESS-GLOBAL bookmark text. bookmarkCounter is global to the process
//     (bolt/server/bookmark.go:13), so an issued bookmark's literal value depends on
//     how many transactions every other test committed first — and so does the
//     ADVANCE between two of them, which is why renderIssued shows neither. Issued
//     bookmarks are rendered POSITIONALLY, and the stale-autocommit reading as the
//     RELATION it is adjudicated on rather than as its text.
//   - Harness error TEXT. BoltBookmarkArm.ReadFailed can carry a transport message
//     whose wording is not seed-determined, so its PRESENCE is rendered and its text is
//     left to the violation that reports it.
func (e *BoltBeginExtrasEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-begin-extras evidence (seed %d):", e.Seed)
	fmt.Fprintf(&b, "\n  writer: %d node(s) under :%s committed in %d transaction(s)",
		e.CommittedNodes, beginCausalLabel, len(e.IssuedBookmarks))
	fmt.Fprintf(&b, "\n  bookmarks issued: %s", e.renderIssued())
	fmt.Fprintf(&b, "\n  autocommit bookmark: fresh=%s after-commit=%s reported-update=%v",
		beginRenderBookmarkSlot(e.AutocommitBookmarkFresh),
		beginRenderStaleRelation(e.AutocommitBookmarkAfterCommit, e.AutocommitAfterCommitExpected),
		e.AutocommitStatsSeen)
	b.WriteString("\n  causal reads (one connection each, seed-drawn order):")
	for i := range e.Bookmarks {
		a := &e.Bookmarks[i]
		fmt.Fprintf(&b, "\n    %-24s sent=%-24s server-saw=%d accepted=%-5v observed=%d/%d read-failed=%v",
			a.Name, beginRenderSent(a), a.ServerSawTokens, a.Accepted, a.Observed, e.CommittedNodes,
			a.ReadFailed != "")
		if a.GotCode != "" {
			fmt.Fprintf(&b, "\n      refused: %q / %q", a.GotCode, a.GotMessage)
		}
	}
	b.WriteString("\n  tx_timeout (fixed order; each arm on its own fake clock):")
	for i := range e.Timeouts {
		a := &e.Timeouts[i]
		fmt.Fprintf(&b, "\n    %-22s sent=%-12s idle=%-8s default=%-8s advances=%v reaped-after=%d timers=%d committed=%v",
			a.Name, beginRenderTimeoutSent(a), a.IdleBound, a.DefaultTotalBound, a.Advanced,
			a.ReapedAfter, a.TimersArmed, a.Committed)
		if a.GotCode != "" {
			fmt.Fprintf(&b, "\n      told: %q / %q; second message: %s", a.GotCode, a.GotMessage, a.SecondReplyKind)
		}
	}
	b.WriteString("\n  mode (seed-drawn order):")
	for i := range e.Modes {
		a := &e.Modes[i]
		fmt.Fprintf(&b, "\n    %-14s sent=%-9s registry-mode=%-3q write-accepted=%v",
			a.Name, beginRenderPresence(a.SentKey, a.SentMode), a.RegistryMode, a.WriteAccepted)
		if a.WriteCode != "" {
			fmt.Fprintf(&b, "\n      write refused: %q / %q", a.WriteCode, a.WriteMessage)
		}
	}
	b.WriteString("\n  db (seed-drawn order):")
	for i := range e.DBs {
		a := &e.DBs[i]
		fmt.Fprintf(&b, "\n    %-22s sent=%-18s run=%-18q terminal=%-18q commit-keys=%v",
			a.Name, beginRenderPresence(a.SentKey, a.SentDB), a.ReportedOnRun, a.ReportedDB, a.CommitMetaKeys)
	}
	fmt.Fprintf(&b, "\n  route: listener=%q accepted=%v roles=%v entries=%d ttl=%d(present=%v) table-db=%q",
		e.Route.ListenerAddr, e.Route.Accepted, e.Route.Roles, e.Route.RoleCount,
		e.Route.TTL, e.Route.TTLPresent, e.Route.TableDB)
	fmt.Fprintf(&b, "\n    asked for db=%q bookmark=%q; populated==zero: %v",
		e.Route.SentDB, e.Route.SentBookmark, e.Route.PopulatedRender == e.Route.ZeroMessageRender)
	fmt.Fprintf(&b, "\n    table: %s", e.Route.PopulatedRender)
	fmt.Fprintf(&b, "\n  tx_metadata: sent=%v accepted=%v begin=%v terminal=%v commit=%v",
		e.Metadata.SentKeys, e.Metadata.Accepted, e.Metadata.BeginMetaKeys,
		e.Metadata.TerminalMetaKeys, e.Metadata.CommitMetaKeys)
	return b.String()
}

// renderIssued renders the writer's bookmarks POSITIONALLY: each slot's ordinal and
// whether it held a well-formed bookmark, and nothing else.
//
// The numeric ADVANCE between consecutive counters is deliberately not rendered, even
// though it looks more informative. bookmarkCounter is global to the process
// (bolt/server/bookmark.go:13), so "two COMMITs by this writer advance it by exactly
// one" holds only under the qualifier "with nothing else committing in between" — and
// the swarm breaks that qualifier by construction: `sim -swarm -workers N` runs N
// scenarios concurrently in ONE process (internal/sim/swarm.go:271-278). MEASURED over
// six fixed seeds, the advance read +1 at workers=1 and +5, +6 or +7 at workers=6, so
// a rendering carrying it could not be asserted byte-identical and
// [TestBoltBeginExtras_Deterministic] was green only because it runs serially.
//
// The property is not lost, only relocated to where it survives an interleaving: the
// bookmark-strictly-advances clause adjudicates the RELATION n > prev between two
// OBSERVED counters, which holds however many other transactions commit in between.
func (e *BoltBeginExtrasEvidence) renderIssued() string {
	if len(e.IssuedBookmarks) == 0 {
		return "<none>"
	}
	var b strings.Builder
	for i, bm := range e.IssuedBookmarks {
		if i > 0 {
			b.WriteString(" ")
		}
		if _, ok := beginBookmarkCounter(bm); !ok {
			fmt.Fprintf(&b, "#%d=<malformed>", i)
			continue
		}
		fmt.Fprintf(&b, "#%d=<issued>", i)
	}
	return b.String()
}

// beginRenderSent renders what a causal-read arm put on the wire.
func beginRenderSent(a *BoltBookmarkArm) string {
	if !a.SentKey {
		return "<no bookmarks key>"
	}
	return a.SentToken
}

// beginRenderBookmarkSlot renders a bookmark reading whose literal value is not
// seed-reachable: the empty string is shown as such, anything else positionally.
func beginRenderBookmarkSlot(bm string) string {
	if bm == "" {
		return "<empty>"
	}
	return beginIssuedTag
}

// beginRenderStaleRelation renders the stale-autocommit reading as the RELATION the
// clause adjudicates, never as either bookmark's text.
func beginRenderStaleRelation(got, want string) string {
	switch {
	case got == "" && want == "":
		return "<both empty>"
	case got == want:
		return "<equals the prior COMMIT's>"
	default:
		return "<DIFFERS from the prior COMMIT's>"
	}
}

// beginRenderTimeoutSent renders a timeout arm's `tx_timeout`, distinguishing an absent
// key from a sent value.
func beginRenderTimeoutSent(a *BoltTimeoutArm) string {
	if !a.SentKey {
		return "<absent>"
	}
	return strconv.FormatInt(a.SentTimeoutMS, 10) + "ms"
}

// beginRenderPresence renders an extras value, distinguishing an absent key from a
// present one, including a present EMPTY value. That distinction is in the RENDERING
// only. The server collapses the two for `db` exactly as it does for `mode`:
// selectDatabaseFrom DISCARDS extractString's found-bool
// (bolt/server/session.go:322-324), so a present "" and an absent key both leave
// clientDatabase empty and databaseName falls back identically (:309-317) — as the
// server's own comment at :319-321 says. No arm here sends an empty value either
// way.
func beginRenderPresence(present bool, value string) string {
	if !present {
		return "<absent>"
	}
	return strconv.Quote(value)
}

// -----------------------------------------------------------------------------
// Catalogue wiring
// -----------------------------------------------------------------------------

// boltBeginExtrasDefaultSeed is the catalogue default for [ScenarioBoltBeginExtras].
const boltBeginExtrasDefaultSeed = 0x2485_B00C

// boltBeginExtrasScenario drives the deterministic BEGIN extras battery.
func boltBeginExtrasScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltBeginExtras,
		Description: "Bolt BEGIN extras: a causal read across connections must observe a committed write AND must " +
			"observe it identically whether it presents the writer's real bookmark, a bookmark this server never " +
			"issued, or none — which is what proves the token is ignored rather than honoured; a client tx_timeout " +
			"must abort the transaction with the exact typed code while the same script without the key survives " +
			"the identical advance; only the exact mode \"r\" may select read-only; a foreign db name must be " +
			"echoed and never reported empty; tx_metadata must be accepted and echoed nowhere; and ROUTE must " +
			"advertise this server's own listener address under all three roles",
		Mode:        ModeDeterministic,
		DefaultSeed: boltBeginExtrasDefaultSeed,
		run:         runBoltBeginExtrasScenario,
	}
}

// runBoltBeginExtrasScenario collects the evidence and adjudicates it against both the
// contract and the non-vacuity gate.
func runBoltBeginExtrasScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltBeginExtras(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltBeginExtras(ev), checkBoltBeginExtrasNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return &SimReport{
		Scenario:   ScenarioBoltBeginExtras,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt BEGIN extras>"},
		Violations: v,
	}, nil
}
