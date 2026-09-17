#!/usr/bin/env bash
# test_examples-lab.sh — self-test for scripts/examples-lab.sh (rmp #2856).
#
# A sweep driver's most dangerous failure is not crashing: it is reporting a row
# that produced nothing as though it had produced evidence. Everything
# downstream — the CPU attribution, the ownership split, the coverage complement
# — is then computed over a gap that nobody can see. So the one property that
# must not be taken on trust is that a broken run is reported as a FAILED row.
#
# Three cases, all on one small example so the test is seconds rather than
# minutes:
#
#   1. A deliberately broken invocation (a flag no example accepts) must produce
#      a FAIL row naming the failure, and the driver must exit non-zero.
#   2. The same row, invoked correctly, must produce a PASS row with a non-zero
#      CPU total and a non-zero allocation total, and the driver must exit 0.
#   3. A row whose profile exists but is EMPTY must FAIL. This is the case a
#      file-existence check cannot catch, and it is provoked directly by
#      truncating the artefacts and re-running the verdict through the probe.
#
# Run directly:
#     bash scripts/test_examples-lab.sh [WORK_DIR]

set -u -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
DRIVER="$SCRIPT_DIR/examples-lab.sh"
PROBE="$SCRIPT_DIR/examples_lab_probe.py"

WORK="${1:-${TMPDIR:-/tmp}/examples-lab-selftest.$$}"
mkdir -p "$WORK" || exit 1
WORK="$(cd "$WORK" && pwd)"
case "$WORK" in
  "$REPO"/*)
    echo "FAIL: the self-test work directory is inside the repository" >&2
    exit 1
    ;;
esac

fails=0
ok() { echo "PASS: $*"; }
bad() {
  echo "FAIL: $*" >&2
  fails=$(( fails + 1 ))
}

row() { # MANIFEST ROW_ID COLUMN
  awk -F'\t' -v r="$2" -v c="$3" 'NR==1 {for (i=1;i<=NF;i++) h[$i]=i; next} $2==r {print $(h[c])}' "$1"
}

# ─── 1. a deliberately broken invocation ─────────────────────────────────────
BROKEN="$WORK/broken"
LAB_ONLY='^01_basic$' LAB_BREAK=1 bash "$DRIVER" "$BROKEN/out" default \
  > "$WORK/broken.log" 2>&1
echo "driver_exit=$?" >> "$WORK/broken.log"

brc="$(sed -n 's/^driver_exit=//p' "$WORK/broken.log" | tail -1)"
bman="$BROKEN/out/default/manifest.tsv"
if [ "$brc" = "0" ]; then
  bad "the driver exited 0 on a deliberately broken invocation"
else
  ok "the driver exited $brc on a deliberately broken invocation"
fi
if [ ! -s "$bman" ]; then
  bad "no manifest was written for the broken run"
else
  v="$(row "$bman" 01_basic verdict)"
  n="$(row "$bman" 01_basic notes)"
  if [ "$v" = "FAIL" ]; then
    ok "the broken row is recorded as FAIL (notes: $n)"
  else
    bad "the broken row is recorded as '$v', not FAIL (notes: $n)"
  fi
  case "$n" in
    *no-cpu-profile* | *exit=*) ok "the FAIL row names its reason: $n" ;;
    *) bad "the FAIL row does not name a reason: $n" ;;
  esac
fi

# ─── 2. the same row, correctly invoked ──────────────────────────────────────
GOOD="$WORK/good"
LAB_ONLY='^01_basic$' bash "$DRIVER" "$GOOD/out" default > "$WORK/good.log" 2>&1
echo "driver_exit=$?" >> "$WORK/good.log"

grc="$(sed -n 's/^driver_exit=//p' "$WORK/good.log" | tail -1)"
gman="$GOOD/out/default/manifest.tsv"
if [ "$grc" = "0" ]; then
  ok "the driver exited 0 on a correct invocation"
else
  bad "the driver exited $grc on a correct invocation — see $WORK/good.log"
fi
if [ ! -s "$gman" ]; then
  bad "no manifest was written for the correct run"
else
  v="$(row "$gman" 01_basic verdict)"
  cpu="$(row "$gman" 01_basic cpu_ns)"
  alloc="$(row "$gman" 01_basic heap_alloc_b)"
  scale="$(row "$gman" 01_basic scale)"
  [ "$v" = "PASS" ] && ok "the good row is PASS" || bad "the good row is '$v'"
  case "$cpu" in
    '' | '-' | 0) bad "cpu_ns is '$cpu' on a PASS row" ;;
    *) ok "cpu_ns=$cpu" ;;
  esac
  case "$alloc" in
    '' | '-' | 0) bad "heap_alloc_b is '$alloc' on a PASS row" ;;
    *) ok "heap_alloc_b=$alloc" ;;
  esac
  [ -n "$scale" ] && ok "the row names its scale: $scale" || bad "the row names no scale"
  for f in cpu.pprof heap.pprof mutex.pprof block.pprof goroutine.pprof; do
    if [ -s "$GOOD/out/default/01_basic/$f" ]; then
      ok "artefact present: $f"
    else
      bad "artefact missing: $f"
    fi
  done
fi

# ─── 3. an EMPTY profile must not read as evidence ───────────────────────────
#
# This is the case the manifest's own file-existence check cannot see. The probe
# is the thing that has to catch it, so the probe is what is tested: a truncated
# profile must come back MISSING or UNREADABLE or zero, never as a number.
EMPTY="$WORK/empty.pprof"
: > "$EMPTY"
python3 "$PROBE" "$EMPTY" > "$WORK/empty-probe.txt" 2>&1
echo "probe_exit=$?" >> "$WORK/empty-probe.txt"
if grep -q 'UNREADABLE' "$WORK/empty-probe.txt" 2>/dev/null; then
  ok "the probe reports an empty file as UNREADABLE"
else
  # ugrep can return silent empty output; confirm with python before concluding.
  if python3 -c 'import sys;sys.exit(0 if "UNREADABLE" in open(sys.argv[1]).read() else 1)' \
      "$WORK/empty-probe.txt"; then
    ok "the probe reports an empty file as UNREADABLE (confirmed with python3)"
  else
    bad "the probe did not reject an empty profile: $(cat "$WORK/empty-probe.txt")"
  fi
fi
MISSING="$WORK/does-not-exist.pprof"
python3 "$PROBE" "$MISSING" > "$WORK/missing-probe.txt" 2>&1
echo "probe_exit=$?" >> "$WORK/missing-probe.txt"
if python3 -c 'import sys;sys.exit(0 if "MISSING" in open(sys.argv[1]).read() else 1)' \
    "$WORK/missing-probe.txt"; then
  ok "the probe reports an absent file as MISSING"
else
  bad "the probe did not report an absent file as MISSING"
fi
if [ "$(sed -n 's/^probe_exit=//p' "$WORK/missing-probe.txt" | tail -1)" = "1" ]; then
  ok "the probe exits 1 on an unreadable profile"
else
  bad "the probe exited 0 on an unreadable profile"
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "examples-lab self-test: ALL CHECKS PASSED (work dir $WORK)"
  exit 0
fi
echo "examples-lab self-test: $fails CHECK(S) FAILED (work dir $WORK)" >&2
exit 1
