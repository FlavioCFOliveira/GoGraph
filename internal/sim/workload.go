package sim

// Workload is a weighted mix of actors. On each tick the simulator asks the
// workload to select an actor (by weight) and that actor produces the next
// operation. The weights need not sum to 1; [Workload.SelectActor] normalises
// against their running total.
//
// # Concurrency contract
//
// Workload is NOT safe for concurrent use; it is consulted from the single
// simulation goroutine. The Seed passed to SelectActor is the same shared
// single-goroutine seed, preserving determinism.
type Workload struct {
	Actors  []Actor
	Weights []float64
}

// DefaultWorkload returns a balanced mix: 40% writer, 60% reader. The seed is
// accepted for symmetry with the other constructors (and to allow future
// seed-dependent compositions) but the default mix is fixed.
func DefaultWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{HonestWriter{}, HonestReader{}},
		Weights: []float64{0.4, 0.6},
	}
}

// WriteHeavyWorkload returns an 80% writer / 20% reader mix, stressing the
// write and recovery paths.
func WriteHeavyWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{HonestWriter{}, HonestReader{}},
		Weights: []float64{0.8, 0.2},
	}
}

// ReadHeavyWorkload returns a 20% writer / 80% reader mix, stressing the read
// path and isolation.
func ReadHeavyWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{HonestWriter{}, HonestReader{}},
		Weights: []float64{0.2, 0.8},
	}
}

// BadActorWorkload returns a mix that injects a [MalformedSender] alongside the
// honest actors (50% writer, 30% reader, 20% malformed), so the safety loop
// continuously exercises the engine's rejection paths while honest traffic keeps
// the graph populated. The malformed traffic must never panic, corrupt state, or
// trip an invariant: each ill-formed op is modelled by the oracle as a no-op, so
// a clean run sees engine and oracle stay in lock-step across every rejection.
func BadActorWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{HonestWriter{}, HonestReader{}, MalformedSender{}},
		Weights: []float64{0.5, 0.3, 0.2},
	}
}

// SteadyStateWorkload returns a mix tuned to keep the modelled graph BOUNDED
// over a very long run: a writer that creates and deletes in equal measure
// (via [BoundedChurnWriter]) plus a reader. It is the long-running scenario's
// workload — the point there is heap/goroutine stability across millions of
// small ops, which requires the working set not to grow without bound.
func SteadyStateWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{BoundedChurnWriter{}, HonestReader{}},
		Weights: []float64{0.6, 0.4},
	}
}

// SelectActor returns one actor chosen with probability proportional to its
// weight, drawing a single float64 from seed. It panics if the workload has no
// actors (a programmer error). A non-positive total weight falls back to a
// uniform first-actor choice rather than dividing by zero.
func (w *Workload) SelectActor(seed *Seed) Actor {
	if len(w.Actors) == 0 {
		panic("sim: SelectActor on empty workload")
	}
	var total float64
	for _, weight := range w.Weights {
		if weight > 0 {
			total += weight
		}
	}
	if total <= 0 {
		_ = seed.Float64() // keep the draw stream stable.
		return w.Actors[0]
	}
	target := seed.Float64() * total
	var cum float64
	for i, actor := range w.Actors {
		if i < len(w.Weights) && w.Weights[i] > 0 {
			cum += w.Weights[i]
			if target < cum {
				return actor
			}
		}
	}
	// Floating-point edge: target rounded up to total. Return the last actor.
	return w.Actors[len(w.Actors)-1]
}
