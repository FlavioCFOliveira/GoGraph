# Example 34 — Bolt transactions, writes, auth and TLS

## What it demonstrates

The Bolt v5 write and transaction surface of GoGraph's embedded server, driven
end to end with the official `neo4j-go-driver`. Where
[example 23](../23_bolt_server) measures read throughput, this example
exercises the parts of the protocol that carry the ACID and security
guarantees over the wire:

- **Authentication** — the server runs a `server.BasicAuthHandler` backed by
  `server.ConstantTimeValidate`; a client with the wrong password is rejected,
  the right one is accepted.
- **Write transactions** — a managed `ExecuteWrite` (BEGIN/RUN/COMMIT) creates
  a node, and the commit is observed by a follow-up read.
- **Rollback** — an explicit transaction that creates a node then `Rollback`s
  leaves the graph unchanged: atomicity on the abort path, over Bolt.
- **Failure classification and RESET recovery** — an invalid statement returns
  a Bolt FAILURE the driver surfaces as an error, after which the same session
  recovers (the driver issues RESET) and serves the next query.
- **TLS** — one authenticated read is repeated over an encrypted `bolt+ssc`
  connection against a server started from `server.DefaultTLSConfig` with a
  freshly generated self-signed certificate.

The server is secure-by-default — it refuses to start with a nil `Auth`
handler — so a real credential handler is wired here rather than `NoAuth`.

## Domain / scenario

A small user directory: `-persons` deterministically-named `:Person` nodes in
an in-memory multigraph engine. Each scenario is driven once, in sequence, so
the node-count assertions around the committed and rolled-back writes are
deterministic.

## How to run

```sh
go run ./examples/34_bolt_transactions                 # small deterministic default
go run ./examples/34_bolt_transactions -persons 100000 # observable-scale run
```

Run the package under the race detector to exercise the server/driver
concurrency and confirm no goroutine leaks:

```sh
go test -race ./examples/34_bolt_transactions/...
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-persons` | number of `:Person` nodes seeded | `200` | `100000` |
| `-password` | the Bolt credential the server requires | `correct-horse-battery-staple` | any |
| `-seed` | RNG seed (reserved) | `1` | any |

## Expected output

At the default config the deterministic **fact** lines are:

```
config.persons=200
auth.bad_rejected=true
auth.good_accepted=true
tx.write_committed=true
tx.rollback_discarded=true
error.failure_then_recovered=true
tls.query_succeeded=true
```

Interleaved with the facts is volatile **telemetry**, prefixed with `# `:

```
# scenarios.elapsed=8.30ms
```

Each fact is a wire-level guarantee proven by an actual round-trip; the test
pins all of them. (The server also logs an expected WARN when the plain server
starts without TLS and an expected ERROR on the deliberately-rejected auth and
invalid query — those are server-side observability, not failures.)

## Evidence it collects

For the Cypher/Bolt subject (per `docs/examples-standard.md`): the correctness
evidence is the six wire-level guarantees asserted as facts, and the scenario
wall-clock as telemetry. Scaling `-persons` makes the write and count latencies
observable.

## Key APIs

- `bolt/server.NewServer` / `server.Options` — start the embedded Bolt v5 server; `Options.TLSConfig` selects encrypted transport.
- `bolt/server.BasicAuthHandler` / `server.ConstantTimeValidate` — credential authentication without a timing side-channel.
- `bolt/server.DefaultTLSConfig` — a hardened TLS 1.2+ baseline for the server transport.
- `neo4j.NewDriverWithContext` (`bolt://` and `bolt+ssc://`) / `neo4j.BasicAuth` — the client driver and its auth token.
- `SessionWithContext.ExecuteWrite` / `BeginTransaction` / `Run` — managed write transactions, explicit transactions with `Commit`/`Rollback`, and auto-commit statements.

## Further reading

- [`bolt/server`](../../bolt/server) — embedded Bolt server package documentation
- [Example 23 — Bolt server](../23_bolt_server) — read throughput and the no-leak guarantee over many sessions
- [Example 25 — Software-house API](../25_software_house_api) — a persistent, kill-9-safe REST API over the same engine
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
