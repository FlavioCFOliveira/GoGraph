# Security Policy

This document is the canonical entry point for reporting vulnerabilities
in GoGraph. It complements the runtime contracts documented in
[CLAUDE.md](CLAUDE.md) and the validation pipeline documented in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Supported versions

Security fixes are issued for the following release lines:

| Version line | Status                | Receives security fixes |
|--------------|-----------------------|-------------------------|
| v3.0.x       | Stable (current)      | Yes                     |
| v2.0.x       | Previous major        | No — upgrade to v3.0.x  |
| < v2.0       | Pre-release / end-of-life | No — upgrade to v3.0.x |

A single patch release covers each backported fix; we do not publish
out-of-band security branches. The release process is documented in
[docs/release.md](docs/release.md).

## Reporting a vulnerability

**Do not** open a public GitHub Issue for a suspected vulnerability.
Use either of the private channels below:

1. **Preferred — GitHub Security Advisories.**
   <https://github.com/xumiga/gograph/security/advisories/new>
   The form lets you describe the issue, attach a proof of concept,
   and propose a fix. Maintainers are notified privately and the
   advisory is published only after a fix has been released.

2. **Email.** Send a signed message to `security@xumiga.example`
   (substitute your organisation's published security mailbox when
   forking). PGP-encrypted email is welcome; the public key for the
   maintainer team is published at
   `https://xumiga.example/.well-known/security-pgp-key.txt`.

Please include in your report:

- A clear description of the vulnerability and the security impact.
- The affected version (commit SHA or release tag).
- A minimal reproduction (Go test, shell script, packet capture, or
  Cypher query).
- Any known mitigations or workarounds.
- Whether you wish to be credited in the published advisory.

## Response timeline

The maintainer team commits to the following service-level objectives,
measured from the time the private report is received:

| Stage                                 | Target turnaround |
|---------------------------------------|-------------------|
| Acknowledgement of receipt            | 48 hours          |
| Initial triage and severity decision  | 5 business days   |
| Fix landed on `main` (under embargo)  | 30 days           |
| Coordinated disclosure (advisory + release) | 90 days     |

Critical vulnerabilities (CVSS 9.0+, exploitable in the default
configuration, no mitigation available) are expedited and may
ship inside 7 days.

## Embargo and disclosure

By default we ask reporters to honour a 90-day embargo from the date
of initial acknowledgement. The embargo may be shortened by mutual
agreement (for example, when the vulnerability is already being
exploited in the wild) or extended once if a fix proves more invasive
than the initial triage indicated. Reporters who disclose during the
embargo without coordination forfeit credit in the published advisory.

## Scope

In scope:

- Memory safety, race conditions, panics, deadlocks, and any issue
  surfaced by `go test -race`, `goleak`, `govulncheck`, or the soak
  harness against the default configuration.
- Cryptographic weaknesses in transport (Bolt over TLS), persistence
  (WAL, snapshot, CSR file), and any future authentication surface.
- Denial-of-service vectors against the Bolt server, including
  unbounded allocations, CPU exhaustion, and connection exhaustion.
- Supply-chain risks against the published release artefacts
  (goreleaser pipeline, SBOM, checksums).

Out of scope:

- Issues that require an attacker who already has filesystem write
  access to the database directory or process memory.
- Issues in third-party tools that consume GoGraph as a library
  unless they are caused by a GoGraph API contract violation.
- Performance regressions that do not cross a documented SLO.

## Credit

We acknowledge reporters in the published advisory, the CHANGELOG.md
entry for the release that ships the fix, and (when consented) on the
README's contributors list. Anonymous reports are accepted; we will
honour requests to omit a name from the public record.
