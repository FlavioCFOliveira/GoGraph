#!/usr/bin/env bash
# sync-tck.sh — sync TCK feature files from a pinned openCypher tag.
#
# Usage:
#   ./tools/sync-tck.sh [TAG]
#
# TAG defaults to 1.0.0-M23. Pass any valid openCypher tag or branch name.
# After syncing, update cypher/tck/TCK_VERSION to reflect the new source.
set -euo pipefail

TAG="${1:-1.0.0-M23}"
TMPDIR="$(mktemp -d)"
DEST="cypher/tck/features"

cleanup() {
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

echo "Cloning openCypher at tag '${TAG}'…"
git clone --depth=1 --branch="${TAG}" https://github.com/opencypher/openCypher.git "${TMPDIR}/oc"

echo "Copying feature files to ${DEST}/…"
cp -r "${TMPDIR}/oc/tck/features/" "${DEST}/"

echo "Synced TCK from tag ${TAG}"
echo "Remember to update cypher/tck/TCK_VERSION accordingly."
