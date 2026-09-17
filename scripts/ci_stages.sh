#!/usr/bin/env bash
# ci_stages.sh — run the `make ci` gate stages IN ORDER, stop at the first
# failure, time each one, and record which stages are already proved against the
# current tree so a correction costs only the work it invalidates.
#
# WHY THIS EXISTS (measured, not hypothetical). Publishing v0.14.2 cost ~70
# minutes of gate time and most of it was avoidable. `ci` listed its members as
#
#   shell-guard tidy fmt vet build vulncheck test-short test-timing \
#   test-uninstrumented lint cover-gate ci-kg-verify
#
# so `lint` ran TENTH — after the ~25-minute `-race ./...` suite. A single
# `revive: context-as-argument` violation in one file therefore failed the gate
# AFTER the suite had run, and the two stages behind it never executed at all.
# Ordering the gate so that the cheap checks precede the expensive ones is worth
# more than any speed-up inside them, and ordering is only defensible when the
# costs are measured — hence the per-stage timing this script prints on every
# run.
#
# WHAT IT DOES NOT DO. It does not change what any stage checks. Every stage is
# still the Makefile target of the same name, invoked through `$(MAKE)`, so the
# recipe — including the SHELL flags that `shell-guard` asserts — is unchanged.
# This script only decides the ORDER, the timing report, and whether a stage may
# be skipped because it has already passed against this exact tree.
#
# Usage (always through the Makefile, which supplies CI_STAGES):
#   CI_MODE=fresh   bash scripts/ci_stages.sh     # make ci        — run everything
#   CI_MODE=resume  bash scripts/ci_stages.sh     # make ci-resume — honour stamps
#   CI_MODE=from CI_FROM=lint bash scripts/ci_stages.sh
#                                                 # make ci-from STAGE=lint
#
# Environment:
#   CI_STAGES        REQUIRED. Ordered, space-separated make targets to run.
#   CI_MODE          fresh (default) | resume | from
#   CI_FROM          with CI_MODE=from, the stage to start at (must be in CI_STAGES)
#   CI_STAMP_DIR     where stamps and per-stage logs live (default build/ci).
#                    MUST be a git-ignored path — see "the key" below.
#   CI_NEVER_STAMP   stages that are never stamped and therefore always re-run
#   CI_KEY_EXTRA     extra text folded into the key, for env overrides that change
#                    what a stage does without changing a file
#   MAKE             the make binary (default: make)
#
# Exit status: the failing stage's status, or 0. The last line printed is always
# `ci-stages: CI_STAGES_EXIT=<n>`, so the verdict can be read FROM INSIDE the log
# rather than from a pipeline wrapper — this project has already had a real
# `make: *** Error 1` masked as exit 0 by a `| tail`.

set -uo pipefail

# Force a deterministic numeric locale, for the same reason scripts/cover_gate.sh
# does. Measured, not theorised: the first full run of this script on the
# reference host (pt_PT locale) printed every row of its own summary table as
# `0,0s`, because awk read the durations — written as `0.4` — through a locale
# whose decimal separator is a comma. The per-stage progress lines were unharmed
# (they use %s), so the run looked fine until the table at the end.
export LC_ALL=C

MAKE_BIN=${MAKE:-make}
CI_MODE=${CI_MODE:-fresh}
CI_FROM=${CI_FROM:-}
CI_STAMP_DIR=${CI_STAMP_DIR:-build/ci}
CI_NEVER_STAMP=${CI_NEVER_STAMP:-}
CI_KEY_EXTRA=${CI_KEY_EXTRA:-}

if [ -z "${CI_STAGES:-}" ]; then
  echo "ci-stages: FATAL - CI_STAGES is empty; invoke this through the Makefile" >&2
  echo "ci-stages: CI_STAGES_EXIT=2"
  exit 2
fi

# ── the key ───────────────────────────────────────────────────────────────────
# A stamp records the KEY the tree had when that stage passed. A stamp is honoured
# only when the key still matches EXACTLY. The rule is deliberately blunt:
#
#   ANY change to ANY file git does not ignore invalidates EVERY stamp.
#
# There is no per-stage cleverness about which files a stage "cares about",
# because that is precisely where a stamp comes to wrongly survive — and a stamp
# that wrongly survives silently SKIPS A GATE, which is far worse than one that
# wrongly expires and merely costs time. Every stage here reads Go sources, so
# there is no honest narrower rule to write: edit a .go file and the race suite
# genuinely has to run again.
#
# What resumability therefore buys is NOT "skip the suite after a code fix". It is
# the case where the tree did NOT change:
#
#   * `ci-kg-verify` failed because the graph server was not running;
#   * `vulncheck` failed because the network was down;
#   * the gate was interrupted (Ctrl-C, the lid closed, a reboot);
#   * a stage failed for an environmental reason and the fix was outside the tree.
#
# In those cases every earlier stage is still proved and `make ci-resume` starts
# at the one that failed. For a fix that edits a file, the honest answer is that
# everything re-runs — and the REORDERING is what makes that cheap, because the
# failure now happens in the first minutes instead of after the suite.
#
# The key covers the whole non-ignored working tree by CONTENT, not by mtime:
# `git ls-files --cached --others --exclude-standard` is the exact set of files
# git tracks or would track, so build artefacts and the stamp directory itself are
# excluded by .gitignore, and a file that is added, edited, renamed or deleted all
# move the key. It also folds in the toolchain versions, because a Go or
# golangci-lint upgrade changes what a stage concludes without changing a file.
#
# Measured on this repository (Apple M4, 4374 non-ignored files): 0.23-0.31 s,
# byte-identical across repeated runs. That is cheap enough to recompute after
# every stage, which is what makes stamping correct in the presence of the two
# stages that REWRITE the tree — `tidy` and `fmt`. A stage is stamped with the key
# as it stands when that stage FINISHES, so "fmt rewrote a file" leaves the stages
# before it correctly invalidated rather than silently trusted.
tree_key() {
  local files_digest tool_digest
  # `pipefail` is set for this script, so the command substitution's status IS
  # the pipeline's status. A failure anywhere in it (not a git repo, a tracked
  # file deleted from the working tree, shasum unavailable) must DISABLE
  # stamping, never produce a weaker key: an empty key means "unknown", and an
  # unknown key can match nothing.
  if ! files_digest=$(
        git ls-files --cached --others --exclude-standard -z 2>/dev/null \
          | xargs -0 shasum -a 256 2>/dev/null \
          | shasum -a 256
      ); then
    echo ""
    return 0
  fi
  if [ -z "${files_digest}" ]; then
    echo ""
    return 0
  fi
  tool_digest=$(
    {
      go version 2>/dev/null || echo "go: absent"
      golangci-lint version 2>/dev/null || echo "golangci-lint: absent"
      echo "key-extra: ${CI_KEY_EXTRA}"
      echo "stages: ${CI_STAGES}"
    } | shasum -a 256
  )
  echo "${files_digest%% *}-${tool_digest%% *}"
}

now() { python3 -c 'import time; print("%.3f" % time.time())'; }
elapsed() { python3 -c "print('%.1f' % (${2} - ${1}))"; }

in_list() {
  # in_list <needle> <space-separated haystack>
  local needle=$1 hay=$2 item
  for item in ${hay}; do
    [ "${item}" = "${needle}" ] && return 0
  done
  return 1
}

stamp_path() { echo "${CI_STAMP_DIR}/stamp.$1"; }
stage_log()  { echo "${CI_STAMP_DIR}/$1.log"; }

# ── mode validation ───────────────────────────────────────────────────────────
case "${CI_MODE}" in
  fresh|resume|from) ;;
  *)
    echo "ci-stages: FATAL - unknown CI_MODE '${CI_MODE}' (expected fresh, resume or from)" >&2
    echo "ci-stages: CI_STAGES_EXIT=2"
    exit 2
    ;;
esac

if [ "${CI_MODE}" = "from" ]; then
  if [ -z "${CI_FROM}" ]; then
    echo "ci-stages: FATAL - CI_MODE=from needs CI_FROM (use: make ci-from STAGE=<stage>)" >&2
    echo "ci-stages: valid stages: ${CI_STAGES}" >&2
    echo "ci-stages: CI_STAGES_EXIT=2"
    exit 2
  fi
  if ! in_list "${CI_FROM}" "${CI_STAGES}"; then
    echo "ci-stages: FATAL - '${CI_FROM}' is not a stage of this gate" >&2
    echo "ci-stages: valid stages: ${CI_STAGES}" >&2
    echo "ci-stages: CI_STAGES_EXIT=2"
    exit 2
  fi
fi

mkdir -p "${CI_STAMP_DIR}"

# `ci` means RUN THE WHOLE GATE. It therefore discards every stamp before it
# starts, so a fresh run can never silently skip a stage on the strength of an
# earlier one — and then re-stamps as it goes, so a later `ci-resume` has
# something to honour.
if [ "${CI_MODE}" = "fresh" ]; then
  rm -f "${CI_STAMP_DIR}"/stamp.* 2>/dev/null || true
fi

start_key=$(tree_key)
total_stages=$(echo ${CI_STAGES} | wc -w | tr -d ' ')

echo "================================================================"
echo "ci-stages: mode=${CI_MODE}${CI_FROM:+ from=${CI_FROM}}  stages=${total_stages}"
echo "ci-stages: order  ${CI_STAGES}"
if [ -n "${start_key}" ]; then
  echo "ci-stages: tree key ${start_key}"
else
  echo "ci-stages: tree key UNAVAILABLE - stamping disabled, every stage will run"
fi
[ -n "${CI_NEVER_STAMP}" ] && echo "ci-stages: never stamped (verdict depends on state outside the tree): ${CI_NEVER_STAMP}"
echo "ci-stages: logs and stamps under ${CI_STAMP_DIR}/"
echo "ci-stages: started $(date '+%Y-%m-%d %H:%M:%S')  loadavg:$(uptime | sed 's/.*averages*://')"
echo "================================================================"

gate_t0=$(now)
summary=""
failed_stage=""
rc=0
idx=0
skipping_until="${CI_FROM}"

for stage in ${CI_STAGES}; do
  idx=$((idx + 1))

  # CI_MODE=from: skip unconditionally until the named stage is reached. This is
  # the DEVELOPER'S assertion that the earlier stages are unaffected; the gate
  # cannot verify it, which is why it is a separate, explicitly-unsafe target and
  # not what `ci-resume` does.
  if [ -n "${skipping_until}" ]; then
    if [ "${stage}" = "${skipping_until}" ]; then
      skipping_until=""
    else
      printf '  [%2d/%2d] %-20s SKIPPED (ci-from %s — NOT verified)\n' \
        "${idx}" "${total_stages}" "${stage}" "${CI_FROM}"
      summary="${summary}${stage}|skipped-from|0.0
"
      continue
    fi
  fi

  # CI_MODE=resume: skip only a stage whose stamp matches the CURRENT key.
  if [ "${CI_MODE}" = "resume" ] && [ -n "${start_key}" ] \
     && ! in_list "${stage}" "${CI_NEVER_STAMP}"; then
    sp=$(stamp_path "${stage}")
    if [ -f "${sp}" ] && [ "$(cat "${sp}" 2>/dev/null)" = "${start_key}" ]; then
      printf '  [%2d/%2d] %-20s skipped (already green against this tree)\n' \
        "${idx}" "${total_stages}" "${stage}"
      summary="${summary}${stage}|skipped-stamp|0.0
"
      continue
    fi
  fi

  echo
  echo "---------------- [${idx}/${total_stages}] ${stage} ----------------"
  log=$(stage_log "${stage}")
  t0=$(now)
  # PIPESTATUS[0] is read explicitly and IMMEDIATELY, so `tee` cannot report
  # success for a failing make. The script deliberately does not run under
  # `set -e`: every stage's status is inspected here by hand, because this
  # project has already shipped a gate whose real failure was masked by a
  # pipeline wrapper.
  "${MAKE_BIN}" "${stage}" 2>&1 | tee "${log}"
  stage_rc=${PIPESTATUS[0]}
  t1=$(now)
  dur=$(elapsed "${t0}" "${t1}")

  if [ "${stage_rc}" -ne 0 ]; then
    printf '  [%2d/%2d] %-20s FAILED  %8ss  (exit %d)\n' \
      "${idx}" "${total_stages}" "${stage}" "${dur}" "${stage_rc}"
    summary="${summary}${stage}|FAILED|${dur}
"
    failed_stage="${stage}"
    rc="${stage_rc}"
    break
  fi

  printf '  [%2d/%2d] %-20s ok      %8ss\n' "${idx}" "${total_stages}" "${stage}" "${dur}"
  summary="${summary}${stage}|ok|${dur}
"

  # Stamp with the key AS IT STANDS NOW, not as it stood when the gate started:
  # `tidy` and `fmt` rewrite the tree, and a stage must record the tree it
  # actually passed against.
  if ! in_list "${stage}" "${CI_NEVER_STAMP}"; then
    k=$(tree_key)
    if [ -n "${k}" ]; then
      printf '%s' "${k}" > "$(stamp_path "${stage}")"
    fi
  fi
done

gate_t1=$(now)
gate_dur=$(elapsed "${gate_t0}" "${gate_t1}")
end_key=$(tree_key)

echo
echo "================================================================"
echo "ci-stages: per-stage wall clock"
printf '%s' "${summary}" | awk -F'|' '{ printf "    %-22s %-14s %9.1fs\n", $1, $2, $3 }'
echo "    ----------------------------------------------------"
printf '    %-22s %-14s %9ss\n' "TOTAL" "" "${gate_dur}"
echo "ci-stages: finished $(date '+%Y-%m-%d %H:%M:%S')  loadavg:$(uptime | sed 's/.*averages*://')"

if [ -n "${start_key}" ] && [ "${start_key}" != "${end_key}" ]; then
  echo "ci-stages: NOTE - the tree key changed during this run"
  echo "ci-stages:        start ${start_key}"
  echo "ci-stages:        end   ${end_key}"
  echo "ci-stages:        Either a stage rewrote a non-ignored file (tidy and fmt"
  echo "ci-stages:        do this by design) or the tree was edited WHILE the gate"
  echo "ci-stages:        ran. Stamps record the key at each stage's own"
  echo "ci-stages:        completion, so the stages before the change are correctly"
  echo "ci-stages:        invalidated rather than trusted. This is not an error,"
  echo "ci-stages:        but if you were editing during the run, the verdict"
  echo "ci-stages:        covers no single tree - re-run it on a frozen one."
fi

if [ -n "${failed_stage}" ]; then
  echo "ci-stages: FAILED at '${failed_stage}' (stage ${idx} of ${total_stages}) after ${gate_dur}s"
  echo "ci-stages: that stage's output: $(stage_log "${failed_stage}")"
  echo "ci-stages: fix it, then:"
  echo "ci-stages:   make ci-resume            # re-runs everything the fix invalidated"
  echo "ci-stages:                             # (a file edit invalidates every stamp)"
  echo "ci-stages:   make ci-from STAGE=${failed_stage}"
  echo "ci-stages:                             # UNVERIFIED shortcut: you assert the"
  echo "ci-stages:                             # earlier stages are unaffected"
else
  echo "ci-stages: ALL ${total_stages} STAGES GREEN in ${gate_dur}s"
fi
echo "ci-stages: CI_STAGES_EXIT=${rc}"
echo "================================================================"
exit "${rc}"
