#!/usr/bin/env python3
"""Post-process the ANTLR4-generated Cypher parser/lexer for the GoGraph build.

This script performs two deterministic, behaviour-neutral transformations on the
freshly generated files in ``cypher/parser/gen/``:

1. **'go vet' clean-up of cypher_parser.go.** ANTLR4's Go generator emits two
   constructs that 'go vet' rejects:

   - Unreachable ``goto errorExit`` lines (after a ``return`` statement). ANTLR
     inserts these as a trick to stop the Go compiler complaining about an
     unused label when the label is only reached by the generated goto pattern.
     In functions that DO have another ``goto errorExit`` jump the trick line is
     genuinely unreachable; in functions that do NOT, the label itself is unused.
   - Unused ``errorExit:`` labels in functions that contain no explicit goto
     jump to that label (the only reference was the now-removed trick line).

   The script removes the trick goto lines in functions that have at least one
   other ``goto errorExit`` jump (the label stays in use), and removes the
   ``errorExit:`` label in functions where no goto jump exists (the
   error-handling block becomes plain sequential code, which is semantically
   equivalent since control already flows through it).

2. **Header path normalisation (all generated .go files).** ANTLR embeds the
   absolute path of the grammar file in the leading ``// Code generated from
   <ABS>/cypher/parser/grammar/<X>.g4 by ANTLR 4.13.1. DO NOT EDIT.`` comment.
   That absolute path makes the output depend on the checkout location, so the
   header is rewritten to the stable, repo-relative form
   ``cypher/parser/grammar/<X>.g4``. This is what makes a clean
   ``make generate-cypher-parser`` reproduce the checked-in gen byte-for-byte
   regardless of where the repository is checked out.

The remaining reproducibility steps — import grouping (``goimports``) and
re-applying the hand-written parser patches (``cypher/parser/grammar/gen-patches.patch``)
— are driven by the Makefile target, not by this script. See
``cypher/parser/grammar/README.md`` for the full pipeline.

Usage:
    python3 scripts/fix-antlr-gen.py <gen-dir>            # process the gen dir
    python3 scripts/fix-antlr-gen.py <gen-dir>/cypher_parser.go   # legacy form
"""

import os
import re
import sys

TRICK_LINE = "\tgoto errorExit // Trick to prevent compiler error if the label is not used\n"
LABEL_LINE = "errorExit:\n"

PARSER_FILE = "cypher_parser.go"

# Matches the ANTLR codegen header and captures the repo-relative grammar path.
_HEADER_RE = re.compile(
    r"^// Code generated from .*?(cypher/parser/grammar/[A-Za-z0-9_]+\.g4) by"
)


def fix_vet(path: str) -> None:
    """Remove unreachable trick gotos and unused errorExit labels (see module doc)."""
    with open(path, "r") as fh:
        lines = fh.readlines()

    # For every function, record its trick-goto line indices, its errorExit:
    # label indices, and how many *other* 'goto errorExit' jumps it contains.
    functions: list[dict] = []
    cur: dict = {"trick": [], "labels": [], "gotos": 0}

    for i, line in enumerate(lines):
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

    remove: set[int] = set()
    for fn in functions:
        if fn["gotos"] > 0:
            # Label still referenced by a real jump → drop only the trick lines.
            remove.update(fn["trick"])
        else:
            # No jump references the label → drop both the trick line and label.
            remove.update(fn["trick"])
            remove.update(fn["labels"])

    result = [line for i, line in enumerate(lines) if i not in remove]
    print(
        f"fix-antlr-gen: removed {len(lines) - len(result)} lines from {path}",
        file=sys.stderr,
    )
    with open(path, "w") as fh:
        fh.writelines(result)


def normalize_header(path: str) -> None:
    """Rewrite the absolute grammar path in the codegen header to a repo-relative one."""
    with open(path, "r") as fh:
        lines = fh.readlines()
    if not lines:
        return
    m = _HEADER_RE.match(lines[0])
    if m is None:
        return
    rel = m.group(1)
    new = f"// Code generated from {rel} by ANTLR 4.13.1. DO NOT EDIT.\n"
    if lines[0] != new:
        lines[0] = new
        with open(path, "w") as fh:
            fh.writelines(lines)


def process_dir(gen_dir: str) -> None:
    go_files = sorted(f for f in os.listdir(gen_dir) if f.endswith(".go"))
    for name in go_files:
        normalize_header(os.path.join(gen_dir, name))
    parser_path = os.path.join(gen_dir, PARSER_FILE)
    if os.path.isfile(parser_path):
        fix_vet(parser_path)
    else:
        print(
            f"fix-antlr-gen: warning: {PARSER_FILE} not found in {gen_dir}",
            file=sys.stderr,
        )


def main(arg: str) -> None:
    if os.path.isdir(arg):
        process_dir(arg)
        return
    # Legacy form: a single file path. Derive the gen dir and process it whole,
    # so callers of the old contract still get header normalisation as well.
    if os.path.isfile(arg):
        process_dir(os.path.dirname(arg) or ".")
        return
    print(f"fix-antlr-gen: no such file or directory: {arg}", file=sys.stderr)
    sys.exit(1)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"Usage: {sys.argv[0]} <gen-dir|file.go>", file=sys.stderr)
        sys.exit(1)
    main(sys.argv[1])
