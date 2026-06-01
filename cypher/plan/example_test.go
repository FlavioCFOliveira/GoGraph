package plan_test

// example_test.go — runnable godoc examples for the cost-based planner (#1112).
// They show two exported planner entry points: the plan-cache key function and
// the expand-direction selector that picks the cheaper traversal side.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/plan"
)

// ExampleCacheKey computes a stable plan-cache key for a query and its
// parameter type names. The key is independent of parameter order.
func ExampleCacheKey() {
	a := plan.CacheKey("MATCH (n) RETURN n", []string{"Integer", "String"})
	b := plan.CacheKey("MATCH (n) RETURN n", []string{"String", "Integer"})

	// Same query and the same param types (in any order) hash identically.
	fmt.Println("order-independent:", a == b)

	c := plan.CacheKey("MATCH (n:Person) RETURN n", nil)
	fmt.Println("different query differs:", a != c)
	// Output:
	// order-independent: true
	// different query differs: true
}

// fixedEstimator is a minimal Estimator returning canned cardinalities so the
// example output is deterministic.
type fixedEstimator struct {
	labelCounts map[uint32]uint64
}

func (e fixedEstimator) LabelCount(label uint32) uint64                { return e.labelCounts[label] }
func (e fixedEstimator) HashLookupCount(uint32, any) uint64            { return 0 }
func (e fixedEstimator) BTreeRangeCount(uint32, string, string) uint64 { return 0 }
func (e fixedEstimator) AvgOutDegree(uint32, uint32, uint32) float64   { return 4.0 }

// ExampleSelectExpandDirection shows the planner choosing to expand from the
// more selective (smaller) side of an undirected relationship. Here the dst
// label has far fewer nodes, so the planner expands IN.
func ExampleSelectExpandDirection() {
	const (
		srcLabel uint32 = 1
		relType  uint32 = 10
		dstLabel uint32 = 2
	)
	est := fixedEstimator{labelCounts: map[uint32]uint64{
		srcLabel: 10_000, // many source nodes
		dstLabel: 100,    // few destination nodes
	}}

	decision := plan.SelectExpandDirection(srcLabel, relType, dstLabel, est)

	dir := "OUT"
	if decision.Dir == plan.ExpandIn {
		dir = "IN"
	}
	fmt.Printf("direction=%s frontier=%.0f\n", dir, decision.EstimatedFrontier)
	// Output:
	// direction=IN frontier=400
}
