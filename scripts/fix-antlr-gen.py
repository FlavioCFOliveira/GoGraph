#!/usr/bin/env python3
"""Post-process an ANTLR4-generated Go parser to make 'go vet' pass.

ANTLR4's Go code generator emits two constructs that cause 'go vet' to
complain:

1. Unreachable 'goto errorExit' lines (after a 'return' statement).
   ANTLR inserts these as a trick to prevent the Go compiler from
   complaining about an unused label when the label is only reached by
   the generated goto pattern.  In functions that DO have other goto
   errorExit jumps the trick line is genuinely unreachable; in functions
   that do NOT have other jumps the label itself is unused.

2. Unused 'errorExit:' labels in functions that contain no explicit goto
   jump to that label (the only reference was the now-removed trick line).

This script handles both cases:
- Removes the trick goto lines in functions that have at least one other
  goto errorExit jump (the label remains in use).
- Removes the errorExit: label in functions where no goto jump exists
  (the error-handling block becomes plain sequential code, which is
  semantically equivalent since control flows through it anyway).

Usage:
    python3 scripts/fix-antlr-gen.py <generated_file.go>
"""

import sys
import re

TRICK_LINE = "\tgoto errorExit // Trick to prevent compiler error if the label is not used\n"
LABEL_LINE = "errorExit:\n"


def fix(path: str) -> None:
    with open(path, "r") as fh:
        lines = fh.readlines()

    # First pass: determine, for each trick-goto line, whether its enclosing
    # function has at least one *other* goto errorExit jump.
    func_start_idx: int = -1
    # Maps trick-goto line index → count of other gotos seen so far in func.
    trick_lines: dict[int, int] = {}
    other_goto_count: int = 0

    for i, line in enumerate(lines):
        if line.startswith("func "):
            func_start_idx = i
            other_goto_count = 0
        if line == TRICK_LINE:
            trick_lines[i] = other_goto_count  # snapshot of count at this point
        elif "goto errorExit" in line:
            other_goto_count += 1

    # Build remove set for trick-goto lines.
    # A trick-goto is safe to remove when the label is still referenced
    # by at least one other goto jump in the same function.
    # When other_goto_count==0 at the trick line, the label will become
    # unused after removal — handle that in the second pass.
    remove_trick: set[int] = set()
    no_other_goto_trick: set[int] = set()
    for idx, count in trick_lines.items():
        if count > 0:
            remove_trick.add(idx)
        else:
            no_other_goto_trick.add(idx)

    # Remove trick lines where label stays referenced.
    filtered: list[str] = []
    for i, line in enumerate(lines):
        if i in remove_trick:
            continue
        filtered.append(line)

    # Second pass on the filtered list: remove errorExit: labels in
    # functions that had no other goto jump (and whose trick line was
    # kept above — now we also remove the trick line and the label).
    # At this point no_other_goto_trick contains the original line indices;
    # we need to map them into the filtered list.
    #
    # Simpler: do a single-pass rewrite to handle both cases atomically.
    # Rebuild from scratch using the original lines.

    lines2: list[str] = []
    func_other_gotos: int = 0
    trick_in_func: list[int] = []  # indices into lines2 for trick lines in cur func
    func_label_idx: list[int] = []  # indices into lines2 for errorExit: labels

    def flush_func() -> None:
        """Called when we hit the next function — finalise previous function."""
        nonlocal func_other_gotos, trick_in_func, func_label_idx
        func_other_gotos = 0
        trick_in_func = []
        func_label_idx = []

    func_other_gotos = 0
    trick_indices: list[int] = []   # positions in lines2 that are trick lines
    label_indices: list[int] = []   # positions in lines2 that are errorExit: labels
    # We cannot easily do a two-pass on lines2 because we don't know
    # which functions own which labels at write time.
    # Instead: collect all data, then do a targeted removal.

    lines_out: list[str] = list(lines)  # work on original

    # Reset and do a proper two-pass.
    # Pass 1: for every function, record:
    #   - trick goto line indices
    #   - errorExit: label indices
    #   - count of other goto errorExit
    functions: list[dict] = []  # list of {trick: [idx], labels: [idx], gotos: int}
    cur: dict = {"trick": [], "labels": [], "gotos": 0}

    for i, line in enumerate(lines_out):
        if line.startswith("func "):
            functions.append(cur)
            cur = {"trick": [], "labels": [], "gotos": 0}
        if line == TRICK_LINE:
            cur["trick"].append(i)
        elif line == LABEL_LINE:
            cur["labels"].append(i)
        elif "goto errorExit" in line:
            cur["gotos"] += 1
    functions.append(cur)

    # Build final remove set.
    final_remove: set[int] = set()
    for fn in functions:
        if fn["gotos"] > 0:
            # Label is referenced → remove trick lines only.
            final_remove.update(fn["trick"])
        else:
            # Label is not referenced by any jump → remove trick AND label.
            final_remove.update(fn["trick"])
            final_remove.update(fn["labels"])

    result = [line for i, line in enumerate(lines_out) if i not in final_remove]

    removed = len(lines_out) - len(result)
    print(f"fix-antlr-gen: removed {removed} lines from {path}", file=sys.stderr)

    with open(path, "w") as fh:
        fh.writelines(result)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"Usage: {sys.argv[0]} <file.go>", file=sys.stderr)
        sys.exit(1)
    fix(sys.argv[1])
