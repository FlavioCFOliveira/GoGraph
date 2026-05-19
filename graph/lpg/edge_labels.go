package lpg

// EdgeLabels returns the names of every label attached to the
// directed edge (src, dst) in unspecified order. The returned slice
// is freshly allocated and may be mutated by the caller. If either
// endpoint is unknown or the endpoint pair has no labels attached,
// EdgeLabels returns nil.
//
// EdgeLabels is the dual of [Graph.NodeLabels]. It is safe for
// concurrent use; the snapshot is taken under the per-edge RWMutex
// and the registry's own lock.
func (g *Graph[N, W]) EdgeLabels(src, dst N) []string {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	g.edgeMu.RLock()
	bag, ok := g.edgeBag[edgeKey{src: srcID, dst: dstID}]
	if !ok {
		g.edgeMu.RUnlock()
		return nil
	}
	out := make([]string, 0, len(bag))
	for lid := range bag {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	g.edgeMu.RUnlock()
	return out
}
