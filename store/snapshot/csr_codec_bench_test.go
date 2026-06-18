package snapshot

import (
	"bytes"
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// csrBenchMinEdges is the lower bound on the edge count of the fixture CSR
// used by the codec benchmarks and the allocation-regression gate. The CSR
// codec memory audit (sprint 210) characterised the transient overhead at the
// 1M-edge scale, so the fixture is sized to clear that threshold.
const csrBenchMinEdges = 1 << 20 // 1,048,576 edges

// buildBenchCSR builds a single deterministic CSR with at least
// csrBenchMinEdges edges, reused across every benchmark iteration so that the
// fixture-construction cost (an adjacency-list build plus the CSR transform)
// is never folded into the measured WriteCSR / ReadCSR allocation figures.
//
// The fixture is a deterministic k-out digraph: node i (0 <= i < n) has an
// edge to each of (i+1), (i+2), ... (i+k) modulo n, giving exactly n*k edges
// with int64 weights. Building the adjacency list directly (no shapegen, no
// RNG) sidesteps the small node caps on the random generators and keeps the
// one-time build affordable for a short-layer benchmark. The int64 weights
// (W=int64) make WriteCSR exercise the weights section and ReadCSR the
// weight-bytes readback — the realistic recovery shape.
func buildBenchCSR(tb testing.TB) *csr.CSR[int64] {
	tb.Helper()
	const k = 4
	n := csrBenchMinEdges/k + 1
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		for j := 1; j <= k; j++ {
			dst := (i + j) % n
			if err := a.AddEdge(i, dst, int64(i*k+j)); err != nil {
				tb.Fatalf("build fixture adjlist: AddEdge(%d,%d): %v", i, dst, err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	if got := len(c.EdgesSlice()); got < csrBenchMinEdges {
		tb.Fatalf("fixture has %d edges, want >= %d", got, csrBenchMinEdges)
	}
	return c
}

// csrPayloadBytes returns the live on-disk payload size of c in bytes: the
// number of bytes WriteCSR serialises (header + vertices + edges + weights +
// optional handle block). The allocation-regression gate compares the codec's
// transient B/op against a small multiple of this figure, so a re-introduced
// whole-slice widening copy (a second edge or vertex column held live during
// the call) is caught.
func csrPayloadBytes(c *csr.CSR[int64]) int64 {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	handles := c.HandlesSlice()

	total := int64(8 + 8 + 2) // nVertices + nEdges + (hasW, wsize)
	total += int64(8 * len(verts))
	total += int64(8 * len(edges))
	if weights != nil {
		total += int64(int(csrWeightSize[int64]())) * int64(len(edges))
	}
	if handles != nil {
		total += 1 + int64(8*len(handles))
	}
	return total
}

// BenchmarkWriteCSR measures the per-call allocation cost of serialising a
// >=1M-edge CSR with [WriteCSR]. The fixture is built once outside the timed
// loop. b.ReportAllocs surfaces B/op and allocs/op so a benchstat run (and the
// bench-history gate) flags any regression in the writer's transient working
// set.
func BenchmarkWriteCSR(b *testing.B) {
	c := buildBenchCSR(b)
	b.SetBytes(csrPayloadBytes(c))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := WriteCSR(io.Discard, c); err != nil {
			b.Fatalf("WriteCSR: %v", err)
		}
	}
}

// BenchmarkReadCSR measures the per-call allocation cost of parsing a
// >=1M-edge CSR with [ReadCSR] (the recovery cold-start hot path:
// recovery.openCodec -> LoadSnapshotFull -> readVerifiedCSR -> readCSRLimited).
// The serialised image is produced once outside the timed loop; each iteration
// re-parses it from a fresh bytes.Reader so only the readback allocation is
// measured.
func BenchmarkReadCSR(b *testing.B) {
	c := buildBenchCSR(b)
	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, c); err != nil {
		b.Fatalf("WriteCSR: %v", err)
	}
	image := buf.Bytes()
	b.SetBytes(int64(len(image)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := ReadCSR(bytes.NewReader(image)); err != nil {
			b.Fatalf("ReadCSR: %v", err)
		}
	}
}

// TestCSRCodec_AllocBudget is the allocation-regression gate that runs under
// the standard `go test ./...` layer (not only under -bench). It executes the
// codec benchmarks in-process via testing.Benchmark and asserts that the
// transient B/op of each direction stays within a small multiple of the live
// payload. The multiplier admits the unavoidable working set (bufio buffer,
// the single readback edge column for ReadCSR, header scratch) while rejecting
// the previously-removed whole-slice widening copies (#1593 ReadCSR's `raw`
// uint64 array; #1594 WriteCSR's `tmp` uint64 array): each of those was a
// second full edge column, so its return would push B/op past the bound.
//
// The factor is expressed against the EDGE-column payload (8 bytes/edge), the
// dimension both removed copies scaled with, so the bound is meaningful
// regardless of the vertex/weight ratio. Both directions must hold:
//
//   - ReadCSR allocates the parsed vertices, the parsed edges column, and the
//     weight bytes — roughly one payload's worth. A re-introduced `raw` array
//     adds a whole extra edge column (8 bytes/edge).
//   - WriteCSR streams to its destination; after the fix it holds only a
//     fixed-size chunk buffer plus the bufio writer. A re-introduced `tmp`
//     array adds a whole edge column (8 bytes/edge).
func TestCSRCodec_AllocBudget(t *testing.T) {
	c := buildBenchCSR(t)
	edgeBytes := int64(8 * len(c.EdgesSlice()))
	payload := csrPayloadBytes(c)

	// WriteCSR: after the fix the writer holds no per-element widening copy,
	// only a bounded chunk buffer (csrWriteChunk) and the 1 MiB bufio buffer.
	// Budget: a small constant well under one edge column. We assert it does
	// not exceed half the edge-column payload — a re-introduced `tmp` edge
	// column alone (1.0x edgeBytes) blows past this.
	var image bytes.Buffer
	if _, _, err := WriteCSR(&image, c); err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	writeRes := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, _, err := WriteCSR(io.Discard, c); err != nil {
				b.Fatalf("WriteCSR: %v", err)
			}
		}
	})
	writeBytesPerOp := writeRes.AllocedBytesPerOp()
	if writeBytesPerOp > edgeBytes/2 {
		t.Errorf("WriteCSR B/op = %d, want <= %d (half the %d-byte edge column); "+
			"a whole-slice widening copy was likely re-introduced",
			writeBytesPerOp, edgeBytes/2, edgeBytes)
	}

	// ReadCSR: after the fix the parser allocates the vertices column, the
	// single edges column (reinterpreted in place — no `raw` shadow), and the
	// weight bytes: ~one payload. Budget 2x payload leaves head-room for the
	// bufio reader and header scratch while a re-introduced `raw` edge column
	// (an extra 8 bytes/edge on top of the payload) is still caught when the
	// edge column dominates.
	imageBytes := image.Bytes()
	readRes := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := ReadCSR(bytes.NewReader(imageBytes)); err != nil {
				b.Fatalf("ReadCSR: %v", err)
			}
		}
	})
	readBytesPerOp := readRes.AllocedBytesPerOp()
	if readBytesPerOp > 2*payload {
		t.Errorf("ReadCSR B/op = %d, want <= %d (2x the %d-byte payload); "+
			"the `raw` widening copy was likely re-introduced",
			readBytesPerOp, 2*payload, payload)
	}

	t.Logf("payload=%d edgeBytes=%d WriteCSR B/op=%d ReadCSR B/op=%d",
		payload, edgeBytes, writeBytesPerOp, readBytesPerOp)
}
