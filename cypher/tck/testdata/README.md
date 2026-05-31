# TCK test data

## `zoneinfo-slim.zip` — pinned IANA time-zone database

A handful of openCypher TCK temporal scenarios resolve the UTC offset of a
*named* time zone at a pre-standard-time instant. The canonical example, from
[`Temporal2.feature`](../features/expressions/temporal/Temporal2.feature):

```cypher
RETURN datetime('1818-07-21T21:40:32.142[Europe/Stockholm]') AS result
-- expected: '1818-07-21T21:40:32.142+00:53:28[Europe/Stockholm]'
```

The expected offset `+00:53:28` is fixed verbatim in the upstream openCypher TCK
feature files. It is the value produced by a **slim**-format compilation of the
IANA database (`zic -b slim`). A **fat**-format compilation (`zic -b fat` — the
default on most Linux distributions and in Go's bundled
`$GOROOT/lib/time/zoneinfo.zip`) keeps the explicit pre-1879 Local Mean Time
transition for Stockholm and instead yields `+01:12:12` for the same instant.

Because the two builds disagree, leaving the test to read the host's system
database makes openCypher TCK conformance depend on which tzdata build the
machine happens to ship: green on macOS (Apple ships a slim build), red on the
Ubuntu CI runner (fat). That violates the project's *100 % openCypher TCK
compliant* invariant, which must hold on every host.

### How it is used

`zoneinfo-slim.zip` is a vendored, slim-format IANA database. The CI workflows
and the `Makefile` test targets set the `ZONEINFO` environment variable to its
absolute path. Go's `time` package consults `ZONEINFO` **before** the system
database, so the test process reads deterministic time-zone data regardless of
the host OS.

The archive entries are **stored uncompressed**. Go's minimal time-package zip
reader rejects compressed entries (`"unsupported compression"`) and then
*silently falls back* to the next `ZONEINFO` source — the host system database
— which would defeat the purpose of pinning. The regeneration script builds
with `zip -0`, and `TestZoneinfoFixtureIsUsableAndSlim` (in `cypher/tck`) fails
the build if any entry is ever compressed.

- Workflows: `.github/workflows/ci.yml`, `tck.yml`, `nightly.yml` set
  `ZONEINFO: ${{ github.workspace }}/cypher/tck/testdata/zoneinfo-slim.zip`.
- Local: the `Makefile` exports
  `ZONEINFO := $(CURDIR)/cypher/tck/testdata/zoneinfo-slim.zip`, so `make test-*`
  is deterministic on every platform. A bare `go test` that bypasses `make`
  inherits the host database unless you export `ZONEINFO` yourself.

### Provenance and regeneration

| Property | Value |
|---|---|
| Source | IANA `tzdata2026b` (<https://data.iana.org/time-zones/releases/>) |
| Build mode | `zic -b slim` |
| Region files | `africa antarctica asia australasia europe northamerica southamerica etcetera backward` |
| Canary | `Europe/Stockholm` @ `1818-07-21` ⇒ `+00:53:28` |

Regenerate with the self-validating script (it fails if the canary offset
drifts):

```bash
scripts/gen_tck_tzdata.sh
```

Regeneration may produce a byte-different but semantically identical archive.
Bump `TZ_VERSION` in the script only if the upstream openCypher TCK temporal
expectations change, and re-run the suite afterwards:

```bash
ZONEINFO="$(pwd)/cypher/tck/testdata/zoneinfo-slim.zip" \
  go test ./cypher/tck/... -run TestTCKExecution -count=1
```
