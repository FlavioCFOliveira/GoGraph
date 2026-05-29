// Package gograph is a Go module for graph persistence, manipulation, and
// fast search.
//
// The library scales from small in-memory graphs to graphs too large to
// fit in RAM, while remaining idiomatic, allocation-conscious, and safe
// under high load and high concurrency.
//
// Subpackages provide the building blocks:
//
//   - graph         — core types, generic node identifiers, and graph interfaces.
//   - graph/adjlist — mutable adjacency-list backend.
//   - graph/csr     — immutable compressed sparse row view for analytics.
//   - graph/lpg     — labelled property graph model (labels, typed properties).
//   - graph/index   — secondary indexes (label bitmap, hash, B+ tree).
//   - graph/io      — importers and exporters (CSV, GraphML, DOT, JSON Lines).
//   - search        — traversal and path-finding algorithms.
//   - search/centrality, search/community, search/flow — analytics suites.
//   - store         — durable persistence (WAL, snapshots, mmap'd CSR).
//
// Subpackages are added incrementally per the project roadmap; the
// present package documents the top-level module only.
//
// # Common tasks and their entrypoints
//
// The following map points each common task at the function or type that
// starts it. Every link resolves to an exported symbol; follow it for the
// full signature and contract.
//
// Build a labelled property graph:
//
//   - [lpg.New] constructs a Graph[N, W]; add nodes, labels, typed
//     properties, and edges through its methods.
//
// Run a Cypher query:
//
//   - [cypher.NewEngine] wraps an in-memory lpg.Graph[string, float64].
//   - [cypher.Engine.Run] executes a query string with typed parameters.
//
// Run durable, WAL-backed Cypher queries:
//
//   - [cypher.NewEngineWithStore] binds the engine to a [txn.Store], so
//     writes are journalled and survive a crash.
//
// Pass parameters to a query:
//
//   - [cypher.Engine.RunAny] accepts plain Go values as parameters.
//   - [cypher.BindParams] converts a map of Go values into the typed
//     parameter map that [cypher.Engine.Run] expects.
//
// Find a shortest path (weighted):
//
//   - [search.Dijkstra] for non-negative edge weights.
//   - [search.AStar] when an admissible heuristic is available.
//
// Traverse without weights:
//
//   - [search.BFS] for breadth-first order and unweighted distances.
//   - [search.DFS] for depth-first order.
//
// Compute analytics:
//
//   - [centrality.PageRank] for influence ranking.
//   - [community.Leiden] (or [community.LabelPropagation]) for community
//     detection; pair with [community.DefaultLeidenOptions].
//   - [flow.MaxFlow] / [flow.MinCostMaxFlow] for network-flow problems.
//
// Import and export graphs:
//
//   - CSV: [csv.ReadInto] and [csv.Write].
//   - GraphML: [graphml.ReadInto] / [graphml.ReadWithProps] and
//     [graphml.Write] / [graphml.WriteWithProps].
//   - JSON Lines: [jsonl.ReadInto] / [jsonl.ReadWithProps] and
//     [jsonl.Write] / [jsonl.WriteWithProps].
//   - DOT (export only): [dot.Write].
//
// Persist and recover:
//
//   - [wal.Open] opens a write-ahead log for appending frames.
//   - [snapshot.WriteSnapshotFull] writes a full CSR-plus-labels snapshot
//     to a directory.
//   - [recovery.Open] reconstructs a graph from a snapshot and its WAL.
//
// Serve the Bolt protocol:
//
//   - [server.NewServer] starts a Bolt v5 server backed by a [cypher.Engine].
//
// # NodeID space, MaxNodeID, and live nodes
//
// The graph.Mapper interns user keys into compact NodeIDs using a
// 256-way sharded layout; the shard index occupies the top byte of
// each NodeID. As a result MaxNodeID() typically rounds up well above
// the number of distinct keys, and analytical algorithms that
// allocate per-NodeID buffers (rank vectors, community-ID slices)
// produce slices of length MaxNodeID() with sentinel values in the
// "ghost" slots. Use graph/csr.CSR.LiveMask, LiveNodes, or LiveCount
// to iterate only the meaningful results.
//
// See docs/maxnodeid.md for a worked example and recipes for
// translating live NodeIDs back to user keys via Mapper.Resolve.
package gograph
