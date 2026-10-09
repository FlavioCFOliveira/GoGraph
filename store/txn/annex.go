package txn

// annex.go — the commit marker's id annex (WAL v2 step 3, rmp #3021 A,
// docs/design-wal-v2.md §1.2, §5.5).
//
// A transaction that creates nodes names, in its OpCommit marker, the exact
// NodeID each created node received. Recovery binds every annexed key to that id
// before it replays the ops (Mapper.PlaceUnborn), so ids are identical across a
// restart whatever order concurrent transactions interned and committed in.
//
// Body of an annexed OpCommit (after the 8-byte commit timestamp):
//
//	uvarint nCreated
//	nCreated × { uvarint ref ; [codec key bytes if ref == 0] ; uvarint nodeID }
//
// ref = 1 + 2·opIndex + endpoint (0 src, 1 dst) names an op of the same
// transaction whose key slot holds the node's key; ref = 0 carries the key
// inline. A marker written before this step ends after the timestamp, which is
// how recovery tells the two apart: only a marker that carries an annex is
// replayed under the strict rule.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ErrAnnexUnresolvable is returned by a commit whose id annex cannot be built:
// a created id or an op's key that the graph no longer resolves. Nothing is
// written; the transaction is not committed.
var ErrAnnexUnresolvable = errors.New("txn: commit id annex cannot be resolved")

// NodeKeySlots reports which key slots of an op of kind k can CREATE a node when
// applied — the op interns its key — so a replay must find those keys bound.
// Every op body begins with the src and dst keys (codec-encoded); only the kinds
// reported here intern them.
func (k OpKind) NodeKeySlots() (src, dst bool) {
	switch k {
	case OpAddNode, OpSetNodeLabel, OpSetNodeProperty:
		return true, false
	case OpAddEdge, OpAddEdgeWeighted, OpAddEdgeH:
		return true, true
	}
	return false, false
}

// AttachWriteTx names the lpg write transaction that applied this transaction's
// ops, so its commit marker can carry the exact ids of the nodes it created
// ([lpg.WriteTx.CreatedNodes]). An embedder that applies eagerly and commits with
// [Tx.CommitWALOnly] calls it before the commit, while wtx is still open. Without
// it the marker carries the fallback annex: every key an intern-capable op names,
// resolved through the graph — correct, and larger.
//
// Not safe for concurrent use; like every other Tx method it belongs to the
// goroutine driving the transaction.
func (t *Tx[N, W]) AttachWriteTx(wtx lpg.WriteTx) {
	t.wtx, t.wtxAttached = wtx, wtx.Valid()
}

// annexEntry is one created node: the key's ref (0 for inline), the key, and the id.
type annexEntry[N comparable] struct {
	key N
	ref uint64
	id  graph.NodeID
}

// annexIDPool recycles the created-id buffers, so a commit allocates no id slice.
var annexIDPool = sync.Pool{New: func() any { b := make([]graph.NodeID, 0, 16); return &b }}

// buildAnnex encodes the id annex for t's ops into buf (length 0) and returns it.
// It runs BEFORE the WAL append run, so key resolution never extends the WAL
// writer's critical section.
func (t *Tx[N, W]) buildAnnex(buf []byte) ([]byte, error) {
	m := t.store.g.AdjList().Mapper()
	var entries []annexEntry[N]
	if t.wtxAttached {
		idsp, _ := annexIDPool.Get().(*[]graph.NodeID)
		ids := t.wtx.CreatedNodes((*idsp)[:0])
		slices.Sort(ids)
		ids = slices.Compact(ids)
		entries = make([]annexEntry[N], 0, len(ids))
		for _, id := range ids {
			k, ok := m.Resolve(id)
			if !ok {
				*idsp = ids[:0]
				annexIDPool.Put(idsp)
				return nil, fmt.Errorf("%w: created node %d resolves to no key", ErrAnnexUnresolvable, uint64(id))
			}
			entries = append(entries, annexEntry[N]{key: k, id: id})
		}
		*idsp = ids[:0]
		annexIDPool.Put(idsp)
		t.setRefs(entries)
	} else {
		entries = t.fallbackAnnex()
	}
	buf = binary.AppendUvarint(buf, uint64(len(entries)))
	for i := range entries {
		e := &entries[i]
		buf = binary.AppendUvarint(buf, e.ref)
		if e.ref == 0 {
			var err error
			if buf, err = t.store.codec.Encode(buf, e.key); err != nil {
				return nil, fmt.Errorf("txn: encode annex key: %w", err)
			}
		}
		buf = binary.AppendUvarint(buf, uint64(e.id))
	}
	return buf, nil
}

// setRefs points every entry at the first op key slot holding its key, or leaves
// it inline (ref 0) when no op names the key — a creation a statement rollback
// withdrew.
func (t *Tx[N, W]) setRefs(entries []annexEntry[N]) {
	if len(entries) == 0 {
		return
	}
	// A map only when the quadratic scan would be large.
	if len(entries)*len(t.ops) > 4096 {
		first := make(map[N]uint64, len(t.ops))
		for i := range t.ops {
			src, dst := t.ops[i].Kind.NodeKeySlots()
			if src {
				if _, ok := first[t.ops[i].Src]; !ok {
					first[t.ops[i].Src] = 1 + 2*uint64(i)
				}
			}
			if dst {
				if _, ok := first[t.ops[i].Dst]; !ok {
					first[t.ops[i].Dst] = 2 + 2*uint64(i)
				}
			}
		}
		for i := range entries {
			entries[i].ref = first[entries[i].key]
		}
		return
	}
	for i := range entries {
		entries[i].ref = t.firstRef(entries[i].key)
	}
}

// firstRef returns the ref of the first intern-capable op key slot holding k, or 0.
func (t *Tx[N, W]) firstRef(k N) uint64 {
	for i := range t.ops {
		src, dst := t.ops[i].Kind.NodeKeySlots()
		if src && t.ops[i].Src == k {
			return 1 + 2*uint64(i)
		}
		if dst && t.ops[i].Dst == k {
			return 2 + 2*uint64(i)
		}
	}
	return 0
}

// fallbackAnnex annexes every distinct key an intern-capable op names, with the
// id the graph holds for it (§5.5). Used when no write transaction is attached.
// Each key is referenced by its first slot, never inline. A key the graph has not
// interned — the caller wrote the op to the WAL without applying it — is reserved
// as a never-born node ([lpg.Graph.ReserveNodeID]), so the id the marker names is
// one this graph will never give to another key, and recovery can place it.
func (t *Tx[N, W]) fallbackAnnex() []annexEntry[N] {
	var (
		entries []annexEntry[N]
		seen    map[N]struct{}
	)
	add := func(k N, ref uint64) {
		if seen == nil {
			seen = make(map[N]struct{})
		}
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		entries = append(entries, annexEntry[N]{key: k, ref: ref, id: t.store.g.ReserveNodeID(k)})
	}
	for i := range t.ops {
		src, dst := t.ops[i].Kind.NodeKeySlots()
		if src {
			add(t.ops[i].Src, 1+2*uint64(i))
		}
		if dst {
			add(t.ops[i].Dst, 2+2*uint64(i))
		}
	}
	return entries
}
