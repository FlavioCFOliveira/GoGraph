package flow

import (
	"errors"
	"math/bits"
)

// capInf is the "infinite push" sentinel used as the initial bottleneck
// in every max-flow routine ([MaxFlowCtx], [EdmondsKarpCtx],
// [MinCostMaxFlowCtx]) and as the unreachable-distance marker in the
// successive-shortest-paths Dijkstra. A real capacity must stay strictly
// below it, otherwise a genuine edge would be indistinguishable from the
// sentinel and the bottleneck arithmetic would be wrong.
const capInf = 1 << 62

// ErrCapacityOverflow is returned by the context-aware flow entry points
// ([MaxFlowCtx], [EdmondsKarpCtx], [PushRelabelMaxFlowCtx],
// [MinCostMaxFlowCtx]) when the input network's capacities (or, for the
// min-cost variant, the capacity-times-cost product) cannot be summed
// without overflowing int64.
//
// The residual updates and cost accumulation in the augmenting loops use
// unchecked native int arithmetic for speed; this boundary check is the
// single point at which an overflow-prone network is rejected, so the
// hot loop can trust its inputs. Under the module's "fail-stop, never
// fail-silent" contract a network that would silently produce a wrapped
// (negative) max-flow or min-cost must be refused here rather than
// returning a corrupt result.
//
// The non-context entry points ([MaxFlow], [EdmondsKarp],
// [PushRelabelMaxFlow], [MinCostMaxFlow]) cannot surface an error in
// their signature; on a violation they return the zero result
// (0, or (0, 0) for min-cost), mirroring how [MinCostMaxFlow] already
// swallows [ErrNegativeCycle].
var ErrCapacityOverflow = errors.New("flow: edge capacities or costs overflow int64")

// validateCapacities checks that a [Network]'s capacities cannot drive
// the max-flow accumulation past int64. It enforces two conditions:
//
//   - every forward edge capacity is in the range [0, capInf): a
//     capacity at or above the sentinel would alias the "infinite push"
//     marker and corrupt the bottleneck computation;
//   - the sum of the capacities leaving src is below capInf.
//
// The second condition is conservative but sufficient: the value of any
// s-t flow is bounded above by the capacity of the cut ({src}, V\{src}),
// i.e. the sum of capacities out of src. Keeping that sum below capInf
// therefore bounds the returned flow (and hence the running total and
// every residual back-edge increment, since a back-edge accumulates at
// most the flow through its forward edge) strictly below the sentinel,
// so no accumulation can wrap.
//
// Cost: O(E) over the source's adjacency plus an O(1) scan attempt; it
// runs once at the public boundary and never inside the augmenting loop.
func validateCapacities(g *Network, src int) error {
	for _, c := range g.cap {
		if c < 0 || c >= capInf {
			return ErrCapacityOverflow
		}
	}
	if src < 0 || src >= len(g.heads) {
		return nil // out-of-range src is handled by the callers' own guards
	}
	sum := 0
	for _, e := range g.heads[src] {
		// Only forward residual arcs carry positive capacity initially;
		// reverse arcs start at zero so they never inflate the cut sum.
		c := g.cap[e]
		if c <= 0 {
			continue
		}
		sum += c
		if sum < 0 || sum >= capInf {
			return ErrCapacityOverflow
		}
	}
	return nil
}

// validateCostCapacities extends [validateCapacities] for a
// [CostNetwork]: in addition to bounding the flow it bounds the total
// cost. The total cost of a min-cost max-flow is at most
// maxFlow * maxAbsCost, where maxFlow is bounded by the source cut and
// maxAbsCost is the largest absolute per-unit cost on any arc. The
// product is computed with [bits.Mul64] so the overflow test itself
// cannot overflow.
//
// Rejecting here keeps the totalCost += push * g.cost[e] accumulation in
// [MinCostMaxFlowCtx] free of per-step overflow checks while still
// honouring "fail-stop, never fail-silent".
func validateCostCapacities(g *CostNetwork, src int) error {
	if err := validateCapacities(g.Network, src); err != nil {
		return err
	}
	if src < 0 || src >= len(g.heads) {
		return nil
	}
	// Largest absolute per-unit cost over all arcs (forward and reverse;
	// reverse arcs carry the negated cost, so the magnitude matches).
	maxAbsCost := 0
	for _, cst := range g.cost {
		a := cst
		if a < 0 {
			a = -a
		}
		if a > maxAbsCost {
			maxAbsCost = a
		}
	}
	// Source-cut capacity bounds the achievable flow.
	srcCut := 0
	for _, e := range g.heads[src] {
		if c := g.cap[e]; c > 0 {
			srcCut += c
		}
	}
	// maxFlow * maxAbsCost must fit a non-negative int64. Both operands
	// are already known to be < capInf (1<<62) and non-negative, so the
	// high word is a sufficient overflow oracle.
	hi, lo := bits.Mul64(uint64(srcCut), uint64(maxAbsCost))
	if hi != 0 || lo > uint64(maxInt64) {
		return ErrCapacityOverflow
	}
	return nil
}

// maxInt64 is the largest representable int64; the cost-product test
// rejects any total cost that would not fit a signed 64-bit accumulator.
const maxInt64 = int64(^uint64(0) >> 1)
