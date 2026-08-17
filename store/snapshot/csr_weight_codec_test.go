package snapshot

// csr_weight_codec_test.go — regression tests for rmp #2526: a weight the CSR
// writer cannot size must never be persisted as "no weights".
//
// The defect these pin: csrWeightSize returns 0 for every weight type outside a
// hardcoded set of Go primitives, and 0 already meant "this graph has no
// weights". So a graph weighted by a struct, or by a NAMED integer type such as
// time.Duration, serialised to the exact bytes of a weightless graph, reported
// success, and the checkpoint then truncated the WAL prefix holding the only
// other copy.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ---- the weight types under test ---------------------------------------

// latency is a NAMED integer type — the natural weight for a routing or
// latency graph, and the case most likely to be met in practice, because a Go
// type switch matches exact types and so never matches it.
type latency time.Duration

// money is a struct weight, the case txn.NewBinaryMarshalerWeightCodec exists
// to persist.
type money struct {
	Cents int64
	Rate  float64
}

var errWeightProbe = errors.New("test: malformed weight payload")

// latencyCodec is a varint codec for [latency]: deliberately VARIABLE width, so
// the offsets array is genuinely exercised rather than degenerating into a
// uniform stride that a fixed-width reader could accidentally get right.
type latencyCodec struct{}

func (latencyCodec) Encode(buf []byte, w latency) ([]byte, error) {
	return binary.AppendVarint(buf, int64(w)), nil
}

func (latencyCodec) Decode(buf []byte) (latency, []byte, error) {
	v, n := binary.Varint(buf)
	if n <= 0 {
		return 0, buf, errWeightProbe
	}
	return latency(v), buf[n:], nil
}

// moneyCodec is a fixed-16-byte codec for [money].
type moneyCodec struct{}

func (moneyCodec) Encode(buf []byte, w money) ([]byte, error) {
	buf = binary.LittleEndian.AppendUint64(buf, uint64(w.Cents))
	return binary.LittleEndian.AppendUint64(buf, uint64(int64(w.Rate*1e6))), nil
}

func (moneyCodec) Decode(buf []byte) (money, []byte, error) {
	if len(buf) < 16 {
		return money{}, buf, errWeightProbe
	}
	return money{
		Cents: int64(binary.LittleEndian.Uint64(buf[0:8])),
		Rate:  float64(int64(binary.LittleEndian.Uint64(buf[8:16]))) / 1e6,
	}, buf[16:], nil
}

// ---- helpers -----------------------------------------------------------

func buildWeightedCSR[W any](t *testing.T, edges map[[2]string]W) *csr.CSR[W] {
	t.Helper()
	a := adjlist.New[string, W](adjlist.Config{Directed: true})
	for k, w := range edges {
		if err := a.AddEdge(k[0], k[1], w); err != nil {
			t.Fatalf("AddEdge %v: %v", k, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	if c.WeightsSlice() == nil {
		t.Fatal("precondition: CSR carries no weights slice")
	}
	return c
}

// ---- the core defect ---------------------------------------------------

// TestCSRWeights_UnsizableTypeRefusesRatherThanEliding is THE #2526 regression.
// Before the fix WriteCSR returned (size, crc, nil) here and emitted
// hasWeights=0; the checkpoint that called it then truncated the WAL and the
// weights were gone for good.
func TestCSRWeights_UnsizableTypeRefusesRatherThanEliding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (int64, error)
	}{
		{"named integer (time.Duration alias)", func(t *testing.T) (int64, error) {
			c := buildWeightedCSR(t, map[[2]string]latency{
				{"a", "b"}: latency(11 * time.Millisecond),
				{"a", "c"}: latency(22 * time.Millisecond),
			})
			n, _, err := WriteCSR(new(bytes.Buffer), c)
			return n, err
		}},
		{"struct", func(t *testing.T) (int64, error) {
			c := buildWeightedCSR(t, map[[2]string]money{
				{"a", "b"}: {Cents: 1250, Rate: 1.5},
			})
			n, _, err := WriteCSR(new(bytes.Buffer), c)
			return n, err
		}},
		{"string", func(t *testing.T) (int64, error) {
			c := buildWeightedCSR(t, map[[2]string]string{{"a", "b"}: "heavy"})
			n, _, err := WriteCSR(new(bytes.Buffer), c)
			return n, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			size, err := tc.run(t)
			if !errors.Is(err, ErrWeightNotPersistable) {
				t.Fatalf("WriteCSR on an unpersistable weight returned (size=%d, err=%v),"+
					" want ErrWeightNotPersistable. Returning success here writes a"+
					" byte-for-byte WEIGHTLESS snapshot, after which the caller's"+
					" checkpoint truncates the WAL prefix that still holds the real"+
					" weights and the loss becomes permanent (rmp #2526)", size, err)
			}
		})
	}
}

// TestCSRWeights_AllZeroUnsizableWeightsAreElidedNotRefused pins the ONE case
// where recording no weights is truthful rather than lossy: a store built
// without a weight codec, where txn.ErrNoWeightCodec already rejected every
// non-zero weight, so every weight really is the zero value.
//
// Without this the refusal above would break a legitimate configuration to
// prevent a loss that cannot happen.
func TestCSRWeights_AllZeroUnsizableWeightsAreElidedNotRefused(t *testing.T) {
	t.Parallel()
	c := buildWeightedCSR(t, map[[2]string]money{
		{"a", "b"}: {},
		{"a", "c"}: {},
	})
	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, c); err != nil {
		t.Fatalf("WriteCSR on all-zero unsizable weights: %v, want nil"+
			" (nothing is lost by recording no weights when every weight IS the zero value)", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if rb.HasWeights {
		t.Fatalf("all-zero unsizable weights: HasWeights=true, want false")
	}
}

// ---- the round trip ----------------------------------------------------

// TestCSRWeights_CodecRoundTrip proves the weights survive the whole
// write -> read -> apply chain, which is the path a checkpoint + recovery
// takes, and that they come back attached to the RIGHT edges.
func TestCSRWeights_CodecRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("variable width (varint)", func(t *testing.T) {
		t.Parallel()
		want := map[[2]string]latency{
			{"a", "b"}: latency(1),                     // 1-byte varint
			{"a", "c"}: latency(11 * time.Millisecond), // 4-byte varint
			{"b", "c"}: latency(99 * time.Hour),        // 7-byte varint
			{"c", "a"}: latency(0),                     // 1-byte varint
		}
		assertWeightRoundTrip(t, want, latencyCodec{}, latencyCodec{})
	})

	t.Run("fixed width struct", func(t *testing.T) {
		t.Parallel()
		want := map[[2]string]money{
			{"a", "b"}: {Cents: 1250, Rate: 1.5},
			{"a", "c"}: {Cents: -99, Rate: 0.25},
			{"b", "c"}: {},
		}
		assertWeightRoundTrip(t, want, moneyCodec{}, moneyCodec{})
	})
}

// assertWeightRoundTrip writes edges through enc, reads them back, applies them
// to a fresh graph through dec, and requires every edge to carry the weight it
// was written with.
func assertWeightRoundTrip[W comparable](
	t *testing.T, edges map[[2]string]W, enc weightEncoder[W], dec weightDecoder[W],
) {
	t.Helper()
	c := buildWeightedCSR(t, edges)

	var buf bytes.Buffer
	if _, _, err := WriteCSRWithWeightCodec(&buf, c, enc); err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if !rb.CodecWeights() {
		t.Fatalf("readback is not codec-encoded: HasWeights=%v WeightSize=%d,"+
			" want HasWeights=true WeightSize=%d", rb.HasWeights, rb.WeightSize, weightSizeCodec)
	}
	if got, want := len(rb.WeightOffsets), len(rb.Edges)+1; got != want {
		t.Fatalf("offsets length %d, want %d (edges+1)", got, want)
	}

	// Apply into a graph whose mapper already holds the same keys, so every
	// edge resolves and the weights land where they can be read back by key.
	g := lpg.New[string, W](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(g, mapperReadbackFor(t, c, edges)); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}
	if err := ApplyCSRToGraphWithWeightCodec(g, &rb, dec); err != nil {
		t.Fatalf("ApplyCSRToGraphWithWeightCodec: %v", err)
	}
	for k, w := range edges {
		got, ok := g.EdgeWeight(k[0], k[1])
		if !ok {
			t.Errorf("edge %s->%s did not come back at all", k[0], k[1])
			continue
		}
		if got != w {
			t.Errorf("edge %s->%s came back with weight %v, want %v", k[0], k[1], got, w)
		}
	}
}

// mapperReadbackFor builds the (NodeID -> key) readback matching c's own
// mapper, so the apply step resolves every endpoint.
func mapperReadbackFor[W any](t *testing.T, _ *csr.CSR[W], edges map[[2]string]W) MapperReadback {
	t.Helper()
	// Rebuild an equivalent adjacency to recover the same interning order the
	// CSR was built from; the mapper assigns ids by first-touch, and both
	// graphs touch the same keys in the same map-iteration-independent order
	// because AddEdge interns src then dst per edge.
	a := adjlist.New[string, W](adjlist.Config{Directed: true})
	for k, w := range edges {
		if err := a.AddEdge(k[0], k[1], w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	var rb MapperReadback
	a.Mapper().Walk(func(id graph.NodeID, key string) bool {
		rb.Pairs = append(rb.Pairs, MapperPair{ID: id, Key: key})
		return true
	})
	return rb
}

// ---- the read-side half of the safety property -------------------------

// TestCSRWeights_ApplyWithoutCodecRefuses proves the read side refuses rather
// than degrading: weights that ARE durably on disk must not be silently
// replaced by zeros because the caller forgot a codec.
func TestCSRWeights_ApplyWithoutCodecRefuses(t *testing.T) {
	t.Parallel()
	c := buildWeightedCSR(t, map[[2]string]money{{"a", "b"}: {Cents: 7}})
	var buf bytes.Buffer
	if _, _, err := WriteCSRWithWeightCodec(&buf, c, moneyCodec{}); err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	g := lpg.New[string, money](adjlist.Config{Directed: true})
	err = ApplyCSRToGraph(g, &rb)
	if !errors.Is(err, ErrWeightCodecRequired) {
		t.Fatalf("ApplyCSRToGraph on a codec-encoded snapshot without a codec returned %v,"+
			" want ErrWeightCodecRequired. Applying zero weights instead would discard"+
			" values that are durably present on disk (rmp #2526)", err)
	}
}

// ---- byte-compatibility ------------------------------------------------

// TestCSRWeights_FixedWidthBytesUnchangedByCodec is the compatibility claim
// that lets this change ship without a format version step: for every weight
// type the dense native layout can size, the bytes are IDENTICAL whether or not
// a codec is supplied. The codec is consulted only where the dense layout
// cannot work, so no existing snapshot's encoding moves.
func TestCSRWeights_FixedWidthBytesUnchangedByCodec(t *testing.T) {
	t.Parallel()

	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		c := buildWeightedCSR(t, map[[2]string]float64{
			{"a", "b"}: 1.5, {"a", "c"}: -2.25, {"b", "c"}: 0,
		})
		assertBytesEqualWithAndWithoutCodec[float64](t, c, float64PanicCodec{})
	})

	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		c := buildWeightedCSR(t, map[[2]string]int64{
			{"a", "b"}: 11, {"a", "c"}: -22, {"b", "c"}: 0,
		})
		assertBytesEqualWithAndWithoutCodec[int64](t, c, int64PanicCodec{})
	})
}

// float64PanicCodec / int64PanicCodec fail the test if they are ever called.
// That is the sharper assertion: it proves the fixed-width path does not merely
// PRODUCE the same bytes through the codec, it never consults the codec at all.
type float64PanicCodec struct{}

func (float64PanicCodec) Encode([]byte, float64) ([]byte, error) {
	return nil, errors.New("codec consulted for a fixed-width weight type")
}

type int64PanicCodec struct{}

func (int64PanicCodec) Encode([]byte, int64) ([]byte, error) {
	return nil, errors.New("codec consulted for a fixed-width weight type")
}

func assertBytesEqualWithAndWithoutCodec[W any](t *testing.T, c *csr.CSR[W], enc weightEncoder[W]) {
	t.Helper()
	var plain, withCodec bytes.Buffer
	sizeA, crcA, err := WriteCSR(&plain, c)
	if err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	sizeB, crcB, err := WriteCSRWithWeightCodec(&withCodec, c, enc)
	if err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v (the codec must not have been consulted"+
			" for a fixed-width weight type)", err)
	}
	if sizeA != sizeB || crcA != crcB || !bytes.Equal(plain.Bytes(), withCodec.Bytes()) {
		t.Fatalf("supplying a weight codec changed the bytes of a fixed-width weight column:"+
			" (size=%d crc=%08x) vs (size=%d crc=%08x). Existing snapshots would need a"+
			" migration, which this change is designed to avoid", sizeA, crcA, sizeB, crcB)
	}
}

// ---- structural validation of the new section --------------------------

// TestCSRWeights_UnknownWidthByteIsRejected pins the range check the reader
// performs BEFORE dispatching on the width byte. Without it an unknown width —
// one flipped bit in an 8 — silently took the dense path and mis-sliced the
// whole column.
func TestCSRWeights_UnknownWidthByteIsRejected(t *testing.T) {
	t.Parallel()
	c := buildWeightedCSR(t, map[[2]string]float64{{"a", "b"}: 1.5, {"a", "c"}: 2.5})
	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, c); err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	for _, bad := range []uint8{3, 5, 6, 7, 9, 16, 100, 254} {
		raw := append([]byte(nil), buf.Bytes()...)
		raw[17] = bad // weightSizeBytes is the 18th byte: 8 + 8 + hasWeights
		_, err := ReadCSR(bytes.NewReader(raw))
		if !errors.Is(err, ErrCSRCorrupted) {
			t.Errorf("ReadCSR with weight width %d returned %v, want ErrCSRCorrupted", bad, err)
		}
	}
}

// TestCSRWeights_CorruptOffsetsRejected proves the offsets array is validated
// structurally at parse time, so the per-slot slice in the apply loop is
// unconditionally in range and a hostile file cannot drive it out of bounds.
func TestCSRWeights_CorruptOffsetsRejected(t *testing.T) {
	t.Parallel()
	c := buildWeightedCSR(t, map[[2]string]latency{
		{"a", "b"}: latency(1), {"a", "c"}: latency(1 << 40), {"b", "c"}: latency(7),
	})
	var buf bytes.Buffer
	if _, _, err := WriteCSRWithWeightCodec(&buf, c, latencyCodec{}); err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v", err)
	}
	base := buf.Bytes()
	rb, err := ReadCSR(bytes.NewReader(base))
	if err != nil {
		t.Fatalf("ReadCSR of the intact file: %v", err)
	}
	// The offsets array starts after the 18-byte header, the vertices array and
	// the edges array.
	offAt := 18 + 8*len(rb.Vertices) + 8*len(rb.Edges)

	for _, tc := range []struct {
		name  string
		index int
		value uint64
	}{
		{"offsets[0] not zero", 0, 1},
		// offsets[2] < offsets[1]. Note EQUAL consecutive offsets are legal —
		// they denote a zero-length encoded value — so a genuine violation has
		// to go backwards, matching Arrow's rule that offsets are
		// non-decreasing (offsets[j+1] >= offsets[j]), not strictly increasing.
		{"non-monotonic", 2, 0},
		{"payload length beyond the file", len(rb.WeightOffsets) - 1, 1 << 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := append([]byte(nil), base...)
			binary.LittleEndian.PutUint64(raw[offAt+8*tc.index:], tc.value)
			_, err := ReadCSR(bytes.NewReader(raw))
			if !errors.Is(err, ErrCSRCorrupted) {
				t.Fatalf("ReadCSR with %s returned %v, want ErrCSRCorrupted", tc.name, err)
			}
		})
	}
}

// TestCSRWeights_DecoderLeavingATailIsRejected proves a codec that disagrees
// with the writer about a value's extent is reported rather than tolerated.
// Tolerating it would let a drifted codec return a silently wrong weight.
func TestCSRWeights_DecoderLeavingATailIsRejected(t *testing.T) {
	t.Parallel()
	edges := map[[2]string]money{{"a", "b"}: {Cents: 7}}
	c := buildWeightedCSR(t, edges)
	var buf bytes.Buffer
	if _, _, err := WriteCSRWithWeightCodec(&buf, c, moneyCodec{}); err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	g := lpg.New[string, money](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(g, mapperReadbackFor(t, c, edges)); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}
	err = ApplyCSRToGraphWithWeightCodec(g, &rb, shortMoneyCodec{})
	if !errors.Is(err, ErrCSRCorrupted) {
		t.Fatalf("apply with a decoder that under-consumes returned %v, want ErrCSRCorrupted", err)
	}
}

// shortMoneyCodec consumes only 8 of the 16 bytes moneyCodec writes.
type shortMoneyCodec struct{}

func (shortMoneyCodec) Decode(buf []byte) (money, []byte, error) {
	if len(buf) < 8 {
		return money{}, buf, errWeightProbe
	}
	return money{Cents: int64(binary.LittleEndian.Uint64(buf[0:8]))}, buf[8:], nil
}

// ---- forward-compatibility of the sentinel -----------------------------

// TestCSRWeights_SentinelIsNotAHistoricalWidth is the compatibility argument
// made executable: 0xFF is safe to reuse as a layout selector ONLY because no
// writer ever emitted it as a width. csrWeightSize is the sole source of widths
// and this pins its whole range.
func TestCSRWeights_SentinelIsNotAHistoricalWidth(t *testing.T) {
	t.Parallel()
	widths := map[uint8]string{
		csrWeightSize[struct{}](): "struct{}",
		csrWeightSize[int8]():     "int8",
		csrWeightSize[uint8]():    "uint8",
		csrWeightSize[bool]():     "bool",
		csrWeightSize[int16]():    "int16",
		csrWeightSize[uint16]():   "uint16",
		csrWeightSize[int32]():    "int32",
		csrWeightSize[uint32]():   "uint32",
		csrWeightSize[float32]():  "float32",
		csrWeightSize[int]():      "int",
		csrWeightSize[uint]():     "uint",
		csrWeightSize[int64]():    "int64",
		csrWeightSize[uint64]():   "uint64",
		csrWeightSize[float64]():  "float64",
		csrWeightSize[uintptr]():  "uintptr",
	}
	if name, clash := widths[weightSizeCodec]; clash {
		t.Fatalf("weight type %s has native width %d, which collides with weightSizeCodec:"+
			" an older reader could not distinguish a codec-encoded section from a dense"+
			" one, and the sentinel would have to change", name, weightSizeCodec)
	}
	for w, name := range widths {
		if w != 0 && !validCSRWeightSize(w) {
			t.Errorf("csrWeightSize[%s] = %d is not accepted by validCSRWeightSize:"+
				" the reader would reject a file this writer can produce", name, w)
		}
	}
}

// TestCSRWeights_OldReaderRejectsNewFile documents, executably, what an older
// binary does when handed a codec-encoded snapshot: it must FAIL, not misread.
//
// The old reader's behaviour is reproduced exactly — it had no range check on
// the width byte, so it reached the width guard in ApplyCSRToGraph, which
// compares the on-disk width against csrWeightSize[W]() for the store's own W.
// 0xFF cannot equal any value that function returns, so the comparison always
// fails and the old reader always errors.
func TestCSRWeights_OldReaderRejectsNewFile(t *testing.T) {
	t.Parallel()
	for _, w := range []uint8{
		csrWeightSize[float64](), csrWeightSize[int64](), csrWeightSize[int32](),
		csrWeightSize[int16](), csrWeightSize[int8](), csrWeightSize[money](),
	} {
		if w == weightSizeCodec {
			t.Fatalf("native width %d equals the codec sentinel: an old reader's width"+
				" guard would ACCEPT a codec-encoded file and misread the whole column", w)
		}
	}
	// And the modern reader refuses it earlier still, at parse time, when the
	// declared width is not one it knows.
	if validCSRWeightSize(0) {
		t.Error("width 0 must not be accepted as a valid weights-section width:" +
			" hasWeights=1 with width 0 is not a layout any writer produces")
	}
}
