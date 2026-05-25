package lpg

// SetEdgeProperty records the named property on the directed edge
// (src, dst). The edge must already exist; otherwise the call is a
// no-op (mirroring SetEdgeLabel). Returns any error returned by the
// installed [SchemaValidator]; when the validator rejects the write the
// graph state is left unchanged.
func (g *Graph[N, W]) SetEdgeProperty(src, dst N, key string, value PropertyValue) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	if !g.adj.HasEdge(src, dst) {
		return nil
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	keyID := g.pkeys.Intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	s := g.edgePropShardFor(k)
	s.mu.Lock()
	bag, ok := s.m[k]
	if !ok {
		bag = make(map[PropertyKeyID]PropertyValue)
		s.m[k] = bag
	}
	bag[keyID] = value
	s.mu.Unlock()
	return nil
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
	k := edgeKey{src: srcID, dst: dstID}
	s := g.edgePropShardFor(k)
	s.mu.RLock()
	defer s.mu.RUnlock()
	bag, ok := s.m[k]
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
	k := edgeKey{src: srcID, dst: dstID}
	s := g.edgePropShardFor(k)
	s.mu.Lock()
	if bag, ok2 := s.m[k]; ok2 {
		delete(bag, keyID)
		if len(bag) == 0 {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
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
	k := edgeKey{src: srcID, dst: dstID}
	s := g.edgePropShardFor(k)
	s.mu.RLock()
	bag, ok := s.m[k]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, len(bag))
	for kk, v := range bag {
		if name, ok := g.pkeys.Resolve(kk); ok {
			out[name] = v
		}
	}
	s.mu.RUnlock()
	return out
}
