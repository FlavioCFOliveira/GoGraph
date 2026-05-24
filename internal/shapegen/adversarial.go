package shapegen

import (
	"math"
	"strings"
	"time"

	"gograph/graph"
	"gograph/graph/lpg"
)

// adversarial.go provides deterministic generators for "adversarial"
// property values that exercise every codec, WAL, snapshot and
// recovery path in GoGraph under the most demanding inputs: integer
// extremes (math.MaxInt64, math.MinInt64), IEEE-754 corner cases
// (NaN, ±Inf, denormals, -0), Unicode strings with NUL bytes and
// astral-plane code points (both NFC and NFD forms), bytes payloads
// of all zeros, time values near year 1 and year 9999, and
// strings/bytes of multi-megabyte length.
//
// The corpora are pure functions of their (empty) input — the
// returned slices are deterministic across calls and platforms —
// so goldens and property tests can pin specific entries by index.
//
// # Idempotency
//
// [ApplyAdversarialProps] is idempotent: a second call with the
// same arguments rewrites the same (key, value) pairs over
// themselves. Property-based tests pin this invariant.
//
// # NFC byte-equality
//
// The string corpus includes both NFC and NFD forms of the same
// abstract character ("é" as U+00E9 vs "e" + U+0301). The
// catalogue guarantees byte-by-byte preservation through
// SetNodeProperty/GetNodeProperty and through the persistence
// layer: callers receive back exactly the bytes they supplied,
// with no Unicode normalisation pass.
//
// # Length tiers
//
// The corpus tier covers three length scales:
//
//   - Trivial: empty string, empty []byte.
//   - Common: short ASCII, mixed-script Unicode, a handful of
//     extreme code points.
//   - Stress: a 1 MB ASCII filler string and 1 KB / 1 MB all-zero
//     byte payloads. These exist so codec/WAL tests in the same
//     sprint can validate that nothing in the persistence pipeline
//     truncates or rejects multi-megabyte values.

// AdversarialIntWeights returns the canonical int64 adversarial
// soup: signed extremes, signed extremes ± 1, ± 1, 0, ± 2^62, and
// 0 once (so the corpus is non-empty when callers index by id %
// len).
func AdversarialIntWeights() []int64 {
	return []int64{
		math.MinInt64,
		math.MinInt64 + 1,
		-(1 << 62),
		-1,
		0,
		1,
		1 << 62,
		math.MaxInt64 - 1,
		math.MaxInt64,
	}
}

// AdversarialFloatWeights returns the canonical float64
// adversarial soup, including both infinities, both signs of zero,
// the largest and smallest representable finite magnitudes, and a
// quiet NaN. Callers comparing NaN entries must use math.IsNaN
// rather than ==.
func AdversarialFloatWeights() []float64 {
	negZero := math.Copysign(0, -1)
	return []float64{
		math.Inf(-1),
		-math.MaxFloat64,
		-1.0,
		-math.SmallestNonzeroFloat64,
		negZero,
		0,
		math.SmallestNonzeroFloat64,
		1.0,
		math.MaxFloat64,
		math.Inf(+1),
		math.NaN(),
	}
}

// AdversarialStrings returns the canonical string adversarial
// soup: empty string, a short ASCII control, NUL-embedded bytes,
// all-NUL payloads, mixed-script Unicode, the NFC and NFD forms of
// "é", a BMP emoji, a triple of mathematical-bold capitals (astral
// plane code points), and a 1 MB ASCII filler string. The NFC and
// NFD entries occupy adjacent positions so callers indexing by
// (id % len) cover both forms.
func AdversarialStrings() []string {
	megaA := strings.Repeat("a", 1<<20)
	return []string{
		"",
		"ascii",
		"a\x00b",
		"\x00\x00\x00",
		"líneas con tildes ñ ü ç",
		"é",                              // NFC: U+00E9
		"é",                             // NFD: e + U+0301
		"\U0001F600",                     // 😀
		"\U0001D400\U0001D401\U0001D402", // 𝐀 𝐁 𝐂 (mathematical bold)
		megaA,
	}
}

// AdversarialBytes returns the canonical []byte adversarial soup:
// the empty slice, a 1 KB all-zero payload, a 1 MB all-zero
// payload, and a short slice of selected single-byte values
// (boundary cases for varint and length-prefixed codecs).
func AdversarialBytes() [][]byte {
	return [][]byte{
		{},
		make([]byte, 1024),
		make([]byte, 1<<20),
		{0x00, 0xFF, 0x7F, 0x80, 0x01, 0xFE},
	}
}

// AdversarialTimes returns the canonical time.Time adversarial
// soup: year 1, the Unix epoch, the Y2038 boundary, and the last
// representable second of year 9999.
func AdversarialTimes() []time.Time {
	return []time.Time{
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(0, 0).UTC(),
		time.Date(2038, 1, 19, 3, 14, 7, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
	}
}

// AdversarialBools returns both boolean values, in canonical
// (false, true) order.
func AdversarialBools() []bool {
	return []bool{false, true}
}

// Mix selects which adversarial property kinds [ApplyAdversarialProps]
// stamps on each node and edge.
type Mix struct {
	Ints, Floats, Strings, Bytes, Times, Bools bool
}

// AllMix returns a [Mix] with every kind enabled.
func AllMix() Mix {
	return Mix{Ints: true, Floats: true, Strings: true, Bytes: true, Times: true, Bools: true}
}

// Property-key constants used by [ApplyAdversarialProps] so the
// same call site rewrites the same key on every invocation,
// preserving idempotency.
const (
	AdvKeyInt    = "adv.int"
	AdvKeyFloat  = "adv.float"
	AdvKeyString = "adv.string"
	AdvKeyBytes  = "adv.bytes"
	AdvKeyTime   = "adv.time"
	AdvKeyBool   = "adv.bool"
)

// ApplyAdversarialProps decorates every node and edge of g with a
// deterministic selection of adversarial properties chosen by mix.
// Calling this function a second time with the same (g, mix) is a
// no-op (idempotent): every (key, value) pair is recomputed
// deterministically from the node id and the edge iteration index,
// so re-writing yields the same shard contents.
//
// Node bucket: AdversarialXxx()[id % len(corpus)].
// Edge bucket: AdversarialXxx()[(srcID*7 + edgeIdx) % len(corpus)],
// where edgeIdx walks the in-iteration order of
// [adjlist.AdjList.Neighbours]. Multiplying by 7 mixes the source
// id into the edge bucket so adjacent edges of the same source
// do not all collide on the same corpus entry.
func ApplyAdversarialProps[N comparable, W any](g *lpg.Graph[N, W], mix Mix) {
	if g == nil {
		return
	}
	ints := AdversarialIntWeights()
	floats := AdversarialFloatWeights()
	strs := AdversarialStrings()
	bs := AdversarialBytes()
	times := AdversarialTimes()
	bools := AdversarialBools()
	adj := g.AdjList()
	maxID := uint64(adj.MaxNodeID())
	keys := make([]N, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		k, ok := adj.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		keys = append(keys, k)
	}
	for idx, key := range keys {
		applyAdversarialNode(g, key, idx, mix, ints, floats, strs, bs, times, bools)
	}
	for srcIdx, src := range keys {
		edgeIdx := 0
		for dst := range adj.Neighbours(src) {
			applyAdversarialEdge(g, src, dst, srcIdx, edgeIdx, mix, ints, floats, strs, bs, times, bools)
			edgeIdx++
		}
	}
}

// applyAdversarialNode stamps one node with the per-kind
// adversarial selection picked by idx. SetNodeProperty errors are
// only reachable when the underlying [adjlist.AdjList] is at
// shard capacity; in that case the caller is exercising the
// shard-full path explicitly and we drop the error here so the
// per-node loop completes without aborting the rest of the
// adversarial soup.
//
//nolint:gocritic // paramTypeCombine: the (ints, floats, strs, ...) tail mirrors the AdversarialXxx() corpus order; combining types would obscure intent.
func applyAdversarialNode[N comparable, W any](
	g *lpg.Graph[N, W], key N, idx int, mix Mix,
	ints []int64, floats []float64, strs []string, bs [][]byte, times []time.Time, bools []bool,
) {
	if mix.Ints {
		_ = g.SetNodeProperty(key, AdvKeyInt, lpg.Int64Value(ints[idx%len(ints)]))
	}
	if mix.Floats {
		_ = g.SetNodeProperty(key, AdvKeyFloat, lpg.Float64Value(floats[idx%len(floats)]))
	}
	if mix.Strings {
		_ = g.SetNodeProperty(key, AdvKeyString, lpg.StringValue(strs[idx%len(strs)]))
	}
	if mix.Bytes {
		_ = g.SetNodeProperty(key, AdvKeyBytes, lpg.BytesValue(bs[idx%len(bs)]))
	}
	if mix.Times {
		_ = g.SetNodeProperty(key, AdvKeyTime, lpg.TimeValue(times[idx%len(times)]))
	}
	if mix.Bools {
		_ = g.SetNodeProperty(key, AdvKeyBool, lpg.BoolValue(bools[idx%len(bools)]))
	}
}

// applyAdversarialEdge stamps the directed edge (src, dst) with the
// per-kind adversarial selection picked by (srcIdx*7 + edgeIdx).
//
//nolint:gocritic // paramTypeCombine: see [applyAdversarialNode].
func applyAdversarialEdge[N comparable, W any](
	g *lpg.Graph[N, W], src, dst N, srcIdx, edgeIdx int, mix Mix,
	ints []int64, floats []float64, strs []string, bs [][]byte, times []time.Time, bools []bool,
) {
	bucket := srcIdx*7 + edgeIdx
	if mix.Ints {
		g.SetEdgeProperty(src, dst, AdvKeyInt, lpg.Int64Value(ints[bucket%len(ints)]))
	}
	if mix.Floats {
		g.SetEdgeProperty(src, dst, AdvKeyFloat, lpg.Float64Value(floats[bucket%len(floats)]))
	}
	if mix.Strings {
		g.SetEdgeProperty(src, dst, AdvKeyString, lpg.StringValue(strs[bucket%len(strs)]))
	}
	if mix.Bytes {
		g.SetEdgeProperty(src, dst, AdvKeyBytes, lpg.BytesValue(bs[bucket%len(bs)]))
	}
	if mix.Times {
		g.SetEdgeProperty(src, dst, AdvKeyTime, lpg.TimeValue(times[bucket%len(times)]))
	}
	if mix.Bools {
		g.SetEdgeProperty(src, dst, AdvKeyBool, lpg.BoolValue(bools[bucket%len(bools)]))
	}
}
