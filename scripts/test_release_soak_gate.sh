#!/usr/bin/env bash
# test_release_soak_gate.sh — self-contained gate test for
# release_soak_gate.sh.
#
# Injects a stub `gh` so no network or authentication is needed and
# verifies three properties:
#   1. No green soak run for the release commit -> gate FAILS.
#   2. A green soak run present                 -> gate PASSES.
#   3. No gh CLI available                       -> gate FAILS (fail-closed).
#
# Run directly from the repo root:
#   bash scripts/test_release_soak_gate.sh

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$REPO/scripts/release_soak_gate.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Stub gh: prints the run count taken from $STUB_COUNT, mimicking
# `gh run list ... --jq length`.
cat > "$tmp/gh" <<'EOF'
#!/usr/bin/env bash
echo "${STUB_COUNT:-0}"
EOF
chmod +x "$tmp/gh"

FAKE_SHA="0123456789abcdef0123456789abcdef01234567"

# 1) No green soak run -> FAIL.
if STUB_COUNT=0 GH_BIN="$tmp/gh" RELEASE_SHA="$FAKE_SHA" bash "$SCRIPT" >"$tmp/o1" 2>&1; then
  echo "FAIL [1/3]: gate passed despite no green soak run"; cat "$tmp/o1"; exit 1
fi
grep -q "no green" "$tmp/o1" || { echo "FAIL [1/3]: missing 'no green' message"; cat "$tmp/o1"; exit 1; }
echo "PASS [1/3]: missing soak run rejected"

# 2) A green soak run -> PASS.
if STUB_COUNT=1 GH_BIN="$tmp/gh" RELEASE_SHA="$FAKE_SHA" bash "$SCRIPT" >"$tmp/o2" 2>&1; then
  echo "PASS [2/3]: green soak run accepted"
else
  echo "FAIL [2/3]: gate rejected a green soak run"; cat "$tmp/o2"; exit 1
fi

# 3) No gh CLI -> FAIL closed.
if GH_BIN="$tmp/definitely-not-a-real-gh" RELEASE_SHA="$FAKE_SHA" bash "$SCRIPT" >"$tmp/o3" 2>&1; then
  echo "FAIL [3/3]: gate passed with no gh CLI present"; cat "$tmp/o3"; exit 1
fi
grep -q "CLI not found" "$tmp/o3" || { echo "FAIL [3/3]: missing 'CLI not found' message"; cat "$tmp/o3"; exit 1; }
echo "PASS [3/3]: missing gh CLI rejected (fail-closed)"

echo "ALL CASES PASSED"
