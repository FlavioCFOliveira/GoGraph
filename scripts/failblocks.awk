# failblocks.awk - extract COMPLETE failure blocks from `go test` output.
#
# WHY THIS EXISTS (rmp #2347). The gate that surfaces a failing coverage run
# used to filter the captured log with a line-pattern grep:
#
#   grep -E '(^--- FAIL|^FAIL[[:space:]]|panic:|...|_test\.go:[0-9]+:|...)'
#
# That matches only the FIRST line a failing test prints - the one carrying the
# `foo_test.go:NN:` prefix - and silently discards every continuation line the
# testing package indents beneath it. On 2026-08-07 an ST3 durability violation
# reached the operator as
#
#   --- FAIL: TestST3_CheckpointTeardown_Scenario (12.34s)
#       durable_scenarios_test.go:193: ST3 seed 0xc0ffee violation:
#   FAIL	github.com/FlavioCFOliveira/GoGraph/internal/sim	123.4s
#
# and nothing else. The report body naming the violated ACID invariant, and the
# `Reproduce with:` line carrying the seed, had both been thrown away here - not
# by the renderer, which cannot produce an empty string. A durability sighting
# was made unactionable by its own log filter.
#
# TWO BLOCK SHAPES, because go test uses two:
#
#   INDENTED (blk=1) - a `--- FAIL` header followed by detail lines that the
#     testing package indents beneath it. Closed by the first column-0 line.
#   VERBATIM (blk=2) - a panic, a fatal error, or a race report. Their bodies
#     are NOT indented (goroutine frames and race frames start at column 0), so
#     the indentation rule cannot carry them; they are closed only by a package
#     verdict.
#
# `max` caps the output. When the cap bites it is ANNOUNCED, and the cap keeps
# the FIRST lines rather than the last, because the first failure in a run is
# the one that explains the others. Callers must still point at the complete
# log: this filter is a summary, never the record.

BEGIN { blk = 0; n = 0; cut = 0; if (max == 0) max = 400 }

function emit(s) {
  if (n >= max) { cut++; return }
  print s; n++
}

# --- openers -----------------------------------------------------------------
/^[[:space:]]*--- FAIL/            { blk = 1; emit($0); next }
/^panic:/ || /^fatal error:/       { blk = 2; emit($0); next }
/^={10,}$/ || /WARNING: DATA RACE/ { blk = 2; emit($0); next }

# --- terminators -------------------------------------------------------------
# A package verdict closes any block. `ok` and `?` lines are noise on a failing
# run; every other verdict is surfaced.
/^(ok|FAIL|\?|---)[[:space:]]/ || /^FAIL$/ {
  blk = 0
  if ($0 !~ /^(ok|\?)[[:space:]]/) emit($0)
  next
}

# --- continuations -----------------------------------------------------------
blk == 2                       { emit($0); next }
blk == 1 && /^([[:space:]]|$)/ { emit($0); next }

# Any other column-0 line closes an indented block.
{ blk = 0 }

END {
  if (cut > 0)
    print "  [... " cut " further line(s) suppressed by the " max "-line cap; see the full log ...]"
}
