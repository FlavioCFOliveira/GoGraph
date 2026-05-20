// Package gen contains the ANTLR4-generated lexer and parser for openCypher 9.
//
// The sources in this package are produced by the ANTLR4 tool from the
// grammars in ../grammar/ and are not hand-edited.
//
// Regenerate with:
//
//	make generate-cypher-parser
//
// or manually:
//
//	java -jar ~/.antlr/antlr-4.13.1-complete.jar \
//	    -Dlanguage=Go -package gen -visitor \
//	    -o . \
//	    ../grammar/CypherLexer.g4 ../grammar/CypherParser.g4
//
// After generation, the post-processing step in scripts/fix-antlr-gen.py
// removes unreachable "goto errorExit" lines emitted by ANTLR so that
// "go vet ./cypher/parser/gen/..." passes without modifications to the
// grammar or the ANTLR tool itself.
package gen

//go:generate make -C ../../.. generate-cypher-parser
