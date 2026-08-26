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
//     declared at cypher/api.go:4303 and enforced at :4256), which is a SECOND,
//     LOWER, INDEPENDENT cap on the same axis and which this task discovered by
//     measurement rather than by reading.
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
//	engine param cap (>32)         Neo.ClientError.Statement.ArgumentError        FAILED  (next message IGNORED)
//
// The third row was Neo.DatabaseError.General.UnknownError when this scenario
// first measured it, and saying so in a report is what got it FIXED: rmp #2570
// reclassified it as a client fault. This family's arms are what pinned the old
// answer, so the correction failed them on purpose and they were updated with the
// ticket rather than deleted.
//
// The two READY rows and the one FAILED row do NOT disagree, and the reason is
// worth stating because it looks like an inconsistency. The first two refuse
// during decode, in the serve loop, before the message reaches a session at all —
// there is no request to fail, so the session is untouched. The third refuses
// inside handleRun, after a message that decoded correctly was dispatched, which
// the Bolt state machine puts in FAILED. Staying READY is reserved for
// back-pressure, where retrying the same request can succeed (state.go); a depth
// refusal is deterministic, so retrying it cannot.
//
// The classification segment is load-bearing and is asserted on its own:
// neo4j-go-driver's IsRetriableTransient tests `classification == "TransientError"`
// (recorded at bolt/server/errors.go:129-131), so "typed retryable backpressure"
// is a checkable property of the code the server actually sent and not a vibe.
// The driver populates that field only for a code of EXACTLY four dot-separated
// segments, so the property is one of the code's SHAPE as well as of its second
// segment, and [boltDecodeClassification] mirrors that arity deliberately
// (rmp #2575). The pool
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
// then confirmed end to end at the nine ceilings from 2 MiB to 32 MiB the soak
// sweep drives (TestBoltDecodePool_SoakCeilingSweep), where it named the last
// accepted element count EXACTLY at every one. The scenario does not trust
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
	// boltDecodeCodeParamDepth is what the ENGINE'S parameter nesting cap returns.
	// It is a ClientError, so a driver does not retry it — correct, since retrying
	// an over-nested parameter fails identically — and it names the fault as the
	// client's, which is what rmp #2570 corrected: it was
	// Neo.DatabaseError.General.UnknownError, sanitised to generic internal-error
	// text, so the code said "server bug" about a wholly client-supplied payload.
	//
	// The Statement family rather than the Request family, because the message
	// DECODED and the statement was dispatched: what is invalid is the statement's
	// argument, not the form of the request. Request.Invalid belongs to the wire cap
	// above, which refuses a frame that will not decode.
	boltDecodeCodeParamDepth = "Neo.ClientError.Statement.ArgumentError"
)

// The exact message texts the two decode-layer refusals carry. A code alone is
// not enough: the generic Request.Invalid is shared with every other malformed
// message, so the pair (code, message) is what a client actually reads.
const (
	boltDecodeMsgBudget  = "server inbound decode memory limit reached; retry later"
	boltDecodeMsgInvalid = "malformed Bolt message"
	// boltDecodeMsgParamDepth is the WHOLE message the parameter cap now carries.
	// It used to be pinned as a PREFIX ("An internal error occurred.") because the
	// sanitised text ended in a per-session id that had to be redacted before
	// recording. Since rmp #2570 the message is a client-fault message that bypasses
	// the sanitiser, is a pure function of the parameter key and the limit, and
	// carries no session id — so it is pinned in full, which is strictly stronger.
	//
	// It names the key this family binds under, so a change to [boltDecodeParamKey]
	// must change this literal too. The nesting-message clause is what would catch
	// that, by name.
	boltDecodeMsgParamDepth = `cypher: ArgumentError.ParameterNestedTooDeep: parameter "d" is nested deeper than the supported limit of 32 levels`
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
	// boltDecodeParamDepthCap is cypher's maxParamBindDepth (declared at
	// cypher/api.go:4303, enforced at :4256), reached only by a message the decoder
	// already ACCEPTED.
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
// exchange is a WIDE one: it sends its RUN, holds the open stream without pulling,
// and only then drains. The exchange is genuinely in flight for that whole time —
// the server has acknowledged the statement and is holding a cursor for it.
//
// The hold is NOT set by waiting for a refusal to be counted. That would make
// any clause resting on it true by construction of the HARNESS instead of by
// behaviour of the SERVER, which is the failure mode a coverage gate exists to
// prevent. It is [boltDecodeSwarmWideHold] of wall time and then a wait on the
// FLEET'S OWN PROGRESS — round trips completed, of any outcome — which is a
// different thing entirely: see [boltDecodeSwarm.wideHold] for the distinction and
// for why a hold fixed in milliseconds cannot hold its meaning on a machine that
// slows the fleet down (rmp #2611).
//
// # What the width does NOT buy, measured (rmp #2596)
//
// The wide exchanges used to carry the overlap claim on their own: half of them
// had to straddle a refusal. That made a FIXED 50 ms budget the yardstick for a
// refusal cadence the machine controls, and it failed on a clean engine. Measured
// on the reference host, 96 swarm runs under 32 concurrent coverage-instrumented
// test binaries: 13 of 96 runs put only 1 of the 4 wide exchanges over a refusal
// (1/4 in 13 runs, 2/4 in 27, 3/4 in 37, 4/4 in 19), and those 13 were the only
// gate failures in the sweep. The cause is arithmetic: a wide window can only
// contain a refusal if one arrives inside it, and under load the fleet's refusals
// become both rarer (10 to 87 per run) and spread over a honest window that
// dilates with the load while the 50 ms hold, as it then stood, did not.
//
// So no COUNT of wide exchanges is adjudicated any more. The wide exchanges
// themselves stay, and they stay load-bearing: they are most of the honest client's
// total in-flight time, and that in-flight time is what an abusive round trip has
// to overlap for nv-swarm-overlap to count it. What went is the threshold on how
// many of them individually contained an event.
//
// What #2596 diagnosed and did not fix is that the hold's 50 ms did not dilate with
// the load while the fleet's round trip did, so under load a wide exchange stopped
// being able to contain a complete abusive round trip at all. rmp #2611 made the
// hold wait on the fleet's progress as well as on the clock; the 50 ms is now the
// floor rather than the whole hold.
const (
	boltDecodeSwarmWideEvery = 6
	// boltDecodeSwarmWideHold is the wall-clock FLOOR of a wide exchange's hold. It
	// is what decides the hold on an idle host, where an abusive round trip is
	// single-figure milliseconds and the fleet progress below is reached long before
	// this elapses.
	boltDecodeSwarmWideHold = 50 * time.Millisecond
	// boltDecodeSwarmWideFleetRounds is how many further abusive round trips the
	// fleet must COMPLETE before a wide exchange stops holding. One per abuser is
	// the smallest count that means "the whole fleet got a turn inside this
	// exchange" rather than "one member did", and it is what makes the hold dilate
	// with the fleet instead of against it (rmp #2611). See
	// [boltDecodeSwarm.wideHold].
	boltDecodeSwarmWideFleetRounds = boltDecodeSwarmAbusers
	// boltDecodeSwarmWideHoldStep is how often the hold samples the fleet's round
	// counter. It is coarser than [boltDecodeSwarmBarrierStep] because the quantity
	// it waits on is coarser: an abusive round trip is milliseconds at best and
	// hundreds of milliseconds under load, so a 100 us step would cost thousands of
	// timer wake-ups per hold to resolve an event that cannot arrive that often.
	boltDecodeSwarmWideHoldStep = time.Millisecond
	// boltDecodeSwarmWideHoldMax caps one hold. It bounds a wide exchange that is
	// waiting on a fleet which has stopped making progress; it is not a pace, being
	// two orders of magnitude above a measured abusive round trip and a thirtieth of
	// the [boltDecodeHonestBound] starvation bound the same exchange is charged
	// against. Expiries are counted and rendered.
	boltDecodeSwarmWideHoldMax = 1 * time.Second
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
	// read. After > Before means abusive traffic was REFUSED, AND COUNTED, while
	// this exchange was in flight.
	//
	// That is weaker than it looks and it is REPORTED, never adjudicated: an abuser
	// increments the counter only after decoding its reply, so a refusal issued
	// before this exchange opened can be counted inside it. See
	// [boltDecodeSwarmInFlightRefusals] for the measurement that establishes how
	// badly, and [BoltDecodeEvidence.RefusalsConcurrentWithHonest] for the
	// instrument nv-swarm-overlap uses instead.
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
	// [boltDecodeSwarmWideHold] between RUN and PULL. Its in-flight window is wide
	// enough to hold several refusals when they are arriving quickly, and the run
	// reports how many the narrowest one held; nothing thresholds it (rmp #2596).
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
	// connection down (see the floor arithmetic on [boltDecodeSwarmAbusers]), so a
	// non-empty list here is the honest report of the swarm having been sized wrong
	// — not a silent pass.
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
	// RefusalsConcurrentWithHonest is how many abusive messages were refused on a
	// round trip that OVERLAPPED a honest exchange's own flight.
	//
	// It is the overlap instrument, and it is an interval intersection rather than
	// an event-in-window test: the abuser samples the honest client's in-flight
	// state immediately before it writes and again at the moment it counts the
	// refusal, and both endpoints are published by the goroutine that owns them.
	// See [boltDecodeSwarm.abuser] for the sampling and
	// [checkBoltDecodeSwarmNonVacuity] for what the clause may and may not read
	// into it.
	RefusalsConcurrentWithHonest int64
	// RejectionsSpanningHonestWindow is how many abusive messages were refused on a
	// round trip that INTERSECTED the honest window — the interval from the first
	// honest exchange starting to the last one finishing.
	//
	// It is the quantity nv-swarm-pressure-density adjudicates, and it is an
	// interval intersection rather than a counter differenced across the window: the
	// abuser samples the honest window's published phase immediately before it
	// writes and again at the moment it counts the refusal. See
	// [boltDecodeSwarm.honestPhase] for the three phases and
	// [checkBoltDecodeSwarmNonVacuity] for why the counter difference it replaced
	// could not carry the claim.
	//
	// No FLOOR is ever put on it beyond nonzero. It is a count of events over an
	// interval whose length the machine controls, so any floor above that passes on
	// an idle host and fails on a busy one — which is exactly what two earlier
	// versions of nv-swarm-pressure-density did (rmp #2587).
	//
	// It cannot carry the overlap claim on its own: a fleet that took turns with the
	// honest client, never once refusing on a round trip that overlapped honest
	// work, would produce a large count here and witness nothing about concurrency.
	// Overlap is adjudicated on RefusalsConcurrentWithHonest above, and this total is
	// what gates it — a window that held no refusals at all is this clause's own
	// subject, not that one's.
	RefusalsSpanningHonestWindow int64
	// RejectionsDuringHonest is how many abusive messages the fleet-wide counter
	// ADVANCED BY between the honest client's own sample after the start barrier and
	// its deferred sample once the last exchange had finished.
	//
	// DIAGNOSTIC ONLY — no clause reads it any more, and rmp #2611 is why. It was
	// the density clause's instrument through two earlier versions, and a nonzero
	// floor on it was described here as the one floor that was safe. It is not: the
	// quantity is not a count of refusals drawn inside the window, it is a
	// difference between two reads of a counter an abuser increments only after
	// decoding its refusal reply. A refusal drawn on a round trip that straddles the
	// window's close is therefore counted AFTER the window and missed entirely, and
	// under load — where an abusive round trip outlasts the whole honest window —
	// that case stops being rare. Measured on a coverage-instrumented sweep of 100
	// seeds, 2 seeds per sweep read zero here while the fleet had drawn 3 refusals
	// and the overlap instrument had attributed 2 of them to honest FLIGHT: the
	// pressure was demonstrably live and this instrument could not see it.
	//
	// So this is the THIRD iteration of one defect. #2587 removed a numeric floor
	// (eight refusals) and then a positional one (one per segment) after both failed
	// on clean engines; what it left was a nonzero floor on this counter, which is
	// scheduler-dependent for a different reason — not the RATE of refusals but
	// WHERE the counter records them. The floor is gone with the instrument: the
	// claim now rests on RefusalsSpanningHonestWindow above.
	//
	// It is still rendered, and [boltDecodeSwarmInFlightRefusals] plus
	// [boltDecodeSwarmGapRefusals] is still exactly this number minus its tail, so
	// the three together keep showing where the counter recorded the pressure
	// relative to the honest client's own samples.
	RejectionsDuringHonest int64
	// WideHoldExpiries is how many WIDE honest exchanges reached
	// [boltDecodeSwarmWideHoldMax] before the fleet had completed the round trips
	// their hold was waiting for (see [boltDecodeSwarm.wideHold]).
	//
	// RENDERED and adjudicated nowhere, for the same reason as GateTimeouts below: a
	// threshold on it would be another floor that machine load moves. Zero is the
	// ordinary reading; a nonzero one says the fleet stalled, and the refusals that
	// run drew inside honest flight were coincidences again.
	WideHoldExpiries int64
	// GateTimeouts is how many times an abuser gave up waiting for a partner at the
	// fleet's rendezvous (see [boltDecodeSwarmGate]).
	//
	// It is RENDERED and adjudicated nowhere. Zero is the ordinary reading and every
	// regime measured for rmp #2611 returned zero; a nonzero one says the fleet
	// stopped pairing, so the refusals that run drew were coincidences again rather
	// than consequences of the construction. Thresholding it would put back exactly
	// the kind of load-moved floor this surface has now removed three times, so it
	// is reported as data and read by a human.
	GateTimeouts int64
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
// details (session: <16 hex>)." (bolt/server/session.go). The id is minted per
// connection and is NOT a function of the seed, so recording it verbatim would make
// the rendering vary run to run — the exact trap rmp #2486's own determinism test
// caught. The id is replaced rather than dropped so the report still shows that a
// session id was present.
//
// It is currently a NO-OP on every arm, and that is a consequence of rmp #2570: the
// parameter cap was the only refusal in this family that reached the sanitiser, and
// it now answers with a client-fault message that bypasses it. Every message this
// family records is therefore a pure function of the seed by construction. This is
// kept as insurance for a future arm that does draw a sanitised message, and the
// determinism test says so where its "the redaction really fired" clause used to be
// — that clause was retired rather than left to fail, because nothing can satisfy
// it any more.
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
		return "FAILURE", boltDecodeCodeParamDepth, false
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
//   - every honest exchange returned the value the harness computed for it, and a
//     refusal was drawn on an abusive round trip that overlapped one of them IN
//     FLIGHT, so honest service demonstrably overlapped the pressure window rather
//     than merely preceding or following it (see [boltDecodeSwarmInFlightRefusals]
//     for the instrument that could NOT carry that claim, and why);
//   - the pool comes back: a message sized to the whole ceiling is admitted once
//     the swarm quiesces.
//
// Each abuser's message is sized at [boltDecodeSwarmChargeNum]/[boltDecodeSwarmChargeDen]
// of the pool, so one fits and two cannot. See the floor arithmetic on
// [boltDecodeSwarmAbusers] for why that sizing also keeps the honest client clear
// of the reassembly reader, whose own budget breach is NOT the
// connection-preserving refusal the decode layer's is.
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

	s := &boltDecodeSwarm{
		srv:    srv,
		ev:     ev,
		abuseN: abuseN,
		gate:   newBoltDecodeSwarmGate(boltDecodeSwarmAbusers),
	}

	// MEASURE the boundary before the fleet starts, so the leak probe below is
	// sized to what this server actually admits rather than to what the model
	// predicts. See [boltDecodeSwarm.boundaryScan] for why that is what makes
	// nv-swarm-leak-probe-tight a statement about the server (rmp #2579).
	if err := s.boundaryScan(ctx); err != nil {
		return nil, err
	}

	if err := s.run(ctx, NewSeed(seed^boltDecodeSwarmSeedMix)); err != nil {
		return nil, err
	}
	// Read after every goroutine has joined, so no further increment is possible.
	ev.RefusalsConcurrentWithHonest = s.concurrent.Load()
	ev.RefusalsSpanningHonestWindow = s.spanning.Load()
	ev.GateTimeouts = s.gate.timeouts.Load()
	ev.WideHoldExpiries = s.wideHoldExpiries.Load()

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
	n := ev.MeasuredBoundary
	if n < 0 {
		// The scan did not bracket a transition, so there is no calibrated size.
		// Fall back to the model's and let nv-swarm-window-spans-boundary report the
		// shortfall — the fallback must never be taken SILENTLY, because it is
		// exactly what would turn this clause back into a harness guard.
		n = ev.ModelBoundary
	}
	p, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, n, ev.Budget)
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
	// rejected is the fleet-wide count of typed pool refusals, sampled by the honest
	// client either side of every exchange and once at each end of its whole run.
	//
	// It is a DIAGNOSTIC instrument and no longer an adjudicated one. The difference
	// across a single exchange does not prove a refusal was issued while that
	// exchange was in flight — an abuser increments it only after decoding its
	// reply, which the injected experiment on [boltDecodeSwarmInFlightRefusals]
	// shows is late enough to make that reading wrong — and rmp #2611 established
	// that the difference across the whole RUN carries the same lag: a refusal drawn
	// on a round trip that straddled the honest window's close is counted after the
	// window, so the difference reads zero on runs where the fleet demonstrably WAS
	// being refused alongside honest service. Both adjudicated quantities are
	// measured by the fleet instead; see [boltDecodeSwarm.concurrent] and
	// [boltDecodeSwarm.spanning].
	rejected atomic.Int64
	// honestDone is set once the honest client has finished its exchanges. The
	// abusers read it every round and stop, so the pressure window is guaranteed to
	// have OUTLASTED the honest window rather than merely overlapped it by luck.
	honestDone atomic.Bool
	// honestFlight is how many honest exchanges are in flight right now (0 or 1:
	// the honest client is sequential), and honestStarts counts how many it has
	// begun. The abusers read both either side of every message they send, which is
	// what lets a refusal be attributed to a round trip that OVERLAPPED honest work
	// rather than to one that merely followed it; see [boltDecodeSwarm.abuser].
	honestFlight atomic.Int64
	honestStarts atomic.Int64
	// concurrent counts refusals whose round trip overlapped honest flight. It is
	// the quantity nv-swarm-overlap adjudicates.
	concurrent atomic.Int64
	// honestPhase is the honest WINDOW's state, published by the honest client and
	// read by the fleet either side of every message: 0 before its first exchange
	// has begun, 1 while the window is open, 2 once it has closed. It only ever
	// moves forward, and it moves to 2 only from 1 — a run whose honest client never
	// opened a window leaves it at 0, so nothing can be attributed to a window that
	// does not exist.
	//
	// It is the density instrument's other endpoint. Sampling it before the write
	// and again at the moment a refusal is counted turns "was this refusal drawn
	// inside the honest window?" into an intersection of two intervals, each
	// endpoint published by the goroutine that owns it, instead of a comparison
	// against a counter that is incremented late (rmp #2611).
	honestPhase atomic.Int64
	// spanning counts refusals whose round trip INTERSECTED the honest window. It is
	// the quantity nv-swarm-pressure-density adjudicates.
	//
	// It dominates both of the other two by construction: a refusal counted inside
	// the window has its round trip ending inside the window, and a round trip that
	// overlapped honest FLIGHT necessarily overlapped the window that contains that
	// flight. So moving the clause onto it can only ever admit runs the counter
	// difference rejected, never reject runs it admitted.
	spanning atomic.Int64
	// fleetRounds counts abusive round trips COMPLETED, of any outcome. It is the
	// honest client's clock for a wide exchange's hold: see
	// [boltDecodeSwarm.wideHold] for why the hold is measured in fleet progress
	// rather than only in wall time.
	fleetRounds atomic.Int64
	// wideHoldExpiries counts wide holds that reached their wall-clock cap before
	// the fleet had made the progress they were waiting for.
	wideHoldExpiries atomic.Int64
	// gate is the fleet's rendezvous. Every abuser waits at it before every message
	// so that two of them are released together and the pool's arithmetic — not the
	// scheduler — decides that one of the two is refused. See
	// [boltDecodeSwarmGate].
	gate *boltDecodeSwarmGate
	// mu guards the evidence slices the abusers append to. The abusers' own results
	// are order-independent (the report is a census, never a sequence), so a plain
	// mutex is the right primitive and the ordering it does not provide is not
	// something any clause reads.
	mu     sync.Mutex
	abuseN int
}

// boundaryScan measures the shared pool's admission boundary against a PRISTINE
// pool, before any abuser has dialled.
//
// # Why the swarm measures a boundary at all (rmp #2579)
//
// The leak probe's SLACK is its sensitivity: a probe that leaves room to spare
// would be admitted by a pool that came back short, so nv-swarm-leak-probe-tight
// requires the slack to stay under [boltDecodeLeakSensitivity]. Sizing the probe
// from [boltDecodeModelElementsFor] alone made that requirement unfalsifiable:
// the model and the ceiling are both compile-time constants, so the slack was the
// constant 16 B on every run and no server behaviour could move it. The clause
// was accurate but decorative, and #2576 had to label it a harness guard.
//
// Measuring the boundary is what removes the label. The probe is now sized to the
// largest element count THIS server admitted, so a server that admits fewer
// elements than the model predicts — a changed packstream per-slot cost, a charge
// added or removed, a pool that starts short — moves the measured boundary down,
// widens the probe's slack by 48 B per element, and fires the clause. That is the
// same construction the deterministic scenario uses, and it is now the same kind
// of statement.
//
// # Why measuring here is sound, and does not race the fleet
//
// The scan runs before [boltDecodeSwarm.run] starts a single goroutine, so it is
// as sequential as the deterministic scenario's and observes the same pristine
// pool. Measuring it DURING the swarm would have been unsound — under aggregate
// pressure the boundary is whatever the other connections happen to be holding at
// that instant, which is the scheduler's business and not the pool's arithmetic —
// and that is precisely why it is measured before the fleet exists rather than
// alongside it.
//
// Every charge is released before its reply is written, and every accepted probe
// is drained, so the scan leaves the pool exactly as it found it: the fleet still
// meets a full pool, and the post-swarm probe still asks whether it came back.
// Nothing the scan records feeds any other clause — the swarm adjudicator reads
// neither Window nor MeasuredBoundary — so the measurement adds a boundary for
// the leak probe to stand on and changes no other verdict.
func (s *boltDecodeSwarm) boundaryScan(ctx context.Context) error {
	c, err := s.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-decode swarm boundary scan dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm boundary scan connect: %w", err)
	}
	if err := c.Conn().SetReadDeadline(time.Now().Add(boltDecodeArmBound)); err != nil {
		return fmt.Errorf("sim: bolt-decode swarm boundary scan deadline: %w", err)
	}
	// One connection carries the whole scan: a pool refusal is answered above the
	// session state machine, so it leaves the session READY, and every accepted
	// probe is drained.
	for d := -boltDecodeBoundaryWindow; d <= boltDecodeBoundaryWindow; d++ {
		n := s.ev.ModelBoundary + d
		if n < 0 {
			continue
		}
		p, err := boltDecodeSendProbe(c, boltDecodeProbeQuery, n, s.ev.Budget)
		if err != nil {
			return err
		}
		s.ev.Window = append(s.ev.Window, p)
		if p.Accepted && n > s.ev.MeasuredBoundary {
			s.ev.MeasuredBoundary = n
		}
	}
	return nil
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

// The fleet's rendezvous, which is what makes an abusive refusal arithmetic rather
// than a coincidence. See [boltDecodeSwarmGate].
const (
	// boltDecodeSwarmGatePairs is how many abusers are released together. Two is
	// what the pool's arithmetic needs — 2 x 55% is 110% of the ceiling — and no
	// more than the fleet can spare: a wider barrier would run every round at the
	// pace of the slowest member.
	boltDecodeSwarmGatePairs = 2
	// boltDecodeSwarmGateBound bounds one wait at the gate. It exists so a stalled
	// abuser cannot stall the fleet, not to pace it: a measured abusive round trip
	// is ~2 ms on an idle host and ~170 ms under 16 concurrent coverage-instrumented
	// binaries, so this bound is 2500x and 29x those, far above any pace it could
	// set. Expiries are counted in [BoltDecodeEvidence.GateTimeouts] and
	// rendered, because a fleet that stopped pairing is a fleet whose pressure went
	// back to being drawn rather than constructed.
	boltDecodeSwarmGateBound = 5 * time.Second
)

// The three states of [boltDecodeSwarm.honestPhase], which is the honest WINDOW's
// published endpoint pair. They are ordered and the phase only moves forward, so a
// pair of samples taken either side of an abusive round trip decides an interval
// intersection exactly: see [boltDecodeSwarm.abuser].
const (
	// boltDecodeHonestWindowPending is before the honest client's first exchange has
	// begun. A run that stays here never opened a window at all.
	boltDecodeHonestWindowPending int64 = 0
	// boltDecodeHonestWindowOpen is from the first exchange's wire operations
	// starting until the honest client returns.
	boltDecodeHonestWindowOpen int64 = 1
	// boltDecodeHonestWindowClosed is once the honest client has returned. It is
	// reached only from Open.
	boltDecodeHonestWindowClosed int64 = 2
)

// boltDecodeSwarmHonestPauseMaxUs bounds the honest client's inter-exchange
// pause, in microseconds. It exists so the honest exchanges SPREAD across the
// abusers' rounds instead of finishing in a burst before the pressure builds;
// the overlap clause still has to observe the spread rather than assume it.
const boltDecodeSwarmHonestPauseMaxUs = 2000

// boltDecodeSwarmGate is the fleet's rendezvous: an abuser waits at it before
// every message until [boltDecodeSwarmGatePairs] of them are waiting together, and
// they are then released in the same instant.
//
// # Why the fleet needs one at all (rmp #2611)
//
// The abusers already keep pushing until the honest client has finished, so the
// honest window is guaranteed to be crossed by abusive TRAFFIC. It was never
// guaranteed to be crossed by abusive PRESSURE, and those are not the same thing:
// each message models a hold of 55% of the pool, so a refusal needs two charges
// outstanding at once, and whether two free-running abusers overlap is the
// scheduler's decision. Measured on a coverage-instrumented sweep, a free-running
// fleet drew 3 refusals in 30 messages — a tenth of its traffic — and 2 seeds in
// every 100 had none of them land inside the honest window.
//
// Releasing two abusers together makes the refusal arithmetic rather than
// coincidence: 55% + 55% is 110% of the pool, so of any two messages the pool has
// admitted together at most one can stand. The gate constructs the CO-RESIDENCE;
// the server's refusal is still what the clause adjudicates, and a pool that
// admitted both would fail nv-swarm-rejections' sizing argument by name.
//
// # Why two and not the whole fleet
//
// Two is what the arithmetic needs, and it is what costs the fleet least. A
// four-way barrier would run the fleet at the pace of its slowest member every
// round, which under load is exactly when the honest window can least afford a
// slower fleet. A pair gate trips as soon as any two of the four are ready, so the
// other two keep moving.
//
// # Deadlock, and the bound
//
// An abuser that leaves — a transport fault, a cancelled context, the round floor
// reached with the honest client finished — calls [boltDecodeSwarmGate.leave],
// which lowers the live count and releases anyone the smaller fleet can no longer
// pair. A fleet of one never blocks. The wait is bounded anyway, because a bound
// that is never reached costs nothing and a missing one would turn a stalled
// abuser into a stalled scenario. The bound is generous against a measured round
// trip (single-figure milliseconds idle, under a second at 16x oversubscription)
// so it is not itself a scheduling gate, and every expiry is COUNTED and rendered:
// a run whose fleet never managed to pair is a run whose pressure was drawn rather
// than constructed, and that has to be visible rather than silent.
type boltDecodeSwarmGate struct {
	release  chan struct{}
	mu       sync.Mutex
	parties  int
	waiting  int
	timeouts atomic.Int64
}

// newBoltDecodeSwarmGate returns a gate for parties live abusers.
func newBoltDecodeSwarmGate(parties int) *boltDecodeSwarmGate {
	return &boltDecodeSwarmGate{release: make(chan struct{}), parties: parties}
}

// arrive blocks until enough abusers are waiting together, the context ends, or
// [boltDecodeSwarmGateBound] expires.
func (g *boltDecodeSwarmGate) arrive(ctx context.Context) {
	g.mu.Lock()
	ch := g.release
	g.waiting++
	if g.waiting >= g.threshold() {
		g.trip()
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	timer := time.NewTimer(boltDecodeSwarmGateBound)
	defer timer.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
		g.withdraw(ch)
	case <-timer.C:
		g.timeouts.Add(1)
		g.withdraw(ch)
	}
}

// leave drops one abuser from the live count and releases whoever the smaller
// fleet can no longer pair.
func (g *boltDecodeSwarmGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parties--
	if g.parties > 0 && g.waiting >= g.threshold() {
		g.trip()
	}
}

// threshold is how many arrivals trip the gate: the pair the arithmetic needs, or
// the whole fleet when fewer than that are left alive. The caller holds g.mu.
func (g *boltDecodeSwarmGate) threshold() int {
	if g.parties < boltDecodeSwarmGatePairs {
		return g.parties
	}
	return boltDecodeSwarmGatePairs
}

// trip releases the current generation and opens the next. The caller holds g.mu.
func (g *boltDecodeSwarmGate) trip() {
	g.waiting = 0
	ch := g.release
	g.release = make(chan struct{})
	close(ch)
}

// withdraw removes a waiter that gave up, but only from the generation it was
// actually waiting on: if that generation has already been released the waiter was
// counted out by trip, and decrementing again would let a later arrival trip the
// gate one short.
func (g *boltDecodeSwarmGate) withdraw(ch chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release == ch && g.waiting > 0 {
		g.waiting--
	}
}

// abuser drives one connection's rounds of oversized parameters.
func (s *boltDecodeSwarm) abuser(ctx context.Context, id int) error {
	// Drop out of the rendezvous however this returns, registered BEFORE the dial:
	// an abuser that never got a connection is one the rest of the fleet must stop
	// counting on, exactly like one that stopped pushing.
	defer s.gate.leave()

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
		// honest client that finished before the pressure built must still meet a
		// fleet that is pushing.
		if round >= boltDecodeSwarmMinRounds && s.honestDone.Load() {
			break
		}
		// Rendezvous, so this message and another abuser's are outstanding together
		// and the pool's arithmetic decides that one of them is refused. It is taken
		// BEFORE the samples below, so the sampled interval is the round trip itself
		// and never the wait for a partner.
		s.gate.arrive(ctx)
		// Sampled immediately before the write and again at the moment the refusal is
		// counted. Their disjunction below is exact: honest work was in flight at the
		// start of this round trip, or at its end, or an exchange began and ended
		// entirely inside it. phaseBefore is the same sampling for the honest WINDOW
		// rather than for a single exchange's flight.
		flightBefore, startsBefore := s.honestFlight.Load(), s.honestStarts.Load()
		phaseBefore := s.honestPhase.Load()
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
			if flightBefore > 0 || s.honestFlight.Load() > 0 || s.honestStarts.Load() > startsBefore {
				s.concurrent.Add(1)
			}
			// The window intersection, on the same two samples. The window was open
			// when this round trip started, or when its refusal was counted, or it
			// opened and closed entirely inside the round trip — which is the case the
			// counter difference cannot see at all, because the increment below lands
			// after the window has closed.
			if phaseAfter := s.honestPhase.Load(); phaseBefore == boltDecodeHonestWindowOpen ||
				phaseAfter == boltDecodeHonestWindowOpen ||
				(phaseBefore == boltDecodeHonestWindowPending && phaseAfter == boltDecodeHonestWindowClosed) {
				s.spanning.Add(1)
			}
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
		// Published last, so the round trip this counts is finished: it is the honest
		// client's clock for how long a wide exchange holds its stream open.
		s.fleetRounds.Add(1)
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
	// cannot leave the abusers pushing until their safety bound. The window closes
	// with it, and only if it was ever opened: CompareAndSwap rather than Store, so
	// a honest client that never ran an exchange leaves the phase at
	// [boltDecodeHonestWindowPending] and no refusal can be attributed to a window
	// that never existed.
	defer s.honestDone.Store(true)
	defer s.honestPhase.CompareAndSwap(boltDecodeHonestWindowOpen, boltDecodeHonestWindowClosed)

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
		// Published for the fleet, around the wire operations alone: the pause before
		// this exchange is NOT honest work in flight and must not be claimed as it.
		// The window opens with the FIRST exchange, for the same reason.
		s.honestPhase.CompareAndSwap(boltDecodeHonestWindowPending, boltDecodeHonestWindowOpen)
		s.honestStarts.Add(1)
		s.honestFlight.Add(1)
		if h.Wide {
			ok, got = boltDecodeWideExchange(c, int64(i), func() { s.wideHold(ctx) })
		} else {
			ok, got = boltDecodeFollowUp(c, int64(i))
		}
		s.honestFlight.Add(-1)
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

// wideHold is how long a WIDE honest exchange keeps its stream open: at least
// [boltDecodeSwarmWideHold] of wall time, and then until the fleet has completed
// [boltDecodeSwarmWideFleetRounds] more round trips than it had when the hold
// began, capped at [boltDecodeSwarmWideHoldMax].
//
// # Why the hold is measured in fleet progress and not only in wall time
//
// rmp #2596 diagnosed the erosion precisely and then only half-fixed it: "a wide
// window can only contain a refusal if one arrives inside it, and under load the
// fleet's refusals become both rarer and spread over a honest window that dilates
// with the load while the 50 ms hold does not". A fixed hold is a fixed number of
// MILLISECONDS offered to a fleet whose round trip takes a load-dependent number of
// milliseconds, so the number of abusive round trips a wide exchange can contain
// falls as the machine fills. Measured under 16 concurrent coverage-instrumented
// binaries: a run drew 13 refusals from 24 abusive messages — the fleet was being
// refused more than half the time it spoke — and exactly ONE of those refusals fell
// inside a honest window holding four 50 ms exchanges. The window had stopped being
// long enough to contain the fleet's round trips, and the clause that fired was not
// this arm's rate but an EXISTENCE claim, on a clean engine serving 24 of 24 honest
// exchanges correctly.
//
// Waiting on the fleet's own progress makes the hold dilate exactly as the fleet
// does. On an idle host an abusive round trip is single-figure milliseconds, so the
// wall-clock floor still decides and the arm behaves as it did; under load the hold
// grows with the thing it is meant to overlap.
//
// # What it deliberately does NOT wait for
//
// It waits for round trips COMPLETED, of any outcome — never for a refusal. Waiting
// for a refusal would make every clause resting on this hold true by construction
// of the HARNESS instead of by behaviour of the SERVER, which is the failure mode a
// coverage gate exists to prevent and is why the hold has never been set that way.
// What the construction supplies is the CO-RESIDENCE the pool's arithmetic needs;
// what the server supplies, and what the clauses adjudicate, is the refusal.
//
// The cap is a bound, not a pace: it is far above the round trip it waits on at
// every load measured — a factor of about 500 on an idle host, where an abusive
// round trip runs at ~2 ms, and of about 6 under 16 concurrent
// coverage-instrumented binaries, where it runs at ~170 ms — it is a thirtieth of
// the [boltDecodeHonestBound] starvation bound this hold is charged against, and
// every expiry is counted into
// [BoltDecodeEvidence.WideHoldExpiries] and rendered so a run whose holds gave up
// waiting is visible rather than silent.
func (s *boltDecodeSwarm) wideHold(ctx context.Context) {
	want := s.fleetRounds.Load() + boltDecodeSwarmWideFleetRounds
	time.Sleep(boltDecodeSwarmWideHold)
	deadline := time.Now().Add(boltDecodeSwarmWideHoldMax)
	for s.fleetRounds.Load() < want {
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(deadline) {
			s.wideHoldExpiries.Add(1)
			return
		}
		time.Sleep(boltDecodeSwarmWideHoldStep)
	}
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
func boltDecodeWideExchange(c *WireClient, want int64, hold func()) (bool, int64) {
	resp, err := c.Run("RETURN "+strconv.FormatInt(want, 10)+" AS v", nil)
	if err != nil {
		return false, -1
	}
	if kind, _, _ := boltDecodeReplyKind(resp); kind != "SUCCESS" {
		return false, -1
	}
	hold()
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
// the second of its EXACTLY four dot-separated parts — or "" when the code is not
// in that shape.
//
// The segment is what a driver's retry decision turns on: neo4j-go-driver's
// IsRetriableTransient tests `classification == "TransientError"`
// (bolt/server/errors.go:129-131). Reading it out of the OBSERVED code, rather
// than comparing the whole string to a literal, is what makes the retryability
// clause a statement about what the server sent instead of a restatement of the
// constant this file already declares.
//
// # Why the arity is EXACTLY four, and why that is a mirror (rmp #2575)
//
// The arity is not this harness's choice: it is copied from the driver whose
// behaviour the clause claims to predict. In
// github.com/neo4j/neo4j-go-driver/v5 v5.28.4, (*Neo4jError).parse
// (neo4j/db/errors.go:114-127) returns early on `len(parts) != 4` and therefore
// leaves the classification field the EMPTY string, so IsRetriableTransient
// (neo4j/db/errors.go:156-159, `return e.classification == "TransientError"`)
// reports false for any code that is not four segments long.
//
// A laxer guard here would have accepted arities the driver refuses. A regression
// emitting the three-segment "Neo.TransientError.OutOfMemoryError" reads as
// classification "TransientError" under a `len(parts) >= 2` rule and would have
// SATISFIED pool-refusal-retryable, while no real driver would ever retry it —
// which is precisely the property that clause exists to check. Mirroring the
// driver's arity is what keeps the clause's claim and the client's actual
// behaviour the same claim.
//
// The mirroring is deliberate, and it is a THIRD-PARTY contract that can drift
// under this module: if the driver changes its parse rule or its arity, this
// guard must be RE-DERIVED by reading the dependency at the version go.mod pins,
// never reasoned about from memory. The consequence is pinned by the
// "backpressure whose code the real driver cannot classify at all" case in
// TestBoltDecodePressure_ContractCanFail.
func boltDecodeClassification(code string) string {
	parts := strings.Split(code, ".")
	// Mirrors neo4j-go-driver v5.28.4 neo4j/db/errors.go:121-123 exactly; see the
	// godoc above before relaxing this.
	if len(parts) != 4 {
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
	// A FIXED order, not a map literal's: two runs of one seed must render the same
	// report, and Go randomises map iteration, so a map here would have permuted a
	// failing run's violations between runs at the same seed.
	for _, c := range []struct {
		name   string
		counts map[string]int
	}{{"live", e.LiveCensus}, {"recovered", e.RecoveredCensus}} {
		census, counts := c.name, c.counts
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
	// A guard on the HARNESS, not on the server: [boltDecodeRunner.armControlRaisedCeiling]
	// replays breach.Elements verbatim, so the two counts are one variable read twice
	// inside a single run and no server behaviour can separate them. It is kept
	// because the wiring it guards is exactly what makes the control a control.
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
		// A guard on the HARNESS, not on the server: every nesting payload is built
		// here, so its wire size is a function of this file's own constants and of the
		// seeded depth draw alone. The largest one the family can reach is the
		// far-over-cap RUN at depth boltDecodeFarDepthMin+boltDecodeFarDepthSpan-1,
		// which is 6166 bytes — an order of magnitude under the ceiling. It is kept
		// because it is the clause that would fire if the family were ever re-sized
		// past the point where the anti-confound argument still holds.
		if a.WireBytes >= boltDecodeNestingWireCeiling {
			v = append(v, boltDecodeViolation(ViolationVacuousRun, "nesting-not-by-size",
				fmt.Sprintf("%s is %d wire bytes, at or over the %d B anti-confound ceiling: a payload "+
					"this large could have been refused for its SIZE, so it proves nothing about a DEPTH cap",
					a.Name, a.WireBytes, boltDecodeNestingWireCeiling)))
		}
		if wantCode == boltDecodeCodeInvalid && a.Message != boltDecodeMsgInvalid {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-message",
				fmt.Sprintf("%s carries message %q, want %q", a.Name, a.Message, boltDecodeMsgInvalid)))
		}
		// The WHOLE message, not a prefix. Before rmp #2570 the sanitised text ended
		// in a per-session id, so only its prefix could be pinned; the client-fault
		// message that replaced it is a pure function of the parameter key and the
		// limit, so pinning it entire is strictly stronger and catches a message that
		// named the wrong key or the wrong number.
		if wantCode == boltDecodeCodeParamDepth && a.Message != boltDecodeMsgParamDepth {
			v = append(v, boltDecodeViolation(ViolationOracleDeviation, "nesting-message",
				fmt.Sprintf("%s carries message %q, want %q", a.Name, a.Message, boltDecodeMsgParamDepth)))
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
// A server that answered aggregate memory pressure, a stack-overflow attempt and
// an over-nested parameter with one code would be indistinguishable, from the
// client's side, from a server with no aggregate pool and no depth cap at all.
//
// This clause is not the only thing standing between the run and such a server,
// and it does not need to be. [checkBoltDecodeNesting] pins each nesting arm's
// exact expected code and [checkBoltDecodeRefusalTyping] pins the budget literal,
// so as long as the three code constants stay pairwise distinct, a server-side
// collapse necessarily moves an observed code off the literal it is pinned to,
// and one of those two fires first.
//
// What this clause adds is what those literals cannot give. Every other code
// clause here compares an observed code against one of this file's own constants;
// this one compares the observed codes with EACH OTHER, and against no constant
// at all. It is therefore the only distinctness statement that keeps its meaning
// if the three constants are themselves edited to collide — which is the defect
// their own godoc warns against, and the reason they must stay pairwise distinct.
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
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-window-spans-boundary",
			fmt.Sprintf("the %d-probe window around the model's boundary (n=%d) produced %d accepted and "+
				"%d refused: it did not BRACKET the transition, so the boundary was never located and "+
				"pool-boundary-monotone had nothing to be monotone about",
				len(e.Window), e.ModelBoundary, windowAccepted, windowRefused)))
	}
	if refused < boltDecodeMinRefusals {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-pool-refusals-observed",
			fmt.Sprintf("the pool refused %d message(s), want at least %d: a run in which the aggregate "+
				"ceiling never fired satisfies every clause about how it fires",
				refused, boltDecodeMinRefusals)))
	}
	if accepted == 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-pool-accepts-observed",
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
	// A guard on the HARNESS, not on the server: [boltDecodeRunner.driveNestArm]
	// records an arm for every spec or returns an error, and an error aborts the run
	// before anything is adjudicated, so at this point the roster is always complete.
	// It is kept because it is what would fire if an arm were ever recorded
	// conditionally rather than unconditionally.
	for _, spec := range boltDecodeNestArms {
		if !seen[spec.name] {
			v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-nesting-family-complete",
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
		case a.Code == boltDecodeCodeParamDepth:
			outcomes["engine cap"]++
		}
		if a.Vehicle == "hello" && a.Reply == "FAILURE" {
			preauthRefusals++
		}
	}
	for _, want := range []string{"served", "wire cap", "engine cap"} {
		if outcomes[want] == 0 {
			v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-nesting-three-outcomes",
				fmt.Sprintf("the nesting family produced no %q outcome (served=%d wire-cap=%d "+
					"engine-cap=%d): with fewer than three distinct answers the distinctness clause "+
					"returns without adjudicating anything",
					want, outcomes["served"], outcomes["wire cap"], outcomes["engine cap"])))
		}
	}
	if preauthRefusals == 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-preauth-refusal-observed",
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
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-control-differs",
			"the control arm or the breach arm did not run"))
	// This branch is a guard on the HARNESS, not on the server: both ceilings are
	// compile-time constants (boltDecodePressuredBudget, 4 MiB, against
	// boltDecodeControlBudget, 64 MiB), so it can fire only if one of them is edited
	// to meet the other. The two branches around it are not: they read what the run
	// and the server actually did.
	case e.ControlBudget <= e.Budget:
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-control-differs",
			fmt.Sprintf("the control's ceiling (%d B) is not above the pressured one (%d B), so the two "+
				"servers are not an A/B on the ceiling at all", e.ControlBudget, e.Budget)))
	case ctl.Reply == breach.Reply:
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-control-differs",
			fmt.Sprintf("the control and the pressured server both answered %q for the same bytes: the "+
				"A/B did not separate, so nothing about the ceiling was isolated", ctl.Reply)))
	}
	if n := e.LiveCensus[boltDecodeFitLabel]; n == 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-census-nonempty",
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
		return []Violation{boltDecodeViolation(ViolationVacuousRun, clause,
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
// Nothing thresholds how many of them straddled a refusal any more — that was the
// rate rmp #2596 removed — but the run still has to DRIVE them, and the overlap
// clause still depends on them. At [boltDecodeSwarmWideHold] each they are most of
// the honest client's in-flight time, and that is the time an abusive round trip has
// to overlap for nv-swarm-overlap to count it: a run reduced to narrow exchanges
// would shrink it by an order of magnitude. They are also the shape the report's
// margin line is computed from.
const boltDecodeSwarmMinWide = boltDecodeSwarmHonestOps / boltDecodeSwarmWideEvery

// boltDecodeSwarmPressureSegments is how many contiguous segments the honest
// client's run is split into FOR REPORTING. Nothing thresholds it.
//
// It exists because the per-segment distribution is the diagnostic that made two
// successive versions of this clause visibly wrong, so it is rendered on every
// failing swarm run — but it must never be adjudicated. See
// [checkBoltDecodeSwarmNonVacuity] for why, and what the density clause asserts
// instead.
const boltDecodeSwarmPressureSegments = boltDecodeSwarmMinWide

// boltDecodeSwarmSegmentRefusals splits the honest exchanges into
// [boltDecodeSwarmPressureSegments] contiguous segments and reports how many
// abusive messages the fleet had refused inside each.
//
// DIAGNOSTIC ONLY — no clause thresholds this. Measured on a correct engine under
// 32 concurrent -race test binaries, it returns shapes like [2 0 0 0]: three
// empty segments while the server was serving 24 of 24 honest exchanges
// correctly. See [checkBoltDecodeSwarmNonVacuity].
//
// A segment's count is read from the per-exchange counter samples, so it covers
// the pauses between exchanges as well as the exchanges themselves. The gap
// between one segment's last sample and the next segment's first is attributed to
// neither, which can only understate a segment and never invent one.
//
// It returns nil when the run holds fewer exchanges than segments; that is a
// honest client that stopped early, which nv-swarm-honest-count reports.
func boltDecodeSwarmSegmentRefusals(honest []BoltDecodeHonest) []int64 {
	if len(honest) < boltDecodeSwarmPressureSegments {
		return nil
	}
	out := make([]int64, boltDecodeSwarmPressureSegments)
	for s := range out {
		lo := s * len(honest) / boltDecodeSwarmPressureSegments
		hi := (s + 1) * len(honest) / boltDecodeSwarmPressureSegments
		out[s] = honest[hi-1].RejectionsAfter - honest[lo].RejectionsBefore
	}
	return out
}

// boltDecodeSwarmInFlightRefusals is how many abusive refusals were OBSERVED while
// a honest exchange was in flight, judged by the fleet-wide counter sampled either
// side of each exchange.
//
// DIAGNOSTIC ONLY — no clause thresholds it, and the section below is why. The
// overlap claim is adjudicated on
// [BoltDecodeEvidence.RefusalsConcurrentWithHonest] instead, which the abusers
// measure at their own round trips rather than inferring from this counter.
//
// It is reported because it is the visible half of the arithmetic a reader expects:
// this quantity plus [boltDecodeSwarmGapRefusals] is exactly
// `RejectionsDuringHonest` minus whatever the counter recorded after the last
// exchange closed, so the three together show where the counter recorded the
// pressure relative to the honest client's own samples. None of the three is
// adjudicated; see [BoltDecodeEvidence.RejectionsDuringHonest] (rmp #2611).
//
// # Why nothing thresholds it: measured on the shape it replaced
//
// Requiring half of the four wide exchanges to have straddled a refusal — the
// clause rmp #2596 removed — asked each fixed 50 ms window separately to contain an
// event, which is a rate. Over 96 swarm runs under 32 concurrent
// coverage-instrumented test binaries on the reference host, 13 failed that floor
// on a clean engine and nothing else in the gate fired. Widening it to "any refusal
// observed inside any exchange" was measured too, and is not enough either: in the
// whole-module coverage regime the gate actually runs in, that quantity came back
// as 1 on 2 of 4 runs. A coverage clause whose measured minimum is a single event
// has no margin at all.
//
// # What no clause built on this counter can claim, MEASURED
//
// The counter is incremented by an abuser goroutine AFTER its refusal reply has
// been decoded, and the server released the budget and wrote that reply strictly
// earlier. So each refusal is counted some unbounded, load-dependent time after it
// happened, and an in-flight attribution is "OBSERVED while in flight", never
// "issued while in flight".
//
// That lag is not a rounding error, it DOMINATES. Injected experiment (rmp #2596):
// the fleet was made to hold off sending while any honest exchange was in flight,
// so that no refusal could possibly be issued during one — the exact behaviour a
// server serialising honest statements against the fleet would show. The
// instrument still attributed 16 to 25 refusals per run to honest flight and still
// reported 4 of 4 wide exchanges straddling, and every clause in the gate stayed
// green over 6 runs. An abusive round trip carries 4.5 MiB and outlasts a honest
// exchange, so refusals it produced land in the counter well after the exchange
// they were concurrent with has closed.
//
// So this counter cannot carry the claim — and rmp #2611 established that neither
// can the same counter differenced across the whole honest window, which is what
// nv-swarm-pressure-density read until then. What replaced BOTH does not ask WHEN a
// refusal was counted at all: each abuser samples the honest client's in-flight
// state (and the honest window's phase) immediately before it writes and again at
// the moment it counts the refusal,
// and a refusal is attributed to honest work only when those two samples bracket a
// honest exchange's own flight. That is an intersection of two intervals, each
// endpoint published by the goroutine that owns it, instead of an event compared
// against a window it was recorded into late. Measured on an uninstrumented host it
// attributes 413 to 432 of a run's 424 to 439 window refusals, against roughly half
// for this counter — and under the injected experiment above it is the instrument
// that can go to zero, because the fleet and the honest client really did take
// turns.
//
// Sharper still would have to come from the server, and there is nothing to hand:
// packstream.InboundBudget (bolt/packstream/inbound_budget.go) is an atomic counter
// with no observer, and internal/metrics is a process-global sink another scenario
// may already have replaced, so routing through it would be cross-scenario
// contamination rather than a measurement.
func boltDecodeSwarmInFlightRefusals(honest []BoltDecodeHonest) int64 {
	var n int64
	for i := range honest {
		if d := honest[i].RejectionsAfter - honest[i].RejectionsBefore; d > 0 {
			n += d
		}
	}
	return n
}

// boltDecodeSwarmGapRefusals is how many abusive messages were refused in the gaps
// BETWEEN honest exchanges: after one exchange's closing sample and before the
// next one's opening sample.
//
// DIAGNOSTIC ONLY — no clause thresholds it. It is rendered on every failing swarm
// run because it is the other half of the same total, and the two together are
// what make a shortfall in [boltDecodeSwarmInFlightRefusals] readable: pressure
// that stopped altogether looks nothing like pressure that kept going while the
// honest client happened to be descheduled.
func boltDecodeSwarmGapRefusals(honest []BoltDecodeHonest) int64 {
	var n int64
	for i := 1; i < len(honest); i++ {
		if d := honest[i].RejectionsBefore - honest[i-1].RejectionsAfter; d > 0 {
			n += d
		}
	}
	return n
}

// checkBoltDecodeSwarmNonVacuity reports what the concurrent arm failed to
// construct.
//
// # What nv-swarm-pressure-density asserts, and what it deliberately does not
//
// It asserts only that the honest window lay INSIDE the pressure window: the start
// barrier was satisfied, and at least one abusive round trip that overlapped the
// honest window was refused. That is weaker than two earlier versions of this
// clause, and the weakening is deliberate and measured (rmp #2587).
//
// # The instrument, and why the counter difference it replaced could not carry it
//
// The clause reads [BoltDecodeEvidence.RefusalsSpanningHonestWindow], which the
// FLEET measures: each abuser samples the honest window's published phase
// immediately before it writes and again at the moment it counts a refusal, and
// the refusal is attributed to the window when those two samples intersect it.
//
// It used to read the difference between two samples of the fleet-wide refusal
// counter taken by the honest client itself, and rmp #2611 established that this
// cannot work. An abuser increments that counter only after decoding its refusal
// reply, so a refusal drawn on a round trip that straddles the window's CLOSE is
// recorded after the window and missed. Under load an abusive round trip outlasts
// the entire honest window, so the case is not rare: on a coverage-instrumented
// sweep of 100 seeds, 2 seeds per sweep read ZERO across the window while the
// fleet had drawn 3 refusals and the overlap instrument — which was already an
// interval intersection, because rmp #2596 had fixed exactly this lag for the
// overlap claim and did not touch the density one — had attributed 2 of them to
// honest FLIGHT. Every honest exchange was served correctly and every abuser
// connection survived. That is a clean engine failing a coverage gate for the
// third time on one root cause.
//
// The new quantity DOMINATES both of the others by construction: a refusal counted
// inside the window ended inside the window, and a round trip overlapping honest
// FLIGHT overlaps the window containing it. So the move can only admit runs the
// old instrument rejected and can never reject one it admitted.
//
// The clause used to threshold HOW MANY refusals landed in the honest window
// (eight), and then WHERE they landed (at least one in each of four segments).
// Both are density claims, and density here is a rate over an interval whose
// length the machine partly controls: the honest window then contained four
// FIXED [boltDecodeSwarmWideHold] sleeps, which did not dilate under load, while
// the fleet's refusal rate did. (rmp #2611 has since made those holds wait on the
// fleet's own progress too — see [boltDecodeSwarm.wideHold] — which is why the
// window now dilates with the fleet; it does not bring the thresholds back, because
// a rate over an interval is still a rate.) Both versions therefore failed
// `make ci` on an engine that had done nothing wrong. Measured on the reference
// host under -race, same 12 seeds per regime:
//
//	regime                          wall/run    fleet msgs   refused during honest
//	idle                            0.67 s      73..82       56..65
//	12 concurrent -race binaries     2.0 s      28..43       16..32
//	32 concurrent -race binaries    19..23 s    24           2..3
//
// At 32 binaries the per-segment distribution reads [2 0 0 0] on a correct engine
// — three of four segments empty — while all 24 honest exchanges are still served
// correctly and every abuser connection survives. So an empty stretch of honest
// service is ORDINARY under load, not evidence of anything, and no threshold on
// where refusals fall can survive.
//
// What the clause therefore CANNOT detect: pressure that stops part-way through
// honest service. That is a real gap, accepted knowingly, because every instrument
// that could see it is one the machine's load moves further than the defect does
// — the trade docs/test-layers.md already forces for wall clock.
//
// What it CAN still detect is the vacuity it was written for: a run whose honest
// client was served entirely outside the pressure window — no abusive round trip
// overlapping it drew a refusal at all. On the instrument it now reads, that
// requires the fleet to have been admitted (or to have stopped) throughout a window
// it is pushing across by construction, which is a statement about the SERVER
// rather than about when the scheduler ran the fleet.
//
// The per-segment distribution and the total are RENDERED on every failing run
// (see [BoltDecodeEvidence.renderSwarm]) so the erosion stays visible as data.
//
// # How nv-swarm-overlap differs from it, and why both are needed
//
// The density clause asks whether the honest window held any refusal at all.
// nv-swarm-overlap asks a strictly harder question about the same events: whether
// any of them was drawn on a round trip that OVERLAPPED a honest exchange's own
// flight, rather than starting and finishing between two of them (see
// [BoltDecodeEvidence.RefusalsConcurrentWithHonest]).
//
// The two are not one predicate, and the difference is the arm's whole reason to
// exist. A fleet and a honest client that took turns would satisfy density
// perfectly: the pressure was there, it just never coincided with honest work. That
// is what a server serialising honest statements against the fleet would look like,
// and it is reachable only by a concurrent arm — every other clause in this file a
// deterministic script could satisfy.
//
// Overlap can therefore fail while density passes, which is proved by fixture in
// [TestBoltDecodeSwarm_OverlapIsNotTheDensityClause] and by injection in the
// harness experiment recorded on [boltDecodeSwarmInFlightRefusals]. What it cannot
// do is fail while density fails: it is gated on the window having held refusals at
// all, so one cause is never reported as two findings.
//
// Neither clause thresholds a rate. Density asks that the window's total is
// nonzero; overlap asks that the overlapping count is nonzero. rmp #2596 removed
// the last rate in this gate, which was a floor of 2 on how many of the four wide
// exchanges individually contained a refusal.
//
// The two counts are close but neither contains the other. Overlap counts refusals
// drawn on a round trip that overlapped honest flight, and such a round trip can
// have its refusal counted after the honest window has closed — measured, one
// loaded run reported 9 overlapping against a window total of 6. Both counts were
// measured together: the overlapping count's minimum was 4 over 96 runs under 32
// concurrent coverage-instrumented binaries, 386 over a 100-seed sweep on an idle
// host, and it ran at about 97% of the window total there.
func checkBoltDecodeSwarmNonVacuity(e *BoltDecodeEvidence) []Violation {
	var v []Violation
	if e.AbuserRejected == 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-rejections",
			fmt.Sprintf("%d abusers produced NO refusal (start barrier satisfied: %v): the pool was never "+
				"actually pressured, so every clause about how it refuses passed without being tested. "+
				"Each message models a hold of %d%% of the %d B pool, so two in flight at once should not "+
				"fit — if they now do, the sizing or the model needs re-deriving",
				e.Abusers, e.PressureStarted,
				boltDecodeSwarmChargeNum*100/boltDecodeSwarmChargeDen, e.Budget)))
	}
	if e.AbuserAccepted == 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-accepts",
			fmt.Sprintf("all %d abusive messages were refused: a pool stuck at zero refuses everything and "+
				"satisfies 'every refusal is typed' exactly, so at least one message has to get through "+
				"for the refusals to mean the pool was FULL rather than BROKEN", e.AbuserRejected)))
	}
	wide := 0
	for i := range e.Honest {
		if e.Honest[i].Wide {
			wide++
		}
	}
	// Gated on there having BEEN pressure inside the honest window. A run whose
	// window held no refusal at all is nv-swarm-pressure-density's subject, and
	// reporting it here as well would name one cause twice while looking like two
	// findings. This clause answers only the next question: given that the window
	// held refusals, did any of them coincide with honest work in flight?
	if e.RefusalsConcurrentWithHonest <= 0 && e.RefusalsSpanningHonestWindow > 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-overlap",
			fmt.Sprintf("NOT ONE of the %d refusals drawn while the honest client was running was drawn "+
				"on a round trip that OVERLAPPED a honest exchange's own flight: every one of them "+
				"started and finished between two honest exchanges (%d were observed inside an exchange "+
				"and %d in the gaps between them; %d wide exchanges each held their stream open for %s; "+
				"per segment %v; start barrier satisfied: %v). The fleet and the honest client then took "+
				"turns instead of overlapping, and this arm exists precisely to drive them at the same "+
				"time — a deterministic script can reach everything else it asserts",
				e.RefusalsSpanningHonestWindow, boltDecodeSwarmInFlightRefusals(e.Honest),
				boltDecodeSwarmGapRefusals(e.Honest), wide, boltDecodeSwarmWideHold,
				boltDecodeSwarmSegmentRefusals(e.Honest), e.PressureStarted)))
	}
	// The honest window must lie INSIDE the pressure window: pressure demonstrably
	// under way before honest service began, and still live once it had. Both halves
	// are needed — the first alone admits a run whose pressure had finished by the
	// time the honest client started, and the second alone admits one whose honest
	// client ran first and met the pressure only at the end.
	if !e.PressureStarted {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-pressure-density",
			fmt.Sprintf("the honest client's start barrier EXPIRED after %s without a single abusive "+
				"message having been refused, so honest service did not begin inside the pressure "+
				"window at all. Every clause about honest traffic staying live under backpressure is "+
				"then a statement about an UNPRESSURED server", boltDecodeSwarmStartBarrier)))
	}
	if e.RefusalsSpanningHonestWindow <= 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-pressure-density",
			fmt.Sprintf("NOT ONE abusive round trip that overlapped the honest window — from the first "+
				"exchange starting to the last one finishing — was refused (the counter difference across "+
				"the window read %d; per segment: %v; start barrier satisfied: %v; the fleet drew %d "+
				"refusals in total, %d of them on a round trip overlapping a honest exchange's own "+
				"flight). The pressure had stopped by the time honest service ran, so 'honest traffic "+
				"stays live under backpressure' was never actually put to the test",
				e.RejectionsDuringHonest, boltDecodeSwarmSegmentRefusals(e.Honest), e.PressureStarted,
				e.AbuserRejected, e.RefusalsConcurrentWithHonest)))
	}
	if wide < boltDecodeSwarmMinWide {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-wide-exchanges",
			fmt.Sprintf("the honest client ran %d wide exchanges, want at least %d: at %s each they are "+
				"most of the in-flight time an abusive round trip has to overlap for the overlap clause "+
				"to count it, so a run without them shrinks that time by an order of magnitude and "+
				"certifies far less about overlap than the clause reads as claiming",
				wide, boltDecodeSwarmMinWide, boltDecodeSwarmWideHold)))
	}
	if len(e.Honest) != boltDecodeSwarmHonestOps {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-honest-count",
			fmt.Sprintf("the honest client completed %d of %d exchanges: it stopped early, so the run "+
				"sampled less of the pressure window than it was meant to",
				len(e.Honest), boltDecodeSwarmHonestOps)))
	}
	// The pre-fleet scan has to have BRACKETED the transition, because the leak
	// probe is sized from it. Without this clause the "no calibrated size" fallback
	// in [RunBoltDecodeSwarm] would quietly re-size the probe to the MODEL's
	// boundary, and the clause below would go back to reading a constant while
	// still looking like a measurement (rmp #2579).
	swarmWindowAccepted, swarmWindowRefused := 0, 0
	for i := range e.Window {
		if e.Window[i].Accepted {
			swarmWindowAccepted++
		} else {
			swarmWindowRefused++
		}
	}
	if swarmWindowAccepted == 0 || swarmWindowRefused == 0 || e.MeasuredBoundary < 0 {
		v = append(v, boltDecodeViolation(ViolationVacuousRun, "nv-swarm-window-spans-boundary",
			fmt.Sprintf("the pre-fleet scan accepted %d and refused %d of %d probes (measured boundary %d): "+
				"it did not bracket the pool's admission transition, so there is no MEASURED size for the "+
				"leak probe to be sized to and it falls back to the model's, which no server behaviour can "+
				"move. Either the window is too narrow for where the boundary now is, or the pool did not "+
				"start full",
				swarmWindowAccepted, swarmWindowRefused, len(e.Window), e.MeasuredBoundary)))
	}
	// NOT a guard on the harness: the swarm's leak probe is sent at the MEASURED
	// boundary (see [boltDecodeSwarm.boundaryScan]), so a server that admits fewer
	// elements than the model predicts widens the slack by 48 B per element and
	// fires this. It reads the same way as the deterministic scenario's sibling
	// because it is now the same construction.
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
	// The SMALLEST number of refusals any wide window contained. No clause reads
	// it — rmp #2596 removed the threshold that did — but it is the sharpest single
	// number for how quickly the fleet was being refused relative to a 50 ms
	// honest hold, so it is reported and the margin stays visible as data.
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
		"(wide: %d/%d, narrowest wide window held %d refusals; %d refusals across the honest run, "+
		"%d of them in flight and %d in the gaps between exchanges, per segment %v)\n",
		served, len(e.Honest), straddled, wideStraddled, wide, minWideRefusals,
		e.RejectionsDuringHonest, boltDecodeSwarmInFlightRefusals(e.Honest),
		boltDecodeSwarmGapRefusals(e.Honest), boltDecodeSwarmSegmentRefusals(e.Honest))
	fmt.Fprintf(&b, "  window: %d refusals were drawn on a round trip overlapping the honest window; "+
		"%d of those overlapped a honest exchange's own flight (fleet rendezvous: %d of %d released "+
		"together, %d wait(s) expired; %d wide hold(s) gave up waiting for the fleet)\n",
		e.RefusalsSpanningHonestWindow, e.RefusalsConcurrentWithHonest,
		boltDecodeSwarmGatePairs, e.Abusers, e.GateTimeouts, e.WideHoldExpiries)
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
