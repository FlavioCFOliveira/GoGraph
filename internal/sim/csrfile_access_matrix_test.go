package sim

// csrfile_access_matrix_test.go — the tests for the csrfile WeightKind x
// AccessPattern matrix (rmp #2478).
//
// The two gates are reported SEPARATELY throughout, as the sprint's standing
// structure requires: a VERDICT violation is a defect in store/csrfile, a
// NON-VACUITY violation is a run that exercised too little for the verdict to
// have meant anything. Everything else the run learned is a WITNESS, logged
// rather than asserted a second time.
//
// One arm here deliberately leaves the deterministic in-memory disk: the
// madvise hint path is unreachable through the DST filesystem seam, because a
// byte-backed Reader has no mapping to advise and [csrfile.Reader.SetHint]
// returns nil before it would call madvise. TestCSRFileMatrix_MadviseOverReal
// Mapping is the only place in this package where the syscall actually runs.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// -----------------------------------------------------------------------------
// The grid
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_Grid drives every (weight kind, access pattern) combination
// across the storage-fault seeds and reports the two gates apart.
func TestCSRFileMatrix_Grid(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, seed := range storageFaultTestSeeds {
		res, err := runCSRFileAccessMatrix(seed)
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		for _, v := range res.verdict {
			t.Errorf("seed %#x VERDICT %s: %s: %s", seed, v.Kind, v.Op, v.Message)
		}
		for _, v := range res.vacuity {
			t.Errorf("seed %#x NON-VACUITY %s: %s", seed, v.Op, v.Message)
		}
	}

	// Witness for one seed: the full grid, the truncation battery, and the
	// alignment sweep, as observed rather than as intended.
	res, err := runCSRFileAccessMatrix(storageFaultTestSeeds[1])
	if err != nil {
		t.Fatalf("witness run: %v", err)
	}
	t.Logf("shape: %d vertices, %d edges", res.shape.order, len(res.shape.edges))
	for _, c := range res.cells {
		t.Logf("%s", c.summary())
	}
	for _, c := range res.truncs {
		t.Logf("%s", c.summary())
	}
	t.Logf("Reinterpret byte phases: accepted=%v refused=%v first refusal=%q",
		res.alignSweep.accepted, res.alignSweep.refused, res.alignSweep.refusalMsg)
	for _, arm := range csrfileWeightArms() {
		t.Logf("published size [%s] = %d bytes", arm.label(), res.armBytes[arm.label()])
	}
}

// TestCSRFileMatrix_Deterministic pins bit-reproducibility: the same seed twice
// yields the same evidence, so any failure the grid reports replays.
func TestCSRFileMatrix_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seed = 0x2478_0001
	a, err := runCSRFileAccessMatrix(seed)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	b, err := runCSRFileAccessMatrix(seed)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(a.cells) != len(b.cells) {
		t.Fatalf("cell count differs: %d vs %d", len(a.cells), len(b.cells))
	}
	for i := range a.cells {
		if a.cells[i].summary() != b.cells[i].summary() {
			t.Fatalf("cell %d differs:\n%s\n%s", i, a.cells[i].summary(), b.cells[i].summary())
		}
	}
	for i := range a.truncs {
		if a.truncs[i].summary() != b.truncs[i].summary() {
			t.Fatalf("truncation %d differs:\n%s\n%s", i, a.truncs[i].summary(), b.truncs[i].summary())
		}
	}
	if !slices.Equal(a.shape.edges, b.shape.edges) || !slices.Equal(a.shape.mags, b.shape.mags) {
		t.Fatal("the fixture shape is not reproducible from the seed")
	}
}

// TestCSRFileMatrix_GridIsComplete guards the grid itself. Every declared
// weight kind and every declared access pattern must appear exactly once per
// arm, and the arm table must map each Go weight type to a DISTINCT kind —
// which is what makes the kind verdict discriminating rather than a tautology
// over one kind repeated five times.
func TestCSRFileMatrix_GridIsComplete(t *testing.T) {
	arms := csrfileWeightArms()
	patterns := csrfileAccessPatterns()
	if len(patterns) != 5 {
		t.Fatalf("csrfileAccessPatterns() has %d entries, want 5 (default, sequential, random,"+
			" will-need, dont-need)", len(patterns))
	}
	// The five declared AccessPattern values, named from the package's own
	// constants so a new one added upstream shows up here as a gap.
	wantPatterns := []csrfile.AccessPattern{
		csrfile.AccessDefault, csrfile.AccessSequential, csrfile.AccessRandom,
		csrfile.AccessWillNeed, csrfile.AccessDontNeed,
	}
	if !slices.Equal(patterns, wantPatterns) {
		t.Fatalf("csrfileAccessPatterns() = %v, want %v", patterns, wantPatterns)
	}
	seenKind := make(map[csrfile.WeightKind]string, len(arms))
	for _, arm := range arms {
		if prev, dup := seenKind[arm.wantKind()]; dup {
			t.Errorf("arms %q and %q both claim weight kind %s", prev, arm.label(),
				csrfileWeightKindName(arm.wantKind()))
		}
		seenKind[arm.wantKind()] = arm.label()
	}
	for _, k := range []csrfile.WeightKind{
		csrfile.WeightAbsent, csrfile.WeightUint32, csrfile.WeightUint64,
		csrfile.WeightFloat32, csrfile.WeightFloat64,
	} {
		if _, ok := seenKind[k]; !ok {
			t.Errorf("no arm drives weight kind %s", csrfileWeightKindName(k))
		}
	}
}

// -----------------------------------------------------------------------------
// The non-vacuity gate can fire
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_NonVacuityGateFires proves the shape-only gate is
// discriminating: a run that reached one weight kind, one access pattern and a
// truncation that truncated nothing must be REFUSED by it. Without this the
// gate could be permanently silent and the grid's coverage claim unfalsifiable.
func TestCSRFileMatrix_NonVacuityGateFires(t *testing.T) {
	full, err := runCSRFileAccessMatrix(0x2478_0002)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if len(full.vacuity) > 0 {
		t.Fatalf("the non-vacuity gate fired on a full grid run:\n%v", full.vacuity)
	}

	degenerate := csrfileMatrixResult{
		shape: full.shape,
		cells: []csrfileCell{{
			arm: "float64", wantKind: csrfile.WeightFloat64, observedKind: csrfile.WeightFloat64,
			pattern: csrfile.AccessDefault, weightBytes: 64, hintApplied: true,
		}},
		truncs: []csrfileTruncCell{
			// A cut that removed nothing: the file is its original length.
			{arm: "float64", label: "aligned", cut: 128, aligned: true, origLen: 452, gotLen: 452},
			// A cut labelled misaligned that is in fact a multiple of 64.
			{arm: "float64", label: "misaligned", cut: 192, aligned: true, origLen: 452, gotLen: 192},
		},
	}
	v := checkCSRFileMatrixNonVacuity(&degenerate)
	if len(v) == 0 {
		t.Fatal("the non-vacuity gate accepted a run that reached one kind, one pattern and" +
			" truncated nothing")
	}
	msgs := make([]string, 0, len(v))
	for _, viol := range v {
		msgs = append(msgs, viol.Op+" "+viol.Message)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{
		"<csrfile-kind-unreached>",
		"<csrfile-pattern-unreached>",
		"<csrfile-absent-unreached>",
		"<csrfile-trunc-noop>",
		"<csrfile-trunc-not-misaligned>",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the non-vacuity report does not mention %s:\n%s", want, joined)
		}
	}
	t.Logf("non-vacuity gate on a degenerate run fired %d times:\n%s", len(v), joined)
}

// TestCSRFileMatrix_KindCoverageReadsTheSubstrate proves the gate's kind
// coverage is read from the PUBLISHED HEADER and not from the arm that was
// selected. The vehicle is csrfile's own documented downgrade: a weighted CSR
// carrying an EMPTY weights slice is published as WeightAbsent. Driving the
// float64 arm with such a CSR therefore produces a run that "selected float64"
// and observed absent — and the gate must call that kind unreached.
func TestCSRFileMatrix_KindCoverageReadsTheSubstrate(t *testing.T) {
	disk := NewSimDisk(NewSeed(0x2478_0003), 0)
	fsys := simCSRFS{disk: disk}
	sh := buildCSRFileShape(NewSeed(0x2478_0003))

	// A float64 CSR whose weights slice is empty.
	c := csr.FromArrays[float64](sh.vertices, sh.edges, nil, sh.order, uint64(len(sh.edges)))
	h, err := csrfile.WriteToFileWith[float64](fsys, csrfileMatrixPath, c)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if h.Weight != csrfile.WeightAbsent {
		t.Fatalf("a float64 CSR with no weights published kind %s, want absent"+
			" (the documented runtime downgrade)", csrfileWeightKindName(h.Weight))
	}
	res := csrfileMatrixResult{
		shape: sh,
		cells: []csrfileCell{{
			arm: "float64", wantKind: csrfile.WeightFloat64, observedKind: h.Weight,
			pattern: csrfile.AccessDefault, hintApplied: true,
		}},
	}
	v := checkCSRFileMatrixNonVacuity(&res)
	var sawFloat64Gap bool
	for _, viol := range v {
		if viol.Op == "<csrfile-kind-unreached>" && strings.Contains(viol.Message, "float64") {
			sawFloat64Gap = true
		}
	}
	if !sawFloat64Gap {
		t.Fatalf("the gate counted float64 as covered although the published header says %s:"+
			" it is reading the loop variable, not the substrate\n%v",
			csrfileWeightKindName(h.Weight), v)
	}
	t.Logf("downgrade witnessed: a float64 CSR with an empty weights slice publishes as %s",
		csrfileWeightKindName(h.Weight))
}

// -----------------------------------------------------------------------------
// WeightAbsent versus a weighted file
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_WeightAbsentIsDistinguishable answers, on the substrate,
// whether "this file has no weights" is distinguishable from "this file has
// weights". It checks four independent signals over one shared topology, and
// then pins the ONE case where the distinction genuinely collapses.
//
// The collapse matters because it is the csrfile-side shape of rmp #2526: a CSR
// declared at a weighted Go type but carrying no weights at runtime is
// downgraded by the writer and lands on disk BYTE-IDENTICAL to a graph that
// never had weights. No reader can tell them apart, because there is nothing to
// tell apart.
func TestCSRFileMatrix_WeightAbsentIsDistinguishable(t *testing.T) {
	sh := buildCSRFileShape(NewSeed(0x2478_0004))
	if len(sh.edges) == 0 {
		t.Fatal("fixture has no edges")
	}

	publish := func(t *testing.T, arm csrfileWeightArm) (csrfile.Header, []byte, []byte) {
		t.Helper()
		disk := NewSimDisk(NewSeed(1), 0)
		fsys := simCSRFS{disk: disk}
		if err := arm.publish(fsys, csrfileMatrixPath, sh); err != nil {
			t.Fatalf("publish %s: %v", arm.label(), err)
		}
		img, err := disk.ReadFile(csrfileMatrixPath)
		if err != nil {
			t.Fatalf("read %s: %v", arm.label(), err)
		}
		r, err := csrfile.OpenWith(fsys, csrfileMatrixPath)
		if err != nil {
			t.Fatalf("open %s: %v", arm.label(), err)
		}
		defer func() { _ = r.Close() }()
		return r.Header(), slices.Clone(r.WeightsRaw()), img
	}

	arms := csrfileWeightArms()
	var absentHeader csrfile.Header
	var absentRaw, absentImg []byte
	for _, arm := range arms {
		if arm.wantKind() == csrfile.WeightAbsent {
			absentHeader, absentRaw, absentImg = publish(t, arm)
		}
	}
	if absentHeader.Weight != csrfile.WeightAbsent {
		t.Fatalf("the unweighted arm published kind %s", csrfileWeightKindName(absentHeader.Weight))
	}
	if absentRaw != nil {
		t.Errorf("an unweighted file yielded a %d-byte weights section", len(absentRaw))
	}
	if absentHeader.WeightsOffset != 0 {
		t.Errorf("an unweighted header carries WeightsOffset=%d, want 0", absentHeader.WeightsOffset)
	}

	for _, arm := range arms {
		if arm.wantKind() == csrfile.WeightAbsent {
			continue
		}
		h, raw, img := publish(t, arm)
		// 1. The header kind differs.
		if h.Weight == csrfile.WeightAbsent {
			t.Errorf("[%s] published as absent", arm.label())
		}
		// 2. The weights section exists and is the declared width.
		if want := arm.wantKind().Size() * len(sh.edges); len(raw) != want {
			t.Errorf("[%s] weights section is %d bytes, want %d", arm.label(), len(raw), want)
		}
		// 3. The header points at a weights section.
		if h.WeightsOffset == 0 {
			t.Errorf("[%s] weighted header carries WeightsOffset=0", arm.label())
		}
		// 4. The file is strictly larger — the substrate signal, independent of
		//    every header field.
		if len(img) <= len(absentImg) {
			t.Errorf("[%s] the weighted file is %d bytes and the unweighted file of the same"+
				" topology is %d: not larger", arm.label(), len(img), len(absentImg))
		}
		t.Logf("[%-7s] kind=%s weights=%dB file=%dB (unweighted file=%dB)",
			arm.label(), csrfileWeightKindName(h.Weight), len(raw), len(img), len(absentImg))
	}

	// The collapse: a weighted Go type with no weights materialised.
	disk := NewSimDisk(NewSeed(2), 0)
	fsys := simCSRFS{disk: disk}
	c := csr.FromArrays[float64](sh.vertices, sh.edges, nil, sh.order, uint64(len(sh.edges)))
	if _, err := csrfile.WriteToFileWith[float64](fsys, csrfileMatrixPath, c); err != nil {
		t.Fatalf("publish float64-without-weights: %v", err)
	}
	downgraded, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		t.Fatalf("read downgraded: %v", err)
	}
	if !slices.Equal(downgraded, absentImg) {
		t.Fatalf("a float64 CSR with no weights published %d bytes and a struct{} CSR published %d:"+
			" the documented downgrade no longer produces identical bytes — re-read the writer",
			len(downgraded), len(absentImg))
	}
	t.Logf("pinned: a float64 CSR carrying an EMPTY weights slice publishes byte-identically"+
		" (%d bytes) to a struct{} CSR — 'declared weighted, nothing materialised' is"+
		" indistinguishable on disk from 'unweighted'", len(downgraded))
}

// -----------------------------------------------------------------------------
// Truncation: aligned versus misaligned
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_TruncationSentinels pins what a truncated csrfile actually
// does, and answers the question the task asked: whether a cut ON an alignment
// boundary and a cut OFF one behave differently.
//
// They do not, and the reason is structural. [csrfile.Header.validate] compares
// the file's length against the ONE canonical layout for its counts and demands
// EXACT equality, so any length other than the right one is refused identically
// — alignment never enters the decision. The only threshold that changes the
// answer is HeaderSize+4: below it the length gate fires first.
func TestCSRFileMatrix_TruncationSentinels(t *testing.T) {
	disk := NewSimDisk(NewSeed(0x2478_0005), 0)
	fsys := simCSRFS{disk: disk}
	sh := buildCSRFileShape(NewSeed(0x2478_0005))
	arm := csrfileWeightArms()[4] // float64
	if err := arm.publish(fsys, csrfileMatrixPath, sh); err != nil {
		t.Fatalf("publish: %v", err)
	}
	r, err := csrfile.OpenWith(fsys, csrfileMatrixPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	h := r.Header()
	_ = r.Close()
	img, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	total := len(img)
	verticesEnd := int64(h.VerticesOffset + 8*h.NVertices)

	cases := []struct {
		name string
		cut  int64
		want string
	}{
		{"empty", 0, "ErrHeaderTooShort"},
		{"one-byte", 1, "ErrHeaderTooShort"},
		{"below-header-boundary", csrfile.HeaderSize + 3, "ErrHeaderTooShort"},
		{"header-boundary", csrfile.HeaderSize + 4, "ErrHeaderInconsistent"},
		{"aligned-in-vertices", int64(h.VerticesOffset + csrfile.Alignment), "ErrHeaderInconsistent"},
		{"misaligned-in-vertices", verticesEnd - 3, "ErrHeaderInconsistent"},
		{"aligned-at-edges", int64(h.EdgesOffset), "ErrHeaderInconsistent"},
		{"aligned-at-crc", int64(h.TailCRCOffset), "ErrHeaderInconsistent"},
		{"one-byte-short", int64(total) - 1, "ErrHeaderInconsistent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSimDisk(NewSeed(7), 0)
			f := simCSRFS{disk: d}
			if err := arm.publish(f, csrfileMatrixPath, sh); err != nil {
				t.Fatalf("publish: %v", err)
			}
			before, err := d.ReadFile(csrfileMatrixPath)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			if err := d.TruncatePath(csrfileMatrixPath, tc.cut); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			after, err := d.ReadFile(csrfileMatrixPath)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			// Non-vacuity for this case: the cut must have shortened the file.
			if len(after) >= len(before) || int64(len(after)) != tc.cut {
				t.Fatalf("the cut did not truncate: %d -> %d bytes, asked for %d",
					len(before), len(after), tc.cut)
			}
			rr, oerr := csrfile.OpenWith(f, csrfileMatrixPath)
			if oerr == nil {
				_ = rr.Close()
				t.Fatalf("the reader ACCEPTED a %d-byte truncation of a %d-byte file", tc.cut, len(before))
			}
			if got := csrfileSentinelName(oerr); got != tc.want {
				t.Fatalf("cut=%d (aligned=%t) gave %s (%v), want %s",
					tc.cut, tc.cut%int64(csrfile.Alignment) == 0, got, oerr, tc.want)
			}
			if tc.want == "ErrHeaderInconsistent" && !errors.Is(oerr, csrfile.ErrFileCorrupted) {
				t.Fatalf("the structural refusal %v does not wrap ErrFileCorrupted", oerr)
			}
			t.Logf("cut=%4d aligned=%-5t -> %s", tc.cut, tc.cut%int64(csrfile.Alignment) == 0, oerr)
		})
	}

	// The comparison the task asked for, stated once as an assertion rather
	// than left implicit in the table: the aligned and the misaligned cut in
	// the vertices section produce the SAME sentinel.
	sentinelAt := func(cut int64) string {
		d := NewSimDisk(NewSeed(8), 0)
		f := simCSRFS{disk: d}
		if err := arm.publish(f, csrfileMatrixPath, sh); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if err := d.TruncatePath(csrfileMatrixPath, cut); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		_, oerr := csrfile.OpenWith(f, csrfileMatrixPath)
		return csrfileSentinelName(oerr)
	}
	alignedCut := int64(h.VerticesOffset + csrfile.Alignment)
	misalignedCut := verticesEnd - 3
	if alignedCut%int64(csrfile.Alignment) != 0 || misalignedCut%8 == 0 {
		t.Fatalf("the two cuts do not differ in alignment (aligned=%d, misaligned=%d):"+
			" comparing them proves nothing", alignedCut, misalignedCut)
	}
	if a, b := sentinelAt(alignedCut), sentinelAt(misalignedCut); a != b {
		t.Fatalf("an aligned cut gave %s and a misaligned cut gave %s — the difference is real"+
			" and this test's premise (that alignment does not enter Open's decision) is wrong", a, b)
	} else {
		t.Logf("aligned cut %d and misaligned cut %d both give %s: Open compares the file's LENGTH"+
			" against the one canonical layout, so alignment never enters its decision",
			alignedCut, misalignedCut, a)
	}
}

// -----------------------------------------------------------------------------
// Reinterpret refuses; it does not return an error
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_ReinterpretRefuses pins [csrfile.Reinterpret]'s refusal
// contract. Two things are worth stating plainly because a caller expecting a
// typed error would get neither:
//
//   - Reinterpret refuses by PANIC. There is no error return.
//   - Its alignment precondition is on the BASE ADDRESS of the buffer, which is
//     why truncating a file can never violate it: truncation changes a length,
//     and the length is the OTHER half of the precondition.
func TestCSRFileMatrix_ReinterpretRefuses(t *testing.T) {
	sw := csrfileAlignmentSweep()
	if v := checkCSRFileAlignSweep(sw); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("VERDICT %s: %s", viol.Op, viol.Message)
		}
	}
	t.Logf("byte phases: accepted=%v refused=%v; refusal=%q", sw.accepted, sw.refused, sw.refusalMsg)

	buf := make([]byte, 4096)
	cases := []struct {
		name string
		run  func()
		want string
	}{
		{"short buffer", func() { _ = csrfile.Reinterpret[uint64](buf[:7], 1) }, "need 8 bytes"},
		{"overflowing n", func() { _ = csrfile.Reinterpret[uint64](buf, math.MaxInt) }, "need 9223372036854775807 bytes"},
		{"negative n", func() { _ = csrfile.Reinterpret[uint64](buf, -1) }, "negative n"},
	}
	for _, tc := range cases {
		msg := csrfileRecover(tc.run)
		if msg == "" {
			t.Errorf("%s: Reinterpret returned a view instead of refusing", tc.name)
			continue
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: refusal %q does not contain %q", tc.name, msg, tc.want)
		}
		t.Logf("%-14s -> %s", tc.name, msg)
	}

	// n == 0 is the documented non-refusal: it returns nil without inspecting
	// the buffer at all, so it does NOT probe alignment. Pinning this keeps a
	// future probe from being written at n=0 and proving nothing.
	if msg := csrfileRecover(func() {
		if s := csrfile.Reinterpret[uint64](buf[1:], 0); s != nil {
			panic(fmt.Sprintf("n=0 returned a %d-element slice", len(s)))
		}
	}); msg != "" {
		t.Errorf("n=0 over a misaligned base: %s", msg)
	} else {
		t.Log("n=0 returns nil without checking alignment — an alignment probe must use n>0")
	}

	// The aligned-copy helper the truncation probe depends on must really
	// produce a base Reinterpret accepts; otherwise every "data too short"
	// refusal it observes could in fact be an alignment refusal in disguise.
	img := make([]byte, 512)
	aligned, phase := csrfileAlignedCopy(img)
	if len(aligned) != len(img) {
		t.Fatalf("csrfileAlignedCopy returned %d bytes for a %d-byte image", len(aligned), len(img))
	}
	if msg := csrfileRecover(func() { _ = csrfile.Reinterpret[uint64](aligned, 64) }); msg != "" {
		t.Fatalf("csrfileAlignedCopy (phase %d) produced a base Reinterpret refuses: %s", phase, msg)
	}
	t.Logf("csrfileAlignedCopy shifted by phase %d to reach an 8-byte-aligned base", phase)
}

// -----------------------------------------------------------------------------
// The madvise path — the only arm that leaves the in-memory disk
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_MadviseOverRealMapping is the arm the deterministic disk
// cannot reach. [csrfile.Open] always mmaps — there is no byte-backed branch in
// it — so a Reader obtained from Open necessarily routes SetHint into the
// platform madvise call, whereas the DST seam's byte-backed Reader returns nil
// before it gets there.
//
// The oracle is that an ADVISORY hint changes nothing observable. AccessDontNeed
// makes that a real question rather than a formality: on a live mapping it tells
// the kernel to drop the resident pages, so the read that follows must fault
// them back in and yield exactly the same bytes.
func TestCSRFileMatrix_MadviseOverRealMapping(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "matrix.csr")
	sh := buildCSRFileShape(NewSeed(0x2478_0006))
	weights := make([]float64, len(sh.mags))
	for i, m := range sh.mags {
		weights[i] = float64(m) + 0.25
	}
	c := csr.FromArrays[float64](sh.vertices, sh.edges, weights, sh.order, uint64(len(sh.edges)))
	if _, err := csrfile.WriteToFile[float64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snapshot := func() ([]uint64, []graph.NodeID, []byte) {
		var vs []uint64
		var es []graph.NodeID
		var ws []byte
		if err := r.Read(func(a []uint64, b []graph.NodeID, cbytes []byte) error {
			vs, es, ws = slices.Clone(a), slices.Clone(b), slices.Clone(cbytes)
			return nil
		}); err != nil {
			t.Fatalf("Read: %v", err)
		}
		return vs, es, ws
	}
	baseV, baseE, baseW := snapshot()
	if len(baseW) != 8*len(sh.edges) {
		t.Fatalf("weights section is %d bytes, want %d", len(baseW), 8*len(sh.edges))
	}

	// Every pattern, applied in turn to the SAME live mapping, so the hints
	// compose the way a long-lived reader would apply them.
	applied := 0
	for _, p := range csrfileAccessPatterns() {
		if err := r.SetHint(p); err != nil {
			t.Errorf("SetHint(%s) over a real mapping: %v", csrfileAccessPatternName(p), err)
			continue
		}
		applied++
		vs, es, ws := snapshot()
		if !slices.Equal(vs, baseV) || !slices.Equal(es, baseE) || !slices.Equal(ws, baseW) {
			t.Fatalf("the mapping's contents changed after SetHint(%s): an advisory hint altered"+
				" the data", csrfileAccessPatternName(p))
		}
		t.Logf("madvise %-10s -> nil; %d vertices, %d edges, %d weight bytes unchanged",
			csrfileAccessPatternName(p), len(vs), len(es), len(ws))
	}
	// Non-vacuity for this arm: every pattern must have reached the syscall.
	if applied != len(csrfileAccessPatterns()) {
		t.Fatalf("only %d of %d access patterns were applied to the real mapping",
			applied, len(csrfileAccessPatterns()))
	}

	// An out-of-range pattern is NOT rejected: setHint's switch falls through to
	// MADV_NORMAL. Pinned rather than asserted as desirable — a caller cannot
	// use SetHint to validate a pattern value.
	if err := r.SetHint(csrfile.AccessPattern(200)); err != nil {
		t.Errorf("SetHint with an out-of-range pattern returned %v; the measured behaviour was nil", err)
	} else {
		t.Log("pinned: an out-of-range AccessPattern is accepted and mapped to MADV_NORMAL," +
			" so SetHint validates nothing")
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.SetHint(csrfile.AccessRandom); !errors.Is(err, csrfile.ErrReaderClosed) {
		t.Fatalf("SetHint after Close over a real mapping returned %v, want ErrReaderClosed", err)
	}
}

// TestCSRFileMatrix_RealMmapTruncationDiverges measures the truncation battery
// again over the REAL mmap path and records where it diverges from the in-memory
// seam. The divergence is at length zero: the byte-backed reader reports the
// typed [csrfile.ErrHeaderTooShort], while the mmap path fails earlier — mmap(2)
// itself refuses a zero-length mapping — and surfaces an UNTYPED wrapped
// syscall error. A caller matching on the package's sentinels will not match it.
func TestCSRFileMatrix_RealMmapTruncationDiverges(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := t.TempDir()
	sh := buildCSRFileShape(NewSeed(0x2478_0007))
	arm := csrfileWeightArms()[4] // float64
	weights := make([]float64, len(sh.mags))
	for i, m := range sh.mags {
		weights[i] = float64(m) + 0.25
	}
	c := csr.FromArrays[float64](sh.vertices, sh.edges, weights, sh.order, uint64(len(sh.edges)))

	ref := filepath.Join(dir, "ref.csr")
	h, err := csrfile.WriteToFile[float64](ref, c)
	if err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	info, err := os.Stat(ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	total := info.Size()

	cuts := []struct {
		name string
		cut  int64
	}{
		{"empty", 0},
		{"below-header-boundary", csrfile.HeaderSize + 3},
		{"header-boundary", csrfile.HeaderSize + 4},
		{"aligned-in-vertices", int64(h.VerticesOffset + csrfile.Alignment)},
		{"misaligned-in-vertices", int64(h.VerticesOffset+8*h.NVertices) - 3},
		{"one-byte-short", total - 1},
	}
	for _, tc := range cuts {
		path := filepath.Join(dir, tc.name+".csr")
		if _, err := csrfile.WriteToFile[float64](path, c); err != nil {
			t.Fatalf("%s: WriteToFile: %v", tc.name, err)
		}
		if err := os.Truncate(path, tc.cut); err != nil {
			t.Fatalf("%s: Truncate: %v", tc.name, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: Stat: %v", tc.name, err)
		}
		if st.Size() != tc.cut || st.Size() >= total {
			t.Fatalf("%s: the cut did not truncate: %d bytes, asked for %d, original %d",
				tc.name, st.Size(), tc.cut, total)
		}
		rr, oerr := csrfile.Open(path)
		if oerr == nil {
			_ = rr.Close()
			t.Fatalf("%s: the mmap reader ACCEPTED a %d-byte truncation of a %d-byte file",
				tc.name, tc.cut, total)
		}

		// The same cut through the in-memory seam, for the comparison.
		d := NewSimDisk(NewSeed(9), 0)
		f := simCSRFS{disk: d}
		if err := arm.publish(f, csrfileMatrixPath, sh); err != nil {
			t.Fatalf("%s: sim publish: %v", tc.name, err)
		}
		if err := d.TruncatePath(csrfileMatrixPath, tc.cut); err != nil {
			t.Fatalf("%s: sim truncate: %v", tc.name, err)
		}
		_, serr := csrfile.OpenWith(f, csrfileMatrixPath)
		realName, simName := csrfileSentinelName(oerr), csrfileSentinelName(serr)
		t.Logf("cut=%-5d %-22s mmap=%-21s bytes=%s", tc.cut, tc.name, realName, simName)

		if tc.cut == 0 {
			if realName != "untyped" {
				t.Errorf("a zero-length csrfile through mmap gave %s (%v); the measured behaviour"+
					" was an untyped wrapped mmap error", realName, oerr)
			}
			if simName != "ErrHeaderTooShort" {
				t.Errorf("a zero-length csrfile through the byte-backed seam gave %s (%v),"+
					" want ErrHeaderTooShort", simName, serr)
			}
			continue
		}
		if realName != simName {
			t.Errorf("cut=%d: the mmap path gave %s and the byte-backed path gave %s;"+
				" the two backends disagree on a non-empty truncation", tc.cut, realName, simName)
		}
	}
}

// -----------------------------------------------------------------------------
// The verdicts can fail
// -----------------------------------------------------------------------------

// TestCSRFileMatrix_VerdictsCanFail proves every verdict in this file is
// falsifiable. A green matrix is worth exactly as much as the demonstration
// that its oracles CAN go red, and each of the four below is driven red here by
// a control that differs from the passing case in one deliberate way.
func TestCSRFileMatrix_VerdictsCanFail(t *testing.T) {
	sh := buildCSRFileShape(NewSeed(0x2478_0008))
	arms := csrfileWeightArms()

	// 1. The round-trip verdict must reject a file that does not match the
	//    topology it is adjudicated against. Publishing shape A and verifying
	//    against shape B is the control: if this stayed silent, the round-trip
	//    check would be comparing the file with itself.
	t.Run("round-trip rejects a different topology", func(t *testing.T) {
		other := buildCSRFileShape(NewSeed(0x2478_0009))
		if slices.Equal(other.edges, sh.edges) {
			t.Fatal("the two control shapes are identical; the control proves nothing")
		}
		for _, arm := range arms {
			disk := NewSimDisk(NewSeed(1), 0)
			fsys := simCSRFS{disk: disk}
			if err := arm.publish(fsys, csrfileMatrixPath, sh); err != nil {
				t.Fatalf("publish %s: %v", arm.label(), err)
			}
			_, v := arm.verify(fsys, csrfileMatrixPath, other, csrfile.AccessDefault)
			if len(v) == 0 {
				t.Errorf("[%s] the round-trip verdict accepted a file published from a"+
					" DIFFERENT topology", arm.label())
			}
		}
	})

	// 2. The weights half specifically — same topology, different magnitudes.
	//    Without this, a check that compared only the section's LENGTH would
	//    pass the control above (different topology changes the length too).
	t.Run("round-trip rejects different weight values", func(t *testing.T) {
		altered := &csrfileShape{
			vertices: slices.Clone(sh.vertices),
			edges:    slices.Clone(sh.edges),
			mags:     slices.Clone(sh.mags),
			order:    sh.order,
		}
		altered.mags[0]++ // still exactly representable in every weight type
		for _, arm := range arms {
			if arm.wantKind() == csrfile.WeightAbsent {
				continue // an unweighted file carries no magnitudes to differ in
			}
			disk := NewSimDisk(NewSeed(2), 0)
			fsys := simCSRFS{disk: disk}
			if err := arm.publish(fsys, csrfileMatrixPath, sh); err != nil {
				t.Fatalf("publish %s: %v", arm.label(), err)
			}
			_, v := arm.verify(fsys, csrfileMatrixPath, altered, csrfile.AccessDefault)
			if len(v) == 0 {
				t.Errorf("[%s] the round-trip verdict accepted a file whose weights differ"+
					" from the expected ones in exactly one edge: the weights are being"+
					" compared by length, not by value", arm.label())
				continue
			}
			var sawWeights bool
			for _, viol := range v {
				if strings.Contains(viol.Op, "weights") {
					sawWeights = true
				}
			}
			if !sawWeights {
				t.Errorf("[%s] a differing weight value produced %v, none of which names the"+
					" weights section", arm.label(), v)
			}
		}
	})

	// 3. The truncation verdict must fire when nothing was truncated. Driving
	//    the probe at the file's FULL length makes the reader accept, makes the
	//    sentinel wrong, and makes Reinterpret return a view — all three
	//    truncation verdicts at once.
	t.Run("truncation verdict fires on an untruncated file", func(t *testing.T) {
		arm := arms[4] // float64
		header, total := csrfile.Layout(uint64(len(sh.vertices)), uint64(len(sh.edges)), arm.wantKind())
		if total == 0 {
			t.Fatal("Layout rejected the control shape")
		}
		cell, v, err := csrfileTruncationProbe(arm, sh, "control-no-cut", int64(total), header)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if len(v) == 0 {
			t.Fatal("the truncation verdict stayed silent on a file that was not truncated:" +
				" it cannot distinguish a refused short file from an accepted whole one")
		}
		ops := make([]string, 0, len(v))
		for _, viol := range v {
			ops = append(ops, viol.Op)
		}
		for _, want := range []string{
			"<csrfile-trunc-accepted>", "<csrfile-trunc-sentinel>", "<csrfile-trunc-reinterpret>",
		} {
			if !slices.Contains(ops, want) {
				t.Errorf("the untruncated control did not raise %s (raised %v)", want, ops)
			}
		}
		t.Logf("untruncated control: len=%d->%d sentinel=%s reinterpret=%q",
			cell.origLen, cell.gotLen, cell.sentinel, cell.reinterpretPanic)
	})

	// 4. The weight-discrimination verdict must fire when a weighted file is not
	//    larger than the unweighted one of the same topology — the shape a
	//    writer that dropped a weights section would produce.
	t.Run("weight discrimination fires when sizes collapse", func(t *testing.T) {
		collapsed := map[string]int{"absent": 580, "uint32": 580, "uint64": 900, "float32": 512, "float64": 900}
		v := checkCSRFileWeightDiscrimination(collapsed, arms)
		if len(v) != 2 {
			t.Fatalf("the discrimination verdict raised %d violations for two collapsed arms:\n%v",
				len(v), v)
		}
		joined := fmt.Sprint(v)
		if !strings.Contains(joined, "uint32") || !strings.Contains(joined, "float32") {
			t.Errorf("the collapsed arms are not both named:\n%s", joined)
		}
	})

	// 5. The alignment sweep verdict must fire on a gate that accepts every
	//    phase — which is what a Reinterpret with no alignment check looks like.
	t.Run("alignment sweep fires on an unchecked gate", func(t *testing.T) {
		unchecked := csrfileAlignSweep{accepted: []int{0, 1, 2, 3, 4, 5, 6, 7}}
		if v := checkCSRFileAlignSweep(unchecked); len(v) == 0 {
			t.Fatal("the alignment verdict accepted a gate that refused nothing")
		}
		wrongMsg := csrfileAlignSweep{
			accepted: []int{0}, refused: []int{1, 2, 3, 4, 5, 6, 7},
			refusalMsg: "csrfile: Reinterpret: need 8 bytes for 1 x uint64, got 7",
		}
		v := checkCSRFileAlignSweep(wrongMsg)
		if len(v) == 0 {
			t.Fatal("the alignment verdict accepted a sweep whose refusals came from the LENGTH" +
				" check: it would pass on a buffer that was merely too short")
		}
		t.Logf("alignment verdict on a length-shaped refusal: %s", v[0].Message)
	})
}
