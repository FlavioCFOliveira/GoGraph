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

echo "Verifying …"
java -jar "${ANTLR_JAR}" 2>&1 | head -1
echo "Done."
