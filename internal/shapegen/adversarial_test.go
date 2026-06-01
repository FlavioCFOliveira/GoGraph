package shapegen

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestAdversarial_IntCorpusBoundaries pins the canonical entries
// the int64 corpus must carry. Codec round-trip tests rely on
// MinInt64 and MaxInt64 being present and at the slice extremities
// for fast boundary indexing.
func TestAdversarial_IntCorpusBoundaries(t *testing.T) {
	t.Parallel()
	ints := AdversarialIntWeights()
	if len(ints) < 4 {
		t.Fatalf("AdversarialIntWeights len = %d, want >= 4", len(ints))
	}
	if ints[0] != math.MinInt64 {
		t.Errorf("ints[0] = %d, want math.MinInt64", ints[0])
	}
	if ints[len(ints)-1] != math.MaxInt64 {
		t.Errorf("ints[len-1] = %d, want math.MaxInt64", ints[len(ints)-1])
	}
	mustContain := []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64}
	seen := make(map[int64]bool, len(ints))
	for _, v := range ints {
		seen[v] = true
	}
	for _, v := range mustContain {
		if !seen[v] {
			t.Errorf("AdversarialIntWeights missing %d", v)
		}
	}
}

// TestAdversarial_FloatCorpusCornerCases pins the IEEE-754 corner
// cases the float corpus must carry: ±Inf, NaN, ±0,
// ±SmallestNonzeroFloat64, ±MaxFloat64.
func TestAdversarial_FloatCorpusCornerCases(t *testing.T) {
	t.Parallel()
	floats := AdversarialFloatWeights()
	var (
		sawPosInf, sawNegInf, sawNaN, sawPosZero, sawNegZero bool
		sawDenormalPos, sawDenormalNeg                       bool
		sawMaxPos, sawMaxNeg                                 bool
	)
	for _, f := range floats {
		switch {
		case math.IsNaN(f):
			sawNaN = true
		case math.IsInf(f, +1):
			sawPosInf = true
		case math.IsInf(f, -1):
			sawNegInf = true
		case f == 0 && math.Signbit(f):
			sawNegZero = true
		case f == 0:
			sawPosZero = true
		case f == math.SmallestNonzeroFloat64:
			sawDenormalPos = true
		case f == -math.SmallestNonzeroFloat64:
			sawDenormalNeg = true
		case f == math.MaxFloat64:
			sawMaxPos = true
		case f == -math.MaxFloat64:
			sawMaxNeg = true
		}
	}
	checks := []struct {
		got  bool
		name string
	}{
		{sawPosInf, "+Inf"},
		{sawNegInf, "-Inf"},
		{sawNaN, "NaN"},
		{sawPosZero, "+0"},
		{sawNegZero, "-0"},
		{sawDenormalPos, "+SmallestNonzeroFloat64"},
		{sawDenormalNeg, "-SmallestNonzeroFloat64"},
		{sawMaxPos, "+MaxFloat64"},
		{sawMaxNeg, "-MaxFloat64"},
	}
	for _, c := range checks {
		if !c.got {
			t.Errorf("AdversarialFloatWeights missing %s", c.name)
		}
	}
}

// TestAdversarial_StringCorpusContents pins the qualitative
// contents of the string corpus: empty, embedded NUL, NFC vs NFD,
// astral plane, 1 MB filler. NFC and NFD entries must be present
// and byte-distinct.
func TestAdversarial_StringCorpusContents(t *testing.T) {
	t.Parallel()
	strs := AdversarialStrings()
	var (
		sawEmpty, sawNULByte, sawAstral, sawMega bool
		sawNFC, sawNFD                           bool
	)
	for _, s := range strs {
		switch {
		case s == "":
			sawEmpty = true
		case len(s) >= 1<<20:
			sawMega = true
		}
		if s == "é" {
			sawNFC = true
		}
		if s == "é" {
			sawNFD = true
		}
		for _, b := range []byte(s) {
			if b == 0 {
				sawNULByte = true
				break
			}
		}
		for _, r := range s {
			if r >= 0x10000 {
				sawAstral = true
				break
			}
		}
	}
	checks := []struct {
		got  bool
		name string
	}{
		{sawEmpty, "empty string"},
		{sawNULByte, "NUL byte"},
		{sawAstral, "astral-plane code point"},
		{sawMega, "1 MB filler"},
		{sawNFC, "NFC \"é\""},
		{sawNFD, "NFD \"é\""},
	}
	for _, c := range checks {
		if !c.got {
			t.Errorf("AdversarialStrings missing %s", c.name)
		}
	}
	if "é" == "é" {
		t.Fatal("NFC and NFD forms of \"é\" must be byte-distinct; runtime equates them")
	}
}

// TestAdversarial_BytesCorpusLengths pins the lengths declared by
// the doc: empty, 1 KB zeros, 1 MB zeros, and a short sentinel-byte
// payload.
func TestAdversarial_BytesCorpusLengths(t *testing.T) {
	t.Parallel()
	bs := AdversarialBytes()
	if len(bs) < 4 {
		t.Fatalf("AdversarialBytes len = %d, want >= 4", len(bs))
	}
	var (
		sawEmpty, sawKB, sawMB, sawSentinel bool
	)
	for _, b := range bs {
		switch len(b) {
		case 0:
			sawEmpty = true
		case 1024:
			if allZero(b) {
				sawKB = true
			}
		case 1 << 20:
			if allZero(b) {
				sawMB = true
			}
		default:
			if bytes.Contains(b, []byte{0xFF}) && bytes.Contains(b, []byte{0x80}) {
				sawSentinel = true
			}
		}
	}
	checks := []struct {
		got  bool
		name string
	}{
		{sawEmpty, "empty []byte"},
		{sawKB, "1 KB all-zero"},
		{sawMB, "1 MB all-zero"},
		{sawSentinel, "sentinel-byte payload"},
	}
	for _, c := range checks {
		if !c.got {
			t.Errorf("AdversarialBytes missing %s", c.name)
		}
	}
}

// allZero reports whether every byte of b equals 0.
func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// TestAdversarial_TimesCorpusEndpoints pins year-1 and the last
// representable second of year-9999.
func TestAdversarial_TimesCorpusEndpoints(t *testing.T) {
	t.Parallel()
	times := AdversarialTimes()
	if len(times) < 2 {
		t.Fatalf("AdversarialTimes len = %d, want >= 2", len(times))
	}
	want1 := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	want9999 := time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC)
	var sawY1, sawY9999 bool
	for _, ts := range times {
		if ts.Equal(want1) {
			sawY1 = true
		}
		if ts.Equal(want9999) {
			sawY9999 = true
		}
	}
	if !sawY1 {
		t.Error("AdversarialTimes missing year 1")
	}
	if !sawY9999 {
		t.Error("AdversarialTimes missing year 9999")
	}
}

// TestAdversarial_GeneratorsAreDeterministic asserts every corpus
// generator returns the same slice across calls (modulo NaN, which
// is never == itself: we compare bitwise on the float corpus).
func TestAdversarial_GeneratorsAreDeterministic(t *testing.T) {
	t.Parallel()
	if !reflect.DeepEqual(AdversarialIntWeights(), AdversarialIntWeights()) {
		t.Fatal("AdversarialIntWeights is not deterministic")
	}
	f1, f2 := AdversarialFloatWeights(), AdversarialFloatWeights()
	if len(f1) != len(f2) {
		t.Fatalf("AdversarialFloatWeights lengths diverge: %d vs %d", len(f1), len(f2))
	}
	for i := range f1 {
		if math.Float64bits(f1[i]) != math.Float64bits(f2[i]) {
			t.Fatalf("AdversarialFloatWeights[%d] bitwise diverges: %x vs %x",
				i, math.Float64bits(f1[i]), math.Float64bits(f2[i]))
		}
	}
	if !reflect.DeepEqual(AdversarialStrings(), AdversarialStrings()) {
		t.Fatal("AdversarialStrings is not deterministic")
	}
	if !reflect.DeepEqual(AdversarialBytes(), AdversarialBytes()) {
		t.Fatal("AdversarialBytes is not deterministic")
	}
	if !reflect.DeepEqual(AdversarialTimes(), AdversarialTimes()) {
		t.Fatal("AdversarialTimes is not deterministic")
	}
	if !reflect.DeepEqual(AdversarialBools(), AdversarialBools()) {
		t.Fatal("AdversarialBools is not deterministic")
	}
}

// TestAdversarial_ApplyNilGraphIsNoOp ensures ApplyAdversarialProps
// tolerates a nil graph without panicking — guards against callers
// that forward an unconstructed handle.
func TestAdversarial_ApplyNilGraphIsNoOp(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ApplyAdversarialProps(nil, _) panicked: %v", r)
		}
	}()
	ApplyAdversarialProps[int, int64](nil, AllMix())
}

// TestAdversarial_ApplyIsIdempotent exercises the AC #2 property
// (idempotency): a second call with the same Mix on the same graph
// produces the same per-node and per-edge property bags. The test
// uses pgregory.net/rapid to sweep small Path-like shapes so the
// invariant holds across many topologies.
func TestAdversarial_ApplyIsIdempotent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		n := rapid.IntRange(2, 12).Draw(r, "n")
		mix := drawMix(r)
		g1 := buildAdversarialFixture(t, n)
		ApplyAdversarialProps(g1, mix)
		snapshotA := snapshotAllProps(g1, n)
		ApplyAdversarialProps(g1, mix)
		snapshotB := snapshotAllProps(g1, n)
		if !propsEqual(snapshotA, snapshotB) {
			t.Fatalf("ApplyAdversarialProps not idempotent for n=%d, mix=%+v", n, mix)
		}
	})
}

// TestAdversarial_NFCRoundTripsByteEqual exercises AC #3: a string
// supplied in NFC ("é" = U+00E9) round-trips through
// SetNodeProperty/GetNodeProperty byte-equal. The companion NFD
// form ("e" + U+0301) is also pinned: the LPG layer does not
// normalise.
func TestAdversarial_NFCRoundTripsByteEqual(t *testing.T) {
	t.Parallel()
	g := lpg.New[int, int64](defaultCfg)
	if err := g.SetNodeProperty(0, "nfc", lpg.StringValue("é")); err != nil {
		t.Fatalf("Set NFC: %v", err)
	}
	if err := g.SetNodeProperty(1, "nfd", lpg.StringValue("é")); err != nil {
		t.Fatalf("Set NFD: %v", err)
	}
	v, ok := g.GetNodeProperty(0, "nfc")
	if !ok {
		t.Fatal("nfc property missing")
	}
	got, ok := v.String()
	if !ok || got != "é" {
		t.Fatalf("Get NFC = %q ok=%v, want %q", got, ok, "é")
	}
	if !bytes.Equal([]byte(got), []byte("é")) {
		t.Fatalf("Get NFC bytes = % x, want % x", []byte(got), []byte("é"))
	}
	v, ok = g.GetNodeProperty(1, "nfd")
	if !ok {
		t.Fatal("nfd property missing")
	}
	got, ok = v.String()
	if !ok || got != "é" {
		t.Fatalf("Get NFD = %q ok=%v, want %q", got, ok, "é")
	}
	if !bytes.Equal([]byte(got), []byte("é")) {
		t.Fatalf("Get NFD bytes = % x, want % x", []byte(got), []byte("é"))
	}
}

// TestAdversarial_AllMixCoverage confirms AllMix enables every
// kind: the corresponding boolean flags are set.
func TestAdversarial_AllMixCoverage(t *testing.T) {
	t.Parallel()
	m := AllMix()
	if !m.Ints || !m.Floats || !m.Strings || !m.Bytes || !m.Times || !m.Bools {
		t.Fatalf("AllMix() = %+v, want every flag true", m)
	}
}

// TestAdversarial_ApplyStampsExpectedKeys confirms that
// ApplyAdversarialProps lands the expected key names on a small
// graph: every node carries adv.int/.float/.string/.bytes/.time/
// .bool, and every edge does too.
func TestAdversarial_ApplyStampsExpectedKeys(t *testing.T) {
	t.Parallel()
	const n = 4
	g := buildAdversarialFixture(t, n)
	ApplyAdversarialProps(g, AllMix())
	wantKeys := []string{AdvKeyInt, AdvKeyFloat, AdvKeyString, AdvKeyBytes, AdvKeyTime, AdvKeyBool}
	for v := 0; v < n; v++ {
		for _, k := range wantKeys {
			if _, ok := g.GetNodeProperty(v, k); !ok {
				t.Errorf("node %d missing %s", v, k)
			}
		}
	}
	adj := g.AdjList()
	saw := 0
	for v := 0; v < n; v++ {
		for w := range adj.Neighbours(v) {
			for _, k := range wantKeys {
				if _, ok := g.GetEdgeProperty(v, w, k); !ok {
					t.Errorf("edge (%d,%d) missing %s", v, w, k)
				}
			}
			saw++
		}
	}
	if saw == 0 {
		t.Fatal("fixture has no edges; cannot validate edge-property coverage")
	}
}

// buildAdversarialFixture returns a small directed path-and-back
// graph with int keys 0..n-1 used by the idempotency property tests.
func buildAdversarialFixture(t *testing.T, n int) *lpg.Graph[int, int64] {
	t.Helper()
	g := lpg.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := g.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	for i := 0; i < n-1; i++ {
		if err := g.AddEdge(i, i+1, int64(i)); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", i, i+1, err)
		}
	}
	if n >= 2 {
		if err := g.AddEdge(n-1, 0, int64(n)); err != nil {
			t.Fatalf("AddEdge(%d,0): %v", n-1, err)
		}
	}
	return g
}

// propSnapshot is the readable form of a node's or edge's property
// bag used by the idempotency comparison.
type propSnapshot struct {
	nodes map[int]map[string]lpg.PropertyValue
	edges map[[2]int]map[string]lpg.PropertyValue
}

// snapshotAllProps returns a deep copy of every node and edge
// property bag in g, keyed by (node id) and (src, dst).
func snapshotAllProps(g *lpg.Graph[int, int64], n int) propSnapshot {
	out := propSnapshot{
		nodes: make(map[int]map[string]lpg.PropertyValue, n),
		edges: make(map[[2]int]map[string]lpg.PropertyValue, n),
	}
	adj := g.AdjList()
	for v := 0; v < n; v++ {
		bag := g.NodeProperties(v)
		out.nodes[v] = bag
		for w := range adj.Neighbours(v) {
			out.edges[[2]int{v, w}] = g.EdgeProperties(v, w)
		}
	}
	return out
}

// propsEqual compares two snapshots value-by-value. NaN entries are
// compared on bit pattern (NaN != NaN under ==).
func propsEqual(a, b propSnapshot) bool {
	if len(a.nodes) != len(b.nodes) || len(a.edges) != len(b.edges) {
		return false
	}
	for k, av := range a.nodes {
		if !bagEqual(av, b.nodes[k]) {
			return false
		}
	}
	for k, av := range a.edges {
		if !bagEqual(av, b.edges[k]) {
			return false
		}
	}
	return true
}

func bagEqual(a, b map[string]lpg.PropertyValue) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if !propValueEqual(av, bv) {
			return false
		}
	}
	return true
}

func propValueEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		as, _ := a.String()
		bs, _ := b.String()
		return as == bs
	case lpg.PropInt64:
		ai, _ := a.Int64()
		bi, _ := b.Int64()
		return ai == bi
	case lpg.PropFloat64:
		af, _ := a.Float64()
		bf, _ := b.Float64()
		return math.Float64bits(af) == math.Float64bits(bf)
	case lpg.PropBool:
		ab, _ := a.Bool()
		bb, _ := b.Bool()
		return ab == bb
	case lpg.PropTime:
		at, _ := a.Time()
		bt, _ := b.Time()
		return at.Equal(bt)
	case lpg.PropBytes:
		ab, _ := a.Bytes()
		bb, _ := b.Bytes()
		return bytes.Equal(ab, bb)
	}
	return false
}

// drawMix samples a random Mix using rapid. At least one flag is
// always enabled so the test exercises a non-empty property write.
func drawMix(r *rapid.T) Mix {
	for attempt := 0; attempt < 8; attempt++ {
		m := Mix{
			Ints:    rapid.Bool().Draw(r, "ints"),
			Floats:  rapid.Bool().Draw(r, "floats"),
			Strings: rapid.Bool().Draw(r, "strings"),
			Bytes:   rapid.Bool().Draw(r, "bytes"),
			Times:   rapid.Bool().Draw(r, "times"),
			Bools:   rapid.Bool().Draw(r, "bools"),
		}
		if m.Ints || m.Floats || m.Strings || m.Bytes || m.Times || m.Bools {
			return m
		}
	}
	return AllMix()
}
