package snapshot

import (
	"bytes"
	"io"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// countingWriter appends every Write to buf and counts the calls, so a test can
// compare both the bytes and the number of destination writes (syscalls, for a
// file) two writer configurations produce.
type countingWriter struct {
	buf   bytes.Buffer
	calls int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	return c.buf.Write(p)
}

// componentOut is one serialised component: its bytes, reported size and CRC,
// and how many Write calls reached the destination.
type componentOut struct {
	bytes []byte
	size  int64
	crc   uint32
	calls int
}

// bufSizeCases are the staging-buffer sizes the identity test compares. The
// first is the fixed size every component writer used before rmp #3083 — the
// "old writer" — and the rest are the sizes the writers use now plus a
// degenerate one (bufio raises 16 to its 16-byte minimum) that forces a flush
// on almost every write.
var bufSizeCases = []int{componentBufMax, captureBufSize, 4097, 16}

// TestComponentWriters_ByteIdenticalAcrossBufferSizes pins rmp #3083's
// invariant: the staging-buffer size of a snapshot component writer is a pure
// resource choice. For an empty, a small and a large graph — large meaning
// components above the 1 MiB cap — every component writer emits the same bytes,
// size and CRC under the old fixed 1 MiB buffer and under every size it may use
// now; and the CSR writer, the one that writes straight to a file, never makes
// more destination writes than the old writer did.
func TestComponentWriters_ByteIdenticalAcrossBufferSizes(t *testing.T) {
	type graphCase struct {
		name          string
		nodes, degree int
	}
	graphs := []graphCase{{"empty", 0, 0}, {"small", 50, 2}, {"large", 40_000, 4}}

	sawOverCap := false
	for _, gc := range graphs {
		t.Run(gc.name, func(t *testing.T) {
			g := buildPublishBenchLPG(t, gc.nodes, gc.degree)
			gi := buildInt64KeyedLPG(t, gc.nodes)
			cs := csr.BuildFromAdjList(g.AdjList())
			capt, err := CaptureGraph[string, int64](g, cs, nil, nil)
			if err != nil {
				t.Fatalf("CaptureGraph: %v", err)
			}
			captured := map[string][]byte{
				"labels":        capt.labels.bytes,
				"properties":    capt.properties.bytes,
				"mapper-string": capt.mapper.bytes,
			}

			writers := map[string]func(w io.Writer, bufSize int) (int64, uint32, error){
				"labels": func(w io.Writer, n int) (int64, uint32, error) {
					return writeLabels(w, g, nil, snapshotRegistry, n)
				},
				"properties": func(w io.Writer, n int) (int64, uint32, error) {
					return writeProperties(w, g, nil, snapshotPropertyKeys, newPropValueArena, n)
				},
				"mapper-string": func(w io.Writer, n int) (int64, uint32, error) {
					s, c, _, err := writeMapperStringN(w, g.AdjList().Mapper(), nil, n)
					return s, c, err
				},
				"mapper-codec": func(w io.Writer, n int) (int64, uint32, error) {
					s, c, _, err := writeMapperN(w, gi.AdjList().Mapper(), txn.NewInt64Codec(), nil, n)
					return s, c, err
				},
				"csr": func(w io.Writer, n int) (int64, uint32, error) {
					return writeCSRBuffered(w, cs, txn.NewInt64WeightCodec(), n)
				},
			}
			for name, write := range writers {
				run := func(bufSize int) componentOut {
					var cw countingWriter
					size, crc, err := write(&cw, bufSize)
					if err != nil {
						t.Fatalf("%s(bufSize=%d): %v", name, bufSize, err)
					}
					if size != int64(cw.buf.Len()) {
						t.Fatalf("%s(bufSize=%d): reported size %d, wrote %d bytes", name, bufSize, size, cw.buf.Len())
					}
					return componentOut{bytes: cw.buf.Bytes(), size: size, crc: crc, calls: cw.calls}
				}
				old := run(componentBufMax)
				if old.size == 0 {
					t.Fatalf("%s: old writer emitted nothing; the comparison would be vacuous", name)
				}
				if old.size > componentBufMax {
					sawOverCap = true
				}
				for _, n := range bufSizeCases[1:] {
					got := run(n)
					if got.size != old.size || got.crc != old.crc || !bytes.Equal(got.bytes, old.bytes) {
						t.Fatalf("%s: bufSize=%d output differs from the 1 MiB writer (size %d vs %d, crc %#x vs %#x)",
							name, n, got.size, old.size, got.crc, old.crc)
					}
				}
				if capturedBytes, ok := captured[name]; ok && !bytes.Equal(capturedBytes, old.bytes) {
					t.Fatalf("%s: CaptureGraph's component bytes differ from the 1 MiB writer's", name)
				}
				if name == "csr" {
					sized := run(0)
					if sized.size != old.size || sized.crc != old.crc || !bytes.Equal(sized.bytes, old.bytes) {
						t.Fatalf("csr: the length-sized writer's output differs from the 1 MiB writer")
					}
					if sized.calls > old.calls {
						t.Fatalf("csr: the length-sized writer made %d destination writes, the 1 MiB writer %d", sized.calls, old.calls)
					}
				}
			}
		})
	}
	if !sawOverCap {
		t.Fatal("no component exceeded the 1 MiB cap: the large case does not exercise it")
	}
}

// buildInt64KeyedLPG builds an int64-keyed graph whose mapper takes the codec
// (non-string) serialisation path.
func buildInt64KeyedLPG(tb testing.TB, nodes int) *lpg.Graph[int64, int64] {
	tb.Helper()
	g := lpg.New[int64, int64](adjlist.Config{})
	for i := 0; i < nodes; i++ {
		if err := g.AddNode(int64(i) * 7); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
	}
	return g
}

// TestCaptureGraph_StagingBuffersAreSmall guards rmp #3083's resource claim: a
// capture of a small graph allocates far less than the 3 MiB its three
// in-memory components' 1 MiB staging buffers alone used to cost. The bytes are
// read from runtime.MemStats.TotalAlloc, a process-wide counter, so the test is
// serial and keeps the smallest of three readings to discard any foreign
// allocation that lands inside one window.
func TestCaptureGraph_StagingBuffersAreSmall(t *testing.T) {
	g := buildPublishBenchLPG(t, 50, 2)
	cs := csr.BuildFromAdjList(g.AdjList())
	const limit = 1 << 20 // one old staging buffer; the old capture cost >= 3x this
	best := uint64(1<<63 - 1)
	for i := 0; i < 3; i++ {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		capt, err := CaptureGraph[string, int64](g, cs, nil, nil)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatalf("CaptureGraph: %v", err)
		}
		if !capt.labels.present || !capt.properties.present || !capt.mapper.present {
			t.Fatal("capture did not serialise labels, properties and mapper; the measurement would be vacuous")
		}
		best = min(best, after.TotalAlloc-before.TotalAlloc)
	}
	if best >= limit {
		t.Fatalf("CaptureGraph of a 50-node graph allocated %d bytes, want < %d", best, limit)
	}
}

// TestChunkSink_JoinsWritesExactly checks that [chunkSink] returns exactly the
// concatenation of its writes, with length and capacity equal to the total,
// across write sizes that land inside one chunk, straddle chunk boundaries and
// exceed the largest chunk.
func TestChunkSink_JoinsWritesExactly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes []int
	}{
		{"none", nil},
		{"one-small", []int{10}},
		{"exactly-first-chunk", []int{chunkSinkFirst}},
		{"straddle", []int{chunkSinkFirst - 1, 2, 3 * chunkSinkFirst}},
		{"many-flushes", []int{4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 1}},
		{"over-max", []int{chunkSinkMax + 17, chunkSinkMax * 2, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink chunkSink
			var want []byte
			for i, n := range tc.sizes {
				p := make([]byte, n)
				for j := range p {
					p[j] = byte(i*31 + j)
				}
				w, err := sink.Write(p)
				if err != nil || w != n {
					t.Fatalf("Write(%d) = %d, %v", n, w, err)
				}
				want = append(want, p...)
			}
			got := sink.bytes()
			if !bytes.Equal(got, want) {
				t.Fatalf("joined %d bytes differ from the %d written", len(got), len(want))
			}
			if len(want) > chunkSinkFirst && cap(got) != len(want) {
				t.Fatalf("cap %d, want exactly %d", cap(got), len(want))
			}
		})
	}
}
