# Example 23 — Bolt v5 server round-trip

## What it demonstrates

GoGraph speaking the Bolt v5 wire protocol end to end: it starts the
embedded `bolt/server` over an in-memory graph, connects the official
`neo4j-go-driver/v5` as a real client, runs a Cypher query through a
session, prints the returned rows, and shuts the server down cleanly
with no goroutine left behind.

## Domain / scenario

A trivial social graph: three `:Person` nodes — `Alice`, `Bob`, and
`Carol` — each with a `name` property. The nodes are seeded in process
through the engine before the server starts. A Bolt client then issues
`MATCH (n:Person) RETURN n.name AS name ORDER BY name` over the network
and reads the ordered names back off the wire.

The listener binds to `127.0.0.1:0`, so the kernel assigns a free port;
the client discovers it from `ln.Addr()`. The server is fully
compatible with `neo4j-go-driver/v5` and `cypher-shell`.

## How to run

```sh
go run ./examples/23_bolt_server
```

## Expected output

The query result — the three ordered names and the row count — is
deterministic. The listener address on the first line varies per run
because the port is OS-assigned.

```
GoGraph Bolt v5 server listening on 127.0.0.1:55710
Compatible with neo4j-go-driver v5 and cypher-shell.
Client query: MATCH (n:Person) RETURN n.name AS name ORDER BY name
Returned 3 rows:
  name = Alice
  name = Bob
  name = Carol
Server shut down cleanly.
```

The `127.0.0.1:55710` port is illustrative; every run prints a
different OS-assigned port.

## Key APIs

- `bolt/server.NewServer` / `Server.Serve` / `Server.Shutdown` — start the Bolt v5 TCP server on a listener and tear it down gracefully, draining every per-connection goroutine.
- `bolt/server.Options` — bound concurrent connections (`MaxConnections`) and set the per-connection idle deadline (`ConnTimeout`).
- `cypher.NewEngine` / `Engine.RunInTxAny` — build the query engine over an in-memory graph and seed `:Person` nodes via an in-process write transaction.
- `graph/lpg.New` — the in-memory labelled property graph the engine serves.
- `github.com/neo4j/neo4j-go-driver/v5/neo4j` — the official Bolt client: `NewDriverWithContext`, `NewSession`, `Session.Run`, `Result.Collect`, and clean `Close` on teardown.

## Further reading

- [`bolt/server`](../../bolt/server) — the Bolt v5 server package documentation
- [`bolt/server/example_test.go`](../../bolt/server/example_test.go) — the in-repo precedent this example mirrors for the client round-trip and no-leak teardown
- [`cypher`](../../cypher) — the Cypher query engine the server executes against
- [Example 22 — Cypher engine](../22_cypher) — running Cypher in process without the Bolt wire layer
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
