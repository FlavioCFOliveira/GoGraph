package snapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// csrHeader builds a minimal csr.bin header: nV, nE, then the
// (hasWeights, weightSizeBytes) flag pair. No payload follows, so a
// reader that survives the count/overflow guards will fail later on a
// short read — the security guards under test must trip first, before
// any giant allocation is attempted.
func csrHeader(nV, nE uint64, hasW, wsize byte) []byte {
	b := make([]byte, 0, 18)
	b = binary.LittleEndian.AppendUint64(b, nV)
	b = binary.LittleEndian.AppendUint64(b, nE)
	b = append(b, hasW, wsize)
	return b
}

// boundedAllocBudget caps the heap growth a guarded read may incur. A
// correctly guarded reader rejects an implausible header before
// allocating the vertex/edge slices, so growth stays in the kilobytes;
// the unguarded reader allocated >= 2 GiB. 64 MiB leaves generous
// headroom for test scaffolding while still failing hard if the giant
// make() is reached.
const boundedAllocBudget = 64 << 20

// assertBoundedAlloc runs fn and fails if it grew the heap past
// boundedAllocBudget, evidence that the giant allocation was attempted.
//
// It forces a process-wide runtime.GC and reads global runtime.MemStats,
// so every caller MUST run serially (no t.Parallel): a concurrent test
// both pollutes the TotalAlloc delta and trips the race detector against
// encoding/json's internal sync.Pool, which GC drains mid-encode.
func assertBoundedAlloc(t *testing.T, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc
	if grew > boundedAllocBudget {
		t.Fatalf("read allocated %d bytes (> %d budget): giant allocation was not prevented",
			grew, boundedAllocBudget)
	}
}

// assertNoPanic runs fn and fails if it panicked, evidence that the
// overflow-driven make() with a negative/garbage size was reached.
func assertNoPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("read panicked: %v", r)
		}
	}()
	fn()
}

// writeMaliciousSnapshot lays out a snapshot directory whose csr.bin
// holds only an 18-byte header that lies about its vertex/edge counts,
// and a manifest whose FileEntry.Size truthfully records that the file
// is just hdr bytes long. The manifest CRC32C is computed over the real
// file bytes so the only thing standing between LoadSnapshotFull/Open
// and a giant allocation is the precise size bound: the count exceeds
// what 18 bytes could ever hold, so readCSRLimited must reject it before
// allocating the vertex/edge slice — and well before the CRC check that
// would otherwise also fail.
func writeMaliciousSnapshot(t *testing.T, hdr []byte) string {
	t.Helper()
	dir := t.TempDir()
	csrPath := filepath.Join(dir, CSRFile)
	if err := os.WriteFile(csrPath, hdr, 0o600); err != nil {
		t.Fatalf("WriteFile(csr.bin): %v", err)
	}
	crc := crc32.Checksum(hdr, castagnoli)
	m := Manifest{
		Version:   manifestVersionLegacy,
		CreatedAt: time.Now().UTC(),
		Order:     0,
		Size:      0,
		Files: []FileEntry{
			// Size is the truthful on-disk length: only the header.
			{Name: CSRFile, Size: int64(len(hdr)), CRC32C: crc},
		},
	}
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile(manifest.json): %v", err)
	}
	return dir
}

// TestOpen_ManifestSizeBoundRejectsHugeVertexCount is the PoC against the
// REAL load path. A snapshot whose manifest Size is tiny (18 bytes) but
// whose csr.bin header claims nV = 1<<28 must be rejected by Open with
// the typed corruption sentinel and WITHOUT the ~2 GiB allocation the
// unguarded reader would have made. Bounded allocation is asserted via a
// runtime.MemStats TotalAlloc delta.
func TestOpen_ManifestSizeBoundRejectsHugeVertexCount(t *testing.T) {
	// Not t.Parallel: assertBoundedAlloc forces a process-wide runtime.GC
	// and reads global MemStats. Running it concurrently with other tests
	// both pollutes the TotalAlloc delta and trips the race detector on
	// encoding/json's internal sync.Pool (GC drains pools mid-encode).
	hdr := csrHeader(1<<28, 0, 0, 0)
	dir := writeMaliciousSnapshot(t, hdr)
	assertBoundedAlloc(t, func() {
		_, err := Open(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("Open = %v, want ErrCSRCorrupted", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("Open = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestOpen_ManifestSizeBoundRejectsHugeEdgeCount mirrors the vertex test
// for the edge count on the Open path. nV is a valid 0 so the guard
// under test is the edge-count bound.
func TestOpen_ManifestSizeBoundRejectsHugeEdgeCount(t *testing.T) {
	// Not t.Parallel — see TestOpen_ManifestSizeBoundRejectsHugeVertexCount.
	hdr := csrHeader(0, 1<<28, 0, 0)
	dir := writeMaliciousSnapshot(t, hdr)
	assertBoundedAlloc(t, func() {
		_, err := Open(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("Open = %v, want ErrCSRCorrupted", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("Open = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestLoadSnapshotFull_ManifestSizeBoundRejectsHugeVertexCount is the
// recovery-path twin: LoadSnapshotFull (used by store/recovery) flows
// through readVerifiedCSR, which receives the manifest size. The same
// malicious header must be rejected there too, with no giant allocation.
func TestLoadSnapshotFull_ManifestSizeBoundRejectsHugeVertexCount(t *testing.T) {
	// Not t.Parallel — see TestOpen_ManifestSizeBoundRejectsHugeVertexCount.
	hdr := csrHeader(1<<28, 0, 0, 0)
	dir := writeMaliciousSnapshot(t, hdr)
	assertBoundedAlloc(t, func() {
		_, err := LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want ErrCSRCorrupted", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestLoadSnapshotFull_ManifestSizeBoundRejectsHugeEdgeCount is the
// edge-count recovery-path twin.
func TestLoadSnapshotFull_ManifestSizeBoundRejectsHugeEdgeCount(t *testing.T) {
	// Not t.Parallel — see TestOpen_ManifestSizeBoundRejectsHugeVertexCount.
	hdr := csrHeader(0, 1<<28, 0, 0)
	dir := writeMaliciousSnapshot(t, hdr)
	assertBoundedAlloc(t, func() {
		_, err := LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want ErrCSRCorrupted", err)
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("LoadSnapshotFull = %v, want it wrapped under ErrCorrupted", err)
		}
	})
}

// TestOpen_ManifestSizeBoundRejectsHostileWeightsSize exercises the
// weights file-budget bound on the real Open path. A legal weight is at
// most 8 bytes, but wsize is read straight off the wire and a hostile
// header can claim wsize=255. Here nE is small enough to pass the
// edge-count guard against the manifest size, yet 255*nE bytes of
// weights far exceed the whole file budget — weightsByteLen must reject
// it with the typed corruption error, no panic, no short buffer.
func TestOpen_ManifestSizeBoundRejectsHostileWeightsSize(t *testing.T) {
	// Not t.Parallel: this test writes a manifest via encoding/json, whose
	// internal sync.Pool races with the runtime.GC the sibling
	// assertBoundedAlloc tests force during the parallel phase.
	// nE = 4, hostile wsize = 255. The file carries the real 8-byte edge
	// payload so the edge array read succeeds and execution reaches the
	// weights guard. Manifest Size = 18 (header) + 8*4 (edges) = 50 bytes
	// => recordCap = 50/8 = 6, so nE=4 passes the count guard. Weights
	// then demand 255*4 = 1020 bytes, far past the 50-byte file budget =>
	// weightsByteLen rejects via the maxBytes>0 budget bound.
	const nE = 4
	body := csrHeader(0, nE, 1, 255)
	for i := 0; i < nE; i++ {
		// Real edge values so binary.Read of the edge array succeeds.
		body = binary.LittleEndian.AppendUint64(body, uint64(i))
	}
	dir := t.TempDir()
	csrPath := filepath.Join(dir, CSRFile)
	if err := os.WriteFile(csrPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile(csr.bin): %v", err)
	}
	size := int64(18 + 8*nE)
	crc := crc32.Checksum(body, castagnoli)
	m := Manifest{
		Version:   manifestVersionLegacy,
		CreatedAt: time.Now().UTC(),
		Files:     []FileEntry{{Name: CSRFile, Size: size, CRC32C: crc}},
	}
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile(manifest.json): %v", err)
	}
	assertNoPanic(t, func() {
		_, err := Open(dir)
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("Open = %v, want ErrCSRCorrupted (weights budget)", err)
		}
	})
}

// TestOpen_ManifestSizeBoundValidRoundTrip is the positive regression on
// the real path: a snapshot written by the production writer
// (WriteSnapshotCSR) still loads through Open with the precise size
// bound in place — the legitimate header always fits its own file size.
func TestOpen_ManifestSizeBoundValidRoundTrip(t *testing.T) {
	// Not t.Parallel — see TestOpen_ManifestSizeBoundRejectsHostileWeightsSize.
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range [][2]string{{"a", "b"}, {"a", "c"}, {"b", "c"}, {"c", "a"}} {
		if err := a.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}
	loaded, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(valid snapshot): %v", err)
	}
	if len(loaded.CSR.Vertices) != len(c.VerticesSlice()) {
		t.Fatalf("vertices = %d, want %d", len(loaded.CSR.Vertices), len(c.VerticesSlice()))
	}
	if len(loaded.CSR.Edges) != len(c.EdgesSlice()) {
		t.Fatalf("edges = %d, want %d", len(loaded.CSR.Edges), len(c.EdgesSlice()))
	}
}

// TestLoadSnapshotFull_ManifestSizeBoundValidRoundTrip is the positive
// regression on the recovery path: a full v3 snapshot still loads with
// the precise size bound in place.
func TestLoadSnapshotFull_ManifestSizeBoundValidRoundTrip(t *testing.T) {
	// Not t.Parallel — see TestOpen_ManifestSizeBoundRejectsHostileWeightsSize.
	dir := buildFullSnapshot(t)
	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull(valid snapshot): %v", err)
	}
	if len(loaded.CSR.Vertices) == 0 {
		t.Fatal("LoadSnapshotFull: vertices empty, want a populated CSR")
	}
}

// ---------------------------------------------------------------------
// Bare-reader backstop: the [ReadCSR] entry point has no manifest size,
// so its only structural guard is the absolute backstop cap maxCSRCount
// (1<<40) plus the overflow-safe weights computation. A count at or below
// the backstop (e.g. 1<<28) is NOT a corruption to the bare reader — only
// the size-bounded real path (Open / LoadSnapshotFull, covered above)
// rejects 1<<28. These tests pin the backstop contract: a count strictly
// greater than 1<<40 is rejected with the typed error before allocation,
// and a hostile weight size cannot drive an overflowing make().
// ---------------------------------------------------------------------

// TestReadCSR_BackstopRejectsCountAboveCap asserts the bare-reader
// backstop: a vertex or edge count strictly above maxCSRCount is rejected
// with ErrCSRCorrupted and without the giant allocation the unguarded
// reader would have made. Counts at or below the cap (1<<28) are
// deliberately excluded — the bare reader cannot know the file is too
// short for them; only the size-bounded path rejects those.
func TestReadCSR_BackstopRejectsCountAboveCap(t *testing.T) {
	// Not t.Parallel: assertBoundedAlloc reads process-global MemStats.
	cases := []struct {
		name   string
		nV, nE uint64
	}{
		{name: "vertex just over cap", nV: maxCSRCount + 1},
		{name: "vertex near 2^41", nV: 1 << 41},
		{name: "edge just over cap", nE: maxCSRCount + 1},
		{name: "edge near 2^41", nE: 1 << 41},
	}
	for _, tc := range cases {
		// Subtests run serially (no t.Parallel) — see assertBoundedAlloc.
		t.Run(tc.name, func(t *testing.T) {
			data := csrHeader(tc.nV, tc.nE, 0, 0)
			assertBoundedAlloc(t, func() {
				_, err := ReadCSR(bytes.NewReader(data))
				if !errors.Is(err, ErrCSRCorrupted) {
					t.Fatalf("ReadCSR = %v, want ErrCSRCorrupted", err)
				}
			})
		})
	}
}

// TestReadCSR_BackstopHostileWeightsNeverPanics pins the overflow-safe
// guarantee of [weightsByteLen] on the bare-reader path, where no manifest
// size is known (maxBytes <= 0) so the byte-budget rejection cannot apply.
//
// A hostile header declares a small, legal edge count whose payload is
// present (so the edge array reads back without EOF) but a maximal weight
// size (wsize = 255). The naive int(wsize)*int(nE) could wrap to a
// negative/garbage size and panic the make(); the overflow-safe
// bits.Mul64 path must instead size weightBytes from the unwrapped low
// word and then fail cleanly on the short read — never panic, never a
// silent truncation. The precise byte-budget rejection of an oversized
// weights array is a property of the *size-bounded* path and is covered by
// [TestOpen_ManifestSizeBoundRejectsHostileWeightsSize]; on the bare
// reader the contract is only "no panic, surface the short read".
func TestReadCSR_BackstopHostileWeightsNeverPanics(t *testing.T) {
	// Not t.Parallel: keep the whole backstop family serial alongside the
	// alloc-measuring siblings sharing this process.
	const nE = 4
	body := csrHeader(0, nE, 1, 255)
	for i := 0; i < nE; i++ {
		// Real 8-byte edge values so the edge-array read succeeds and
		// execution reaches the weights sizing under test.
		body = binary.LittleEndian.AppendUint64(body, uint64(i))
	}
	assertNoPanic(t, func() {
		// The 255-byte-per-edge weights payload is absent, so a correct
		// reader sizes the buffer via the overflow-safe path and then
		// returns the short read (io.ErrUnexpectedEOF / EOF) without a
		// panic. We assert only the absence of a panic and of corruption-
		// classification confusion: a short read is an I/O error, not a
		// structural-corruption sentinel.
		_, err := ReadCSR(bytes.NewReader(body))
		if err == nil {
			t.Fatal("ReadCSR = nil, want a short-read error for absent weights payload")
		}
		if errors.Is(err, ErrCSRCorrupted) {
			t.Fatalf("ReadCSR = %v, want a plain short-read error, not ErrCSRCorrupted "+
				"(byte-budget rejection is the size-bounded path's job, not the bare reader's)", err)
		}
	})
}

// TestReadCSR_RoundTripValidSnapshot is the positive regression on the
// bare-reader path: a CSR written by WriteCSR still reads back with
// identical vertices, edges, and weight metadata after the backstop and
// overflow guards were added.
func TestReadCSR_RoundTripValidSnapshot(t *testing.T) {
	t.Parallel() // no global-alloc measurement here; safe to parallelise.
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	const n = 64
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, int64(i*7+1)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, c); err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if len(rb.Vertices) != len(c.VerticesSlice()) {
		t.Fatalf("vertices len = %d, want %d", len(rb.Vertices), len(c.VerticesSlice()))
	}
	if len(rb.Edges) != len(c.EdgesSlice()) {
		t.Fatalf("edges len = %d, want %d", len(rb.Edges), len(c.EdgesSlice()))
	}
	for i, want := range c.VerticesSlice() {
		if rb.Vertices[i] != want {
			t.Fatalf("vertex[%d] = %d, want %d", i, rb.Vertices[i], want)
		}
	}
	for i, want := range c.EdgesSlice() {
		if rb.Edges[i] != want {
			t.Fatalf("edge[%d] = %d, want %d", i, rb.Edges[i], want)
		}
	}
	if !rb.HasWeights {
		t.Fatalf("HasWeights = false, want true for an int64-weighted CSR")
	}
	if rb.WeightSize != 8 {
		t.Fatalf("WeightSize = %d, want 8", rb.WeightSize)
	}
	if len(rb.WeightBytes) != 8*len(c.EdgesSlice()) {
		t.Fatalf("WeightBytes len = %d, want %d", len(rb.WeightBytes), 8*len(c.EdgesSlice()))
	}
}
