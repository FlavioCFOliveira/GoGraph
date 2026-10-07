package recovery

// annex.go — replay of the commit marker's id annex (WAL v2 step 3, rmp #3021 A,
// docs/design-wal-v2.md §1.3).

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ErrUnboundNodeKey is returned by [Open] (and is [Result.TailErr]) when an op of
// a transaction whose commit marker carries an id annex names, in a slot that
// creates nodes, a key that is neither bound by an earlier transaction nor placed
// by the annex. The producer annexes every node it creates, so the record is
// corrupt; replaying it by interning the key afresh would give the node an id no
// process ever assigned. Fail-stop (WAL v2 step 3).
var ErrUnboundNodeKey = errors.New("recovery: op names a node key the commit annex does not bind")

// ErrCommitAnnexCorrupt is returned by [Open] (and is [Result.TailErr]) when a
// commit marker's id annex cannot be decoded: a truncated entry, a reference to
// an op the transaction does not have or to a key slot that cannot hold a node,
// or a key the codec refuses. Fail-stop (WAL v2 step 3).
var ErrCommitAnnexCorrupt = errors.New("recovery: commit marker id annex is corrupt")

// commitAnnex returns the annex bytes of a commit marker body, and whether the
// marker carries one. A marker written before WAL v2 step 3 ends after its 8-byte
// commit timestamp (or is empty), and replays exactly as it always did.
func commitAnnex(body []byte) ([]byte, bool) {
	if len(body) <= 8 {
		return nil, false
	}
	return body[8:], true
}

// opKey decodes the key in slot endpoint (0 src, 1 dst) of op.
func opKey[N comparable](op *Op, endpoint uint64, codec txn.Codec[N]) (N, error) {
	k, rest, err := codec.Decode(op.Body)
	if err != nil || endpoint == 0 {
		return k, err
	}
	k, _, err = codec.Decode(rest)
	return k, err
}

// placeAnnex binds every key the annex names to its id, before the transaction's
// ops are replayed, and checks the strict rule: every key an intern-capable op of
// the transaction names must then be bound. It returns the ids it newly placed
// whose key no intern-capable op names — creations a statement rollback withdrew
// — for the caller to settle after the ops are applied.
//
// The annex is decoded in full before anything is placed. When an entry refers to
// an op whose own body does not decode, the annex is skipped and nothing is
// placed: replaying that op fails on its own, and the replay classifies the
// transaction as it always has ([ErrCommittedTxnCorruptOp]) without binding any of
// its keys into the recovered graph.
func placeAnnex[N comparable, W any](g *lpg.Graph[N, W], committed []Op, annex []byte, codec txn.Codec[N]) (*annexPlacements[N], error) {
	n, read := binary.Uvarint(annex)
	if read <= 0 || n > uint64(len(annex)) {
		return nil, fmt.Errorf("%w: entry count", ErrCommitAnnexCorrupt)
	}
	annex = annex[read:]
	entries := make([]annexPlacement[N], 0, n)
	for i := uint64(0); i < n; i++ {
		ref, r := binary.Uvarint(annex)
		if r <= 0 {
			return nil, fmt.Errorf("%w: entry %d ref", ErrCommitAnnexCorrupt, i)
		}
		annex = annex[r:]
		var key N
		if ref == 0 {
			k, rest, err := codec.Decode(annex)
			if err != nil {
				return nil, fmt.Errorf("%w: entry %d inline key: %w", ErrCommitAnnexCorrupt, i, err)
			}
			key, annex = k, rest
		} else {
			opIdx, endpoint := (ref-1)/2, (ref-1)%2
			if opIdx >= uint64(len(committed)) {
				return nil, fmt.Errorf("%w: entry %d names op %d of %d", ErrCommitAnnexCorrupt, i, opIdx, len(committed))
			}
			src, dst := committed[opIdx].Kind.NodeKeySlots()
			if (endpoint == 0 && !src) || (endpoint == 1 && !dst) {
				return nil, fmt.Errorf("%w: entry %d names a slot of op kind %d that holds no node", ErrCommitAnnexCorrupt, i, committed[opIdx].Kind)
			}
			k, err := opKey(&committed[opIdx], endpoint, codec)
			if err != nil {
				// The referenced op is itself undecodable; see above. Its own apply
				// reports it, so the annex is skipped rather than failed.
				return nil, nil //nolint:nilerr // deliberate: the op's replay classifies this corruption
			}
			key = k
		}
		raw, r2 := binary.Uvarint(annex)
		if r2 <= 0 {
			return nil, fmt.Errorf("%w: entry %d id", ErrCommitAnnexCorrupt, i)
		}
		annex = annex[r2:]
		entries = append(entries, annexPlacement[N]{key: key, id: graph.NodeID(raw)})
	}
	if len(annex) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCommitAnnexCorrupt, len(annex))
	}
	var placed []annexPlacement[N]
	for _, e := range entries {
		isNew, err := g.PlaceNodeID(e.key, e.id)
		if err != nil {
			return nil, err
		}
		if isNew {
			placed = append(placed, e)
		}
	}

	// The strict rule, and the set of keys the ops name.
	mapper := g.AdjList().Mapper()
	var named map[N]struct{}
	if len(placed) > 0 {
		named = make(map[N]struct{}, len(placed))
	}
	for i := range committed {
		src, dst := committed[i].Kind.NodeKeySlots()
		for endpoint, has := range [2]bool{src, dst} {
			if !has {
				continue
			}
			k, err := opKey(&committed[i], uint64(endpoint), codec)
			if err != nil {
				// The op's own apply reports an undecodable body.
				continue
			}
			if _, ok := mapper.Lookup(k); !ok {
				return nil, fmt.Errorf("%w: op %d of %d (kind %d)", ErrUnboundNodeKey, i+1, len(committed), committed[i].Kind)
			}
			if named != nil {
				named[k] = struct{}{}
			}
		}
	}
	out := &annexPlacements[N]{placed: placed}
	for _, p := range placed {
		if _, ok := named[p.key]; !ok {
			out.unnamed = append(out.unnamed, p.id)
		}
	}
	return out, nil
}

// annexPlacement is one annex entry: a key and the id it is bound to.
type annexPlacement[N comparable] struct {
	key N
	id  graph.NodeID
}

// annexPlacements is what [placeAnnex] did: the bindings it newly placed, and the
// ids among them that no intern-capable op names.
type annexPlacements[N comparable] struct {
	placed  []annexPlacement[N]
	unnamed []graph.NodeID
}

// settle finishes a transaction whose ops all replayed: every placed id no op named
// is a creation the transaction withdrew ([lpg.Graph.SettleNeverBorn]).
func (a *annexPlacements[N]) settle(g interface{ SettleNeverBorn(graph.NodeID) }) {
	if a == nil {
		return
	}
	for _, id := range a.unnamed {
		g.SettleNeverBorn(id)
	}
}

// withdraw undoes the placements of a transaction whose replay stopped at an op it
// could not apply, keeping only the keys an op that DID apply names — exactly the
// keys the replay before WAL v2 step 3 would have interned — so the discarded
// remainder of the transaction binds nothing into the recovered graph.
func withdrawAnnex[N comparable, W any](g *lpg.Graph[N, W], a *annexPlacements[N], applied []Op, codec txn.Codec[N]) {
	if a == nil || len(a.placed) == 0 {
		return
	}
	keep := make(map[N]struct{})
	for i := range applied {
		src, dst := applied[i].Kind.NodeKeySlots()
		for endpoint, has := range [2]bool{src, dst} {
			if !has {
				continue
			}
			if k, err := opKey(&applied[i], uint64(endpoint), codec); err == nil {
				keep[k] = struct{}{}
			}
		}
	}
	for _, p := range a.placed {
		if _, ok := keep[p.key]; !ok {
			g.UnplaceNodeID(p.key, p.id)
		}
	}
}

// replayCommitAnnex runs [placeAnnex] for a commit marker body that carries an id
// annex, and does nothing for one that does not.
func replayCommitAnnex[N comparable, W any](g *lpg.Graph[N, W], committed []Op, body []byte, codec txn.Codec[N]) (*annexPlacements[N], error) {
	annex, ok := commitAnnex(body)
	if !ok {
		return nil, nil
	}
	return placeAnnex(g, committed, annex, codec)
}
