#!/usr/bin/env bash
#
# test_vulncheck_gate.sh — proves scripts/vulncheck_gate.sh can FAIL (rmp #2722).
#
# The gate's whole reason to exist is that a vulnerability scan can exit 0
# having analysed nothing. An assertion that has never been seen to fail is
# worth nothing, so this harness feeds the gate deliberately broken scanners
# through GOVULNCHECK and requires it to reject each one.
#
# It also runs a case that must PASS. A harness that can only produce failures
# proves nothing either: the PASS case shows each failure above is caused by
# the injected defect and not by the injection mechanism.
#
# Run:  bash scripts/test_vulncheck_gate.sh
# Optional: VULNCHECK_STALE_BIN=/path/to/an/old-toolchain/govulncheck adds the
# real-world reproduction (a govulncheck built for a different Go minor). When
# unset, the harness looks for one among the binaries installed on this host.

set -euo pipefail

cd "$(dirname "$0")/.."
GO=${GO:-go}
GATE=scripts/vulncheck_gate.sh

work=$(mktemp -d "${TMPDIR:-/tmp}/test-vulncheck-gate.XXXXXX")
trap 'rm -rf "$work"' EXIT

pass_count=0; fail_count=0
ok()  { echo "  ok   $*"; pass_count=$((pass_count + 1)); }
bad() { echo "  FAIL $*"; fail_count=$((fail_count + 1)); }

CONFIG='{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.7.0-stub","db":"https://vuln.go.dev","db_last_modified":"2026-09-02T19:12:04Z","go_version":"go1.27.1","scan_level":"symbol","scan_mode":"source"}}'

# The real package set, so the "narrowed scan" and "complete scan" cases are
# measured against the module as it actually is rather than a magic number.
"$GO" list ./... > "$work/pkgs.txt"
ALL_ROOTS=$(python3 -c 'import json,sys; print(json.dumps([l.strip() for l in open(sys.argv[1]) if l.strip()]))' "$work/pkgs.txt")
SBOM_FULL="{\"SBOM\":{\"go_version\":\"go1.27.1\",\"modules\":[{\"path\":\"github.com/FlavioCFOliveira/GoGraph\"},{\"path\":\"stdlib\",\"version\":\"v1.27.1\"}],\"roots\":$ALL_ROOTS}}"

# make_stub <name> <exit-code> <payload-file>  -> path to a fake govulncheck
make_stub() {
  local name=$1 code=$2 payload=$3 stub="$work/$1"
  cat > "$stub" <<STUB
#!/usr/bin/env bash
if [ "\${1:-}" = "-version" ]; then
  printf 'Go: go1.27.1\nScanner: govulncheck@v0.0.0-$name-stub\nDB: https://vuln.go.dev\n'
  exit 0
fi
cat "$payload"
exit $code
STUB
  chmod +x "$stub"
  printf '%s' "$stub"
}

# A label is used as a filename, so strip anything a path cannot carry.
slug() { printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'; }

# The reason matters. A gate that exits non-zero for an unrelated reason -
# `set -e` tripping over an inspection helper, say - looks exactly like a gate
# that caught the defect. Measured while writing this harness: the entire
# no-analysis battery below passed for precisely that wrong reason until the
# expected-reason assertion was added. So every case names the message it must
# see, and `sed` rather than `grep` reads it back, because the interactive
# `grep` on this host is a ugrep wrapper that can return silently empty.
reason() { sed -n 's/^vulncheck-gate: FAIL - //p' "$1" | head -1; }

# expect_fail <label> <expected-reason-substring> <stub> [extra env...]
expect_fail() {
  local label=$1 want=$2 stub=$3; shift 3
  local log="$work/$(slug "$label").log" rc=0 got
  env GOVULNCHECK="$stub" VULNCHECK_AUTO_INSTALL=0 "$@" bash "$GATE" > "$log" 2>&1 || rc=$?
  got=$(reason "$log")
  if [ "$rc" -eq 0 ]; then
    bad "$label: the gate PASSED a run it must reject (this is the vacuity the gate exists to remove)"
    sed 's/^/       /' "$log"
  elif [ -z "$got" ]; then
    bad "$label: exited $rc but printed no 'FAIL -' reason, so it did NOT fail on its assertion"
    sed 's/^/       /' "$log"
  elif [[ $got == *"$want"* ]]; then
    ok "$label: rejected (exit $rc) - $got"
  else
    bad "$label: rejected (exit $rc) but for the wrong reason"
    echo "       wanted a reason containing: $want"
    echo "       got:                        $got"
  fi
}

# expect_pass <label> <stub> [extra env...]
expect_pass() {
  local label=$1 stub=$2; shift 2
  local log="$work/$(slug "$label").log" rc=0
  env GOVULNCHECK="$stub" VULNCHECK_AUTO_INSTALL=0 "$@" bash "$GATE" > "$log" 2>&1 || rc=$?
  if [ "$rc" -eq 0 ]; then
    ok "$label: accepted - $(sed -n 's/^vulncheck-gate: PASS - //p' "$log" | head -1)"
  else
    bad "$label: the gate REJECTED a run it must accept (exit $rc)"
    sed 's/^/       /' "$log"
  fi
}

echo "test_vulncheck_gate: the gate must FAIL on every no-analysis shape below"

# 1. THE v0.12.0 SHAPE: exit 0, config emitted, nothing analysed. A gate that
#    reads $? passes this. This case is the whole point of the exercise.
printf '%s\n' "$CONFIG" > "$work/p1.json"
expect_fail "config-only-exit-0" "NO ANALYSIS HAPPENED" "$(make_stub s1 0 "$work/p1.json")"

# 2. Same, but exit 1 - the loud form the stale binary produces today.
printf '%s\n' "$CONFIG" > "$work/p2.json"
expect_fail "config-only-exit-1" "NO ANALYSIS HAPPENED" "$(make_stub s2 1 "$work/p2.json")"

# 3. An SBOM that reached the report but carries no root packages.
printf '%s\n%s\n' "$CONFIG" '{"SBOM":{"go_version":"go1.27.1","modules":[],"roots":[]}}' > "$work/p3.json"
expect_fail "empty-roots" "NO ANALYSIS HAPPENED" "$(make_stub s3 0 "$work/p3.json")"

# 4. A scan silently narrowed to one package - a pattern typo must not certify
#    the module from a fraction of it.
printf '%s\n%s\n' "$CONFIG" '{"SBOM":{"go_version":"go1.27.1","modules":[{"path":"github.com/FlavioCFOliveira/GoGraph"}],"roots":["github.com/FlavioCFOliveira/GoGraph/ds"]}}' > "$work/p4.json"
expect_fail "narrowed-scan" "were NOT scanned" "$(make_stub s4 0 "$work/p4.json")"

# 5. The weaker -scan=module fallback dressed up as a full scan.
printf '%s\n%s\n' \
  '{"config":{"scanner_name":"govulncheck","scanner_version":"v1.7.0-stub","db":"https://vuln.go.dev","db_last_modified":"2026-09-02T19:12:04Z","go_version":"go1.27.1","scan_level":"module","scan_mode":"source"}}' \
  "$SBOM_FULL" > "$work/p5.json"
expect_fail "module-level-scan" "scan_level is" "$(make_stub s5 0 "$work/p5.json")"

# 6. A real vulnerability reported while the process exits 0 - again, $? lies.
printf '%s\n%s\n%s\n%s\n' "$CONFIG" "$SBOM_FULL" \
  '{"osv":{"schema_version":"1.3.1","id":"GO-0000-0000","aliases":["CVE-0000-0000"],"summary":"Synthetic vulnerability injected by test_vulncheck_gate.sh"}}' \
  '{"finding":{"osv":"GO-0000-0000","fixed_version":"v9.9.9","trace":[{"module":"example.com/fake","package":"example.com/fake","function":"Vulnerable"}]}}' \
  > "$work/p6.json"
expect_fail "vulnerability-with-exit-0" "reported 1 vulnerability" "$(make_stub s6 0 "$work/p6.json")"

# 7. Output that is not a JSON stream at all.
printf 'this is not json\n' > "$work/p7.json"
expect_fail "garbage-output" "not a valid govulncheck JSON stream" "$(make_stub s7 0 "$work/p7.json")"

# 8. No usable binary, and installing is disabled: the gate must fail, never skip.
rc=0
env GOVULNCHECK="$work/does-not-exist" VULNCHECK_AUTO_INSTALL=0 bash "$GATE" > "$work/p8.log" 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then bad "missing-binary: the gate PASSED with no scanner at all"; else
  ok "missing-binary: rejected (exit $rc) - $(reason "$work/p8.log")"
fi

# 9. THE REAL REPRODUCTION: a govulncheck built against a different Go minor.
stale=${VULNCHECK_STALE_BIN:-}
if [ -z "$stale" ]; then
  cur=$("$GO" env GOVERSION | sed -E 's/^(go[0-9]+\.[0-9]+).*/\1/')
  for c in "$HOME/.local/bin/govulncheck" "$(command -v govulncheck 2>/dev/null || true)" "$($GO env GOPATH)/bin/govulncheck"; do
    [ -n "$c" ] && [ -x "$c" ] || continue
    tc=$("$GO" version -m "$c" 2>/dev/null | awk 'NR==1 {print $2}' | sed -E 's/^(go[0-9]+\.[0-9]+).*/\1/')
    if [ -n "$tc" ] && [ "$tc" != "$cur" ]; then stale=$c; break; fi
  done
fi
if [ -n "$stale" ]; then
  expect_fail "stale-toolchain-binary($stale)" "NO ANALYSIS HAPPENED" "$stale"
else
  echo "  --   stale-toolchain-binary: no binary built for another Go minor is installed on this"
  echo "       host, so the real-world reproduction is unavailable here. Cases 1-2 reproduce"
  echo "       its output shape exactly (config-only, exit 0 and exit 1). Set"
  echo "       VULNCHECK_STALE_BIN=/path/to/old/govulncheck to run it against the real binary."
fi

echo "test_vulncheck_gate: and it must PASS a run that really analysed the module"

# 10. The control. Without it, every result above could come from the injection
#     mechanism rather than from the injected defect.
printf '%s\n%s\n' "$CONFIG" "$SBOM_FULL" > "$work/p10.json"
expect_pass "complete-scan-no-findings" "$(make_stub s10 0 "$work/p10.json")"

# 11. End to end with the real scanner on a real package, no stub anywhere.
rc=0
if env VULNCHECK_PATTERNS="./ds/..." VULNCHECK_REQUIRE_ALL_PACKAGES=0 bash "$GATE" > "$work/p11.log" 2>&1; then
  ok "real-scanner-end-to-end: $(sed -n 's/^vulncheck-gate: PASS - //p' "$work/p11.log" | head -1)"
else
  rc=$?
  bad "real-scanner-end-to-end: the gate failed against the real scanner (exit $rc)"
  sed 's/^/       /' "$work/p11.log"
fi

echo "test_vulncheck_gate: $pass_count passed, $fail_count failed"
[ "$fail_count" -eq 0 ] || exit 1
