#!/usr/bin/env bash
#
# gen_tck_tzdata.sh — regenerate the pinned IANA time-zone database used by the
# openCypher TCK execution suite.
#
# Why this exists
# ---------------
# A handful of openCypher TCK temporal scenarios resolve the UTC offset of a
# *named* time zone at a pre-standard-time instant — for example
# `datetime('1818-07-21T21:40:32.142[Europe/Stockholm]')`. The expected result
# baked into the upstream TCK feature files
# (cypher/tck/features/expressions/temporal/Temporal2.feature) is
# `+00:53:28`, which is the value produced by a **slim**-format compilation of
# the IANA database (`zic -b slim`).
#
# A **fat**-format compilation (`zic -b fat`, the default on most Linux distros
# and in Go's bundled `lib/time/zoneinfo.zip`) keeps the explicit pre-1879 Local
# Mean Time transition and instead yields `+01:12:12` for the same instant. That
# mismatch makes TCK conformance depend on whichever tzdata build the host
# happens to ship — green on macOS, red on the Ubuntu CI runner.
#
# To make conformance host-independent we vendor a slim database and point the
# test process at it via the `ZONEINFO` environment variable, which Go's `time`
# package consults *before* the system database. This script regenerates that
# vendored zip from a pinned IANA release so the artefact is reproducible rather
# than an opaque binary blob.
#
# Requirements: curl, zic (IANA tzcode — present on macOS and most Linux), zip,
# and a Go toolchain (used only for the self-check).
#
# Usage:
#   scripts/gen_tck_tzdata.sh
#
set -euo pipefail

# Pinned IANA release. Bump this only when the upstream openCypher TCK temporal
# expectations change; re-run the self-check below after any bump.
TZ_VERSION="2026b"

# Expected offset for the canary scenario, used as a self-check.
CANARY_EXPECTED="+00:53:28"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_zip="${repo_root}/cypher/tck/testdata/zoneinfo-slim.zip"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

echo "==> Downloading IANA tzdata${TZ_VERSION}"
curl -fsSL -o "${workdir}/tzdata.tar.gz" \
  "https://data.iana.org/time-zones/releases/tzdata${TZ_VERSION}.tar.gz"
mkdir -p "${workdir}/src" "${workdir}/out"
tar -xzf "${workdir}/tzdata.tar.gz" -C "${workdir}/src"

echo "==> Compiling with zic -b slim"
# Primary region files plus 'backward' (compatibility links). 'backzone'
# (opt-in pre-1970 data) is deliberately excluded to match the default build.
zone_files=(africa antarctica asia australasia europe northamerica \
            southamerica etcetera backward)
( cd "${workdir}/src" && zic -b slim -d "${workdir}/out" "${zone_files[@]}" )

echo "==> Packaging ${out_zip}"
mkdir -p "$(dirname "${out_zip}")"
rm -f "${out_zip}"
# -0 stores entries UNCOMPRESSED: Go's minimal time-package zip reader rejects
# any compressed entry ("unsupported compression") and then silently falls back
# to the host system database — which would reintroduce the very host-dependence
# this file exists to remove. -X drops extra attributes, -D omits directory
# entries (Go wants bare "Area/Location" names). Entries are added in sorted
# order for a stable archive layout.
( cd "${workdir}/out" \
  && find . -type f | sed 's|^\./||' | LC_ALL=C sort \
     | zip -0 -X -D -q "${out_zip}" -@ )

echo "==> Self-check (fallback-proof): Stockholm @ 1818-07-21 must be ${CANARY_EXPECTED}"
# This check reads the zone bytes DIRECTLY from the archive and parses them with
# time.LoadLocationFromTZData, so it cannot be masked by Go falling back to the
# host's own (possibly slim) system tzdata — the trap that hid the compression
# bug on macOS. It also asserts every entry is stored (method 0).
cat > "${workdir}/check.go" <<'GO'
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	path := os.Args[1]
	r, err := zip.OpenReader(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open zip:", err)
		os.Exit(2)
	}
	defer r.Close()

	var stockholm []byte
	for _, f := range r.File {
		if f.Method != zip.Store {
			fmt.Fprintf(os.Stderr, "entry %q is compressed (method=%d); Go's tz reader needs stored\n", f.Name, f.Method)
			os.Exit(3)
		}
		if f.Name == "Europe/Stockholm" {
			rc, err := f.Open()
			if err != nil {
				fmt.Fprintln(os.Stderr, "open entry:", err)
				os.Exit(2)
			}
			stockholm, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if stockholm == nil {
		fmt.Fprintln(os.Stderr, "Europe/Stockholm not found in archive")
		os.Exit(2)
	}
	loc, err := time.LoadLocationFromTZData("Europe/Stockholm", stockholm)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse tzdata:", err)
		os.Exit(2)
	}
	t := time.Date(1818, 7, 21, 21, 40, 32, 142000000, loc)
	fmt.Print(t.Format("-07:00:00"))
}
GO
got="$(go run "${workdir}/check.go" "${out_zip}")"
if [[ "${got}" != "${CANARY_EXPECTED}" ]]; then
  echo "FAIL: canary offset is ${got}, expected ${CANARY_EXPECTED}" >&2
  echo "The pinned IANA release or zic build mode no longer matches the" >&2
  echo "upstream openCypher TCK expectation. Investigate before committing." >&2
  exit 1
fi

echo "==> OK: ${out_zip} ($(wc -c < "${out_zip}" | tr -d ' ') bytes), canary=${got}"
