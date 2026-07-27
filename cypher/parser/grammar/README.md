# Cypher ANTLR4 Grammar (vendored)

This directory contains vendored ANTLR4 grammar files for the Cypher query
language, sourced from the community-maintained
[antlr/grammars-v4](https://github.com/antlr/grammars-v4) repository.

## Files

| File | Description |
|---|---|
| `CypherLexer.g4` | ANTLR4 lexer grammar — tokenises Cypher input into a token stream. |
| `CypherParser.g4` | ANTLR4 parser grammar — defines the full Cypher syntax tree over that token stream. |
| `NOTICE.txt` | Attribution and BSD-3-Clause licence text. |

## Source

- **Repository:** https://github.com/antlr/grammars-v4/tree/master/cypher
- **Pinned commit:** `284602b3f23ca54dc30778204ab7ae9e969145e9` (HEAD as of 2026-05-20)
- **License:** BSD-3-Clause
- **Original author:** Boris Zhguchev and contributors

These files are **not** an official openCypher artefact and are not endorsed by
the openCypher project or Neo4j.

## Local modifications

The vendored grammar is modified locally where the upstream lacks a construct
GoGraph supports. These changes are **not** in the pinned upstream and must be
re-applied after any upstream refresh:

- **FOREACH** — the `FOREACH` lexer token (placed immediately before `ID` to
  avoid shifting existing keyword token ids) and the `foreachSt` parser rule
  (`FOREACH LPAREN symbol IN expression STICK updatingStatement+ RPAREN`, added
  as the last parser rule and listed in `updatingStatement`). Implements the
  openCypher FOREACH updating clause.

## How to update

1. Identify the new commit hash:

   ```bash
   git ls-remote https://github.com/antlr/grammars-v4 HEAD
   ```

2. Download the updated grammar files:

   ```bash
   GRAMMAR_DIR=cypher/parser/grammar
   BASE=https://raw.githubusercontent.com/antlr/grammars-v4/<commit-hash>/cypher

   curl -fsSL "$BASE/CypherLexer.g4"  -o "$GRAMMAR_DIR/CypherLexer.g4"
   curl -fsSL "$BASE/CypherParser.g4" -o "$GRAMMAR_DIR/CypherParser.g4"
   ```

3. Update the pinned commit hash and date in both this `README.md` and
   `NOTICE.txt`.

4. Re-run code generation with the project Makefile target:

   ```bash
   make generate-cypher-parser
   ```

   The target runs ANTLR, then `scripts/fix-antlr-gen.py` (`go vet` clean-up
   plus checkout-independent header normalisation), then `goimports`, then
   re-applies the hand-written parser patches captured in `gen-patches.patch`
   (see `docs/tck/parser-report.md` — numeric-ID workarounds, chained-WITH,
   optional CALL parentheses, and `reduce()`). For an unchanged grammar this
   reproduces `cypher/parser/gen/` byte-for-byte.

   If a grammar change shifts the code the patches target, the `git apply`
   step will fail. In that case re-apply the affected hand edits manually,
   confirm `go test ./cypher/parser/...` and the full TCK still pass, then
   refresh the patch:

   ```bash
   git diff cypher/parser/gen/ > cypher/parser/grammar/gen-patches.patch
   ```

   **A clean `git apply` is not proof the patch is still correct.** Several
   hunks pin *absolute ATN state numbers* (`p.SetState(N)`). Adding a rule
   alternative inserts states, so every state in a rule defined *after* the
   edited one shifts, and a hunk can still match on line context while
   pointing at the wrong state. Task #2216 hit this: one added alternative in
   `subqueryExist`/`subqueryCount` shifted every later state by **+10**, and
   four hunks in `Literal`, `RangeLit`, and `NumLit` had to be corrected by
   hand even though the build and `go vet` were clean.

   Two practices contain this:

   - **Prefer adding an *alternative* to an existing rule over adding a new
     rule.** A new rule shifts every rule index, which invalidates the
     `CypherParserRULE_*` constants — including `reduceExpression`, which the
     patch appends by hand — and regenerates the listener and visitor files.
     An added alternative leaves rule indices and those files untouched.
   - **Run `TestGenPatchBehaviours`** (`cypher/genpatch_behaviour_test.go`)
     after every regeneration. It pins each patched behaviour to a concrete
     result, which a compile check cannot do.

   To find the shift, compare state numbers between the pre-change generated
   file and the new one for a rule the patch touches; the delta is uniform for
   every rule after the edit.

5. Run the full test suite:

   ```bash
   go test -race ./...
   ```

6. Commit with a message of the form:

   ```
   chore(cypher): update vendored Cypher grammar to <short-hash>
   ```
