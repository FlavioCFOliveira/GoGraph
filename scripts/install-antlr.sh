#!/usr/bin/env bash
# install-antlr.sh — Download the ANTLR4 complete jar to ~/.antlr/.
#
# Usage:
#   bash scripts/install-antlr.sh [VERSION]
#
# VERSION defaults to 4.13.1 (the version pinned in go.mod).
# After running this script, regenerate the parser with:
#   make generate-cypher-parser
#
# Requirements: curl, java (JRE/JDK 11+).

set -euo pipefail

ANTLR_VERSION="${1:-4.13.1}"
ANTLR_JAR="${HOME}/.antlr/antlr-${ANTLR_VERSION}-complete.jar"
ANTLR_URL="https://www.antlr.org/download/antlr-${ANTLR_VERSION}-complete.jar"

# Pinned SHA-256 for the supported ANTLR versions. Verified against
# the upstream publish at https://www.antlr.org/download.html. Add a
# new case when bumping ANTLR_VERSION; the script refuses to proceed
# without one so a tampered upstream cannot silently replace the jar.
# A 'case' is used instead of 'declare -A' for portability with the
# bash 3.2 shipped on macOS.
case "${ANTLR_VERSION}" in
  "4.13.1") EXPECTED_SHA256="bc13a9c57a8dd7d5196888211e5ede657cb64a3ce968608697e4f668251a8487" ;;
  *)
    echo "ERROR: no pinned SHA-256 for ANTLR ${ANTLR_VERSION}." >&2
    echo "Add a case for it to this script before installing." >&2
    exit 1
    ;;
esac

# Verify java is present.
if ! command -v java >/dev/null 2>&1; then
  echo "ERROR: java not found on PATH." >&2
  echo "Install a JDK (e.g. 'brew install openjdk@21') and ensure it is on PATH." >&2
  exit 1
fi

mkdir -p "${HOME}/.antlr"

if [[ -f "${ANTLR_JAR}" ]]; then
  echo "ANTLR ${ANTLR_VERSION} jar already present at ${ANTLR_JAR}"
else
  echo "Downloading ANTLR ${ANTLR_VERSION} from ${ANTLR_URL} …"
  curl -sSL -o "${ANTLR_JAR}" "${ANTLR_URL}"
  echo "Saved to ${ANTLR_JAR}"
fi

echo "Verifying checksum …"
if command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${ANTLR_JAR}" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${ANTLR_JAR}" | awk '{print $1}')"
else
  echo "ERROR: neither shasum nor sha256sum found on PATH." >&2
  exit 1
fi
if [[ "${actual}" != "${EXPECTED_SHA256}" ]]; then
  echo "ERROR: ANTLR jar checksum mismatch." >&2
  echo "  expected: ${EXPECTED_SHA256}" >&2
  echo "  actual:   ${actual}" >&2
  echo "  file:     ${ANTLR_JAR}" >&2
  echo "Remove the file and rerun, or update the pinned SHA-256 if you trust the new upstream." >&2
  exit 1
fi
echo "Checksum OK: ${actual}"

echo "Verifying jar runs …"
java -jar "${ANTLR_JAR}" 2>&1 | head -1
echo "Done."
