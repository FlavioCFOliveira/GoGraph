package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"gograph/graph"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// ErrMapperApply is returned by [ApplyMapperToGraph] when the supplied
// readback violates an invariant the writer is responsible for
// upholding (intra-shard gap, hash/shard mismatch, duplicate key, or a
// non-empty target mapper). It wraps the underlying [graph.ErrMapper…]
// sentinels so callers can branch on the typed cause via [errors.Is].
var ErrMapperApply = errors.New("snapshot: cannot apply mapper")

// ApplyMapperToGraph rebuilds g's underlying [graph.Mapper] from the
// snapshot readback. It is only meaningful for string-keyed graphs:
// any other N type returns nil without touching g, because no v3
// mapper.bin is ever produced for non-string graphs. The caller is
// expected to invoke this function before [ApplyCSRToGraph],
// [ApplyLabelsToGraph], or [ApplyPropertiesToGraph] so subsequent
// resolution calls see the restored interning table.
//
// Pre-condition: g must hold a fresh (empty) mapper. Calling on a
// graph that already has interned values returns [ErrMapperApply]
// wrapping [graph.ErrMapperNotEmpty] so the caller can distinguish a
// programmer error from a corruption error.
//
// Concurrency: [ApplyMapperToGraph] is not safe to call concurrently
// with mutations or reads on g. It is intended for the one-shot
// snapshot-load phase of recovery.
func ApplyMapperToGraph[N comparable, W any](g *lpg.Graph[N, W], rb MapperReadback) error {
	defer metrics.Time("store.snapshot.ApplyMapperToGraph")()
	if len(rb.Pairs) == 0 {
		return nil
	}
	mapper := g.AdjList().Mapper()
	stringMapper, ok := any(mapper).(*graph.Mapper[string])
	if !ok {
		// A v3 snapshot mapper.bin only exists for string-keyed graphs;
		// the recovery wiring should never call this with N!=string.
		// Treat as a logic error rather than a corruption: fail loudly.
		metrics.IncCounter("store.snapshot.ApplyMapperToGraph.errors", 1)
		return fmt.Errorf("%w: non-string-keyed graph received v3 mapper readback", ErrMapperApply)
	}
	entries := make([]graph.MapperEntry[string], len(rb.Pairs))
	for i := range rb.Pairs {
		entries[i] = graph.MapperEntry[string]{
			ID:  rb.Pairs[i].ID,
			Key: rb.Pairs[i].Key,
		}
	}
	if err := stringMapper.LoadFrom(entries); err != nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraph.errors", 1)
		return fmt.Errorf("%w: %w", ErrMapperApply, err)
	}
	return nil
}

// ApplyMapperToGraphWithCodec rebuilds g's underlying [graph.Mapper]
// from a version-2 (codec) snapshot readback for ANY comparable key
// type N. Each [MapperRawPair] carries the codec-encoded key bytes the
// snapshot writer produced via [WriteMapper]; this function decodes
// them back into N via the supplied codec (the same one the store uses
// on the WAL) and seeds the interning table through [graph.Mapper.LoadFrom].
//
// It is the codec-aware dual of [ApplyMapperToGraph]: recovery calls
// this when the loaded readback carries RawPairs (non-string keys) and
// the string-specialised path when it carries Pairs. An empty readback
// is a no-op.
//
// Pre-condition and concurrency contract match [ApplyMapperToGraph]: g
// must hold a fresh (empty) mapper, and the call must not race with any
// other access to g. A decode failure surfaces as [ErrMapperApply]
// wrapping the codec error; a structural violation surfaces as
// [ErrMapperApply] wrapping the relevant [graph.ErrMapper…] sentinel.
func ApplyMapperToGraphWithCodec[N comparable, W any](g *lpg.Graph[N, W], rb MapperReadback, codec keyDecoder[N]) error {
	defer metrics.Time("store.snapshot.ApplyMapperToGraphWithCodec")()
	if len(rb.RawPairs) == 0 {
		return nil
	}
	if codec == nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
		return fmt.Errorf("%w: nil codec", ErrMapperApply)
	}
	mapper := g.AdjList().Mapper()
	entries := make([]graph.MapperEntry[N], len(rb.RawPairs))
	for i := range rb.RawPairs {
		key, rest, derr := codec.Decode(rb.RawPairs[i].Key)
		if derr != nil {
			metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
			return fmt.Errorf("%w: decode key for node %d: %w",
				ErrMapperApply, uint64(rb.RawPairs[i].ID), derr)
		}
		if len(rest) != 0 {
			// The writer encoded exactly one key per record; trailing bytes
			// mean the on-disk record and the codec disagree on framing.
			metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
			return fmt.Errorf("%w: trailing bytes after key for node %d (%d left)",
				ErrMapperApply, uint64(rb.RawPairs[i].ID), len(rest))
		}
		entries[i] = graph.MapperEntry[N]{ID: rb.RawPairs[i].ID, Key: key}
	}
	if err := mapper.LoadFrom(entries); err != nil {
		metrics.IncCounter("store.snapshot.ApplyMapperToGraphWithCodec.errors", 1)
		return fmt.Errorf("%w: %w", ErrMapperApply, err)
	}
	return nil
}

// ApplyCSRToGraph replays the adjacency in rb into g. The pre-
// condition is that g's underlying mapper has already been populated
// with every NodeID referenced by rb — typically by an immediately-
// preceding [ApplyMapperToGraph] (v3 snapshots) or by a WAL replay
// (v2 snapshots that pair with a WAL prefix). Records whose
// endpoints the mapper cannot resolve are skipped and counted via
// `store.snapshot.ApplyCSR.unresolved`; the function does not return
// an error for them so a partial mapper degrades cleanly rather than
// aborting recovery mid-way.
//
// Weight decoding is supported for the common int/float weight types
// (int8/uint8/bool, int16/uint16, int32/uint32/float32, int/uint/
// int64/uint64/float64/uintptr). Other W types apply zero weights;
// the metric `store.snapshot.ApplyCSR.weightFallback` reports the
// fallback count for observability.
//
// ApplyCSRToGraph is idempotent against a freshly-loaded mapper but
// not against a graph that already contains edges: re-applying a CSR
// to a graph with existing edges may duplicate them in multigraph
// mode or no-op in simple-graph mode. Callers should run this exactly
// once per recovery, immediately after the mapper restore and before
// any WAL replay.
//
// rb is passed by pointer to avoid copying the three slices in the
// readback (vertices, edges, weight bytes) on every call. The
// function does not mutate rb.
//
//nolint:gocyclo // CSR apply walks every src slot, resolves endpoints, decodes weight by W type
func ApplyCSRToGraph[N comparable, W any](g *lpg.Graph[N, W], rb *CSRReadback) error {
	defer metrics.Time("store.snapshot.ApplyCSRToGraph")()
	if len(rb.Vertices) == 0 {
		return nil
	}
	mapper := g.AdjList().Mapper()

	// CSR vertices is the offset array: vertices[i]..vertices[i+1] is
	// the half-open edge slice for source NodeID i. Walk every src
	// slot up to len(vertices)-1. Slots without an interned value are
	// silently skipped (they exist only because the mapper packs
	// NodeIDs into 256 shards, so the addressable range typically
	// overshoots Order()).
	maxSrc := uint64(len(rb.Vertices) - 1)
	weightSize := uint64(rb.WeightSize)
	for src := uint64(0); src < maxSrc; src++ {
		start := rb.Vertices[src]
		end := rb.Vertices[src+1]
		if start == end {
			continue
		}
		srcN, ok := mapper.Resolve(graph.NodeID(src))
		if !ok {
			metrics.IncCounter("store.snapshot.ApplyCSR.unresolved", uint64(end-start))
			continue
		}
		for k := start; k < end; k++ {
			dstID := rb.Edges[k]
			dstN, ok := mapper.Resolve(dstID)
			if !ok {
				metrics.IncCounter("store.snapshot.ApplyCSR.unresolved", 1)
				continue
			}
			var weight W
			if rb.HasWeights && len(rb.WeightBytes) > 0 {
				off := k * weightSize
				if uint64(len(rb.WeightBytes)) >= off+weightSize {
					weight = decodeCSRWeight[W](rb.WeightBytes[off : off+weightSize])
				}
			}
			if err := g.AddEdge(srcN, dstN, weight); err != nil {
				metrics.IncCounter("store.snapshot.ApplyCSR.addEdgeErrors", 1)
				return fmt.Errorf("snapshot.ApplyCSRToGraph: AddEdge: %w", err)
			}
		}
	}
	// Ensure every interned-but-isolated node is present in the
	// adjacency layer. Isolated nodes have no entry in vertices, so
	// the AddEdge loop above never touched them. AdjList.AddNode is
	// idempotent (it just calls mapper.Intern, which is a cache hit
	// here) and the LPG's Order() already counts them via the mapper.
	// We still walk the mapper to maintain symmetry with the write
	// path's expectations for any future AdjList that materialises
	// per-node state.
	mapper.Walk(func(_ graph.NodeID, n N) bool {
		_ = g.AddNode(n)
		return true
	})
	return nil
}

// decodeCSRWeight reconstructs the W value previously serialised by
// [WriteCSR] for one edge. The conversion mirrors the [csrWeightSize]
// type switch in writer.go: writer and reader must agree on width and
// endianness for each W. Unsupported W types return the zero value
// and bump the `store.snapshot.ApplyCSR.weightFallback` counter so
// observability surfaces the loss.
//
//nolint:gocyclo // CSR weight decode: one branch per supported W type
func decodeCSRWeight[W any](buf []byte) W {
	var zero W
	if _, ok := any(zero).(struct{}); ok {
		return zero
	}
	if len(buf) == 0 {
		metrics.IncCounter("store.snapshot.ApplyCSR.weightFallback", 1)
		return zero
	}
	// We use a typed-result switch driven by the W zero value: each
	// arm decodes the on-disk bytes and writes back through a
	// pointer to a fresh W instance, then returns it. The any()
	// indirection here is unavoidable for the type switch; the hot
	// recovery loop runs this once per edge, not in any inner search
	// pass, so the cost is acceptable.
	var out W
	switch v := any(&out).(type) {
	case *int8:
		*v = int8(buf[0])
	case *uint8:
		*v = buf[0]
	case *bool:
		*v = buf[0] != 0
	case *int16:
		*v = int16(binary.LittleEndian.Uint16(buf))
	case *uint16:
		*v = binary.LittleEndian.Uint16(buf)
	case *int32:
		*v = int32(binary.LittleEndian.Uint32(buf))
	case *uint32:
		*v = binary.LittleEndian.Uint32(buf)
	case *float32:
		*v = math.Float32frombits(binary.LittleEndian.Uint32(buf))
	case *int:
		*v = int(int64(binary.LittleEndian.Uint64(buf))) //nolint:gosec // round-trip of writer-emitted bytes
	case *uint:
		*v = uint(binary.LittleEndian.Uint64(buf))
	case *int64:
		*v = int64(binary.LittleEndian.Uint64(buf))
	case *uint64:
		*v = binary.LittleEndian.Uint64(buf)
	case *float64:
		*v = math.Float64frombits(binary.LittleEndian.Uint64(buf))
	case *uintptr:
		*v = uintptr(binary.LittleEndian.Uint64(buf))
	default:
		metrics.IncCounter("store.snapshot.ApplyCSR.weightFallback", 1)
	}
	return out
}
