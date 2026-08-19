package sim

// bolt_decode_pressure.go — the Bolt inbound-decode memory surface under
// simulation (rmp #2487).
//
// # The gap this closes
//
// The harness had exactly one arm anywhere near inbound-memory abuse: the
// BoltAbuser's oversized frame, which drives the PER-MESSAGE framing cap
// (proto.ErrMessageTooLarge) on ONE connection. Three bounds that matter more
// were driven by nothing at all:
//
//   - the ENGINE-WIDE inbound-decode budget (packstream.InboundBudget,
//     bolt/packstream/inbound_budget.go), the pool every connection's decoder and
//     reassembly reader draw on. It is created ONCE PER SERVER
//     (bolt/server/serve.go:654, NewInboundBudget(resolveMaxInboundDecodeBytes(...)))
//     and that single pointer is what makes it the CROSS-CONNECTION vector: the
//     per-message cap times the connection limit is unbounded and
//     pre-authentication-reachable, which is the CWE-770 the pool exists to close;
//   - the WIRE nesting cap (packstream maxValueDepth = 128, value.go:21), whose
//     breach is a hard security boundary — a crafted message can otherwise request
//     millions of stack frames and kill the process, and it is reachable during the
//     first HELLO decode, before any authentication;
//   - the ENGINE'S OWN parameter nesting cap (cypher maxParamBindDepth = 32,
//     cypher/api.go:4257), which is a SECOND, LOWER, INDEPENDENT cap on the same
//     axis and which this task discovered by measurement rather than by reading.
//
// # The sharpest thing here: three abuse vectors, three DIFFERENT answers
//
// A server that answered all three the same way would be indistinguishable from
// one with no aggregate pool at all. The three answers were MEASURED against the
// live server, not inferred:
//
//	vector                         code                                          session afterwards
//	aggregate pool breach          Neo.TransientError.General.OutOfMemoryError    READY   (usable with NO RESET)
//	wire nesting cap (>=128)       Neo.ClientError.Request.Invalid                READY   (usable with NO RESET)
//	engine param cap (>32)         Neo.DatabaseError.General.UnknownError         FAILED  (next message IGNORED)
//
// The classification segment is load-bearing and is asserted on its own:
// neo4j-go-driver's IsRetriableTransient tests `classification == "TransientError"`
// (recorded at bolt/server/errors.go:130-131), so "typed retryable backpressure"
// is a checkable property of the code's SECOND segment and not a vibe. The pool
// breach is the only one of the three that a driver will retry, which is exactly
// right: it is the only one where retrying can succeed.
//
// The first two are answered ABOVE the session state machine — the serve loop
// rejects them between the read and sess.HandleMessage (serve.go:1258 and :1289) —
// which is why the session survives them intact. The third travels through the
// state machine into cypher.BindParams, so it FAILS the session. That
// state-after difference is a third discriminator, independent of the codes, and
// it is asserted too.
//
// # Why the aggregate arm needs concurrency, and the deterministic arm cannot fake it
//
// Every charge against the pool is released before the reply is written: the
// reassembly reader releases on every return path from ReadMessage
// (bolt/proto/chunking.go:160-165) and the decoder's hold is released by the
// deferred ReleaseInboundBudget in decodeRequestBudgeted (serve.go:1419-1423). A
// single-threaded lock-step script therefore can NEVER observe two charges
// outstanding at once, whatever it sends. The cross-connection vector is
// inherently concurrent, so it lives in [RunBoltDecodeSwarm]; the deterministic
// scenario pins the pool's CONTRACT — its arithmetic, its typed answer, its
// atomicity and its restoration — one connection at a time.
//
// # The load-bearing oracle: a closed-form model of the pool, not the server's word
//
// The harness re-derives, independently, how many bytes a RUN's decode holds from
// the shared pool, from the published per-slot costs:
//
//	held(query, key, N) = 32 + 3*48    (the RUN struct: container + 3 fields)
//	                    + 512 + 112    (the 1-entry parameters map)
//	                    + 32           (the parameter list's container)
//	                    + 512          (the empty extra map)
//	                    + len(query)   (String payloads are charged 1:1, decoder.go:712)
//	                    + len(key)     (so is a map key)
//	                    + 48*N         (one 48-byte slot per list element)
//	                    = 1344 + len(query) + len(key) + 48*N
//
// and the pool accepts the message iff held <= the ceiling. The constants are
// packstream's collectionCost (32), listElemCost (48), mapEntryCost (112) and
// mapCollectionCost (512) (bolt/packstream/decoder.go:101-140), re-stated here
// rather than imported, because a copy that must AGREE is an oracle and an import
// that must agree with itself is not.
//
// The model was calibrated against the real decoder by binary search on the
// smallest budget that admits a payload — held was 48N + 1353 for every N tried
// with an 8-byte query and a 1-byte key, exactly as the closed form says — and
// then confirmed end to end at three ceilings (3, 4 and 8 MiB), where it named the
// last accepted element count EXACTLY in all three. The scenario does not trust
// that: it SCANS a small window around the prediction and requires the measured
// boundary to be one element wide, monotone, and equal to the model's. That makes
// the clause a tripwire on the five packstream cost constants — if one changes,
// this scenario names the divergence instead of quietly drifting.
//
// A one-element-wide boundary is also the strongest available refutation of "the
// per-message cap did it": the last accepted and the first refused message differ
// by 48 charged bytes out of ~4 MiB, both are ~2 orders of magnitude under the
// 16 MiB message cap and ~32x under the 128 MiB per-message decoded-collection
// cap, and the CONTROL — the identical bytes against a server whose ONLY
// difference is a larger ceiling — is served.
//
// # Seed-purity
//
// Nothing rendered is process-global. Node ids are never rendered (a created
// node's hidden key is minted from a process-global counter; see
// bolt_stream_semantics.go), the census renders counts by label, and the engine's
// internal-error message carries a per-session id which is REDACTED before it is
// recorded ([boltDecodeRedactSession]) so the rendering stays a function of the
// seed. Elapsed times bound liveness but are never rendered.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// -----------------------------------------------------------------------------
// The contract's constants
// -----------------------------------------------------------------------------

// The three Bolt failure codes this surface must produce, spelled out so the
// oracle pins the EXACT code a driver sees. They must stay pairwise distinct:
// collapsing any two would make the corresponding abuse vectors
// indistinguishable to a client, which is the defect
// [checkBoltDecodeCapsAnswerDifferently] exists to catch.
const (
	// boltDecodeCodeBudget is what the ENGINE-WIDE inbound-decode pool returns
	// (bolt/server/serve.go:1266-1268). TransientError is the classification a driver
	// retries.
	boltDecodeCodeBudget = "Neo.TransientError.General.OutOfMemoryError"
	// boltDecodeCodeInvalid is what any decode fault returns, the WIRE nesting cap
	// among them (serve.go:1289).
	boltDecodeCodeInvalid = "Neo.ClientError.Request.Invalid"
	// boltDecodeCodeInternal is what the ENGINE'S parameter nesting cap returns,
	// via the failure sanitiser. It is a DatabaseError, so a driver does not retry
	// it — correct, since retrying an over-nested parameter fails identically —
	// though the code says "server bug" about what is a client fault. See the
	// scenario's report note.
	boltDecodeCodeInternal = "Neo.DatabaseError.General.UnknownError"
)

// The exact message texts the two decode-layer refusals carry. A code alone is
// not enough: the generic Request.Invalid is shared with every other malformed
// message, so the pair (code, message) is what a client actually reads.
const (
	boltDecodeMsgBudget  = "server inbound decode memory limit reached; retry later"
	boltDecodeMsgInvalid = "malformed Bolt message"
	// boltDecodeMsgInternalPrefix is the STABLE prefix of the sanitised internal
	// error; the remainder names a per-session id and is redacted before recording.
	boltDecodeMsgInternalPrefix = "An internal error occurred."
)

// boltDecodeRetriableClassification is the second dot-segment a Bolt status code
// must carry for neo4j-go-driver to retry it: IsRetriableTransient tests
// `classification == "TransientError"` (github.com/neo4j/neo4j-go-driver/v5
// v5.28.4, neo4j/db/errors.go — the same reading recorded at
// bolt/server/errors.go:130-131). Asserting the segment, and not only the whole
// code string, is what makes "typed RETRYABLE backpressure" a checked property
// rather than a restatement of the literal above.
const boltDecodeRetriableClassification = "TransientError"

// The harness's independent model of what one decode holds from the shared pool.
// These re-state packstream's per-slot costs (bolt/packstream/decoder.go:101-140);
// see the file comment for why they are copied rather than imported.
const (
	boltDecodeCollectionCost    = 32  // a List or Struct container
	boltDecodeListElemCost      = 48  // one List element or Struct field
	boltDecodeMapEntryCost      = 112 // one map entry
	boltDecodeMapCollectionCost = 512 // a map container
)

// boltDecodeRunFixedCost is what a RUN whose parameters are a single named list
// charges BEFORE the list's elements and before any string payload: the RUN
// struct (container + 3 fields), the one-entry parameters map, the parameter
// list's own container, and the empty extra map.
const boltDecodeRunFixedCost = boltDecodeCollectionCost + 3*boltDecodeListElemCost + // 176, the RUN struct
	boltDecodeMapCollectionCost + boltDecodeMapEntryCost + // 624, the parameters map
	boltDecodeCollectionCost + // 32, the parameter list container
	boltDecodeMapCollectionCost // 512, the empty extra map

// The two nesting caps, re-stated from the code that enforces them so a change to
// either fails this scenario by name instead of silently moving the boundary the
// arms drive.
const (
	// boltDecodeWireDepthCap is packstream's maxValueDepth (bolt/packstream/value.go:21):
	// readValue refuses at ENTRY when depth > cap, and the params map is read at
	// depth 0, so a chain of k composites under a parameter key puts its innermost
	// value at depth k+1 and the refusal begins at k == cap.
	boltDecodeWireDepthCap = 128
	// boltDecodeParamDepthCap is cypher's maxParamBindDepth (cypher/api.go:4257),
	// reached only by a message the decoder already ACCEPTED.
	boltDecodeParamDepthCap = 32
)

// boltDecodeNestingWireCeiling is the size every nesting payload must stay under,
// in bytes. It is the anti-confound bound: a message rejected for being large
// proves nothing about a DEPTH cap, so the family is required to be small enough
// that neither the 16 MiB framing cap (proto.DefaultMaxMessageBytes) nor the
// 128 MiB per-message decoded-collection cap can be what fired. 64 KiB is 256x
// under the first and 2048x under the second.
const boltDecodeNestingWireCeiling = 64 << 10

// The pressured server's ceiling and its control. The control differs in the
// ceiling and in NOTHING else, so a payload the first refuses and the second
// serves isolates the pool as the cause.
const (
	// boltDecodePressuredBudget is small enough that one ~4 MiB decode fills it,
	// and large enough that the 1 MiB reservation granularity
	// (packstream inboundReserveChunk) is not itself the bound.
	boltDecodePressuredBudget int64 = 4 << 20
	// boltDecodeControlBudget is the raised ceiling of the control server: 16x the
	// pressured one, so every payload this scenario sends fits with room to spare.
	boltDecodeControlBudget int64 = 64 << 20
)

// The swarm's shape. The per-message charge is sized so ONE abuser's decode fits
// the pool and TWO do not, which is what makes the pressure genuinely AGGREGATE
// rather than a per-message ceiling wearing a different name.
const (
	// boltDecodeSwarmBudget is the shared ceiling the whole fleet draws on.
	boltDecodeSwarmBudget int64 = 8 << 20
	// boltDecodeSwarmChargeNum/Den set each abusive message's charge as a fraction
	// of the pool: 55%, so 1 fits (0.55 < 1) and 2 cannot (1.10 > 1). The margin
	// on both sides is 5% of the pool — ~420 KiB — which is far more than the
	// ~1 MiB-granular reservation noise can move either way.
	boltDecodeSwarmChargeNum = 55
	boltDecodeSwarmChargeDen = 100
	// boltDecodeSwarmAbusers is how many connections push concurrently. Four is
	// enough that at most one can hold the big charge while the other three are
	// refused, and few enough that the pool's FLOOR stays comfortably above what
	// the honest client and the reassembly reader need.
	//
	// That floor is what keeps a liveness oracle from becoming a flake, because a
	// reassembly-layer breach is NOT the typed, connection-preserving refusal the
	// decode-layer breach is: ChunkedReader.ReadMessage returns
	// packstream.ErrInboundBudgetExceeded as a READ error
	// (bolt/proto/chunking.go:223-227) and the serve loop tears the connection
	// down on every read error (serve.go:1237-1247).
	//
	// The arithmetic: at most one abuser holds the big charge (0.55 of the pool),
	// leaving 0.45. Each refused abuser transiently holds at most one 1 MiB
	// reservation chunk before its deferred release — three of those is 3 MiB
	// against a MEASURED 3.6 MiB residual (a 4.4 MiB charge out of an 8 MiB pool),
	// so the worst floor is ~0.6 MiB. The honest
	// client needs ~100 bytes of reassembly and a few hundred bytes of decode (the
	// decoder falls back to the exact remainder when a full chunk will not fit,
	// decoder.go:336-341), and each abuser's own reassembly needs at most one
	// 64 KiB chunk. Half a megabyte of floor covers all of it with three orders of
	// magnitude to spare, and swarm-no-transport-loss is the clause that reports it
	// if the sizing ever stops holding.
	boltDecodeSwarmAbusers = 4
	// boltDecodeSwarmMinRounds is the fewest messages each abuser sends. It is the
	// floor that guarantees pressure even if the honest client finishes instantly.
	boltDecodeSwarmMinRounds = 6
	// boltDecodeSwarmMaxRounds is the most, a safety bound so a honest client that
	// died cannot leave the abusers pushing for ever.
	boltDecodeSwarmMaxRounds = 400
	// boltDecodeSwarmHonestOps is how many honest exchanges run against the same
	// server while the abusers push.
	boltDecodeSwarmHonestOps = 24
)

// The pressure window is CONSTRUCTED to contain the honest window, rather than
// the two being started together and the overlap left to the scheduler.
//
// The first attempt did start them together, and it MEASURED the cost of that:
// with each side running a fixed count, the abusers finished in ~38 ms while the
// honest client was still pausing between exchanges, and exactly ONE of 24 honest
// exchanges straddled a refusal. A non-vacuity clause satisfied by a single
// sample is a clause one scheduling decision away from failing for no reason —
// and, worse, one scheduling decision away from PASSING while the run showed
// honest traffic working before and after the pressure rather than during it.
//
// So the two ends are pinned instead. The honest client waits for the pressure to
// have demonstrably started (a refusal has been counted) before its first
// exchange, and the abusers keep pushing until the honest client has finished. The
// honest window then lies strictly inside the pressure window by construction, and
// the overlap count becomes a property of the design rather than of the run.
const (
	// boltDecodeSwarmStartBarrier bounds the honest client's wait for the first
	// refusal. Expiring it is not fatal — the honest exchanges still run, so every
	// other clause keeps its evidence — and the shortfall surfaces as
	// nv-swarm-rejections, which is the honest report of pressure that never built.
	boltDecodeSwarmStartBarrier = 10 * time.Second
	// boltDecodeSwarmBarrierStep is how often the barrier samples the counter.
	boltDecodeSwarmBarrierStep = 100 * time.Microsecond
)

// Even inside the pressure window, whether a given honest exchange STRADDLES a
// refusal is a race, and pinning the window's ends does not fix that. Measured
// under -race at the catalogue seed: a narrow honest exchange takes 633 us at the
// median (357 us to 902 us over 20 samples) while refusals arrive about once per
// 3.8 ms of honest in-flight time, so most exchanges land in a gap. With the
// window pinned and nothing else changed: 9 straddles out of 24.
//
// So the WIDTH is controlled too. Every [boltDecodeSwarmWideEvery]-th honest
// exchange is a WIDE one: it sends its RUN, holds the open stream for
// [boltDecodeSwarmWideHold] without pulling, and only then drains. The exchange is
// genuinely in flight for that whole time — the server has acknowledged the
// statement and is holding a cursor for it — so a wide exchange's overlap is a
// property of the design rather than of the run.
//
// The hold is NOT set by waiting for a refusal to be counted. That would make
// the clause true by construction of the HARNESS instead of by behaviour of the
// SERVER, which is the failure mode a coverage gate exists to prevent. It is a
// fixed duration, chosen against a measured rate, and the run REPORTS how many
// refusals the narrowest wide window actually contained so a margin that has
// quietly eroded is visible before it starts failing.
const (
	boltDecodeSwarmWideEvery = 6
	boltDecodeSwarmWideHold  = 50 * time.Millisecond
)

// boltDecodeHonestBound is the liveness bound on ONE honest exchange under
// aggregate pressure.
//
// A wall clock is the right oracle here and it is the exception, not the rule.
// rmp #2567 removed deadlines that were being used as oracles over BOUNDED
// payloads, where a deadline can only misattribute a slow machine. This wait is
// not bounded by anything: a server that starved honest traffic under pressure
// would never serve it at all, so the honest client would wait forever and only a
// clock can tell. The bound is therefore set generously against a MEASUREMENT
// rather than a guess: with the fleet pushing under -race, an honest exchange took
// 633 us at the median and 902 us at its worst over 20 samples, so 30 s is about
// 33,000x the worst observed service time and a saturated machine cannot reach it.
// The clause that fires says STARVED rather than "slow". It is paired with an assertion on what
// was actually SERVED: every honest exchange must return the value the harness
// computed for it, which is the claim that matters and which no clock is involved
// in.
const boltDecodeHonestBound = 30 * time.Second

// boltDecodeArmBound is the harness's own outer bound on a single deterministic
// arm's round trip. It is a harness error, never a violation: a reply that never
// comes is a stall, and reporting a stall as a passing scenario is the failure
// mode this exists to prevent.
const boltDecodeArmBound = 60 * time.Second

// boltDecodeBoundaryWindow is how many element counts either side of the model's
// prediction the boundary scan probes. Three is enough to show the transition is
// one element wide with two accepted below it and two refused above, and small
// enough that the scan costs seven ~4 MiB decodes (measured at ~30 ms each under
// -race).
const boltDecodeBoundaryWindow = 3

// boltDecodeLeakSensitivity is the largest pool leak the final probe could miss,
// in bytes. The probe sends the message at the measured boundary, whose slack
// against the ceiling is under one element's charge; the gate requires the
// measured slack to be under this, so a probe that had gone slack — and could
// therefore no longer detect a leak — is reported rather than trusted.
const boltDecodeLeakSensitivity = boltDecodeListElemCost

// The labels the write arms use. They are distinct so a census attributes a node
// to the arm that created it by label alone.
const (
	// boltDecodeFitLabel is written by the large-parameter message the pool
	// ACCEPTS. It must exist, live and after recovery: an accepted message has its
	// full effect.
	boltDecodeFitLabel = "DecodePressureFit"
	// boltDecodeBreachLabel is written by the message the pool REFUSES. Its query
	// text is the same shape, so it WOULD have created a node had it executed; it
	// must exist NOWHERE, live or after recovery. A refused decode is atomic in the
	// strongest sense — nothing was parsed, so nothing could run.
	//
	// The CONTROL server replays the identical bytes, so it writes this same label
	// — into its OWN engine. Giving the control a label of its own would have meant
	// sending different bytes, and a control that changed the payload controls for
	// nothing; two engines censused separately is what keeps the payload identical
	// while the counts stay attributable.
	boltDecodeBreachLabel = "DecodePressureBreach"
)

// boltDecodeParamKey is the single parameter key every modelled message binds.
// One byte, because a map key's bytes are charged 1:1 against the pool and a
// one-byte key keeps the model's arithmetic legible.
const boltDecodeParamKey = "d"

// boltDecodeSeedMix / boltDecodeSwarmSeedMix decorrelate each scenario's SimDisk
// (or draw) sub-seed from the scenario seed. Each must DIFFER from its scenario's
// default seed: XOR is self-annihilating, so a mix equal to the default would
// make the one run every report starts from draw from NewSeed(0). The guard is
// TestBoltDecodePressure_SeedMixDoesNotCancelTheDefaultSeed.
const (
	boltDecodeSeedMix      = 0x2487_5EED
	boltDecodeSwarmSeedMix = 0x2487_5EA5
)

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// BoltDecodeProbe is one modelled message and the pool's verdict on it. It is the
// unit the boundary scan and the leak probes are made of.
type BoltDecodeProbe struct {
	// Code is the failure code when the pool refused, else "".
	Code string
	// Elements is the parameter list's element count, the only thing that varies
	// across a scan.
	Elements int
	// ModelHeld is what [boltDecodeModelHeld] says this message's decode holds from
	// the shared pool at its peak. It is computed by the harness, never read from
	// the server.
	ModelHeld int64
	// Slack is Budget - ModelHeld: positive when the model says the message fits.
	// It is what makes the leak probe's SENSITIVITY explicit.
	Slack int64
	// Accepted is whether the server decoded and ran the message.
	Accepted bool
}

// BoltDecodeExchange is one named arm: a message sent, the reply it drew, and
// whether the connection and the session survived it.
type BoltDecodeExchange struct {
	// Name is the arm's stable name; it is what a violation cites.
	Name string
	// Vehicle is the message kind the abuse rode in on: "run" or "hello". The
	// HELLO arms matter on their own because the wire nesting cap is reachable
	// during the FIRST HELLO decode, before any authentication.
	Vehicle string
	// Composite is the nesting arm's chain kind — "list" or "map" — or "" for the
	// pool arms. Both kinds must hit the same cap: readValue's bound is on
	// composite depth, not on lists.
	Composite string
	// Reply is the terminal reply kind: "SUCCESS", "FAILURE", "IGNORED", or
	// "RECORD+SUCCESS" when records preceded it.
	Reply string
	// Code and Message are the FAILURE's, with the per-session id redacted
	// ([boltDecodeRedactSession]) so the rendering stays a function of the seed.
	Code    string
	Message string
	// Depth is the nesting chain's length for a nesting arm, 0 otherwise.
	Depth int
	// Elements is the parameter list's element count for a pool arm, -1 otherwise.
	Elements int
	// WireBytes is the message's size on the wire. For the nesting family it is the
	// evidence that SIZE cannot be what refused it.
	WireBytes int
	// ModelHeld is the harness's model of the pool hold, for pool arms only.
	ModelHeld int64
	// SessionUsable is whether the SAME connection served a follow-up exchange
	// with NO RESET in between. It is the third discriminator between the caps:
	// the two decode-layer refusals happen above the session state machine and
	// leave it READY; the engine's parameter cap travels through it and FAILS it.
	SessionUsable bool
	// FollowUpValue is what the follow-up RUN returned, or -1 when it was not
	// served. The harness picks the expected value, so a connection that answered
	// with someone else's row is a failure and not a pass.
	FollowUpValue int64
}

// BoltDecodeHonest is one honest exchange run against the server while the swarm
// pushes. It carries its own overlap evidence.
type BoltDecodeHonest struct {
	// Index identifies the exchange and IS the value its query must return, so the
	// oracle is arithmetic rather than the server's own claim.
	Index int
	// RejectionsBefore/RejectionsAfter sample the fleet-wide rejection counter
	// immediately before the request is written and immediately after the reply is
	// read. After > Before means abusive traffic was REJECTED WHILE this exchange
	// was in flight: a clock-free, interleaving-invariant proof that honest service
	// OVERLAPPED the pressure window rather than merely preceding or following it.
	RejectionsBefore int64
	RejectionsAfter  int64
	// Value is what the exchange returned, or -1 when it returned nothing.
	Value int64
	// Elapsed bounds liveness. It is deliberately NOT rendered: it is a property of
	// the machine, not of the seed.
	Elapsed time.Duration
	// OK is whether the exchange completed and returned exactly one record.
	OK bool
	// Wide marks an exchange that deliberately held its stream open for
	// [boltDecodeSwarmWideHold] between RUN and PULL, so its in-flight window is
	// wide enough to contain refusals by construction rather than by luck.
	Wide bool
}

// BoltDecodeEvidence is everything one run observed.
type BoltDecodeEvidence struct {
	// Census counts are read over the wire (live) and straight off a store reopened
	// through real recovery. Keyed by label; ids are never recorded, because a
	// created node's hidden key is minted from a process-global counter.
	LiveCensus      map[string]int
	RecoveredCensus map[string]int
	// ControlCensus is the same census on the CONTROL server's own engine, where
	// the bytes the pressured pool refused were served. Two engines, one payload:
	// the count differing between them IS the arm's claim.
	ControlCensus map[string]int
	// AbuserReplies is the swarm's reply census: code (or "SUCCESS") to count. It
	// is an invariant SUMMARY — which code, how many — and never says which
	// connection drew what, because that is the scheduler's business.
	AbuserReplies map[string]int
	// TransportErrors names every abuser exchange that failed at the transport
	// rather than drawing a Bolt reply. A reassembly-layer budget breach tears the
	// connection down (see [boltDecodeSwarmFloorNote]), so a non-empty list here is
	// the honest report of the swarm having been sized wrong — not a silent pass.
	TransportErrors []string
	// Window is the boundary scan, in ascending element count.
	Window []BoltDecodeProbe
	// Arms are the named deterministic arms, in the order they were driven.
	Arms []BoltDecodeExchange
	// Honest are the swarm's honest exchanges, in order.
	Honest []BoltDecodeHonest
	// Seed is the run's seed.
	Seed uint64
	// Budget is the pressured (or swarm) server's ceiling in bytes.
	Budget int64
	// ControlBudget is the control server's ceiling, 0 on the swarm arm.
	ControlBudget int64
	// ModelBoundary is the largest element count the harness's model says the pool
	// admits. MeasuredBoundary is the largest the SERVER actually admitted inside
	// the scanned window, or -1 when the window did not span a transition.
	ModelBoundary    int
	MeasuredBoundary int
	// LeakProbeInitial and LeakProbeFinal are the same boundary-sized message sent
	// before any pressure and after all of it. The first CALIBRATES (a pristine
	// pool must admit it); the second is the no-leak oracle, since a pool that
	// failed to return any of what it lent would no longer admit it.
	LeakProbeInitial BoltDecodeProbe
	LeakProbeFinal   BoltDecodeProbe
	// AbuserAccepted / AbuserRejected are the swarm's totals.
	AbuserAccepted int
	AbuserRejected int
	// AbuserAliveAfter is how many abuser connections still served an honest
	// exchange once the swarm quiesced.
	AbuserAliveAfter int
	// Abusers is how many pushed.
	Abusers int
	// RejectionsDuringHonest is how many abusive messages were refused between the
	// first honest exchange starting and the last one finishing. It is the
	// window-wide density claim, independent of whether any INDIVIDUAL exchange
	// straddled: it says the server was under pressure for the whole time it was
	// serving honest traffic, which is what "honest traffic stays live under
	// backpressure" has to mean before the per-exchange overlap can sharpen it.
	RejectionsDuringHonest int64
	// PressureStarted records whether the honest client saw a refusal counted
	// before it began, i.e. whether the start barrier was satisfied rather than
	// expiring. It is what tells a failing run apart: honest exchanges that did not
	// straddle because there was no pressure, versus ones that did not straddle
	// while there was.
	PressureStarted bool
	// Swarm marks the concurrent arm, whose adjudication differs.
	Swarm bool
}

// -----------------------------------------------------------------------------
// The charge model
// -----------------------------------------------------------------------------

// boltDecodeModelHeld returns how many bytes the decode of a RUN carrying one
// named list parameter of n elements holds from the shared inbound pool at its
// peak.
//
// It is the harness's INDEPENDENT re-derivation of packstream's accounting (see
// the file comment for the term-by-term breakdown and for the calibration that
// confirmed it byte-exact). Nothing in it reads the server.
func boltDecodeModelHeld(query, key string, n int) int64 {
	return int64(boltDecodeRunFixedCost) +
		int64(len(query)) + int64(len(key)) +
		int64(n)*boltDecodeListElemCost
}

// boltDecodeModelElementsFor returns the largest element count whose modelled
// hold still fits budget, i.e. the last element count the pool admits.
//
// It inverts [boltDecodeModelHeld]: n <= (budget - fixed - len(query) - len(key)) / 48.
// A budget too small for even an empty list yields 0.
func boltDecodeModelElementsFor(query, key string, budget int64) int {
	room := budget - int64(boltDecodeRunFixedCost) - int64(len(query)) - int64(len(key))
	if room < 0 {
		return 0
	}
	return int(room / boltDecodeListElemCost)
}

// boltDecodeModelElementsAt returns the element count whose modelled hold is the
// given FRACTION num/den of budget, used to size the swarm's abusive message so
// one fits the pool and two do not.
func boltDecodeModelElementsAt(query, key string, budget, num, den int64) int {
	return boltDecodeModelElementsFor(query, key, budget*num/den)
}

// boltDecodeParams builds the parameter map for a modelled message: one key
// bound to a list of n zero integers.
//
// Zeros are deliberate. The pool is charged 48 bytes per ELEMENT whatever the
// element is, so the value does not affect the model; a zero encodes as a
// one-byte TinyInt, which keeps the message ~48x smaller on the wire than the
// charge it carries. That ratio is what lets the decode-layer breach fire while
// the reassembly layer — which charges wire bytes, not decoded slots — stays far
// from its own share of the same pool.
func boltDecodeParams(n int) map[string]any {
	return map[string]any{boltDecodeParamKey: make([]int64, n)}
}

// boltDecodeRedactSession replaces the per-session id the failure sanitiser
// appends to its internal-error text with a fixed marker.
//
// The sanitised message reads "An internal error occurred. See server logs for
// details (session: <16 hex>)". The id is minted per connection and is NOT a
// function of the seed, so recording it verbatim would make the rendering vary
// run to run — the exact trap rmp #2486's own determinism test caught. The
// STABLE prefix is what the oracle pins; the id is replaced rather than dropped
// so the report still shows that a session id was present.
func boltDecodeRedactSession(msg string) string {
	i := strings.Index(msg, "(session: ")
	if i < 0 {
		return msg
	}
	return msg[:i] + "(session: <redacted>)"
}

// -----------------------------------------------------------------------------
// Hand-built wire payloads
// -----------------------------------------------------------------------------
//
// The nesting family is written byte by byte from PackStream's marker table
// rather than through [packstream.Encoder], and NOT because raw bytes are more
// authentic. The encoder CANNOT produce these messages: writeValue carries the
// SAME maxValueDepth bound as readValue (bolt/packstream/value.go:68-69), so
// asking the module to encode a 128-deep value returns an error instead of
// bytes. An abuse the module's own encoder refuses to express is exactly the
// abuse a hostile peer would hand-roll, and building it here keeps the harness
// from validating the decoder with the encoder that shares its bound.

// The PackStream markers these builders use, from the table in
// bolt/packstream/encoder.go:25-54.
const (
	boltDecodeMarkerNull          = 0xC0
	boltDecodeTinyStringBase      = 0x80
	boltDecodeTinyListBase        = 0x90
	boltDecodeTinyMapBase         = 0xA0
	boltDecodeTinyStructBase      = 0xB0
	boltDecodeString8Marker       = 0xD0
	boltDecodeTagRun         byte = 0x10 // proto.TagRun
	boltDecodeTagHello       byte = 0x01 // proto.TagHello
)

// boltDecodeTinyString appends s as a PackStream String. It handles the two
// forms the harness needs — TinyString for up to 15 bytes and STRING8 beyond —
// and PANICS above 255, because every string this file writes is a literal the
// harness controls and a silently truncated query would corrupt the model's
// len(query) term rather than fail.
func boltDecodeTinyString(dst []byte, s string) []byte {
	switch {
	case len(s) <= 15:
		dst = append(dst, byte(boltDecodeTinyStringBase|len(s)))
	case len(s) <= 255:
		dst = append(dst, boltDecodeString8Marker, byte(len(s)))
	default:
		panic("sim: bolt-decode string literal longer than 255 bytes: " + s)
	}
	return append(dst, s...)
}

// boltDecodeNestChain builds a chain of depth single-child composites ending in a
// NULL: TinyLists (0x91) when composite is "list", one-entry TinyMaps
// (0xA1 0x81 'k') when it is "map".
//
// Depth accounting, from readValue (bolt/packstream/value.go:151-229): the
// parameters map is read at depth 0 and its VALUE at depth 1, so the i-th link of
// this chain is entered at depth i and the terminating NULL at depth depth+1. The
// bound refuses at ENTRY when depth > maxValueDepth, so the refusal begins at
// chain length == maxValueDepth, and a chain of maxValueDepth-1 is the deepest
// the decoder accepts. Both figures were confirmed against the live server before
// they were written down.
func boltDecodeNestChain(depth int, composite string) []byte {
	var link []byte
	switch composite {
	case "map":
		link = []byte{boltDecodeTinyMapBase | 1, boltDecodeTinyStringBase | 1, 'k'}
	default:
		link = []byte{boltDecodeTinyListBase | 1}
	}
	out := make([]byte, 0, depth*len(link)+1)
	for range depth {
		out = append(out, link...)
	}
	return append(out, boltDecodeMarkerNull)
}

// boltDecodeRawRun builds a RUN carrying query, one parameter named
// [boltDecodeParamKey] whose encoded value is val, and an empty extra map.
func boltDecodeRawRun(query string, val []byte) []byte {
	out := []byte{boltDecodeTinyStructBase | 3, boltDecodeTagRun}
	out = boltDecodeTinyString(out, query)
	out = append(out, boltDecodeTinyMapBase|1)
	out = boltDecodeTinyString(out, boltDecodeParamKey)
	out = append(out, val...)
	// An EMPTY TinyMap is the bare marker byte: the tiny forms encode the count in
	// the low nibble, so a count of zero is the base marker unchanged.
	return append(out, boltDecodeTinyMapBase)
}

// boltDecodeRawHello builds a HELLO whose extra map holds one key bound to val.
// It carries no user_agent and no auth token: the point of the arm is that the
// decode happens BEFORE anything in the message can be authenticated, so the
// message is deliberately as bare as the wire allows.
func boltDecodeRawHello(val []byte) []byte {
	out := []byte{boltDecodeTinyStructBase | 1, boltDecodeTagHello}
	out = append(out, boltDecodeTinyMapBase|1)
	out = boltDecodeTinyString(out, boltDecodeParamKey)
	return append(out, val...)
}

// -----------------------------------------------------------------------------
// Reply classification
// -----------------------------------------------------------------------------

// boltDecodeReplyKind names a terminal reply. It distinguishes the kinds an
// oracle must tell apart — a FAILURE is not an IGNORED and neither is a SUCCESS —
// rather than collapsing them into a bool.
func boltDecodeReplyKind(resp any) (kind, code, message string) {
	switch m := resp.(type) {
	case *proto.Success:
		return "SUCCESS", "", ""
	case *proto.Failure:
		return "FAILURE", m.Code, boltDecodeRedactSession(m.Message)
	case *proto.Ignored:
		return "IGNORED", "", ""
	case *proto.Record:
		return "RECORD", "", ""
	case nil:
		return "", "", ""
	default:
		return fmt.Sprintf("%T", resp), "", ""
	}
}

// -----------------------------------------------------------------------------
// The nesting family
// -----------------------------------------------------------------------------

// boltDecodeNestArm names one member of the nesting-abuse family. The EXPECTED
// answer is deliberately absent: it is derived from the two caps by
// [boltDecodeExpectedNesting], so the table cannot drift out of step with the
// bounds it is meant to be probing, and a changed cap moves every expectation at
// once instead of leaving a stale literal that still passes.
type boltDecodeNestArm struct {
	name      string
	vehicle   string // "run" or "hello"
	composite string // "list" or "map"
	// depthOf computes the chain length from the two caps and, for the deliberately
	// excessive arm, from the seeded draw.
	depthOf func(far int) int
}

// boltDecodeNestArms is the family, in the order it is driven.
//
// It sweeps BOTH caps with BOTH composite kinds, and each cap is bracketed:
// one arm on each side of every boundary, differing in nothing but the chain
// length. That bracketing is the whole attribution device. A refusal shared by
// every depth would be a generic malformation; a refusal that switches on the
// chain length ALONE, at exactly the documented cap, and switches to a DIFFERENT
// code at the second cap, can only be the caps.
var boltDecodeNestArms = []boltDecodeNestArm{
	// The engine's parameter cap, bracketed. Both are decoded successfully; only
	// the second reaches BindParams' bound.
	{name: "param-depth-at-cap", vehicle: "run", composite: "list", depthOf: func(int) int { return boltDecodeParamDepthCap }},
	{name: "param-depth-over-cap", vehicle: "run", composite: "list", depthOf: func(int) int { return boltDecodeParamDepthCap + 1 }},
	{name: "param-depth-at-cap-map", vehicle: "run", composite: "map", depthOf: func(int) int { return boltDecodeParamDepthCap }},
	{name: "param-depth-over-cap-map", vehicle: "run", composite: "map", depthOf: func(int) int { return boltDecodeParamDepthCap + 1 }},

	// The wire cap, bracketed. The under-cap arm is still refused — by the ENGINE,
	// with a different code — which is precisely what proves the decoder accepted
	// a 127-deep value and that the two caps are independent.
	{name: "wire-depth-under-cap", vehicle: "run", composite: "list", depthOf: func(int) int { return boltDecodeWireDepthCap - 1 }},
	{name: "wire-depth-at-cap", vehicle: "run", composite: "list", depthOf: func(int) int { return boltDecodeWireDepthCap }},
	{name: "wire-depth-under-cap-map", vehicle: "run", composite: "map", depthOf: func(int) int { return boltDecodeWireDepthCap - 1 }},
	{name: "wire-depth-at-cap-map", vehicle: "run", composite: "map", depthOf: func(int) int { return boltDecodeWireDepthCap }},
	{name: "wire-depth-far-over-cap", vehicle: "run", composite: "list", depthOf: func(far int) int { return far }},

	// The wire cap PRE-AUTHENTICATION, bracketed. HELLO is the message packstream's
	// own comment names as the reason the bound is "a hard security boundary, not a
	// convenience" (value.go:8-15), and it is the only vehicle that isolates the
	// wire cap cleanly: no parameter is bound, so the engine's cap is not in the
	// way and the under-cap arm SUCCEEDS outright.
	{name: "preauth-depth-under-cap", vehicle: "hello", composite: "list", depthOf: func(int) int { return boltDecodeWireDepthCap - 1 }},
	{name: "preauth-depth-at-cap", vehicle: "hello", composite: "list", depthOf: func(int) int { return boltDecodeWireDepthCap }},
	{name: "preauth-depth-at-cap-map", vehicle: "hello", composite: "map", depthOf: func(int) int { return boltDecodeWireDepthCap }},
	{name: "preauth-depth-far-over-cap", vehicle: "hello", composite: "list", depthOf: func(far int) int { return far }},
}

// The excessive arm's chain length is drawn from [min, min+span). It is far past
// the cap and still two orders of magnitude under
// [boltDecodeNestingWireCeiling], so it is the arm that proves the refusal is by
// DEPTH: a message 48x deeper than the bound but a thousandth the size of the
// framing cap cannot have been refused for being big.
const (
	boltDecodeFarDepthMin  = 2048
	boltDecodeFarDepthSpan = 4096
)

// boltDecodeExpectedNesting derives what a nesting arm MUST draw, from the two
// caps alone.
//
// The three outcomes are the three-way discrimination the file comment sets out:
// the wire cap refuses above the session state machine and leaves the session
// usable; the engine's parameter cap refuses inside it and leaves the session
// FAILED; below both, the message is served.
func boltDecodeExpectedNesting(vehicle string, depth int) (kind, code string, sessionUsable bool) {
	if depth >= boltDecodeWireDepthCap {
		// The decoder refuses at entry, before proto.DecodeRequest can even tell
		// which message this is, so the vehicle does not matter.
		return "FAILURE", boltDecodeCodeInvalid, true
	}
	if vehicle == "run" && depth > boltDecodeParamDepthCap {
		// Decoded fine; cypher.BindParams then refuses the parameter and the
		// sanitiser turns that into a DatabaseError, which enters the FAILED state.
		return "FAILURE", boltDecodeCodeInternal, false
	}
	return "SUCCESS", "", true
}

// -----------------------------------------------------------------------------
// The deterministic runner
// -----------------------------------------------------------------------------

// RunBoltDecodePressure drives the whole deterministic inbound-decode surface
// once and returns the evidence.
//
// The subject is a WAL-backed server whose engine-wide inbound-decode ceiling is
// [boltDecodePressuredBudget]; the CONTROL is a second server that differs from
// it in that ceiling and in nothing else, so a payload the first refuses and the
// second serves isolates the pool as the cause rather than the payload.
//
// It is bit-reproducible from seed: every arm is a fixed lock-step script, the
// element counts are computed from the harness's model of the pool, and the only
// seeded draws are the excessive nesting depth, the two write arms' element
// counts and the SimDisk the WAL lives on. Nothing it records is a node id or a
// WAL byte total.
//
// The returned error is reserved for harness failures (the store would not open,
// a dial was refused, a reply did not arrive inside [boltDecodeArmBound]). A
// refused message is EVIDENCE, not an error.
func RunBoltDecodePressure(ctx context.Context, seed uint64) (*BoltDecodeEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^boltDecodeSeedMix), 0) // faultRate 0: this scenario faults nothing
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-decode open store: %w", err)
	}
	srv, err := NewSimServerInboundBudget(st.Engine(), clock.Real(), boltDecodePressuredBudget)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("sim: bolt-decode server: %w", err)
	}
	// The control runs on its own in-memory engine: the arm's claim is about the
	// CEILING, and giving it a separate graph is what lets the same bytes be
	// censused on both sides without one arm's node polluting the other's count.
	ctl, err := NewSimServerInboundBudget(SimEngineForServer(), clock.Real(), boltDecodeControlBudget)
	if err != nil {
		_ = srv.Close()
		_ = st.Close()
		return nil, fmt.Errorf("sim: bolt-decode control server: %w", err)
	}
	defer func() { _ = ctl.Close() }()

	ev := &BoltDecodeEvidence{
		Seed:             seed,
		Budget:           boltDecodePressuredBudget,
		ControlBudget:    boltDecodeControlBudget,
		MeasuredBoundary: -1,
		LiveCensus:       map[string]int{},
		RecoveredCensus:  map[string]int{},
		ControlCensus:    map[string]int{},
	}
	r := &boltDecodeRunner{srv: srv, ctl: ctl, st: st, ev: ev, rng: NewSeed(seed)}
	if err := r.driveAll(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}
	if err := r.censusLive(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}

	// Crash (drop the engine, keep the SimDisk image) and reopen through real
	// recovery. A refused message that had somehow reached the WAL without becoming
	// visible in the live engine is invisible to the live census and surfaces only
	// here.
	_ = srv.Close()
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-decode reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	return ev, r.censusRecovered(ctx, st2)
}

// boltDecodeRunner threads the two servers, the store, the seeded draws and the
// accumulating evidence through the arms.
type boltDecodeRunner struct {
	srv *SimServer
	ctl *SimServer
	st  *SimStore
	ev  *BoltDecodeEvidence
	rng *Seed
}

// driveAll runs every arm in a fixed order.
//
// The boundary scan runs FIRST, against a pristine pool: its accepted probes are
// what CALIBRATE the leak probe, and running them before anything has drawn on
// the pool is what makes "the pool was full at the start" an observation rather
// than an assumption. The leak probe runs LAST, after every abuse, so the
// question it answers is whether the pool came back.
func (r *boltDecodeRunner) driveAll(ctx context.Context) error {
	steps := []func(context.Context) error{
		r.armBoundaryScan,
		r.armPoolFitWrite,
		r.armPoolBreachWrite,
		r.armControlRaisedCeiling,
		r.armNesting,
		r.armLeakProbeFinal,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// connect dials the pressured server and completes handshake + HELLO + LOGON.
func (r *boltDecodeRunner) connect(ctx context.Context, srv *SimServer) (*WireClient, error) {
	c, err := srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-decode dial: %w", err)
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-decode connect: %w", err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-decode arm read deadline: %w", err)
	}
	return c, nil
}

// boltDecodeProbeQuery is the read-only query the boundary scan and the leak
// probes carry. It touches nothing, so an accepted probe has no effect a later
// census could confuse with an arm's write, and its length is a constant term in
// the model.
const boltDecodeProbeQuery = "RETURN 1 AS one"

// sendModelled sends one modelled RUN on c and reports whether the pool admitted
// it. On acceptance the stream is drained, so the session returns to READY and
// the caller may reuse the connection.
//
// It returns a harness error only for a transport fault. A pool refusal is the
// probe's answer, not a failure.
func (r *boltDecodeRunner) sendModelled(c *WireClient, query string, n int, budget int64) (BoltDecodeProbe, error) {
	p := BoltDecodeProbe{
		Elements:  n,
		ModelHeld: boltDecodeModelHeld(query, boltDecodeParamKey, n),
	}
	p.Slack = budget - p.ModelHeld

	resp, err := c.Run(query, boltDecodeParams(n))
	if err != nil {
		return p, fmt.Errorf("sim: bolt-decode modelled RUN (n=%d): %w", n, err)
	}
	kind, code, _ := boltDecodeReplyKind(resp)
	p.Code = code
	p.Accepted = kind == "SUCCESS"
	if p.Accepted {
		if _, _, err := c.PullAll(); err != nil {
			return p, fmt.Errorf("sim: bolt-decode modelled drain (n=%d): %w", n, err)
		}
	}
	return p, nil
}

// armBoundaryScan probes the element counts either side of the model's
// prediction and records the transition.
//
// One connection carries the whole scan: a pool refusal is answered ABOVE the
// session state machine, so it leaves the session READY, and every accepted probe
// is drained. Reusing the connection is therefore not a shortcut — it is a
// second, independent observation of the same claim, made once per probe.
func (r *boltDecodeRunner) armBoundaryScan(ctx context.Context) error {
	c, err := r.connect(ctx, r.srv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	r.ev.ModelBoundary = boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, r.ev.Budget)
	for d := -boltDecodeBoundaryWindow; d <= boltDecodeBoundaryWindow; d++ {
		n := r.ev.ModelBoundary + d
		if n < 0 {
			continue
		}
		p, err := r.sendModelled(c, boltDecodeProbeQuery, n, r.ev.Budget)
		if err != nil {
			return err
		}
		r.ev.Window = append(r.ev.Window, p)
		if p.Accepted && n > r.ev.MeasuredBoundary {
			r.ev.MeasuredBoundary = n
		}
	}
	// The largest accepted probe IS the calibration: it was drawn from a pool no
	// arm had yet touched, so its acceptance establishes both that the pool started
	// full and that a message this size is admissible at all. The final probe
	// repeats exactly it.
	for i := range r.ev.Window {
		if r.ev.Window[i].Elements == r.ev.MeasuredBoundary {
			r.ev.LeakProbeInitial = r.ev.Window[i]
		}
	}
	return nil
}

// armLeakProbeFinal repeats the calibrated boundary message after every abuse
// arm has run.
//
// This is the no-leak oracle, and it is stated in what a CLIENT can see rather
// than by reading the pool. [packstream.InboundBudget] exposes Enabled, TryReserve
// and Release but no Remaining, and the Server's pool is unexported, so there is
// no accessor to read — nor should this harness reach for one. Instead: a message
// whose modelled hold is within [boltDecodeLeakSensitivity] bytes of the whole
// ceiling can only be admitted by a pool that has been restored to within that
// many bytes of full. Its acceptance IS the proof, and its slack IS the
// sensitivity, which the gate requires to stay tight.
func (r *boltDecodeRunner) armLeakProbeFinal(ctx context.Context) error {
	n := r.ev.MeasuredBoundary
	if n < 0 {
		// The window did not span a transition, so there is no calibrated size. Fall
		// back to the model's, and let the non-vacuity gate report the shortfall
		// rather than quietly probing something meaningless.
		n = r.ev.ModelBoundary
	}
	c, err := r.connect(ctx, r.srv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	p, err := r.sendModelled(c, boltDecodeProbeQuery, n, r.ev.Budget)
	if err != nil {
		return err
	}
	r.ev.LeakProbeFinal = p
	return nil
}

// boltDecodeWriteQuery is the query text the fit and breach arms carry: the same
// shape, differing only in the label, so the pair differs in the element count
// and in nothing that could independently explain one running and the other not.
func boltDecodeWriteQuery(label string) string {
	return "CREATE (n:" + label + ") RETURN 1 AS one"
}

// armPoolFitWrite sends a WRITE whose parameter list is large but comfortably
// inside the pool. It must be served, and its node must survive to both censuses.
//
// The element count is drawn from the seed, in the lower half of what the pool
// admits, so the arm is seed-varying while its oracle stays computed rather than
// tabulated.
func (r *boltDecodeRunner) armPoolFitWrite(ctx context.Context) error {
	query := boltDecodeWriteQuery(boltDecodeFitLabel)
	maxN := boltDecodeModelElementsFor(query, boltDecodeParamKey, r.ev.Budget)
	n := maxN/4 + r.rng.IntN(maxN/4+1)
	return r.driveModelledArm(ctx, "pool-fit-write", query, n)
}

// armPoolBreachWrite sends the SAME shape of write with a parameter list well
// past what the pool admits. It must be refused with the typed, retryable code,
// the connection must survive, the session must still be READY without a RESET,
// and the node it WOULD have created must exist nowhere.
func (r *boltDecodeRunner) armPoolBreachWrite(ctx context.Context) error {
	query := boltDecodeWriteQuery(boltDecodeBreachLabel)
	maxN := boltDecodeModelElementsFor(query, boltDecodeParamKey, r.ev.Budget)
	n := maxN + 1 + r.rng.IntN(maxN)
	return r.driveModelledArm(ctx, "pool-breach-write", query, n)
}

// armControlRaisedCeiling replays the breach arm's message against the control
// server, whose ONLY difference is a 16x larger ceiling.
//
// This is the arm that makes "the aggregate pool refused it" a measurement rather
// than an inference. The bytes are identical — same query text, same key, same
// element count, rebuilt from the same generator — so every other candidate
// explanation (the 16 MiB framing cap, the 128 MiB per-message decoded-collection
// cap, the query, the parameter's shape) is held fixed. Only the ceiling moves,
// and the answer flips.
func (r *boltDecodeRunner) armControlRaisedCeiling(ctx context.Context) error {
	breach := boltDecodeFindArm(r.ev.Arms, "pool-breach-write")
	if breach == nil {
		return errors.New("sim: bolt-decode control arm: the breach arm did not run")
	}
	c, err := r.connect(ctx, r.ctl)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	query := boltDecodeWriteQuery(boltDecodeBreachLabel)
	p, err := r.sendModelled(c, query, breach.Elements, r.ev.ControlBudget)
	if err != nil {
		return err
	}
	arm := BoltDecodeExchange{
		Name:      "pool-control-raised-ceiling",
		Vehicle:   "run",
		Elements:  p.Elements,
		ModelHeld: p.ModelHeld,
		Reply:     "FAILURE",
	}
	if p.Accepted {
		arm.Reply = "SUCCESS"
	} else {
		arm.Code = p.Code
	}
	arm.SessionUsable, arm.FollowUpValue = r.followUpRun(c, boltDecodeFollowUpValue("pool-control-raised-ceiling"))
	r.ev.Arms = append(r.ev.Arms, arm)
	return nil
}

// boltDecodeFindArm returns the named arm, or nil.
func boltDecodeFindArm(arms []BoltDecodeExchange, name string) *BoltDecodeExchange {
	for i := range arms {
		if arms[i].Name == name {
			return &arms[i]
		}
	}
	return nil
}

// boltDecodeFollowUpValue derives the integer a named arm's follow-up query must
// return. It is a function of the arm's NAME, so a reply that belonged to another
// exchange cannot satisfy this one's liveness check.
func boltDecodeFollowUpValue(name string) int64 {
	var sum int64
	for i := 0; i < len(name); i++ {
		sum = sum*31 + int64(name[i])
	}
	if sum < 0 {
		sum = -sum
	}
	return sum%9000 + 1000
}

// followUpRun sends one honest RUN on c with NO RESET in between and reports
// whether it was served with exactly the expected value.
//
// "With no RESET" is the point. A refusal answered ABOVE the session state
// machine leaves the session READY, so the next request is served outright; a
// refusal that travelled through the state machine leaves it FAILED, so the next
// request is soft-IGNORED. Inserting a RESET first would erase the difference the
// arm exists to measure.
func (r *boltDecodeRunner) followUpRun(c *WireClient, want int64) (bool, int64) {
	return boltDecodeFollowUp(c, want)
}

// driveModelledArm drives one named modelled RUN on its own connection and
// records the arm, including whether the session survived the reply.
func (r *boltDecodeRunner) driveModelledArm(ctx context.Context, name, query string, n int) error {
	c, err := r.connect(ctx, r.srv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	arm := BoltDecodeExchange{
		Name:      name,
		Vehicle:   "run",
		Elements:  n,
		ModelHeld: boltDecodeModelHeld(query, boltDecodeParamKey, n),
	}
	resp, err := c.Run(query, boltDecodeParams(n))
	if err != nil {
		return fmt.Errorf("sim: bolt-decode arm %q: %w", name, err)
	}
	arm.Reply, arm.Code, arm.Message = boltDecodeReplyKind(resp)
	if arm.Reply == "SUCCESS" {
		if _, _, err := c.PullAll(); err != nil {
			return fmt.Errorf("sim: bolt-decode arm %q drain: %w", name, err)
		}
	}
	arm.SessionUsable, arm.FollowUpValue = r.followUpRun(c, boltDecodeFollowUpValue(name))
	r.ev.Arms = append(r.ev.Arms, arm)
	return nil
}

// armNesting drives the whole nesting family, each arm on its own connection.
func (r *boltDecodeRunner) armNesting(ctx context.Context) error {
	far := boltDecodeFarDepthMin + r.rng.IntN(boltDecodeFarDepthSpan)
	for _, spec := range boltDecodeNestArms {
		if err := r.driveNestArm(ctx, spec, spec.depthOf(far)); err != nil {
			return err
		}
	}
	return nil
}

// driveNestArm sends one hand-built over-nested message and records the reply,
// the wire size, and whether the session survived.
func (r *boltDecodeRunner) driveNestArm(ctx context.Context, spec boltDecodeNestArm, depth int) error {
	chain := boltDecodeNestChain(depth, spec.composite)
	arm := BoltDecodeExchange{
		Name:      spec.name,
		Vehicle:   spec.vehicle,
		Composite: spec.composite,
		Depth:     depth,
		Elements:  -1,
	}

	c, err := r.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-decode nest %q dial: %w", spec.name, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Handshake(ctx); err != nil {
		return fmt.Errorf("sim: bolt-decode nest %q handshake: %w", spec.name, err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		return fmt.Errorf("sim: bolt-decode nest %q deadline: %w", spec.name, err)
	}

	var payload []byte
	if spec.vehicle == "hello" {
		payload = boltDecodeRawHello(chain)
	} else {
		// A RUN vehicle needs an authenticated session first; the pre-auth arms
		// deliberately do not.
		if _, err := c.Hello(nil); err != nil {
			return fmt.Errorf("sim: bolt-decode nest %q HELLO: %w", spec.name, err)
		}
		if _, err := c.Logon(); err != nil {
			return fmt.Errorf("sim: bolt-decode nest %q LOGON: %w", spec.name, err)
		}
		payload = boltDecodeRawRun(boltDecodeProbeQuery, chain)
	}
	arm.WireBytes = len(payload)

	if err := c.WriteChunkedRaw(payload); err != nil {
		return fmt.Errorf("sim: bolt-decode nest %q write: %w", spec.name, err)
	}
	resp, err := c.Recv()
	if err != nil {
		return fmt.Errorf("sim: bolt-decode nest %q reply: %w", spec.name, err)
	}
	arm.Reply, arm.Code, arm.Message = boltDecodeReplyKind(resp)
	if arm.Reply == "SUCCESS" && spec.vehicle == "run" {
		if _, _, err := c.PullAll(); err != nil {
			return fmt.Errorf("sim: bolt-decode nest %q drain: %w", spec.name, err)
		}
	}

	arm.SessionUsable, arm.FollowUpValue = r.nestFollowUp(c, spec, arm.Reply)
	r.ev.Arms = append(r.ev.Arms, arm)
	return nil
}

// nestFollowUp completes the liveness check for one nesting arm.
//
// A pre-authentication arm's follow-up is two claims, not one: the connection is
// still usable AND the refused HELLO did not authenticate the session. A plain
// HELLO after a REFUSED one must therefore SUCCEED — which it can only do if the
// session never left NEGOTIATION — whereas after an ACCEPTED one the session is
// already past HELLO and a second would be illegal, so that path goes straight to
// LOGON. Both then run the same honest RUN, so the two arms end with the identical
// evidence and differ only where they must.
func (r *boltDecodeRunner) nestFollowUp(c *WireClient, spec boltDecodeNestArm, reply string) (bool, int64) {
	want := boltDecodeFollowUpValue(spec.name)
	if spec.vehicle != "hello" {
		return r.followUpRun(c, want)
	}
	if reply != "SUCCESS" {
		resp, err := c.Hello(nil)
		if err != nil {
			return false, -1
		}
		if kind, _, _ := boltDecodeReplyKind(resp); kind != "SUCCESS" {
			return false, -1
		}
	}
	resp, err := c.Logon()
	if err != nil {
		return false, -1
	}
	if kind, _, _ := boltDecodeReplyKind(resp); kind != "SUCCESS" {
		return false, -1
	}
	return r.followUpRun(c, want)
}

// censusLive counts each arm's label in the live engine, over the genuine wire,
// and the control server's own engine over its own wire.
func (r *boltDecodeRunner) censusLive(ctx context.Context) error {
	c, err := r.connect(ctx, r.srv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	for _, label := range []string{boltDecodeFitLabel, boltDecodeBreachLabel} {
		n, err := boltDecodeCountLabel(c, label)
		if err != nil {
			return err
		}
		r.ev.LiveCensus[label] = n
	}

	cc, err := r.connect(ctx, r.ctl)
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	n, err := boltDecodeCountLabel(cc, boltDecodeBreachLabel)
	if err != nil {
		return err
	}
	r.ev.ControlCensus[boltDecodeBreachLabel] = n
	return nil
}

// censusRecovered counts each arm's label on a store reopened through real
// recovery, reading the engine directly.
func (r *boltDecodeRunner) censusRecovered(ctx context.Context, st *SimStore) error {
	for _, label := range []string{boltDecodeFitLabel, boltDecodeBreachLabel} {
		n, err := scalarCountViaEngine(ctx, st.Engine(), fmt.Sprintf("MATCH (n:%s) RETURN count(n)", label))
		if err != nil {
			return fmt.Errorf("sim: bolt-decode recovered count %s: %w", label, err)
		}
		r.ev.RecoveredCensus[label] = int(n)
	}
	return nil
}

// boltDecodeCountLabel returns the number of nodes carrying label, read over the
// wire.
func boltDecodeCountLabel(c *WireClient, label string) (int, error) {
	recs, err := wireQuery(c, fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", label), nil)
	if err != nil {
		return 0, fmt.Errorf("sim: bolt-decode count %s: %w", label, err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("sim: bolt-decode count %s: got %d record(s), want 1 with 1 field", label, len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("sim: bolt-decode count %s: got %T, want int64", label, recs[0].Data[0])
	}
	return int(n), nil
}

// -----------------------------------------------------------------------------
// The concurrent swarm
// -----------------------------------------------------------------------------

// RunBoltDecodeSwarm drives the AGGREGATE arm: K abusers push large-collection
// parameters at one shared pool while an honest client keeps working against the
// same server.
//
// It is NOT bit-reproducible, and it cannot be: which abuser wins the pool on any
// given round is the scheduler's decision, and the whole point of the arm is that
// two charges are outstanding at once — a state a deterministic script cannot
// reach at all (see the file comment). The seed drives the honest client's
// interleave delay and nothing else. The oracle is what EVERY interleaving must
// share:
//
//   - every abuser reply is either SUCCESS or the typed retryable refusal, never
//     a third code and never a dropped connection;
//   - at least one message was refused (the pressure was real) and at least one
//     was served (the pool is not simply broken — a pool stuck at zero would
//     satisfy "every refusal is typed" perfectly);
//   - every honest exchange returned the value the harness computed for it, and at
//     least one of them STRADDLED a refusal, so honest service demonstrably
//     overlapped the pressure window rather than merely preceding or following it;
//   - the pool comes back: a message sized to the whole ceiling is admitted once
//     the swarm quiesces.
//
// Each abuser's message is sized at [boltDecodeSwarmChargeNum]/[boltDecodeSwarmChargeDen]
// of the pool, so one fits and two cannot. See [boltDecodeSwarmFloorNote] for why
// that sizing also keeps the honest client clear of the reassembly reader, whose
// own budget breach is NOT the connection-preserving refusal the decode layer's is.
func RunBoltDecodeSwarm(ctx context.Context, seed uint64) (*BoltDecodeEvidence, error) {
	srv, err := NewSimServerInboundBudget(SimEngineForServer(), clock.Real(), boltDecodeSwarmBudget)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-decode swarm server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	ev := &BoltDecodeEvidence{
		Seed:             seed,
		Swarm:            true,
		Budget:           boltDecodeSwarmBudget,
		Abusers:          boltDecodeSwarmAbusers,
		AbuserReplies:    map[string]int{},
		LiveCensus:       map[string]int{},
		RecoveredCensus:  map[string]int{},
		ControlCensus:    map[string]int{},
		MeasuredBoundary: -1,
	}
	ev.ModelBoundary = boltDecodeModelElementsFor(boltDecodeProbeQuery, boltDecodeParamKey, ev.Budget)
	abuseN := boltDecodeModelElementsAt(boltDecodeProbeQuery, boltDecodeParamKey, ev.Budget,
		boltDecodeSwarmChargeNum, boltDecodeSwarmChargeDen)

	s := &boltDecodeSwarm{srv: srv, ev: ev, abuseN: abuseN}
	if err := s.run(ctx, NewSeed(seed^boltDecodeSwarmSeedMix)); err != nil {
		return nil, err
	}

	// The pool must be whole again. This runs after every abuser and the honest
	// client have joined, so nothing else can be holding a reservation.
	c, err := srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-decode swarm leak probe dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("sim: bolt-decode swarm leak probe connect: %w", err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		return nil, fmt.Errorf("sim: bolt-decode swarm leak probe deadline: %w", err)
	}
	p, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, ev.ModelBoundary, ev.Budget)
	if err != nil {
		return nil, err
	}
	ev.LeakProbeFinal = p
	return ev, nil
}

// boltDecodeSwarm carries the shared state of one concurrent run.
type boltDecodeSwarm struct {
	srv *SimServer
	ev  *BoltDecodeEvidence
	// rejected is the fleet-wide count of typed pool refusals, sampled by the
	// honest client either side of every exchange. It is the OVERLAP instrument:
	// a strictly increasing sample across one honest round trip proves abusive
	// traffic was being refused while that round trip was in flight, without any
	// reference to a clock.
	rejected atomic.Int64
	// honestDone is set once the honest client has finished its exchanges. The
	// abusers read it every round and stop, so the pressure window is guaranteed to
	// have OUTLASTED the honest window rather than merely overlapped it by luck.
	honestDone atomic.Bool
	// mu guards the evidence slices the abusers append to. The abusers' own results
	// are order-independent (the report is a census, never a sequence), so a plain
	// mutex is the right primitive and the ordering it does not provide is not
	// something any clause reads.
	mu     sync.Mutex
	abuseN int
}

// awaitPressure blocks until at least one abusive message has been refused, or
// until [boltDecodeSwarmStartBarrier] expires. It reports whether the pressure
// was observed to start.
func (s *boltDecodeSwarm) awaitPressure(ctx context.Context) bool {
	deadline := time.Now().Add(boltDecodeSwarmStartBarrier)
	for {
		if s.rejected.Load() > 0 {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(boltDecodeSwarmBarrierStep)
	}
}

// boltDecodeSendProbe sends one modelled RUN and reports the pool's verdict. It
// is the connection-agnostic form of [boltDecodeRunner.sendModelled], shared by
// the swarm and the leak probe.
func boltDecodeSendProbe(c *WireClient, query string, n int, budget int64) (BoltDecodeProbe, error) {
	p := BoltDecodeProbe{Elements: n, ModelHeld: boltDecodeModelHeld(query, boltDecodeParamKey, n)}
	p.Slack = budget - p.ModelHeld
	resp, err := c.Run(query, boltDecodeParams(n))
	if err != nil {
		return p, fmt.Errorf("sim: bolt-decode probe (n=%d): %w", n, err)
	}
	kind, code, _ := boltDecodeReplyKind(resp)
	p.Code = code
	p.Accepted = kind == "SUCCESS"
	if p.Accepted {
		if _, _, err := c.PullAll(); err != nil {
			return p, fmt.Errorf("sim: bolt-decode probe drain (n=%d): %w", n, err)
		}
	}
	return p, nil
}

// run starts the abusers and the honest client together and joins them.
func (s *boltDecodeSwarm) run(ctx context.Context, rng *Seed) error {
	// The honest client's per-exchange pause is drawn ONCE, before any goroutine
	// starts: a *Seed is not safe for concurrent use, and drawing from inside the
	// fleet would be both a data race and a source of non-reproducibility the arm
	// does not need.
	pauses := make([]time.Duration, boltDecodeSwarmHonestOps)
	for i := range pauses {
		pauses[i] = time.Duration(rng.IntN(boltDecodeSwarmHonestPauseMaxUs)) * time.Microsecond
	}

	var wg sync.WaitGroup
	errs := make(chan error, boltDecodeSwarmAbusers+1)
	for i := range boltDecodeSwarmAbusers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := s.abuser(ctx, id); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.honest(ctx, pauses); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	return <-errs
}

// boltDecodeSwarmHonestPauseMaxUs bounds the honest client's inter-exchange
// pause, in microseconds. It exists so the honest exchanges SPREAD across the
// abusers' rounds instead of finishing in a burst before the pressure builds;
// the overlap clause still has to observe the spread rather than assume it.
const boltDecodeSwarmHonestPauseMaxUs = 2000

// abuser drives one connection's rounds of oversized parameters.
func (s *boltDecodeSwarm) abuser(ctx context.Context, id int) error {
	c, err := s.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-decode swarm abuser %d dial: %w", id, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm abuser %d connect: %w", id, err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm abuser %d deadline: %w", id, err)
	}

	for round := range boltDecodeSwarmMaxRounds {
		if ctx.Err() != nil {
			return nil
		}
		// Keep pushing until the honest client has finished, but never fewer than
		// the floor: the honest window must lie INSIDE the pressure window, and a
		// honest client that finished before the pressure built must still find
		// some to have straddled.
		if round >= boltDecodeSwarmMinRounds && s.honestDone.Load() {
			break
		}
		resp, err := c.Run(boltDecodeProbeQuery, boltDecodeParams(s.abuseN))
		if err != nil {
			// A transport fault is recorded, not returned: a reassembly-layer budget
			// breach tears the connection down, and the honest report of that is a
			// named entry the contract fails on, never a harness error that hides it.
			s.mu.Lock()
			s.ev.TransportErrors = append(s.ev.TransportErrors,
				fmt.Sprintf("abuser %d round %d: %v", id, round, err))
			s.mu.Unlock()
			return nil
		}
		kind, code, _ := boltDecodeReplyKind(resp)
		key := kind
		if kind == "FAILURE" {
			key = code
			s.rejected.Add(1)
		}
		s.mu.Lock()
		s.ev.AbuserReplies[key]++
		if kind == "SUCCESS" {
			s.ev.AbuserAccepted++
		} else {
			s.ev.AbuserRejected++
		}
		s.mu.Unlock()
		if kind == "SUCCESS" {
			if _, _, err := c.PullAll(); err != nil {
				s.mu.Lock()
				s.ev.TransportErrors = append(s.ev.TransportErrors,
					fmt.Sprintf("abuser %d round %d drain: %v", id, round, err))
				s.mu.Unlock()
				return nil
			}
		}
	}

	// The connection must still work once this abuser has stopped pushing: a
	// backpressure refusal that quietly poisoned the connection would satisfy every
	// clause about the refusal itself and still be a defect.
	want := boltDecodeFollowUpValue(fmt.Sprintf("swarm-abuser-%d", id))
	if ok, _ := boltDecodeFollowUp(c, want); ok {
		s.mu.Lock()
		s.ev.AbuserAliveAfter++
		s.mu.Unlock()
	}
	return nil
}

// honest drives the honest client: short reads that must all be served correctly
// while the abusers push.
func (s *boltDecodeSwarm) honest(ctx context.Context, pauses []time.Duration) error {
	c, err := s.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-decode swarm honest dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm honest connect: %w", err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm honest deadline: %w", err)
	}

	// Signal completion however this returns, so a honest client that stops early
	// cannot leave the abusers pushing until their safety bound.
	defer s.honestDone.Store(true)

	s.ev.PressureStarted = s.awaitPressure(ctx)
	firstSample := s.rejected.Load()
	defer func() { s.ev.RejectionsDuringHonest = s.rejected.Load() - firstSample }()
	for i := range pauses {
		if ctx.Err() != nil {
			return nil
		}
		time.Sleep(pauses[i])
		h := BoltDecodeHonest{
			Index:            i,
			Wide:             i%boltDecodeSwarmWideEvery == 0,
			RejectionsBefore: s.rejected.Load(),
			Value:            -1,
		}
		start := time.Now()
		var (
			ok  bool
			got int64
		)
		if h.Wide {
			ok, got = boltDecodeWideExchange(c, int64(i))
		} else {
			ok, got = boltDecodeFollowUp(c, int64(i))
		}
		h.Elapsed = time.Since(start)
		h.RejectionsAfter = s.rejected.Load()
		h.OK, h.Value = ok, got
		s.mu.Lock()
		s.ev.Honest = append(s.ev.Honest, h)
		s.mu.Unlock()
		if !ok {
			// Stop at the first starved or wrong answer: the evidence is complete and
			// continuing would bury it under 20 more entries.
			return nil
		}
	}
	return nil
}

// boltDecodeWideExchange runs one honest exchange that deliberately holds its
// stream open between RUN and PULL, so the exchange is in flight for a window
// wide enough to contain several refusals.
//
// The hold is client-side on purpose. Widening the window with engine work — a
// large UNWIND, say — would make the width a property of the machine, which is
// the very thing that must not decide whether the oracle fires. A sleep between
// RUN and PULL makes it exactly [boltDecodeSwarmWideHold] on any machine, while
// leaving the exchange genuinely open: the server has acknowledged the statement
// and is holding a cursor for it the whole time.
func boltDecodeWideExchange(c *WireClient, want int64) (bool, int64) {
	resp, err := c.Run("RETURN "+strconv.FormatInt(want, 10)+" AS v", nil)
	if err != nil {
		return false, -1
	}
	if kind, _, _ := boltDecodeReplyKind(resp); kind != "SUCCESS" {
		return false, -1
	}
	time.Sleep(boltDecodeSwarmWideHold)
	return boltDecodeDrainOne(c, want)
}

// boltDecodeFollowUp runs one honest RETURN <want> exchange and reports whether
// it came back with exactly that value.
//
// The value is the ORACLE: the harness chose it, so a reply carrying anything
// else — including a correct-looking row belonging to a different exchange — is a
// failure. That is what makes "honest traffic stayed live" a statement about
// correct service rather than about a reply arriving.
func boltDecodeFollowUp(c *WireClient, want int64) (bool, int64) {
	resp, err := c.Run("RETURN "+strconv.FormatInt(want, 10)+" AS v", nil)
	if err != nil {
		return false, -1
	}
	if kind, _, _ := boltDecodeReplyKind(resp); kind != "SUCCESS" {
		return false, -1
	}
	return boltDecodeDrainOne(c, want)
}

// boltDecodeDrainOne drains an open stream and reports whether it delivered
// exactly one record holding exactly want.
func boltDecodeDrainOne(c *WireClient, want int64) (bool, int64) {
	recs, terminal, err := c.PullAll()
	if err != nil {
		return false, -1
	}
	if kind, _, _ := boltDecodeReplyKind(terminal); kind != "SUCCESS" {
		return false, -1
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return false, -1
	}
	got, ok := recs[0].Data[0].(int64)
	if !ok {
		return false, -1
	}
	return got == want, got
}

// -----------------------------------------------------------------------------
// The contract
// -----------------------------------------------------------------------------

// boltDecodeOp renders the Op field of a violation for one clause, so a report
// names the clause that fired.
func boltDecodeOp(clause string) string { return "<bolt-decode:" + clause + ">" }

// boltDecodeViolation builds one violation for a clause.
func boltDecodeViolation(kind ViolationKind, clause, msg string) Violation {
	return Violation{Kind: kind, Op: boltDecodeOp(clause), Message: msg}
}

// boltDecodeClassification returns a Bolt status code's classification segment —
// the second of its four dot-separated parts — or "" when the code is not in that
// shape.
//
// The segment is what a driver's retry decision turns on: neo4j-go-driver's
// IsRetriableTransient tests `classification == "TransientError"`
// (bolt/server/errors.go:130-131). Reading it out of the OBSERVED code, rather
// than comparing the whole string to a literal, is what makes the retryability
// clause a statement about what the server sent instead of a restatement of the
// constant this file already declares.
func boltDecodeClassification(code string) string {
	parts := strings.Split(code, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// checkBoltDecodePressure adjudicates the deterministic evidence.
func checkBoltDecodePressure(e *BoltDecodeEvidence) []Violation {
	if e.Swarm {
		return checkBoltDecodeSwarm(e)
	}
	// One family per clause group, in the order the file declares them, so a
	// report reads top to bottom the way the surface does.
	families := []func(*BoltDecodeEvidence) []Violation{
		checkBoltDecodeBoundary,
		checkBoltDecodeModelAgreement,
		checkBoltDecodeRefusalTyping,
		checkBoltDecodeEffects,
		checkBoltDecodeControl,
		checkBoltDecodeNesting,
		checkBoltDecodeCapsAnswerDifferently,
		checkBoltDecodeBudgetRestored,
	}
	v := make([]Violation, 0, len(families))
	for _, family := range families {
		v = append(v, family(e)...)
	}
	return v
}

// checkBoltDecodeBoundary requires the pool's admission boundary to be a single,
// monotone, one-element-wide step that lands exactly where the harness's model
// says it does.
//
// Monotonicity is the part that rules out coincidence. A pool that refused for
// some reason OTHER than its arithmetic — a flaky allocation, an unrelated cap, a
// message-shape fault — has no reason to accept every count below one threshold
// and refuse every count above it, across seven probes differing by 48 charged
// bytes each.
func checkBoltDecodeBoundary(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	seenRefusal := false
	for i := range e.Window {
		p := &e.Window[i]
		if !p.Accepted {
			seenRefusal = true
			continue
		}
		if seenRefusal {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-boundary-monotone",
				fmt.Sprintf("the pool accepted n=%d (model hold %d B) AFTER refusing a SMALLER count: "+
					"admission is not monotone in the charge, so the boundary is not the pool's arithmetic",
					p.Elements, p.ModelHeld)))
		}
	}
	if e.MeasuredBoundary < 0 {
		// Reported by the non-vacuity gate as a window shortfall; the contract has
		// nothing to say about a boundary that was never bracketed.
		return v
	}
	if e.MeasuredBoundary != e.ModelBoundary {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-boundary-matches-model",
			fmt.Sprintf("the pool admits at most %d elements but the harness's model says %d "+
				"(hold = %d + len(query) + len(key) + %d*n, ceiling %d B). Either a packstream per-slot "+
				"cost changed (collectionCost %d, listElemCost %d, mapEntryCost %d, mapCollectionCost %d) "+
				"or a charge was added or removed; the model in this file must be re-derived",
				e.MeasuredBoundary, e.ModelBoundary, boltDecodeRunFixedCost, boltDecodeListElemCost, e.Budget,
				boltDecodeCollectionCost, boltDecodeListElemCost, boltDecodeMapEntryCost, boltDecodeMapCollectionCost)))
	}
	return v
}

// checkBoltDecodeModelAgreement requires EVERY modelled message — the window, the
// two write arms, the control and both leak probes — to have been admitted
// exactly when the model says it fits.
//
// It is the same claim as the boundary clause made over the whole run rather than
// over one sweep, so an arm sized far from the boundary cannot drift unnoticed.
func checkBoltDecodeModelAgreement(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	judge := func(what string, n int, held, budget int64, accepted bool) {
		wantAccept := held <= budget
		if accepted == wantAccept {
			return
		}
		verb, why := "refused", "the model says it fits"
		if accepted {
			verb, why = "served", "the model says it does not fit"
		}
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-model-agreement",
			fmt.Sprintf("%s (n=%d, model hold %d B against a %d B ceiling, slack %d B) was %s, but %s",
				what, n, held, budget, budget-held, verb, why)))
	}
	for i := range e.Window {
		p := &e.Window[i]
		judge(fmt.Sprintf("boundary probe %d", i), p.Elements, p.ModelHeld, e.Budget, p.Accepted)
	}
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Elements < 0 {
			continue // a nesting arm: not a modelled charge
		}
		budget := e.Budget
		if a.Name == "pool-control-raised-ceiling" {
			budget = e.ControlBudget
		}
		judge("arm "+a.Name, a.Elements, a.ModelHeld, budget, a.Reply == "SUCCESS")
	}
	judge("the final leak probe", e.LeakProbeFinal.Elements, e.LeakProbeFinal.ModelHeld, e.Budget, e.LeakProbeFinal.Accepted)
	return v
}

// checkBoltDecodeRefusalTyping requires every pool refusal to be the typed,
// RETRYABLE backpressure a driver can act on, and to leave the session usable.
func checkBoltDecodeRefusalTyping(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	// The two halves are checked INDEPENDENTLY, and deliberately so. Testing the
	// classification only after the whole code matched the literal would make the
	// classification clause unreachable — the literal this file declares already
	// carries "TransientError", so the guard could never fire and would certify
	// nothing. Read out of the OBSERVED code on its own, it is the half that says
	// a driver will RETRY, which is the property that actually matters and which
	// survives the code being renamed.
	check := func(what, code string) {
		if got := boltDecodeClassification(code); got != boltDecodeRetriableClassification {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-refusal-retryable",
				fmt.Sprintf("%s drew classification %q in %q, want %q: neo4j-go-driver retries on the "+
					"classification segment alone (bolt/server/errors.go:130-131), so backpressure "+
					"carrying any other one is never retried and the client sees a hard failure where "+
					"the server meant 'try again'",
					what, got, code, boltDecodeRetriableClassification)))
		}
		if code != boltDecodeCodeBudget {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-refusal-typed",
				fmt.Sprintf("%s was refused with %q, want %q: the aggregate-pool breach must be "+
					"distinguishable from every other refusal a client can draw",
					what, code, boltDecodeCodeBudget)))
		}
	}
	for i := range e.Window {
		if p := &e.Window[i]; !p.Accepted {
			check(fmt.Sprintf("boundary probe n=%d", p.Elements), p.Code)
		}
	}
	breach := boltDecodeFindArm(e.Arms, "pool-breach-write")
	if breach == nil {
		return append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-breach-refused",
			"the breach arm did not run"))
	}
	if breach.Reply != "FAILURE" {
		return append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-breach-refused",
			fmt.Sprintf("the breach arm (n=%d, model hold %d B against a %d B ceiling) drew %q, want a FAILURE",
				breach.Elements, breach.ModelHeld, e.Budget, breach.Reply)))
	}
	check("the breach arm", breach.Code)
	if breach.Message != boltDecodeMsgBudget {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-refusal-message",
			fmt.Sprintf("the breach arm's message is %q, want %q: the code alone does not tell an operator "+
				"which of the server's memory ceilings was reached", breach.Message, boltDecodeMsgBudget)))
	}
	if !breach.SessionUsable {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-refusal-keeps-session",
			fmt.Sprintf("after the pool refusal the SAME connection would not serve a plain RETURN with no "+
				"RESET (it answered %d, want %d). Backpressure that costs the client its session is not "+
				"backpressure: the refusal is answered above the session state machine (serve.go:1258) "+
				"precisely so the session stays READY",
				breach.FollowUpValue, boltDecodeFollowUpValue(breach.Name))))
	}
	return v
}

// checkBoltDecodeEffects requires an accepted message to have had its full effect
// and a refused one to have had none — live AND after real WAL recovery.
//
// The pair is the atomicity claim in its strongest available form: the two arms
// carry the same query SHAPE and differ only in how many elements their parameter
// list holds, so "one node exists and the other does not" cannot be explained by
// anything but the pool's verdict.
func checkBoltDecodeEffects(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	fit := boltDecodeFindArm(e.Arms, "pool-fit-write")
	if fit == nil || fit.Reply != "SUCCESS" {
		got := "did not run"
		if fit != nil {
			got = fmt.Sprintf("drew %q %s", fit.Reply, fit.Code)
		}
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-fit-served",
			fmt.Sprintf("the fit arm %s, but its model hold fits the ceiling", got)))
	}
	for census, counts := range map[string]map[string]int{"live": e.LiveCensus, "recovered": e.RecoveredCensus} {
		if n := counts[boltDecodeFitLabel]; n != 1 {
			v = append(v, boltDecodeViolation(ViolationACIDDurability, "pool-fit-durable",
				fmt.Sprintf("%s census counts %d :%s node(s), want exactly 1: a message the pool ACCEPTED "+
					"must have its full effect and survive recovery", census, n, boltDecodeFitLabel)))
		}
		if n := counts[boltDecodeBreachLabel]; n != 0 {
			v = append(v, boltDecodeViolation(ViolationACIDAtomicity, "pool-breach-no-effect",
				fmt.Sprintf("%s census counts %d :%s node(s), want 0: the message that would have created "+
					"it was refused before it was decoded, so nothing could have run",
					census, n, boltDecodeBreachLabel)))
		}
	}
	return v
}

// checkBoltDecodeControl requires the identical bytes the pressured pool refused
// to be SERVED by a server whose only difference is a larger ceiling.
func checkBoltDecodeControl(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	ctl := boltDecodeFindArm(e.Arms, "pool-control-raised-ceiling")
	breach := boltDecodeFindArm(e.Arms, "pool-breach-write")
	if ctl == nil || breach == nil {
		return append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-control-served",
			"the control arm or the breach arm did not run"))
	}
	if ctl.Elements != breach.Elements {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-control-identical-payload",
			fmt.Sprintf("the control replayed %d elements against the breach arm's %d: a control that sent "+
				"DIFFERENT bytes controls for nothing", ctl.Elements, breach.Elements)))
	}
	if ctl.Reply != "SUCCESS" {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-control-served",
			fmt.Sprintf("the control server (ceiling %d B, %dx the pressured %d B) drew %q %s for the very "+
				"bytes the pressured server refused. The refusal is therefore NOT attributable to the "+
				"aggregate pool: some other bound — the %d B framing cap or the per-message "+
				"decoded-collection cap — is what fired, and this scenario is measuring the wrong thing",
				e.ControlBudget, e.ControlBudget/e.Budget, e.Budget, ctl.Reply, ctl.Code, 16<<20)))
	}
	if n := e.ControlCensus[boltDecodeBreachLabel]; n != 1 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "pool-control-effect",
			fmt.Sprintf("the control server's engine holds %d :%s node(s), want exactly 1: the same bytes "+
				"that left the pressured engine untouched must have had their full effect here",
				n, boltDecodeBreachLabel)))
	}
	return v
}

// checkBoltDecodeNesting requires every nesting arm to have drawn exactly the
// answer the two caps predict, and requires the family to be small enough on the
// wire that SIZE cannot be the explanation.
func checkBoltDecodeNesting(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Depth == 0 {
			continue // not a nesting arm
		}
		wantKind, wantCode, wantUsable := boltDecodeExpectedNesting(a.Vehicle, a.Depth)
		if a.Reply != wantKind || a.Code != wantCode {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-answer",
				fmt.Sprintf("%s (%s vehicle, %s chain of %d, wire cap %d, parameter cap %d) drew %q %q, "+
					"want %q %q. The two caps are the only thing that varies across this family, so a "+
					"different answer means a cap moved or the two collapsed into one",
					a.Name, a.Vehicle, a.Composite, a.Depth, boltDecodeWireDepthCap, boltDecodeParamDepthCap,
					a.Reply, a.Code, wantKind, wantCode)))
		}
		if a.SessionUsable != wantUsable {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-session-after",
				fmt.Sprintf("%s left the session usable=%v, want %v. The refusals answered ABOVE the "+
					"session state machine (serve.go:1258 and :1289) must leave it READY without a RESET; "+
					"the one that travels through it into cypher.BindParams must leave it FAILED. That "+
					"difference is the third discriminator between the caps and it is not optional",
					a.Name, a.SessionUsable, wantUsable)))
		}
		if a.WireBytes >= boltDecodeNestingWireCeiling {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-not-by-size",
				fmt.Sprintf("%s is %d wire bytes, at or over the %d B anti-confound ceiling: a payload "+
					"this large could have been refused for its SIZE, so it proves nothing about a DEPTH cap",
					a.Name, a.WireBytes, boltDecodeNestingWireCeiling)))
		}
		if wantCode == boltDecodeCodeInvalid && a.Message != boltDecodeMsgInvalid {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-message",
				fmt.Sprintf("%s carries message %q, want %q", a.Name, a.Message, boltDecodeMsgInvalid)))
		}
		if wantCode == boltDecodeCodeInternal && !strings.HasPrefix(a.Message, boltDecodeMsgInternalPrefix) {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-message",
				fmt.Sprintf("%s carries message %q, want a message beginning %q",
					a.Name, a.Message, boltDecodeMsgInternalPrefix)))
		}
		// Only the OBSERVED code is tested. boltDecodeExpectedNesting can never
		// return the budget code, so a disjunct on wantCode would be a dead guard
		// dressed up as a check.
		if a.Code == boltDecodeCodeBudget {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-is-not-backpressure",
				fmt.Sprintf("%s drew the AGGREGATE POOL's code %q. A depth abuse answered as memory "+
					"backpressure tells a driver to RETRY a message that will fail identically for ever",
					a.Name, boltDecodeCodeBudget)))
		}
	}
	return v
}

// checkBoltDecodeCapsAnswerDifferently requires the three abuse vectors to have
// drawn three PAIRWISE DISTINCT codes, read off the arms that actually ran.
//
// This is the clause the whole scenario is built around. A server that answered
// aggregate memory pressure, a stack-overflow attempt and an over-nested
// parameter with one code would be indistinguishable, from the client's side,
// from a server with no aggregate pool and no depth cap at all — every other
// clause here could still pass against it.
func checkBoltDecodeCapsAnswerDifferently(e *BoltDecodeEvidence) []Violation {
	observed := map[string]string{} // vector -> code
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Reply != "FAILURE" {
			continue
		}
		switch {
		case a.Name == "pool-breach-write":
			observed["aggregate pool"] = a.Code
		case a.Depth >= boltDecodeWireDepthCap:
			observed["wire nesting cap"] = a.Code
		case a.Depth > boltDecodeParamDepthCap:
			observed["engine parameter cap"] = a.Code
		}
	}
	if len(observed) < 3 {
		// Fewer than three vectors fired: the non-vacuity gate names that, and a
		// distinctness claim over two codes would be a weaker statement pretending
		// to be this one.
		return nil
	}
	v := make([]Violation, 0, len(boltDecodeVectorOrder))
	seen := map[string]string{} // code -> the vector that drew it
	for _, vector := range boltDecodeVectorOrder {
		code := observed[vector]
		if prior, dup := seen[code]; dup {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "caps-answer-differently",
				fmt.Sprintf("the %s and the %s both answer %q. A client cannot tell aggregate memory "+
					"pressure (retry later) from a malformed message (never retry) from an over-nested "+
					"parameter (split the payload), so it cannot react correctly to any of them",
					prior, vector, code)))
			continue
		}
		seen[code] = vector
	}
	return v
}

// boltDecodeVectorOrder fixes the order the distinctness clause reports in, so a
// failure message is stable rather than map-iteration order.
var boltDecodeVectorOrder = []string{"aggregate pool", "wire nesting cap", "engine parameter cap"}

// checkBoltDecodeBudgetRestored requires the pool to have come back.
func checkBoltDecodeBudgetRestored(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	if !e.LeakProbeInitial.Accepted {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "budget-probe-calibrated",
			fmt.Sprintf("the calibration probe (n=%d, model hold %d B, slack %d B) was refused against a "+
				"PRISTINE pool, so the final probe measures nothing: it would be refused whether or not "+
				"anything leaked", e.LeakProbeInitial.Elements, e.LeakProbeInitial.ModelHeld, e.LeakProbeInitial.Slack)))
		return v
	}
	if !e.LeakProbeFinal.Accepted {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "budget-restored",
			fmt.Sprintf("the identical probe (n=%d, model hold %d B, slack %d B) was SERVED against a "+
				"pristine pool and REFUSED (%s) after the abuse arms. The pool did not return at least %d "+
				"bytes of what it lent: a reservation was taken and never released",
				e.LeakProbeFinal.Elements, e.LeakProbeFinal.ModelHeld, e.LeakProbeFinal.Slack,
				e.LeakProbeFinal.Code, e.LeakProbeFinal.Slack+1)))
	}
	return v
}

// checkBoltDecodeSwarm adjudicates the concurrent arm, on clauses that hold under
// ANY interleaving.
func checkBoltDecodeSwarm(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	for code, n := range e.AbuserReplies {
		if code == "SUCCESS" || code == boltDecodeCodeBudget {
			continue
		}
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-replies-typed",
			fmt.Sprintf("%d abusive message(s) drew %q: under aggregate pressure a message is either "+
				"served or refused with %q, and nothing else", n, code, boltDecodeCodeBudget)))
	}
	// Read off the OBSERVED reply keys, not off the literal: asking whether this
	// file's own constant carries "TransientError" is a question about this file.
	for code, n := range e.AbuserReplies {
		if code == "SUCCESS" {
			continue
		}
		if got := boltDecodeClassification(code); got != boltDecodeRetriableClassification {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-refusal-retryable",
				fmt.Sprintf("%d refusal(s) carry classification %q in %q, want %q: a refusal a driver "+
					"does not retry is not backpressure, it is a hard failure",
					n, got, code, boltDecodeRetriableClassification)))
		}
	}
	for _, te := range e.TransportErrors {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-no-transport-loss",
			fmt.Sprintf("an abuser lost its CONNECTION under aggregate pressure rather than drawing a "+
				"typed refusal (%s). The decode-layer breach preserves the connection; the reassembly-layer "+
				"one does not (bolt/proto/chunking.go:223-227 returns a READ error and serve.go:1237-1247 "+
				"tears the connection down), so either the swarm is sized so the reassembly reader loses "+
				"the race for the pool — see the floor arithmetic on boltDecodeSwarmAbusers — or the two "+
				"layers' behaviour has "+
				"changed", te)))
	}
	for i := range e.Honest {
		h := &e.Honest[i]
		if !h.OK || h.Value != int64(h.Index) {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-honest-served",
				fmt.Sprintf("honest exchange %d returned %d (ok=%v), want %d: honest traffic must be served "+
					"CORRECTLY while the fleet is refused, not merely answered", h.Index, h.Value, h.OK, h.Index)))
		}
		if h.Elapsed > boltDecodeHonestBound {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-honest-live",
				fmt.Sprintf("honest exchange %d was STARVED: it waited %s for a reply that a healthy server "+
					"answers in milliseconds, past the %s bound. A server that lets aggregate pressure "+
					"monopolise its accept and decode path never serves honest traffic at all, so the wait "+
					"is unbounded and a clock is the only instrument that can see it",
					h.Index, h.Elapsed, boltDecodeHonestBound)))
		}
	}
	if e.AbuserAliveAfter != e.Abusers {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-abusers-alive",
			fmt.Sprintf("%d of %d abuser connections still served an honest exchange after the swarm "+
				"quiesced. A backpressure refusal must not cost the refused client its connection",
				e.AbuserAliveAfter, e.Abusers)))
	}
	if !e.LeakProbeFinal.Accepted {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "swarm-budget-restored",
			fmt.Sprintf("after the swarm quiesced, a probe sized to the whole ceiling (n=%d, model hold "+
				"%d B, slack %d B) was refused (%s): %d concurrent connections drew on the pool and at "+
				"least %d bytes of it never came back",
				e.LeakProbeFinal.Elements, e.LeakProbeFinal.ModelHeld, e.LeakProbeFinal.Slack,
				e.LeakProbeFinal.Code, e.Abusers+1, e.LeakProbeFinal.Slack+1)))
	}
	return v
}

// -----------------------------------------------------------------------------
// The non-vacuity gate
// -----------------------------------------------------------------------------
//
// Every clause above is conditional on the run having CONSTRUCTED the state it
// adjudicates. A run in which the pool never fired satisfies "every refusal is
// typed" perfectly and proves nothing; a run whose honest client was served
// entirely before the pressure began satisfies "honest traffic stayed live" and
// proves nothing; a nesting family that produced one outcome instead of three
// makes the distinctness clause return early without a word. This gate is what
// turns each of those silences into a reported shortfall.

// boltDecodeMinRefusals is the fewest pool refusals a deterministic run must
// have observed. It is more than one because a single refusal cannot show the
// boundary was bracketed: the window contributes at least
// [boltDecodeBoundaryWindow] of them and the breach arm one more.
const boltDecodeMinRefusals = boltDecodeBoundaryWindow + 1

// checkBoltDecodePressureNonVacuity reports what the run failed to construct.
func checkBoltDecodePressureNonVacuity(e *BoltDecodeEvidence) []Violation {
	if e.Swarm {
		return checkBoltDecodeSwarmNonVacuity(e)
	}
	var v []Violation

	// The window's own counts and the run-wide ones are kept SEPARATE, because they
	// answer different questions and folding them together hides both. The window
	// question is whether the scan bracketed the transition; the run-wide one is
	// whether the pool fired and admitted at all. An earlier revision summed them,
	// and the consequence was measured: a window that had gone entirely green still
	// satisfied nv-window-spans-boundary, because the breach arm's single refusal
	// was being counted as if the scan had produced it.
	windowAccepted, windowRefused := 0, 0
	for i := range e.Window {
		if e.Window[i].Accepted {
			windowAccepted++
		} else {
			windowRefused++
		}
	}
	accepted, refused := windowAccepted, windowRefused
	for i := range e.Arms {
		if e.Arms[i].Elements < 0 {
			continue
		}
		if e.Arms[i].Reply == "FAILURE" {
			refused++
		} else {
			accepted++
		}
	}
	if windowAccepted == 0 || windowRefused == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-window-spans-boundary",
			fmt.Sprintf("the %d-probe window around the model's boundary (n=%d) produced %d accepted and "+
				"%d refused: it did not BRACKET the transition, so the boundary was never located and "+
				"pool-boundary-monotone had nothing to be monotone about",
				len(e.Window), e.ModelBoundary, windowAccepted, windowRefused)))
	}
	if refused < boltDecodeMinRefusals {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-pool-refusals-observed",
			fmt.Sprintf("the pool refused %d message(s), want at least %d: a run in which the aggregate "+
				"ceiling never fired satisfies every clause about how it fires",
				refused, boltDecodeMinRefusals)))
	}
	if accepted == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-pool-accepts-observed",
			"the pool admitted nothing: a ceiling that refuses everything also satisfies "+
				"'every refusal is typed', so an accepted message is what makes the refusals meaningful"))
	}
	v = append(v, checkBoltDecodeLeakProbeTight(e, e.LeakProbeFinal, "nv-leak-probe-tight")...)

	// The nesting family must be complete AND must have produced all three
	// outcomes; two of three collapses checkBoltDecodeCapsAnswerDifferently into a
	// no-op that reports nothing.
	seen := map[string]bool{}
	for i := range e.Arms {
		seen[e.Arms[i].Name] = true
	}
	for _, spec := range boltDecodeNestArms {
		if !seen[spec.name] {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-nesting-family-complete",
				fmt.Sprintf("nesting arm %q did not run", spec.name)))
		}
	}
	outcomes := map[string]int{}
	preauthRefusals := 0
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Depth == 0 {
			continue
		}
		switch {
		case a.Reply == "SUCCESS":
			outcomes["served"]++
		case a.Code == boltDecodeCodeInvalid:
			outcomes["wire cap"]++
		case a.Code == boltDecodeCodeInternal:
			outcomes["engine cap"]++
		}
		if a.Vehicle == "hello" && a.Reply == "FAILURE" {
			preauthRefusals++
		}
	}
	for _, want := range []string{"served", "wire cap", "engine cap"} {
		if outcomes[want] == 0 {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-nesting-three-outcomes",
				fmt.Sprintf("the nesting family produced no %q outcome (served=%d wire-cap=%d "+
					"engine-cap=%d): with fewer than three distinct answers the distinctness clause "+
					"returns without adjudicating anything",
					want, outcomes["served"], outcomes["wire cap"], outcomes["engine cap"])))
		}
	}
	if preauthRefusals == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-preauth-refusal-observed",
			"no over-nested HELLO was refused: the wire depth cap's whole justification is that it is "+
				"reachable during the FIRST HELLO decode, before anything can be authenticated, and a "+
				"family that only abuses RUN never visits that path"))
	}

	// The control must be a genuinely different configuration that genuinely
	// disagreed; a control with the same ceiling, or one that refused too, controls
	// for nothing.
	ctl := boltDecodeFindArm(e.Arms, "pool-control-raised-ceiling")
	breach := boltDecodeFindArm(e.Arms, "pool-breach-write")
	switch {
	case ctl == nil || breach == nil:
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-control-differs",
			"the control arm or the breach arm did not run"))
	case e.ControlBudget <= e.Budget:
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-control-differs",
			fmt.Sprintf("the control's ceiling (%d B) is not above the pressured one (%d B), so the two "+
				"servers are not an A/B on the ceiling at all", e.ControlBudget, e.Budget)))
	case ctl.Reply == breach.Reply:
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-control-differs",
			fmt.Sprintf("the control and the pressured server both answered %q for the same bytes: the "+
				"A/B did not separate, so nothing about the ceiling was isolated", ctl.Reply)))
	}
	if n := e.LiveCensus[boltDecodeFitLabel]; n == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-census-nonempty",
			fmt.Sprintf("the live census holds no :%s node, so 'the refused write left nothing behind' is "+
				"trivially true of a graph in which nothing was written at all", boltDecodeFitLabel)))
	}
	return v
}

// checkBoltDecodeLeakProbeTight requires the no-leak probe to be sensitive enough
// to detect a leak at all.
//
// A probe with generous slack passes whether or not the pool came back short, so
// the SLACK is the sensitivity and it has to be asserted. The gate also catches
// the reverse: a probe whose modelled hold EXCEEDS the ceiling was never going to
// be admitted, so its refusal would read as a leak that is not there.
func checkBoltDecodeLeakProbeTight(e *BoltDecodeEvidence, p BoltDecodeProbe, clause string) []Violation {
	if p.Slack < 0 {
		return []Violation{boltDecodeViolation(ViolationOracleDeviation, clause,
			fmt.Sprintf("the leak probe (n=%d) models a hold of %d B against a %d B ceiling: it does not "+
				"fit even a pristine pool, so its refusal says nothing about a leak",
				p.Elements, p.ModelHeld, e.Budget))}
	}
	if p.Slack >= boltDecodeLeakSensitivity {
		return []Violation{boltDecodeViolation(ViolationOracleDeviation, clause,
			fmt.Sprintf("the leak probe (n=%d) leaves %d B of slack against the %d B ceiling, at or over "+
				"the %d B sensitivity bound: a pool that failed to return up to that many bytes would "+
				"still admit it, so the probe cannot see the leak it exists to find",
				p.Elements, p.Slack, e.Budget, boltDecodeLeakSensitivity))}
	}
	return nil
}

// boltDecodeSwarmMinWide is how many WIDE honest exchanges a run must have driven.
//
// The overlap clause gates on the wide exchanges and on nothing else, because
// they are the only ones whose in-flight window is guaranteed to be wider than
// the interval between refusals. A run that drove none of them would satisfy the
// clause by having nothing to check, so the count is asserted separately.
const boltDecodeSwarmMinWide = boltDecodeSwarmHonestOps / boltDecodeSwarmWideEvery

// boltDecodeSwarmMinWideStraddles is how many of those wide exchanges must have
// straddled a refusal.
//
// Half of them, not all and not one. Measured under -race at the 50 ms hold: 4 of
// 4 wide exchanges straddled on every one of 25 seeds, with the narrowest wide
// window holding 9 to 11 refusals; the 100-seed soak sweep, which runs under
// heavier concurrent load, saw a WORST case of 6. A 20 ms hold had held only 2 or
// 3, which is what the widening bought. The hold is ~13x the measured
// inter-refusal interval (3.8 ms of honest in-flight time per refusal) and ~79x
// the median narrow exchange (633 us), which is why the wide ones straddle and the
// narrow ones mostly do not. Requiring all four would make one unlucky window a
// red run; requiring one would leave the clause resting on a single sample, which
// is not a threshold at all. Half keeps real falsification power with a measured
// margin behind it.
const boltDecodeSwarmMinWideStraddles = boltDecodeSwarmMinWide / 2

// boltDecodeSwarmMinRefusalsDuringHonest is how many refusals must fall inside
// the honest client's whole run. Measured 41 to 47 across the same 25 seeds under
// -race, so eight is a ~5x margin. It is the density claim that keeps the
// liveness oracle honest independently of the per-exchange overlap.
const boltDecodeSwarmMinRefusalsDuringHonest = 8

// checkBoltDecodeSwarmNonVacuity reports what the concurrent arm failed to
// construct.
func checkBoltDecodeSwarmNonVacuity(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	if e.AbuserRejected == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-rejections",
			fmt.Sprintf("%d abusers produced NO refusal (start barrier satisfied: %v): the pool was never "+
				"actually pressured, so every clause about how it refuses passed without being tested. "+
				"Each message models a hold of %d%% of the %d B pool, so two in flight at once should not "+
				"fit — if they now do, the sizing or the model needs re-deriving",
				e.Abusers, e.PressureStarted,
				boltDecodeSwarmChargeNum*100/boltDecodeSwarmChargeDen, e.Budget)))
	}
	if e.AbuserAccepted == 0 {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-accepts",
			fmt.Sprintf("all %d abusive messages were refused: a pool stuck at zero refuses everything and "+
				"satisfies 'every refusal is typed' exactly, so at least one message has to get through "+
				"for the refusals to mean the pool was FULL rather than BROKEN", e.AbuserRejected)))
	}
	wide, wideStraddled, narrowStraddled := 0, 0, 0
	for i := range e.Honest {
		h := &e.Honest[i]
		straddled := h.RejectionsAfter > h.RejectionsBefore
		switch {
		case !h.Wide:
			if straddled {
				narrowStraddled++
			}
		default:
			wide++
			if straddled {
				wideStraddled++
			}
		}
	}
	if wideStraddled < boltDecodeSwarmMinWideStraddles {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-overlap",
			fmt.Sprintf("only %d of %d WIDE honest exchanges STRADDLED a refusal, want at least %d "+
				"(each held its stream open for %s; %d narrow exchanges straddled; start barrier "+
				"satisfied: %v). A wide exchange's window is wider than the interval between refusals by "+
				"design, so a shortfall means honest service did not overlap the pressure window — the "+
				"run shows honest traffic working BEFORE or AFTER the abuse and never DURING it, which "+
				"is the only thing the liveness claim is about",
				wideStraddled, wide, boltDecodeSwarmMinWideStraddles, boltDecodeSwarmWideHold,
				narrowStraddled, e.PressureStarted)))
	}
	if e.RejectionsDuringHonest < boltDecodeSwarmMinRefusalsDuringHonest {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-pressure-density",
			fmt.Sprintf("only %d abusive message(s) were refused across the whole honest client's run, "+
				"want at least %d. The per-exchange overlap clause can be satisfied by one lucky window; "+
				"this one says the server was under pressure for the DURATION of honest service, and it "+
				"is the clause that makes the liveness claim non-vacuous when the other is thin",
				e.RejectionsDuringHonest, boltDecodeSwarmMinRefusalsDuringHonest)))
	}
	if wide < boltDecodeSwarmMinWide {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-wide-exchanges",
			fmt.Sprintf("the honest client ran %d wide exchanges, want at least %d: the overlap clause "+
				"gates on those alone, so a run without them certifies nothing about overlap",
				wide, boltDecodeSwarmMinWide)))
	}
	if len(e.Honest) != boltDecodeSwarmHonestOps {
		v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nv-swarm-honest-count",
			fmt.Sprintf("the honest client completed %d of %d exchanges: it stopped early, so the run "+
				"sampled less of the pressure window than it was meant to",
				len(e.Honest), boltDecodeSwarmHonestOps)))
	}
	v = append(v, checkBoltDecodeLeakProbeTight(e, e.LeakProbeFinal, "nv-swarm-leak-probe-tight")...)
	return v
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// String renders the evidence.
//
// Everything here is a function of the seed. Node ids never appear (a created
// node's hidden internal key is minted from a process-global counter, so an id is
// not seed-reachable — the trap rmp #2486's determinism test caught); the census
// is counts by label; the engine's internal-error text has its per-session id
// redacted before it is ever recorded; and the honest client's elapsed times,
// which are a property of the machine, are not rendered at all.
//
// The SWARM rendering is deliberately a census and not a sequence: which abuser
// won the pool on a given round is the scheduler's business and would differ run
// to run. It is rendered only when the arm FAILS, and the determinism test covers
// the deterministic scenario alone.
func (e *BoltDecodeEvidence) String() string {
	var b strings.Builder
	if e.Swarm {
		return e.renderSwarm()
	}
	fmt.Fprintf(&b, "bolt-decode-pressure seed=%#x ceiling=%dB control=%dB\n", e.Seed, e.Budget, e.ControlBudget)
	fmt.Fprintf(&b, "  boundary: model=%d measured=%d (hold = %d + len(query) + len(key) + %d*n)\n",
		e.ModelBoundary, e.MeasuredBoundary, boltDecodeRunFixedCost, boltDecodeListElemCost)
	for i := range e.Window {
		p := &e.Window[i]
		fmt.Fprintf(&b, "    n=%d hold=%dB slack=%+dB %s\n", p.Elements, p.ModelHeld, p.Slack, boltDecodeVerdict(p))
	}
	for i := range e.Arms {
		b.WriteString("  " + e.Arms[i].render() + "\n")
	}
	fmt.Fprintf(&b, "  leak probe: initial %s / final %s (slack %+dB, sensitivity %dB)\n",
		boltDecodeVerdict(&e.LeakProbeInitial), boltDecodeVerdict(&e.LeakProbeFinal),
		e.LeakProbeFinal.Slack, boltDecodeLeakSensitivity)
	fmt.Fprintf(&b, "  census live=%s recovered=%s control=%s\n",
		boltDecodeRenderCensus(e.LiveCensus), boltDecodeRenderCensus(e.RecoveredCensus),
		boltDecodeRenderCensus(e.ControlCensus))
	return b.String()
}

// renderSwarm renders the concurrent arm as invariant totals.
func (e *BoltDecodeEvidence) renderSwarm() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-decode-swarm seed=%#x ceiling=%dB abusers=%d rounds=[%d,%d] pressure-started=%v\n",
		e.Seed, e.Budget, e.Abusers, boltDecodeSwarmMinRounds, boltDecodeSwarmMaxRounds, e.PressureStarted)
	fmt.Fprintf(&b, "  abusive replies: %s (served=%d refused=%d, connections alive after=%d/%d)\n",
		boltDecodeRenderCensus(e.AbuserReplies), e.AbuserAccepted, e.AbuserRejected, e.AbuserAliveAfter, e.Abusers)
	served, straddled, wide, wideStraddled := 0, 0, 0, 0
	for i := range e.Honest {
		h := &e.Honest[i]
		if h.OK && h.Value == int64(h.Index) {
			served++
		}
		s := h.RejectionsAfter > h.RejectionsBefore
		if s {
			straddled++
		}
		if h.Wide {
			wide++
			if s {
				wideStraddled++
			}
		}
	}
	// The margin the wide clause rests on is the SMALLEST number of refusals any
	// wide window contained: 1 would mean the construction only just held, and a
	// large number means it held with room to spare. Reporting it is what keeps a
	// clause that stopped being robust visible before it starts failing.
	minWideRefusals := -1
	for i := range e.Honest {
		h := &e.Honest[i]
		if !h.Wide {
			continue
		}
		n := int(h.RejectionsAfter - h.RejectionsBefore)
		if minWideRefusals < 0 || n < minWideRefusals {
			minWideRefusals = n
		}
	}
	fmt.Fprintf(&b, "  honest: %d/%d served correctly, %d straddled a refusal "+
		"(wide: %d/%d, narrowest wide window held %d refusals; %d refusals across the honest run)\n",
		served, len(e.Honest), straddled, wideStraddled, wide, minWideRefusals, e.RejectionsDuringHonest)
	for _, te := range e.TransportErrors {
		b.WriteString("  transport loss: " + te + "\n")
	}
	fmt.Fprintf(&b, "  leak probe: %s (n=%d slack %+dB)\n",
		boltDecodeVerdict(&e.LeakProbeFinal), e.LeakProbeFinal.Elements, e.LeakProbeFinal.Slack)
	return b.String()
}

// render renders one arm.
func (a *BoltDecodeExchange) render() string {
	var b strings.Builder
	b.WriteString(a.Name + ": ")
	switch {
	case a.Depth > 0:
		fmt.Fprintf(&b, "%s %s-chain depth=%d wire=%dB ", a.Vehicle, a.Composite, a.Depth, a.WireBytes)
	case a.Elements >= 0:
		fmt.Fprintf(&b, "n=%d hold=%dB ", a.Elements, a.ModelHeld)
	}
	b.WriteString(a.Reply)
	if a.Code != "" {
		b.WriteString(" " + a.Code)
	}
	if a.Message != "" {
		b.WriteString(" | " + a.Message)
	}
	fmt.Fprintf(&b, " (session usable after: %v)", a.SessionUsable)
	return b.String()
}

// boltDecodeVerdict renders a probe's outcome, naming the code when refused.
func boltDecodeVerdict(p *BoltDecodeProbe) string {
	if p.Accepted {
		return "SERVED"
	}
	if p.Code == "" {
		return "REFUSED"
	}
	return "REFUSED " + p.Code
}

// boltDecodeRenderCensus renders a count map with its keys sorted, so the
// rendering does not depend on Go's map iteration order.
func boltDecodeRenderCensus(m map[string]int) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// -----------------------------------------------------------------------------
// The scenarios
// -----------------------------------------------------------------------------

// The catalogue defaults for the two scenarios. Each must DIFFER from its
// scenario's seed mix; see [boltDecodeSeedMix].
const (
	boltDecodePressureDefaultSeed = 0x2487_5701
	boltDecodeSwarmDefaultSeed    = 0x2487_57A1
)

// boltDecodePressureScenario drives the deterministic inbound-decode battery: the
// aggregate pool's admission boundary against a closed-form model of its
// arithmetic, the typed retryable refusal and the session that survives it, the
// atomicity of a refused write live and after recovery, a raised-ceiling control
// on the identical bytes, and the nesting family that separates the wire cap, the
// engine's parameter cap and the pool by the three different answers they give.
func boltDecodePressureScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltDecodePressure,
		Description: "Bolt aggregate inbound-decode pressure: the engine-wide decode pool must admit a " +
			"message exactly when a closed-form model of its per-slot charges says it fits, refuse the " +
			"next element count with a TYPED RETRYABLE TransientError that leaves the session READY " +
			"without a RESET, leave no trace of the write it refused either live or after WAL recovery, " +
			"serve the identical bytes when only the ceiling is raised, and answer the wire nesting cap, " +
			"the engine's parameter nesting cap and the pool with three DIFFERENT codes",
		Mode:        ModeDeterministic,
		DefaultSeed: boltDecodePressureDefaultSeed,
		run:         runBoltDecodePressureScenario,
	}
}

// boltDecodeSwarmScenario drives the concurrent sibling: K connections pushing
// large-collection parameters at one shared pool while an honest client works
// against the same server.
//
// It is registered separately because it is NOT bit-reproducible — which abuser
// holds the pool on a given round is the scheduler's decision, and that is
// precisely the state the deterministic scenario cannot reach, since every charge
// is released before its reply is written.
func boltDecodeSwarmScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltDecodeSwarm,
		Description: "Bolt inbound-decode pressure across connections: with each abusive message sized to " +
			"55% of one shared pool so that one fits and two cannot, every abuser must be either served " +
			"or refused with typed retryable backpressure and keep its connection, an honest client must " +
			"be served CORRECTLY throughout with its WIDE exchanges — the ones holding an open stream for " +
			"longer than the interval between refusals — STRADDLING refusals, and the pool " +
			"must return to full once the swarm quiesces",
		Mode:        ModeConcurrent,
		DefaultSeed: boltDecodeSwarmDefaultSeed,
		Connections: boltDecodeSwarmAbusers + 1,
		OpsPerConn:  boltDecodeSwarmMinRounds,
		Mix:         &ConcurrentMix{ReaderWeight: 1.0},
		run:         runBoltDecodeSwarmScenario,
	}
}

// runBoltDecodePressureScenario collects the deterministic evidence and
// adjudicates it against both the contract and the non-vacuity gate.
func runBoltDecodePressureScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltDecodePressure(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltDecodePressure(ev), checkBoltDecodePressureNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return boltDecodeReport(ScenarioBoltDecodePressure, ModeDeterministic, seed, v), nil
}

// runBoltDecodeSwarmScenario drives the concurrent arm.
func runBoltDecodeSwarmScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltDecodeSwarm(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltDecodePressure(ev), checkBoltDecodePressureNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return boltDecodeReport(ScenarioBoltDecodeSwarm, ModeConcurrent, seed, v), nil
}

// boltDecodeReport wraps violations in a scenario report.
func boltDecodeReport(name string, mode ExecMode, seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   name,
		Mode:       mode,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt inbound-decode pressure>"},
		Violations: v,
	}
}
