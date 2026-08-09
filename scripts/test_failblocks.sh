#!/usr/bin/env bash
# test_failblocks.sh - self-contained gate test for failblocks.awk.
#
# The filter exists because the line-pattern grep it replaces silently
# discarded the BODY of a failing test's output (rmp #2347). This test pins
# that it no longer can, and it does so the only way that is evidence: by
# running the OLD filter alongside the new one on the same input and asserting
# that the old one loses the body while the new one keeps it. A test that only
# exercised the new filter could not tell you the defect was ever real.
#
# Run directly:
#   bash scripts/test_failblocks.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AWK_FILTER="${SCRIPT_DIR}/failblocks.awk"

if [[ ! -f "$AWK_FILTER" ]]; then
  echo "FAIL: failblocks.awk not found at ${AWK_FILTER}" >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

fails=0
check() { # check <description> <condition-result>
  if [[ "$2" == "0" ]]; then
    echo "  PASS: $1"
  else
    echo "  FAIL: $1" >&2
    fails=$((fails + 1))
  fi
}

contains() { grep -qF -- "$2" "$1" && echo 0 || echo 1; }
lacks() { grep -qF -- "$2" "$1" && echo 1 || echo 0; }

# The filter the fix replaced, reproduced verbatim so the regression it guards
# against stays demonstrable rather than merely asserted.
old_filter() {
  grep -E '(^--- FAIL|^FAIL[[:space:]]|panic:|fatal error:|_test\.go:[0-9]+:|signal:|DATA RACE)' "$1" || true
}

# ── fixture: the three failure shapes go test actually emits ────────────────
LOG="${TMPDIR}/testlog"
cat > "$LOG" <<'EOF'
ok  	github.com/FlavioCFOliveira/GoGraph/graph/lpg	31.204s
?   	github.com/FlavioCFOliveira/GoGraph/cmd/x	[no test files]
--- FAIL: TestST3_CheckpointTeardown_Scenario (12.34s)
    durable_scenarios_test.go:193: ST3 seed 0xc0ffee violation:
        SIMULATION FAILED
          Seed:        12648430
          Violations (1):
            - [ACID_DURABILITY] tick=0 op="commit": acked commit missing after recovery
        Reproduce with: go run ./cmd/sim 12648430
FAIL
FAIL	github.com/FlavioCFOliveira/GoGraph/internal/sim	123.4s
==================
WARNING: DATA RACE
Write at 0x00c000123456 by goroutine 12:
  github.com/FlavioCFOliveira/GoGraph/graph/lpg.(*Graph).setNodeLabelInfo()
      /src/graph/lpg/lpg.go:2698 +0x1a4
==================
--- FAIL: TestRace (0.10s)
    testing.go:1490: race detected during execution of test
FAIL	github.com/FlavioCFOliveira/GoGraph/graph/lpg	9.9s
panic: send on closed channel

goroutine 42 [running]:
main.worker()
	/src/x.go:10 +0x20
exit status 2
FAIL	github.com/FlavioCFOliveira/GoGraph/store/wal	0.5s
EOF

OLD="${TMPDIR}/old.out"
NEW="${TMPDIR}/new.out"
old_filter "$LOG" > "$OLD"
awk -f "$AWK_FILTER" "$LOG" > "$NEW"

echo "== the defect is real: the OLD filter loses the report body =="
check "old filter keeps the --- FAIL header" \
  "$(contains "$OLD" '--- FAIL: TestST3_CheckpointTeardown_Scenario')"
check "old filter keeps the test:line prefix" \
  "$(contains "$OLD" 'durable_scenarios_test.go:193')"
check "old filter LOSES the violated invariant" \
  "$(lacks "$OLD" 'ACID_DURABILITY')"
check "old filter LOSES the reproduction seed" \
  "$(lacks "$OLD" 'Reproduce with')"
check "old filter LOSES the race frames" \
  "$(lacks "$OLD" 'setNodeLabelInfo')"
check "old filter LOSES the panic goroutine dump" \
  "$(lacks "$OLD" 'goroutine 42')"

echo "== the fix: the NEW filter keeps every failure block whole =="
check "new filter keeps the --- FAIL header" \
  "$(contains "$NEW" '--- FAIL: TestST3_CheckpointTeardown_Scenario')"
check "new filter keeps the violated invariant" \
  "$(contains "$NEW" 'ACID_DURABILITY')"
check "new filter keeps the reproduction seed" \
  "$(contains "$NEW" 'Reproduce with: go run ./cmd/sim 12648430')"
check "new filter keeps the race frames" \
  "$(contains "$NEW" 'setNodeLabelInfo')"
check "new filter keeps the panic goroutine dump" \
  "$(contains "$NEW" 'goroutine 42')"
check "new filter keeps the package verdicts" \
  "$(contains "$NEW" 'FAIL	github.com/FlavioCFOliveira/GoGraph/internal/sim')"
check "new filter drops passing packages" \
  "$(lacks "$NEW" 'ok  	github.com/FlavioCFOliveira/GoGraph/graph/lpg')"

echo "== the cap announces itself instead of truncating silently =="
CAPPED="${TMPDIR}/capped.out"
awk -v max=4 -f "$AWK_FILTER" "$LOG" > "$CAPPED"
check "capped output announces the suppression" \
  "$(contains "$CAPPED" 'further line(s) suppressed')"
check "capped output keeps the FIRST failure, not the last" \
  "$(contains "$CAPPED" 'durable_scenarios_test.go:193')"

echo "== a clean log yields no failure output at all =="
CLEAN="${TMPDIR}/clean"
printf 'ok  \tgithub.com/x/y\t1.0s\n?   \tgithub.com/x/z\t[no test files]\n' > "$CLEAN"
awk -f "$AWK_FILTER" "$CLEAN" > "${TMPDIR}/clean.out"
check "clean log produces empty output" \
  "$([[ ! -s "${TMPDIR}/clean.out" ]] && echo 0 || echo 1)"

if [[ "$fails" -ne 0 ]]; then
  echo "FAIL: ${fails} assertion(s) failed" >&2
  exit 1
fi
echo "PASS: failblocks.awk preserves every failure block the old filter discarded"
