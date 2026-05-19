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
type Op struct {
	Kind     txn.OpKind
	SrcBytes []byte
	DstBytes []byte
	Label    string
}

// Decode parses one payload back into an [Op].
func Decode(payload []byte) (Op, error) {
	if len(payload) < 1 {
		return Op{}, errors.New("recovery: short payload")
	}
	op := Op{Kind: txn.OpKind(payload[0])}
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
			return Op{}, err
		}
		n := int(binary.LittleEndian.Uint16(lenb))
		buf, err := read(n)
		if err != nil {
			return Op{}, err
		}
		*ptr = append([]byte(nil), buf...)
	}
	lenb, err := read(2)
	if err != nil {
		return Op{}, err
	}
	n := int(binary.LittleEndian.Uint16(lenb))
	lbl, err := read(n)
	if err != nil {
		return Op{}, err
	}
	op.Label = string(lbl)
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
	return OpenStringCtx(context.Background(), dir)
}

// OpenStringCtx is the context-aware variant of [OpenString]. ctx.Err()
// is checked at the snapshot-load boundary and at every 4096 WAL
// frames replayed; on cancellation returns the partially-recovered
// Result paired with the wrapped ctx.Err.
//
//nolint:gocyclo // recovery: snapshot probe + WAL open + per-frame decode + per-frame apply + ctx ticks
func OpenStringCtx(ctx context.Context, dir string) (Result[string, int64], error) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	res := Result[string, int64]{Graph: g}

	if err := ctx.Err(); err != nil {
		return res, err
	}
	snapDir := filepath.Join(dir, "snapshot")
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err == nil {
		if _, err := snapshot.Open(snapDir); err != nil {
			return res, fmt.Errorf("recovery: snapshot open: %w", err)
		}
		res.SnapshotHit = true
	}

	walPath := filepath.Join(dir, "wal")
	if _, err := os.Stat(walPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	r, err := wal.OpenReader(walPath)
	if err != nil {
		return res, err
	}
	defer func() { _ = r.Close() }()
	for f := range r.Frames() {
		if res.WALOps&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return res, err
			}
		}
		op, derr := Decode(f.Payload)
		if derr != nil {
			res.TailErr = derr
			break
		}
		applyOpString(g, op)
		res.WALOps++
	}
	res.TailErr = r.TailError()
	return res, nil
}

func applyOpString(g *lpg.Graph[string, int64], op Op) {
	src := string(op.SrcBytes)
	dst := string(op.DstBytes)
	switch op.Kind {
	case txn.OpAddEdge:
		g.AddEdge(src, dst, 0)
	case txn.OpSetNodeLabel:
		g.SetNodeLabel(src, op.Label)
	case txn.OpSetEdgeLabel:
		g.SetEdgeLabel(src, dst, op.Label)
	}
}
