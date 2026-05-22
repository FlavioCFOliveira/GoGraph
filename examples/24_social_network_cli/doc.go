// Package main implements `24_social_network_cli`, an example one-shot CLI
// that demonstrates how to build, persist and query a labelled property
// graph for a social-network domain using GoGraph.
//
// The example exercises four pillars of the module in a single deliverable:
//
//  1. Graph initialisation with a labelled property graph (LPG) backend.
//  2. Crash-safe ACID persistence via a write-ahead log plus snapshots
//     (recovery.Open[string, float64] and snapshot.WriteSnapshotFull).
//  3. CRUD via Cypher through a WAL-backed engine
//     (cypher.NewEngineWithStore and Engine.RunInTx).
//  4. A small CLI surface that accepts ad-hoc Cypher queries from
//     positional arguments or stdin and streams results as JSON Lines.
//
// # Schema
//
// Node labels and their natural keys:
//
//   - User    — `username` (string, unique natural key)
//   - Post    — `id`       (string, unique natural key)
//   - Comment — `id`       (string, unique natural key)
//
// Node properties (all timestamps are ISO-8601 UTC strings, fixed values
// in the seed fixture so the regression baseline is byte-deterministic):
//
//   - User.username     string
//   - User.display_name string
//   - User.created_at   string
//   - Post.id           string
//   - Post.text         string
//   - Post.created_at   string
//   - Comment.id        string
//   - Comment.text      string
//   - Comment.created_at string
//
// Relationship types:
//
//   - (:User)-[:FOLLOWS]->(:User)             a user follows another user
//   - (:User)-[:AUTHORED]->(:Post|:Comment)   authorship of a post or comment
//   - (:Comment)-[:ON]->(:Post)               a comment is attached to a post
//   - (:Comment)-[:REPLY_OF]->(:Comment)      a comment is a reply to another
//   - (:User)-[:LIKED]->(:Post|:Comment)      polymorphic like edge
//
// # Subcommands
//
// All subcommands require the data directory flag `-d <dir>`. The
// directory holds the WAL plus snapshot files managed by `recovery` and
// `snapshot`. The CLI exits with code 0 on success, 1 on runtime failure
// (I/O, Cypher, validation), or 2 on usage errors (unknown subcommand,
// missing or malformed flags).
//
//	init -d <dir>
//	    Open or create the data directory. If the directory does not
//	    exist it is created (mkdir -p). An empty initial snapshot is
//	    written so that subsequent reopens via recovery.Open succeed
//	    even before any writes. Idempotent: running init twice
//	    on the same directory is a no-op.
//	    On success prints one JSON object:
//	        {"data_dir":"<absolute path>","status":"ok"}
//
//	seed -d <dir>
//	    Populate the graph with a deterministic fixture: 5 users
//	    (alice, bob, carol, dave, erin), 8 :FOLLOWS edges, 3 :Post
//	    nodes with their :AUTHORED edges, 5 :Comment nodes attached
//	    via :ON (some chained via :REPLY_OF) and 7 :LIKED edges
//	    spanning both posts and comments. The writes go through the
//	    direct txn.Store / txn.Tx API, mirroring the canonical
//	    pattern in examples/04_persistence so the seed remains
//	    independent of the Cypher write planner. Idempotent: running
//	    seed twice is a no-op when at least one :User node is
//	    already present. The reply is:
//	        {"seeded":<bool>,"status":"ok"}
//
//	query -d <dir> [cypher]
//	    Run a Cypher query (read or write) against the data directory.
//	    The query is read from the positional argument; if absent, the
//	    entire stdin stream is consumed and used as the query. Each
//	    Cypher Record is emitted as a single JSON object on its own
//	    line (JSON Lines, RFC 7464 framing). No envelope, no summary.
//	    Examples:
//	        social query -d data 'MATCH (u:User) RETURN u.username AS username'
//	        echo 'MATCH (p:Post) RETURN p.id AS id' | social query -d data
//
//	snapshot -d <dir>
//	    Force a manual checkpoint by calling
//	    snapshot.WriteSnapshotFull on the current in-memory state.
//	    On success prints one JSON object containing the snapshot
//	    directory and the manifest path:
//	        {"snapshot_dir":"<dir>","status":"ok"}
//
//	stats -d <dir>
//	    Count nodes by label and edges by relationship type. The
//	    output is a single JSON object with alphabetically ordered
//	    integer keys:
//	        {"authored":N,"comments":N,"follows":N,"likes":N,
//	         "on":N,"posts":N,"replies":N,"users":N}
//	    (`replies` counts :REPLY_OF edges.)
//
// # Output Format (JSON Lines)
//
// Every Cypher Record returned by `query` is encoded as one JSON object
// per line, terminated by `\n`. Map keys are emitted in alphabetical
// order so that the byte stream is reproducible.
//
// Value type mapping from the Cypher runtime value model (expr.Value)
// to JSON, performed by output.go's jsonValue / jsonExprValue helpers:
//
//   - expr.IntegerValue       -> JSON integer
//   - expr.FloatValue         -> JSON float
//   - expr.StringValue        -> JSON string
//   - expr.BoolValue          -> JSON boolean
//   - expr.ListValue          -> JSON array
//   - expr.MapValue           -> JSON object (alphabetically keyed)
//   - expr.NodeValue          -> JSON object with the leading-underscore
//     fields {_id, _labels, _properties}
//     (neo4j-go-driver compatible)
//   - expr.RelationshipValue  -> JSON object with the fields
//     {_id, _type, _start, _end, _properties}
//   - expr.Null               -> JSON null
//   - graph.NodeID            -> JSON integer
//   - native Go scalars       -> passthrough
//   - []byte                  -> JSON string (avoids base64)
//   - other values            -> Stringer or %v fallback
//
// A write-only Cypher statement (CREATE / SET / DELETE without RETURN)
// produces one synthetic empty row that the engine uses to drive its
// pipeline; query filters those rows out so the stream stays a
// faithful "rows" view.
//
// # Persistence Contract
//
// The data directory contains <dir>/wal (the append-only write-ahead
// log) and <dir>/snapshot/* (manifest plus csr.bin, labels.bin,
// properties.bin and any per-index files). On open, recovery.Open
// (the canonical [string, float64] generic entry point — the
// deprecated OpenString wrapper is avoided) loads the most recent
// valid snapshot and replays the WAL tail to rebuild the in-memory
// graph. Every write performed through Engine.RunInTx is appended to
// the WAL with fsync at commit, so a process crash mid-write leaves
// the data directory recoverable.
//
// The CLI is one-shot: every invocation opens the data directory,
// performs its operation, and closes the recovery handle. There is no
// background process and no long-running file lock between invocations.
//
// # Example Invocation
//
// A typical end-to-end session:
//
//	go run ./examples/24_social_network_cli init     -d /tmp/social
//	go run ./examples/24_social_network_cli seed     -d /tmp/social
//	go run ./examples/24_social_network_cli stats    -d /tmp/social
//	go run ./examples/24_social_network_cli query    -d /tmp/social \
//	    'MATCH (u:User)-[:FOLLOWS]->(v:User) RETURN u.username AS from, v.username AS to'
//	go run ./examples/24_social_network_cli snapshot -d /tmp/social
//
// # History
//
// The three engine constraints originally documented here — CREATE
// with RETURN, multi-edge CREATE, and cross-process snapshot drift —
// were fixed in Sprint 56 of the gograph roadmap (tasks #498, #499,
// #500). The corresponding regression tests live in
// cypher/write_with_return_test.go, cypher/multi_edge_create_test.go,
// graph/mapper_stable_test.go and the cross-process round-trip in
// cross_process_test.go in this package.
package main
