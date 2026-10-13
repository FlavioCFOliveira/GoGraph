package recovery

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ErrLegacyMirrorConflict is returned by a recovery of a legacy undirected
// store (rmp #3072) that cannot be migrated because an edge holds different
// values in its two directions.
//
// The undirected engine stored every edge twice, once per direction, and kept
// its properties and relationship types under the direction a write named, so
// the two copies of one edge could disagree. A directed relationship has one
// value, and keeping one of the two would silently discard the other, so
// recovery refuses the store instead and leaves it as it found it. The two
// directions disagree when both hold the same property key with different
// values — on the pair or on one relationship — or both hold a relationship
// type, or a set of types, and they differ. A value held in one direction only
// is kept. The wrapping error names the conflicting edges by node id, at most
// [legacyConflictListLimit] of them.
var ErrLegacyMirrorConflict = errors.New("recovery: legacy undirected edge holds different values in its two directions")

// legacyConflictListLimit bounds how many conflicts the error of
// [ErrLegacyMirrorConflict] names; the rest are counted.
const legacyConflictListLimit = 16

// mirrorLegacyEdgeOp applies to the reverse direction (dst, src) what the
// undirected engine applied there for an edge operation of kind it recorded as
// (src, dst), once the operation itself has been applied: an insertion stores
// the mirror arc with the same handle and weight, and a removal removes it. A
// removal of the last arc between the two clears the pair's records in both
// directions, as that engine did. Every other operation acts on the direction
// it names only, exactly as it did on that engine, and a self-loop has one
// direction. removed holds the src→dst handles before an [txn.OpRemoveEdge],
// which names no handle, so the removed one can be found. rest is the body
// after the endpoints. It reports false for a body it cannot read or a write
// the graph refuses.
func mirrorLegacyEdgeOp[N comparable, W any](g *lpg.Graph[N, W], kind txn.OpKind, src, dst N, rest []byte, removed []uint64) bool {
	if src == dst {
		return true
	}
	switch kind {
	case txn.OpAddEdge, txn.OpAddEdgeWeighted, txn.OpAddEdgeH:
		return mirrorLegacyInsertion(g, kind, src, dst, rest)
	case txn.OpRemoveEdge:
		after := g.AppendEdgeHandles(src, dst, nil)
		for _, h := range removed {
			if i := slices.Index(after, h); i >= 0 {
				after = slices.Delete(after, i, i+1)
				continue
			}
			// h is the arc the removal took; its mirror carries the same handle.
			_, err := g.RemoveEdgeByHandle(dst, src, h)
			return err == nil
		}
		return true // nothing was removed
	case txn.OpRemoveEdgeByHandle, txn.OpRemoveEdgeInstanceByHandle:
		h, ok := legacyBodyHandle(rest)
		if !ok {
			return false
		}
		if kind == txn.OpRemoveEdgeByHandle {
			_, err := g.RemoveEdgeByHandle(dst, src, h)
			return err == nil
		}
		return g.RemoveEdgeInstanceByHandle(dst, src, h) == nil
	}
	return true
}

// mirrorLegacyInsertion stores the dst→src mirror of the src→dst arc an
// insertion just stored: the arc carrying the handle an [txn.OpAddEdgeH] names,
// or else the last src→dst arc, which is the one the insertion appended.
func mirrorLegacyInsertion[N comparable, W any](g *lpg.Graph[N, W], kind txn.OpKind, src, dst N, rest []byte) bool {
	adj := g.AdjList()
	srcID, ok := adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	var want uint64
	if kind == txn.OpAddEdgeH {
		if want, ok = legacyBodyHandle(rest); !ok {
			return false
		}
	}
	nbs, ws, hs := adj.LoadEntryH(srcID)
	for i := len(nbs) - 1; i >= 0; i-- {
		if nbs[i] != dstID || i >= len(hs) || (kind == txn.OpAddEdgeH && hs[i] != want) {
			continue
		}
		var w W
		if i < len(ws) {
			w = ws[i]
		}
		_, err := g.AddEdgeHIfAbsent(dst, src, w, hs[i])
		return err == nil
	}
	return false
}

// legacyBodyHandle returns the 8-byte handle every handle-bearing edge
// operation's body ends with.
func legacyBodyHandle(rest []byte) (uint64, bool) {
	if len(rest) < 8 {
		return 0, false
	}
	return trailingHandle(rest[len(rest)-8:])
}

// legacySide is what one direction of a pair holds: its arcs by handle, the
// relationship type in each arc's label column, the pair's overflow types and
// properties, and each arc's handle-keyed types and properties.
type legacySide[W any] struct {
	arcs        map[uint64]W // weight by handle
	slotType    map[uint64]string
	overflow    []string
	props       map[string]lpg.PropertyValue
	handleTypes map[uint64][]string
	handleProps map[uint64]map[string]lpg.PropertyValue
}

// readLegacySide reads what the direction src→dst of g holds.
func readLegacySide[N comparable, W any](g *lpg.Graph[N, W], src, dst graph.NodeID) legacySide[W] {
	s := legacySide[W]{
		arcs:        map[uint64]W{},
		slotType:    map[uint64]string{},
		handleTypes: map[uint64][]string{},
		handleProps: map[uint64]map[string]lpg.PropertyValue{},
	}
	nbs, ws, hs := g.AdjList().LoadEntryH(src)
	var ordered []uint64 // the pair's handles in canonical slot order
	for i, nb := range nbs {
		if nb != dst || i >= len(hs) {
			continue
		}
		var w W
		if i < len(ws) {
			w = ws[i]
		}
		s.arcs[hs[i]] = w
		ordered = append(ordered, hs[i])
	}
	slices.SortStableFunc(ordered, cmp.Compare[uint64])
	g.ForEachPairSlotRelTypeByID(src, dst, func(ordinal int, name string) {
		if ordinal < len(ordered) {
			s.slotType[ordered[ordinal]] = name
		}
	})
	g.ForEachPairOverflowRelTypeByID(src, dst, func(name string) { s.overflow = append(s.overflow, name) })
	s.props = g.EdgePropertiesByID(src, dst)
	for h := range s.arcs {
		if t := g.EdgeLabelsByHandleID(src, dst, h); len(t) > 0 {
			s.handleTypes[h] = t
		}
		if p := g.EdgePropertiesByHandleID(src, dst, h); len(p) > 0 {
			s.handleProps[h] = p
		}
	}
	return s
}

// foldLegacyStore folds g with [foldLegacyUndirected] when legacy is an
// undirected store, and does nothing otherwise.
func foldLegacyStore[N comparable, W any](g *lpg.Graph[N, W], legacy *legacyReplay) error {
	if legacy == nil || !legacy.undirected {
		return nil
	}
	return foldLegacyUndirected(g)
}

// legacyPair is one unordered pair of a two-sided legacy image, lo < hi.
type legacyPair struct{ lo, hi graph.NodeID }

// foldLegacyUndirected folds g, a legacy undirected graph replayed with both
// directions of every edge (rmp #3072), to one relationship per edge, oriented
// from the lower node id to the higher. For every pair it keeps the lower→higher
// arcs; a higher→lower arc whose mirror is missing is re-oriented and kept with
// its handle and weight. What the higher→lower direction holds is merged into
// the kept relationships when the lower→higher direction does not hold it, and
// the higher→lower arcs and records are then removed. A self-loop has one
// direction and is left as it is.
//
// It refuses, before changing g, with an error wrapping
// [ErrLegacyMirrorConflict] when the two directions disagree.
func foldLegacyUndirected[N comparable, W any](g *lpg.Graph[N, W]) error {
	m := g.AdjList().Mapper()
	pairs := map[legacyPair]struct{}{}
	m.Walk(func(id graph.NodeID, _ N) bool {
		nbs, _, _ := g.AdjList().LoadEntryH(id)
		for _, nb := range nbs {
			if nb < id {
				pairs[legacyPair{nb, id}] = struct{}{}
			}
		}
		return true
	})
	order := slices.SortedFunc(maps.Keys(pairs), func(a, b legacyPair) int {
		return cmp.Or(cmp.Compare(a.lo, b.lo), cmp.Compare(a.hi, b.hi))
	})
	type plan struct {
		pair     legacyPair
		fwd, rev legacySide[W]
	}
	plans := make([]plan, 0, len(order))
	conflicts := make([]string, 0, legacyConflictListLimit)
	for _, p := range order {
		pl := plan{pair: p, fwd: readLegacySide(g, p.lo, p.hi), rev: readLegacySide(g, p.hi, p.lo)}
		conflicts = append(conflicts, legacyConflicts(p, &pl.fwd, &pl.rev)...)
		plans = append(plans, pl)
	}
	if len(conflicts) > 0 {
		metrics.IncCounter("store.recovery.legacyUndirected.conflicts", uint64(len(conflicts)))
		shown := conflicts[:min(len(conflicts), legacyConflictListLimit)]
		msg := strings.Join(shown, "; ")
		if more := len(conflicts) - len(shown); more > 0 {
			msg = fmt.Sprintf("%s; and %d more", msg, more)
		}
		return fmt.Errorf("%w: %d conflicts: %s", ErrLegacyMirrorConflict, len(conflicts), msg)
	}
	for i := range plans {
		lo, _ := m.Resolve(plans[i].pair.lo)
		hi, _ := m.Resolve(plans[i].pair.hi)
		if err := foldLegacyPair(g, plans[i].pair, lo, hi, &plans[i].fwd, &plans[i].rev); err != nil {
			return fmt.Errorf("fold (%d)-(%d): %w", plans[i].pair.lo, plans[i].pair.hi, err)
		}
	}
	return nil
}

// legacyConflicts names every value the two directions of p disagree on.
func legacyConflicts[W any](p legacyPair, fwd, rev *legacySide[W]) []string {
	var out []string
	name := func(what string) string { return fmt.Sprintf("(%d)-(%d) %s", p.lo, p.hi, what) }
	for _, k := range conflictingKeys(fwd.props, rev.props) {
		out = append(out, name(fmt.Sprintf("property %q", k)))
	}
	if len(fwd.overflow) > 0 && len(rev.overflow) > 0 && !sameTypeSet(fwd.overflow, rev.overflow) {
		out = append(out, name("relationship types"))
	}
	for _, h := range slices.Sorted(maps.Keys(rev.arcs)) {
		if _, both := fwd.arcs[h]; !both {
			continue
		}
		a, b := fwd.slotType[h], rev.slotType[h]
		ta, tb := fwd.handleTypes[h], rev.handleTypes[h]
		if (a != "" && b != "" && a != b) || (len(ta) > 0 && len(tb) > 0 && !sameTypeSet(ta, tb)) {
			out = append(out, name(fmt.Sprintf("relationship %d type", h)))
		}
		for _, k := range conflictingKeys(fwd.handleProps[h], rev.handleProps[h]) {
			out = append(out, name(fmt.Sprintf("relationship %d property %q", h, k)))
		}
	}
	return out
}

// conflictingKeys returns, sorted, the keys a and b both hold with different
// values.
func conflictingKeys(a, b map[string]lpg.PropertyValue) []string {
	var out []string
	for k, va := range a {
		if vb, ok := b[k]; ok && !sameLegacyValue(va, vb) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// sameTypeSet reports whether a and b hold the same set of types.
func sameTypeSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// sameLegacyValue reports whether a and b are the same property value: the
// same kind and the same payload, a float compared by its bits and a time by
// its instant and zone offset.
func sameLegacyValue(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		x, _ := a.String()
		y, _ := b.String()
		return x == y
	case lpg.PropInt64:
		x, _ := a.Int64()
		y, _ := b.Int64()
		return x == y
	case lpg.PropFloat64:
		x, _ := a.Float64()
		y, _ := b.Float64()
		return math.Float64bits(x) == math.Float64bits(y)
	case lpg.PropBool:
		x, _ := a.Bool()
		y, _ := b.Bool()
		return x == y
	case lpg.PropTime:
		x, _ := a.Time()
		y, _ := b.Time()
		_, ox := x.Zone()
		_, oy := y.Zone()
		return x.Equal(y) && ox == oy
	case lpg.PropBytes:
		x, _ := a.Bytes()
		y, _ := b.Bytes()
		return bytes.Equal(x, y)
	case lpg.PropList:
		x, _ := a.List()
		y, _ := b.List()
		return slices.EqualFunc(x, y, sameLegacyValue)
	}
	return false
}

// foldLegacyPair folds the pair p, whose endpoints are the keys lo and hi and
// whose two directions agree, into its lower→higher direction.
func foldLegacyPair[N comparable, W any](g *lpg.Graph[N, W], p legacyPair, lo, hi N, fwd, rev *legacySide[W]) error {
	if len(rev.arcs) == 0 {
		return nil
	}
	handles := slices.Sorted(maps.Keys(rev.arcs))
	var reoriented uint64
	for _, h := range handles {
		if _, ok := fwd.arcs[h]; ok {
			continue
		}
		if _, err := g.AddEdgeHIfAbsent(lo, hi, rev.arcs[h], h); err != nil {
			return err
		}
		reoriented++
	}
	if reoriented > 0 {
		metrics.IncCounter("store.recovery.legacyUndirected.reoriented", reoriented)
	}
	for k, v := range rev.props {
		if _, ok := fwd.props[k]; !ok {
			if err := g.SetEdgeProperty(lo, hi, k, v); err != nil {
				return err
			}
		}
	}
	if len(fwd.overflow) == 0 {
		for _, t := range rev.overflow {
			if _, err := g.AddEdgeRelTypeOverflowByID(p.lo, p.hi, t); err != nil {
				return err
			}
		}
	}
	// The canonical slot order of the lower→higher direction, re-oriented arcs
	// included.
	var ordered []uint64
	nbs, _, hs := g.AdjList().LoadEntryH(p.lo)
	for i, nb := range nbs {
		if nb == p.hi && i < len(hs) {
			ordered = append(ordered, hs[i])
		}
	}
	slices.SortStableFunc(ordered, cmp.Compare[uint64])
	for _, h := range handles {
		if t := rev.slotType[h]; t != "" && fwd.slotType[h] == "" {
			if _, err := g.SetEdgeRelTypeAtSlotByID(p.lo, p.hi, slices.Index(ordered, h), t); err != nil {
				return err
			}
		}
		if len(fwd.handleTypes[h]) == 0 {
			for _, t := range rev.handleTypes[h] {
				if err := g.SetEdgeLabelByHandleID(p.lo, p.hi, h, t); err != nil {
					return err
				}
			}
		}
		for k, v := range rev.handleProps[h] {
			if _, ok := fwd.handleProps[h][k]; !ok {
				if err := g.SetEdgePropertyByHandleID(p.lo, p.hi, h, k, v); err != nil {
					return err
				}
			}
		}
	}
	for _, h := range handles {
		if _, err := g.RemoveEdgeByHandle(hi, lo, h); err != nil {
			return err
		}
	}
	return nil
}
