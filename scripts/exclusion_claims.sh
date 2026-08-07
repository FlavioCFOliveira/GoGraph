#!/usr/bin/env bash
# exclusion_claims.sh — enumerate every place in the module that still asserts an
# exclusion the MVCC transition retired (rmp #2345).
#
# WHY THIS IS A SCRIPT AND NOT A ONE-OFF READING
#
# Sprint 334 retired the module's pre-MVCC exclusion mechanisms: Engine.writeMu
# removed outright, the exclusive barrier taken off the autocommit write path, the
# transaction-lifetime lock across client think-time removed, reads stopped taking
# the barrier, the capacity-one store semaphore retired. The MECHANISMS are gone.
# The danger is that code and comments elsewhere still ASSUME them — and at least
# three did, each found by reading rather than by any check:
#
#   * graph/adjlist justified in-place mutation of a published builder by "all
#     readers hold visMu.RLock"; an MVCC snapshot reader reaches it without one.
#   * store/checkpoint justified pairing a durability position with a visibility
#     position by "a writer's registration spans its WHOLE commit" — true for
#     Tx.Commit and FALSE for Tx.CommitWALOnly, the engine's actual path. That one
#     LOST AN ACKNOWLEDGED COMMIT (rmp #2349).
#   * cypher/exectx.go's BeginTx doc still claimed an exclusive barrier hold
#     spanning the whole transaction, contradicting its own file header.
#
# An assumption that is merely stale is documentation debt; one that is load-bearing
# is a defect. Neither is findable by grepping once and forgetting. This script is
# the enumeration, so the audit can be RE-RUN as the code changes rather than
# re-derived from memory.
#
# USAGE
#   bash scripts/exclusion_claims.sh            # human-readable, grouped by file
#   bash scripts/exclusion_claims.sh --count    # just the totals, for tracking
#
# It exits 0 always: this is an INVENTORY, not a gate. A claim is not a defect on
# its face — most are correct statements about locks that still exist, and several
# are deliberate retractions that NAME the retired premise in order to correct it.
# Turning it into a pass/fail check would either forbid honest history or demand a
# suppression list that rots. What it gives is a bounded list a human can walk.

set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

MODE="${1:-report}"

# The claim patterns, each one a phrase that asserts an exclusion rather than merely
# naming a lock. Deliberately narrow: "visMu" alone matches every retraction in the
# module, which is noise, so the patterns target the ASSERTION shapes the three known
# cases had — a structure believed unobservable, a sequence believed atomic, an
# ordering believed guaranteed by serialised writers.
PATTERNS=(
	'serialised by the caller'
	'serialises writers'
	'serialises every writer'
	'writers are serialised'
	'barrier-serialised'
	'single-writer'
	'no concurrent writer'
	'nothing else can (publish|write|mutate)'
	'because the barrier'
	'under the (visibility )?barrier, so'
	'holds? (the )?visMu'
	'visMu\.(R?)Lock'
	'writeMu'
	'exclusive barrier'
	'while the barrier is held'
)

total=0
declare -a HITS=()

for pat in "${PATTERNS[@]}"; do
	while IFS= read -r line; do
		[ -z "$line" ] && continue
		HITS+=("$line")
		total=$((total + 1))
	done < <(grep -rnE --include='*.go' "$pat" . 2>/dev/null | grep -v '/vendor/' || true)
done

if [ "$MODE" = "--count" ]; then
	echo "$total"
	exit 0
fi

if [ "$total" -eq 0 ]; then
	echo "exclusion_claims: no sites assert a retired exclusion."
	exit 0
fi

printf 'exclusion_claims: %d site(s) mention an exclusion claim.\n' "$total"
printf '\nVerdict each one as TRUE (the exclusion still exists and is relied on),\n'
printf 'STALE-BUT-SOUND (the claim is false but the code is correct for another,\n'
printf 'stated reason), or DEFECT (the code rests on it). The test is always the\n'
printf 'same: what would this code do if NO exclusion existed?\n\n'

printf '%s\n' "${HITS[@]}" | sort -u -t: -k1,1 -k2,2n | awk -F: '
	{
		file = $1
		if (file != last) { printf "\n%s\n", file; last = file }
		line = $2
		$1 = ""; $2 = ""
		sub(/^  /, "")
		# Trim leading whitespace from the source line for readability.
		gsub(/^[ \t]+/, "")
		printf "  %6s: %s\n", line, substr($0, 1, 110)
	}'
