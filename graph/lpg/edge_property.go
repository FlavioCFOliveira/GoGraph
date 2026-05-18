package lpg

// SetEdgeProperty records the named property on the directed edge
// (src, dst). The edge must already exist; otherwise the call is a
// no-op (mirroring SetEdgeLabel).
func (g *Graph[N, W]) SetEdgeProperty(src, dst N, key string, value PropertyValue) {
	if !g.adj.HasEdge(src, dst) {
		return
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	keyID := g.pkeys.Intern(key)
	g.propMu.Lock()
	k := edgeKey{src: srcID, dst: dstID}
	bag, ok := g.edgeProps[k]
	if !ok {
		bag = make(map[PropertyKeyID]PropertyValue)
		g.edgeProps[k] = bag
	}
	bag[keyID] = value
	g.propMu.Unlock()
}

// GetEdgeProperty returns the property value attached to the
// directed edge (src, dst) under key.
func (g *Graph[N, W]) GetEdgeProperty(src, dst N, key string) (PropertyValue, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return PropertyValue{}, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return PropertyValue{}, false
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	g.propMu.RLock()
	defer g.propMu.RUnlock()
	bag, ok := g.edgeProps[edgeKey{src: srcID, dst: dstID}]
	if !ok {
		return PropertyValue{}, false
	}
	v, ok := bag[keyID]
	return v, ok
}

// DelEdgeProperty removes the named property from the directed edge
// (src, dst). No-op if absent.
func (g *Graph[N, W]) DelEdgeProperty(src, dst N, key string) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return
	}
	g.propMu.Lock()
	k := edgeKey{src: srcID, dst: dstID}
	if bag, ok2 := g.edgeProps[k]; ok2 {
		delete(bag, keyID)
		if len(bag) == 0 {
			delete(g.edgeProps, k)
		}
	}
	g.propMu.Unlock()
}

// EdgeProperties returns a snapshot of every property currently
// attached to the directed edge (src, dst).
func (g *Graph[N, W]) EdgeProperties(src, dst N) map[string]PropertyValue {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	g.propMu.RLock()
	bag, ok := g.edgeProps[edgeKey{src: srcID, dst: dstID}]
	if !ok {
		g.propMu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, len(bag))
	for k, v := range bag {
		if name, ok := g.pkeys.Resolve(k); ok {
			out[name] = v
		}
	}
	g.propMu.RUnlock()
	return out
}
