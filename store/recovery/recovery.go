// Package recovery rebuilds the in-memory graph state from a
// snapshot (when present) plus the WAL tail, and exposes the harness
// used to fuzz crash semantics in tests.
//
// Recovery is the dual of [store/txn.Tx.Commit]: a Tx writes ops to
// the WAL, syncs, then applies them in memory. After a crash any
// op that reached the WAL is replayed during Open; ops that did not
// fsync are dropped — exactly the durability contract documented on
// Tx.Commit.
package recovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// Result reports what Open found.
type Result[N comparable, W any] struct {
	Graph       *lpg.Graph[N, W]
	SnapshotHit bool
	WALOps      int
	TailErr     error
}

// Op is the decoded form of a transaction-encoded WAL payload,
// mirroring the encoder in [store/txn].
//
// The struct is the union of v1 and v2 record shapes:
//
//   - For a v1 (legacy, untagged) frame, [Op.Version] is [txn.OpRecordV1]
//     and [Op.SrcBytes] / [Op.DstBytes] carry the length-prefixed
//     fmt.Sprintf bytes that the legacy [store/txn].NewStore wrote.
//   - For a v2 (typed, tagged) frame, [Op.Version] is [txn.OpRecordV2] and
//     [Op.Body] carries the opaque codec-encoded endpoints (src then
//     dst, back-to-back, self-delimiting per the installed [txn.Codec]).
//     [Op.SrcBytes] / [Op.DstBytes] are nil; the caller walks them
//     out of [Op.Body] via the codec.
//
// [Op.Kind] and [Op.Label] are populated for both versions.
type Op struct {
	Kind     txn.OpKind
	SrcBytes []byte
	DstBytes []byte
	Label    string
	Version  uint8
	Body     []byte
}

// Decode parses one payload back into an [Op]. The parser peeks the
// first byte to disambiguate v1 from v2:
//
//   - 0xFE ([txn.OpRecordV2]) introduces a v2 (tagged) record; the
//     remainder up to the uint16-length-prefixed trailing label is
//     copied into [Op.Body] verbatim for the typed open path.
//   - Any other first byte is interpreted as the v1 [txn.OpKind] and
//     parsed via the legacy length-prefixed layout.
func Decode(payload []byte) (Op, error) {
	defer metrics.Time("store.recovery.Decode")()
	if len(payload) < 1 {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, errors.New("recovery: short payload")
	}
	if payload[0] == txn.OpRecordV2 {
		return decodeV2(payload)
	}
	return decodeV1(payload)
}

// decodeV1 parses a legacy untagged record. The original layout —
// kept verbatim so all pre-existing v1 frames replay unchanged.
func decodeV1(payload []byte) (Op, error) {
	op := Op{Kind: txn.OpKind(payload[0]), Version: txn.OpRecordV1}
	off := 1
	read := func(want int) ([]byte, error) {
		if len(payload)-off < want {
			return nil, errors.New("recovery: truncated payload")
		}
		out := payload[off : off+want]
		off += want
		return out, nil
	}
	for _, ptr := range []*[]byte{&op.SrcBytes, &op.DstBytes} {
		lenb, err := read(2)
		if err != nil {
			metrics.IncCounter("store.recovery.Decode.errors", 1)
			return Op{}, err
		}
		n := int(binary.LittleEndian.Uint16(lenb))
		buf, err := read(n)
		if err != nil {
			metrics.IncCounter("store.recovery.Decode.errors", 1)
			return Op{}, err
		}
		*ptr = append([]byte(nil), buf...)
	}
	lenb, err := read(2)
	if err != nil {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, err
	}
	n := int(binary.LittleEndian.Uint16(lenb))
	lbl, err := read(n)
	if err != nil {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, err
	}
	op.Label = string(lbl)
	return op, nil
}

// decodeV2 parses a typed tagged record. The codec-encoded endpoints
// and the trailing label are opaque to this layer: locating the
// boundary between them requires walking the codec, so [Decode]
// returns the entire post-header region in [Op.Body] and leaves
// [Op.Label] empty. The typed open path ([OpenWithCodec]) is
// responsible for invoking the codec on [Op.Body] to extract src and
// dst, then reading the uint16 label length prefix and label bytes
// from the remaining tail.
func decodeV2(payload []byte) (Op, error) {
	// version + kind = 2 bytes minimum. The body may be empty for
	// hypothetical zero-byte-codec endpoints, so we do not enforce a
	// lower bound beyond the header here; downstream codec.Decode will
	// fail loudly on a truncated body.
	if len(payload) < 2 {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, errors.New("recovery: short v2 payload")
	}
	op := Op{
		Version: txn.OpRecordV2,
		Kind:    txn.OpKind(payload[1]),
		Body:    append([]byte(nil), payload[2:]...),
	}
	return op, nil
}

// OpenString opens the store at dir for graphs keyed by string node
// values. It loads any snapshot under dir/snapshot, then replays the
// WAL at dir/wal applying each op into the live graph.
//
// The function is the recovery entry point used by both the test
// harness and production restart logic; it is generic-by-instantiation
// (string nodes only in this v1) so the WAL payload decode can map
// the byte src/dst back to N. Future N types are added by mirroring
// this constructor.
func OpenString(dir string) (Result[string, int64], error) {
	defer metrics.Time("store.recovery.OpenString")()
	res, err := OpenStringCtx(context.Background(), dir)
	if err != nil {
		metrics.IncCounter("store.recovery.OpenString.errors", 1)
	}
	return res, err
}

// OpenStringCtx is the context-aware variant of [OpenString]. ctx.Err()
// is checked at the snapshot-load boundary and at every 4096 WAL
// frames replayed; on cancellation returns the partially-recovered
// Result paired with the wrapped ctx.Err.
//
//nolint:gocyclo // recovery: snapshot probe + WAL open + per-frame decode + per-frame apply + ctx ticks
func OpenStringCtx(ctx context.Context, dir string) (Result[string, int64], error) {
	defer metrics.Time("store.recovery.OpenStringCtx")()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	res := Result[string, int64]{Graph: g}

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.recovery.OpenStringCtx.errors", 1)
		return res, err
	}
	snapDir := filepath.Join(dir, "snapshot")
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err == nil {
		if _, err := snapshot.Open(snapDir); err != nil {
			metrics.IncCounter("store.recovery.OpenStringCtx.errors", 1)
			return res, fmt.Errorf("recovery: snapshot open: %w", err)
		}
		res.SnapshotHit = true
	}

	walPath := filepath.Join(dir, "wal")
	if _, err := os.Stat(walPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		metrics.IncCounter("store.recovery.OpenStringCtx.errors", 1)
		return res, err
	}
	r, err := wal.OpenReader(walPath)
	if err != nil {
		metrics.IncCounter("store.recovery.OpenStringCtx.errors", 1)
		return res, err
	}
	defer func() { _ = r.Close() }()
	for f := range r.Frames() {
		if res.WALOps&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("store.recovery.OpenStringCtx.errors", 1)
				return res, err
			}
		}
		op, derr := Decode(f.Payload)
		if derr != nil {
			res.TailErr = derr
			break
		}
		applyOpString(g, &op)
		res.WALOps++
	}
	res.TailErr = r.TailError()
	return res, nil
}

func applyOpString(g *lpg.Graph[string, int64], op *Op) {
	var src, dst, label string
	switch op.Version {
	case txn.OpRecordV2:
		// v2 string records are encoded with the canonical StringCodec:
		// uint32 LE length prefix + utf-8 bytes. Walk it twice to peel
		// src and dst, then parse the trailing uint16 label length and
		// label bytes from what remains.
		codec := txn.NewStringCodec()
		var rest []byte
		var err error
		src, rest, err = codec.Decode(op.Body)
		if err != nil {
			metrics.IncCounter("store.recovery.applyOpString.errors", 1)
			return
		}
		dst, rest, err = codec.Decode(rest)
		if err != nil {
			metrics.IncCounter("store.recovery.applyOpString.errors", 1)
			return
		}
		if len(rest) < 2 {
			metrics.IncCounter("store.recovery.applyOpString.errors", 1)
			return
		}
		n := binary.LittleEndian.Uint16(rest)
		rest = rest[2:]
		if uint64(len(rest)) < uint64(n) {
			metrics.IncCounter("store.recovery.applyOpString.errors", 1)
			return
		}
		label = string(rest[:n])
	default:
		src = string(op.SrcBytes)
		dst = string(op.DstBytes)
		label = op.Label
	}
	switch op.Kind {
	case txn.OpAddEdge:
		g.AddEdge(src, dst, 0)
	case txn.OpSetNodeLabel:
		g.SetNodeLabel(src, label)
	case txn.OpSetEdgeLabel:
		g.SetEdgeLabel(src, dst, label)
	}
}

// OpenWithCodec opens the store at dir for graphs keyed by N values,
// using codec to decode endpoint identifiers from v2 (tagged) WAL
// frames. It is the generalised dual of [OpenString].
//
// v1 (legacy fmt.Sprintf-based) frames are not generally invertible
// because the original write path used fmt.Sprintf("%v") which has no
// inverse for arbitrary N. The function therefore only supports
// instantiations where the legacy fallback can be implemented; the
// recovery path either skips a v1 frame or surfaces it as a tail
// error, depending on the recoverable state. Callers that need to
// migrate a v1 corpus to a typed codec should use [OpenString] to
// drain the existing log and re-emit it via a typed Store.
func OpenWithCodec[N comparable, W any](dir string, codec txn.Codec[N]) (Result[N, W], error) {
	defer metrics.Time("store.recovery.OpenWithCodec")()
	res, err := OpenWithCodecCtx[N, W](context.Background(), dir, codec)
	if err != nil {
		metrics.IncCounter("store.recovery.OpenWithCodec.errors", 1)
	}
	return res, err
}

// OpenWithCodecCtx is the context-aware variant of [OpenWithCodec].
// ctx.Err() is checked at the snapshot-load boundary and every 4096
// WAL frames during replay.
//
//nolint:gocyclo // recovery: snapshot probe + WAL open + per-frame decode + per-frame apply + ctx ticks
func OpenWithCodecCtx[N comparable, W any](ctx context.Context, dir string, codec txn.Codec[N]) (Result[N, W], error) {
	defer metrics.Time("store.recovery.OpenWithCodecCtx")()
	if codec == nil {
		return Result[N, W]{}, errors.New("recovery: nil codec")
	}
	g := lpg.New[N, W](adjlist.Config{Directed: true})
	res := Result[N, W]{Graph: g}

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.recovery.OpenWithCodecCtx.errors", 1)
		return res, err
	}
	snapDir := filepath.Join(dir, "snapshot")
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err == nil {
		if _, err := snapshot.Open(snapDir); err != nil {
			metrics.IncCounter("store.recovery.OpenWithCodecCtx.errors", 1)
			return res, fmt.Errorf("recovery: snapshot open: %w", err)
		}
		res.SnapshotHit = true
	}

	walPath := filepath.Join(dir, "wal")
	if _, err := os.Stat(walPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		metrics.IncCounter("store.recovery.OpenWithCodecCtx.errors", 1)
		return res, err
	}
	r, err := wal.OpenReader(walPath)
	if err != nil {
		metrics.IncCounter("store.recovery.OpenWithCodecCtx.errors", 1)
		return res, err
	}
	defer func() { _ = r.Close() }()
	for f := range r.Frames() {
		if res.WALOps&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("store.recovery.OpenWithCodecCtx.errors", 1)
				return res, err
			}
		}
		op, derr := Decode(f.Payload)
		if derr != nil {
			res.TailErr = derr
			break
		}
		if !applyOpCodec(g, &op, codec) {
			// A v1 frame met an instantiation with no inverse; stop
			// replay so callers see the cut-off boundary deterministically.
			res.TailErr = errors.New("recovery: v1 frame is not decodable through the supplied codec")
			break
		}
		res.WALOps++
	}
	res.TailErr = r.TailError()
	return res, nil
}

// applyOpCodec applies a decoded op into g via codec. It returns
// true if the op was applied. For v1 (legacy, untagged) frames the
// function returns false because the legacy fmt.Sprintf encoding is
// not generally invertible: callers needing to replay a v1 corpus
// must use [OpenString] (string-keyed only) and then re-emit via
// [txn.NewStoreWithCodec] to migrate the WAL to v2.
//
// The Op is taken by pointer to keep the inner recovery loop
// allocation-free; the function does not mutate op.
func applyOpCodec[N comparable, W any](g *lpg.Graph[N, W], op *Op, codec txn.Codec[N]) bool {
	if op.Version != txn.OpRecordV2 {
		return false
	}
	src, rest, err := codec.Decode(op.Body)
	if err != nil {
		return false
	}
	dst, rest, err := codec.Decode(rest)
	if err != nil {
		return false
	}
	if len(rest) < 2 {
		return false
	}
	n := binary.LittleEndian.Uint16(rest)
	rest = rest[2:]
	if uint64(len(rest)) < uint64(n) {
		return false
	}
	label := string(rest[:n])
	switch op.Kind {
	case txn.OpAddEdge:
		var zero W
		g.AddEdge(src, dst, zero)
	case txn.OpSetNodeLabel:
		g.SetNodeLabel(src, label)
	case txn.OpSetEdgeLabel:
		g.SetEdgeLabel(src, dst, label)
	}
	return true
}
