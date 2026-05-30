# Migration guide: GoGraph 1.x → 2.x

This document collects the API and on-disk format changes between
the 1.x stable line and the 2.x release line, with the upgrade
steps required to land them.

**TL;DR.** Most 1.x code compiles unchanged against 2.x because the
breaking-change surface is intentionally narrow: a few error-return
additions on previously infallible methods, the on-disk manifest
version bump, and a small handful of newly required `Options`
fields. Consumers who only used the in-memory graph and search
algorithms can move from 1.x to 2.x by re-running `go build`.

> **Breaking change — the v1 WAL format is removed in 2.0.0.** The
> legacy untagged WAL record format, the `store/txn.NewStore`
> constructor that produced it, and the `recovery.OpenString`,
> `recovery.OpenWithCodec`, and `recovery.OpenWithOptions` read
> wrappers (and their `*Ctx` variants) **no longer exist**. 2.0.0
> contains **no in-place v1 WAL reader**: a v1 WAL corpus is rejected
> on open with `recovery.ErrUnsupportedRecordVersion`. A v1 corpus
> must be migrated/replayed to a typed v2 store with a **1.x build
> before upgrading** — see [Migrate a v1 WAL corpus](#0-migrate-a-v1-wal-corpus-before-upgrading)
> below. (On-disk **snapshot** directories are unaffected: a v1
> snapshot still loads in 2.0.0; only the WAL record format changed.)

## Compatibility matrix

| Concern                 | 1.x behaviour                            | 2.x behaviour                                                                   |
|-------------------------|------------------------------------------|---------------------------------------------------------------------------------|
| `graph.Graph.AddNode`   | `func (g) AddNode(N)`                    | `func (g) AddNode(N) error` — returns `adjlist.ErrShardFull` on bounded growth. |
| `graph.Graph.AddEdge`   | `func (g) AddEdge(N, N, W)`              | `func (g) AddEdge(N, N, W) error` — same bound surface as `AddNode`.            |
| `adjlist.Config`        | `Directed`, `Multigraph`                 | Same, plus `MaxShardCapacity` (zero = unbounded; positive = hard cap).          |
| `cypher.Engine`         | (not present in 1.x)                     | New in 2.x — see [docs/cypher.md](cypher.md).                                   |
| `cypher.Result.Close`   | (n/a)                                    | Mandatory; finalizer-backed safety net increments `cypher.result.leaked`.       |
| `bolt/server`           | (not present in 1.x)                     | New in 2.x — see [docs/bolt.md](bolt.md). Per-connection cursor cap = 1.        |
| `store/txn.NewStore`    | Single-type (`string` keys, `int64` w.)  | **Removed.** Use `NewStoreWithCodec` (typed N) or `NewStoreWithOptions` (typed N + W). |
| `recovery.OpenString` / `OpenWithCodec` / `OpenWithOptions` | Recovery read wrappers | **Removed.** Use `recovery.Open` / `OpenCtx` with explicit `recovery.Options` codecs. |
| Snapshot manifest       | v1 (CSR-only)                            | v2 (CSR + labels + properties + indexes). v1 snapshot manifests still load.      |
| WAL record format       | v1 untagged (`fmt.Sprintf` endpoints)    | **v1 removed.** v3 (`OpRecordV3 = 0xFD`, transaction-grouped) is produced; v2 (`0xFE`) still read; v1 rejected on read. |
| Toolchain               | `go 1.21`                                | `go 1.26` + `toolchain go1.26.3` pin (see [release.md](release.md)).            |

## Step-by-step upgrade

### 0. Migrate a v1 WAL corpus *before* upgrading

This step is mandatory **only** if you have on-disk WALs written by a 1.x
`store/txn.NewStore` (the single-type constructor). 2.0.0 removed the v1
WAL record format and every API that read or wrote it, and it ships **no
in-place v1 reader**: opening a v1 WAL under 2.0.0 fails with
`recovery.ErrUnsupportedRecordVersion`.

Convert the corpus while still on a 1.x build:

1. With a 1.x binary, replay the v1 WAL into an in-memory graph via the
   (1.x-only) string-recovery wrapper.
2. Construct a new store via `txn.NewStoreWithCodec` (or
   `NewStoreWithOptions` for durable weights) against a **fresh** WAL.
3. Re-commit the recovered ops through the new store. The new WAL now
   carries only typed, tagged frames.

The resulting WAL opens cleanly under 2.0.0. A snapshot + WAL-truncation
checkpoint performs the same conversion, so a store that has taken at
least one self-sufficient checkpoint under 1.x and truncated its v1 WAL
prefix needs no manual step. On-disk **snapshot** directories are not
affected by this step — a v1 snapshot loads unchanged in 2.0.0.

### 1. Bump the Go module

```diff
- go 1.21
+ go 1.26
+ toolchain go1.26.3
```

Run `go mod tidy && go mod verify` after the edit. The dependency
policy in [CONTRIBUTING.md](../CONTRIBUTING.md#dependency-policy)
governs subsequent dependency upgrades.

### 2. Handle the new error returns on `AddNode` / `AddEdge`

In 1.x:

```go
g.AddNode("alice")
g.AddEdge("alice", "bob", 1.0)
```

In 2.x:

```go
if err := g.AddNode("alice"); err != nil {
    return err
}
if err := g.AddEdge("alice", "bob", 1.0); err != nil {
    return err
}
```

Loaders that build large graphs in tight loops can preserve the
hot path by checking the error only when `adjlist.Config.MaxShardCapacity`
is set; with the default zero cap `AddNode` and `AddEdge` never
return an error.

### 3. Adopt the Cypher / Bolt surface (optional)

These are entirely new in 2.x; pre-2.x callers are unaffected.
When adopting:

- [docs/cypher.md](cypher.md) — language reference.
- [docs/bolt.md](bolt.md) — Bolt v5 server, TLS, auth.
- `cypher.Result.Close()` is mandatory — see the
  [Result lifecycle contract](cypher.md) in the API reference and
  the safety-net finalizer (`cypher.result.leaked` metric).

### 4. Migrate the persistence layer (if used)

Existing v1 **snapshot** directories load unchanged in 2.x because the
reader accepts every manifest version (writers emit v2/v3). The **WAL**
is different: see [Step 0](#0-migrate-a-v1-wal-corpus-before-upgrading)
— a v1 WAL must be converted on a 1.x build before upgrading.

`store/txn.NewStore` was **removed** (it is no longer a deprecated alias;
referencing it is a compile error). Code that pinned it must adopt
`NewStoreWithCodec` (typed node key, no durable weights) or
`NewStoreWithOptions` (typed node key + typed weight). Both take the
graph and an explicit `*wal.Writer`; `NewStoreWithOptions` takes its two
codecs through `txn.Options`:

```diff
- s, err := txn.NewStore[string, int64](walPath)
+ w, err := wal.Open(walPath)
+ // ... handle err ...
+ s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
+     Codec:       txn.NewStringCodec(),
+     WeightCodec: txn.NewInt64WeightCodec(),
+ })
```

On the recovery side, the matching wrappers `recovery.OpenString`,
`recovery.OpenWithCodec`, and `recovery.OpenWithOptions` (and their
`*Ctx` variants) were also removed. Use `recovery.Open` / `OpenCtx`,
passing the same two codecs via `recovery.Options`:

```diff
- res, err := recovery.OpenString(dir)
+ res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
+     Codec:       txn.NewStringCodec(),
+     WeightCodec: txn.NewInt64WeightCodec(),
+ })
```

### 5. Adopt the new bounded-resource defaults

2.x adds explicit caps to several previously unbounded surfaces:

- `bolt/server.Options.MaxConnections` (default 1 024).
- `bolt/server.Options.MaxMessageBytes` (default 16 MiB; see
  `proto.DefaultMaxMessageBytes`).
- `bolt/server.Options.MaxInFlightPerConnection` (default 1).
- `cypher.EngineOptions.PlanCacheCapacity` (default 1 024).
- `adjlist.Config.MaxShardCapacity` (zero = unbounded; opt-in).

Consumers who legitimately need to raise these caps must do so
explicitly via the `Options` struct passed to the relevant
constructor.

## Cross-references

- [CHANGELOG.md](../CHANGELOG.md) — per-release entries with full
  rationale for each breaking change.
- [docs/semver.md](semver.md) — versioning policy and v2.0.0
  stable gate.
- [docs/release.md](release.md) — release workflow, dependency
  policy, toolchain pin, SBOM.

---

*Last reviewed: 2026-05-30 against commit `9c31f06`.*
