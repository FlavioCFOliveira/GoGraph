# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

Sprint 1 — Foundation & In-Memory Core. The module currently provides:

- `gograph/graph` — generic node identifiers and the `Graph[N, W]`
  contract.
- `gograph/graph/adjlist` — mutable, sharded adjacency-list backend
  with copy-on-write snapshots and lock-free reads.
- `gograph/graph/csr` — immutable Compressed Sparse Row view for
  read-mostly analytics.
- `gograph/search` — traversal and path-finding algorithms (BFS,
  iterative DFS, Dijkstra, Bellman-Ford, A\*, bidirectional BFS,
  topological sort (Kahn), Tarjan SCC).
- `gograph/ds` — disjoint-set (union-find) primitive.

## Getting Started

```go
package main

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("Lisbon", "Madrid", 624)
	a.AddEdge("Lisbon", "Paris", 1737)
	a.AddEdge("Madrid", "Paris", 1274)
	a.AddEdge("Madrid", "Rome", 1969)
	a.AddEdge("Paris", "Rome", 1422)

	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup("Lisbon")

	d, err := search.Dijkstra(c, src)
	if err != nil {
		panic(err)
	}
	for _, city := range []string{"Madrid", "Paris", "Rome"} {
		id, _ := a.Mapper().Lookup(city)
		dist, _ := d.Distance(id)
		fmt.Printf("Lisbon -> %s : %d km\n", city, dist)
	}
}
```

## Workflow

The project follows a strict `Specify -> Implement -> Test -> Document`
workflow. Sprint planning lives in the local `rmp` CLI roadmap. The
`Makefile` `ci` target runs the full validation pipeline:

```
make ci
```

The pipeline includes `go mod tidy`, `gofmt`, `go vet`, `go build`,
`go test`, `go test -race`, and `golangci-lint run`. Every change must
pass it before being committed.

## Performance

Benchmarks (Apple M4, Go 1.26.3):

| Operation | Throughput |
|---|---|
| `Mapper.Intern` (hot key) | 17 ns/op, 0 allocs |
| `adjlist.HasEdge` (hot cache) | 49 ns/op, 0 allocs |
| `csr.NeighboursByID` | 11 ns/op, 0 allocs |
| `csr.BuildFromAdjList` of 10^7 edges | 51 ms |
| `search.BFS` on 10^7-node chain | 38 ms, 1.25 MB peak, 0 allocs/call after warmup |
| `search.Dijkstra` on 1M-node / 4M-edge random graph | 320 ms |
| `search.BellmanFord` on 16K-vertex / 64K-edge | 1.8 ms |

## Module Layout

```
graph/        — core types and Mapper
graph/adjlist — mutable adjacency list (writer-side)
graph/csr     — immutable CSR (reader-side)
search/       — algorithms over CSR
ds/           — supporting data structures
```

## License

To be selected before the first public release.
