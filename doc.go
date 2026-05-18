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
package gograph
