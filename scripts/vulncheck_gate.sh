#!/usr/bin/env bash
#
# vulncheck_gate.sh — the module's vulnerability gate (rmp #2722).
#
# ── WHY THIS SCRIPT EXISTS ────────────────────────────────────────────────────
#
# Until rmp #2722 the vulnerability scan was wired into NOTHING: `govulncheck`
# appeared in no Makefile target, no `make ci` path and no `.sh`/`.yml`/`.yaml`
# file in the repository — only in CONTRIBUTING.md §4 and SECURITY.md as prose
# describing a command a human was expected to type. A gate nobody invokes
# cannot fail loudly; it simply never runs, which is exactly why it could stop
# working for a whole toolchain bump without anyone noticing.
#
# ── WHY IT DOES NOT TRUST THE EXIT CODE ───────────────────────────────────────
#
# The failure mode recorded against the v0.12.0 release, and documented in
# CONTRIBUTING.md §4, is a govulncheck binary built against an older Go minor
# than the toolchain on PATH that "exits 0 while performing no analysis at
# all". A gate that reads `$?` passes that run. So this script asserts that
# analysis ACTUALLY HAPPENED, and it evaluates that assertion BEFORE it looks
# at the exit status.
#
# The evidence it asserts on is the `SBOM` message of `-format json`:
#
#   * `config` is emitted BEFORE any package loading — it describes the
#     scanner, not the scan — so its presence proves nothing. Measured
#     2026-09-03: govulncheck@v1.3.0 (built for go1.26.3) on ./graph/adjlist/...
#     under go1.27.1 emitted 289 bytes containing ONLY the config message,
#     because go1.26's source-processing packages cannot parse go1.27's
#     math/rand/v2. A pattern matching no packages (./docs/...) stops at
#     config too.
#   * `SBOM.roots` is built from the LOADED package graph and holds RESOLVED
#     import paths, not the pattern text: the pattern `./ds/...` produces the
#     root `github.com/FlavioCFOliveira/GoGraph/ds`. A run that loaded nothing
#     has nothing to resolve, so it cannot emit a non-empty `roots`. That is
#     what makes this evidence unforgeable by a broken run, and it is why the
#     assertion is on `roots` and not on "the command produced output".
#
# On top of a non-empty `roots` the gate asserts three further properties, each
# closing a way the scan could silently shrink:
#
#   * every package `go list ./...` reports must appear in `roots`, so a scan
#     narrowed to a subset (a pattern typo, a stray directory argument) fails
#     instead of certifying the module from a fraction of it. Measured
#     2026-09-03: the two sets are exactly equal at 136 packages.
#   * `scan_mode` must be `source` and `scan_level` must be `symbol`, so the
#     weaker `-scan=module` fallback that CONTRIBUTING.md §4 currently
#     prescribes cannot pass as if it were the full reachability analysis.
#   * findings are read out of the JSON itself rather than inferred from the
#     exit status, so a vulnerability fails the gate whatever `$?` says.
#
# ── WHY IT DOES NOT TRUST `PATH` ──────────────────────────────────────────────
#
# Measured 2026-09-03 on this host: `which govulncheck` resolves to
# ~/.local/bin/govulncheck, which `go version -m` reports as built with
# go1.26.3 — the binary that analyses nothing. The toolchain-matched build
# lives in GOBIN/GOPATH/bin. Resolution therefore prefers the candidate whose
# BUILD TOOLCHAIN MINOR matches the running `go`, prints every candidate it
# found with both versions so a shadow is visible in the log of every run, and
# installs a pinned build when none is usable. It never silently skips: a
# skipped security gate is the defect this gate exists to remove.
#
# ── ENVIRONMENT ───────────────────────────────────────────────────────────────
#
#   GO                              go command (default: go)
#   GOVULNCHECK                     explicit binary; used verbatim, no matching.
#                                   This is the injection point the self-test
#                                   uses to feed the gate deliberately broken
#                                   scanners.
#   GOVULNCHECK_VERSION             version installed when none is usable
#   VULNCHECK_PATTERNS              package patterns (default: ./...)
#   VULNCHECK_REQUIRE_ALL_PACKAGES  1 = every `go list` package must be scanned
#   VULNCHECK_AUTO_INSTALL          1 = install a pinned build when none usable
#   VULNCHECK_DB                    vulnerability database URL/mirror; the gate
#                                   needs the database, and an unreachable one
#                                   is a FAILED scan, never a clean one
#   VULNCHECK_REPORT                keep the JSON report at this path
#
# Exit status: 0 only when analysis is proven AND no vulnerability was found.

set -euo pipefail

GO=${GO:-go}
PATTERNS=${VULNCHECK_PATTERNS:-./...}
PINNED_VERSION=${GOVULNCHECK_VERSION:-v1.7.0}
REQUIRE_ALL=${VULNCHECK_REQUIRE_ALL_PACKAGES:-1}
AUTO_INSTALL=${VULNCHECK_AUTO_INSTALL:-1}
REPORT=${VULNCHECK_REPORT:-}
DB=${VULNCHECK_DB:-}

say()  { echo "vulncheck-gate: $*"; }
fail() { echo "vulncheck-gate: FAIL - $*" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 \
  || fail "python3 is required to verify the report (scripts/pkg_time_budget.sh already depends on it)"

# ── Binary resolution ─────────────────────────────────────────────────────────

# Build toolchain of a Go binary, e.g. "go1.26.3". Empty when unreadable.
# `go version -m` fails on anything that is not a Go binary. Both helpers must
# still succeed, or `set -e` would kill the gate while it is merely INSPECTING a
# candidate — and the gate would then exit non-zero for a reason that has
# nothing to do with its assertion. Measured while writing the self-test: the
# whole no-analysis battery passed for exactly that wrong reason.
bin_toolchain() { { "$GO" version -m "$1" 2>/dev/null | awk 'NR==1 {print $2}'; } || true; }
# Scanner version as reported by the binary itself, e.g. "govulncheck@v1.7.0".
bin_scanner()   { { "$1" -version 2>/dev/null | awk '/^Scanner:/ {print $2}'; } || true; }
minor()         { sed -E 's/^(go[0-9]+\.[0-9]+).*/\1/'; }

GO_VERSION=$("$GO" env GOVERSION)
GO_MINOR=$(printf '%s' "$GO_VERSION" | minor)

candidates=()
add_candidate() {
  local p=$1 c
  [[ -n $p && -x $p ]] || return 0
  for c in ${candidates+"${candidates[@]}"}; do [[ $c == "$p" ]] && return 0; done
  candidates+=("$p")
}

# Order encodes the preference argued above: an explicit override first, then
# the toolchain-managed install locations, then PATH last — PATH is where the
# stale shadow lives on this host.
add_candidate "${GOVULNCHECK:-}"
gobin=$("$GO" env GOBIN);  [[ -n $gobin  ]] && add_candidate "$gobin/govulncheck"
gopath=$("$GO" env GOPATH); [[ -n $gopath ]] && add_candidate "$gopath/bin/govulncheck"
add_candidate "$(command -v govulncheck 2>/dev/null || true)"

BIN=""
if [[ -n ${GOVULNCHECK:-} ]]; then
  # An explicit override is honoured verbatim, even when it is the wrong build:
  # the analysis assertion below is what judges it. This keeps the override
  # from becoming a way to bypass the gate.
  [[ -x ${GOVULNCHECK} ]] || fail "GOVULNCHECK=${GOVULNCHECK} is not an executable file"
  BIN=${GOVULNCHECK}
fi

say "go: $GO_VERSION (minor $GO_MINOR)"
if [[ ${#candidates[@]} -eq 0 ]]; then
  say "govulncheck candidates: none found"
else
  say "govulncheck candidates, in preference order:"
  for c in "${candidates[@]}"; do
    tc=$(bin_toolchain "$c"); sc=$(bin_scanner "$c")
    mark="   "
    [[ -n $tc && $(printf '%s' "$tc" | minor) == "$GO_MINOR" ]] || mark="!! "
    say "  ${mark}$c  built-with=${tc:-unknown}  scanner=${sc:-unknown}"
    [[ -z $BIN && $mark == "   " ]] && BIN=$c
  done
fi

if [[ -z $BIN ]]; then
  for c in ${candidates+"${candidates[@]}"}; do
    say "REJECTED $c: built with $(bin_toolchain "$c" || true), toolchain on PATH is $GO_VERSION."
    say "         A govulncheck built for a different Go minor cannot parse this"
    say "         toolchain's source and analyses nothing (CONTRIBUTING.md §4)."
  done
  if [[ $AUTO_INSTALL == 1 ]]; then
    say "installing golang.org/x/vuln/cmd/govulncheck@$PINNED_VERSION with $GO_VERSION (as \`make lint\` installs golangci-lint)"
    "$GO" install "golang.org/x/vuln/cmd/govulncheck@$PINNED_VERSION" \
      || fail "go install golang.org/x/vuln/cmd/govulncheck@$PINNED_VERSION failed"
    installed=${gobin:-$gopath/bin}/govulncheck
    [[ -x $installed ]] || fail "install reported success but $installed is not executable"
    tc=$(bin_toolchain "$installed")
    [[ $(printf '%s' "$tc" | minor) == "$GO_MINOR" ]] \
      || fail "freshly installed $installed reports built-with=$tc, expected $GO_MINOR"
    BIN=$installed
    say "installed $BIN (built-with=$tc, scanner=$(bin_scanner "$BIN"))"
  else
    fail "no govulncheck built for $GO_MINOR is available and VULNCHECK_AUTO_INSTALL=0.
       Install one with:  $GO install golang.org/x/vuln/cmd/govulncheck@$PINNED_VERSION
       Do NOT skip this gate: a vulnerability scan that does not run is not a clean scan."
  fi
fi

# Name the shadow explicitly rather than quietly working around it.
path_bin=$(command -v govulncheck 2>/dev/null || true)
if [[ -n $path_bin && $path_bin != "$BIN" ]]; then
  say "NOTE: \`govulncheck\` on PATH is $path_bin (built-with=$(bin_toolchain "$path_bin" || echo unknown),"
  say "      scanner=$(bin_scanner "$path_bin" || echo unknown)); this gate is deliberately using $BIN instead."
fi

say "using $BIN"

# ── Run ───────────────────────────────────────────────────────────────────────

workdir=$(mktemp -d "${TMPDIR:-/tmp}/vulncheck-gate.XXXXXX")
json=$workdir/report.json
errlog=$workdir/govulncheck.err
expected=$workdir/expected-packages.txt
cleanup() { rm -rf "$workdir"; }
trap cleanup EXIT

if [[ $REQUIRE_ALL == 1 && $PATTERNS == "./..." ]]; then
  "$GO" list ./... > "$expected" 2> "$workdir/golist.err" || {
    sed 's/^/  /' "$workdir/golist.err" >&2
    fail "\`$GO list ./...\` failed, so the set of packages that MUST be scanned cannot be
       established. A module that does not list cannot be certified."
  }
else
  : > "$expected"
fi

say "scanning $PATTERNS (source mode, symbol level)…"
rc=0
db_flag=()
[[ -n $DB ]] && { db_flag=(-db "$DB"); say "using vulnerability database $DB"; }
# shellcheck disable=SC2086  # PATTERNS is intentionally word-split: several patterns are allowed.
"$BIN" -format json ${db_flag+"${db_flag[@]}"} $PATTERNS > "$json" 2> "$errlog" || rc=$?

if [[ -s $errlog ]]; then
  say "govulncheck stderr:"
  sed 's/^/  /' "$errlog"
fi
say "govulncheck exit status: $rc (recorded, NOT trusted — the assertion below is what decides)"

[[ -n $REPORT ]] && { cp "$json" "$REPORT"; say "JSON report kept at $REPORT"; }

# ── Assert that analysis happened ─────────────────────────────────────────────

python3 - "$json" "$rc" "$expected" "$BIN" <<'PY'
import json, sys, datetime

report_path, rc_s, expected_path, binpath = sys.argv[1:5]
rc = int(rc_s)

def die(msg, *extra):
    print("vulncheck-gate: FAIL - " + msg, file=sys.stderr)
    for line in extra:
        print("       " + line, file=sys.stderr)
    sys.exit(1)

raw = open(report_path, encoding="utf-8").read()
dec, i, msgs = json.JSONDecoder(), 0, []
while i < len(raw):
    while i < len(raw) and raw[i] in " \n\t\r":
        i += 1
    if i >= len(raw):
        break
    try:
        obj, i = dec.raw_decode(raw, i)
    except ValueError as exc:
        die("the report is not a valid govulncheck JSON stream: %s" % exc,
            "report: %s (%d bytes)" % (report_path, len(raw)))
    msgs.append(obj)

kinds = {}
for m in msgs:
    for k in m:
        kinds[k] = kinds.get(k, 0) + 1

cfg = next((m["config"] for m in msgs if "config" in m), None)
if cfg is None:
    die("govulncheck emitted no `config` message at all (%d bytes of output)."
        % len(raw),
        "The scanner did not even start. Check its stderr above.")

sbom = next((m["SBOM"] for m in msgs if "SBOM" in m), None)
roots = list(sbom.get("roots", [])) if sbom else []
modules = list(sbom.get("modules", [])) if sbom else []

# ---- THE ASSERTION: analysis happened. Evaluated before the exit status. ----
if not roots:
    die("NO ANALYSIS HAPPENED - govulncheck loaded 0 root packages.",
        "It emitted %s and never reached the SBOM, which is built from the loaded" % kinds,
        "package graph. `config` is emitted before loading, so its presence proves",
        "nothing. This is the v0.12.0 failure mode (CONTRIBUTING.md §4): the scan",
        "did not run. Exit status was %d and is irrelevant to this verdict." % rc,
        "Two causes produce this, and govulncheck's stderr above distinguishes them:",
        "  (a) govulncheck is built against a different Go minor than the toolchain",
        "      on PATH - it says so explicitly. Rebuild it:",
        "        go install golang.org/x/vuln/cmd/govulncheck@latest",
        "  (b) the module's own packages do not load - fix the build first; a tree",
        "      that does not compile cannot be certified.",
        "Binary used: %s" % binpath)

mode, level = cfg.get("scan_mode"), cfg.get("scan_level")
if mode != "source":
    die("scan_mode is %r, expected 'source'." % mode,
        "A non-source scan does not analyse this module's own code.")
if level != "symbol":
    die("scan_level is %r, expected 'symbol'." % level,
        "Module-level scanning reports vulnerable dependencies without proving",
        "reachability, and cannot pass as the full analysis.")

expected_pkgs = [l.strip() for l in open(expected_path, encoding="utf-8") if l.strip()]
if expected_pkgs:
    missing = sorted(set(expected_pkgs) - set(roots))
    if missing:
        die("the scan covered %d of the module's %d packages - %d were NOT scanned."
            % (len(set(roots) & set(expected_pkgs)), len(expected_pkgs), len(missing)),
            *(["missing: " + p for p in missing[:10]]
              + (["... and %d more" % (len(missing) - 10)] if len(missing) > 10 else [])
              + ["A scan narrowed to a subset cannot certify the module."]))

# ---- Findings, read from the report rather than inferred from $? ----
findings = [m["finding"] for m in msgs if "finding" in m]
osvs = {m["osv"]["id"]: m["osv"] for m in msgs if "osv" in m}
called = {}
for f in findings:
    osv = f.get("osv", "?")
    trace = f.get("trace") or []
    reachable = any(fr.get("function") for fr in trace)
    entry = called.setdefault(osv, {"reachable": False, "modules": set()})
    entry["reachable"] |= reachable
    if trace:
        entry["modules"].add(trace[0].get("module", "?"))

stamp = datetime.date.today().isoformat()
record = ("RECORD %s  scanner=%s@%s  go=%s  db=%s  db_last_modified=%s  "
          "scan=%s/%s  packages_analysed=%d  modules_in_scope=%d  vulnerabilities=%d"
          % (stamp, cfg.get("scanner_name"), cfg.get("scanner_version"),
             cfg.get("go_version"), cfg.get("db"), cfg.get("db_last_modified"),
             mode, level, len(roots), len(modules), len(called)))

if called:
    print("vulncheck-gate: " + record)
    lines = []
    for osv_id, info in sorted(called.items()):
        o = osvs.get(osv_id, {})
        lines.append("%s: %s [%s] reachable=%s modules=%s"
                     % (osv_id, o.get("summary", "?"),
                        ",".join(o.get("aliases", [])) or "no alias",
                        info["reachable"], ",".join(sorted(info["modules"]))))
    die("govulncheck reported %d vulnerability/vulnerabilities." % len(called),
        *(lines + ["Triage each one and register it as its own task; do not silence this gate.",
                   "govulncheck's own exit status was %d." % rc]))

if rc not in (0, 3):
    die("analysis completed (%d packages) but govulncheck exited %d." % (len(roots), rc),
        "See its stderr above.")

print("vulncheck-gate: " + record)
print("vulncheck-gate: PASS - %d packages across %d modules analysed at %s/%s level; "
      "no vulnerabilities found." % (len(roots), len(modules), mode, level))
PY
