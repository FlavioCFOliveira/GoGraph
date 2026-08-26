package sim

// label_index_scoped.go — rmp #2496: the scoped and range half of
// [github.com/FlavioCFOliveira/GoGraph/graph/index/label].Index driven directly
// against a naive model, plus the serialized form round-tripped and damaged on a
// [SimDisk].
//
// # What was unreached
//
// `graph/index/label/index.go` is 514 lines and the whole of it is public. Of
// that surface, VERIFIED by an `os.walk`+`re` sweep of every production
// (non-`_test.go`) file in the tree — not by grep, since the shimmed `grep` on the
// reference host can return a silent empty result, and an empty match is not
// evidence of absence:
//
//   - `NewNodeIndex`, `NewEdgeIndex` and `Scope` have NO production caller. The
//     only two `label.Index` values in the whole module are lpg's `nodeIdx` and
//     `edgeIdx`, both built with `label.NewIndex()` at
//     `graph/lpg/lpg.go:1685-1686`.
//   - `AddRange`, `RemoveRange`, `Scan`, `Union`, `Serialize` and `Deserialize`
//     have no non-test caller anywhere.
//
// So the parts lpg drives — `Add`, `Remove`, `Count`, `Has`, `Intersect`,
// `IntersectCardinality` — are exercised by every DST run that touches a label,
// and the parts it does not drive were carried by their godoc alone.
//
// # Why lpg uses the UNSCOPED constructor, established rather than assumed
//
// The task asked for this to be recorded, and the answer is sharper than
// "the scope does not matter here".
//
// `Scope` is read in exactly one place: inside [label.Index.Apply]
// (`graph/index/label/index.go:319-338`). `Apply` in turn runs only through the
// [index.Manager] fan-out (`graph/index/manager.go:254` and `:266`). And no
// `label.Index` is ever registered with a Manager. `Manager.CreateIndex`
// (`manager.go:166`) is the sole writer of the subscriber registry, its twelve
// production call sites — `cypher/index_binding.go:711,714,731,734,788,890,900`,
// `cypher/api.go:1728,3273,3289`, `cypher/exec/create_index.go:107`,
// `cypher/exec/create_constraint.go:125` — every one of them registers a
// `hash.Index` or a `btree.Index`, and lpg's two label indexes are never handed
// to a manager at all: they are maintained by direct `Add`/`Remove` calls from
// `graph/lpg`. Only three production packages even import `graph/index/label`
// (`cypher`, `cypher/exec`, `graph/lpg`, VERIFIED through `go list`), and two of
// them only read an index lpg owns.
//
// A second, structural check agrees without reading a single return type: only
// FOUR production files name the package at all — `graph/lpg/lpg.go`, which
// constructs both indexes; `cypher/exec/scan_label.go` and
// `cypher/stats_estimate.go`, which take one as a read source; and
// `cypher/api.go`, whose two mentions are doc comments. Not one of the twelve
// `CreateIndex` call sites is in any of them, so none of them can be handing a
// `label.Index` to a manager.
//
// So `NewIndex()` is correct for both because the field the other two
// constructors set is never read on a directly-driven index. That is the
// rationale in the narrow sense.
//
// The sharper half is that using `NewEdgeIndex()` for the edge index would not
// merely be inert — it would be WRONG if it ever took effect.
// `index.OpAddEdgeLabel` IS constructed in production, at `cypher/api.go:17890`
// and `cypher/api.go:18804`, and it IS delivered: `exec.IndexBuffer.Commit`
// calls `Manager.ApplyBatch` at `cypher/exec/index_writeback.go:45`. Its partner
// `index.OpRemoveEdgeLabel` is constructed NOWHERE in production — VERIFIED, all
// four production mentions are the enum declaration (`manager.go:106`), the
// `IsEdgeChange` switch (`manager.go:144`), one doc comment
// (`label/index.go:65`) and the `Apply` case that consumes it
// (`label/index.go:333`). A registered `ScopeEdge` label index would therefore
// receive every edge-label ADDITION and never one removal, and would accumulate
// stale postings for as long as the process ran. `NewEdgeIndex` names a scope
// whose event stream is, today, only half-emitted.
//
// [liDriveScope] measures both halves rather than restating them: it drives the
// four change kinds at each of the three constructors through a naive routing
// table, and it reproduces the stale-posting consequence directly.
//
// # The model is naive and independent
//
// [liModel] is a `map[uint32]map[uint64]bool` recomputed from the op stream. It
// knows nothing about roaring, nothing about [index.NodeSet]'s four-state tier
// machine, and it never asks the index what the answer should be. Every
// membership clause — `Count`, `Scan`, `Has`, `Union`, and the CONTENT of a
// deserialized image — is adjudicated against it.
//
// It deliberately does NOT model which tier a label sits on, or when the index
// deletes a label's map entry. Doing so would be a second copy of
// `graph/index/nodeset.go` sitting in the harness and agreeing with the original
// by construction, which is the failure mode this file exists to avoid. The one
// entry-population clause is therefore tier-free and one-sided —
// `serializedLabelCount >= labels the model says have a member`, because an
// index that lost an entry for a live label lost data — and the EXCESS is
// reported as a measured number rather than predicted. That excess is not noise:
// it is the phantom population [liDrivePhantom] pins.
//
// # Overlap and adjacency are CONSTRUCTED, not drawn
//
// The inclusive-to-exclusive `+1` conversions at `graph/index/nodeset.go:339`
// and `:378` are where an off-by-one would live, and a drawn pair of endpoints
// reaches an exact adjacency (`[a,b]` then `[b+1,c]`) only by luck. So the
// thirteen relationships in [liRel] — disjoint, adjacent on each side, partial
// overlap on each side, contained, containing, identical, single-element inside
// and outside, inverted, and the off-by-one empty range — are SWEPT, each in
// both directions (`AddRange` and `RemoveRange`), twenty-six cells per epoch, in
// a seed-shuffled order. The seed decides the order and the base band; it never
// decides the coverage.
//
// # Two defects this file FIXED, and one it still WITNESSES
//
// The boundary defect is FIXED (#2607): `to == math.MaxUint64` used to drop the
// WHOLE range in both directions, because `to+1` wrapped to 0 and roaring treats
// `start >= end` as a no-op, and the two tiers disagreed about it because
// `RemoveRange`'s inline branches filter on the closed interval directly.
// `graph/index/nodeset.go` now converts through `addRangeClosed` /
// `removeRangeClosed`, which split the call at the top of the range.
// [liDriveBoundary] is therefore a REGRESSION arm: it asserts the closed
// interval is honoured at `math.MaxUint64` on BOTH tiers, with the same range
// one id shorter as its control.
//
// The empty-interval defect is FIXED too (#2608): an inverted or empty
// `AddRange` on a label with no entry used to CREATE one holding an empty
// bitmap, permanently and in the serialized image — MEASURED at the time, 1 000
// such calls on distinct labels produced a 20 016-byte image against 16 bytes
// for an empty index, 20 bytes and one `labelCount` apiece, while `Count`
// reported 0 for every one. `NodeSet.AddRange` now returns before promoting when
// the interval is inverted, and `label.Index.AddRange` deletes rather than
// stores a set that is empty after the call — mirroring `RemoveRange`'s own
// godoc promise ("Empty bitmaps are deleted so the map does not grow
// unboundedly"). [liDriveWithPhantom] is a REGRESSION arm, with a range naming
// five ids as its control.
//
// The one that remains is latent — neither range method has a production caller,
// so no production label is ever built the way it requires — and it is pinned to
// its MEASURED behaviour with a clause that says so, so that fixing it fires
// loudly here instead of passing silently.
//
//  1. The serialized form is NOT idempotent for a run-encoded label small enough
//     to be down-converted on the way back in. MEASURED: an AddRange-built label
//     of 8 ids serializes to 55 bytes and, after one Serialize/Deserialize
//     cycle, to 72 — because [index.NodeSetFromBitmap] moves a bitmap of at most
//     smallSetMax ids to the inline tier, which re-materialises through
//     `AddMany` as an ARRAY container where `AddRange` had built a RUN one. The
//     control one id above the threshold stays at 55. No content is ever lost,
//     but a checkpoint, reload and re-checkpoint cycle produces different bytes
//     for the same logical state. [liDriveDenseSmall] pins it.
//
// # An over-strong assertion of this harness's own, corrected
//
// The tier arm's first draft compared an Add-built index with an AddRange-built
// one and required byte-identity. It FAILED at widths 4 and above, and the
// failure was the harness's fault, not the module's: `Serialize`'s godoc
// promises the form is deterministic "for a given in-memory state", never that
// it is a function of the logical contents. What the module actually claims
// (`graph/index/label/index.go:407-415`) is that the INLINE tier serializes like
// a bitmap holding the same ids, which is what made #1585 a zero-migration
// change — and that claim HOLDS, verified at six widths by
// [liDriveTierIdentity] with the bitmap side reached by growth-then-trim. The
// Add-versus-AddRange difference is kept as a MEASUREMENT with no clause on it
// ([liDriveRangeTier]), because the consequence is easy to assume away and is
// real: two indexes that answer every query identically can have different
// images, so byte-comparing two snapshots is not a valid way to ask whether two
// graphs carry the same labels.
//
// The same mistake nearly reached the round-trip clause. `image ==
// round-tripped image` is FALSE for the band in defect 1, and the sweep can
// reach that band — an AddRange followed by a RemoveRange that trims the label
// to a handful of ids — so the clause would have passed on this seed and flaked
// on another. What is asserted instead is that the form is a FIXPOINT after at
// most one cycle, which is true across every width probed.
//
// # What the corruption arm can and cannot reach
//
// The serialized form ends in a CRC32C over every preceding byte, so a raw
// single-byte flip is caught by the CRC WHEREVER it lands — MEASURED across all
// seven regions of the layout (magic, version, count, labelID, bitmapLen,
// bitmap payload, and the trailer itself). That is the whole detectable
// population for a bad sector, and it means the four structural guards inside
// `Deserialize` — bad magic, unsupported version, implausible bitmap length,
// bitmap parse failure — are UNREACHABLE by corruption alone. To reach them the
// image has to be damaged AND its trailer recomputed, so [liDriveCorruption]
// runs two families: a `SimDisk.CorruptRange` family that must always be refused
// by the CRC, and a re-stamped family that must reach each named guard.
//
// The re-stamped family also measures where the format has no redundancy at all:
// a re-stamped `labelCount` of 0 is ACCEPTED and yields an empty index, and the
// body's trailing bytes are ignored rather than rejected. That is not a defect
// against CRC32C's contract — an error-detecting code is not a MAC, and a
// corruption that can recompute the trailer is outside its threat model — but it
// is recorded, because a structural check that the body was fully consumed would
// cost nothing and does not exist.
//
// # Cadence
//
// The scenario is a pure in-memory exercise of one library type: no engine, no
// store, no Cypher. It therefore carries a run override and no crash/recovery
// arm — there is no durable state for a crash to damage, and the one durable
// artefact it does produce (the serialized image on a SimDisk) is damaged
// directly and deliberately, which is a stronger test than waiting for a fault
// to land on it. The soak layer sweeps seeds, widens the relationship sweep over
// longer op streams, and drives every byte offset of a small image through
// `CorruptRange` rather than seven sampled regions.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// ScenarioLabelIndexScoped is the catalogue key for this scenario.
const ScenarioLabelIndexScoped = "label-index-scoped"

// labelIndexScopedDefaultSeed is the catalogue seed.
const labelIndexScopedDefaultSeed uint64 = 0x2496_5CD0_9ED1

// labelIndexScopedSeedMix salts the scenario seed so this file's draw stream is
// disjoint from every other checker's. Nothing else in the package draws from
// it, so adding this scenario moved no existing fixture's draws.
const labelIndexScopedSeedMix uint64 = 0x2496_B17E_5C0D_7A11

// liEpochs is the number of relationship-sweep epochs the short layer drives.
// Each epoch sweeps all 2*liRelCount cells twice (isolated and accumulating),
// so the short run drives 24*26*2 = 1248 adjudicated range operations. The whole
// scenario is in-memory arithmetic and MEASURED well under a second.
const liEpochs = 24

// liUnionDraws is the number of Union subsets adjudicated per epoch.
const liUnionDraws = 4

// liLabelPool is the number of distinct label ids the accumulating phase reuses.
// Small on purpose: the labels must collide often enough that a label survives
// many operations and reaches the bitmap tier, rather than each op landing on a
// fresh singleton.
const liLabelPool = 6

// liBaseLo / liBaseSpan bound the base band's lower endpoint. It sits far from
// 0 and far from math.MaxUint64 so the relationships below can extend past the
// band on BOTH sides without underflowing or colliding with the boundary arm's
// deliberate overflow.
const (
	liBaseLo   = 1 << 20
	liBaseSpan = 1 << 16
)

// liBaseWidthMin / liBaseWidthSpan bound the base band's width. The minimum is
// above liRelPad so every relationship below stays well-formed: a "contained"
// range needs at least three members to be strictly inside, and an "overlap"
// needs the overlap and the non-overlapping tail both to be non-empty.
const (
	liBaseWidthMin  = 32
	liBaseWidthSpan = 96
)

// liRelPad is the distance the disjoint and adjacent relationships stand off
// the base band, and the reach of the overlapping ones.
const liRelPad = 8

// liProbePoints is how many membership probes [liCompareLabel] takes outside the
// label's own scan, so `Has` is exercised on non-members as well as members.
const liProbePoints = 6

// liReportCap bounds how many violations one report carries, so a wholesale
// failure cannot produce an unbounded report.
//
// It is deliberately ABOVE the maximum a single clause can emit. `range-model`
// fires once per (relationship, direction) cell plus one unattributed catch-all,
// which is liRelCount*liDirCount+1 = 27, so a cap at or below that would let one
// broken relationship crowd every other clause out of the report and hide, say,
// a simultaneous corruption failure. The package's own tests re-adjudicate the
// evidence ([liAdjudicate]) rather than reading the report, so they always see
// the complete list whatever this is set to.
const liReportCap = 32

// liDiskPath is the SimDisk path the serialized image is written to. It is
// root-level, which the SimDisk treats as durably linked on creation — this arm
// is about the bytes, not about the dirent protocol.
const liDiskPath = "label-index.bin"

// liCastagnoli is the CRC32C table the re-stamping family uses to recompute a
// damaged image's trailer. It mirrors the table `graph/index/label` uses; a
// drift would make every re-stamped image fail the CRC instead of reaching the
// structural guard it targets, which the `restamp-guard` clause reports by name
// rather than absorbing.
var liCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// -----------------------------------------------------------------------------
// The naive model
// -----------------------------------------------------------------------------

// liModel is the independent membership oracle: a plain label -> set-of-nodes
// map recomputed from the op stream. It knows nothing about roaring, nothing
// about [index.NodeSet]'s tiers, and it never consults the index.
//
// Its range methods are written the obvious way — walk the closed interval and
// set or clear each id — which is exactly what makes them an oracle for the
// inclusive/exclusive conversion the index performs. It refuses an inverted
// range by iterating zero times, the same answer the naive reading of "every id
// in [from, to]" gives.
type liModel struct {
	members map[uint32]map[uint64]bool
}

// newLIModel returns an empty model.
func newLIModel() *liModel { return &liModel{members: map[uint32]map[uint64]bool{}} }

// add records that node carries label.
func (m *liModel) add(lbl uint32, node uint64) {
	s := m.members[lbl]
	if s == nil {
		s = map[uint64]bool{}
		m.members[lbl] = s
	}
	s[node] = true
}

// remove records that node no longer carries label, dropping the label when its
// last member goes.
func (m *liModel) remove(lbl uint32, node uint64) {
	s := m.members[lbl]
	if s == nil {
		return
	}
	delete(s, node)
	if len(s) == 0 {
		delete(m.members, lbl)
	}
}

// addRange adds every id in the CLOSED interval [from, to]. An inverted range
// (from > to) names no ids and is a no-op, which is what "every id in [from,to]"
// means when the interval is empty.
func (m *liModel) addRange(lbl uint32, from, to uint64) {
	if from > to {
		return
	}
	for id := from; ; id++ {
		m.add(lbl, id)
		if id == to {
			return
		}
	}
}

// removeRange removes every id in the CLOSED interval [from, to].
func (m *liModel) removeRange(lbl uint32, from, to uint64) {
	if from > to {
		return
	}
	for id := from; ; id++ {
		m.remove(lbl, id)
		if id == to {
			return
		}
	}
}

// count returns the model's cardinality for label.
func (m *liModel) count(lbl uint32) uint64 { return uint64(len(m.members[lbl])) }

// has reports the model's membership.
func (m *liModel) has(lbl uint32, node uint64) bool { return m.members[lbl][node] }

// scan returns the model's members for label in ascending order, nil when the
// label has none — matching [label.Index.Scan]'s documented nil-for-absent
// contract, which is asserted rather than assumed (MEASURED: Scan of an absent
// label is nil, and Scan of a present-but-empty entry is nil too, because the
// index returns nil whenever the materialised array is empty).
func (m *liModel) scan(lbl uint32) []uint64 {
	s := m.members[lbl]
	if len(s) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// union returns the ascending union of the given labels' members. Duplicate and
// unknown labels contribute nothing extra, which is the naive reading of a set
// union and is what the Union clause holds the index to.
func (m *liModel) union(labels ...uint32) []uint64 {
	seen := map[uint64]bool{}
	for _, l := range labels {
		for id := range m.members[l] {
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// liveLabels returns the labels the model says carry at least one member, in
// ascending order.
func (m *liModel) liveLabels() []uint32 {
	out := make([]uint32, 0, len(m.members))
	for l, s := range m.members {
		if len(s) > 0 {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// totalMembers returns the model's total (label, node) pair count.
func (m *liModel) totalMembers() int {
	n := 0
	for _, s := range m.members {
		n += len(s)
	}
	return n
}

// -----------------------------------------------------------------------------
// The relationship sweep
// -----------------------------------------------------------------------------

// liRel names one geometric relationship between an operand range and the base
// band [lo, hi] already on the label. The set is CONSTRUCTED to bracket the
// inclusive/exclusive conversion from both sides: adjacency touches the band
// without overlapping it, overlap crosses exactly one endpoint, and the two
// degenerate kinds probe what the conversion does when the interval names no
// ids at all.
type liRel uint8

// The relationship kinds. Their order is fixed; the SWEEP order is shuffled per
// epoch, so the seed decides the order and never the coverage.
const (
	// liRelDisjointBelow lies wholly below the band with a gap.
	liRelDisjointBelow liRel = iota
	// liRelAdjacentBelow ends exactly at lo-1: it TOUCHES the band and overlaps
	// nothing. Together with liRelAdjacentAbove this is the pair an off-by-one in
	// the `to+1` conversion would break first.
	liRelAdjacentBelow
	// liRelOverlapLow crosses lo: part below the band, part inside.
	liRelOverlapLow
	// liRelContained lies strictly inside the band.
	liRelContained
	// liRelIdentical is exactly [lo, hi].
	liRelIdentical
	// liRelContaining extends past the band on both sides.
	liRelContaining
	// liRelOverlapHigh crosses hi: part inside the band, part above.
	liRelOverlapHigh
	// liRelAdjacentAbove starts exactly at hi+1.
	liRelAdjacentAbove
	// liRelDisjointAbove lies wholly above the band with a gap.
	liRelDisjointAbove
	// liRelSingleInside is the one-element range [x, x] for an x inside the band.
	liRelSingleInside
	// liRelSingleOutside is the one-element range [x, x] for an x outside it.
	liRelSingleOutside
	// liRelInverted is [hi, lo] with hi > lo: from > to by the band's whole width.
	liRelInverted
	// liRelEmptyOffByOne is [lo+1, lo]: from == to+1, the SMALLEST inverted range
	// there is. It is separate from liRelInverted because a conversion that
	// clamped rather than wrapped would treat the two differently.
	liRelEmptyOffByOne
	// liRelCount is the number of relationship kinds.
	liRelCount
)

// String renders a relationship for evidence and for a clause message.
func (r liRel) String() string {
	switch r {
	case liRelDisjointBelow:
		return "disjoint-below"
	case liRelAdjacentBelow:
		return "adjacent-below"
	case liRelOverlapLow:
		return "overlap-low"
	case liRelContained:
		return "contained"
	case liRelIdentical:
		return "identical"
	case liRelContaining:
		return "containing"
	case liRelOverlapHigh:
		return "overlap-high"
	case liRelAdjacentAbove:
		return "adjacent-above"
	case liRelDisjointAbove:
		return "disjoint-above"
	case liRelSingleInside:
		return "single-inside"
	case liRelSingleOutside:
		return "single-outside"
	case liRelInverted:
		return "inverted"
	case liRelEmptyOffByOne:
		return "empty-off-by-one"
	default:
		return fmt.Sprintf("rel(%d)", uint8(r))
	}
}

// liRelBounds returns the operand range for relationship r against the base band
// [lo, hi]. The caller guarantees hi-lo >= liBaseWidthMin and lo >= liRelPad*2,
// which [liDrawBase] enforces, so every arithmetic result below is well within
// the uint64 range and the two degenerate kinds are the ONLY ones that come back
// inverted.
func liRelBounds(lo, hi uint64, r liRel) (from, to uint64) {
	switch r {
	case liRelDisjointBelow:
		return lo - 2*liRelPad, lo - liRelPad
	case liRelAdjacentBelow:
		return lo - liRelPad, lo - 1
	case liRelOverlapLow:
		return lo - liRelPad, lo + liRelPad
	case liRelContained:
		return lo + 1, hi - 1
	case liRelIdentical:
		return lo, hi
	case liRelContaining:
		return lo - liRelPad, hi + liRelPad
	case liRelOverlapHigh:
		return hi - liRelPad, hi + liRelPad
	case liRelAdjacentAbove:
		return hi + 1, hi + liRelPad
	case liRelDisjointAbove:
		return hi + liRelPad, hi + 2*liRelPad
	case liRelSingleInside:
		return lo + liRelPad, lo + liRelPad
	case liRelSingleOutside:
		return hi + 2*liRelPad, hi + 2*liRelPad
	case liRelInverted:
		return hi, lo
	default: // liRelEmptyOffByOne
		return lo + 1, lo
	}
}

// liDir is the direction a relationship is driven in.
type liDir uint8

// The two directions. Both are swept for every relationship, because the
// inclusive/exclusive conversion happens independently in AddRange
// (`nodeset.go:339`) and RemoveRange (`nodeset.go:378`) and a defect in one says
// nothing about the other.
const (
	liDirAdd liDir = iota
	liDirRemove
	liDirCount
)

// String renders a direction.
func (d liDir) String() string {
	if d == liDirAdd {
		return "AddRange"
	}
	return "RemoveRange"
}

// liCell is one (relationship, direction) pair of the sweep.
type liCell struct {
	Rel liRel
	Dir liDir
}

// liAllCells returns every cell of the sweep in a fixed order. The caller
// shuffles it.
func liAllCells() []liCell {
	out := make([]liCell, 0, int(liRelCount)*int(liDirCount))
	for r := liRel(0); r < liRelCount; r++ {
		for d := liDir(0); d < liDirCount; d++ {
			out = append(out, liCell{Rel: r, Dir: d})
		}
	}
	return out
}

// liShuffleCells returns a seed-shuffled copy of cells. It is a Fisher-Yates
// over the seed's own stream, so the order is a pure function of the seed and
// the COVERAGE is a property of the slice rather than of the draw.
func liShuffleCells(seed *Seed, cells []liCell) []liCell {
	out := append([]liCell(nil), cells...)
	for i := len(out) - 1; i > 0; i-- {
		j := seed.IntN(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// liCellEvidence aggregates every drive of one (relationship, direction) cell.
// The two delta columns are the compact form of the parity claim: how many
// (label, node) pairs the INDEX gained or lost across the cell's drives, beside
// how many the MODEL did. They are equal on a correct index and they are what an
// off-by-one in the inclusive/exclusive conversion moves by exactly one per
// drive.
type liCellEvidence struct {
	Rel        liRel
	Dir        liDir
	Drives     int
	Mismatches int
	IndexDelta int64
	ModelDelta int64
}

// liCorruptFamily names which of the two damage families a corruption row
// belongs to.
type liCorruptFamily uint8

// The two families. They exist because the CRC32C trailer bounds what a raw
// corruption can reach: every raw flip is caught by the checksum, so the four
// structural guards inside Deserialize are reachable only by an image whose
// trailer was recomputed after the damage.
const (
	// liFamilyRaw is a byte flip applied through [SimDisk.CorruptRange], with the
	// trailer left as it was.
	liFamilyRaw liCorruptFamily = iota
	// liFamilyRestamp is a structural edit whose CRC32C trailer is recomputed, so
	// the image is internally consistent and the reader's own guards decide.
	liFamilyRestamp
	// liFamilyTruncate is a short read: the image cut to fewer bytes.
	liFamilyTruncate
)

// String renders a family.
func (f liCorruptFamily) String() string {
	switch f {
	case liFamilyRaw:
		return "raw"
	case liFamilyRestamp:
		return "restamp"
	default:
		return "truncate"
	}
}

// liCorruptEvidence is one damaged-image trial.
type liCorruptEvidence struct {
	// Region names what was damaged, for the report and for the coverage gate.
	Region string
	// Guard is the guard that answered, classified from the error's own wording
	// by [liClassifyGuard]. "accepted" when the image was not refused at all.
	Guard  string
	Family liCorruptFamily
	// Off and Len describe the damage.
	Off int64
	Len int
	// WantGuard is the guard this trial was constructed to reach. Empty means the
	// trial only requires SOME refusal, which is the raw family's contract: the
	// CRC catches everything, so which byte moved does not change the answer.
	WantGuard string
	// Refused / IsCorrupted / Restored are the three things a refusal must get
	// right: it happened, it wrapped the sentinel, and the receiver survived it.
	Refused     bool
	IsCorrupted bool
	Restored    bool
	// TargetWasPopulated records that the receiver held state before the call, so
	// "restored" is a claim about something rather than about an empty index.
	TargetWasPopulated bool
}

// liScopeEvidence is one (constructor, change kind) routing trial.
type liScopeEvidence struct {
	Ctor string
	Op   string
	// Scope is the scope the constructor reported.
	Scope uint8
	// Accepted is what the index did; WantAccepted is what the naive routing
	// table says it should have done.
	Accepted     bool
	WantAccepted bool
}

// liBoundaryEvidence records the measured behaviour at the top of the NodeID
// range. Since #2607 it is a CONTRACT: the closed interval must be honoured at
// math.MaxUint64, identically on both tiers. See the file header.
type liBoundaryEvidence struct {
	// AddNaive / AddGot are what a naive reading of the closed interval says the
	// cardinality should be, and what the index produced, for a range ending at
	// math.MaxUint64.
	AddNaive uint64
	AddGot   uint64
	// AddBelowNaive / AddBelowGot are the CONTROL: the same range one id shorter,
	// which must be exact. Without it "the range was dropped" could be a property
	// of the whole top of the id space rather than of the final id.
	AddBelowNaive uint64
	AddBelowGot   uint64
	// RemoveBefore / RemoveNaive / RemoveGot are the same experiment for
	// RemoveRange: the cardinality before, what a naive removal leaves, and what
	// the index left.
	RemoveBefore uint64
	RemoveNaive  uint64
	RemoveGot    uint64
	// RemoveInlineBefore / RemoveInlineGot repeat the RemoveRange experiment over
	// an INLINE-tier label holding the identical membership. The tier a set
	// occupies is not observable through the public surface, so the two must
	// agree; before #2607 they did not, and that divergence was the sharper half
	// of the defect.
	RemoveInlineBefore uint64
	RemoveInlineGot    uint64
}

// liPhantomEvidence records the measured cost of an inverted AddRange on a label
// with no entry.
type liPhantomEvidence struct {
	// EmptyBytes is the serialized size of an index with nothing in it.
	EmptyBytes int
	// AfterBytes is the size after N inverted AddRange calls on distinct labels,
	// and AfterLabelCount the labelCount those bytes declare.
	AfterBytes      int
	AfterLabelCount uint32
	// Labels is N.
	Labels int
	// QueryVisible records whether an empty-interval label is observable through
	// the query surface at all: Count, Scan and Has must ALL say the label is
	// empty. This was true of the old phantom and must stay true.
	QueryVisible bool
	// RoundTripLabelCount is the labelCount the image declares after a
	// Serialize/Deserialize/Serialize cycle. It must equal AfterLabelCount.
	RoundTripLabelCount uint32
	// CtrlLabelCount / CtrlCount are the CONTROL: a range naming at least one id
	// must still be recorded, so that "no entry was created" cannot be satisfied
	// by AddRange having stopped working altogether.
	CtrlLabelCount uint32
	CtrlCount      uint64
}

// liTierRow is one width of the Add-versus-AddRange byte comparison. It is
// MEASUREMENT, not a clause: see [liDriveRangeTier] for why the two are allowed
// to differ and what the difference is.
type liTierRow struct {
	Width      int
	AddBytes   int
	RangeBytes int
	Equal      bool
}

// liDenseSmallEvidence records the round-trip fixpoint experiment over the
// narrow band where a run-container label is small enough to be down-converted
// on the way back in. See [liDriveDenseSmall].
type liDenseSmallEvidence struct {
	// The TREATMENT: an AddRange-built label of exactly smallSetMax ids.
	Width  int
	First  int
	Second int
	Third  int
	// Stable is whether the first cycle left the bytes alone. MEASURED false.
	Stable bool
	// The CONTROL: the same construction one id ABOVE the threshold, which stays
	// on the bitmap tier and MEASURED true.
	CtrlWidth  int
	CtrlFirst  int
	CtrlSecond int
	CtrlStable bool
}

// LabelIndexScopedEvidence is everything one run measured.
type LabelIndexScopedEvidence struct {
	Cells   []liCellEvidence
	Corrupt []liCorruptEvidence
	Scopes  []liScopeEvidence

	Boundary liBoundaryEvidence
	Phantom  liPhantomEvidence

	// FirstMismatch localises the first membership disagreement of the whole run,
	// so a failing report names an id rather than only a count.
	FirstMismatch string

	// Sweep aggregates.
	Epochs       int
	RangeOps     int
	SingleOps    int
	Compares     int
	Mismatches   int
	FinalLabels  int
	FinalMembers int
	PeakMembers  int
	// EmptiedLabels counts the isolated labels a RemoveRange drove from non-empty
	// to empty, which is the path RemoveRange's godoc promises deletes the map
	// entry.
	EmptiedLabels int
	// PromotedAfterAdd counts the epochs in which the accumulating label took
	// individual Adds BEFORE its first AddRange of the epoch. It is a property of
	// the OP STREAM, not an observation of the tier: which tier a set sits on is
	// deliberately unobservable through this index's public surface (the whole
	// point of the byte-identity claim), so this counter says what was DRIVEN and
	// claims nothing about what the set became.
	PromotedAfterAdd int

	// Union arm.
	UnionDraws        int
	UnionMismatches   int
	UnionMultiLabel   int
	UnionUnknownLabel int
	UnionDuplicate    int
	UnionEmptyDraws   int

	// Round-trip arm.
	RangeTier  []liTierRow
	DenseSmall liDenseSmallEvidence

	RoundTrips        int
	RTContentMismatch int
	// RTByteMismatch counts fixpoint failures: the image produced by the SECOND
	// serialize must equal the one produced by the third. The FIRST cycle is
	// deliberately not asserted — see [liDriveRoundTrip].
	RTByteMismatch int
	// FirstCycleStable records whether the very first round trip left the bytes
	// alone. It is a MEASUREMENT, not a claim: a run-container label whose
	// cardinality is at most smallSetMax is re-tiered on the way back in and
	// legitimately changes size. [liDriveDenseSmall] pins that case directly.
	FirstCycleStable  bool
	SerializeReruns   int
	SerializeUnstable int
	TierChecks        int
	TierMismatch      int
	ImageBytes        int
	// ImageBytes2 is the size of the image the second serialize produced.
	ImageBytes2 int
	// ImageLabelCount is the labelCount the final image declares, and
	// ModelLiveLabels how many labels the model says carry a member. The excess
	// is PhantomExcess: entries the index holds for labels with nothing in them.
	ImageLabelCount uint32
	ModelLiveLabels int
	PhantomExcess   int

	// Digest is an order-sensitive hash of every reproducible fact.
	Digest  uint64
	Perturb liPerturb
}

// liMix is the FNV-1a step the digest is built from.
func liMix(h, v uint64) uint64 { return (h ^ v) * 1099511628211 }

// liBoolBits renders a bool as a digest input.
func liBoolBits(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// liStrBits folds a string into the digest.
func liStrBits(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h = liMix(h, uint64(s[i]))
	}
	return h
}

// computeDigest folds every reproducible fact into an order-sensitive hash.
// Everything this scenario measures IS a pure function of the seed — it drives
// one library type with no clock, no goroutine and no process-global counter —
// so nothing is excluded, which is why the determinism claim can be an equality
// on the whole digest rather than on a filtered summary.
func (e *LabelIndexScopedEvidence) computeDigest() uint64 {
	h := uint64(14695981039346656037)
	for _, v := range []int64{
		int64(e.Epochs), int64(e.RangeOps), int64(e.SingleOps), int64(e.Compares),
		int64(e.Mismatches), int64(e.FinalLabels), int64(e.FinalMembers), int64(e.PeakMembers),
		int64(e.EmptiedLabels), int64(e.PromotedAfterAdd),
		int64(e.UnionDraws), int64(e.UnionMismatches), int64(e.UnionMultiLabel),
		int64(e.UnionUnknownLabel), int64(e.UnionDuplicate), int64(e.UnionEmptyDraws),
		int64(e.RoundTrips), int64(e.RTContentMismatch), int64(e.RTByteMismatch),
		int64(e.SerializeReruns), int64(e.SerializeUnstable), int64(e.TierChecks),
		int64(e.TierMismatch), int64(e.ImageBytes), int64(e.ImageBytes2),
		int64(e.ImageLabelCount), int64(e.ModelLiveLabels), int64(e.PhantomExcess),
	} {
		h = liMix(h, uint64(v))
	}
	h = liMix(h, liBoolBits(e.FirstCycleStable))
	for i := range e.RangeTier {
		r := &e.RangeTier[i]
		h = liMix(h, uint64(r.Width))
		h = liMix(h, uint64(r.AddBytes))
		h = liMix(h, uint64(r.RangeBytes))
		h = liMix(h, liBoolBits(r.Equal))
	}
	for _, v := range []int{
		e.DenseSmall.Width, e.DenseSmall.First, e.DenseSmall.Second, e.DenseSmall.Third,
		e.DenseSmall.CtrlWidth, e.DenseSmall.CtrlFirst, e.DenseSmall.CtrlSecond,
	} {
		h = liMix(h, uint64(v))
	}
	h = liMix(h, liBoolBits(e.DenseSmall.Stable))
	h = liMix(h, liBoolBits(e.DenseSmall.CtrlStable))
	for i := range e.Cells {
		c := &e.Cells[i]
		for _, v := range []int64{
			int64(c.Rel), int64(c.Dir), int64(c.Drives), int64(c.Mismatches),
			c.IndexDelta, c.ModelDelta,
		} {
			h = liMix(h, uint64(v))
		}
	}
	for i := range e.Corrupt {
		c := &e.Corrupt[i]
		h = liStrBits(h, c.Region)
		h = liStrBits(h, c.Guard)
		h = liStrBits(h, c.WantGuard)
		h = liMix(h, uint64(c.Family))
		h = liMix(h, uint64(c.Off))
		h = liMix(h, uint64(c.Len))
		h = liMix(h, liBoolBits(c.Refused))
		h = liMix(h, liBoolBits(c.IsCorrupted))
		h = liMix(h, liBoolBits(c.Restored))
		h = liMix(h, liBoolBits(c.TargetWasPopulated))
	}
	for i := range e.Scopes {
		s := &e.Scopes[i]
		h = liStrBits(h, s.Ctor)
		h = liStrBits(h, s.Op)
		h = liMix(h, uint64(s.Scope))
		h = liMix(h, liBoolBits(s.Accepted))
		h = liMix(h, liBoolBits(s.WantAccepted))
	}
	b := &e.Boundary
	for _, v := range []uint64{
		b.AddNaive, b.AddGot, b.AddBelowNaive, b.AddBelowGot,
		b.RemoveBefore, b.RemoveNaive, b.RemoveGot,
	} {
		h = liMix(h, v)
	}
	p := &e.Phantom
	for _, v := range []int64{
		int64(p.EmptyBytes), int64(p.AfterBytes), int64(p.AfterLabelCount), int64(p.Labels),
	} {
		h = liMix(h, uint64(v))
	}
	h = liMix(h, liBoolBits(p.QueryVisible))
	h = liMix(h, uint64(p.RoundTripLabelCount))
	h = liMix(h, uint64(p.CtrlLabelCount))
	h = liMix(h, p.CtrlCount)
	return h
}

// String renders the evidence for a report and a log line.
func (e *LabelIndexScopedEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "label-index-scoped: epochs=%d range-ops=%d single-ops=%d compares=%d; ",
		e.Epochs, e.RangeOps, e.SingleOps, e.Compares)
	fmt.Fprintf(&b, "final labels=%d members=%d (peak %d) emptied=%d promoted-after-add=%d mismatches=%d; ",
		e.FinalLabels, e.FinalMembers, e.PeakMembers, e.EmptiedLabels, e.PromotedAfterAdd, e.Mismatches)
	if e.FirstMismatch != "" {
		fmt.Fprintf(&b, "first-mismatch=%q; ", e.FirstMismatch)
	}
	fmt.Fprintf(&b, "union draws=%d multi=%d unknown=%d dup=%d empty=%d mismatch=%d; ",
		e.UnionDraws, e.UnionMultiLabel, e.UnionUnknownLabel, e.UnionDuplicate,
		e.UnionEmptyDraws, e.UnionMismatches)
	fmt.Fprintf(&b, "round-trips=%d content-bad=%d fixpoint-bad=%d first-cycle-stable=%v; "+
		"serialize reruns=%d unstable=%d; ",
		e.RoundTrips, e.RTContentMismatch, e.RTByteMismatch, e.FirstCycleStable,
		e.SerializeReruns, e.SerializeUnstable)
	fmt.Fprintf(&b, "tier-identity %d checks %d bad; image=%dB->%dB labelCount=%d model-live=%d "+
		"phantom-excess=%d; ",
		e.TierChecks, e.TierMismatch, e.ImageBytes, e.ImageBytes2, e.ImageLabelCount,
		e.ModelLiveLabels, e.PhantomExcess)
	d := &e.DenseSmall
	fmt.Fprintf(&b, "dense-small w=%d %dB->%dB->%dB stable=%v (control w=%d %dB->%dB stable=%v); ",
		d.Width, d.First, d.Second, d.Third, d.Stable,
		d.CtrlWidth, d.CtrlFirst, d.CtrlSecond, d.CtrlStable)
	fmt.Fprintf(&b, "boundary add %d/%d (control %d/%d) remove %d->%d/%d; ",
		e.Boundary.AddGot, e.Boundary.AddNaive, e.Boundary.AddBelowGot, e.Boundary.AddBelowNaive,
		e.Boundary.RemoveBefore, e.Boundary.RemoveGot, e.Boundary.RemoveNaive)
	fmt.Fprintf(&b, "empty-interval %d labels %dB->%dB count=%d rt-count=%d query-visible=%v "+
		"(control count=%d labels=%d); ",
		e.Phantom.Labels, e.Phantom.EmptyBytes, e.Phantom.AfterBytes, e.Phantom.AfterLabelCount,
		e.Phantom.RoundTripLabelCount, e.Phantom.QueryVisible,
		e.Phantom.CtrlCount, e.Phantom.CtrlLabelCount)
	fmt.Fprintf(&b, "perturb=%s; digest=%#016x", e.Perturb, e.Digest)
	for i := range e.Cells {
		c := &e.Cells[i]
		fmt.Fprintf(&b, "\n  %-16s %-11s drives=%d mismatch=%d delta index=%+d model=%+d",
			c.Rel, c.Dir, c.Drives, c.Mismatches, c.IndexDelta, c.ModelDelta)
	}
	for i := range e.Corrupt {
		c := &e.Corrupt[i]
		fmt.Fprintf(&b, "\n  corrupt %-9s %-22s off=%d len=%d refused=%v sentinel=%v restored=%v "+
			"guard=%s want=%q populated=%v",
			c.Family, c.Region, c.Off, c.Len, c.Refused, c.IsCorrupted, c.Restored,
			c.Guard, c.WantGuard, c.TargetWasPopulated)
	}
	for i := range e.Scopes {
		s := &e.Scopes[i]
		fmt.Fprintf(&b, "\n  scope %-14s scope=%d %-18s accepted=%v want=%v",
			s.Ctor, s.Scope, s.Op, s.Accepted, s.WantAccepted)
	}
	for i := range e.RangeTier {
		r := &e.RangeTier[i]
		fmt.Fprintf(&b, "\n  range-tier w=%-4d Add=%dB AddRange=%dB identical=%v",
			r.Width, r.AddBytes, r.RangeBytes, r.Equal)
	}
	return b.String()
}

// ReproducibleSummary renders the facts a determinism comparison uses. Every
// measurement this scenario takes is a pure function of the seed, so this is the
// digest plus the aggregates that would localise a divergence.
func (e *LabelIndexScopedEvidence) ReproducibleSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "epochs=%d ops=%d/%d cmp=%d labels=%d members=%d/%d union=%d/%d rt=%d/%d/%d "+
		"tier=%d/%d img=%d/%d live=%d phantom=%d corrupt=%d scope=%d bad=%d empt=%d pro=%d digest=%#016x",
		e.Epochs, e.RangeOps, e.SingleOps, e.Compares, e.FinalLabels, e.FinalMembers,
		e.PeakMembers, e.UnionDraws, e.UnionMismatches, e.RoundTrips, e.RTContentMismatch,
		e.RTByteMismatch, e.TierChecks, e.TierMismatch, e.ImageBytes, e.ImageLabelCount,
		e.ModelLiveLabels, e.PhantomExcess, len(e.Corrupt), len(e.Scopes), e.Mismatches,
		e.EmptiedLabels, e.PromotedAfterAdd, e.Digest)
	fmt.Fprintf(&b, " img2=%d fcs=%v ds=%d/%d/%d/%v ctl=%d/%d/%v", e.ImageBytes2,
		e.FirstCycleStable, e.DenseSmall.First, e.DenseSmall.Second, e.DenseSmall.Third,
		e.DenseSmall.Stable, e.DenseSmall.CtrlFirst, e.DenseSmall.CtrlSecond,
		e.DenseSmall.CtrlStable)
	for i := range e.RangeTier {
		r := &e.RangeTier[i]
		fmt.Fprintf(&b, " t%d[%d:%d/%d %v]", i, r.Width, r.AddBytes, r.RangeBytes, r.Equal)
	}
	for i := range e.Cells {
		c := &e.Cells[i]
		fmt.Fprintf(&b, " c%d[%d/%d d=%d m=%d %+d/%+d]", i, c.Rel, c.Dir, c.Drives,
			c.Mismatches, c.IndexDelta, c.ModelDelta)
	}
	for i := range e.Corrupt {
		c := &e.Corrupt[i]
		fmt.Fprintf(&b, " k%d[%s@%d r=%v s=%v x=%v %s]", i, c.Region, c.Off,
			c.Refused, c.IsCorrupted, c.Restored, c.Guard)
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// Perturbations
// -----------------------------------------------------------------------------

// liPerturb names one deliberate corruption, applied at the exact comparison
// point the clause it targets reads. Perturbations are threaded as a
// configuration field rather than held in a package-level variable, so two
// concurrent runs never see each other's.
//
// Every clause and every non-vacuity gate in this file has one, and
// [TestLabelIndexScoped_PerturbationsFire] requires each to fire its NAMED
// target with the unperturbed control silent first. A clause nobody has shown
// can fail is not a clause.
type liPerturb uint8

// The perturbations.
const (
	// liPerturbNone is the unperturbed run.
	liPerturbNone liPerturb = iota
	// liPerturbDropScanTail drops the last id of the index's Scan before it is
	// compared, reproducing a range whose upper endpoint was excluded — the exact
	// output an off-by-one in the `to+1` conversion produces.
	liPerturbDropScanTail
	// liPerturbBumpCount reports Count one higher than the index returned,
	// reproducing a cardinality that disagrees with the members.
	liPerturbBumpCount
	// liPerturbFlipHas inverts one membership probe, reproducing a Contains that
	// disagrees with the set it is probing.
	liPerturbFlipHas
	// liPerturbDropUnionMember drops one id from the index's Union result,
	// reproducing a fold that missed a label.
	liPerturbDropUnionMember
	// liPerturbRoundTripContent drops a label from the deserialized image before
	// it is compared with the model, reproducing a reader that lost an entry.
	liPerturbRoundTripContent
	// liPerturbRoundTripBytes mangles the re-serialized image, reproducing a
	// round trip that is not byte-stable.
	liPerturbRoundTripBytes
	// liPerturbUnstableSerialize mangles the second of two Serialize calls on one
	// unchanged index, reproducing a non-deterministic emission order.
	liPerturbUnstableSerialize
	// liPerturbTierDivergence mangles the bitmap-tier image of the tier-identity
	// pair, reproducing a format whose bytes depend on which tier the set sits on.
	liPerturbTierDivergence
	// liPerturbSkipDamage leaves a corruption trial's image intact while still
	// requiring a refusal, reproducing damage the reader failed to detect.
	liPerturbSkipDamage
	// liPerturbHideRestore reports a refused Deserialize's receiver as changed,
	// reproducing a reader that mutated state before failing.
	liPerturbHideRestore
	// liPerturbBreakCleanControl damages the clean control image, so the arm's
	// "an undamaged image is accepted" premise fails and the refusals stop being
	// attributable to the damage.
	liPerturbBreakCleanControl
	// liPerturbWrongGuard re-targets a re-stamped trial at a guard it cannot
	// reach, reproducing a structural check that stopped answering.
	liPerturbWrongGuard
	// liPerturbScopeSwap reports NewEdgeIndex's scope as ScopeNode.
	liPerturbScopeSwap
	// liPerturbScopeRouting flips one Apply routing outcome, reproducing a
	// subscriber that consumed a change kind outside its scope.
	liPerturbScopeRouting
	// liPerturbBoundaryWraps reports the math.MaxUint64 range as having been
	// dropped whole, in both directions. It reproduces the pre-#2607 `to+1`
	// overflow, so the regression arm has a demonstrated way to fail.
	liPerturbBoundaryWraps
	// liPerturbPhantomKept reports the empty-interval experiment as leaving one
	// entry behind apiece. It reproduces the pre-#2608 measurement, so the
	// regression arm has a demonstrated way to fail.
	liPerturbPhantomKept
	// liPerturbPhantomCtrlBad zeroes the non-empty CONTROL's labelCount, so the
	// arm cannot be satisfied by an AddRange that records nothing at all.
	liPerturbPhantomCtrlBad
	// liPerturbEntryFloor reports a serialized labelCount below the number of
	// labels the model says carry a member, reproducing a lost entry.
	liPerturbEntryFloor
	// liPerturbEmptySweep drives no cells at all, so the coverage gate has
	// nothing to certify.
	liPerturbEmptySweep
	// liPerturbUnionSingleShape draws only single known labels, so the union arm
	// never reaches a multi-label, unknown-label or duplicate-label subset.
	liPerturbUnionSingleShape
	// liPerturbSkipRegions drives one corruption region instead of the full set.
	liPerturbSkipRegions
	// liPerturbEmptyTarget deserializes into an EMPTY receiver, so "the receiver
	// was restored" is satisfied by an index that had nothing to restore.
	liPerturbEmptyTarget
	// liPerturbBoundaryControlBad breaks the boundary arm's control, so the loss
	// at math.MaxUint64 is no longer attributable to the final id.
	liPerturbBoundaryControlBad
	// liPerturbRangeTierFlat reports every Add-versus-AddRange width as
	// identical, so the measurement no longer brackets the crossover.
	liPerturbRangeTierFlat
	// liPerturbDenseSmallStable reports the dense-small round trip as
	// byte-stable. It is the tripwire direction: this is what the evidence will
	// look like the day the down-convert stops re-encoding the container.
	liPerturbDenseSmallStable
	// liPerturbDenseSmallCtrl reports the dense-small CONTROL as unstable, so the
	// instability stops being attributable to the smallSetMax threshold.
	liPerturbDenseSmallCtrl
)

// String renders a perturbation.
func (p liPerturb) String() string {
	switch p {
	case liPerturbNone:
		return "none"
	case liPerturbDropScanTail:
		return "drop-scan-tail"
	case liPerturbBumpCount:
		return "bump-count"
	case liPerturbFlipHas:
		return "flip-has"
	case liPerturbDropUnionMember:
		return "drop-union-member"
	case liPerturbRoundTripContent:
		return "roundtrip-content"
	case liPerturbRoundTripBytes:
		return "roundtrip-bytes"
	case liPerturbUnstableSerialize:
		return "unstable-serialize"
	case liPerturbTierDivergence:
		return "tier-divergence"
	case liPerturbSkipDamage:
		return "skip-damage"
	case liPerturbHideRestore:
		return "hide-restore"
	case liPerturbBreakCleanControl:
		return "break-clean-control"
	case liPerturbWrongGuard:
		return "wrong-guard"
	case liPerturbScopeSwap:
		return "scope-swap"
	case liPerturbScopeRouting:
		return "scope-routing"
	case liPerturbBoundaryWraps:
		return "boundary-wraps"
	case liPerturbPhantomKept:
		return "phantom-kept"
	case liPerturbPhantomCtrlBad:
		return "phantom-control-bad"
	case liPerturbEntryFloor:
		return "entry-floor"
	case liPerturbEmptySweep:
		return "empty-sweep"
	case liPerturbUnionSingleShape:
		return "union-single-shape"
	case liPerturbSkipRegions:
		return "skip-regions"
	case liPerturbEmptyTarget:
		return "empty-target"
	case liPerturbBoundaryControlBad:
		return "boundary-control-bad"
	case liPerturbRangeTierFlat:
		return "range-tier-flat"
	case liPerturbDenseSmallStable:
		return "dense-small-stable"
	case liPerturbDenseSmallCtrl:
		return "dense-small-ctrl"
	default:
		return fmt.Sprintf("perturb(%d)", uint8(p))
	}
}

// liAllPerturbs returns every perturbation except [liPerturbNone], for the
// short-layer test that requires each to fire its named target.
func liAllPerturbs() []liPerturb {
	out := make([]liPerturb, 0, int(liPerturbDenseSmallCtrl))
	for p := liPerturbDropScanTail; p <= liPerturbDenseSmallCtrl; p++ {
		out = append(out, p)
	}
	return out
}

// -----------------------------------------------------------------------------
// The relationship sweep
// -----------------------------------------------------------------------------

// liDrawBase draws the epoch's base band. Both endpoints sit far from 0 and far
// from math.MaxUint64 so every relationship in [liRelBounds] stays inside the id
// space, and the width is at least liBaseWidthMin so the "contained" and
// "overlap" kinds are non-degenerate.
func liDrawBase(seed *Seed) (lo, hi uint64) {
	lo = liBaseLo + seed.Uint64N(liBaseSpan)
	hi = lo + liBaseWidthMin + seed.Uint64N(liBaseWidthSpan)
	return lo, hi
}

// liCompareLabel adjudicates one label's whole query surface against the model:
// the ascending member list from Scan, the cardinality from Count, and Has at
// every member plus liProbePoints non-members drawn around the band. It returns
// the number of disagreements and a human-readable first difference.
//
// The three are compared TOGETHER rather than separately because they are three
// different code paths over the same [index.NodeSet] — Scan goes through
// ToArray, Count through Cardinality, Has through Contains — and a tier bug can
// move one without moving the others.
func liCompareLabel(
	idx *label.Index, m *liModel, lbl uint32, seed *Seed, p liPerturb,
) (int, string) {
	want := m.scan(lbl)
	gotIDs := idx.Scan(lbl)
	got := make([]uint64, len(gotIDs))
	for i, v := range gotIDs {
		got[i] = uint64(v)
	}
	if p == liPerturbDropScanTail && len(got) > 0 {
		got = got[:len(got)-1]
	}

	bad, detail := 0, ""
	note := func(format string, args ...any) {
		bad++
		if detail == "" {
			detail = fmt.Sprintf(format, args...)
		}
	}
	if len(got) != len(want) {
		note("label %d: Scan returned %d ids, the model holds %d", lbl, len(got), len(want))
	} else {
		for i := range got {
			if got[i] != want[i] {
				note("label %d: Scan[%d] = %d, the model holds %d", lbl, i, got[i], want[i])
				break
			}
		}
	}
	// Scan's documented nil-for-absent contract, asserted rather than assumed: a
	// caller that ranges over the result cannot tell nil from empty, but a caller
	// that compares against nil can.
	if len(want) == 0 && gotIDs != nil {
		note("label %d: Scan returned a non-nil empty slice; the godoc says nil when the "+
			"label has no entries", lbl)
	}

	gotCount := idx.Count(lbl)
	if p == liPerturbBumpCount {
		gotCount++
	}
	if gotCount != m.count(lbl) {
		note("label %d: Count = %d, the model holds %d", lbl, gotCount, m.count(lbl))
	}

	probes := append([]uint64(nil), want...)
	base := uint64(liBaseLo)
	if len(want) > 0 {
		base = want[0]
	}
	for i := 0; i < liProbePoints; i++ {
		// Probe a window around the label's own lowest member, so the non-member
		// probes land next to the band rather than in unrelated id space where any
		// implementation would answer false.
		probes = append(probes, base+seed.Uint64N(4*liRelPad)-2*liRelPad)
	}
	for i, id := range probes {
		gotHas := idx.Has(lbl, graph.NodeID(id))
		if p == liPerturbFlipHas && i == 0 {
			gotHas = !gotHas
		}
		if gotHas != m.has(lbl, id) {
			note("label %d: Has(%d) = %v, the model says %v", lbl, id, gotHas, m.has(lbl, id))
			break
		}
	}
	return bad, detail
}

// liIsolatedLabel returns the label id the isolated half of a cell uses. It is a
// pure function of the epoch and the cell position, disjoint from the
// accumulating pool (0..liLabelPool-1) and from the label ids the boundary and
// phantom arms use, so no two arms can interfere.
func liIsolatedLabel(epoch, cellIdx int) uint32 {
	return uint32(1000 + epoch*1000 + cellIdx)
}

// liApplyRange drives one range operation on both the index and the model.
func liApplyRange(idx *label.Index, m *liModel, lbl uint32, dir liDir, from, to uint64) {
	if dir == liDirAdd {
		idx.AddRange(lbl, graph.NodeID(from), graph.NodeID(to))
		m.addRange(lbl, from, to)
		return
	}
	idx.RemoveRange(lbl, graph.NodeID(from), graph.NodeID(to))
	m.removeRange(lbl, from, to)
}

// liSweepState carries the per-cell aggregates through the sweep.
type liSweepState struct {
	cells map[liCell]*liCellEvidence
}

// note records one drive of a cell.
func (s *liSweepState) note(c liCell, bad int, indexDelta, modelDelta int64) {
	e := s.cells[c]
	if e == nil {
		e = &liCellEvidence{Rel: c.Rel, Dir: c.Dir}
		s.cells[c] = e
	}
	e.Drives++
	e.Mismatches += bad
	e.IndexDelta += indexDelta
	e.ModelDelta += modelDelta
}

// flatten returns the per-cell evidence in the fixed (relationship, direction)
// order, so the digest is independent of map iteration.
func (s *liSweepState) flatten() []liCellEvidence {
	out := make([]liCellEvidence, 0, len(s.cells))
	for r := liRel(0); r < liRelCount; r++ {
		for d := liDir(0); d < liDirCount; d++ {
			if e := s.cells[liCell{Rel: r, Dir: d}]; e != nil {
				out = append(out, *e)
			}
		}
	}
	return out
}

// liDriveSweep drives the whole relationship sweep against one index and one
// model, and returns them for the arms that follow.
//
// Each epoch draws a base band and sweeps every (relationship, direction) cell
// in a shuffled order, twice over:
//
//   - ISOLATED — a fresh label carrying exactly the base band takes the one
//     operation, is adjudicated, and is then cleared with a RemoveRange wide
//     enough to cover every id any relationship can reach. The isolation is what
//     makes a mismatch attributable to the RELATIONSHIP rather than to whatever
//     the label already held, and the clear exercises the empty-the-entry path
//     RemoveRange's godoc describes.
//   - ACCUMULATING — a label from a small reused pool takes the same operation on
//     top of everything it already holds, and is adjudicated. This is where
//     overlapping history, repeated promotion and interleaved single-id
//     operations meet.
//
// Individual Add and Remove calls are interleaved on the pool label BEFORE the
// epoch's range operations, so the epoch drives the add-then-range order as well
// as the range-only one.
func liDriveSweep(
	cfg *LabelIndexScopedConfig, seed *Seed, ev *LabelIndexScopedEvidence,
) (*label.Index, *liModel) {
	idx := label.NewIndex()
	m := newLIModel()
	st := &liSweepState{cells: map[liCell]*liCellEvidence{}}

	epochs := cfg.Epochs
	if cfg.Perturb == liPerturbEmptySweep {
		epochs = 0
	}
	ev.Epochs = epochs

	noteBad := func(bad int, detail string) {
		ev.Compares++
		ev.Mismatches += bad
		if bad > 0 && ev.FirstMismatch == "" {
			ev.FirstMismatch = detail
		}
	}

	for epoch := 0; epoch < epochs; epoch++ {
		lo, hi := liDrawBase(seed)
		pool := uint32(seed.IntN(liLabelPool))

		// Individual ids first, so this epoch's ranges land on a label that already
		// holds scattered members rather than on a clean band.
		singles := 1 + seed.IntN(4)
		added := false
		for k := 0; k < singles; k++ {
			id := lo + seed.Uint64N(hi-lo+1)
			if seed.Bool(0.25) {
				idx.Remove(pool, graph.NodeID(id))
				m.remove(pool, id)
			} else {
				idx.Add(pool, graph.NodeID(id))
				m.add(pool, id)
				added = true
			}
			ev.SingleOps++
		}
		// Counted only when an ADD really happened: the draw can make every single
		// op of an epoch a Remove, and an epoch of removals does not drive the
		// add-then-range order this counter certifies.
		if added {
			ev.PromotedAfterAdd++
		}

		for ci, cell := range liShuffleCells(seed, liAllCells()) {
			from, to := liRelBounds(lo, hi, cell.Rel)

			// Isolated half.
			iso := liIsolatedLabel(epoch, ci)
			idx.AddRange(iso, graph.NodeID(lo), graph.NodeID(hi))
			m.addRange(iso, lo, hi)
			ev.RangeOps++

			ib, mb := idx.Count(iso), m.count(iso)
			liApplyRange(idx, m, iso, cell.Dir, from, to)
			ev.RangeOps++
			ia, ma := idx.Count(iso), m.count(iso)
			bad, detail := liCompareLabel(idx, m, iso, seed, cfg.Perturb)
			noteBad(bad, detail)
			st.note(cell, bad, int64(ia)-int64(ib), int64(ma)-int64(mb))

			// Clear the isolated label across the widest span any relationship
			// reaches, so the next cell starts from nothing and the emptying path is
			// driven every time.
			if ia > 0 {
				ev.EmptiedLabels++
			}
			clearLo, clearHi := lo-2*liRelPad, hi+2*liRelPad
			idx.RemoveRange(iso, graph.NodeID(clearLo), graph.NodeID(clearHi))
			m.removeRange(iso, clearLo, clearHi)
			ev.RangeOps++
			bad, detail = liCompareLabel(idx, m, iso, seed, cfg.Perturb)
			noteBad(bad, detail)

			// Accumulating half.
			ib, mb = idx.Count(pool), m.count(pool)
			liApplyRange(idx, m, pool, cell.Dir, from, to)
			ev.RangeOps++
			ia, ma = idx.Count(pool), m.count(pool)
			bad, detail = liCompareLabel(idx, m, pool, seed, cfg.Perturb)
			noteBad(bad, detail)
			st.note(cell, bad, int64(ia)-int64(ib), int64(ma)-int64(mb))

			if n := m.totalMembers(); n > ev.PeakMembers {
				ev.PeakMembers = n
			}
		}
	}

	ev.Cells = st.flatten()
	ev.FinalLabels = len(m.liveLabels())
	ev.FinalMembers = m.totalMembers()
	return idx, m
}

// -----------------------------------------------------------------------------
// The Union arm
// -----------------------------------------------------------------------------

// liDriveUnion adjudicates [label.Index.Union] against a per-label naive union
// recomputed from the model.
//
// The subsets are CONSTRUCTED to reach the four shapes that make the clause
// worth having, and the run gates on all four having been reached:
//
//   - MULTI-LABEL, so the fold across labels is actually exercised. A union of
//     one label is a Scan with extra steps.
//   - UNKNOWN LABEL, so the "a label absent from the index contributes nothing"
//     path is driven rather than assumed. Union takes the absent case through a
//     map miss, which is a different branch from the present one.
//   - DUPLICATE LABEL, because a union that double-counted would still produce
//     the right SET (roaring's Or is idempotent) but a fold that mis-handled the
//     repeat would not.
//   - THE EMPTY SUBSET, which the godoc says returns the empty bitmap; MEASURED,
//     it returns a non-nil empty bitmap, and the clause pins that rather than
//     accepting either.
func liDriveUnion(
	cfg *LabelIndexScopedConfig, seed *Seed, idx *label.Index, m *liModel,
	ev *LabelIndexScopedEvidence,
) {
	live := m.liveLabels()
	if len(live) == 0 {
		return
	}
	// An id no live label carries, so an unknown-label draw is genuinely unknown.
	unknown := uint32(0xFFFF_0000)

	shapes := []string{"single", "multi", "unknown", "duplicate", "empty"}
	if cfg.Perturb == liPerturbUnionSingleShape {
		shapes = []string{"single"}
	}
	for epoch := 0; epoch < cfg.UnionDraws; epoch++ {
		for _, shape := range shapes {
			var labels []uint32
			switch shape {
			case "single":
				labels = []uint32{live[seed.IntN(len(live))]}
			case "multi":
				n := 2 + seed.IntN(3)
				for k := 0; k < n && k < len(live); k++ {
					labels = append(labels, live[seed.IntN(len(live))])
				}
				ev.UnionMultiLabel++
			case "unknown":
				labels = []uint32{live[seed.IntN(len(live))], unknown + uint32(epoch)}
				ev.UnionUnknownLabel++
			case "duplicate":
				l := live[seed.IntN(len(live))]
				labels = []uint32{l, l, live[seed.IntN(len(live))]}
				ev.UnionDuplicate++
			default: // empty
				labels = nil
				ev.UnionEmptyDraws++
			}
			ev.UnionDraws++

			bm := idx.Union(labels...)
			if bm == nil {
				ev.UnionMismatches++
				if ev.FirstMismatch == "" {
					ev.FirstMismatch = "Union returned a nil bitmap; the godoc says the empty bitmap"
				}
				continue
			}
			got := bm.ToArray()
			if cfg.Perturb == liPerturbDropUnionMember && len(got) > 0 {
				got = got[:len(got)-1]
			}
			want := m.union(labels...)
			if len(got) != len(want) {
				ev.UnionMismatches++
				if ev.FirstMismatch == "" {
					ev.FirstMismatch = fmt.Sprintf("Union(%v) returned %d ids, the model holds %d",
						labels, len(got), len(want))
				}
				continue
			}
			for i := range got {
				if got[i] != want[i] {
					ev.UnionMismatches++
					if ev.FirstMismatch == "" {
						ev.FirstMismatch = fmt.Sprintf("Union(%v)[%d] = %d, the model holds %d",
							labels, i, got[i], want[i])
					}
					break
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The round-trip arm
// -----------------------------------------------------------------------------

// liSerialize returns the index's serialized image.
func liSerialize(idx *label.Index) ([]byte, error) {
	var b bytes.Buffer
	if err := idx.Serialize(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// liImageLabelCount reads the labelCount field out of a serialized image. It
// parses the documented layout directly rather than asking the index, so the
// entry-population clause reads the BYTES rather than a second opinion from the
// thing under test.
func liImageLabelCount(img []byte) uint32 {
	if len(img) < 12 {
		return 0
	}
	return binary.LittleEndian.Uint32(img[8:12])
}

// liRestamp returns a copy of img whose trailing CRC32C is recomputed over the
// (possibly mutated) body, so the reader's structural guards decide the outcome
// instead of the checksum.
func liRestamp(img []byte) []byte {
	out := append([]byte(nil), img...)
	if len(out) < 4 {
		return out
	}
	binary.LittleEndian.PutUint32(out[len(out)-4:], crc32.Checksum(out[:len(out)-4], liCastagnoli))
	return out
}

// liCompareAll adjudicates every label the model knows about against idx, and
// additionally probes a label the model does NOT know about, so a reader that
// invented an entry is caught as well as one that lost one.
func liCompareAll(idx *label.Index, m *liModel, seed *Seed, p liPerturb) (int, string) {
	bad, detail := 0, ""
	for _, lbl := range m.liveLabels() {
		b, d := liCompareLabel(idx, m, lbl, seed, p)
		bad += b
		if b > 0 && detail == "" {
			detail = d
		}
	}
	return bad, detail
}

// liSmallSetMaxMirror mirrors `smallSetMax` in graph/index/nodeset.go, which is
// unexported and has no accessor. It is the largest cardinality a NodeSet holds
// on the inline tier, and it is the threshold [index.NodeSetFromBitmap] uses to
// decide whether a bitmap read back off disk is down-converted.
//
// [TestLabelIndexScoped_SmallSetMaxMirrorMatchesSource] parses the constant out
// of the library source and fails if the two ever drift. A drift matters here
// more than it looks: every width in [liDriveTierIdentity] and both arms of
// [liDriveDenseSmall] are positioned RELATIVE to this number, so a silent change
// would move the treatment and the control to the same side of the threshold and
// make the pin vacuous while it still passed.
const liSmallSetMaxMirror = 8

// liDriveRoundTrip serializes the swept index, re-reads it, and adjudicates four
// separate claims.
//
// # The fixpoint claim, and why it is not the obvious one
//
// The obvious claim is that serializing, deserializing and re-serializing
// reproduces the original bytes. It is WRONG, and asserting it would have
// produced a scenario that passed on its own seed and flaked on others.
//
// [index.NodeSetFromBitmap] down-converts a bitmap whose cardinality is at most
// smallSetMax to the inline tier, and the inline tier re-materialises through
// `AddMany`, which builds an ARRAY container where `AddRange` had built a RUN
// container. So a dense label small enough to be down-converted comes back in a
// different representation and goes out in different BYTES. MEASURED: an
// AddRange-built label of 8 ids serializes to 55 bytes, and after one round trip
// to 72. The sweep can reach exactly that shape — an AddRange followed by a
// RemoveRange that trims the label to a handful of ids — so an
// `image == round-tripped image` clause would fire on whichever seed first left
// a label with 4 to 8 members on the bitmap tier.
//
// What IS true, and is asserted, is that the form is a FIXPOINT after at most
// one cycle: whatever re-tiering the first read performs, the second image and
// the third must be identical. MEASURED across every width probed, including the
// unstable band. Whether the FIRST cycle was stable is recorded as a number
// instead, because it is a property of the fixture rather than of the code.
//
// The other three claims:
//
//   - CONTENT, at both round trips, against the MODEL rather than against the
//     index the image came from, so the round trip is checked by something that
//     did not produce it.
//   - EMISSION DETERMINISM: serializing one unchanged index twice must produce
//     identical bytes. The ascending-labelID sort exists for this and nothing
//     else asserted it.
//   - The one-sided ENTRY-POPULATION floor, and the tier and dense-small arms
//     below.
func liDriveRoundTrip(
	cfg *LabelIndexScopedConfig, seed *Seed, idx *label.Index, m *liModel,
	ev *LabelIndexScopedEvidence,
) error {
	img, err := liSerialize(idx)
	if err != nil {
		return fmt.Errorf("serialize the swept index: %w", err)
	}
	ev.ImageBytes = len(img)
	ev.ImageLabelCount = liImageLabelCount(img)
	ev.ModelLiveLabels = len(m.liveLabels())
	if excess := int(ev.ImageLabelCount) - ev.ModelLiveLabels; excess > 0 {
		ev.PhantomExcess = excess
	}
	if cfg.Perturb == liPerturbEntryFloor && ev.ImageLabelCount > 0 {
		ev.ImageLabelCount--
	}

	again, err := liSerialize(idx)
	if err != nil {
		return fmt.Errorf("re-serialize the swept index: %w", err)
	}
	if cfg.Perturb == liPerturbUnstableSerialize && len(again) > 12 {
		again = append([]byte(nil), again...)
		again[12] ^= 0xFF
	}
	ev.SerializeReruns++
	if !bytes.Equal(img, again) {
		ev.SerializeUnstable++
	}

	// Cycle one.
	second := label.NewIndex()
	if derr := second.Deserialize(bytes.NewReader(img)); derr != nil {
		return fmt.Errorf("deserialize the clean image: %w", derr)
	}
	ev.RoundTrips++
	if cfg.Perturb == liPerturbRoundTripContent {
		if live := m.liveLabels(); len(live) > 0 {
			for _, id := range second.Scan(live[0]) {
				second.Remove(live[0], id)
			}
		}
	}
	bad, detail := liCompareAll(second, m, seed, liPerturbNone)
	ev.Compares++
	if bad > 0 {
		ev.RTContentMismatch += bad
		if ev.FirstMismatch == "" {
			ev.FirstMismatch = detail
		}
	}
	img2, err := liSerialize(second)
	if err != nil {
		return fmt.Errorf("re-serialize after the first cycle: %w", err)
	}
	ev.ImageBytes2 = len(img2)
	ev.FirstCycleStable = bytes.Equal(img, img2)

	// Cycle two: the form must now be a fixpoint.
	third := label.NewIndex()
	if derr := third.Deserialize(bytes.NewReader(img2)); derr != nil {
		return fmt.Errorf("deserialize the second image: %w", derr)
	}
	ev.RoundTrips++
	bad, detail = liCompareAll(third, m, seed, liPerturbNone)
	ev.Compares++
	if bad > 0 {
		ev.RTContentMismatch += bad
		if ev.FirstMismatch == "" {
			ev.FirstMismatch = detail
		}
	}
	img3, err := liSerialize(third)
	if err != nil {
		return fmt.Errorf("re-serialize after the second cycle: %w", err)
	}
	if cfg.Perturb == liPerturbRoundTripBytes && len(img3) > 12 {
		img3 = append([]byte(nil), img3...)
		img3[12] ^= 0xFF
	}
	if !bytes.Equal(img2, img3) {
		ev.RTByteMismatch++
	}

	if err := liDriveTierIdentity(cfg, ev); err != nil {
		return err
	}
	liDriveRangeTier(cfg, ev)
	return liDriveDenseSmall(cfg, ev)
}

// liTierWidths are the cardinalities the documented byte-identity claim is
// checked at. Every one is at or below smallSetMax, which is the point: the
// claim is about the INLINE tier producing the bytes a bitmap holding the same
// ids produces, so a width above the threshold would compare two bitmaps and
// assert nothing.
func liTierWidths() []uint64 { return []uint64{1, 2, 3, 4, 5, 8} }

// liTierGrowTo is the cardinality the bitmap side is grown to before being
// trimmed back. It must exceed smallSetMax (so the set really promotes) and
// every width in [liTierWidths] (so the trim really runs).
const liTierGrowTo = liSmallSetMaxMirror + 8

// liDriveTierIdentity checks the claim `graph/index/label/index.go:407-415`
// actually makes: a set held on the INLINE small-set tier serializes to exactly
// the bytes a `*roaring64.Bitmap` holding the same ids produces, which is what
// made the #1585 tiering refactor a zero-migration change.
//
// The bitmap side is reached by GROWTH — individual Adds past smallSetMax, then
// Removes back down to the target cardinality, exploiting the documented
// one-way promotion so the set stays a bitmap while holding only a handful of
// ids. That is the construction the claim is about. An earlier draft of this arm
// built the bitmap side with `AddRange` instead and FAILED, because AddRange
// produces a run container rather than an array one; that was an over-strong
// assertion of this harness's own, not a defect, and what it actually measures
// now lives in [liDriveRangeTier].
func liDriveTierIdentity(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) error {
	const base = uint64(4096)
	for _, w := range liTierWidths() {
		inline := label.NewIndex()
		for k := uint64(0); k < w; k++ {
			inline.Add(1, graph.NodeID(base+k))
		}
		grown := label.NewIndex()
		for k := uint64(0); k < liTierGrowTo; k++ {
			grown.Add(1, graph.NodeID(base+k))
		}
		for k := w; k < liTierGrowTo; k++ {
			grown.Remove(1, graph.NodeID(base+k))
		}
		if grown.Count(1) != w || inline.Count(1) != w {
			return fmt.Errorf("sim: label-index-scoped: the tier-identity fixture at width %d holds "+
				"%d (inline) and %d (grown) ids; both sides must hold exactly %d or the comparison "+
				"is between two different sets", w, inline.Count(1), grown.Count(1), w)
		}

		a, err := liSerialize(inline)
		if err != nil {
			return fmt.Errorf("serialize the inline-tier index at width %d: %w", w, err)
		}
		b, err := liSerialize(grown)
		if err != nil {
			return fmt.Errorf("serialize the bitmap-tier index at width %d: %w", w, err)
		}
		if cfg.Perturb == liPerturbTierDivergence && w == liTierWidths()[0] && len(b) > 12 {
			b = append([]byte(nil), b...)
			b[12] ^= 0xFF
		}
		ev.TierChecks++
		if !bytes.Equal(a, b) {
			ev.TierMismatch++
			if ev.FirstMismatch == "" {
				ev.FirstMismatch = fmt.Sprintf("at width %d the inline-tier image is %d bytes and "+
					"the bitmap-tier image holding the same ids is %d", w, len(a), len(b))
			}
		}
	}
	return nil
}

// liRangeTierWidths brackets the width at which an AddRange-built label stops
// serializing like an Add-built one. 1..3 are below the crossover and 4..8 above
// it, so the measurement contains both answers and the gate can require that.
func liRangeTierWidths() []uint64 { return []uint64{1, 2, 3, 4, 6, 8} }

// liDriveRangeTier MEASURES, and does not assert, how an AddRange-built label's
// image compares with an Add-built one holding the identical ids.
//
// They diverge from width 4 upwards: roaring encodes a four-or-longer contiguous
// run as a RUN container, where `AddMany` of the same ids builds an ARRAY
// container. MEASURED on the reference host: identical at widths 1, 2 and 3
// (58, 60 and 62 bytes), and 64/66/68/70/72 bytes against a flat 55 from width 4
// to width 8 — flat because a run container costs the same whatever the run's
// length.
//
// This is NOT a defect and there is no clause on it. `Serialize`'s godoc
// promises that the on-disk form is deterministic "for a given in-memory state",
// which is exactly what holds; it never promised the bytes were a function of
// the logical contents alone. The measurement is recorded because the
// consequence is easy to assume away and is real: two indexes that answer every
// query identically can have different images, so byte-comparing two snapshots
// is not a valid way to ask whether two graphs carry the same labels.
func liDriveRangeTier(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) {
	const base = uint64(8192)
	for _, w := range liRangeTierWidths() {
		byAdd := label.NewIndex()
		for k := uint64(0); k < w; k++ {
			byAdd.Add(1, graph.NodeID(base+k))
		}
		byRange := label.NewIndex()
		byRange.AddRange(1, graph.NodeID(base), graph.NodeID(base+w-1))
		a, aerr := liSerialize(byAdd)
		b, berr := liSerialize(byRange)
		if aerr != nil || berr != nil {
			continue
		}
		equal := bytes.Equal(a, b)
		if cfg.Perturb == liPerturbRangeTierFlat {
			equal = true
		}
		ev.RangeTier = append(ev.RangeTier, liTierRow{
			Width: int(w), AddBytes: len(a), RangeBytes: len(b), Equal: equal,
		})
	}
}

// liDriveDenseSmall pins the round-trip NON-IDEMPOTENCE this scenario found.
//
// # What it measures
//
// A label built by `AddRange` sits on the bitmap tier holding a run container.
// If its cardinality is at most smallSetMax, [index.NodeSetFromBitmap]
// down-converts it to the inline tier when the image is read back, and the
// inline tier re-materialises through `AddMany` as an array container. So the
// image the reader produces on the way OUT is not the image it was handed on the
// way IN. MEASURED: 55 bytes in, 72 bytes out at a cardinality of 8; the third
// image equals the second, so the form converges after exactly one cycle.
//
// The CONTROL is the same construction one id above the threshold. It stays on
// the bitmap tier, keeps its run container, and MEASURED comes back at 55 bytes
// unchanged. Without it the instability would be consistent with round trips
// changing bytes in general, and would not be attributable to the down-convert.
//
// # It is a defect, it is latent, and it is reported rather than fixed
//
// Content is never lost — every membership query agrees before and after — but a
// checkpoint, reload and re-checkpoint cycle produces different bytes for the
// same logical state, which is exactly what a fixture diff, a content-addressed
// store, or an incremental backup's deduplication relies on not happening.
//
// It is unreachable in production today: `AddRange` has no production caller, so
// no production label is ever a run container, and every Add-built label is
// MEASURED stable from the first cycle at every width. The fix — teaching
// `NodeSetFromBitmap` not to down-convert a run-encoded bitmap, or teaching the
// inline tier to re-materialise the container it came from — is a design choice
// for whoever owns the type.
//
// So this arm asserts the MEASURED behaviour, and the clause says so. It fires
// the day the behaviour changes, which is when this note and the fixpoint
// formulation above need revisiting together.
func liDriveDenseSmall(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) error {
	const base = uint64(16384)
	run := func(w int) (first, second, third int, stable bool, err error) {
		idx := label.NewIndex()
		idx.AddRange(1, graph.NodeID(base), graph.NodeID(base+uint64(w)-1))
		i1, err := liSerialize(idx)
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("serialize the dense-small fixture at width %d: %w", w, err)
		}
		a := label.NewIndex()
		if derr := a.Deserialize(bytes.NewReader(i1)); derr != nil {
			return 0, 0, 0, false, fmt.Errorf("first cycle at width %d: %w", w, derr)
		}
		i2, err := liSerialize(a)
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("re-serialize the first cycle at width %d: %w", w, err)
		}
		b := label.NewIndex()
		if derr := b.Deserialize(bytes.NewReader(i2)); derr != nil {
			return 0, 0, 0, false, fmt.Errorf("second cycle at width %d: %w", w, derr)
		}
		i3, err := liSerialize(b)
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("re-serialize the second cycle at width %d: %w", w, err)
		}
		// The content must survive whatever the bytes do, or this is a data-loss
		// finding rather than an encoding one.
		if a.Count(1) != uint64(w) || b.Count(1) != uint64(w) {
			return 0, 0, 0, false, fmt.Errorf("the dense-small fixture at width %d lost ids across "+
				"the round trips (%d then %d)", w, a.Count(1), b.Count(1))
		}
		return len(i1), len(i2), len(i3), bytes.Equal(i1, i2), nil
	}

	d := &ev.DenseSmall
	d.Width = liSmallSetMaxMirror
	d.CtrlWidth = liSmallSetMaxMirror + 1
	var err error
	if d.First, d.Second, d.Third, d.Stable, err = run(d.Width); err != nil {
		return err
	}
	var ctrlThird int
	if d.CtrlFirst, d.CtrlSecond, ctrlThird, d.CtrlStable, err = run(d.CtrlWidth); err != nil {
		return err
	}
	if ctrlThird != d.CtrlSecond {
		return fmt.Errorf("sim: label-index-scoped: the dense-small CONTROL at width %d is not a "+
			"fixpoint either (%d then %d bytes); the arm cannot attribute anything",
			d.CtrlWidth, d.CtrlSecond, ctrlThird)
	}
	if cfg.Perturb == liPerturbDenseSmallStable {
		d.Stable = true
	}
	if cfg.Perturb == liPerturbDenseSmallCtrl {
		d.CtrlStable = false
	}
	return nil
}

// -----------------------------------------------------------------------------
// The corruption arm
// -----------------------------------------------------------------------------

// The guard names [liClassifyGuard] can return. They are the distinct answers
// `Deserialize` gives, one per branch that can refuse.
const (
	liGuardAccepted = "accepted"
	liGuardCRC      = "crc32c"
	liGuardShort    = "short-payload"
	liGuardMagic    = "bad-magic"
	liGuardVersion  = "bad-version"
	liGuardBMLen    = "implausible-bitmap-length"
	liGuardBMParse  = "bitmap-parse"
	liGuardTrailing = "truncated-entry"
	liGuardUnknown  = "unclassified"
)

// liGuardNeedles maps each guard to the WHOLE distinctive phrase its message
// carries. They are full phrases rather than single words on purpose: "bitmap"
// alone appears in both the length guard and the parse guard, and a needle that
// matched either would let one guard's answer be credited to the other. A
// message that matches none, or more than one, is reported as
// [liGuardUnknown] rather than binned into the nearest fit.
func liGuardNeedles() map[string]string {
	return map[string]string{
		liGuardCRC:      "crc32c mismatch",
		liGuardShort:    "short payload",
		liGuardMagic:    "bad magic",
		liGuardVersion:  "unsupported format version",
		liGuardBMLen:    "implausible bitmap length",
		liGuardBMParse:  "bitmap parse",
		liGuardTrailing: "labelID: ",
	}
}

// liClassifyGuard names which branch of Deserialize answered, from the error's
// own wording. A nil error is [liGuardAccepted].
func liClassifyGuard(err error) string {
	if err == nil {
		return liGuardAccepted
	}
	msg := err.Error()
	hit := ""
	for guard, needle := range liGuardNeedles() {
		if strings.Contains(msg, needle) {
			if hit != "" {
				return liGuardUnknown
			}
			hit = guard
		}
	}
	if hit == "" {
		return liGuardUnknown
	}
	return hit
}

// liTargetLabel / liTargetNodes are the receiver the corruption arm deserializes
// INTO. It is populated before every trial so "the receiver was restored to its
// pre-call state" is a claim about state that existed; a fresh empty index would
// satisfy it for the wrong reason, and the `corrupt-populated` gate exists to
// keep that from happening silently.
const liTargetLabel uint32 = 0xD00D
const liTargetNodes = 5

// liFreshTarget returns a populated receiver and its serialized image, so a
// refused Deserialize can be checked to have changed nothing.
func liFreshTarget(cfg *LabelIndexScopedConfig) (*label.Index, []byte, bool, error) {
	t := label.NewIndex()
	populated := cfg.Perturb != liPerturbEmptyTarget
	if populated {
		for k := 0; k < liTargetNodes; k++ {
			t.Add(liTargetLabel, graph.NodeID(9000+k))
		}
		t.AddRange(liTargetLabel+1, 200, 260)
	}
	img, err := liSerialize(t)
	if err != nil {
		return nil, nil, false, err
	}
	return t, img, populated, nil
}

// liRegionOffsets returns the named byte offsets of the serialized layout,
// derived from the image itself rather than hard-coded, so a format change
// relocates the probes instead of silently aiming them at the wrong field.
func liRegionOffsets(img []byte) map[string]int64 {
	out := map[string]int64{
		"magic":   0,
		"version": 4,
		"count":   8,
		"trailer": int64(len(img) - 4),
	}
	if len(img) >= 24 {
		out["labelID"] = 12
		out["bitmapLen"] = 16
		bmLen := binary.LittleEndian.Uint64(img[16:24])
		if bmLen > 0 && 24+int64(bmLen) <= int64(len(img)-4) {
			out["bitmapPayload"] = 24 + int64(bmLen/2)
		}
	}
	return out
}

// liRegionOrder is the fixed order the raw family walks its regions in, so the
// evidence and the digest do not depend on map iteration.
func liRegionOrder() []string {
	return []string{"magic", "version", "count", "labelID", "bitmapLen", "bitmapPayload", "trailer"}
}

// liDriveCorruption writes the swept image to a [SimDisk] and drives three
// damage families against it.
//
// # Why the families are separate
//
// The format ends in a CRC32C over every preceding byte, so a raw flip is caught
// by the checksum WHEREVER it lands — which means the CRC is the only reachable
// detector for a bad sector, and the four structural guards inside Deserialize
// cannot be reached by corruption at all. The raw family therefore asserts only
// that the refusal happened, wrapped the sentinel, and left the receiver alone;
// it deliberately does NOT name a guard, because "which byte moved" cannot
// change the answer.
//
// The re-stamped family recomputes the trailer after the edit, so the reader's
// own guards decide, and each trial names the guard it must reach. Two of its
// trials are not refusals at all and say so: a re-stamped labelCount BELOW the
// true one is ACCEPTED, and the body's remaining bytes are silently ignored.
// That is not a defect against CRC32C's contract — an error-detecting code is
// not a MAC — but it is the measurement that shows the format carries no
// redundancy tying labelCount to the payload length.
//
// The truncate family covers the short read, which reaches a different branch
// again: below four bytes there is no trailer to compare and the reader answers
// "short payload" before any CRC arithmetic.
func liDriveCorruption(
	cfg *LabelIndexScopedConfig, seed *Seed, img []byte, m *liModel,
	ev *LabelIndexScopedEvidence,
) error {
	disk := NewSimDisk(NewSeed(cfg.Seed^labelIndexScopedSeedMix), 0) // faultRate 0: the damage here is deliberate
	h, err := disk.OpenFile(liDiskPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("open the image on the SimDisk: %w", err)
	}
	if _, werr := h.Write(img); werr != nil {
		_ = h.Close()
		return fmt.Errorf("write the image to the SimDisk: %w", werr)
	}
	if serr := h.Sync(); serr != nil {
		_ = h.Close()
		return fmt.Errorf("sync the image: %w", serr)
	}
	if cerr := h.Close(); cerr != nil {
		return fmt.Errorf("close the image: %w", cerr)
	}

	// The CLEAN CONTROL first. Without it every refusal below would be consistent
	// with a reader that refuses everything, and the arm would prove nothing.
	if err := liCorruptTrial(cfg, seed, ev, liCorruptTrialSpec{
		Family: liFamilyRaw, Region: "clean-control", WantGuard: liGuardAccepted,
		Payload: liControlPayload(cfg, disk, img),
	}, m); err != nil {
		return err
	}

	if err := liDriveRawFamily(cfg, seed, disk, img, ev, m); err != nil {
		return err
	}
	if err := liDriveRestampFamily(cfg, seed, img, ev, m); err != nil {
		return err
	}
	return liDriveTruncateFamily(cfg, seed, img, ev, m)
}

// liControlPayload returns the image read back off the disk, damaged only when
// the run is perturbed to break the control on purpose.
func liControlPayload(cfg *LabelIndexScopedConfig, disk *SimDisk, img []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		got, err := disk.ReadFile(liDiskPath)
		if err != nil {
			return nil, fmt.Errorf("read the image back: %w", err)
		}
		if !bytes.Equal(got, img) {
			return nil, fmt.Errorf("the image read back off the SimDisk differs from the one written "+
				"(%d bytes vs %d) before any damage was injected", len(got), len(img))
		}
		if cfg.Perturb == liPerturbBreakCleanControl && len(got) > 12 {
			got = append([]byte(nil), got...)
			got[12] ^= 0xFF
		}
		return got, nil
	}
}

// liCorruptTrialSpec describes one damaged-image trial.
type liCorruptTrialSpec struct {
	Payload   func() ([]byte, error)
	Region    string
	WantGuard string
	Family    liCorruptFamily
	Off       int64
	Len       int
}

// liCorruptTrial runs one trial: build the payload, deserialize it into a
// freshly populated receiver, and record what happened.
//
// When the trial expects acceptance the receiver's CONTENT is adjudicated
// against the model, so an accepted image is checked for being right and not
// merely for being accepted.
func liCorruptTrial(
	cfg *LabelIndexScopedConfig, seed *Seed, ev *LabelIndexScopedEvidence,
	spec liCorruptTrialSpec, m *liModel,
) error {
	payload, err := spec.Payload()
	if err != nil {
		return err
	}
	target, before, populated, err := liFreshTarget(cfg)
	if err != nil {
		return fmt.Errorf("build the receiver for %s/%s: %w", spec.Family, spec.Region, err)
	}

	derr := target.Deserialize(bytes.NewReader(payload))
	after, serr := liSerialize(target)
	if serr != nil {
		return fmt.Errorf("re-serialize the receiver after %s/%s: %w", spec.Family, spec.Region, serr)
	}
	restored := bytes.Equal(before, after)
	if cfg.Perturb == liPerturbHideRestore && derr != nil {
		restored = false
	}
	guard := liClassifyGuard(derr)
	want := spec.WantGuard
	if cfg.Perturb == liPerturbWrongGuard && spec.Family == liFamilyRestamp && want == liGuardMagic {
		want = liGuardVersion
	}

	row := liCorruptEvidence{
		Family: spec.Family, Region: spec.Region, Off: spec.Off, Len: spec.Len,
		WantGuard: want, Guard: guard,
		Refused: derr != nil, IsCorrupted: errors.Is(derr, index.ErrIndexCorrupted),
		Restored: restored, TargetWasPopulated: populated,
	}
	ev.Corrupt = append(ev.Corrupt, row)

	// An accepted image must also be RIGHT. This is what stops the clean control
	// from passing on a reader that accepted the bytes and produced nothing.
	if derr == nil && spec.Region == "clean-control" {
		bad, detail := liCompareAll(target, m, seed, liPerturbNone)
		ev.Compares++
		if bad > 0 {
			ev.RTContentMismatch += bad
			if ev.FirstMismatch == "" {
				ev.FirstMismatch = detail
			}
		}
	}
	return nil
}

// liDriveRawFamily flips one byte of each named region through
// [SimDisk.CorruptRange] and requires a refusal every time. The flip is undone
// after each trial so the regions are independent.
//
// What the resulting clause detects is the CRC CHECK, not a CRC collision: a
// single-byte error cannot survive CRC32C, so a refusal here is guaranteed while
// the check is performed before the parse, and the clause fires only if that
// stops being true. See [liCheckCorruption] for why that is still worth having.
//
// Each trial also confirms the flip actually reached the disk, because
// [SimDisk.CorruptRange] operating on the wrong path or the wrong offset would
// otherwise turn every trial into a silent no-op that the refusal clause would
// then fail on for the wrong reason.
func liDriveRawFamily(
	cfg *LabelIndexScopedConfig, seed *Seed, disk *SimDisk, img []byte,
	ev *LabelIndexScopedEvidence, m *liModel,
) error {
	offsets := liRegionOffsets(img)
	regions := liRegionOrder()
	if cfg.Perturb == liPerturbSkipRegions {
		regions = regions[:1]
	}
	for _, region := range regions {
		off, ok := offsets[region]
		if !ok {
			continue
		}
		spec := liCorruptTrialSpec{
			Family: liFamilyRaw, Region: region, Off: off, Len: 1,
			// No WantGuard: the CRC covers the whole body, so every raw flip lands on
			// the same detector and naming one would assert nothing extra.
			Payload: func() ([]byte, error) {
				if cfg.Perturb != liPerturbSkipDamage {
					if err := disk.CorruptRange(liDiskPath, off, 1); err != nil {
						return nil, fmt.Errorf("corrupt %s at %d: %w", region, off, err)
					}
				}
				got, err := disk.ReadFile(liDiskPath)
				if err != nil {
					return nil, fmt.Errorf("read back after corrupting %s: %w", region, err)
				}
				out := append([]byte(nil), got...)
				if cfg.Perturb != liPerturbSkipDamage {
					// XOR 0xFF is its own inverse, so this restores the image for the
					// next region.
					if err := disk.CorruptRange(liDiskPath, off, 1); err != nil {
						return nil, fmt.Errorf("restore %s at %d: %w", region, off, err)
					}
					if bytes.Equal(out, img) {
						return nil, fmt.Errorf("flipping byte %d of region %s changed nothing on disk",
							off, region)
					}
				}
				return out, nil
			},
		}
		if err := liCorruptTrial(cfg, seed, ev, spec, m); err != nil {
			return err
		}
	}
	return nil
}

// liDriveRestampFamily damages a structural field and recomputes the trailer, so
// the reader's own guards decide rather than the checksum. Each trial names the
// guard it must reach.
func liDriveRestampFamily(
	cfg *LabelIndexScopedConfig, seed *Seed, img []byte,
	ev *LabelIndexScopedEvidence, m *liModel,
) error {
	mutate := func(f func([]byte)) func() ([]byte, error) {
		return func() ([]byte, error) {
			out := append([]byte(nil), img...)
			if cfg.Perturb != liPerturbSkipDamage {
				f(out)
			}
			return liRestamp(out), nil
		}
	}
	trials := []liCorruptTrialSpec{{
		Family: liFamilyRestamp, Region: "restamp-magic", Off: 0, Len: 4,
		WantGuard: liGuardMagic,
		Payload:   mutate(func(p []byte) { p[0] ^= 0xFF }),
	}, {
		Family: liFamilyRestamp, Region: "restamp-version", Off: 4, Len: 4,
		WantGuard: liGuardVersion,
		Payload:   mutate(func(p []byte) { binary.LittleEndian.PutUint32(p[4:8], 99) }),
	}, {
		Family: liFamilyRestamp, Region: "restamp-count-high", Off: 8, Len: 4,
		// A labelCount ABOVE the truth runs the reader off the end of the body, so
		// it fails reading the next labelID rather than at a length guard.
		WantGuard: liGuardTrailing,
		Payload: mutate(func(p []byte) {
			binary.LittleEndian.PutUint32(p[8:12], liImageLabelCount(img)+64)
		}),
	}, {
		Family: liFamilyRestamp, Region: "restamp-count-zero", Off: 8, Len: 4,
		// MEASURED and deliberate: a labelCount of 0 over a body full of entries is
		// ACCEPTED, and the reader yields an EMPTY index. The trailing bytes are
		// never examined. This trial exists to record that, not to condemn it: the
		// trailer is a CRC32C, an error-detecting code, and an adversary who can
		// recompute it is outside its threat model. What the measurement does show
		// is that the format carries no redundancy tying labelCount to the payload
		// length, so a structural "the body was fully consumed" check — which would
		// cost nothing — is absent.
		WantGuard: liGuardAccepted,
		Payload:   mutate(func(p []byte) { binary.LittleEndian.PutUint32(p[8:12], 0) }),
	}, {
		Family: liFamilyRestamp, Region: "restamp-bitmap-len", Off: 16, Len: 8,
		WantGuard: liGuardBMLen,
		Payload: mutate(func(p []byte) {
			if len(p) >= 24 {
				binary.LittleEndian.PutUint64(p[16:24], 1<<40)
			}
		}),
	}, {
		Family: liFamilyRestamp, Region: "restamp-bitmap-payload", Off: 24, Len: 1,
		WantGuard: liGuardBMParse,
		Payload: mutate(func(p []byte) {
			if len(p) > 26 {
				p[26] ^= 0xFF
			}
		}),
	}}
	if cfg.Perturb == liPerturbSkipRegions {
		trials = trials[:1]
	}
	for _, spec := range trials {
		if err := liCorruptTrial(cfg, seed, ev, spec, m); err != nil {
			return err
		}
	}
	return nil
}

// liTruncateLengths returns the image lengths the truncate family reads at,
// derived from the image so they straddle the four-byte floor below which there
// is no trailer to compare.
func liTruncateLengths(img []byte) []int {
	out := []int{0, 3, 4, 11}
	if len(img) > 24 {
		out = append(out, 24, len(img)-1)
	}
	return out
}

// liDriveTruncateFamily reads the image short. Below four bytes the reader
// answers "short payload" before any CRC arithmetic; at or above it the
// truncated body simply fails its checksum. Both must refuse.
func liDriveTruncateFamily(
	cfg *LabelIndexScopedConfig, seed *Seed, img []byte,
	ev *LabelIndexScopedEvidence, m *liModel,
) error {
	lengths := liTruncateLengths(img)
	if cfg.Perturb == liPerturbSkipRegions {
		lengths = lengths[:1]
	}
	for _, n := range lengths {
		want := liGuardCRC
		if n < 4 {
			want = liGuardShort
		}
		spec := liCorruptTrialSpec{
			Family: liFamilyTruncate, Region: fmt.Sprintf("truncate-%d", n),
			Off: int64(n), Len: len(img) - n, WantGuard: want,
			Payload: func() ([]byte, error) {
				if cfg.Perturb == liPerturbSkipDamage {
					return append([]byte(nil), img...), nil
				}
				return append([]byte(nil), img[:n]...), nil
			},
		}
		if err := liCorruptTrial(cfg, seed, ev, spec, m); err != nil {
			return err
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// The scope arm
// -----------------------------------------------------------------------------

// liScopeOpName renders a change op for the evidence.
func liScopeOpName(op index.ChangeOp) string {
	switch op {
	case index.OpAddNodeLabel:
		return "OpAddNodeLabel"
	case index.OpRemoveNodeLabel:
		return "OpRemoveNodeLabel"
	case index.OpAddEdgeLabel:
		return "OpAddEdgeLabel"
	case index.OpRemoveEdgeLabel:
		return "OpRemoveEdgeLabel"
	default:
		return fmt.Sprintf("op(%d)", uint8(op))
	}
}

// liScopeOps returns the four label change kinds, in a fixed order.
func liScopeOps() []index.ChangeOp {
	return []index.ChangeOp{
		index.OpAddNodeLabel, index.OpRemoveNodeLabel,
		index.OpAddEdgeLabel, index.OpRemoveEdgeLabel,
	}
}

// liWantAccepted is the NAIVE routing table: a node-scoped index consumes the
// two node kinds and a edge-scoped index the two edge kinds. It is written out
// as a table rather than derived from the index, so the clause compares two
// independent statements of the rule.
func liWantAccepted(scope label.Scope, op index.ChangeOp) bool {
	switch op {
	case index.OpAddNodeLabel, index.OpRemoveNodeLabel:
		return scope == label.ScopeNode
	case index.OpAddEdgeLabel, index.OpRemoveEdgeLabel:
		return scope == label.ScopeEdge
	default:
		return false
	}
}

// liDriveScope drives every (constructor, change kind) pair through
// [label.Index.Apply] and adjudicates the routing against [liWantAccepted].
//
// This is the ONLY place `Scope` is observable, and the arm exists because of
// what that implies rather than in spite of it: `Scope` is read only inside
// `Apply`, and `Apply` runs only through the [index.Manager] fan-out, and no
// production `label.Index` is ever registered with a manager (see the file
// header for the verified call-site census). So the three constructors differ in
// exactly one respect, this arm measures exactly that respect, and the
// difference is unreachable on any production path today.
//
// `NewIndex` is driven alongside the other two so the row that matters most is
// present as a MEASUREMENT and not as a remark: lpg builds its EDGE index with
// `NewIndex()`, which is `ScopeNode`, so were that index ever registered it
// would DROP every `OpAddEdgeLabel` — and `OpAddEdgeLabel` is a change kind
// production really does construct and really does deliver
// (`cypher/api.go:17890` and `:18804`, through
// `cypher/exec/index_writeback.go:45`).
func liDriveScope(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) {
	ctors := []struct {
		name string
		make func() *label.Index
	}{
		{"NewIndex", label.NewIndex},
		{"NewNodeIndex", label.NewNodeIndex},
		{"NewEdgeIndex", label.NewEdgeIndex},
	}
	const lbl uint32 = 4242
	const node = graph.NodeID(31337)

	for _, c := range ctors {
		// `scope` is what the constructor REPORTED and is what the
		// `scope-constructor` clause adjudicates; WantAccepted below is derived
		// from a freshly read scope instead, so a perturbation of the reported
		// value moves one clause and not both.
		scope := c.make().Scope()
		if cfg.Perturb == liPerturbScopeSwap && c.name == "NewEdgeIndex" {
			scope = label.ScopeNode
		}
		for _, op := range liScopeOps() {
			idx := c.make()
			// A removal needs something to remove, so the receiver is pre-loaded
			// through Add — which bypasses Apply and therefore cannot itself be
			// filtered by the scope under test.
			isRemove := op == index.OpRemoveNodeLabel || op == index.OpRemoveEdgeLabel
			if isRemove {
				idx.Add(lbl, node)
			}
			before := idx.Count(lbl)
			idx.Apply(index.Change{Op: op, Label: lbl, Node: node})
			after := idx.Count(lbl)

			accepted := after != before
			if cfg.Perturb == liPerturbScopeRouting && c.name == "NewNodeIndex" &&
				op == index.OpAddEdgeLabel {
				accepted = !accepted
			}
			ev.Scopes = append(ev.Scopes, liScopeEvidence{
				Ctor: c.name, Op: liScopeOpName(op), Scope: uint8(scope),
				Accepted: accepted, WantAccepted: liWantAccepted(c.make().Scope(), op),
			})
		}
	}
}

// liExpectedScope is the scope each constructor is documented to produce.
// `NewIndex` is `ScopeNode` by its own godoc ("equivalent to NewNodeIndex").
func liExpectedScope(ctor string) uint8 {
	if ctor == "NewEdgeIndex" {
		return uint8(label.ScopeEdge)
	}
	return uint8(label.ScopeNode)
}

// -----------------------------------------------------------------------------
// The boundary arm — a DEFECT pinned to its measured behaviour
// -----------------------------------------------------------------------------

// liBoundarySpan is how far below math.MaxUint64 the boundary fixture starts.
const liBoundarySpan = 5

// liDriveBoundary measures what the range methods do when the CLOSED interval
// ends at math.MaxUint64.
//
// # This clause was a defect pin and is now a REGRESSION arm (#2607)
//
// [index.NodeSet.AddRange] used to convert the inclusive upper endpoint to
// roaring's exclusive one with a plain `to+1`, and [index.NodeSet.RemoveRange]
// did the same in its BITMAP branch. At `to == math.MaxUint64` that wraps to 0,
// and roaring returns immediately on `start >= end` — so the whole range was
// silently dropped, not merely its last id. MEASURED at the time:
// `AddRange(max-5, max)` yielded cardinality 0 where the closed interval says 6,
// and `RemoveRange(max-3, max)` over a five-element BITMAP-tier set removed
// nothing at all.
//
// The tier mattered, and only for `RemoveRange`. Its singleton and small
// branches filter on the closed interval directly (`v < from || v > to`) with no
// `+1` and therefore no wrap, so the identical call over an INLINE-tier set left
// the correct answer. The same logical operation on the same membership answered
// differently depending on which tier the set happened to occupy — state the
// public surface does not expose. `AddRange` had no such split: it promotes to
// the bitmap tier BEFORE looking at the interval, so it was uniformly wrong.
//
// `graph/index/nodeset.go` now routes both directions through `addRangeClosed` /
// `removeRangeClosed`, which split the call at the top of the range rather than
// relying on a bound that the uint64 element type cannot name. This arm asserts
// the repaired contract: the closed interval is honoured at the boundary, and
// BOTH tiers answer identically. It drives the RemoveRange experiment twice over
// identical membership — once on a bitmap-tier label, once on an inline-tier one
// — because tier agreement is the half that a single-tier arm cannot see.
//
// The arm keeps its CONTROL one id lower, where the conversion was always exact.
// Without it, "the range was handled" would be consistent with the whole top of
// the id space behaving in some other uniform way, and a future regression at
// the final id would not be attributable to the final id.
func liDriveBoundary(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) {
	const maxU64 = uint64(math.MaxUint64)
	const lbl uint32 = 0xB0
	b := &ev.Boundary

	// The treatment: a closed interval whose upper endpoint IS math.MaxUint64.
	add := label.NewIndex()
	add.AddRange(lbl, graph.NodeID(maxU64-liBoundarySpan), graph.NodeID(maxU64))
	b.AddNaive = liBoundarySpan + 1
	b.AddGot = add.Count(lbl)
	if cfg.Perturb == liPerturbBoundaryWraps {
		b.AddGot = 0
	}

	// The control: the same interval one id shorter, which must be exact.
	ctrl := label.NewIndex()
	ctrl.AddRange(lbl, graph.NodeID(maxU64-liBoundarySpan), graph.NodeID(maxU64-1))
	b.AddBelowNaive = liBoundarySpan
	b.AddBelowGot = ctrl.Count(lbl)
	if cfg.Perturb == liPerturbBoundaryControlBad && b.AddBelowGot > 0 {
		b.AddBelowGot--
	}

	// The same experiment for RemoveRange, over a BITMAP-tier set the control
	// path built. A closed removal of [max-3, max] from {max-5 .. max-1} leaves
	// the two ids below it.
	b.RemoveNaive = 2
	rem := label.NewIndex()
	rem.AddRange(lbl, graph.NodeID(maxU64-liBoundarySpan), graph.NodeID(maxU64-1))
	b.RemoveBefore = rem.Count(lbl)
	rem.RemoveRange(lbl, graph.NodeID(maxU64-3), graph.NodeID(maxU64))
	b.RemoveGot = rem.Count(lbl)
	if cfg.Perturb == liPerturbBoundaryWraps {
		b.RemoveGot = b.RemoveBefore
	}

	// And again over an INLINE-tier set holding the identical membership, built
	// with Add so it never crosses the promotion threshold. This is the tier the
	// defect answered correctly, so it is the arm's cross-check rather than its
	// treatment: the two must agree whatever the answer is.
	inl := label.NewIndex()
	for id := maxU64 - liBoundarySpan; id <= maxU64-1; id++ {
		inl.Add(lbl, graph.NodeID(id))
	}
	b.RemoveInlineBefore = inl.Count(lbl)
	inl.RemoveRange(lbl, graph.NodeID(maxU64-3), graph.NodeID(maxU64))
	b.RemoveInlineGot = inl.Count(lbl)
}

// -----------------------------------------------------------------------------
// The phantom arm — a second DEFECT pinned to its measured behaviour
// -----------------------------------------------------------------------------

// liPhantomLabels is how many distinct labels the phantom experiment touches.
// It is large enough that the growth is a NUMBER rather than an anecdote and
// small enough to cost nothing.
const liPhantomLabels = 1000

// liPhantomBase is the first label id the experiment uses, chosen far from every
// other arm's ids.
const liPhantomBase uint32 = 0x0100_0000

// liDriveWithPhantom measures what an inverted (or empty) AddRange does to a
// label that has no entry.
//
// # This clause was a defect pin and is now a REGRESSION arm (#2608)
//
// [label.Index.AddRange] used to read the label's [index.NodeSet] out of the
// map, call `AddRange` on it, and store it back UNCONDITIONALLY, while
// `NodeSet.AddRange` promoted to the bitmap tier BEFORE looking at the interval.
// An interval that named no ids therefore left a bitmap behind, and the
// store-back created a map entry for a label with nothing in it.
//
// The entry was invisible through the query surface — `Count` 0, `Scan` nil,
// `Has` false — and permanent. It was also SERIALIZED, so it cost on-disk bytes
// and inflated the image's `labelCount`. MEASURED at the time: 1 000 inverted
// `AddRange` calls on distinct labels turned a 16-byte empty image into a
// 20 016-byte one declaring 1 000 labels, none carrying a single id.
//
// [label.Index.RemoveRange]'s godoc has always promised the opposite for its own
// direction — "Empty bitmaps are deleted so the map does not grow unboundedly
// after bulk-remove operations" — and MEASURED it kept that promise. `AddRange`
// now mirrors it: `NodeSet.AddRange` returns before promoting when the interval
// is inverted, and `label.Index.AddRange` deletes rather than stores an entry
// whose set is empty after the call. The two halves fix different things and
// neither alone suffices — without the first an existing inline label is still
// promoted one-way for nothing; without the second the zero-value set is still
// stored back and the entry is still minted.
//
// So this arm now asserts the repaired contract: an interval naming no ids
// leaves the image byte-identical to an empty index. It keeps the query-surface
// probe, because "invisible to every query path" was true before the fix and
// must stay true after it, and it keeps the round-trip check, which now asserts
// that nothing is re-declared on the way back in.
func liDriveWithPhantom(cfg *LabelIndexScopedConfig, ev *LabelIndexScopedEvidence) error {
	p := &ev.Phantom
	p.Labels = liPhantomLabels

	empty, err := liSerialize(label.NewIndex())
	if err != nil {
		return fmt.Errorf("serialize an empty index: %w", err)
	}
	p.EmptyBytes = len(empty)

	idx := label.NewIndex()
	for k := 0; k < liPhantomLabels; k++ {
		lbl := liPhantomBase + uint32(k)
		// from > to by one: the smallest interval that names nothing.
		idx.AddRange(lbl, graph.NodeID(k+1), graph.NodeID(k))
	}
	img, err := liSerialize(idx)
	if err != nil {
		return fmt.Errorf("serialize the empty-interval index: %w", err)
	}
	p.AfterBytes = len(img)
	p.AfterLabelCount = liImageLabelCount(img)
	if cfg.Perturb == liPerturbPhantomKept {
		// Reproduce the pre-#2608 measurement: one 20-byte entry apiece.
		p.AfterBytes = p.EmptyBytes + 20*liPhantomLabels
		p.AfterLabelCount = uint32(liPhantomLabels)
	}

	// The entry must be invisible to every query path. This was true of the
	// phantom too, and it is what confined the old defect to the serialized
	// form; it must not regress into a VISIBLE empty label.
	p.QueryVisible = false
	for k := 0; k < liPhantomLabels; k++ {
		lbl := liPhantomBase + uint32(k)
		if idx.Count(lbl) != 0 || idx.Scan(lbl) != nil || idx.Has(lbl, graph.NodeID(k)) {
			p.QueryVisible = true
			break
		}
	}

	// And nothing is re-declared on the way back in.
	fresh := label.NewIndex()
	if derr := fresh.Deserialize(bytes.NewReader(img)); derr != nil {
		return fmt.Errorf("deserialize the empty-interval image: %w", derr)
	}
	back, err := liSerialize(fresh)
	if err != nil {
		return fmt.Errorf("re-serialize the empty-interval image: %w", err)
	}
	p.RoundTripLabelCount = liImageLabelCount(back)
	if cfg.Perturb == liPerturbPhantomKept {
		p.RoundTripLabelCount = uint32(liPhantomLabels)
	}

	// The CONTROL: a label built by a range that names at least one id must
	// still be recorded. Without it, "no entry was created" would be consistent
	// with AddRange having stopped working altogether.
	ctrl := label.NewIndex()
	ctrl.AddRange(liPhantomBase, 10, 14)
	cimg, err := liSerialize(ctrl)
	if err != nil {
		return fmt.Errorf("serialize the non-empty control: %w", err)
	}
	p.CtrlLabelCount = liImageLabelCount(cimg)
	p.CtrlCount = ctrl.Count(liPhantomBase)
	if cfg.Perturb == liPerturbPhantomCtrlBad {
		p.CtrlLabelCount = 0
	}
	return nil
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// LabelIndexScopedConfig parameterises one run.
type LabelIndexScopedConfig struct {
	// Seed drives the base bands, the sweep order and the membership probes.
	Seed uint64
	// Epochs is the number of relationship-sweep epochs. 0 uses liEpochs.
	Epochs int
	// UnionDraws is the number of Union subsets per shape. 0 uses liUnionDraws.
	UnionDraws int
	// Perturb is the deliberate corruption to apply, threaded as a parameter so
	// no package-level variable carries it and two concurrent runs cannot see
	// each other's.
	Perturb liPerturb
}

// DefaultLabelIndexScopedConfig returns the configuration the catalogue runs.
func DefaultLabelIndexScopedConfig(seed uint64) LabelIndexScopedConfig {
	return LabelIndexScopedConfig{Seed: seed, Epochs: liEpochs, UnionDraws: liUnionDraws}
}

// RunLabelIndexScoped drives one whole run: the relationship sweep against the
// naive model, the Union arm, the serialize/deserialize round trip, the three
// damage families on a [SimDisk], the scope-routing table, and the two boundary
// pins.
//
// It returns the evidence in every case, a report when a clause or a gate fired,
// and an error only for a harness failure — which here means an entry point
// returned an error the arm did not construct, or the SimDisk refused a
// fixture step.
//
// The whole run is a pure function of the seed. There is no clock, no goroutine,
// no process-global counter and no allocation measurement anywhere in it, so
// unlike most scenarios in this package nothing has to be excluded from the
// digest, and the determinism claim is an equality on the whole of it.
func RunLabelIndexScoped(
	ctx context.Context, cfg LabelIndexScopedConfig,
) (*LabelIndexScopedEvidence, *SimReport, error) {
	if cfg.Epochs == 0 {
		cfg.Epochs = liEpochs
	}
	if cfg.UnionDraws == 0 {
		cfg.UnionDraws = liUnionDraws
	}
	seed := NewSeed(cfg.Seed ^ labelIndexScopedSeedMix)
	ev := &LabelIndexScopedEvidence{Perturb: cfg.Perturb}

	idx, model := liDriveSweep(&cfg, seed, ev)
	if err := ctx.Err(); err != nil {
		return ev, nil, err
	}
	liDriveUnion(&cfg, seed, idx, model, ev)
	if err := liDriveRoundTrip(&cfg, seed, idx, model, ev); err != nil {
		return ev, nil, err
	}
	if err := ctx.Err(); err != nil {
		return ev, nil, err
	}
	img, err := liSerialize(idx)
	if err != nil {
		return ev, nil, fmt.Errorf("serialize for the corruption arm: %w", err)
	}
	if err := liDriveCorruption(&cfg, seed, img, model, ev); err != nil {
		return ev, nil, err
	}
	liDriveScope(&cfg, ev)
	liDriveBoundary(&cfg, ev)
	if err := liDriveWithPhantom(&cfg, ev); err != nil {
		return ev, nil, err
	}
	ev.Digest = ev.computeDigest()

	v := append(checkLabelIndexScoped(ev), checkLabelIndexScopedNonVacuity(ev)...)
	if len(v) == 0 {
		return ev, nil, nil
	}
	return ev, liReport(cfg.Seed, ev, v), nil
}

// -----------------------------------------------------------------------------
// Clauses
// -----------------------------------------------------------------------------

// liOp renders a clause name as a Violation's Op field, in the package's
// standard bracketed form so a test can recover the clause name from a report.
func liOp(clause string) string { return "<label-index-scoped:" + clause + ">" }

// liViolation builds one clause violation.
func liViolation(kind ViolationKind, clause, format string, args ...any) Violation {
	return Violation{Kind: kind, Op: liOp(clause), Message: fmt.Sprintf(format, args...)}
}

// checkLabelIndexScoped adjudicates the run against every contract clause. The
// non-vacuity gates are separate ([checkLabelIndexScopedNonVacuity]) so "nothing
// was wrong" and "nothing was exercised" are never the same verdict.
func checkLabelIndexScoped(e *LabelIndexScopedEvidence) []Violation {
	v := make([]Violation, 0, 8)
	v = append(v, liCheckRangeModel(e)...)
	v = append(v, liCheckSerialization(e)...)
	v = append(v, liCheckCorruption(e)...)
	v = append(v, liCheckScope(e)...)
	v = append(v, liCheckPins(e)...)
	return v
}

// liCheckRangeModel adjudicates the membership surface against the naive model.
func liCheckRangeModel(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	attributed := 0
	for i := range e.Cells {
		c := &e.Cells[i]
		attributed += c.Mismatches
		if c.Mismatches == 0 {
			continue
		}
		v = append(v, liViolation(ViolationGraphIntegrity, "range-model",
			"the %s relationship driven through %s disagreed with the naive model on %d of %d "+
				"drives; across those drives the index's cardinality moved by %+d and the model's by "+
				"%+d. First difference: %s",
			c.Rel, c.Dir, c.Mismatches, c.Drives, c.IndexDelta, c.ModelDelta, e.FirstMismatch))
	}
	if e.Mismatches > attributed {
		v = append(v, liViolation(ViolationGraphIntegrity, "range-model",
			"%d membership disagreements with the naive model were not attributable to a swept "+
				"relationship (they landed on the post-clear comparison, which requires the label to "+
				"be empty). First difference: %s", e.Mismatches-attributed, e.FirstMismatch))
	}
	if e.UnionMismatches > 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "union-model",
			"%d of %d Union draws disagreed with the per-label naive union recomputed from the "+
				"model. First difference: %s", e.UnionMismatches, e.UnionDraws, e.FirstMismatch))
	}
	return v
}

// liCheckSerialization adjudicates the round trip, the emission determinism, the
// tier-identity claim, and the one-sided entry-population floor.
func liCheckSerialization(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	if e.RTContentMismatch > 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "roundtrip-content",
			"the deserialized image disagreed with the naive model on %d checks; the round trip is "+
				"compared against the MODEL, not against the index it came from. First difference: %s",
			e.RTContentMismatch, e.FirstMismatch))
	}
	if e.RTByteMismatch > 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "roundtrip-fixpoint",
			"the serialized form is not a FIXPOINT: the image after one round trip (%d bytes) and "+
				"the image after two differ, so re-reading and re-writing an index keeps changing "+
				"its bytes. The FIRST cycle is allowed to re-tier (measured stable=%v here, and "+
				"legitimately false for a run-encoded label of at most %d ids); every cycle after "+
				"it must be idempotent", e.ImageBytes2, e.FirstCycleStable, liSmallSetMaxMirror))
	}
	if e.SerializeUnstable > 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "serialize-stable",
			"%d of %d repeat Serialize calls on an UNCHANGED index produced different bytes; the "+
				"ascending-labelID sort exists to make the emission deterministic",
			e.SerializeUnstable, e.SerializeReruns))
	}
	if e.TierMismatch > 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "tier-identity",
			"%d of %d width pairs disagreed: an inline-tier set and a bitmap-tier set holding the "+
				"IDENTICAL ids serialized to different bytes. graph/index/label/index.go:407-415 "+
				"claims they are byte-identical, and that claim is what made the #1585 tiering "+
				"refactor a zero-migration change. First difference: %s",
			e.TierMismatch, e.TierChecks, e.FirstMismatch))
	}
	v = append(v, liCheckDenseSmallPin(e)...)
	if int(e.ImageLabelCount) < e.ModelLiveLabels {
		v = append(v, liViolation(ViolationGraphIntegrity, "entry-floor",
			"the serialized image declares %d labels but the naive model says %d labels carry at "+
				"least one member, so at least %d entries were lost on the way out",
			e.ImageLabelCount, e.ModelLiveLabels, e.ModelLiveLabels-int(e.ImageLabelCount)))
	}
	return v
}

// liCleanControlRegion is the region name of the undamaged trial.
const liCleanControlRegion = "clean-control"

// liCheckCorruption adjudicates the three damage families.
//
// The four clauses are deliberately disjoint, so one fact never fires two of
// them:
//
//   - `corrupt-clean` owns the undamaged control, and nothing else looks at it.
//   - `corrupt-refusal` owns every trial that must be REFUSED, and asks only
//     whether it was refused and whether the refusal wrapped
//     [index.ErrIndexCorrupted]. It never asks which guard answered, because for
//     the raw family that question has one answer by construction.
//   - `corrupt-guard` owns which guard answered, for the trials that name one,
//     and skips the control.
//   - `corrupt-restore` owns the receiver, for every trial that was refused.
//
// # Two of them are TRIPWIRES on their production path, and say so
//
// A clause that looks like a detector and cannot fail is worse than no clause,
// so both cases are named here rather than left to look like checks.
//
// `corrupt-restore` CANNOT fire against the current `Deserialize`. It reads the
// whole payload, validates the trailer, parses into a FRESH `bits` map, and only
// then takes the write lock and swaps — every error path returns before the
// swap, so the receiver is untouched by construction, not by good behaviour. The
// clause is kept because the swap is exactly what a plausible future
// optimisation would remove: parsing straight into the live map to avoid the
// second allocation would make a refused image leave the index half-populated,
// an ACID Consistency violation that nothing else here would notice. Its LOGIC
// is proved fireable by [liPerturbHideRestore].
//
// `corrupt-refusal` on the RAW family does not detect a CRC collision — CRC32C
// detects every single-byte error, so a flip cannot survive it — it detects the
// CRC CHECK being weakened, reordered after the parse, or removed. That is a
// real regression and a real detector; it is simply not a detector of the thing
// its name suggests. On the re-stamped and truncate families it is an ordinary
// detector, since those payloads reach the structural reader.
func liCheckCorruption(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	for i := range e.Corrupt {
		c := &e.Corrupt[i]
		if c.Region == liCleanControlRegion {
			if c.Refused {
				v = append(v, liViolation(ViolationGraphIntegrity, "corrupt-clean",
					"the UNDAMAGED image read back off the SimDisk was refused (%s); every refusal "+
						"below is then consistent with a reader that refuses everything, and the arm "+
						"proves nothing", c.Guard))
			}
			continue
		}
		if c.WantGuard != liGuardAccepted {
			if !c.Refused {
				v = append(v, liViolation(ViolationACIDDurability, "corrupt-refusal",
					"the %s damage to %s (offset %d, %d byte(s)) was ACCEPTED; a damaged index image "+
						"must be refused, not loaded", c.Family, c.Region, c.Off, c.Len))
			} else if !c.IsCorrupted {
				v = append(v, liViolation(ViolationACIDDurability, "corrupt-refusal",
					"the %s damage to %s was refused but the error does not wrap "+
						"index.ErrIndexCorrupted, so a caller cannot classify it as corruption",
					c.Family, c.Region))
			}
		}
		if c.WantGuard != "" && c.Guard != c.WantGuard {
			v = append(v, liViolation(ViolationACIDDurability, "corrupt-guard",
				"the %s damage to %s was answered by the %q guard, want %q; the guards are what "+
					"separate a checksum failure from a structural one and a caller reading the "+
					"message cannot tell them apart if they drift",
				c.Family, c.Region, c.Guard, c.WantGuard))
		}
		if c.Refused && !c.Restored {
			v = append(v, liViolation(ViolationACIDAtomicity, "corrupt-restore",
				"the receiver CHANGED across a refused Deserialize of the %s damage to %s; the "+
					"godoc says the receiver is restored to its pre-call state, and a reader that "+
					"half-applies a corrupt image leaves the index inconsistent",
				c.Family, c.Region))
		}
	}
	return v
}

// liCheckScope adjudicates the constructors' scopes and the Apply routing.
func liCheckScope(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	for i := range e.Scopes {
		s := &e.Scopes[i]
		if want := liExpectedScope(s.Ctor); s.Scope != want {
			v = append(v, liViolation(ViolationOracleDeviation, "scope-constructor",
				"%s built an index reporting scope %d, want %d", s.Ctor, s.Scope, want))
		}
		if s.Accepted != s.WantAccepted {
			verb := "consumed"
			if !s.Accepted {
				verb = "dropped"
			}
			v = append(v, liViolation(ViolationOracleDeviation, "scope-routing",
				"an index from %s (scope %d) %s a %s change; the naive routing table says a "+
					"node-scoped index takes the two node kinds and an edge-scoped index the two "+
					"edge kinds, so this one should have %s it",
				s.Ctor, s.Scope, verb, s.Op,
				map[bool]string{true: "consumed", false: "dropped"}[s.WantAccepted]))
		}
	}
	return v
}

// liCheckPins adjudicates the boundary contract and the two remaining defect
// pins.
//
// The boundary clauses assert the CORRECT answer: since #2607 the closed
// interval is honoured at math.MaxUint64 and both tiers agree. The phantom
// clauses still assert MEASURED behaviour that is wrong, and say so in their
// message — tripwires in the honest direction, firing the day the behaviour
// changes, which is exactly when the naive model and the docs need updating
// together. They are not a claim that the current behaviour is correct.
func liCheckPins(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	b := &e.Boundary
	if b.AddGot != b.AddNaive {
		v = append(v, liViolation(ViolationGraphIntegrity, "boundary-pin",
			"AddRange over the closed interval [max-%d, max] yielded cardinality %d, want %d. "+
				"The closed interval must be honoured at math.MaxUint64: the half-open conversion "+
				"cannot name one-past-max, so graph/index/nodeset.go splits the call through "+
				"addRangeClosed (#2607). A plain `to+1` wraps to zero and roaring treats "+
				"start>=end as a no-op, dropping the whole range",
			liBoundarySpan, b.AddGot, b.AddNaive))
	}
	if b.RemoveGot != b.RemoveNaive {
		v = append(v, liViolation(ViolationGraphIntegrity, "boundary-pin",
			"RemoveRange over the closed interval [max-3, max] left %d of %d ids on the BITMAP "+
				"tier, want %d. Same conversion, same wrap, repaired the same way through "+
				"removeRangeClosed (#2607)",
			b.RemoveGot, b.RemoveBefore, b.RemoveNaive))
	}
	if b.RemoveInlineGot != b.RemoveGot {
		v = append(v, liViolation(ViolationGraphIntegrity, "boundary-pin",
			"the same RemoveRange over the same membership left %d ids on the INLINE tier and %d "+
				"on the BITMAP tier. The tier a set occupies is not observable through the public "+
				"surface, so the two cannot disagree; before #2607 they did, because the inline "+
				"branches filter on the closed interval directly and never wrapped",
			b.RemoveInlineGot, b.RemoveGot))
	}
	if b.RemoveInlineBefore != b.RemoveBefore {
		v = append(v, liViolation(ViolationOracleDeviation, "boundary-pin",
			"the inline and bitmap fixtures started from %d and %d ids, so the tier comparison "+
				"above is not over identical membership and proves nothing",
			b.RemoveInlineBefore, b.RemoveBefore))
	}
	p := &e.Phantom
	if p.AfterLabelCount != 0 {
		v = append(v, liViolation(ViolationGraphIntegrity, "phantom-pin",
			"%d AddRange calls over an interval naming NO ids left an image declaring %d labels, "+
				"want 0. An interval that names nothing must leave no entry: label.Index.AddRange "+
				"deletes rather than stores a set that is empty after the call, mirroring "+
				"RemoveRange's own godoc promise (#2608)", p.Labels, p.AfterLabelCount))
	}
	if p.AfterBytes != p.EmptyBytes {
		v = append(v, liViolation(ViolationGraphIntegrity, "phantom-pin",
			"the empty-interval image is %d bytes against %d for an empty index; the entries must "+
				"cost nothing on disk (they used to cost 20 apiece)",
			p.AfterBytes, p.EmptyBytes))
	}
	if p.QueryVisible {
		v = append(v, liViolation(ViolationGraphIntegrity, "phantom-pin",
			"an empty-interval label was observable through Count, Scan or Has; it must be "+
				"invisible to every query path, which was true of the old phantom too and must "+
				"not regress into a visible empty label"))
	}
	if p.RoundTripLabelCount != p.AfterLabelCount {
		v = append(v, liViolation(ViolationOracleDeviation, "phantom-pin",
			"the image declared %d labels and %d after a Serialize/Deserialize cycle; the reader "+
				"re-declares an entry for every labelID in the image, so the two must agree",
			p.AfterLabelCount, p.RoundTripLabelCount))
	}
	if p.CtrlCount != 5 {
		v = append(v, liViolation(ViolationOracleDeviation, "phantom-pin",
			"the non-empty CONTROL recorded %d ids over a five-id range, want 5", p.CtrlCount))
	}
	return v
}

// -----------------------------------------------------------------------------
// Non-vacuity gates
// -----------------------------------------------------------------------------

// The coverage floors every run must clear. They are set from what the catalogue
// seed MEASURES, with enough margin that a different seed cannot trip them, and
// they exist so that "the run passed" cannot mean "the run reached nothing".
// [TestLabelIndexScoped_GatesAreWired] proves each is reachable by driving a
// configuration that cannot satisfy it.
const (
	// liFloorLabels is the number of labels the final index must carry. The
	// accumulating pool is liLabelPool labels and every epoch picks one, so a run
	// of liEpochs epochs reaches all of them with overwhelming probability; the
	// floor sits well below that.
	liFloorLabels = 3
	// liFloorMembers is the peak (label, node) pair count the sweep must reach, so
	// the roaring tier is genuinely exercised and not just the inline one.
	liFloorMembers = 500
	// liFloorRangeOps is the number of range operations the sweep must drive.
	liFloorRangeOps = 200
)

// checkLabelIndexScopedNonVacuity reports what the run FAILED TO REACH. A gate
// firing does not mean the index misbehaved; it means the run could not have
// noticed if it had.
func checkLabelIndexScopedNonVacuity(e *LabelIndexScopedEvidence) []Violation {
	v := make([]Violation, 0, 4)
	v = append(v, liGateSweep(e)...)
	v = append(v, liGateUnion(e)...)
	v = append(v, liGateCorruption(e)...)
	v = append(v, liGateScopeAndPins(e)...)
	v = append(v, liGateRoundTripShape(e)...)
	return v
}

// liGateSweep certifies that the relationship sweep reached every cell and moved
// enough state to be worth adjudicating.
func liGateSweep(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	seen := map[liCell]int{}
	for i := range e.Cells {
		seen[liCell{Rel: e.Cells[i].Rel, Dir: e.Cells[i].Dir}] = e.Cells[i].Drives
	}
	var missing []string
	for r := liRel(0); r < liRelCount; r++ {
		for d := liDir(0); d < liDirCount; d++ {
			if seen[liCell{Rel: r, Dir: d}] == 0 {
				missing = append(missing, r.String()+"/"+d.String())
			}
		}
	}
	if len(missing) > 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:cells",
			"%d of the %d (relationship, direction) cells were never driven, so the "+
				"inclusive/exclusive conversion was not swept: %s",
			len(missing), int(liRelCount)*int(liDirCount), strings.Join(missing, ", ")))
	}
	if e.FinalLabels < liFloorLabels {
		v = append(v, liViolation(ViolationVacuousRun, "gate:model-size",
			"the final index carries %d labels, below the floor of %d; a sweep that ends on one "+
				"label never exercises the per-label map at all",
			e.FinalLabels, liFloorLabels))
	}
	if e.PeakMembers < liFloorMembers {
		v = append(v, liViolation(ViolationVacuousRun, "gate:model-size",
			"the model peaked at %d (label, node) pairs, below the floor of %d; below the "+
				"small-set threshold of 8 per label the bitmap tier is never reached and the range "+
				"methods are compared only against the inline path",
			e.PeakMembers, liFloorMembers))
	}
	if e.RangeOps < liFloorRangeOps {
		v = append(v, liViolation(ViolationVacuousRun, "gate:model-size",
			"the sweep drove %d range operations, below the floor of %d",
			e.RangeOps, liFloorRangeOps))
	}
	if e.EmptiedLabels == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:emptied",
			"no RemoveRange ever drove a non-empty label to empty, so the entry-deletion path "+
				"RemoveRange's godoc describes was never taken"))
	}
	if e.PromotedAfterAdd == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:promoted",
			"no epoch drove individual Adds BEFORE its range operations, so the add-then-range "+
				"order — the one that folds an existing inline set into a fresh bitmap — was never "+
				"driven"))
	}
	if e.TierChecks != len(liTierWidths()) {
		v = append(v, liViolation(ViolationVacuousRun, "gate:tier-checks",
			"the tier-identity arm compared %d width pairs, want %d",
			e.TierChecks, len(liTierWidths())))
	}
	if e.RoundTrips == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:roundtrip",
			"no image was round-tripped, so the content, byte-stability and determinism clauses "+
				"adjudicated nothing"))
	}
	return v
}

// liGateUnion certifies that the Union arm reached all four subset shapes. A
// union of one known label is a Scan with extra steps, and a run that only drew
// those would satisfy `union-model` without exercising the fold.
func liGateUnion(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	var missing []string
	if e.UnionMultiLabel == 0 {
		missing = append(missing, "multi-label")
	}
	if e.UnionUnknownLabel == 0 {
		missing = append(missing, "unknown-label")
	}
	if e.UnionDuplicate == 0 {
		missing = append(missing, "duplicate-label")
	}
	if e.UnionEmptyDraws == 0 {
		missing = append(missing, "empty-subset")
	}
	if len(missing) > 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:union-shapes",
			"the Union arm never reached %s across %d draws; those are the shapes that separate a "+
				"real fold from a single-label Scan", strings.Join(missing, ", "), e.UnionDraws))
	}
	return v
}

// liGateCorruption certifies that every damage region was driven, that the
// undamaged control was present, and that every receiver held state before the
// call — without which "restored to its pre-call state" is a claim about an
// empty index.
func liGateCorruption(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	rawSeen := map[string]bool{}
	restampSeen, truncateSeen, control := 0, 0, false
	unpopulated := 0
	for i := range e.Corrupt {
		c := &e.Corrupt[i]
		if !c.TargetWasPopulated {
			unpopulated++
		}
		switch {
		case c.Region == liCleanControlRegion:
			control = true
		case c.Family == liFamilyRaw:
			rawSeen[c.Region] = true
		case c.Family == liFamilyRestamp:
			restampSeen++
		default:
			truncateSeen++
		}
	}
	var missing []string
	for _, region := range liRegionOrder() {
		if !rawSeen[region] {
			missing = append(missing, region)
		}
	}
	if len(missing) > 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:corrupt-regions",
			"the raw damage family never reached %s; every region of the layout must be flipped, "+
				"because the claim being made is that the CRC covers all of them",
			strings.Join(missing, ", ")))
	}
	if restampSeen < liRestampTrials {
		v = append(v, liViolation(ViolationVacuousRun, "gate:corrupt-regions",
			"the re-stamped family drove %d trials, want %d; the structural guards inside "+
				"Deserialize are unreachable without a recomputed trailer, so a run that skips them "+
				"has tested only the checksum", restampSeen, liRestampTrials))
	}
	if truncateSeen < liTruncateTrialsMin {
		v = append(v, liViolation(ViolationVacuousRun, "gate:corrupt-regions",
			"the truncate family drove %d trials, want at least %d — the short-payload branch and "+
				"the truncated-body branch are different code",
			truncateSeen, liTruncateTrialsMin))
	}
	if !control {
		v = append(v, liViolation(ViolationVacuousRun, "gate:clean-control",
			"no undamaged control was driven, so nothing established that this reader accepts a "+
				"good image at all"))
	}
	if unpopulated > 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:corrupt-populated",
			"%d of %d damage trials deserialized into an EMPTY receiver, so \"the receiver was "+
				"restored to its pre-call state\" was satisfied by an index that had no state to "+
				"restore", unpopulated, len(e.Corrupt)))
	}
	return v
}

// liRestampTrials / liTruncateTrialsMin are the trial counts the corruption
// gates require. They mirror the tables in [liDriveRestampFamily] and
// [liTruncateLengths]; a table that shrinks without the constant moving fails
// the gate rather than silently narrowing the sweep.
const (
	liRestampTrials     = 6
	liTruncateTrialsMin = 6
)

// liGateScopeAndPins certifies the scope table is complete and both directions
// of the routing rule are represented, that the boundary arm's control is exact
// — without which behaviour at math.MaxUint64 would not be attributable to the
// final id — and that the empty-interval arm's control actually recorded
// something, without which its "no entry" assertion is satisfied by a harness
// that measured nothing.
func liGateScopeAndPins(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	wantRows := 3 * len(liScopeOps())
	if len(e.Scopes) != wantRows {
		v = append(v, liViolation(ViolationVacuousRun, "gate:scope-rows",
			"the scope arm drove %d (constructor, change kind) rows, want %d",
			len(e.Scopes), wantRows))
	}
	accepts, drops := 0, 0
	for i := range e.Scopes {
		if e.Scopes[i].WantAccepted {
			accepts++
		} else {
			drops++
		}
	}
	if accepts == 0 || drops == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:scope-rows",
			"the routing table expects %d acceptances and %d drops; with either at zero the "+
				"`scope-routing` clause would pass on an index that ignored every change, or on one "+
				"that consumed every change", accepts, drops))
	}
	if e.Boundary.AddBelowGot != e.Boundary.AddBelowNaive {
		v = append(v, liViolation(ViolationVacuousRun, "gate:boundary-control",
			"the boundary CONTROL — the same interval ending one id below math.MaxUint64 — yielded "+
				"%d ids, want %d. Until the control is exact, the loss the pin records is not "+
				"attributable to the final id rather than to the whole top of the id space",
			e.Boundary.AddBelowGot, e.Boundary.AddBelowNaive))
	}
	if e.Phantom.Labels == 0 || e.Phantom.CtrlLabelCount == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:phantom-armed",
			"the empty-interval experiment drove %d labels and its CONTROL — a range naming five "+
				"ids — left %d entries in the image. With either at zero, "+
				"\"no entry was created\" is satisfied by a harness that called nothing, or by an "+
				"AddRange that records nothing at all",
			e.Phantom.Labels, e.Phantom.CtrlLabelCount))
	}
	return v
}

// -----------------------------------------------------------------------------
// Catalogue wiring
// -----------------------------------------------------------------------------

// liReport wraps violations in a scenario report. It PANICS on an empty
// violation slice: a non-nil report that names nothing is a reporting defect,
// which SimReport.String shouts about and report_render_test pins.
func liReport(seed uint64, ev *LabelIndexScopedEvidence, v []Violation) *SimReport {
	if len(v) == 0 {
		panic("sim: liReport called with no violations; a report must always name what it found")
	}
	if len(v) > liReportCap {
		v = v[:liReportCap]
	}
	return &SimReport{
		Scenario:   ScenarioLabelIndexScoped,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<label-index-scoped: " + ev.String() + ">"},
		Violations: v,
	}
}

// labelIndexScopedScenario builds the catalogue entry.
//
// Mode is [ModeDeterministic] with a run override. The run is bit-reproducible
// from the seed with nothing excluded — it drives one library type with no
// clock, no goroutine, no store and no Cypher — and the override is mandatory
// rather than stylistic: a ModeDeterministic scenario without one dispatches to
// runDeterministic, which drives the engine's Cypher surface through a shadow
// oracle, and this scenario has no engine. generation-swap, readtx-isolation and
// pagerank-ranker use the same pairing for the same reason.
func labelIndexScopedScenario() Scenario {
	return Scenario{
		Name: ScenarioLabelIndexScoped,
		Description: "the label index's scoped and range surface driven directly against a naive " +
			"map model: thirteen geometric relationships between an operand range and an existing " +
			"band — disjoint, adjacent on each side, overlapping on each side, contained, " +
			"containing, identical, single-element, inverted and the off-by-one empty range — " +
			"swept in BOTH directions so the inclusive-to-exclusive conversion is bracketed from " +
			"each side, with Scan, Count and Has adjudicated after every operation; Union held to " +
			"a per-label naive union across multi-label, unknown-label, duplicate-label and empty " +
			"subsets; the serialized image round-tripped and compared against the MODEL rather " +
			"than against the index that produced it, its emission proved deterministic and its " +
			"bytes proved independent of which storage tier the set sits on; three damage " +
			"families on a SimDisk — a raw byte flip in each of the seven layout regions, which " +
			"the CRC32C catches wherever it lands, a re-stamped family that is the only way to " +
			"reach the four structural guards, and a short read — each refused with " +
			"ErrIndexCorrupted and each leaving a POPULATED receiver untouched; the three " +
			"constructors' scopes and the Apply routing table that is the only place a scope is " +
			"observable; and two latent defects pinned to their measured behaviour",
		Mode:        ModeDeterministic,
		DefaultSeed: labelIndexScopedDefaultSeed,
		run:         runLabelIndexScopedScenario,
	}
}

// runLabelIndexScopedScenario is the scenario's run override.
func runLabelIndexScopedScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := RunLabelIndexScoped(ctx, DefaultLabelIndexScopedConfig(seed))
	if err != nil {
		return nil, err
	}
	return report, nil
}

// liCheckDenseSmallPin adjudicates the round-trip non-idempotence pin.
//
// Like the boundary and phantom pins, this clause asserts MEASURED behaviour
// that is wrong, and says so in its message. It fires the day the behaviour
// changes, which is exactly when the fixpoint formulation in [liDriveRoundTrip]
// and the note in the file header need revisiting.
func liCheckDenseSmallPin(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	d := &e.DenseSmall
	if d.Stable {
		v = append(v, liViolation(ViolationOracleDeviation, "dense-small-pin",
			"an AddRange-built label of %d ids round-tripped byte-stably (%d -> %d bytes), and this "+
				"pin records the MEASURED instability (55 -> 72 on the reference host): "+
				"index.NodeSetFromBitmap down-converts a bitmap of at most smallSetMax=%d ids to the "+
				"inline tier, which re-materialises through AddMany as an ARRAY container where "+
				"AddRange had built a RUN container. If this fired because the down-convert now "+
				"preserves the encoding, that is good news: update this pin and the fixpoint note in "+
				"liDriveRoundTrip", d.Width, d.First, d.Second, liSmallSetMaxMirror))
	}
	if d.Second != d.Third {
		v = append(v, liViolation(ViolationGraphIntegrity, "dense-small-pin",
			"the dense-small fixture never converges: %d, then %d, then %d bytes. The re-tiering is "+
				"supposed to happen at most ONCE, and a form that keeps changing is a different and "+
				"worse defect than the one this pin records", d.First, d.Second, d.Third))
	}
	return v
}

// liGateRoundTripShape certifies the two attribution conditions the serialization
// arms depend on.
func liGateRoundTripShape(e *LabelIndexScopedEvidence) []Violation {
	var v []Violation
	if !e.DenseSmall.CtrlStable {
		v = append(v, liViolation(ViolationVacuousRun, "gate:dense-small-control",
			"the dense-small CONTROL — the same AddRange construction at %d ids, one ABOVE "+
				"smallSetMax — was not byte-stable across its round trip (%d -> %d). Until the "+
				"control holds, the instability at %d ids is not attributable to the down-convert "+
				"threshold rather than to round trips in general",
			e.DenseSmall.CtrlWidth, e.DenseSmall.CtrlFirst, e.DenseSmall.CtrlSecond,
			e.DenseSmall.Width))
	}
	same, differ := 0, 0
	for i := range e.RangeTier {
		if e.RangeTier[i].Equal {
			same++
		} else {
			differ++
		}
	}
	if same == 0 || differ == 0 {
		v = append(v, liViolation(ViolationVacuousRun, "gate:range-tier-crossover",
			"the Add-versus-AddRange measurement found %d identical and %d differing widths; it "+
				"must BRACKET the crossover (below it roaring keeps an array container, at and above "+
				"it a run container), or the row is an anecdote rather than a measurement",
			same, differ))
	}
	return v
}
