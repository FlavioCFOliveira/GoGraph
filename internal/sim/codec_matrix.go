package sim

// codec_matrix.go — the key and weight codec matrix across crash and upgrade
// (rmp #2473).
//
// # What was untested, and why it stayed that way
//
// Until this file the simulator drove exactly ONE key codec and ONE weight
// codec. That was not an oversight in the harness: it is forced by the shape of
// the engine. Every Cypher-driven scenario reaches the graph through
// [github.com/FlavioCFOliveira/GoGraph/cypher.Engine], and the two engine
// constructors take a *txn.Store[string, float64] and nothing else. So
// [OpenSimStore] hardcoded txn.NewStringCodec / txn.NewFloat64WeightCodec, and
// with them the WHOLE remaining codec surface — NewIntCodec, NewInt32Codec,
// NewInt64Codec, NewUint64Codec, NewUUIDCodec, NewBinaryMarshalerCodec,
// NewInt64WeightCodec, NewBinaryMarshalerWeightCodec — never appeared in a
// single simulated crash.
//
// One consequence is sharper than the rest. [snapshot.WriteMapper] dispatches
// on the key type: for N == string it delegates to WriteMapperString and emits
// the frozen VERSION-1 layout, and only for some other N does it emit the
// VERSION-2 codec-framed layout. The read side mirrors that split
// (peekMapperVersion -> ReadMapperBytes -> [snapshot.ApplyMapperToGraphWithCodec]
// for version 2, ReadMapperString -> ApplyMapperToGraph for version 1). A
// string-only simulator therefore exercised the version-1 half of the mapper on
// every one of its crashes and the version-2 half on none of them, however many
// snapshot faults it injected.
//
// # What this file drives
//
// Each arm is one (key codec, weight codec) pair. Every arm runs the same two
// scenarios over the same durable stack the string scenarios use — the real
// WAL, the real [checkpoint.Checkpointer], the real snapshot publish, the real
// [recovery.OpenFS] — differing ONLY in the codec pair handed to
// [openSimTypedStore]:
//
//	crash-storm — the three publish windows of checkpoint_crash_storm.go
//	    (stranded-backup, publish-rename, archive-rename), crashing inside the
//	    crash-atomic swap and requiring the reopen to lose nothing.
//	upgrade — write, close gracefully, reopen across the boundary, then fold
//	    every op into one snapshot, MEASURE the WAL going to zero, crash, and
//	    require the reopen to replay ZERO WAL ops. What comes back after that
//	    can only have come through the mapper.
//
// # The oracles are codec-agnostic; the ENGINE is not
//
// The task this file closes assumed the existing oracles were codec-agnostic
// and that wiring a second codec would be plumbing. Half of that is true and
// half is not, and the difference is worth stating because it bounds what any
// future codec work can reuse.
//
// The DURABILITY oracles are genuinely codec-agnostic in FORM — acked ⊆
// recovered ⊆ issued, no failed commit resurrected, no phantom — and this file
// re-expresses them over node ORDINALS, with the arm's keyOf mapping an ordinal
// onto its concrete key type. One adjudicator then serves every arm.
//
// The oracle IMPLEMENTATIONS in this package are not reusable at all. Every one
// of them — [GraphOracle], [InvariantChecker], [CheckIndexConsistency],
// recoveredPersonNames, RunConcurrent, the Bolt server — reads the graph by
// running Cypher through [EngineAdapter], and the engine is fixed at string
// keys. They are not codec-agnostic; they are codec-BOUND, structurally, and no
// amount of parameterising the store changes that. A non-string arm therefore
// drives [txn.Store] directly and adjudicates against the recovered
// [lpg.Graph]. The practical cost is that the codec arms are single-writer:
// concurrency, Bolt, and Cypher-surface coverage remain the string arm's, and
// the codec arms add the codec dimension to durability and recovery only.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// codecMatrixDiskSeedMix decorrelates the SimDisk sub-seed from the run seed,
// so an arm's disk stream shares no low-order bits with its workload stream.
const codecMatrixDiskSeedMix uint64 = 0x2473_D15C_0DEC_9111

// The label and property every codec-matrix node carries. The property is the
// node's own ordinal, which is what turns "the node came back" into "the node
// came back AS ITSELF": a key codec that round-tripped to a different value
// resolves to no node, and one that collided resolves to the wrong ordinal.
const (
	codecMatrixLabel   = "CodecNode"
	codecMatrixOrdProp = "ord"
)

// The mapper.bin header constants, duplicated here because the snapshot
// package keeps them unexported. They are pinned by [snapshot.WriteMapper]'s
// documented on-disk layout and asserted against the DURABLE bytes, so a drift
// in either direction is caught rather than assumed away.
const (
	// codecMapperMagic is the four-byte 'GMAP' magic every mapper.bin starts
	// with, little-endian.
	codecMapperMagic uint32 = 0x50414D47
	// codecMapperFormatString is the frozen version-1 layout the writer emits
	// for string keys (raw UTF-8 key bytes, no codec framing).
	codecMapperFormatString uint16 = 1
	// codecMapperFormatBytes is the version-2 layout the writer emits for every
	// other key type, whose per-record key bytes are codec.Encode output.
	codecMapperFormatBytes uint16 = 2
)

// -----------------------------------------------------------------------------
// The binary-marshaler arm's key and weight types
// -----------------------------------------------------------------------------

// simCodecKey is the composite node identifier the BINARY-MARSHALER key arm
// uses. It is deliberately a struct rather than a named integer, because that
// is the case [txn.NewBinaryMarshalerCodec] exists for: a key whose wire form
// the type itself defines, framed by the codec behind a uint32 length prefix.
type simCodecKey struct {
	// Tenant is a fixed discriminator, present so the encoded form is not a
	// bare counter and a codec that dropped bytes would be visible.
	Tenant uint32
	// Serial is the node's ordinal.
	Serial uint32
}

// MarshalBinary encodes the key as eight big-endian bytes: Tenant then Serial.
// Big-endian is deliberate — the surrounding codecs are little-endian, so a
// byte order confused between the two layers would corrupt the key rather than
// happening to round-trip.
func (k simCodecKey) MarshalBinary() ([]byte, error) {
	var b [8]byte
	binary.BigEndian.PutUint32(b[0:4], k.Tenant)
	binary.BigEndian.PutUint32(b[4:8], k.Serial)
	return b[:], nil
}

// UnmarshalBinary reverses [simCodecKey.MarshalBinary], rejecting any payload
// that is not exactly eight bytes.
func (k *simCodecKey) UnmarshalBinary(b []byte) error {
	if len(b) != 8 {
		return fmt.Errorf("sim: simCodecKey: want 8 bytes, got %d", len(b))
	}
	k.Tenant = binary.BigEndian.Uint32(b[0:4])
	k.Serial = binary.BigEndian.Uint32(b[4:8])
	return nil
}

// simCodecWeight is the edge weight the BINARY-MARSHALER weight arm uses, an
// integral number of thousandths so equality after a round-trip is exact.
type simCodecWeight struct {
	// Millis is the weight in thousandths.
	Millis int64
}

// MarshalBinary encodes the weight as eight big-endian bytes.
func (w simCodecWeight) MarshalBinary() ([]byte, error) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(w.Millis))
	return b[:], nil
}

// UnmarshalBinary reverses [simCodecWeight.MarshalBinary], rejecting any
// payload that is not exactly eight bytes.
func (w *simCodecWeight) UnmarshalBinary(b []byte) error {
	if len(b) != 8 {
		return fmt.Errorf("sim: simCodecWeight: want 8 bytes, got %d", len(b))
	}
	w.Millis = int64(binary.BigEndian.Uint64(b))
	return nil
}

// -----------------------------------------------------------------------------
// Arms
// -----------------------------------------------------------------------------

// codecArm is one (key codec, weight codec) pair, type-erased so arms of
// different instantiations can sit in one slice. Every method it exposes is
// independent of N and W, which is what lets the matrix runner treat a uuid arm
// and an int64 arm identically.
type codecArm interface {
	// name identifies the arm in evidence and in violation messages.
	name() string
	// wantMapperFormat is the mapper.bin format version this arm's KEY TYPE
	// must make [snapshot.WriteMapper] emit.
	wantMapperFormat() uint16
	// runCrashStorm crashes inside the snapshot publish at each of the three
	// windows and adjudicates the durability contract across every reopen.
	runCrashStorm(ctx context.Context, seed uint64, size codecMatrixSize) (codecArmEvidence, []Violation, error)
	// runUpgrade crosses the graceful-restart boundary, then the snapshot
	// boundary, and adjudicates that the recovered graph came out of the mapper.
	runUpgrade(ctx context.Context, seed uint64, size codecMatrixSize) (codecArmEvidence, []Violation, error)
	// weightSurvivesSnapshot reports whether this arm's WEIGHT type survives a
	// checkpoint. See [codecArmOf.snapshotWeightSupported] for the measured
	// engine limitation this exists to describe.
	weightSurvivesSnapshot() bool
}

// codecPhase names WHERE the graph under adjudication came from, which is what
// decides how strictly the weight can be asserted (see
// [codecArmOf.snapshotWeightSupported]).
type codecPhase int

const (
	// codecPhaseWAL is a recovery with no snapshot on disk: every op came out of
	// the WAL, through the arm's own [txn.WeightCodec]. Weights are asserted in
	// full here for EVERY arm.
	codecPhaseWAL codecPhase = iota
	// codecPhaseSnapshotOnly is a recovery whose WAL was emptied by a folding
	// checkpoint and which replayed zero ops: every edge came out of the
	// snapshot's CSR component.
	codecPhaseSnapshotOnly
	// codecPhaseMixed is a recovery that loaded a snapshot AND replayed a WAL
	// tail on top, so an individual edge's provenance is not determined.
	codecPhaseMixed
)

// String renders a phase for evidence and violation messages.
func (p codecPhase) String() string {
	switch p {
	case codecPhaseWAL:
		return "WAL-only"
	case codecPhaseSnapshotOnly:
		return "snapshot-only"
	case codecPhaseMixed:
		return "snapshot+WAL"
	default:
		return fmt.Sprintf("codecPhase(%d)", int(p))
	}
}

// codecArmOf is the generic implementation of [codecArm] for one codec pair.
//
// W is constrained to comparable rather than any so a recovered weight can be
// compared with the weight that was written; [txn.Store] itself accepts any W,
// so this narrows the harness, never the engine.
type codecArmOf[N comparable, W comparable] struct {
	// label names the arm, e.g. "uuid/float64".
	label string
	// codec / wcodec are the pair under test.
	codec  txn.Codec[N]
	wcodec txn.WeightCodec[W]
	// keyOf maps a node ordinal onto this arm's key type. It MUST be injective:
	// the whole oracle rests on one ordinal naming one node.
	keyOf func(ord int) N
	// weightOf maps a node ordinal onto the weight of the edge leaving it.
	weightOf func(ord int) W
	// mapperFormat is the mapper.bin version this key type must produce.
	mapperFormat uint16
	// snapshotWeightSupported records whether this arm's WEIGHT type survives a
	// checkpoint. It is false ONLY for the binary-marshaler weight arm, and it
	// pins a MEASURED engine limitation rather than a harness preference:
	//
	// store/snapshot's CSR component does not use the transaction layer's
	// [txn.WeightCodec] at all. It sizes a weight with the fixed table in
	// csrWeightSize (store/snapshot/writer.go), which answers 0 for every type
	// outside the Go primitives — including any struct, which is precisely the
	// case [txn.NewBinaryMarshalerWeightCodec] exists to persist. WriteCSR then
	// writes hasWeights=0, the SAME on-disk encoding a deliberately weightless
	// graph produces (adjlist.Config{Weightless: true}), and the checkpoint goes
	// on to truncate the WAL prefix that held the real values. Measured directly:
	// csrWeightSize[float64] = 8, csrWeightSize[int64] = 8, csrWeightSize[struct
	// with one int64 field] = 0, csrWeightSize[struct{}] = 0.
	//
	// The flag does not excuse that. It SCOPES the assertion so the gap is
	// pinned instead of tolerated: on a snapshot-only recovery an arm with this
	// flag false must come back with the ZERO weight — not a lost edge, and not
	// the right weight either. The day the engine learns to persist these
	// weights, that assertion fails and this flag has to be retired, which is the
	// point of writing it as an assertion rather than as a skip.
	snapshotWeightSupported bool
}

func (a codecArmOf[N, W]) name() string { return a.label }

func (a codecArmOf[N, W]) wantMapperFormat() uint16 { return a.mapperFormat }

func (a codecArmOf[N, W]) weightSurvivesSnapshot() bool { return a.snapshotWeightSupported }

// codecMatrixArms is the matrix this file drives: every key codec the txn
// package exports, paired with a weight codec, plus the string/float64 arm as
// the CONTROL. The control matters twice over — it is the pair every other
// scenario in the package runs on, and it is the only arm whose expected mapper
// format is version 1, so an arm list that had somehow collapsed onto one codec
// would fail the format verdict rather than pass quietly.
func codecMatrixArms() []codecArm {
	return []codecArm{
		codecArmOf[string, float64]{
			label: "string/float64", codec: txn.NewStringCodec(), wcodec: txn.NewFloat64WeightCodec(),
			keyOf:        func(ord int) string { return fmt.Sprintf("k%08d", ord) },
			weightOf:     func(ord int) float64 { return float64(ord) + 0.5 },
			mapperFormat: codecMapperFormatString, snapshotWeightSupported: true,
		},
		codecArmOf[[16]byte, float64]{
			label: "uuid/float64", codec: txn.NewUUIDCodec(), wcodec: txn.NewFloat64WeightCodec(),
			keyOf:        codecUUIDKey,
			weightOf:     func(ord int) float64 { return float64(ord) + 0.5 },
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: true,
		},
		codecArmOf[int64, int64]{
			label: "int64/int64", codec: txn.NewInt64Codec(), wcodec: txn.NewInt64WeightCodec(),
			keyOf:        func(ord int) int64 { return int64(ord) },
			weightOf:     func(ord int) int64 { return int64(ord) * 7 },
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: true,
		},
		codecArmOf[simCodecKey, simCodecWeight]{
			label:  "binarymarshaler/binarymarshaler",
			codec:  txn.NewBinaryMarshalerCodec[simCodecKey, *simCodecKey](),
			wcodec: txn.NewBinaryMarshalerWeightCodec[simCodecWeight, *simCodecWeight](),
			keyOf: func(ord int) simCodecKey {
				return simCodecKey{Tenant: 0xA5A5_5A5A, Serial: uint32(ord)}
			},
			weightOf: func(ord int) simCodecWeight { return simCodecWeight{Millis: int64(ord) * 13} },
			// The ONLY arm whose weight does not survive a checkpoint; see
			// snapshotWeightSupported for the measured cause.
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: false,
		},
		codecArmOf[int, float64]{
			label: "int/float64", codec: txn.NewIntCodec(), wcodec: txn.NewFloat64WeightCodec(),
			keyOf:        func(ord int) int { return ord },
			weightOf:     func(ord int) float64 { return float64(ord) + 0.25 },
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: true,
		},
		codecArmOf[int32, int64]{
			label: "int32/int64", codec: txn.NewInt32Codec(), wcodec: txn.NewInt64WeightCodec(),
			keyOf:        func(ord int) int32 { return int32(ord) },
			weightOf:     func(ord int) int64 { return int64(ord) * 3 },
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: true,
		},
		codecArmOf[uint64, float64]{
			label: "uint64/float64", codec: txn.NewUint64Codec(), wcodec: txn.NewFloat64WeightCodec(),
			keyOf:        func(ord int) uint64 { return uint64(ord) },
			weightOf:     func(ord int) float64 { return float64(ord) * 1.5 },
			mapperFormat: codecMapperFormatBytes, snapshotWeightSupported: true,
		},
	}
}

// codecUUIDKey renders an ordinal as a 16-byte UUID-shaped key: a fixed
// eight-byte prefix so the encoding is not a bare counter, then the ordinal
// big-endian.
func codecUUIDKey(ord int) [16]byte {
	var k [16]byte
	binary.BigEndian.PutUint64(k[0:8], 0x2473_0000_C0DE_C000)
	binary.BigEndian.PutUint64(k[8:16], uint64(ord))
	return k
}

// -----------------------------------------------------------------------------
// Sizing, ordinals, ledger
// -----------------------------------------------------------------------------

// codecMatrixSize sizes one arm's run: how many transactions each write phase
// commits and how many fresh nodes each transaction creates.
type codecMatrixSize struct {
	txns        int
	nodesPerTxn int
}

// smokeCodecMatrix is the SHORT-layer size. The short layer runs one arm's
// upgrade scenario only, as a wiring smoke test; the matrix itself is a soak
// scenario (the package's short budget is 60 s and already spent).
func smokeCodecMatrix() codecMatrixSize { return codecMatrixSize{txns: 3, nodesPerTxn: 3} }

// codecOrdinals hands out fresh, never-reused node ordinals for the whole of
// one arm's run, so a key is minted at most once per durable image and "this
// key is present" is unambiguous evidence about the transaction that made it.
type codecOrdinals struct{ next int }

// take returns the next n ordinals.
func (o *codecOrdinals) take(n int) []int {
	ords := make([]int, n)
	for i := range ords {
		ords[i] = o.next
		o.next++
	}
	return ords
}

// codecLedger models what one arm's workload ISSUED, what the store
// ACKNOWLEDGED, and what it explicitly FAILED — in codec-independent terms.
// Every node is named by its ordinal; the arm's keyOf maps an ordinal onto the
// concrete key. That indirection is what lets ONE oracle adjudicate every arm.
type codecLedger struct {
	issued map[int]struct{}
	acked  map[int]struct{}
	failed map[int]struct{}
	// ackedEdges are the (src ordinal, dst ordinal) pairs whose transaction
	// committed. The edge leaving src carries weightOf(src), so a recovered
	// weight adjudicates the WEIGHT codec independently of the key codec.
	ackedEdges map[[2]int]struct{}
	// lastAcked is the highest ordinal whose transaction committed, used as the
	// anchor a later transaction links back into.
	lastAcked int
}

// newCodecLedger returns an empty ledger with no anchor yet.
func newCodecLedger() *codecLedger {
	return &codecLedger{
		issued:     make(map[int]struct{}),
		acked:      make(map[int]struct{}),
		failed:     make(map[int]struct{}),
		ackedEdges: make(map[[2]int]struct{}),
		lastAcked:  -1,
	}
}

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// codecArmEvidence is what ONE arm's run MEASURED. Every field is read from the
// recovered graph or from the durable byte image, never assumed, so the
// non-vacuity gate adjudicates facts rather than an intention to exercise them.
type codecArmEvidence struct {
	// arm / scenario name what produced this evidence.
	arm      string
	scenario string
	// ran is set once the arm reached its terminal reopen. A false here is the
	// only thing that distinguishes "the arm passed" from "the arm never ran".
	ran bool
	// issuedNodes / ackedNodes / recoveredNodes are the ledger sizes and the
	// count of acknowledged ordinals actually found in the recovered graph.
	issuedNodes    int
	ackedNodes     int
	recoveredNodes int
	// ackedEdges / recoveredEdges count acknowledged edges and how many came
	// back at all (topology, independent of weight).
	ackedEdges     int
	recoveredEdges int
	// recoveredWeights is how many of those edges also came back with the weight
	// they were written with; weightGaps is how many came back with a weight the
	// snapshot CSR writer could not persist. The two are kept apart so an arm
	// whose weight type the snapshot drops is visible as a MEASURED number rather
	// than as an absence (see [codecArmOf.snapshotWeightSupported]).
	recoveredWeights int
	weightGaps       int
	// liveOrder is the recovered graph's own live node count, read independently
	// of the ordinal sweep so a phantom outside the modelled range is visible.
	liveOrder uint64
	// mapperFormat / mapperBytes are the format version and byte length read
	// from the DURABLE mapper.bin after a snapshot was published. A zero
	// mapperFormat means no snapshot component was found to read.
	mapperFormat uint16
	mapperBytes  int
	// boundary is the measured snapshot crossing (upgrade scenario only).
	boundary snapshotBoundary
	// windowsEntered / promotes count crash-storm cycles whose armed publish
	// fault really fired, and those whose reopen ran recovery's promote repair.
	windowsEntered int
	promotes       int
}

// summary renders an arm's measured numbers for a test log.
func (e *codecArmEvidence) summary() string {
	return fmt.Sprintf(
		"%s/%s: nodes issued=%d acked=%d recovered=%d (liveOrder=%d), edges acked=%d recovered=%d "+
			"(weights kept=%d, dropped by the snapshot CSR writer=%d), mapper.bin v%d (%d bytes), "+
			"windows entered=%d promotes=%d",
		e.arm, e.scenario, e.issuedNodes, e.ackedNodes, e.recoveredNodes, e.liveOrder,
		e.ackedEdges, e.recoveredEdges, e.recoveredWeights, e.weightGaps,
		e.mapperFormat, e.mapperBytes, e.windowsEntered, e.promotes)
}

// -----------------------------------------------------------------------------
// Workload
// -----------------------------------------------------------------------------

// commitBatch commits ONE transaction that creates len(ords) fresh nodes, each
// labelled and carrying its own ordinal as a property, chained by weighted
// edges, plus one edge back into already-durable state when an anchor exists.
//
// Both endpoints of every intra-batch edge are created by the SAME transaction,
// so an edge is durable exactly when its nodes are: the edge ledger cannot
// disagree with the node ledger for any reason other than a defect.
//
// A commit the store REJECTS is recorded in the ledger's failed set and
// returned as nil — a rejected transaction is data for the atomicity oracle,
// not a harness failure. Only a genuine harness fault (a buffering call that
// should not fail, a store that will not begin) is returned as an error.
func (a codecArmOf[N, W]) commitBatch(
	ctx context.Context, st *simTypedStore[N, W], led *codecLedger, ords []int,
) error {
	tx, err := st.store.BeginCtx(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	edges := make([][2]int, 0, len(ords)+1)
	for i, ord := range ords {
		key := a.keyOf(ord)
		if err := tx.AddNode(key); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("add node %d: %w", ord, err)
		}
		if err := tx.SetNodeLabel(key, codecMatrixLabel); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set label on node %d: %w", ord, err)
		}
		if err := tx.SetNodeProperty(key, codecMatrixOrdProp, lpg.Int64Value(int64(ord))); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set property on node %d: %w", ord, err)
		}
		if i == 0 {
			continue
		}
		if err := tx.AddEdge(key, a.keyOf(ords[i-1]), a.weightOf(ord)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("add edge %d->%d: %w", ord, ords[i-1], err)
		}
		edges = append(edges, [2]int{ord, ords[i-1]})
	}
	// One cross-transaction link back into state that is ALREADY durable, so the
	// key codec is exercised on a key this session decoded out of a WAL or a
	// snapshot rather than only on keys it minted itself.
	if led.lastAcked >= 0 && len(ords) > 0 {
		src := ords[0]
		if err := tx.AddEdge(a.keyOf(src), a.keyOf(led.lastAcked), a.weightOf(src)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("add anchor edge %d->%d: %w", src, led.lastAcked, err)
		}
		edges = append(edges, [2]int{src, led.lastAcked})
	}

	commitErr := tx.Commit()
	for _, ord := range ords {
		led.issued[ord] = struct{}{}
	}
	if commitErr != nil {
		for _, ord := range ords {
			led.failed[ord] = struct{}{}
		}
		return nil
	}
	for _, ord := range ords {
		led.acked[ord] = struct{}{}
		if ord > led.lastAcked {
			led.lastAcked = ord
		}
	}
	for _, e := range edges {
		led.ackedEdges[e] = struct{}{}
	}
	return nil
}

// writePhase commits size.txns transactions of size.nodesPerTxn fresh nodes.
func (a codecArmOf[N, W]) writePhase(
	ctx context.Context, st *simTypedStore[N, W], led *codecLedger, ords *codecOrdinals, size codecMatrixSize,
) error {
	for i := 0; i < size.txns; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.commitBatch(ctx, st, led, ords.take(size.nodesPerTxn)); err != nil {
			return err
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// The VERDICT gate
// -----------------------------------------------------------------------------

// checkRecovered is the VERDICT gate: the durability, atomicity and consistency
// contract over the recovered graph, expressed once for every arm.
//
// It is UNCONDITIONAL. It does not ask whether the workload exercised anything;
// an arm that committed nothing simply has nothing in its acked set and passes
// here, and the SEPARATE non-vacuity gate is what refuses that run. Keeping the
// two apart is what stops an uninformative run from being reported as a fault
// and stops a fault from being excused as uninformative.
//
// Every read is BY KEY, which is what makes this a mapper assertion rather than
// a count: a key codec that round-tripped to a different value resolves to no
// node at all, and one that collided resolves to a node carrying somebody
// else's ordinal.
func (a codecArmOf[N, W]) checkRecovered(
	tick int64, phase codecPhase, g *lpg.Graph[N, W], led *codecLedger, ev *codecArmEvidence,
) []Violation {
	var v []Violation
	add := func(kind ViolationKind, op, format string, args ...any) {
		v = append(v, Violation{Kind: kind, Op: op, Tick: tick, Message: fmt.Sprintf(format, args...)})
	}

	ev.issuedNodes, ev.ackedNodes, ev.ackedEdges = len(led.issued), len(led.acked), len(led.ackedEdges)
	ev.recoveredNodes, ev.recoveredEdges = 0, 0

	for _, ord := range slices.Sorted(maps.Keys(led.acked)) {
		key := a.keyOf(ord)
		pv, ok := g.GetNodeProperty(key, codecMatrixOrdProp)
		if !ok {
			add(ViolationACIDDurability, "<codec-node-lost>",
				"[%s] acknowledged node ordinal %d did not come back under its own key after recovery"+
					" (acked=%d, live order=%d)", a.label, ord, len(led.acked), g.LiveOrder())
			continue
		}
		got, isInt := pv.Int64()
		if !isInt || got != int64(ord) {
			add(ViolationACIDConsistency, "<codec-key-mismatch>",
				"[%s] the key for ordinal %d resolved to a node carrying ordinal %v (int=%t):"+
					" the key codec did not round-trip injectively", a.label, ord, got, isInt)
			continue
		}
		if !g.HasNodeLabel(key, codecMatrixLabel) {
			add(ViolationACIDAtomicity, "<codec-label-lost>",
				"[%s] node ordinal %d came back without the %s label its creating transaction set:"+
					" the transaction was applied partially", a.label, ord, codecMatrixLabel)
			continue
		}
		ev.recoveredNodes++
	}

	// Atomicity: a transaction the store REJECTED must leave nothing behind.
	for _, ord := range slices.Sorted(maps.Keys(led.failed)) {
		if _, acked := led.acked[ord]; acked {
			continue
		}
		if _, ok := g.GetNodeProperty(a.keyOf(ord), codecMatrixOrdProp); ok {
			add(ViolationACIDAtomicity, "<codec-failed-resurrected>",
				"[%s] node ordinal %d is present after recovery although its transaction was REJECTED",
				a.label, ord)
		}
	}

	// Weight-codec round-trip. The EDGE must always come back — that half is
	// unconditional, on every arm and in every phase. What may be asserted about
	// its WEIGHT depends on where the edge came from, because the snapshot's CSR
	// component does not consult the weight codec at all (see
	// [codecArmOf.snapshotWeightSupported]).
	var zeroW W
	for _, e := range sortedCodecEdges(led.ackedEdges) {
		w, ok := g.EdgeWeight(a.keyOf(e[0]), a.keyOf(e[1]))
		if !ok {
			add(ViolationACIDDurability, "<codec-edge-lost>",
				"[%s] acknowledged edge %d->%d did not come back after recovery (%s)",
				a.label, e[0], e[1], phase)
			continue
		}
		ev.recoveredEdges++
		want := a.weightOf(e[0])
		switch {
		case a.snapshotWeightSupported || phase == codecPhaseWAL:
			// The weight codec is genuinely in the path: assert the round-trip.
			if w != want {
				add(ViolationACIDConsistency, "<codec-weight-drift>",
					"[%s] edge %d->%d came back from a %s recovery with weight %v, want %v:"+
						" the weight codec did not round-trip", a.label, e[0], e[1], phase, w, want)
				continue
			}
			ev.recoveredWeights++
		case phase == codecPhaseSnapshotOnly:
			// PINNED GAP. Every edge here came out of the snapshot, whose CSR
			// writer sized this weight type at 0 bytes and wrote hasWeights=0. The
			// documented consequence is the ZERO weight — assert exactly that, so
			// the day the engine learns to persist these weights this fires and
			// the snapshotWeightSupported flag has to be retired.
			if w != zeroW {
				add(ViolationOracleDeviation, "<codec-weight-gap-changed>",
					"[%s] edge %d->%d came back from a snapshot-only recovery with weight %v,"+
						" but this arm's weight type is documented as unpersistable by the snapshot"+
						" CSR writer (expected the zero value %v). The engine's behaviour has changed:"+
						" retire snapshotWeightSupported for this arm and restore the full round-trip"+
						" assertion", a.label, e[0], e[1], w, zeroW)
				continue
			}
			ev.weightGaps++
		default:
			// Mixed snapshot+WAL recovery: this edge's provenance is not
			// determined, so no weight assertion is possible for an arm whose
			// weight survives only one of the two paths. Counted as evidence and
			// logged by the caller, never silently ignored.
			if w == want {
				ev.recoveredWeights++
			} else {
				ev.weightGaps++
			}
		}
	}

	// Consistency: the recovered graph must not carry more live nodes than the
	// workload ever issued. The ordinal sweep above cannot see a phantom outside
	// the modelled range; this can.
	ev.liveOrder = g.LiveOrder()
	if ev.liveOrder > uint64(len(led.issued)) {
		add(ViolationACIDConsistency, "<codec-phantom>",
			"[%s] the recovered graph holds %d live nodes but the workload only ever issued %d",
			a.label, ev.liveOrder, len(led.issued))
	}
	return v
}

// sortedCodecEdges returns the edge set in a deterministic order, so a failure
// message names the same edge on every replay of a seed.
func sortedCodecEdges(set map[[2]int]struct{}) [][2]int {
	out := make([][2]int, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	slices.SortFunc(out, func(x, y [2]int) int {
		if x[0] != y[0] {
			return x[0] - y[0]
		}
		return x[1] - y[1]
	})
	return out
}

// checkCodecMapperFormat is the second half of the VERDICT gate: the durable
// mapper.bin must carry the layout this arm's KEY TYPE selects — version 1 for
// string keys (the frozen, byte-compatible layout) and version 2 for every
// other key type.
//
// It is conditional on a mapper having been READ, and deliberately so. Whether
// a snapshot was published at all is a question about how much the run
// exercised, which belongs to the non-vacuity gate; given that one was, which
// layout it carries is a correctness property of the writer and belongs here.
func checkCodecMapperFormat(tick int64, arm codecArm, ev *codecArmEvidence) []Violation {
	if ev.mapperFormat == 0 {
		return nil
	}
	if ev.mapperFormat == arm.wantMapperFormat() {
		return nil
	}
	return []Violation{{
		Kind: ViolationACIDConsistency, Tick: tick, Op: "<codec-mapper-format>",
		Message: fmt.Sprintf(
			"[%s] the published snapshot's %s carries format version %d, want %d:"+
				" the snapshot writer selected the wrong mapper layout for this key type",
			arm.name(), snapshot.MapperFile, ev.mapperFormat, arm.wantMapperFormat()),
	}}
}

// -----------------------------------------------------------------------------
// The NON-VACUITY gate — shape only
// -----------------------------------------------------------------------------

// codecMatrixMinNodes is the smallest recovered node count that counts as a
// non-trivial graph. An arm that silently degenerated to an empty (or nearly
// empty) graph would satisfy every verdict above by having nothing to check,
// which is exactly the failure mode this gate exists to refuse.
const codecMatrixMinNodes = 4

// checkCodecMatrixNonVacuity is the SEPARATE, shape-only gate. It never asserts
// that the engine behaved correctly — the verdict gates own that — only that
// the run was informative enough for those verdicts to have meant something:
// every arm ran, every arm built a non-trivial graph, and every arm's snapshot
// really produced a mapper to adjudicate.
//
// Keeping it separate is the point. A run that exercised too little is not a
// defect in the engine, and reporting it as one is how a suite acquires guards
// that cannot discriminate.
func checkCodecMatrixNonVacuity(arms []codecArm, ev []codecArmEvidence) []Violation {
	var v []Violation
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}

	byName := make(map[string]codecArm, len(arms))
	for _, arm := range arms {
		byName[arm.name()] = arm
	}
	// weightEvidence[arm] records whether the arm's weight behaviour was
	// actually observed in the phase where it is assertable, so neither the full
	// round-trip assertion nor the PINNED-GAP assertion can pass by never having
	// looked at an edge.
	weightsKept := make(map[string]int, len(arms))
	weightsDropped := make(map[string]int, len(arms))

	seen := make(map[string]bool, len(ev))
	for i := range ev {
		e := &ev[i]
		if !e.ran {
			add("<arm-did-not-run>", "arm %s (%s) never reached its terminal reopen", e.arm, e.scenario)
			continue
		}
		seen[e.arm] = true
		weightsKept[e.arm] += e.recoveredWeights
		weightsDropped[e.arm] += e.weightGaps
		if e.ackedNodes < codecMatrixMinNodes {
			add("<arm-degenerate>",
				"arm %s (%s) acknowledged only %d nodes (want >= %d): the durability oracle had"+
					" almost nothing to adjudicate", e.arm, e.scenario, e.ackedNodes, codecMatrixMinNodes)
		}
		if e.recoveredNodes < codecMatrixMinNodes {
			add("<arm-degenerate>",
				"arm %s (%s) recovered only %d nodes (want >= %d): the graph the codec round-tripped"+
					" was trivial", e.arm, e.scenario, e.recoveredNodes, codecMatrixMinNodes)
		}
		if e.ackedEdges == 0 {
			add("<weight-codec-unexercised>",
				"arm %s (%s) acknowledged no edge, so its WEIGHT codec was never round-tripped",
				e.arm, e.scenario)
		}
		if e.mapperFormat == 0 {
			add("<mapper-unread>",
				"arm %s (%s) published no %s to read, so the mapper layout verdict was vacuous",
				e.arm, e.scenario, snapshot.MapperFile)
		}
	}
	for _, arm := range arms {
		name := arm.name()
		if !seen[name] {
			add("<arm-unexercised>", "codec arm %s produced no evidence at all", name)
			continue
		}
		if arm.weightSurvivesSnapshot() {
			if weightsKept[name] == 0 {
				add("<weight-roundtrip-unobserved>",
					"arm %s never had a single edge weight confirmed after a recovery, so its weight"+
						" codec was never actually round-tripped", name)
			}
			continue
		}
		// The PINNED GAP has to be observed to be a pin at all. An arm documented
		// as losing its weights through the snapshot, which never once produced a
		// dropped weight, is either no longer reaching the snapshot or no longer
		// losing them — and in both cases the assertion is proving nothing.
		if weightsDropped[name] == 0 {
			add("<weight-gap-unobserved>",
				"arm %s is documented as having a weight type the snapshot CSR writer cannot persist,"+
					" but no dropped weight was observed: either the gap closed (retire"+
					" snapshotWeightSupported) or the run never reached a snapshot-sourced edge", name)
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Durable-image probes
// -----------------------------------------------------------------------------

// readSimMapperFormat reads the mapper.bin header straight out of the DURABLE
// SimDisk image and returns its format version and byte length. It parses the
// bytes rather than asking the snapshot package which writer it used, so the
// answer is what is actually on disk.
//
// A missing component is reported as (0, 0, nil): no snapshot has been
// published yet, which the non-vacuity gate — not this probe — decides what to
// make of.
func readSimMapperFormat(disk *SimDisk, snapDir string) (uint16, int, error) {
	data, err := disk.ReadFile(snapDir + "/" + snapshot.MapperFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	if len(data) < 6 {
		return 0, len(data), fmt.Errorf("sim: %s is %d bytes, too short for a header",
			snapshot.MapperFile, len(data))
	}
	if magic := binary.LittleEndian.Uint32(data[0:4]); magic != codecMapperMagic {
		return 0, len(data), fmt.Errorf("sim: %s has magic %#x, want %#x",
			snapshot.MapperFile, magic, codecMapperMagic)
	}
	return binary.LittleEndian.Uint16(data[4:6]), len(data), nil
}
