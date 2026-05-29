// Command 25_software_house_api is a persistent REST WebAPI that
// demonstrates how to build, query and mutate a multi-layer Labeled
// Property Graph (LPG) with GoGraph in a production-shaped service. The
// domain is task management inside a software-house: one graph spans the
// code base, the work that changes it, and the people who do that work.
//
// The full data model, REST contract and maintenance-query catalogue are
// specified in SPEC.md alongside this file. This doc comment summarises
// the runtime surface.
//
// # The multi-layer model
//
// A single LPG holds three layers, distinguished by a layer label on
// every node (each node also carries a type label):
//
//   - Code   — Repository, Module, Component, joined by CONTAINS
//     (containment) and DEPENDS_ON (the dependency graph).
//   - Work   — Task, Sprint, WorkflowState, joined by SUBTASK_OF, NEXT,
//     BLOCKS, HAS_STATE and IN_SPRINT.
//   - People — Developer, Team, joined by MEMBER_OF.
//
// Two inter-layer "coupling" edges stitch the layers together and form the
// spine of every cross-layer question:
//
//	(:Developer)-[:ASSIGNED_TO]->(:Task)-[:TOUCHES]->(:Component)
//
// Completed work carries TOUCHES edges (realised history); planned work
// carries only ASSIGNED_TO {state:'planned'}. See SPEC.md §5.
//
// # Endpoints
//
// The server listens on the address given by -addr (default :8080) and
// exposes four routes. Request and response bodies are JSON.
//
//	POST /query
//	    Run an arbitrary Cypher statement (read or write). Request:
//	        {"query": "<cypher>", "params": {<optional>}}
//	    Success (200) returns one object per row keyed by the output
//	    columns:
//	        {"columns": ["..."], "rows": [{"col": value, ...}]}
//	    A write-only statement (no RETURN) returns
//	    {"columns":[],"rows":[]} after the write is durably committed.
//
//	POST /seed
//	    Idempotently load the deterministic fixture. Returns
//	        {"seeded": <bool>, "status": "ok"}
//	    seeded is false when the graph was already populated.
//
//	GET /stats
//	    Node counts by type label and edge counts by relationship type:
//	        {"nodes": {"Component": N, ...},
//	         "edges": {"DEPENDS_ON": N, ...}}
//
//	GET /healthz
//	    Liveness probe; returns {"status":"ok"} without touching the graph.
//
// Errors use a typed envelope {"error": "<message>", "kind": "<kind>"}
// with the matching status: 400 (malformed JSON or a Cypher syntax /
// unsupported-feature error), 405 (wrong method), 413 (body over 1 MiB),
// 422 (Cypher semantic or bad-parameter error), 500 (runtime), 503
// (shutting down).
//
// # Persistence and recovery
//
// The data directory given by -d holds <dir>/wal (the append-only,
// CRC-framed write-ahead log) and <dir>/snapshot/* (a manifest plus the
// CSR, labels, properties and mapper images). Every committed write is
// fsynced to the WAL before the commit is acknowledged, so the store is
// kill -9 safe: a crash with no clean shutdown still recovers every
// acknowledged write by replaying the WAL on the next start. On startup
// the server calls recovery.OpenCtx, which loads the snapshot and replays
// any WAL tail; on graceful shutdown it writes a final snapshot (an
// optimisation that shortens the next replay) and closes the WAL.
//
// # Concurrency and isolation
//
// The store is opened once and shared by every handler. The Cypher
// engine's read execution is lock-free over an immutable snapshot and
// write commits are atomic; however, its plan- and filter-building phase
// reads the live adjacency offsets and interning tables, which a
// concurrent write mutates. The server therefore serialises access with an
// RWMutex: read queries take a shared lock and run in parallel, while write
// queries and the seed take the exclusive lock. The mutex is the outermost
// lock — acquired before any engine call — so it cannot invert with the
// store's internal single-writer mutex. The guarantee delivered is
// snapshot-isolation reads with serialised writes: a reader never observes
// a partially-applied write, and writers never overlap.
//
// # Lifecycle and flags
//
//	-d <dir>          data directory holding the WAL and snapshot (required)
//	-addr <host:port> HTTP listen address (default ":8080")
//
// On SIGINT or SIGTERM the server stops accepting connections, lets
// in-flight requests finish, writes a final snapshot, and closes the WAL,
// in that order. It exits 0 on a clean run, 1 on a runtime failure, and 2
// on a usage error.
//
// # Example session
//
//	go run ./examples/25_software_house_api -d /tmp/shop -addr :8080 &
//	curl -s -XPOST localhost:8080/seed
//	curl -s localhost:8080/stats
//	curl -s -XPOST localhost:8080/query \
//	    -d '{"query":"MATCH (c:Component)<-[:DEPENDS_ON]-(d) RETURN c.key AS component, count(d) AS inDegree ORDER BY inDegree DESC LIMIT 5"}'
//
// See README.md for the full maintenance-query catalogue with sample
// output and a kill -9 / restart persistence demonstration.
package main
