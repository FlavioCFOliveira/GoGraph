package csrfile

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// v1FixturePath is the frozen v1 csrfile sample both tests in this file read.
var v1FixturePath = filepath.Join("testdata", "v1", "sample.csr")

// v1FixtureSHA256 is the SHA-256 of that file exactly as committed. Pinning it
// is what stops the fixture being regenerated, deliberately or by accident.
//
// Regenerating it destroys the only property it has. The file was written on
// 2026-05-19 by commit 89660c76, so it is evidence that TODAY's reader still
// reads a file written by an OLDER build. A sample.csr refreshed by the current
// build would prove only that the writer agrees with itself, which
// TestWriter_Determinism and TestFixtureGenerator_Determinism already prove
// without needing a committed file at all.
//
// If a deliberate format change ever makes this file genuinely unreadable, the
// answer is a NEW fixture beside it under testdata/v2/, not a refreshed
// sample.csr.
const v1FixtureSHA256 = "ad71f9a842e6e19176243a2ea4ec4ec454f23eb77092dee41347010e70bdcfc1"

// The fixture's frozen header, field by field.
//
// These are the values the FILE holds, written as literals rather than as the
// current build's [CurrentVersion] / [Alignment] constants — see
// TestCompat_V1FixtureMaps for why comparing a frozen artefact against a moving
// constant is the wrong way round.
//
// NVertices is 257, not 32: it counts the entries in the CSR offsets array,
// which spans the whole NodeID space including the gaps [graph.Mapper] leaves
// between its 256 shards (see [Header.NVertices]). NEdges is 94, not the 96
// [FixtureSpec] asked for: the generating spec left Multigraph false, so the
// two duplicate draws in the 96 collapsed.
const (
	v1FixtureVersion        uint16     = 1
	v1FixtureAlignment      uint8      = 64
	v1FixtureNVertices      uint64     = 257
	v1FixtureNEdges         uint64     = 94
	v1FixtureWeight         WeightKind = WeightAbsent
	v1FixtureVerticesOffset uint64     = 64
	v1FixtureEdgesOffset    uint64     = 2176
	v1FixtureWeightsOffset  uint64     = 0
	v1FixtureTailCRCOffset  uint64     = 2944
)

// v1FixtureRows is the graph the fixture encodes: every non-empty CSR row, in
// the file's own order, with each row's out-neighbours in the order the edges
// section stores them.
//
// The NodeIDs are not 0..31. The fixture's 32 vertex keys are the integers
// 0..31, but a NodeID is packNodeID(shard, intra-shard index) and every one of
// these 32 keys landed in a shard of its own at index 0, so each NodeID here IS
// the shard index the writing build's hash chose for that key. That hash has
// since changed, which is the whole reason these numbers must be read from the
// file and can never be recomputed — see TestCompat_V1FixtureMaps.
var v1FixtureRows = []struct {
	src  graph.NodeID
	dsts []graph.NodeID
}{
	{10, []graph.NodeID{143}},
	{15, []graph.NodeID{221, 158, 234}},
	{27, []graph.NodeID{124, 138}},
	{32, []graph.NodeID{124, 145, 114, 37}},
	{37, []graph.NodeID{167, 166, 101, 143}},
	{38, []graph.NodeID{32, 204, 27, 212}},
	{46, []graph.NodeID{32}},
	{77, []graph.NodeID{46}},
	{90, []graph.NodeID{37}},
	{97, []graph.NodeID{231, 234, 114, 204, 205}},
	{101, []graph.NodeID{124, 241, 77, 46}},
	{114, []graph.NodeID{135, 234, 208, 114}},
	{124, []graph.NodeID{166, 212, 114, 97, 10}},
	{135, []graph.NodeID{176, 37, 77, 15, 124}},
	{138, []graph.NodeID{143, 145, 208, 135}},
	{145, []graph.NodeID{174}},
	{158, []graph.NodeID{234, 37, 32, 158, 135}},
	{166, []graph.NodeID{124, 90, 145, 138, 101}},
	{167, []graph.NodeID{124, 167}},
	{174, []graph.NodeID{90, 234, 145, 37, 174}},
	{176, []graph.NodeID{101, 10}},
	{189, []graph.NodeID{143}},
	{204, []graph.NodeID{145, 124, 27, 205}},
	{205, []graph.NodeID{114, 90, 227, 205}},
	{208, []graph.NodeID{205, 32, 158, 46, 10, 27}},
	{212, []graph.NodeID{46}},
	{221, []graph.NodeID{231}},
	{231, []graph.NodeID{124, 37, 27}},
	{234, []graph.NodeID{77, 38, 174, 167}},
	{241, []graph.NodeID{38, 167}},
}

// v1FixtureSections rebuilds the exact contents both sections must decode to.
//
// Given NVertices, CSR is a bijection between (offsets array, targets array)
// and this row list, so stating the golden as 30 readable rows is exactly as
// strict as writing out 257 offsets and 94 targets by hand — and it says what
// the numbers mean. The caller must have checked that v1FixtureRows is in
// ascending src order, which this reconstruction assumes.
func v1FixtureSections() (vertices []uint64, edges []graph.NodeID) {
	vertices = make([]uint64, v1FixtureNVertices)
	edges = make([]graph.NodeID, 0, v1FixtureNEdges)
	next := 0
	for src := uint64(0); src < v1FixtureNVertices-1; src++ {
		vertices[src] = uint64(len(edges))
		if next < len(v1FixtureRows) && uint64(v1FixtureRows[next].src) == src {
			edges = append(edges, v1FixtureRows[next].dsts...)
			next++
		}
	}
	vertices[v1FixtureNVertices-1] = uint64(len(edges))
	return vertices, edges
}

// TestCompat_V1FixtureMaps is the forward-compatibility gate for the csrfile
// READER: it proves that the current build still opens a v1 file written by an
// OLDER build, and still decodes out of it exactly the graph that build
// encoded.
//
// # What is guaranteed
//
//   - store/csrfile/testdata/v1/sample.csr is byte-for-byte the file committed
//     in 89660c76 on 2026-05-19 (SHA-256 pinned in [v1FixtureSHA256]).
//   - [Open] accepts it, which is itself a chain of format assertions: the
//     magic, a version this build supports, little-endian byte order, a known
//     [WeightKind], offsets equal to the ones [Layout] computes for those
//     counts, a file length equal to that layout's total, a CRC32C (Castagnoli)
//     over every preceding byte, and the three CSR semantic invariants.
//   - The header decodes to the fixture's own frozen field values.
//   - Both sections decode to their exact frozen contents — all 257 offsets and
//     all 94 edge targets, checked element by element against [v1FixtureRows].
//
// The last of those is the point. Any change to how bytes on disk become
// numbers in memory — a header field moved or resized, the section alignment
// or offsets changed, the element width or byte order changed, the CRC
// algorithm or its coverage changed, a length or count prefix added to a
// section — makes this test fail, either because [Open] rejects the file or
// because the decoded values move.
//
// # What is NOT guaranteed, and must not be inferred
//
// This is not a byte-golden for the WRITER, and the fixture cannot be one. It
// is not regenerable: DO NOT run `go run ./cmd/fmtfixture -pkg csrfile` to
// "refresh" it.
//
// The docstring that stood here until rmp #2752 said the opposite — it named
// that command as the way to regenerate, and claimed "any mismatch flags an
// unintended on-disk-format change" while asserting nothing but four header
// fields. Both halves were wrong, and measurably so. Running that command today
// produces a file with an IDENTICAL header and 341 differing payload bytes
// (sha 1fdb55d9… against the committed ad71f9a8…), so the drift the docstring
// promised to flag was the one thing the assertions could not see.
//
// The 341 bytes are 243 vertex-offset low bytes, 94 edge-target low bytes and
// the 4-byte CRC trailer; every differing 8-byte word differs only in byte 0,
// because every offset and every NodeID in this fixture is below 256. The
// graph is unchanged: same 32 keys, same 94 distinct directed edges, same 5
// self-loops, identical in- and out-degree multisets, and the two edge sets map
// onto each other exactly under a bijection that colour refinement determines
// uniquely. What moved is the NodeID each key interns to, because
// [graph.Mapper]'s shard hash changed from hash/maphash.Comparable — seeded per
// process with maphash.MakeSeed — to unseeded FNV-1a, in ba813d08 on
// 2026-05-22, three days after this fixture was committed.
//
// So the payload was never reproducible: the build that wrote it drew a fresh
// random seed on every run and could not reproduce its own bytes a second time.
// And the payload encodes a graph.Mapper property, not a csrfile format
// property, so pinning it as a writer golden would fire on mapper changes that
// leave the on-disk format untouched. The writer's byte stability is pinned
// where it belongs, against itself, by TestWriter_Determinism and
// TestFixtureGenerator_Determinism.
func TestCompat_V1FixtureMaps(t *testing.T) {
	t.Parallel()

	// The golden must actually say something.
	if len(v1FixtureRows) == 0 {
		t.Fatal("v1FixtureRows is empty; this test would assert nothing")
	}
	for i := 1; i < len(v1FixtureRows); i++ {
		if v1FixtureRows[i].src <= v1FixtureRows[i-1].src {
			t.Fatalf("v1FixtureRows[%d].src = %d is not above [%d].src = %d; "+
				"v1FixtureSections assumes ascending src order",
				i, v1FixtureRows[i].src, i-1, v1FixtureRows[i-1].src)
		}
	}
	wantVertices, wantEdges := v1FixtureSections()
	if uint64(len(wantEdges)) != v1FixtureNEdges {
		t.Fatalf("v1FixtureRows carries %d edges, but the frozen header says %d",
			len(wantEdges), v1FixtureNEdges)
	}

	// The fixture is the file an older build wrote. If these bytes changed,
	// nothing below is evidence of anything.
	raw, err := os.ReadFile(v1FixturePath) //nolint:gosec // testdata
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", v1FixturePath, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != v1FixtureSHA256 {
		t.Fatalf("%s sha256 = %s, want %s.\n"+
			"The frozen fixture has been modified. It is the file an OLDER build wrote, "+
			"and regenerating it deletes the only guarantee it provides; restore it rather "+
			"than updating this constant. See this test's docstring.",
			v1FixturePath, got, v1FixtureSHA256)
	}

	r, err := Open(v1FixturePath)
	if err != nil {
		t.Fatalf("Open(%s): %v", v1FixturePath, err)
	}
	defer func() { _ = r.Close() }()

	// The header, against the FIXTURE's frozen values.
	h := r.Header()
	for _, f := range []struct {
		name      string
		got, want uint64
	}{
		{"Version", uint64(h.Version), uint64(v1FixtureVersion)},
		{"Alignment", uint64(h.Alignment), uint64(v1FixtureAlignment)},
		{"NVertices", h.NVertices, v1FixtureNVertices},
		{"NEdges", h.NEdges, v1FixtureNEdges},
		{"Weight", uint64(h.Weight), uint64(v1FixtureWeight)},
		{"VerticesOffset", h.VerticesOffset, v1FixtureVerticesOffset},
		{"EdgesOffset", h.EdgesOffset, v1FixtureEdgesOffset},
		{"WeightsOffset", h.WeightsOffset, v1FixtureWeightsOffset},
		{"TailCRCOffset", h.TailCRCOffset, v1FixtureTailCRCOffset},
	} {
		if f.got != f.want {
			t.Errorf("Header.%s = %d, want %d", f.name, f.got, f.want)
		}
	}
	if n := len(r.WeightsRaw()); n != 0 {
		t.Errorf("WeightsRaw() has %d bytes, want 0: the fixture is unweighted", n)
	}

	// The payload: what the old build's bytes mean to today's reader.
	gotVertices := r.Vertices()
	if len(gotVertices) != len(wantVertices) {
		t.Fatalf("len(Vertices()) = %d, want %d", len(gotVertices), len(wantVertices))
	}
	for i := range wantVertices {
		if gotVertices[i] != wantVertices[i] {
			t.Fatalf("Vertices()[%d] = %d, want %d (first of possibly several)",
				i, gotVertices[i], wantVertices[i])
		}
	}
	gotEdges := r.Edges()
	if len(gotEdges) != len(wantEdges) {
		t.Fatalf("len(Edges()) = %d, want %d", len(gotEdges), len(wantEdges))
	}
	for i := range wantEdges {
		if gotEdges[i] != wantEdges[i] {
			t.Fatalf("Edges()[%d] = %d, want %d (first of possibly several)",
				i, gotEdges[i], wantEdges[i])
		}
	}
}

// TestCompat_FutureVersionRejected synthesises a v1 file whose
// version field is bumped past CurrentVersion and verifies the
// header decoder returns ErrUnsupportedVersion.
func TestCompat_FutureVersionRejected(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(v1FixturePath) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < HeaderSize {
		t.Fatalf("fixture shorter than HeaderSize")
	}
	bumped := append([]byte(nil), data...)
	binary.LittleEndian.PutUint16(bumped[4:6], CurrentVersion+1)
	if _, err := DecodeHeader(bumped[:HeaderSize]); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("future-version DecodeHeader = %v, want ErrUnsupportedVersion", err)
	}
}
