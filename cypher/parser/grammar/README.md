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

4. Re-run code generation (see `cypher/parser/tools.go` and the project
   Makefile target, when available) to regenerate the Go parser from the
   updated grammars.

5. Run the full test suite:

   ```bash
   go test -race ./...
   ```

6. Commit with a message of the form:

   ```
   chore(cypher): update vendored Cypher grammar to <short-hash>
   ```
