package sim

// instanceKey returns the key of the first modelled label relationship from
// k.src to k.dst (the lowest discriminator), so a test that perturbs the model
// by pair reaches the instance a CREATE added under a synthetic discriminator.
// It returns k unchanged when the pair has none.
func (o *GraphOracle) instanceKey(k edgeKey) edgeKey {
	if ks := o.edgeInstances(k.src, k.dst, k.label); len(ks) > 0 {
		return ks[0]
	}
	return k
}
