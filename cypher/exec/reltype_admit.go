package exec

// reltype_admit.go — the slot-aligned relationship-type column and the
// branch-free admission test built over it (rmp #2251).
//
// # What it replaces, and why
//
// Every typed expand used to test a slot's relationship type with a probe into a
// map[uint64]string keyed by the slot's ABSOLUTE FORWARD CSR POSITION. That map
// carried one entry per accepted slot across the WHOLE graph, so on a graph with
// one dominant relationship type it was Θ(E) — and the probe was paid once per
// CSR slot the traversal touched. The REVERSE direction was worse: a reverse slot
// carries no type information at all, so the operator first had to recover the
// slot's FORWARD position (an O(log d + r) handle match, or an O(log d) lower
// bound) and only then probe the map.
//
// A [RelTypeColumn] is the same information laid out slot-for-slot beside the CSR
// arcs it describes: fwdCodes[pos] is the resolved relationship type of forward
// arc pos, revCodes[revPos] the same for the reverse arc. A type check becomes one
// indexed load and one bit test, in BOTH directions, with no position recovery on
// the reverse side.
//
// # The code space is the LabelID space
//
// A code is [lpg.EncodeSlotLabel]'s encoding of a relationship type's LabelID:
// lid+1, with 0 reserved for "this slot carries no relationship type". Reusing
// that encoding rather than inventing a private dictionary means the column speaks
// the same language as the adjacency label column it is derived from, and it makes
// the admission mask a bitmap over LabelID space — see [RelTypeMask].
//
// # One code per slot, plus a patch list
//
// A relationship carries exactly one type in openCypher, so one uint32 per slot
// describes every arc of every Cypher-built graph. The Go API can nonetheless
// attach several types to one arc ([lpg.Graph.SetEdgeLabelByHandle] over a bag,
// the pair overflow list), so the dense column holds the FIRST resolved type and a
// sparse exception map holds the rest. That is dictionary encoding with a patch
// list: dense and cache-friendly for the case that is universal, exact for the
// case that is not. The exception maps are nil for every graph Cypher built, so
// the hot path's only cost for supporting them is one nil check on a rejected
// slot.
//
// # Ownership and concurrency
//
// A [RelTypeColumn] is IMMUTABLE once built and is cached beside the CSR pair it
// describes, so any number of goroutines may hold and read one concurrently. A
// [RelTypeAdmit] pairs a column with ONE pattern's accepted-type mask; it is
// likewise immutable and safe for concurrent use. Neither owns the CSR.

// relTypeUnknown marks a reverse slot whose forward counterpart could not be
// established when the column was built, so the operator must fall back to its own
// runtime position recovery for that slot. It is distinct from code 0 ("this slot
// carries no type", a definitive answer that rejects) precisely because a
// permissive stand-in for an unresolved slot is what rmp #2220/#2236 had to
// remove: an unresolved slot is an ABSENCE OF INFORMATION, never an admission.
const relTypeUnknown = ^uint32(0)

// RelTypeColumn is the resolved relationship type of every arc of one CSR pair,
// laid out slot-for-slot beside the arcs.
//
// It is keyed to the exact CSR pair it was built against — the positions name
// that pair's arcs and no other's — exactly as the position-keyed filter map it
// replaces was. A column built for one pair applied to another silently MISTYPES
// relationships rather than merely missing them.
//
// A RelTypeColumn is immutable once built and safe for concurrent use.
type RelTypeColumn struct {
	// fwdCodes[pos] is the encoded relationship type of forward arc pos, or 0
	// when that arc carries no type. Always non-nil and len == len(fwd arcs).
	fwdCodes []uint32
	// revCodes[revPos] is the same for the reverse arc, or [relTypeUnknown] for a
	// slot whose forward counterpart could not be established. nil when the
	// reverse pairing could not be established AT ALL, in which case every reverse
	// slot falls back to the caller's own recovery.
	revCodes []uint32
	// fwdExtra/revExtra hold the SECOND AND LATER types of the rare arc that
	// carries more than one. nil (never merely empty) for every graph built
	// through Cypher.
	fwdExtra map[uint64][]uint32
	revExtra map[uint64][]uint32
	// revExact reports that revCodes resolves EVERY reverse slot — no
	// [relTypeUnknown] entry anywhere. It is the precondition a two-sided search
	// needs before it may test reverse slots directly (see
	// [ShortestPath.canBidirectional]).
	revExact bool
}

// NewRelTypeColumn assembles a column from already-resolved code arrays. It is
// the constructor the cypher package's builder uses; the arrays must be sized to
// the CSR pair's forward and reverse arc counts respectively.
//
// revCodes may be nil, meaning "no reverse pairing was established"; extra maps
// may be nil, meaning "no arc carries more than one type".
func NewRelTypeColumn(fwdCodes, revCodes []uint32, fwdExtra, revExtra map[uint64][]uint32) *RelTypeColumn {
	c := &RelTypeColumn{fwdCodes: fwdCodes, revCodes: revCodes, fwdExtra: fwdExtra, revExtra: revExtra}
	c.revExact = revCodes != nil
	for _, code := range revCodes {
		if code == relTypeUnknown {
			c.revExact = false
			break
		}
	}
	return c
}

// RelTypeUnknownCode is [relTypeUnknown] exported for the builder in the cypher
// package, which must mark an unresolvable reverse slot with the same sentinel
// this package tests for.
const RelTypeUnknownCode = relTypeUnknown

// RelTypeMask is the set of relationship-type codes ONE pattern accepts, as a
// bitmap over LabelID space.
//
// Admission is therefore O(1) in the number of accepted types rather than O(types)
// per candidate slot: a `[r:A|B|C|…]` with fifty alternatives costs exactly what
// `[r:A]` costs. The common case — every LabelID below 64 — is a single word held
// inline, so the test is one shift and one AND with no indirection at all; hi
// carries LabelIDs 64 and above and is nil until one is actually accepted.
//
// The hi fallback is not decorative: LabelID space is shared between node labels
// and relationship types, so a schema with a few dozen of each reaches 64 without
// anything exotic happening. [TestRelTypeMask_HighLabelIDs] exercises it.
type RelTypeMask struct {
	lo uint64   // LabelIDs 0..63
	hi []uint64 // LabelIDs 64..; nil until one is set
}

// setCode adds an encoded code (lid+1) to the mask. Code 0 ("no type") is not a
// type any pattern can name and is ignored.
func (m *RelTypeMask) setCode(code uint32) {
	if code == 0 || code == relTypeUnknown {
		return
	}
	lid := uint64(code - 1)
	if lid < 64 {
		m.lo |= 1 << lid
		return
	}
	w := int(lid/64) - 1
	for len(m.hi) <= w {
		m.hi = append(m.hi, 0)
	}
	m.hi[w] |= 1 << (lid % 64)
}

// has reports whether code is accepted.
func (m *RelTypeMask) has(code uint32) bool {
	if code == 0 {
		return false
	}
	lid := uint64(code - 1)
	if lid < 64 {
		return m.lo&(1<<lid) != 0
	}
	w := int(lid/64) - 1
	if w >= len(m.hi) {
		return false
	}
	return m.hi[w]&(1<<(lid%64)) != 0
}

// RelTypeAdmit pairs a [RelTypeColumn] with one pattern's accepted-type mask. It
// is what an operator holds for the life of an Init, and it is immutable and safe
// for concurrent use.
//
// # It is a VALUE, deliberately
//
// An [AdjacencySource] is consulted in Init, and Init runs once per OUTER ROW
// under Apply — so anything the source allocates is allocated per row. Returning
// the view by value keeps that at zero allocations for every graph whose
// relationship types live below LabelID 64, which is every ordinary schema. The
// zero value means "no type filter was resolved", the state the nil filter map it
// replaced stood for.
type RelTypeAdmit struct {
	col  *RelTypeColumn
	mask RelTypeMask
}

// Admit returns the admission view of c for the given encoded codes — the
// pattern's accepted relationship types, already encoded by the caller through
// the same encoding the column uses.
//
// The column is shared; the returned view is not, but it is immutable, so a
// caller may hold it for as long as the column lives.
func (c *RelTypeColumn) Admit(codes []uint32) RelTypeAdmit {
	a := RelTypeAdmit{col: c}
	for _, code := range codes {
		a.mask.setCode(code)
	}
	return a
}

// Active reports whether this view carries a column at all. A view that does not
// is the zero value, which every operator reads exactly as it read a nil filter
// map.
func (a *RelTypeAdmit) Active() bool { return a != nil && a.col != nil }

// RevExact reports whether every reverse slot's type is known from the column, so
// a reverse-scanning search may test slots directly instead of recovering each
// one's forward position. False means "some slot would have to fall back", which
// is the condition a two-sided typed search must decline on rather than answer
// permissively (rmp #2236).
func (a *RelTypeAdmit) RevExact() bool {
	return a != nil && a.col != nil && a.col.revExact
}

// Fwd reports whether the forward arc at pos is of a type this pattern accepts.
//
// A position outside the column REJECTS. The column and the CSR are the same
// snapshot by construction, so an out-of-range position is a caller error, and the
// filter map this replaced answered it the same way — an absent key is a
// rejection.
func (a *RelTypeAdmit) Fwd(pos uint64) bool {
	if a == nil || a.col == nil {
		return false
	}
	if pos >= uint64(len(a.col.fwdCodes)) {
		return false
	}
	if a.mask.has(a.col.fwdCodes[pos]) {
		return true
	}
	if a.col.fwdExtra == nil {
		return false
	}
	return a.anyExtra(a.col.fwdExtra, pos)
}

// Rev reports whether the REVERSE arc at revPos is of an accepted type, and
// whether the column could answer at all. known == false means the caller must
// recover the arc's forward position itself and test that with [RelTypeAdmit.Fwd]
// — never that the arc is admitted.
func (a *RelTypeAdmit) Rev(revPos uint64) (admit, known bool) {
	if a == nil || a.col == nil || a.col.revCodes == nil {
		return false, false
	}
	if revPos >= uint64(len(a.col.revCodes)) {
		return false, false
	}
	code := a.col.revCodes[revPos]
	if code == relTypeUnknown {
		return false, false
	}
	if a.mask.has(code) {
		return true, true
	}
	if a.col.revExtra == nil {
		return false, true
	}
	return a.anyExtra(a.col.revExtra, revPos), true
}

// anyExtra tests the patch list of a multi-type arc. It is deliberately NOT
// inlined into Fwd/Rev: keeping it out of line leaves those two as a bounds check,
// an indexed load and a bit test, which is the whole point of the column.
func (a *RelTypeAdmit) anyExtra(extra map[uint64][]uint32, pos uint64) bool {
	for _, code := range extra[pos] {
		if a.mask.has(code) {
			return true
		}
	}
	return false
}

// relTypeAdmitFromPositions builds a column-backed admission view from an
// explicit set of ACCEPTED forward positions — the shape [StaticAdjacency] takes.
//
// It exists so a caller that genuinely holds a position set (an offline
// traversal, or a test that builds its own CSR) still exercises the column and
// not a second, parallel admission mechanism. Each distinct value in the set is
// given its own synthetic code and every code is accepted, so admission over the
// column is exactly membership of the set. The reverse codes are derived by the
// same transpose replay the production builder uses, and are simply absent when it
// declines — the caller then falls back to its own position recovery, as it did
// before the column existed.
//
// A nil set returns the zero view, which every operator reads as "no filter was
// resolved" and answers exactly as it answered a nil map.
func relTypeAdmitFromPositions(fwd, rev CSRAdjacency, accepted map[uint64]string) RelTypeAdmit {
	if accepted == nil || fwd == nil {
		return RelTypeAdmit{}
	}
	fwdEdges := fwd.EdgesSlice()
	fwdCodes := make([]uint32, len(fwdEdges))
	codeOf := make(map[string]uint32, len(accepted))
	var codes []uint32
	for pos, name := range accepted {
		if pos >= uint64(len(fwdCodes)) {
			continue
		}
		code, ok := codeOf[name]
		if !ok {
			code = uint32(len(codeOf)) + 1
			codeOf[name] = code
			codes = append(codes, code)
		}
		fwdCodes[pos] = code
	}
	col := NewRelTypeColumn(fwdCodes, transposeRevCodes(fwd, rev, fwdCodes), nil, nil)
	return col.Admit(codes)
}

// transposeRevCodes projects a forward code column onto reverse-CSR positions by
// replaying [csr.CSR.BuildReverse]'s counting-sort scatter ([fwdToRevByTranspose]),
// or returns nil when that replay declines.
//
// The replay is used rather than a per-slot scan for the reason
// [revTypeAdmitSet] documents: every other reverse→forward mapping in this package
// has a case it cannot resolve, and for a TYPE test an unresolved case can only be
// answered permissively — which is precisely the defect rmp #2236 removed. The
// replay validates every slot or declines outright, so a nil result here means
// "the caller keeps its own recovery", never "admit".
func transposeRevCodes(fwd, rev CSRAdjacency, fwdCodes []uint32) []uint32 {
	if rev == nil {
		return nil
	}
	fwdVerts, fwdEdges, fwdHandles := fwd.VerticesSlice(), fwd.EdgesSlice(), fwd.HandlesSlice()
	revVerts, revEdges, revHandles := rev.VerticesSlice(), rev.EdgesSlice(), rev.HandlesSlice()
	revCodes := make([]uint32, len(revEdges))
	if !projectFwdToRevByTranspose(
		fwdVerts, fwdEdges, fwdHandles, revVerts, revEdges, revHandles,
		func(fwdPos, revPos uint64) { revCodes[revPos] = fwdCodes[fwdPos] },
	) {
		return nil
	}
	return revCodes
}

// NewRelTypeColumnFor assembles the column for one CSR pair from an
// already-resolved FORWARD code array, deriving the reverse array itself.
//
// It is the constructor a query planner uses: resolving an arc's type is the
// planner's job (it needs the graph), while pairing a reverse slot with its
// forward counterpart is this package's, because the pairing machinery
// ([fwdToRevByTranspose], [buildRevToFwd]) lives here and every other consumer of
// it does too.
//
// # The reverse derivation, and its decline path
//
// The transpose replay is tried first. It validates every slot or declines
// outright, so when it succeeds the reverse column is EXACT for every slot and a
// two-sided typed search may run ([RelTypeAdmit.RevExact]).
//
// When it declines — a reverse CSR that is not the canonical transpose of this
// forward one — the per-slot scan is used instead. That scan HAS an unresolvable
// case, and the decline path is where a type check goes wrong if it is written
// carelessly: rmp #2236 had to remove exactly such a permissive stand-in. So an
// unresolvable slot is recorded as [relTypeUnknown], which is not an admission and
// not a rejection but an instruction to the operator to recover the position
// itself, and it makes RevExact false so a two-sided search declines rather than
// guessing.
//
// A nil reverse adjacency yields a nil reverse column, which every caller reads
// the same way: fall back to its own recovery.
func NewRelTypeColumnFor(
	fwd, rev CSRAdjacency, fwdCodes []uint32, fwdExtra map[uint64][]uint32,
) *RelTypeColumn {
	if rev == nil {
		return NewRelTypeColumn(fwdCodes, nil, fwdExtra, nil)
	}
	fwdVerts, fwdEdges, fwdHandles := fwd.VerticesSlice(), fwd.EdgesSlice(), fwd.HandlesSlice()
	revVerts, revEdges, revHandles := rev.VerticesSlice(), rev.EdgesSlice(), rev.HandlesSlice()

	revCodes := make([]uint32, len(revEdges))
	var revExtra map[uint64][]uint32

	// Project the forward codes along the transpose replay WITHOUT materialising
	// the position mapping: the mapping array is len(fwdEdges) uint64s — 7.68 MB on
	// the 960k-arc cypher_scale fixture — and nothing here needs it after the
	// projection. See [projectFwdToRevByTranspose] for why applying before a later
	// validation failure is safe HERE specifically: the fallback below overwrites
	// every revCodes entry.
	if projectFwdToRevByTranspose(
		fwdVerts, fwdEdges, fwdHandles, revVerts, revEdges, revHandles,
		func(fwdPos, revPos uint64) {
			revCodes[revPos] = fwdCodes[fwdPos]
			if extra, ok := fwdExtra[fwdPos]; ok {
				if revExtra == nil {
					revExtra = make(map[uint64][]uint32, len(fwdExtra))
				}
				revExtra[revPos] = extra
			}
		},
	) {
		return NewRelTypeColumn(fwdCodes, revCodes, fwdExtra, revExtra)
	}
	// The replay declined, possibly after writing part of revCodes. Every entry is
	// rewritten below, and revExtra is discarded, so no partial state survives.
	revExtra = nil

	revToFwd := buildRevToFwd(fwdVerts, fwdEdges, fwdHandles, revVerts, revEdges, revHandles)
	for revPos, fwdPos := range revToFwd {
		if fwdPos == unresolvedFwdPos || fwdPos >= uint64(len(fwdCodes)) {
			revCodes[revPos] = relTypeUnknown
			continue
		}
		revCodes[revPos] = fwdCodes[fwdPos]
		if extra, ok := fwdExtra[fwdPos]; ok {
			if revExtra == nil {
				revExtra = make(map[uint64][]uint32, len(fwdExtra))
			}
			revExtra[uint64(revPos)] = extra
		}
	}
	return NewRelTypeColumn(fwdCodes, revCodes, fwdExtra, revExtra)
}

// RelTypeColumnBytes reports the resident footprint of the column's own arrays and
// patch lists, so a caller can measure what the column costs rather than assume it.
// It counts the dense code arrays and the exception maps' keys and values; it does
// NOT count the CSR the column describes, which the caller already holds.
func (c *RelTypeColumn) RelTypeColumnBytes() int64 {
	if c == nil {
		return 0
	}
	const codeBytes = 4
	n := int64(len(c.fwdCodes)+len(c.revCodes)) * codeBytes
	for _, m := range []map[uint64][]uint32{c.fwdExtra, c.revExtra} {
		for _, v := range m {
			// One map entry: an 8-byte key, a 24-byte slice header, and the codes.
			n += 8 + 24 + int64(len(v))*codeBytes
		}
	}
	return n
}
