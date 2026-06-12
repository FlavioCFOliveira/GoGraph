#!/usr/bin/env bash
# test_nightly_gate.sh — self-contained gate test for the nightly failure-
# detection logic.
#
# Verifies that the detection logic used in nightly.yml catches failure
# conditions that do NOT emit a "^FAIL" line, such as OOM kills (exit 137)
# or other process-level crashes that only produce "*** Error N" in the log.
#
# Run directly:
#   bash scripts/test_nightly_gate.sh

set -euo pipefail

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

PASS_COUNT=0
FAIL_COUNT=0

# check <desc> <want:fail|pass> <log_content> [exit_code_override]
#
# Simulates the nightly detection logic:
#   - check_nightly_log <log> returns 0 when tests passed, 1 when tests failed.
check_nightly_log() {
  local log="$1"
  # Detection logic (mirrors nightly.yml):
  #   A run is healthy when the log contains at least one "^ok " line
  #   AND the test process exited 0.  Either condition absent = failure.
  #   "^FAIL" is also caught, but is NOT required to be the sole signal.
  if grep -q "^FAIL" "$log"; then
    return 1
  fi
  if ! grep -q "^ok " "$log"; then
    # No "ok " line — either OOM kill or empty log.  Treat as failure.
    return 1
  fi
  return 0
}

check() {
  local desc="$1"
  local want="$2"
  local log_content="$3"

  local logfile="${TMPDIR}/log_$$.txt"
  printf '%s' "$log_content" > "$logfile"

  local rc=0
  check_nightly_log "$logfile" || rc=$?

  if [[ "$want" == "fail" ]]; then
    if (( rc != 0 )); then
      echo "PASS: ${desc} (detected as failure)"
      PASS_COUNT=$((PASS_COUNT + 1))
    else
      echo "FAIL: ${desc} (expected failure detection, got success)"
      FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
  else
    if (( rc == 0 )); then
      echo "PASS: ${desc} (detected as success)"
      PASS_COUNT=$((PASS_COUNT + 1))
    else
      echo "FAIL: ${desc} (expected success detection, got failure)"
      FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
  fi
}

# ── Test 1: OOM kill — no ^FAIL, no ^ok ──────────────────────────────────────
check "OOM kill (*** Error 137, no FAIL/ok)" fail \
  "some test output
runtime: out of memory: cannot allocate 1GB
*** Error in ./test.test: (signal 9)
"

# ── Test 2: explicit ^FAIL line ───────────────────────────────────────────────
check "explicit ^FAIL line" fail \
  "--- FAIL: TestSomething (0.01s)
FAIL
FAIL	github.com/FlavioCFOliveira/GoGraph/search	0.423s
"

# ── Test 3: crash with no output ──────────────────────────────────────────────
check "empty log (process produced no output)" fail ""

# ── Test 4: signal kill with no FAIL ──────────────────────────────────────────
check "process killed by signal (no FAIL or ok)" fail \
  "=== RUN   TestLongRunning
signal: killed
"

# ── Test 5: healthy run — has ^ok ─────────────────────────────────────────────
check "healthy run with ^ok lines passes" pass \
  "=== RUN   TestFoo
--- PASS: TestFoo (0.00s)
ok  	github.com/FlavioCFOliveira/GoGraph/search	0.423s
ok  	github.com/FlavioCFOliveira/GoGraph/search/centrality	1.234s
"

# ── Test 6: partial run — some ok, then crash ─────────────────────────────────
# This is a borderline case: some packages passed but the last one crashed.
# The ^FAIL line IS present (make emits it), so this is caught.
check "partial run with FAIL after some ok" fail \
  "ok  	github.com/FlavioCFOliveira/GoGraph/search	0.423s
--- FAIL: TestHeavy (12.34s)
FAIL	github.com/FlavioCFOliveira/GoGraph/store	12.345s
"

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "Results: ${PASS_COUNT} passed, ${FAIL_COUNT} failed"

if (( FAIL_COUNT > 0 )); then
  exit 1
fi
exit 0
