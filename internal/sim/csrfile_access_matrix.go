package sim

// csrfile_access_matrix.go — the WeightKind x AccessPattern matrix for the
// csrfile arm of the DST storage battery (rmp #2478).
//
// Before this task the csrfile arm (ST4, storage_fault_scenarios.go) published
// exactly ONE fixture: a float64-weighted CSR, read back through the default
// access pattern. Four gaps followed from that:
//
//   - Four of the five [csrfile.WeightKind] values were never written by the
//     simulator, so no reader path for them was ever adjudicated here.
//   - [csrfile.AccessPattern] and [csrfile.Reader.SetHint] were never called at
//     all, so nothing pinned that an advisory hint cannot change the data.
//   - [csrfile.WeightAbsent] — the value that says "this file carries no
//     weights" — was never produced, so nothing distinguished it from a
//     weighted file. That distinction is what rmp #2526 turned into a durability
//     question: there, "no weight" and "a weight this writer cannot encode"
//     produced the SAME bytes, and the difference was permanent data loss.
//   - [csrfile.Reinterpret]'s documented alignment precondition was never
//     probed, so nothing pinned that it refuses rather than handing back a
//     view over memory it may not read.
//
// This file drives the whole 5 x 5 grid — every weight kind against every
// access pattern — over the in-memory [SimDisk], plus a truncation battery
// against both an ALIGNED and a NON-aligned cut, and records what each
// combination actually did.
//
// # One thing the in-memory backend CANNOT reach
//
// [csrfile.Reader.SetHint] short-circuits on a byte-backed Reader: the DST
// filesystem seam ([csrfile.OpenWith] over a non-OS backend) reads the whole
// image into a heap buffer and leaves Reader.mm nil, and SetHint returns nil
// without calling madvise when there is no mapping to advise. So every cell
// here proves the CONTRACT (a hint is accepted on a live reader, refused on a
// closed one, and never alters a byte) but NOT the syscall. The madvise path
// itself is only reachable through [csrfile.Open], which always mmaps; that arm
// lives in csrfile_access_matrix_test.go against a real temp directory. Reading
// a green matrix here as evidence that madvise ran would be exactly the
// mistake this comment exists to prevent.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// csrfileMatrixPath is where every matrix arm publishes. It is a subdirectory
// key so the publish exercises the parent-dir fsync step, matching
// [csrFilePath].
const csrfileMatrixPath = "bulk/matrix.csr"

// csrfileAccessPatterns is the full set of [csrfile.AccessPattern] values the
// package declares. The matrix enumerates it rather than sampling it: a
// seed-drawn pattern would leave the run's coverage at the mercy of the draw,
// and the non-vacuity gate would then be asserting something the seed, not the
// code, decides.
//
// The audit that opened this task described three patterns (default /
// sequential / random). There are FIVE; AccessWillNeed and AccessDontNeed are
// the two the description omitted, and AccessDontNeed is the one no test in the
// repository reached before this task.
func csrfileAccessPatterns() []csrfile.AccessPattern {
	return []csrfile.AccessPattern{
		csrfile.AccessDefault,
		csrfile.AccessSequential,
		csrfile.AccessRandom,
		csrfile.AccessWillNeed,
		csrfile.AccessDontNeed,
	}
}

// csrfileAccessPatternName renders an access pattern for a message.
func csrfileAccessPatternName(p csrfile.AccessPattern) string {
	switch p {
	case csrfile.AccessDefault:
		return "default"
	case csrfile.AccessSequential:
		return "sequential"
	case csrfile.AccessRandom:
		return "random"
	case csrfile.AccessWillNeed:
		return "will-need"
	case csrfile.AccessDontNeed:
		return "dont-need"
	}
	return fmt.Sprintf("pattern(%d)", uint8(p))
}

// csrfileWeightKindName renders a weight kind for a message.
func csrfileWeightKindName(k csrfile.WeightKind) string {
	switch k {
	case csrfile.WeightAbsent:
		return "absent"
	case csrfile.WeightUint32:
		return "uint32"
	case csrfile.WeightUint64:
		return "uint64"
	case csrfile.WeightFloat32:
		return "float32"
	case csrfile.WeightFloat64:
		return "float64"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// -----------------------------------------------------------------------------
// The fixture shape
// -----------------------------------------------------------------------------

// csrfileShape is one seed-derived CSR topology, shared by every weight arm so
// the published files differ ONLY in their weights section. That is what makes
// the size comparison in the verdict meaningful: an unweighted file being
// smaller than a weighted one is evidence only when the graph is the same.
type csrfileShape struct {
	// vertices is the sentinel-inclusive offsets array (len == order+1).
	vertices []uint64
	// edges holds every edge target, grouped by source.
	edges []graph.NodeID
	// mags carries one seed-derived magnitude per edge; each weight arm maps it
	// into its own Go weight type deterministically.
	mags []uint64
	// order is the number of real vertices.
	order uint64
}

// csrfileShapeMagnitude bounds the per-edge magnitude. It is small enough that
// every weight type in the matrix — including float32 — represents the derived
// value EXACTLY, so the round-trip assertion is exact equality rather than a
// tolerance. A tolerance would quietly accept a codec that lost the low bits.
const csrfileShapeMagnitude = 1000

// buildCSRFileShape derives a small directed topology from s. Every target is
// in range and the offsets are monotone by construction, so the CSR is valid.
// At least one edge is guaranteed: a shape with no edges would give every arm
// an empty weights section and make the whole weight dimension vacuous.
func buildCSRFileShape(s *Seed) *csrfileShape {
	order := 8 + s.IntN(24) // 8..31 vertices
	sh := &csrfileShape{
		vertices: make([]uint64, order+1),
		order:    uint64(order),
	}
	for src := 0; src < order; src++ {
		deg := s.IntN(4) // 0..3 out-edges
		for j := 0; j < deg; j++ {
			sh.edges = append(sh.edges, graph.NodeID(s.Uint64N(uint64(order))))
			sh.mags = append(sh.mags, s.Uint64N(csrfileShapeMagnitude))
		}
		sh.vertices[src+1] = uint64(len(sh.edges))
	}
	if len(sh.edges) == 0 {
		// Force a self-edge on vertex 0 so the weights dimension is never empty.
		sh.edges = append(sh.edges, 0)
		sh.mags = append(sh.mags, s.Uint64N(csrfileShapeMagnitude))
		for i := 1; i <= order; i++ {
			sh.vertices[i] = 1
		}
	}
	return sh
}

// -----------------------------------------------------------------------------
// The weight arms
// -----------------------------------------------------------------------------

// csrfileWeightArm is one (Go weight type -> [csrfile.WeightKind]) arm of the
// matrix. The interface exists because [csrfile.WriteToFileWith] is generic
// over the Go weight type while the matrix must hold arms of different types in
// one table; each arm is a [csrfileArmOf] instantiated at its own W.
type csrfileWeightArm interface {
	// label names the arm in evidence and messages.
	label() string
	// wantKind is the [csrfile.WeightKind] this arm's Go type must select.
	wantKind() csrfile.WeightKind
	// publish writes the shape through the atomic csrfile publish path.
	publish(fsys simCSRFS, path string, sh *csrfileShape) error
	// verify opens the published file, applies pattern, and adjudicates the
	// round-trip. It returns one evidence cell and any verdict violations.
	verify(fsys simCSRFS, path string, sh *csrfileShape, pattern csrfile.AccessPattern) (csrfileCell, []Violation)
}

// csrfileArmOf implements [csrfileWeightArm] for one Go weight type W.
//
// W is constrained to comparable rather than any because the round-trip verdict
// compares the recovered weights against the written ones by VALUE. That is the
// whole assertion: a weight that came back as a different value is exactly the
// defect rmp #2526 was about, and an interface that could not compare would
// reduce the check to a length test.
type csrfileArmOf[W comparable] struct {
	lbl  string
	kind csrfile.WeightKind
	// weightAt maps an edge magnitude to this arm's weight value. It is nil for
	// the unweighted arm, whose CSR carries no weights slice at all.
	weightAt func(mag uint64) W
	// decodeRaw decodes the reader's RAW weights bytes independently of the
	// package's typed accessors. It is the second decoder: a typed accessor and
	// the writer that fed it can agree with each other and still both be wrong,
	// so the round-trip is adjudicated against a decode this file owns.
	decodeRaw func(raw []byte) []W
	// typed is the package's typed accessor for this kind, or nil when the
	// package exposes none (uint32 and float32 have raw bytes only).
	typed func(r *csrfile.Reader) ([]W, bool)
}

func (a csrfileArmOf[W]) label() string                { return a.lbl }
func (a csrfileArmOf[W]) wantKind() csrfile.WeightKind { return a.kind }

// weights materialises this arm's weights slice for sh, or nil when the arm is
// unweighted.
func (a csrfileArmOf[W]) weights(sh *csrfileShape) []W {
	if a.weightAt == nil {
		return nil
	}
	out := make([]W, len(sh.mags))
	for i, m := range sh.mags {
		out[i] = a.weightAt(m)
	}
	return out
}

// publish writes sh through [csrfile.WriteToFileWith] at this arm's weight type.
func (a csrfileArmOf[W]) publish(fsys simCSRFS, path string, sh *csrfileShape) error {
	c := csr.FromArrays[W](sh.vertices, sh.edges, a.weights(sh), sh.order, uint64(len(sh.edges)))
	if err := c.Validate(); err != nil {
		return fmt.Errorf("csr.Validate: %w", err)
	}
	if _, err := csrfile.WriteToFileWith[W](fsys, path, c); err != nil {
		return err
	}
	return nil
}

// csrfileWeightArms is the arm table: one arm per [csrfile.WeightKind].
//
// The Go types are chosen to be the ones the writer actually accepts.
// [csrfile.WriteToFile]'s own weightKindOf ALSO advertises int, uint and
// uintptr as WeightUint64, but binary.Write refuses those ("some values are not
// fixed-sized in type []int"), so a publish at those types always fails. That
// mismatch is rmp #2529 and is out of scope here; this table stays on the four
// widths that round-trip, plus struct{} for the unweighted arm.
func csrfileWeightArms() []csrfileWeightArm {
	return []csrfileWeightArm{
		csrfileArmOf[struct{}]{
			lbl: "absent", kind: csrfile.WeightAbsent,
			weightAt:  nil,
			decodeRaw: func([]byte) []struct{} { return nil },
		},
		csrfileArmOf[uint32]{
			lbl: "uint32", kind: csrfile.WeightUint32,
			weightAt:  func(m uint64) uint32 { return uint32(m) }, // m < csrfileShapeMagnitude
			decodeRaw: decodeLEUint32,
		},
		csrfileArmOf[uint64]{
			lbl: "uint64", kind: csrfile.WeightUint64,
			weightAt:  func(m uint64) uint64 { return m },
			decodeRaw: decodeLEUint64,
			typed:     func(r *csrfile.Reader) ([]uint64, bool) { return r.WeightsUint64() },
		},
		csrfileArmOf[float32]{
			lbl: "float32", kind: csrfile.WeightFloat32,
			weightAt:  func(m uint64) float32 { return float32(m) + 0.5 },
			decodeRaw: decodeLEFloat32,
		},
		csrfileArmOf[float64]{
			lbl: "float64", kind: csrfile.WeightFloat64,
			weightAt:  func(m uint64) float64 { return float64(m) + 0.25 },
			decodeRaw: decodeLEFloat64,
			typed:     func(r *csrfile.Reader) ([]float64, bool) { return r.WeightsFloat64() },
		},
	}
}

// decodeLEUint32 decodes a little-endian uint32 weights section.
func decodeLEUint32(raw []byte) []uint32 {
	out := make([]uint32, 0, len(raw)/4)
	for i := 0; i+4 <= len(raw); i += 4 {
		out = append(out, binary.LittleEndian.Uint32(raw[i:]))
	}
	return out
}

// decodeLEUint64 decodes a little-endian uint64 weights section.
func decodeLEUint64(raw []byte) []uint64 {
	out := make([]uint64, 0, len(raw)/8)
	for i := 0; i+8 <= len(raw); i += 8 {
		out = append(out, binary.LittleEndian.Uint64(raw[i:]))
	}
	return out
}

// decodeLEFloat32 decodes a little-endian float32 weights section.
func decodeLEFloat32(raw []byte) []float32 {
	out := make([]float32, 0, len(raw)/4)
	for i := 0; i+4 <= len(raw); i += 4 {
		out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(raw[i:])))
	}
	return out
}

// decodeLEFloat64 decodes a little-endian float64 weights section.
func decodeLEFloat64(raw []byte) []float64 {
	out := make([]float64, 0, len(raw)/8)
	for i := 0; i+8 <= len(raw); i += 8 {
		out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(raw[i:])))
	}
	return out
}

// -----------------------------------------------------------------------------
// One cell of the grid
// -----------------------------------------------------------------------------

// csrfileCell records what one (weight kind, access pattern) combination
// actually did. Every field is an OBSERVATION, never a restatement of the loop
// variable that selected the combination: observedKind is read back out of the
// published header, and hintApplied is set only after [csrfile.Reader.SetHint]
// returned nil on a live reader. The non-vacuity gate reads these, so it
// adjudicates what the run reached rather than what it intended to reach.
type csrfileCell struct {
	arm          string
	wantKind     csrfile.WeightKind
	observedKind csrfile.WeightKind
	pattern      csrfile.AccessPattern
	// fileBytes is the published file's size on the SimDisk.
	fileBytes int
	// weightBytes is len(Reader.WeightsRaw()) — zero for an unweighted file.
	weightBytes int
	// hintApplied is true when SetHint returned nil on a live reader.
	hintApplied bool
	// hintErr is what SetHint returned on the live reader.
	hintErr error
	// closedHintErr / closedReadErr are what SetHint and Read returned AFTER
	// Close. They are the discriminating half of the hint contract: without
	// them, "SetHint returned nil" would be satisfied by a SetHint that ignored
	// its receiver entirely.
	closedHintErr error
	closedReadErr error
	// stableAcrossHint is true when every byte read after the hint equalled the
	// bytes read before it.
	stableAcrossHint bool
	// roundTripped is true when vertices, edges and weights all matched the
	// source shape after the hint.
	roundTripped bool
}

// summary renders the cell as one witness line.
func (c *csrfileCell) summary() string {
	return fmt.Sprintf(
		"csrfile[%-7s x %-10s] kind=%s bytes=%d weights=%dB hint=%v stable=%t roundtrip=%t closedHint=%v",
		c.arm, csrfileAccessPatternName(c.pattern), csrfileWeightKindName(c.observedKind),
		c.fileBytes, c.weightBytes, c.hintErr, c.stableAcrossHint, c.roundTripped, c.closedHintErr)
}

// verify opens the published file, reads it, applies the access hint, reads it
// again, and adjudicates. The two reads go through [csrfile.Reader.Read], the
// documented safe accessor, so the hint is applied against a live mapping held
// by the reader's own lock rather than against bare slices.
func (a csrfileArmOf[W]) verify(
	fsys simCSRFS, path string, sh *csrfileShape, pattern csrfile.AccessPattern,
) (csrfileCell, []Violation) {
	cell := csrfileCell{arm: a.lbl, wantKind: a.kind, pattern: pattern}
	var v []Violation
	add := func(kind ViolationKind, op, format string, args ...any) {
		v = append(v, Violation{Kind: kind, Op: op, Message: fmt.Sprintf(format, args...)})
	}

	if img, err := fsys.ReadFile(path); err == nil {
		cell.fileBytes = len(img)
	}

	r, err := csrfile.OpenWith(fsys, path)
	if err != nil {
		add(ViolationACIDConsistency, "<csrfile-matrix-open>",
			"[%s x %s] the reader rejected a cleanly published csrfile: %v",
			a.lbl, csrfileAccessPatternName(pattern), err)
		return cell, v
	}
	cell.observedKind = r.Header().Weight

	// Baseline read, BEFORE the hint.
	var beforeV []uint64
	var beforeE []graph.NodeID
	var beforeW []byte
	if err := r.Read(func(vs []uint64, es []graph.NodeID, ws []byte) error {
		beforeV = slices.Clone(vs)
		beforeE = slices.Clone(es)
		beforeW = slices.Clone(ws)
		return nil
	}); err != nil {
		add(ViolationACIDConsistency, "<csrfile-matrix-read>",
			"[%s x %s] Read on a live reader failed: %v", a.lbl, csrfileAccessPatternName(pattern), err)
		_ = r.Close()
		return cell, v
	}
	cell.weightBytes = len(beforeW)

	// The hint itself. An advisory hint on a live reader must be accepted.
	cell.hintErr = r.SetHint(pattern)
	if cell.hintErr == nil {
		cell.hintApplied = true
	} else {
		add(ViolationACIDConsistency, "<csrfile-matrix-hint>",
			"[%s x %s] SetHint on a live reader returned %v, want nil",
			a.lbl, csrfileAccessPatternName(pattern), cell.hintErr)
	}

	// Second read, AFTER the hint. An advisory hint is advice about paging; it
	// must not change one byte of what the mapping yields. On a real mapping
	// AccessDontNeed genuinely drops the pages, so this is the assertion that
	// the data comes back identical when they are faulted in again.
	if err := r.Read(func(vs []uint64, es []graph.NodeID, ws []byte) error {
		if !slices.Equal(vs, beforeV) || !slices.Equal(es, beforeE) || !slices.Equal(ws, beforeW) {
			add(ViolationACIDConsistency, "<csrfile-matrix-hint-mutated>",
				"[%s x %s] the data changed across SetHint (vertices %d->%d, edges %d->%d, weights %dB->%dB):"+
					" an advisory access hint altered what the mapping yields",
				a.lbl, csrfileAccessPatternName(pattern),
				len(beforeV), len(vs), len(beforeE), len(es), len(beforeW), len(ws))
			return nil
		}
		cell.stableAcrossHint = true
		v = append(v, a.checkRoundTrip(r, sh, pattern, vs, es, ws, &cell)...)
		return nil
	}); err != nil {
		add(ViolationACIDConsistency, "<csrfile-matrix-read>",
			"[%s x %s] Read after SetHint failed: %v", a.lbl, csrfileAccessPatternName(pattern), err)
	}

	// The closed-reader half of the contract.
	if err := r.Close(); err != nil {
		add(ViolationACIDConsistency, "<csrfile-matrix-close>",
			"[%s x %s] Close returned %v", a.lbl, csrfileAccessPatternName(pattern), err)
	}
	cell.closedHintErr = r.SetHint(pattern)
	cell.closedReadErr = r.Read(func([]uint64, []graph.NodeID, []byte) error { return nil })
	if !errors.Is(cell.closedHintErr, csrfile.ErrReaderClosed) {
		add(ViolationACIDConsistency, "<csrfile-matrix-hint-after-close>",
			"[%s x %s] SetHint after Close returned %v, want ErrReaderClosed: a hint that is accepted"+
				" on a released mapping is a hint that never looked at the mapping",
			a.lbl, csrfileAccessPatternName(pattern), cell.closedHintErr)
	}
	if !errors.Is(cell.closedReadErr, csrfile.ErrReaderClosed) {
		add(ViolationACIDConsistency, "<csrfile-matrix-read-after-close>",
			"[%s x %s] Read after Close returned %v, want ErrReaderClosed",
			a.lbl, csrfileAccessPatternName(pattern), cell.closedReadErr)
	}
	return cell, v
}

// checkRoundTrip adjudicates the payload the reader yielded against the source
// shape, and the weights section against BOTH an independent little-endian
// decode and the package's typed accessors.
func (a csrfileArmOf[W]) checkRoundTrip(
	r *csrfile.Reader, sh *csrfileShape, pattern csrfile.AccessPattern,
	vs []uint64, es []graph.NodeID, ws []byte, cell *csrfileCell,
) []Violation {
	var v []Violation
	tag := fmt.Sprintf("[%s x %s]", a.lbl, csrfileAccessPatternName(pattern))
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: tag + " " + fmt.Sprintf(format, args...),
		})
	}

	// The header must carry the kind this arm's Go type selects. A drift here is
	// the #2526 failure shape: a writer that silently records a weighted graph
	// as something else.
	if cell.observedKind != a.kind {
		add("<csrfile-matrix-kind>",
			"the published header carries weight kind %s, want %s: the writer did not select"+
				" the kind this Go weight type maps to",
			csrfileWeightKindName(cell.observedKind), csrfileWeightKindName(a.kind))
	}

	ok := true
	if !slices.Equal(vs, sh.vertices) {
		add("<csrfile-matrix-vertices>", "the vertex offsets did not round-trip (%d entries, want %d)",
			len(vs), len(sh.vertices))
		ok = false
	}
	if !slices.Equal(es, sh.edges) {
		add("<csrfile-matrix-edges>", "the edge targets did not round-trip (%d entries, want %d)",
			len(es), len(sh.edges))
		ok = false
	}

	// The typed accessors must gate on the kind, not merely on the bytes being
	// present: WeightsUint64 must answer for a uint64 file and REFUSE every
	// other, and likewise WeightsFloat64. Asserting only the positive direction
	// would pass on an accessor that reinterpreted any 8-byte section.
	_, gotU64 := r.WeightsUint64()
	_, gotF64 := r.WeightsFloat64()
	wantU64 := a.kind == csrfile.WeightUint64 && len(sh.edges) > 0
	wantF64 := a.kind == csrfile.WeightFloat64 && len(sh.edges) > 0
	if gotU64 != wantU64 {
		add("<csrfile-matrix-accessor>", "WeightsUint64 answered %t, want %t for a %s file",
			gotU64, wantU64, csrfileWeightKindName(a.kind))
		ok = false
	}
	if gotF64 != wantF64 {
		add("<csrfile-matrix-accessor>", "WeightsFloat64 answered %t, want %t for a %s file",
			gotF64, wantF64, csrfileWeightKindName(a.kind))
		ok = false
	}

	if a.kind == csrfile.WeightAbsent {
		// The unweighted arm: the absence must be visible in the substrate, not
		// only in the header byte. WeightsRaw is the substrate.
		if ws != nil {
			add("<csrfile-matrix-absent>",
				"an unweighted file yielded a %d-byte weights section, want none", len(ws))
			ok = false
		}
		cell.roundTripped = ok
		return v
	}

	wantW := a.weights(sh)
	wantBytes := a.kind.Size() * len(sh.edges)
	if len(ws) != wantBytes {
		add("<csrfile-matrix-weights-size>",
			"the weights section is %d bytes, want %d (%d edges x %d)",
			len(ws), wantBytes, len(sh.edges), a.kind.Size())
		ok = false
	}
	if gotW := a.decodeRaw(ws); !slices.Equal(gotW, wantW) {
		add("<csrfile-matrix-weights>",
			"the weights did not round-trip through an independent little-endian decode"+
				" (%d decoded, %d written)", len(gotW), len(wantW))
		ok = false
	}
	if a.typed != nil {
		if gotW, okTyped := a.typed(r); !okTyped || !slices.Equal(gotW, wantW) {
			add("<csrfile-matrix-weights-typed>",
				"the package's typed weights accessor answered ok=%t with %d values;"+
					" it disagrees with the raw section this file decoded independently",
				okTyped, len(gotW))
			ok = false
		}
	}
	cell.roundTripped = ok
	return v
}

// -----------------------------------------------------------------------------
// Truncation and the alignment contract
// -----------------------------------------------------------------------------

// csrfileTruncCell records one truncation of a published csrfile and what the
// reader — and [csrfile.Reinterpret] over the same shortened image — did with
// it.
type csrfileTruncCell struct {
	arm string
	// label names the cut ("aligned", "misaligned", "below-header",
	// "header-boundary").
	label string
	// cut is the byte length the file was truncated to.
	cut int64
	// aligned reports whether cut is a multiple of [csrfile.Alignment].
	aligned bool
	// origLen / gotLen are the file's length before and after the truncation,
	// read back from the disk. gotLen < origLen is what proves the truncation
	// happened at all.
	origLen int
	gotLen  int
	// openErr is what Open returned over the truncated image.
	openErr error
	// sentinel names which typed sentinel openErr matched.
	sentinel string
	// reinterpretPanic is the message [csrfile.Reinterpret] panicked with when
	// asked for the vertices section of the truncated image, or "" if it
	// returned a view.
	reinterpretPanic string
}

// summary renders the truncation cell as one witness line.
func (c *csrfileTruncCell) summary() string {
	return fmt.Sprintf("csrfile-trunc[%-7s %-16s] cut=%d aligned=%t len=%d->%d sentinel=%s reinterpret=%q",
		c.arm, c.label, c.cut, c.aligned, c.origLen, c.gotLen, c.sentinel, c.reinterpretPanic)
}

// csrfileSentinelName classifies an Open error against the package's typed
// sentinels. It reports the MOST specific match, and "untyped" when the error
// matches none — which is itself the interesting answer for a zero-length file
// opened through the real mmap path.
func csrfileSentinelName(err error) string {
	switch {
	case err == nil:
		return "<none>"
	case errors.Is(err, csrfile.ErrHeaderTooShort):
		return "ErrHeaderTooShort"
	case errors.Is(err, csrfile.ErrHeaderInconsistent):
		return "ErrHeaderInconsistent"
	case errors.Is(err, csrfile.ErrFileCorrupted):
		return "ErrFileCorrupted"
	case errors.Is(err, csrfile.ErrBadMagic):
		return "ErrBadMagic"
	case errors.Is(err, csrfile.ErrUnsupportedVersion):
		return "ErrUnsupportedVersion"
	case errors.Is(err, csrfile.ErrUnknownWeightKind):
		return "ErrUnknownWeightKind"
	}
	return "untyped"
}

// csrfileTruncationCuts derives the four cuts for a header, in bytes.
//
// The two that carry the task's question are "aligned" and "misaligned", and
// both land strictly INSIDE the vertices section, so each genuinely removes
// payload the reader would otherwise reinterpret:
//
//   - aligned      = VerticesOffset + Alignment, a multiple of Alignment;
//   - misaligned   = the last byte but three of the vertices section, which is
//     congruent to 5 mod 8 and therefore a multiple of neither Alignment nor 8.
//
// The other two bracket the ONE length threshold that changes the outcome:
// HeaderSize+4 is the shortest image [csrfile.Open] will look at, so a cut one
// byte below it is refused by the length gate and a cut exactly on it reaches
// the structural gate instead.
func csrfileTruncationCuts(h csrfile.Header) []struct {
	label string
	cut   int64
} {
	verticesEnd := h.VerticesOffset + 8*h.NVertices
	return []struct {
		label string
		cut   int64
	}{
		{"aligned", int64(h.VerticesOffset + csrfile.Alignment)}, // header validated by Open
		{"misaligned", int64(verticesEnd - 3)},                   // verticesEnd >= 136 for this fixture
		{"below-header", csrfile.HeaderSize + 3},
		{"header-boundary", csrfile.HeaderSize + 4},
	}
}

// csrfileTruncationProbe publishes arm's fixture on a FRESH disk, truncates the
// published file to cut, and adjudicates both the reader and
// [csrfile.Reinterpret] over the shortened image.
//
// The verdict it enforces is that a short file is REFUSED with a typed error
// and that Reinterpret refuses to build a view rather than aliasing memory it
// cannot justify. What it deliberately does NOT assume is that an aligned cut
// and a misaligned cut behave differently: they do not, and the run records the
// sentinel each produced so the equality is evidence rather than an assumption.
func csrfileTruncationProbe(
	arm csrfileWeightArm, sh *csrfileShape, cutLabel string, cut int64, h csrfile.Header,
) (csrfileTruncCell, []Violation, error) {
	cell := csrfileTruncCell{
		arm: arm.label(), label: cutLabel, cut: cut,
		aligned: cut%int64(csrfile.Alignment) == 0,
	}
	disk := NewSimDisk(NewSeed(uint64(cut)+0x7C5F), 0) // cut is a small positive length
	fsys := simCSRFS{disk: disk}
	if err := arm.publish(fsys, csrfileMatrixPath, sh); err != nil {
		return cell, nil, fmt.Errorf("publish %s: %w", arm.label(), err)
	}
	img, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		return cell, nil, fmt.Errorf("read published %s: %w", arm.label(), err)
	}
	cell.origLen = len(img)
	if err := disk.TruncatePath(csrfileMatrixPath, cut); err != nil {
		return cell, nil, fmt.Errorf("truncate %s to %d: %w", arm.label(), cut, err)
	}
	short, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		return cell, nil, fmt.Errorf("read truncated %s: %w", arm.label(), err)
	}
	cell.gotLen = len(short)

	r, openErr := csrfile.OpenWith(fsys, csrfileMatrixPath)
	if openErr == nil {
		_ = r.Close()
	}
	cell.openErr = openErr
	cell.sentinel = csrfileSentinelName(openErr)

	// Reinterpret the vertices section of the SHORTENED image. The buffer is
	// copied to an 8-byte-aligned base first, so the only remaining reason to
	// refuse is the one under test: not enough bytes.
	aligned, _ := csrfileAlignedCopy(short)
	cell.reinterpretPanic = csrfileRecover(func() {
		start := min(int(h.VerticesOffset), len(aligned))
		_ = csrfile.Reinterpret[uint64](aligned[start:], int(h.NVertices)) // NVertices <= 32 here
	})

	var v []Violation
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: op,
			Message: fmt.Sprintf("[%s %s cut=%d] ", arm.label(), cutLabel, cut) + fmt.Sprintf(format, args...),
		})
	}
	if openErr == nil {
		add("<csrfile-trunc-accepted>",
			"the reader ACCEPTED a truncated csrfile (%d of %d bytes) and returned a usable view",
			cell.gotLen, cell.origLen)
	}
	// Which sentinel is correct is a function of the length threshold, not of
	// the alignment: below HeaderSize+4 the length gate fires first, at or above
	// it the structural gate does.
	wantSentinel := "ErrHeaderInconsistent"
	if cut < csrfile.HeaderSize+4 {
		wantSentinel = "ErrHeaderTooShort"
	}
	if cell.sentinel != wantSentinel {
		add("<csrfile-trunc-sentinel>",
			"a truncated csrfile was refused with %s (%v), want %s",
			cell.sentinel, openErr, wantSentinel)
	}
	// ErrHeaderInconsistent is documented to wrap ErrFileCorrupted so that
	// callers already testing for corruption keep working; pin that.
	if wantSentinel == "ErrHeaderInconsistent" && !errors.Is(openErr, csrfile.ErrFileCorrupted) {
		add("<csrfile-trunc-sentinel>",
			"the structural refusal %v does not wrap ErrFileCorrupted", openErr)
	}
	if cell.reinterpretPanic == "" {
		add("<csrfile-trunc-reinterpret>",
			"Reinterpret returned a %d-element uint64 view over a %d-byte truncated image"+
				" instead of refusing: that view aliases memory the file no longer holds",
			h.NVertices, cell.gotLen)
	}
	return cell, v, nil
}

// -----------------------------------------------------------------------------
// The Reinterpret alignment sweep
// -----------------------------------------------------------------------------

// csrfileAlignSweep records [csrfile.Reinterpret]'s behaviour across the eight
// possible byte phases of a buffer. Exactly one of any eight consecutive byte
// offsets is 8-byte aligned, so a correct alignment gate accepts exactly one
// and refuses seven — which is what makes this sweep a real probe of the gate
// rather than a single lucky offset.
//
// This is also the ONLY way truncation and alignment meet in this file, and the
// answer is that they do not: Reinterpret's alignment precondition is on the
// BASE ADDRESS of the buffer, which truncating a file cannot change. Truncation
// can only ever trip the length half of the precondition.
type csrfileAlignSweep struct {
	// accepted / refused hold the byte phases Reinterpret accepted and refused.
	accepted []int
	refused  []int
	// refusalMsg is the message from the first refused phase.
	refusalMsg string
}

// csrfileRecover runs fn and returns the message it panicked with, or "" when
// it returned normally. [csrfile.Reinterpret] refuses by PANIC — it is
// documented to panic on a short buffer or an incompatible alignment, not to
// return an error — so recovering is the only way to adjudicate its refusal.
func csrfileRecover(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	fn()
	return ""
}

// csrfileAlignmentSweep probes all eight byte phases of a fresh buffer.
func csrfileAlignmentSweep() csrfileAlignSweep {
	var sw csrfileAlignSweep
	buf := make([]byte, 64+16)
	for k := 0; k < 8; k++ {
		// buf[k:] always holds at least 16 bytes, so a refusal at n=1 can only
		// be about alignment, never about length.
		msg := csrfileRecover(func() { _ = csrfile.Reinterpret[uint64](buf[k:], 1) })
		if msg == "" {
			sw.accepted = append(sw.accepted, k)
			continue
		}
		sw.refused = append(sw.refused, k)
		if sw.refusalMsg == "" {
			sw.refusalMsg = msg
		}
	}
	return sw
}

// csrfileAlignedCopy returns a copy of img whose base address is 8-byte
// aligned, together with the phase it used.
//
// It discovers the allocation's phase with [csrfile.Reinterpret] itself rather
// than importing unsafe here: of the eight offsets into an over-allocated
// buffer, exactly one has an 8-aligned base, and Reinterpret is precisely the
// oracle that says which. When no phase is accepted — which would mean the
// alignment gate is broken — it returns the buffer unshifted, and the sweep's
// own verdict is what reports that.
func csrfileAlignedCopy(img []byte) ([]byte, int) {
	buf := make([]byte, len(img)+16)
	for k := 0; k < 8; k++ {
		if csrfileRecover(func() { _ = csrfile.Reinterpret[uint64](buf[k:], 1) }) != "" {
			continue
		}
		copy(buf[k:], img)
		return buf[k : k+len(img)], k
	}
	copy(buf, img)
	return buf[:len(img)], 0
}

// checkCSRFileAlignSweep is the verdict on the alignment gate.
func checkCSRFileAlignSweep(sw csrfileAlignSweep) []Violation {
	var v []Violation
	add := func(format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<csrfile-reinterpret-align>",
			Message: fmt.Sprintf(format, args...),
		})
	}
	if len(sw.accepted) != 1 {
		add("Reinterpret accepted %d of the 8 byte phases (%v), want exactly 1:"+
			" only one phase in eight has an 8-byte-aligned base, so any other count means"+
			" the alignment precondition is not being enforced", len(sw.accepted), sw.accepted)
	}
	if len(sw.refused) != 7 {
		add("Reinterpret refused %d of the 8 byte phases (%v), want exactly 7",
			len(sw.refused), sw.refused)
	}
	if len(sw.refused) > 0 && !strings.Contains(sw.refusalMsg, "not aligned") {
		add("Reinterpret refused a misaligned base with %q, which does not name alignment:"+
			" the refusal may be coming from the length check instead", sw.refusalMsg)
	}
	return v
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// csrfileMatrixResult is one full matrix run: the evidence, and the two gates
// kept apart. A verdict violation is a defect in csrfile; a vacuity violation
// is a run that exercised too little for the verdict to have meant anything.
type csrfileMatrixResult struct {
	shape      *csrfileShape
	cells      []csrfileCell
	truncs     []csrfileTruncCell
	alignSweep csrfileAlignSweep
	// armBytes maps an arm label to the published file size for the shared
	// shape, so the unweighted file can be compared against every weighted one.
	armBytes map[string]int
	verdict  []Violation
	vacuity  []Violation
}

// runCSRFileAccessMatrix drives the whole grid for one seed: every weight kind
// against every access pattern, then the truncation battery and the alignment
// sweep.
//
// The grid is ENUMERATED, not sampled — every combination is exercised on every
// seed — while the seed derives the topology, the weight magnitudes and the
// order in which the cells are visited. Sampling the grid instead would make
// the coverage a property of the draw, and the non-vacuity gate below would
// then be adjudicating the seed rather than the code.
func runCSRFileAccessMatrix(seed uint64) (csrfileMatrixResult, error) {
	s := NewSeed(seed)
	arms := csrfileWeightArms()
	patterns := csrfileAccessPatterns()
	res := csrfileMatrixResult{
		shape:      buildCSRFileShape(s),
		armBytes:   make(map[string]int, len(arms)),
		alignSweep: csrfileAlignmentSweep(),
	}
	res.verdict = append(res.verdict, checkCSRFileAlignSweep(res.alignSweep)...)

	// Visit the grid in a seed-derived order. The order cannot change any
	// verdict — each cell publishes onto a fresh disk — but it keeps the run
	// from depending on a fixed traversal should a future cell acquire state.
	type coord struct {
		arm     int
		pattern int
	}
	grid := make([]coord, 0, len(arms)*len(patterns))
	for i := range arms {
		for j := range patterns {
			grid = append(grid, coord{arm: i, pattern: j})
		}
	}
	for i := len(grid) - 1; i > 0; i-- {
		j := s.IntN(i + 1)
		grid[i], grid[j] = grid[j], grid[i]
	}

	res.cells = make([]csrfileCell, 0, len(grid))
	for _, g := range grid {
		arm := arms[g.arm]
		disk := NewSimDisk(NewSeed(seed^uint64(g.arm*17+g.pattern)), 0) // small non-negative index
		fsys := simCSRFS{disk: disk}
		if err := arm.publish(fsys, csrfileMatrixPath, res.shape); err != nil {
			return res, fmt.Errorf("sim: csrfile matrix publish %s: %w", arm.label(), err)
		}
		cell, v := arm.verify(fsys, csrfileMatrixPath, res.shape, patterns[g.pattern])
		res.cells = append(res.cells, cell)
		res.verdict = append(res.verdict, v...)
		if cell.fileBytes > 0 {
			res.armBytes[arm.label()] = cell.fileBytes
		}
	}

	res.verdict = append(res.verdict, checkCSRFileWeightDiscrimination(res.armBytes, arms)...)

	// The truncation battery: every arm, every cut. The header comes from a
	// clean publish of the same shape, so the cuts are derived from the real
	// layout rather than from constants that could drift away from it.
	for _, arm := range arms {
		armHeader, armTotal := csrfile.Layout(uint64(len(res.shape.vertices)), uint64(len(res.shape.edges)), arm.wantKind())
		if armTotal == 0 {
			return res, fmt.Errorf("sim: csrfile matrix: Layout rejected %s", arm.label())
		}
		for _, c := range csrfileTruncationCuts(armHeader) {
			cell, v, err := csrfileTruncationProbe(arm, res.shape, c.label, c.cut, armHeader)
			if err != nil {
				return res, fmt.Errorf("sim: csrfile matrix truncation: %w", err)
			}
			res.truncs = append(res.truncs, cell)
			res.verdict = append(res.verdict, v...)
		}
	}

	res.vacuity = checkCSRFileMatrixNonVacuity(&res)
	return res, nil
}

// checkCSRFileWeightDiscrimination is the verdict that [csrfile.WeightAbsent]
// is DISTINGUISHABLE from a weighted file in the substrate, not merely in the
// header byte.
//
// Same topology, same vertex and edge sections: the unweighted file must be
// strictly smaller than every weighted one, because a weighted layout adds an
// aligned weights section of at least one alignment unit. A writer that
// recorded a weighted graph as unweighted — the rmp #2526 shape — would produce
// a file of exactly the unweighted size, and this is what would catch it even
// if the header byte were somehow right.
func checkCSRFileWeightDiscrimination(armBytes map[string]int, arms []csrfileWeightArm) []Violation {
	var v []Violation
	absent, ok := armBytes["absent"]
	if !ok {
		return nil // the non-vacuity gate owns "the unweighted arm never ran".
	}
	for _, arm := range arms {
		if arm.wantKind() == csrfile.WeightAbsent {
			continue
		}
		weighted, ok := armBytes[arm.label()]
		if !ok {
			continue
		}
		if weighted <= absent {
			v = append(v, Violation{
				Kind: ViolationACIDConsistency, Op: "<csrfile-weight-indistinguishable>",
				Message: fmt.Sprintf(
					"a %s-weighted file is %d bytes and the unweighted file of the SAME topology is %d:"+
						" the weighted file is not larger, so its weights section is missing and"+
						" 'weighted' is indistinguishable from 'unweighted' on disk",
					arm.label(), weighted, absent),
			})
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// The NON-VACUITY gate — shape only
// -----------------------------------------------------------------------------

// checkCSRFileMatrixNonVacuity is the SEPARATE, shape-only gate. It never
// asserts that csrfile behaved correctly — the verdicts above own that — only
// that the run reached enough for those verdicts to have meant something.
//
// Every question it asks is answered from an OBSERVATION rather than from the
// loop that drove the run:
//
//   - which weight kinds were exercised is read from the published headers, so
//     a writer that silently downgraded a kind (as it does, by design, when a
//     weighted CSR carries an empty weights slice) shows up as a missing kind
//     rather than as coverage;
//   - which access patterns were exercised counts only cells whose SetHint
//     returned nil on a live reader, so a pattern that was selected but never
//     accepted is not counted as covered;
//   - whether the truncations truncated is measured on the disk image after the
//     cut, not assumed from having called Truncate.
func checkCSRFileMatrixNonVacuity(res *csrfileMatrixResult) []Violation {
	var v []Violation
	add := func(op, format string, args ...any) {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: op, Message: fmt.Sprintf(format, args...),
		})
	}

	// The fixture itself must be non-trivial: no edges would make every weights
	// section empty and the whole weight dimension vacuous.
	if len(res.shape.edges) == 0 {
		add("<csrfile-shape-degenerate>",
			"the fixture shape has no edges, so no weights section was ever written")
	}
	if res.shape.order < 8 {
		add("<csrfile-shape-degenerate>",
			"the fixture shape has only %d vertices", res.shape.order)
	}

	// Every weight kind must have been OBSERVED in a published header.
	observedKinds := make(map[csrfile.WeightKind]int)
	appliedPatterns := make(map[csrfile.AccessPattern]int)
	weightedCells, absentCells := 0, 0
	for _, c := range res.cells {
		observedKinds[c.observedKind]++
		if c.hintApplied {
			appliedPatterns[c.pattern]++
		}
		switch {
		case c.observedKind == csrfile.WeightAbsent && c.weightBytes == 0:
			absentCells++
		case c.observedKind != csrfile.WeightAbsent && c.weightBytes > 0:
			weightedCells++
		}
	}
	for _, arm := range csrfileWeightArms() {
		if observedKinds[arm.wantKind()] == 0 {
			add("<csrfile-kind-unreached>",
				"weight kind %s was never observed in a published header, so no verdict in this run"+
					" adjudicated it", csrfileWeightKindName(arm.wantKind()))
		}
	}
	for _, p := range csrfileAccessPatterns() {
		if appliedPatterns[p] == 0 {
			add("<csrfile-pattern-unreached>",
				"access pattern %s was never applied to a live reader, so the 'a hint does not change"+
					" the data' verdict never ran for it", csrfileAccessPatternName(p))
		}
	}
	if weightedCells == 0 {
		add("<csrfile-weights-unread>",
			"no cell read a non-empty weights section, so the weight round-trip verdict was vacuous")
	}
	if absentCells == 0 {
		add("<csrfile-absent-unreached>",
			"no cell produced an unweighted file, so nothing distinguished WeightAbsent from a"+
				" weighted file")
	}

	// The truncations must have truncated, and the aligned/misaligned pair must
	// really differ in alignment — otherwise comparing them proves nothing.
	alignedCuts, misalignedCuts := 0, 0
	for _, c := range res.truncs {
		if c.gotLen >= c.origLen {
			add("<csrfile-trunc-noop>",
				"the %s cut of the %s file left it at %d of %d bytes: nothing was truncated,"+
					" so the refusal it produced is not evidence about a short file",
				c.label, c.arm, c.gotLen, c.origLen)
			continue
		}
		if c.gotLen != int(c.cut) {
			add("<csrfile-trunc-noop>",
				"the %s cut of the %s file asked for %d bytes and left %d",
				c.label, c.arm, c.cut, c.gotLen)
		}
		switch c.label {
		case "aligned":
			if !c.aligned {
				add("<csrfile-trunc-not-aligned>",
					"the cut labelled aligned (%d) is not a multiple of Alignment=%d",
					c.cut, csrfile.Alignment)
				continue
			}
			alignedCuts++
		case "misaligned":
			if c.aligned || c.cut%8 == 0 {
				add("<csrfile-trunc-not-misaligned>",
					"the cut labelled misaligned (%d) is a multiple of %d: it does not land off an"+
						" alignment boundary, so it cannot discriminate", c.cut, gcdAlignmentHint(c.cut))
				continue
			}
			misalignedCuts++
		}
	}
	if alignedCuts == 0 {
		add("<csrfile-trunc-unreached>", "no aligned truncation ran")
	}
	if misalignedCuts == 0 {
		add("<csrfile-trunc-unreached>", "no misaligned truncation ran")
	}
	return v
}

// gcdAlignmentHint names which divisor a cut fell on, for the message above.
func gcdAlignmentHint(cut int64) int64 {
	if cut%int64(csrfile.Alignment) == 0 {
		return int64(csrfile.Alignment)
	}
	return 8
}
