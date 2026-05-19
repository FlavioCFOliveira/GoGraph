// Example 10_dimacs9_routing — exercises the DIMACS 9 SSSP
// harness over a small synthetic road graph.
package main

import (
	"context"
	"fmt"

	"gograph/bench/dimacs9"
)

func main() {
	rep := dimacs9.Run(context.Background(), dimacs9.Default())
	fmt.Printf("Ingest:  %v\n", rep.IngestTime)
	fmt.Printf("Build:   %v\n", rep.BuildTime)
	fmt.Printf("p50:     %v\n", rep.Percentile(0.5))
	fmt.Printf("p95:     %v\n", rep.Percentile(0.95))
	fmt.Printf("p99:     %v\n", rep.Percentile(0.99))
}
