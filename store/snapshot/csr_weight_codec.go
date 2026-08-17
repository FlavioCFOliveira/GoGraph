package snapshot

// csr_weight_codec.go — the codec-encoded weights section of csr.bin (rmp
// #2526).
//
// # The defect this closes
//
// csr.bin historically carried edge weights as a DENSE FIXED-WIDTH array: a
// header byte declaring the per-weight width in bytes, then nEdges * width raw
// little-endian bytes, decoded by reinterpreting those bytes as the native Go
// value. The width came from [csrWeightSize], a hardcoded type switch over Go
// primitives, and its default arm returned 0.
//
// Zero was already spoken for. A width of 0 makes the writer emit hasWeights=0
// — the encoding of a graph that legitimately has NO weights (W is struct{},
// or the adjacency was built with adjlist.Config{Weightless: true}). So every
// weight type outside the primitive set — any struct, any NAMED integer type
// such as [time.Duration], any string — serialised to the exact bytes of a
// weightless graph. Nothing errored. The checkpoint then truncated the WAL
// prefix that still held the real values, and the loss became permanent: the
// recovered graph was indistinguishable from one that never had weights.
//
// The module is generic over W and ships [txn.NewInt64WeightCodec] and
// [txn.NewBinaryMarshalerWeightCodec] precisely so callers CAN use such types,
// so these were supported uses being destroyed silently.
//
// # The two rules that replace it
//
//  1. A weight that cannot be persisted is NEVER silently encoded as "no
//     weight". Either it is written through the caller's [txn.WeightCodec], or
//     the write FAILS with [ErrWeightNotPersistable] — which fails the
//     checkpoint, which leaves the WAL prefix holding the surviving copy
//     intact. Refusing to write is what stops a bug becoming data loss.
//  2. A reader that meets a codec-encoded section and holds no codec FAILS with
//     [ErrWeightCodecRequired] rather than returning zero weights. Silence on
//     read would re-create the same loss one layer down.
//
// # Why the fixed-width layout is KEPT for the primitives
//
// The codec-encoded section is ADDITIVE and is used only where the fixed-width
// layout cannot work. Routing every weight through [txn.WeightCodec] instead
// was rejected: the codecs do not agree with the existing on-disk bytes
// (txn.NewInt64WeightCodec is a signed VARINT, csr.bin's int64 weight is fixed
// 8-byte little-endian), so it would have rewritten a layout that already
// works, broken the pinned byte-equality fixtures, and forced a migration on
// float64 and int64 stores that have no defect. It would also cost space: the
// dense native array carries zero per-element overhead, which no
// self-delimiting codec framing can match. Prior art agrees on keeping both:
// Lucene's Lucene90DocValuesConsumer.addBinaryField writes NO per-document
// addresses when every value has the same length and falls back to an
// addresses structure only when they differ; Parquet keeps FIXED_LEN_BYTE_ARRAY
// distinct from BYTE_ARRAY for the same reason.
//
// # The layout
//
// Signalled by the sentinel [weightSizeCodec] in the existing weightSizeBytes
// header byte, in place of a width. What follows the edges array is then:
//
//	[offsets]   (nEdges+1) * 8 bytes, little-endian uint64
//	[payload]   offsets[nEdges] bytes
//
// offsets[k]..offsets[k+1] is the encoded payload of edge k, so weights are
// randomly addressable by slot. That matters: [ApplyCSRToGraph] SKIPS edge
// slots whose endpoints the mapper cannot resolve, so it does not visit every k
// in order and a purely sequential, self-delimiting concatenation would
// desynchronise against it.
//
// An (n+1) offsets array is the same idiom csr.bin already uses for adjacency
// itself — the vertices array is exactly this shape — and the same one Apache
// Arrow's Binary/LargeBinary layout uses. There is deliberately NO separate
// payload-length field: offsets[nEdges] IS the payload length, so there is no
// second copy of the same fact that could disagree with the first.
//
// # Compatibility
//
// A NEW reader meeting an OLD file is unaffected: widths 0/1/2/4/8 take the
// original dense path, byte for byte.
//
// An OLD reader meeting a NEW file fails LOUDLY, and does so deterministically
// rather than by luck. [weightSizeCodec] is 0xFF; [csrWeightSize] only ever
// returned 0, 1, 2, 4 or 8, so no legitimate historical file carries it. An old
// binary reaches ApplyCSRToGraph's width guard, finds 0xFF != csrWeightSize[W]()
// for every possible W, and returns ErrCorrupted. A second, independent guard
// catches it earlier on most files: an old readCSRLimited computes the dense
// extent as nEdges*255 and rejects it against the manifest-recorded file size.
//
// No manifest version was bumped, and that is deliberate. The sentinel appears
// ONLY in files that actually need it, so a float64 or int64 snapshot stays
// byte-identical and keeps opening on older builds; a version step would have
// made older readers reject every new snapshot including those. This is the
// per-record must-understand shape RocksDB uses for VersionEdit
// (kTagSafeIgnoreMask / kCustomTagNonSafeIgnoreMask in db/version_edit.h) — the
// marker travels with the record that needs it — rather than a whole-file
// version gate. It follows the same ruling taken for the manifest CRC trailer
// (rmp #2520): do not bump a schema version for a framing change.

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"reflect"
)

// weightSizeCodec is the sentinel written into csr.bin's weightSizeBytes
// header byte to mean "the weights are codec-encoded, variable width" rather
// than "each weight is N bytes wide".
//
// 0xFF is safe as a sentinel because it is not a width any writer ever emitted:
// [csrWeightSize] returns only 0, 1, 2, 4 or 8, and a 0 is never paired with
// hasWeights=1. It is also not a width any writer ever COULD emit — a 255-byte
// dense weight is not a Go primitive — so the value can never collide with a
// future fixed-width type either.
const weightSizeCodec uint8 = 0xFF

// validCSRWeightSize reports whether w is a weight width any writer could
// legitimately have emitted: one of the dense native widths [csrWeightSize]
// produces, or the [weightSizeCodec] sentinel.
//
// It exists so [readCSRLimited] can reject an unknown width at parse time
// rather than dispatching on it. The width selects the whole layout of the rest
// of the file, so a value in neither set — a single flipped bit turning an 8
// into a 9 — must be a loud corruption error and not a fallthrough into
// whichever branch happens to catch it.
func validCSRWeightSize(w uint8) bool {
	switch w {
	case 1, 2, 4, 8, weightSizeCodec:
		return true
	}
	return false
}

// ErrWeightNotPersistable is returned by the CSR writer when the graph carries
// edge weights that cannot be persisted: the weight type W is not one of the
// fixed-width primitives [csrWeightSize] knows, and no [txn.WeightCodec] was
// supplied to encode it.
//
// It is deliberately a hard failure and not a degradation. Returning it fails
// the snapshot write, which fails the checkpoint, which leaves the WAL prefix
// holding the only surviving copy of those weights UNTRUNCATED. Writing a
// weightless snapshot instead — what this code did before rmp #2526 — let the
// checkpoint go on to discard that prefix and made the loss permanent.
//
// The remedy is to supply the store's weight codec to the checkpointer:
//
//	checkpoint.New(cfg, g, wlog, &mu,
//		checkpoint.WithMapperCodec[N, W](st.Codec()),
//		checkpoint.WithWeightCodec[N, W](st.WeightCodec()))
var ErrWeightNotPersistable = errors.New(
	"snapshot: edge weights cannot be persisted: weight type is not a fixed-width primitive and no WeightCodec was supplied")

// ErrWeightCodecRequired is returned when applying a CSR whose weights section
// is codec-encoded ([weightSizeCodec]) to a graph, without a weight codec to
// decode it.
//
// Like [ErrWeightNotPersistable] this fails rather than degrades. Silently
// applying zero weights would discard values that ARE durably on disk, which is
// the same class of loss on the read side.
var ErrWeightCodecRequired = errors.New(
	"snapshot: csr.bin carries codec-encoded edge weights but no WeightCodec was supplied to decode them")

// weightEncoder is the minimal slice of [txn.WeightCodec] the CSR writer needs.
// It is declared locally, exactly as [keyEncoder] is, to avoid an upward
// dependency from store/snapshot on store/txn; any txn.WeightCodec[W] satisfies
// it structurally.
type weightEncoder[W any] interface {
	// Encode appends the wire form of w to buf and returns the extended slice.
	Encode(buf []byte, w W) ([]byte, error)
}

// weightDecoder is the read-side dual of [weightEncoder], the minimal slice of
// [txn.WeightCodec] that [ApplyCSRToGraphWithWeightCodec] needs.
type weightDecoder[W any] interface {
	// Decode reads a value from the head of buf, returning the decoded value,
	// the unread tail, and any error.
	Decode(buf []byte) (value W, rest []byte, err error)
}

// encodedWeights is the serialised form of a codec-encoded weights section:
// the (nEdges+1) offsets array and the concatenated payload it indexes.
type encodedWeights struct {
	offsets []uint64
	payload []byte
}

// byteLen reports the total on-disk size of the section.
func (e *encodedWeights) byteLen() int64 {
	return int64(len(e.offsets))*8 + int64(len(e.payload))
}

// encodeCSRWeights runs every weight through enc and builds the offsets array
// that indexes the result. enc must not be nil.
//
// The payload buffer is grown by append across the whole column rather than
// per weight, so the encoder writes into one buffer and the section costs a
// handful of re-grows instead of one allocation per edge.
func encodeCSRWeights[W any](weights []W, enc weightEncoder[W]) (encodedWeights, error) {
	out := encodedWeights{offsets: make([]uint64, 0, len(weights)+1)}
	// A modest per-weight guess; append grows it from here.
	out.payload = make([]byte, 0, len(weights)*8)
	out.offsets = append(out.offsets, 0)
	for i := range weights {
		var err error
		if out.payload, err = enc.Encode(out.payload, weights[i]); err != nil {
			return encodedWeights{}, fmt.Errorf("snapshot: encode weight for edge slot %d: %w", i, err)
		}
		out.offsets = append(out.offsets, uint64(len(out.payload)))
	}
	return out, nil
}

// writeEncodedWeights streams the section to w.
func writeEncodedWeights(w io.Writer, e *encodedWeights) error {
	if err := streamLE(w, uint64sAsBytes(e.offsets)); err != nil {
		return err
	}
	_, err := w.Write(e.payload)
	return err
}

// anyWeightNonZero reports whether any weight differs from the zero value of W.
//
// It exists for one narrow case: a store whose W is not a fixed-width primitive
// and which has NO weight codec. [txn.Tx.AddEdge] rejects a non-zero weight on
// such a store with txn.ErrNoWeightCodec, so every weight it holds is the zero
// value — and a snapshot that records no weights for it loses nothing, because
// there is nothing there to lose. Refusing to checkpoint that store would break
// a legitimate configuration to prevent a loss that cannot occur.
//
// So the refusal is conditioned on evidence rather than on the type alone: only
// a weight that actually carries information makes the write fail.
//
// reflect is used because W is unconstrained, so the zero value cannot be
// compared with ==; reflect.Value.IsZero is field-wise and therefore correct
// for structs with padding, which a raw memory comparison would not be. The
// cost is one reflect call per edge, paid only on this already-degenerate path
// (an unsizable W with no codec) and never on any path that has a codec or a
// primitive weight.
func anyWeightNonZero[W any](weights []W) bool {
	for i := range weights {
		rv := reflect.ValueOf(any(weights[i]))
		// An invalid Value means W is an interface type holding nil, which is
		// its zero value.
		if rv.IsValid() && !rv.IsZero() {
			return true
		}
	}
	return false
}

// codecWeightsOffsetsLen computes the byte length of the (nEdges+1) offsets
// array overflow-safely and bounds it before any make(), mirroring
// [weightsByteLen] for the dense layout.
//
// nE is a full uint64 read from an untrusted header, so nE+1 can wrap and
// (nE+1)*8 can overflow. bits.Add64 and bits.Mul64 surface both in their carry
// and high words; the result is then bounded against the file's byte budget and
// the platform int range before the conversion.
func codecWeightsOffsetsLen(nE, byteBudget uint64) (int, error) {
	n, carry := bits.Add64(nE, 1, 0)
	if carry != 0 {
		return 0, fmt.Errorf("%w: codec weights offset count overflow: nE=%d", ErrCSRCorrupted, nE)
	}
	hi, lo := bits.Mul64(n, 8)
	if hi != 0 || lo > uint64(maxInt) {
		return 0, fmt.Errorf("%w: codec weights offsets size overflow: nE=%d", ErrCSRCorrupted, nE)
	}
	if lo > byteBudget {
		return 0, fmt.Errorf("%w: codec weights offsets %d bytes exceed file budget %d: nE=%d",
			ErrCSRCorrupted, lo, byteBudget, nE)
	}
	return int(lo), nil
}

// readCodecWeights parses a codec-encoded weights section: the (nEdges+1)
// offsets array, then the payload it indexes.
//
// Every structural property is checked BEFORE the payload make(), so a hostile
// or corrupt header cannot drive an unbounded allocation and cannot produce an
// offsets array that later slices out of range:
//
//   - the offsets array's own byte length is bounded against the file budget;
//   - offsets[0] must be 0;
//   - offsets must be monotonically non-decreasing;
//   - offsets[nEdges] — the payload length — must fit the file budget.
//
// A SHORT READ is deliberately NOT reclassified as [ErrCSRCorrupted]. The dense
// weights path returns the bare io.ErrUnexpectedEOF for the same failure, and
// the package draws that line on purpose: on the bare reader a short read is an
// I/O error, structural implausibility is corruption, and the two are kept
// distinguishable. [Open] wraps either under ErrCorrupted for its callers.
//
// Together these make the per-slot slice payload[offsets[k]:offsets[k+1]]
// unconditionally in range, so the apply loop needs no further bounds check.
func readCodecWeights(br io.Reader, nE, byteBudget uint64) (offsets []uint64, payload []byte, err error) {
	offBytes, err := codecWeightsOffsetsLen(nE, byteBudget)
	if err != nil {
		return nil, nil, err
	}
	offsets = make([]uint64, offBytes/8)
	if _, err := io.ReadFull(br, uint64sAsBytes(offsets)); err != nil {
		return nil, nil, err
	}
	if offsets[0] != 0 {
		return nil, nil, fmt.Errorf("%w: codec weights offsets[0] = %d, want 0", ErrCSRCorrupted, offsets[0])
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i] < offsets[i-1] {
			return nil, nil, fmt.Errorf("%w: non-monotonic codec weights offsets at index %d (%d < %d)",
				ErrCSRCorrupted, i, offsets[i], offsets[i-1])
		}
	}
	total := offsets[len(offsets)-1]
	if total > byteBudget || total > uint64(maxInt) {
		return nil, nil, fmt.Errorf("%w: codec weights payload %d bytes exceeds file budget %d",
			ErrCSRCorrupted, total, byteBudget)
	}
	payload = make([]byte, total)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, nil, err
	}
	return offsets, payload, nil
}

// decodeCSRWeightCodec decodes one edge slot's weight through dec.
//
// The whole slot payload must be consumed. A decoder that leaves a tail has
// disagreed with the writer about the value's extent, which means the column is
// misframed — reported rather than tolerated, because tolerating it would let a
// drifted codec silently return a wrong weight for every subsequent edge.
func decodeCSRWeightCodec[W any](buf []byte, dec weightDecoder[W], slot uint64) (W, error) {
	var zero W
	v, rest, err := dec.Decode(buf)
	if err != nil {
		return zero, fmt.Errorf("snapshot: decode weight for edge slot %d: %w", slot, err)
	}
	if len(rest) != 0 {
		return zero, fmt.Errorf("%w: weight decoder left %d of %d bytes unread at edge slot %d",
			ErrCSRCorrupted, len(rest), len(buf), slot)
	}
	return v, nil
}
